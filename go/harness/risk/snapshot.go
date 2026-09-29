// Package risk is the pure position model, capital allocator and invariant
// assertions of harness-spec.md §8, §10.2, §17 V3.
//
// Pure by the same rule that makes core pure (port-spec §3): no clock, no I/O,
// no goroutines. Callers pass time in.
package risk

import (
	"time"

	"lip/harness/num"
	"lip/harness/quote"
)

// Side is the shared book-side type, independent of quote and risk layering.
type Side = num.Side

const (
	Yes = num.SideYes
	No  = num.SideNo
)

// SideSnap is our own resting presence on one side of one market.
type SideSnap struct {
	// PriceCents is our resting price in that side's own cents, 0 if absent.
	PriceCents int
	// Resting is the aggregate exchange-confirmed resting quantity.
	Resting num.Qty
	// AtRisk is the aggregate RESTING + SENDING + UNKNOWN + unconfirmed-cancel
	// quantity (H-Q-5b, A11). Every size cap is evaluated against THIS, never
	// against Resting and never against a single order.
	//
	// H-FAIL-3: an order we have merely requested a cancel for is live and
	// fillable, so it stays in AtRisk until the exchange confirms it absent.
	AtRisk num.Qty
	// FieldScore is the field's qualifying score on this side, excluding us.
	FieldScore float64
	// TouchCents is external_best for this side -- the book with our own
	// resting size subtracted (H-Q-10). Reading the raw book means reading our
	// own order and chasing ourselves.
	TouchCents int
}

// MarketSnap is one market's published state. Immutable once published.
type MarketSnap struct {
	Ticker   string
	State    quote.MarketState
	Q        num.Qty
	Selected bool
	// Gated is true when the market's own qualifying walk fails on either side
	// (core.Book.Qualifies() == 0). Reward for a gated interval is zero for
	// every participant, so the interval is excluded from the uptime
	// denominator rather than counted as our downtime (H-Q-4).
	Gated bool
	// BookActionable is false while the book is quarantined after any
	// disconnect, clean or not (H-FAIL-5), or when portfolio truth has aged out
	// (H-FAIL-4). A13 forbids taking any placement decision from it.
	BookActionable bool
	Sides          [2]SideSnap
}

// Snapshot is the immutable view the owner goroutine publishes after each tick
// and the monitor reads via atomic.Pointer. It is the ONLY channel between
// them: the monitor holds no reference to the quote engine, the order queue or
// the REST client, and so cannot be stopped by any decision they make (I2).
//
// H-TOP-5. Seq and PubMono are set by the owner AT PUBLICATION, and Seq
// advances monotonically. They exist because I2 makes the monitor unstoppable
// but does NOT make it truthful:
//
//	The owner publishes q = 0, then deadlocks. One second later a resting
//	order fills. The monitor re-reads the same pointer forever, stamps a
//	fresh snap row every second, and pushes hourly heartbeats reporting
//	q = 0 -- while real inventory grows unobserved. A5 passes at every tick.
//	F18 never fires, because the process and its heartbeat are both alive.
//
// That is probebot.py's observable -- confident silence about live risk --
// reached by a different route than probebot.py's `break`. A monitor that
// cannot fail is not the same thing as a monitor that cannot lie, and an
// earlier version of this design had conflated them.
//
// I3: the monitor must be able to detect that its own input has stopped
// advancing, and must say so rather than continue republishing it.
type AccountSnapshot struct {
	TruthFresh bool
	CapitalMax num.Money
	Exposures  []Exposure
	TradingPnL PnLResult
}

type Snapshot struct {
	// Seq advances by at least one on every publication. A repeat means the
	// owner has not published since we last looked.
	Seq uint64
	// PubMono is a monotonic stamp taken at publication. Monotonic, not wall,
	// because a clock step (F21) must not be readable as staleness.
	PubMono time.Duration
	// PubWallMs is for rows and heartbeats only. Never compared for freshness.
	PubWallMs int64

	Global  quote.GlobalState
	Markets []MarketSnap
	// Account is immutable aggregate evidence from the same owner publication.
	// TradingPnL.Evaluable is false when basis, marks or truth are unavailable.
	Account AccountSnapshot
}

// Selected returns the markets A5 must find a sample for.
func (s *Snapshot) SelectedTickers() []string {
	out := make([]string, 0, len(s.Markets))
	for i := range s.Markets {
		if s.Markets[i].Selected {
			out = append(out, s.Markets[i].Ticker)
		}
	}
	return out
}

// confidence: high
