package main

import (
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"

	// The driver registers itself from its init. `hstore` already links it, so
	// this import is redundant to the linker and not to the reader: a
	// verification that reaches `sql.Open("sqlite", ...)` on the strength of
	// somebody else's import stops working the day that import moves, and it
	// stops working by failing to open the file it was checking.
	_ "modernc.org/sqlite"

	"lip/harness/hstore"
)

// This file is the ONLY place a harness database is allowed to come into
// existence, and it is a separate operator act rather than a step of starting
// up.
//
// `hstore.Open` creates the five-table schema when the file is absent, and
// `hstore.StoreConfig.DBPath` states the resulting rule in one line -- "It is
// opened, never created fresh and never deleted". The reason is that an absent
// path and a MISTYPED path are the same thing to `Open`. The ledger it would
// create for either recognises no order id at all, so §7.5's adoption walk
// reads this harness's own resting orders as foreign activity on a dedicated
// account (H-ORD-9), latches a durable global stop, and pages the operator
// about a third party that does not exist. Afterwards the file looks exactly
// like a legitimate first run, so nothing downstream can detect what happened.
//
// `requireExistingDB` in `runtime.go` is the refusal that keeps the run path
// clean. This file is the other half of that bargain: the deliberate act it
// points at. Everything below is either a REFUSAL or a VERIFICATION -- the
// schema itself belongs to `hstore` and is not restated here, because two
// copies of a schema are two schemas.

// provisionDirMode is the mode of any directory this step has to create.
//
// 0700 rather than 0755: what goes underneath it is the ownership ledger of a
// live trading account and the operator's copy of everything that has gone
// wrong on it. `hstore` opens the anomaly journal 0600 for exactly that reason,
// and a world-readable directory around it puts the same information one `ls`
// away.
const provisionDirMode = 0o700

// pilotTables is §15's cut as this step expects to find it, RESTATED.
//
// `hstore.userTables` is unexported, and this is deliberately not a copy
// reached by an import even if it were: a verification that reads its
// expectation out of the artefact it is verifying verifies nothing. If the two
// ever disagree, provisioning refuses while an operator is standing in front of
// it -- the one moment in this system's life when a schema surprise is cheap.
var pilotTables = []string{
	"anomaly", "our_fill", "owned_order", "run", "state_event",
}

// pilotSchemaVersion and pilotJournalMode are `PRAGMA user_version` and
// `PRAGMA journal_mode`, restated for the same reason.
//
// Both are properties of the FILE and survive the connection that set them,
// which is what makes them checkable from here at all. `synchronous` and
// `foreign_keys` are per-CONNECTION -- `hstore.writerDSN` carries them and
// `hstore.Open` reads them back off the write connection before returning --
// so asking a fresh handle about them would report this handle's settings and
// prove nothing about the durability of a write. What IS checkable about
// `foreign_keys` from here is the reference it enforces, and that is checked
// below instead.
const (
	pilotSchemaVersion = 2
	pilotJournalMode   = "wal"
)

// provision is `harness -provision`: create the store, once, on purpose.
//
// `out` is an argument rather than `os.Stdout` because the printed record is
// part of what this produces. An operator who provisioned the wrong path has
// exactly one artefact saying which path it was, and a test that cannot read it
// cannot assert that the artefact exists.
func provision(c config, out io.Writer) error {
	if err := refuseExistingArtifacts(c.Paths); err != nil {
		return err
	}

	// The directories, before the store. `hstore.Open` reports a missing parent
	// as an unopenable database, which reads to an operator as a corrupt file
	// rather than as a directory that was never made.
	for _, dir := range []string{
		filepath.Dir(c.Paths.DB), filepath.Dir(c.Paths.AnomalyLog),
	} {
		if err := os.MkdirAll(dir, provisionDirMode); err != nil {
			return fmt.Errorf("creating %s: %w", dir, err)
		}
	}

	store, err := hstore.Open(hstore.StoreConfig{
		DBPath:         c.Paths.DB,
		AnomalyLogPath: c.Paths.AnomalyLog,
	})
	if err != nil {
		return fmt.Errorf("creating the store at %s: %w", c.Paths.DB, err)
	}

	// The table set is read through the store's own reader, while it is open,
	// and the store is then CLOSED before anything else is asked of the file.
	// Both errors are collected before either is returned: a close failure on a
	// database this call has just created is the more serious of the two, and
	// returning early on the read would leave the connection open in a process
	// that is about to report success.
	tables, tablesErr := store.Reader().Tables()
	closeErr := store.Close()
	if tablesErr != nil {
		return fmt.Errorf("reading back the schema of %s: %w",
			c.Paths.DB, tablesErr)
	}
	if closeErr != nil {
		return fmt.Errorf("the store at %s was created and would not close "+
			"cleanly, so what is on disk is not what a run would open: %w",
			c.Paths.DB, closeErr)
	}
	if !reflect.DeepEqual(tables, pilotTables) {
		return fmt.Errorf("%s was created with tables %v, not the pilot five "+
			"%v; §15's cut is five records and this process does not know what "+
			"the rows of a sixth mean", c.Paths.DB, tables, pilotTables)
	}

	facts, err := readDatabaseFacts(c.Paths.DB)
	if err != nil {
		return err
	}
	if !strings.EqualFold(facts.journalMode, pilotJournalMode) {
		return fmt.Errorf("%s reads back journal_mode %q, want %s; without WAL "+
			"the reader and the single writer serialise against each other and "+
			"a crash leaves a rollback journal this schema was not designed "+
			"around", c.Paths.DB, facts.journalMode, pilotJournalMode)
	}
	if facts.userVersion != pilotSchemaVersion {
		return fmt.Errorf("%s reads back user_version %d, want %d; a database "+
			"whose version this process does not know is one whose owned_order "+
			"rows it cannot safely read, and reading one wrong classifies a "+
			"fill", c.Paths.DB, facts.userVersion, pilotSchemaVersion)
	}
	if !facts.fillReferencesOwnedOrder {
		return fmt.Errorf("%s was created without our_fill.order_id REFERENCES "+
			"owned_order(order_id); that reference IS H-ORD-9 -- ownership is a "+
			"fact in the ledger and never an inference at the fill -- and "+
			"without it a fill can be recorded against an order nothing ever "+
			"bound", c.Paths.DB)
	}

	// The journal is the half of §13.1 the operator reads when the database
	// will not open, so its existence is asserted rather than assumed from
	// `Open` having returned.
	logInfo, err := os.Stat(c.Paths.AnomalyLog)
	if err != nil {
		return fmt.Errorf("the anomaly journal %s was not created: %w",
			c.Paths.AnomalyLog, err)
	}

	return writeProvisionReport(out, c, tables, facts, logInfo.Mode().Perm())
}

// refuseExistingArtifacts is the refusal this whole file exists for.
//
// Provisioning is a FIRST-RUN act. Re-running it over a live ledger has to be
// impossible rather than discouraged: `hstore.Open` on an existing version-2
// database is a perfectly ordinary open, so a second `-provision` would report
// success, print a record of having created something, and leave the operator
// believing a fresh store exists where months of `owned_order` rows actually
// do.
//
// The database and the anomaly journal are also refused SEPARATELY when only
// one of them is there. They are two durable records of the same events, kept
// so that losing one does not lose the other, and `hstore` reconciles them
// against each other at every open -- a journal with no database, or a database
// with no journal, is half of a pair whose other half somebody moved, and
// creating the missing one silently ratifies the loss. (`distinctArtifacts`
// already refuses the case where they are the same file.)
func refuseExistingArtifacts(p paths) error {
	dbInfo, dbErr := os.Stat(p.DB)
	switch {
	case dbErr == nil:
		what := "a file"
		if dbInfo.IsDir() {
			what = "a DIRECTORY"
		}
		return fmt.Errorf("refusing to provision: %s already exists (%s, %d "+
			"bytes).\n\n"+
			"Creation is a first-run act and this is not a first run. Opening "+
			"an existing version-2 database is an ordinary open, so a second "+
			"provision would report success over the top of a ledger that "+
			"already classifies every fill on the account. If this really is a "+
			"fresh deployment, move %s aside deliberately and by hand; there "+
			"is no flag for it here",
			p.DB, what, dbInfo.Size(), p.DB)
	case !errors.Is(dbErr, os.ErrNotExist):
		return fmt.Errorf("%s could not be examined: %w", p.DB, dbErr)
	}

	_, logErr := os.Stat(p.AnomalyLog)
	switch {
	case logErr == nil:
		return fmt.Errorf("refusing to provision: the anomaly journal %s "+
			"already exists but the database %s does not.\n\n"+
			"§13.1's two journals are two records of the same events, and they "+
			"are reconciled against each other at every open. A journal whose "+
			"database is gone is half a pair: creating a fresh database "+
			"beside it would either fail closed on the first line it cannot "+
			"match, or -- if the journal is empty -- silently ratify the loss "+
			"of the other half. Work out which artefact moved before creating "+
			"anything", p.AnomalyLog, p.DB)
	case !errors.Is(logErr, os.ErrNotExist):
		return fmt.Errorf("%s could not be examined: %w", p.AnomalyLog, logErr)
	}
	return nil
}

// databaseFacts is what can be read back about the FILE once it is closed.
type databaseFacts struct {
	journalMode string
	userVersion int64
	// fillReferencesOwnedOrder is `our_fill.order_id -> owned_order.order_id`,
	// read out of the schema SQLite parsed rather than out of the DDL text.
	fillReferencesOwnedOrder bool
}

// readDatabaseFacts opens a query-only connection and reads the file back.
//
// It runs AFTER the store has closed, on a connection of its own, and that is
// the point: everything it checks is a property the file carries independently
// of whoever created it, so a bug in this process's own write path cannot make
// the check pass. `query_only` is carried in the DSN for the reason
// `hstore.readerDSN` documents -- the pragma is per-connection and
// `database/sql` is a pool that opens replacements whenever it likes.
func readDatabaseFacts(path string) (databaseFacts, error) {
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=query_only(1)")
	if err != nil {
		return databaseFacts{}, fmt.Errorf("re-opening %s to verify it: %w",
			path, err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)

	var f databaseFacts
	if err := db.QueryRow("PRAGMA journal_mode").Scan(&f.journalMode); err != nil {
		return databaseFacts{}, fmt.Errorf("reading journal_mode of %s: %w",
			path, err)
	}
	if err := db.QueryRow("PRAGMA user_version").Scan(&f.userVersion); err != nil {
		return databaseFacts{}, fmt.Errorf("reading user_version of %s: %w",
			path, err)
	}

	rows, err := db.Query("PRAGMA foreign_key_list(our_fill)")
	if err != nil {
		return databaseFacts{}, fmt.Errorf("reading our_fill's foreign keys "+
			"in %s: %w", path, err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			id, seq                        int64
			table, from                    string
			to                             sql.NullString
			onUpdate, onDelete, matchClaus string
		)
		if err := rows.Scan(&id, &seq, &table, &from, &to, &onUpdate,
			&onDelete, &matchClaus); err != nil {
			return databaseFacts{}, fmt.Errorf("reading our_fill's foreign "+
				"keys in %s: %w", path, err)
		}
		if table == "owned_order" && from == "order_id" &&
			to.String == "order_id" {
			f.fillReferencesOwnedOrder = true
		}
	}
	if err := rows.Err(); err != nil {
		return databaseFacts{}, fmt.Errorf("reading our_fill's foreign keys "+
			"in %s: %w", path, err)
	}
	return f, nil
}

// writeProvisionReport prints what was created, in one write.
//
// One write rather than a series of `Fprintf`s so that the error has somewhere
// to go. A provisioning step whose output was truncated has left the operator
// with a database and no record of which path it is at, which is the same
// missing fact this whole file exists to protect.
func writeProvisionReport(out io.Writer, c config, tables []string,
	f databaseFacts, logMode os.FileMode) error {

	var b strings.Builder
	fmt.Fprintf(&b, "provisioned the harness store for %s (rung %s)\n",
		c.Ticker, c.Rung.name)
	fmt.Fprintf(&b, "  database      %s\n", c.Paths.DB)
	fmt.Fprintf(&b, "  anomaly log   %s (mode %#o)\n", c.Paths.AnomalyLog,
		logMode)
	fmt.Fprintf(&b, "  tables        %s\n", strings.Join(tables, ", "))
	fmt.Fprintf(&b, "  user_version  %d\n", f.userVersion)
	fmt.Fprintf(&b, "  journal_mode  %s\n", f.journalMode)
	fmt.Fprintf(&b, "  our_fill.order_id -> owned_order(order_id): declared\n")
	b.WriteString("\nThis will not run again over this file. Every later " +
		"start OPENS it: a fresh\nledger recognises no order id, and one " +
		"created by accident makes the harness's\nown resting orders read as " +
		"a third party's (H-ORD-9).\n")

	_, err := io.WriteString(out, b.String())
	if err != nil {
		return fmt.Errorf("the store was created but the record of it could "+
			"not be printed, so nothing says which path was provisioned: %w",
			err)
	}
	return nil
}

// confidence: high
