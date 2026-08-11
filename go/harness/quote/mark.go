package quote

import (
	"lip/core"
)

// ---------------------------------------------------------------------------
// H-HALT-5 — the mark
// ---------------------------------------------------------------------------

// MarkUnavailable says why no mark could be formed. The zero value means a mark
// WAS formed, so a caller that ignores the reason cannot mistake a failure for
// a price.
type MarkUnavailable int

const (
	// MarkOK is a usable mark.
	MarkOK MarkUnavailable = iota
	// MarkNoBook is one or both sides absent entirely: no levels at all.
	MarkNoBook
	// MarkOneSided is a book with liquidity on only one side, or whose other
	// side is entirely our own resting size once H-Q-10's subtraction has run.
	MarkOneSided
)

// ExternalMark4 is H-HALT-5's mark: the midpoint of the EXTERNAL book,
// denominated in YES and returned in the exchange's own 1e-4 dollar quantum.
//
//	yesBid = ExternalBest(yes)
//	yesAsk = 100 - ExternalBest(no)
//	mark4  = (yesBid + yesAsk) * 50
//
// Three things about that arithmetic are load-bearing.
//
// It is EXTERNAL, so our own resting size is subtracted on both sides by
// H-Q-10. Valuing inventory against a touch we are ourselves posting is the
// same self-reference H-Q-8 exists to prevent, arriving at the loss floor
// instead of at the quote: our own bid would hold the mark up while the
// position it is valuing gets worse, and `pnl_kill` would fire late or not at
// all. `Book.Mid()` is exactly that unsubtracted reading and is therefore not
// usable here, which is why this function takes levels rather than a book.
//
// It is YES-DENOMINATED, because `q` is signed YES-positive (§8.1). A NO bid at
// p cents is a YES offer at 100-p (H-CO-1), so the NO side of the book is the
// ASK side of the YES market and the mid needs both.
//
// It is exact. `(yesBid + yesAsk) * 50` is the mid in 1e-4 dollars without ever
// dividing: a half-cent mid is 50 units, not a rounded 0 or 1. The value is
// compared against a loss floor at a boundary V1.13 tests from both sides, so a
// rounding step here would decide the kill on arithmetic rather than on price.
//
// BOTH sides must be present. A one-sided external book has no midpoint, and
// substituting the side that does exist would value inventory at a price nobody
// is offering.
func ExternalMark4(yes, no *core.Levels, oursYes, oursNo []Resting) (int64, MarkUnavailable) {
	if yes == nil || no == nil {
		return 0, MarkNoBook
	}
	bid := ExternalBest(yes, oursYes)
	ask := ExternalBest(no, oursNo)
	if !bid.Found || !ask.Found {
		// Found is false for an empty side AND for a side that is entirely
		// ours. Both are the same answer here -- there is no external price --
		// and the caller reports staleness versus absence from the gate's mark
		// state rather than from this distinction.
		return 0, MarkOneSided
	}
	return int64(bid.Price+(100-ask.Price)) * 50, MarkOK
}

// confidence: high
