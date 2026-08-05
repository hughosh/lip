package store

import (
	"database/sql"
	"path/filepath"
	"testing"

	"lip/core"
)

// P12. The conflict clauses are asymmetric and neither is exercised by any
// captured tape — there are zero duplicate trade_ids across 544k frames, so the
// differential gate cannot tell OR IGNORE from OR REPLACE. Gate 7 found that
// hole; these tests close it.

func openTemp(t *testing.T) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	return s, path
}

func TestP12_FillInsertOrIgnoreKeepsTheFirstRow(t *testing.T) {
	s, path := openTemp(t)

	first := core.FillRow{TradeID: "dup", TsMs: 1, Ticker: "AAA",
		RestingSide: "yes", Price: 48, Size: 10}
	second := first
	second.Price = 99
	second.Size = 999

	s.Fill(first)
	s.Fill(second)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	var n, price int
	if err := db.QueryRow(`SELECT COUNT(*), MAX(price) FROM fill`).Scan(&n, &price); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("got %d rows, want 1", n)
	}
	if price != 48 {
		t.Errorf("price = %d, want 48: INSERT OR IGNORE keeps the FIRST row, so the "+
			"book state captured at first sight wins. OR REPLACE would give 99.", price)
	}
}

func TestP12_ReferenceInsertOrReplaceKeepsTheLastRow(t *testing.T) {
	s, path := openTemp(t)

	y1, y2 := 48, 55
	s.Reference(core.ReferenceRow{TsMs: 7, Ticker: "AAA", RefYes: &y1, Gate: 0})
	s.Reference(core.ReferenceRow{TsMs: 7, Ticker: "AAA", RefYes: &y2, Gate: 1})
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	var n, refYes int
	if err := db.QueryRow(`SELECT COUNT(*), MAX(ref_yes) FROM reference`).Scan(&n, &refYes); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("got %d rows, want 1 (PK is (ts_ms, ticker))", n)
	}
	if refYes != 55 {
		t.Errorf("ref_yes = %d, want 55: reference uses OR REPLACE, so the LAST "+
			"write wins — the opposite of fill", refYes)
	}
}

// NULL versus zero is the single most common way a Go port silently corrupts a
// column, because both are invisible at compile time. See testdata/NULLABILITY.tsv.
func TestP18_NilPointersPersistAsNullNotZero(t *testing.T) {
	s, path := openTemp(t)

	s.Fill(core.FillRow{
		TradeID: "nulls", TsMs: 1, Ticker: "AAA", RestingSide: "yes",
		Price: 48, Size: 10,
		PreBestYes: nil, PreBestNo: nil, PreMid: nil, PreSpread: nil, BookLagMs: nil,
		PreYesSize: 0, PreNoSize: 0, DepthAtPrice: 0, // zero, and must stay zero
	})
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	var nulls, zeros int
	err = db.QueryRow(`SELECT
		(pre_best_yes IS NULL) + (pre_best_no IS NULL) + (pre_mid IS NULL)
		  + (pre_spread IS NULL) + (book_lag_ms IS NULL),
		(pre_yes_size = 0.0) + (pre_no_size = 0.0) + (depth_at_price = 0.0)
		FROM fill`).Scan(&nulls, &zeros)
	if err != nil {
		t.Fatal(err)
	}
	if nulls != 5 {
		t.Errorf("%d of 5 nullable columns are NULL; the rest were written as zero", nulls)
	}
	if zeros != 3 {
		t.Errorf("%d of 3 zero-valued columns are 0.0; the rest became NULL", zeros)
	}
	var src string
	if err := db.QueryRow(`SELECT source FROM fill`).Scan(&src); err != nil {
		t.Fatal(err)
	}
	if src != "observed" {
		t.Errorf("source = %q, want \"observed\"", src)
	}
}
