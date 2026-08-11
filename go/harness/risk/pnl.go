package risk

import (
	"sort"

	"lip/harness/num"
	"lip/harness/quote"
)

// ---------------------------------------------------------------------------
// H-HALT-5 — trading P&L
// ---------------------------------------------------------------------------

// settlePrice4 is $1.00 in the exchange's own quantum. A NO price is the YES
// price's complement about it (H-CO-1).
const settlePrice4 = 10000

// TradingPnL is the account's trading profit and loss, defined per H-HALT-5.
//
// It is a TRADING P&L and nothing else. Rewards, deposits, withdrawals and any
// foreign flow are excluded by construction: the only thing that can enter this
// type is an owned `FillEvent`, and there is no method by which a balance
// reaches it. That exclusion is the whole reason the rule is defined this way.
// Red-team HR-021 found two conforming implementations disagreeing completely
// on the same position -- a fills-and-cost-basis reading saw -$90 and halted
// while a balance-delta reading saw nothing -- and, worse, "an incoming $100
// LIP reward masks the drawdown entirely". The reward is the thing this system
// is measuring itself against. It is not a component of its P&L.
//
// It is SEPARATE FROM `Portfolio`, deliberately. `Portfolio` maintains
// `q_local` from acks and fills and then has it overwritten wholesale by
// H-POS-1's authoritative read; it is a position model, and its job is to be
// corrected. P&L is a cash-flow ledger over owned fills, where a correction is
// not available -- money that moved, moved. Putting cost basis on `Portfolio`
// would tie an accounting record to a field designed to be overwritten by
// something that carries no cash information at all.
type TradingPnL struct {
	markets map[string]*marketPnL
	// seen deduplicates by `trade_id` ACROSS markets, because that is the
	// scope of H-ORD-6's primary key. The same fill can arrive from the
	// startup backfill, a live poll and a durable replay on restart.
	seen map[string]struct{}
}

// marketPnL is one market's ledger.
//
// `cash` and `qty` are the two exact figures, and they are the only two the
// kill decision reads. `openBasis` exists for the realised/unrealised SPLIT,
// which is reporting rather than decision -- see Total.
type marketPnL struct {
	// fills is kept in `(ExchangeTsMs, TradeID)` order because average cost is
	// PATH-DEPENDENT: a fill that crosses through zero closes one position and
	// opens another at its own price, so the same set of fills applied in a
	// different order produces a different basis. History does not always
	// arrive in order -- the startup backfill walks 24h backwards while live
	// polls push forward -- so a late arrival replays rather than appends.
	fills []FillEvent
	// cash is the signed cash flow, YES-denominated, fees included. Exact.
	cash num.Money
	// qty is the fill-derived signed position, YES-positive. Exact.
	qty num.Qty
	// openBasis is the cash still tied up in the OPEN position, in the same
	// sign convention as `cash`: for a long holding that cost $4, it is -$4.
	// Realised is then `cash - openBasis` and unrealised is `qty*mark +
	// openBasis`, and their sum is `cash + qty*mark` with `openBasis`
	// cancelling -- which is why any rounding inside it cannot reach the kill.
	openBasis num.Money
}

// NewTradingPnL builds an empty ledger.
func NewTradingPnL() *TradingPnL {
	return &TradingPnL{
		markets: map[string]*marketPnL{},
		seen:    map[string]struct{}{},
	}
}

// Apply adds one owned fill and reports whether it was new.
//
// It is idempotent by `trade_id`. Every source of owned history -- the §7.5
// startup backfill, the live fill poll, and the durable `our_fill` replay a
// restart performs -- offers the same fills again, and a ledger that
// double-counted them would report a loss that never happened and could fire
// the kill on arithmetic.
func (t *TradingPnL) Apply(f FillEvent) bool {
	if f.TradeID == "" {
		return false
	}
	if _, dup := t.seen[f.TradeID]; dup {
		return false
	}
	t.seen[f.TradeID] = struct{}{}

	m := t.markets[f.Ticker]
	if m == nil {
		m = &marketPnL{}
		t.markets[f.Ticker] = m
	}

	// The common case is chronological arrival, which extends the ledger in
	// place. Anything else rebuilds it, because basis is path-dependent and a
	// fill inserted before existing ones changes what those ones did.
	if n := len(m.fills); n == 0 || fillBefore(m.fills[n-1], f) {
		m.fills = append(m.fills, f)
		m.advance(f)
		return true
	}
	m.fills = append(m.fills, f)
	sort.SliceStable(m.fills, func(i, j int) bool {
		return fillBefore(m.fills[i], m.fills[j])
	})
	m.cash, m.qty, m.openBasis = 0, 0, 0
	for _, e := range m.fills {
		m.advance(e)
	}
	return true
}

// fillBefore is the total order fills are replayed in: exchange time, then
// `trade_id` to break a tie. H-ORD-6 makes `trade_id` the primary key, so the
// pair orders any two distinct fills deterministically -- which is what lets a
// restart recompute an identical basis from the same rows.
func fillBefore(a, b FillEvent) bool {
	if a.ExchangeTsMs != b.ExchangeTsMs {
		return a.ExchangeTsMs < b.ExchangeTsMs
	}
	return a.TradeID < b.TradeID
}

// advance applies one fill to the running figures.
//
// The normalisation is H-HALT-5's, and it is YES-denominated because `q` is
// signed YES-positive (§8.1). A NO fill is a SHORT YES: it moves the position
// the other way, at the complementary price (H-CO-1). Both sides then share one
// cash line, `cash -= delta * yesPrice`, which is the property that makes the
// two directions impossible to get inconsistent with each other.
func (m *marketPnL) advance(f FillEvent) {
	yesPrice4 := f.Price4
	delta := f.Count
	if f.Side == quote.SideNo {
		yesPrice4 = settlePrice4 - f.Price4
		delta = -f.Count
	}

	// Exact: Qty is hundredths of a contract and Price4 is 1e-4 dollars, so
	// their product is already in the 1e-6 dollars Money counts. No division,
	// no rounding, at any position size.
	consideration := num.Money(int64(delta) * yesPrice4)
	m.cash -= consideration
	// The fee is a realised cost and NEVER part of the basis. Folding it into
	// average cost would make it reappear in unrealised P&L on every later
	// revaluation of the position it was paid on -- charging it once, then
	// charging it again a little at a time.
	m.cash -= f.Fee

	before := m.qty
	after := before + delta
	switch {
	case before.Sign() == 0 || before.Sign() == delta.Sign():
		// Opening, or adding in the same direction. The basis grows by exactly
		// what was paid, which IS the quantity-weighted average maintained
		// without ever computing an average.
		m.openBasis -= consideration
	case after.Sign() == 0:
		// Exactly flat. Nothing is open, so nothing is tied up.
		m.openBasis = 0
	case before.Sign() == after.Sign():
		// A partial close in the opposite direction. Basis leaves in the
		// proportion the position did.
		//
		// This is the one division in the type, and it is confined to the
		// realised/unrealised SPLIT: `openBasis` cancels out of Total, so a
		// residue here can never move the figure the kill compares. Multiply
		// before dividing so the proportion is taken at full precision.
		m.openBasis = num.Money(int64(m.openBasis) * int64(after) / int64(before))
	default:
		// A crossing. The old position is closed in full and a NEW one is
		// opened, holding only the residual, at THIS fill's price -- the old
		// average cost has nothing to do with contracts acquired now. Carrying
		// it across the crossing is how a long's cost basis silently becomes a
		// short's, which values the new position against a price that was
		// never paid for it.
		m.openBasis = -num.Money(int64(after) * yesPrice4)
	}
	m.qty = after
}

// PnLInput is one market's evaluation input: what the exchange says we hold,
// and what the book says it is worth.
type PnLInput struct {
	Ticker string
	// QExch is the AUTHORITATIVE position, after H-POS-1's overwrite. The
	// ledger's own fill-derived quantity is checked against it rather than
	// trusted over it.
	QExch num.Qty
	// Mark4 is the external mid in 1e-4 dollars, and MarkOK says a mark of
	// acceptable age exists at all (H-HALT-5, `pnl_mark_max_age_s`).
	Mark4  int64
	MarkOK bool
}

// PnLResult is the verdict.
//
// `Evaluable` false is NOT-FIRED, and it is never fired-or-safe. H-HALT-5 is
// explicit: "if no mark of acceptable age exists, the trigger cannot be
// evaluated: emit SEV2 and treat it as not-fired". The two reason slices exist
// so the operator's SEV2 can say WHICH market and WHY, because "P&L
// unevaluable" without a market is not a thing anyone can act on at 3am.
type PnLResult struct {
	Evaluable bool
	Total     num.Money
	// NoMark is every non-flat market with no usable mark.
	NoMark []string
	// NoBasis is every market whose fill-derived quantity disagrees with the
	// authoritative one.
	NoBasis []string
}

// Evaluate computes the trading P&L, or reports why it cannot be.
//
// Two independent preconditions, and both are refusals rather than guesses.
//
// A NON-FLAT market needs a mark. A flat one does not: it has no unrealised
// component, so its contribution is realised cash that no price can change. If
// every market is flat the total is fully evaluable with no mark at all, which
// matters because that is the state a wound-down harness sits in.
//
// The ledger's own quantity must AGREE with the authoritative one. Disagreement
// means the account holds contracts this ledger has no fills for -- a manual
// trade, a settlement, a fill we never saw -- and there is no honest basis to
// value them at. Valuing them at the mark would silently assume they were
// acquired at the current price, which is the assumption most likely to hide a
// loss. H-ORD-5a's principle applies: ignorance is reported, not resolved.
func (t *TradingPnL) Evaluate(in []PnLInput) PnLResult {
	var res PnLResult
	res.Evaluable = true

	for _, mk := range in {
		m := t.markets[mk.Ticker]
		var cash num.Money
		var qty num.Qty
		if m != nil {
			cash, qty = m.cash, m.qty
		}
		if qty != mk.QExch {
			res.NoBasis = append(res.NoBasis, mk.Ticker)
			res.Evaluable = false
			continue
		}
		res.Total += cash
		if qty.IsFlat() {
			continue
		}
		if !mk.MarkOK {
			res.NoMark = append(res.NoMark, mk.Ticker)
			res.Evaluable = false
			continue
		}
		// Exact, and the reason the comparison is written this way: `cash +
		// q*mark` is algebraically identical to `realised + q*(mark - avgCost)`
		// but performs no division, so the figure at the kill boundary is the
		// arithmetic of the fills rather than of a rounded average.
		res.Total += num.Money(int64(qty) * mk.Mark4)
	}

	// A market the ledger knows about but the caller did not ask about is a
	// market whose contribution is missing from the total. Silently omitting it
	// would understate a loss.
	for ticker, m := range t.markets {
		if m.qty.IsFlat() && m.cash == 0 {
			continue
		}
		if !containsTicker(in, ticker) {
			res.NoBasis = append(res.NoBasis, ticker)
			res.Evaluable = false
		}
	}

	sort.Strings(res.NoMark)
	sort.Strings(res.NoBasis)
	if !res.Evaluable {
		res.Total = 0
	}
	return res
}

func containsTicker(in []PnLInput, ticker string) bool {
	for _, mk := range in {
		if mk.Ticker == ticker {
			return true
		}
	}
	return false
}

// Qty is the ledger's own fill-derived position for a market.
func (t *TradingPnL) Qty(ticker string) num.Qty {
	if m := t.markets[ticker]; m != nil {
		return m.qty
	}
	return 0
}

// Realised is the closed-out cash for a market, fees included.
//
// Reporting only. Nothing in §12 reads it: the halt compares the TOTAL, and the
// realised/unrealised split is what the operator reads afterwards to understand
// which part of the loss was booked and which is still open.
func (t *TradingPnL) Realised(ticker string) num.Money {
	m := t.markets[ticker]
	if m == nil {
		return 0
	}
	return m.cash - m.openBasis
}

// confidence: high
