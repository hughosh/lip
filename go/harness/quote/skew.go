package quote

import (
	"lip/harness/cfg"
	"lip/harness/num"
)

// ---------------------------------------------------------------------------
// §6.2 — the skew function, the only inventory-reduction mechanism
// ---------------------------------------------------------------------------

// ReducingSide is §6.2's R: "no" if q > 0, "yes" if q < 0, none if q == 0.
//
// The second result is false at exactly q == 0, which is the case the caller
// must handle rather than defaulting: a flat market has no exit to keep alive,
// and A4's obligation is conditioned on q != 0. IsFlat is the permitted zero
// test (H-CO-4a) and is exact, so this is a decision and not an epsilon.
func ReducingSide(q num.Qty) (Side, bool) {
	switch {
	case q > 0:
		return SideNo, true
	case q < 0:
		return SideYes, true
	}
	return SideYes, false
}

// AddingSide is §6.2's A, the opposite of R. At q == 0 there is no reducing
// side and therefore no "opposite": both sides are adding, and the caller
// quotes them symmetrically.
func AddingSide(q num.Qty) (Side, bool) {
	r, ok := ReducingSide(q)
	if !ok {
		return SideYes, false
	}
	if r == SideNo {
		return SideYes, true
	}
	return SideNo, true
}

// SizeA is §6.2's adding-side taper:
//
//	          ┌ S                                                  |q| <= inv_soft
//	size_A =  │ S · (1 − (|q| − inv_soft)/(inv_hard − inv_soft))    inv_soft < |q| <= inv_hard
//	          └ 0                                                   |q| > inv_hard
//
// Evaluated in the exact algebraic rearrangement
//
//	size_A = S · (inv_hard − |q|) / (inv_hard − inv_soft)
//
// which is the same function -- 1 − (x−a)/(b−a) = (b−x)/(b−a) -- computed in
// integer quanta with one division instead of three float operations. Both
// endpoints fall out of the rearranged form exactly: |q| = inv_soft gives S and
// |q| = inv_hard gives 0, so the piecewise branches meet without a gap and the
// taper is continuous, which is what §6.2 requires of it. A cliff produces order
// churn against the rate limit for no scoring benefit.
//
// The integer division truncates toward zero, and in the taper branch every
// term is non-negative, so the result is FLOORED to the quantum. That direction
// is chosen rather than inherited: size_A is the risk-ADDING size, and rounding
// it up would place a fraction of a contract more exposure than the taper asks
// for, at exactly the inventory levels where the taper exists to be removing
// exposure. The residue is at most one hundredth of a contract.
//
// Returns 0 above inv_hard, which is the state that also sends the market to
// REDUCING (§5.2). Sizing and state agree there by construction rather than by
// two separate comparisons that could drift apart.
func SizeA(q num.Qty, p cfg.Params) num.Qty {
	a := q.Abs()
	switch {
	case a <= p.InvSoft:
		return p.S
	case a > p.InvHard:
		return 0
	}
	// inv_soft < |q| <= inv_hard, and Validate() guarantees
	// inv_soft < inv_hard, so the denominator is strictly positive.
	span := int64(p.InvHard - p.InvSoft)
	return num.Qty(int64(p.S) * int64(p.InvHard-a) / span)
}

// SizeR is §6.2's reducing-side size: min(|q|, S_max, funded).
//
// The cap at |q| is H-Q-5a and it is the one that matters: every positive
// partial fill must strictly decrease |q|, and a full fill must produce exactly
// zero. There is no q and no fill sequence for which a reducing order can
// change the sign of q (A12).
//
// An earlier version of this spec sized the reducer at |q| + S and argued a
// full fill left "a normal-sized position ... the system is self-correcting".
// That was false against §10.3's own parameters (HR-003): with S = 100 and
// inv_hard = 60, q = +61 posted 161 contracts, a full fill gave q = −100 --
// above inv_hard, so immediately back in REDUCING, now posting 200. Full fills
// alternate −100/+100 indefinitely, and REDUCING exits only at exactly zero. A
// reducer sized |q| + S is not a reducing quote; it is a reducing quote of |q|
// plus an ADDING quote of S on the opposite side, which is precisely what
// REDUCING exists to switch off.
//
// `funded` is a CONTRACT COUNT, not an amount of money: harness/risk converts
// the capital available to this market's exit (H-CAP-4's global priority and
// H-CAP-6's exemption from the concentration cap) into contracts at the
// reducing side's own price, because this package is pure and holds no prices.
// A negative or zero `funded` yields 0 -- an unfundable reducer is not sized
// down to something affordable, it is not placed, and A4 explicitly does not
// accept an unfundable intent as satisfying the obligation to have an exit.
//
// The result is an AGGREGATE across RESTING + SENDING + UNKNOWN +
// unconfirmed-cancel on that side (H-Q-5b), never a per-order size. The caller
// subtracts what is already working; it does not get to place this much again.
func SizeR(q, sMax, funded num.Qty) num.Qty {
	r := q.Abs()
	if sMax < r {
		r = sMax
	}
	if funded < r {
		r = funded
	}
	if r < 0 {
		return 0
	}
	return r
}

// Sizing is the pair of aggregate sizes §5.2's table states for one market, at
// one position, in one state.
//
// Both sides are AGGREGATE targets (H-Q-5b): the total quantity that may be
// working on that side, counting RESTING + SENDING + UNKNOWN +
// unconfirmed-cancel. Neither is a per-order count, and neither is what goes on
// the wire -- the caller subtracts what is already working, then validates the
// remainder under H-CO-4b.
type Sizing struct {
	// Add is the aggregate permitted on the adding side, and AddSide names it.
	// HasAdd is false when the state rests no adding quote at all, which is a
	// different statement from Add == 0: REDUCING must have its adding side
	// CANCELLED and exchange-confirmed absent (A8), while a SKEWED market whose
	// taper has reached zero simply has nothing more to place.
	Add     num.Qty
	AddSide Side
	HasAdd  bool

	// Reduce is the aggregate permitted on the reducing side. HasReduce is
	// false only when there is no reducing side to speak of -- q == 0 -- or the
	// state quotes nothing.
	Reduce     num.Qty
	ReduceSide Side
	HasReduce  bool

	// Symmetric records that this state quotes S on BOTH sides and designates
	// no reducer, which is true of QUOTING and of nothing else.
	Symmetric bool

	// FinalCancel is H-CLOSE-3: past final_lead, everything in this market is
	// cancelled and verified with a sweep, and NOTHING of ours rests into the
	// close.
	//
	// It is a separate field from `HasAdd == false && HasReduce == false`
	// because those two say "place nothing more" and this says "have nothing
	// resting", which is an obligation to act. IDLE and CLOSED also quote
	// nothing and impose no such duty.
	FinalCancel bool
}

// SizeInput is what §5.2's table needs to size one market.
//
// PastFinalLead is a parameter rather than a state because §5.2 defines the
// state set and final_lead does not add to it -- H-CLOSE-3 is an action inside
// SETTLING, not a sixth state. Making it an input means the caller, which holds
// the clock, cannot forget to supply it: there is no default that quietly means
// "not yet".
type SizeInput struct {
	State  MarketState
	Q      num.Qty
	Funded num.Qty

	// PastFinalLead is `close_time − now <= final_lead`.
	PastFinalLead bool
}

// SizesFor is §5.2's per-state size table, evaluated.
//
//	| State    | Adding side          | Reducing side              |
//	| IDLE     | —                    | —                          |
//	| QUOTING  | S at touch           | S at touch (symmetric)     |
//	| SKEWED   | tapered size_A       | min(|q|, S_max, funded)    |
//	| REDUCING | cancelled, confirmed | min(|q|, S_max, funded)    |
//	| SETTLING | cancelled, confirmed | min(|q|, S_max, funded)    |
//	| CLOSED   | —                    | —                          |
//
// **QUOTING is symmetric, and neither side is a reducer.** This is the one
// reading in this file that the spec states rather than derives, and getting it
// backwards breaks the strategy in one direction or the safety rule in the
// other, so it is recorded here.
//
// §5.2 gives QUOTING "size S at touch" in both columns. Applying size_R's |q|
// cap to the side opposite the position in QUOTING would cap that side at |q|,
// so at q = +0.01 the harness would quote 12 contracts against 0.01 and
// effectively stop two-sided market making at any nonzero position -- which is
// the entire strategy, abandoned inside the tolerance band that exists to say
// the position is small enough to ignore.
//
// It does not violate H-Q-5a, because H-Q-5a and A12 are scoped to a REDUCER:
// "no reducing order's aggregate quantity exceeds |q|; no fill sequence can
// change the sign of q VIA A REDUCER". QUOTING designates no reducer. A full
// fill of one side of a symmetric quote can indeed cross zero -- q = +3 filled
// 12 on the no side lands at −9 -- and that is ordinary two-sided market
// making, bounded by S, and handled by inv_hard sending the market to REDUCING
// on the next evaluation. The rule H-Q-5a protects is that the ORDER PLACED TO
// EXIT never overshoots the exit, and a symmetric quote is not that order.
//
// A market designates a reducer exactly when |q| > inv_soft takes it out of
// QUOTING, which is where the taper starts and where the exit begins to matter.
//
// **Past final_lead nothing is quoted at all, in any state.** H-CLOSE-3:
// "cancel everything in that market and verify with a sweep. Nothing of ours
// rests into the close." This is tested FIRST, before the state table, because
// the failure it prevents is a composition rather than a single wrong answer:
// the final cancel runs and confirms every order absent, and then the next
// sizing pass -- looking only at SETTLING and a nonzero q -- asks for the
// capped reducer again and undoes it. A4 is not violated by the silence,
// because A4's own exemption begins "after final_lead".
func SizesFor(in SizeInput, p cfg.Params) Sizing {
	var s Sizing

	if in.PastFinalLead && in.State != Closed {
		s.FinalCancel = true
		return s
	}
	q, funded := in.Q, in.Funded

	switch in.State {
	case Idle, Closed:
		return s

	case Quoting:
		// Symmetric: S on both sides, no reducer designated. The sides are
		// named from q only so a caller can label them; at q == 0 there is no
		// asymmetry to name and both are adding.
		s.Symmetric = true
		s.Add, s.HasAdd = p.S, true
		s.Reduce, s.HasReduce = p.S, true
		if a, ok := AddingSide(q); ok {
			s.AddSide = a
			s.ReduceSide, _ = ReducingSide(q)
		} else {
			s.AddSide, s.ReduceSide = SideYes, SideNo
		}
		return s

	case Skewed:
		if a, ok := AddingSide(q); ok {
			s.AddSide, s.HasAdd = a, true
			s.Add = SizeA(q, p)
		}
		// The reducing side is sized after the switch, shared with the two
		// states below.

	case Reducing, Settling:
		// The adding side is not sized to zero, it is CANCELLED and
		// exchange-confirmed absent (A8, H-FAIL-3). HasAdd stays false so a
		// caller cannot read Add == 0 as "place nothing more" when the
		// obligation is "have nothing resting".

	default:
		return s
	}

	if r, ok := ReducingSide(q); ok {
		s.ReduceSide, s.HasReduce = r, true
		s.Reduce = SizeR(q, p.SMax, funded)
	}
	return s
}

// confidence: high
