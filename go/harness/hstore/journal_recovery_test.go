package hstore

import (
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"lip/harness/risk"
)

// reopen closes the store and opens the same two artifacts through the real
// Open, which is where reconcile runs and where a self-contradicting journal
// fails closed.
func reopen(t *testing.T, s *Store, dbPath, logPath string) {
	t.Helper()
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	s2, err := Open(StoreConfig{DBPath: dbPath, AnomalyLogPath: logPath})
	if err != nil {
		t.Fatalf("the store could not be reopened after a journal failure, so "+
			"one transient disk error has ended the harness until a human "+
			"edits the file the system calls tamper-evident: %v", err)
	}
	s2.Close()
}

// TestJournalRetryAfterPostWriteSyncFailureDoesNotDuplicate covers the sync
// that fails after the bytes landed.
//
// The store retries such a failure forever, by design. The retry must produce a
// line the previous attempt could not be distinguished from: reconcile tolerates
// an identical duplicate and refuses a disagreeing one, so a per-attempt
// timestamp turns one transient EIO into a permanent refusal to start.
func TestJournalRetryAfterPostWriteSyncFailureDoesNotDuplicate(t *testing.T) {
	dbPath, logPath := paths(t)
	s, c, clk := gatedStoreAt(t, dbPath, logPath)
	h := begin(t, s, "runa", 1_700_000_000_000)

	c.setJrnlErrAfterWrite(errors.New("input/output error"))
	if _, err := s.RecordAnomaly(h, "anom-1",
		anomaly("FOREIGN_FILL", risk.SEV1, "KXTEST-A"),
		1_700_000_010_000); err != nil {
		t.Fatalf("RecordAnomaly: %v", err)
	}
	pump(t, s)

	// Time passes between the failure and the retry, as it does in production.
	// Without this the two attempts would stamp the same millisecond and the
	// defect would be invisible -- the setup has to reach the branch.
	clk.advanceWallOnly(time.Second)
	c.setJrnlErr(nil)
	pump(t, s)

	raw, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read journal: %v", err)
	}
	lines := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("expected the landed line plus its retry, saw %d: %s",
			len(lines), raw)
	}
	if lines[0] != lines[1] {
		t.Fatalf("the retry wrote a line that disagrees with the one that "+
			"already landed, which reconcile refuses forever:\n%s\n%s",
			lines[0], lines[1])
	}
	reopen(t, s, dbPath, logPath)
}

// TestJournalRetryAfterPartialWriteDoesNotCorrupt covers a short write: a
// prefix of the line reaches the file and the error arrives with the rest of it
// undelivered.
//
// Appending the retry after those bytes merges two records into one line that
// parses as neither. The file must be returned to its last known-good length
// first, which is what makes the retry byte-identical to a first attempt.
func TestJournalRetryAfterPartialWriteDoesNotCorrupt(t *testing.T) {
	origWrite := journalWriteFn
	t.Cleanup(func() { journalWriteFn = origWrite })
	fired := false
	journalWriteFn = func(f *os.File, b []byte) (int, error) {
		if !fired {
			fired = true
			n, _ := f.Write(b[:len(b)/2])
			return n, errors.New("no space left on device")
		}
		return f.Write(b)
	}

	dbPath, logPath := paths(t)
	s, _, clk := gatedStoreAt(t, dbPath, logPath)
	h := begin(t, s, "runa", 1_700_000_000_000)

	if _, err := s.RecordAnomaly(h, "anom-1",
		anomaly("FOREIGN_FILL", risk.SEV1, "KXTEST-A"),
		1_700_000_010_000); err != nil {
		t.Fatalf("RecordAnomaly: %v", err)
	}
	pump(t, s)
	clk.advanceWallOnly(time.Second)
	pump(t, s)

	raw, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read journal: %v", err)
	}
	if _, err := parseJournal(raw); err != nil {
		t.Fatalf("the journal no longer parses after a short write and its "+
			"retry, so the operator's fallback copy is gone: %v\n%s", err, raw)
	}
	if n := strings.Count(string(raw), `"anomaly_id":"anom-1"`); n != 1 {
		t.Fatalf("expected exactly one complete record for anom-1, saw %d:\n%s",
			n, raw)
	}
	reopen(t, s, dbPath, logPath)
}

// TestOpenRepairsTornTailFromCrashDuringAppend covers the power cut DURING the
// append, which the reconcile doc did not enumerate and which is the likeliest
// crash point in an anomaly burst.
//
// The torn tail is this store's own artifact, and the record it belongs to was
// never delivery-visible: its row still carries `journaled_ms` NULL, so
// reconcile re-journals it from the database. Refusing to start over it refuses
// to start over nothing. A malformed line with complete lines AFTER it is not a
// crash artifact and must still fail closed.
func TestOpenRepairsTornTailFromCrashDuringAppend(t *testing.T) {
	dbPath, logPath := paths(t)
	s, _, _ := gatedStoreAt(t, dbPath, logPath)
	h := begin(t, s, "runa", 1_700_000_000_000)
	if _, err := s.RecordAnomaly(h, "anom-1",
		anomaly("FOREIGN_FILL", risk.SEV1, "KXTEST-A"),
		1_700_000_010_000); err != nil {
		t.Fatalf("RecordAnomaly: %v", err)
	}
	pump(t, s)
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// The crash: a second record's line reached the disk half-written.
	f, err := os.OpenFile(logPath, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatalf("open journal for the torn write: %v", err)
	}
	if _, err := f.WriteString(`{"anomaly_id":"anom-2","run_id":"ru`); err != nil {
		t.Fatalf("torn write: %v", err)
	}
	f.Close()

	s2, err := Open(StoreConfig{DBPath: dbPath, AnomalyLogPath: logPath})
	if err != nil {
		t.Fatalf("a crash during an append left a torn tail and the store now "+
			"refuses to start, which is the harness declining to run because "+
			"of its own crash artifact: %v", err)
	}
	t.Cleanup(func() { s2.Close() })

	raw, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read journal: %v", err)
	}
	if strings.Contains(string(raw), "anom-2") {
		t.Fatalf("the partial record survived the repair:\n%s", raw)
	}
	if n := strings.Count(string(raw), `"anomaly_id":"anom-1"`); n != 1 {
		t.Fatalf("the repair did not preserve the complete record before the "+
			"torn one, saw %d:\n%s", n, raw)
	}

	// A malformed line that is NOT the tail is not a crash artifact.
	if err := s2.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	f2, err := os.OpenFile(logPath, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatalf("open journal: %v", err)
	}
	if _, err := f2.WriteString("not json at all\n"); err != nil {
		t.Fatalf("write: %v", err)
	}
	f2.Close()
	if _, err := Open(StoreConfig{DBPath: dbPath,
		AnomalyLogPath: logPath}); err == nil {
		t.Fatal("a complete but unparseable line was accepted; tamper-evidence " +
			"narrows to the crash artifact only, not to anything malformed")
	}
}
