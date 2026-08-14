package rest

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"lip/harness/num"
	"lip/harness/risk"
)

func cancelBody(orderID, coid, reducedBy string) []byte {
	// The real shape: a flat acknowledgement, NOT an order object.
	b, _ := json.Marshal(map[string]any{
		"order_id": orderID, "client_order_id": coid,
		"reduced_by": reducedBy, "ts_ms": 1,
	})
	return b
}

func ourOrder(id, coid string) Order {
	return Order{OrderID: id, ClientOrderID: coid, Ticker: "T1", Ours: true}
}

// M12 — "skip the cancel-sweep verification", against a sim that ignores the
// first cancel. A 2xx is a cancel *request*; only a complete read is a fact.
func TestSweepCatchesAnIgnoredFirstCancel(t *testing.T) {
	const coid = "lipH-run1-000-yes-00000001"
	deletes := 0
	d := &scriptedDoer{t: t, handle: func(n int, req Request) (Response, error) {
		if req.Method == "DELETE" {
			deletes++
			// Both DELETEs answer 200. The first one is ignored anyway --
			// which is exactly why the answer cannot be trusted.
			return Response{Status: 200, Body: cancelBody("o1", coid, "1.00")}, nil
		}
		if deletes == 1 {
			// Round 1's verification: the order is still there.
			return jsonPage(EpOrders, "", map[string][]any{
				"orders": {order("o1", coid, "T1", "yes", 0.58, "1.00")},
			}), nil
		}
		// Round 2's verification: gone.
		return jsonPage(EpOrders, "", map[string][]any{"orders": {}}), nil
	}}

	res := NewClient(d).CancelAndSweep(context.Background(), "T1",
		[]Order{ourOrder("o1", coid)})

	if !res.Clean {
		t.Fatalf("the retry round should have cleared it: %+v", res.StillResting)
	}
	if res.Rounds != 2 {
		t.Fatalf("want 2 rounds (issue, verify, retry, verify), got %d", res.Rounds)
	}
	if deletes != 2 {
		t.Fatalf("want 2 DELETEs; a sweep that still finds our orders retries "+
			"once (H-ORD-4), got %d", deletes)
	}
}

// A sweep that still finds our orders after its one retry pings SEV1.
func TestSweepEscalatesWhenTheOrderWillNotDie(t *testing.T) {
	const coid = "lipH-run1-000-yes-00000002"
	d := &scriptedDoer{t: t, handle: func(n int, req Request) (Response, error) {
		if req.Method == "DELETE" {
			return Response{Status: 200, Body: cancelBody("o1", coid, "0.00")}, nil
		}
		return jsonPage(EpOrders, "", map[string][]any{
			"orders": {order("o1", coid, "T1", "yes", 0.58, "1.00")},
		}), nil
	}}

	res := NewClient(d).CancelAndSweep(context.Background(), "T1",
		[]Order{ourOrder("o1", coid)})

	if res.Clean {
		t.Fatal("an order a complete read still shows resting is not off")
	}
	if len(res.StillResting) != 1 || res.StillResting[0].OrderID != "o1" {
		t.Fatalf("still resting: %+v", res.StillResting)
	}
	if res.Rounds != 2 {
		t.Fatalf("want exactly 1 + 1 retry rounds, got %d", res.Rounds)
	}
	var found bool
	for _, a := range res.Anomalies {
		if a.Class == "SWEEP_INCOMPLETE" && a.Sev == risk.SEV1 {
			found = true
		}
	}
	if !found {
		t.Fatalf("want SEV1 SWEEP_INCOMPLETE, got %+v", res.Anomalies)
	}
}

// M21 — "treat a cancel-requested adding order as off". H-FAIL-3: "off" means
// exchange-confirmed absent, never cancel-requested. A 200 with a reduced_by
// equal to the whole order is still only a request.
func TestAcceptedCancelAloneDoesNotMakeAnOrderOff(t *testing.T) {
	const coid = "lipH-run1-000-yes-00000003"
	d := &scriptedDoer{t: t, handle: func(n int, req Request) (Response, error) {
		if req.Method == "DELETE" {
			return Response{Status: 200, Body: cancelBody("o1", coid, "1.00")}, nil
		}
		// The exchange still shows it resting: the cancel was requested, not
		// effected.
		return jsonPage(EpOrders, "", map[string][]any{
			"orders": {order("o1", coid, "T1", "yes", 0.58, "1.00")},
		}), nil
	}}

	res := NewClient(d).CancelAndSweep(context.Background(), "T1",
		[]Order{ourOrder("o1", coid)})

	// Every DELETE said 200 and reduced_by 1.00 -- the whole order.
	for i, c := range res.Cancels {
		if c.Outcome != CancelAccepted || c.ReducedBy != num.QtyFromFloat(1) {
			t.Fatalf("cancel %d: %+v", i, c)
		}
	}
	if res.Clean {
		t.Fatal("a 200 with reduced_by covering the whole order is a cancel " +
			"REQUEST; the order is live and fillable until a complete read " +
			"confirms it absent (H-FAIL-3)")
	}
}

// An incomplete verifying read cannot declare a sweep clean. This is the
// single-page defect in a new place: an order at item 201 reads as absent.
func TestIncompleteVerifyingReadIsNeverClean(t *testing.T) {
	const coid = "lipH-run1-000-yes-00000004"
	d := &scriptedDoer{t: t, handle: func(n int, req Request) (Response, error) {
		if req.Method == "DELETE" {
			return Response{Status: 200, Body: cancelBody("o1", coid, "1.00")}, nil
		}
		return Response{}, errors.New("the verifying read never completed")
	}}

	res := NewClient(d).CancelAndSweep(context.Background(), "T1",
		[]Order{ourOrder("o1", coid)})

	if res.Clean {
		t.Fatal("an incomplete read establishes nothing and must not license " +
			"treating the order as off")
	}
	if res.StillResting != nil {
		t.Fatalf("an unverified sweep must not claim to know what rests: %+v",
			res.StillResting)
	}
	var found bool
	for _, a := range res.Anomalies {
		if a.Class == "SWEEP_UNVERIFIED" && a.Sev == risk.SEV1 {
			found = true
		}
	}
	if !found {
		t.Fatalf("want SEV1 SWEEP_UNVERIFIED, got %+v", res.Anomalies)
	}
}

// A partial cancel is legal, not an error, and it says nothing about position.
func TestPartialCancelIsNotAnErrorAndNotAFillReport(t *testing.T) {
	const coid = "lipH-run1-000-yes-00000005"
	d := &scriptedDoer{t: t, handle: func(n int, req Request) (Response, error) {
		return Response{Status: 200, Body: cancelBody("o1", coid, "0.40")}, nil
	}}
	res := NewClient(d).Cancel(context.Background(), "o1")

	if res.Outcome != CancelAccepted {
		t.Fatalf("a partial cancel is not an error, got %s (%v)", res.Outcome, res.Err)
	}
	if res.Err != nil {
		t.Fatalf("unexpected error: %v", res.Err)
	}
	if res.ReducedBy != num.QtyFromFloat(0.4) {
		t.Fatalf("reduced_by = %v, want 0.40", res.ReducedBy)
	}
	if res.Coid != coid {
		t.Fatalf("coid = %q", res.Coid)
	}

	// The structural half of H-ORD-4a: there is nothing on this type to
	// subtract a reduced_by FROM. `remainder := requested - ReducedBy` is the
	// arithmetic that reads a retry's reduced_by=0 as "the remainder filled",
	// and it is unavailable because no field here describes the order's size,
	// the position, or a fill.
	forbidden := map[string]bool{
		"Requested": true, "Remaining": true, "Filled": true,
		"Position": true, "Q": true, "Remainder": true, "Count": true,
	}
	rt := reflect.TypeOf(CancelResult{})
	for i := 0; i < rt.NumField(); i++ {
		if forbidden[rt.Field(i).Name] {
			t.Errorf("CancelResult.%s exists; `reduced_by` proves only what "+
				"that DELETE removed and never implies the position moved. "+
				"Position changes come from /portfolio/positions and "+
				"/portfolio/fills, never from a cancel response (H-ORD-4a)",
				rt.Field(i).Name)
		}
	}
}

// The zero value of a CancelResult must not read as success.
func TestZeroCancelResultIsUnknown(t *testing.T) {
	var res CancelResult
	if res.Outcome != CancelUnknown {
		t.Fatalf("the zero outcome is %s; an unpopulated cancel that read as "+
			"ACCEPTED would retire a live order from the risk model", res.Outcome)
	}
}

// A 404 is a definite answer about that order, and it is not a fill report.
func TestCancelOf404IsGoneNotFilled(t *testing.T) {
	d := &scriptedDoer{t: t, handle: func(n int, req Request) (Response, error) {
		return Response{Status: 404, Body: []byte(`{"error":{"code":"not_found"}}`)}, nil
	}}
	res := NewClient(d).Cancel(context.Background(), "o1")
	if res.Outcome != CancelGone {
		t.Fatalf("want GONE, got %s", res.Outcome)
	}
	if res.ReducedBy != 0 {
		t.Fatalf("a 404 removed nothing, got reduced_by %v", res.ReducedBy)
	}
	if res.Err != nil {
		t.Fatalf("a 404 on a cancel is not an error condition: %v", res.Err)
	}
}

// TestRejectedCancelCarriesTheExchangeErrorCode is the cancel half of the same
// seam.
//
// A cancel is a write and the exchange refuses it through the same mechanism it
// refuses a create, so a 429 on a DELETE is F8's detection condition arriving on
// the one write that only ever reduces exposure. The status and the code must
// survive together, and the classification is unchanged: every row here stays
// CancelRejected, which is what §7.4 already said.
func TestRejectedCancelCarriesTheExchangeErrorCode(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
		want   string
	}{
		{"429 rate limit", 429,
			`{"error":{"code":"rate_limited","message":"too many requests"}}`,
			"rate_limited"},
		{"not cancelable", 400,
			`{"error":{"code":"order_not_cancelable","message":"terminal"}}`,
			"order_not_cancelable"},

		// Malformed or code-less: empty, and the classification does not move.
		{"body is not json", 400, `<html>bad request</html>`, ""},
		{"error carries no code", 400, `{"error":{"message":"nope"}}`, ""},
		{"no error object", 403, `{}`, ""},
		{"code is empty", 400, `{"error":{"code":""}}`, ""},
		{"null body", 400, `null`, ""},
		{"empty body", 400, ``, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := &scriptedDoer{t: t, handle: func(n int, req Request) (Response, error) {
				return Response{Status: tc.status, Body: []byte(tc.body)}, nil
			}}
			res := NewClient(d).Cancel(context.Background(), "o1")

			if res.Outcome != CancelRejected {
				t.Fatalf("outcome = %s, want REJECTED (%v)", res.Outcome, res.Err)
			}
			if res.Status != tc.status {
				t.Fatalf("status = %d, want %d", res.Status, tc.status)
			}
			if res.RejectReason != tc.want {
				t.Fatalf("reject reason = %q, want %q: the reason is the "+
					"exchange's error.code and nothing else",
					res.RejectReason, tc.want)
			}
			if !res.Sent {
				t.Fatal("a DELETE the exchange answered was not reported sent")
			}
			if res.ReducedBy != 0 {
				t.Fatalf("a rejected cancel removed nothing, got reduced_by %v",
					res.ReducedBy)
			}
		})
	}
}

// TestOnlyARejectedCancelCarriesAReason keeps the field from inventing facts.
//
// The 404 row is the one with teeth. `CancelGone` is the exchange answering
// about the ORDER — there is no such open order — rather than rejecting our
// request, and it is not a fill report either (H-ORD-4a). A 404 body naming a
// code must not become a reject reason: it would feed F9's definite-reject
// count with an event that was not a reject at all.
func TestOnlyARejectedCancelCarriesAReason(t *testing.T) {
	const coid = "lipH-run1-000-yes-00000010"

	for name, run := range map[string]func(t *testing.T) CancelResult{
		"accepted": func(t *testing.T) CancelResult {
			d := &scriptedDoer{t: t, handle: func(n int, req Request) (Response, error) {
				return Response{Status: 200, Body: cancelBody("o1", coid, "1.00")}, nil
			}}
			return NewClient(d).Cancel(context.Background(), "o1")
		},
		"404 gone, with a code in the body": func(t *testing.T) CancelResult {
			d := &scriptedDoer{t: t, handle: func(n int, req Request) (Response, error) {
				return Response{Status: 404,
					Body: []byte(`{"error":{"code":"order_not_found"}}`)}, nil
			}}
			return NewClient(d).Cancel(context.Background(), "o1")
		},
		"5xx": func(t *testing.T) CancelResult {
			d := &scriptedDoer{t: t, handle: func(n int, req Request) (Response, error) {
				return Response{Status: 503,
					Body: []byte(`{"error":{"code":"unavailable"}}`)}, nil
			}}
			return NewClient(d).Cancel(context.Background(), "o1")
		},
		"dropped response": func(t *testing.T) CancelResult {
			d := &scriptedDoer{t: t, handle: func(n int, req Request) (Response, error) {
				return Response{}, errors.New("timeout")
			}}
			return NewClient(d).Cancel(context.Background(), "o1")
		},
		"guarded refusal": func(t *testing.T) CancelResult {
			inner := &scriptedDoer{t: t, handle: func(int, Request) (Response, error) {
				t.Fatal("a refused cancel reached the transport")
				return Response{}, nil
			}}
			g, err := NewWriteGuard(inner, WriteArm{}) // read-only
			if err != nil {
				t.Fatalf("NewWriteGuard: %v", err)
			}
			return NewClient(g).Cancel(context.Background(), "o1")
		},
		"empty order id": func(t *testing.T) CancelResult {
			d := &scriptedDoer{t: t, handle: func(n int, req Request) (Response, error) {
				t.Fatalf("a cancel with no order id reached the exchange: %+v", req)
				return Response{}, nil
			}}
			return NewClient(d).Cancel(context.Background(), "")
		},
	} {
		t.Run(name, func(t *testing.T) {
			res := run(t)
			if res.RejectReason != "" {
				t.Fatalf("%s produced outcome %s carrying reject reason %q; only "+
					"an ordinary 4xx is the exchange rejecting the request and "+
					"saying why", name, res.Outcome, res.RejectReason)
			}
		})
	}
}

// A dropped DELETE response leaves the order live, not cancelled.
func TestAmbiguousCancelStaysUnknown(t *testing.T) {
	d := &scriptedDoer{t: t, handle: func(n int, req Request) (Response, error) {
		return Response{}, errors.New("timeout")
	}}
	res := NewClient(d).Cancel(context.Background(), "o1")
	if res.Outcome != CancelUnknown {
		t.Fatalf("want UNKNOWN, got %s", res.Outcome)
	}
}

// The DELETE goes to the V2 path, and the sweep verifies with the pinned
// `resting` status string.
func TestCancelUsesV2PathAndSweepUsesRestingStatus(t *testing.T) {
	const coid = "lipH-run1-000-yes-00000006"
	var sawDelete, sawRead bool
	d := &scriptedDoer{t: t, handle: func(n int, req Request) (Response, error) {
		if req.Method == "DELETE" {
			sawDelete = true
			if req.Path != "/portfolio/events/orders/o1" {
				t.Fatalf("DELETE path %q", req.Path)
			}
			return Response{Status: 200, Body: cancelBody("o1", coid, "1.00")}, nil
		}
		sawRead = true
		if got := req.Query.Get("status"); got != StatusResting {
			t.Fatalf("the verifying read must filter on the pinned %q, got %q",
				StatusResting, got)
		}
		if got := req.Query.Get("ticker"); got != "T1" {
			t.Fatalf("ticker filter %q", got)
		}
		return jsonPage(EpOrders, "", map[string][]any{"orders": {}}), nil
	}}

	res := NewClient(d).CancelAndSweep(context.Background(), "T1",
		[]Order{ourOrder("o1", coid)})
	if !sawDelete || !sawRead {
		t.Fatalf("delete=%v read=%v", sawDelete, sawRead)
	}
	if !res.Clean {
		t.Fatalf("want a clean sweep: %+v", res)
	}
}

// M2 territory. The verifying read turns up another of OUR orders that was not
// in the requested set — on a market halt that is the reducing quote, and A4
// requires it be kept alive. A sweep that cancelled "everything the verifying
// read found" would remove the exit and keep the risk, which is `probebot.py`'s
// exact defect.
func TestSweepDoesNotCancelOurOrdersItWasNotAskedTo(t *testing.T) {
	const addCoid = "lipH-run1-000-yes-00000008"
	const reducerCoid = "lipH-run1-000-no-00000009"
	var deleted []string
	d := &scriptedDoer{t: t, handle: func(n int, req Request) (Response, error) {
		if req.Method == "DELETE" {
			id := strings.TrimPrefix(req.Path, "/portfolio/events/orders/")
			deleted = append(deleted, id)
			return Response{Status: 200, Body: cancelBody(id, addCoid, "1.00")}, nil
		}
		// The adding order is gone; the reducer is still there, as it must be.
		return jsonPage(EpOrders, "", map[string][]any{
			"orders": {order("oR", reducerCoid, "T1", "no", 0.41, "3.00")},
		}), nil
	}}

	res := NewClient(d).CancelAndSweep(context.Background(), "T1",
		[]Order{ourOrder("oA", addCoid)})

	if !res.Clean {
		t.Fatalf("every REQUESTED order is gone, so the sweep is clean: %+v",
			res.StillResting)
	}
	if len(res.OtherOurs) != 1 || res.OtherOurs[0].OrderID != "oR" {
		t.Fatalf("the reducer must be reported, not silently dropped: %+v",
			res.OtherOurs)
	}
	for _, id := range deleted {
		if id == "oR" {
			t.Fatal("the sweep cancelled the reducing quote it was not asked " +
				"to cancel; A4 requires the reducer be kept alive on a halt, " +
				"and removing the exit while keeping the risk is the defect " +
				"this whole harness exists to prevent")
		}
	}
	if len(deleted) != 1 || deleted[0] != "oA" {
		t.Fatalf("want exactly the requested order deleted, got %v", deleted)
	}
}

// F5 — requested-ID matching must happen BEFORE ownership classification.
//
// We asked to cancel a specific order id. Whether the exchange echoed a coid we
// recognise does not change whether that id is still resting. Filtering by
// `Ours()` first drops a requested order whose `client_order_id` came back
// missing or malformed: it is classified foreign, omitted from StillResting,
// and the sweep reports Clean while the order is live and fillable. The caller
// then places a replacement on top of it — 24 contracts, $23.76.
//
// This is the permanent gate on `read.Orders` not reverting to `read.Ours()`.
func TestSweepMatchesRequestedIDBeforeOwnership(t *testing.T) {
	for name, coid := range map[string]string{
		"missing coid":   "",
		"malformed coid": "lipH-not-a-valid-coid",
	} {
		t.Run(name, func(t *testing.T) {
			deletes := 0
			d := &scriptedDoer{t: t, handle: func(n int, req Request) (Response, error) {
				if req.Method == "DELETE" {
					deletes++
					return Response{Status: 200,
						Body: cancelBody("o1", coid, "0.00")}, nil
				}
				// Both verifying reads still show the requested order id, with
				// a coid we cannot attribute.
				return jsonPage(EpOrders, "", map[string][]any{
					"orders": {order("o1", coid, "T1", "yes", 0.58, "1.00")},
				}), nil
			}}

			res := NewClient(d).CancelAndSweep(context.Background(), "T1",
				[]Order{ourOrder("o1", "lipH-run1-000-yes-00000001")})

			if res.Clean {
				t.Fatal("the requested order is still resting, but the sweep " +
					"reported Clean because its coid did not parse; an " +
					"unrecognised coid does not make a live order absent")
			}
			if len(res.StillResting) != 1 || res.StillResting[0].OrderID != "o1" {
				t.Fatalf("the requested id must be retained regardless of coid "+
					"parseability, got %+v", res.StillResting)
			}
			if deletes != 2 {
				t.Fatalf("want 2 DELETEs (initial plus the one retry), got %d",
					deletes)
			}
		})
	}
}

// A foreign order resting on the account does not block our sweep -- H-ORD-5
// step 5 says do not cancel it -- but it must not be cancelled either.
func TestSweepIgnoresForeignOrders(t *testing.T) {
	const coid = "lipH-run1-000-yes-00000007"
	var deleted []string
	d := &scriptedDoer{t: t, handle: func(n int, req Request) (Response, error) {
		if req.Method == "DELETE" {
			deleted = append(deleted, strings.TrimPrefix(req.Path,
				"/portfolio/events/orders/"))
			return Response{Status: 200, Body: cancelBody("o1", coid, "1.00")}, nil
		}
		return jsonPage(EpOrders, "", map[string][]any{
			"orders": {order("oX", "not-ours", "T1", "yes", 0.58, "1.00")},
		}), nil
	}}

	res := NewClient(d).CancelAndSweep(context.Background(), "T1",
		[]Order{ourOrder("o1", coid)})

	if !res.Clean {
		t.Fatalf("a foreign order is not ours and does not make our sweep "+
			"dirty: %+v", res.StillResting)
	}
	if len(deleted) != 1 || deleted[0] != "o1" {
		t.Fatalf("only our own order may be cancelled, deleted: %v", deleted)
	}
}

// TestRefusedCancelIsNotCountedAsSent is why `CancelResult` grew a `Sent` field.
//
// A guarded refusal and a lost answer both arrive as `CancelUnknown` with a
// non-nil `Err`, and they lead to the same conclusion about the ORDER -- it is
// still resting -- for opposite reasons. There, we do not know; here, we know
// nothing was transmitted.
//
// The difference matters to the dispatcher rather than to the risk model. A
// refusal consumed no write capacity, so its token is refunded; an ambiguous
// cancel really did use one. Without `Sent` the two are indistinguishable, and
// a read-only rehearsal would burn its whole write budget on requests it never
// made.
func TestRefusedCancelIsNotCountedAsSent(t *testing.T) {
	t.Run("refused", func(t *testing.T) {
		inner := &scriptedDoer{t: t, handle: func(int, Request) (Response, error) {
			t.Fatal("a refused cancel reached the transport")
			return Response{}, nil
		}}
		g, err := NewWriteGuard(inner, WriteArm{}) // read-only
		if err != nil {
			t.Fatalf("NewWriteGuard: %v", err)
		}

		res := NewClient(g).Cancel(context.Background(), "EX-1")

		if res.Sent {
			t.Fatal("a refused cancel reports Sent; the dispatcher would not " +
				"refund its capacity token for a request that never happened")
		}
		if res.Outcome != CancelUnknown {
			t.Fatalf("outcome is %s, want unknown: the order is untouched and "+
				"still resting, so it must stay live in the risk model",
				res.Outcome)
		}
		var refused *WriteRefused
		if !errors.As(res.Err, &refused) {
			t.Fatalf("err is %v, want WriteRefused", res.Err)
		}
		if len(inner.Calls()) != 0 {
			t.Fatalf("%d requests reached the transport", len(inner.Calls()))
		}
	})

	t.Run("ambiguous is sent", func(t *testing.T) {
		// The contrast that gives the assertion above its meaning: an error
		// AFTER the request left must still read as sent.
		inner := &scriptedDoer{t: t, handle: func(int, Request) (Response, error) {
			return Response{}, errors.New("connection reset")
		}}
		res := NewClient(inner).Cancel(context.Background(), "EX-1")

		if !res.Sent {
			t.Fatal("an ambiguous cancel reports Sent=false; its capacity " +
				"token would be refunded for a DELETE that may have executed")
		}
		if res.Outcome != CancelUnknown {
			t.Fatalf("outcome is %s, want unknown", res.Outcome)
		}
	})
}
