package main

import (
	"context"
	"errors"
	"flag"
	"io"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"lip/harness/hstore"
	"lip/harness/ping"
	"lip/harness/qual"
	"lip/harness/quote"
	"lip/harness/rest"
	"lip/harness/risk"
)

type quietTestAlertStepper struct{}

func (quietTestAlertStepper) Step(_ context.Context, nowMs int64,
	_ ping.Heartbeat) ping.Effects {
	return ping.Effects{NextStepMs: nowMs + int64(time.Hour/time.Millisecond)}
}

type scriptedAlertStepper struct {
	mu      sync.Mutex
	effects []ping.Effects
	calls   int
	active  int
	max     int
	hearts  []ping.Heartbeat
	reader  *hstore.Reader
	reads   []error
	notify  chan struct{}
}

type blockingAlertStepper struct {
	mu      sync.Mutex
	reader  *hstore.Reader
	started chan struct{}
	release chan struct{}
	calls   int
	active  int
	max     int
	reads   []error
}

// hangAfterFailureStepper is F20 at the component boundary: one observable
// failed delivery followed by a call which never returns until the test tears
// it down.  It deliberately ignores ctx while blocked, because the property
// under test is that a transport which fails to honour cancellation still
// cannot acquire the owner or monitor control path.
type hangAfterFailureStepper struct {
	mu      sync.Mutex
	calls   int
	blocked chan struct{}
	release chan struct{}
}

func (s *hangAfterFailureStepper) Step(_ context.Context, nowMs int64,
	_ ping.Heartbeat) ping.Effects {
	s.mu.Lock()
	s.calls++
	call := s.calls
	s.mu.Unlock()
	if call == 1 {
		return ping.Effects{
			Pushes: []ping.Push{{Kind: ping.PushHeartbeat,
				Err: errors.New("injected alert delivery failure")}},
			DeadmanErr: errors.New("injected dead-man failure"),
			NextStepMs: nowMs,
		}
	}
	if call == 2 {
		close(s.blocked)
		<-s.release
	}
	return ping.Effects{NextStepMs: nowMs + int64(time.Hour/time.Millisecond)}
}

func (s *blockingAlertStepper) Step(_ context.Context, nowMs int64,
	_ ping.Heartbeat) ping.Effects {
	s.mu.Lock()
	s.calls++
	call := s.calls
	s.active++
	if s.active > s.max {
		s.max = s.active
	}
	s.mu.Unlock()
	if call == 1 {
		close(s.started)
		<-s.release
	}
	_, err := s.reader.UndeliveredCount()
	s.mu.Lock()
	s.reads = append(s.reads, err)
	s.active--
	s.mu.Unlock()
	return ping.Effects{NextStepMs: nowMs + int64(time.Hour/time.Millisecond)}
}

func (s *blockingAlertStepper) snapshot() (calls, max int, reads []error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls, s.max, append([]error(nil), s.reads...)
}

func (s *scriptedAlertStepper) Step(_ context.Context, nowMs int64,
	hb ping.Heartbeat) ping.Effects {
	s.mu.Lock()
	s.active++
	if s.active > s.max {
		s.max = s.active
	}
	s.calls++
	s.hearts = append(s.hearts, hb)
	var eff ping.Effects
	if len(s.effects) > 0 {
		i := s.calls - 1
		if i >= len(s.effects) {
			i = len(s.effects) - 1
		}
		eff = s.effects[i]
	} else {
		eff.NextStepMs = nowMs + 1
	}
	if s.reader != nil {
		_, err := s.reader.UndeliveredCount()
		s.reads = append(s.reads, err)
	}
	s.active--
	s.mu.Unlock()
	if s.notify != nil {
		select {
		case s.notify <- struct{}{}:
		default:
		}
	}
	return eff
}

func (s *scriptedAlertStepper) snapshot() (calls, max int,
	hearts []ping.Heartbeat, reads []error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls, s.max, append([]ping.Heartbeat(nil), s.hearts...),
		append([]error(nil), s.reads...)
}

func waitAlertCalls(t *testing.T, s *scriptedAlertStepper, want int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		calls, _, _, _ := s.snapshot()
		if calls >= want {
			return
		}
		select {
		case <-s.notify:
		case <-time.After(time.Millisecond):
		}
	}
	calls, _, _, _ := s.snapshot()
	t.Fatalf("alert Step called %d time(s), want at least %d", calls, want)
}

func newBareAlertRig(stepper alertStepper) *rig {
	return &rig{
		cfg: config{},
		ex: exchange{
			NowMs: func() int64 { return time.Now().UnixMilli() },
			Mono:  func() time.Duration { return 0 },
		},
		snap:       new(atomic.Pointer[risk.Snapshot]),
		alerts:     stepper,
		alertWake:  make(chan struct{}, 1),
		alertFlush: make(chan chan struct{}),
	}
}

func TestAlertFactoryAndStepperAreRequired(t *testing.T) {
	h := newSeamHarness(t, seamOptions{SkipRig: true})
	for _, tc := range []struct {
		name    string
		factory alertFactory
		want    string
	}{
		{name: "factory", factory: nil, want: "alert factory"},
		{name: "stepper", factory: func(*hstore.Reader, *hstore.Store,
			time.Duration) (alertStepper, error) {
			return nil, nil
		}, want: "returned no service"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := newRig(context.Background(), h.cfg, false, h.xch,
				tc.factory, newAnomalySink(), nil)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("newRig error = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestRunAndDeployValidateAlertsBeforeCredentialsOrAccountAccess(t *testing.T) {
	configPath := writeConfig(t, `{"ticker":"KXTEST-A","rung":"canary","s":1,`+
		goodTail(t)+`}`)
	for _, tc := range []struct {
		name   string
		deploy bool
	}{
		{name: "run"},
		{name: "deploy", deploy: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fs := flag.NewFlagSet("harness", flag.ContinueOnError)
			fs.SetOutput(io.Discard)
			err := run(fs, configPath, "", "", "", "", false, tc.deploy, false, false)
			var ref *refusal
			if !errors.As(err, &ref) || !strings.Contains(err.Error(), "alert topic") {
				t.Fatalf("run error = %v, want alert-topic refusal before the missing key, store, or any exchange request", err)
			}
		})
	}
}

func TestAlertLoopStartsImmediatelyAndContinuesAfterFailures(t *testing.T) {
	secret := "must-not-appear"
	stepper := &scriptedAlertStepper{
		notify: make(chan struct{}, 8),
		effects: []ping.Effects{
			{Err: errors.New(secret), DeadmanErr: errors.New(secret),
				Pushes:     []ping.Push{{Kind: ping.PushHeartbeat, Err: errors.New(secret)}},
				NextStepMs: time.Now().Add(time.Hour).UnixMilli()},
			{NextStepMs: time.Now().Add(time.Hour).UnixMilli()},
		},
	}
	r := newBareAlertRig(stepper)
	if err := r.startAlerts(context.Background()); err != nil {
		t.Fatalf("startAlerts: %v", err)
	}
	waitAlertCalls(t, stepper, 1)
	r.wakeAlerts()
	waitAlertCalls(t, stepper, 2)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := r.stopAlerts(ctx); err != nil {
		t.Fatalf("stopAlerts: %v", err)
	}
}

func TestServeStartsAlertLoopBeforeStartupCanReturn(t *testing.T) {
	h := newSeamHarness(t, seamOptions{SkipRig: true})
	startupEntered := make(chan struct{})
	startupRelease := make(chan struct{})
	var enterOnce, releaseOnce sync.Once
	h.ex.beforeDo = func(rest.Request) {
		enterOnce.Do(func() { close(startupEntered) })
		<-startupRelease
	}
	t.Cleanup(func() { releaseOnce.Do(func() { close(startupRelease) }) })

	stepper := &scriptedAlertStepper{
		notify: make(chan struct{}, 2),
		effects: []ping.Effects{{
			NextStepMs: time.Now().Add(time.Hour).UnixMilli(),
		}},
	}
	factory := func(*hstore.Reader, *hstore.Store,
		time.Duration) (alertStepper, error) {
		return stepper, nil
	}
	h.anom = newAnomalySink()
	r, err := newRig(context.Background(), h.cfg, false, h.xch,
		factory, h.anom, nil)
	if err != nil {
		t.Fatalf("newRig: %v", err)
	}
	h.rig = r
	t.Cleanup(h.closeRig)
	h.start()
	select {
	case <-startupEntered:
	case <-time.After(time.Second):
		t.Fatal("startup made no exchange request for the ordering fixture to hold")
	}
	// Startup is still blocked above. Removing startAlerts from serve, or moving
	// it below startup, therefore leaves this Step unreachable.
	waitAlertCalls(t, stepper, 1)
	releaseOnce.Do(func() { close(startupRelease) })
}

func TestAlertWakeRacingTimerExpiryDoesNotHang(t *testing.T) {
	if got := alertDelay(100, 0); got != time.Second {
		t.Fatalf("a missing deadline polls after %v, want 1s (no busy loop)", got)
	}
	stepper := &scriptedAlertStepper{notify: make(chan struct{}, 1)}
	r := newBareAlertRig(stepper)
	if err := r.startAlerts(context.Background()); err != nil {
		t.Fatalf("startAlerts: %v", err)
	}
	waitAlertCalls(t, stepper, 1)
	for i := 0; i < 500; i++ {
		r.wakeAlerts()
		if i%5 == 0 {
			time.Sleep(200 * time.Microsecond)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := r.stopAlerts(ctx); err != nil {
		t.Fatalf("wake/expiry race wedged the alert loop: %v", err)
	}
}

func TestStopAlertTimerReturnsAfterExpiryWasAlreadyReceived(t *testing.T) {
	timer := time.NewTimer(time.Millisecond)
	<-timer.C
	done := make(chan struct{})
	go func() {
		stopAlertTimer(timer)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("stopAlertTimer blocked draining a timer value the receiver had already consumed")
	}
}

// TestAlertHangCannotBlockOwnerReductionOrMonitoring is lip-30p's composed F20
// catcher.  No ping component assertion can establish this: the observable is
// a real owner consuming a real poll, publishing, dispatching a reducer through
// the real store barrier, and being sampled by the real monitor while Step is
// stuck on a different goroutine.
func TestAlertHangCannotBlockOwnerReductionOrMonitoring(t *testing.T) {
	h := newSeamHarness(t, seamOptions{
		Latched: true, Resume: true, SkipRig: true, Rung: "pilot",
	})
	stepper := &hangAfterFailureStepper{
		blocked: make(chan struct{}), release: make(chan struct{}),
	}
	factory := func(*hstore.Reader, *hstore.Store,
		time.Duration) (alertStepper, error) {
		return stepper, nil
	}
	h.anom = newAnomalySink()
	r, err := newRig(context.Background(), h.cfg, true, h.xch,
		factory, h.anom, nil)
	if err != nil {
		t.Fatalf("newRig: %v", err)
	}
	h.rig = r
	t.Cleanup(h.closeRig)
	h.start()
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(stepper.release) }) })

	select {
	case <-stepper.blocked:
	case <-time.After(2 * time.Second):
		t.Fatal("the injected alert service never reached its hanging second Step")
	}

	// Everything below happens while Step is blocked.  The resumed latch keeps
	// adding authority off, so a newly observed +4 YES position has exactly one
	// legitimate action: a four-contract NO reducer.
	h.awaitActionable()
	beforePosition := h.snapSeq()
	h.ex.setPosition(seamTicker, "4.00")
	h.clk.Advance(h.cfg.Params.PositionPoll)
	h.await("the fresh +4 poll to publish a reducing market", func() bool {
		m, ok := h.market()
		return ok && m.Q.Wire() == "4.00" && m.State == quote.Reducing
	})
	reducerSeq := h.snapSeq()
	if reducerSeq <= beforePosition {
		t.Fatalf("reducer publication seq=%d, want later than pre-poll seq=%d",
			reducerSeq, beforePosition)
	}
	h.await("the four-contract reducer to reach the exchange", func() bool {
		return h.ex.createCount() >= 1
	})
	create, ok := h.ex.createAt(0)
	if !ok || create.Count != "4.00" || create.WireSide != string(rest.Ask) {
		t.Fatalf("reducing create = %+v (present=%v), want a 4.00 NO bid "+
			"represented as a YES-denominated ask", create, ok)
	}

	h.await("owner publications to continue after the reducer", func() bool {
		return h.snapSeq() > reducerSeq
	})
	h.await("a fresh monitor result derived from the reducer publication", func() bool {
		last := h.rig.last.Load()
		if last == nil || last.Stale {
			return false
		}
		for _, sample := range last.Samples {
			if sample.Ticker == seamTicker && sample.SourceSeq >= reducerSeq {
				return true
			}
		}
		return false
	})
	last := h.rig.last.Load()
	if last == nil || last.Stale || len(last.Samples) == 0 ||
		last.Samples[0].SourceSeq < reducerSeq {
		t.Fatalf("monitor result = %+v, want fresh SourceSeq >= %d", last, reducerSeq)
	}
	select {
	case <-h.serveDone:
		t.Fatalf("serve returned while alert Step was hung: %v", h.serveErr)
	default:
	}
}

func TestOnlyACommittedAnomalyWakesAlertDelivery(t *testing.T) {
	f := newFixture(t, nil)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := f.rig.stopAlerts(ctx); err != nil {
		t.Fatalf("stopping fixture alert loop: %v", err)
	}
	f.rig.alertWake = make(chan struct{}, 1)

	f.sd.handleResults([]hstore.Result{{Kind: hstore.KindAnomaly,
		Err: errors.New("terminal write failure")}})
	select {
	case <-f.rig.alertWake:
		t.Fatal("a failed anomaly record woke delivery before it was durable")
	default:
	}
	f.flush()
	f.sd.handleResults(f.rig.store.TakeResults())
	select {
	case <-f.rig.alertWake:
	case <-time.After(time.Second):
		t.Fatal("the committed rejection anomaly did not wake alert delivery")
	}
}

func TestHeartbeatUsesPublishedAndDurableTruth(t *testing.T) {
	f := newFixture(t, nil)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := f.rig.stopAlerts(ctx); err != nil {
		t.Fatalf("stopping fixture alert loop: %v", err)
	}
	f.mono = 37 * time.Minute
	f.rig.snap = new(atomic.Pointer[risk.Snapshot])
	f.rig.snap.Store(&risk.Snapshot{
		Global: quote.Running,
		Markets: []risk.MarketSnap{{
			Ticker: "KXTEST-HEART", State: quote.Skewed,
			Q: f.rig.cfg.Params.S,
		}},
	})
	f.rig.last.Store(&risk.StepResult{Stale: true})
	if _, err := f.rig.store.RecordAnomaly(f.rig.run, "heartbeat-pending",
		risk.Anomaly{Class: "TEST", Sev: risk.SEV2, Text: "heartbeat queue test"}, f.now()); err != nil {
		t.Fatalf("RecordAnomaly: %v", err)
	}
	f.flush()

	hb := f.rig.heartbeat()
	if hb.Global != quote.Running || len(hb.Markets) != 1 ||
		hb.Markets[0].Ticker != "KXTEST-HEART" ||
		!hb.Markets[0].Q.Known || hb.Markets[0].Q.V != f.rig.cfg.Params.S {
		t.Fatalf("heartbeat state/market = %+v", hb)
	}
	if !hb.Capital.Known || hb.Capital.V != f.rig.cfg.Params.CapitalMax ||
		!hb.Uptime.Known || hb.Uptime.V != f.mono ||
		!hb.SourceStale.Known || !hb.SourceStale.V ||
		!hb.Undelivered.Known || hb.Undelivered.V != 1 {
		t.Fatalf("heartbeat metrics = %+v", hb)
	}
	if hb.Integrated.Known {
		t.Fatalf("integrated presence = %+v, want unknown until an authoritative accumulator exists", hb.Integrated)
	}
}

func TestImmediateAlertStepSeesPendingRowsFromThePreviousProcess(t *testing.T) {
	h := newSeamHarness(t, seamOptions{SkipRig: true})
	r1, err := newRig(context.Background(), h.cfg, false, h.xch,
		seamAlertFactory, newAnomalySink(), nil)
	if err != nil {
		t.Fatalf("first newRig: %v", err)
	}
	if _, err := r1.store.RecordAnomaly(r1.run, "previous-process",
		risk.Anomaly{Class: "RESTART_TEST", Sev: risk.SEV1,
			Text: "pending alert survives restart"}, h.xch.NowMs()); err != nil {
		t.Fatalf("RecordAnomaly: %v", err)
	}
	if err := r1.store.Drain(context.Background()); err != nil {
		t.Fatalf("draining first process: %v", err)
	}
	if err := r1.close(context.Background()); err != nil {
		t.Fatalf("closing first process: %v", err)
	}

	stepper := &scriptedAlertStepper{
		notify: make(chan struct{}, 2),
		effects: []ping.Effects{{
			Pushes: []ping.Push{{Kind: ping.PushAnomaly,
				Err: errors.New("delivery unavailable")}},
			NextStepMs: time.Now().Add(time.Hour).UnixMilli(),
		}},
	}
	factory := func(reader *hstore.Reader, _ *hstore.Store,
		_ time.Duration) (alertStepper, error) {
		stepper.reader = reader
		return stepper, nil
	}
	r2, err := newRig(context.Background(), h.cfg, false, h.xch,
		factory, newAnomalySink(), nil)
	if err != nil {
		t.Fatalf("second newRig: %v", err)
	}
	if err := r2.startAlerts(context.Background()); err != nil {
		t.Fatalf("startAlerts: %v", err)
	}
	waitAlertCalls(t, stepper, 1)
	_, _, _, reads := stepper.snapshot()
	if len(reads) == 0 || reads[0] != nil {
		t.Fatalf("the restart Step could not read the durable pending queue: %v", reads)
	}
	rows, err := r2.store.Reader().PendingAnomalies()
	if err != nil || len(rows) != 1 || rows[0].AnomalyID != "previous-process" {
		t.Fatalf("pending after restart = %+v, err=%v", rows, err)
	}
	if err := r2.close(context.Background()); err != nil {
		t.Fatalf("closing second process: %v", err)
	}
}

func TestSuccessfulHeartbeatAndDeadmanAreQualificationEvents(t *testing.T) {
	qrec, err := qual.Open(t.TempDir()+"/qualification.json", qual.Metadata{
		SchemaVersion: qual.SchemaVersion, ConfigHash: "sha256:alerts",
		BinaryIdentity: "sha256:binary", Ticker: "KXTEST-A", Rung: "canary",
		Live: false,
	}, qual.SegmentStart{ID: "alerts", PID: os.Getpid(), StartedAt: time.Now().UTC()})
	if err != nil {
		t.Fatalf("qual.Open: %v", err)
	}
	r := &rig{qual: qrec, qualErrors: make(chan error, 1)}
	r.observeAlertEffects(ping.Effects{
		Pushes: []ping.Push{{Kind: ping.PushHeartbeat}}, Deadman: true,
	})
	counts := map[string]uint64{}
	for _, event := range qrec.Snapshot().Events {
		if event.Category == qual.EventHeartbeat {
			counts[event.Name] = event.Count
		}
	}
	if counts["sent"] != 1 || counts["deadman_checkin_sent"] != 1 {
		t.Fatalf("qualification heartbeat events = %v", counts)
	}
}

func TestAlertFailureLoggingCannotRevealTransportSecrets(t *testing.T) {
	old := os.Stderr
	read, write, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	os.Stderr = write
	t.Cleanup(func() { os.Stderr = old })
	secret := "https://watch.example/opaque-secret"
	(&rig{}).observeAlertEffects(ping.Effects{
		Err: errors.New(secret), DeadmanErr: errors.New(secret),
		Pushes: []ping.Push{{Kind: ping.PushAnomaly, Err: errors.New(secret)}},
	})
	if err := write.Close(); err != nil {
		t.Fatalf("closing stderr writer: %v", err)
	}
	os.Stderr = old
	b, err := io.ReadAll(read)
	if err != nil {
		t.Fatalf("reading stderr: %v", err)
	}
	if strings.Contains(string(b), secret) {
		t.Fatalf("alert failure log revealed the endpoint: %q", b)
	}
	for _, want := range []string{"queue could not be read", "delivery failed", "check-in failed"} {
		if !strings.Contains(string(b), want) {
			t.Fatalf("alert failure log %q does not name %q", b, want)
		}
	}
}

func TestOrderlyStopRunsOneSerializedFinalStepBeforeClosingTheStore(t *testing.T) {
	h := newSeamHarness(t, seamOptions{SkipRig: true})
	stepper := &blockingAlertStepper{
		started: make(chan struct{}), release: make(chan struct{}),
	}
	factory := func(reader *hstore.Reader, _ *hstore.Store,
		_ time.Duration) (alertStepper, error) {
		stepper.reader = reader
		return stepper, nil
	}
	r, err := newRig(context.Background(), h.cfg, false, h.xch,
		factory, newAnomalySink(), nil)
	if err != nil {
		t.Fatalf("newRig: %v", err)
	}
	if err := r.startAlerts(context.Background()); err != nil {
		t.Fatalf("startAlerts: %v", err)
	}
	select {
	case <-stepper.started:
	case <-time.After(time.Second):
		t.Fatal("immediate Step did not start")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stopDone := make(chan error, 1)
	go func() { stopDone <- newShutdown(r).stop(ctx) }()
	// The first Step is still active. A final Step called directly by shutdown
	// would now overlap it; sending the command to the loop's sole owner cannot.
	time.Sleep(25 * time.Millisecond)
	calls, max, _ := stepper.snapshot()
	if calls != 1 || max != 1 {
		close(stepper.release)
		t.Fatalf("shutdown ran Step concurrently: calls=%d max-active=%d", calls, max)
	}
	close(stepper.release)
	if err := <-stopDone; err != nil {
		t.Fatalf("orderly stop: %v", err)
	}
	calls, max, reads := stepper.snapshot()
	if calls < 2 {
		t.Fatalf("Step called %d time(s), want immediate plus final", calls)
	}
	if max != 1 {
		t.Fatalf("%d alert Steps overlapped; retry state has more than one owner", max)
	}
	for i, err := range reads {
		if err != nil {
			t.Fatalf("Step %d read the store after close: %v", i+1, err)
		}
	}
	select {
	case <-r.alertDone:
	default:
		t.Fatal("orderly stop closed the store before the alert loop stopped")
	}
	if _, err := r.store.Reader().UndeliveredCount(); err == nil {
		t.Fatal("store reader remains usable after orderly stop; close-order assertion is inconclusive")
	}
}

// confidence: high
