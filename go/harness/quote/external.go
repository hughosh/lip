package quote

import (
	"lip/core"
	"lip/harness/num"
)

// ---------------------------------------------------------------------------
// H-Q-10 — the external book
// ---------------------------------------------------------------------------

// selfEpsilon is the size below which a level counts as empty after our own
// size has been removed, in contracts.
//
// Half a quantum, and derived rather than picked. Book sizes arrive as float64
// (H-CO-4) and our own size is exact (num.Qty), so the subtraction leaves a
// binary residue near 1e-16 when a level is entirely ours -- the same class as
// HR-026 one layer out. The threshold has to exceed that residue and must not
// reach the smallest real quantity the exchange can express, which is one
// quantum, 0.01 contracts. Half a quantum is the midpoint of that range: it can
// never discard a genuine level, and it discards every residue by fourteen
// orders of magnitude.
//
// `probebot.py:external_best` used 1e-9 here. That also works; this is stated
// in the units of the thing being measured instead of in the units of the
// floating-point error, so it stays correct if the exchange changes its
// quantum and this file does not.
const selfEpsilon = 0.5 / num.QtyScale

// Resting is our own aggregate quantity at one price on one side of one
// market's book.
//
// AGGREGATE (H-Q-5b): RESTING + SENDING + UNKNOWN + unconfirmed-cancel. An
// order we have asked the exchange to cancel but have not seen confirmed absent
// is still ours and is still in the book (H-FAIL-3), so leaving it out of this
// list would leave it in the external touch -- and we would then chase it.
type Resting struct {
	Price int     // integer cents (H-CO-3a)
	Size  num.Qty // aggregate, exact
}

// External is the result of H-Q-10's read.
type External struct {
	// Price is the best price on this side that somebody other than us is
	// bidding, in integer cents (H-CO-3a). Found is false when nobody else is
	// on the book at all -- an empty book, or a book that is entirely ours.
	Price int
	Size  float64
	Found bool

	// OverSubtracted records that our recorded resting size at some price
	// exceeded the book's own size there, so the subtraction was clamped at the
	// level rather than allowed to go negative.
	//
	// This is NOT necessarily a defect. Our order fills, the exchange removes
	// it from the book, and the delta reaches us before the fill report does --
	// in that window our model legitimately claims size the book no longer
	// shows. Clamping is what keeps that window from deleting somebody else's
	// real liquidity, which is the failure `probebot.py:external_levels`
	// documents for the dry-run case.
	//
	// Sustained, it means our order model and the book disagree, and the caller
	// is the one holding the clock that can tell those two apart.
	OverSubtracted bool
}

// ExternalBest is H-Q-10: the best price on `side` bid by somebody other than
// us, read from the book with our own resting size subtracted from its own
// price levels.
//
// > "Every touch used for a placement decision comes from external_best(side):
// > the book with our own resting size subtracted from its price level. Reading
// > the raw book means reading our own order and chasing ourselves."
//
// The failure this prevents is not hypothetical arithmetic. Live, the websocket
// book already carries our own orders, so the raw book cannot distinguish "we
// are at the touch alongside a real field" from "we are the last bid standing
// at a price everyone else has left" -- both read as best == our price. Under
// H-Q-6 the second case then requotes us against ourselves: we are behind
// nothing, so nothing moves, and under H-Q-8's stranded brake we would never
// snap back because our own order is the touch we are measuring against. M9 is
// exactly this mutation, and the test that catches it is that our own order
// must never move the quote.
//
// Subtraction happens only at OUR price levels. Every other level is somebody
// else's and is read as it stands.
func ExternalBest(lv *core.Levels, ours []Resting) External {
	var ext External
	if lv == nil {
		return ext
	}

	// Sum by price: the aggregate can legitimately be spread over more than one
	// order at the same price, and during a place-then-cancel requote (H-Q-9)
	// it is spread over two DIFFERENT prices at once. Subtracting only one of
	// them would leave the other in the touch, which is the self-chase this
	// function exists to prevent, reintroduced through the requote path.
	mine := make(map[int]float64, len(ours))
	for _, r := range ours {
		if r.Size <= 0 {
			continue
		}
		mine[r.Price] += r.Size.Float()
	}

	for _, price := range lv.SortedDesc() {
		size, ok := lv.Get(price)
		if !ok {
			continue
		}
		if m := mine[price]; m > 0 {
			if m > size {
				ext.OverSubtracted = true
				size = 0
			} else {
				size -= m
			}
		}
		if size > selfEpsilon {
			ext.Price, ext.Size, ext.Found = price, size, true
			return ext
		}
	}
	return ext
}

// BehindBy reports how many ticks our resting price is behind the external
// touch, which is the quantity H-Q-6 and H-Q-8 are both stated in.
//
// Both sides of a Kalshi market are BIDS (§4), so "behind" is always "lower",
// on both sides, and the sign convention needs no per-side special case. A
// negative result means we are IMPROVING the touch, which H-Q-2 forbids
// placing into but which is reachable transiently when the external touch
// falls away beneath us.
//
// Returns false when there is no external touch to be behind -- an empty book,
// or a book that is entirely ours. That is not "zero ticks behind": it is the
// case where the question has no answer, and a caller that read it as 0 would
// conclude we are exactly at a touch that does not exist.
func BehindBy(ourPrice int, ext External) (int, bool) {
	if !ext.Found {
		return 0, false
	}
	return ext.Price - ourPrice, true
}

// confidence: high
