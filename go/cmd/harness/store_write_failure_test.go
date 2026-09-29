package main

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"lip/harness/quote"
)

// TestRuntimeStoreRealSQLiteWriteFailureLatchesStop holds SQLite's WAL writer
// lock on the seam's private database. The production hstore writer then makes
// a real INSERT attempt and receives SQLITE_BUSY from its zero-timeout handle.
func TestRuntimeStoreRealSQLiteWriteFailureLatchesStop(t *testing.T) {
	h := newSeamHarness(t, seamOptions{})
	o := h.ownerFor()
	if health := h.rig.store.Health(); !health.Healthy() || health.Failures() != 0 {
		t.Fatalf("store was unhealthy before contention: %+v", health)
	}

	contender, err := sql.Open("sqlite", "file:"+h.cfg.Paths.DB+"?_pragma=busy_timeout(0)")
	if err != nil {
		t.Fatal(err)
	}
	conn, err := contender.Conn(context.Background())
	if err != nil {
		_ = contender.Close()
		t.Fatal(err)
	}
	locked := false
	released := false
	release := func() {
		if released {
			return
		}
		released = true
		if locked {
			if _, err := conn.ExecContext(context.Background(), "ROLLBACK"); err != nil {
				t.Errorf("releasing private SQLite write lock: %v", err)
			}
			locked = false
		}
		if err := conn.Close(); err != nil {
			t.Errorf("closing SQLite contender connection: %v", err)
		}
		if err := contender.Close(); err != nil {
			t.Errorf("closing SQLite contender: %v", err)
		}
	}
	defer release()
	if _, err := conn.ExecContext(context.Background(), "BEGIN IMMEDIATE"); err != nil {
		t.Fatalf("acquiring private SQLite write lock: %v", err)
	}
	locked = true

	const pollMs = int64(1_700_000_060_000)
	if _, err := h.rig.store.RecordBalancePoll(h.rig.run, pollMs, 10_000); err != nil {
		t.Fatalf("submitting balance row to real writer: %v", err)
	}
	h.await("the real SQLite writer to report a failed INSERT", func() bool {
		return h.rig.store.Health().Failures() > 0
	})
	health := h.rig.store.Health()
	if health.Healthy() || health.AllowsAdding() || health.Pending() == 0 || health.LastError() == "" {
		t.Fatalf("failed SQLite write did not revoke adding: %+v", health)
	}
	rows, err := h.rig.store.Reader().BalancePolls()
	if err != nil {
		t.Fatalf("reading private SQLite database during contention: %v", err)
	}
	for _, row := range rows {
		if row.TsMs == pollMs {
			t.Fatal("contended INSERT was reported failed but became durable")
		}
	}

	o.evaluate(0)
	if o.global != quote.WindingDown || h.latchTrigger() != "store_unhealthy" {
		t.Fatalf("real write failure global=%s durable trigger=%q", o.global, h.latchTrigger())
	}

	release()
	h.await("the failed write and stop evidence to retry durably", func() bool {
		return h.rig.store.Health().Pending() == 0
	})
	rows, err = h.rig.store.Reader().BalancePolls()
	if err != nil {
		t.Fatalf("reading retried balance row: %v", err)
	}
	found := false
	for _, row := range rows {
		found = found || row.TsMs == pollMs
	}
	if !found {
		t.Fatal("failed balance INSERT did not become durable after lock release")
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
