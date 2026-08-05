package quote

import (
	"fmt"
	"sort"
	"time"
)

// This file is harness-spec.md §6.6 -- the order priority queue -- together
// with H-Q-9, H-Q-9a and the three rules the red team added to it: H-QUE-1
// (dependent writes are one intent), H-QUE-2 (cancelling a reducer is not
// risk-reducing) and H-QUE-3 (reducers get reserved capacity). V1.9 is its
// gate.
//
// Two properties of this file are load-bearing and are the reason it looks the
// way it does.
//
// It holds INTENTS, NOT REQUESTS. §6.6: "An intent whose triggering condition
// is no longer true is dropped, not sent -- the queue holds intents,
// re-evaluated at dequeue, not pre-built requests." So nothing here carries a
// payload, a price or a size, and every classification decision is taken from
// conditions handed in at Dequeue rather than from anything captured at
// Enqueue.
//
// It is PURE (H-TOP-3): no clock, no I/O, no goroutines, no mutex. Time,
// validity, confirmations and capacity all arrive as arguments. That is what
// lets §14's simulator replay a tape and get a deterministic trace, and it is
// why the anti-starvation rule is expressed as arithmetic on two caller-supplied
// durations rather than as a timer.

// ---------------------------------------------------------------------------
// Vocabulary
// ---------------------------------------------------------------------------

// Side indexes the two sides of one market's book. Both are BIDS in book terms
// (§4): yes bids at cents and no bids at cents.
//
// harness/risk carries the same two-valued type and this one is deliberately
// not shared with it: risk imports quote for the market states, so the reverse
// import would be a cycle -- the same layering num's doc comment records for
// Qty.
type Side uint8

const (
	SideYes Side = iota
	SideNo
)

func (s Side) String() string {
	if s == SideNo {
		return "no"
	}
	return "yes"
}

// Role is the distinction H-QUE-2 turns on, and the only one: whether a write
// can increase exposure, or can only decrease it.
//
// P0's justification is that a cancel can only reduce exposure. That is true of
// an adding-side cancel and false of a reducing-side cancel, which removes the
// exit. Every classification decision below that mentions a cancel consults
// this field first.
type Role uint8

const (
	RoleAdding Role = iota
	RoleReducing
)

func (r Role) String() string {
	if r == RoleReducing {
		return "reducing"
	}
	return "adding"
}

// Op is the concrete write one dispatchable leg produces. There is no amend
// (H-ORD-3): the primitive is cancel-then-place, or place-then-cancel under
// H-Q-9.
type Op uint8

const (
	OpPlace Op = iota
	OpCancel
)

func (o Op) String() string {
	if o == OpCancel {
		return "cancel"
	}
	return "place"
}

// Reason is why the intent exists: the §6.6 row it is classified by. It is
// distinct from Kind because one reason can take either leg structure -- a
// requote is place-then-cancel on the adding side and cancel-confirm-place on a
// reducing one (H-Q-9 clause 3).
type Reason uint8

const (
	// ReasonCancel is a cancel wanted for its own sake: the adding side is
	// coming off, or the order is being retired outright.
	ReasonCancel Reason = iota
	// ReasonReduce is a reducing-side placement. §6.6's P1 row is this reason
	// while |q| > inv_soft.
	ReasonReduce
	// ReasonPresence is presence restoration: a qualifying side with nothing
	// resting. Presence gaps are revenue (S4).
	ReasonPresence
	// ReasonRequote is H-Q-6: the reference moved against us and our order is
	// >= 1 tick behind the touch.
	ReasonRequote
	// ReasonTopUp is a top-up after a partial fill.
	ReasonTopUp
)

func (r Reason) String() string {
	switch r {
	case ReasonCancel:
		return "cancel"
	case ReasonReduce:
		return "reduce"
	case ReasonPresence:
		return "presence"
	case ReasonRequote:
		return "requote"
	case ReasonTopUp:
		return "topup"
	}
	return "INVALID"
}

// Kind is the leg structure of an intent: how many writes it contains and in
// what order they may be dispatched.
//
// H-QUE-1 is the whole reason this type exists. A place-then-cancel requote
// enqueued as two intents puts the cancel at P0 and the placement at P3, and
// §6.6 then dispatches them in the exact reverse of the mandated order (red-team
// HR-019). One intent, two gated legs, is the shape that cannot invert.
type Kind uint8

const (
	// KindPlace is a single placement: presence restoration, a reducing
	// placement, a top-up, or a replacement whose cancel is already confirmed.
	KindPlace Kind = iota
	// KindCancel is a standalone cancel. Legal on the adding side only
	// (H-QUE-2): a reducing-side cancel removes the exit and is only ever the
	// first leg of KindCancelConfirmPlace.
	KindCancel
	// KindPlaceThenCancel is H-Q-9: place the replacement first so there is no
	// presence gap, then retire the old order. Adding side only -- on a
	// reducing side the momentary aggregate would exceed |q|, which H-Q-5a
	// forbids (red-team HR-004). The cancel leg becomes eligible only after the
	// placement ACKs (H-Q-9a); dispatching the placement is not enough.
	KindPlaceThenCancel
	// KindCancelConfirmPlace is the reducing and settling form of H-Q-9: cancel
	// first, and do not dispatch the replacement until the cancel is confirmed
	// by response or sweep. The presence gap is accepted, because a reducer
	// that overshoots is worse than a reducer that is briefly absent.
	KindCancelConfirmPlace
)

func (k Kind) String() string {
	switch k {
	case KindPlace:
		return "place"
	case KindCancel:
		return "cancel"
	case KindPlaceThenCancel:
		return "place-then-cancel"
	case KindCancelConfirmPlace:
		return "cancel-confirm-place"
	}
	return "INVALID"
}

// dependent reports whether the kind carries a second, confirmation-gated leg.
func (k Kind) dependent() bool {
	return k == KindPlaceThenCancel || k == KindCancelConfirmPlace
}

// Stage is the dependent-leg state of one intent. A single-leg intent leaves
// the queue on dispatch and is never seen in any stage but StageFirst.
type Stage uint8

const (
	// StageFirst: the first leg has not been sent. This is the only stage in
	// which an intent has changed nothing at the exchange.
	StageFirst Stage = iota
	// StageFirstSent: the first leg is in flight and the second is NOT
	// eligible. H-Q-9a: dispatching the placement does not unlock the cancel;
	// only the ACK does.
	StageFirstSent
	// StageSecond: the confirmation arrived and the second leg is dispatchable.
	StageSecond
)

func (s Stage) String() string {
	switch s {
	case StageFirst:
		return "first"
	case StageFirstSent:
		return "first-sent"
	case StageSecond:
		return "second"
	}
	return "INVALID"
}

// Class is a §6.6 priority class. Lower is more urgent, so P0 is zero: the
// numeric order IS the dispatch order, and anti-starvation promotion is a
// subtraction that saturates at the top of the table.
type Class uint8

const (
	// P0: any cancel, and any write in WINDING_DOWN. Bypasses the ordinary
	// local token bucket entirely. Never starves. H-QUE-2 is its one exception.
	P0 Class = iota
	// P1: a reducing-side placement while |q| > inv_soft, and -- by H-QUE-2 --
	// a reducing-side cancel. Never starves: H-QUE-3 holds capacity for it.
	P1
	// P2: presence restoration. Does not starve, by promotion.
	P2
	// P3: a requote. Starves, boundedly, by promotion.
	P3
	// P4: a top-up after a partial fill. Starves, boundedly, by promotion.
	P4
)

func (c Class) String() string {
	switch c {
	case P0:
		return "P0"
	case P1:
		return "P1"
	case P2:
		return "P2"
	case P3:
		return "P3"
	case P4:
		return "P4"
	}
	return "INVALID"
}

// ---------------------------------------------------------------------------
// Intents
// ---------------------------------------------------------------------------

// Intent is one intended write, or one ordered pair of dependent writes
// (H-QUE-1).
//
// It names a market, a side, a role and a reason -- and nothing that could be
// sent. There is no price, no size and no client order id here on purpose: a
// queue entry that carried a payload would be a pre-built request, and §6.6
// forbids exactly that.
type Intent struct {
	// ID is assigned by Enqueue and is zero before it. It is how a caller
	// reports an ACK, an absence, or an abandonment back.
	ID uint64

	// Market is the ticker. It is the first half of H-QUE-3's cancel
	// coalescing key.
	Market string
	// Side is the second half of that key.
	Side Side
	// Role is H-QUE-2's discriminator.
	Role Role
	// Kind is the leg structure (H-QUE-1).
	Kind Kind
	// Reason is the §6.6 row.
	Reason Reason

	// Enqueued is the caller's monotonic reading at Enqueue. Anti-starvation is
	// a difference against Conditions.Now and nothing else; this package never
	// reads a clock.
	Enqueued time.Duration

	// stage is dependent-leg state. Unexported so that a caller cannot
	// construct an intent that claims a confirmation it never received: the
	// only ways to advance it are AckPlace and ConfirmAbsent.
	stage Stage
}

// Stage reports the intent's dependent-leg state.
func (in Intent) Stage() Stage { return in.stage }

// Op is the write the intent's CURRENT leg produces.
//
// It is a function of Kind and Stage, which is what makes the two dependent
// sequences impossible to reverse: a place-then-cancel cannot present a cancel
// before StageSecond, and a cancel-confirm-place cannot present a placement
// before it.
func (in Intent) Op() Op {
	switch in.Kind {
	case KindCancel:
		return OpCancel
	case KindPlaceThenCancel:
		if in.stage == StageSecond {
			return OpCancel
		}
		return OpPlace
	case KindCancelConfirmPlace:
		if in.stage == StageSecond {
			return OpPlace
		}
		return OpCancel
	}
	return OpPlace
}

// Dispatchable reports whether the current leg may be selected at all. It is
// false exactly while a first leg is in flight: H-Q-9a's gate.
func (in Intent) Dispatchable() bool {
	return in.stage == StageFirst || in.stage == StageSecond
}

// Base is §6.6's class table, before anti-starvation promotion.
//
// The order of the tests is the rule, not an implementation detail:
//
//  1. A cancel on a REDUCING side is P1 (H-QUE-2). This is checked first
//     because it is the stated exception to BOTH of P0's clauses -- "any
//     cancel" and "any write in WINDING_DOWN". A reducing-side cancel removes
//     the exit; it does not get P0's bucket bypass under any circumstances.
//  2. Any other cancel is P0.
//  3. Any other write in WINDING_DOWN is P0.
//  4. A placement on the reducing side while |q| > inv_soft is P1 -- whatever
//     triggered it. §6.6's P1 row is "reducing-side placement where
//     |q| > inv_soft", not "a placement whose reason was reduction": a requote
//     of the exit is still the exit, and it does not queue behind an adding
//     side's requote.
//  5. Otherwise the reason names the row.
func (in Intent) Base(c Conditions) Class {
	if in.Op() == OpCancel {
		if in.Role == RoleReducing {
			return P1
		}
		return P0
	}
	if c.Global == WindingDown {
		return P0
	}
	if in.Role == RoleReducing && c.AboveSoft[in.Market] {
		return P1
	}
	switch in.Reason {
	case ReasonReduce:
		// Reached only when the reducer has fallen back under inv_soft while it
		// waited -- above it, rule 4 already returned P1. §6.6's P1 row is
		// conditioned on |q| > inv_soft and no longer describes this write:
		// below the taper's start both sides quote, so what is left is an
		// ordinary qualifying side with nothing resting, which is P2.
		//
		// Ordinarily Conditions.Valid drops such an intent before this line is
		// reached. This branch exists so that a caller who still wants the
		// placement gets it at the class it actually is, rather than at a
		// priority justified by risk that is no longer there.
		return P2
	case ReasonPresence:
		return P2
	case ReasonRequote:
		return P3
	case ReasonTopUp:
		return P4
	}
	return P4
}

// validate rejects the intent shapes the spec says cannot exist, at the only
// point where rejecting them is cheap.
//
// These are not defensive checks. Each one is a rule that is otherwise
// enforceable only by hoping the caller read it.
func (in Intent) validate() error {
	var errs []error
	bad := func(format string, a ...any) {
		errs = append(errs, fmt.Errorf(format, a...))
	}

	if in.Market == "" {
		bad("intent has no market: it could never be coalesced by (market, side) (H-QUE-3)")
	}
	if in.Side != SideYes && in.Side != SideNo {
		bad("side = %d is neither yes nor no", in.Side)
	}
	if in.Role != RoleAdding && in.Role != RoleReducing {
		bad("role = %d is neither adding nor reducing; H-QUE-2 is undecidable without it", in.Role)
	}
	if in.Kind > KindCancelConfirmPlace {
		bad("kind = %d is not a leg structure this queue knows", in.Kind)
	}
	if in.Reason > ReasonTopUp {
		bad("reason = %d is not a §6.6 row", in.Reason)
	}
	if in.stage != StageFirst {
		bad("intent was submitted at stage %s: a leg gate may only be opened by "+
			"AckPlace or ConfirmAbsent (H-Q-9a)", in.stage)
	}

	switch in.Kind {
	case KindCancel:
		if in.Role == RoleReducing {
			bad("a reducing-side cancel may not be enqueued as standalone work "+
				"(H-QUE-2): it removes the exit, so it is P1 and is only ever the "+
				"first leg of a %s intent", KindCancelConfirmPlace)
		}
		if in.Reason != ReasonCancel {
			bad("kind %s carries reason %s; a standalone cancel's reason is %s",
				in.Kind, in.Reason, ReasonCancel)
		}
	case KindPlaceThenCancel:
		if in.Role == RoleReducing {
			bad("place-then-cancel is permitted on the adding side only (H-Q-9 " +
				"clause 3): on a reducing side the momentary aggregate would " +
				"exceed |q|, which H-Q-5a forbids (HR-004)")
		}
		if in.Reason == ReasonCancel {
			bad("kind %s carries reason %s; it places before it cancels", in.Kind, in.Reason)
		}
	case KindPlace, KindCancelConfirmPlace:
		if in.Reason == ReasonCancel {
			bad("kind %s carries reason %s; its first dispatched write is not a "+
				"cancel wanted for its own sake", in.Kind, in.Reason)
		}
	}

	if in.Reason == ReasonReduce && in.Role != RoleReducing {
		bad("reason %s on the %s side: §6.6's P1 row is a REDUCING-side placement",
			in.Reason, in.Role)
	}

	if len(errs) == 0 {
		return nil
	}
	msg := fmt.Sprintf("invalid intent (%d problem(s)):", len(errs))
	for _, e := range errs {
		msg += "\n  - " + e.Error()
	}
	return fmt.Errorf("%s", msg)
}

// Promote is §6.6's anti-starvation rule: "any queued intent older than
// max_queue_age (30s) is promoted one class".
//
// One class per COMPLETED interval, saturating at P0. Expressed as arithmetic
// on an age rather than as a repeated event, because a promotion that depended
// on how often Dequeue happened to be called would make starvation a function
// of load -- which is exactly the condition under which starvation happens.
//
// A non-positive maxQueueAge disables promotion rather than dividing by zero;
// cfg.Validate already refuses that configuration for this reason.
func Promote(base Class, age, maxQueueAge time.Duration) Class {
	if maxQueueAge <= 0 || age < maxQueueAge {
		return base
	}
	steps := int64(age / maxQueueAge)
	if steps >= int64(base) {
		return P0
	}
	return base - Class(steps)
}

// ---------------------------------------------------------------------------
// Dequeue-time conditions
// ---------------------------------------------------------------------------

// Conditions is the caller-supplied truth every intent is re-evaluated against
// at dequeue.
//
// Nothing in here is captured at enqueue and nothing is cached between calls.
// §6.6 is explicit that the queue holds intents re-evaluated at dequeue, and a
// condition that was true 30 seconds ago is precisely what "not pre-built
// requests" is guarding against.
type Conditions struct {
	// Now is the caller's monotonic reading. This package has no clock
	// (H-TOP-3); every age below is a difference of two caller-supplied values.
	Now time.Duration

	// Global is §5.1's state. In WINDING_DOWN every write is P0 -- except
	// H-QUE-2's reducing-side cancel.
	Global GlobalState

	// AboveSoft reports per market whether |q| > inv_soft. It is what separates
	// §6.6's P1 reducing placement from an ordinary one. A missing entry reads
	// as false.
	AboveSoft map[string]bool

	// Valid reports whether an intent's triggering condition still holds. False
	// means "drop it, do not send it". A nil Valid asserts that every queued
	// intent is still wanted.
	Valid func(Intent) bool
}

// stillWanted applies Valid where dropping the intent cannot strand a live
// order.
//
// The gate is not "every entry", and the reason is a safety argument rather
// than an optimisation:
//
//   - StageFirst: nothing has been sent, so dropping changes nothing at the
//     exchange. Always re-evaluated. This is §6.6's drop rule.
//   - StageSecond of a cancel-confirm-place: the cancel is already confirmed
//     and nothing of ours rests. Dropping the replacement leaves us absent,
//     which is the accepted outcome of that sequence anyway. Re-evaluated.
//   - StageSecond of a place-then-cancel: the pending leg is the CANCEL of the
//     order the replacement was placed against. Dropping it leaves two orders
//     resting on one side -- the aggregate overlap H-Q-5a forbids and HR-004
//     found. Never dropped: the pair is committed once its first leg goes out.
//   - StageFirstSent: in flight. A write cannot be un-sent.
func (c Conditions) stillWanted(in Intent) bool {
	if c.Valid == nil {
		return true
	}
	switch {
	case in.stage == StageFirst:
		return c.Valid(in)
	case in.stage == StageSecond && in.Kind == KindCancelConfirmPlace:
		return c.Valid(in)
	}
	return true
}

// ---------------------------------------------------------------------------
// Capacity -- H-QUE-3
// ---------------------------------------------------------------------------

// Grant is the capacity decision for one dispatch: which pools the caller must
// charge the write to.
//
// It is returned rather than applied because this package owns no state that
// ticks. cmd/harness owns the workers and the bucket and calls Capacity.Take.
type Grant struct {
	// ReservedWorker means the write took the P1-only worker reserve. Only a
	// P1 write can ever see this true.
	ReservedWorker bool
	// ReservedToken means the write was charged to the P1-only budget share.
	// Only a P1 write can ever see this true.
	ReservedToken bool
	// BypassBucket means the write skipped the ordinary local token bucket
	// entirely, which is P0's privilege and P0's only privilege. §6.6: "P0
	// bypasses rate limiting entirely." H-QUE-3: it bypasses only the LOCAL
	// bucket -- never the exchange's, and never the P1 reserve.
	BypassBucket bool
}

// Capacity is the shared write resource, as the caller sees it at this instant.
//
// H-QUE-3 is the whole point of the split fields. A cancel storm can absorb
// 429s, occupy every REST worker in backoff and starve a P1 reducer
// indefinitely, so a reserve that P0 cannot reach is held at all times. The
// general pool is Workers - ReservedWorkers, and it is the only pool anything
// other than P1 may draw from.
type Capacity struct {
	// Workers is K, the REST worker count (H-TOP-4).
	Workers int
	// ReservedWorkers is how many of those are held exclusively for P1.
	// H-QUE-3 requires at least one, always.
	ReservedWorkers int
	// BusyGeneral is how many general-pool workers are occupied.
	BusyGeneral int
	// BusyReserved is how many reserved slots are occupied by P1 work.
	BusyReserved int

	// Tokens is the ordinary local bucket's currently available whole writes
	// (§16 write_rate, write_burst).
	Tokens int
	// ReservedTokens is the P1-only share of the write budget, currently
	// available. H-QUE-3 requires the share to be positive.
	ReservedTokens int
}

// ReserveWorkers is H-QUE-3's P1 worker reserve for a pool of the given size.
//
// At least one slot, always, and never the whole pool: a reserve that consumed
// every worker would starve P0, whose cancels are the writes that can only
// reduce exposure.
func ReserveWorkers(workers int) int {
	if workers < 2 {
		return 1
	}
	r := workers / 4
	if r < 1 {
		r = 1
	}
	if r > workers-1 {
		r = workers - 1
	}
	return r
}

// ReserveWrites is H-QUE-3's P1 share of the §16 write budget.
//
// At least one write whatever the burst is: a reserve of zero is not a reserve,
// and "a reserved share of the write budget is held for P1 at all times" is not
// satisfiable by a share that rounds away.
func ReserveWrites(burst int) int {
	r := burst / 4
	if r < 1 {
		r = 1
	}
	return r
}

// NewCapacity builds a full, idle capacity for a worker count and a §16 write
// burst, with H-QUE-3's reserves already carved out of them.
func NewCapacity(workers, burst int) Capacity {
	rw := ReserveWorkers(workers)
	rt := ReserveWrites(burst)
	if workers < rw+1 {
		workers = rw + 1
	}
	tokens := burst - rt
	if tokens < 0 {
		tokens = 0
	}
	return Capacity{
		Workers:         workers,
		ReservedWorkers: rw,
		Tokens:          tokens,
		ReservedTokens:  rt,
	}
}

// Validate enforces H-QUE-3's floors on a capacity the caller assembled itself.
func (c Capacity) Validate() error {
	var errs []error
	bad := func(format string, a ...any) {
		errs = append(errs, fmt.Errorf(format, a...))
	}

	if c.ReservedWorkers < 1 {
		bad("reserved_workers = %d: H-QUE-3 holds at least one worker slot for "+
			"P1 at all times, and a cancel storm occupies the rest", c.ReservedWorkers)
	}
	if c.Workers <= c.ReservedWorkers {
		bad("workers = %d with %d reserved leaves no general pool: every P0 "+
			"cancel would be unable to dispatch", c.Workers, c.ReservedWorkers)
	}
	if c.BusyGeneral < 0 || c.BusyGeneral > c.Workers-c.ReservedWorkers {
		bad("busy_general = %d is outside the general pool of %d",
			c.BusyGeneral, c.Workers-c.ReservedWorkers)
	}
	if c.BusyReserved < 0 || c.BusyReserved > c.ReservedWorkers {
		bad("busy_reserved = %d is outside the reserve of %d",
			c.BusyReserved, c.ReservedWorkers)
	}
	if c.Tokens < 0 {
		bad("tokens = %d is negative", c.Tokens)
	}
	if c.ReservedTokens < 0 {
		bad("reserved_tokens = %d is negative", c.ReservedTokens)
	}

	if len(errs) == 0 {
		return nil
	}
	msg := fmt.Sprintf("invalid capacity (%d problem(s)):", len(errs))
	for _, e := range errs {
		msg += "\n  - " + e.Error()
	}
	return fmt.Errorf("%s", msg)
}

// Admit is H-QUE-3's capacity decision, as a pure function of the class.
//
// The class handed in is the BASE class -- §6.6's row for the write as it is
// right now -- and never the promoted effective class. H-QUE-3's reserve exists
// because a reducing placement is the exit, and a write does not stop being the
// exit, or become one, by having waited 30 seconds. Dequeue is where that
// distinction is enforced; see its comment.
//
// Two independent resources, and P1 is the only class that can reach either
// reserve:
//
//   - A worker. Everything needs one. Non-P1 may only take the general pool;
//     P1 prefers the general pool and falls back to the reserve. A P0 cancel
//     storm can therefore fill the general pool and no more.
//   - A write. P0 bypasses the ordinary local bucket entirely (§6.6) and takes
//     nothing from the reserve. P1 spends an ordinary token if there is one and
//     a reserved token otherwise. P2..P4 need an ordinary token.
func (c Capacity) Admit(k Class) (Grant, bool) {
	var g Grant

	switch {
	case c.BusyGeneral < c.Workers-c.ReservedWorkers:
		// The general pool has room. Anything may use it, P1 included.
	case k == P1 && c.BusyReserved < c.ReservedWorkers:
		g.ReservedWorker = true
	default:
		return Grant{}, false
	}

	switch {
	case k == P0:
		g.BypassBucket = true
	case c.Tokens > 0:
		// The ordinary bucket pays for it.
	case k == P1 && c.ReservedTokens > 0:
		g.ReservedToken = true
	default:
		return Grant{}, false
	}

	return g, true
}

// Take is c with one grant applied. Pure: it returns the next capacity rather
// than mutating this one.
func (c Capacity) Take(g Grant) Capacity {
	if g.ReservedWorker {
		c.BusyReserved++
	} else {
		c.BusyGeneral++
	}
	switch {
	case g.BypassBucket:
		// P0 spent no local token by construction.
	case g.ReservedToken:
		c.ReservedTokens--
	default:
		c.Tokens--
	}
	return c
}

// ---------------------------------------------------------------------------
// The queue
// ---------------------------------------------------------------------------

// Dispatch is one write the caller may now send. It is derived at dequeue from
// current conditions and is not stored anywhere: §6.6's queue holds intents,
// not requests.
type Dispatch struct {
	// IDs is every intent this single write discharges. A placement discharges
	// exactly one. A cancel discharges the whole coalesced (market, side)
	// group, because H-QUE-3 requires cancels to be "coalesced per (market,
	// side) rather than issued per order".
	IDs []uint64

	Market string
	Side   Side
	// Role is the earliest-queued member's role. For a coalesced cancel every
	// member shares a (market, side), and a market side is either adding or
	// reducing at one instant, so the group is single-role in practice.
	Role Role
	Op   Op

	// Class is the EFFECTIVE class the write was selected at: §6.6's row after
	// anti-starvation promotion. It is the scheduling position and NOT the class
	// the capacity was charged against -- Grant is that, and it is decided on the
	// unpromoted row (H-QUE-3). See Dequeue.
	Class Class

	// Grant is the capacity this write consumes. The caller applies it with
	// Capacity.Take.
	Grant Grant
}

type entry struct {
	in Intent
	// seq is the enqueue order, and is the FIFO tiebreak within an effective
	// class. It survives leg advancement: an intent does not go to the back of
	// its class for having been confirmed.
	seq uint64
}

// Queue is §6.6's single order priority queue.
//
// One queue, all markets, all classes -- because with N markets live the write
// budget is the binding constraint and a per-market queue cannot arbitrate a
// global budget.
//
// It carries no mutex, exactly as core carries none (H-TOP-4): safety here is
// single-writer, and the owner goroutine is that writer.
type Queue struct {
	maxQueueAge time.Duration
	nextID      uint64
	nextSeq     uint64
	items       []*entry
	drops       uint64
}

// NewQueue builds an empty queue promoting on the given §16 max_queue_age.
func NewQueue(maxQueueAge time.Duration) *Queue {
	return &Queue{maxQueueAge: maxQueueAge}
}

// MaxQueueAge is the promotion interval this queue was built with.
func (q *Queue) MaxQueueAge() time.Duration { return q.maxQueueAge }

// Len is the number of intents still held, dispatched-but-unresolved first legs
// included.
func (q *Queue) Len() int { return len(q.items) }

// Drops is the cumulative count of intents removed at dequeue because their
// triggering condition had cleared. §6.6: dropped, not sent.
func (q *Queue) Drops() uint64 { return q.drops }

// Enqueue admits an intent, stamping it with an id and the caller's monotonic
// reading. It returns an error, and admits nothing, for any shape the spec says
// cannot exist -- notably a standalone reducing-side cancel (H-QUE-2).
func (q *Queue) Enqueue(now time.Duration, in Intent) (uint64, error) {
	if err := in.validate(); err != nil {
		return 0, err
	}
	q.nextID++
	q.nextSeq++
	in.ID = q.nextID
	in.Enqueued = now
	q.items = append(q.items, &entry{in: in, seq: q.nextSeq})
	return in.ID, nil
}

// Get returns a copy of the intent with this id.
func (q *Queue) Get(id uint64) (Intent, bool) {
	for _, e := range q.items {
		if e.in.ID == id {
			return e.in, true
		}
	}
	return Intent{}, false
}

// Pending returns every held intent in enqueue order.
func (q *Queue) Pending() []Intent {
	out := make([]Intent, 0, len(q.items))
	for _, e := range q.items {
		out = append(out, e.in)
	}
	return out
}

// Effective is the class the intent would be selected at right now: its §6.6
// row after anti-starvation promotion.
func (q *Queue) Effective(in Intent, c Conditions) Class {
	return Promote(in.Base(c), c.Now-in.Enqueued, q.maxQueueAge)
}

// Drop removes an intent outright -- a rejected placement, an abandoned
// sequence. It reports whether anything was removed.
func (q *Queue) Drop(id uint64) bool {
	for i, e := range q.items {
		if e.in.ID == id {
			q.items = append(q.items[:i], q.items[i+1:]...)
			return true
		}
	}
	return false
}

// AckPlace records the exchange's ACK of a dispatched placement.
//
// H-Q-9a: this, and only this, makes a place-then-cancel's cancel leg eligible.
// Dispatching the placement does not, because a dispatched write that has not
// ACKed may still be REJECTED or UNKNOWN (§7.2), and cancelling the old order
// against a replacement that never rested is the presence gap H-Q-9 exists to
// avoid -- reached by a longer route.
//
// It deliberately does nothing to a cancel-confirm-place: that sequence's gate
// is absence, not an ACK, and crossing the two would let a cancel's own
// response unlock the replacement it is not yet entitled to.
func (q *Queue) AckPlace(id uint64) bool {
	for _, e := range q.items {
		if e.in.ID != id {
			continue
		}
		if e.in.Kind == KindPlaceThenCancel && e.in.stage == StageFirstSent {
			e.in.stage = StageSecond
			return true
		}
		return false
	}
	return false
}

// ConfirmAbsent records that nothing of ours rests on (market, side), confirmed
// by cancel response or by sweep (§7.4, H-ORD-4).
//
// It is what unlocks the replacement leg of every cancel-confirm-place on that
// key (H-Q-9 clause 3, H-QUE-2). The key is (market, side) rather than an order
// id because a sweep is what makes a cancel a fact, and a sweep answers per
// side, not per order -- the same key H-QUE-3 coalesces cancels on.
//
// It returns how many intents it advanced.
func (q *Queue) ConfirmAbsent(market string, side Side) int {
	n := 0
	for _, e := range q.items {
		if e.in.Market != market || e.in.Side != side {
			continue
		}
		if e.in.Kind == KindCancelConfirmPlace && e.in.stage == StageFirstSent {
			e.in.stage = StageSecond
			n++
		}
	}
	return n
}

// cancelKey is H-QUE-3's coalescing key.
type cancelKey struct {
	market string
	side   Side
}

// candidate is one selectable unit: a single placement, or a coalesced cancel
// group.
//
// class and base are both carried because they answer different questions and
// the answers diverge the moment anything ages. class is the effective class --
// §6.6's row after promotion -- and decides ORDER. base is the unpromoted §6.6
// row and decides ENTITLEMENT. See Dequeue.
type candidate struct {
	class   Class
	base    Class
	seq     uint64
	op      Op
	market  string
	side    Side
	role    Role
	entries []*entry
}

// Dequeue selects the next write to send under the given conditions and
// capacity, or reports that nothing is dispatchable.
//
// In order:
//
//  1. Every re-evaluable intent whose triggering condition has cleared is
//     dropped, not sent (§6.6).
//  2. Every intent with an eligible leg is classified from CURRENT conditions
//     and promoted by age.
//  3. Pending cancels are coalesced per (market, side) into one logical write
//     (H-QUE-3).
//  4. Candidates are ordered by effective class, then FIFO within it.
//  5. The first candidate the capacity admits wins. Selection CONTINUES past a
//     candidate capacity refuses -- which is what lets a waiting P1 reducer
//     reach its reserve while a P0 cancel storm holds the general pool.
//
// Promotion controls ORDER AND NOTHING ELSE. Admission is asked against the
// candidate's BASE class, not its promoted one, and the two rules are separate
// clauses that happen to speak the same vocabulary:
//
//   - §6.6's anti-starvation rule promotes a queued intent one class per
//     completed max_queue_age so that it is SELECTED sooner. It is a statement
//     about queue position.
//   - H-QUE-3 holds a worker slot and a share of the write budget for P1 at all
//     times because a reducing-side write is the exit. It is a statement about
//     what the write IS.
//
// Coupling them inverts H-QUE-3 exactly where it is needed. An above-inv_soft
// reducing placement that waits out one interval is promoted to effective P0;
// admitted as P0 it can no longer reach either P1 reserve, so the longer it has
// been starved by a cancel storm the less capacity it is entitled to -- and the
// storm holds the general pool by construction. The write is still the exit. It
// keeps P1's reserves and merely sorts ahead of them.
//
// The converse matters too: an aged P2 or P3 sorts at P0 but does not acquire
// P0's bucket bypass. That bypass is justified by "a cancel can only reduce
// exposure" (§6.6), which is a property of the write, and a requote does not
// acquire it by waiting.
func (q *Queue) Dequeue(c Conditions, capacity Capacity) (Dispatch, bool) {
	q.dropCleared(c)

	cands := q.candidates(c)
	sort.SliceStable(cands, func(i, j int) bool {
		if cands[i].class != cands[j].class {
			return cands[i].class < cands[j].class
		}
		return cands[i].seq < cands[j].seq
	})

	for _, cd := range cands {
		g, ok := capacity.Admit(cd.base)
		if !ok {
			continue
		}
		return q.commit(cd, g), true
	}
	return Dispatch{}, false
}

// dropCleared applies §6.6's drop rule. See Conditions.stillWanted for which
// entries are re-evaluable and why the rest are not.
func (q *Queue) dropCleared(c Conditions) {
	if c.Valid == nil {
		return
	}
	kept := q.items[:0]
	for _, e := range q.items {
		if !c.stillWanted(e.in) {
			q.drops++
			continue
		}
		kept = append(kept, e)
	}
	for i := len(kept); i < len(q.items); i++ {
		q.items[i] = nil
	}
	q.items = kept
}

// candidates builds the selectable set, coalescing pending cancels per
// (market, side).
//
// The group's class is its most urgent member's, and its FIFO position is its
// earliest member's: one DELETE discharges all of them, so making the group
// wait for its laziest member would be a starvation channel dressed up as
// fairness.
//
// Its base class is merged the same way, and a group is single-role in practice
// -- a market side is either adding or reducing at one instant -- so the merge
// only ever chooses between equal values. It is written as a merge anyway so
// that base cannot silently become "whichever member happened to be seen
// first".
func (q *Queue) candidates(c Conditions) []candidate {
	var out []candidate
	groups := make(map[cancelKey]int)

	for _, e := range q.items {
		if !e.in.Dispatchable() {
			continue
		}
		base := e.in.Base(c)
		cl := Promote(base, c.Now-e.in.Enqueued, q.maxQueueAge)
		op := e.in.Op()

		if op == OpCancel {
			k := cancelKey{market: e.in.Market, side: e.in.Side}
			if i, ok := groups[k]; ok {
				out[i].entries = append(out[i].entries, e)
				if cl < out[i].class {
					out[i].class = cl
				}
				if base < out[i].base {
					out[i].base = base
				}
				if e.seq < out[i].seq {
					out[i].seq = e.seq
				}
				continue
			}
			groups[k] = len(out)
		}

		out = append(out, candidate{
			class:   cl,
			base:    base,
			seq:     e.seq,
			op:      op,
			market:  e.in.Market,
			side:    e.in.Side,
			role:    e.in.Role,
			entries: []*entry{e},
		})
	}
	return out
}

// commit turns a selected candidate into a Dispatch and advances the leg state
// of everything it discharges.
//
// A dependent intent's first leg goes to StageFirstSent -- NOT to StageSecond.
// That one line is H-Q-9a: dispatch is not confirmation.
func (q *Queue) commit(cd candidate, g Grant) Dispatch {
	d := Dispatch{
		Market: cd.market,
		Side:   cd.side,
		Role:   cd.role,
		Op:     cd.op,
		Class:  cd.class,
		Grant:  g,
	}

	finished := make(map[uint64]bool, len(cd.entries))
	for _, e := range cd.entries {
		d.IDs = append(d.IDs, e.in.ID)
		if e.in.Kind.dependent() && e.in.stage == StageFirst {
			e.in.stage = StageFirstSent
			continue
		}
		finished[e.in.ID] = true
	}

	if len(finished) > 0 {
		kept := q.items[:0]
		for _, e := range q.items {
			if finished[e.in.ID] {
				continue
			}
			kept = append(kept, e)
		}
		for i := len(kept); i < len(q.items); i++ {
			q.items[i] = nil
		}
		q.items = kept
	}
	return d
}

// confidence: high
