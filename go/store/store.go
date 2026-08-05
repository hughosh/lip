// Package store persists rig rows to SQLite.
//
// The schema text is copied verbatim from rig.py so that a DB written by the Go
// rig and one written by the Python rig are interchangeable, and so the
// differential harness can point the same queries at both.
//
// Driver is modernc.org/sqlite (pure Go) rather than mattn/go-sqlite3 (CGO).
// The rig runs at roughly 1.7% of one core in its own measurement code, so the
// C driver's speed edge is irrelevant here, and staying CGO-free keeps the
// build a single static binary and cross-compilation trivial.
package store

import (
	"database/sql"
	"fmt"
	"os"

	_ "modernc.org/sqlite"

	"lip/core"
)

// Verbatim from rig.py:88-135. Do not reformat: the differential harness
// compares schemas, and a DB written by either implementation must satisfy the
// other's queries unchanged.
const Schema = `
CREATE TABLE IF NOT EXISTS fill (
    trade_id       TEXT PRIMARY KEY,
    ts_ms          INTEGER NOT NULL,
    ticker         TEXT    NOT NULL,
    resting_side   TEXT    NOT NULL,   -- side that was passively filled
    price          INTEGER NOT NULL,   -- cents paid by the resting order
    size           REAL    NOT NULL,
    taker_side     TEXT,
    trade_through  INTEGER,            -- 1 = proof a resting order at ` + "`price`" + ` filled
    pre_best_yes   INTEGER,
    pre_best_no    INTEGER,
    pre_yes_size   REAL,
    pre_no_size    REAL,
    pre_mid        REAL,               -- yes-denominated mid, cents
    pre_spread     INTEGER,
    depth_at_price REAL,               -- resting size at ` + "`price`" + ` before the trade
    book_lag_ms    INTEGER,            -- age of the book state used; audit field
    mid_1m         REAL,
    mid_5m         REAL,
    mid_30m        REAL,
    source         TEXT DEFAULT 'observed'  -- 'observed' | 'ours'
);
CREATE INDEX IF NOT EXISTS idx_fill_ticker ON fill(ticker);
CREATE INDEX IF NOT EXISTS idx_fill_tt     ON fill(trade_through);

CREATE TABLE IF NOT EXISTS pending_mid (
    trade_id TEXT    NOT NULL,
    ticker   TEXT    NOT NULL,
    horizon  TEXT    NOT NULL,
    due_ms   INTEGER NOT NULL,
    PRIMARY KEY (trade_id, horizon)
);
CREATE INDEX IF NOT EXISTS idx_pending_due ON pending_mid(due_ms);

CREATE TABLE IF NOT EXISTS reference (
    ts_ms     INTEGER NOT NULL,
    ticker    TEXT    NOT NULL,
    ref_yes   INTEGER,           -- highest yes bid = LIP Reference Yes Price
    ref_no    INTEGER,
    gate      INTEGER,           -- 1 = both sides reach Target Size
    yes_depth REAL,
    no_depth  REAL,
    target    REAL,
    PRIMARY KEY (ts_ms, ticker)
);
CREATE INDEX IF NOT EXISTS idx_ref_ticker ON reference(ticker);
`

type Store struct {
	db *sql.DB
	tx *sql.Tx

	fill    *sql.Stmt
	pending *sql.Stmt
	ref     *sql.Stmt
	due     *sql.Stmt
	unpend  *sql.Stmt
	setMid  map[string]*sql.Stmt

	// Deferred: the Sink interface has no error return, matching Python's
	// fire-and-forget conn.execute. The first failure is kept and surfaced by
	// Commit or Close, so a broken write cannot pass silently.
	err error
}

// Open creates or opens a DB and applies the schema.
func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	// One writer. Concurrency here would buy nothing and risks interleaving.
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(Schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	s := &Store{db: db}
	if err := s.begin(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// Create removes any existing file first, for replay runs that must start clean.
func Create(path string) (*Store, error) {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	return Open(path)
}

func (s *Store) begin() error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	s.tx = tx

	// The conflict clauses are load-bearing and asymmetric (port-spec.md P12):
	//
	//   fill        OR IGNORE  -- a repeated trade_id keeps the FIRST row, so
	//                             the book state captured at first sight wins
	//   pending_mid OR IGNORE
	//   reference   OR REPLACE -- keyed (ts_ms, ticker); the last write wins
	if s.fill, err = tx.Prepare(
		`INSERT OR IGNORE INTO fill (trade_id, ts_ms, ticker, resting_side,
		 price, size, taker_side, trade_through, pre_best_yes, pre_best_no,
		 pre_yes_size, pre_no_size, pre_mid, pre_spread, depth_at_price,
		 book_lag_ms, source)
		 VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,'observed')`); err != nil {
		return err
	}
	if s.pending, err = tx.Prepare(
		`INSERT OR IGNORE INTO pending_mid VALUES (?,?,?,?)`); err != nil {
		return err
	}
	if s.ref, err = tx.Prepare(
		`INSERT OR REPLACE INTO reference VALUES (?,?,?,?,?,?,?,?)`); err != nil {
		return err
	}

	// The forward-mid resolver's three statements. rig.py:388-406.
	if s.due, err = tx.Prepare(
		`SELECT trade_id, ticker, horizon FROM pending_mid
		 WHERE due_ms <= ? LIMIT ?`); err != nil {
		return err
	}
	if s.unpend, err = tx.Prepare(
		`DELETE FROM pending_mid WHERE trade_id = ? AND horizon = ?`); err != nil {
		return err
	}
	// Python interpolates the horizon into the column name. Here the statement
	// per horizon is prepared up front from core.Horizons, so the column can
	// only ever be one of the three the schema defines.
	s.setMid = make(map[string]*sql.Stmt, len(core.Horizons))
	for _, h := range core.Horizons {
		st, perr := tx.Prepare(
			`UPDATE fill SET mid_` + h.Name + ` = ? WHERE trade_id = ?`)
		if perr != nil {
			return perr
		}
		s.setMid[h.Name] = st
	}
	return nil
}

// DueMid is one pending forward-mid horizon that has come due.
type DueMid struct{ TradeID, Ticker, Horizon string }

// DueMids returns horizons due at or before nowMs. rig.py:388-392.
//
// Every row is read out before anything is written, matching Python's
// .fetchall(): the resolver's updates land on the same transaction, and holding
// a cursor open across them on a single connection is asking for trouble.
func (s *Store) DueMids(nowMs int64, limit int) ([]DueMid, error) {
	if s.err != nil {
		return nil, s.err
	}
	rows, err := s.due.Query(nowMs, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DueMid
	for rows.Next() {
		var d DueMid
		if err := rows.Scan(&d.TradeID, &d.Ticker, &d.Horizon); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// SetMid writes one resolved forward mid.
func (s *Store) SetMid(tradeID, horizon string, mid float64) error {
	if s.err != nil {
		return s.err
	}
	st, ok := s.setMid[horizon]
	if !ok {
		return fmt.Errorf("unknown horizon %q", horizon)
	}
	_, err := st.Exec(mid, tradeID)
	return err
}

// DeletePending retires one pending horizon.
func (s *Store) DeletePending(tradeID, horizon string) error {
	if s.err != nil {
		return s.err
	}
	_, err := s.unpend.Exec(tradeID, horizon)
	return err
}

// ResolveMids fills in mid_1m / mid_5m / mid_30m for every horizon now due, and
// returns how many it retired. Port of rig.py:387-407.
//
// `mid` reads the live in-memory book for a ticker and returns nil when there is
// no two-sided mid. It is called on the caller's goroutine, which is the one
// that owns the books.
//
// The whole of port-spec.md P28 lives here:
//
//   - LIMIT is passed by the caller and is 2000 in the live rig;
//   - nothing due means NO COMMIT, not an empty one;
//   - the mid is written only when it exists, but the pending row is deleted
//     UNCONDITIONALLY.
//
// That last one loses the markout for a market that happened to be one-sided at
// the horizon, permanently, rather than retrying at the next tick. It is a
// defect, it is deferred, and "fixing" it here would produce rows the Python
// never produced.
// `commit` is injected rather than called directly because rig.py:407's
// conn.commit() commits the WHOLE shared transaction — every `fill` and
// `reference` row since the last commit, not just this pass's mid updates
// (§6a.1). The live rig therefore has to funnel it through the same gate as
// every other commit so a SIGTERM cannot slip between the decision and the
// write. Pass s.Commit for a caller with no such gate.
func (s *Store) ResolveMids(nowMs int64, limit int, mid func(ticker string) *float64,
	commit func() error) (int, error) {
	due, err := s.DueMids(nowMs, limit)
	if err != nil {
		return 0, err
	}
	if len(due) == 0 {
		return 0, nil
	}
	for _, d := range due {
		if m := mid(d.Ticker); m != nil {
			if err := s.SetMid(d.TradeID, d.Horizon, *m); err != nil {
				return 0, err
			}
		}
		if err := s.DeletePending(d.TradeID, d.Horizon); err != nil {
			return 0, err
		}
	}
	return len(due), commit()
}

func (s *Store) Fill(r core.FillRow) {
	if s.err != nil {
		return
	}
	// The nil pointers below reach SQLite as NULL, which is distinct from the
	// zero values in the same row. See testdata/NULLABILITY.tsv.
	_, err := s.fill.Exec(
		r.TradeID, r.TsMs, r.Ticker, r.RestingSide, r.Price, r.Size,
		r.TakerSide, r.TradeThrough, r.PreBestYes, r.PreBestNo,
		r.PreYesSize, r.PreNoSize, r.PreMid, r.PreSpread,
		r.DepthAtPrice, r.BookLagMs,
	)
	s.note(err)
}

func (s *Store) PendingMid(tradeID, ticker, horizon string, dueMS int64) {
	if s.err != nil {
		return
	}
	_, err := s.pending.Exec(tradeID, ticker, horizon, dueMS)
	s.note(err)
}

func (s *Store) Reference(r core.ReferenceRow) {
	if s.err != nil {
		return
	}
	_, err := s.ref.Exec(r.TsMs, r.Ticker, r.RefYes, r.RefNo, r.Gate,
		r.YesDepth, r.NoDepth, r.Target)
	s.note(err)
}

func (s *Store) note(err error) {
	if err != nil && s.err == nil {
		s.err = err
	}
}

// Commit flushes the open transaction and starts a fresh one.
func (s *Store) Commit() error {
	if s.err != nil {
		return s.err
	}
	if err := s.tx.Commit(); err != nil {
		return err
	}
	return s.begin()
}

func (s *Store) Close() error {
	if s.err != nil {
		s.tx.Rollback()
		s.db.Close()
		return s.err
	}
	if err := s.tx.Commit(); err != nil {
		s.db.Close()
		return err
	}
	return s.db.Close()
}

// Discard drops the open transaction and closes.
//
// This is the SIGTERM path. Python installs no SIGTERM handler, so the process
// dies without reaching either `finally` and SQLite rolls the open transaction
// back — losing up to one commit interval of `fill` and `reference` rows. Go
// committing there instead would keep rows Python discards, on every single
// restart, which is a systematic divergence the shadow run would surface as a
// Go-only row surplus.
//
// SIGINT is the opposite case and must still commit: Python catches
// KeyboardInterrupt and its `finally` runs (rig.py:565-569, 661-667).
func (s *Store) Discard() error {
	if s.tx != nil {
		s.tx.Rollback()
	}
	return s.db.Close()
}

// Summarise reopens a finished DB for the replay report. Separate from Store so
// it cannot contend with the single writer connection's open transaction.
func Summarise(path string) (fills, tt, refs int64, err error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return
	}
	defer db.Close()
	if err = db.QueryRow(
		`SELECT COUNT(*), COALESCE(SUM(trade_through), 0) FROM fill`).
		Scan(&fills, &tt); err != nil {
		return
	}
	err = db.QueryRow(`SELECT COUNT(*) FROM reference`).Scan(&refs)
	return
}

var _ core.Sink = (*Store)(nil)

// confidence: high
