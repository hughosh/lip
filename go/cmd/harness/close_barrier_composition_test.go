package main

import (
	"context"
	"database/sql"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"lip/harness/rest"
	"lip/harness/risk"
)

// The exchange seam deliberately ignores the canceled request context after
// entering this callback. A real transport can finish just as cancellation is
// delivered, and its balance row still belongs to the running store writer.
type heldBalanceCallback struct {
	armed   atomic.Bool
	claimed atomic.Bool
	calls   atomic.Int32
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func newHeldBalanceCallback() *heldBalanceCallback {
	return &heldBalanceCallback{
		entered: make(chan struct{}), release: make(chan struct{}),
	}
}

func (b *heldBalanceCallback) beforeDo(req rest.Request) {
	if req.Method != "GET" || req.Path != "/portfolio/balance" || !b.armed.Load() {
		return
	}
	b.calls.Add(1)
	if b.claimed.CompareAndSwap(false, true) {
		close(b.entered)
		<-b.release
	}
}

func (b *heldBalanceCallback) unblock() { b.once.Do(func() { close(b.release) }) }

func heldBalanceRig(t *testing.T) (*seamHarness, *servingCloseStepper, *shutdown, *heldBalanceCallback, *atomic.Int32) {
	t.Helper()
	h := newSeamHarness(t, seamOptions{ReadOnly: true})
	// Change only the fixture's telemetry cadence. Startup keeps the ordinary
	// balance read; the callback is armed once startup truth is actionable.
	h.rig.cfg.Params.BalancePoll = 25 * time.Millisecond
	monitorSamples := new(atomic.Int32)
	emit := h.rig.mon.emit
	h.rig.mon.emit = func(now time.Duration, result risk.StepResult) {
		monitorSamples.Add(1)
		emit(now, result)
	}
	b := newHeldBalanceCallback()
	h.ex.beforeDo = b.beforeDo
	stepper := &servingCloseStepper{}
	sd := startServingClose(t, h, stepper)
	t.Cleanup(b.unblock) // LIFO: release the callback before stopServe.
	h.awaitActionable()
	b.armed.Store(true)
	h.await("post-startup balance callback", func() bool {
		select {
		case <-b.entered:
			return true
		default:
			return false
		}
	})
	h.clk.Advance(time.Second)
	// The held request reads this distinct valid balance only after release.
	h.ex.mu.Lock()
	h.ex.balanceCents = 123_456
	h.ex.mu.Unlock()
	return h, stepper, sd, b, monitorSamples
}

func servingCloseBalanceValueCount(t *testing.T, path string, cents int64) int {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(0)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var count int
	if err := db.QueryRow(`SELECT count(*) FROM balance_poll WHERE balance_cents=?`, cents).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func waitCloseResult(t *testing.T, ch <-chan error, timeout time.Duration) error {
	t.Helper()
	select {
	case err := <-ch:
		return err
	case <-time.After(timeout):
		t.Fatalf("serving close did not return within %v", timeout)
		return nil
	}
}

func TestServingCloseJoinsCanceledBalanceCallbackBeforeStoreClose(t *testing.T) {
	h, _, sd, held, _ := heldBalanceRig(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	closed := make(chan error, 1)
	go func() { closed <- sd.stop(ctx) }()

	// The callback is still inside exchange.Do. The final close cannot report
	// success or close the writer until that producer has submitted its row.
	select {
	case err := <-closed:
		t.Fatalf("close returned with balance callback still active: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	held.unblock()
	if err := waitCloseResult(t, closed, 5*time.Second); err != nil {
		t.Fatalf("serving close after balance callback: %v", err)
	}
	if _, err := h.rig.store.Reader().BalancePolls(); err == nil {
		t.Fatal("store reader remained open after successful close")
	}
	if got := servingCloseBalanceValueCount(t, h.cfg.Paths.DB, 123_456); got != 1 {
		t.Fatalf("balance callback rows persisted after close = %d, want 1", got)
	}
}

func TestServingCloseTimeoutKeepsOwnerAndObserversUntilBalanceCallbackJoins(t *testing.T) {
	h, stepper, sd, held, monitorSamples := heldBalanceRig(t)
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	started := time.Now()
	err := sd.stop(ctx)
	if err == nil || !strings.Contains(err.Error(), "joining runtime producers") {
		t.Fatalf("close with held callback = %v, want producer-join refusal", err)
	}
	if elapsed := time.Since(started); elapsed > finalCloseTimeout+time.Second {
		t.Fatalf("close refusal took %v, want prompt %v budget", elapsed, finalCloseTimeout)
	}
	if _, err := seamNewRig(t, h.ctx, h.cfg, false, h.xch); err == nil || !strings.Contains(err.Error(), "lock") {
		t.Fatalf("refused close released instance ownership: %v", err)
	}
	seq, steps, samples := h.snapSeq(), stepper.calls.Load(), monitorSamples.Load()
	h.await("owner, monitor, and alerts after refused close", func() bool {
		return h.snapSeq() > seq && stepper.calls.Load() > steps && monitorSamples.Load() > samples
	})
	if got := held.calls.Load(); got != 1 {
		t.Fatalf("replacement balance callback entered before prior callback joined: %d calls", got)
	}
	held.unblock()
	h.await("held balance row to be accepted by the original store", func() bool {
		return servingCloseBalanceValueCount(t, h.cfg.Paths.DB, 123_456) == 1
	})
	retryCtx, retryCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer retryCancel()
	if err := sd.stop(retryCtx); err != nil {
		t.Fatalf("retrying close after balance callback joined: %v", err)
	}
	if got := servingCloseBalanceValueCount(t, h.cfg.Paths.DB, 123_456); got != 1 {
		t.Fatalf("accepted balance rows after retry = %d, want 1", got)
	}
}
