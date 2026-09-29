package main

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"lip/harness/lifecycle"
)

// Exercise rig.close itself: a refusal after alert cancellation must restore
// that exact service and must not release the account's instance lock.
func TestRigCloseRefusalRetainsLockAndAlerts(t *testing.T) {
	h := newSeamHarness(t, seamOptions{ReadOnly: true})
	stepper := &servingCloseStepper{}
	h.rig.alerts = stepper
	if err := h.rig.startAlerts(h.ctx); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", "file:"+h.cfg.Paths.DB+"?_pragma=busy_timeout(0)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	conn, err := db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err = conn.ExecContext(context.Background(), "BEGIN IMMEDIATE"); err != nil {
		t.Fatal(err)
	}
	defer conn.ExecContext(context.Background(), "ROLLBACK")
	const at = 1_700_000_070_321
	if _, err = h.rig.store.RecordBalancePoll(h.rig.run, at, 12000); err != nil {
		t.Fatal(err)
	}
	h.await("SQLite pending record", func() bool { return h.rig.store.Health().Failures() > 0 })
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if err = h.rig.close(ctx); err == nil {
		t.Fatal("close crossed accepted pending record")
	}
	lock, err := lifecycle.AcquireInstanceLock(h.cfg.Paths.Lock)
	if err == nil {
		_ = lock.Close()
		t.Fatal("refused close released instance lock")
	}
	if released, _ := h.rig.closeFailed(); released {
		t.Fatal("a refused close reported the instance lock released")
	}
	before := stepper.calls.Load()
	h.await("alert service after refused close", func() bool { return stepper.calls.Load() > before })
	select {
	case <-h.rig.storeDone:
		t.Fatal("refused close stopped writer")
	default:
	}
	if _, err = conn.ExecContext(context.Background(), "ROLLBACK"); err != nil {
		t.Fatal(err)
	}
	if err = h.rig.close(context.Background()); err != nil {
		t.Fatalf("recovered close: %v", err)
	}
	if released, failed := h.rig.closeFailed(); !released || failed != nil {
		t.Fatalf("after a clean close: lock released=%t, recorded failure=%v", released, failed)
	}
	if got := servingCloseBalanceCount(t, h.cfg.Paths.DB, at); got != 1 {
		t.Fatalf("lost accepted row: %d", got)
	}
	lock, err = lifecycle.AcquireInstanceLock(h.cfg.Paths.Lock)
	if err != nil {
		t.Fatalf("successful retry retained lock: %v", err)
	}
	_ = lock.Close()
}
