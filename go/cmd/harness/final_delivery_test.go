package main

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"lip/harness/hstore"
	"lip/harness/ping"
)

type finalDeliveryStepper struct {
	store *hstore.Store
	calls atomic.Int32
	errs  chan error
}

func (s *finalDeliveryStepper) Step(_ context.Context, now int64, _ ping.Heartbeat) ping.Effects {
	if s.calls.Add(1) == 2 {
		// Valid submission shape but a nonexistent durable anomaly. SQLite must
		// terminally reject the accepted delivery record on the final alert pass.
		_, err := s.store.RecordDeliveryAttempt(hstore.DeliveryAttempt{AnomalyIDs: []string{"missing-anomaly"}, AttemptMs: now, Delivered: true, DeliveredMs: now})
		s.errs <- err
	}
	return ping.Effects{NextStepMs: now + int64(time.Hour/time.Millisecond)}
}
func TestFinalAlertResultFailureIsDrainedBeforeClose(t *testing.T) {
	h := newSeamHarness(t, seamOptions{ReadOnly: true})
	stepper := &finalDeliveryStepper{store: h.rig.store, errs: make(chan error, 1)}
	h.rig.alerts = stepper
	if err := h.rig.startAlerts(h.ctx); err != nil {
		t.Fatal(err)
	}
	h.await("initial alert pass", func() bool { return stepper.calls.Load() == 1 })
	sd := newShutdown(h.rig)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := sd.stopStore(ctx)
	if err == nil || !strings.Contains(err.Error(), "final delivery produced new record-failure evidence") {
		t.Fatalf("final delivery lost-result refusal: %v", err)
	}
	if err := <-stepper.errs; err != nil {
		t.Fatalf("fixture record was not accepted: %v", err)
	}
	if got := servingCloseAnomalyCount(t, h.cfg.Paths.DB, hstore.RecordRejectedClass); got != 1 {
		t.Fatalf("terminal delivery rejection records=%d, want 1", got)
	}
	select {
	case <-h.rig.storeDone:
		t.Fatal("writer closed before late failure could be alerted")
	default:
	}
	h.await("alert service restored for late failure", func() bool { return stepper.calls.Load() > 2 })
	if err := sd.stopStore(ctx); err != nil {
		t.Fatalf("retry after final failure evidence: %v", err)
	}
}
