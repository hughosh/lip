package risk

import (
	"testing"

	"lip/harness/num"
	"lip/harness/quote"
)

// pnlFill builds one owned fill. Prices are in the exchange's 1e-4 quantum and
// counts in hundredths of a contract, so the arguments are the wire's own
// units rather than a convenience the test has to convert back.
func pnlFill(id string, side quote.Side, price4 int64, count num.Qty,
	fee num.Money, tsMs int64) FillEvent {

	return FillEvent{
		TradeID: id, OrderID: "o-" + id, Ticker: "T", Side: side,
		Price4: price4, Count: count, Fee: fee, ExchangeTsMs: tsMs,
	}
}

func pnlEval(t *TradingPnL, q num.Qty, mark4 int64) PnLResult {
	return t.Evaluate([]PnLInput{{
		Ticker: "T", QExch: q, Mark4: mark4, MarkOK: true,
	}})
}

// TestTradingPnLReproducesTheSpecWorkedExample is H-HALT-5's own numbers.
//
// Red-team HR-021 pinned the definition with a worked case: "with q = +150
// acquired at 80 and the mid at 20, a fills-and-cost-basis reading sees -$90
// and halts, while a balance-delta reading sees nothing". That is the example
// the rule exists to make unambiguous, so it is the first thing to reproduce.
func TestTradingPnLReproducesTheSpecWorkedExample(t *testing.T) {
	p := NewTradingPnL()
	p.Apply(pnlFill("a", quote.SideYes, 8000, 15000, 0, 1))

	got := pnlEval(p, 15000, 2000)
	if !got.Evaluable {
		t.Fatalf("the spec's own example is unevaluable: %+v", got)
	}
	if want := num.MoneyFromDollars(-90); got.Total != want {
		t.Fatalf("150 contracts bought at $0.80 and marked at $0.20 give "+
			"pnl %s, want %s. This is the arithmetic H-HALT-5 pins by worked "+
			"example (HR-021); if it disagrees, the rule is being computed by "+
			"a different definition than the one the spec settled on",
			got.Total, want)
	}
}

// TestTradingPnLTreatsANoFillAsAShortYesAtTheComplement is the §8.1 sign
// convention, and it is the half of the ledger a YES-only test cannot reach.
//
// A NO fill is not a separate kind of position. `q` is signed YES-positive, so
// buying NO at p is holding YES at -q and the complementary price (H-CO-1). If
// that conversion is wrong, every NO fill enters the P&L with the wrong sign or
// the wrong price, and the loss floor measures the opposite of what it should.
func TestTradingPnLTreatsANoFillAsAShortYesAtTheComplement(t *testing.T) {
	p := NewTradingPnL()
	// One contract of NO at $0.60 -- a short YES at $0.40.
	p.Apply(pnlFill("a", quote.SideNo, 6000, 100, 0, 1))

	if q := p.Qty("T"); q != -100 {
		t.Fatalf("a NO fill left q = %s, want -1.00: §8.1 makes q signed and "+
			"YES-positive, so buying NO is being short YES", q.Wire())
	}
	// YES settles false, so the NO paid: the position is worth nothing and the
	// $0.40 received is kept.
	if got := pnlEval(p, -100, 0); got.Total != num.MoneyFromDollars(0.40) {
		t.Fatalf("NO at $0.60 with YES marked at $0.00 gives %s, want $0.40",
			got.Total)
	}
	// YES settles true, so the NO lost: $0.40 received against $1.00 owed.
	if got := pnlEval(p, -100, settlePrice4); got.Total != num.MoneyFromDollars(-0.60) {
		t.Fatalf("NO at $0.60 with YES marked at $1.00 gives %s, want -$0.60 "+
			"-- the $0.60 actually paid for the contract", got.Total)
	}
}

// TestTradingPnLChargesEachFeeExactlyOnce guards the double-charge.
//
// A fee is realised the moment it is paid and is never part of what the
// position cost. Folding it into the basis charges it again, a little at a
// time, on every later revaluation of that position.
func TestTradingPnLChargesEachFeeExactlyOnce(t *testing.T) {
	fee := num.MoneyFromDollars(0.07)
	p := NewTradingPnL()
	p.Apply(pnlFill("a", quote.SideYes, 5000, 100, fee, 1))

	// Marked at cost, so the only P&L is the fee.
	if got := pnlEval(p, 100, 5000); got.Total != -fee {
		t.Fatalf("a fill marked at its own price gives %s, want %s -- the fee "+
			"and nothing else", got.Total, -fee)
	}
	// Revaluing must not charge it again.
	if got := pnlEval(p, 100, 6000); got.Total != num.MoneyFromDollars(0.10)-fee {
		t.Fatalf("revaluing $0.10 up gives %s, want $0.10 less the one fee "+
			"(%s). A fee inside the cost basis is charged again on every "+
			"revaluation", got.Total, fee)
	}
}

// TestTradingPnLCrossingZeroReopensAtTheIncomingPrice is the path-dependence
// the average-cost rules exist for.
//
// Long 1 at $0.20, then a NO fill of 3 at $0.30 (a short YES of 3 at $0.70).
// The long closes at $0.70 for a realised gain, and the position that remains
// is SHORT 2 -- opened at $0.70, which is the only price ever paid for it. The
// old long's basis has nothing to do with those contracts, and carrying it
// across the crossing values the new position at a price it never traded at.
func TestTradingPnLCrossingZeroReopensAtTheIncomingPrice(t *testing.T) {
	p := NewTradingPnL()
	p.Apply(pnlFill("a", quote.SideYes, 2000, 100, 0, 1))
	p.Apply(pnlFill("b", quote.SideNo, 3000, 300, 0, 2))

	if q := p.Qty("T"); q != -200 {
		t.Fatalf("q = %s after crossing, want -2.00", q.Wire())
	}
	// Marked back at $0.70, the short is flat against its own entry and the
	// only P&L is the closed long: bought at $0.20, sold at $0.70.
	if got := pnlEval(p, -200, 7000); got.Total != num.MoneyFromDollars(0.50) {
		t.Fatalf("after crossing, marked at the crossing price, pnl is %s, "+
			"want $0.50 -- the realised gain on the closed long alone. Any "+
			"other figure means the residual short is being valued against a "+
			"basis it did not trade at", got.Total)
	}
}

// TestTradingPnLPartialCloseLeavesTheRestOpen covers the ordinary reduction.
func TestTradingPnLPartialCloseLeavesTheRestOpen(t *testing.T) {
	p := NewTradingPnL()
	p.Apply(pnlFill("a", quote.SideYes, 4000, 400, 0, 1))
	// Close one of the four at $0.90 (a NO fill at $0.10).
	p.Apply(pnlFill("b", quote.SideNo, 1000, 100, 0, 2))

	if q := p.Qty("T"); q != 300 {
		t.Fatalf("q = %s after a partial close, want 3.00", q.Wire())
	}
	// Three remain at cost, one was closed $0.50 up.
	if got := pnlEval(p, 300, 4000); got.Total != num.MoneyFromDollars(0.50) {
		t.Fatalf("pnl %s after closing one of four $0.50 up, want $0.50",
			got.Total)
	}
}

// TestTradingPnLIsIdempotentByTradeID is H-ORD-6's key doing its job.
//
// The same fill is offered by the §7.5 backfill, by the live poll, and by the
// durable replay a restart performs. Counting it more than once reports a loss
// that never happened -- and can fire the loss floor on arithmetic alone.
func TestTradingPnLIsIdempotentByTradeID(t *testing.T) {
	p := NewTradingPnL()
	f := pnlFill("a", quote.SideYes, 8000, 15000, 0, 1)
	if !p.Apply(f) {
		t.Fatal("the first application of a fill reported nothing new")
	}
	for i := 0; i < 3; i++ {
		if p.Apply(f) {
			t.Fatal("a repeated trade_id was accepted as new")
		}
	}
	if got := pnlEval(p, 15000, 2000); got.Total != num.MoneyFromDollars(-90) {
		t.Fatalf("pnl %s after replaying one fill four times, want -$90",
			got.Total)
	}
}

// TestTradingPnLReplaysLateHistoryInExchangeOrder is why the ledger sorts.
//
// The startup backfill walks 24h BACKWARDS while live polls push forward, so
// history genuinely arrives out of order. Because basis is path-dependent, the
// same fills applied in arrival order rather than exchange order can produce a
// different basis -- and a restart would then disagree with itself.
func TestTradingPnLReplaysLateHistoryInExchangeOrder(t *testing.T) {
	forward := NewTradingPnL()
	forward.Apply(pnlFill("a", quote.SideYes, 2000, 100, 0, 1))
	forward.Apply(pnlFill("b", quote.SideNo, 3000, 300, 0, 2))

	// The same two fills, learned in the opposite order.
	backward := NewTradingPnL()
	backward.Apply(pnlFill("b", quote.SideNo, 3000, 300, 0, 2))
	backward.Apply(pnlFill("a", quote.SideYes, 2000, 100, 0, 1))

	f, b := pnlEval(forward, -200, 7000), pnlEval(backward, -200, 7000)
	if f.Total != b.Total {
		t.Fatalf("the same two fills give %s learned forwards and %s learned "+
			"backwards. Average cost is path-dependent, so the ledger must "+
			"replay in exchange order rather than arrival order or a restart "+
			"computes a different basis from identical rows", f.Total, b.Total)
	}
}

// TestTradingPnLRefusesAPositionItHasNoFillsFor is the basis precondition.
//
// A quantity the ledger cannot explain is inventory acquired by something it
// never saw. There is no honest price to value it at, and valuing it at the
// mark would assume it was acquired at the current price -- the assumption most
// likely to hide a loss. Refusing is not-fired, never fired-or-safe.
func TestTradingPnLRefusesAPositionItHasNoFillsFor(t *testing.T) {
	p := NewTradingPnL()
	p.Apply(pnlFill("a", quote.SideYes, 5000, 100, 0, 1))

	got := pnlEval(p, 900, 5000)
	if got.Evaluable {
		t.Fatal("a position of 9.00 against fills for 1.00 was evaluated " +
			"anyway, so unexplained inventory is being valued at the mark")
	}
	if len(got.NoBasis) != 1 || got.NoBasis[0] != "T" {
		t.Fatalf("NoBasis = %v, want the one disagreeing market named", got.NoBasis)
	}
	if got.Total != 0 {
		t.Fatalf("an unevaluable result carried a total of %s; a figure that "+
			"could not be computed must not be reported as one", got.Total)
	}
}

// TestTradingPnLNeedsNoMarkWhileFlat is the exemption, and it matters because
// flat is the state a wound-down harness sits in.
//
// A flat market has no unrealised component, so no price can change its
// contribution. Requiring a mark for it would make a harness that has finished
// reducing unable to evaluate its own P&L for want of a book it no longer needs.
func TestTradingPnLNeedsNoMarkWhileFlat(t *testing.T) {
	p := NewTradingPnL()
	p.Apply(pnlFill("a", quote.SideYes, 4000, 100, 0, 1))
	p.Apply(pnlFill("b", quote.SideNo, 3000, 100, 0, 2))

	got := p.Evaluate([]PnLInput{{Ticker: "T", QExch: 0, MarkOK: false}})
	if !got.Evaluable {
		t.Fatalf("a flat market was unevaluable without a mark: %+v", got)
	}
	if want := num.MoneyFromDollars(0.30); got.Total != want {
		t.Fatalf("realised pnl %s while flat, want %s", got.Total, want)
	}
}

// TestTradingPnLWithoutAMarkIsUnevaluableRatherThanRealisedOnly is the trap
// H-HALT-5 names: never fired-or-safe by default.
//
// The tempting failure is to fall back to realised-only when no mark is
// available. That reports a SMALLER loss than the real one on exactly the
// position whose value is unknown, which is the direction that fails to halt.
func TestTradingPnLWithoutAMarkIsUnevaluableRatherThanRealisedOnly(t *testing.T) {
	p := NewTradingPnL()
	p.Apply(pnlFill("a", quote.SideYes, 8000, 15000, 0, 1))

	got := p.Evaluate([]PnLInput{{Ticker: "T", QExch: 15000, MarkOK: false}})
	if got.Evaluable {
		t.Fatal("a non-flat market with no mark was evaluated anyway")
	}
	if len(got.NoMark) != 1 || got.NoMark[0] != "T" {
		t.Fatalf("NoMark = %v, want the one unmarkable market named", got.NoMark)
	}
}

// TestTradingPnLCountsAMarketTheCallerDidNotAskAbout closes the omission hole.
//
// H-ORD-5b puts every market with a non-zero position under management,
// including ones outside the selection set. A total that silently skipped one
// would understate the loss by exactly the position nobody was looking at.
func TestTradingPnLCountsAMarketTheCallerDidNotAskAbout(t *testing.T) {
	p := NewTradingPnL()
	p.Apply(FillEvent{
		TradeID: "x", Ticker: "OTHER", Side: quote.SideYes,
		Price4: 8000, Count: 100, ExchangeTsMs: 1,
	})

	got := p.Evaluate(nil)
	if got.Evaluable {
		t.Fatal("a ledger holding a position in an unasked-about market " +
			"evaluated anyway, so that market's loss is invisible")
	}
}

// confidence: high
