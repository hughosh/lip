package main

import (
	"context"
	"sync/atomic"
	"time"

	"lip/harness/risk"
)

// monitor is the I2 goroutine of harness-spec.md §8.3.
//
// I2 -- the monitoring path is structurally unreachable from the quoting path's
// control flow. This struct holds a *sync/atomic.Pointer to a snapshot and
// nothing else. It has no reference to the quote engine, the order queue, the
// REST client or the owner goroutine, and there is no field it could be given
// one through. It cannot be terminated by any decision the quote engine makes,
// because there is no channel by which such a decision could reach it.
//
// I2 is stated structurally, not behaviourally, on purpose. "Remember to keep
// sampling after a halt" is the kind of rule that gets unlearned. "The sampler
// is not in that function and has no way to be" is not.
type monitor struct {
	src   *atomic.Pointer[risk.Snapshot]
	state risk.MonitorState
	a5    *risk.A5Tracker

	// stallAfter is owner_stall_s (§16, default 3s).
	stallAfter time.Duration
	// emit receives every sample and anomaly. It is a send to the hstore-writer
	// goroutine's buffered channel (H-STORE-2), never a write. A monitor that
	// could block on a disk is a monitor that a disk can stop.
	emit func(time.Duration, risk.StepResult)
	// now is injected so the simulator can step this deterministically. In
	// production it is a monotonic reading.
	now func() time.Duration
}

func newMonitor(src *atomic.Pointer[risk.Snapshot], stallAfter time.Duration,
	now func() time.Duration, emit func(time.Duration, risk.StepResult)) *monitor {
	return &monitor{
		src: src, a5: risk.NewA5Tracker(), stallAfter: stallAfter,
		now: now, emit: emit,
	}
}

// run is the monitor loop. It runs at 1 Hz, independent of everything.
//
// THERE IS DELIBERATELY NO CONDITION IN THIS LOOP.
//
// It does not consult the global state. It does not consult any market state.
// It does not check whether quoting is active, whether the harness is winding
// down, whether inventory is open, or whether the last tick succeeded. It ticks
// and it samples, and the only thing that ends it is the process ending.
//
// That absence is the fix for the defect this entire harness exists to not
// have. probebot.py's halt() cancelled resting orders, never flattened, and the
// snapshot loop `break`ed in the same control-loop iteration -- 2 snapshots
// over the following 6.14 hours while 61 contracts sat naked and directional,
// in a market that fell 57 -> 49 unobserved and then settled YES at $1.00.
//
// Mutation M1 adds a condition here. It must fail A5.
func (m *monitor) run(ctx context.Context, tick <-chan time.Time) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick:
			now := m.now()
			snap := m.src.Load()
			res := m.state.Step(now, snap, m.stallAfter)
			m.a5.Observe(now, res)
			if m.emit != nil {
				m.emit(now, res)
			}
		}
	}
}

// checkA5 asserts A5 against what the monitor has actually produced.
func (m *monitor) checkA5(now time.Duration, snap *risk.Snapshot) []risk.Violation {
	return m.a5.Check(now, snap, m.stallAfter)
}

// confidence: high
