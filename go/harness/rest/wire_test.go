package rest

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"lip/harness/cfg"
	"lip/harness/num"
	"lip/harness/quote"
)

func qty(c float64) num.Qty {
	half := 0.5
	if c < 0 {
		half = -0.5
	}
	return num.Qty(int64(c*num.QtyScale + half))
}

// TestTransformAllNinetyNinePrices is V1.1.
//
// "Fails if a NO bid at 42c does not encode as {"side":"ask","price":"0.5800"}."
//
// Every price, both sides, and the round trip. M10 -- reverse the H-CO-1
// transform for the NO side -- is the mutation this must catch, and it is worth
// being precise about why it is dangerous rather than merely wrong: a reversed
// NO transform sends a YES ask at 42c when we meant 58c. That is an order 16
// ticks INSIDE the touch on the wrong side of the book, which post_only will
// reject if it would cross and will otherwise rest at a price we did not
// choose. Both outcomes are bad and only one of them is loud.
func TestTransformAllNinetyNinePrices(t *testing.T) {
	// The named example, exactly as §4 and kalshi.py state it.
	ws, yes, err := ToWire(quote.SideNo, 42)
	if err != nil {
		t.Fatalf("ToWire(no, 42c): %v", err)
	}
	if ws != Ask || yes != 58 {
		t.Fatalf("a NO bid at 42c encoded as {%q, %dc}, want {\"ask\", 58c}: "+
			"BookSide is the YES leg only, so buying NO at 42c is selling YES "+
			"at 58c", ws, yes)
	}
	if got := PriceWire(yes); got != "0.5800" {
		t.Fatalf("price formatted as %q, want \"0.5800\"", got)
	}

	for p := quote.MinPrice; p <= quote.MaxPrice; p++ {
		// YES side: identity.
		ws, yes, err := ToWire(quote.SideYes, p)
		if err != nil {
			t.Fatalf("ToWire(yes, %dc): %v", p, err)
		}
		if ws != Bid || yes != p {
			t.Fatalf("a YES bid at %dc encoded as {%q, %dc}, want {\"bid\", %dc}",
				p, ws, yes, p)
		}

		// NO side: the complement.
		ws, yes, err = ToWire(quote.SideNo, p)
		if err != nil {
			t.Fatalf("ToWire(no, %dc): %v", p, err)
		}
		if ws != Ask {
			t.Fatalf("a NO bid at %dc encoded as side %q, want \"ask\"", p, ws)
		}
		if yes != 100-p {
			t.Fatalf("a NO bid at %dc encoded as YES %dc, want %dc: a NO bid "+
				"at p IS a YES ask at 100 − p", p, yes, 100-p)
		}

		// Round trip, both sides. This is the half that catches a transform
		// which is self-consistent and wrong.
		for _, side := range []quote.Side{quote.SideYes, quote.SideNo} {
			ws, yes, err := ToWire(side, p)
			if err != nil {
				t.Fatal(err)
			}
			backSide, backPrice, err := FromWire(ws, yes)
			if err != nil {
				t.Fatalf("FromWire(%q, %dc): %v", ws, yes, err)
			}
			if backSide != side || backPrice != p {
				t.Fatalf("%s at %dc round-tripped to %s at %dc: §7.5's startup "+
					"adoption reads orders back off the exchange, and a "+
					"reconciliation that mistook a NO bid at 42c for a YES bid "+
					"at 58c would adopt the position with the wrong sign",
					side, p, backSide, backPrice)
			}
		}
	}
}

// TestTransformRejectsUntradablePrices.
func TestTransformRejectsUntradablePrices(t *testing.T) {
	for _, p := range []int{-1, 0, 100, 101} {
		for _, side := range []quote.Side{quote.SideYes, quote.SideNo} {
			if _, _, err := ToWire(side, p); err == nil {
				t.Errorf("ToWire(%s, %dc) was accepted", side, p)
			}
		}
		if _, _, err := FromWire(Bid, p); err == nil {
			t.Errorf("FromWire(bid, %dc) was accepted", p)
		}
	}
	if _, _, err := FromWire("buy", 50); err == nil {
		t.Error("FromWire accepted an unknown wire side")
	}
}

// TestCreateOrderPayloadIsByteForByte is V1.2.
//
// "Fails if count/price become JSON numbers, or .2f/.4f formatting drifts."
//
// The expectation is a literal JSON string, not a struct comparison, because
// the failure being guarded is in the ENCODING and a struct comparison happens
// on the near side of it. `{"count": 12.0}` and `{"count": "12.00"}` are the
// same Go value under two sets of tags, and only one of them is a valid order.
func TestCreateOrderPayloadIsByteForByte(t *testing.T) {
	const coid = "lipH-01JQRUN-042-no-00000007"
	body, err := NewCreateOrder("KXTEST-26AUG05-A", quote.SideNo, 42,
		qty(12), qty(12), coid)
	if err != nil {
		t.Fatalf("NewCreateOrder: %v", err)
	}

	raw := capturePayload(t, body)

	want := `{"ticker":"KXTEST-26AUG05-A","side":"ask","count":"12.00",` +
		`"price":"0.5800","time_in_force":"good_till_canceled",` +
		`"self_trade_prevention_type":"taker_at_cross","post_only":true,` +
		`"client_order_id":"` + coid + `"}`

	if got := string(raw); got != want {
		t.Fatalf("payload drift.\n got: %s\nwant: %s", got, want)
	}

	// The two encodings that are schema violations, named individually so a
	// failure says which one happened.
	s := string(raw)
	if strings.Contains(s, `"count":12`) || strings.Contains(s, `"count":1`) {
		t.Error(`count was sent as a JSON number: H-CO-2 makes count and price ` +
			`fixed-point STRINGS, and sending numbers is a schema violation`)
	}
	if strings.Contains(s, `"price":0`) {
		t.Error(`price was sent as a JSON number`)
	}
}

// capturePayload returns the bytes Client.Create actually put on the wire.
//
// V1.2 is asserted HERE and not against `json.Marshal(CreateOrder)`, because
// the public type is no longer the wire body and marshalling it would be
// testing a shape that is never sent. The oracle has to be the real send path:
// that is exactly where `body.PostOnly = false` survived 68 tests.
func capturePayload(t *testing.T, body CreateOrder) []byte {
	t.Helper()
	var sent []byte
	d := &scriptedDoer{t: t, handle: func(n int, req Request) (Response, error) {
		if req.Method == "POST" {
			sent = req.Body
			return Response{Status: 200, Body: ackBody("o1",
				body.ClientOrderID(), body.Count().Wire(), "0.00")}, nil
		}
		t.Fatalf("unexpected %s %s", req.Method, req.Path)
		return Response{}, nil
	}}
	res := NewClient(d).Create(context.Background(), body, cfg.Default())
	if res.Outcome != CreateAcked {
		t.Fatalf("create did not ack: %s (%v)", res.Outcome, res.Err)
	}
	if sent == nil {
		t.Fatal("nothing was sent")
	}
	return sent
}

// TestPostOnlyIsStructural is H-Q-3 and A1.
//
// "post_only: true on every order the harness ever sends, with no exception, no
// flag, and no code path that sets it false."
//
// The assertion is made against the BYTES ON THE WIRE, over every order this
// package can construct. The previous version of this test read the fields of
// the struct returned by the constructor, which proved only that the
// constructor was right -- and the constructor was never the problem.
// `body.PostOnly = false` inside `Create` compiled and passed all 68 tests.
//
// The public type now has no post_only, STP or time-in-force field at all, so
// that particular mutation no longer compiles. Its permanent compiling
// replacement (M5a) mutates the private wire value after validation and
// immediately before marshal, and this test is the oracle that catches it.
func TestPostOnlyIsStructural(t *testing.T) {
	for p := quote.MinPrice; p <= quote.MaxPrice; p++ {
		for _, side := range []quote.Side{quote.SideYes, quote.SideNo} {
			body, err := NewCreateOrder("T", side, p, qty(1), 0,
				canonicalCoid(t, side, 1))
			if err != nil {
				t.Fatalf("NewCreateOrder(%s, %dc): %v", side, p, err)
			}
			var w createOrderWire
			if err := json.Unmarshal(capturePayload(t, body), &w); err != nil {
				t.Fatal(err)
			}
			if !w.PostOnly {
				t.Fatalf("post_only was false on the wire for %s at %dc: A1 "+
					"asserts it over every order ever sent, and A2 over every "+
					"fill -- no fill of ours ever has is_taker == true or "+
					"fee_cost > 0", side, p)
			}
			if w.SelfTradePrevention != "taker_at_cross" {
				t.Fatalf("self_trade_prevention_type = %q, want "+
					"\"taker_at_cross\": `maker` cancels our RESTING order on "+
					"a self-match and silently destroys the scoring presence "+
					"we are paid for (H-CO-5)", w.SelfTradePrevention)
			}
			if w.TimeInForce != "good_till_canceled" {
				t.Fatalf("time_in_force = %q", w.TimeInForce)
			}
		}
	}
}

// canonicalCoid builds a well-formed coid for `side`.
func canonicalCoid(t *testing.T, side quote.Side, seq uint64) string {
	t.Helper()
	c, err := Coid("01JQRUN", 42, side, seq)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// TestCreateOrderValidatesTheCountBeforeFormatting is H-CO-4b.
//
// "A formatted count is validated before dispatch: it must be strictly positive
// and no greater than the quantized position it derives from. '0.00' is never
// sent."
//
// The ordering matters. A count validated after formatting has already been
// turned into a string, and the string "0.00" is the one thing H-CO-4b names
// outright -- so the check has to happen on the number.
func TestCreateOrderValidatesTheCountBeforeFormatting(t *testing.T) {
	for _, tc := range []struct {
		name        string
		count       num.Qty
		derivedFrom num.Qty
		why         string
	}{
		{"zero", 0, qty(12), `"0.00" is never sent`},
		{"negative", qty(-1), qty(12), "a negative count is not a bid"},
		{"over its bound", qty(13), qty(12),
			"H-Q-5a caps the reducer at |q|, and a count that exceeds the " +
				"position it derives from is the sign flip A12 forbids"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := NewCreateOrder("T", quote.SideYes, 50,
				tc.count, tc.derivedFrom, canonicalCoid(t, quote.SideYes, 1)); err == nil {
				t.Fatalf("a count of %s against a bound of %s was accepted -- %s",
					tc.count.Wire(), tc.derivedFrom.Wire(), tc.why)
			}
		})
	}

	// A fractional count is ordinary, not exceptional: sizes are fractional in
	// ~20.5% of resting book levels (H-CO-4).
	body, err := NewCreateOrder("T", quote.SideYes, 50, qty(0.07), qty(0.07),
		canonicalCoid(t, quote.SideYes, 1))
	if err != nil {
		t.Fatalf("a 0.07 count was rejected: %v", err)
	}
	// Asserted on the wire, where the formatting actually happens.
	var w createOrderWire
	if err := json.Unmarshal(capturePayload(t, body), &w); err != nil {
		t.Fatal(err)
	}
	if w.Count != "0.07" {
		t.Errorf("count formatted as %q, want \"0.07\"", w.Count)
	}

	// And an empty coid is refused: §7.1 makes the coid deterministic precisely
	// so that an ambiguous create can be identified afterwards (H-ORD-2b).
	if _, err := NewCreateOrder("T", quote.SideYes, 50, qty(1), 0, ""); err == nil {
		t.Error("an order with no client_order_id was accepted")
	}
	if _, err := NewCreateOrder("", quote.SideYes, 50, qty(1), 0,
		canonicalCoid(t, quote.SideYes, 1)); err == nil {
		t.Error("an order with no ticker was accepted")
	}
}

// TestPriceWireIsFourDecimals sweeps the price encoding.
func TestPriceWireIsFourDecimals(t *testing.T) {
	for _, tc := range []struct {
		cents int
		want  string
	}{
		{1, "0.0100"}, {42, "0.4200"}, {58, "0.5800"}, {99, "0.9900"},
		{50, "0.5000"},
	} {
		if got := PriceWire(tc.cents); got != tc.want {
			t.Errorf("PriceWire(%dc) = %q, want %q", tc.cents, got, tc.want)
		}
	}
	for c := quote.MinPrice; c <= quote.MaxPrice; c++ {
		if got := PriceWire(c); len(got) != 6 || got[1] != '.' {
			t.Fatalf("PriceWire(%dc) = %q, want a four-decimal dollar string",
				c, got)
		}
	}
}
