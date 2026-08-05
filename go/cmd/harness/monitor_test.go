package main

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"lip/harness/num"
	"lip/harness/quote"
	"lip/harness/risk"
)

// This file is the regression test for the $9.32 loss.
//
// M1 is written FIRST, before the harness works, and it must fail against the
// mutation and pass against the real code. If M1 ever stops failing against its
// mutation, the verification is broken regardless of what else is green
// (harness-spec.md §17 V5).

const stallAfter = 3 * time.Second

// driver runs a monitor against a hand-stepped clock and a hand-driven tick
// channel, so nothing here depends on wall time.
type driver struct {
	mu    sync.Mutex
	now   time.Duration
	src   atomic.Pointer[risk.Snapshot]
	mon   *monitor
	tick  chan time.Time
	seq   uint64
	res   []risk.StepResult
	done  chan struct{}
	ctx   context.Context
	stop  context.CancelFunc
	ticks int
}

func newDriver(t *testing.T) *driver {
	t.Helper()
	d := &driver{tick: make(chan time.Time), done: make(chan struct{})}
	d.ctx, d.stop = context.WithCancel(context.Background())
	d.mon = newMonitor(&d.src, stallAfter,
		func() time.Duration { d.mu.Lock(); defer d.mu.Unlock(); return d.now },
		func(_ time.Duration, r risk.StepResult) {
			d.mu.Lock()
			defer d.mu.Unlock()
			d.res = append(d.res, r)
		})
	go func() { d.mon.run(d.ctx, d.tick); close(d.done) }()
	t.Cleanup(func() { d.stop(); <-d.done })
	return d
}

// publish writes a new snapshot with an ADVANCING seq, as the owner would.
func (d *driver) publish(g quote.GlobalState, tickers ...string) {
	d.mu.Lock()
	d.seq++
	seq := d.seq
	now := d.now
	d.mu.Unlock()
	ms := make([]risk.MarketSnap, 0, len(tickers))
	for _, tk := range tickers {
		ms = append(ms, risk.MarketSnap{Ticker: tk, Selected: true,
			State: quote.Reducing, Q: num.QtyFromFloat(61)})
	}
	d.src.Store(&risk.Snapshot{Seq: seq, PubMono: now, Global: g, Markets: ms})
}

// step advances the injected clock and delivers exactly one tick, waiting for
// the monitor to consume it so the test never races.
func (d *driver) step(by time.Duration) {
	d.mu.Lock()
	d.now += by
	d.mu.Unlock()
	select {
	case d.tick <- time.Time{}:
		d.ticks++
	case <-d.done:
		return // the loop has exited -- that is itself the M1 observable
	}
	// The send only returns once run() has received it; give the tick body a
	// moment to finish before the test inspects state.
	for i := 0; i < 1000; i++ {
		d.mu.Lock()
		n := len(d.res)
		d.mu.Unlock()
		if n >= d.ticks {
			return
		}
		time.Sleep(time.Millisecond)
	}
}

func (d *driver) snapshot() *risk.Snapshot { return d.src.Load() }

func (d *driver) curNow() time.Duration {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.now
}

// TestM1_MonitorKeepsSamplingAfterGlobalStateLeavesRunning is the M1 gate.
//
// probebot.py's defect, exactly: the global state leaves RUNNING and the
// snapshot loop stops. Here the harness enters WINDING_DOWN -- which is the
// state it is SUPPOSED to spend hours in, managing an exit -- and the monitor
// must keep producing fresh samples for every selected market throughout.
//
// A5's clause "in every global state" is what this asserts. Against the M1
// mutation (a `break`/`return` in run() when the global state is not RUNNING)
// the loop exits, samples stop, and A5 reports a violation for every selected
// market within owner_stall_s.
func TestM1_MonitorKeepsSamplingAfterGlobalStateLeavesRunning(t *testing.T) {
	d := newDriver(t)

	// Healthy: RUNNING, with inventory open.
	d.publish(quote.Running, "KXTEST-A", "KXTEST-B")
	d.step(time.Second)
	if v := d.mon.checkA5(d.curNow(), d.snapshot()); len(v) != 0 {
		t.Fatalf("A5 violated while RUNNING: %v", v)
	}

	// The event that killed the probe: a global stop, with inventory still
	// open. WINDING_DOWN keeps the reducing quote alive and the process alive,
	// so the monitor MUST keep sampling.
	for i := 0; i < 10; i++ {
		d.publish(quote.WindingDown, "KXTEST-A", "KXTEST-B")
		d.step(time.Second)

		select {
		case <-d.done:
			t.Fatal("M1: the monitor loop exited when the global state left " +
				"RUNNING -- this is probebot.py's defect (2 snapshots in " +
				"6.14 hours while 61 contracts sat naked)")
		default:
		}

		if v := d.mon.checkA5(d.curNow(), d.snapshot()); len(v) != 0 {
			t.Fatalf("M1: A5 violated %v after the global state left RUNNING "+
				"(tick %d): %v", d.curNow(), i, v)
		}
	}

	total, stale := d.mon.state.Counts()
	if want := uint64(2 * 11); total != want {
		t.Fatalf("M1: produced %d samples across 11 ticks of 2 markets, "+
			"want %d -- sampling stopped or thinned out", total, want)
	}
	if stale != 0 {
		t.Fatalf("M1: %d samples were stale while the owner was publishing "+
			"normally", stale)
	}
}

// TestM14_FrozenOwnerPublicationIsReportedStale is the M14 gate.
//
// The second mutation the adversarial pass produced, and the one that defeats
// I2 on I2's own terms: freeze owner snapshot publication but leave the monitor
// and ping goroutines alive. The monitor is structurally unstoppable and stays
// alive -- it just describes a world that stopped moving.
//
// A5 as originally written checked that a row was WRITTEN, not that the thing
// it described had MOVED, so this passed at every tick and the heartbeat kept
// reporting q = 0 while real inventory grew.
func TestM14_FrozenOwnerPublicationIsReportedStale(t *testing.T) {
	d := newDriver(t)

	d.publish(quote.Running, "KXTEST-A")
	d.step(time.Second)
	if d.mon.state.Stalled() {
		t.Fatal("stalled while the owner was publishing normally")
	}

	// The owner deadlocks here. It publishes nothing further. Note the monitor
	// keeps ticking -- that is I2 working exactly as designed, and is precisely
	// why it is not sufficient on its own.
	var sawStallPing bool
	for i := 0; i < 6; i++ {
		d.step(time.Second)
		d.mu.Lock()
		last := d.res[len(d.res)-1]
		d.mu.Unlock()
		for _, a := range last.Anomalies {
			if a.Class == "OWNER_STALLED" && a.Sev == risk.SEV1 {
				sawStallPing = true
			}
		}
	}

	if !sawStallPing {
		t.Fatal("M14: no SEV1 OWNER_STALLED was emitted while the owner's " +
			"snapshot sequence was frozen")
	}
	if !d.mon.state.Stalled() {
		t.Fatal("M14: the monitor does not consider a frozen source stalled")
	}

	// The rows must be produced (silence is worse) but marked stale, and must
	// not be integrated into uptime.
	d.mu.Lock()
	last := d.res[len(d.res)-1]
	d.mu.Unlock()
	if !last.Stale || last.IntegrateUptime {
		t.Fatalf("M14: stale tick was presented as current "+
			"(stale=%v integrate=%v)", last.Stale, last.IntegrateUptime)
	}
	if len(last.Samples) != 1 || !last.Samples[0].Stale {
		t.Fatalf("M14: sample not marked stale: %+v", last.Samples)
	}

	// And A5 must fail: fresh rows about a frozen world do not satisfy it.
	if v := d.mon.checkA5(d.curNow(), d.snapshot()); len(v) == 0 {
		t.Fatal("M14: A5 passed against a frozen owner -- it is asserting row " +
			"freshness, not source advancement")
	}

	// Recovery: once the owner publishes again, the monitor stops lying.
	d.publish(quote.Running, "KXTEST-A")
	d.step(time.Second)
	if d.mon.state.Stalled() {
		t.Fatal("M14: monitor stayed stalled after the source advanced")
	}
	if v := d.mon.checkA5(d.curNow(), d.snapshot()); len(v) != 0 {
		t.Fatalf("M14: A5 still failing after recovery: %v", v)
	}
}

// TestMonitorSamplesInEveryGlobalState pins A5's "in every global state"
// clause across the whole state set, not just the WINDING_DOWN case M1 uses.
func TestMonitorSamplesInEveryGlobalState(t *testing.T) {
	for _, g := range []quote.GlobalState{
		quote.Starting, quote.UnknownRisk, quote.Running,
		quote.WindingDown, quote.Drained,
	} {
		t.Run(g.String(), func(t *testing.T) {
			d := newDriver(t)
			for i := 0; i < 4; i++ {
				d.publish(g, "KXTEST-A")
				d.step(time.Second)
			}
			select {
			case <-d.done:
				t.Fatalf("monitor loop exited in global state %s", g)
			default:
			}
			if v := d.mon.checkA5(d.curNow(), d.snapshot()); len(v) != 0 {
				t.Fatalf("A5 violated in global state %s: %v", g, v)
			}
			if total, _ := d.mon.state.Counts(); total != 4 {
				t.Fatalf("state %s: %d samples, want 4", g, total)
			}
		})
	}
}

// TestA5FailsWhenNoSampleIsEverProduced guards the guard: an A5 that only ever
// looked at recorded samples would pass vacuously against a monitor that never
// ran at all.
func TestA5FailsWhenNoSampleIsEverProduced(t *testing.T) {
	tr := risk.NewA5Tracker()
	snap := &risk.Snapshot{Seq: 1, Global: quote.Running,
		Markets: []risk.MarketSnap{{Ticker: "KXTEST-A", Selected: true}}}

	// Tracking starts, but the monitor produces nothing.
	tr.Observe(0, risk.StepResult{})
	if v := tr.Check(stallAfter+time.Second, snap, stallAfter); len(v) == 0 {
		t.Fatal("A5 passed against a monitor that never produced a sample")
	}
}
