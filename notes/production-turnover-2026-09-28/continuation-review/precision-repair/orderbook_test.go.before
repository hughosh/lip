package rest

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"lip/harness/num"
	"lip/harness/risk"
)

const obTicker = "KXSILVER15M-26AUG071800-00"

// obBody builds the measured response shape: levels under `orderbook_fp`, keys
// `yes_dollars` / `no_dollars`, each a list of [price, size] fixed-point
// STRING pairs, BEST LEVEL LAST (H-CO-3).
func obBody(t *testing.T, yes, no any) []byte {
	t.Helper()
	book := map[string]any{}
	if _, omit := yes.(omitField); !omit {
		book["yes_dollars"] = yes
	}
	if _, omit := no.(omitField); !omit {
		book["no_dollars"] = no
	}
	raw, err := json.Marshal(map[string]any{"orderbook_fp": book})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func obLevels(pairs ...[2]string) []any {
	out := make([]any, 0, len(pairs))
	for _, p := range pairs {
		out = append(out, []string{p[0], p[1]})
	}
	return out
}

// obClient answers one orderbook request with `resp`/`err` and records the
// request it was given.
func obClient(t *testing.T, resp Response, err error) (*Client, *scriptedDoer) {
	t.Helper()
	d := &scriptedDoer{t: t, handle: func(int, Request) (Response, error) {
		return resp, err
	}}
	return NewClient(d), d
}

// TestOrderbookReadsTheMeasuredShapeAndReversesIt is H-CO-3 at the wire.
//
// Three things are asserted together because getting any one of them wrong
// produces a book that PARSES and is not the exchange's:
//
//   - the endpoint. `GET /markets/{ticker}/orderbook` and nothing else; the
//     path is interpolated into the string the signature covers, and
//     `/markets/{ticker}` is a different endpoint that answers 200 with a
//     schedule.
//   - the ORDER. The exchange sends best level LAST, and every walk in this
//     system runs from the touch downwards. A book read in wire order has its
//     far end where its touch should be, and F5 would compare the deepest
//     levels of two books and call them agreement.
//   - the PARSERS. `core.ParsePriceCents` and `core.ParseSize`, which is what
//     the websocket book is built with. A second decoder here would make the
//     cross-check a comparison of decoders.
func TestOrderbookReadsTheMeasuredShapeAndReversesIt(t *testing.T) {
	c, d := obClient(t, Response{Status: 200, Body: obBody(t,
		obLevels([2]string{"0.3900", "5.00"}, [2]string{"0.4000", "20.00"}),
		obLevels([2]string{"0.5400", "7.50"}, [2]string{"0.5500", "12.25"}),
	)}, nil)

	res := c.Orderbook(context.Background(), obTicker)
	if !res.Read() {
		t.Fatalf("outcome = %v, err = %v", res.Outcome, res.Err)
	}

	calls := d.Calls()
	if len(calls) != 1 {
		t.Fatalf("%d requests were made, want exactly 1", len(calls))
	}
	if calls[0].Method != "GET" {
		t.Fatalf("method = %q, want GET", calls[0].Method)
	}
	if want := "/markets/" + obTicker + "/orderbook"; calls[0].Path != want {
		t.Fatalf("path = %q, want %q", calls[0].Path, want)
	}
	if len(calls[0].Query) != 0 {
		t.Fatalf("query = %v; the signature covers the path WITHOUT the query "+
			"string, and this endpoint takes no parameters", calls[0].Query)
	}

	// Best FIRST after the reversal.
	if len(res.Yes) != 2 || res.Yes[0].Cents != 40 || res.Yes[1].Cents != 39 {
		t.Fatalf("yes = %+v, want 40c then 39c: the endpoint sends best LAST "+
			"and H-CO-3 says reverse before use", res.Yes)
	}
	if res.Yes[0].Size != 20 || res.Yes[1].Size != 5 {
		t.Fatalf("yes sizes = %+v, want 20 then 5", res.Yes)
	}
	if len(res.No) != 2 || res.No[0].Cents != 55 || res.No[1].Cents != 54 {
		t.Fatalf("no = %+v, want 55c then 54c", res.No)
	}
	if res.No[0].Size != 12.25 {
		t.Fatalf("no best size = %v, want 12.25; sizes are float64 and "+
			"fractional in ~20.5%% of observed levels (H-CO-4)", res.No[0].Size)
	}

	// And the wire pairs are carried through in the ORDER THE EXCHANGE SENT
	// THEM, because that is the order `core.Levels` would have been built in by
	// the snapshot this book stands in for.
	yes, no := res.Snapshot()
	if len(yes) != 2 || yes[0] != [2]string{"0.3900", "5.00"} ||
		yes[1] != [2]string{"0.4000", "20.00"} {
		t.Fatalf("Snapshot yes = %v, want the wire order back", yes)
	}
	if len(no) != 2 || no[1] != [2]string{"0.5500", "12.25"} {
		t.Fatalf("Snapshot no = %v, want the wire order back", no)
	}
}

// TestOrderbookRefusesEveryShapeItHasNotMeasured is the register `Schedule` and
// `Balance` are in: a non-200 is an ANSWER and not an error, and every field is
// either accounted for or the read fails.
//
// The absent-array cases are the dangerous ones and they are why this is not a
// lenient decoder. An absent `yes_dollars` and an empty one are the same bytes
// to `json.Unmarshal` into a slice, and opposite facts to F5: "there is no
// liquidity on this side" fails the Target Size walk and sends a healthy market
// to REDUCING, and a response we never really read must never be able to say it.
func TestOrderbookRefusesEveryShapeItHasNotMeasured(t *testing.T) {
	full := obLevels([2]string{"0.4000", "20.00"})

	for _, tc := range []struct {
		name string
		resp Response
		err  error
		want string
	}{
		{"transport error", Response{}, errors.New("connection reset"),
			"connection reset"},
		{"HTTP 500", Response{Status: 500, Body: []byte(`{"error":"x"}`)},
			nil, "HTTP 500"},
		{"HTTP 404", Response{Status: 404, Body: []byte(`{}`)}, nil, "HTTP 404"},
		{"undecodable body", Response{Status: 200, Body: []byte(`{`)}, nil,
			"undecodable"},
		{"no orderbook_fp", Response{Status: 200, Body: []byte(`{"orderbook":{}}`)},
			nil, "orderbook_fp"},
		{"null orderbook_fp",
			Response{Status: 200, Body: []byte(`{"orderbook_fp":null}`)},
			nil, "orderbook_fp"},
		{"no yes_dollars", Response{Status: 200,
			Body: obBody(t, omitField{}, full)}, nil, "yes_dollars"},
		{"no no_dollars", Response{Status: 200,
			Body: obBody(t, full, omitField{})}, nil, "no_dollars"},
		{"null side", Response{Status: 200,
			Body: []byte(`{"orderbook_fp":{"yes_dollars":null,` +
				`"no_dollars":[["0.5500","20.00"]]}}`)}, nil, "yes_dollars"},
		{"levels are numbers, not strings", Response{Status: 200,
			Body: []byte(`{"orderbook_fp":{"yes_dollars":[[0.40,20]],` +
				`"no_dollars":[["0.5500","20.00"]]}}`)}, nil,
			"STRING pairs"},
		{"a size that does not parse", Response{Status: 200, Body: obBody(t,
			obLevels([2]string{"0.4000", "twenty"}), full)}, nil, "size"},
		{"the side is not sorted", Response{Status: 200, Body: obBody(t,
			obLevels([2]string{"0.4000", "20.00"}, [2]string{"0.3900", "5.00"}),
			full)}, nil, "ascending"},
		{"a price appears twice", Response{Status: 200, Body: obBody(t,
			obLevels([2]string{"0.4000", "20.00"}, [2]string{"0.4000", "5.00"}),
			full)}, nil, "ascending"},
		{"the ticker would re-target the request", Response{Status: 200,
			Body: obBody(t, full, full)}, nil, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := obClient(t, tc.resp, tc.err)
			ticker := obTicker
			if tc.want == "" {
				// The one case that never reaches the Doer at all.
				ticker = "KX/../portfolio/orders"
			}
			res := c.Orderbook(context.Background(), ticker)
			if res.Outcome != OrderbookFailed {
				t.Fatalf("outcome = %v, want failed", res.Outcome)
			}
			if res.Read() {
				t.Fatal("a failed read reported itself readable")
			}
			if res.Err == nil {
				t.Fatal("a failed read carries no cause")
			}
			if len(res.Yes) != 0 || len(res.No) != 0 {
				t.Fatalf("a failed read carried levels: %+v / %+v", res.Yes, res.No)
			}
			if tc.want != "" && !strings.Contains(res.Err.Error(), tc.want) {
				t.Fatalf("error %q does not mention %q", res.Err, tc.want)
			}
		})
	}

	// The zero value is not a book, and it is not agreement either.
	var zero OrderbookResult
	if zero.Read() {
		t.Fatal("the zero OrderbookResult reports itself readable; every field " +
			"of it describes an empty book on both sides, which is a plausible " +
			"fact and a catastrophic default")
	}
}

// TestOrderbookAssertsIntegerCentsRatherThanAssumingThem is H-CO-3a on the REST
// transport.
//
// It is not a parse failure and it is not agreement. The exchange answered, and
// what it said is that the tick size has changed -- so the outcome is its own,
// carries `SEV2 BOOK_PRICE_GRANULARITY`, and the levels are withheld so that
// nothing downstream can install them.
func TestOrderbookAssertsIntegerCentsRatherThanAssumingThem(t *testing.T) {
	for _, tc := range []struct {
		name     string
		yes, no  []any
		wantSide string
	}{
		{"a fractional yes level",
			obLevels([2]string{"0.4150", "20.00"}),
			obLevels([2]string{"0.5500", "20.00"}), "yes_dollars"},
		{"a fractional no level",
			obLevels([2]string{"0.4000", "20.00"}),
			obLevels([2]string{"0.5525", "20.00"}), "no_dollars"},
		{"a fifth decimal place",
			obLevels([2]string{"0.40001", "20.00"}),
			obLevels([2]string{"0.5500", "20.00"}), "yes_dollars"},
		{"a price that is not a number",
			obLevels([2]string{"forty", "20.00"}),
			obLevels([2]string{"0.5500", "20.00"}), "yes_dollars"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := obClient(t, Response{Status: 200,
				Body: obBody(t, tc.yes, tc.no)}, nil)

			res := c.Orderbook(context.Background(), obTicker)
			if res.Outcome != OrderbookGranularity {
				t.Fatalf("outcome = %v, want granularity", res.Outcome)
			}
			if res.Read() {
				t.Fatal("a fractional-cent book reported itself readable, so it " +
					"could replace a live book")
			}
			if len(res.Yes) != 0 || len(res.No) != 0 {
				t.Fatalf("levels survived: %+v / %+v", res.Yes, res.No)
			}
			if len(res.Anomalies) != 1 {
				t.Fatalf("anomalies = %v, want exactly one", res.Anomalies)
			}
			a := res.Anomalies[0]
			if a.Class != "BOOK_PRICE_GRANULARITY" || a.Sev != risk.SEV2 {
				t.Fatalf("anomaly = %+v, want SEV2 BOOK_PRICE_GRANULARITY", a)
			}
			if a.Ticker != obTicker {
				t.Fatalf("anomaly ticker = %q, want %q", a.Ticker, obTicker)
			}
			if !strings.Contains(a.Text, tc.wantSide) {
				t.Fatalf("anomaly %q does not name the side %q", a.Text, tc.wantSide)
			}
		})
	}
}

// TestOrderbookDropsZeroSizeLevelsLikeCoreDoes keeps the two books comparable.
//
// `core.parseLevels` filters `if size > 0` (rig.py:165), so a zero-size level is
// absent from the websocket book. Keeping it here would make two IDENTICAL books
// compare as different, and F5 would quarantine a healthy market on a level that
// is not there in either of them.
func TestOrderbookDropsZeroSizeLevelsLikeCoreDoes(t *testing.T) {
	c, _ := obClient(t, Response{Status: 200, Body: obBody(t,
		obLevels([2]string{"0.3900", "0.00"}, [2]string{"0.4000", "20.00"}),
		obLevels([2]string{"0.5500", "20.00"}),
	)}, nil)

	res := c.Orderbook(context.Background(), obTicker)
	if !res.Read() {
		t.Fatalf("outcome = %v, err = %v", res.Outcome, res.Err)
	}
	if len(res.Yes) != 1 || res.Yes[0].Cents != 40 {
		t.Fatalf("yes = %+v, want the zero-size level dropped", res.Yes)
	}
}

// ---------------------------------------------------------------------------
// H-FAIL-6
// ---------------------------------------------------------------------------

func lv(cents int, size float64) BookLevel {
	return BookLevel{Cents: cents, Size: size}
}

// TestCompareBooksWalksBothSidesToTargetSize is H-FAIL-6, case by case.
//
// The HR-015 row is the one this whole file exists for: the touch prices agree
// and the depth does not. A cross-check comparing prices resets the staleness
// clock and keeps quoting into a market that no longer qualifies, with an
// `external_best` subtraction that is now wrong -- so H-Q-10 self-chases against
// liquidity nobody is offering.
func TestCompareBooksWalksBothSidesToTargetSize(t *testing.T) {
	for _, tc := range []struct {
		name  string
		check BookCheck
		agree bool
		why   string
	}{
		{
			name: "identical books through target agree",
			check: BookCheck{Target: 25,
				Yes: SideBooks{
					WS:   []BookLevel{lv(50, 10), lv(49, 20), lv(48, 5)},
					REST: []BookLevel{lv(50, 10), lv(49, 20), lv(48, 5)}},
				No: SideBooks{
					WS: []BookLevel{lv(49, 30)}, REST: []BookLevel{lv(49, 30)}}},
			agree: true,
		},
		{
			name: "levels strictly beyond BOTH completed walks are irrelevant",
			check: BookCheck{Target: 25,
				Yes: SideBooks{
					WS:   []BookLevel{lv(50, 10), lv(49, 20), lv(48, 5)},
					REST: []BookLevel{lv(50, 10), lv(49, 20), lv(47, 999)}},
				No: SideBooks{
					WS: []BookLevel{lv(49, 30)}, REST: []BookLevel{lv(49, 30)}}},
			agree: true,
		},
		{
			name: "HR-015: the touch price agrees and the size does not",
			check: BookCheck{Target: 1000,
				Yes: SideBooks{
					WS:   []BookLevel{lv(50, 1300)},
					REST: []BookLevel{lv(50, 10), lv(49, 1200)}},
				No: SideBooks{
					WS: []BookLevel{lv(49, 1300)}, REST: []BookLevel{lv(49, 1300)}}},
			why: "disagrees at depth 1",
		},
		{
			name: "a DEEP price difference inside the walk",
			check: BookCheck{Target: 25,
				Yes: SideBooks{
					WS:   []BookLevel{lv(50, 10), lv(49, 20)},
					REST: []BookLevel{lv(50, 10), lv(48, 20)}},
				No: SideBooks{
					WS: []BookLevel{lv(49, 30)}, REST: []BookLevel{lv(49, 30)}}},
			why: "disagrees at depth 2",
		},
		{
			name: "a DEEP size difference inside the walk",
			check: BookCheck{Target: 25,
				Yes: SideBooks{
					WS:   []BookLevel{lv(50, 10), lv(49, 20)},
					REST: []BookLevel{lv(50, 10), lv(49, 21)}},
				No: SideBooks{
					WS: []BookLevel{lv(49, 30)}, REST: []BookLevel{lv(49, 30)}}},
			why: "disagrees at depth 2",
		},
		{
			name: "the OTHER side is checked too",
			check: BookCheck{Target: 25,
				Yes: SideBooks{
					WS: []BookLevel{lv(50, 30)}, REST: []BookLevel{lv(50, 30)}},
				No: SideBooks{
					WS: []BookLevel{lv(49, 30)}, REST: []BookLevel{lv(48, 30)}}},
			why: "the no side",
		},
		{
			name: "REST never reaches Target Size",
			check: BookCheck{Target: 25,
				Yes: SideBooks{
					WS:   []BookLevel{lv(50, 30)},
					REST: []BookLevel{lv(50, 10), lv(49, 10)}},
				No: SideBooks{
					WS: []BookLevel{lv(49, 30)}, REST: []BookLevel{lv(49, 30)}}},
			why: "does not reach Target Size",
		},
		{
			name: "the websocket never reaches Target Size",
			check: BookCheck{Target: 25,
				Yes: SideBooks{
					WS:   []BookLevel{lv(50, 10)},
					REST: []BookLevel{lv(50, 30)}},
				No: SideBooks{
					WS: []BookLevel{lv(49, 30)}, REST: []BookLevel{lv(49, 30)}}},
			why: "does not reach Target Size",
		},
		{
			name: "an empty REST side",
			check: BookCheck{Target: 25,
				Yes: SideBooks{WS: []BookLevel{lv(50, 30)}, REST: nil},
				No: SideBooks{
					WS: []BookLevel{lv(49, 30)}, REST: []BookLevel{lv(49, 30)}}},
			why: "does not reach Target Size",
		},
		{
			name: "no Target Size at all",
			check: BookCheck{Target: 0,
				Yes: SideBooks{
					WS: []BookLevel{lv(50, 30)}, REST: []BookLevel{lv(50, 30)}},
				No: SideBooks{
					WS: []BookLevel{lv(49, 30)}, REST: []BookLevel{lv(49, 30)}}},
			why: "no Target Size",
		},
		{
			name: "the walk includes the level that REACHES the target",
			check: BookCheck{Target: 25,
				Yes: SideBooks{
					WS:   []BookLevel{lv(50, 10), lv(49, 20)},
					REST: []BookLevel{lv(50, 10), lv(49, 20)}},
				No: SideBooks{
					// Exactly at the target on the first level: the walk stops
					// there and the difference below it cannot matter.
					WS:   []BookLevel{lv(49, 25), lv(48, 1)},
					REST: []BookLevel{lv(49, 25), lv(48, 2)}}},
			agree: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v := CompareBooks(tc.check)
			if v.Agree != tc.agree {
				t.Fatalf("agree = %v, want %v (why: %q)", v.Agree, tc.agree, v.Why)
			}
			if tc.agree && v.Why != "" {
				t.Fatalf("an agreement carried a reason: %q", v.Why)
			}
			if !tc.agree {
				if v.Why == "" {
					t.Fatal("a disagreement carried no reason; the reason is the " +
						"operator's SEV2 text")
				}
				if !strings.Contains(v.Why, tc.why) {
					t.Fatalf("reason %q does not mention %q", v.Why, tc.why)
				}
			}
		})
	}
}

// TestCompareBooksChecksOurOwnRestingSizeAtAnyDepth is H-FAIL-6's second
// clause: "additionally checks that our own resting size appears where we
// believe it is."
//
// It runs over the WHOLE side and not the Target Size prefix. Our resting order
// can be deeper than the walk and is still ours, and "the size we believe is
// resting is not there" is the discrepancy that costs money at any depth -- it
// means either the order is gone (and we are unhedged while believing we are
// not) or the book we hold is stale in a way the prefix did not reach.
func TestCompareBooksChecksOurOwnRestingSizeAtAnyDepth(t *testing.T) {
	// Target 25 is reached on the first level of each side, so every case below
	// that is about a deeper price is about a level the walk never visits.
	base := func(ourSide *SideBooks) BookCheck {
		c := BookCheck{Target: 25,
			Yes: SideBooks{
				WS:   []BookLevel{lv(50, 30), lv(40, 8)},
				REST: []BookLevel{lv(50, 30), lv(40, 8)}},
			No: SideBooks{
				WS: []BookLevel{lv(49, 30)}, REST: []BookLevel{lv(49, 30)}}}
		if ourSide != nil {
			c.Yes.Ours = ourSide.Ours
		}
		return c
	}
	ours := func(cents int, size float64) *SideBooks {
		return &SideBooks{Ours: []OwnResting{{
			Cents: cents, Size: num.QtyFromFloat(size)}}}
	}

	for _, tc := range []struct {
		name  string
		check BookCheck
		agree bool
		why   string
	}{
		{"our size is there, beyond the walk", base(ours(40, 8)), true, ""},
		{"our size is there and the level has more", base(ours(40, 5)), true, ""},
		{"our size is inside the walk", base(ours(50, 30)), true, ""},
		{"the level shows LESS than we believe is ours",
			base(ours(40, 9)), false, "shows only"},
		{"there is no level where we believe we rest",
			base(ours(41, 1)), false, "no level there at all"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v := CompareBooks(tc.check)
			if v.Agree != tc.agree {
				t.Fatalf("agree = %v, want %v (why: %q)", v.Agree, tc.agree, v.Why)
			}
			if !tc.agree && !strings.Contains(v.Why, tc.why) {
				t.Fatalf("reason %q does not mention %q", v.Why, tc.why)
			}
		})
	}

	// BOTH books have to show it. A websocket book that still shows our order
	// while REST does not is the case that matters most -- the order is gone and
	// the stale feed is the only thing still saying otherwise -- and the reverse
	// is a REST answer we cannot reconcile with the book we are quoting from.
	for _, tc := range []struct {
		name       string
		ws, remote []BookLevel
		want       string
	}{
		{"REST does not show it", []BookLevel{lv(50, 30), lv(40, 8)},
			[]BookLevel{lv(50, 30), lv(40, 1)}, "REST book shows only"},
		{"the websocket does not show it", []BookLevel{lv(50, 30), lv(40, 1)},
			[]BookLevel{lv(50, 30), lv(40, 8)}, "websocket book shows only"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v := CompareBooks(BookCheck{Target: 25,
				Yes: SideBooks{WS: tc.ws, REST: tc.remote,
					Ours: []OwnResting{{Cents: 40, Size: num.QtyFromFloat(8)}}},
				No: SideBooks{
					WS: []BookLevel{lv(49, 30)}, REST: []BookLevel{lv(49, 30)}}})
			if v.Agree {
				t.Fatal("a book missing our own resting size agreed")
			}
			if !strings.Contains(v.Why, tc.want) {
				t.Fatalf("reason %q does not mention %q", v.Why, tc.want)
			}
		})
	}
}

func TestOrderbookRejectsExtraTupleFields(t *testing.T) {
	c, _ := obClient(t, Response{Status: 200, Body: obBody(t, [][]string{{"0.40", "20", "ignored"}}, [][]string{})}, nil)
	if c.Orderbook(context.Background(), obTicker).Read() {
		t.Fatal("extra fields accepted")
	}
}

func TestReducerBookIndependentAndFresh(t *testing.T) {
	c, _ := obClient(t, Response{Status: 200, Body: obBody(t, obLevels([2]string{"0.40", "20"}), obLevels([2]string{"0.55", "20"}))}, nil)
	r := c.Orderbook(context.Background(), obTicker)
	var saved ReducerBook
	if !saved.Retain(r, 10*time.Second) {
		t.Fatal(r.Err)
	}
	r.Yes[0].Wire[0] = "0.01"
	b, ok := saved.Snapshot(obTicker, 10, 20*time.Second, 10*time.Second)
	if !ok {
		t.Fatal("fresh source unavailable")
	}
	if err := b.ApplySnapshot(nil, nil); err != nil {
		t.Fatal(err)
	}
	b2, ok := saved.Snapshot(obTicker, 10, 20*time.Second, 10*time.Second)
	if !ok || b2.Yes().Sum() != 20 {
		t.Fatal("caller mutated retained book")
	}
	if _, ok := saved.Snapshot(obTicker, 10, 20*time.Second+1, 10*time.Second); ok {
		t.Fatal("stale source licensed")
	}
	if _, ok := saved.Snapshot("OTHER", 10, 20*time.Second, 10*time.Second); ok {
		t.Fatal("wrong market licensed")
	}
	saved.Retain(OrderbookResult{}, 20*time.Second)
	if _, ok := saved.Snapshot(obTicker, 10, 20*time.Second, 10*time.Second); ok {
		t.Fatal("failed refresh retained authority")
	}
}

func TestCompareBooksAggregatesExpectedOwnedSize(t *testing.T) {
	side := SideBooks{WS: []BookLevel{{Cents: 50, Size: 10}}, REST: []BookLevel{{Cents: 50, Size: 10}}, Ours: []OwnResting{{Cents: 50, Size: num.QtyFromFloat(6)}, {Cents: 50, Size: num.QtyFromFloat(6)}}}
	if CompareBooks(BookCheck{Target: 5, Yes: side, No: side}).Agree {
		t.Fatal("two owned orders exceed visible depth")
	}
}
