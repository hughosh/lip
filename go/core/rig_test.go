package core

import (
	"fmt"
	"testing"
)

// These tests exist because gate 7 found that the differential replay could NOT
// detect three preservation-rule violations: the tape has no negative book lag,
// no duplicate trade_id, and no sequence gap, so those paths never execute. A
// gate that cannot fail on a rule is not evidence for that rule, so each one is
// pinned here deterministically instead of hoping a future tape contains it.

type recorder struct {
	fills   []FillRow
	refs    []ReferenceRow
	pending []string
}

func (r *recorder) Fill(f FillRow)           { r.fills = append(r.fills, f) }
func (r *recorder) Reference(x ReferenceRow) { r.refs = append(r.refs, x) }
func (r *recorder) PendingMid(tradeID, ticker, horizon string, dueMS int64) {
	r.pending = append(r.pending, fmt.Sprintf("%s/%s/%d", tradeID, horizon, dueMS))
}

func feed(t *testing.T, r *Rig, frames ...string) {
	t.Helper()
	for i, f := range frames {
		if err := r.Handle([]byte(f)); err != nil {
			t.Fatalf("frame %d (%.60s): %v", i, f, err)
		}
	}
}

func snapshot(sid, seq int, ticker string) string {
	return fmt.Sprintf(`{"type":"orderbook_snapshot","sid":%d,"seq":%d,"msg":{
		"market_ticker":"%s",
		"yes_dollars_fp":[["0.4800","50.00"],["0.4700","60.00"]],
		"no_dollars_fp":[["0.5000","70.00"],["0.4900","40.00"]]}}`, sid, seq, ticker)
}

func delta(sid, seq int, ticker string, tsMs int64) string {
	return fmt.Sprintf(`{"type":"orderbook_delta","sid":%d,"seq":%d,"msg":{
		"market_ticker":"%s","ts_ms":%d,"side":"yes",
		"price_dollars":"0.4800","delta_fp":"5.00"}}`, sid, seq, ticker, tsMs)
}

func trade(sid, seq int, ticker, id string, tsMs int64) string {
	return fmt.Sprintf(`{"type":"trade","sid":%d,"seq":%d,"msg":{
		"trade_id":"%s","market_ticker":"%s","ts_ms":%d,"count_fp":"3.00",
		"yes_price_dollars":"0.4800","no_price_dollars":"0.5200",
		"taker_side":"no"}}`, sid, seq, id, ticker, tsMs)
}

// --- P5 / P7: negative lag, truncated toward zero -------------------------

func TestP7_NegativeBookLagTruncatesTowardZero(t *testing.T) {
	rec := &recorder{}
	r := NewRig(rec, map[string]float64{"AAA": 0, "BBB": 0})

	// Drive the watermark to 2_000_000 on BBB, THEN snapshot AAA. The snapshot
	// clears AAA's history and re-checkpoints it at the current watermark, so
	// every checkpoint AAA has is newer than the trade that follows.
	feed(t, r,
		snapshot(2, 1, "BBB"),
		delta(2, 2, "BBB", 2_000_000),
		snapshot(2, 3, "AAA"),
		trade(1, 1, "AAA", "t-neg", 1_999_863),
	)

	if len(rec.fills) != 1 {
		t.Fatalf("got %d fills, want 1", len(rec.fills))
	}
	got := rec.fills[0].BookLagMs
	if got == nil {
		t.Fatal("book_lag_ms is nil; the oldest-checkpoint fallback should give a real lag")
	}
	// lag = 1999.863 - 2000.0 = -0.1369999999999436, so lag*1000 is
	// -136.9999999999436. Truncation toward zero gives -136; math.Floor would
	// give -137. Python's int() truncates. See port-spec.md P7.
	if *got != -136 {
		t.Errorf("book_lag_ms = %d, want -136 (truncated toward zero, not floored)", *got)
	}
}

// --- P13: a sequence gap quarantines every book ---------------------------

func TestP13_SequenceGapQuarantinesAllBooksAndDropsTheFrame(t *testing.T) {
	rec := &recorder{}
	r := NewRig(rec, map[string]float64{"AAA": 0, "BBB": 0})

	feed(t, r, snapshot(2, 1, "AAA"), snapshot(2, 2, "BBB"))
	if r.StaleCount() != 0 {
		t.Fatalf("after both snapshots, stale = %d, want 0", r.StaleCount())
	}
	before := len(rec.refs)

	feed(t, r, delta(2, 4, "AAA", 3_000_000)) // seq 2 -> 4: a gap

	if r.Stats.Gaps != 1 {
		t.Errorf("gaps = %d, want 1", r.Stats.Gaps)
	}
	if r.StaleCount() != 2 {
		t.Errorf("stale = %d, want 2: a gap is subscription-wide, so EVERY book is suspect",
			r.StaleCount())
	}
	if !r.NeedsResnapshot {
		t.Error("NeedsResnapshot not set")
	}
	if len(rec.refs) != before {
		t.Error("the gap frame's book update was applied; it must be dropped entirely")
	}

	// A trade for a quarantined market is counted, not recorded.
	feed(t, r, trade(1, 1, "AAA", "t-quar", 3_100_000))
	if r.Stats.Quarantined != 1 {
		t.Errorf("quarantined = %d, want 1", r.Stats.Quarantined)
	}
	if len(rec.fills) != 0 {
		t.Errorf("got %d fills from a quarantined market, want 0", len(rec.fills))
	}
}

// --- P15: the watermark/stale asymmetry ------------------------------------

func TestP15_StaleDeltaDoesNotAdvanceWatermarkButStaleTradeDoes(t *testing.T) {
	rec := &recorder{}
	r := NewRig(rec, map[string]float64{"AAA": 0})

	feed(t, r,
		snapshot(2, 1, "AAA"),
		delta(2, 2, "AAA", 1_000_000), // watermark -> 1_000_000
		delta(2, 4, "AAA", 2_000_000), // seq gap: frame dropped, AAA quarantined
	)
	if r.Watermark() != 1_000_000 {
		t.Fatalf("watermark = %d before the stale delta, want 1000000", r.Watermark())
	}

	feed(t, r, delta(2, 5, "AAA", 9_000_000)) // AAA is stale
	if got := r.Watermark(); got != 1_000_000 {
		t.Errorf("watermark = %d after a delta for a STALE market, want 1000000: the "+
			"delta branch returns BEFORE advancing the watermark", got)
	}

	// The trade branch is the other half of the asymmetry: it advances the
	// watermark BEFORE the stale check, so a quarantined trade still moves it.
	feed(t, r, trade(1, 1, "AAA", "t-x", 5_000_000))
	if got := r.Watermark(); got != 5_000_000 {
		t.Errorf("watermark = %d after a trade for a STALE market, want 5000000: the "+
			"trade branch advances the watermark BEFORE the stale check", got)
	}
	if r.Stats.Quarantined != 1 {
		t.Errorf("quarantined = %d, want 1", r.Stats.Quarantined)
	}
}

// --- P16: whole-exchange trade tape is filtered before the counters --------

func TestP16_TradeOutsideUniverseIsNotCounted(t *testing.T) {
	rec := &recorder{}
	r := NewRig(rec, map[string]float64{"AAA": 0})
	feed(t, r, snapshot(2, 1, "AAA"), trade(1, 1, "ZZZ", "t-other", 1_000_000))

	if r.Stats.Trades != 0 || r.Stats.Quarantined != 0 || len(rec.fills) != 0 {
		t.Errorf("trades=%d quarantined=%d fills=%d, want all zero: a trade for a "+
			"market outside the universe returns before every counter",
			r.Stats.Trades, r.Stats.Quarantined, len(rec.fills))
	}
}

// --- P21: pending_mid horizon order ---------------------------------------

func TestP21_PendingMidsAreEmittedInHorizonOrder(t *testing.T) {
	rec := &recorder{}
	r := NewRig(rec, map[string]float64{"AAA": 0})
	feed(t, r, snapshot(2, 1, "AAA"), trade(1, 1, "AAA", "t1", 1_000_000))

	// ts_ms 1_000_000 plus 60s, 300s and 1800s.
	want := []string{"t1/1m/1060000", "t1/5m/1300000", "t1/30m/2800000"}
	if len(rec.pending) != 3 {
		t.Fatalf("got %d pending mids, want 3", len(rec.pending))
	}
	for i, w := range want {
		if rec.pending[i] != w {
			t.Errorf("pending[%d] = %s, want %s (Python iterates the dict literal "+
				"in 1m, 5m, 30m order)", i, rec.pending[i], w)
		}
	}
}

// --- P8: the falsy watermark guard, and why ts_ms needs no pointer ----------
//
// `if ts_ms:` in Python is falsy, so a MISSING timestamp, a JSON null and a
// literal ZERO all skip the update. A plain int64 reproduces that exactly —
// absent and null both decode to zero, and zero already skips — which is why
// the snapshot and delta branches carry no pointer. That removed one heap
// allocation per snapshot and per delta frame.
//
// The trade branch is NOT the same and keeps its pointer: there an absent ts_ms
// must be an error, where a present zero is a legitimate row.
func TestP8_WatermarkTreatsAbsentNullAndZeroAlike(t *testing.T) {
	for _, c := range []struct {
		name, tsField string
	}{
		{"absent", ""},
		{"null", `"ts_ms":null,`},
		{"zero", `"ts_ms":0,`},
	} {
		rec := &recorder{}
		r := NewRig(rec, map[string]float64{"AAA": 0})
		feed(t, r, snapshot(2, 1, "AAA"), delta(2, 2, "AAA", 7_000_000))
		if r.Watermark() != 7_000_000 {
			t.Fatalf("%s: setup watermark = %d", c.name, r.Watermark())
		}
		// A delta whose ts_ms is absent/null/zero must NOT move the watermark.
		frame := fmt.Sprintf(`{"type":"orderbook_delta","sid":2,"seq":3,"msg":{
			"market_ticker":"AAA",%s"side":"yes",
			"price_dollars":"0.4800","delta_fp":"1.00"}}`, c.tsField)
		feed(t, r, frame)
		if got := r.Watermark(); got != 7_000_000 {
			t.Errorf("%s ts_ms moved the watermark to %d, want 7000000: the guard "+
				"is FALSY, so absent, null and zero all skip (P8)", c.name, got)
		}
	}
}
