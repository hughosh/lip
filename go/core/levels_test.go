package core

import (
	"fmt"
	"testing"
)

// P2, through the PUBLIC accessor. book_test.go already pins the same rules
// against the private l.keys, and that is not redundant with this file — it is
// the reason this file exists.
//
// A mutation that deletes the ordered bookkeeping outright, or swaps Levels for
// a plain map[int]float64, breaks a test that reads l.keys by failing to
// COMPILE. §2 is explicit that a mutation caught that way tests the Go compiler
// rather than the oracle; two of the original ten did exactly that and had to be
// rewritten. Reading the order back through a method that any substitute
// implementation would still have to provide — and still have to get right —
// turns that into a semantic catch.
//
// What this does NOT do is make gate 7's m02 catchable. m02 changes only which
// order Sum CONSUMES — it reverses the traversal — leaving the bookkeeping
// intact, and it is inert for the reason recorded in port-spec.md P2:
// compensated summation is order-independent across 700k randomised trials, and
// Sum is order's only consumer. That inert label is correct and stays.

func keysOf(l *Levels) string { return fmt.Sprint(l.Keys()) }

// The three CPython dict rules, each pinned separately so a failure names which
// one broke.

func TestP2_NewKeyAppendsAtTheEnd(t *testing.T) {
	l := NewLevels()
	// Descending prices, so any implementation that sorts disagrees immediately.
	l.Set(50, 1)
	l.Set(30, 2)
	l.Set(40, 3)
	if got, want := keysOf(l), "[50 30 40]"; got != want {
		t.Errorf("keys = %s, want %s: a new key appends, it does not sort", got, want)
	}
}

func TestP2_OverwriteKeepsPosition(t *testing.T) {
	l := NewLevels()
	l.Set(50, 1)
	l.Set(30, 2)
	l.Set(40, 3)
	l.Set(50, 99) // existing key: position unchanged, value replaced
	if got, want := keysOf(l), "[50 30 40]"; got != want {
		t.Errorf("keys = %s, want %s: assigning to an existing key must not move it",
			got, want)
	}
	if v, _ := l.Get(50); v != 99 {
		t.Errorf("value at 50 = %v, want 99", v)
	}
}

func TestP2_DeleteThenReinsertAppendsAtTheEnd(t *testing.T) {
	l := NewLevels()
	l.Set(50, 1)
	l.Set(30, 2)
	l.Set(40, 3)
	l.Delete(50)
	if got, want := keysOf(l), "[30 40]"; got != want {
		t.Fatalf("keys after delete = %s, want %s", got, want)
	}
	l.Set(50, 7) // reinserted: now the NEWEST key, not the oldest
	if got, want := keysOf(l), "[30 40 50]"; got != want {
		t.Errorf("keys = %s, want %s: a deleted key that comes back appends at the "+
			"end; it does not return to its original slot", got, want)
	}
}

// Deleting from the middle must shift the tail of BOTH slices, keeping them in
// step. Swap-remove is the idiomatic O(1) alternative and it destroys the order;
// dropping one slice's element desynchronises them. Mutations m12 and m24.
func TestP2_DeleteFromTheMiddleKeepsTheRestOrdered(t *testing.T) {
	l := NewLevels()
	for _, p := range []int{50, 40, 30, 20, 10} {
		l.Set(p, float64(p))
	}
	l.Delete(30)
	if got, want := keysOf(l), "[50 40 20 10]"; got != want {
		t.Fatalf("keys = %s, want %s", got, want)
	}
	l.Delete(50)
	l.Delete(10)
	if got, want := keysOf(l), "[40 20]"; got != want {
		t.Errorf("keys = %s, want %s: the index map was not reindexed after the "+
			"first middle delete", got, want)
	}
	for _, p := range []int{40, 20} {
		if v, ok := l.Get(p); !ok || v != float64(p) {
			t.Errorf("value at %d = %v (present %v), want %v", p, v, ok, p)
		}
	}
}

// Clone must carry the order across, not just the contents. Checkpoint clones
// every side on every frame, so an order-losing Clone would silently unpick P2
// everywhere at once.
func TestP2_CloneCarriesInsertionOrder(t *testing.T) {
	l := NewLevels()
	l.Set(50, 1)
	l.Set(30, 2)
	l.Delete(50)
	l.Set(50, 3)
	l.Set(40, 4)

	c := l.Clone()
	if got, want := keysOf(c), keysOf(l); got != want {
		t.Errorf("clone keys = %s, want %s", got, want)
	}
	// And the clone must be independent.
	c.Set(99, 5)
	if len(l.Keys()) == len(c.Keys()) {
		t.Error("mutating the clone changed the original")
	}
}

// ApplySnapshot's duplicate-price rule (P22): the later size wins, the FIRST
// position is kept. That is CPython dict-assignment semantics and it interacts
// with P2 directly.
func TestP2_SnapshotDuplicatePriceKeepsFirstPositionAndLastSize(t *testing.T) {
	b := NewBook(0)
	err := b.ApplySnapshot([][2]string{
		{"0.5000", "10.00"},
		{"0.3000", "20.00"},
		{"0.5000", "99.00"}, // duplicate of the first price
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := keysOf(b.Yes()), "[50 30]"; got != want {
		t.Errorf("keys = %s, want %s: a duplicated price keeps its first position",
			got, want)
	}
	if v, _ := b.Yes().Get(50); v != 99.0 {
		t.Errorf("size at 50 = %v, want 99: the later size wins", v)
	}
}

// The ordering is only worth defending if it is actually different from the
// obvious alternatives. If sorted order and insertion order agreed on every case
// above, these tests would pass against a sorted implementation and prove
// nothing — the same discriminating-power guard the rounding and summation
// fixtures carry.
func TestP2_FixtureDiscriminatesSortedOrder(t *testing.T) {
	l := NewLevels()
	l.Set(50, 1)
	l.Set(30, 2)
	l.Set(40, 3)
	k := l.Keys()
	ascending, descending := true, true
	for i := 1; i < len(k); i++ {
		if k[i] < k[i-1] {
			ascending = false
		}
		if k[i] > k[i-1] {
			descending = false
		}
	}
	if ascending || descending {
		t.Fatal("the insertion order used in these tests is also a sorted order, so " +
			"they cannot distinguish an insertion-ordered map from a sorted one")
	}
}
