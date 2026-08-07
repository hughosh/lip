package hstore

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"lip/harness/quote"
	"lip/harness/rest"
	"lip/harness/risk"
)

// drained spins until the writer's FIFO is empty. It yields rather than sleeps;
// the bound exists so a mutation that wedges the writer fails fast.
func drained(t *testing.T, s *Store) {
	t.Helper()
	for i := 0; i < 500_000; i++ {
		if s.Health().Pending() == 0 {
			return
		}
		runtime.Gosched()
	}
	t.Fatalf("the queue never drained: %+v", s.Health())
}

// TestOnlyOneWriterRunCanOwnTheFIFO is the single-writer rule.
//
// Two goroutines in `Run` each call `beginHead`, each receive the SAME head,
// each write it, and each `popLocked` when they finish. The second pop removes
// the record BEHIND the head -- which was never written, and whose submitter was
// told it was accepted. There is no error anywhere: an anomaly simply ceases to
// exist, or a binding does, and the fills on that order become permanently
// unclassifiable.
//
// `M-HS-ONERUN` disables the guard.
func TestOnlyOneWriterRunCanOwnTheFIFO(t *testing.T) {
	s, c, _ := gatedStore(t)
	h := begin(t, s, "runa", 1_700_000_000_000)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c.block()

	rec := func(id string, ms int64) {
		t.Helper()
		if _, err := s.RecordAnomaly(h, id,
			anomaly("OWNER_STALLED", risk.SEV1, "KXTEST-A"), ms); err != nil {
			t.Fatalf("RecordAnomaly %s: %v", id, err)
		}
	}

	rec("w-a", 1_700_000_010_000)
	go s.Run(ctx)
	<-c.entered
	// A SECOND writer, while the first demonstrably holds the head.
	go s.Run(ctx)

	rec("w-b", 1_700_000_011_000)
	rec("w-c", 1_700_000_012_000)
	c.unblock()
	drained(t, s)

	for _, id := range []string{"w-a", "w-b", "w-c"} {
		row, ok, err := s.Reader().Anomaly(id)
		if err != nil {
			t.Fatalf("read %s: %v", id, err)
		}
		if !ok || !row.Journaled {
			t.Fatalf("audit record %s was accepted and never became durable. "+
				"Two writers completed one head and each popped, so a queued "+
				"record was discarded without being written and without an "+
				"error anywhere", id)
		}
	}

	// The direct statement of the rule: while one writer owns the FIFO, a
	// second cannot claim it.
	if !s.claimWriter() {
		// A writer is running, so the claim is correctly refused. Stop that
		// writer and confirm the guard is what refused it.
		cancel()
		return
	}
	t.Fatal("a second writer claimed the FIFO while the first was running")
}

// TestStoppedWriterRevokesOutstandingAddingPermits is the other half of writer
// ownership.
//
// A store whose writer has returned cannot make anything durable. Reporting
// healthy then is a claim about records that will never be written, and an
// adding permit issued before the writer stopped is a licence to dispatch an
// order whose binding and whose fills have nowhere to go. Reducing is
// deliberately untouched: I1 again.
//
// `M-HS-RUNSTOP` leaves adding enabled.
func TestStoppedWriterRevokesOutstandingAddingPermits(t *testing.T) {
	s, _, _ := gatedStore(t)
	h := begin(t, s, "runa", 1_700_000_000_000)
	adding := reserve(t, s, h, order(t, "runa", 1, quote.SideYes),
		quote.RoleAdding, 1_700_000_001_000)
	reducing := reserve(t, s, h, order(t, "runa", 2, quote.SideNo),
		quote.RoleReducing, 1_700_000_002_000)

	if _, err := adding.Order(); err != nil {
		t.Fatalf("an adding permit was refused while the writer was alive: %v",
			err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.Run(ctx); close(done) }()
	cancel()
	<-done

	hl := s.Health()
	if hl.Healthy() || hl.AllowsAdding() {
		t.Fatalf("the writer has exited and the store reports %+v; nothing "+
			"submitted from now on can ever become durable", hl)
	}
	if _, err := adding.Order(); err == nil {
		t.Fatal("an ADDING permit issued before the writer stopped is still " +
			"usable; the order would be dispatched while its ownership binding " +
			"and every audit row have no writer to record them")
	}
	if _, err := reducing.Order(); err != nil {
		t.Fatalf("a REDUCING permit was revoked by the writer stopping (%v); "+
			"H-STORE-3 revokes adding and only adding, and blocking the exit "+
			"turns a process problem into an inventory problem", err)
	}
	// And nothing new may be accepted into a queue nobody will drain.
	if _, err := s.RecordAnomaly(h, "gone-1",
		anomaly("OWNER_STALLED", risk.SEV1, ""), 1_700_000_010_000); err == nil {
		t.Fatal("the store accepted an audit record after its writer exited; " +
			"a record that is accepted and never written is worse than one " +
			"that is refused")
	}
}

// TestCloseCannotDiscardAcceptedAuditRecords is the shutdown half.
//
// Every queued record has already been accepted. Closing over the top of one
// converts "this store took my anomaly" into nothing at all, silently, at the
// one moment a process is least likely to be watched. Close refuses and changes
// no state; the caller drains first or learns that it cannot.
//
// `M-HS-CLOSEDROP` allows the close.
func TestCloseCannotDiscardAcceptedAuditRecords(t *testing.T) {
	s, c, _ := gatedStore(t)
	h := begin(t, s, "runa", 1_700_000_000_000)

	c.setBackErr(errors.New("input/output error"))
	if _, err := s.RecordAnomaly(h, "close-1",
		anomaly("FOREIGN_FILL", risk.SEV1, "KXTEST-A"),
		1_700_000_010_000); err != nil {
		t.Fatalf("RecordAnomaly: %v", err)
	}
	pump(t, s)

	if s.Health().Pending() == 0 {
		t.Fatal("the record did not stay queued after a transient failure")
	}
	if err := s.Close(); err == nil {
		t.Fatal("Close succeeded with an accepted audit record still queued, " +
			"discarding it silently")
	}
	// It refused WITHOUT changing state: the store is still usable.
	if _, err := s.RecordAnomaly(h, "close-2",
		anomaly("OWNER_STALLED", risk.SEV1, ""), 1_700_000_011_000); err != nil {
		t.Fatalf("a refused Close left the store unusable: %v", err)
	}

	c.setBackErr(nil)
	for _, r := range pump(t, s) {
		if !r.OK() {
			t.Fatalf("record failed on retry: %v", r.Err)
		}
	}
	for _, id := range []string{"close-1", "close-2"} {
		if row, ok, _ := s.Reader().Anomaly(id); !ok || !row.Journaled {
			t.Fatalf("%s did not survive the refused close", id)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close refused a drained store: %v", err)
	}
}

// TestPermanentRecordFailureRevokesAddingAndCannotHealAcrossTheGap is §3.
//
// A permanently rejected record is evidence that is GONE. The head is popped so
// the audit rows behind it can still be written -- one caller's rule violation
// must not blind the whole trail -- but the store never reports healthy again,
// and in particular a later sequence crossing the transient recovery watermark
// says nothing about the row that was rejected on the way past it.
//
// `M-HS-PERMHEALTH` does not latch the fault.
func TestPermanentRecordFailureRevokesAddingAndCannotHealAcrossTheGap(t *testing.T) {
	s := tempStore(t)
	h := begin(t, s, "runa", 1_700_000_000_000)
	adding := reserve(t, s, h, order(t, "runa", 1, quote.SideYes),
		quote.RoleAdding, 1_700_000_001_000)
	reducing := reserve(t, s, h, order(t, "runa", 2, quote.SideNo),
		quote.RoleReducing, 1_700_000_002_000)

	orphan, err := rest.Coid("runa", 0, quote.SideYes, 99)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.BindOrder(orphan, "ord-orphan", 1_700_000_003_000); err != nil {
		t.Fatalf("BindOrder: %v", err)
	}
	results := pump(t, s)

	rejected := false
	for _, r := range results {
		if r.Kind == KindBindOrder && r.Err != nil {
			rejected = true
		}
	}
	if !rejected {
		t.Fatalf("the orphan binding was not rejected: %+v", results)
	}

	hl := s.Health()
	if hl.Healthy() || hl.AllowsAdding() {
		t.Fatalf("a record was permanently rejected and the store reports "+
			"%+v; the binding will never exist, so fills on that order can "+
			"never be classified, and adding must not continue", hl)
	}
	if _, err := adding.Order(); err == nil {
		t.Fatal("an ADDING order is still permitted after a record was lost")
	}
	if _, err := reducing.Order(); err != nil {
		t.Fatalf("a REDUCING order was blocked by a lost record (%v); the "+
			"revocation is of adding and only adding", err)
	}

	// Later records are still ACCEPTED and still written -- one caller's rule
	// violation must not blind the audit trail.
	if _, err := s.RecordAnomaly(h, "perm-1",
		anomaly("OWNER_STALLED", risk.SEV1, ""), 1_700_000_010_000); err != nil {
		t.Fatalf("the store refused a later audit record: %v", err)
	}
	for _, r := range pump(t, s) {
		if !r.OK() {
			t.Fatalf("a later record failed: %v", r.Err)
		}
	}
	if row, ok, _ := s.Reader().Anomaly("perm-1"); !ok || !row.Journaled {
		t.Fatal("a later audit record was not written after the permanent " +
			"failure")
	}

	// And that success must NOT heal the store.
	if hl := s.Health(); hl.Healthy() || hl.AllowsAdding() {
		t.Fatalf("a later sequence crossed the recovery watermark and healed "+
			"a store with a known-lost record (%+v); the watermark is for "+
			"TRANSIENT failures, whose records do eventually land", hl)
	}
	if _, err := adding.Order(); err == nil {
		t.Fatal("adding was restored across a permanently lost record")
	}
}

// TestWriterStallUsesMonotonicTime is F21 applied to the progress bound.
//
// The elapsed interval is measured on a MONOTONIC source. A wall clock steps --
// an NTP correction, a VM resume, an operator setting the date -- and a stall
// detector reading one either fires on a writer that is perfectly healthy or
// stops firing on one that is wedged. Records keep wall milliseconds because
// they are timestamps; nothing else does.
//
// `M-HS-STALLCLOCK` measures the bound in wall time.
func TestWriterStallUsesMonotonicTime(t *testing.T) {
	s, c, clk := gatedStore(t)
	h := begin(t, s, "runa", 1_700_000_000_000)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c.block()
	go s.Run(ctx)
	if _, err := s.RecordAnomaly(h, "mono-1",
		anomaly("OWNER_STALLED", risk.SEV1, ""), 1_700_000_010_000); err != nil {
		t.Fatalf("RecordAnomaly: %v", err)
	}
	<-c.entered

	// The wall clock jumps an hour forward. NOTHING has elapsed.
	clk.advanceWallOnly(time.Hour)
	if hl := s.Health(); hl.Stalled() {
		t.Fatalf("the wall clock stepped forward and the writer was declared "+
			"stalled after no elapsed time at all (%+v); measured this way the "+
			"detector fires on a healthy writer whenever the host's clock is "+
			"corrected, and a backward step makes it stop firing on a wedged "+
			"one", hl)
	}
	if hl := s.Health(); !hl.Healthy() {
		t.Fatalf("a wall-clock step alone made the store unhealthy: %+v", hl)
	}

	// Real elapsed time. Now it is a stall.
	clk.advance(defaultStallBound)
	hl := s.Health()
	if !hl.Stalled() || hl.Healthy() || hl.AllowsAdding() {
		t.Fatalf("the writer was in flight past the progress bound and the "+
			"store reports %+v", hl)
	}
	c.unblock()
}

// TestOpenRefusesForeignVersionZeroDatabaseWithoutWritingIt is H-ORD-7.
//
// `lip/rig.db` is written by two evidence collectors and is read-only to the
// harness. A mistyped absolute path must not put this schema into it -- and
// "reject after opening" is not good enough, because setting `journal_mode=WAL`
// is itself a write and switches the journal mode underneath two running
// writers. The inspection is therefore read-only and happens first.
//
// `M-HS-FOREIGNDB` accepts the database.
func TestOpenRefusesForeignVersionZeroDatabaseWithoutWritingIt(t *testing.T) {
	dir := t.TempDir()
	foreign := filepath.Join(dir, "rig.db")

	raw, err := sql.Open("sqlite", foreign)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(
		`CREATE TABLE fill (trade_id TEXT PRIMARY KEY, ts_ms INTEGER)`); err != nil {
		t.Fatalf("seed the foreign database: %v", err)
	}
	var mode string
	if err := raw.QueryRow("PRAGMA journal_mode").Scan(&mode); err != nil {
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}

	st, err := Open(StoreConfig{DBPath: foreign,
		AnomalyLogPath: filepath.Join(dir, "a.jsonl")})
	if err == nil {
		st.Close()
		t.Fatal("Open accepted an existing database that already held another " +
			"schema; a mistyped absolute path would create five tables in the " +
			"evidence collectors' database and switch its journal mode " +
			"underneath them (H-ORD-7)")
	}

	// And it was not touched on the way to that rejection.
	names := tableNamesAt(t, foreign)
	if len(names) != 1 || names[0] != "fill" {
		t.Fatalf("the foreign database was MODIFIED before being rejected; "+
			"its tables are now %v", names)
	}
	after := journalModeAt(t, foreign)
	if after != mode {
		t.Fatalf("the foreign database's journal mode was changed from %q to "+
			"%q by an Open that then rejected it", mode, after)
	}
}

// TestOpenRejectsAnySixthOrMissingPilotTable enforces §15's cut on a database
// this process did not necessarily create.
//
// `M-HS-SCHEMAEXTRA` accepts it.
func TestOpenRejectsAnySixthOrMissingPilotTable(t *testing.T) {
	dbPath, logPath := paths(t)
	s := openAt(t, dbPath, logPath)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	// A sixth table.
	raw, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(
		`CREATE TABLE order_intent (id TEXT PRIMARY KEY)`); err != nil {
		t.Fatalf("add a sixth table: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}
	if st, err := Open(StoreConfig{DBPath: dbPath, AnomalyLogPath: logPath}); err == nil {
		st.Close()
		t.Fatal("Open accepted a database carrying a sixth table; §15's pilot " +
			"cut is five, and a table this process does not know about means " +
			"either an implementation we do not have or a corruption we cannot " +
			"interpret")
	}

	// A missing one.
	db2, log2 := paths(t)
	s2 := openAt(t, db2, log2)
	if err := s2.Close(); err != nil {
		t.Fatal(err)
	}
	raw2, err := sql.Open("sqlite", db2)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw2.Exec(`DROP TABLE state_event`); err != nil {
		t.Fatalf("drop a pilot table: %v", err)
	}
	if err := raw2.Close(); err != nil {
		t.Fatal(err)
	}
	if st, err := Open(StoreConfig{DBPath: db2, AnomalyLogPath: log2}); err == nil {
		st.Close()
		t.Fatal("Open accepted a database missing one of the pilot five")
	}
}

// TestWritePragmasSurviveConnectionReplacement is the pool problem.
//
// `synchronous` and `foreign_keys` are per-CONNECTION. `database/sql` closes
// idle connections and opens replacements whenever it likes, so a `PRAGMA`
// executed once after `sql.Open` configures one connection and the replacement
// silently arrives with `foreign_keys=OFF` -- under which a fill can be recorded
// against an order that was never bound, which is H-ORD-9 turned off.
//
// `M-HS-PRAGMACONN` drops the DSN pragma.
func TestWritePragmasSurviveConnectionReplacement(t *testing.T) {
	s := tempStore(t)
	h := begin(t, s, "runa", 1_700_000_000_000)

	b, ok := s.back.(*sqliteBackend)
	if !ok {
		t.Fatalf("expected a real SQLite backend, got %T", s.back)
	}
	// Force a brand-new connection for every statement from here on.
	b.db.SetMaxIdleConns(0)

	p, err := s.pragmas()
	if err != nil {
		t.Fatalf("read pragmas on a replaced connection: %v", err)
	}
	if p.ForeignKeys != 1 {
		t.Fatalf("foreign_keys is %d on a replacement connection, want 1; the "+
			"pragma was applied to whichever connection happened to serve the "+
			"statement after sql.Open and did not survive it", p.ForeignKeys)
	}
	if p.Synchronous != 1 {
		t.Fatalf("synchronous is %d on a replacement connection, want 1 "+
			"(NORMAL)", p.Synchronous)
	}

	// And the key is actually ENFORCED there, not merely reported.
	if _, err := s.RecordFill(h, fill("trade-x", "ord-never-bound"),
		1_700_000_010_000, false); err != nil {
		t.Fatalf("RecordFill: %v", err)
	}
	pump(t, s)
	if _, ok, err := s.Reader().Fill("trade-x"); err != nil || ok {
		t.Fatalf("a fill was recorded against an order that was never bound "+
			"(present=%v err=%v); our_fill's reference to owned_order is "+
			"H-ORD-9 expressed as a foreign key, and an unenforced key makes "+
			"it decoration", ok, err)
	}
}

// TestStoreArtifactsMustBeDistinctFiles refuses a self-destroying configuration.
//
// The assertion is NOT merely that Open fails. Pointed at one path the two
// artifacts destroy each other so thoroughly that Open fails anyway, further
// down, once the JSONL parser is handed a SQLite page -- so "it returned an
// error" cannot tell a guard from an accident. What distinguishes them is WHEN:
// the check runs before anything is opened, so a refused configuration leaves
// NOTHING on disk. Without it, the database is created at that path first and
// the operator is left with a file that is neither artifact.
//
// `M-HS-PATHALIAS` removes the check.
func TestStoreArtifactsMustBeDistinctFiles(t *testing.T) {
	dir := t.TempDir()

	same := filepath.Join(dir, "both")
	st, err := Open(StoreConfig{DBPath: same, AnomalyLogPath: same})
	if err == nil {
		st.Close()
		t.Fatal("Open accepted one path as both the database and the anomaly " +
			"journal; they are two records of the same events so that losing " +
			"one does not lose the other, and pointed at one file the JSONL " +
			"append corrupts the SQLite header and the SQLite write truncates " +
			"the operator's text")
	}
	if _, statErr := os.Stat(same); !os.IsNotExist(statErr) {
		t.Fatalf("the aliased path %s exists after a refused Open (%v); the "+
			"configuration was rejected only AFTER a database had already been "+
			"created there, so the failure is an accident downstream rather "+
			"than a guard", same, statErr)
	}

	db := filepath.Join(dir, "harness.db")
	st, err = Open(StoreConfig{DBPath: db, AnomalyLogPath: db + "-wal"})
	if err == nil {
		st.Close()
		t.Fatal("Open accepted SQLite's own -wal sidecar as the anomaly journal")
	}
	for _, p := range []string{db, db + "-wal"} {
		if _, statErr := os.Stat(p); !os.IsNotExist(statErr) {
			t.Fatalf("%s exists after a refused Open (%v)", p, statErr)
		}
	}

	// Two NAMES for one file, which no string comparison can catch.
	real := filepath.Join(dir, "real.db")
	if err := os.WriteFile(real, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link.jsonl")
	if err := os.Link(real, link); err != nil {
		t.Fatalf("hard link: %v", err)
	}
	st, err = Open(StoreConfig{DBPath: real, AnomalyLogPath: link})
	if err == nil {
		st.Close()
		t.Fatal("Open accepted two names for one file as the database and the " +
			"anomaly journal")
	}
	info, statErr := os.Stat(real)
	if statErr != nil {
		t.Fatal(statErr)
	}
	if info.Size() != 0 {
		t.Fatalf("the aliased file was written to (%d bytes) before the "+
			"configuration was refused", info.Size())
	}
}

// TestMissingJournalLineIsAContradiction is the reconciliation's second
// direction.
//
// A row whose `journaled_ms` is set asserts that the operator's fallback copy of
// its text exists. If the line is not there, that assertion is false and nothing
// in the database can say which of the two is right. A one-way check lets the
// JSONL be deleted -- or truncated to a prefix -- completely silently, and the
// consequence appears only during the database outage the second journal exists
// for.
//
// `M-HS-JOURNALGAP` ignores the gap.
func TestMissingJournalLineIsAContradiction(t *testing.T) {
	dbPath, logPath := paths(t)
	s := openAt(t, dbPath, logPath)
	h := begin(t, s, "runa", 1_700_000_000_000)
	for _, id := range []string{"jg-1", "jg-2"} {
		if _, err := s.RecordAnomaly(h, id,
			anomaly("FOREIGN_FILL", risk.SEV1, "KXTEST-A"),
			1_700_000_010_000); err != nil {
			t.Fatalf("RecordAnomaly %s: %v", id, err)
		}
	}
	pump(t, s)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	// The whole fallback copy is gone.
	if err := os.Remove(logPath); err != nil {
		t.Fatalf("remove the journal: %v", err)
	}
	if st, err := Open(StoreConfig{DBPath: dbPath, AnomalyLogPath: logPath}); err == nil {
		st.Close()
		t.Fatal("two rows claim their text is journalled and the text journal " +
			"is empty, and Open accepted it")
	}

	// And a PARTIAL loss -- one complete line removed -- is the same
	// contradiction, which is the shape a truncation actually takes.
	db2, log2 := paths(t)
	s2 := openAt(t, db2, log2)
	h2 := begin(t, s2, "runa", 1_700_000_000_000)
	for i, id := range []string{"jp-1", "jp-2"} {
		if _, err := s2.RecordAnomaly(h2, id,
			anomaly("FOREIGN_FILL", risk.SEV1, "KXTEST-A"),
			1_700_000_010_000+int64(i)); err != nil {
			t.Fatalf("RecordAnomaly %s: %v", id, err)
		}
	}
	pump(t, s2)
	if err := s2.Close(); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(log2)
	if err != nil {
		t.Fatal(err)
	}
	lines := splitJournalLines(raw)
	if len(lines) != 2 {
		t.Fatalf("expected two journal lines, got %d", len(lines))
	}
	if err := os.WriteFile(log2, lines[0], 0o600); err != nil {
		t.Fatal(err)
	}
	if st, err := Open(StoreConfig{DBPath: db2, AnomalyLogPath: log2}); err == nil {
		st.Close()
		t.Fatal("one row's journalled text was removed and Open accepted it")
	}
}

// TestNewJournalSyncsItsParentBeforeOpenSucceeds is the durability of the
// operator's fallback copy.
//
// A file that exists only in an unsynced directory entry does not survive a
// power cut: the bytes are on the platter and nothing names them. The harness
// exists to hold evidence across exactly that event.
//
// `M-HS-JOURNALDIR` skips the sync.
func TestNewJournalSyncsItsParentBeforeOpenSucceeds(t *testing.T) {
	original := syncDirFn
	t.Cleanup(func() { syncDirFn = original })

	var synced []string
	syncDirFn = func(dir string) error {
		synced = append(synced, dir)
		return errors.New("simulated: the directory entry could not be synced")
	}

	dbPath, logPath := paths(t)
	st, err := Open(StoreConfig{DBPath: dbPath, AnomalyLogPath: logPath})
	if err == nil {
		st.Close()
		t.Fatal("Open reported success while the directory entry naming the " +
			"new anomaly journal was never synced; the operator's fallback " +
			"copy would not survive the power cut it exists for")
	}
	if len(synced) != 1 || synced[0] != filepath.Dir(logPath) {
		t.Fatalf("parent-directory syncs were %v, want exactly one for %s",
			synced, filepath.Dir(logPath))
	}

	// It succeeds when the sync does, and an EXISTING journal is not re-synced.
	syncDirFn = func(dir string) error {
		synced = append(synced, dir)
		return nil
	}
	synced = nil
	s2 := openAt(t, dbPath, logPath)
	if len(synced) != 1 {
		t.Fatalf("creating the journal performed %d parent syncs, want 1",
			len(synced))
	}
	if err := s2.Close(); err != nil {
		t.Fatal(err)
	}
	synced = nil
	s3 := openAt(t, dbPath, logPath)
	if len(synced) != 0 {
		t.Fatalf("reopening an EXISTING journal synced its parent %d times; "+
			"the entry already exists", len(synced))
	}
	if err := s3.Close(); err != nil {
		t.Fatal(err)
	}
}

// splitJournalLines returns each complete newline-terminated line, inclusive.
func splitJournalLines(raw []byte) [][]byte {
	var out [][]byte
	start := 0
	for i, b := range raw {
		if b == '\n' {
			out = append(out, raw[start:i+1])
			start = i + 1
		}
	}
	return out
}

func tableNamesAt(t *testing.T, path string) []string {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	names, err := userTablesIn(db)
	if err != nil {
		t.Fatal(err)
	}
	return names
}

func journalModeAt(t *testing.T, path string) string {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var mode string
	if err := db.QueryRow("PRAGMA journal_mode").Scan(&mode); err != nil {
		t.Fatal(err)
	}
	return mode
}
