package quote

import (
	"time"

	"lip/harness/cfg"
	"lip/harness/num"
)

// ---------------------------------------------------------------------------
// §5.1, §5.2, §9 — the two state machines
// ---------------------------------------------------------------------------
//
// H-HALT-2: there is exactly one place in the codebase that transitions the
// global state, and exactly one that transitions a market's state. Those two
// are NextGlobal and NextMarket below, and a grep for them enumerates every
// stop path in the system. Both return the trigger alongside the state, because
// A9 requires every transition to write a state_event row carrying it and a
// caller that had to infer the reason would eventually infer it wrongly.
//
// Neither reads a clock. Every deadline arrives as a duration the caller
// measured, so §17's scenarios can step time without the machine noticing.

// MarketTrigger names why a market's state changed. It is the `trigger` column
// of `state_event` (A9) and the text an operator reads at 3am.
type MarketTrigger uint8

const (
	MTNone MarketTrigger = iota
	MTSelected
	MTInvSoft      // |q| crossed inv_soft
	MTInvSoftClear // |q| fell back under inv_soft
	MTInvHard      // |q| crossed inv_hard
	MTStop         // a market-scoped §12 trigger
	MTProgramEnd   // H-CLOSE-1
	MTGlobalStop   // the global state stopped adding
	MTCloseLead    // H-CLOSE-2
	MTTradingClose // H-CLOSE-3 / settlement
	MTFlat         // q reached exactly zero
	MTDeselected
)

func (t MarketTrigger) String() string {
	switch t {
	case MTNone:
		return "none"
	case MTSelected:
		return "selected"
	case MTInvSoft:
		return "inv_soft"
	case MTInvSoftClear:
		return "inv_soft_clear"
	case MTInvHard:
		return "inv_hard"
	case MTStop:
		return "market_stop"
	case MTProgramEnd:
		return "program_end"
	case MTGlobalStop:
		return "global_stop"
	case MTCloseLead:
		return "close_lead"
	case MTTradingClose:
		return "trading_close"
	case MTFlat:
		return "flat"
	case MTDeselected:
		return "deselected"
	}
	return "INVALID"
}

// MarketInput is everything §5.2 and §9 need to place one market.
//
// The stop conditions arrive already detected. This package does not decide
// whether a feed is wedged or a reject rate is too high -- it decides what a
// market in that condition may do, which is the part that must be provably
// right and must not depend on a clock or a socket.
type MarketInput struct {
	State MarketState
	Q     num.Qty

	// Global is §5.1's state. A market may not add while the global state
	// forbids it, whatever its own state says (I1).
	Global GlobalState

	// Selected is §10's selection decision. It is the only way into QUOTING.
	Selected bool

	// Stop is any market-scoped §12 trigger: reject rate over 10%/50, feed
	// wedge (F5), disconnect beyond disconnect_reduce_s, a book that has been
	// quarantined. Each sends the market to REDUCING -- never to a state that
	// cancels everything, which is the inversion the whole design turns on.
	Stop bool

	// ProgramEnded is H-CLOSE-1: at end_date there is no reward left to earn,
	// so the adding quote has no purpose. The reducing quote stays, and is now
	// genuinely free -- there is no reward left for it to forfeit.
	ProgramEnded bool

	// UntilClose is close_time − now. HasClose is false when the schedule has
	// not been read yet, which is NOT the same as a close far away: a market
	// whose close_time we do not know is one we cannot enforce a lead on, and
	// H-CLOSE-0 makes that the caller's problem to escalate rather than this
	// function's to assume away.
	UntilClose time.Duration
	HasClose   bool

	// TradingClosed is the close observed, not merely computed. can_close_early
	// markets settle before close_time (H-CLOSE-4), so the arithmetic is not
	// authoritative and this flag is.
	TradingClosed bool
}

// NextMarket is §5.2 and §9, evaluated. It is the ONLY place a market's state
// changes (H-HALT-2).
//
// The order of the tests is the rule, not an implementation detail. Read top to
// bottom it is a precedence list, and two of the orderings are load-bearing:
//
//  1. **The close lead outranks the inventory triggers.** §5.2 draws
//     REDUCING → SETTLING at close_time − close_lead, so a market that is both
//     over inv_hard and inside its close lead is SETTLING. The two states
//     behave alike -- adding side cancelled, capped reducer resting -- but only
//     SETTLING carries H-CLOSE-3's final cancel, and a market that stayed in
//     REDUCING through the close would rest orders into it.
//
//  2. **A stopped market with q == 0 goes to IDLE, and IDLE only leaves for
//     QUOTING when nothing is stopping it.** Otherwise a wedged feed that
//     happens to be flat would exit REDUCING to IDLE and re-enter QUOTING on
//     the next selection tick, resuming adding into the condition that stopped
//     it. Making the guard part of the edge is what stops that being a
//     property of whoever calls the selector.
func NextMarket(in MarketInput, p cfg.Params) (MarketState, MarketTrigger) {
	same := func() (MarketState, MarketTrigger) { return in.State, MTNone }

	// CLOSED is terminal. Nothing reopens a settled market.
	if in.State == Closed {
		return same()
	}
	if in.TradingClosed {
		return Closed, MTTradingClose
	}

	// H-CLOSE-2. Entered from QUOTING, SKEWED or REDUCING per §5.2, and from
	// IDLE as well -- a flat market inside its own close lead must not be
	// re-selected, and routing it here makes that structural rather than a
	// property the selector is trusted to have.
	//
	// SETTLING is NOT an exemption from having an exit (H-CLOSE-2a). Until
	// final_lead a SETTLING market with q != 0 must still have a reducing quote
	// resting or an in-flight intent to place one; SizesFor keeps that
	// obligation, and M13 -- cancel everything on entry and never place the
	// capped reducer -- is the mutation that must not survive.
	if in.HasClose && in.UntilClose <= p.CloseLead {
		if in.State != Settling {
			return Settling, MTCloseLead
		}
		return same()
	}
	// A market in SETTLING whose close has moved back out beyond the lead falls
	// through to the ordinary rules below. H-CLOSE-0 treats close_time as not
	// static, so this is reachable, and leaving the market in SETTLING would
	// abandon its adding side indefinitely on the strength of a deadline that
	// no longer exists.
	//
	// An earlier version routed it through a "conservative" path that returned
	// REDUCING for any nonzero position. That was worse, not safer: REDUCING
	// leaves only at exactly zero, so a market that came back from a close
	// window holding q = +5 -- below inv_hard = 7, ordinary SKEWED inventory --
	// was trapped one-sided for the remaining five hours, at roughly two thirds
	// of the market's reward rate, or gated out entirely if the field alone
	// misses Target Size on the abandoned side. The comment claiming it would
	// "re-earn its adding side on the next tick" was simply false.

	// A market-scoped stop, program end, or a global state that has stopped
	// adding. All three land in the same place, which is the inversion §5.2
	// states outright: "there is no HALTED per-market state that cancels
	// everything. A market-level halt trigger sends the market to REDUCING,
	// which keeps the exit alive."
	stopped, trig := marketStopped(in)
	if stopped {
		if in.Q.IsFlat() {
			// Nothing to reduce. IDLE, and the guard on the IDLE → QUOTING
			// edge below is what keeps it from resuming.
			if in.State != Idle {
				return Idle, trig
			}
			return same()
		}
		if in.State != Reducing {
			return Reducing, trig
		}
		return same()
	}

	// §5.2's inventory ladder.
	a := in.Q.Abs()
	switch {
	case a > p.InvHard:
		if in.State != Reducing {
			return Reducing, MTInvHard
		}
		return same()

	case in.State == Reducing:
		// REDUCING leaves only at exactly zero (§5.2). Not "under inv_hard" --
		// that would let a market that reduced from 10 to 6 resume adding with
		// six contracts of unwanted inventory, and the exit it just used would
		// be replaced by a quote on the other side.
		//
		// IsFlat is the permitted zero test (H-CO-4a) and is exact. A float
		// equality here was gating this very transition, and a residue near
		// 5.6e-17 kept REDUCING from ever reaching IDLE.
		if in.Q.IsFlat() {
			return Idle, MTFlat
		}
		return same()

	case !in.Selected:
		if in.Q.IsFlat() {
			if in.State != Idle {
				return Idle, MTDeselected
			}
			return same()
		}
		// H-SEL-11: never deselect a market that still holds inventory. If the
		// caller has done it anyway, the exit stays alive rather than the
		// position being abandoned.
		if in.State != Reducing {
			return Reducing, MTDeselected
		}
		return same()

	case a > p.InvSoft:
		if in.State != Skewed {
			return Skewed, MTInvSoft
		}
		return same()

	default:
		if in.State != Quoting {
			t := MTSelected
			if in.State == Skewed {
				t = MTInvSoftClear
			}
			return Quoting, t
		}
		return same()
	}
}

// marketStopped collects the three conditions that send a market out of its
// quoting states, in the order they should be reported.
func marketStopped(in MarketInput) (bool, MarketTrigger) {
	switch {
	case in.Stop:
		return true, MTStop
	case in.ProgramEnded:
		return true, MTProgramEnd
	case !in.Global.AddsRisk():
		return true, MTGlobalStop
	}
	return false, MTNone
}

// ---------------------------------------------------------------------------
// §5.1 — the global state machine
// ---------------------------------------------------------------------------

// GlobalTrigger is the `trigger` column for a global transition.
type GlobalTrigger uint8

const (
	GTNone        GlobalTrigger = iota
	GTLatch                     // H-HALT-4: a durable latch from a previous incarnation
	GTTruthFailed               // portfolio truth unreadable at startup
	GTReconciled                // H-ORD-5: a complete successful reconciliation
	GTStop                      // any §12 global trigger
	GTDrained                   // every market flat or closed
	GTInventory                 // inventory reappeared under DRAINED
)

func (t GlobalTrigger) String() string {
	switch t {
	case GTNone:
		return "none"
	case GTLatch:
		return "halt_latch"
	case GTTruthFailed:
		return "truth_unreadable"
	case GTReconciled:
		return "reconciled"
	case GTStop:
		return "global_stop"
	case GTDrained:
		return "drained"
	case GTInventory:
		return "inventory_reappeared"
	}
	return "INVALID"
}

// GlobalInput is §5.1's inputs.
type GlobalInput struct {
	State GlobalState

	// Latched is the durable halt latch on disk (H-HALT-4), read before
	// anything else at STARTING. The file is authoritative if it and the
	// database disagree, because SQLite may be the thing that failed.
	Latched bool

	// TruthReadable is whether portfolio truth can be read at all.
	// Reconciled is H-ORD-5's complete successful reconciliation.
	TruthReadable bool
	Reconciled    bool

	// Stop is any §12 global trigger: inv_kill, pnl_kill, a taker fill,
	// insufficient_balance, hard position drift, the harness.stop sentinel,
	// SIGINT/SIGTERM.
	Stop bool

	// AnyInventory is true while any market holds q != 0.
	AnyInventory bool

	// AnyLiveOrder is true while ANY order of ours is RESTING, SENDING,
	// UNKNOWN, or cancel-requested but not yet exchange-confirmed absent
	// (H-FAIL-3, H-Q-5b).
	//
	// DRAINED requires this to be false as well as AnyInventory. An account
	// that is flat but still has fillable orders on the book is not drained: it
	// is one ignored cancel away from being long again, and DRAINED is a state
	// that rests no reducer. The sequence is ordinary rather than exotic --
	// SIGTERM enters WINDING_DOWN, cancels are dispatched, and inventory reads
	// zero before any of their responses land.
	AnyLiveOrder bool
}

// NextGlobal is §5.1, evaluated. It is the ONLY place the global state changes
// (H-HALT-2).
//
// **There is deliberately no RUNNING → UNKNOWN_RISK edge**, and the omission is
// the interesting part. UNKNOWN_RISK reads as the natural home for "position
// polls are failing", but it places nothing AND reduces nothing -- §5.1 gives
// it "no reducing quotes, q is unknown, so no reducer can be sized". Entering
// it from RUNNING would cancel a reducer we had already sized correctly from
// the last good read, in response to a read failure. H-FAIL-4 and A13 handle
// stale truth the right way instead: dispatch stops, resting orders stay, and
// the exit survives the outage that caused it.
//
// UNKNOWN_RISK is for the one case where there is genuinely no q to work
// from -- startup, before any successful read.
func NextGlobal(in GlobalInput) (GlobalState, GlobalTrigger) {
	same := func() (GlobalState, GlobalTrigger) { return in.State, GTNone }

	// H-HALT-4 / A14, checked before every other rule and in every state that
	// is not already a halted one. A latch on disk implies the global state is
	// WINDING_DOWN or DRAINED; anything else is an invariant violation that
	// fires SEV1 and forces the transition anyway, so making the machine
	// produce it directly is the only reading under which A14 can hold.
	//
	// This is the HR-009 sequence: a taker fill latches WINDING_DOWN -- the
	// most serious stop condition in the system -- an unrelated panic kills the
	// process, and launchd KeepAlive restarts it. Without the latch being read
	// FIRST, flat markets resume adding, and the halt self-clears with no
	// operator action because the supervision policy the spec mandates erased
	// the safety state the spec mandates.
	//
	// It outranks UNKNOWN_RISK's own recovery gate, which is the case an
	// earlier version got wrong: an unreadable truth read left the process in
	// UNKNOWN_RISK with a durable stop on disk that no state audit or heartbeat
	// would report. Neither state places anything, so nothing is traded away by
	// the correction -- but WINDING_DOWN is the one that says so out loud, and
	// it is the one with no path back to RUNNING without an operator (§10.4).
	if in.Latched && in.State != WindingDown && in.State != Drained {
		return WindingDown, GTLatch
	}

	switch in.State {
	case Starting:
		if !in.TruthReadable {
			return UnknownRisk, GTTruthFailed
		}
		if in.Reconciled {
			return Running, GTReconciled
		}
		return same()

	case UnknownRisk:
		// "It never places, never exits, and never decays into RUNNING without
		// a complete successful reconciliation." Both conditions, not either.
		if !in.TruthReadable || !in.Reconciled {
			return same()
		}
		return Running, GTReconciled

	case Running:
		if in.Stop {
			return WindingDown, GTStop
		}
		return same()

	case WindingDown:
		// "all flat -> DRAINED". There is no transition back to RUNNING
		// without an operator action (§10.4), and this function offers none.
		//
		// Flat is necessary and NOT sufficient. H-FAIL-3: an order we have
		// merely requested a cancel for is live and fillable until the exchange
		// confirms it absent, so an account that reads flat while its cancels
		// are in flight is one ignored cancel away from being long again -- and
		// DRAINED rests no reducer. Declaring the drain complete there is a
		// stop condition that adds risk after announcing it has stopped.
		if !in.AnyInventory && !in.AnyLiveOrder {
			return Drained, GTDrained
		}
		return same()

	case Drained:
		// DRAINED does not exit the process; it idles and keeps monitoring.
		// If inventory reappears -- a fill reported late, a position poll
		// disagreeing, an adoption at restart -- it is not idle any more, and
		// the state whose job is keeping a reducing quote alive is the one to
		// be in. §5.1 does not draw this edge; it also does not draw a market
		// acquiring inventory while DRAINED, and between abandoning that
		// position and re-entering WINDING_DOWN there is only one answer
		// consistent with I1.
		//
		// A live order reappearing counts too, for the same reason it blocks
		// the drain in the first place: it is fillable.
		if in.AnyInventory || in.AnyLiveOrder {
			return WindingDown, GTInventory
		}
		return same()
	}
	return same()
}

// ---------------------------------------------------------------------------
// §9 — close catch-up
// ---------------------------------------------------------------------------

// CloseAction is one of §9's scheduled boundaries.
type CloseAction uint8

const (
	// ActionFinalCancel is H-CLOSE-3: at close_time − final_lead, cancel
	// everything in that market and verify with a sweep. Nothing of ours rests
	// into the close.
	ActionFinalCancel CloseAction = iota
	// ActionCloseLead is H-CLOSE-2: cancel the adding side, confirm it absent,
	// enter SETTLING, keep the capped reducer.
	ActionCloseLead
)

func (a CloseAction) String() string {
	if a == ActionFinalCancel {
		return "final_cancel"
	}
	return "close_lead"
}

// DueCloseActions is H-CLOSE-0's catch-up.
//
// > "A newly observed close_time that is already inside a lead runs its
// > catch-up actions synchronously and immediately, in lead order, with the
// > final cancel taking precedence over everything else."
//
// Returned **in lead order**: the close lead first, then the final cancel. That
// is chronological order, and it is the order that leaves the market in the
// right END STATE rather than merely performing the right actions.
//
// An earlier version returned the final cancel first, reading "the final cancel
// taking precedence over everything else" as an ordering instruction. Run that
// way the catch-up cancels everything, confirms it absent, and then the
// close-lead step enters SETTLING -- at which point the next sizing pass sees a
// SETTLING market with q != 0, asks for the capped reducer, and places an order
// into the final minute that the final cancel had just removed. **The final
// cancel is undone by the step that follows it.**
//
// "Takes precedence over everything else" is about not being skipped or
// deferred, and the thing that actually enforces it is the placement latch:
// past final_lead, SizesFor quotes nothing in any state and returns
// FinalCancel instead. With that latch in place the ordering is belt to its
// braces -- close-lead-first cannot place either, because the latch is already
// true when both boundaries are due.
//
// HR-017 is the sequence this exists for: at schedule_poll_s = 300 with
// final_lead = 60s, a close_time that moves from 17:00 to 12:03 at 12:00:01 is
// next observed at 12:05, after the close, and NEITHER action ever ran. The 30s
// poll of §16 makes that window narrow; running the catch-up synchronously is
// what makes a narrow window survivable rather than merely unlikely.
//
// Both actions are returned when both boundaries have passed. A caller that ran
// only the final cancel would leave the state machine believing the market was
// still QUOTING, and A9's state_event row for the close lead would never be
// written -- so the run's own record would show a market that went from quoting
// to closed with nothing in between.
func DueCloseActions(untilClose time.Duration, hasClose bool, p cfg.Params) []CloseAction {
	if !hasClose {
		return nil
	}
	var due []CloseAction
	if untilClose <= p.CloseLead {
		due = append(due, ActionCloseLead)
	}
	if untilClose <= p.FinalLead {
		due = append(due, ActionFinalCancel)
	}
	return due
}

// PastFinalLead is H-CLOSE-3's placement latch, and the input SizesFor needs.
//
// Past this boundary nothing of ours may rest into the close, so nothing more
// is placed regardless of state or position. A4's obligation to hold an exit
// lapses here by its own terms: "the exemption applies only after final_lead,
// after trading close, or at q = 0."
func PastFinalLead(untilClose time.Duration, hasClose bool, p cfg.Params) bool {
	return hasClose && untilClose <= p.FinalLead
}

// confidence: high
