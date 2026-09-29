package hstore

import (
	"database/sql"
	"reflect"
	"testing"
)

func TestBalancePollIsDurableAndSeparateFromRunCash(t *testing.T) {
	s := tempStore(t)
	h := begin(t, s, "runa", 1_700_000_000_000)
	if _, err := s.RecordBalancePoll(h, 1_700_000_060_000, 10_000); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RecordBalancePoll(h, 1_700_000_120_000, 10_100); err != nil {
		t.Fatal(err)
	}
	for _, result := range pump(t, s) {
		if !result.OK() || result.Kind != KindBalancePoll {
			t.Fatalf("balance writer result: %+v", result)
		}
	}
	got, err := s.Reader().BalancePolls()
	if err != nil {
		t.Fatal(err)
	}
	want := []BalancePollRow{
		{TsMs: 1_700_000_060_000, RunID: "runa", BalanceCents: 10_000},
		{TsMs: 1_700_000_120_000, RunID: "runa", BalanceCents: 10_100},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("balance rows = %+v, want %+v", got, want)
	}
}

func TestBalancePollMigrationPreservesPilotRows(t *testing.T) {
	dbPath, logPath := paths(t)
	s := openAt(t, dbPath, logPath)
	begin(t, s, "runa", 1_700_000_000_000)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("DROP TABLE balance_poll"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("PRAGMA user_version=2"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO owned_order
		(coid, run_id, reserved_ms, ticker, side, role, price_cents,
		 count_q, order_id, bound_ms, abandoned_ms)
		VALUES ('coid-a', 'runa', 1700000000001, 'KXTEST-A', 'yes',
		 'adding', 50, 100, 'order-a', 1700000000002, NULL)`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	s = openAt(t, dbPath, logPath)
	defer s.Close()
	if _, ok, err := s.Reader().Run("runa"); err != nil || !ok {
		t.Fatalf("migration lost existing run: ok=%v err=%v", ok, err)
	}
	if row, ok, err := s.Reader().OwnedOrder("coid-a"); err != nil || !ok || row.OrderID != "order-a" {
		t.Fatalf("migration lost existing ownership binding: row=%+v ok=%v err=%v", row, ok, err)
	}
	tables, err := s.Reader().Tables()
	if err != nil || !reflect.DeepEqual(tables, userTables) {
		t.Fatalf("migrated tables = %v err=%v", tables, err)
	}
	if p, err := s.pragmas(); err != nil || p.UserVersion != schemaVersion {
		t.Fatalf("migrated version = %+v err=%v", p, err)
	}
}
