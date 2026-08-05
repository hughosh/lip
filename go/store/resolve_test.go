package store

import (
	"database/sql"
	"testing"

	"lip/core"
)

// seedFill writes one fill plus its three pending horizons, the way the handler
// would.
func seedFill(t *testing.T, s *Store, tradeID, ticker string, tsMs int64) {
	t.Helper()
	s.Fill(core.FillRow{
		TradeID: tradeID, TsMs: tsMs, Ticker: ticker,
		RestingSide: "yes", Price: 48, Size: 3, TakerSide: "no",
	})
	for _, h := range core.Horizons {
		s.PendingMid(tradeID, ticker, h.Name, tsMs+h.Secs*1000)
	}
}

func midCols(t *testing.T, path, tradeID string) (m1, m5, m30 sql.NullFloat64) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.QueryRow(
		`SELECT mid_1m, mid_5m, mid_30m FROM fill WHERE trade_id = ?`, tradeID).
		Scan(&m1, &m5, &m30); err != nil {
		t.Fatal(err)
	}
	return
}

func pendingCount(t *testing.T, path string) int {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM pending_mid`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func always(v float64) func(string) *float64 {
	return func(string) *float64 { return &v }
}

func never(string) *float64 { return nil }

// --- P28: the resolver's three quirks ---------------------------------------

func TestP28_NothingDueDoesNotCommit(t *testing.T) {
	s, path := openTemp(t)
	defer s.Close()

	seedFill(t, s, "t1", "AAA", 1_000_000)

	// Nothing is due yet, so ResolveMids must return without committing. If it
	// committed, the seeded fill would be visible to a second connection.
	n, err := s.ResolveMids(1_000_000, 2000, always(50), s.Commit)
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("resolved %d rows, want 0", n)
	}
	if got := pendingCount(t, path); got != 0 {
		t.Errorf("a second connection sees %d pending rows, want 0: ResolveMids "+
			"committed when nothing was due (P28)", got)
	}
}

func TestP28_PendingRowIsDeletedEvenWhenNoMidExists(t *testing.T) {
	s, path := openTemp(t)
	defer s.Close()

	seedFill(t, s, "t1", "AAA", 1_000_000)

	// All three horizons due, but the market is one-sided so there is no mid.
	n, err := s.ResolveMids(9_000_000, 2000, never, s.Commit)
	if err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Fatalf("resolved %d rows, want 3", n)
	}
	if got := pendingCount(t, path); got != 0 {
		t.Errorf("%d pending rows survived, want 0. The delete is UNCONDITIONAL: a "+
			"market that is one-sided at the horizon loses that markout permanently "+
			"rather than retrying. That is a deferred defect, and making it retry "+
			"here would produce rows the Python never produced (P28).", got)
	}
	m1, m5, m30 := midCols(t, path, "t1")
	if m1.Valid || m5.Valid || m30.Valid {
		t.Errorf("mids = %v/%v/%v, want all NULL when no mid could be computed",
			m1, m5, m30)
	}
}

func TestP28_MidIsWrittenToTheRightColumnPerHorizon(t *testing.T) {
	s, path := openTemp(t)
	defer s.Close()

	seedFill(t, s, "t1", "AAA", 1_000_000)

	// Only the 1m horizon is due at ts+60s.
	n, err := s.ResolveMids(1_060_000, 2000, always(48.5), s.Commit)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("resolved %d rows, want 1: only mid_1m is due", n)
	}
	m1, m5, m30 := midCols(t, path, "t1")
	if !m1.Valid || m1.Float64 != 48.5 {
		t.Errorf("mid_1m = %v, want 48.5", m1)
	}
	if m5.Valid || m30.Valid {
		t.Errorf("mid_5m/mid_30m = %v/%v, want NULL: neither is due yet", m5, m30)
	}
	if got := pendingCount(t, path); got != 2 {
		t.Errorf("pending = %d, want 2", got)
	}
}

func TestP28_LimitCapsOnePass(t *testing.T) {
	s, path := openTemp(t)
	defer s.Close()

	for _, id := range []string{"t1", "t2", "t3"} {
		seedFill(t, s, id, "AAA", 1_000_000)
	}
	n, err := s.ResolveMids(9_000_000, 2, always(50), s.Commit)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("resolved %d rows, want 2: the LIMIT caps one pass", n)
	}
	if got := pendingCount(t, path); got != 7 {
		t.Errorf("pending = %d, want 7 (9 seeded, 2 retired)", got)
	}
}

// The resolver reads the ticker from the pending row, not from the fill, and
// must tolerate a market that is no longer in the universe.
func TestP28_UnknownTickerStillRetiresTheRow(t *testing.T) {
	s, path := openTemp(t)
	defer s.Close()

	seedFill(t, s, "t1", "GONE", 1_000_000)
	seen := ""
	n, err := s.ResolveMids(9_000_000, 2000, func(ticker string) *float64 {
		seen = ticker
		return nil
	}, s.Commit)
	if err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Fatalf("resolved %d rows, want 3", n)
	}
	if seen != "GONE" {
		t.Errorf("mid() was called with %q, want GONE", seen)
	}
	if got := pendingCount(t, path); got != 0 {
		t.Errorf("pending = %d, want 0", got)
	}
}

// --- Discard vs Close: the SIGTERM/SIGINT split -----------------------------
//
// Python installs no SIGTERM handler, so the process dies without reaching its
// `finally` and SQLite rolls the open transaction back, losing up to one commit
// interval of rows. It DOES catch SIGINT as KeyboardInterrupt, and there the
// `finally` runs and commits (rig.py:565-569, 661-667).
//
// Committing on SIGTERM would leave Go holding rows Python discards on every
// restart — a systematic Go-only row surplus in the shadow comparison.

func rowCount(t *testing.T, path, table string) int {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestDiscardRollsBackUncommittedRows(t *testing.T) {
	s, path := openTemp(t)
	seedFill(t, s, "t1", "AAA", 1_000_000)
	if err := s.Discard(); err != nil {
		t.Fatal(err)
	}
	if n := rowCount(t, path, "fill"); n != 0 {
		t.Errorf("fill rows after Discard = %d, want 0. SIGTERM must lose the open "+
			"transaction, as Python's unhandled signal does.", n)
	}
	if n := rowCount(t, path, "pending_mid"); n != 0 {
		t.Errorf("pending_mid rows after Discard = %d, want 0", n)
	}
}

func TestCloseCommitsUncommittedRows(t *testing.T) {
	s, path := openTemp(t)
	seedFill(t, s, "t1", "AAA", 1_000_000)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if n := rowCount(t, path, "fill"); n != 1 {
		t.Errorf("fill rows after Close = %d, want 1. SIGINT reaches Python's "+
			"finally and commits.", n)
	}
}

// Rows already committed survive a later Discard: only the OPEN transaction is
// lost, which is the same boundary Python's crash leaves behind.
func TestDiscardKeepsAlreadyCommittedRows(t *testing.T) {
	s, path := openTemp(t)
	seedFill(t, s, "kept", "AAA", 1_000_000)
	if err := s.Commit(); err != nil {
		t.Fatal(err)
	}
	seedFill(t, s, "lost", "AAA", 2_000_000)
	if err := s.Discard(); err != nil {
		t.Fatal(err)
	}
	if n := rowCount(t, path, "fill"); n != 1 {
		t.Fatalf("fill rows = %d, want 1 (the committed one only)", n)
	}
}
