package rest

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"lip/harness/num"
	"lip/harness/quote"
	"lip/harness/risk"
)

// The read half of H-CO-1 must be exact. A price is money, and going through
// float64 loses it: 0.58 is not representable, and truncating 0.58*10000 is how
// a 58c order reads back as 57c.
func TestParsePrice4IsExactForEveryCent(t *testing.T) {
	for cents := 0; cents <= 100; cents++ {
		s := PriceWire(cents)
		got, err := ParsePrice4(s)
		if err != nil {
			t.Fatalf("%dc formatted as %q: %v", cents, s, err)
		}
		if want := int64(cents) * 100; got != want {
			t.Fatalf("%dc -> %q -> %d/1e4, want %d", cents, s, got, want)
		}
		back, exact := CentsExact(got)
		if !exact || back != cents {
			t.Fatalf("%dc did not round-trip: exact=%v back=%d", cents, exact, back)
		}
	}
}

func TestParsePrice4Grammar(t *testing.T) {
	ok := map[string]int64{
		"0.5800": 5800, "0.58": 5800, "1.0000": 10000, "1": 10000,
		"0": 0, "0.0001": 1, ".5": 5000, "0.5801": 5801,
	}
	for in, want := range ok {
		got, err := ParsePrice4(in)
		if err != nil {
			t.Errorf("%q: unexpected error %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("%q -> %d, want %d", in, got, want)
		}
	}
	// Go literal syntax is strictly wider than the wire. Each of these is a
	// string we do not understand, and accepting it turns it into a confident
	// price.
	for _, bad := range []string{
		"1e3", "1_000.00", "0x1p+10", "0.58abc", "abc", "", " 0.58",
		"0.58 ", "-0.58", "+0.58", "0.58001", "1.2.3", "NaN", "Inf",
	} {
		if v, err := ParsePrice4(bad); err == nil {
			t.Errorf("%q parsed as %d; the wire never emits this", bad, v)
		}
	}
}

// H-CO-3a: assert integer-cent resting prices, do not assume them. A violation
// is SEV2 BOOK_PRICE_GRANULARITY, not a rounding.
func TestFractionalRestingPricePingsRatherThanRounds(t *testing.T) {
	rec := map[string]any{
		"order_id":          "o1",
		"client_order_id":   "lipH-run1-000-yes-00000001",
		"ticker":            "T1",
		"book_side":         "bid",
		"yes_price_dollars": "0.5850",
		"remaining_count":   "1.00",
	}
	raw, _ := json.Marshal(rec)
	o, anoms, err := decodeOrder(raw)
	if err != nil {
		t.Fatalf("a fractional price must be reported, not rejected: %v", err)
	}
	if !o.Fractional {
		t.Fatal("5850/1e4 is not an integer cent and must be flagged")
	}
	if o.Price4 != 5850 {
		t.Fatalf("the exact price must survive: got %d", o.Price4)
	}
	if len(anoms) != 1 || anoms[0].Class != "BOOK_PRICE_GRANULARITY" ||
		anoms[0].Sev != risk.SEV2 {
		t.Fatalf("want one SEV2 BOOK_PRICE_GRANULARITY, got %+v", anoms)
	}
	if anoms[0].Ticker != "T1" {
		t.Fatalf("the anomaly must name the market that goes to REDUCING, got %q",
			anoms[0].Ticker)
	}
}

// The read-side counterpart of M10. `book_side: ask` is a NO bid, and its price
// is the NO price -- not the YES price, and not the YES price relabelled.
// Getting this backwards adopts a position with the wrong sign.
func TestBookSideIsAuthoritativeOverSide(t *testing.T) {
	// A NO bid at 42c. The wire calls it an ask at a YES price of 58c, and the
	// `side` field here deliberately disagrees with `book_side`.
	rec := map[string]any{
		"order_id":          "o1",
		"client_order_id":   "lipH-run1-000-no-00000007",
		"ticker":            "T1",
		"book_side":         "ask",
		"side":              "yes", // the lie book_side must override
		"yes_price_dollars": "0.5800",
		"no_price_dollars":  "0.4200",
		"remaining_count":   "3.00",
	}
	raw, _ := json.Marshal(rec)
	o, _, err := decodeOrder(raw)
	if err != nil {
		t.Fatal(err)
	}
	if o.Side != quote.SideNo {
		t.Fatalf("book_side=ask is a NO bid whatever `side` claims; got %v", o.Side)
	}
	if o.PriceCents != 42 {
		t.Fatalf("a NO bid at 42c must read back as 42c, got %dc — reading the "+
			"YES leg here adopts the position with the wrong sign", o.PriceCents)
	}
	if !o.Ours || o.Parsed.Side != quote.SideNo || o.Parsed.Seq != 7 {
		t.Fatalf("the coid must be parsed for adoption: %+v ours=%v", o.Parsed, o.Ours)
	}
	if o.Remaining != num.QtyFromFloat(3) {
		t.Fatalf("remaining %v, want 3.00", o.Remaining)
	}
}

// If the two price fields ever stop being complements, every price this package
// returns is suspect. Say so rather than pick one.
func TestNonComplementaryPricesAreRefused(t *testing.T) {
	rec := map[string]any{
		"order_id":          "o1",
		"ticker":            "T1",
		"book_side":         "bid",
		"yes_price_dollars": "0.5800",
		"no_price_dollars":  "0.4300", // 58 + 43 = 101
		"remaining_count":   "1.00",
	}
	raw, _ := json.Marshal(rec)
	if _, _, err := decodeOrder(raw); err == nil {
		t.Fatal("yes+no must sum to exactly $1.00; a contradiction must not be " +
			"resolved by preferring one field")
	}
}

// Only one price field present: H-CO-1 fills in the other.
func TestSinglePriceFieldIsTransformed(t *testing.T) {
	rec := map[string]any{
		"order_id":          "o1",
		"ticker":            "T1",
		"book_side":         "ask", // a NO bid
		"yes_price_dollars": "0.5800",
		"remaining_count":   "1.00",
	}
	raw, _ := json.Marshal(rec)
	o, _, err := decodeOrder(raw)
	if err != nil {
		t.Fatal(err)
	}
	if o.PriceCents != 42 {
		t.Fatalf("a NO bid with only a YES price of 58c is 42c, got %dc", o.PriceCents)
	}
}

// A misspelled status is refused before it reaches the wire, because on the
// wire it is indistinguishable from "no orders rest".
func TestOrdersRefusesAMisspelledStatusWithoutSending(t *testing.T) {
	d := &scriptedDoer{t: t, handle: func(n int, req Request) (Response, error) {
		t.Fatalf("a misspelled status must not be sent; got %+v", req)
		return Response{}, nil
	}}
	r := NewClient(d).Orders(context.Background(), "T1", "cancelled")
	if r.Replaces() {
		t.Fatal("a refused status must not produce a replaceable result")
	}
	if len(d.Calls()) != 0 {
		t.Fatalf("want 0 requests, got %d", len(d.Calls()))
	}
}

// H-POS-4: an incomplete response tells us nothing and must not retire an order
// from our model.
func TestOrdersIncompleteWalkYieldsNothing(t *testing.T) {
	d := &scriptedDoer{t: t, handle: func(n int, req Request) (Response, error) {
		if n == 0 {
			return jsonPage(EpOrders, "C2", map[string][]any{
				"orders": {order("o1", "lipH-run1-000-yes-00000001", "T1", "yes", 0.58, "1.00")},
			}), nil
		}
		return Response{Status: 500, Body: []byte(`{}`)}, nil
	}}
	r := NewClient(d).Orders(context.Background(), "T1", StatusResting)
	if r.Replaces() {
		t.Fatal("a walk that failed at page two must not replace the order map")
	}
	if len(r.Orders) != 0 {
		t.Fatalf("an incomplete walk produced %d orders", len(r.Orders))
	}
}

// A resting order we cannot parse is resting size we would otherwise believe is
// absent. H-FAIL-3: "off" means exchange-confirmed absent, not unreadable.
func TestOneUndecodableOrderInvalidatesTheWalk(t *testing.T) {
	d := &scriptedDoer{t: t, handle: func(n int, req Request) (Response, error) {
		return jsonPage(EpOrders, "", map[string][]any{
			"orders": {
				order("o1", "lipH-run1-000-yes-00000001", "T1", "yes", 0.58, "1.00"),
				map[string]any{"order_id": "o2", "ticker": "T1", "book_side": "bid",
					"yes_price_dollars": "0.5800"}, // no remaining count
			},
		}), nil
	}}
	r := NewClient(d).Orders(context.Background(), "T1", StatusResting)
	if r.Replaces() {
		t.Fatal("an undecodable order must invalidate the whole walk, not be skipped")
	}
	if !strings.Contains(r.Err.Error(), "remaining") {
		t.Fatalf("the error must name the missing field: %v", r.Err)
	}
}

func TestOrdersClassifiesForeign(t *testing.T) {
	d := &scriptedDoer{t: t, handle: func(n int, req Request) (Response, error) {
		return jsonPage(EpOrders, "", map[string][]any{
			"orders": {
				order("o1", "lipH-run1-000-yes-00000001", "T1", "yes", 0.58, "1.00"),
				order("o2", "someone-elses-order", "T1", "no", 0.41, "2.00"),
			},
		}), nil
	}}
	r := NewClient(d).Orders(context.Background(), "", StatusResting)
	if !r.Replaces() {
		t.Fatalf("want a complete walk: %v", r.Err)
	}
	if len(r.Ours()) != 1 || r.Ours()[0].OrderID != "o1" {
		t.Fatalf("ours: %+v", r.Ours())
	}
	if len(r.Foreign()) != 1 || r.Foreign()[0].OrderID != "o2" {
		t.Fatalf("foreign: %+v", r.Foreign())
	}
}

// H-ORD-5 step 1 / H-ORD-5b: positions are read for EVERY market, including
// markets not in the selection set, so the read takes no ticker filter.
func TestPositionsAreUnfilteredAndSigned(t *testing.T) {
	d := &scriptedDoer{t: t, handle: func(n int, req Request) (Response, error) {
		if req.Query.Get("ticker") != "" {
			t.Fatalf("positions must not be filtered by ticker: the managed set " +
				"is the union of selected markets and every market we hold")
		}
		return jsonPage(EpPositions, "", map[string][]any{
			"market_positions": {
				map[string]any{"ticker": "T1", "position_fp": "3.50"},
				map[string]any{"ticker": "T2", "position_fp": "-2.00"},
			},
		}), nil
	}}
	r := NewClient(d).Positions(context.Background())
	if !r.Replaces() {
		t.Fatalf("want a complete walk: %v", r.Err)
	}
	if got := r.ByTicker["T1"]; got != num.QtyFromFloat(3.5) {
		t.Fatalf("T1 q = %v, want 3.50", got)
	}
	if got := r.ByTicker["T2"]; got != num.QtyFromFloat(-2) {
		t.Fatalf("T2 q = %v, want -2.00 (q is YES-positive and signed)", got)
	}
}

// Defaulting an unparseable position to zero reports a held market as flat, and
// flat is the answer that lets REDUCING reach IDLE and lets the process exit.
func TestUnparseablePositionIsNotZero(t *testing.T) {
	d := &scriptedDoer{t: t, handle: func(n int, req Request) (Response, error) {
		return jsonPage(EpPositions, "", map[string][]any{
			"market_positions": {map[string]any{"ticker": "T1", "position_fp": "NaN"}},
		}), nil
	}}
	r := NewClient(d).Positions(context.Background())
	if r.Replaces() {
		t.Fatal("an unparseable position must not produce a usable result; " +
			"quantizing it to zero abandons the position")
	}
	if _, ok := r.ByTicker["T1"]; ok {
		t.Fatal("no position may be reported at all")
	}
}

// H-ORD-8's taker detector is the cheapest detector for the most expensive bug.
// It must not fail open.
func TestFillWithoutIsTakerIsRefused(t *testing.T) {
	f := fill("f1", "t1", "o1", "T1", false)
	delete(f, "is_taker")
	d := &scriptedDoer{t: t, handle: func(n int, req Request) (Response, error) {
		return jsonPage(EpFills, "", map[string][]any{"fills": {f}}), nil
	}}
	r := NewClient(d).Fills(context.Background(), "", time.Time{})
	if r.Replaces() {
		t.Fatal("a fill with no is_taker leaves H-ORD-8 unevaluable; absence " +
			"must not read as false")
	}
}

// F4 — `is_taker: null` is NOT false.
//
// `json.Unmarshal([]byte("null"), &someBool)` succeeds and leaves the bool at
// false, because "unmarshaling a JSON null into any other Go type has no effect
// on the value and produces no error". H-ORD-8's taker detector must never fail
// open: a null read as false hides continued taker execution until the account
// is exhausted. Distinct from the absent-field case above, and the reason the
// field is decoded through a pointer.
func TestNullIsTakerIsRefused(t *testing.T) {
	f := fill("f1", "t1", "o1", "T1", false)
	f["is_taker"] = nil
	d := &scriptedDoer{t: t, handle: func(n int, req Request) (Response, error) {
		return jsonPage(EpFills, "", map[string][]any{"fills": {f}}), nil
	}}
	r := NewClient(d).Fills(context.Background(), "", time.Time{})
	if r.Replaces() {
		t.Fatal("a null is_taker was accepted; null is not false, and reading " +
			"it as false disables H-ORD-8's detector silently")
	}
	if len(r.Takers()) != 0 {
		t.Fatal("no fill may be reported at all from an invalidated walk")
	}
}

// F4 — `balance: null` is NOT zero. A zero balance would leave a held position
// with no funded reducer (H-CAP-8).
func TestNullBalanceIsRefused(t *testing.T) {
	d := &scriptedDoer{t: t, handle: func(n int, req Request) (Response, error) {
		return Response{Status: 200, Body: []byte(`{"balance":null}`)}, nil
	}}
	b, err := NewClient(d).Balance(context.Background())
	if err == nil {
		t.Fatalf("a null balance was accepted as %v; null is not zero", b.Cents)
	}
	if b.Cents != 0 {
		t.Fatalf("a refused balance must carry no value, got %d", b.Cents)
	}
}

func TestFillsCarryBothIdentitiesAndDetectTakers(t *testing.T) {
	d := &scriptedDoer{t: t, handle: func(n int, req Request) (Response, error) {
		return jsonPage(EpFills, "", map[string][]any{
			"fills": {
				fill("f1", "t1", "o1", "T1", false),
				fill("f2", "t2", "o2", "T1", true),
			},
		}), nil
	}}
	r := NewClient(d).Fills(context.Background(), "", time.Time{})
	if !r.Replaces() {
		t.Fatalf("want a complete walk: %v", r.Err)
	}
	if len(r.Fills) != 2 {
		t.Fatalf("want 2 fills, got %d", len(r.Fills))
	}
	// trade_id is the join key against rig.db (H-ORD-6); fill_id is the
	// distinct second identity the walk pages on.
	if r.Fills[0].TradeID != "t1" || r.Fills[0].FillID != "f1" {
		t.Fatalf("both identities must survive: %+v", r.Fills[0])
	}
	takers := r.Takers()
	if len(takers) != 1 || takers[0].TradeID != "t2" {
		t.Fatalf("want exactly the taker fill, got %+v", takers)
	}
}

// A fill with no trade_id cannot be attributed at all: H-ORD-6 makes it
// our_fill's primary key and the join against rig.db.
func TestFillWithoutTradeIDIsRefused(t *testing.T) {
	f := fill("f1", "", "o1", "T1", false)
	delete(f, "trade_id")
	d := &scriptedDoer{t: t, handle: func(n int, req Request) (Response, error) {
		return jsonPage(EpFills, "", map[string][]any{"fills": {f}}), nil
	}}
	r := NewClient(d).Fills(context.Background(), "", time.Time{})
	if r.Replaces() {
		t.Fatal("a fill without trade_id must invalidate the walk")
	}
}

// The backfill window is applied AFTER a complete walk. Stopping the walk early
// on a timestamp would make it incomplete, and only a complete walk may replace
// state.
func TestFillsTimeFilterIsAppliedAfterTheCompleteWalk(t *testing.T) {
	old := fill("f1", "t1", "o1", "T1", false)
	old["ts"] = "1000000000" // seconds, well before the cutoff
	recent := fill("f2", "t2", "o2", "T1", false)
	recent["ts"] = "2000000000"

	pages := 0
	d := &scriptedDoer{t: t, handle: func(n int, req Request) (Response, error) {
		pages++
		switch n {
		case 0:
			return jsonPage(EpFills, "C2", map[string][]any{"fills": {recent}}), nil
		case 1:
			return jsonPage(EpFills, "", map[string][]any{"fills": {old}}), nil
		}
		t.Fatalf("unexpected call %d", n)
		return Response{}, nil
	}}

	since := time.Unix(1_500_000_000, 0)
	r := NewClient(d).Fills(context.Background(), "", since)
	if !r.Replaces() {
		t.Fatalf("want a complete walk: %v", r.Err)
	}
	if pages != 2 {
		t.Fatalf("the walk must run to exhaustion before filtering; %d pages", pages)
	}
	if len(r.Fills) != 1 || r.Fills[0].TradeID != "t2" {
		t.Fatalf("want only the fill at or after `since`, got %+v", r.Fills)
	}

	// The IDENTITIES of the excluded records survive the filter, and that is
	// what makes a sound history boundary possible at all. §7.5 asks for
	// `backfill_h`; the live poll asks for everything with a zero `since`. A
	// consumer holding only the filtered slice cannot tell "this trade is new"
	// from "this trade is older than the window startup happened to ask for",
	// and on a rung that stops at its first live fill those two answers are a
	// working harness and one that latches on every restart.
	want := []string{"t2", "t1"}
	if len(r.AllTradeIDs) != len(want) {
		t.Fatalf("AllTradeIDs is %v, want %v: it is taken from the complete "+
			"walk BEFORE the time filter", r.AllTradeIDs, want)
	}
	for i, id := range want {
		if r.AllTradeIDs[i] != id {
			t.Fatalf("AllTradeIDs is %v, want %v in walk order",
				r.AllTradeIDs, want)
		}
	}
}

func TestBalanceIsCents(t *testing.T) {
	d := &scriptedDoer{t: t, handle: func(n int, req Request) (Response, error) {
		if req.Path != "/portfolio/balance" || req.Method != "GET" {
			t.Fatalf("unexpected request %+v", req)
		}
		return Response{Status: 200, Body: []byte(`{"balance":10000}`)}, nil
	}}
	b, err := NewClient(d).Balance(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if b.Cents != 10000 {
		t.Fatalf("want 10000 cents ($100.00, the pilot capital), got %d", b.Cents)
	}
}

func TestBalanceMissingFieldIsAnError(t *testing.T) {
	d := &scriptedDoer{t: t, handle: func(n int, req Request) (Response, error) {
		return Response{Status: 200, Body: []byte(`{}`)}, nil
	}}
	if b, err := NewClient(d).Balance(context.Background()); err == nil {
		t.Fatalf("an absent balance must not read as $0.00, got %+v", b)
	}
}

// scalar must treat a JSON number and a JSON string alike, and must treat null
// as absent rather than as a zero.
// `feed.Universe` sends `limit=200`, reads one page, and never looks at
// `next_cursor`. Its answer is therefore silently truncated the moment the
// active set exceeds a page -- and a Target Size read from a truncated universe
// does not come back wrong, it comes back ABSENT, which this harness treats as
// "not in the active programme" and refuses to start on.
//
// So the thing to assert is that a ticker on page TWO is found.
func TestProgramsWalksEveryPageAndParsesTargetSize(t *testing.T) {
	d := &scriptedDoer{t: t, handle: func(n int, req Request) (Response, error) {
		if req.Query.Get("status") != "active" {
			t.Fatalf("page %d dropped the status=active filter: %v", n+1, req.Query)
		}
		switch n {
		case 0:
			if req.Query.Get("cursor") != "" {
				t.Fatalf("the first page must not carry a cursor: %v", req.Query)
			}
			return jsonPage(EpPrograms, "PAGE2", map[string][]any{
				"incentive_programs": {
					map[string]any{"market_ticker": "T1", "target_size_fp": "1000.00"},
				},
			}), nil
		case 1:
			// Clause 1: the cursor is used exactly as received.
			if got := req.Query.Get("cursor"); got != "PAGE2" {
				t.Fatalf("page 2 sent cursor %q, want %q", got, "PAGE2")
			}
			return jsonPage(EpPrograms, "", map[string][]any{
				"incentive_programs": {
					map[string]any{"market_ticker": "T2", "target_size_fp": "250.50"},
				},
			}), nil
		}
		t.Fatalf("unexpected page %d", n+1)
		return Response{}, nil
	}}
	r := NewClient(d).Programs(context.Background())
	if !r.Replaces() {
		t.Fatalf("want a complete walk: %v", r.Err)
	}
	if got := r.ByTarget["T1"]; got != 1000 {
		t.Fatalf("T1 target = %v, want 1000", got)
	}
	if got, ok := r.ByTarget["T2"]; !ok || got != 250.5 {
		t.Fatalf("T2 target = %v (present=%v), want 250.50; a market on the "+
			"second page is exactly what a single-page read loses, and losing it "+
			"reads as 'not in the active programme'", got, ok)
	}
}

// A programme with no `target_size_fp` cannot be defaulted. Zero makes
// `Qualifies()` true for every interval, and any positive guess makes it true
// or false for reasons the exchange did not state.
func TestProgramWithoutATargetSizeIsRefused(t *testing.T) {
	d := &scriptedDoer{t: t, handle: func(n int, req Request) (Response, error) {
		return jsonPage(EpPrograms, "", map[string][]any{
			"incentive_programs": {map[string]any{"market_ticker": "T1"}},
		}), nil
	}}
	r := NewClient(d).Programs(context.Background())
	if r.Replaces() {
		t.Fatal("a programme with no target_size_fp must not produce a usable " +
			"universe")
	}
	if _, ok := r.ByTarget["T1"]; ok {
		t.Fatal("no target may be reported at all")
	}
}

func TestScalarHandlesBothWireShapes(t *testing.T) {
	cases := map[string]string{
		`"1.00"`: "1.00", `1.00`: "1.00", `1`: "1", `null`: "", `""`: "",
	}
	for in, want := range cases {
		if got := scalar(json.RawMessage(in)); got != want {
			t.Errorf("scalar(%s) = %q, want %q", in, got, want)
		}
	}
	if got := scalar(nil); got != "" {
		t.Errorf("scalar(nil) = %q", got)
	}
}

// TestParseFee6AcceptsEveryMeasuredFee is `lip-9tr`'s regression test, and the
// values are not invented: they are every distinct `fee_cost` on the real
// account, captured by `go/cmd/conform` on 2026-08-12 and pasted verbatim.
//
// The defect this pins was not that a parser was too strict. It was that the
// PRICE parser was applied to a FEE field, and the two have different measured
// quanta -- prices four decimals, fees six. Every one of these values was
// refused by ParsePrice4, which discarded every fills walk on the account and
// took the harness to a durable WINDING_DOWN 2.3 seconds after launch.
//
// Note "0.000000". The zero fee is refused by ParsePrice4 too: the refusal is
// on WIDTH, not magnitude, so there was no subset of this account's history on
// which the old code succeeded. A fixture of "0.00" -- which is what the tests
// had -- could never have found this.
func TestParseFee6AcceptsEveryMeasuredFee(t *testing.T) {
	measured := []struct {
		wire string
		want num.Money
	}{
		{"0.000000", 0},
		{"0.017200", 17200},
		{"0.017300", 17300},
		{"0.052500", 52500},
		{"0.140000", 140000},
		{"0.396800", 396800},
		{"0.431200", 431200},
		{"0.437400", 437400},
	}
	for _, c := range measured {
		got, err := ParseFee6(c.wire)
		if err != nil {
			t.Fatalf("ParseFee6(%q) refused a fee the exchange really sent: %v\n\n"+
				"Every fee_cost on the account carries exactly six decimals. A "+
				"parser that cannot read them discards the whole fills walk, "+
				"which is lip-9tr.", c.wire, err)
		}
		if got != c.want {
			t.Fatalf("ParseFee6(%q) = %d, want %d micro-dollars.\n\n"+
				"Money is 1e-6 USD (num/money.go:8-24), so a six-decimal dollar "+
				"string maps to it digit for digit with no scaling.",
				c.wire, got, c.want)
		}
	}
}

// The parser must stay exact rather than merely close: these are the cases a
// float64 round-trip gets wrong, and a fee feeds H-ORD-8's `Fee > 0` test.
func TestParseFee6IsExactAndRefusesUnmeasuredShapes(t *testing.T) {
	exact := map[string]num.Money{
		"0":         0,
		"0.0":       0,
		"1":         1_000_000,
		"1.5":       1_500_000,
		"0.1":       100_000,
		"0.000001":  1,
		"0.100000":  100_000,
		"12.345678": 12_345_678,
	}
	for wire, want := range exact {
		got, err := ParseFee6(wire)
		if err != nil {
			t.Fatalf("ParseFee6(%q): %v", wire, err)
		}
		if got != want {
			t.Fatalf("ParseFee6(%q) = %d, want %d", wire, got, want)
		}
	}

	// Seven decimals is a finer quantum than the one measured. It must surface
	// as an error rather than round, for exactly the reason ParsePrice4 gives:
	// truncating discards precision the exchange chose to send.
	for _, bad := range []string{
		"0.0000001", // finer than 1e-6
		"-0.017200", // signed: a rebate is a wire change, not a rounding
		"+0.017200",
		"",
		"abc",
		"0.01.72",
		"1e-3", // Go literal syntax the wire does not emit
	} {
		if got, err := ParseFee6(bad); err == nil {
			t.Fatalf("ParseFee6(%q) = %d, want a refusal.\n\n"+
				"A shape we have not measured must surface rather than be "+
				"turned into a confident number.", bad, got)
		}
	}
}

// The whole point of giving fees their own parser was to leave the PRICE
// assertion standing. If ParsePrice4 ever starts accepting six decimals, the
// fix for lip-9tr has been undone by widening rather than by separating, and a
// genuine tick-size change would stop being detected.
func TestParsePrice4StillRefusesTheFeeQuantum(t *testing.T) {
	for _, s := range []string{"0.000000", "0.017200", "0.437400"} {
		if got, err := ParsePrice4(s); err == nil {
			t.Fatalf("ParsePrice4(%q) = %d, want a refusal.\n\n"+
				"Prices are measured at 1e-4 and fees at 1e-6. Collapsing the "+
				"two parsers destroys the assertion that makes ParsePrice4 "+
				"useful: it is what would detect the exchange changing its "+
				"price quantum (H-CO-3a).", s, got)
		}
	}
}
