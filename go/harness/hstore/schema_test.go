package hstore

import (
	"reflect"
	"strings"
	"testing"

	"lip/harness/cfg"
)

// TestSchemaIsExactlyThePilotFiveAndPragmasArePinned is the structural gate on
// §15's pilot cut and on durability.
//
// It ENUMERATES `sqlite_schema` rather than asserting the five it expects are
// present. Presence is not the property: pilot-plan.md §2.3 cut the table set to
// five, and a sixth appearing is a deferred decision quietly becoming an
// implemented one. The deferred names are asserted absent individually so the
// failure says which one came back.
//
// The pragmas are read back from the WRITE connection, because `synchronous`
// and `foreign_keys` are per-connection and asking the query-only reader would
// prove nothing about the durability of a write. `M-HS-PRAGMA` sets
// `synchronous=OFF`, which is a database that survives a process crash and not a
// power cut -- and the records here are the evidence that a crash happened.
func TestSchemaIsExactlyThePilotFiveAndPragmasArePinned(t *testing.T) {
	s := tempStore(t)

	tables, err := s.Reader().Tables()
	if err != nil {
		t.Fatalf("enumerate tables: %v", err)
	}
	if !reflect.DeepEqual(tables, userTables) {
		t.Fatalf("user tables are %v, want exactly the pilot five %v",
			tables, userTables)
	}
	have := make(map[string]struct{}, len(tables))
	for _, n := range tables {
		have[n] = struct{}{}
	}
	for _, n := range deferredTables {
		if _, present := have[n]; present {
			t.Fatalf("deferred §15 table %q exists; pilot-plan.md §2.3 cut the "+
				"record set to five, and implementing a sixth is a decision "+
				"nobody argued for", n)
		}
	}

	p, err := s.pragmas()
	if err != nil {
		t.Fatalf("read pragmas: %v", err)
	}
	if !strings.EqualFold(p.JournalMode, "wal") {
		t.Fatalf("journal_mode is %q, want WAL", p.JournalMode)
	}
	// 1 is NORMAL. 0 is OFF, which does not survive a power cut.
	if p.Synchronous != 1 {
		t.Fatalf("synchronous is %d, want 1 (NORMAL); 0 is OFF and loses "+
			"committed transactions on a power cut, which is precisely the "+
			"event these records exist to describe", p.Synchronous)
	}
	if p.ForeignKeys != 1 {
		t.Fatalf("foreign_keys is %d, want 1; our_fill references a bound "+
			"owned_order and state_event references run, and unenforced keys "+
			"make both references decoration", p.ForeignKeys)
	}
	if p.UserVersion != schemaVersion {
		t.Fatalf("user_version is %d, want %d", p.UserVersion, schemaVersion)
	}
}

// TestRunRoundTripsEveryCfgFieldExactly holds §15's "every parameter in §16,
// verbatim" to the word verbatim.
//
// Every field is perturbed by REFLECTION rather than by hand, so a parameter
// added to `cfg.Params` tomorrow is covered by this test the day it appears --
// and a field whose kind this test does not know how to perturb fails loudly
// rather than being silently skipped. That is the difference between a
// round-trip test and a round-trip test that happens to cover today's fields.
func TestRunRoundTripsEveryCfgFieldExactly(t *testing.T) {
	s := tempStore(t)

	want := perturbedParams(t)
	if _, err := s.BeginRun("runa", 1_700_000_000_000, want); err != nil {
		t.Fatalf("BeginRun: %v", err)
	}
	for _, r := range pump(t, s) {
		if !r.OK() {
			t.Fatalf("BeginRun failed: %v", r.Err)
		}
	}

	row, ok, err := s.Reader().Run("runa")
	if err != nil || !ok {
		t.Fatalf("read back run: ok=%v err=%v", ok, err)
	}
	if !reflect.DeepEqual(row.Params, want) {
		t.Fatalf("cfg.Params did not round-trip:\n stored %+v\n  read %+v",
			want, row.Params)
	}
	if row.StartedMs != 1_700_000_000_000 {
		t.Fatalf("started_ms %d", row.StartedMs)
	}

	// The perturbation must have moved EVERY field off its default, or a field
	// that silently failed to encode would still compare equal.
	def := reflect.ValueOf(cfg.Default())
	got := reflect.ValueOf(row.Params)
	for i := 0; i < got.NumField(); i++ {
		name := got.Type().Field(i).Name
		if reflect.DeepEqual(got.Field(i).Interface(), def.Field(i).Interface()) {
			t.Fatalf("field %s still holds its default value, so this test "+
				"would pass even if that field were dropped from the encoding",
				name)
		}
	}
}

// perturbedParams moves every field of cfg.Params off its default.
func perturbedParams(t *testing.T) cfg.Params {
	t.Helper()
	p := cfg.Default()
	v := reflect.ValueOf(&p).Elem()
	for i := 0; i < v.NumField(); i++ {
		f := v.Field(i)
		name := v.Type().Field(i).Name
		switch f.Kind() {
		case reflect.Int, reflect.Int64:
			f.SetInt(f.Int() + int64(i) + 1)
		case reflect.Float64:
			f.SetFloat(f.Float() + float64(i+1)/1024)
		case reflect.Bool:
			f.SetBool(!f.Bool())
		default:
			t.Fatalf("cfg.Params field %s has kind %v, which this test does "+
				"not know how to perturb; §15 records every §16 parameter "+
				"verbatim and an unperturbed field is an unverified one",
				name, f.Kind())
		}
	}
	return p
}

// TestStateEventPersistsScopeStatesAndTrigger is A9's record.
//
// The two rejections are the point. `NextGlobal` and `NextMarket` return
// `GTNone`/`MTNone` alongside the SAME state -- that pair is how they say
// nothing happened -- so a row carrying either is a transition whose reason was
// lost or a non-transition recorded as one. An operator reconstructing why the
// harness stopped adding would find a change with no cause. `M-HS-STATE`
// removes the global rejection.
func TestStateEventPersistsScopeStatesAndTrigger(t *testing.T) {
	s := tempStore(t)
	h := begin(t, s, "runa", 1_700_000_000_000)

	if _, err := s.RecordGlobalState(h, "g1", 1_700_000_001_000,
		globalStarting, globalRunning, triggerReconciled); err != nil {
		t.Fatalf("RecordGlobalState: %v", err)
	}
	if _, err := s.RecordMarketState(h, "m1", 1_700_000_002_000, "KXTEST-A",
		marketIdle, marketQuoting, triggerSelected); err != nil {
		t.Fatalf("RecordMarketState: %v", err)
	}
	for _, r := range pump(t, s) {
		if !r.OK() {
			t.Fatalf("state event failed: %v", r.Err)
		}
	}

	rows, err := s.Reader().StateEvents()
	if err != nil {
		t.Fatalf("read state events: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("state events %+v, want two", rows)
	}
	g, m := rows[0], rows[1]
	if g.Scope != "global" || g.Ticker != "" || g.From != "STARTING" ||
		g.To != "RUNNING" || g.Trigger != "reconciled" || g.RunID != "runa" {
		t.Fatalf("global row %+v", g)
	}
	if m.Scope != "market" || m.Ticker != "KXTEST-A" || m.From != "IDLE" ||
		m.To != "QUOTING" || m.Trigger != "selected" || m.RunID != "runa" {
		t.Fatalf("market row %+v", m)
	}

	// A9's reason is not optional.
	if _, err := NewGlobalStateEvent("g2", 1, globalStarting, globalRunning,
		triggerGlobalNone); err == nil {
		t.Fatal("a global transition with trigger `none` was accepted; A9 " +
			"requires the reason, and GTNone is what NextGlobal returns when " +
			"nothing happened")
	}
	if _, err := NewMarketStateEvent("m2", 1, "KXTEST-A", marketIdle,
		marketQuoting, triggerMarketNone); err == nil {
		t.Fatal("a market transition with trigger `none` was accepted")
	}
	// Nor is a real transition.
	if _, err := NewGlobalStateEvent("g3", 1, globalRunning, globalRunning,
		triggerReconciled); err == nil {
		t.Fatal("an unchanged global state was recorded as a transition")
	}
	if _, err := NewMarketStateEvent("m3", 1, "KXTEST-A", marketIdle,
		marketIdle, triggerSelected); err == nil {
		t.Fatal("an unchanged market state was recorded as a transition")
	}
	if _, err := NewMarketStateEvent("m4", 1, "", marketIdle, marketQuoting,
		triggerSelected); err == nil {
		t.Fatal("a market transition with no ticker was accepted")
	}
}
