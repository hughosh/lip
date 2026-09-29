package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"lip/harness/num"
	"lip/harness/quote"
)

func TestOperationsDiskRefusesEitherFullEvidenceVolume(t *testing.T) {
	storeDir, evidenceDir := t.TempDir(), t.TempDir()
	storeDir, _ = filepath.EvalSymlinks(storeDir)
	evidenceDir, _ = filepath.EvalSymlinks(evidenceDir)
	db := filepath.Join(storeDir, "harness.db")
	journal := filepath.Join(storeDir, "anomalies.log")
	evidence := filepath.Join(evidenceDir, "qualification.json")
	for path, contents := range map[string]string{
		db: "ledger", db + "-wal": "wal", journal: "journal", evidence: "evidence",
	} {
		if err := os.WriteFile(path, []byte(contents), 0600); err != nil {
			t.Fatal(err)
		}
	}

	for _, tc := range []struct {
		name, fullDir string
	}{
		{"store", storeDir},
		{"evidence", evidenceDir},
	} {
		t.Run(tc.name, func(t *testing.T) {
			snap, err := checkOperationsDiskWithFree(db, journal, evidence,
				func(dir string) (uint64, error) {
					if dir == tc.fullDir {
						return operationsMinFreeBytes - 1, nil
					}
					return operationsMinFreeBytes + 1, nil
				})
			if err == nil || !strings.Contains(err.Error(), "disk headroom") {
				t.Fatalf("full %s volume was accepted: %v", tc.name, err)
			}
			if snap.ArtifactBytes != uint64(len("ledgerwaljournalevidence")) {
				t.Fatalf("artifact growth was not measured: %+v", snap)
			}
		})
	}
}

func TestOperationsDiskRuntimeLatchesAndKeepsReducerAndObservation(t *testing.T) {
	g := newGateFailOwner(t, num.QtyFromFloat(1))
	o, h := g.o, g.h
	driftRestingOrders(t, g)
	h.ex.setPosition(seamTicker, "1.00")
	reducerConfirmationPoll(t, g)
	if o.global != quote.Running {
		t.Fatalf("fixture starts in %s, not RUNNING", o.global)
	}
	o.operationsDisk = operationsDiskGuard{
		enabled: true, db: h.cfg.Paths.DB, journal: h.cfg.Paths.AnomalyLog,
		free: func(string) (uint64, error) { return operationsMinFreeBytes - 1, nil },
	}
	now := h.clk.Now().Mono
	o.evaluate(now)
	if got := h.latchTrigger(); got != "disk_headroom" {
		t.Fatalf("runtime disk failure did not commit its durable cause: %q", got)
	}
	if o.global.AddsRisk() {
		t.Fatalf("disk failure left global state %s adding", o.global)
	}
	o.evaluate(now)
	if o.market != quote.Reducing {
		t.Fatalf("inventory market %s lost its reducing state", o.market)
	}
	driftAssertReducer(t, g)
	walks := h.ex.ordersWalkCount()
	driftPollAgain(t, g)
	if h.ex.ordersWalkCount() <= walks {
		t.Fatal("disk stop prevented portfolio observation")
	}
	o.publish(now)
	if snap := h.snapshot(); snap == nil || snap.Global.AddsRisk() {
		t.Fatalf("observation did not retain the disk stop: %+v", snap)
	}
	ctx, cancel := context.WithCancel(h.ctx)
	ticks, done := make(chan time.Time, 1), make(chan struct{})
	go func() { defer close(done); h.rig.mon.run(ctx, ticks) }()
	ticks <- time.Now()
	h.await("monitor sample during disk stop", func() bool { return h.rig.last.Load() != nil })
	if hb := h.rig.heartbeat(); hb.Global.AddsRisk() || len(hb.Markets) != 1 || hb.Markets[0].State != quote.Reducing {
		t.Fatalf("heartbeat lost disk-stop reduction: %+v", hb)
	}
	cancel()
	<-done
	// The standing disk condition is reported once; the normal owner tick still
	// runs, and a later sample cannot clear the durable stop.
	o.checkRuntimeDisk(now + 2*time.Minute)
	if !o.operationsDisk.stopped || h.latchTrigger() != "disk_headroom" {
		t.Fatal("runtime disk stop self-cleared on a later tick")
	}
}

func TestOperationsDiskAcceptsReserveBoundaryAndRefusesUnknownMeasurement(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "harness.db")
	journal := filepath.Join(dir, "anomalies.log")
	probe := func(string) (uint64, error) { return operationsMinFreeBytes, nil }
	if _, err := checkOperationsDiskWithFree(db, journal, "", probe); err != nil {
		t.Fatalf("a new WAL and journal may be absent at the reserve boundary: %v", err)
	}
	if _, err := checkOperationsDiskWithFree(db, journal, "",
		func(string) (uint64, error) { return 0, errors.New("statfs failed") }); err == nil || !strings.Contains(err.Error(), "statfs failed") {
		t.Fatalf("unknown headroom must refuse: %v", err)
	}
	if _, err := checkOperationsDiskWithFree(db, journal, "relative-evidence", probe); err == nil || !strings.Contains(err.Error(), "not absolute") {
		t.Fatalf("relative evidence path must refuse: %v", err)
	}
}

func TestOperationsDiskChecksSymlinkTargetVolume(t *testing.T) {
	linkDir, targetDir := t.TempDir(), t.TempDir()
	linkDir, _ = filepath.EvalSymlinks(linkDir)
	targetDir, _ = filepath.EvalSymlinks(targetDir)
	target := filepath.Join(targetDir, "harness.db")
	if err := os.WriteFile(target, []byte("ledger"), 0600); err != nil {
		t.Fatal(err)
	}
	db := filepath.Join(linkDir, "harness.db")
	if err := os.Symlink(target, db); err != nil {
		t.Fatal(err)
	}
	_, err := checkOperationsDiskWithFree(db, filepath.Join(linkDir, "anomalies.log"), "",
		func(dir string) (uint64, error) {
			if dir == targetDir {
				return operationsMinFreeBytes - 1, nil
			}
			return operationsMinFreeBytes + 1, nil
		})
	if err == nil || !strings.Contains(err.Error(), "disk headroom") {
		t.Fatalf("full symlink target volume was accepted: %v", err)
	}
}
