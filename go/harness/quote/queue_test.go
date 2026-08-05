package quote

import (
	"fmt"
	"testing"
	"time"
)

// V1.9: "Priority queue ordering and anti-starvation. Fails if a P3 requote can
// precede a P0 cancel."
//
// Everything below is a table test with no fixtures beyond literals, per §17
// V1: harness/quote is clock-free and I/O-free, so `now` is an argument,
// validity is a function the test supplies, confirmations are method calls and
// capacity is a value. Nothing here sleeps, calls time.Now, starts a goroutine
// or opens a socket -- and if it ever needed to, that would be the bug.

const maxQueueAge = 30 * time.Second // §16

// running is the ordinary dequeue-time world: RUNNING, nothing above inv_soft,
// every queued intent still wanted.
func running() Conditions {
	return Conditions{Global: Running, Valid: func(Intent) bool { return true }}
}

// roomy is a capacity nothing can be refused by, so ordering tests measure
// ordering and not scarcity.
func roomy() Capacity { return NewCapacity(8, 40) }

func mustEnqueue(t *testing.T, q *Queue, now time.Duration, in Intent) uint64 {
	t.Helper()
	id, err := q.Enqueue(now, in)
	if err != nil {
		t.Fatalf("Enqueue(%v, %+v): %v", now, in, err)
	}
	return id
}

func mustDequeue(t *testing.T, q *Queue, c Conditions, cp Capacity) Dispatch {
	t.Helper()
	d, ok := q.Dequeue(c, cp)
	if !ok {
		t.Fatalf("Dequeue at now=%v: nothing dispatchable, queue holds %d", c.Now, q.Len())
	}
	return d
}

func mustNotDequeue(t *testing.T, q *Queue, c Conditions, cp Capacity, why string) {
	t.Helper()
	if d, ok := q.Dequeue(c, cp); ok {
		t.Fatalf("Dequeue at now=%v returned %s %s ids=%v class=%s -- %s",
			c.Now, d.Op, d.Market, d.IDs, d.Class, why)
	}
}

func onlyID(t *testing.T, d Dispatch, want uint64) {
	t.Helper()
	if len(d.IDs) != 1 || d.IDs[0] != want {
		t.Fatalf("dispatch discharged ids %v, want exactly [%d]", d.IDs, want)
	}
}

// place builds a single-leg placement intent.
func place(market string, side Side, role Role, reason Reason) Intent {
	return Intent{Market: market, Side: side, Role: role, Kind: KindPlace, Reason: reason}
}

// addCancel builds a standalone adding-side cancel: §6.6's P0 row.
func addCancel(market string, side Side) Intent {
	return Intent{Market: market, Side: side, Role: RoleAdding, Kind: KindCancel, Reason: ReasonCancel}
}

// ---------------------------------------------------------------------------
// §6.6's class table, and H-QUE-2's exception to it
// ---------------------------------------------------------------------------

// TestBaseClassIsSixSixPlusHQUE2 pins the class table verbatim, including the
// two clauses H-QUE-2 carves out of P0.
//
// P0's justification is that a cancel can only reduce exposure. That is true of
// an adding-side cancel and false of a reducing-side cancel, which removes the
// exit -- so "any cancel" and "any write in WINDING_DOWN" both have exactly one
// exception, and it is the same one.
func TestBaseClassIsSixSixPlusHQUE2(t *testing.T) {
	above := map[string]bool{"HOT": true}

	cases := []struct {
		name   string
		in     Intent
		global GlobalState
		want   Class
	}{
		{"adding cancel is P0",
			addCancel("M", SideYes), Running, P0},
		{"adding cancel in WINDING_DOWN is still P0",
			addCancel("M", SideYes), WindingDown, P0},
		{"reducing placement above inv_soft is P1",
			place("HOT", SideNo, RoleReducing, ReasonReduce), Running, P1},
		{"reducing requote above inv_soft is P1 -- the exit is the exit",
			place("HOT", SideNo, RoleReducing, ReasonRequote), Running, P1},
		{"reducing placement below inv_soft is not P1",
			place("M", SideNo, RoleReducing, ReasonReduce), Running, P2},
		{"presence restoration is P2",
			place("M", SideYes, RoleAdding, ReasonPresence), Running, P2},
		{"requote is P3",
			place("M", SideYes, RoleAdding, ReasonRequote), Running, P3},
		{"top-up after a partial fill is P4",
			place("M", SideYes, RoleAdding, ReasonTopUp), Running, P4},
		{"a requote in WINDING_DOWN is P0 -- any write",
			place("M", SideYes, RoleAdding, ReasonRequote), WindingDown, P0},
		{"a top-up in WINDING_DOWN is P0 -- any write",
			place("M", SideYes, RoleAdding, ReasonTopUp), WindingDown, P0},
		{"a reducing placement in WINDING_DOWN is P0 -- any write",
			place("HOT", SideNo, RoleReducing, ReasonReduce), WindingDown, P0},

		// H-QUE-2, both halves. The first leg of a cancel-confirm-place on the
		// reducing side is a cancel, and it is NOT P0.
		{"reducing cancel is P1, not P0 -- it removes the exit",
			Intent{Market: "HOT", Side: SideNo, Role: RoleReducing,
				Kind: KindCancelConfirmPlace, Reason: ReasonRequote}, Running, P1},
		{"reducing cancel in WINDING_DOWN is STILL P1 -- the exception is to both clauses",
			Intent{Market: "HOT", Side: SideNo, Role: RoleReducing,
				Kind: KindCancelConfirmPlace, Reason: ReasonRequote}, WindingDown, P1},
		{"adding-side cancel-confirm-place cancels at P0",
			Intent{Market: "M", Side: SideYes, Role: RoleAdding,
				Kind: KindCancelConfirmPlace, Reason: ReasonRequote}, Running, P0},
		{"a place-then-cancel presents its PLACEMENT first, so it classifies as its reason",
			Intent{Market: "M", Side: SideYes, Role: RoleAdding,
				Kind: KindPlaceThenCancel, Reason: ReasonRequote}, Running, P3},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := Conditions{Global: tc.global, AboveSoft: above}
			if got := tc.in.Base(c); got != tc.want {
				t.Fatalf("Base(kind=%s reason=%s role=%s market=%s global=%s) = %s, want %s",
					tc.in.Kind, tc.in.Reason, tc.in.Role, tc.in.Market, tc.global, got, tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Ordering
// ---------------------------------------------------------------------------

// TestFreshIntentsDequeueInClassOrder is V1.9's headline: P0 through P4, and a
// P3 requote never precedes a P0 cancel.
//
// The intents are enqueued in the exact reverse of the order they must come
// out, so any scheduler that fell back to plain FIFO fails on the first
// dispatch rather than on a subtle one.
func TestFreshIntentsDequeueInClassOrder(t *testing.T) {
	q := NewQueue(maxQueueAge)
	c := running()
	c.AboveSoft = map[string]bool{"HOT": true}

	p4 := mustEnqueue(t, q, 0, place("A", SideYes, RoleAdding, ReasonTopUp))
	p3 := mustEnqueue(t, q, 0, place("B", SideYes, RoleAdding, ReasonRequote))
	p2 := mustEnqueue(t, q, 0, place("C", SideYes, RoleAdding, ReasonPresence))
	p1 := mustEnqueue(t, q, 0, place("HOT", SideNo, RoleReducing, ReasonReduce))
	p0 := mustEnqueue(t, q, 0, addCancel("D", SideYes))

	want := []struct {
		id    uint64
		class Class
	}{{p0, P0}, {p1, P1}, {p2, P2}, {p3, P3}, {p4, P4}}

	for i, w := range want {
		d := mustDequeue(t, q, c, roomy())
		if d.Class != w.class {
			t.Fatalf("dispatch %d: class %s, want %s (ids %v)", i, d.Class, w.class, d.IDs)
		}
		onlyID(t, d, w.id)
	}
	if q.Len() != 0 {
		t.Fatalf("queue holds %d intents after draining, want 0", q.Len())
	}
}

// TestFIFOWithinAnEffectiveClass pins the tiebreak. Without it, "same class"
// would be resolved by whatever order the implementation happened to scan in,
// and a starving intent could be overtaken forever by its own class.
func TestFIFOWithinAnEffectiveClass(t *testing.T) {
	q := NewQueue(maxQueueAge)
	c := running()

	first := mustEnqueue(t, q, 0, place("A", SideYes, RoleAdding, ReasonRequote))
	second := mustEnqueue(t, q, time.Second, place("B", SideYes, RoleAdding, ReasonRequote))
	third := mustEnqueue(t, q, 2*time.Second, place("C", SideYes, RoleAdding, ReasonRequote))

	c.Now = 3 * time.Second
	for i, want := range []uint64{first, second, third} {
		d := mustDequeue(t, q, c, roomy())
		if d.Class != P3 {
			t.Fatalf("dispatch %d: class %s, want P3", i, d.Class)
		}
		onlyID(t, d, want)
	}
}

// TestEligibleAddingCancelPrecedesFreshRequote is the falsification target
// stated as a test: "a requote ahead of an eligible adding-side cancel".
//
// The requote is enqueued FIRST and is a place-then-cancel, so the queue holds
// a P3 placement, that placement's own not-yet-eligible cancel leg, and a
// separate eligible P0 cancel. Only the eligible one may come out first.
func TestEligibleAddingCancelPrecedesFreshRequote(t *testing.T) {
	q := NewQueue(maxQueueAge)
	c := running()

	requote := mustEnqueue(t, q, 0, Intent{Market: "A", Side: SideYes, Role: RoleAdding,
		Kind: KindPlaceThenCancel, Reason: ReasonRequote})
	cancel := mustEnqueue(t, q, 0, addCancel("B", SideYes))

	d := mustDequeue(t, q, c, roomy())
	if d.Op != OpCancel || d.Class != P0 {
		t.Fatalf("first dispatch is %s at %s (ids %v); the eligible adding-side "+
			"cancel must precede the P3 requote", d.Op, d.Class, d.IDs)
	}
	onlyID(t, d, cancel)

	d = mustDequeue(t, q, c, roomy())
	if d.Op != OpPlace || d.Class != P3 {
		t.Fatalf("second dispatch is %s at %s, want the P3 placement", d.Op, d.Class)
	}
	onlyID(t, d, requote)
}

// ---------------------------------------------------------------------------
// The queue holds intents, not requests (§6.6)
// ---------------------------------------------------------------------------

// TestClearedIntentIsDroppedNotDispatched is the "re-evaluated at dequeue"
// rule. An intent whose triggering condition is no longer true is dropped, not
// sent -- and selection continues to the next valid one rather than stalling on
// the dead entry.
func TestClearedIntentIsDroppedNotDispatched(t *testing.T) {
	q := NewQueue(maxQueueAge)

	stale := mustEnqueue(t, q, 0, addCancel("GONE", SideYes))
	live := mustEnqueue(t, q, 0, place("STILL", SideYes, RoleAdding, ReasonPresence))

	c := running()
	// The cancel's order filled between the decision and the dequeue: there is
	// nothing left to cancel.
	c.Valid = func(in Intent) bool { return in.Market != "GONE" }

	d := mustDequeue(t, q, c, roomy())
	onlyID(t, d, live)
	if d.Class != P2 {
		t.Fatalf("dispatched class %s, want P2 -- the P0 was stale and must not have been sent", d.Class)
	}
	if q.Len() != 0 {
		t.Fatalf("queue holds %d after the drop and the dispatch, want 0", q.Len())
	}
	if _, ok := q.Get(stale); ok {
		t.Fatalf("the stale intent %d is still queued; it must be removed without dispatch", stale)
	}
	if q.Drops() != 1 {
		t.Fatalf("Drops() = %d, want 1", q.Drops())
	}
}

// TestEveryPendingIntentIsRevalidated checks that the drop pass is not a
// one-entry special case: a whole queue whose conditions cleared empties
// without a single dispatch.
func TestEveryPendingIntentIsRevalidated(t *testing.T) {
	q := NewQueue(maxQueueAge)
	for i, in := range []Intent{
		addCancel("A", SideYes),
		place("B", SideYes, RoleAdding, ReasonPresence),
		place("C", SideYes, RoleAdding, ReasonRequote),
		place("D", SideYes, RoleAdding, ReasonTopUp),
		{Market: "E", Side: SideYes, Role: RoleAdding, Kind: KindPlaceThenCancel, Reason: ReasonRequote},
		{Market: "F", Side: SideNo, Role: RoleReducing, Kind: KindCancelConfirmPlace, Reason: ReasonRequote},
	} {
		mustEnqueue(t, q, time.Duration(i)*time.Millisecond, in)
	}

	c := running()
	c.Valid = func(Intent) bool { return false }
	mustNotDequeue(t, q, c, roomy(), "every triggering condition had cleared")

	if q.Len() != 0 {
		t.Fatalf("queue holds %d after full invalidation, want 0", q.Len())
	}
	if q.Drops() != 6 {
		t.Fatalf("Drops() = %d, want 6", q.Drops())
	}
}

// TestCommittedPlaceThenCancelIsNotDroppedMidSequence.
//
// The drop rule is bounded by a safety argument, and this pins the boundary: a
// place-then-cancel whose replacement is already resting must still retire the
// old order. Dropping its cancel leg would leave two orders on one side, which
// is the aggregate overlap H-Q-5a forbids and red-team HR-004 found.
func TestCommittedPlaceThenCancelIsNotDroppedMidSequence(t *testing.T) {
	q := NewQueue(maxQueueAge)
	id := mustEnqueue(t, q, 0, Intent{Market: "A", Side: SideYes, Role: RoleAdding,
		Kind: KindPlaceThenCancel, Reason: ReasonRequote})

	c := running()
	d := mustDequeue(t, q, c, roomy())
	if d.Op != OpPlace {
		t.Fatalf("first leg dispatched %s, want place", d.Op)
	}
	if !q.AckPlace(id) {
		t.Fatalf("AckPlace(%d) did not advance the intent", id)
	}

	// The requote's trigger has since cleared -- the touch came back.
	c.Valid = func(Intent) bool { return false }
	d = mustDequeue(t, q, c, roomy())
	if d.Op != OpCancel {
		t.Fatalf("dispatched %s, want the committed cancel leg: dropping it "+
			"strands the order the replacement was placed against", d.Op)
	}
	onlyID(t, d, id)
	if q.Drops() != 0 {
		t.Fatalf("Drops() = %d; a committed second leg must not be dropped", q.Drops())
	}
}

// ---------------------------------------------------------------------------
// Anti-starvation (§6.6)
// ---------------------------------------------------------------------------

// TestPromotionIsOneClassPerCompletedInterval pins the arithmetic at both
// boundaries and at the P0 ceiling. One class per COMPLETED max_queue_age, so
// 29.999s promotes nothing and 30.000s promotes once.
func TestPromotionIsOneClassPerCompletedInterval(t *testing.T) {
	for _, tc := range []struct {
		age  time.Duration
		want Class
	}{
		{0, P4},
		{maxQueueAge - time.Nanosecond, P4},
		{maxQueueAge, P3},
		{2*maxQueueAge - time.Nanosecond, P3},
		{2 * maxQueueAge, P2},
		{3 * maxQueueAge, P1},
		{4 * maxQueueAge, P0},
		{5 * maxQueueAge, P0},
		{1000 * maxQueueAge, P0},
	} {
		if got := Promote(P4, tc.age, maxQueueAge); got != tc.want {
			t.Fatalf("Promote(P4, age=%v, %v) = %s, want %s", tc.age, maxQueueAge, got, tc.want)
		}
	}

	// The ceiling is P0 from every starting class, and a class already at P0
	// cannot wrap past it.
	for _, base := range []Class{P0, P1, P2, P3, P4} {
		if got := Promote(base, 99*maxQueueAge, maxQueueAge); got != P0 {
			t.Fatalf("Promote(%s, 99 intervals) = %s, want P0", base, got)
		}
	}
}

// TestPromotionThroughTheQueueAPI walks the same ladder through Queue.Effective
// with injected timestamps, which is what a caller actually uses.
func TestPromotionThroughTheQueueAPI(t *testing.T) {
	q := NewQueue(maxQueueAge)
	id := mustEnqueue(t, q, 10*time.Second, place("A", SideYes, RoleAdding, ReasonTopUp))
	in, ok := q.Get(id)
	if !ok {
		t.Fatalf("Get(%d) missing", id)
	}
	if in.Enqueued != 10*time.Second {
		t.Fatalf("Enqueued = %v, want the caller-supplied 10s", in.Enqueued)
	}

	for _, tc := range []struct {
		now  time.Duration
		want Class
	}{
		{10 * time.Second, P4},
		{39*time.Second + 999*time.Millisecond, P4},
		{40 * time.Second, P3},
		{70 * time.Second, P2},
		{100 * time.Second, P1},
		{130 * time.Second, P0},
		{1000 * time.Second, P0},
	} {
		c := running()
		c.Now = tc.now
		if got := q.Effective(in, c); got != tc.want {
			t.Fatalf("Effective at now=%v (enqueued 10s) = %s, want %s", tc.now, got, tc.want)
		}
	}
}

// TestAgedP2IsNotStarvedByAFreshP0.
//
// §6.6's table says P2 does not starve. The only thing that makes that true is
// promotion, and the only way to observe it is a P2 that has aged to P0 winning
// the FIFO tiebreak against a P0 enqueued after it.
func TestAgedP2IsNotStarvedByAFreshP0(t *testing.T) {
	q := NewQueue(maxQueueAge)
	aged := mustEnqueue(t, q, 0, place("OLD", SideYes, RoleAdding, ReasonPresence))
	fresh := mustEnqueue(t, q, 2*maxQueueAge, addCancel("NEW", SideYes))

	c := running()
	c.Now = 2 * maxQueueAge // the P2 has completed two intervals: P2 -> P0

	d := mustDequeue(t, q, c, roomy())
	if d.Class != P0 {
		t.Fatalf("aged presence intent dequeued at %s, want P0 after two intervals", d.Class)
	}
	onlyID(t, d, aged)

	d = mustDequeue(t, q, c, roomy())
	onlyID(t, d, fresh)
}

// TestP4StarvationIsBounded.
//
// A top-up sits behind an unending stream of requotes. Without promotion it
// never dispatches; with it, one completed interval is enough to put it level
// with a fresh P3, where being older wins.
func TestP4StarvationIsBounded(t *testing.T) {
	q := NewQueue(maxQueueAge)
	topUp := mustEnqueue(t, q, 0, place("TOPUP", SideYes, RoleAdding, ReasonTopUp))

	c := running()

	// A requote arrives 10s in. The top-up is still P4 and still loses.
	early := mustEnqueue(t, q, 10*time.Second, place("R1", SideYes, RoleAdding, ReasonRequote))
	c.Now = 10 * time.Second
	d := mustDequeue(t, q, c, roomy())
	onlyID(t, d, early)

	// Another requote arrives exactly at max_queue_age. The top-up has now
	// completed one interval, is P3, and is older.
	late := mustEnqueue(t, q, maxQueueAge, place("R2", SideYes, RoleAdding, ReasonRequote))
	c.Now = maxQueueAge
	d = mustDequeue(t, q, c, roomy())
	if d.Class != P3 {
		t.Fatalf("aged top-up dequeued at %s, want P3 after one completed interval", d.Class)
	}
	onlyID(t, d, topUp)

	d = mustDequeue(t, q, c, roomy())
	onlyID(t, d, late)
}

// ---------------------------------------------------------------------------
// H-QUE-1 / H-Q-9a -- dependent writes are one intent
// ---------------------------------------------------------------------------

// TestPlaceThenCancelIsOneIntentUnlockedOnlyByAck.
//
// H-QUE-1: enqueued as two intents the cancel is P0 and the placement is P3,
// and §6.6 dispatches them in the exact reverse of the mandated order
// (HR-019). As one intent the order is structural, and the gate is the ACK --
// not the dispatch, because a dispatched write can still be REJECTED or
// UNKNOWN (§7.2) and cancelling against a replacement that never rested is the
// presence gap H-Q-9 exists to avoid.
func TestPlaceThenCancelIsOneIntentUnlockedOnlyByAck(t *testing.T) {
	q := NewQueue(maxQueueAge)
	c := running()
	id := mustEnqueue(t, q, 0, Intent{Market: "A", Side: SideYes, Role: RoleAdding,
		Kind: KindPlaceThenCancel, Reason: ReasonRequote})

	if q.Len() != 1 {
		t.Fatalf("Len() = %d; a place-then-cancel occupies ONE logical intent", q.Len())
	}
	in, _ := q.Get(id)
	if in.Op() != OpPlace {
		t.Fatalf("initial leg is %s, want place: only the placement is initially dispatchable", in.Op())
	}

	d := mustDequeue(t, q, c, roomy())
	if d.Op != OpPlace || d.Class != P3 {
		t.Fatalf("first dispatch is %s at %s, want place at P3", d.Op, d.Class)
	}
	onlyID(t, d, id)

	// Dispatch alone does not unlock the cancel.
	in, ok := q.Get(id)
	if !ok {
		t.Fatalf("the intent left the queue after its first leg; the cancel leg is still owed")
	}
	if in.Stage() != StageFirstSent {
		t.Fatalf("stage = %s after dispatch, want %s", in.Stage(), StageFirstSent)
	}
	if in.Dispatchable() {
		t.Fatalf("the cancel leg is dispatchable with the placement still in flight (H-Q-9a)")
	}
	mustNotDequeue(t, q, c, roomy(), "dispatching the placement does not ACK it (H-Q-9a)")

	// Nor does an absence confirmation: that is the OTHER sequence's gate.
	if n := q.ConfirmAbsent("A", SideYes); n != 0 {
		t.Fatalf("ConfirmAbsent advanced %d place-then-cancel intents; its gate is the ACK", n)
	}
	mustNotDequeue(t, q, c, roomy(), "absence is not a placement ACK")

	// The explicit ACK, and only it, opens the gate.
	if !q.AckPlace(id) {
		t.Fatalf("AckPlace(%d) did not advance the intent", id)
	}
	in, _ = q.Get(id)
	if in.Stage() != StageSecond || in.Op() != OpCancel {
		t.Fatalf("after ACK: stage %s op %s, want %s / cancel", in.Stage(), in.Op(), StageSecond)
	}

	d = mustDequeue(t, q, c, roomy())
	if d.Op != OpCancel || d.Class != P0 {
		t.Fatalf("second dispatch is %s at %s, want the adding-side cancel at P0", d.Op, d.Class)
	}
	onlyID(t, d, id)
	if q.Len() != 0 {
		t.Fatalf("Len() = %d once both legs are out, want 0", q.Len())
	}
}

// TestPlaceThenCancelNeverPresentsItsCancelFirst is the inversion check stated
// directly: at no point before the ACK does the intent offer a cancel, at any
// class, under any conditions.
func TestPlaceThenCancelNeverPresentsItsCancelFirst(t *testing.T) {
	q := NewQueue(maxQueueAge)
	id := mustEnqueue(t, q, 0, Intent{Market: "A", Side: SideYes, Role: RoleAdding,
		Kind: KindPlaceThenCancel, Reason: ReasonRequote})

	// Age it far past every promotion boundary and put the world in
	// WINDING_DOWN, where "any write" is P0. Neither may reorder the legs.
	for _, g := range []GlobalState{Running, WindingDown} {
		c := running()
		c.Global = g
		c.Now = 10 * maxQueueAge
		in, _ := q.Get(id)
		if in.Op() != OpPlace {
			t.Fatalf("global=%s: leg is %s before the ACK, want place", g, in.Op())
		}
		d, ok := q.Dequeue(c, roomy())
		if !ok {
			t.Fatalf("global=%s: nothing dispatchable", g)
		}
		if d.Op != OpPlace {
			t.Fatalf("global=%s: dispatched %s first, want place (H-Q-9a, HR-019)", g, d.Op)
		}
		// Put it back for the next round.
		if !q.Drop(id) {
			t.Fatalf("Drop(%d) removed nothing", id)
		}
		id = mustEnqueue(t, q, 0, Intent{Market: "A", Side: SideYes, Role: RoleAdding,
			Kind: KindPlaceThenCancel, Reason: ReasonRequote})
	}
}

// ---------------------------------------------------------------------------
// H-QUE-2 -- cancelling a reducer is not "risk-reducing"
// ---------------------------------------------------------------------------

// TestStandaloneReducingCancelCannotExist.
//
// "A reducing-side cancel is P1 and is only ever issued as the first leg of a
// cancel-confirm-place -- never on its own." The queue refuses to hold one at
// all, which is the only form of that rule an implementation cannot forget.
func TestStandaloneReducingCancelCannotExist(t *testing.T) {
	q := NewQueue(maxQueueAge)
	_, err := q.Enqueue(0, Intent{Market: "HOT", Side: SideNo, Role: RoleReducing,
		Kind: KindCancel, Reason: ReasonCancel})
	if err == nil {
		t.Fatalf("Enqueue accepted a standalone reducing-side cancel; H-QUE-2 forbids it")
	}
	if q.Len() != 0 {
		t.Fatalf("a rejected intent was still admitted: Len() = %d", q.Len())
	}
}

// TestCancelConfirmPlaceIsP1AndGatedOnAbsence.
//
// The reducing sequence, end to end: the cancel is the FIRST leg, it is P1 and
// not P0, and the replacement stays ineligible until absence is confirmed by
// cancel response or sweep.
func TestCancelConfirmPlaceIsP1AndGatedOnAbsence(t *testing.T) {
	q := NewQueue(maxQueueAge)
	c := running()
	c.AboveSoft = map[string]bool{"HOT": true}

	id := mustEnqueue(t, q, 0, Intent{Market: "HOT", Side: SideNo, Role: RoleReducing,
		Kind: KindCancelConfirmPlace, Reason: ReasonRequote})

	d := mustDequeue(t, q, c, roomy())
	if d.Op != OpCancel {
		t.Fatalf("first leg is %s, want cancel", d.Op)
	}
	if d.Class != P1 {
		t.Fatalf("the reducing-side cancel dispatched at %s; H-QUE-2 makes it P1, "+
			"never P0 -- it removes the exit and does not get P0's bucket bypass", d.Class)
	}
	if d.Grant.BypassBucket {
		t.Fatalf("the reducing-side cancel was granted P0's bucket bypass")
	}
	onlyID(t, d, id)

	// The replacement is not eligible yet.
	in, ok := q.Get(id)
	if !ok {
		t.Fatalf("the intent left the queue after its cancel; the replacement is still owed")
	}
	if in.Stage() != StageFirstSent || in.Dispatchable() {
		t.Fatalf("stage %s dispatchable=%v after the cancel went out; the replacement "+
			"waits for confirmed absence", in.Stage(), in.Dispatchable())
	}
	mustNotDequeue(t, q, c, roomy(), "absence is not yet confirmed (H-Q-9 clause 3)")

	// A placement ACK is the OTHER sequence's gate and must not open this one.
	if q.AckPlace(id) {
		t.Fatalf("AckPlace advanced a cancel-confirm-place; its gate is absence, not an ACK")
	}
	mustNotDequeue(t, q, c, roomy(), "a placement ACK does not confirm absence")

	// A different market side is not this one.
	if n := q.ConfirmAbsent("HOT", SideYes); n != 0 {
		t.Fatalf("ConfirmAbsent on the other side advanced %d intents", n)
	}
	if n := q.ConfirmAbsent("COLD", SideNo); n != 0 {
		t.Fatalf("ConfirmAbsent on another market advanced %d intents", n)
	}
	mustNotDequeue(t, q, c, roomy(), "no confirmation for this (market, side) yet")

	if n := q.ConfirmAbsent("HOT", SideNo); n != 1 {
		t.Fatalf("ConfirmAbsent(HOT, no) advanced %d intents, want 1", n)
	}
	d = mustDequeue(t, q, c, roomy())
	if d.Op != OpPlace {
		t.Fatalf("second leg is %s, want the replacement placement", d.Op)
	}
	if d.Class != P1 {
		t.Fatalf("the reducing replacement above inv_soft dispatched at %s, want P1", d.Class)
	}
	onlyID(t, d, id)
	if q.Len() != 0 {
		t.Fatalf("Len() = %d once both legs are out, want 0", q.Len())
	}
}

// TestCancelConfirmPlaceNeverPlacesFirst is the inversion check for the
// reducing sequence: the replacement can never precede its own cancel, however
// old the intent gets or whatever the global state is.
func TestCancelConfirmPlaceNeverPlacesFirst(t *testing.T) {
	q := NewQueue(maxQueueAge)
	for _, g := range []GlobalState{Running, WindingDown} {
		id := mustEnqueue(t, q, 0, Intent{Market: "HOT", Side: SideNo, Role: RoleReducing,
			Kind: KindCancelConfirmPlace, Reason: ReasonRequote})
		c := running()
		c.Global = g
		c.Now = 10 * maxQueueAge
		c.AboveSoft = map[string]bool{"HOT": true}

		in, _ := q.Get(id)
		if in.Op() != OpCancel {
			t.Fatalf("global=%s: first leg is %s, want cancel", g, in.Op())
		}
		d := mustDequeue(t, q, c, roomy())
		if d.Op != OpCancel {
			t.Fatalf("global=%s: dispatched %s first, want the cancel", g, d.Op)
		}
		if !q.Drop(id) {
			t.Fatalf("Drop(%d) removed nothing", id)
		}
	}
}

// ---------------------------------------------------------------------------
// H-QUE-3 -- coalescing, and the reserve
// ---------------------------------------------------------------------------

// TestPendingCancelsCoalescePerMarketSide.
//
// "Cancels are coalesced per (market, side) rather than issued per order." Four
// pending cancels on one side are one write; the other side and the other
// market stay separate writes.
func TestPendingCancelsCoalescePerMarketSide(t *testing.T) {
	q := NewQueue(maxQueueAge)
	c := running()

	var aYes []uint64
	for i := 0; i < 4; i++ {
		aYes = append(aYes, mustEnqueue(t, q, time.Duration(i)*time.Millisecond, addCancel("A", SideYes)))
	}
	aNo := mustEnqueue(t, q, 10*time.Millisecond, addCancel("A", SideNo))
	bYes := mustEnqueue(t, q, 11*time.Millisecond, addCancel("B", SideYes))

	c.Now = 20 * time.Millisecond
	d := mustDequeue(t, q, c, roomy())
	if d.Market != "A" || d.Side != SideYes || d.Op != OpCancel {
		t.Fatalf("first dispatch is %s %s %s, want a cancel of A/yes", d.Op, d.Market, d.Side)
	}
	if len(d.IDs) != 4 {
		t.Fatalf("the A/yes cancel discharged %v (%d intents), want all 4 coalesced into one write",
			d.IDs, len(d.IDs))
	}
	for i, id := range aYes {
		if d.IDs[i] != id {
			t.Fatalf("coalesced ids %v, want %v in enqueue order", d.IDs, aYes)
		}
	}

	// The other keys are distinct and each is still its own write.
	d = mustDequeue(t, q, c, roomy())
	if d.Market != "A" || d.Side != SideNo {
		t.Fatalf("second dispatch is %s/%s, want A/no", d.Market, d.Side)
	}
	onlyID(t, d, aNo)

	d = mustDequeue(t, q, c, roomy())
	if d.Market != "B" || d.Side != SideYes {
		t.Fatalf("third dispatch is %s/%s, want B/yes", d.Market, d.Side)
	}
	onlyID(t, d, bYes)

	if q.Len() != 0 {
		t.Fatalf("Len() = %d after three cancel writes, want 0", q.Len())
	}
}

// TestCoalescedCancelIsIndependentlyDispatchable checks that a group is one
// unit of scheduling as well as one write: it takes ONE capacity grant, not one
// per member.
func TestCoalescedCancelIsIndependentlyDispatchable(t *testing.T) {
	q := NewQueue(maxQueueAge)
	c := running()
	for i := 0; i < 5; i++ {
		mustEnqueue(t, q, 0, addCancel("A", SideYes))
	}
	// One general worker, one write, and the reserve untouchable by P0.
	cp := Capacity{Workers: 2, ReservedWorkers: 1, Tokens: 1, ReservedTokens: 1}
	if err := cp.Validate(); err != nil {
		t.Fatalf("test capacity is invalid: %v", err)
	}

	d := mustDequeue(t, q, c, cp)
	if len(d.IDs) != 5 {
		t.Fatalf("coalesced group discharged %d intents, want 5", len(d.IDs))
	}
	if q.Len() != 0 {
		t.Fatalf("Len() = %d after the coalesced write, want 0 -- one grant cleared the group", q.Len())
	}
}

// TestReserveFloorsArePositive pins H-QUE-3's floors across the whole plausible
// parameter space: "at least one worker slot and a reserved share of the write
// budget are held for P1 at all times".
func TestReserveFloorsArePositive(t *testing.T) {
	for workers := 1; workers <= 64; workers++ {
		r := ReserveWorkers(workers)
		if r < 1 {
			t.Fatalf("ReserveWorkers(%d) = %d; H-QUE-3 holds at least one slot for P1", workers, r)
		}
		if workers >= 2 && r > workers-1 {
			t.Fatalf("ReserveWorkers(%d) = %d leaves no general pool", workers, r)
		}
	}
	for burst := 1; burst <= 200; burst++ {
		if w := ReserveWrites(burst); w < 1 {
			t.Fatalf("ReserveWrites(%d) = %d; a reserve of zero is not a reserve", burst, w)
		}
	}
	// The §16 defaults must produce a usable capacity.
	cp := NewCapacity(4, 10)
	if err := cp.Validate(); err != nil {
		t.Fatalf("NewCapacity(4, 10) is invalid: %v", err)
	}
	if cp.ReservedWorkers < 1 || cp.ReservedTokens < 1 {
		t.Fatalf("NewCapacity(4, 10) reserved %d workers / %d writes, want both positive",
			cp.ReservedWorkers, cp.ReservedTokens)
	}
	if cp.Workers <= cp.ReservedWorkers {
		t.Fatalf("NewCapacity(4, 10) left no general pool: %d workers, %d reserved",
			cp.Workers, cp.ReservedWorkers)
	}
}

// TestAdmitKeepsBothReservesForP1Only is H-QUE-3 as a truth table.
//
// P0 bypasses only the LOCAL token bucket. It does not bypass the worker pool,
// and it cannot reach either reserve -- which is the entire point: a cancel
// storm absorbing 429s must not be able to occupy the slot the exit needs.
func TestAdmitKeepsBothReservesForP1Only(t *testing.T) {
	// The general pool is full; only the P1 reserve is left.
	full := Capacity{Workers: 3, ReservedWorkers: 1, BusyGeneral: 2, Tokens: 4, ReservedTokens: 2}
	if err := full.Validate(); err != nil {
		t.Fatalf("test capacity is invalid: %v", err)
	}
	for _, k := range []Class{P0, P2, P3, P4} {
		if g, ok := full.Admit(k); ok {
			t.Fatalf("%s admitted with the general worker pool full (grant %+v); "+
				"the reserve is P1's", k, g)
		}
	}
	g, ok := full.Admit(P1)
	if !ok {
		t.Fatalf("P1 refused a worker with its reserve free; H-QUE-3 holds it for exactly this")
	}
	if !g.ReservedWorker {
		t.Fatalf("P1 grant %+v did not draw on the worker reserve", g)
	}
	if g.ReservedToken {
		t.Fatalf("P1 spent the token reserve while the ordinary bucket had %d", full.Tokens)
	}

	// The ordinary bucket is empty; only the write reserve is left.
	dry := Capacity{Workers: 3, ReservedWorkers: 1, Tokens: 0, ReservedTokens: 2}
	for _, k := range []Class{P2, P3, P4} {
		if _, ok := dry.Admit(k); ok {
			t.Fatalf("%s admitted with an empty bucket; the reserved writes are P1's", k)
		}
	}
	if g, ok := dry.Admit(P0); !ok || !g.BypassBucket || g.ReservedToken {
		t.Fatalf("P0 with an empty bucket: ok=%v grant=%+v; §6.6 gives it the bypass "+
			"and H-QUE-3 gives it nothing from the reserve", ok, g)
	}
	if g, ok := dry.Admit(P1); !ok || !g.ReservedToken {
		t.Fatalf("P1 with an empty bucket: ok=%v grant=%+v, want the reserved write", ok, g)
	}

	// Everything gone: even P1 is refused, and nothing is invented.
	empty := Capacity{Workers: 2, ReservedWorkers: 1, BusyGeneral: 1, BusyReserved: 1,
		Tokens: 0, ReservedTokens: 0}
	for _, k := range []Class{P0, P1, P2, P3, P4} {
		if _, ok := empty.Admit(k); ok {
			t.Fatalf("%s admitted with no capacity at all", k)
		}
	}
}

// TestP0CancelStormCannotStarveAWaitingP1Reducer is V4's
// unbounded-cancel-storm-with-waiting-reducer case, run against the scheduler
// alone.
//
// The storm is unbounded: a fresh P0 cancel is enqueued after every dispatch,
// on a market of its own so nothing coalesces it away. The bucket is empty, so
// only P0's bypass and P1's reserve can pay for anything. The reducer is
// enqueued LAST, so both class order and FIFO order are against it, and it must
// still get out.
func TestP0CancelStormCannotStarveAWaitingP1Reducer(t *testing.T) {
	q := NewQueue(maxQueueAge)
	c := running()
	c.AboveSoft = map[string]bool{"HOT": true}

	storm := 0
	enqueueStormCancel := func(now time.Duration) {
		storm++
		mustEnqueue(t, q, now, addCancel(fmt.Sprintf("STORM-%03d", storm), SideYes))
	}
	for i := 0; i < 8; i++ {
		enqueueStormCancel(0)
	}
	reducer := mustEnqueue(t, q, time.Millisecond,
		place("HOT", SideNo, RoleReducing, ReasonReduce))

	// Two general workers, one reserved for P1. The local bucket is already
	// drained -- which is exactly the state a cancel storm produces, and which
	// P0 does not care about because it bypasses it.
	cp := Capacity{Workers: 3, ReservedWorkers: 1, Tokens: 0, ReservedTokens: 2}
	if err := cp.Validate(); err != nil {
		t.Fatalf("test capacity is invalid: %v", err)
	}

	var reducerDispatch *Dispatch
	c.Now = time.Second
	for i := 0; i < 32; i++ {
		d, ok := q.Dequeue(c, cp)
		if !ok {
			break
		}
		cp = cp.Take(d.Grant)
		if d.Class == P0 {
			if d.Grant.ReservedWorker || d.Grant.ReservedToken {
				t.Fatalf("a P0 cancel consumed P1 capacity: grant %+v", d.Grant)
			}
			// Unbounded: the storm replenishes itself.
			enqueueStormCancel(c.Now)
			continue
		}
		if len(d.IDs) == 1 && d.IDs[0] == reducer {
			got := d
			reducerDispatch = &got
			break
		}
		t.Fatalf("unexpected dispatch %+v", d)
	}

	if reducerDispatch == nil {
		t.Fatalf("the waiting P1 reducer never dispatched under an unbounded P0 cancel "+
			"storm; H-QUE-3 reserves a worker slot and a write share for exactly this "+
			"(capacity ended at %+v)", cp)
	}
	d := *reducerDispatch
	if d.Class != P1 {
		t.Fatalf("the reducer dispatched at %s, want P1", d.Class)
	}
	if !d.Grant.ReservedWorker {
		t.Fatalf("the reducer did not need the worker reserve (grant %+v); the storm was "+
			"not actually occupying the general pool, so this test proves nothing", d.Grant)
	}
	if !d.Grant.ReservedToken {
		t.Fatalf("the reducer did not need the write reserve (grant %+v); the ordinary "+
			"bucket was supposed to be empty", d.Grant)
	}

	// And with both reserves now spent, nothing else gets through -- the P0
	// storm never acquires them by attrition.
	if d, ok := q.Dequeue(c, cp); ok {
		t.Fatalf("dispatched %s %s at %s with every pool exhausted (grant %+v)",
			d.Op, d.Market, d.Class, d.Grant)
	}

	// ------------------------------------------------------------------
	// Act two: the same storm, against a reducer that has WAITED.
	//
	// Above the first act reaches its reducer at now=1s, before any promotion
	// boundary, so it only ever exercises a reducer whose effective class and
	// base class agree. They stop agreeing after one completed max_queue_age,
	// and that is the state H-QUE-3 is actually about: the longer a cancel
	// storm holds the general pool, the older the exit gets, and promotion
	// makes it effective P0. If that promoted class is what capacity is asked
	// about, the exit loses both reserves at exactly the moment it has proved
	// it needs them, and the storm starves it permanently.
	// ------------------------------------------------------------------
	cp = Capacity{Workers: 3, ReservedWorkers: 1, Tokens: 0, ReservedTokens: 2}
	if err := cp.Validate(); err != nil {
		t.Fatalf("act two capacity is invalid: %v", err)
	}
	aged := mustEnqueue(t, q, c.Now, place("HOT", SideNo, RoleReducing, ReasonReduce))
	c.Now += maxQueueAge // one completed interval

	in, ok := q.Get(aged)
	if !ok {
		t.Fatalf("the waiting reducer left the queue before it dispatched")
	}
	if got := in.Base(c); got != P1 {
		t.Fatalf("the waiting reducer's base class is %s, want P1: it is the exit "+
			"above inv_soft, and waiting did not stop it being the exit", got)
	}
	if got := q.Effective(in, c); got != P0 {
		t.Fatalf("the waiting reducer is effective %s after a completed interval, "+
			"want P0; this act does not reach the promoted state and proves nothing", got)
	}

	var agedDispatch *Dispatch
	for i := 0; i < 32; i++ {
		d, ok := q.Dequeue(c, cp)
		if !ok {
			break
		}
		cp = cp.Take(d.Grant)
		if len(d.IDs) == 1 && d.IDs[0] == aged {
			got := d
			agedDispatch = &got
			break
		}
		if d.Class != P0 {
			t.Fatalf("unexpected dispatch %+v", d)
		}
		if d.Grant.ReservedWorker || d.Grant.ReservedToken {
			t.Fatalf("a P0 cancel consumed P1 capacity: grant %+v", d.Grant)
		}
		// Still unbounded.
		enqueueStormCancel(c.Now)
	}

	if agedDispatch == nil {
		t.Fatalf("the reducer never dispatched once promotion had made it effective "+
			"P0: the storm starves the exit precisely by outlasting it. H-QUE-3's "+
			"reserves belong to the write's §6.6 row, which is still P1; promotion "+
			"moves it up the queue and must not move it out of its reserves "+
			"(capacity ended at %+v)", cp)
	}
	d = *agedDispatch
	if d.Class != P0 {
		t.Fatalf("the promoted reducer dispatched at %s, want the effective class P0", d.Class)
	}
	if !d.Grant.ReservedWorker {
		t.Fatalf("the promoted reducer did not need the worker reserve (grant %+v); "+
			"the storm was not occupying the general pool, so this act proves nothing",
			d.Grant)
	}
	if !d.Grant.ReservedToken {
		t.Fatalf("the promoted reducer did not need the write reserve (grant %+v); "+
			"the ordinary bucket was supposed to be empty", d.Grant)
	}
	if d.Grant.BypassBucket {
		t.Fatalf("the promoted reducer was granted P0's bucket bypass (grant %+v); "+
			"that privilege is a cancel's, and promotion does not confer it", d.Grant)
	}
}

// TestPromotionMovesOrderNotEntitlement separates the two rules that share
// §6.6's vocabulary, with no storm and no coalescing in the way.
//
//   - §6.6's anti-starvation rule promotes a waiting intent one class per
//     completed max_queue_age. It decides where the intent SITS in the queue.
//   - H-QUE-3 holds a worker slot and a share of the write budget for P1 at all
//     times. It decides what the intent may SPEND, and it is justified by what
//     the write is -- a reducing-side write above inv_soft is the exit.
//
// Age changes the first and cannot change the second, in either direction: an
// aged reducer keeps both P1 reserves after promotion has made it effective P0,
// and an aged requote does not acquire P0's bucket bypass by waiting. Charging
// capacity for the promoted class instead of the §6.6 row silently swaps both.
func TestPromotionMovesOrderNotEntitlement(t *testing.T) {
	c := running()
	c.AboveSoft = map[string]bool{"HOT": true}
	c.Now = maxQueueAge // one completed interval for anything enqueued at 0

	// A reducing placement above inv_soft: base P1, promoted to effective P0.
	q := NewQueue(maxQueueAge)
	reducer := mustEnqueue(t, q, 0, place("HOT", SideNo, RoleReducing, ReasonReduce))
	in, ok := q.Get(reducer)
	if !ok {
		t.Fatalf("Get(%d) found nothing", reducer)
	}
	if base := in.Base(c); base != P1 {
		t.Fatalf("base class is %s, want P1: §6.6's P1 row is a reducing-side "+
			"placement while |q| > inv_soft, and that is what this is", base)
	}
	if eff := q.Effective(in, c); eff != P0 {
		t.Fatalf("effective class is %s after one completed interval, want P0", eff)
	}

	// Only the reserves are left: the general pool is full and the ordinary
	// bucket is dry. This is the state a cancel storm produces.
	reserveOnly := Capacity{Workers: 3, ReservedWorkers: 1, BusyGeneral: 2,
		Tokens: 0, ReservedTokens: 1}
	if err := reserveOnly.Validate(); err != nil {
		t.Fatalf("test capacity is invalid: %v", err)
	}

	d := mustDequeue(t, q, c, reserveOnly)
	onlyID(t, d, reducer)
	if d.Class != P0 {
		t.Fatalf("the reducer dispatched at %s; promotion is what stops P1 waiting "+
			"behind fresher work, so the reported class is the effective one", d.Class)
	}
	if !d.Grant.ReservedWorker || !d.Grant.ReservedToken {
		t.Fatalf("the promoted reducer got grant %+v from %+v, want both reserves: "+
			"H-QUE-3 holds them for the exit, and the exit is what this write is "+
			"whatever its queue position has aged into", d.Grant, reserveOnly)
	}
	if d.Grant.BypassBucket {
		t.Fatalf("the promoted reducer got P0's bucket bypass (grant %+v)", d.Grant)
	}

	// The converse. A requote is P3 and stays a requote; promotion may sort it
	// at P0 but §6.6's bypass is justified by "a cancel can only reduce
	// exposure", which is a property of the write and not of its age.
	q = NewQueue(maxQueueAge)
	requote := mustEnqueue(t, q, 0, place("HOT", SideYes, RoleAdding, ReasonRequote))
	c.Now = 3 * maxQueueAge // three completed intervals: P3 -> P0
	in, ok = q.Get(requote)
	if !ok {
		t.Fatalf("Get(%d) found nothing", requote)
	}
	if base, eff := in.Base(c), q.Effective(in, c); base != P3 || eff != P0 {
		t.Fatalf("the aged requote is base %s / effective %s, want P3 / P0", base, eff)
	}

	// A free general worker, no ordinary token, and a reserve it may not touch.
	dry := Capacity{Workers: 3, ReservedWorkers: 1, Tokens: 0, ReservedTokens: 1}
	if err := dry.Validate(); err != nil {
		t.Fatalf("test capacity is invalid: %v", err)
	}
	mustNotDequeue(t, q, c, dry,
		"an aged requote is sorted at P0 but is not a cancel: it pays an ordinary "+
			"token like every other P3, and there is none")

	// One ordinary token and it goes, paying for itself and touching nothing
	// reserved.
	dry.Tokens = 1
	d = mustDequeue(t, q, c, dry)
	onlyID(t, d, requote)
	if d.Grant.BypassBucket || d.Grant.ReservedWorker || d.Grant.ReservedToken {
		t.Fatalf("the aged requote dispatched on grant %+v; it is entitled to the "+
			"general pool and an ordinary token, and to nothing else", d.Grant)
	}
	if left := dry.Take(d.Grant); left.Tokens != 0 || left.ReservedTokens != 1 {
		t.Fatalf("after the requote: %d ordinary / %d reserved tokens, want 0 / 1",
			left.Tokens, left.ReservedTokens)
	}
}

// ---------------------------------------------------------------------------
// Shapes that must not be admitted
// ---------------------------------------------------------------------------

func TestEnqueueRejectsImpossibleIntentShapes(t *testing.T) {
	cases := []struct {
		name string
		in   Intent
	}{
		{"standalone reducing cancel (H-QUE-2)",
			Intent{Market: "M", Side: SideNo, Role: RoleReducing, Kind: KindCancel, Reason: ReasonCancel}},
		{"place-then-cancel on the reducing side (H-Q-9 clause 3, HR-004)",
			Intent{Market: "M", Side: SideNo, Role: RoleReducing, Kind: KindPlaceThenCancel, Reason: ReasonRequote}},
		{"no market, so nothing to coalesce on",
			Intent{Side: SideYes, Role: RoleAdding, Kind: KindCancel, Reason: ReasonCancel}},
		{"a bare placement claiming the cancel reason",
			Intent{Market: "M", Side: SideYes, Role: RoleAdding, Kind: KindPlace, Reason: ReasonCancel}},
		{"a standalone cancel claiming a placement reason",
			Intent{Market: "M", Side: SideYes, Role: RoleAdding, Kind: KindCancel, Reason: ReasonRequote}},
		{"cancel-confirm-place claiming the cancel reason",
			Intent{Market: "M", Side: SideNo, Role: RoleReducing, Kind: KindCancelConfirmPlace, Reason: ReasonCancel}},
		{"the P1 reduce row on the adding side",
			Intent{Market: "M", Side: SideYes, Role: RoleAdding, Kind: KindPlace, Reason: ReasonReduce}},
		{"an undefined kind",
			Intent{Market: "M", Side: SideYes, Role: RoleAdding, Kind: Kind(9), Reason: ReasonPresence}},
		{"an undefined reason",
			Intent{Market: "M", Side: SideYes, Role: RoleAdding, Kind: KindPlace, Reason: Reason(9)}},
		{"an undefined side",
			Intent{Market: "M", Side: Side(7), Role: RoleAdding, Kind: KindPlace, Reason: ReasonPresence}},
		{"an undefined role",
			Intent{Market: "M", Side: SideYes, Role: Role(7), Kind: KindPlace, Reason: ReasonPresence}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			q := NewQueue(maxQueueAge)
			id, err := q.Enqueue(0, tc.in)
			if err == nil {
				t.Fatalf("Enqueue admitted %+v as id %d", tc.in, id)
			}
			if q.Len() != 0 {
				t.Fatalf("a rejected intent was admitted anyway: Len() = %d", q.Len())
			}
		})
	}
}

// TestEnqueueAdmitsEveryLegalShape is the other half: the rejections above must
// not have been achieved by refusing things the spec requires.
func TestEnqueueAdmitsEveryLegalShape(t *testing.T) {
	legal := []Intent{
		addCancel("M", SideYes),
		place("M", SideYes, RoleAdding, ReasonPresence),
		place("M", SideYes, RoleAdding, ReasonRequote),
		place("M", SideYes, RoleAdding, ReasonTopUp),
		place("M", SideNo, RoleReducing, ReasonReduce),
		place("M", SideNo, RoleReducing, ReasonRequote),
		{Market: "M", Side: SideYes, Role: RoleAdding, Kind: KindPlaceThenCancel, Reason: ReasonRequote},
		{Market: "M", Side: SideNo, Role: RoleReducing, Kind: KindCancelConfirmPlace, Reason: ReasonRequote},
		{Market: "M", Side: SideYes, Role: RoleAdding, Kind: KindCancelConfirmPlace, Reason: ReasonRequote},
	}
	q := NewQueue(maxQueueAge)
	for i, in := range legal {
		if _, err := q.Enqueue(time.Duration(i), in); err != nil {
			t.Fatalf("Enqueue rejected the legal intent %+v: %v", in, err)
		}
	}
	if q.Len() != len(legal) {
		t.Fatalf("Len() = %d, want %d", q.Len(), len(legal))
	}
}

// TestEnqueueRefusesAPreOpenedGate. An intent submitted already at StageSecond
// would be claiming a confirmation it never received, which is H-Q-9a's gate
// removed by the caller rather than by the scheduler.
func TestEnqueueRefusesAPreOpenedGate(t *testing.T) {
	q := NewQueue(maxQueueAge)
	in := Intent{Market: "M", Side: SideYes, Role: RoleAdding,
		Kind: KindPlaceThenCancel, Reason: ReasonRequote, stage: StageSecond}
	if _, err := q.Enqueue(0, in); err == nil {
		t.Fatalf("Enqueue admitted an intent whose leg gate was already open")
	}
}

func TestCapacityValidateRefusesAMissingReserve(t *testing.T) {
	cases := []struct {
		name string
		cp   Capacity
	}{
		{"no reserved worker", Capacity{Workers: 4, ReservedWorkers: 0, Tokens: 5, ReservedTokens: 2}},
		{"reserve is the whole pool", Capacity{Workers: 2, ReservedWorkers: 2, Tokens: 5, ReservedTokens: 2}},
		{"more busy than the general pool holds",
			Capacity{Workers: 3, ReservedWorkers: 1, BusyGeneral: 3, Tokens: 5, ReservedTokens: 2}},
		{"more reserved busy than reserved",
			Capacity{Workers: 3, ReservedWorkers: 1, BusyReserved: 2, Tokens: 5, ReservedTokens: 2}},
		{"negative tokens", Capacity{Workers: 3, ReservedWorkers: 1, Tokens: -1, ReservedTokens: 2}},
		{"negative reserved tokens", Capacity{Workers: 3, ReservedWorkers: 1, Tokens: 1, ReservedTokens: -1}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.cp.Validate(); err == nil {
				t.Fatalf("Validate accepted %+v", tc.cp)
			}
		})
	}
}

// TestTakeChargesThePoolTheGrantNames keeps the accounting honest: a bypassing
// P0 spends no token, and a reserved grant never draws down the ordinary pools.
func TestTakeChargesThePoolTheGrantNames(t *testing.T) {
	base := Capacity{Workers: 3, ReservedWorkers: 1, Tokens: 2, ReservedTokens: 2}

	after := base.Take(Grant{BypassBucket: true})
	if after.BusyGeneral != 1 || after.Tokens != 2 || after.ReservedTokens != 2 || after.BusyReserved != 0 {
		t.Fatalf("P0 bypass charged %+v, want one general worker and no token", after)
	}

	after = base.Take(Grant{ReservedWorker: true, ReservedToken: true})
	if after.BusyReserved != 1 || after.ReservedTokens != 1 || after.BusyGeneral != 0 || after.Tokens != 2 {
		t.Fatalf("reserved grant charged %+v, want one reserved worker and one reserved write", after)
	}

	after = base.Take(Grant{})
	if after.BusyGeneral != 1 || after.Tokens != 1 || after.BusyReserved != 0 || after.ReservedTokens != 2 {
		t.Fatalf("ordinary grant charged %+v, want one general worker and one ordinary write", after)
	}
}

// ---------------------------------------------------------------------------
// Bookkeeping
// ---------------------------------------------------------------------------

// TestDropRemovesAnAbandonedSequence covers the rejected-placement path: a
// place-then-cancel whose replacement was REJECTED (§7.2) has no cancel to
// issue, and the caller must be able to abandon it.
func TestDropRemovesAnAbandonedSequence(t *testing.T) {
	q := NewQueue(maxQueueAge)
	c := running()
	id := mustEnqueue(t, q, 0, Intent{Market: "M", Side: SideYes, Role: RoleAdding,
		Kind: KindPlaceThenCancel, Reason: ReasonRequote})
	mustDequeue(t, q, c, roomy())

	if !q.Drop(id) {
		t.Fatalf("Drop(%d) removed nothing", id)
	}
	if q.Drop(id) {
		t.Fatalf("Drop(%d) removed the same intent twice", id)
	}
	if q.Len() != 0 {
		t.Fatalf("Len() = %d after the drop, want 0", q.Len())
	}
	if q.AckPlace(id) {
		t.Fatalf("AckPlace advanced a dropped intent")
	}
}

// TestIDsAndPendingOrder pins the two observables the rest of the harness
// reconciles against: ids are unique and monotonic, and Pending is enqueue
// order.
func TestIDsAndPendingOrder(t *testing.T) {
	q := NewQueue(maxQueueAge)
	var ids []uint64
	for i := 0; i < 5; i++ {
		ids = append(ids, mustEnqueue(t, q, time.Duration(i), addCancel(fmt.Sprintf("M%d", i), SideYes)))
	}
	seen := map[uint64]bool{}
	for i, id := range ids {
		if id == 0 || seen[id] {
			t.Fatalf("id %d at position %d is zero or a duplicate of an earlier one", id, i)
		}
		if i > 0 && id <= ids[i-1] {
			t.Fatalf("ids are not monotonic: %v", ids)
		}
		seen[id] = true
	}
	pending := q.Pending()
	if len(pending) != len(ids) {
		t.Fatalf("Pending() has %d, want %d", len(pending), len(ids))
	}
	for i, in := range pending {
		if in.ID != ids[i] {
			t.Fatalf("Pending()[%d].ID = %d, want %d", i, in.ID, ids[i])
		}
		if in.Enqueued != time.Duration(i) {
			t.Fatalf("Pending()[%d].Enqueued = %v, want %v", i, in.Enqueued, time.Duration(i))
		}
	}
}

// TestEmptyQueueDispatchesNothing. A queue with nothing in it must report that,
// not a zero-valued Dispatch a caller could mistake for a write.
func TestEmptyQueueDispatchesNothing(t *testing.T) {
	q := NewQueue(maxQueueAge)
	mustNotDequeue(t, q, running(), roomy(), "the queue is empty")
	if q.Len() != 0 || q.Drops() != 0 {
		t.Fatalf("Len() = %d Drops() = %d on an empty queue", q.Len(), q.Drops())
	}
}
