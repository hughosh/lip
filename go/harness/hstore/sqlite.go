package hstore

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"

	sqlitedrv "modernc.org/sqlite"

	"lip/harness/cfg"
	"lip/harness/num"
	"lip/harness/risk"
)

// ---------------------------------------------------------------------------
// The write backend
// ---------------------------------------------------------------------------

// backend is the durable side of one record, and it exists as an interface for
// exactly one reason: the failure paths this store is judged on -- a write that
// blocks forever, a disk that is full -- cannot be produced deterministically by
// a real SQLite file. A test injects a backend that blocks or fails on demand,
// and `TestAuditQueueNeverDropsWhileWriterIsStalled` becomes a statement about
// the queue rather than about the machine it ran on.
//
// It is unexported. There is no public way to substitute storage, because a
// substitutable durable ledger is a ledger that can be substituted for one that
// remembers nothing.
type backend interface {
	beginRun(runRecord) error
	reserveOrder(orderReservation) error
	bindOrder(orderBinding) error
	recordFill(fillRecord) error
	recordState(runID string, ev StateEvent) error
	insertAnomaly(anomalyRecord) error
	markJournaled(anomalyID string, journaledMs int64) error
	recordDelivery(DeliveryAttempt) error
	abandonReservation(coid string, abandonedMs int64) error
	loadLedger() (bindings map[string]string, unresolved map[string]struct{},
		err error)
	anomalyJournalStates() ([]anomalyJournalState, error)
	anomalyByID(anomalyID string) (anomalyRecord, bool, error)
	pragmas() (Pragmas, error)
	close() error
}

// Pragmas is the pinned SQLite configuration, read back from the WRITE
// connection. `TestSchemaIsExactlyThePilotFiveAndPragmasArePinned` asserts every
// field, which is what makes `M-HS-PRAGMA` a mutation rather than a preference.
type Pragmas struct {
	JournalMode string
	// Synchronous is SQLite's numeric encoding: 0 OFF, 1 NORMAL, 2 FULL.
	Synchronous int64
	ForeignKeys int64
	UserVersion int64
}

// anomalyJournalState is one anomaly row and whether the database believes its
// text journal line is durable.
type anomalyJournalState struct {
	rec       anomalyRecord
	journaled bool
}

// sqliteBackend is the real one: a single writable connection.
type sqliteBackend struct {
	db *sql.DB
}

// openSQLite opens or creates the operational database.
//
// It never deletes and never recreates. `store.Create` exists in the RIG store
// for replay runs that must start clean; there is deliberately no analogue here,
// because the one file this package owns is the record of what a live account
// did, and a "start clean" path is one mistaken flag away from erasing it.
func openSQLite(path string) (*sqliteBackend, error) {
	if !filepath.IsAbs(path) {
		return nil, fmt.Errorf("database path %q is not absolute: a relative "+
			"path makes the ownership ledger depend on the working directory "+
			"of whatever started the process, and a harness that opened the "+
			"wrong file would classify every fill as foreign", path)
	}
	// STAT FIRST, and before any connection exists. Opening writable CREATES a
	// zero-length file, after which "was this file already here" is a question
	// nothing can answer -- so the one check that needs the answer runs first.
	fresh, err := statFresh(path)
	if err != nil {
		return nil, err
	}

	db, err := sql.Open("sqlite", writerDSN(path))
	if err != nil {
		return nil, err
	}
	// One writable connection. §15's tables are licences, not a log: two
	// writers make ownership racy in the direction that reads "not ours".
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)

	b := &sqliteBackend{db: db}
	// INSPECT ON THIS CONNECTION, before a single writable statement.
	//
	// Setting `journal_mode=WAL` is itself a write, and running it on a
	// mistyped path is how this process would put its own schema and its own
	// journal mode into `lip/rig.db` -- a file two evidence collectors are
	// writing, that H-ORD-7 makes read-only to the harness, and that no
	// subsequent rejection can un-modify.
	//
	// On THIS connection and not a separate read-only one, which is the whole
	// of the change: a verdict reached over one handle and acted on through
	// another is a verdict about a file that may no longer be the file being
	// written. Opening the connection is not a write, so verify-then-write on
	// a single handle costs nothing and leaves no window.
	if !fresh {
		fresh, err = inspectDatabase(db, path)
		if err != nil {
			db.Close()
			return nil, err
		}
	}
	if err := b.applyJournalMode(); err != nil {
		db.Close()
		return nil, err
	}
	if fresh {
		if err := b.createSchema(); err != nil {
			db.Close()
			return nil, err
		}
	}
	if err := b.assertPragmas(); err != nil {
		db.Close()
		return nil, err
	}
	return b, nil
}

// writerDSN carries the CONNECTION-LOCAL pragmas.
//
// `synchronous` and `foreign_keys` are per-connection settings, and
// `database/sql` is a POOL: it may close an idle connection and open a
// replacement at any time. A `PRAGMA` executed once after `sql.Open` therefore
// configures whichever connection happened to serve that statement, and the
// replacement silently arrives with SQLite's defaults -- `foreign_keys=OFF`,
// under which `our_fill.order_id REFERENCES owned_order(order_id)` stops being
// enforced and a fill can be recorded against an order we never bound.
// modernc's `_pragma` DSN parameter runs them on every connection it opens.
func writerDSN(path string) string {
	return "file:" + path +
		"?_pragma=synchronous(" + pragmaSynchronous + ")" +
		"&_pragma=foreign_keys(" + pragmaForeignKeys + ")" +
		"&_pragma=busy_timeout(0)"
}

// statFresh reports whether this path names a file that does not exist yet.
//
// FRESH IS NOT-EXIST, and nothing else. A zero-length file is refused rather
// than adopted: SQLite will happily start writing into one, but it is not a
// file this process created. A `>` in the wrong shell, a copy that ran out of
// disk, a restore that produced nothing, or this process's own failure between
// creating the file and its first write all leave exactly this artifact --
// and creating a fresh schema over the top of it produces an EMPTY ownership
// ledger, under which no order id is recognised and every fill the account has
// ever produced classifies as somebody else's (H-ORD-9). A blank ledger reads
// exactly like a clean start, which is why the operator has to be told instead.
func statFresh(path string) (bool, error) {
	info, err := os.Stat(path)
	if os.IsNotExist(err) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	if info.Size() == 0 {
		return false, fmt.Errorf("%s exists and is zero bytes long: this "+
			"process did not leave it that way after a successful open, so "+
			"something else made it -- a truncating redirect, a copy that ran "+
			"out of disk, or a restore that produced nothing. Creating a "+
			"schema in it would hand this run an EMPTY ownership ledger, "+
			"which recognises no order id and makes every fill on the account "+
			"foreign (H-ORD-9). Move %s aside and let a fresh one be created, "+
			"or restore the real one", path, path)
	}
	return false, nil
}

// inspectDatabase reads an existing file WITHOUT writing to it, and reports
// whether a fresh schema must be created.
//
// It runs on the WRITABLE connection, before any write statement. A verdict
// reached over a separate read-only handle and then acted on through this one
// is a verdict about a file that may have been replaced in between; opening a
// connection is not a write, so there is nothing to be gained by inspecting
// anywhere else.
//
// The two rejections are different failures:
//
//   - `user_version = 0` with any user table in it is somebody else's database.
//     Version zero is SQLite's default, so "version zero" alone means nothing;
//     it is the presence of tables that says the file is in use. This is the
//     `rig.db` case.
//   - `user_version = 1` whose table set is not exactly the pilot five is a
//     database this schema does not describe. §15's cut is five, and a sixth
//     table -- or a missing one -- means either an implementation we do not
//     have or a corruption we cannot interpret.
func inspectDatabase(db *sql.DB, path string) (fresh bool, err error) {
	var version int64
	if err := db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		return false, fmt.Errorf("%s could not be read as a database: %w",
			path, err)
	}
	tables, err := userTablesIn(db)
	if err != nil {
		return false, err
	}

	switch version {
	case 0:
		if len(tables) > 0 {
			return false, fmt.Errorf("%s is an existing database with no "+
				"harness schema version and %d table(s) already in it (%v); "+
				"refusing to write to it. H-ORD-7 makes the evidence "+
				"collectors' database read-only to the harness, and creating "+
				"this schema in it would also switch its journal mode "+
				"underneath two running writers", path, len(tables), tables)
		}
		return true, nil
	case schemaVersion:
		if !reflect.DeepEqual(tables, userTables) {
			return false, fmt.Errorf("%s declares schema version %d but its "+
				"tables are %v, not the pilot five %v; this process does not "+
				"know what its rows mean, and reading an owned_order row wrong "+
				"classifies a fill", path, version, tables, userTables)
		}
		return false, nil
	case legacySchemaVersion:
		return false, fmt.Errorf("%s is a version-%d harness database and this "+
			"process writes version %d; there is no migration. Version %d's "+
			"owned_order has no abandoned_ms, so every reservation in it that "+
			"was never bound is indistinguishable from one the exchange "+
			"refused -- reading the file would make the ownership answer for "+
			"an unrecognised order id depend on which of those it was. Nothing "+
			"is deployed on version %d: move %s aside and let a fresh one be "+
			"created", path, version, schemaVersion, legacySchemaVersion,
			legacySchemaVersion, path)
	default:
		return false, fmt.Errorf("database schema version %d is not %d: this "+
			"process does not know what its owned_order rows mean",
			version, schemaVersion)
	}
}

func userTablesIn(db *sql.DB) ([]string, error) {
	rows, err := db.Query(
		`SELECT name FROM sqlite_schema
		  WHERE type = 'table' AND name NOT LIKE 'sqlite_%' ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.Strings(out)
	return out, nil
}

// applyJournalMode sets WAL. It is a DATABASE property rather than a connection
// one, so it is set once and persists.
func (b *sqliteBackend) applyJournalMode() error {
	var mode string
	if err := b.db.QueryRow(
		"PRAGMA journal_mode=" + pragmaJournalMode).Scan(&mode); err != nil {
		return fmt.Errorf("set journal_mode=%s: %w", pragmaJournalMode, err)
	}
	if !strings.EqualFold(mode, pragmaJournalMode) {
		return fmt.Errorf("journal_mode is %q after asking for %s", mode,
			pragmaJournalMode)
	}
	return nil
}

// createSchema creates the five tables and pins the version ATOMICALLY.
//
// One transaction, so a crash between the tables and the version cannot leave a
// database that looks like somebody else's version-zero file -- which the
// inspection above would then correctly refuse to touch forever.
func (b *sqliteBackend) createSchema() error {
	return b.tx(func(tx *sql.Tx) error {
		if _, err := tx.Exec(Schema); err != nil {
			return fmt.Errorf("apply schema: %w", err)
		}
		if _, err := tx.Exec(
			fmt.Sprintf("PRAGMA user_version=%d", schemaVersion)); err != nil {
			return fmt.Errorf("pin user_version: %w", err)
		}
		return nil
	})
}

// assertPragmas is the read-back. Asking for a setting and assuming it took is
// the same class of mistake as an unverified cancel (H-FAIL-3).
func (b *sqliteBackend) assertPragmas() error {
	p, err := b.pragmas()
	if err != nil {
		return err
	}
	if !strings.EqualFold(p.JournalMode, pragmaJournalMode) {
		return fmt.Errorf("journal_mode reads back as %q", p.JournalMode)
	}
	if p.ForeignKeys != 1 {
		return fmt.Errorf("foreign_keys reads back as %d; our_fill's reference "+
			"to a bound owned_order would not be enforced", p.ForeignKeys)
	}
	if p.UserVersion != schemaVersion {
		return fmt.Errorf("user_version reads back as %d, want %d",
			p.UserVersion, schemaVersion)
	}
	return nil
}

func (b *sqliteBackend) close() error { return b.db.Close() }

// sqliteConstraint is SQLITE_CONSTRAINT. Extended codes are `19 | (n << 8)`, so
// the primary code is the low byte.
const sqliteConstraint = 19

// classify marks a constraint violation PERMANENT.
//
// A foreign key that does not resolve, a primary key that already exists with
// other content, a CHECK that rejects a `none` trigger: none of those become
// true by waiting. Retrying one forever at the head of a strictly ordered queue
// would block every audit record behind it for the life of the process, so the
// record is failed, reported to its submitter, and the queue moves on.
// Everything else -- a full disk, a locked file, an I/O error -- is transient
// and is retried indefinitely, because the record is evidence.
func classify(err error) error {
	if err == nil {
		return nil
	}
	var serr *sqlitedrv.Error
	if errors.As(err, &serr) && serr.Code()&0xff == sqliteConstraint {
		return permanentError{err: err}
	}
	return err
}

// pragmas reads the pinned settings back from the write connection.
func (b *sqliteBackend) pragmas() (Pragmas, error) {
	var p Pragmas
	for _, q := range []struct {
		stmt string
		dst  any
	}{
		{"PRAGMA journal_mode", &p.JournalMode},
		{"PRAGMA synchronous", &p.Synchronous},
		{"PRAGMA foreign_keys", &p.ForeignKeys},
		{"PRAGMA user_version", &p.UserVersion},
	} {
		if err := b.db.QueryRow(q.stmt).Scan(q.dst); err != nil {
			return Pragmas{}, fmt.Errorf("%s: %w", q.stmt, err)
		}
	}
	return p, nil
}

// anomalyByID reads one anomaly's immutable half, for crash reconciliation.
func (b *sqliteBackend) anomalyByID(id string) (anomalyRecord, bool, error) {
	var a anomalyRecord
	err := b.db.QueryRow(
		`SELECT anomaly_id, run_id, class, sev, ticker, text, first_ms
		   FROM anomaly WHERE anomaly_id = ?`, id).Scan(
		&a.AnomalyID, &a.RunID, &a.Class, &a.Sev, &a.Ticker, &a.Text, &a.FirstMs)
	if errors.Is(err, sql.ErrNoRows) {
		return anomalyRecord{}, false, nil
	}
	if err != nil {
		return anomalyRecord{}, false, err
	}
	return a, true, nil
}

// tx runs fn in a transaction, rolling back on any error.
func (b *sqliteBackend) tx(fn func(*sql.Tx) error) error {
	tx, err := b.db.Begin()
	if err != nil {
		return classify(err)
	}
	if err := fn(tx); err != nil {
		tx.Rollback()
		return classify(err)
	}
	return classify(tx.Commit())
}

// beginRun writes the §15 `run` row, encoding cfg.Params losslessly.
//
// A repeat with identical content succeeds; a repeat with DIFFERENT content is
// an error. That asymmetry is the whole of the idempotence rule: a retry of a
// write we are not sure landed must be safe, and a second run reusing a run id
// with different parameters must not silently keep the first one's record --
// every piece of evidence downstream is interpreted through that row.
func (b *sqliteBackend) beginRun(r runRecord) error {
	return b.tx(func(tx *sql.Tx) error {
		var startedMs int64
		var configJSON []byte
		err := tx.QueryRow(
			`SELECT started_ms, config_json FROM run WHERE run_id = ?`,
			r.RunID).Scan(&startedMs, &configJSON)
		switch {
		case err == nil:
			if startedMs != r.StartedMs || !bytes.Equal(configJSON, r.ConfigJSON) {
				return permanent("run %s already exists with different "+
					"content; a run row is how every record beneath it is "+
					"interpreted, so an id collision is an error and not an "+
					"update", r.RunID)
			}
			return nil
		case errors.Is(err, sql.ErrNoRows):
		default:
			return err
		}
		_, err = tx.Exec(
			`INSERT INTO run (run_id, started_ms, config_json) VALUES (?, ?, ?)`,
			r.RunID, r.StartedMs, r.ConfigJSON)
		return err
	})
}

// reserveOrder is stage one of `owned_order`: the coid becomes durable before
// the order is dispatched (H-ORD-6).
func (b *sqliteBackend) reserveOrder(r orderReservation) error {
	return b.tx(func(tx *sql.Tx) error {
		var got orderReservation
		err := tx.QueryRow(
			`SELECT run_id, reserved_ms, ticker, side, role, price_cents, count_q
			   FROM owned_order WHERE coid = ?`, r.Coid).Scan(
			&got.RunID, &got.ReservedMs, &got.Ticker, &got.Side, &got.Role,
			&got.PriceCents, &got.CountQ)
		switch {
		case err == nil:
			got.Coid = r.Coid
			if got != r {
				return permanent("coid %s is already reserved with different "+
					"content (%+v vs %+v); H-ORD-1 permits at most one order "+
					"per coid, so this is a collision and not a retry",
					r.Coid, got, r)
			}
			return nil
		case errors.Is(err, sql.ErrNoRows):
		default:
			return err
		}
		_, err = tx.Exec(
			`INSERT INTO owned_order
			   (coid, run_id, reserved_ms, ticker, side, role, price_cents,
			    count_q, order_id, bound_ms)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, NULL, NULL)`,
			r.Coid, r.RunID, r.ReservedMs, r.Ticker, r.Side, r.Role,
			r.PriceCents, r.CountQ)
		return err
	})
}

// bindOrder is stage two: the exchange's order id, learned from an ACK, a
// same-coid recovery, or an adopted resting-order walk.
//
// The four rules are the directive's, and each closes a different way for the
// ledger to start lying:
//
//   - unknown coid: an order id bound to a reservation that never committed
//     would make a dispatch we have no record of look owned.
//   - same mapping: idempotent, because a recovery walk re-learns bindings we
//     already have and must not fail for it.
//   - coid already mapped differently: one coid is at most one order (H-ORD-1).
//   - order id already mapped to another coid: one order is at most one coid,
//     or `our_fill`'s foreign key points at whichever row won the race.
func (b *sqliteBackend) bindOrder(bind orderBinding) error {
	return b.tx(func(tx *sql.Tx) error {
		var (
			existingID  sql.NullString
			abandonedMs sql.NullInt64
		)
		err := tx.QueryRow(
			`SELECT order_id, abandoned_ms FROM owned_order WHERE coid = ?`,
			bind.Coid).Scan(&existingID, &abandonedMs)
		if errors.Is(err, sql.ErrNoRows) {
			return permanent("coid %s has no reservation, so there is nothing "+
				"to bind order %s to; H-ORD-6 reserves before dispatch and a "+
				"binding without one records an order we never claimed",
				bind.Coid, bind.OrderID)
		}
		if err != nil {
			return err
		}
		if abandonedMs.Valid {
			// The mirror of `abandonReservation`'s bound-coid refusal. This
			// ledger has already recorded, terminally, that the exchange never
			// took this coid; a binding arriving afterwards says it did. One of
			// the two is wrong, and a store that resolved the disagreement by
			// preferring the later writer would be deciding which of its own
			// durable records to believe.
			return permanent("coid %s was recorded as abandoned at %d and "+
				"cannot now be bound to order %s; the ledger already concluded "+
				"the exchange never took this reservation", bind.Coid,
				abandonedMs.Int64, bind.OrderID)
		}
		if existingID.Valid {
			if existingID.String == bind.OrderID {
				return nil
			}
			return permanent("coid %s is already bound to order %s and cannot "+
				"be rebound to %s", bind.Coid, existingID.String, bind.OrderID)
		}

		var otherCoid string
		err = tx.QueryRow(`SELECT coid FROM owned_order WHERE order_id = ?`,
			bind.OrderID).Scan(&otherCoid)
		switch {
		case err == nil:
			return permanent("order %s is already bound to coid %s and cannot "+
				"also be bound to %s", bind.OrderID, otherCoid, bind.Coid)
		case errors.Is(err, sql.ErrNoRows):
		default:
			return err
		}

		_, err = tx.Exec(
			`UPDATE owned_order SET order_id = ?, bound_ms = ? WHERE coid = ?`,
			bind.OrderID, bind.BoundMs, bind.Coid)
		return err
	})
}

// abandonReservation is the OTHER terminal answer for a reservation: the
// exchange never took it, so no order id will ever exist to bind.
//
// It is a durable record and not a deletion. A reservation that committed is
// evidence that this process was about to dispatch, and `lip-eyq`'s startup walk
// concludes only when every outstanding reservation has been resolved one way or
// the other -- so the resolution has to survive the restart that the walk runs
// after. Deleting the row would make an abandoned reservation and one that was
// never made look identical, which is the same erasure `Open` refuses elsewhere.
//
// The four rules mirror `bindOrder`'s:
//
//   - unknown coid: abandoning a reservation that never committed records a
//     conclusion about a dispatch we have no evidence of.
//   - already bound: the exchange demonstrably DID take it. Permanent.
//   - already abandoned: idempotent, because `lip-eyq`'s walk re-runs on every
//     startup and must not fail for re-reaching the same conclusion. The FIRST
//     timestamp stands -- when we concluded it is a fact about the run that
//     concluded it.
//   - otherwise: stamp it.
func (b *sqliteBackend) abandonReservation(coid string, abandonedMs int64) error {
	return b.tx(func(tx *sql.Tx) error {
		var (
			existingID   sql.NullString
			existingAbnd sql.NullInt64
		)
		err := tx.QueryRow(
			`SELECT order_id, abandoned_ms FROM owned_order WHERE coid = ?`,
			coid).Scan(&existingID, &existingAbnd)
		if errors.Is(err, sql.ErrNoRows) {
			return permanent("coid %s has no reservation, so there is nothing "+
				"to abandon; a conclusion recorded about a dispatch this store "+
				"has no record of is a row nobody can interpret", coid)
		}
		if err != nil {
			return err
		}
		if existingID.Valid {
			return permanent("coid %s is bound to order %s and cannot be "+
				"abandoned; the exchange took this order, and a ledger that "+
				"said otherwise would make its own fills foreign", coid,
				existingID.String)
		}
		if existingAbnd.Valid {
			return nil
		}
		_, err = tx.Exec(
			`UPDATE owned_order SET abandoned_ms = ? WHERE coid = ?`,
			abandonedMs, coid)
		return err
	})
}

// fillFacts is the EXCHANGE's account of one trade: everything about it that
// two observers of the same trade id must agree on.
//
// The observation columns -- `first_run_id`, `first_seen_ms`, `backfilled` --
// are deliberately absent. They record who saw it and when, which is a
// different thing per observer by construction, and comparing them would make
// every backfill walk a contradiction.
type fillFacts struct {
	OrderID      string
	Ticker       string
	Side         string
	Price4       int64
	CountQ       int64
	FeeMicros    int64
	IsTaker      bool
	ExchangeTsMs int64
}

func factsOf(f fillRecord) fillFacts {
	return fillFacts{
		OrderID: f.OrderID, Ticker: f.Ticker, Side: f.Side, Price4: f.Price4,
		CountQ: f.CountQ, FeeMicros: f.FeeMicros, IsTaker: f.IsTaker,
		ExchangeTsMs: f.ExchangeTsMs,
	}
}

// recordFill writes `our_fill` READ-COMPARE-INSERT, in one transaction.
//
// H-ORD-6 is two rules and a bare `INSERT OR IGNORE` expressed only one of
// them. The first observer and its `backfilled` value win -- a fill seen live
// and then re-read by a later run's backfill walk is the same fill, and letting
// the backfill overwrite it would relabel a live observation as history, which
// is the field `lip-gp8` uses to decide whether a cash flow was ours to have
// seen. But `OR IGNORE` cannot tell that case from a trade id coming back with
// a DIFFERENT price, count or fee, and it discards the second silently in both.
// A contradiction about money is not a duplicate: the row would go on asserting
// one figure while the account held another, with nothing anywhere recording
// that two were seen.
//
// So the comparison is over the exchange facts only. Identical is idempotent;
// divergent is PERMANENT, because no amount of retrying makes two accounts of
// one trade agree, and a store that has rejected a record never reports healthy
// again -- which is exactly the surfacing this needs.
//
// `M-HS-FILLREPLACE` lets the later observer overwrite the observation columns;
// `M-HS-FILLBLINDDUP` restores the blind duplicate.
func (b *sqliteBackend) recordFill(f fillRecord) error {
	return b.tx(func(tx *sql.Tx) error {
		var got fillFacts
		var isTaker int64
		err := tx.QueryRow(
			`SELECT order_id, ticker, side, price4, count_q, fee_micros,
			        is_taker, exchange_ts_ms
			   FROM our_fill WHERE trade_id = ?`, f.TradeID).Scan(
			&got.OrderID, &got.Ticker, &got.Side, &got.Price4, &got.CountQ,
			&got.FeeMicros, &isTaker, &got.ExchangeTsMs)
		switch {
		case err == nil:
			got.IsTaker = isTaker != 0
			if want := factsOf(f); got != want {
				return permanent("trade %s is already recorded as %+v and has "+
					"now been reported as %+v; two accounts of one trade "+
					"cannot both be true, and accepting this one silently "+
					"would leave the ledger asserting a figure the account "+
					"does not hold", f.TradeID, got, want)
			}
			// H-ORD-6: the first observer and its `backfilled` value win, so an
			// identical re-observation writes nothing at all.
			return nil
		case errors.Is(err, sql.ErrNoRows):
		default:
			return err
		}
		_, err = tx.Exec(
			`INSERT INTO our_fill
			   (trade_id, first_run_id, first_seen_ms, backfilled, order_id,
			    ticker, side, price4, count_q, fee_micros, is_taker, exchange_ts_ms)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			f.TradeID, f.FirstRunID, f.FirstSeenMs, boolInt(f.Backfilled), f.OrderID,
			f.Ticker, f.Side, f.Price4, f.CountQ, f.FeeMicros, boolInt(f.IsTaker),
			f.ExchangeTsMs)
		return classify(err)
	})
}

// recordState writes one A9 `state_event` row.
func (b *sqliteBackend) recordState(runID string, ev StateEvent) error {
	return b.tx(func(tx *sql.Tx) error {
		var got StateEvent
		var gotRun string
		err := tx.QueryRow(
			`SELECT run_id, ts_ms, scope, ticker, from_state, to_state, "trigger"
			   FROM state_event WHERE event_id = ?`, ev.eventID).Scan(
			&gotRun, &got.tsMs, &got.scope, &got.ticker, &got.from, &got.to,
			&got.trigger)
		switch {
		case err == nil:
			got.eventID = ev.eventID
			if gotRun != runID || got != ev {
				return permanent("state event %s already exists with "+
					"different content", ev.eventID)
			}
			return nil
		case errors.Is(err, sql.ErrNoRows):
		default:
			return err
		}
		_, err = tx.Exec(
			`INSERT INTO state_event
			   (event_id, run_id, ts_ms, scope, ticker, from_state, to_state,
			    "trigger")
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			ev.eventID, runID, ev.tsMs, ev.scope, ev.ticker, ev.from, ev.to,
			ev.trigger)
		return err
	})
}

// insertAnomaly is step ONE of the dual journal: the row commits with
// `journaled_ms` NULL, which is what keeps it invisible to delivery.
func (b *sqliteBackend) insertAnomaly(a anomalyRecord) error {
	return b.tx(func(tx *sql.Tx) error {
		var got anomalyRecord
		err := tx.QueryRow(
			`SELECT run_id, class, sev, ticker, text, first_ms
			   FROM anomaly WHERE anomaly_id = ?`, a.AnomalyID).Scan(
			&got.RunID, &got.Class, &got.Sev, &got.Ticker, &got.Text,
			&got.FirstMs)
		switch {
		case err == nil:
			got.AnomalyID = a.AnomalyID
			if got != a {
				return permanent("anomaly %s already exists with different "+
					"content", a.AnomalyID)
			}
			return nil
		case errors.Is(err, sql.ErrNoRows):
		default:
			return err
		}
		_, err = tx.Exec(
			`INSERT INTO anomaly
			   (anomaly_id, run_id, class, sev, ticker, text, first_ms,
			    journaled_ms, delivered_ms, attempts, last_attempt_ms,
			    suppressed_count)
			 VALUES (?, ?, ?, ?, ?, ?, ?, NULL, NULL, 0, NULL, 0)`,
			a.AnomalyID, a.RunID, a.Class, a.Sev, a.Ticker, a.Text, a.FirstMs)
		return err
	})
}

// markJournaled is step THREE: only now is the anomaly delivery-visible.
func (b *sqliteBackend) markJournaled(anomalyID string, journaledMs int64) error {
	return b.tx(func(tx *sql.Tx) error {
		var existing sql.NullInt64
		err := tx.QueryRow(`SELECT journaled_ms FROM anomaly WHERE anomaly_id = ?`,
			anomalyID).Scan(&existing)
		if errors.Is(err, sql.ErrNoRows) {
			return permanent("anomaly %s has no row to mark journalled",
				anomalyID)
		}
		if err != nil {
			return err
		}
		if existing.Valid {
			return nil
		}
		_, err = tx.Exec(
			`UPDATE anomaly SET journaled_ms = ? WHERE anomaly_id = ?`,
			journaledMs, anomalyID)
		return err
	})
}

// recordDelivery folds one push's outcome into every row it represented.
//
// The representative carries the suppression count so that §13.2's "one push
// per (class,ticker) per 15 minutes" is reconstructable from the table: a reader
// can see both that four occurrences happened and that the operator was told
// once.
func (b *sqliteBackend) recordDelivery(d DeliveryAttempt) error {
	return b.tx(func(tx *sql.Tx) error {
		for _, id := range d.AnomalyIDs {
			res, err := tx.Exec(
				`UPDATE anomaly
				    SET attempts = attempts + 1, last_attempt_ms = ?
				  WHERE anomaly_id = ?`, d.AttemptMs, id)
			if err != nil {
				return err
			}
			n, err := res.RowsAffected()
			if err != nil {
				return err
			}
			if n != 1 {
				return permanent("delivery attempt named anomaly %s, which "+
					"has no row", id)
			}
			if d.Delivered {
				if _, err := tx.Exec(
					`UPDATE anomaly SET delivered_ms = ?
					  WHERE anomaly_id = ? AND delivered_ms IS NULL`,
					d.DeliveredMs, id); err != nil {
					return err
				}
			}
		}
		if d.Delivered && len(d.AnomalyIDs) > 1 {
			if _, err := tx.Exec(
				`UPDATE anomaly SET suppressed_count = ? WHERE anomaly_id = ?`,
				len(d.AnomalyIDs)-1, d.AnomalyIDs[0]); err != nil {
				return err
			}
		}
		return nil
	})
}

// loadLedger reads the whole of `owned_order` that bears on classification: the
// committed coid->order-id bindings AND the reservations still outstanding, from
// EVERY run.
//
// H-ORD-9: fills are classified against the ledger "never by heuristic, never
// by `run_id`". An order placed by a previous incarnation is still ours, and one
// that has since gone terminal is still ours -- `owned_order` rows are never
// deleted and never filtered by the current run. `M-HS-OWNRUN` adds that filter
// to show what it costs: every fill from before the last restart becomes
// foreign, which is a SEV1 and a global stop fired by starting up correctly.
//
// The second half is the F2 repair. A row with no `order_id` and no
// `abandoned_ms` is a reservation that committed before dispatch and whose
// exchange order id this process never learned -- the H-ORD-6 crash window. The
// old reader selected `order_id IS NOT NULL` and nothing else, so those rows
// were invisible at Open and a fill on the order they produced was classified
// FOREIGN: a SEV1, a global stop and a durable WINDING_DOWN latch, fired by our
// own order. `M-HS-OWNNULLSKIP` restores that filter.
func (b *sqliteBackend) loadLedger() (map[string]string, map[string]struct{},
	error) {

	rows, err := b.db.Query(
		`SELECT coid, order_id, abandoned_ms FROM owned_order`)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	bindings := make(map[string]string)
	unresolved := make(map[string]struct{})
	for rows.Next() {
		var (
			coid        string
			orderID     sql.NullString
			abandonedMs sql.NullInt64
		)
		if err := rows.Scan(&coid, &orderID, &abandonedMs); err != nil {
			return nil, nil, err
		}
		switch {
		case orderID.Valid:
			bindings[orderID.String] = coid
		case abandonedMs.Valid:
			// Terminal the other way: the exchange never took this order, so
			// there is no order id it could ever produce a fill under.
		default:
			unresolved[coid] = struct{}{}
		}
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	return bindings, unresolved, nil
}

// anomalyJournalStates returns EVERY anomaly with whether the database believes
// its text is durable.
//
// Every row and not just the unjournalled ones, because the reconciliation is
// bidirectional: a row that claims `journaled_ms` and has no line in the JSONL
// is a contradiction in the direction that matters most. The text journal is
// what the operator reads when the database will not open, and a one-way check
// lets that copy be deleted -- or truncated to a prefix -- while every row goes
// on asserting it exists.
func (b *sqliteBackend) anomalyJournalStates() ([]anomalyJournalState, error) {
	rows, err := b.db.Query(
		`SELECT anomaly_id, run_id, class, sev, ticker, text, first_ms,
		        journaled_ms
		   FROM anomaly ORDER BY first_ms, anomaly_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []anomalyJournalState
	for rows.Next() {
		var st anomalyJournalState
		var journaled sql.NullInt64
		if err := rows.Scan(&st.rec.AnomalyID, &st.rec.RunID, &st.rec.Class,
			&st.rec.Sev, &st.rec.Ticker, &st.rec.Text, &st.rec.FirstMs,
			&journaled); err != nil {
			return nil, err
		}
		st.journaled = journaled.Valid
		out = append(out, st)
	}
	return out, rows.Err()
}

func boolInt(b bool) int64 {
	if b {
		return 1
	}
	return 0
}

// ---------------------------------------------------------------------------
// The text journal
// ---------------------------------------------------------------------------

// journalLine is one JSONL record. It carries the anomaly id so crash recovery
// can reconcile the two journals against each other, and `written_ms` so the
// recovery can complete step three with the time the text actually became
// durable rather than with the time the recovery noticed.
type journalLine struct {
	AnomalyID string `json:"anomaly_id"`
	RunID     string `json:"run_id"`
	Class     string `json:"class"`
	Sev       int    `json:"sev"`
	Ticker    string `json:"ticker"`
	Text      string `json:"text"`
	FirstMs   int64  `json:"first_ms"`
	WrittenMs int64  `json:"written_ms"`
}

func (l journalLine) record() anomalyRecord {
	return anomalyRecord{
		AnomalyID: l.AnomalyID, RunID: l.RunID, Class: l.Class, Sev: l.Sev,
		Ticker: l.Ticker, Text: l.Text, FirstMs: l.FirstMs,
	}
}

// journal is the second durable half. Injectable for the same reason `backend`
// is: a full disk is not producible on demand.
type journal interface {
	appendLine(journalLine) error
	readAll() ([]journalLine, error)
	close() error
}

// fileJournal is the append-only, mode-0600 JSONL of §13.1.
//
// goodOff is the file's length in its last known-good state: every byte below
// it belongs to a line that was written AND synced. A failed append truncates
// back to it, and that is what makes the retry idempotent -- the file is
// byte-identical to before the attempt, so the retry IS the first attempt
// again. Without it, a write that lands and then reports failure leaves bytes
// the next attempt appends after, and §13.1's two journals disagree forever.
type fileJournal struct {
	path    string
	f       *os.File
	goodOff int64
	// dirty means a truncate could not be completed, so the bytes above
	// goodOff are unknown. No append may proceed until it succeeds -- it is
	// re-attempted on every later call, so a disk that comes back heals the
	// journal with no operator action.
	dirty bool
}

// journalWriteFn and journalSyncFn are the journal's two failure points, as
// seams, for the same reason `backend` and `journal` are injectable: a full
// disk is not producible on demand.
//
// They are here rather than on a wrapper because a wrapper cannot produce the
// shapes that matter. Failing BEFORE delegating is atomic-nothing, which is a
// failure mode no real file has; the two that brick this store are a short
// write that leaves a partial line, and a sync that fails after the bytes
// landed.
var (
	journalWriteFn = func(f *os.File, b []byte) (int, error) { return f.Write(b) }
	journalSyncFn  = func(f *os.File) error { return f.Sync() }
)

// syncDirFn is the parent-directory sync, as a seam.
//
// A file that exists only in an unsynced directory entry does not survive a
// power cut: the bytes are on the platter and nothing names them. The harness
// exists to hold evidence across exactly that event, so the ENTRY is synced
// before Open reports success -- the same argument `lifecycle.FileLatch` makes
// for the halt latch, applied to the operator's fallback copy.
var syncDirFn = syncDir

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

func openJournal(path string) (*fileJournal, error) {
	if !filepath.IsAbs(path) {
		return nil, fmt.Errorf("anomaly log path %q is not absolute", path)
	}
	_, statErr := os.Stat(path)
	created := os.IsNotExist(statErr)

	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	// An existing file created with a laxer umask stays laxer unless it is
	// chmod'd. The anomaly text is the operator's copy of everything that went
	// wrong on a live trading account.
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		return nil, err
	}
	if created {
		// A file this call created and could not make durable is REMOVED. Left
		// behind it is worse than absent: the next Open would find an existing
		// journal, skip the creation path, and never sync the entry -- so one
		// failed sync would permanently downgrade the artifact it guards.
		fail := func(err error) (*fileJournal, error) {
			f.Close()
			if rmErr := os.Remove(path); rmErr != nil && !os.IsNotExist(rmErr) {
				return nil, errors.Join(err, rmErr)
			}
			return nil, err
		}
		if err := f.Sync(); err != nil {
			return fail(fmt.Errorf("the new anomaly journal %s could not be "+
				"synced: %w", path, err))
		}
		if err := syncDirFn(filepath.Dir(path)); err != nil {
			return fail(fmt.Errorf("the directory entry naming the new anomaly "+
				"journal %s could not be synced, so the operator's fallback "+
				"copy may not survive a power cut: %w", path, err))
		}
	}
	j := &fileJournal{path: path, f: f}
	if err := j.repairTail(); err != nil {
		f.Close()
		return nil, err
	}
	return j, nil
}

// appendLine writes and SYNCS. Without the sync the "durable" step of §13.1 is
// a claim about the page cache, and the crash it exists for is the one that
// empties it.
func (j *fileJournal) appendLine(l journalLine) error {
	b, err := json.Marshal(l)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	if err := j.settle(); err != nil {
		return err
	}
	if _, err := journalWriteFn(j.f, b); err != nil {
		return j.rollback(err)
	}
	if err := journalSyncFn(j.f); err != nil {
		return j.rollback(err)
	}
	j.goodOff += int64(len(b))
	return nil
}

// rollback returns the file to its last known-good length and reports the
// ORIGINAL error, because that is the one describing what the disk did. The
// caller retries; the file is byte-identical to before the attempt, so the
// retry can neither duplicate a record nor merge two into one.
//
// A truncate that itself fails is NOT permanent. The record stays queued and
// the store stays unhealthy, which revokes adding and nothing else (H-STORE-3);
// popping it here would discard the evidence the failure is about.
func (j *fileJournal) rollback(cause error) error {
	if err := j.truncateToGood(); err != nil {
		j.dirty = true
		return errors.Join(cause, err)
	}
	j.dirty = false
	return cause
}

// settle re-attempts a truncate left incomplete by an earlier failure.
func (j *fileJournal) settle() error {
	if !j.dirty {
		return nil
	}
	if err := j.truncateToGood(); err != nil {
		return fmt.Errorf("the anomaly journal could not be returned to its "+
			"last known-good length %d, so a partial record may still be in it "+
			"and appending now would merge two records into one: %w",
			j.goodOff, err)
	}
	j.dirty = false
	return nil
}

func (j *fileJournal) truncateToGood() error {
	if err := j.f.Truncate(j.goodOff); err != nil {
		return err
	}
	return journalSyncFn(j.f)
}

// repairTail removes an incomplete trailing line and records what survives.
//
// A partial trailing line is this store's OWN crash artifact: §13.1 appends and
// syncs, and a power cut during that append leaves exactly this. Refusing to
// start because of it refuses to start over a record that by construction never
// became visible to delivery -- its row still carries `journaled_ms` NULL, and
// reconcile re-journals it from the database. Only the TAIL is repaired: a
// malformed line with complete lines AFTER it is not a crash artifact, and it
// still fails closed.
func (j *fileJournal) repairTail() error {
	raw, err := os.ReadFile(j.path)
	if err != nil {
		return err
	}
	good := int64(bytes.LastIndexByte(raw, '\n') + 1)
	if good == int64(len(raw)) {
		j.goodOff = good
		return nil
	}
	if err := j.f.Truncate(good); err != nil {
		return fmt.Errorf("the anomaly journal %s ends in a partial record "+
			"and it could not be truncated to the last complete one: %w",
			j.path, err)
	}
	if err := journalSyncFn(j.f); err != nil {
		return fmt.Errorf("the anomaly journal %s was truncated to its last "+
			"complete record and the truncation could not be synced: %w",
			j.path, err)
	}
	j.goodOff = good
	return nil
}

func (j *fileJournal) readAll() ([]journalLine, error) {
	raw, err := os.ReadFile(j.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	return parseJournal(raw)
}

func (j *fileJournal) close() error { return j.f.Close() }

// parseJournal fails closed on a truncated or unparseable file.
//
// A partial trailing line is a crash mid-append. It is not repaired and it is
// not ignored: the whole point of the second journal is that its contents can be
// trusted when the database cannot, and a parser that skipped what it could not
// read would silently narrow that guarantee to "the lines that happened to be
// intact".
func parseJournal(raw []byte) ([]journalLine, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	if raw[len(raw)-1] != '\n' {
		return nil, errors.New("the anomaly journal does not end in a newline, " +
			"so its last record is truncated; §13.1's journal is what the " +
			"operator reads when the database cannot be opened, and a partial " +
			"record makes that guarantee conditional")
	}
	parts := bytes.Split(raw[:len(raw)-1], []byte{'\n'})
	out := make([]journalLine, 0, len(parts))
	for i, p := range parts {
		var l journalLine
		if err := json.Unmarshal(p, &l); err != nil {
			return nil, fmt.Errorf("anomaly journal line %d does not parse: %w",
				i+1, err)
		}
		if l.AnomalyID == "" {
			return nil, fmt.Errorf("anomaly journal line %d carries no "+
				"anomaly id, so it cannot be reconciled against the database",
				i+1)
		}
		out = append(out, l)
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// The read-only view
// ---------------------------------------------------------------------------

// Reader is the read side, on its own `query_only` connection.
//
// Every method is a FIXED query. There is deliberately no `Query(sql string)`:
// a generic escape hatch is how a second writer arrives, how a read starts
// filtering `owned_order` by run, and how the one place that decides what a fill
// means stops being one place. `TestStoreSurfaceCannotForgeLicences` asserts
// from outside the package that no exported method takes SQL.
type Reader struct {
	db *sql.DB
}

// openReader opens the read view with `query_only` in the DSN.
//
// In the DSN and not once after `sql.Open`, for the reason `writerDSN`
// documents: the pragma is per-CONNECTION and `database/sql` is a pool that
// closes idle connections and opens replacements whenever it likes. A
// one-time `Exec` configures whichever connection happened to serve it, and the
// replacement arrives with `query_only=OFF` -- at which point the read view is
// a second writer against a database whose entire design is that there is
// exactly one. modernc's `_pragma` parameter runs it on every connection.
//
// The one-time Exec stays as belt: it costs one statement at startup and it
// fails loudly if the DSN parameter were ever silently dropped by a driver
// change.
func openReader(path string) (*Reader, error) {
	db, err := sql.Open("sqlite", readerDSN(path))
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	if _, err := db.Exec("PRAGMA query_only=ON"); err != nil {
		db.Close()
		return nil, fmt.Errorf("set query_only: %w", err)
	}
	return &Reader{db: db}, nil
}

// readerDSN carries the read view's connection-local pragma.
func readerDSN(path string) string {
	return "file:" + path + "?_pragma=query_only(1)"
}

// Close releases the read connection. The Store closes it; a caller holding a
// Reader from `Store.Reader()` must not.
func (r *Reader) Close() error { return r.db.Close() }

// Tables enumerates every user table, sorted. §15's pilot cut is five, and this
// is how the test proves the deferred ones are absent rather than assumed so.
func (r *Reader) Tables() ([]string, error) {
	rows, err := r.db.Query(
		`SELECT name FROM sqlite_schema
		  WHERE type = 'table' AND name NOT LIKE 'sqlite_%' ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	sort.Strings(out)
	return out, rows.Err()
}

// Run reads back one `run` row with its configuration decoded.
func (r *Reader) Run(runID string) (RunRow, bool, error) {
	var row RunRow
	var raw []byte
	err := r.db.QueryRow(
		`SELECT run_id, started_ms, config_json FROM run WHERE run_id = ?`,
		runID).Scan(&row.RunID, &row.StartedMs, &raw)
	if errors.Is(err, sql.ErrNoRows) {
		return RunRow{}, false, nil
	}
	if err != nil {
		return RunRow{}, false, err
	}
	var p cfg.Params
	if err := json.Unmarshal(raw, &p); err != nil {
		return RunRow{}, false, fmt.Errorf("run %s config_json does not "+
			"decode: %w", runID, err)
	}
	row.Params = p
	return row, true, nil
}

const ownedOrderCols = `coid, run_id, reserved_ms, ticker, side, role,
	price_cents, count_q, order_id, bound_ms, abandoned_ms`

func scanOwnedOrder(sc interface{ Scan(...any) error }) (OwnedOrderRow, error) {
	var row OwnedOrderRow
	var countQ int64
	var orderID sql.NullString
	var boundMs, abandonedMs sql.NullInt64
	err := sc.Scan(&row.Coid, &row.RunID, &row.ReservedMs, &row.Ticker,
		&row.Side, &row.Role, &row.PriceCents, &countQ, &orderID, &boundMs,
		&abandonedMs)
	if err != nil {
		return OwnedOrderRow{}, err
	}
	row.Count = num.Qty(countQ)
	row.OrderID = orderID.String
	row.BoundMs = boundMs.Int64
	row.Bound = orderID.Valid
	row.AbandonedMs = abandonedMs.Int64
	row.Abandoned = abandonedMs.Valid
	return row, nil
}

// OwnedOrder reads one reservation by coid.
func (r *Reader) OwnedOrder(coid string) (OwnedOrderRow, bool, error) {
	row, err := scanOwnedOrder(r.db.QueryRow(
		`SELECT `+ownedOrderCols+` FROM owned_order WHERE coid = ?`, coid))
	if errors.Is(err, sql.ErrNoRows) {
		return OwnedOrderRow{}, false, nil
	}
	if err != nil {
		return OwnedOrderRow{}, false, err
	}
	return row, true, nil
}

// OwnedOrders returns every reservation, oldest first. Every run's, always.
func (r *Reader) OwnedOrders() ([]OwnedOrderRow, error) {
	rows, err := r.db.Query(
		`SELECT ` + ownedOrderCols + ` FROM owned_order ORDER BY reserved_ms, coid`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []OwnedOrderRow
	for rows.Next() {
		row, err := scanOwnedOrder(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

const fillCols = `trade_id, first_run_id, first_seen_ms, backfilled, order_id,
	ticker, side, price4, count_q, fee_micros, is_taker, exchange_ts_ms`

func scanFill(sc interface{ Scan(...any) error }) (FillRow, error) {
	var row FillRow
	var countQ, fee int64
	var backfilled, isTaker int64
	err := sc.Scan(&row.TradeID, &row.FirstRunID, &row.FirstSeenMs, &backfilled,
		&row.OrderID, &row.Ticker, &row.Side, &row.Price4, &countQ, &fee,
		&isTaker, &row.ExchangeTsMs)
	if err != nil {
		return FillRow{}, err
	}
	row.Backfilled = backfilled != 0
	row.IsTaker = isTaker != 0
	row.Count = num.Qty(countQ)
	row.Fee = num.Money(fee)
	return row, nil
}

// Fill reads one `our_fill` row by trade id.
func (r *Reader) Fill(tradeID string) (FillRow, bool, error) {
	row, err := scanFill(r.db.QueryRow(
		`SELECT `+fillCols+` FROM our_fill WHERE trade_id = ?`, tradeID))
	if errors.Is(err, sql.ErrNoRows) {
		return FillRow{}, false, nil
	}
	if err != nil {
		return FillRow{}, false, err
	}
	return row, true, nil
}

// Fills returns every `our_fill` row in exchange-time order, oldest first.
//
// EVERY run's, and BOTH provenances. H-HALT-5's realised P&L is the account's
// cash flow over the ownership ledger, not this incarnation's: a position
// adopted at startup was paid for by an earlier run, and a reader that dropped
// those rows would value inventory it holds against a basis it never recorded.
// Filtering by `first_run_id`, or by `backfilled`, is therefore exactly the
// mistake this method exists to make impossible -- which is why it takes no
// arguments to filter by.
//
// The order is `(exchange_ts_ms, trade_id)`, and the second key is not
// decoration. Average cost is path-dependent -- a fill that crosses through
// zero closes one position and opens another at its own price -- so two fills
// bearing the same exchange millisecond must be replayed in the same order on
// every run, or a restart can compute a different basis from identical rows.
// `trade_id` is the primary key (H-ORD-6), so the pair is a total order.
func (r *Reader) Fills() ([]FillRow, error) {
	rows, err := r.db.Query(
		`SELECT ` + fillCols + ` FROM our_fill ORDER BY exchange_ts_ms, trade_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []FillRow
	for rows.Next() {
		row, err := scanFill(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

const anomalyCols = `anomaly_id, run_id, class, sev, ticker, text, first_ms,
	journaled_ms, delivered_ms, attempts, last_attempt_ms, suppressed_count`

func scanAnomaly(sc interface{ Scan(...any) error }) (AnomalyRow, error) {
	var row AnomalyRow
	var sev int
	var journaled, delivered, lastAttempt sql.NullInt64
	err := sc.Scan(&row.AnomalyID, &row.RunID, &row.Class, &sev, &row.Ticker,
		&row.Text, &row.FirstMs, &journaled, &delivered, &row.Attempts,
		&lastAttempt, &row.SuppressedCount)
	if err != nil {
		return AnomalyRow{}, err
	}
	row.Sev = risk.Severity(sev)
	row.Journaled = journaled.Valid
	row.JournaledMs = journaled.Int64
	row.Delivered = delivered.Valid
	row.DeliveredMs = delivered.Int64
	row.LastAttemptMs = lastAttempt.Int64
	return row, nil
}

// Anomaly reads one row by id, including its delivery state.
func (r *Reader) Anomaly(id string) (AnomalyRow, bool, error) {
	row, err := scanAnomaly(r.db.QueryRow(
		`SELECT `+anomalyCols+` FROM anomaly WHERE anomaly_id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return AnomalyRow{}, false, nil
	}
	if err != nil {
		return AnomalyRow{}, false, err
	}
	return row, true, nil
}

// PendingAnomalies is the delivery queue: journalled in BOTH places, not yet
// delivered, oldest first, from EVERY run.
//
// The two clauses are the two mutations this method exists to fail under.
// `journaled_ms IS NOT NULL` is §13.1's ordering -- a row visible to delivery
// before its text is durable can be pushed once and then lost, which is the
// worst of both journals. And there is no `run_id` filter, because an alert
// that was undelivered when the process died is an alert the operator still has
// not seen; scoping the drain to the current run means every restart quietly
// discards the backlog that the restart itself is evidence for.
func (r *Reader) PendingAnomalies() ([]AnomalyRow, error) {
	rows, err := r.db.Query(
		`SELECT ` + anomalyCols + ` FROM anomaly
		  WHERE journaled_ms IS NOT NULL AND delivered_ms IS NULL
		  ORDER BY first_ms, anomaly_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AnomalyRow
	for rows.Next() {
		row, err := scanAnomaly(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// UndeliveredCount is the heartbeat's "undelivered" metric.
func (r *Reader) UndeliveredCount() (int, error) {
	var n int
	err := r.db.QueryRow(
		`SELECT COUNT(*) FROM anomaly WHERE delivered_ms IS NULL`).Scan(&n)
	return n, err
}

// StateEvents returns every A9 transition, oldest first.
func (r *Reader) StateEvents() ([]StateEventRow, error) {
	rows, err := r.db.Query(
		`SELECT event_id, run_id, ts_ms, scope, ticker, from_state, to_state,
		        "trigger"
		   FROM state_event ORDER BY ts_ms, event_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []StateEventRow
	for rows.Next() {
		var row StateEventRow
		if err := rows.Scan(&row.EventID, &row.RunID, &row.TsMs, &row.Scope,
			&row.Ticker, &row.From, &row.To, &row.Trigger); err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// confidence: high
