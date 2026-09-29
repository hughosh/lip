package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"lip/harness/num"
	"lip/harness/quote"
)

func TestRuntimeStoreFailureAndStallStopWithOwnedInventory(t *testing.T) {
	for _, tc := range []struct {
		name   string
		sample storeHealthSnapshot
	}{
		{"failed_write_recovered_between_ticks", storeHealthSnapshot{healthy: true, adding: true, failures: 1, lastError: "injected write failure"}},
		{"stalled_writer", storeHealthSnapshot{stalled: true, pending: 1, lastError: "injected stalled writer"}},
		{"stall_recovered_between_ticks", storeHealthSnapshot{healthy: true, adding: true, stalls: 1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := newGateFailOwner(t, num.QtyFromFloat(1))
			o, h := g.o, g.h
			driftRestingOrders(t, g)
			h.ex.setPosition(seamTicker, "1.00")
			reducerConfirmationPoll(t, g)
			if o.global != quote.Running || o.market != quote.Quoting {
				t.Fatalf("control global=%s market=%s", o.global, o.market)
			}
			o.storeHealth.probe = func() storeHealthSnapshot {
				return tc.sample
			}
			now := h.clk.Now().Mono
			o.evaluate(now)
			if got := h.latchTrigger(); got != "store_unhealthy" {
				t.Fatalf("durable stop cause=%q", got)
			}
			if o.global != quote.WindingDown || o.market != quote.Reducing {
				t.Fatalf("store failure global=%s market=%s", o.global, o.market)
			}
			driftAssertReducer(t, g)
			walks := h.ex.ordersWalkCount()
			driftPollAgain(t, g)
			if h.ex.ordersWalkCount() <= walks {
				t.Fatal("store stop interrupted account observation")
			}
			o.publish(now)
			if snap := h.snapshot(); snap == nil || snap.Global.AddsRisk() || snap.Markets[0].State != quote.Reducing {
				t.Fatalf("published state lost store stop: %+v", snap)
			}
		})
	}
}

func TestRuntimeStoreStopHeldUntilLatchWriteRecovers(t *testing.T) {
	h := newSeamHarness(t, seamOptions{LatchDirMissing: true})
	o := h.ownerFor()
	o.storeHealth.probe = func() storeHealthSnapshot {
		return storeHealthSnapshot{lastError: "injected writer failure"}
	}
	o.evaluate(0)
	if !o.stopHeld || o.global != quote.Running || o.market == quote.Quoting || o.stopCause.Trigger != "store_unhealthy" {
		t.Fatalf("failed latch: held=%t cause=%q global=%s market=%s", o.stopHeld, o.stopCause.Trigger, o.global, o.market)
	}
	for i := 0; i < 2; i++ {
		o.evaluate(0)
	}
	if o.stopRetries < 2 || !o.stopHeld {
		t.Fatalf("failed latch retry count=%d held=%t", o.stopRetries, o.stopHeld)
	}
	if err := os.Mkdir(filepath.Dir(h.cfg.Paths.Latch), 0o755); err != nil {
		t.Fatal(err)
	}
	o.evaluate(0)
	if o.stopHeld || o.global != quote.WindingDown || h.latchTrigger() != "store_unhealthy" {
		t.Fatalf("recovered latch: held=%t global=%s trigger=%q", o.stopHeld, o.global, h.latchTrigger())
	}
	if err := h.rig.close(context.Background()); err != nil {
		t.Fatal(err)
	}
	refused, err := seamNewRig(t, h.ctx, h.cfg, false, h.xch)
	if err == nil {
		if refused != nil {
			_ = refused.close(context.Background())
		}
		t.Fatal("restart crossed the durable store-failure latch")
	}
	if !strings.Contains(err.Error(), "-resume") {
		t.Fatalf("restart refusal did not cite the stop: %v", err)
	}
}

func TestRuntimeStoreObservesRealWriterExit(t *testing.T) {
	h := newSeamHarness(t, seamOptions{})
	o := h.ownerFor()
	h.rig.storeCancel()
	select {
	case <-h.rig.storeDone:
	case <-time.After(3 * time.Second):
		t.Fatal("test writer did not exit")
	}
	if h.rig.store.Health().AllowsAdding() {
		t.Fatal("writer exit did not revoke adding authority")
	}
	o.evaluate(0)
	if o.global != quote.WindingDown || h.latchTrigger() != "store_unhealthy" {
		t.Fatalf("writer exit global=%s trigger=%q", o.global, h.latchTrigger())
	}
}

// The owner keeps evaluating until the drain goroutine reaches os.Exit. Its
// next probe after a successful orderly close must not turn that close into a
// store failure; the actual stop path also drains the anomaly sink first.
func TestRuntimeStoreAuthorizedCloseDoesNotRaiseFailure(t *testing.T) {
	h := newSeamHarness(t, seamOptions{})
	o := h.ownerFor()
	if err := h.rig.startAlerts(context.Background()); err != nil {
		t.Fatalf("start alert loop: %v", err)
	}
	if err := o.sd.stopStore(context.Background()); err != nil {
		t.Fatalf("authorized orderly stop: %v", err)
	}
	if h.rig.store.Health().AllowsAdding() {
		t.Fatal("control: orderly stop left writer able to add")
	}
	o.checkRuntimeStore()
	if o.storeHealth.stopped {
		t.Fatal("authorized orderly close was classified as store failure")
	}
	select {
	case a := <-h.rig.anom.ch:
		t.Fatalf("authorized orderly close raised anomaly: %+v", a)
	default:
	}
}

func TestRuntimeStoreProbeBegunBeforeCloseCompletesBeforeWriterStops(t *testing.T) {
	f := newFixture(t, nil)
	o := newOwner(f.rig, f.sd)
	entered := make(chan struct{})
	release := make(chan struct{})
	checked := make(chan struct{})
	o.storeHealth.probe = func() storeHealthSnapshot {
		close(entered)
		<-release
		return storeHealthSnapshot{healthy: true, adding: true}
	}
	go func() {
		o.checkRuntimeStore()
		close(checked)
	}()
	<-entered
	stopped := make(chan error, 1)
	go func() { stopped <- f.sd.stop(context.Background()) }()
	// The owner started its probe before the authorized close. The close
	// cannot overtake that probe and expose a half-closed writer to it.
	select {
	case err := <-stopped:
		t.Fatalf("orderly stop passed an in-flight health probe: %v", err)
	case <-time.After(25 * time.Millisecond):
	}
	close(release)
	<-checked
	if err := <-stopped; err != nil {
		t.Fatalf("orderly stop after health probe: %v", err)
	}
	o.storeHealth.probe = nil
	o.checkRuntimeStore()
	if o.storeHealth.stopped {
		t.Fatal("completed authorized close was classified as store failure")
	}
}

func TestRuntimeStoreRefusedCloseRetainsFailureGuard(t *testing.T) {
	h := newSeamHarness(t, seamOptions{})
	o := h.ownerFor()
	if err := h.rig.startAlerts(context.Background()); err != nil {
		t.Fatalf("start alert loop: %v", err)
	}
	// Force the real Shutdown precondition to refuse, before it cancels the
	// writer or closes the store. Restore the cancel for fixture cleanup.
	cancel := h.rig.storeCancel
	h.rig.storeCancel = nil
	err := o.sd.stopStore(context.Background())
	h.rig.storeCancel = cancel
	if err == nil || !strings.Contains(err.Error(), "cancel") {
		t.Fatalf("orderly close did not refuse missing writer cancel: %v", err)
	}
	if !h.rig.store.Health().AllowsAdding() {
		t.Fatal("refused close stopped the writer")
	}
	cancel()
	select {
	case <-h.rig.storeDone:
	case <-time.After(3 * time.Second):
		t.Fatal("unexpected writer exit did not complete")
	}
	o.checkRuntimeStore()
	if !o.storeHealth.stopped {
		t.Fatal("refused close masked a subsequent unexpected writer exit")
	}
	record, present, loadErr := h.rig.latch.Load()
	if loadErr != nil || !present || record.Trigger != "store_unhealthy" {
		t.Fatalf("writer exit was not durably stopped: record=%+v present=%t err=%v", record, present, loadErr)
	}
	select {
	case a := <-h.rig.anom.ch:
		if a.Class != "STORE_UNHEALTHY" || a.Sev.String() != "SEV1" {
			t.Fatalf("unexpected writer exit raised %+v", a)
		}
	default:
		t.Fatal("unexpected writer exit raised no SEV1")
	}
}
