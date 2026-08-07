package quote

import (
	"time"

	"lip/harness/cfg"
	"lip/harness/num"
)

// ---------------------------------------------------------------------------
// §6.5 — requote policy
// ---------------------------------------------------------------------------

// Trigger names which §6.5 rule wants the order moved, or why nothing moves.
type Trigger uint8

const (
	// TriggerNone: our order is where it should be, or the rule that would
	// move it is not satisfied.
	TriggerNone Trigger = iota

	// TriggerBehind is H-Q-6: the reference moved AGAINST us and our order is
	// at least one tick behind the touch.
	TriggerBehind

	// TriggerStranded is H-Q-8: our resting price is stale_bid_ticks or more
	// away from the external touch and we snap back to it.
	TriggerStranded

	// TriggerAbsent is not a requote at all -- there is nothing of ours on this
	// side and the caller wants presence restored (§6.6's P2). It is reported
	// here so a caller that asks "should this side move?" gets one answer
	// rather than having to know in advance which question it was asking.
	TriggerAbsent
)

func (t Trigger) String() string {
	switch t {
	case TriggerNone:
		return "none"
	case TriggerBehind:
		return "behind"
	case TriggerStranded:
		return "stranded"
	case TriggerAbsent:
		return "absent"
	}
	return "INVALID"
}

// Block names why a wanted move is not being made. It is distinct from
// TriggerNone: nothing wanted to move, versus something wanted to move and was
// refused.
type Block uint8

const (
	BlockNone Block = iota

	// BlockNoTouch: there is no external touch to move to. The book is empty,
	// or it is entirely ours -- in which case moving to "the touch" would mean
	// moving to our own price (H-Q-10).
	BlockNoTouch

	// BlockDebounce is H-Q-7: the new touch has not held for debounce_s.
	BlockDebounce

	// BlockSelfCross is H-CO-6: no price on this side is placeable against our
	// own resting order on the other side.
	BlockSelfCross

	// BlockPriceRange: the touch is outside the tradable range.
	BlockPriceRange
)

func (b Block) String() string {
	switch b {
	case BlockNone:
		return "none"
	case BlockNoTouch:
		return "no-touch"
	case BlockDebounce:
		return "debounce"
	case BlockSelfCross:
		return "self-cross"
	case BlockPriceRange:
		return "price-range"
	}
	return "INVALID"
}

// RequoteInput is everything §6.5 needs to decide one side of one market.
//
// Every clock reading is an input. This package holds no clock (§H-TOP-3), and
// the two durations here are deliberately DIFFERENT measurements rather than
// one reused twice -- see TouchHeld and StrandedFor.
type RequoteInput struct {
	Side Side
	Role Role

	// OurPrice and OurSize are our own aggregate resting state on this side:
	// RESTING + SENDING + UNKNOWN + unconfirmed-cancel (H-Q-5b). HasOurs is
	// false when there is nothing of ours on this side at all, which is
	// presence restoration rather than a requote.
	OurPrice int
	OurSize  num.Qty
	HasOurs  bool

	// Ext is the H-Q-10 touch: the book with our own size removed. Reading the
	// raw book here would compare our order against itself.
	Ext External

	// TouchHeld is how long the external touch has stood at its CURRENT price.
	// H-Q-7's debounce is against this.
	TouchHeld time.Duration

	// StrandedFor is how long our order has been stale_bid_ticks or more from
	// the touch -- the CONDITION's age, not the current price's age.
	//
	// These are two different clocks on purpose. Debouncing the stranded brake
	// against TouchHeld would let a touch that flickers every 200ms reset the
	// timer forever, so the one rule written to rescue an order nobody is
	// trading against would be defeated by exactly the churn that stranded it.
	StrandedFor time.Duration

	// OtherPrice is our own resting price on the OPPOSITE side, for H-CO-6.
	// HasOther distinguishes "nothing resting there" from "resting at 0".
	OtherPrice int
	HasOther   bool

	// Size is the aggregate this side should carry after the move, from
	// SizesFor. Headroom is how much ADDITIONAL aggregate may exist on this
	// side momentarily, which is H-Q-9's second condition: a place-then-cancel
	// has both orders live at once.
	Size     num.Qty
	Headroom num.Qty

	// AllowPlaceThenCancel enables H-Q-9's presence-preserving ordering.
	//
	// The pilot profile sets this FALSE and uses cancel-confirm-place
	// everywhere (pilot-plan §2.7, §7.1). H-Q-9 is a revenue optimisation --
	// it closes a presence gap, and presence gaps are revenue (S4) -- and it is
	// the one place in the write path where two of our orders are deliberately
	// live at the same price band at once. The pilot pays the gap and takes the
	// simpler invariant. The rule is implemented rather than omitted so that
	// turning it on later is this flag and not a new code path.
	AllowPlaceThenCancel bool
}

// Requote is §6.5's decision for one side.
type Requote struct {
	// Move is true when an order should be placed at Price.
	Move  bool
	Price int

	Trigger Trigger
	Block   Block

	// Kind is the leg structure the queue should carry this as (H-QUE-1).
	// Meaningful only when Move is true and something of ours is already
	// resting; a first placement is KindPlace.
	Kind Kind

	// Clamped is true when Price is below the external touch because H-CO-6
	// would not permit the touch itself.
	//
	// Only ever set on a REDUCING side. There, an exit a few ticks back is
	// enormously better than no exit and A4's obligation is to have one
	// resting, so the clamp is reported rather than refused. An adding side in
	// the same position is blocked outright (BlockSelfCross): A4 imposes no
	// obligation there, and a quote parked behind the touch is unremunerated
	// inventory risk.
	//
	// It is also the shape of a book worth looking at, so the caller is told.
	Clamped bool
}

// Decide is §6.5, evaluated.
//
//	H-Q-6  requote only when the reference moves AGAINST us, leaving our order
//	       >= 1 tick behind the touch. Do not requote when it moves in our
//	       favour -- our order is then the best bid and already scores at N = 0.
//	H-Q-7  debounce 250ms: requote only once the new touch has held.
//	H-Q-8  stranded brake: at stale_bid_ticks or more from the touch, snap back
//	       regardless of which direction it moved.
//	H-Q-9  place-then-cancel on an upward requote, adding side only.
//	H-Q-9a the cancel leg is eligible only after the replacement ACKs.
//
// **On the direction of H-Q-8.** The rule says "behind" and its rationale says
// the opposite: "this catches the case where our own order is the only thing
// holding a price level nobody else wants" -- which is our order ABOVE the
// external touch, not below it. "Regardless of which direction it moved"
// settles it. Both distances are implemented, and both are independently
// motivated:
//
//   - Eight ticks BELOW the touch is an order that has stopped scoring, which
//     is what H-Q-6 normally fixes and cannot when the gap opened during a
//     write outage.
//   - Eight ticks ABOVE it is an order improving the touch by eight ticks,
//     which is the maximal form of what H-Q-2 refuses to do deliberately, and
//     `reeval-verdict.md` §3a measured at-touch fills as already completely
//     adversely selected. Being eight ticks inside buys much more of that.
//
// Reading it as one direction only would leave the other case with no rule at
// all, so the union is both the safer reading and the one the rationale asks
// for.
func Decide(in RequoteInput, p cfg.Params) Requote {
	var r Requote

	// H-Q-10 / H-Q-1: there is no placement decision without an external touch.
	// A book that is entirely ours does not tell us where the market is; it
	// tells us where WE are, and moving to it is the self-chase.
	if !in.Ext.Found {
		r.Block = BlockNoTouch
		return r
	}
	if !ValidPrice(in.Ext.Price) {
		r.Block = BlockPriceRange
		return r
	}

	// H-Q-1 / H-Q-2: the target is the touch itself. Never inside it.
	//
	// **The clamp applies to the reducing side ONLY, and the asymmetry is the
	// whole point.** An earlier version clamped both sides, justified from A4 --
	// "an exit a few ticks back is enormously better than no exit". That
	// argument is sound and it is about REDUCERS. A4 imposes no obligation on an
	// adding side, so on that side the clamp buys nothing and costs the
	// difference: a quote parked several ticks behind the touch earns little or
	// nothing under DF = 0.5 while remaining perfectly fillable, so it is
	// unremunerated inventory risk taken deliberately.
	//
	// Concretely, at q = 0 with a stale NO order of ours at 45c and touches at
	// YES 60 / NO 39: clamping puts a YES quote at 54c, six ticks behind, which
	// then fills for 12 contracts when the field falls to 54 and hands us a
	// position we were not paid to take. The right answer on the adding side is
	// to place nothing and resolve the conflicting leg first.
	price := in.Ext.Price
	if in.HasOther {
		max, ok := MaxOpposite(in.OtherPrice)
		if !ok {
			r.Block = BlockSelfCross
			return r
		}
		if price > max {
			if in.Role != RoleReducing {
				r.Block = BlockSelfCross
				return r
			}
			price, r.Clamped = max, true
		}
	}

	// Nothing of ours on this side: presence restoration, not a requote. The
	// debounce does not apply -- H-Q-7 exists to stop us CHASING a touch that
	// has not settled, and there is no order here to chase with. A presence gap
	// is revenue (S4), and waiting 250ms to open one costs more than it saves.
	if !in.HasOurs {
		r.Move, r.Price, r.Trigger, r.Kind = true, price, TriggerAbsent, KindPlace
		return r
	}

	// Already where we want to be.
	if in.OurPrice == price {
		return r
	}

	behind, _ := BehindBy(in.OurPrice, in.Ext)
	away := behind
	if away < 0 {
		away = -away
	}

	switch {
	// H-Q-8 first. It is the rule that fires when the ordinary one has not,
	// and it is the only one that moves an order DOWN, so testing it after
	// H-Q-6 would let the favourable-direction branch return first and the
	// brake would be unreachable in exactly the case it exists for.
	case away >= p.StaleBidTicks:
		if in.StrandedFor < p.Debounce {
			r.Block = BlockDebounce
			return r
		}
		r.Trigger = TriggerStranded

	// H-Q-6: the reference moved against us. `behind > 0` is the touch above
	// our price -- on both sides, because both sides are BIDS (§4) and higher
	// is always better.
	case behind >= 1:
		if in.TouchHeld < p.Debounce {
			r.Block = BlockDebounce
			return r
		}
		r.Trigger = TriggerBehind

	// The reference moved in our FAVOUR and we are within the stranded
	// threshold. H-Q-6 declines this explicitly: our order is the best bid and
	// already scores at N = 0, so moving it down buys nothing and pays a write.
	default:
		return r
	}

	r.Move, r.Price = true, price
	r.Kind = requoteKind(in, price)
	return r
}

// requoteKind is H-Q-9's three conditions.
//
// Place-then-cancel is permitted ONLY when all of:
//
//  1. the new price still satisfies H-CO-6 against our other side;
//  2. the momentary aggregate size is within S_max and the capital cap; and
//  3. it is the adding side.
//
// Otherwise cancel-confirm-place: the replacement is not dispatched until the
// cancel is confirmed by response or sweep. On a reducing side the momentary
// aggregate would exceed |q|, which H-Q-5a forbids -- two individually-capped
// reducers that both fill flip the position (HR-004). The presence gap is
// accepted, because a reducer that overshoots is worse than a reducer that is
// briefly absent.
//
// The fourth condition is in H-Q-9's own first sentence and is easy to read
// past: "on an UPWARD requote. A requote moves our bid up." Place-then-cancel
// on a downward move would rest the new, lower order while the old, higher one
// is still live -- so the higher one remains the touch we are trying to leave,
// and the stranded brake's entire purpose is defeated for as long as the cancel
// takes. Downward moves are cancel-confirm-place regardless of side.
func requoteKind(in RequoteInput, newPrice int) Kind {
	if !in.AllowPlaceThenCancel {
		return KindCancelConfirmPlace
	}
	if in.Role != RoleAdding {
		return KindCancelConfirmPlace // clause 3
	}
	if newPrice <= in.OurPrice {
		return KindCancelConfirmPlace // upward requotes only
	}
	if in.Size > in.Headroom {
		return KindCancelConfirmPlace // clause 2
	}
	// Clause 1 is already satisfied: Decide clamps newPrice under MaxOpposite
	// before this is reached, so a self-crossing replacement cannot get here.
	return KindPlaceThenCancel
}

// confidence: high
