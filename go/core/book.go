package core

// Book is the live orderbook for one market, plus a short history so a trade
// can be attributed to the state that preceded it. Direct port of rig.py:148-250.

// How much book history to retain per market. Trades and their deltas arrive
// within milliseconds of each other; 5s is a wide margin. Never fully emptied,
// so a quiet market still has a book to attribute a trade against.
const (
	HistorySeconds = 5.0
	HistoryMax     = 64
)

type checkpoint struct {
	ts      float64
	yes, no Levels
}

type Book struct {
	yes, no      *Levels
	history      [HistoryMax]checkpoint
	historyStart int
	historyLen   int

	Target float64

	// Scratch for the qualifying walk, reused across calls so Qualifies does not
	// allocate. Never escapes; holds no state between calls.
	qual []level

	// Last-written reference row, for change detection in RecordReference.
	// Pointers because Python starts these at None, which is distinct from a
	// legitimate reference price of zero.
	RefYes, RefNo, Gate *int
}

func NewBook(target float64) *Book {
	return &Book{yes: NewLevels(), no: NewLevels(), Target: target}
}

// Yes and No expose the live sides for the reference-row depth sums.
func (b *Book) Yes() *Levels { return b.yes }
func (b *Book) No() *Levels  { return b.no }

// ApplySnapshot replaces both sides. Prices and sizes arrive as the raw wire
// strings so that the conversion — and any failure of it — stays inside the
// differentially-tested surface.
//
// Both sides are parsed completely before either is assigned, matching
// rig.py:448-451 where the `levels()` comprehensions run to completion before
// apply_snapshot is called: a malformed level aborts the whole snapshot rather
// than leaving the book half-replaced.
func (b *Book) ApplySnapshot(yes, no [][2]string) error {
	py, err := parseLevels(yes)
	if err != nil {
		return err
	}
	pn, err := parseLevels(no)
	if err != nil {
		return err
	}
	b.yes, b.no = py, pn
	return nil
}

func parseLevels(raw [][2]string) (*Levels, error) {
	out := NewLevels()
	for _, ps := range raw {
		price, err := ParsePriceCents(ps[0])
		if err != nil {
			return nil, err
		}
		size, err := ParseSize(ps[1])
		if err != nil {
			return nil, err
		}
		// Strictly greater than zero, per the `if s > 0` filter at rig.py:165.
		// A duplicated price keeps its first position and takes the later
		// size, which is CPython dict-assignment behaviour (port-spec P22).
		if size > 0 {
			out.Set(price, size)
		}
	}
	return out, nil
}

// ApplyDelta adds `delta` to one level. rig.py:168-174.
//
// The 1e-9 threshold is strict: a level whose remaining size lands at or below
// it is deleted rather than kept at a residue. Negative results delete too.
// Anything other than the exact string "yes" selects the no side, matching
// Python's `self.yes if side == "yes" else self.no`.
func (b *Book) ApplyDelta(side string, price int, delta float64) {
	d := b.no
	if side == "yes" {
		d = b.yes
	}
	new := d.GetOr(price, 0.0) + delta
	if new > 1e-9 {
		d.Set(price, new)
	} else {
		d.Delete(price)
	}
}

func (b *Book) historyAt(i int) *checkpoint {
	return &b.history[(b.historyStart+i)%HistoryMax]
}

// Checkpoint records the current state so a later trade can be attributed to
// it. rig.py:176-183.
//
// The eviction condition is compound and the `len > 1` guard is load-bearing:
// history is never fully emptied, so a market that has gone quiet for longer
// than HistorySeconds still has a book to attribute against.
func (b *Book) Checkpoint(ts float64) {
	var h *checkpoint
	if b.historyLen < HistoryMax {
		h = b.historyAt(b.historyLen)
		b.historyLen++
	} else {
		h = b.historyAt(0)
		b.historyStart = (b.historyStart + 1) % HistoryMax
	}

	h.ts = ts
	b.yes.cloneInto(&h.yes)
	b.no.cloneInto(&h.no)

	cutoff := ts - HistorySeconds
	for b.historyLen > 1 && b.historyAt(0).ts < cutoff {
		b.historyStart = (b.historyStart + 1) % HistoryMax
		b.historyLen--
	}
}

func (b *Book) ClearHistory() { b.historyStart, b.historyLen = 0, 0 }

// StateBefore returns the book as of just before `ts`, and the age in seconds
// of the state it chose. rig.py:185-203.
//
// Three behaviours here look like defects and are not:
//
//   - The scan does not break early. Checkpoints are appended in arrival order,
//     and one out-of-order entry would otherwise hide every later one.
//   - The comparison is `>=`, not `>`, so when several checkpoints share a
//     timestamp the LAST of them wins. Ties are routine: a trade and the deltas
//     it caused usually land in the same millisecond.
//   - When every checkpoint is newer than the trade — which happens whenever a
//     trade's deltas are processed before the trade message itself — it falls
//     back to the OLDEST retained state and returns a NEGATIVE lag.
//
// A nil lag means the history was empty and the live book was used; that is
// distinct from a lag of zero and is written to SQL as NULL.
//
// Results are detached from the recyclable ring storage so they remain stable
// across later checkpoints, even after the selected slot wraps (P29).
func (b *Book) StateBefore(ts float64) (yes, no *Levels, lag *float64) {
	var chosen *checkpoint
	for i := 0; i < b.historyLen; i++ {
		h := b.historyAt(i)
		if h.ts < ts && (chosen == nil || h.ts >= chosen.ts) {
			chosen = h
		}
	}
	if chosen == nil {
		if b.historyLen == 0 {
			return b.yes.Clone(), b.no.Clone(), nil
		}
		h := b.historyAt(0)
		l := ts - h.ts
		return h.yes.Clone(), h.no.Clone(), &l
	}
	l := ts - chosen.ts
	return chosen.yes.Clone(), chosen.no.Clone(), &l
}

func (b *Book) BestYes() (int, float64, bool) { return b.yes.Max() }
func (b *Book) BestNo() (int, float64, bool)  { return b.no.Max() }

// Mid is the yes-denominated mid in cents. A resting no bid at n is a yes offer
// at 100-n, so the yes ask is 100 - best_no. rig.py:220-230.
func (b *Book) Mid() *float64 { return MidOf(b.yes, b.no) }

// MidOf is rig.py:253-256. The addition is integer arithmetic before the float
// division, as in Python.
func MidOf(yes, no *Levels) *float64 {
	by, _, okY := yes.Max()
	bn, _, okN := no.Max()
	if !okY || !okN {
		return nil
	}
	m := float64(by+(100-bn)) / 2.0
	return &m
}

// Qualifies reports 1 if BOTH sides reach Target Size cumulatively down the
// book. rig.py:232-250.
//
// It mirrors score.qualify(): the walk is what the LIP rule gates on, and a
// side that never reaches Target Size has its qualifying set CLEARED, not
// truncated. Python spells that with a for/else — the `else` fires when the
// loop finishes without breaking — and silently retaining a partial walk here
// is the single most likely way for this port to go quietly wrong.
//
// Four exits return 0: no target, an empty side, a side whose best price has
// reached the maximum, and a side that never accumulates to target.
func (b *Book) Qualifies() int {
	if b.Target == 0 {
		return 0
	}
	// Iterated explicitly rather than over a []*Levels literal: the literal
	// escapes and allocates on every call, and this runs once per delta frame.
	for side := 0; side < 2; side++ {
		d := b.yes
		if side == 1 {
			d = b.no
		}
		mx, _, ok := d.Max()
		if !ok || mx >= 100 {
			return 0
		}
		// Reused scratch. The walk needs price order AND the size at each
		// price; sorting the pairs together makes the size read O(1) instead of
		// a scan per price, which was quadratic in the level count.
		b.qual = d.descInto(b.qual)
		total := 0.0
		reached := false
		for i := range b.qual {
			// NAIVE addition, deliberately: rig.py:245 accumulates with
			// `total += d[p]` and does NOT use sum()'s compensation. Do not
			// "improve" this to match Levels.Sum — see P9 and P24.
			total += b.qual[i].size
			if total >= b.Target {
				reached = true
				break
			}
		}
		if !reached {
			return 0
		}
	}
	return 1
}

// confidence: high
