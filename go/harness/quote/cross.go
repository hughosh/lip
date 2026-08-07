package quote

import "fmt"

// ---------------------------------------------------------------------------
// H-CO-6 — never self-cross
// ---------------------------------------------------------------------------

// MinPrice and MaxPrice bound a placeable price in cents. A bid at 0 buys
// nothing and a bid at 100 pays the full settlement value for a certainty
// nobody is selling; the exchange's own tradable range is 1..99.
const (
	MinPrice = 1
	MaxPrice = 99
)

// Crosses reports whether a YES bid at `yes` cents and a NO bid at `no` cents
// are a self-cross: H-CO-6's `our_yes_price + our_no_price >= 100`.
//
// Both sides of a Kalshi market are BIDS in book terms (§4), so the two prices
// are not directly comparable the way a bid and an ask are -- the relation that
// matters is their SUM. A NO bid at 42c is a YES ask at 58c (H-CO-1), so a YES
// bid at 58c against a NO bid at 42c is our own bid meeting our own ask at the
// same price. At 100 we are paying the full settlement value across the pair,
// and above it we are paying more than the market can ever return: the two
// legs together are a guaranteed loss of `yes + no − 100` cents per contract,
// bought from ourselves.
//
// It is strict at 100, not at 101. A3 states the invariant as
// `our_yes_price + our_no_price < 100` for every resting pair of ours.
func Crosses(yes, no int) bool { return yes+no >= 100 }

// ValidPrice reports whether a price is inside the exchange's tradable range.
func ValidPrice(p int) bool { return p >= MinPrice && p <= MaxPrice }

// CheckPlacement is H-CO-6's precondition, evaluated before a placement.
//
// `side` and `price` are the order about to be dispatched. `otherSide` is our
// own current aggregate resting price on the OPPOSITE side, and `hasOther`
// distinguishes "we rest at 42c over there" from "we rest nothing over there" --
// which is not the same as resting at 0, and would pass the arithmetic if it
// were.
//
// > "Check before every placement, against our own current resting state, not
// > against the book."
//
// Against our own state, because the hazard is trading with ourselves, and the
// book contains everybody. A pair that crosses the BOOK is ordinary -- it means
// somebody else is willing to take the other half, which is what a market is.
// A pair that crosses OUR OWN resting order is a guaranteed loss with no
// counterparty risk to explain it, and H-CO-5's `taker_at_cross` does not save
// us: it cancels the incoming order on a self-match, so the placement is
// rejected rather than filled -- but a rejected placement is a presence gap,
// and on a reducing side it is a missing exit.
//
// "Our own current resting state" is AGGREGATE (H-Q-5b): an order we have
// requested a cancel for but not seen confirmed absent is still resting
// (H-FAIL-3), and mid-requote we can rest at two prices on one side, in which
// case the constraint is against the HIGHEST of them -- that is the one a new
// order on the other side can cross.
func CheckPlacement(side Side, price int, otherPrice int, hasOther bool) error {
	if !ValidPrice(price) {
		return fmt.Errorf("%s price %dc is outside the tradable range %d..%d",
			side, price, MinPrice, MaxPrice)
	}
	if !hasOther {
		return nil
	}
	yes, no := price, otherPrice
	if side == SideNo {
		yes, no = otherPrice, price
	}
	if Crosses(yes, no) {
		return fmt.Errorf("placing %s at %dc against our own resting %s at "+
			"%dc self-crosses: yes %dc + no %dc = %dc >= 100 (H-CO-6, A3)",
			side, price, side.Opposite(), otherPrice, yes, no, yes+no)
	}
	return nil
}

// Opposite is the other side of the same market.
func (s Side) Opposite() Side {
	if s == SideYes {
		return SideNo
	}
	return SideYes
}

// MaxOpposite is the highest price we may rest on `side` given that we already
// rest at `otherPrice` on the other side, or false when no price on this side
// is placeable at all.
//
// This is Crosses solved for the free variable, and it exists so that a caller
// which has a price it WANTS -- the external touch, under H-Q-1 -- can clamp
// rather than discover the rejection. Under H-Q-2 the harness never improves
// the touch, so the only legal responses to a clamped price are to place at the
// clamp or to place nothing; it never bids above the touch to escape the
// constraint.
func MaxOpposite(otherPrice int) (int, bool) {
	max := 99 - otherPrice
	if max > MaxPrice {
		max = MaxPrice
	}
	if max < MinPrice {
		return 0, false
	}
	return max, true
}

// confidence: high
