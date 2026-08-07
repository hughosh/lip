package rest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"lip/harness/cfg"
	"lip/harness/num"
	"lip/harness/quote"
	"lip/harness/risk"
)

func testOrder(t *testing.T, coid string) CreateOrder {
	t.Helper()
	b, err := NewCreateOrder("T1", quote.SideYes, 58,
		num.QtyFromFloat(1), num.QtyFromFloat(1), coid)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// ackBody builds a VALID acknowledgement. Every valid fixture supplies the
// matching coid, because the measured 200/201 shape carries one and a fixture
// that omitted it would normalise the incomplete shape into "normal".
func ackBody(orderID, coid, remaining, filled string) []byte {
	b, _ := json.Marshal(map[string]any{
		"order_id": orderID, "client_order_id": coid,
		"remaining_count": remaining, "fill_count": filled, "ts_ms": 1,
	})
	return b
}

var alreadyExistsBody = []byte(
	`{"error":{"code":"order_already_exists","message":"order already exists"}}`)

// V1.7a — "same-coid retry against a sim that ACKs then drops the response,
// twice". The exchange executes the create; we never learn that it did.
//
// This is the scenario H-ORD-2b was written for, and the whole point is that it
// resolves in one further round trip instead of requiring us to prove a
// negative.
func TestAmbiguousCreateRecoversViaSameCoid(t *testing.T) {
	const coid = "lipH-run1-000-yes-00000001"
	dropped := errors.New("connection reset after the exchange accepted it")

	var bodies [][]byte
	d := &scriptedDoer{t: t, handle: func(n int, req Request) (Response, error) {
		if req.Method == "POST" {
			bodies = append(bodies, req.Body)
			if n < 2 {
				// Accepted by the exchange; the response never arrives. Twice.
				return Response{}, dropped
			}
			return Response{Status: 409, Body: alreadyExistsBody}, nil
		}
		// The confirming read.
		return jsonPage(EpOrders, "", map[string][]any{
			"orders": {order("o1", coid, "T1", "yes", 0.58, "1.00")},
		}), nil
	}}

	res := NewClient(d).Create(context.Background(), testOrder(t, coid),
		cfg.Default())

	if res.Outcome != CreateAlreadyExists {
		t.Fatalf("want ALREADY_EXISTS, got %s (%v)", res.Outcome, res.Err)
	}
	if !res.Outcome.Exists() {
		t.Fatal("a 409 order_already_exists is a positive identification that " +
			"the original landed, not an error and not an absence")
	}
	if res.Attempts != 3 {
		t.Fatalf("want 3 attempts, got %d", res.Attempts)
	}

	// The coid is byte-identical on every attempt. A retry under a NEW coid is
	// a genuinely new order, the exchange cannot report it as a duplicate
	// because it is not one, and inventory doubles. A10 forbids it.
	if len(bodies) != 3 {
		t.Fatalf("want 3 POSTs, got %d", len(bodies))
	}
	for i, b := range bodies {
		if string(b) != string(bodies[0]) {
			t.Fatalf("attempt %d sent a different body; the retry MUST reuse "+
				"the same coid byte-for-byte\n first: %s\n  this: %s",
				i+1, bodies[0], b)
		}
		var got createOrderWire
		if err := json.Unmarshal(b, &got); err != nil {
			t.Fatal(err)
		}
		if got.ClientOrderID != coid {
			t.Fatalf("attempt %d used coid %q, want %q", i+1, got.ClientOrderID, coid)
		}
	}

	// Exactly one order exists, and the confirming read found it.
	if res.OrderID != "o1" {
		t.Fatalf("the confirming read must identify the order, got %q", res.OrderID)
	}
	if !res.ReconcileNow() {
		t.Fatal("a 409 still schedules RECONCILE_NOW: the dedupe behaviour is " +
			"observed on one account on one day, not documented")
	}
}

// A 409 carrying some OTHER code is a conflict we have not characterised. It is
// not a rejection, because we do not know that no order exists.
func TestUnrecognised409StaysUnknown(t *testing.T) {
	d := &scriptedDoer{t: t, handle: func(n int, req Request) (Response, error) {
		return Response{Status: 409,
			Body: []byte(`{"error":{"code":"market_closed"}}`)}, nil
	}}
	res := NewClient(d).Create(context.Background(),
		testOrder(t, "lipH-run1-000-yes-00000002"), cfg.Default())

	if res.Outcome != CreateUnknown {
		t.Fatalf("want UNKNOWN, got %s — only `%s` makes a 409 a positive "+
			"identification", res.Outcome, errAlreadyExists)
	}
	if res.MaxLive != num.QtyFromFloat(1) {
		t.Fatalf("an uncharacterised conflict must keep the full requested "+
			"size in the risk model, got %v", res.MaxLive)
	}
}

// A definite 4xx means no order exists, and nothing stays in the risk model.
func TestDefiniteRejectionClearsMaxLive(t *testing.T) {
	d := &scriptedDoer{t: t, handle: func(n int, req Request) (Response, error) {
		if n > 0 {
			t.Fatal("a definite rejection must not be retried as though it " +
				"were ambiguous")
		}
		return Response{Status: 400,
			Body: []byte(`{"error":{"code":"invalid_price"}}`)}, nil
	}}
	res := NewClient(d).Create(context.Background(),
		testOrder(t, "lipH-run1-000-yes-00000003"), cfg.Default())

	if res.Outcome != CreateRejected {
		t.Fatalf("want REJECTED, got %s", res.Outcome)
	}
	if res.MaxLive != 0 {
		t.Fatalf("a rejected order is not live; MaxLive = %v", res.MaxLive)
	}
	if res.ReconcileNow() {
		t.Fatal("a definite rejection needs no reconciliation")
	}
}

// M4 / M17 — the retry budget is finite, and exhausting it leaves the order
// UNKNOWN rather than resolving it in either direction. "Not found" is not a
// resolution; the branch that declared it never landed stays deleted.
func TestExhaustedRetriesStayUnknownAndKeepTheSizeLive(t *testing.T) {
	posts := 0
	d := &scriptedDoer{t: t, handle: func(n int, req Request) (Response, error) {
		posts++
		return Response{}, errors.New("timeout")
	}}
	p := cfg.Default()
	res := NewClient(d).Create(context.Background(),
		testOrder(t, "lipH-run1-000-yes-00000004"), p)

	if res.Outcome != CreateUnknown {
		t.Fatalf("want UNKNOWN, got %s", res.Outcome)
	}
	if posts != p.RetrySameCoidMax {
		t.Fatalf("want exactly retry_same_coid_max=%d attempts, got %d",
			p.RetrySameCoidMax, posts)
	}
	if res.MaxLive != num.QtyFromFloat(1) {
		t.Fatalf("an UNKNOWN order's maximum possibly-live quantity stays in "+
			"the risk model and in every aggregate cap until it is positively "+
			"resolved (H-ORD-2 clause 6); MaxLive = %v", res.MaxLive)
	}
	if !res.ReconcileNow() {
		t.Fatal("an unresolved create must schedule RECONCILE_NOW")
	}
	if len(res.Anomalies) != 1 || res.Anomalies[0].Class != "ORDER_UNKNOWN" ||
		res.Anomalies[0].Sev != risk.SEV2 {
		t.Fatalf("want one SEV2 ORDER_UNKNOWN, got %+v", res.Anomalies)
	}
}

// The zero value of a CreateResult must not read as a rejection: a rejection
// licenses re-placing.
func TestZeroCreateResultIsUnknownNotRejected(t *testing.T) {
	var res CreateResult
	if res.Outcome != CreateUnknown {
		t.Fatalf("the zero outcome is %s; an unpopulated result that read as "+
			"REJECTED would license a re-place of an order that may exist",
			res.Outcome)
	}
	if res.Outcome.Definite() || res.Outcome.Exists() {
		t.Fatal("UNKNOWN is neither definite nor known-to-exist")
	}
}

// A clean ack resolves in one round trip and rests only what the exchange says
// rests.
func TestAckRestsOnlyTheRemainingCount(t *testing.T) {
	d := &scriptedDoer{t: t, handle: func(n int, req Request) (Response, error) {
		if n > 0 {
			t.Fatalf("a 2xx is definite; call %d should not happen", n)
		}
		// Placed 1.00, 0.40 filled immediately, 0.60 rests.
		return Response{Status: 201, Body: ackBody("o9",
			"lipH-run1-000-yes-00000005", "0.60", "0.40")}, nil
	}}
	res := NewClient(d).Create(context.Background(),
		testOrder(t, "lipH-run1-000-yes-00000005"), cfg.Default())

	if res.Outcome != CreateAcked {
		t.Fatalf("want ACKED, got %s (%v)", res.Outcome, res.Err)
	}
	if res.OrderID != "o9" {
		t.Fatalf("order id %q", res.OrderID)
	}
	if res.Remaining != num.QtyFromFloat(0.6) || res.Filled != num.QtyFromFloat(0.4) {
		t.Fatalf("remaining=%v filled=%v", res.Remaining, res.Filled)
	}
	// Only `remaining_count` rests and therefore only it scores.
	if res.MaxLive != num.QtyFromFloat(0.6) {
		t.Fatalf("MaxLive = %v, want the resting 0.60", res.MaxLive)
	}
	if res.ReconcileNow() {
		t.Fatal("a clean ack needs no immediate reconciliation")
	}
}

// F1 — only an OBSERVED acknowledgement shape may narrow what we believe is
// live. Everything else stays UNKNOWN at full size, reconcile-now, and
// same-coid recoverable.
//
// The dangerous case is `200 {}`: it used to produce ACKED with Remaining = 0,
// MaxLive = 0, no error and no reconciliation. A 12-contract order at 99c that
// actually landed then looked like nothing at all, and the requote ladder would
// place a replacement on top of it — 24 contracts, $23.76, against a $100
// account, repeatable.
func TestOnlyObservedAckShapesNarrowMaxLive(t *testing.T) {
	for name, body := range map[string]string{
		"empty object":     `{}`,
		"null body":        `null`,
		"not json":         `not json`,
		"no order_id":      `{"remaining_count":"1.00","fill_count":"0.00"}`,
		"empty order_id":   `{"order_id":"","remaining_count":"1.00","fill_count":"0.00"}`,
		"no remaining":     `{"order_id":"o1","fill_count":"0.00"}`,
		"no fill_count":    `{"order_id":"o1","remaining_count":"1.00"}`,
		"null remaining":   `{"order_id":"o1","remaining_count":null,"fill_count":"0.00"}`,
		"negative count":   `{"order_id":"o1","remaining_count":"-1.00","fill_count":"0.00"}`,
		"over-requested":   `{"order_id":"o1","remaining_count":"9.00","fill_count":"0.00"}`,
		"sum over-request": `{"order_id":"o1","remaining_count":"1.00","fill_count":"1.00"}`,
		"unparseable":      `{"order_id":"o1","remaining_count":"1.0abc","fill_count":"0.00"}`,
		"foreign coid": `{"order_id":"o1","client_order_id":"someone-else",` +
			`"remaining_count":"1.00","fill_count":"0.00"}`,
		// The echoed coid is REQUIRED. A response with zero counts and no coid
		// would otherwise narrow a live order to MaxLive=0 and license a
		// replacement on top of it.
		"absent coid": `{"order_id":"o1","remaining_count":"0.00","fill_count":"0.00"}`,
		"null coid": `{"order_id":"o1","client_order_id":null,` +
			`"remaining_count":"0.00","fill_count":"0.00"}`,
		"empty coid": `{"order_id":"o1","client_order_id":"",` +
			`"remaining_count":"0.00","fill_count":"0.00"}`,
	} {
		t.Run(name, func(t *testing.T) {
			d := &scriptedDoer{t: t, handle: func(n int, req Request) (Response, error) {
				return Response{Status: 200, Body: []byte(body)}, nil
			}}
			res := NewClient(d).Create(context.Background(),
				testOrder(t, "lipH-run1-000-yes-00000006"), cfg.Default())

			if res.Outcome != CreateUnknown {
				t.Fatalf("%s was accepted as %s; an unrecognised "+
					"acknowledgement is not evidence about an order",
					name, res.Outcome)
			}
			if res.MaxLive != num.QtyFromFloat(1) {
				t.Fatalf("MaxLive = %v, want the full requested size: an "+
					"order whose ack we cannot read may be resting",
					res.MaxLive)
			}
			if !res.ReconcileNow() {
				t.Fatal("an unresolved create must schedule RECONCILE_NOW")
			}
			// Same-coid recoverable: it kept trying rather than giving up on
			// the first unreadable answer.
			if res.Attempts != cfg.Default().RetrySameCoidMax {
				t.Fatalf("attempts = %d, want the full same-coid budget",
					res.Attempts)
			}
		})
	}
}

// A 2xx outside the observed 200/201 pair is not an acknowledgement either.
func TestUnobserved2xxIsUnknown(t *testing.T) {
	for _, status := range []int{202, 204, 206} {
		d := &scriptedDoer{t: t, handle: func(n int, req Request) (Response, error) {
			return Response{Status: status, Body: []byte(`{}`)}, nil
		}}
		res := NewClient(d).Create(context.Background(),
			testOrder(t, "lipH-run1-000-yes-00000006"), cfg.Default())
		if res.Outcome != CreateUnknown {
			t.Fatalf("HTTP %d produced %s; only 200 and 201 are observed "+
				"create acknowledgements", status, res.Outcome)
		}
		if res.MaxLive != num.QtyFromFloat(1) {
			t.Fatalf("HTTP %d: MaxLive = %v, want full size", status, res.MaxLive)
		}
	}
}

// F3 — the send path revalidates intent AND the completed wire body. A corrupt
// or zero order is rejected before Doer is touched.
func TestCorruptIntentIsRejectedBeforeDispatch(t *testing.T) {
	good := testOrder(t, "lipH-run1-000-yes-00000001")

	corrupt := map[string]func(CreateOrder) CreateOrder{
		"zero value":     func(CreateOrder) CreateOrder { return CreateOrder{} },
		"empty ticker":   func(o CreateOrder) CreateOrder { o.ticker = ""; return o },
		"bad side":       func(o CreateOrder) CreateOrder { o.side = quote.Side(9); return o },
		"price 0":        func(o CreateOrder) CreateOrder { o.priceCents = 0; return o },
		"price 100":      func(o CreateOrder) CreateOrder { o.priceCents = 100; return o },
		"zero count":     func(o CreateOrder) CreateOrder { o.count = 0; return o },
		"negative count": func(o CreateOrder) CreateOrder { o.count = -100; return o },
		"over bound": func(o CreateOrder) CreateOrder {
			o.count = o.derivedFrom + 100
			return o
		},
		"empty coid":   func(o CreateOrder) CreateOrder { o.clientOrderID = ""; return o },
		"foreign coid": func(o CreateOrder) CreateOrder { o.clientOrderID = "x"; return o },
		"coid side mismatch": func(o CreateOrder) CreateOrder {
			o.clientOrderID = "lipH-run1-000-no-00000001"
			return o
		},
		"coid does not round-trip": func(o CreateOrder) CreateOrder {
			o.clientOrderID = "lipH-run1-0000-yes-00000001"
			return o
		},
	}

	for name, mutate := range corrupt {
		t.Run(name, func(t *testing.T) {
			d := &scriptedDoer{t: t, handle: func(n int, req Request) (Response, error) {
				t.Fatalf("%s reached the exchange: %s", name, req.Body)
				return Response{}, nil
			}}
			res := NewClient(d).Create(context.Background(), mutate(good), cfg.Default())
			if res.Outcome != CreateRejected {
				t.Fatalf("want REJECTED, got %s", res.Outcome)
			}
			if res.Attempts != 0 || res.MaxLive != 0 {
				t.Fatalf("nothing was sent, so nothing is live: "+
					"attempts=%d maxlive=%v", res.Attempts, res.MaxLive)
			}
			if len(d.Calls()) != 0 {
				t.Fatalf("Doer was called %d times", len(d.Calls()))
			}
		})
	}
}

// validateWire is the independent second check. It must reject a wire body that
// disagrees with the intent that produced it, whatever produced it.
func TestValidateWireRejectsEveryPolicyDeviation(t *testing.T) {
	o, err := NewCreateOrder("T1", quote.SideNo, 42, num.QtyFromFloat(3),
		num.QtyFromFloat(3), "lipH-run1-000-no-00000005")
	if err != nil {
		t.Fatal(err)
	}
	base, err := o.wire()
	if err != nil {
		t.Fatal(err)
	}
	if err := validateWire(base, o); err != nil {
		t.Fatalf("the honestly-derived wire body must pass: %v", err)
	}

	for name, mutate := range map[string]func(createOrderWire) createOrderWire{
		"post_only false": func(w createOrderWire) createOrderWire {
			w.PostOnly = false
			return w
		},
		"maker STP": func(w createOrderWire) createOrderWire {
			w.SelfTradePrevention = "maker"
			return w
		},
		"empty STP": func(w createOrderWire) createOrderWire {
			w.SelfTradePrevention = ""
			return w
		},
		"immediate or cancel": func(w createOrderWire) createOrderWire {
			w.TimeInForce = "immediate_or_cancel"
			return w
		},
		"other ticker":  func(w createOrderWire) createOrderWire { w.Ticker = "OTHER"; return w },
		"other coid":    func(w createOrderWire) createOrderWire { w.ClientOrderID = "x"; return w },
		"other count":   func(w createOrderWire) createOrderWire { w.Count = "9.00"; return w },
		"count zeroed":  func(w createOrderWire) createOrderWire { w.Count = "0.00"; return w },
		"flipped side":  func(w createOrderWire) createOrderWire { w.Side = Bid; return w },
		"other price":   func(w createOrderWire) createOrderWire { w.Price = "0.9900"; return w },
		"price as cent": func(w createOrderWire) createOrderWire { w.Price = "58"; return w },
	} {
		t.Run(name, func(t *testing.T) {
			if err := validateWire(mutate(base), o); err == nil {
				t.Fatalf("%s was accepted for dispatch", name)
			}
		})
	}
}

// H-ORD-2b: the 409 is belt, the confirming read is braces. An incomplete
// confirming read must not narrow what we believe is live.
func TestIncompleteConfirmingReadKeepsTheFullSizeLive(t *testing.T) {
	const coid = "lipH-run1-000-yes-00000007"
	d := &scriptedDoer{t: t, handle: func(n int, req Request) (Response, error) {
		if req.Method == "POST" {
			return Response{Status: 409, Body: alreadyExistsBody}, nil
		}
		return Response{}, errors.New("the confirming read died at page one")
	}}
	res := NewClient(d).Create(context.Background(), testOrder(t, coid), cfg.Default())

	if res.Outcome != CreateAlreadyExists {
		t.Fatalf("a failed confirming read cannot revoke a positive "+
			"identification; got %s", res.Outcome)
	}
	if res.MaxLive != num.QtyFromFloat(1) {
		t.Fatalf("MaxLive = %v, want the full requested size", res.MaxLive)
	}
	if len(res.Anomalies) == 0 || res.Anomalies[0].Class != "CREATE_UNCONFIRMED" {
		t.Fatalf("want CREATE_UNCONFIRMED, got %+v", res.Anomalies)
	}
}

// A complete confirming read that finds nothing does NOT downgrade the 409.
// The order landed; it is simply no longer open.
func TestCompleteConfirmingReadFindingNothingDoesNotDowngrade(t *testing.T) {
	const coid = "lipH-run1-000-yes-00000008"
	d := &scriptedDoer{t: t, handle: func(n int, req Request) (Response, error) {
		if req.Method == "POST" {
			return Response{Status: 409, Body: alreadyExistsBody}, nil
		}
		return jsonPage(EpOrders, "", map[string][]any{"orders": {}}), nil
	}}
	res := NewClient(d).Create(context.Background(), testOrder(t, coid), cfg.Default())

	if res.Outcome != CreateAlreadyExists {
		t.Fatalf("absence is not evidence against a positive identification "+
			"we already hold; got %s", res.Outcome)
	}
	if len(res.Anomalies) == 0 || res.Anomalies[0].Class != "CREATE_TERMINAL" {
		t.Fatalf("want CREATE_TERMINAL, got %+v", res.Anomalies)
	}
}

// The confirming read is unfiltered by status: an order that landed and then
// filled is still an order that landed, and `status=resting` would miss it.
func TestConfirmingReadIsNotStatusFiltered(t *testing.T) {
	const coid = "lipH-run1-000-yes-00000009"
	d := &scriptedDoer{t: t, handle: func(n int, req Request) (Response, error) {
		if req.Method == "POST" {
			return Response{Status: 409, Body: alreadyExistsBody}, nil
		}
		if s := req.Query.Get("status"); s != "" {
			t.Fatalf("the confirming read must span every status, got status=%q", s)
		}
		return jsonPage(EpOrders, "", map[string][]any{"orders": {}}), nil
	}}
	NewClient(d).Create(context.Background(), testOrder(t, coid), cfg.Default())
}

// A count that is not dispatchable never reaches the wire.
func TestUndispatchableCountIsNotSent(t *testing.T) {
	d := &scriptedDoer{t: t, handle: func(n int, req Request) (Response, error) {
		t.Fatalf("nothing may be sent: %+v", req)
		return Response{}, nil
	}}
	// A zero CreateOrder exists -- Go permits `var o CreateOrder` -- and must
	// never be dispatchable.
	var zero CreateOrder
	res := NewClient(d).Create(context.Background(), zero, cfg.Default())
	if res.Outcome != CreateRejected {
		t.Fatalf("want REJECTED without sending, got %s", res.Outcome)
	}
	if res.Attempts != 0 || res.MaxLive != 0 {
		t.Fatalf("nothing was sent, so nothing is live: attempts=%d maxlive=%v",
			res.Attempts, res.MaxLive)
	}
	if len(d.Calls()) != 0 {
		t.Fatalf("want 0 requests, got %d", len(d.Calls()))
	}
}

// A request that provably never left the process cannot have created an order.
// Treating it as UNKNOWN would not be caution: an UNKNOWN keeps its full size
// in every aggregate cap, so a broken signer would consume the pilot's whole
// capital budget with orders that never existed.
func TestNotSentIsADefiniteRejectionNotUnknown(t *testing.T) {
	attempts := 0
	d := &scriptedDoer{t: t, handle: func(n int, req Request) (Response, error) {
		attempts++
		return Response{}, &NotSent{Err: errors.New("no signing key")}
	}}
	res := NewClient(d).Create(context.Background(),
		testOrder(t, "lipH-run1-000-yes-00000012"), cfg.Default())

	if res.Outcome != CreateRejected {
		t.Fatalf("want REJECTED, got %s", res.Outcome)
	}
	if res.MaxLive != 0 {
		t.Fatalf("an order that was never sent is not live; MaxLive = %v", res.MaxLive)
	}
	if attempts != 1 {
		t.Fatalf("a definite no needs no same-coid retry, got %d attempts", attempts)
	}
	if res.ReconcileNow() {
		t.Fatal("nothing to reconcile: the request never reached the exchange")
	}
}

// The narrowness of NotSent is the point: anything that touched the network
// stays ambiguous, because the exchange may have acted before we lost the
// answer.
func TestWasSentDiscriminates(t *testing.T) {
	if WasSent(&NotSent{Err: errors.New("x")}) {
		t.Error("a NotSent request was not sent")
	}
	if !WasSent(errors.New("connection reset")) {
		t.Error("a reset may have been executed before we lost the answer")
	}
	if !WasSent(fmt.Errorf("wrapped: %w", errors.New("timeout"))) {
		t.Error("a timeout is ambiguous")
	}
	// Wrapping must survive.
	if WasSent(fmt.Errorf("outer: %w", &NotSent{Err: errors.New("x")})) {
		t.Error("NotSent must be detectable through wrapping")
	}
}

// The create posts to the V2 endpoint. The legacy POST /portfolio/orders
// returns 410.
func TestCreateUsesTheV2Endpoint(t *testing.T) {
	d := &scriptedDoer{t: t, handle: func(n int, req Request) (Response, error) {
		if req.Path != "/portfolio/events/orders" {
			t.Fatalf("create posted to %q; the legacy endpoint is gone", req.Path)
		}
		if req.Method != "POST" {
			t.Fatalf("method %q", req.Method)
		}
		return Response{Status: 200, Body: ackBody("o1",
			"lipH-run1-000-yes-00000011", "1.00", "0.00")}, nil
	}}
	NewClient(d).Create(context.Background(),
		testOrder(t, "lipH-run1-000-yes-00000011"), cfg.Default())
}
