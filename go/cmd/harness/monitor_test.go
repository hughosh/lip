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

// marketState is one selected market's published identity for a driven tick.
// The owner's markets do not share a state -- H-CLOSE-2 sends one market to
// SETTLING at its own close_time while its peers keep quoting -- so the driver
// has to be able to say so.
type marketState struct {
	ticker string
	state  quote.MarketState
}

// publishStates writes a new snapshot with an ADVANCING seq and caller-chosen
// per-market states, as the owner would. Every market carries q = 61: the
// contracts the probe left naked, and the reason H-CLOSE-2a and V7.10 care
// about the close window at all.
func (d *driver) publishStates(g quote.GlobalState, markets ...marketState) {
	d.mu.Lock()
	d.seq++
	seq := d.seq
	now := d.now
	d.mu.Unlock()
	ms := make([]risk.MarketSnap, 0, len(markets))
	for _, m := range markets {
		ms = append(ms, risk.MarketSnap{Ticker: m.ticker, Selected: true,
			State: m.state, Q: num.QtyFromFloat(61)})
	}
	d.src.Store(&risk.Snapshot{Seq: seq, PubMono: now, Global: g, Markets: ms})
}

// publish writes a new snapshot with an ADVANCING seq, as the owner would, with
// every market REDUCING.
func (d *driver) publish(g quote.GlobalState, tickers ...string) {
	ms := make([]marketState, 0, len(tickers))
	for _, tk := range tickers {
		ms = append(ms, marketState{ticker: tk, state: quote.Reducing})
	}
	d.publishStates(g, ms...)
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

// results copies every StepResult the monitor has emitted so far, so a test can
// assert on what each individual tick produced rather than only on totals.
func (d *driver) results() []risk.StepResult {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]risk.StepResult, len(d.res))
	copy(out, d.res)
	return out
}

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

// TestMonitorSamplesInEveryMarketState pins §8.3's "write one snap row per
// market" across the whole per-market state set, SETTLING included.
//
// TestMonitorSamplesInEveryGlobalState varies the GLOBAL state and hardcodes
// every market as REDUCING, so nothing above it says anything about the other
// five market states. SETTLING is the one that matters: H-CLOSE-2 makes it an
// ordinary reachable close-window state entered at close_time - close_lead, and
// H-CLOSE-2a plus V7.10 require the whole window to be spent there with q != 0
// and the capped reducer alive. M13 -- the probe's defect confined to the close
// window -- lives in exactly that window, and a monitor that stopped watching a
// SETTLING market would be the same defect one layer down: I1 says no stop path
// stops watching, and I2 says nothing in the quoting path can reach this loop.
//
// The assertions are on the emitted samples and on goroutine liveness, not on
// A5 alone. A5's Check is vacuous until owner_stall_s has elapsed since
// tracking began, so a monitor that produced nothing at all would satisfy it
// over a short run -- driveMarketStates refuses to pass unless at least one A5
// check landed outside that grace period.
func TestMonitorSamplesInEveryMarketState(t *testing.T) {
	// 6s of 1 Hz ticks: twice owner_stall_s, so the run outlasts both the A5
	// grace period and the stall window.
	const ticks = 6

	for _, st := range []quote.MarketState{
		quote.Idle, quote.Quoting, quote.Skewed,
		quote.Reducing, quote.Settling, quote.Closed,
	} {
		t.Run(st.String(), func(t *testing.T) {
			driveMarketStates(t, newDriver(t), []marketState{
				{ticker: "KXTEST-A", state: st},
				{ticker: "KXTEST-B", state: st},
			}, ticks)
		})
	}

	// Market state is per market. A monitor that gave up on the one SETTLING
	// market in a snapshot would take that market's peers -- still quoting,
	// still reducing, still holding inventory -- down with it.
	t.Run("SETTLING_alongside_peers", func(t *testing.T) {
		driveMarketStates(t, newDriver(t), []marketState{
			{ticker: "KXTEST-A", state: quote.Quoting},
			{ticker: "KXTEST-B", state: quote.Settling},
			{ticker: "KXTEST-C", state: quote.Reducing},
		}, ticks)
	})

	// H-CLOSE-2 is a TRANSITION, not a starting condition: a market that was
	// quoting goes to SETTLING while the owner keeps publishing. A monitor that
	// gave up on the edge rather than on the state would survive every subtest
	// above, so the edge is driven too.
	t.Run("entry_to_SETTLING", func(t *testing.T) {
		d := newDriver(t)
		driveMarketStates(t, d, []marketState{
			{ticker: "KXTEST-A", state: quote.Quoting},
			{ticker: "KXTEST-B", state: quote.Reducing},
		}, 4)
		driveMarketStates(t, d, []marketState{
			{ticker: "KXTEST-A", state: quote.Settling},
			{ticker: "KXTEST-B", state: quote.Reducing},
		}, ticks)
	})
}

// driveMarketStates publishes `markets` with an advancing seq and steps the
// monitor one second at a time, asserting after EVERY tick that the loop is
// still alive and that the tick produced exactly one fresh sample per selected
// market. It may be called more than once against the same driver, to drive a
// state transition.
func driveMarketStates(t *testing.T, d *driver, markets []marketState, ticks int) {
	t.Helper()

	base := len(d.results())
	baseTotal, baseStale := d.mon.state.Counts()
	// The driver's clock starts at 0 and the first tick lands at 1s, which is
	// when A5 starts tracking; its grace period runs owner_stall_s from there.
	const firstTick = time.Second
	postGrace := 0

	for i := 0; i < ticks; i++ {
		d.publishStates(quote.Running, markets...)
		d.step(time.Second)

		select {
		case <-d.done:
			t.Fatalf("the monitor loop exited at tick %d with markets %v -- "+
				"I1: no stop path stops watching", base+i, markets)
		default:
		}

		got := d.results()
		if len(got) != base+i+1 {
			t.Fatalf("tick %d produced %d results in total, want %d -- the "+
				"monitor skipped a tick with markets %v",
				base+i, len(got), base+i+1, markets)
		}
		res := got[base+i]
		if res.Stale || !res.IntegrateUptime {
			t.Fatalf("tick %d: an advancing source was reported stale=%v "+
				"integrate=%v with markets %v",
				base+i, res.Stale, res.IntegrateUptime, markets)
		}
		if len(res.Samples) != len(markets) {
			t.Fatalf("tick %d: %d samples, want exactly one per selected "+
				"market (%d): %+v", base+i, len(res.Samples), len(markets),
				res.Samples)
		}
		for j, m := range markets {
			s := res.Samples[j]
			if s.Ticker != m.ticker || s.Snap.State != m.state {
				t.Fatalf("tick %d sample %d: %s in %s, want %s in %s",
					base+i, j, s.Ticker, s.Snap.State, m.ticker, m.state)
			}
			if s.Stale {
				t.Fatalf("tick %d: the sample for %s in %s was marked stale "+
					"while the owner was publishing normally",
					base+i, m.ticker, m.state)
			}
			if s.Snap.Q.IsFlat() {
				t.Fatalf("tick %d: the sample for %s in %s carries q = 0 -- "+
					"H-CLOSE-2a and V7.10 are about the window with q != 0",
					base+i, m.ticker, m.state)
			}
		}

		now := d.curNow()
		if v := d.mon.checkA5(now, d.snapshot()); len(v) != 0 {
			t.Fatalf("tick %d (%v): A5 violated with markets %v: %v",
				base+i, now, markets, v)
		}
		if now-firstTick >= stallAfter {
			postGrace++
		}
	}

	if postGrace == 0 {
		t.Fatalf("every A5 check fell inside its initial grace period (%v "+
			"elapsed, grace %v) -- the A5 assertion was vacuous",
			d.curNow(), stallAfter)
	}
	total, stale := d.mon.state.Counts()
	if want := baseTotal + uint64(ticks*len(markets)); total != want {
		t.Fatalf("%d samples after %d ticks of %d markets %v, want %d -- "+
			"sampling stopped or thinned out",
			total, ticks, len(markets), markets, want)
	}
	if stale != baseStale {
		t.Fatalf("%d samples became stale while the owner was publishing "+
			"normally with markets %v", stale-baseStale, markets)
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
