package main

import (
	"bytes"
	"database/sql"
	"errors"
	"flag"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	_ "modernc.org/sqlite"

	"lip/harness/cfg"
	"lip/harness/hstore"
)

// newProvisionConfig is a config whose artifacts live under t.TempDir().
//
// The database sits one level DOWN from the temp root on purpose: creating the
// parent directory is part of what provisioning does, and a fixture that
// pre-creates it would never exercise it.
//
// Nothing here may name a path outside the temp directory. `rig.db` and
// `lip.db` have live collectors writing them and have had since 24 July; a test
// that opened one of them writable would switch its journal mode underneath two
// running writers.
func newProvisionConfig(t *testing.T) config {
	t.Helper()
	d := t.TempDir()
	store := filepath.Join(d, "store")
	return config{
		Params: cfg.Default(),
		Ticker: "KXTEST-A",
		Rung:   rungs["pilot"],
		Paths: paths{
			DB:         filepath.Join(store, "harness.db"),
			AnomalyLog: filepath.Join(store, "anomaly.jsonl"),
			Latch:      filepath.Join(d, "harness.halt"),
			Lock:       filepath.Join(d, "harness.lock"),
			Key:        filepath.Join(d, "kalshi.pem"),
			Env:        filepath.Join(d, "env"),
			LiveOK:     filepath.Join(d, "live_ok"),
		},
	}
}

// TestProvisionRefusesADatabaseThatAlreadyExists is the refusal this file
// exists for.
//
// A second provision over a live ledger has to be impossible rather than
// discouraged. `hstore.Open` on an existing version-2 database is an ordinary,
// successful open -- so a provisioning step that did not check would print a
// record of having created a fresh store, exit zero, and leave the operator
// believing an empty ledger exists where months of `owned_order` rows do. The
// bytes are compared before and after, because "returned an error" is not the
// property: the property is that the file was not touched.
func TestProvisionRefusesADatabaseThatAlreadyExists(t *testing.T) {
	c := newProvisionConfig(t)
	var out bytes.Buffer
	if err := provision(c, &out); err != nil {
		t.Fatalf("first provision: %v", err)
	}

	before, err := os.ReadFile(c.Paths.DB)
	if err != nil {
		t.Fatalf("read provisioned database: %v", err)
	}

	var second bytes.Buffer
	err = provision(c, &second)
	if err == nil {
		t.Fatalf("provisioning a second time over %s SUCCEEDED. Opening an "+
			"existing version-2 database is an ordinary open, so this would "+
			"report a fresh store over the top of a ledger that already "+
			"classifies every fill on the account", c.Paths.DB)
	}
	if !strings.Contains(err.Error(), c.Paths.DB) {
		t.Fatalf("the refusal does not name the path it refused: %v", err)
	}
	if second.Len() != 0 {
		t.Fatalf("a refused provision printed a record of having created "+
			"something: %q", second.String())
	}

	after, err := os.ReadFile(c.Paths.DB)
	if err != nil {
		t.Fatalf("re-read database: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Fatalf("the refused provision MODIFIED %s (%d bytes before, %d "+
			"after); refusing after having written is not refusing",
			c.Paths.DB, len(before), len(after))
	}
}

// TestProvisionRefusesHalfOfSection13Point1sPair covers the other direction.
//
// The database and the anomaly journal are two durable records of the same
// events, kept so that losing one does not lose the other, and `hstore`
// reconciles them against each other at every open. A journal whose database is
// gone is half a pair somebody moved; creating the missing half beside it
// either fails closed on the first line it cannot match or -- if the journal
// happens to be empty -- silently ratifies the loss.
func TestProvisionRefusesHalfOfSection13Point1sPair(t *testing.T) {
	c := newProvisionConfig(t)
	if err := os.MkdirAll(filepath.Dir(c.Paths.AnomalyLog), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(c.Paths.AnomalyLog, nil, 0o600); err != nil {
		t.Fatalf("write journal: %v", err)
	}

	var out bytes.Buffer
	err := provision(c, &out)
	if err == nil {
		t.Fatalf("provisioning created a database beside an orphaned anomaly "+
			"journal at %s", c.Paths.AnomalyLog)
	}
	if !strings.Contains(err.Error(), c.Paths.AnomalyLog) {
		t.Fatalf("the refusal does not name the orphaned journal: %v", err)
	}
	if _, err := os.Stat(c.Paths.DB); err == nil {
		t.Fatalf("%s was created despite the refusal", c.Paths.DB)
	}
}

// TestProvisionedDatabaseIsThePilotFiveWithThePinnedPragmas reads the artifact
// back on a connection of its own.
//
// Independently of `provision`'s own verification, and with the expected values
// written out as literals rather than imported from the code under test: a
// check that reads its expectation out of the thing it is checking checks
// nothing. §15's cut is FIVE tables (pilot-plan §2.3), so the set is enumerated
// rather than probed for presence -- a sixth appearing is a deferred decision
// quietly becoming an implemented one.
func TestProvisionedDatabaseIsThePilotFiveWithThePinnedPragmas(t *testing.T) {
	c := newProvisionConfig(t)
	var out bytes.Buffer
	if err := provision(c, &out); err != nil {
		t.Fatalf("provision: %v", err)
	}

	db, err := sql.Open("sqlite", "file:"+c.Paths.DB+"?_pragma=query_only(1)")
	if err != nil {
		t.Fatalf("open provisioned database: %v", err)
	}
	defer db.Close()

	rows, err := db.Query(
		`SELECT name FROM sqlite_schema
		  WHERE type = 'table' AND name NOT LIKE 'sqlite_%' ORDER BY name`)
	if err != nil {
		t.Fatalf("enumerate tables: %v", err)
	}
	var tables []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			rows.Close()
			t.Fatalf("scan table name: %v", err)
		}
		tables = append(tables, n)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		t.Fatalf("enumerate tables: %v", err)
	}
	rows.Close()

	want := []string{"anomaly", "our_fill", "owned_order", "run", "state_event"}
	if !reflect.DeepEqual(tables, want) {
		t.Fatalf("provisioned tables are %v, want exactly the pilot five %v",
			tables, want)
	}

	var mode string
	if err := db.QueryRow("PRAGMA journal_mode").Scan(&mode); err != nil {
		t.Fatalf("read journal_mode: %v", err)
	}
	if !strings.EqualFold(mode, "wal") {
		t.Fatalf("journal_mode is %q, want WAL", mode)
	}

	var version int64
	if err := db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		t.Fatalf("read user_version: %v", err)
	}
	if version != 2 {
		t.Fatalf("user_version is %d, want 2; a database whose version the "+
			"harness does not know is one whose owned_order rows it cannot "+
			"safely read, and reading one wrong classifies a fill", version)
	}

	// H-ORD-9 in the schema: a fill can only be recorded against an order that
	// was durably bound. Read out of the schema SQLite parsed, not out of the
	// DDL text.
	fkRows, err := db.Query("PRAGMA foreign_key_list(our_fill)")
	if err != nil {
		t.Fatalf("read our_fill foreign keys: %v", err)
	}
	defer fkRows.Close()
	var found bool
	for fkRows.Next() {
		var (
			id, seq                    int64
			table, from                string
			to                         sql.NullString
			onUpdate, onDelete, match_ string
		)
		if err := fkRows.Scan(&id, &seq, &table, &from, &to, &onUpdate,
			&onDelete, &match_); err != nil {
			t.Fatalf("scan foreign key: %v", err)
		}
		if table == "owned_order" && from == "order_id" &&
			to.String == "order_id" {
			found = true
		}
	}
	if err := fkRows.Err(); err != nil {
		t.Fatalf("read our_fill foreign keys: %v", err)
	}
	if !found {
		t.Fatal("our_fill.order_id does not reference owned_order(order_id); " +
			"that reference IS H-ORD-9, and without it a fill can be recorded " +
			"against an order nothing ever bound")
	}

	if !strings.Contains(out.String(), c.Paths.DB) {
		t.Fatalf("the provisioning record does not name the path it created; "+
			"it is the only artefact saying which one it was:\n%s", out.String())
	}
}

// TestProvisionSatisfiesTheRefusalTheRunPathMakes joins the two halves.
//
// `requireExistingDB` refuses to start a harness against a path that is not
// there, and its message tells the operator to run this. The pair is only
// coherent if what this produces is what that accepts -- and if the store the
// run path then OPENS is openable, which is the step `Open` would otherwise
// have satisfied by creating an empty ledger.
func TestProvisionSatisfiesTheRefusalTheRunPathMakes(t *testing.T) {
	c := newProvisionConfig(t)
	if err := requireExistingDB(c.Paths.DB); err == nil {
		t.Fatalf("requireExistingDB accepted %s before it existed",
			c.Paths.DB)
	}

	var out bytes.Buffer
	if err := provision(c, &out); err != nil {
		t.Fatalf("provision: %v", err)
	}
	if err := requireExistingDB(c.Paths.DB); err != nil {
		t.Fatalf("requireExistingDB refuses the store provision just made, so "+
			"the refusal and the act it names disagree: %v", err)
	}

	store, err := hstore.Open(hstore.StoreConfig{
		DBPath:         c.Paths.DB,
		AnomalyLogPath: c.Paths.AnomalyLog,
	})
	if err != nil {
		t.Fatalf("the run path cannot open the provisioned store: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
}

// TestProvisionNeverCreatesLiveOK keeps setting the harness up and letting it
// trade separate acts (H-VER-1).
//
// `provision` creates everything else the process needs: the store, the anomaly
// journal, their directories. If it created the arming sentinel too, then a
// machine that had been set up would be a machine that was armed, and the second
// key would be present for every future invocation on it -- which is the whole
// failure the sentinel exists to prevent, arriving through the one command an
// operator runs without thinking about writes at all.
func TestProvisionNeverCreatesLiveOK(t *testing.T) {
	c := newProvisionConfig(t)
	var out bytes.Buffer
	if err := provision(c, &out); err != nil {
		t.Fatalf("provision: %v", err)
	}

	// The store really was created, so this is not passing by doing nothing.
	if _, err := os.Stat(c.Paths.DB); err != nil {
		t.Fatalf("provision did not create the store: %v", err)
	}
	if _, err := os.Stat(c.Paths.LiveOK); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("provision created (or found) the live_ok sentinel at %s. "+
			"Provisioning is how a machine is prepared; arming is a separate "+
			"sentence a human types afterwards, having looked at what was "+
			"prepared", c.Paths.LiveOK)
	}
}

// TestProvisionWithLiveRefusesBeforeCreatingArtifacts is the same rule enforced
// at the command line, and it refuses EARLY on purpose.
//
// `-provision -live` reads as "set it up and let it trade". The refusal happens
// before the config is even loaded, so a mistaken invocation leaves no database,
// no journal, and no directories behind -- the operator retries the correct
// command against a clean machine rather than against the debris of the wrong
// one.
func TestProvisionWithLiveRefusesBeforeCreatingArtifacts(t *testing.T) {
	c := newProvisionConfig(t)
	d := t.TempDir()
	cfgPath := filepath.Join(d, "config.json")
	if err := os.WriteFile(cfgPath, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}

	fs := flag.NewFlagSet("harness", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	err := run(fs, cfgPath, "", "", true, false, false, true)

	var ref *refusal
	if !errors.As(err, &ref) {
		t.Fatalf("-provision -live returned %v, want a refusal: launchd's "+
			"KeepAlive retries a failure forever, and this is a statement that "+
			"the invocation should not run at all", err)
	}
	if _, statErr := os.Stat(c.Paths.DB); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatal("the refused -provision -live left a database behind")
	}
}
