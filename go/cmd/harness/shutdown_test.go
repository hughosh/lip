package main

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"lip/harness/cfg"
	"lip/harness/hstore"
	"lip/harness/lifecycle"
	"lip/harness/quote"
	"lip/harness/risk"
)

// The assertions here are all at SEAMS, because every part they join is already
// the most heavily unit-tested code in the tree. `NextGlobal`, `DrainTracker`,
// `SignalController` and `hstore`'s writer each have their own suite and their
// own mutations; what nothing covered until now is whether this file joins them
// in the right order, with the right lifetime, and refuses in the right
// direction.

// fixture is one wired-up rig on a temporary store.
//
// The clock is the only thing faked. `mono` is stepped by hand so the drain
// escalation can be driven across twelve hours in a millisecond, and `wall` is
// separate from it for the same reason `exchange` separates them: an elapsed
// interval measured on a wall clock goes negative over an NTP correction, and a
// test that shared one would be testing a clock the harness does not have.
type fixture struct {
	t     *testing.T
	rig   *rig
	sd    *shutdown
	mono  time.Duration
	wall  int64
	exits []int
}

func newFixture(t *testing.T, tune func(*cfg.Params)) *fixture {
	t.Helper()

	// t.TempDir ONLY. The repository's live collectors are writing rig.db and
	// lip.db right now, and a test that opened one would be a second writer on a
	// database whose whole design is that there is one.
	dir := t.TempDir()

	p := cfg.Default()
	if tune != nil {
		tune(&p)
	}
	if err := p.Validate(); err != nil {
		t.Fatalf("the tuned §16 parameters are not valid: %v", err)
	}

	store, err := hstore.Open(hstore.StoreConfig{
		DBPath:         filepath.Join(dir, "harness.db"),
		AnomalyLogPath: filepath.Join(dir, "anomaly.jsonl"),
	})
	if err != nil {
		t.Fatalf("hstore.Open: %v", err)
	}

	storeCtx, storeCancel := context.WithCancel(context.Background())
	storeDone := make(chan struct{})
	go func() {
		defer close(storeDone)
		store.Run(storeCtx)
	}()

	f := &fixture{t: t, wall: 1_700_000_000_000}

	runID, err := newRunID()
	if err != nil {
		t.Fatalf("newRunID: %v", err)
	}
	rcpt, err := store.BeginRun(runID, f.now(), p)
	if err != nil {
		t.Fatalf("BeginRun: %v", err)
	}
	run, deferred, err := awaitRunHandle(context.Background(), store, rcpt)
	if err != nil {
		t.Fatalf("awaitRunHandle: %v", err)
	}

	latch, err := lifecycle.NewFileLatch(filepath.Join(dir, "halt.latch"))
	if err != nil {
		t.Fatalf("NewFileLatch: %v", err)
	}
	ctrl, _, err := lifecycle.NewGlobalController(latch)
	if err != nil {
		t.Fatalf("NewGlobalController: %v", err)
	}
	sigs, err := lifecycle.NewSignalController(ctrl)
	if err != nil {
		t.Fatalf("NewSignalController: %v", err)
	}
	drain, err := lifecycle.NewDrainTracker(p)
	if err != nil {
		t.Fatalf("NewDrainTracker: %v", err)
	}

	f.rig = &rig{
		cfg:         config{Params: p, Ticker: "KXTEST-25AUG07-A"},
		latch:       latch,
		ctrl:        ctrl,
		sigs:        sigs,
		drain:       drain,
		store:       store,
		storeCancel: storeCancel,
		storeDone:   storeDone,
		run:         run,
		runID:       runID,
		deferred:    deferred,
		anom:        newAnomalySink(),
		ex:          exchange{NowMs: f.now, Mono: f.elapsed},
	}
	f.sd = newShutdown(f.rig)
	// The whole point of the injection: the real os.Exit would take the test
	// binary with it, and "SIGTERM does not exit" would be unassertable.
	f.sd.exit = func(code int) { f.exits = append(f.exits, code) }

	t.Cleanup(func() {
		// Idempotent: rig.close on an already-closed store drains an empty queue
		// and Close returns nil, so a test that stopped deliberately is not
		// penalised here.
		if err := f.rig.close(context.Background()); err != nil {
			t.Errorf("the store would not stop cleanly: %v", err)
		}
	})
	return f
}

func (f *fixture) now() int64             { return f.wall }
func (f *fixture) elapsed() time.Duration { return f.mono }

// flush blocks until every submitted record is durable, so the assertions read
// the database rather than a receipt.
func (f *fixture) flush() {
	f.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := f.rig.store.Drain(ctx); err != nil {
		f.t.Fatalf("the store did not drain: %v", err)
	}
}

func (f *fixture) anomaliesByClass(class string) []hstore.AnomalyRow {
	f.t.Helper()
	rows, err := f.rig.store.Reader().PendingAnomalies()
	if err != nil {
		f.t.Fatalf("PendingAnomalies: %v", err)
	}
	var out []hstore.AnomalyRow
	for _, r := range rows {
		if r.Class == class {
			out = append(out, r)
		}
	}
	return out
}

func (f *fixture) stateEvents() []hstore.StateEventRow {
	f.t.Helper()
	rows, err := f.rig.store.Reader().StateEvents()
	if err != nil {
		f.t.Fatalf("StateEvents: %v", err)
	}
	return rows
}

// takeRaised drains the anomaly sink without submitting anything, so a test can
// assert on what a component RAISED rather than on what survived the writer.
func (f *fixture) takeRaised() []risk.Anomaly {
	var out []risk.Anomaly
	for {
		select {
		case a := <-f.rig.anom.ch:
			out = append(out, a)
		default:
			return out
		}
	}
}

func countClass(as []risk.Anomaly, class string) int {
	n := 0
	for _, a := range as {
		if a.Class == class {
			n++
		}
	}
	return n
}

// TestSIGTERMWithInventoryDoesNotAuthoriseAnExitAndKeepsEscalating is H-HALT-3
// and HR-009 at this file's own seam.
//
// > SIGTERM does not exit. It sets `WINDING_DOWN`, keeps the process alive, and
// > exits only when every market is flat or closed.
//
// > **`drain_timeout_h` (12h) escalates; it does not exit.** At the timeout the
// > harness pings `SEV1` and keeps pinging on an escalating cadence -- it does
// > not terminate with `q != 0`.
//
// The escalation is asserted as a SHORTENING cadence and not merely as "an
// anomaly eventually appears", because HR-009's defect was a timer that got
// quieter and then acted: an operator who has not responded in twelve hours is
// an operator the alert is failing to reach. With drain_timeout_h = 4h the gaps
// are 4h, then 2h, then 1h (the floor), and the observations at 5h59m and 6h59m
// are what prove the tracker is not simply pinging on every tick.
func TestSIGTERMWithInventoryDoesNotAuthoriseAnExitAndKeepsEscalating(t *testing.T) {
	f := newFixture(t, func(p *cfg.Params) { p.DrainTimeout = 4 * time.Hour })

	in := quote.GlobalInput{
		State: quote.Running, TruthReadable: true, Reconciled: true,
		AnyInventory: true, AnyLiveOrder: true, RiskKnown: true,
	}
	eff := f.sd.onSignal(syscall.SIGTERM, in)
	if !eff.Recognised {
		t.Fatal("SIGTERM was not recognised")
	}
	if !eff.Decision.Committed || eff.Decision.State != quote.WindingDown {
		t.Fatalf("SIGTERM did not produce a committed WINDING_DOWN: %+v",
			eff.Decision)
	}
	if !eff.Permit.Valid() || eff.Permit.Trigger() != "sigterm" {
		t.Fatalf("no drain permit was issued for a committed stop: %+v", eff.Permit)
	}
	if !f.rig.drain.Started() {
		t.Fatal("the signal committed a durable stop and no drain was begun; " +
			"nothing would ever escalate about the inventory that is still open")
	}
	f.takeRaised()

	obs := lifecycle.DrainObservation{TruthKnown: true, AnyInventory: true}
	steps := []struct {
		at    time.Duration
		pings int
	}{
		{1 * time.Hour, 0},
		{4 * time.Hour, 1},
		{5*time.Hour + 59*time.Minute, 0},
		{6 * time.Hour, 1},
		{6*time.Hour + 59*time.Minute, 0},
		{7 * time.Hour, 1},
		{8 * time.Hour, 1},
	}
	for _, st := range steps {
		f.mono = st.at
		if f.sd.observeDrain(obs) {
			t.Fatalf("an exit was authorised %s into a drain with inventory "+
				"still open. HR-009: a drain timeout is evidence the operator is "+
				"needed, not authority to abandon", st.at)
		}
		if got := countClass(f.takeRaised(), "DRAIN_TIMEOUT"); got != st.pings {
			t.Fatalf("at %s: %d DRAIN_TIMEOUT escalations, want %d",
				st.at, got, st.pings)
		}
	}

	if len(f.exits) != 0 {
		t.Fatalf("the process exited %v during a drain with inventory open; "+
			"H-FAIL-1 permits exactly one exit this code can cause, and it "+
			"requires the account to be flat and unrested", f.exits)
	}
}

// TestExitIsAuthorisedOnlyByAPermitAndAFlatUnrestedTruthKnownAccount is
// `DrainEffects.ExitAuthorised`'s four conjuncts, each denied in turn.
//
// The unplanned case is the one worth having: §5.1 says "DRAINED does not exit
// the process; it idles and keeps monitoring", so a taker fill or a pnl_kill
// that drains completely must NOT end the process -- exiting there would hand
// `launchd KeepAlive` a process to restart into a latched state forever.
func TestExitIsAuthorisedOnlyByAPermitAndAFlatUnrestedTruthKnownAccount(t *testing.T) {
	cases := []struct {
		name    string
		planned bool
		obs     lifecycle.DrainObservation
		want    bool
	}{
		{"planned drain, truth known, flat, nothing resting",
			true, lifecycle.DrainObservation{TruthKnown: true}, true},
		{"planned drain but truth cannot be read",
			true, lifecycle.DrainObservation{}, false},
		{"planned drain but inventory is open",
			true, lifecycle.DrainObservation{TruthKnown: true, AnyInventory: true}, false},
		{"planned drain but an order of ours still rests",
			true, lifecycle.DrainObservation{TruthKnown: true, AnyLiveOrder: true}, false},
		{"unplanned drain, otherwise perfectly drained",
			false, lifecycle.DrainObservation{TruthKnown: true}, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t, nil)
			if tc.planned {
				eff := f.sd.onSignal(syscall.SIGTERM, quote.GlobalInput{
					State: quote.Running, TruthReadable: true, Reconciled: true,
					RiskKnown: true,
				})
				if !eff.Permit.Valid() {
					t.Fatalf("no permit from a committed stop: %+v", eff)
				}
			} else {
				f.rig.drain.BeginUnplanned(f.elapsed())
			}
			if got := f.sd.observeDrain(tc.obs); got != tc.want {
				t.Fatalf("ExitAuthorised = %v, want %v for %+v",
					got, tc.want, tc.obs)
			}
		})
	}
}

// TestOnlyAnAuthorisedDrainReachesTheProcessExit walks the whole path: a signal,
// an undrained observation that changes nothing, then a drained one.
//
// It asserts the exit happens EXACTLY once and with code 0. `M3` is "os.Exit on
// SIGTERM instead of draining": under it the first observation would already
// have exited, and the count catches that without the test binary dying.
func TestOnlyAnAuthorisedDrainReachesTheProcessExit(t *testing.T) {
	f := newFixture(t, nil)

	eff := f.sd.onSignal(syscall.SIGTERM, quote.GlobalInput{
		State: quote.Running, TruthReadable: true, Reconciled: true,
		AnyInventory: true, RiskKnown: true,
	})
	if !eff.Permit.Valid() {
		t.Fatalf("no permit from a committed stop: %+v", eff)
	}

	obs := make(chan lifecycle.DrainObservation, 2)
	obs <- lifecycle.DrainObservation{TruthKnown: true, AnyInventory: true}
	obs <- lifecycle.DrainObservation{TruthKnown: true}
	close(obs)

	if err := f.sd.drainLoop(context.Background(), obs); err != nil {
		t.Fatalf("drainLoop: %v", err)
	}
	if len(f.exits) != 1 || f.exits[0] != 0 {
		t.Fatalf("process exits = %v, want exactly [0]: the undrained "+
			"observation must change nothing and the drained one must end the "+
			"process cleanly", f.exits)
	}
}

// TestALostStoreRecordProducesOneSEV1PerRecordNotOnePerStore is `reject.go`'s
// argument, asserted.
//
// > It UNDERCOUNTS. `fault` is one string for the whole store, overwritten by
// > each rejection, so two lost records read as one condition and the one it
// > names is whichever happened last.
//
// So three lost records must produce three SEV1s, each carrying its own
// receipt's sequence number and its own reason. A result loop that read
// `Health()` after draining its receipts would produce one anomaly naming the
// last failure, and this test is what tells the two apart.
func TestALostStoreRecordProducesOneSEV1PerRecordNotOnePerStore(t *testing.T) {
	f := newFixture(t, nil)

	// Real receipts. A fabricated zero receipt would make all three records
	// indistinguishable, which is the very condition being ruled out.
	var batch []hstore.Result
	for i := 1; i <= 3; i++ {
		rcpt, err := f.rig.store.RecordAnomaly(f.rig.run,
			fmt.Sprintf("%s-seed%d", f.rig.runID, i),
			risk.Anomaly{
				Class: "BOOK_QUIET", Sev: risk.SEV3,
				Text: fmt.Sprintf("seed %d", i),
			}, f.now())
		if err != nil {
			t.Fatalf("seed submission %d: %v", i, err)
		}
		batch = append(batch, hstore.Result{
			Receipt: rcpt,
			Kind:    hstore.KindAnomaly,
			Err:     fmt.Errorf("the disk went away at record %d", i),
		})
	}
	f.flush()
	f.rig.store.TakeResults()

	f.sd.handleResults(batch)
	f.flush()

	rows := f.anomaliesByClass(hstore.RecordRejectedClass)
	if len(rows) != 3 {
		t.Fatalf("%d %s anomalies for 3 lost records, want 3. One per STORE "+
			"loses the per-record identity, and the record it names is whichever "+
			"failed last", len(rows), hstore.RecordRejectedClass)
	}
	for _, row := range rows {
		if row.Sev != risk.SEV1 {
			t.Fatalf("a lost record raised sev%d, want SEV1: a missing audit row "+
				"is a hole in the evidence the next decision is taken from",
				row.Sev)
		}
	}
	for i, res := range batch {
		want := fmt.Sprintf("the disk went away at record %d", i+1)
		seq := fmt.Sprintf("#%d", res.Receipt.Seq())
		hits := 0
		for _, row := range rows {
			if strings.Contains(row.Text, want) && strings.Contains(row.Text, seq) {
				hits++
			}
		}
		if hits != 1 {
			t.Fatalf("%d anomalies name record %s with reason %q, want exactly 1.\n"+
				"rows: %+v", hits, seq, want, rows)
		}
	}
}

// TestTheResultLoopStartsFromTheResultsThatArrivedDuringConstruction is
// `rig.deferred`'s reason for existing.
//
// `awaitRunHandle` returns results that are not the run row rather than
// discarding them, because "a terminal result is a record that is GONE, and
// `hstore.Rejections` raises a SEV1 per lost record. Dropping one because it
// arrived early would lose exactly the evidence that says the store is already
// failing."
//
// The context is cancelled BEFORE the call, so the loop handles the deferred
// batch, makes one ordinary pass, and returns -- no goroutine, no sleep, no
// timing.
func TestTheResultLoopStartsFromTheResultsThatArrivedDuringConstruction(t *testing.T) {
	f := newFixture(t, nil)

	rcpt, err := f.rig.store.RecordAnomaly(f.rig.run, f.rig.runID+"-seed",
		risk.Anomaly{Class: "BOOK_QUIET", Sev: risk.SEV3, Text: "seed"}, f.now())
	if err != nil {
		t.Fatalf("seed submission: %v", err)
	}
	f.flush()
	f.rig.store.TakeResults()

	f.rig.deferred = []hstore.Result{{
		Receipt: rcpt,
		Kind:    hstore.KindAnomaly,
		Err:     errors.New("this record died before the run row committed"),
	}}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := f.sd.runResults(ctx); err != nil {
		t.Fatalf("runResults: %v", err)
	}
	f.flush()

	rows := f.anomaliesByClass(hstore.RecordRejectedClass)
	if len(rows) != 1 {
		t.Fatalf("%d %s anomalies, want 1: a result that arrived during "+
			"construction is still a record that is gone",
			len(rows), hstore.RecordRejectedClass)
	}
	if f.rig.deferred != nil {
		t.Fatal("the deferred batch was not cleared, so a second pass would " +
			"report the same lost record again")
	}
}

// TestATransitionThatWasNotCommittedWritesNoStateEvent is A9's mirror refusing
// to record a transition the harness never made.
//
// `GlobalDecision.Committed == false` means "no such transition is being
// returned. `State` is then the input state -- the caller publishes nothing,
// blocks adding, and retries." A mirror that wrote it anyway would put a halt in
// the run's own history that never happened, which is worse than the missing row
// it was trying to avoid: an operator reconstructing why the harness stopped
// adding would find a cause the harness rejected.
func TestATransitionThatWasNotCommittedWritesNoStateEvent(t *testing.T) {
	f := newFixture(t, nil)

	// Refused: the shape `GlobalController.refuse` returns when a stop was
	// requested with nothing on the durable latch.
	f.sd.mirrorGlobal(quote.Running, lifecycle.GlobalDecision{
		State: quote.Running, Trigger: quote.GTNone, Committed: false,
		BlockAdding: true, RetryLatch: true,
	})
	// Committed, but nothing changed: NextGlobal's way of saying nothing
	// happened.
	f.sd.mirrorGlobal(quote.Running, lifecycle.GlobalDecision{
		State: quote.Running, Trigger: quote.GTNone, Committed: true,
	})
	// Market side, same two refusals.
	f.sd.mirrorMarket("KXTEST-25AUG07-A", quote.Quoting, quote.Quoting,
		quote.MTNone)
	f.flush()

	if rows := f.stateEvents(); len(rows) != 0 {
		t.Fatalf("%d state_event rows were written for transitions that did not "+
			"happen: %+v", len(rows), rows)
	}
	if raised := f.takeRaised(); countClass(raised, stateEventUnrecordedClass) != 0 {
		t.Fatalf("a non-transition was submitted and refused, rather than never "+
			"submitted: %+v", raised)
	}

	f.sd.mirrorGlobal(quote.Running, lifecycle.GlobalDecision{
		State: quote.WindingDown, Trigger: quote.GTStop, Committed: true,
	})
	f.sd.mirrorMarket("KXTEST-25AUG07-A", quote.Quoting, quote.Reducing,
		quote.MTGlobalStop)
	f.flush()

	rows := f.stateEvents()
	if len(rows) != 2 {
		t.Fatalf("%d state_event rows for two real transitions, want 2: %+v",
			len(rows), rows)
	}
	var global, market hstore.StateEventRow
	for _, r := range rows {
		switch r.Scope {
		case "global":
			global = r
		case "market":
			market = r
		}
	}
	if global.From != "RUNNING" || global.To != "WINDING_DOWN" ||
		global.Trigger != "global_stop" || global.Ticker != "" {
		t.Fatalf("global state_event = %+v, want RUNNING -> WINDING_DOWN "+
			"(global_stop) with no ticker", global)
	}
	if market.From != "QUOTING" || market.To != "REDUCING" ||
		market.Trigger != "global_stop" || market.Ticker != "KXTEST-25AUG07-A" {
		t.Fatalf("market state_event = %+v, want QUOTING -> REDUCING "+
			"(global_stop) on KXTEST-25AUG07-A", market)
	}
	if global.RunID != f.rig.runID || market.RunID != f.rig.runID {
		t.Fatalf("state_event rows are attributed to %q/%q, want %q",
			global.RunID, market.RunID, f.rig.runID)
	}
}

// TestAFullAnomalyBufferProducesADropReportRatherThanSilence is `anomalySink`'s
// honest trade, held to its own promise.
//
// > A burst past it drops, and the drop is COUNTED and reported rather than
// > silent.
//
// Nothing made that sentence true before this file: `takeDropped` had no caller.
// The report is synthesised and submitted DIRECTLY rather than raised, because a
// report about a full buffer that is raised into the full buffer is the drop it
// is reporting.
func TestAFullAnomalyBufferProducesADropReportRatherThanSilence(t *testing.T) {
	f := newFixture(t, nil)
	const overflow = 5

	for i := 0; i < anomalyBuffer+overflow; i++ {
		f.rig.anom.raise(risk.Anomaly{
			Class: "BOOK_QUIET", Sev: risk.SEV2,
			Text: fmt.Sprintf("burst %d", i),
		})
	}

	f.sd.handleAnomalies()
	f.flush()

	reports := f.anomaliesByClass(anomalyDroppedClass)
	if len(reports) != 1 {
		t.Fatalf("%d %s reports for %d dropped anomalies, want exactly 1: a "+
			"drop that is not reported is the monitor's evidence disappearing at "+
			"the moment there was too much of it",
			len(reports), anomalyDroppedClass, overflow)
	}
	if reports[0].Sev != risk.SEV1 {
		t.Fatalf("the drop report is sev%d, want SEV1", reports[0].Sev)
	}
	if !strings.Contains(reports[0].Text, fmt.Sprintf("%d anomal", overflow)) {
		t.Fatalf("the drop report does not name how many were lost: %q",
			reports[0].Text)
	}

	if kept := f.anomaliesByClass("BOOK_QUIET"); len(kept) != anomalyBuffer {
		t.Fatalf("%d of the burst reached the store, want %d: everything the "+
			"buffer DID hold must still be written", len(kept), anomalyBuffer)
	}
	if n := f.rig.anom.takeDropped(); n != 0 {
		t.Fatalf("the drop counter still reads %d after being reported; the "+
			"next report would count these a second time", n)
	}
}
