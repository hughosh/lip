package hstore

import (
	"context"
	"errors"
	"os"
	"testing"

	"lip/harness/quote"
	"lip/harness/risk"
)

func anomaly(class string, sev risk.Severity, ticker string) risk.Anomaly {
	return risk.Anomaly{
		Class: class, Sev: sev, Ticker: ticker,
		Text: "an operator-readable description of " + class,
	}
}

// TestAuditQueueNeverDropsWhileWriterIsStalled is the audit-grade claim.
//
// A store that shed load under pressure would shed exactly the rows describing
// the pressure: the anomalies produced BY the incident are the ones queued
// behind the write the incident stalled. So there is no low-value tier and no
// eviction, and a stall makes the store unhealthy -- which revokes adding -- and
// changes nothing else.
//
// The block is a handshake, not a sleep: the writer signals from inside the
// stalled call, so every assertion below runs at a known point. `M-HS-AUDITDROP`
// evicts the oldest waiting record.
func TestAuditQueueNeverDropsWhileWriterIsStalled(t *testing.T) {
	s, c, clk := gatedStore(t)
	h := begin(t, s, "runa", 1_700_000_000_000)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c.block()
	go s.Run(ctx)

	const n = 8
	ids := make([]string, n)
	for i := range ids {
		ids[i] = string(rune('a'+i)) + "-anom"
	}
	record := func(i int) {
		t.Helper()
		if _, err := s.RecordAnomaly(h, ids[i],
			anomaly("OWNER_STALLED", risk.SEV1, "KXTEST-A"),
			1_700_000_010_000+int64(i)); err != nil {
			t.Fatalf("RecordAnomaly %d: %v", i, err)
		}
	}

	// The first record, then the handshake: everything after this line is
	// submitted while the writer is DEMONSTRABLY inside a stalled write. That
	// ordering is load-bearing. Submitting all eight first and then waiting
	// leaves the store free to have taken none of them under a stall, so a
	// store that evicted under exactly that condition would never be asked to.
	record(0)
	<-c.entered
	for i := 1; i < n; i++ {
		record(i)
	}

	// Time passes while the head is stuck. Nothing behind it is durable.
	clk.advance(defaultStallBound)
	hl := s.Health()
	if hl.Healthy() || hl.AllowsAdding() || !hl.Stalled() {
		t.Fatalf("a writer stuck past the progress bound reported %+v; "+
			"nothing queued behind a stalled write is durable, so adding "+
			"authority must be revoked (H-STORE-3)", hl)
	}
	if hl.Pending() != n {
		t.Fatalf("Health reports %d pending, want %d", hl.Pending(), n)
	}

	// The direct claim: every submitted record is still in the queue, in order.
	s.mu.Lock()
	queued := make([]string, len(s.queue))
	for i, sub := range s.queue {
		queued[i] = sub.anom.AnomalyID
	}
	s.mu.Unlock()
	if len(queued) != n {
		t.Fatalf("the queue holds %d of %d submitted audit records (%v); an "+
			"evicted anomaly is evidence of the incident that caused the "+
			"stall, deleted by the stall", len(queued), n, queued)
	}
	for i, id := range queued {
		if id != ids[i] {
			t.Fatalf("queue order is %v, want %v", queued, ids)
		}
	}

	c.unblock()

	got := 0
	for got < n {
		<-s.Wake()
		for _, r := range s.TakeResults() {
			if !r.OK() {
				t.Fatalf("record failed after the stall cleared: %v", r.Err)
			}
			got++
		}
	}

	for _, id := range ids {
		row, ok, err := s.Reader().Anomaly(id)
		if err != nil || !ok {
			t.Fatalf("anomaly %s did not survive the stall: ok=%v err=%v",
				id, ok, err)
		}
		if !row.Journaled {
			t.Fatalf("anomaly %s committed but never became journalled", id)
		}
	}

	// The failed record and its backlog are durable, so the store is healthy.
	if hl := s.Health(); !hl.Healthy() || !hl.AllowsAdding() {
		t.Fatalf("the store did not recover after draining: %+v", hl)
	}
}

// TestAnomalyIsInvisibleToDeliveryUntilBothJournalsAreDurable is §13.1's
// ordering.
//
// The SQLite row commits FIRST, with `journaled_ms` NULL. Only after the JSONL
// line is appended and synced does the update that makes the row
// delivery-visible run. The reason for that order is the crash window: a record
// visible to delivery before its text is durable can be pushed once and then
// lost, which is the one outcome having two journals was supposed to make
// impossible.
//
// `M-HS-ANOMRACE` exposes the row after only one journal succeeds.
func TestAnomalyIsInvisibleToDeliveryUntilBothJournalsAreDurable(t *testing.T) {
	s, c, _ := gatedStore(t)
	h := begin(t, s, "runa", 1_700_000_000_000)

	c.setJrnlErr(errors.New("no space left on device"))
	if _, err := s.RecordAnomaly(h, "anom-1",
		anomaly("FOREIGN_FILL", risk.SEV1, "KXTEST-A"),
		1_700_000_010_000); err != nil {
		t.Fatalf("RecordAnomaly: %v", err)
	}
	pump(t, s)

	row, ok, err := s.Reader().Anomaly("anom-1")
	if err != nil || !ok {
		t.Fatalf("the anomaly row did not commit first: ok=%v err=%v", ok, err)
	}
	if row.Journaled {
		t.Fatal("the row reports itself journalled while the text journal " +
			"rejected it")
	}

	pending, err := s.Reader().PendingAnomalies()
	if err != nil {
		t.Fatalf("PendingAnomalies: %v", err)
	}
	if len(pending) != 0 {
		t.Fatalf("an anomaly became delivery-visible with only ONE journal "+
			"durable (%+v); §13.1 journals before delivery is attempted, and a "+
			"record delivered before its text is on disk can be pushed once "+
			"and then lost in the crash the journal exists for", pending)
	}

	// H-STORE-3: the failure revokes adding and nothing else.
	if hl := s.Health(); hl.Healthy() || hl.AllowsAdding() {
		t.Fatalf("a failed journal write left the store healthy: %+v", hl)
	}
	if _, err := s.RecordAnomaly(h, "anom-2",
		anomaly("OWNER_STALLED", risk.SEV1, ""), 1_700_000_011_000); err != nil {
		t.Fatalf("the store refused a new anomaly while unhealthy (%v); the "+
			"records a broken store most needs to keep producing are the ones "+
			"describing why it broke", err)
	}

	// The disk comes back. The retry resumes at the journal rather than
	// re-inserting the row.
	c.setJrnlErr(nil)
	for _, r := range pump(t, s) {
		if !r.OK() {
			t.Fatalf("retry failed: %v", r.Err)
		}
	}

	row, ok, err = s.Reader().Anomaly("anom-1")
	if err != nil || !ok || !row.Journaled {
		t.Fatalf("anom-1 after recovery: ok=%v journaled=%v err=%v",
			ok, row.Journaled, err)
	}
	pending, err = s.Reader().PendingAnomalies()
	if err != nil {
		t.Fatalf("PendingAnomalies: %v", err)
	}
	if len(pending) != 2 {
		t.Fatalf("pending after recovery is %+v, want both rows", pending)
	}
	if pending[0].AnomalyID != "anom-1" {
		t.Fatalf("pending is not oldest-first: %+v", pending)
	}
	if hl := s.Health(); !hl.Healthy() || !hl.AllowsAdding() {
		t.Fatalf("the store did not recover: %+v", hl)
	}
}

// TestAnomalyJournalIsPrivateAppendOnlyAndFailsClosed covers the file's mode and
// the reconciliation's refusal to guess.
func TestAnomalyJournalIsPrivateAppendOnlyAndFailsClosed(t *testing.T) {
	dbPath, logPath := paths(t)
	s := openAt(t, dbPath, logPath)
	h := begin(t, s, "runa", 1_700_000_000_000)
	if _, err := s.RecordAnomaly(h, "anom-1",
		anomaly("FOREIGN_FILL", risk.SEV1, "KXTEST-A"),
		1_700_000_010_000); err != nil {
		t.Fatalf("RecordAnomaly: %v", err)
	}
	pump(t, s)
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	info, err := os.Stat(logPath)
	if err != nil {
		t.Fatalf("stat journal: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("the anomaly journal is mode %v, want 0600; it holds every "+
			"description of what went wrong on a live trading account",
			info.Mode().Perm())
	}

	// Reopening a consistent pair is fine.
	s2 := openAt(t, dbPath, logPath)
	if err := s2.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// A truncated final record fails closed rather than being skipped.
	raw, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read journal: %v", err)
	}
	if err := os.WriteFile(logPath, raw[:len(raw)-5], 0o600); err != nil {
		t.Fatalf("truncate journal: %v", err)
	}
	if bad, err := Open(StoreConfig{DBPath: dbPath, AnomalyLogPath: logPath}); err == nil {
		bad.Close()
		t.Fatal("a truncated anomaly journal opened successfully; the journal " +
			"is what the operator reads when the database cannot be opened, " +
			"and a parser that skipped what it could not read would narrow " +
			"that guarantee to \"the lines that happened to be intact\"")
	}
}

// TestStoreFailureBlocksAddingButNotReducerOrMonitorWork is H-STORE-3, which is
// the explicit correction to F19.
//
// Persistence failure revokes ADDING authority. It does not stop the reducer, it
// does not stop monitoring, it does not stop producing audit records, and it
// does not stop the retry. A store failure that stopped the reducer would
// convert a disk problem into an inventory problem -- and every stop path in
// this system stops adding while none stops reducing (I1).
//
// `M-HS-STOREGATE` lets an adding dispatch through while the store is broken.
func TestStoreFailureBlocksAddingButNotReducerOrMonitorWork(t *testing.T) {
	s, c, _ := gatedStore(t)
	h := begin(t, s, "runa", 1_700_000_000_000)

	addOrder := order(t, "runa", 1, quote.SideYes)
	adding := reserve(t, s, h, addOrder, quote.RoleAdding, 1_700_000_001_000)
	reducing := reserve(t, s, h, order(t, "runa", 2, quote.SideNo),
		quote.RoleReducing, 1_700_000_002_000)
	bind(t, s, addOrder.ClientOrderID(), "ord-a", 1_700_000_003_000)

	if _, err := adding.Order(); err != nil {
		t.Fatalf("an adding permit was refused while the store was healthy: %v",
			err)
	}

	// The disk goes away.
	c.setBackErr(errors.New("input/output error"))
	if _, err := s.RecordAnomaly(h, "anom-1",
		anomaly("OWNER_STALLED", risk.SEV1, ""), 1_700_000_010_000); err != nil {
		t.Fatalf("RecordAnomaly: %v", err)
	}
	pump(t, s)

	hl := s.Health()
	if hl.Healthy() || hl.AllowsAdding() {
		t.Fatalf("a failed write left the store healthy: %+v", hl)
	}

	if _, err := adding.Order(); err == nil {
		t.Fatal("an ADDING order was dispatched while storage was unavailable; " +
			"its ownership record could not be written, and an unrecorded " +
			"order produces a fill H-ORD-9 must classify as foreign (H-STORE-3)")
	}
	if _, err := reducing.Order(); err != nil {
		t.Fatalf("a REDUCING order was blocked by a storage failure (%v); "+
			"H-STORE-3 revokes adding and only adding, and blocking the exit "+
			"turns a disk problem into an inventory problem", err)
	}

	// Monitoring, audit production and state recording all continue to be
	// accepted -- they queue, they retry, and they are not refused.
	if _, err := s.RecordFill(h, fill("trade-1", "ord-a"),
		1_700_000_011_000, false); err != nil {
		t.Fatalf("a fill record was refused while the store was unhealthy: %v",
			err)
	}
	if _, err := s.RecordMarketState(h, "m1", 1_700_000_012_000, "KXTEST-A",
		marketIdle, marketQuoting, triggerSelected); err != nil {
		t.Fatalf("a state event was refused while the store was unhealthy: %v",
			err)
	}
	if _, err := s.RecordAnomaly(h, "anom-2",
		anomaly("POSITION_DRIFT", risk.SEV2, "KXTEST-A"),
		1_700_000_013_000); err != nil {
		t.Fatalf("an anomaly was refused while the store was unhealthy: %v", err)
	}
	if s.Health().Pending() < 4 {
		t.Fatalf("records submitted during the outage were not retained: %+v",
			s.Health())
	}

	// The disk comes back and the backlog drains. Only then is adding restored.
	c.setBackErr(nil)
	for _, r := range pump(t, s) {
		if !r.OK() {
			t.Fatalf("a record queued during the outage failed on retry: %v",
				r.Err)
		}
	}

	if hl := s.Health(); !hl.Healthy() || !hl.AllowsAdding() {
		t.Fatalf("the store did not recover after the backlog drained: %+v", hl)
	}
	if _, err := adding.Order(); err != nil {
		t.Fatalf("adding was still revoked after recovery: %v", err)
	}
	if row, ok, err := s.Reader().Anomaly("anom-1"); err != nil || !ok ||
		!row.Journaled {
		t.Fatalf("the record that failed during the outage never became "+
			"durable: ok=%v err=%v", ok, err)
	}
}
