package core

import "testing"

// Gate 3 of port-spec.md: standalone tests pinning each preservation rule by
// name, independent of the tape. The differential replay would probably catch
// most of these eventually, but "probably, on this tape" is not a safety
// argument. A failure here names the rule that broke.

// --- P2: insertion-ordered summation -------------------------------------

// NOTE ON THE MISSING TEST.
//
// There is deliberately NO test asserting that Sum() follows insertion order
// rather than key order, because no such test can be written honestly.
//
// The original theory was that Python's insertion-ordered dict drove the
// `reference` depth sums, since float addition is not associative. That theory
// was wrong: CPython's sum() applies Neumaier compensation (P24), which is
// order-independent for every input reachable here — 700k randomised trials
// across realistic book sizes, large books, and adversarial magnitude mixes
// produced zero order-sensitive cases. Sum() is also the only consumer of
// insertion order; Max() and SortedDesc() are order-free by construction.
//
// So the ordering in Levels is faithful to CPython and free, but it is NOT
// observable in any output, and the negative control confirms that replacing it
// with plain map iteration changes nothing. Asserting otherwise here would be a
// test that passes for the wrong reason. The ordering CONTRACT is still pinned,
// by the two tests below.

func TestP2_OverwriteKeepsPositionDeleteThenReinsertAppends(t *testing.T) {
	l := NewLevels()
	l.Set(10, 1)
	l.Set(20, 2)
	l.Set(30, 3)

	l.Set(10, 99) // assigning an existing key must NOT move it
	if got := l.keys; got[0] != 10 || got[1] != 20 || got[2] != 30 {
		t.Errorf("after overwrite, order = %v, want [10 20 30]", got)
	}

	l.Delete(10)
	l.Set(10, 5) // re-inserting a deleted key appends at the END
	if got := l.keys; got[0] != 20 || got[1] != 30 || got[2] != 10 {
		t.Errorf("after delete+reinsert, order = %v, want [20 30 10]", got)
	}
	if v, _ := l.Get(10); v != 5 {
		t.Errorf("value after reinsert = %v, want 5", v)
	}
}

func TestP2_DeleteReindexesCorrectly(t *testing.T) {
	l := NewLevels()
	for i := 1; i <= 5; i++ {
		l.Set(i*10, float64(i))
	}
	l.Delete(30)
	l.Delete(10)
	if got := l.keys; len(got) != 3 || got[0] != 20 || got[1] != 40 || got[2] != 50 {
		t.Fatalf("order after deletes = %v, want [20 40 50]", got)
	}
	// A stale index would make a subsequent delete remove the wrong key.
	l.Delete(40)
	if got := l.keys; len(got) != 2 || got[0] != 20 || got[1] != 50 {
		t.Errorf("order after third delete = %v, want [20 50]", got)
	}
}

// --- P3/P4/P5/P6: state_before ------------------------------------------

// mark stamps a book with an identifiable yes level so a test can tell which
// checkpoint StateBefore returned.
func mark(b *Book, id int) { b.yes = NewLevels(); b.yes.Set(id, 1) }

func idOf(l *Levels) int {
	p, _, ok := l.Max()
	if !ok {
		return -1
	}
	return p
}

func TestP3_TimestampTieSelectsLastCheckpoint(t *testing.T) {
	b := NewBook(0)
	mark(b, 1)
	b.Checkpoint(1.0)
	mark(b, 2)
	b.Checkpoint(1.0) // same timestamp: this one must win

	yes, _, lag := b.StateBefore(2.0)
	if idOf(yes) != 2 {
		t.Errorf("chose checkpoint %d, want 2 (>= comparison means the LAST tie wins)", idOf(yes))
	}
	if lag == nil || *lag != 1.0 {
		t.Errorf("lag = %v, want 1.0", lag)
	}
}

func TestP4_OutOfOrderCheckpointDoesNotHideLaterOnes(t *testing.T) {
	b := NewBook(0)
	mark(b, 1)
	b.Checkpoint(1.0)
	mark(b, 2)
	b.Checkpoint(5.0) // arrives out of order, newer than the query
	mark(b, 3)
	b.Checkpoint(2.0)

	// An implementation that breaks out of the scan on the first checkpoint
	// newer than ts stops at 5.0 and wrongly returns checkpoint 1.
	yes, _, _ := b.StateBefore(3.0)
	if idOf(yes) != 3 {
		t.Errorf("chose checkpoint %d, want 3 (the scan must not break early)", idOf(yes))
	}
}

func TestP5_AllCheckpointsNewerFallsBackToOldestWithNegativeLag(t *testing.T) {
	b := NewBook(0)
	mark(b, 1)
	b.Checkpoint(10.0)
	mark(b, 2)
	b.Checkpoint(11.0)

	yes, _, lag := b.StateBefore(5.0)
	if idOf(yes) != 1 {
		t.Errorf("chose checkpoint %d, want 1 (the OLDEST retained state)", idOf(yes))
	}
	if lag == nil {
		t.Fatal("lag = nil, want a real negative lag")
	}
	if *lag != -5.0 {
		t.Errorf("lag = %v, want -5.0 (negative lag is legitimate, not an error)", *lag)
	}
}

func TestP6_EmptyHistoryUsesLiveBookAndNilLag(t *testing.T) {
	b := NewBook(0)
	b.yes.Set(42, 7)

	yes, _, lag := b.StateBefore(1.0)
	if lag != nil {
		t.Errorf("lag = %v, want nil (distinct from a lag of zero; writes SQL NULL)", *lag)
	}
	if idOf(yes) != 42 {
		t.Errorf("did not fall back to the live book, got %d", idOf(yes))
	}
	// It must be a copy: mutating the live book afterwards must not alter it.
	b.yes.Set(99, 1)
	if idOf(yes) != 42 {
		t.Error("returned a live reference to the book, not a copy")
	}
}

// --- P9: the qualifying walk --------------------------------------------

func qBook(target float64, yes, no map[int]float64, yesOrder, noOrder []int) *Book {
	b := NewBook(target)
	for _, p := range yesOrder {
		b.yes.Set(p, yes[p])
	}
	for _, p := range noOrder {
		b.no.Set(p, no[p])
	}
	return b
}

func TestP9_QualifiesClearsWhenTargetNeverReached(t *testing.T) {
	// Yes reaches 100 cumulative, no reaches only 40 against a target of 100.
	// A partial walk must CLEAR the side, not truncate it -- codex flagged
	// retaining a partial walk as the most likely silent failure of this port.
	b := qBook(100,
		map[int]float64{50: 60, 49: 40}, map[int]float64{50: 30, 49: 10},
		[]int{50, 49}, []int{50, 49})
	if got := b.Qualifies(); got != 0 {
		t.Errorf("Qualifies() = %d, want 0 (no side never reaches target)", got)
	}
}

func TestP9_QualifiesWhenBothSidesReachTarget(t *testing.T) {
	b := qBook(100,
		map[int]float64{50: 60, 49: 40}, map[int]float64{50: 70, 49: 40},
		[]int{50, 49}, []int{50, 49})
	if got := b.Qualifies(); got != 1 {
		t.Errorf("Qualifies() = %d, want 1", got)
	}
}

func TestP9_QualifiesExactTargetCounts(t *testing.T) {
	// Accumulation stops at >= target, so hitting it exactly must qualify.
	b := qBook(100,
		map[int]float64{50: 100}, map[int]float64{50: 100},
		[]int{50}, []int{50})
	if got := b.Qualifies(); got != 1 {
		t.Errorf("Qualifies() = %d, want 1 (exact target is sufficient)", got)
	}
}

func TestP9_QualifiesZeroTargetAndMaxPriceAndEmptySide(t *testing.T) {
	full := map[int]float64{50: 999}
	order := []int{50}

	if got := qBook(0, full, full, order, order).Qualifies(); got != 0 {
		t.Errorf("zero target: got %d, want 0", got)
	}
	if got := qBook(100, map[int]float64{100: 999}, full, []int{100}, order).Qualifies(); got != 0 {
		t.Errorf("best price at 100: got %d, want 0", got)
	}
	if got := qBook(100, map[int]float64{}, full, nil, order).Qualifies(); got != 0 {
		t.Errorf("empty yes side: got %d, want 0", got)
	}
}

// --- P10: the delete epsilon ---------------------------------------------

func TestP10_DeltaEpsilonIsStrictlyGreaterThan(t *testing.T) {
	b := NewBook(0)
	b.ApplyDelta("yes", 50, 1e-9) // exactly the epsilon: NOT > 1e-9, so deleted
	if _, ok := b.yes.Get(50); ok {
		t.Error("size of exactly 1e-9 was kept; the comparison must be strictly >")
	}
	b.ApplyDelta("yes", 51, 2e-9)
	if _, ok := b.yes.Get(51); !ok {
		t.Error("size above the epsilon was dropped")
	}
	b.ApplyDelta("yes", 51, -3e-9) // driven negative: delete
	if _, ok := b.yes.Get(51); ok {
		t.Error("negative residue was kept")
	}
}

func TestP10_NonYesSideSelectsNo(t *testing.T) {
	b := NewBook(0)
	b.ApplyDelta("no", 40, 5)
	b.ApplyDelta("NO", 41, 5) // anything that is not exactly "yes"
	if b.no.Len() != 2 || b.yes.Len() != 0 {
		t.Errorf("side routing wrong: yes=%d no=%d, want 0/2", b.yes.Len(), b.no.Len())
	}
}

// --- P11: checkpoint eviction --------------------------------------------

func TestP11_HistoryNeverFullyEmptied(t *testing.T) {
	b := NewBook(0)
	b.Checkpoint(0.0)
	b.Checkpoint(100.0) // 0.0 is far outside the 5s window

	// The len > 1 guard stops eviction at one entry, so a market that has gone
	// quiet still has a book to attribute a trade against.
	if b.historyLen != 1 {
		t.Fatalf("history length = %d, want 1", b.historyLen)
	}
	if b.historyAt(0).ts != 100.0 {
		t.Errorf("retained ts = %v, want the newest (100)", b.historyAt(0).ts)
	}
}

func TestP11_HistoryCappedAtMax(t *testing.T) {
	b := NewBook(0)
	for i := 0; i < HistoryMax+10; i++ {
		b.Checkpoint(float64(i) * 0.001) // close together: the cutoff never fires
	}
	if b.historyLen != HistoryMax {
		t.Errorf("history length = %d, want %d", b.historyLen, HistoryMax)
	}
}

// --- P22: snapshot parsing ------------------------------------------------

func TestP22_SnapshotDuplicateKeepsFirstPositionLastValue(t *testing.T) {
	b := NewBook(0)
	err := b.ApplySnapshot([][2]string{
		{"0.5000", "10.00"},
		{"0.6000", "5.00"},
		{"0.5000", "20.00"}, // duplicate: keeps position 0, takes value 20
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := b.yes.keys; len(got) != 2 || got[0] != 50 || got[1] != 60 {
		t.Fatalf("order = %v, want [50 60]", got)
	}
	if v, _ := b.yes.Get(50); v != 20 {
		t.Errorf("value at 50 = %v, want 20 (later assignment wins)", v)
	}
}

func TestP22_SnapshotDropsNonPositiveSizes(t *testing.T) {
	b := NewBook(0)
	if err := b.ApplySnapshot([][2]string{
		{"0.5000", "10.00"},
		{"0.6000", "0.00"},
		{"0.7000", "-1.00"},
	}, nil); err != nil {
		t.Fatal(err)
	}
	if b.yes.Len() != 1 {
		t.Errorf("kept %d levels, want 1 (the s > 0 filter is strict)", b.yes.Len())
	}
}

func TestP22_SnapshotIsAtomicOnParseFailure(t *testing.T) {
	b := NewBook(0)
	b.yes.Set(99, 1)
	err := b.ApplySnapshot(
		[][2]string{{"0.5000", "10.00"}},
		[][2]string{{"not-a-number", "1.00"}},
	)
	if err == nil {
		t.Fatal("expected a parse error")
	}
	// Python builds both level lists before assigning either, so a malformed
	// level leaves the book untouched rather than half-replaced.
	if v, ok := b.yes.Get(99); !ok || v != 1 {
		t.Error("book was mutated despite a parse failure on the other side")
	}
}

// --- Mid ------------------------------------------------------------------

func TestMidIsNilWhenEitherSideEmpty(t *testing.T) {
	b := NewBook(0)
	b.yes.Set(48, 10)
	if b.Mid() != nil {
		t.Error("mid must be nil when the no side is empty")
	}
	b.no.Set(50, 10)
	m := b.Mid()
	if m == nil || *m != 49.0 {
		t.Errorf("mid = %v, want 49 ((48 + (100-50)) / 2)", m)
	}
}

// --- P29: a book returned by StateBefore must stay stable -------------------
//
// rig.py:178 appends a NEW dict pair on every checkpoint and never mutates a
// stored one, so the dicts state_before hands back remain valid for as long as
// the caller holds them.
//
// No tape can exercise this. recordTrade consumes StateBefore's result inside
// the same Handle call with no interleaved Checkpoint, so an implementation
// that recycles checkpoint storage passes the differential gate byte-identically
// while returning a book that mutates under its caller. Found by reading a diff
// during the §10 tournament, after the gate had already gone green on it.
func TestP29_StateBeforeResultSurvivesLaterCheckpoints(t *testing.T) {
	b := NewBook(0)
	mark(b, 11)
	b.Checkpoint(1.0)

	yes, no, _ := b.StateBefore(2.0)
	if got := idOf(yes); got != 11 {
		t.Fatalf("StateBefore returned book %d, want 11", got)
	}

	// Enough checkpoints to wrap any bounded history and reuse every slot.
	for i := 0; i < HistoryMax+5; i++ {
		mark(b, 100+i)
		b.Checkpoint(float64(i) * 0.0001)
	}

	if got := idOf(yes); got != 11 {
		t.Errorf("the yes book returned by StateBefore mutated under the caller: "+
			"%d, want 11. Checkpoint storage must not be recycled while a caller "+
			"may still hold a reference (P29).", got)
	}
	if got := idOf(no); got != -1 {
		t.Errorf("the no book returned by StateBefore mutated under the caller: %d", got)
	}
}

// --- P9: the qualifying walk runs HIGHEST price first -----------------------
//
// This test exists because gate 7 mutation m29 — sorting the walk ascending
// instead of descending — SURVIVED every gate, including a byte-identical
// differential replay over 12,726 reference rows.
//
// The reason it survived is worth stating, because it is not an accident of
// this tape. Sizes are strictly positive (P10's >1e-9 delete and P22's s>0
// filter both guarantee it), so the running total is monotonic and `reached` is
// true iff the WHOLE side sums to Target. The order cannot change that boolean
// — except through float rounding, when the total lands within an ulp of
// Target. Real books never come that close, so no realistic tape discriminates
// the two directions.
//
// The case below does. Walking highest-first, 1e16 absorbs each 1.0 and the
// total stays at 1e16; walking lowest-first, the two 1.0s pair into 2.0 before
// meeting 1e16 and the total reaches 1e16+2. Verified against CPython:
//
//	descending -> (0, 1e+16)
//	ascending  -> (1, 1.0000000000000002e+16)
//
// P24 does NOT apply here: rig.py:245 accumulates with a plain `total += d[p]`,
// not sum(), so this is naive addition and therefore order-dependent.
func TestP9_QualifyingWalkIsHighestPriceFirst(t *testing.T) {
	// Target is computed at runtime, never as a Go constant: constant folding
	// happens at arbitrary precision and would produce a different value. P23.
	one := 1.0
	big := 1e16
	two := 2.0
	// NOT big+one+one: that accumulates left to right at runtime, 1e16+1.0
	// rounds back to 1e16, and the target would come out as 1e16 — which the
	// descending walk reaches immediately, making the case vacuous. Avoiding
	// P23's constant folding is not enough; the runtime order matters too.
	target := big + two

	b := NewBook(target)
	// Inserted in an order that is neither ascending nor descending, so the
	// walk's sort is what decides, not the insertion order.
	b.yes.Set(40, one)
	b.yes.Set(50, big)
	b.yes.Set(30, one)
	// The no side reaches target trivially, so the yes side decides the result.
	b.no.Set(45, target)

	if got := b.Qualifies(); got != 0 {
		t.Errorf("Qualifies = %d, want 0. The walk must run highest price first: "+
			"1e16 absorbs each 1.0 and the total stays at 1e16, short of target. "+
			"Ascending order pairs the 1.0s first and reaches it. (P9)", got)
	}

	// And the discriminating power is real: ascending must give the other answer.
	asc := 0.0
	for _, s := range []float64{one, one, big} {
		asc += s
		if asc >= target {
			asc = -1 // reached
			break
		}
	}
	if asc != -1 {
		t.Fatal("the ascending walk does not reach target either, so this case " +
			"cannot distinguish the two directions")
	}
}
