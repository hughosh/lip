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

// nakedQty is the anchor position for a driven market. 61 contracts: the size
// probebot.py left naked and directional for 6.14 hours, one contract above
// §10.3's inv_hard of 60, and the reason H-CLOSE-2a refuses to let SETTLING be
// an exemption from having an exit.
var nakedQty = num.QtyFromFloat(61)

// peerQty and thirdQty are the positions a driven market's PEERS carry. Every
// driven market holds a distinct nonzero position, because the contract is
// per market: §8.3 step 3 writes one snap row per market and A5 wants a sample
// per selected market, so each sample is held to its own market's q. peerQty is
// negative -- long NO, §8.1 -- and both are fractional, which H-CO-4a records at
// a measured 20.5% of sizes rather than as a theoretical case.
var (
	peerQty  = num.QtyFromFloat(-17.5)
	thirdQty = num.QtyFromFloat(3.25)
)

// drivenQty is the position market i carries when the caller does not name one
// per market. Distinct per index and never flat, for the reason above.
func drivenQty(i int) num.Qty { return nakedQty - num.Qty(i)*num.QtyScale }

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
// has to be able to say so. They do not share a position either: q is per
// market, and so it is the caller's to name too.
type marketState struct {
	ticker string
	state  quote.MarketState
	q      num.Qty
}

// publishStates writes a new snapshot with an ADVANCING seq and caller-chosen
// per-market states, as the owner would, and returns the seq it published.
//
// The returned seq is what the sampling assertions are held to. A sample
// carries the SourceSeq it was derived from (H-TOP-5), and §8.3 step 0 makes
// source ADVANCEMENT rather than row freshness the property that counts, so a
// caller that knows which publication it just made can require the sample to
// have come from that one.
//
// Every market carries the q its caller named, so a sample can be held to the
// position published for THAT market. H-CLOSE-2a and V7.10 are about the close
// window precisely because q != 0 there.
func (d *driver) publishStates(g quote.GlobalState, markets ...marketState) uint64 {
	d.mu.Lock()
	d.seq++
	seq := d.seq
	now := d.now
	d.mu.Unlock()
	ms := make([]risk.MarketSnap, 0, len(markets))
	for _, m := range markets {
		ms = append(ms, risk.MarketSnap{Ticker: m.ticker, Selected: true,
			State: m.state, Q: m.q})
	}
	d.src.Store(&risk.Snapshot{Seq: seq, PubMono: now, Global: g, Markets: ms})
	return seq
}

// publish writes a new snapshot with an ADVANCING seq, as the owner would, with
// every market REDUCING and carrying its own nonzero position.
func (d *driver) publish(g quote.GlobalState, tickers ...string) {
	ms := make([]marketState, 0, len(tickers))
	for i, tk := range tickers {
		ms = append(ms, marketState{ticker: tk, state: quote.Reducing,
			q: drivenQty(i)})
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
			driveMarketStates(t, newDriver(t), quote.Running, []marketState{
				{ticker: "KXTEST-A", state: st, q: nakedQty},
				{ticker: "KXTEST-B", state: st, q: peerQty},
			}, ticks)
		})
	}

	// Market state is per market, and so is inventory: one snapshot, three
	// markets, three states, three positions. §8.3 step 3 writes a row per
	// market and A5 wants a sample per selected market, so the SETTLING market
	// and its still-quoting, still-reducing peers each owe a sample of their
	// own, carrying their own state and their own q.
	t.Run("SETTLING_alongside_peers", func(t *testing.T) {
		driveMarketStates(t, newDriver(t), quote.Running, []marketState{
			{ticker: "KXTEST-A", state: quote.Quoting, q: peerQty},
			{ticker: "KXTEST-B", state: quote.Settling, q: nakedQty},
			{ticker: "KXTEST-C", state: quote.Reducing, q: thirdQty},
		}, ticks)
	})

	// H-CLOSE-2 is a TRANSITION, not a starting condition: at close_time -
	// close_lead a market that was quoting goes to SETTLING while the owner
	// keeps publishing. The subtests above start markets in a state; this one
	// drives the edge into it. The market keeps its own q across the edge, so
	// the transition is held to carrying that inventory over.
	t.Run("entry_to_SETTLING", func(t *testing.T) {
		d := newDriver(t)
		driveMarketStates(t, d, quote.Running, []marketState{
			{ticker: "KXTEST-A", state: quote.Quoting, q: nakedQty},
			{ticker: "KXTEST-B", state: quote.Reducing, q: peerQty},
		}, 4)
		driveMarketStates(t, d, quote.Running, []marketState{
			{ticker: "KXTEST-A", state: quote.Settling, q: nakedQty},
			{ticker: "KXTEST-B", state: quote.Reducing, q: peerQty},
		}, ticks)
	})

	// SETTLING has two inbound edges and the subtest above drives only one.
	// QUOTING -> SETTLING is the plain close_lead crossing; REDUCING ->
	// SETTLING is the same crossing for a market that already passed end_date
	// (H-CLOSE-1). Under §10.3 -- close_lead 1h, final_lead 60s,
	// close_lead_keep_reducing true -- that second edge is the ordinary
	// deployed order of events for any market closing after the program ends,
	// and it is the edge H-CLOSE-2a's q != 0 window opens on.
	t.Run("entry_to_SETTLING_from_REDUCING", func(t *testing.T) {
		d := newDriver(t)
		driveMarketStates(t, d, quote.Running, []marketState{
			{ticker: "KXTEST-A", state: quote.Reducing, q: nakedQty},
			{ticker: "KXTEST-B", state: quote.Quoting, q: peerQty},
		}, 4)
		driveMarketStates(t, d, quote.Running, []marketState{
			{ticker: "KXTEST-A", state: quote.Settling, q: nakedQty},
			{ticker: "KXTEST-B", state: quote.Quoting, q: peerQty},
		}, ticks)
	})
}

// TestMonitorSamplesWhileWindingDownAndSettling drives the one conjunction the
// tests above never reach together: the GLOBAL state in WINDING_DOWN while a
// selected market sits in SETTLING with inventory still open.
//
// M1 covers WINDING_DOWN with every market REDUCING.
// TestMonitorSamplesInEveryMarketState covers SETTLING while the global state
// is RUNNING. Neither says anything about the state the harness is actually in
// during the close window of a wind-down, and that is the ordinary deployed
// order of events, not a contrived one:
//
//   - §10.3 runs with inv_hard = 60, so q = 61 is a position the harness holds
//     rather than one invented for a test;
//   - H-HALT-3 says SIGTERM does not exit -- it sets WINDING_DOWN and keeps the
//     process alive with that inventory still open, for as long as it takes;
//   - H-CLOSE-2 then sends the market to SETTLING at close_time - close_lead,
//     which arrives on the exchange's clock whatever the global state is;
//   - H-CLOSE-2a keeps that window substantive until final_lead: SETTLING with
//     q != 0 still owes a capped reducing quote, so it is precisely the window
//     in which observation must not lapse (V7.10).
//
// §5.1 gives WINDING_DOWN full monitoring and a live process. I1 says no stop
// path stops watching. I2 and §8.3 put the sampler at 1 Hz on a path nothing in
// the quoting side can reach, ending only with the process. A5 then requires a
// source-advanced sample for every selected market in every global state. The
// requirement is therefore exact, and asserted as such below: every tick, every
// market, fresh, from this publication, with that market's own inventory.
func TestMonitorSamplesWhileWindingDownAndSettling(t *testing.T) {
	// Every phase runs longer than owner_stall_s (3s) at 1 Hz, and the phase
	// under test runs twice as long, so nothing here can pass on the strength
	// of A5's startup grace.
	const (
		runTicks     = 4
		windTicks    = 4
		settlingTick = 6
	)

	d := newDriver(t)

	// The same three markets throughout, each holding its own position, so a
	// market's inventory can be followed ACROSS the transitions and not merely
	// within one phase.
	reducing := []marketState{
		{ticker: "KXTEST-A", state: quote.Reducing, q: nakedQty},
		{ticker: "KXTEST-B", state: quote.Reducing, q: peerQty},
		{ticker: "KXTEST-C", state: quote.Reducing, q: thirdQty},
	}

	// 1. RUNNING, already reducing -- the healthy baseline the probe had.
	driveMarketStates(t, d, quote.Running, reducing, runTicks)

	// 2. SIGTERM (H-HALT-3). The process stays alive, adding is off
	//    permanently, the reducers stay live, monitoring stays full.
	driveMarketStates(t, d, quote.WindingDown, reducing, windTicks)

	// 3. The close lead arrives DURING the wind-down (H-CLOSE-2). KXTEST-A
	//    enters SETTLING holding its 61 contracts and stays there, its peers
	//    still reducing their own distinct positions. The conjunction is held
	//    for twice owner_stall_s, so every one of its six ticks is checked --
	//    including the ones past the point where A5's startup grace and the
	//    stall window could have excused silence.
	settling := []marketState{
		{ticker: "KXTEST-A", state: quote.Settling, q: nakedQty},
		{ticker: "KXTEST-B", state: quote.Reducing, q: peerQty},
		{ticker: "KXTEST-C", state: quote.Reducing, q: thirdQty},
	}
	driveMarketStates(t, d, quote.WindingDown, settling, settlingTick)

	// The loop is still the thing that ends only when the process does.
	select {
	case <-d.done:
		t.Fatal("the monitor loop exited during a WINDING_DOWN close window " +
			"-- §5.1 gives WINDING_DOWN full monitoring and a live process, " +
			"and I1 says no stop path stops watching")
	default:
	}
	if err := d.ctx.Err(); err != nil {
		t.Fatalf("the monitor context was cancelled by the test itself (%v) "+
			"-- the liveness assertion proves nothing", err)
	}

	// Exact cumulative arithmetic over the whole lifecycle: three markets,
	// every tick of every phase. A dropped tick or a dropped peer anywhere
	// between RUNNING and the close window changes this number.
	total, stale := d.mon.state.Counts()
	if want := uint64(len(reducing) * (runTicks + windTicks + settlingTick)); total != want {
		t.Fatalf("%d samples across RUNNING -> WINDING_DOWN -> the SETTLING "+
			"close window, want %d -- sampling stopped or thinned out",
			total, want)
	}
	if stale != 0 {
		t.Fatalf("%d samples were stale while the owner published on every "+
			"tick of the wind-down", stale)
	}
	if v := d.mon.checkA5(d.curNow(), d.snapshot()); len(v) != 0 {
		t.Fatalf("A5 violated at the end of the WINDING_DOWN close window "+
			"(%v elapsed): %v", d.curNow(), v)
	}
}

// driveMarketStates publishes `markets` in global state `g` with an advancing
// seq and steps the monitor one second at a time, asserting after EVERY tick
// that the monitor context is still live, that the loop is still alive, and
// that the tick produced exactly one fresh sample per selected market --
// carrying that market's published ticker, state and exact q, derived from the
// seq just published. It may be called more than once against the same driver,
// to drive a transition of the global state, the market states, or both.
func driveMarketStates(t *testing.T, d *driver, g quote.GlobalState,
	markets []marketState, ticks int) {
	t.Helper()

	// The scenario has to be capable of showing what it claims to show. The
	// per-market q assertion below distinguishes one market from another only
	// if their positions differ, and it says something about the window
	// H-CLOSE-2a and V7.10 are written for only if those positions are nonzero.
	// Both are preconditions on the caller, so both are checked here.
	byQty := make(map[num.Qty]string, len(markets))
	for _, m := range markets {
		if m.q.IsFlat() {
			t.Fatalf("market %s in %s was driven flat -- the property under "+
				"test is about a market carrying q != 0", m.ticker, m.state)
		}
		if other, dup := byQty[m.q]; dup {
			t.Fatalf("markets %s and %s were both driven with q = %s -- the "+
				"per-market q assertion needs distinct positions to tell one "+
				"market's inventory from another's", other, m.ticker,
				m.q.Wire())
		}
		byQty[m.q] = m.ticker
	}

	base := len(d.results())
	baseTotal, baseStale := d.mon.state.Counts()
	// The driver's clock starts at 0 and the first tick lands at 1s, which is
	// when A5 starts tracking; its grace period runs owner_stall_s from there.
	const firstTick = time.Second
	postGrace := 0

	for i := 0; i < ticks; i++ {
		seq := d.publishStates(g, markets...)
		d.step(time.Second)

		select {
		case <-d.done:
			t.Fatalf("the monitor loop exited at tick %d in %s with markets "+
				"%v -- I1: no stop path stops watching (ctx err %v: the "+
				"context was never cancelled, so nothing was entitled to stop "+
				"it)", base+i, g, markets, d.ctx.Err())
		default:
		}
		// I2 is structural: the ONLY thing permitted to end this loop is its
		// context, and this test never cancels it before Cleanup. Assert that
		// directly -- the liveness check above says something about the
		// monitor only while the context it was given is uncancelled.
		if err := d.ctx.Err(); err != nil {
			t.Fatalf("tick %d: the monitor context was cancelled by the test "+
				"itself (%v) -- the liveness assertion proves nothing",
				base+i, err)
		}

		got := d.results()
		if len(got) != base+i+1 {
			t.Fatalf("tick %d produced %d results in total, want %d -- the "+
				"monitor skipped a tick in %s with markets %v",
				base+i, len(got), base+i+1, g, markets)
		}
		res := got[base+i]
		if res.Stale || !res.IntegrateUptime {
			t.Fatalf("tick %d: an advancing source was reported stale=%v "+
				"integrate=%v in %s with markets %v",
				base+i, res.Stale, res.IntegrateUptime, g, markets)
		}
		if len(res.Samples) != len(markets) {
			t.Fatalf("tick %d in %s: %d samples, want exactly one per "+
				"selected market (%d): %+v", base+i, g, len(res.Samples),
				len(markets), res.Samples)
		}
		for j, m := range markets {
			s := res.Samples[j]
			if s.Ticker != m.ticker || s.Snap.State != m.state {
				t.Fatalf("tick %d sample %d in %s: %s in %s, want %s in %s",
					base+i, j, g, s.Ticker, s.Snap.State, m.ticker, m.state)
			}
			if s.Stale {
				t.Fatalf("tick %d: the sample for %s in %s under %s was "+
					"marked stale while the owner was publishing normally",
					base+i, m.ticker, m.state, g)
			}
			// The q asserted is THIS market's, and the precondition above
			// guarantees it is nonzero and shared with no peer, so the sample
			// must carry the position published under its own ticker.
			if s.Snap.Q != m.q {
				t.Fatalf("tick %d: the sample for %s in %s under %s carries "+
					"q = %s, want the %s published for that market -- "+
					"H-CLOSE-2a and V7.10 are about the window with q != 0, "+
					"and every sample owes its own market's position",
					base+i, m.ticker, m.state, g, s.Snap.Q.Wire(), m.q.Wire())
			}
			// H-TOP-5 and §8.3 step 0: the sample must come from THIS
			// publication. Source advancement, not row freshness, is what A5
			// is written against, and the seq is what carries it.
			if s.SourceSeq != seq {
				t.Fatalf("tick %d: the sample for %s in %s under %s was "+
					"derived from source seq %d, want the seq just published "+
					"(%d) -- the monitor is describing an older publication",
					base+i, m.ticker, m.state, g, s.SourceSeq, seq)
			}
		}
		if got := d.mon.state.LastSeq(); got != seq {
			t.Fatalf("tick %d: the monitor last observed source seq %d, want "+
				"%d -- it stopped following the owner's advancing sequence "+
				"in %s with markets %v", base+i, got, seq, g, markets)
		}

		now := d.curNow()
		if v := d.mon.checkA5(now, d.snapshot()); len(v) != 0 {
			t.Fatalf("tick %d (%v): A5 violated in %s with markets %v: %v",
				base+i, now, g, markets, v)
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
		t.Fatalf("%d samples after %d ticks of %d markets %v in %s, want %d "+
			"-- sampling stopped or thinned out",
			total, ticks, len(markets), markets, g, want)
	}
	if stale != baseStale {
		t.Fatalf("%d samples became stale while the owner was publishing "+
			"normally in %s with markets %v", stale-baseStale, g, markets)
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
