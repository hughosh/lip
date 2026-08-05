package risk

import (
	"fmt"
	"time"
)

// Violation is a breach of one of the §17 V3 invariants. In the simulator a
// violation fails the test; in production it emits SEV1 and forces
// WINDING_DOWN.
type Violation struct {
	ID     string // "A5"
	Ticker string
	Text   string
}

func (v Violation) Error() string {
	if v.Ticker == "" {
		return fmt.Sprintf("%s violated: %s", v.ID, v.Text)
	}
	return fmt.Sprintf("%s violated [%s]: %s", v.ID, v.Ticker, v.Text)
}

// A5Tracker records what the monitor actually produced, so A5 can be asserted
// against observation rather than against intent.
//
// A5 -- the monitor has produced a sample from a source snapshot whose Seq
// ADVANCED within the last owner_stall_s, for every selected market, in every
// global state.
//
// Both halves of that sentence are load-bearing and each closes a hole that an
// earlier version of this invariant left open:
//
//   - "in every global state" is the clause that catches M1, probebot.py's
//     exact defect: a monitor loop that `break`s when the global state leaves
//     RUNNING. The probe recorded 2 snapshots over the following 6.14 hours
//     while 61 contracts sat naked and directional.
//
//   - "from a source snapshot whose Seq advanced" is the clause that catches
//     M14. The earlier A5 checked that a ROW WAS WRITTEN, not that the thing it
//     described had MOVED -- so a deadlocked owner produced fresh rows about a
//     frozen world forever, and A5 passed at every tick.
//
// Persistence success is NOT required (H-STORE-3). F19 says keep trading and
// monitoring through a SQLite failure; an A5 that required a row to reach disk
// would trip on a disk failure and force WINDING_DOWN, doing exactly what F19
// forbids. A5 asserts sample PRODUCTION. The two are separate properties.
type A5Tracker struct {
	// lastFresh is, per ticker, the monotonic time of the most recent sample
	// produced from an ADVANCED source snapshot.
	lastFresh map[string]time.Duration
	started   time.Duration
	haveStart bool
}

func NewA5Tracker() *A5Tracker {
	return &A5Tracker{lastFresh: make(map[string]time.Duration)}
}

// Observe records one monitor tick's output. Stale samples are recorded as
// having happened but do NOT refresh the per-ticker clock -- that is the whole
// distinction between a monitor that cannot fail and one that cannot lie.
func (t *A5Tracker) Observe(now time.Duration, res StepResult) {
	if !t.haveStart {
		t.started, t.haveStart = now, true
	}
	if res.Stale {
		return
	}
	for i := range res.Samples {
		t.lastFresh[res.Samples[i].Ticker] = now
	}
}

// Check asserts A5 over every selected market in the snapshot, in every global
// state. `maxAge` is owner_stall_s.
//
// A market that has never been sampled violates A5 once maxAge has elapsed
// since tracking began -- otherwise a monitor that never ran at all would pass
// for as long as its map stayed empty.
func (t *A5Tracker) Check(now time.Duration, snap *Snapshot,
	maxAge time.Duration) []Violation {

	if snap == nil || !t.haveStart {
		return nil
	}
	if now-t.started < maxAge {
		return nil // grace: the first tick has not had time to happen
	}
	var out []Violation
	for i := range snap.Markets {
		if !snap.Markets[i].Selected {
			continue
		}
		tk := snap.Markets[i].Ticker
		last, ok := t.lastFresh[tk]
		if !ok {
			out = append(out, Violation{ID: "A5", Ticker: tk,
				Text: fmt.Sprintf("no sample from an advancing source has "+
					"ever been produced (global state %s)", snap.Global)})
			continue
		}
		if age := now - last; age > maxAge {
			out = append(out, Violation{ID: "A5", Ticker: tk,
				Text: fmt.Sprintf("last sample from an advancing source was "+
					"%v ago, limit %v (global state %s)",
					age, maxAge, snap.Global)})
		}
	}
	return out
}

// confidence: high
