package main

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"lip/harness/ping"
	"lip/harness/risk"
)

// These tests use serveWithShutdown, rather than calling shutdown.stopStore
// directly. The owner must join every serving producer before the store closes.
type servingCloseStepper struct{ calls atomic.Int32 }

func (s *servingCloseStepper) Step(_ context.Context, nowMs int64, _ ping.Heartbeat) ping.Effects {
	s.calls.Add(1)
	return ping.Effects{NextStepMs: nowMs + int64(time.Hour/time.Millisecond)}
}

func startServingClose(t *testing.T, h *seamHarness, stepper *servingCloseStepper) *shutdown {
	t.Helper()
	h.rig.alerts = stepper
	ctx, cancel := context.WithCancel(h.ctx)
	h.serveCancel = cancel
	h.serveDone = make(chan struct{})
	h.serveSignals = make(chan os.Signal, 1)
	h.serveExits = make(chan int, 1)
	sd := newShutdown(h.rig)
	sd.exit = func(code int) { h.serveExits <- code; cancel() }
	go func() {
		h.serveErr = h.rig.serveWithShutdown(ctx, sd, h.serveSignals)
		close(h.serveDone)
	}()
	t.Cleanup(h.stopServe)
	return sd
}

func servingCloseBalanceCount(t *testing.T, path string, tsMs int64) int {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(0)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var count int
	if err := db.QueryRow(`SELECT count(*) FROM balance_poll WHERE ts_ms=?`, tsMs).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func servingCloseAnomalyCount(t *testing.T, path, class string) int {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(0)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var count int
	if err := db.QueryRow(`SELECT count(*) FROM anomaly WHERE class=?`, class).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func TestServingClosePersistsAcceptedRecordsBeforeWriterStops(t *testing.T) {
	h := newSeamHarness(t, seamOptions{ReadOnly: true})
	stepper := &servingCloseStepper{}
	sd := startServingClose(t, h, stepper)
	h.awaitActionable()
	const tsMs = int64(1_700_000_060_123)
	if _, err := h.rig.store.RecordBalancePoll(h.rig.run, tsMs, 10_123); err != nil {
		t.Fatalf("submitting accepted balance observation: %v", err)
	}
	h.rig.anom.raise(risk.Anomaly{Class: "SERVING_CLOSE_TEST", Sev: risk.SEV1,
		Text: "accepted just before planned close"})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := sd.stop(ctx); err != nil {
		t.Fatalf("serving orderly close: %v", err)
	}
	if got := servingCloseBalanceCount(t, h.cfg.Paths.DB, tsMs); got != 1 {
		t.Fatalf("accepted balance records persisted after close = %d, want 1", got)
	}
	if got := servingCloseAnomalyCount(t, h.cfg.Paths.DB, "SERVING_CLOSE_TEST"); got != 1 {
		t.Fatalf("accepted anomaly records persisted after close = %d, want 1", got)
	}
	for _, class := range []string{"STORE_UNHEALTHY", "ANOMALY_SUBMISSION_FAILED"} {
		if got := servingCloseAnomalyCount(t, h.cfg.Paths.DB, class); got != 0 {
			t.Fatalf("authorized close wrote %d %s anomaly records", got, class)
		}
	}
	if got := stepper.calls.Load(); got < 2 {
		t.Fatalf("alert steps = %d, want immediate and final", got)
	}
	if h.rig.store.Health().AllowsAdding() {
		t.Fatal("authorized close left store writer able to accept risk")
	}
	select {
	case a := <-h.rig.anom.ch:
		t.Fatalf("post-close anomaly was raised: %+v", a)
	default:
	}
}

func TestServingCloseRefusalRestoresObservationAndRetries(t *testing.T) {
	h := newSeamHarness(t, seamOptions{ReadOnly: true})
	stepper := &servingCloseStepper{}
	sd := startServingClose(t, h, stepper)
	h.awaitActionable()
	contender, err := sql.Open("sqlite", "file:"+h.cfg.Paths.DB+"?_pragma=busy_timeout(0)")
	if err != nil {
		t.Fatal(err)
	}
	defer contender.Close()
	conn, err := contender.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(context.Background(), "BEGIN IMMEDIATE"); err != nil {
		t.Fatalf("holding private SQLite writer lock: %v", err)
	}
	locked := true
	defer func() {
		if locked {
			_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
		}
	}()
	const tsMs = int64(1_700_000_060_124)
	if _, err := h.rig.store.RecordBalancePoll(h.rig.run, tsMs, 10_124); err != nil {
		t.Fatalf("submitting contended balance observation: %v", err)
	}
	h.await("real writer contention", func() bool { return h.rig.store.Health().Failures() > 0 })
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	if err := sd.stop(ctx); err == nil {
		t.Fatal("store close succeeded while an accepted record was blocked")
	}
	if h.rig.store.Health().Pending() == 0 {
		t.Fatal("refused close lost the accepted pending record")
	}
	if _, err := seamNewRig(t, h.ctx, h.cfg, false, h.xch); err == nil || !strings.Contains(err.Error(), "lock") {
		t.Fatalf("refused close released instance ownership: %v", err)
	}
	seq := h.snapSeq()
	steps := stepper.calls.Load()
	walks := h.ex.ordersWalkCount()
	last := h.rig.last.Load()
	h.clk.Advance(5 * time.Second)
	h.await("owner, account observation, monitor and alerts to resume after refusal", func() bool {
		return h.snapSeq() > seq && stepper.calls.Load() > steps && h.ex.ordersWalkCount() > walks && h.rig.last.Load() != last
	})
	if _, err := conn.ExecContext(context.Background(), "ROLLBACK"); err != nil {
		t.Fatalf("releasing private SQLite writer lock: %v", err)
	}
	locked = false
	h.await("accepted record to retry after lock release", func() bool {
		return h.rig.store.Health().Pending() == 0
	})
	retryCtx, retryCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer retryCancel()
	if err := sd.stop(retryCtx); err != nil {
		t.Fatalf("retrying orderly close after writer recovery: %v", err)
	}
	if got := servingCloseBalanceCount(t, h.cfg.Paths.DB, tsMs); got != 1 {
		t.Fatalf("retried accepted balance records persisted = %d, want 1", got)
	}
}

func TestServingCloseUnexpectedWriterLossStillStopsAndObserves(t *testing.T) {
	h := newSeamHarness(t, seamOptions{ReadOnly: true})
	stepper := &servingCloseStepper{}
	startServingClose(t, h, stepper)
	h.awaitActionable()
	h.rig.storeCancel()
	select {
	case <-h.rig.storeDone:
	case <-time.After(3 * time.Second):
		t.Fatal("unexpected writer exit did not complete")
	}
	h.await("owner to durably stop after writer loss", func() bool {
		return h.latchTrigger() == "store_unhealthy"
	})
	seq := h.snapSeq()
	steps := stepper.calls.Load()
	h.await("observation and alerting after writer loss", func() bool {
		return h.snapSeq() > seq && stepper.calls.Load() > steps
	})
}

// lip-6w8: a close failure recorded after the writer stopped, or after the lock
// release failed, is permanent, and a retry returns it unchanged. The owner must
// refuse without tearing its inputs down again, and the drain reports once,
// saying whether the instance lock is still held, instead of every second.
func TestStickyCloseFailureIsReportedOnceWithoutRestartingInputs(t *testing.T) {
	for _, tc := range []struct {
		name     string
		released bool
		want     string
	}{
		{"writer stopped, lock held", false, "instance ownership is retained"},
		{"lock release failed", true, "instance lock is released"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newSeamHarness(t, seamOptions{ReadOnly: true})
			startServingClose(t, h, &servingCloseStepper{})
			h.awaitActionable()
			// hstore's final Close cannot fail after its drain on demand, and
			// InstanceLock hides its descriptor, so the recorded failure is set
			// directly. It is the state the retry rule keys on.
			h.rig.closeMu.Lock()
			h.rig.closeFailure = errors.New("injected close failure after the writer stopped")
			h.rig.lockReleased = tc.released
			h.rig.closeMu.Unlock()
			// Runs before the harness cleanups, so the real store still closes.
			t.Cleanup(func() {
				h.rig.closeMu.Lock()
				h.rig.closeFailure, h.rig.lockReleased = nil, false
				h.rig.closeMu.Unlock()
			})
			subscriptions := len(h.ws.subscriptions())
			h.serveSignals <- syscall.SIGTERM
			refusals := func() []string {
				rows, err := h.rig.store.Reader().PendingAnomalies()
				if err != nil {
					t.Fatalf("PendingAnomalies: %v", err)
				}
				var out []string
				for _, r := range rows {
					if r.Class == stopRefusedClass {
						out = append(out, r.Text)
					}
				}
				return out
			}
			h.await("the sticky refusal", func() bool { return len(refusals()) > 0 })
			time.Sleep(2500 * time.Millisecond) // the drain retried every second
			got := refusals()
			if len(got) != 1 {
				t.Fatalf("%d ORDERLY_STOP_REFUSED rows, want one report of a permanent refusal", len(got))
			}
			if !strings.Contains(got[0], tc.want) {
				t.Fatalf("refusal text %q does not say %q", got[0], tc.want)
			}
			if n := len(h.ws.subscriptions()); n != subscriptions {
				t.Fatalf("websocket subscriptions %d -> %d: the owner tore its inputs down "+
					"and reconnected for a refusal it could not change", subscriptions, n)
			}
			select {
			case code := <-h.serveExits:
				t.Fatalf("serve exited %d after a refused orderly stop", code)
			default:
			}
		})
	}
}
