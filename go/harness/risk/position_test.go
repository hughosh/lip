package risk

import (
	"strings"
	"testing"

	"lip/harness/cfg"
	"lip/harness/num"
	"lip/harness/quote"
)

// pollMs is the caller's poll clock for tests that do not care about it. Only
// the deferral-deadline tests advance it, and they pass their own values.
const pollMs int64 = 1_700_000_000_000

// ledger is a fake OwnershipLookup. The real one is durable and lives in
// `lip-6w5`; this package only ever sees the question, never the storage.
type ledger struct {
	ours map[string]bool
	// unresolved is the ids the ledger cannot conclude about: a reservation is
	// outstanding that could still turn out to be them.
	unresolved map[string]bool
	// err makes the durable ledger unavailable, which is a store fault and not
	// a classification.
	err error
	// short returns a malformed answer: the right kind, the wrong length.
	short bool
}

func (l ledger) OwnsOrders(ids []string) ([]Ownership, error) {
	if l.err != nil {
		return nil, l.err
	}
	if l.short {
		return nil, nil
	}
	out := make([]Ownership, len(ids))
	for i, id := range ids {
		switch {
		case l.ours[id]:
			out[i] = OwnershipOurs
		case l.unresolved[id]:
			out[i] = OwnershipUnresolved
		default:
			out[i] = OwnershipForeign
		}
	}
	return out, nil
}

func owns(ids ...string) ledger {
	m := map[string]bool{}
	for _, id := range ids {
		m[id] = true
	}
	return ledger{ours: m}
}

// unresolves is a ledger that cannot conclude about these ids and calls
// everything else conclusively foreign.
func unresolves(ids ...string) ledger {
	m := map[string]bool{}
	for _, id := range ids {
		m[id] = true
	}
	return ledger{unresolved: m}
}

func anomalyClasses(as []Anomaly) []string {
	out := make([]string, 0, len(as))
	for _, a := range as {
		out = append(out, a.Class)
	}
	return out
}

func hasClass(as []Anomaly, class string) bool {
	for _, a := range as {
		if a.Class == class {
			return true
		}
	}
	return false
}

func sevOf(t *testing.T, as []Anomaly, class string) Severity {
	t.Helper()
	for _, a := range as {
		if a.Class == class {
			return a.Sev
		}
	}
	t.Fatalf("no %s anomaly in %v", class, anomalyClasses(as))
	return SEV3
}

// TestAckAndFillAreOrderIndependentAndNeverDoubleCount is the reconciliation
// property §8.2 depends on and that no single-source model can have.
//
// `q_local` is fed by two sources describing the same event: the write path's
// acknowledgement, which is fast, and the authoritative fills walk, which is
// true. Both must be applied, because dropping the ack costs reaction speed
// between polls and dropping the fill costs the truth. Applying both naively
// counts every fill twice, and a doubled `q` sizes a reducer at twice the real
// inventory -- which under H-Q-5a is the sign-flipping exit the whole design
// forbids.
//
// The test runs the same three events in both orders and requires the same
// final position, and requires that the fills walk being re-delivered on every
// poll -- which is exactly what a complete walk does, forever -- moves nothing.
func TestAckAndFillAreOrderIndependentAndNeverDoubleCount(t *testing.T) {
	const tk = "KXTEST-A"
	own := owns("ord-1")

	ackFirst := NewPortfolio()
	ackFirst.ApplyAck(AckFill{OrderID: "ord-1", Ticker: tk,
		Side: quote.SideYes, Filled: contracts(3)})
	if got := ackFirst.Q(tk); got != contracts(3) {
		t.Fatalf("after ack alone q = %s, want 3.00", got.Wire())
	}
	ackFirst.ApplyFills([]FillEvent{
		{TradeID: "t1", OrderID: "ord-1", Ticker: tk, Side: quote.SideYes,
			Count: contracts(2)},
		{TradeID: "t2", OrderID: "ord-1", Ticker: tk, Side: quote.SideYes,
			Count: contracts(1)},
	}, own, Live, pollMs)

	fillFirst := NewPortfolio()
	fillFirst.ApplyFills([]FillEvent{
		{TradeID: "t1", OrderID: "ord-1", Ticker: tk, Side: quote.SideYes,
			Count: contracts(2)},
		{TradeID: "t2", OrderID: "ord-1", Ticker: tk, Side: quote.SideYes,
			Count: contracts(1)},
	}, own, Live, pollMs)
	fillFirst.ApplyAck(AckFill{OrderID: "ord-1", Ticker: tk,
		Side: quote.SideYes, Filled: contracts(3)})

	if a, b := ackFirst.Q(tk), fillFirst.Q(tk); a != b {
		t.Fatalf("order dependence: ack-then-fill q = %s, fill-then-ack q = %s",
			a.Wire(), b.Wire())
	}
	if got := ackFirst.Q(tk); got != contracts(3) {
		t.Fatalf("q = %s after one ack and two corroborating fills, want 3.00 "+
			"-- the ack and the fills describe the SAME three contracts",
			got.Wire())
	}

	// The fills endpoint is walked in full on every poll, so the same two fills
	// arrive again immediately. H-ORD-6's trade_id dedup is what stops a
	// correct read from growing the position without bound.
	for i := 0; i < 3; i++ {
		ackFirst.ApplyFills([]FillEvent{
			{TradeID: "t1", OrderID: "ord-1", Ticker: tk, Side: quote.SideYes,
				Count: contracts(2)},
			{TradeID: "t2", OrderID: "ord-1", Ticker: tk, Side: quote.SideYes,
				Count: contracts(1)},
		}, own, Live, pollMs)
	}
	if got := ackFirst.Q(tk); got != contracts(3) {
		t.Fatalf("q = %s after re-walking the same fills three times, want "+
			"3.00; trade_id dedup is not holding", got.Wire())
	}

	// A stale ack is a lower view of the same running total, not a retraction.
	ackFirst.ApplyAck(AckFill{OrderID: "ord-1", Ticker: tk,
		Side: quote.SideYes, Filled: contracts(1)})
	if got := ackFirst.Q(tk); got != contracts(3) {
		t.Fatalf("a lower cumulative ack reduced q to %s; acks are a running "+
			"total, so a smaller one is a stale view and not an un-fill",
			got.Wire())
	}

	// A NO fill is the same number with the other sign (§8.1).
	no := NewPortfolio()
	no.ApplyFills([]FillEvent{{TradeID: "t9", OrderID: "ord-9", Ticker: tk,
		Side: quote.SideNo, Count: contracts(4)}}, owns("ord-9"), Live, pollMs)
	if got := no.Q(tk); got != contracts(-4) {
		t.Fatalf("a 4-contract NO fill gave q = %s, want -4.00", got.Wire())
	}
}

// TestSeedModeRecordsIdentityWithoutReapplyingHistory is the startup case.
//
// `GET /portfolio/positions` already reports the position the account's whole
// fill history produced. Replaying that history on top of it doubles every
// contract we hold -- at exactly the moment H-ORD-5 is deciding whether it is
// safe to place anything at all.
func TestSeedModeRecordsIdentityWithoutReapplyingHistory(t *testing.T) {
	const tk = "KXTEST-S"
	p := NewPortfolio()
	p.ReplacePositions(map[string]num.Qty{tk: contracts(7)}, true, cfg.Default())

	eff := p.ApplyFills([]FillEvent{
		{TradeID: "h1", OrderID: "ord-1", Ticker: tk, Side: quote.SideYes,
			Count: contracts(7)},
	}, owns("ord-1"), Seed, pollMs)
	if got := p.Q(tk); got != contracts(7) {
		t.Fatalf("seeding replayed history onto the exchange's own figure: "+
			"q = %s, want 7.00", got.Wire())
	}
	if len(eff.Owned) != 1 {
		t.Fatalf("seed mode dropped the fill identity; Owned = %d, want 1",
			len(eff.Owned))
	}

	// The historical fill is now recorded, so a later live delivery of the same
	// trade_id must not move q either.
	p.ApplyFills([]FillEvent{
		{TradeID: "h1", OrderID: "ord-1", Ticker: tk, Side: quote.SideYes,
			Count: contracts(7)},
	}, owns("ord-1"), Live, pollMs)
	if got := p.Q(tk); got != contracts(7) {
		t.Fatalf("q = %s after re-delivering a seeded fill live, want 7.00",
			got.Wire())
	}

	// A genuinely NEW fill on the same order still moves q: seeding set the
	// baseline, it did not deafen the order.
	p.ApplyFills([]FillEvent{
		{TradeID: "h2", OrderID: "ord-1", Ticker: tk, Side: quote.SideYes,
			Count: contracts(2)},
	}, owns("ord-1"), Live, pollMs)
	if got := p.Q(tk); got != contracts(9) {
		t.Fatalf("q = %s after a new 2-contract fill on a seeded order, "+
			"want 9.00", got.Wire())
	}

	// A taker fill in our HISTORY is still a fact about the harness -- but a
	// Seed walk is history by definition, so it is recorded and not halted on
	// (`lip-jhi`). H-Q-3 is a rule about what this incarnation does.
	seedTaker := NewPortfolio()
	e2 := seedTaker.ApplyFills([]FillEvent{
		{TradeID: "h3", OrderID: "ord-2", Ticker: tk, Side: quote.SideYes,
			Count: contracts(1), IsTaker: true},
	}, owns("ord-2"), Seed, pollMs)
	if !hasClass(e2.Anomalies, "TAKER_FILL_INHERITED") {
		t.Fatalf("a historical taker fill was not classified in seed mode: "+
			"stop=%v classes=%v", e2.Stop, anomalyClasses(e2.Anomalies))
	}
	if e2.Stop {
		t.Fatalf("a taker fill read by the STARTUP walk requested a global "+
			"stop. Every fill a seed walk sees predates this incarnation, so "+
			"halting on one makes the rule unsatisfiable by any account with "+
			"history -- which is what stopped q01 at t+2s (lip-jhi). "+
			"classes=%v", anomalyClasses(e2.Anomalies))
	}
}

// TestFillOwnershipUsesOrderIDLedgerAndForeignStopsGlobally is H-ORD-9.
//
// Ownership is a ledger, not an inference. HR-024 found that without one, a
// manual taker fill either lands in `our_fill` and falsely trips F14's global
// halt, or is filtered out and leaves a hole in the ownership record -- and
// nothing in the data tells the two apart.
//
// The foreign consequence is GLOBAL, not per market. Someone else is trading
// the account our position model describes, so that model is unreliable
// everywhere.
func TestFillOwnershipUsesOrderIDLedgerAndForeignStopsGlobally(t *testing.T) {
	const tk = "KXTEST-F"
	p := NewPortfolio()

	eff := p.ApplyFills([]FillEvent{
		{TradeID: "t1", OrderID: "ours", Ticker: tk, Side: quote.SideYes,
			Count: contracts(2)},
		{TradeID: "t2", OrderID: "theirs", Ticker: tk, Side: quote.SideYes,
			Count: contracts(5)},
	}, owns("ours"), Live, pollMs)

	if got := p.Q(tk); got != contracts(2) {
		t.Fatalf("q = %s; the foreign fill must not enter q_local at all, so "+
			"only our own 2 contracts may appear", got.Wire())
	}
	if len(eff.Owned) != 1 || eff.Owned[0].TradeID != "t1" {
		t.Fatalf("Owned = %v, want exactly the ledger-backed fill t1", eff.Owned)
	}
	if len(eff.Foreign) != 1 || eff.Foreign[0].TradeID != "t2" {
		t.Fatalf("Foreign = %v, want exactly t2", eff.Foreign)
	}
	if !eff.Stop {
		t.Fatal("a foreign fill did not request global WINDING_DOWN; H-ORD-9 " +
			"makes foreign activity global precisely because the account, not " +
			"the market, is what has become untrustworthy")
	}
	if sevOf(t, eff.Anomalies, "FOREIGN_FILL") != SEV1 {
		t.Fatalf("FOREIGN_FILL is not SEV1: %v", eff.Anomalies)
	}
	// A foreign fill must NOT be reported as a taker fill of ours -- that is
	// exactly the F14 misfire HR-024 named.
	if hasClass(eff.Anomalies, "TAKER_FILL") {
		t.Fatalf("a foreign fill produced a TAKER_FILL anomaly: %v",
			anomalyClasses(eff.Anomalies))
	}

	// The ledger covers TERMINAL orders too, so a fill on an order that no
	// longer rests is still ours. Nothing here consults the resting map.
	p2 := NewPortfolio()
	e2 := p2.ApplyFills([]FillEvent{{TradeID: "t3", OrderID: "long-gone",
		Ticker: tk, Side: quote.SideYes, Count: contracts(1)}},
		owns("long-gone"), Live, pollMs)
	if e2.Stop || len(e2.Owned) != 1 {
		t.Fatalf("a fill on a terminal order of ours was not claimed: "+
			"stop=%v owned=%v", e2.Stop, e2.Owned)
	}

	// A nil lookup fails closed. "The ledger is unavailable" and "nothing is
	// ours" are opposite classifications of the same fill.
	p3 := NewPortfolio()
	e3 := p3.ApplyFills([]FillEvent{{TradeID: "t4", OrderID: "x", Ticker: tk,
		Side: quote.SideYes, Count: contracts(1)}}, nil, Live, pollMs)
	if !e3.Stop || p3.Q(tk) != 0 {
		t.Fatalf("a nil ownership ledger did not fail closed: stop=%v q=%s",
			e3.Stop, p3.Q(tk).Wire())
	}
}

// TestOwnedTakerOrPositiveFeeStopsGlobally is H-ORD-8 and its independent
// corroborator S2.
//
// One fill with `is_taker = true` means H-Q-3 has been violated by something --
// a post_only that did not take effect, a marketable price, an API change. It
// is the cheapest possible detector for the most expensive possible bug.
// `fee_cost > 0` is the second, independent witness: maker fees are $0.00, so a
// non-zero fee is a taker fill whatever the flag claims.
//
// The contracts still exist. The position is updated BEFORE the alarm, because
// a reducer sized from a q that omitted them is a second failure on top of the
// first.
func TestOwnedTakerOrPositiveFeeStopsGlobally(t *testing.T) {
	const tk = "KXTEST-T"
	for _, tc := range []struct {
		name string
		fill FillEvent
	}{
		{"is_taker true", FillEvent{TradeID: "t1", OrderID: "o", Ticker: tk,
			Side: quote.SideYes, Count: contracts(3), IsTaker: true}},
		{"fee alone, flag false", FillEvent{TradeID: "t1", OrderID: "o",
			Ticker: tk, Side: quote.SideYes, Count: contracts(3),
			Fee: num.MoneyFromDollars(0.01)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := NewPortfolio()
			eff := p.ApplyFills([]FillEvent{tc.fill}, owns("o"), Live, pollMs)
			if !eff.Stop {
				t.Fatal("no global stop was requested")
			}
			if sevOf(t, eff.Anomalies, "TAKER_FILL") != SEV1 {
				t.Fatalf("TAKER_FILL is not SEV1: %v", eff.Anomalies)
			}
			if got := p.Q(tk); got != contracts(3) {
				t.Fatalf("q = %s; the contracts exist however they were "+
					"acquired, and a reducer sized from a q that omits them "+
					"is the second failure", got.Wire())
			}
			if len(eff.Owned) != 1 {
				t.Fatalf("the taker fill was dropped from Owned: %v", eff.Owned)
			}
		})
	}

	// A zero fee and a false flag is the ordinary case and must stay silent,
	// or the detector is a permanent alarm and stops being read.
	p := NewPortfolio()
	eff := p.ApplyFills([]FillEvent{{TradeID: "t2", OrderID: "o", Ticker: tk,
		Side: quote.SideYes, Count: contracts(3)}}, owns("o"), Live, pollMs)
	if eff.Stop || hasClass(eff.Anomalies, "TAKER_FILL") {
		t.Fatalf("a maker fill tripped the taker detector: stop=%v classes=%v",
			eff.Stop, anomalyClasses(eff.Anomalies))
	}
}

// TestCompletePositionPollOverwritesLocalAndRecordsAgreements is H-POS-1 and
// the recording half of H-POS-2.
//
// Three separate properties, and each closes a distinct hole:
//
//   - the exchange OVERWRITES. `q_local` exists so we can react between polls,
//     not so we can argue with the exchange.
//   - the overwrite covers the UNION of what we thought we held and what came
//     back, so a market the exchange no longer reports goes to zero instead of
//     keeping its stale q and its stale reducer forever.
//   - a poll where the two AGREE is still recorded. A table with only
//     disagreements in it cannot distinguish "no drift" from "not polling",
//     which is the observable probebot.py produced for 6.14 hours.
func TestCompletePositionPollOverwritesLocalAndRecordsAgreements(t *testing.T) {
	p := NewPortfolio()
	own := owns("o1", "o2")
	p.ApplyFills([]FillEvent{
		{TradeID: "t1", OrderID: "o1", Ticker: "A", Side: quote.SideYes,
			Count: contracts(4)},
		{TradeID: "t2", OrderID: "o2", Ticker: "B", Side: quote.SideYes,
			Count: contracts(9)},
	}, own, Live, pollMs)

	eff := p.ReplacePositions(map[string]num.Qty{
		"A": contracts(4), // agrees
		"C": contracts(2), // a market we did not know we held
	}, true, cfg.Default())

	if !eff.Applied {
		t.Fatal("a complete walk was not applied")
	}
	if got := p.Q("A"); got != contracts(4) {
		t.Fatalf("A: q = %s, want 4.00", got.Wire())
	}
	// B is absent from a COMPLETE result, which means flat. That inference is
	// only available because the walk completed.
	if got := p.Q("B"); got != 0 {
		t.Fatalf("B: q = %s, want 0 -- absence from a complete positions walk "+
			"is the exchange saying we hold none", got.Wire())
	}
	if got := p.Q("C"); got != contracts(2) {
		t.Fatalf("C: q = %s, want 2.00 -- H-ORD-5b seeds every market with a "+
			"position, including ones not in the selection set", got.Wire())
	}

	if len(eff.Records) != 3 {
		t.Fatalf("records = %d, want one per ticker over the union {A,B,C}: %v",
			len(eff.Records), eff.Records)
	}
	var sawAgreement bool
	for _, r := range eff.Records {
		if r.Ticker == "A" {
			if !r.Agreed || r.Delta != 0 {
				t.Fatalf("A recorded as disagreeing: %+v", r)
			}
			sawAgreement = true
		}
		if r.Ticker == "B" && (r.QLocal != contracts(9) || r.QExch != 0) {
			t.Fatalf("B record does not carry both readings: %+v", r)
		}
	}
	if !sawAgreement {
		t.Fatal("the agreeing market produced no position_poll record; a " +
			"table with only disagreements cannot tell 'no drift' from " +
			"'not polling'")
	}

	// An INCOMPLETE walk changes nothing at all -- not q, not a record.
	before := p.Q("A")
	inc := p.ReplacePositions(map[string]num.Qty{}, false, cfg.Default())
	if inc.Applied || len(inc.Records) != 0 || p.Q("A") != before {
		t.Fatalf("an incomplete walk was applied: applied=%v records=%d q=%s",
			inc.Applied, len(inc.Records), p.Q("A").Wire())
	}
	if !hasClass(inc.Anomalies, "POSITION_WALK_INCOMPLETE") {
		t.Fatalf("an incomplete walk was silent: %v",
			anomalyClasses(inc.Anomalies))
	}
}

// TestPositionDriftThresholdsAreStrictAndConsecutive is H-POS-2's escalation.
//
// Both comparisons are STRICT, and the tolerance one needs TWO CONSECUTIVE
// polls. A drift exactly at the tolerance is not past it; a single drifting
// poll is the normal case H-POS-2 explicitly calls normal (a fill landed
// between events); and one agreeing poll clears the streak, or a counter that
// only ever climbs eventually reduces every market that has ever traded.
func TestPositionDriftThresholdsAreStrictAndConsecutive(t *testing.T) {
	prm := cfg.Default() // pos_drift_tol 0.01, pos_drift_hard 5

	// Exactly at the tolerance is not past it.
	p := NewPortfolio()
	p.ApplyAck(AckFill{OrderID: "o", Ticker: "A", Side: quote.SideYes,
		Filled: contracts(1)})
	for i := 0; i < 4; i++ {
		eff := p.ReplacePositions(map[string]num.Qty{
			"A": contracts(1) - prm.PosDriftTol}, true, prm)
		if len(eff.Reduce) != 0 {
			t.Fatalf("poll %d reduced on a drift EQUAL to pos_drift_tol; the "+
				"comparison is strictly greater", i)
		}
		// The overwrite has now made them agree, so re-arm the drift.
		p.ApplyAck(AckFill{OrderID: "o", Ticker: "A", Side: quote.SideYes,
			Filled: contracts(1) + num.Qty(i+1)*prm.PosDriftTol})
	}

	// Two consecutive polls past the tolerance reduce the market.
	//
	// Every poll overwrites, so after one the two readings agree by
	// construction. `poll(d)` therefore reports an exchange figure `d` below
	// the current local one, which makes the disagreement at that poll exactly
	// `d` whatever the previous poll left behind.
	q := NewPortfolio()
	poll := func(d num.Qty) PositionEffects {
		return q.ReplacePositions(map[string]num.Qty{"A": q.Q("A") - d}, true, prm)
	}
	e1 := poll(2 * prm.PosDriftTol)
	if len(e1.Reduce) != 0 {
		t.Fatalf("one drifting poll reduced the market; H-POS-2 calls a "+
			"single disagreement normal: %+v", e1)
	}
	e2 := poll(2 * prm.PosDriftTol)
	if len(e2.Reduce) != 1 || e2.Reduce[0] != "A" {
		t.Fatalf("two consecutive drifting polls did not reduce: %+v", e2)
	}
	if sevOf(t, e2.Anomalies, "POSITION_DRIFT") != SEV2 {
		t.Fatalf("sustained tolerance drift is not SEV2: %v", e2.Anomalies)
	}
	if e2.Stop {
		t.Fatal("a tolerance drift requested a GLOBAL stop; H-POS-2 escalates " +
			"to the market, and only pos_drift_hard goes global")
	}

	// An agreeing poll clears the streak, so the next drifting poll is a first
	// one again.
	poll(0)
	e3 := poll(2 * prm.PosDriftTol)
	if len(e3.Reduce) != 0 {
		t.Fatalf("the drift streak survived an agreeing poll: %+v", e3)
	}

	// pos_drift_hard is global on a SINGLE poll, and is also strict.
	h := NewPortfolio()
	h.ApplyAck(AckFill{OrderID: "o", Ticker: "A", Side: quote.SideYes,
		Filled: prm.PosDriftHard})
	eqHard := h.ReplacePositions(map[string]num.Qty{"A": 0}, true, prm)
	if eqHard.Stop {
		t.Fatal("a drift EQUAL to pos_drift_hard went global; the comparison " +
			"is strictly greater")
	}
	h2 := NewPortfolio()
	h2.ApplyAck(AckFill{OrderID: "o", Ticker: "A", Side: quote.SideYes,
		Filled: prm.PosDriftHard + 1})
	hard := h2.ReplacePositions(map[string]num.Qty{"A": 0}, true, prm)
	if !hard.Stop {
		t.Fatal("a drift past pos_drift_hard did not request global " +
			"WINDING_DOWN on its very first poll")
	}
	if sevOf(t, hard.Anomalies, "POSITION_DRIFT") != SEV1 {
		t.Fatalf("hard drift is not SEV1: %v", hard.Anomalies)
	}
	// The exchange still overwrites. Recording that our model was wrong does
	// not license keeping it.
	if got := h2.Q("A"); got != 0 {
		t.Fatalf("q = %s after a hard-drift poll reported flat; the exchange "+
			"is authoritative in EVERY case, including this one", got.Wire())
	}
}

// TestCompleteRestingOrdersReplaceWholesaleOnlyOnCompleteWalk is H-POS-4.
//
// The asymmetry is sharper than H-POS-1's. An incomplete orders walk that
// retired a live order from our model leaves collateral committed and a
// fillable order on the book that no aggregate cap accounts for, while
// H-FAIL-3 is explicit that "off" means exchange-confirmed absent and not "we
// could not read it".
func TestCompleteRestingOrdersReplaceWholesaleOnlyOnCompleteWalk(t *testing.T) {
	p := NewPortfolio()
	a := LiveOrder{OrderID: "a", Ticker: "A", Side: quote.SideYes,
		Price4: cents4(40), Remaining: contracts(5)}
	b := LiveOrder{OrderID: "b", Ticker: "A", Side: quote.SideNo,
		Price4: cents4(55), Remaining: contracts(5)}

	if eff := p.ReplaceOrders([]LiveOrder{a, b}, nil, true); !eff.Applied {
		t.Fatal("a complete walk was not applied")
	}
	if got := p.LiveOrders(); len(got) != 2 {
		t.Fatalf("live orders = %v, want both", got)
	}

	// Wholesale: `a` disappearing from a complete walk retires it, and the map
	// is replaced rather than merged.
	p.ReplaceOrders([]LiveOrder{b}, nil, true)
	got := p.LiveOrders()
	if len(got) != 1 || got[0].OrderID != "b" {
		t.Fatalf("live orders = %v, want only b -- a complete walk REPLACES "+
			"the map, it does not merge into it", got)
	}

	// An incomplete walk changes nothing, and says so.
	eff := p.ReplaceOrders(nil, nil, false)
	if eff.Applied {
		t.Fatal("an incomplete orders walk was applied")
	}
	if got := p.LiveOrders(); len(got) != 1 {
		t.Fatalf("live orders = %v after an incomplete walk, want b still "+
			"present; an order we merely failed to read is still fillable", got)
	}
	if !hasClass(eff.Anomalies, "ORDER_WALK_INCOMPLETE") {
		t.Fatalf("an incomplete orders walk was silent: %v",
			anomalyClasses(eff.Anomalies))
	}

	// Foreign orders are reported separately and never enter the model.
	f := LiveOrder{OrderID: "zz", Ticker: "A", Side: quote.SideYes,
		Price4: cents4(30), Remaining: contracts(1)}
	fe := p.ReplaceOrders([]LiveOrder{b}, []LiveOrder{f}, true)
	if len(fe.Foreign) != 1 || fe.Foreign[0].OrderID != "zz" {
		t.Fatalf("foreign orders = %v, want exactly zz", fe.Foreign)
	}
	for _, o := range p.LiveOrders() {
		if o.OrderID == "zz" {
			t.Fatal("a foreign order entered our own resting model")
		}
	}
	if sevOf(t, fe.Anomalies, "FOREIGN_ORDER") != SEV2 {
		t.Fatalf("FOREIGN_ORDER is not SEV2: %v", fe.Anomalies)
	}
}

// TestOrderSideConflictIsRefusedRatherThanGuessed covers the one contradiction
// the ack/fill reconciliation cannot arithmetically resolve.
//
// An order acknowledged as a YES bid and then filled as a NO bid puts the
// direction of our own risk in dispute. Applying either reading moves q the
// wrong way by twice the fill, so the event is dropped and the caller is told
// to stop adding.
func TestOrderSideConflictIsRefusedRatherThanGuessed(t *testing.T) {
	p := NewPortfolio()
	p.ApplyAck(AckFill{OrderID: "o", Ticker: "A", Side: quote.SideYes,
		Filled: contracts(2)})
	eff := p.ApplyFills([]FillEvent{{TradeID: "t1", OrderID: "o", Ticker: "A",
		Side: quote.SideNo, Count: contracts(2)}}, owns("o"), Live, pollMs)

	if !eff.Stop {
		t.Fatal("a side contradiction did not request a global stop")
	}
	if sevOf(t, eff.Anomalies, "ORDER_SIDE_CONFLICT") != SEV1 {
		t.Fatalf("ORDER_SIDE_CONFLICT is not SEV1: %v", eff.Anomalies)
	}
	if got := p.Q("A"); got != contracts(2) {
		t.Fatalf("q = %s; the contradicting event must not be applied under "+
			"either reading", got.Wire())
	}
}

// TestOrderTickerConflictIsRefusedRatherThanGuessed is the side check's
// contradiction on the other axis.
//
// An order acknowledged on market A and then reported filling on market B is
// the exchange and this process disagreeing about WHICH market our risk is in.
// The order's remembered ticker is the one `settle` moves, so accepting the
// event books the contracts against A while the exchange holds them on B: both
// markets' `q` are then wrong, and H-POS-1's positions poll reports drift on
// two tickers with no way to say which reading was the mistake. The event is
// dropped and the caller is told to stop adding, exactly as for a side
// contradiction.
func TestOrderTickerConflictIsRefusedRatherThanGuessed(t *testing.T) {
	p := NewPortfolio()
	p.ApplyAck(AckFill{OrderID: "o", Ticker: "A", Side: quote.SideYes,
		Filled: contracts(2)})
	// Three contracts and not two: a fill that only corroborates the ack moves
	// nothing whatever the ticker says, and a test whose assertion holds
	// because the branch was never reached is not a test.
	eff := p.ApplyFills([]FillEvent{{TradeID: "t1", OrderID: "o", Ticker: "B",
		Side: quote.SideYes, Count: contracts(3)}}, owns("o"), Live, pollMs)

	if !eff.Stop {
		t.Fatal("a ticker contradiction did not request a global stop")
	}
	if sevOf(t, eff.Anomalies, "ORDER_TICKER_CONFLICT") != SEV1 {
		t.Fatalf("ORDER_TICKER_CONFLICT is not SEV1: %v", eff.Anomalies)
	}
	if got := p.Q("A"); got != contracts(2) {
		t.Fatalf("q(A) = %s, want the acknowledged 2; the contradicting event "+
			"was applied to the order's REMEMBERED market", got.Wire())
	}
	if got := p.Q("B"); got != 0 {
		t.Fatalf("q(B) = %s, want 0; no reading of the contradiction is safe "+
			"to apply", got.Wire())
	}
}

// TestAnomalyTextNamesTheRuleItEnforces keeps the alarms legible. An operator
// woken by a SEV1 gets the class and this text and nothing else.
func TestAnomalyTextNamesTheRuleItEnforces(t *testing.T) {
	p := NewPortfolio()
	eff := p.ApplyFills([]FillEvent{{TradeID: "t", OrderID: "x", Ticker: "A",
		Side: quote.SideYes, Count: contracts(1)}}, owns(), Live, pollMs)
	for _, a := range eff.Anomalies {
		if strings.TrimSpace(a.Text) == "" {
			t.Fatalf("%s carries no text", a.Class)
		}
	}
	if !hasClass(eff.Anomalies, "FOREIGN_FILL") {
		t.Fatalf("classes = %v", anomalyClasses(eff.Anomalies))
	}
}

// TestSeededPortfolioAdoptsWithoutManufacturingDrift is §7.5 step 1's seeding,
// and it is a separate constructor rather than a ReplacePositions call for one
// reason: ReplacePositions exists to compare a poll against a prior belief, and
// at startup there is no prior belief.
//
// A fresh Portfolio has an empty `q`, so every non-zero position the exchange
// reports would read as a full-magnitude delta. On any account holding more than
// `pos_drift_hard` that is an immediate SEV1 POSITION_DRIFT and a global stop --
// fired by the act of starting up correctly, on the exact path H-ORD-5 calls
// normal after a SIGKILL.
func TestSeededPortfolioAdoptsWithoutManufacturingDrift(t *testing.T) {
	p := cfg.Default()
	exch := map[string]num.Qty{
		"BIG":  p.PosDriftHard * 10,
		"SOME": num.QtyFromFloat(3),
		"FLAT": 0,
	}

	seeded := NewSeededPortfolio(exch, nil)

	if got := seeded.Q("BIG"); got != exch["BIG"] {
		t.Fatalf("q(BIG) = %s, want %s", got.Wire(), exch["BIG"].Wire())
	}
	if got := seeded.Q("SOME"); got != num.QtyFromFloat(3) {
		t.Fatalf("q(SOME) = %s, want 3.00", got.Wire())
	}
	// A flat entry is dropped rather than stored as zero, so the managed-set
	// union and a steady-state poll see the same markets.
	if _, held := seeded.Positions()["FLAT"]; held {
		t.Fatal("a flat market was seeded into the position map")
	}
	if n := len(seeded.Positions()); n != 2 {
		t.Fatalf("seeded %d markets, want 2", n)
	}
	if len(seeded.LiveOrders()) != 0 {
		t.Fatal("seeding invented resting orders")
	}

	// The contrast that motivates the constructor: the same input through
	// ReplacePositions on a fresh portfolio is a global stop.
	viaPoll := NewPortfolio().ReplacePositions(exch, true, p)
	if !viaPoll.Stop {
		t.Fatal("ReplacePositions on a fresh portfolio did not stop; this test " +
			"would not be demonstrating anything")
	}

	// And the very next real poll agrees with what was seeded, so no drift
	// streak was left behind either.
	eff := seeded.ReplacePositions(exch, true, p)
	if eff.Stop {
		t.Fatalf("the first poll after seeding requested a global stop: %v",
			anomalyClasses(eff.Anomalies))
	}
	if len(eff.Reduce) != 0 {
		t.Fatalf("the first poll after seeding sent %v to REDUCING", eff.Reduce)
	}
	for _, r := range eff.Records {
		if !r.Agreed {
			t.Fatalf("record %+v disagrees with the position it was seeded from", r)
		}
	}
}

// TestSeededPortfolioLeavesHistoricalFillsToSeedMode pins the other half: the
// constructor seeds `q` and records nothing about how it got there, so the
// historical fills that produced it still have their identities recorded by
// `Seed` mode without moving `q` a second time.
func TestSeededPortfolioLeavesHistoricalFillsToSeedMode(t *testing.T) {
	seeded := NewSeededPortfolio(map[string]num.Qty{"M": num.QtyFromFloat(2)},
		nil)

	fills := []FillEvent{
		{TradeID: "t1", OrderID: "o1", Ticker: "M", Side: quote.SideYes,
			Price4: 5000, Count: num.QtyFromFloat(1)},
		{TradeID: "t2", OrderID: "o1", Ticker: "M", Side: quote.SideYes,
			Price4: 5000, Count: num.QtyFromFloat(1)},
	}
	eff := seeded.ApplyFills(fills, owns("o1"), Seed, pollMs)
	if eff.Stop {
		t.Fatalf("seeding history stopped the harness: %v",
			anomalyClasses(eff.Anomalies))
	}
	if got := seeded.Q("M"); got != num.QtyFromFloat(2) {
		t.Fatalf("q = %s after replaying seeded history, want 2.00", got.Wire())
	}
	if len(eff.Owned) != 2 {
		t.Fatalf("owned fills = %d, want 2: the identities are recorded even "+
			"though q does not move", len(eff.Owned))
	}

	// The next live poll redelivers the same fills -- the endpoint is walked in
	// full every time -- and they must be deduplicated, not counted.
	again := seeded.ApplyFills(fills, owns("o1"), Live, pollMs)
	if got := seeded.Q("M"); got != num.QtyFromFloat(2) {
		t.Fatalf("q = %s after the same fills were redelivered live, want 2.00",
			got.Wire())
	}
	if len(again.Owned) != 0 {
		t.Fatalf("redelivered fills were counted as news: %d", len(again.Owned))
	}
}

// ---------------------------------------------------------------------------
// lip-da6 — the startup baseline
// ---------------------------------------------------------------------------

// baselineOf is the startup trade-id baseline in the form the constructor takes.
func baselineOf(ids ...string) map[string]struct{} {
	m := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		m[id] = struct{}{}
	}
	return m
}

// TestOutOfWindowOwnedFillIsSeededNotReplayed is `lip-da6`, and it is a test
// about two endpoints that disagree about what "history" means.
//
// §7.5 step 3 reads fills with `since = now - backfill_h`. The live poller reads
// them with a ZERO `since`. So a fill older than the window is invisible to
// startup -- never classified, never seeded, never in `seenTrade` -- and brand
// new to the first live poll, which replays it onto a `q` the positions walk had
// already seeded with its effect. `q_local` becomes history-plus-position while
// `q_exch` is the position, and one poll's disagreement equals the replayed
// quantity.
//
// Above `pos_drift_hard` that is a SEV1 `POSITION_DRIFT` and a durable global
// `WINDING_DOWN` with cause `portfolio_read`: the harness stopping itself over a
// discrepancy it manufactured by restarting. The sizes here are codex's
// reproducer -- an owned fill of 5.01 with the exchange position also 5.01,
// which is `pos_drift_hard` plus one quantum -- and the same case one quantum
// BELOW the threshold, where nothing is raised and `q` is silently wrong for a
// poll until the next positions walk overwrites it back.
//
// Both are the same defect and only the first one is loud. That is why the
// assertion is on `q` itself and not on the anomaly.
func TestOutOfWindowOwnedFillIsSeededNotReplayed(t *testing.T) {
	prm := cfg.Default()

	for _, tc := range []struct {
		name string
		size num.Qty
	}{
		{"above pos_drift_hard", prm.PosDriftHard + num.QtyFromFloat(0.01)},
		{"below pos_drift_hard", prm.PosDriftHard - num.QtyFromFloat(0.01)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// The exchange reports the position this fill already produced.
			seeded := NewSeededPortfolio(map[string]num.Qty{"M": tc.size},
				baselineOf("t-old"))

			eff := seeded.ApplyFills([]FillEvent{{
				TradeID: "t-old", OrderID: "o1", Ticker: "M",
				Side: quote.SideYes, Price4: 5000, Count: tc.size,
			}}, owns("o1"), Live, pollMs)

			if got := seeded.Q("M"); got != tc.size {
				t.Fatalf("q = %s after a live walk redelivered inherited "+
					"history, want %s unchanged: the positions walk already "+
					"accounted for this fill and applying it counts it twice",
					got.Wire(), tc.size.Wire())
			}
			if len(eff.Owned) != 0 {
				t.Fatalf("an inherited fill was reported as live: %d in Owned. "+
					"H-ORD-6 is first-observer-wins, so the row it writes with "+
					"backfilled=false is mislabelled permanently", len(eff.Owned))
			}
			if len(eff.OwnedBackfilled) != 1 {
				t.Fatalf("owned backfilled = %d, want 1: the fill is still "+
					"ours and still has to reach our_fill, with the other "+
					"label", len(eff.OwnedBackfilled))
			}
			if eff.Stop {
				t.Fatalf("an inherited fill requested a global stop: %v",
					anomalyClasses(eff.Anomalies))
			}

			// The positions poll that would have reported the manufactured
			// drift. It agrees, because nothing was replayed.
			pe := seeded.ReplacePositions(map[string]num.Qty{"M": tc.size},
				true, prm)
			if hasClass(pe.Anomalies, "POSITION_DRIFT") {
				t.Fatalf("a restart manufactured POSITION_DRIFT out of its own "+
					"history: %v", anomalyClasses(pe.Anomalies))
			}
			if pe.Stop {
				t.Fatal("the positions poll requested a global stop against a " +
					"position the exchange and the model agree about")
			}
			for _, r := range pe.Records {
				if r.Ticker == "M" && !r.Agreed {
					t.Fatalf("position_poll records a disagreement of %s on a "+
						"market neither side moved", r.Delta.Wire())
				}
			}
		})
	}
}

// TestOutOfWindowTakerFillStillRaisesHORD8 is the half that must NOT be
// suppressed.
//
// Seeding a baseline fill's arithmetic is not the same as ignoring it, and the
// difference is the whole argument for classifying baseline ids rather than
// marking them seen. A taker fill in our history is a fact about the harness
// whenever it happened, and it is exactly as true for a fill older than
// `backfill_h` as for one inside it.
//
// WHAT IT DOES NOT DO, since `lip-jhi`, is halt. H-Q-3 is a rule about what
// THIS incarnation does, and 8 of the 15 fills already on the real account are
// takers with non-zero fees: halting on inherited ones made the rule
// unsatisfiable by any account that has ever crossed the spread, and took the
// harness to a durable WINDING_DOWN on its first portfolio poll. So the
// inherited case raises `TAKER_FILL_INHERITED` at SEV2 and does not stop, while
// a fill this incarnation CAUSED still raises `TAKER_FILL` at SEV1 and does.
// Both halves are asserted below; suppressing either is the defect.
//
// `M-R-BASELINESILENT` marks the baseline seen instead, which is candidate A on
// the bead: smaller, and it loses this.
func TestOutOfWindowTakerFillStillRaisesHORD8(t *testing.T) {
	seeded := NewSeededPortfolio(map[string]num.Qty{"M": contracts(3)},
		baselineOf("t-old"))

	eff := seeded.ApplyFills([]FillEvent{{
		TradeID: "t-old", OrderID: "o1", Ticker: "M", Side: quote.SideYes,
		Price4: 5000, Count: contracts(3), IsTaker: true,
	}}, owns("o1"), Live, pollMs)

	if !hasClass(eff.Anomalies, "TAKER_FILL_INHERITED") {
		t.Fatalf("H-ORD-8 was suppressed for an inherited fill: %v\n\n"+
			"The observation must survive. Marking the baseline seen instead "+
			"(M-R-BASELINESILENT) is what loses it.",
			anomalyClasses(eff.Anomalies))
	}
	// SEV2, and this is load-bearing rather than cosmetic: qual/q01.go:215-222
	// fails the whole qualification on ANY anomaly labelled SEV1. Raising this
	// one at SEV1 would stop the harness halting and still fail q01 four to six
	// hours later, on one forbidden anomaly per inherited taker fill.
	if sev := sevOf(t, eff.Anomalies, "TAKER_FILL_INHERITED"); sev != SEV2 {
		t.Fatalf("TAKER_FILL_INHERITED sev = %v, want SEV2: q01 fails on any "+
			"SEV1-labelled anomaly, so an inherited fill raised at SEV1 "+
			"converts a fixed halt into a failed qualification", sev)
	}
	if eff.Stop {
		t.Fatal("an inherited taker fill requested a global stop. H-Q-3 is a " +
			"rule about what THIS incarnation does; 8 such fills already sit " +
			"on the real account and halting on them made the rule " +
			"unsatisfiable by any account that has ever crossed the spread " +
			"(lip-jhi)")
	}
	// The other half of the rule: a fill this incarnation actually caused still
	// halts, at SEV1, exactly as before. Without this assertion the change
	// above could be over-applied to every taker fill and nothing would notice.
	live := NewSeededPortfolio(map[string]num.Qty{"M": contracts(3)},
		baselineOf("t-old"))
	liveEff := live.ApplyFills([]FillEvent{{
		TradeID: "t-new", OrderID: "o1", Ticker: "M", Side: quote.SideYes,
		Price4: 5000, Count: contracts(1), IsTaker: true,
	}}, owns("o1"), Live, pollMs)
	if !hasClass(liveEff.Anomalies, "TAKER_FILL") || !liveEff.Stop {
		t.Fatalf("a LIVE taker fill did not raise H-ORD-8 and halt: stop=%v "+
			"classes=%v", liveEff.Stop, anomalyClasses(liveEff.Anomalies))
	}
	if sev := sevOf(t, liveEff.Anomalies, "TAKER_FILL"); sev != SEV1 {
		t.Fatalf("live TAKER_FILL sev = %v, want SEV1", sev)
	}
	if got := seeded.Q("M"); got != contracts(3) {
		t.Fatalf("q = %s, want 3.00: classification is not application",
			got.Wire())
	}
	if len(eff.OwnedBackfilled) != 1 {
		t.Fatalf("owned backfilled = %d, want 1", len(eff.OwnedBackfilled))
	}
}

// TestBaselineForeignFillStillStopsGlobally pins the in-window foreign fill
// across a restart, which the bead asks to be ASSERTED rather than inherited.
//
// The baseline is every trade the startup walks carried -- ours, foreign, and
// older than the window alike -- so a foreign fill is in it too. Foreign fills
// never reach §7.5's `ApplyFills`, which is handed only the owned subset, so
// they are not in `seenTrade` and the first live walk classifies them fresh.
//
// Since `lip-a3s` the two halves are split by WHEN the fill appeared. A
// disclaimed fill already on the account is reported at SEV2 and not halted on;
// one that appears while we are running still raises SEV1 `FOREIGN_FILL` and
// still requests the global stop. Both are asserted below, because suppressing
// the second would be the F2 catastrophe reached by a bookkeeping change.
//
// The split is forced by what the ledger can actually answer. `hstore.Ownership`
// resolves only from THIS store's reservation bookkeeping, so an order id it
// never reserved is disclaimed by `default:` (hstore/ledger.go:128-129) whether
// it belongs to a stranger or merely predates the store. Treating that as proof
// of a third party latched a durable WINDING_DOWN over all 15 of a real
// account's historical fills.
//
// Neither case moves `q`, so there is no arithmetic to seed either way.
func TestBaselineForeignFillStillStopsGlobally(t *testing.T) {
	seeded := NewSeededPortfolio(map[string]num.Qty{"M": contracts(2)},
		baselineOf("t-foreign"))

	eff := seeded.ApplyFills([]FillEvent{{
		TradeID: "t-foreign", OrderID: "not-ours", Ticker: "M",
		Side: quote.SideYes, Price4: 5000, Count: contracts(1),
	}}, owns("o1"), Live, pollMs)

	if !hasClass(eff.Anomalies, "FOREIGN_FILL_INHERITED") {
		t.Fatalf("a disclaimed baseline fill was not reported at all: %v\n\n"+
			"It must still be recorded -- it is not ours and never enters "+
			"our_fill -- it just does not latch.", anomalyClasses(eff.Anomalies))
	}
	if sev := sevOf(t, eff.Anomalies, "FOREIGN_FILL_INHERITED"); sev != SEV2 {
		t.Fatalf("FOREIGN_FILL_INHERITED sev = %v, want SEV2: q01 fails on any "+
			"SEV1-labelled anomaly, so SEV1 here fixes the halt and still "+
			"fails the qualification", sev)
	}
	if eff.Stop {
		t.Fatal("a fill already on the account before this process started " +
			"latched a durable, operator-only WINDING_DOWN (lip-a3s)")
	}

	// The half that must NOT soften: a disclaimed fill that appears WHILE WE
	// RUN is exactly what H-ORD-9 is about, and still stops the world.
	live := NewSeededPortfolio(map[string]num.Qty{"M": contracts(2)},
		baselineOf("t-foreign"))
	liveEff := live.ApplyFills([]FillEvent{{
		TradeID: "t-new-foreign", OrderID: "not-ours", Ticker: "M",
		Side: quote.SideYes, Price4: 5000, Count: contracts(1),
	}}, owns("o1"), Live, pollMs)

	if !hasClass(liveEff.Anomalies, "FOREIGN_FILL") {
		t.Fatalf("a foreign fill that appeared DURING the run was not "+
			"detected: %v", anomalyClasses(liveEff.Anomalies))
	}
	if sev := sevOf(t, liveEff.Anomalies, "FOREIGN_FILL"); sev != SEV1 {
		t.Fatalf("FOREIGN_FILL sev = %v, want SEV1", sev)
	}
	if !liveEff.Stop {
		t.Fatal("a third party trading the account while we run did not " +
			"request a global stop")
	}
	if len(eff.Foreign) != 1 {
		t.Fatalf("foreign = %d, want 1", len(eff.Foreign))
	}
	if len(eff.Owned)+len(eff.OwnedBackfilled) != 0 {
		t.Fatalf("a disclaimed fill was reported as ours: owned=%d backfilled=%d",
			len(eff.Owned), len(eff.OwnedBackfilled))
	}
	if got := seeded.Q("M"); got != contracts(2) {
		t.Fatalf("q = %s, want 2.00: a foreign fill is not ours to book",
			got.Wire())
	}
}

// TestBaselineDedupAndLiveFillsAreDistinguished is the two ends of the boundary.
//
// A walk that repeats a baseline trade id within itself must produce ONE effect,
// not one per copy -- the batch dedup runs before the baseline is consulted, and
// a baseline fill reported twice would be written to `our_fill` twice.
//
// And the boundary has to cut BOTH ways: a fill absent from the baseline is live
// however old the exchange stamps it, because no timestamp available to us is
// sound (`rest.Fills` filters client-side after a complete walk, an unreadable
// `ts` bypasses the filter, and the live poll asks for all history). It moves
// `q`, it is reported as `Owned`, and §7.9's canary is entitled to latch on it.
//
// The live fill here is on the SAME order as the inherited one, which is the
// case the seed arithmetic exists for: a partially filled order that spans the
// restart moves `q` by the remainder alone and never by the whole.
func TestBaselineDedupAndLiveFillsAreDistinguished(t *testing.T) {
	seeded := NewSeededPortfolio(map[string]num.Qty{"M": contracts(1)},
		baselineOf("t-old"))

	inherited := FillEvent{
		TradeID: "t-old", OrderID: "o1", Ticker: "M", Side: quote.SideYes,
		Price4: 5000, Count: contracts(1),
	}
	live := FillEvent{
		TradeID: "t-new", OrderID: "o1", Ticker: "M", Side: quote.SideYes,
		Price4: 5000, Count: contracts(2),
	}

	// The baseline fill twice in one walk, plus a fill that is genuinely news.
	eff := seeded.ApplyFills([]FillEvent{inherited, inherited, live},
		owns("o1"), Live, pollMs)

	if len(eff.OwnedBackfilled) != 1 {
		t.Fatalf("owned backfilled = %d, want 1: a duplicated trade id in one "+
			"walk is one fill", len(eff.OwnedBackfilled))
	}
	if len(eff.Owned) != 1 || eff.Owned[0].TradeID != "t-new" {
		t.Fatalf("owned = %v, want exactly the fill absent from the baseline",
			eff.Owned)
	}
	// 1.00 seeded from the positions walk, plus the 2.00 remainder of the live
	// fill on the same order -- and NOT the inherited 1.00 a second time.
	if got := seeded.Q("M"); got != contracts(3) {
		t.Fatalf("q = %s, want 3.00", got.Wire())
	}
	if eff.Stop {
		t.Fatalf("unexpected stop: %v", anomalyClasses(eff.Anomalies))
	}

	// Redelivery: the endpoint is walked in full forever, and the second offer
	// of both is news of neither.
	again := seeded.ApplyFills([]FillEvent{inherited, live}, owns("o1"),
		Live, pollMs)
	if len(again.Owned)+len(again.OwnedBackfilled) != 0 {
		t.Fatalf("redelivered fills were counted again: owned=%d backfilled=%d",
			len(again.Owned), len(again.OwnedBackfilled))
	}
	if got := seeded.Q("M"); got != contracts(3) {
		t.Fatalf("q = %s after redelivery, want 3.00", got.Wire())
	}
}
