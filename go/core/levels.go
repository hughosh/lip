package core

import (
	"cmp"
	"math"
	"slices"
	"sort"
)

// Levels holds one side of an orderbook: price in cents -> resting size.
//
// It is an insertion-ordered map, deliberately, because it is replacing a
// CPython dict and one caller depends on the iteration order. `_record_reference`
// (rig.py:319) writes `sum(book.yes.values())`, and Sum reproduces CPython's
// compensated summation over exactly that order.
//
// The three rules being replicated are:
//
//   - inserting a NEW key appends it at the end;
//   - assigning to an EXISTING key leaves its position alone;
//   - deleting a key removes it, and re-inserting it later appends at the end.
//
// See port-spec.md P2. Do not substitute a plain Go map: its range order is
// randomized, which would make the `reference` rows irreproducible against Go's
// own previous run, never mind Python's.
//
// # REPRESENTATION
//
// Two parallel slices, searched linearly, rather than a slice plus two maps.
// A book side carries tens of levels, not thousands, and at that size a linear
// scan over contiguous memory beats hashing — but the reason for the change is
// ALLOCATION, not lookup. Checkpoint clones BOTH sides on every delta frame, so
// the old representation cost two map allocations and their bucket arrays per
// clone, and allocation is what drives the GC pauses sitting in the
// p99.99-to-max band of the handler's latency. See port-spec.md §10.
//
// The observable semantics are unchanged, and are pinned by core/levels_test.go
// through Keys(), plus negative-control mutations m11 and m12.
type Levels struct {
	keys []int
	vals []float64 // vals[i] is the size resting at keys[i]
}

func NewLevels() *Levels { return &Levels{} }

// index returns the position of price, or -1. Linear by design; see the note on
// representation above.
func (l *Levels) index(price int) int {
	for i, p := range l.keys {
		if p == price {
			return i
		}
	}
	return -1
}

func (l *Levels) Get(price int) (float64, bool) {
	if i := l.index(price); i >= 0 {
		return l.vals[i], true
	}
	return 0, false
}

// GetOr mirrors Python's dict.get(price, def).
func (l *Levels) GetOr(price int, def float64) float64 {
	if i := l.index(price); i >= 0 {
		return l.vals[i]
	}
	return def
}

func (l *Levels) Set(price int, size float64) {
	if i := l.index(price); i >= 0 {
		// Existing key: the value is replaced and the POSITION IS UNCHANGED.
		l.vals[i] = size
		return
	}
	l.keys = append(l.keys, price)
	l.vals = append(l.vals, size)
}

func (l *Levels) Delete(price int) {
	i := l.index(price)
	if i < 0 {
		return
	}
	l.keys = append(l.keys[:i], l.keys[i+1:]...)
	l.vals = append(l.vals[:i], l.vals[i+1:]...)
}

func (l *Levels) Len() int { return len(l.keys) }

// Keys returns the prices in CPython dict iteration order.
//
// It exists so that the ordering is defended SEMANTICALLY. Asserting against the
// unexported fields from a test would only produce a COMPILE error if they went
// away, and port-spec.md §2 rejects a compiler catch as evidence about the
// oracle. A method that any substitute implementation would still have to
// provide, and still have to get right, is what actually pins the rule.
//
// Sum deliberately does not call this: it iterates the backing slice directly,
// because this allocates and Sum is on the hot path.
func (l *Levels) Keys() []int {
	out := make([]int, len(l.keys))
	copy(out, l.keys)
	return out
}

// Max returns the highest price and its size, mirroring Book._best
// (rig.py:207-212): an empty side yields (0, 0, false), which the caller must
// map to Python's None rather than to a price of zero.
func (l *Levels) Max() (int, float64, bool) {
	if len(l.keys) == 0 {
		return 0, 0, false
	}
	best := 0
	for i := 1; i < len(l.keys); i++ {
		if l.keys[i] > l.keys[best] {
			best = i
		}
	}
	return l.keys[best], l.vals[best], true
}

// Sum reproduces CPython's builtin sum() over the values, in insertion order.
//
// TWO things have to match, not one. The order is why Levels is ordered (P2),
// and the ALGORITHM is Neumaier compensated summation (P24) — CPython's sum()
// has a float fast path that carries a running compensation term rather than
// adding naively. On realistic two-decimal book sizes the two disagree often,
// by one ULP, which is exactly the discrepancy this replaced.
//
// Naive accumulation here is wrong even with perfect ordering. Do not
// "simplify" this loop.
func (l *Levels) Sum() float64 {
	f, c := 0.0, 0.0
	for _, x := range l.vals {
		t := f + x
		if math.Abs(f) >= math.Abs(x) {
			c += (f - t) + x
		} else {
			c += (x - t) + f
		}
		f = t
	}
	return f + c
}

// level is one (price, size) pair, kept together so the qualifying walk can
// sort once and read the size directly instead of looking it up per price.
type level struct {
	price int
	size  float64
}

// descInto fills buf with every level, highest price first, reusing buf's
// capacity. It is the allocation-free path used by Book.Qualifies on every
// delta frame.
//
// Prices are distinct, so the descending order is unique and the walk's
// accumulation order is fully determined — which matters, because P9's running
// total is NAIVE addition and therefore order-dependent, unlike Sum.
func (l *Levels) descInto(buf []level) []level {
	buf = buf[:0]
	for i, p := range l.keys {
		buf = append(buf, level{price: p, size: l.vals[i]})
	}
	slices.SortFunc(buf, func(a, b level) int { return cmp.Compare(b.price, a.price) })
	return buf
}

// SortedDesc returns prices highest first. Sorting by key makes this
// order-independent, unlike Sum.
func (l *Levels) SortedDesc() []int {
	out := make([]int, len(l.keys))
	copy(out, l.keys)
	sort.Sort(sort.Reverse(sort.IntSlice(out)))
	return out
}

// cloneInto copies l while retaining dst's backing arrays. Ring checkpoints
// use it so steady-state checkpointing allocates only when a side outgrows a slot.
func (l *Levels) cloneInto(dst *Levels) {
	dst.keys = append(dst.keys[:0], l.keys...)
	dst.vals = append(dst.vals[:0], l.vals...)
}

// Clone returns a detached copy suitable for handing to a caller.
func (l *Levels) Clone() *Levels {
	c := NewLevels()
	l.cloneInto(c)
	return c
}

// confidence: high
