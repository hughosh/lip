package hstore

import (
	"reflect"
	"testing"

	"lip/harness/quote"
	"lip/harness/rest"
)

// TestDispatchPermitExistsOnlyAfterCommittedOwnership is H-ORD-6's barrier.
//
// The claim is not "a permit is issued eventually". It is that between
// submitting a reservation and its transaction committing, there is NO permit --
// so there is no code path from "I want to place this order" to "I may place
// this order" that skips the durable record. An order dispatched before its coid
// is on disk is an order whose fill the next incarnation classifies as foreign,
// which is SEV1 and a global stop produced by our own write.
//
// `M-HS-PERMIT` publishes the permit at enqueue time, which is the same program
// with the barrier removed and everything else identical.
func TestDispatchPermitExistsOnlyAfterCommittedOwnership(t *testing.T) {
	s := tempStore(t)
	h := begin(t, s, "runa", 1_700_000_000_000)
	o := order(t, "runa", 1, quote.SideYes)

	if _, err := s.ReserveOrder(h, o, quote.RoleAdding, 1_700_000_001_000); err != nil {
		t.Fatalf("ReserveOrder: %v", err)
	}

	// The writer has not run. Nothing is durable, so nothing may be dispatched.
	for _, r := range s.TakeResults() {
		if _, ok := r.Permit(); ok {
			t.Fatal("a dispatch permit existed before the reservation " +
				"committed; H-ORD-6 makes the ownership record durable BEFORE " +
				"the order is sent, and a permit issued at enqueue time " +
				"licenses exactly the dispatch that rule forbids")
		}
	}
	if row, ok, err := s.Reader().OwnedOrder(o.ClientOrderID()); err != nil || ok {
		t.Fatalf("owned_order row exists before the writer ran: ok=%v row=%+v "+
			"err=%v", ok, row, err)
	}

	var permits []DispatchPermit
	for _, r := range pump(t, s) {
		if p, ok := r.Permit(); ok {
			permits = append(permits, p)
		}
	}
	if len(permits) != 1 {
		t.Fatalf("got %d permits after the commit, want exactly one",
			len(permits))
	}

	// The permit is bound byte-for-byte to its order.
	got, err := permits[0].Order()
	if err != nil {
		t.Fatalf("a permit from a committed reservation refused to release "+
			"its order: %v", err)
	}
	if !reflect.DeepEqual(got, o) {
		t.Fatalf("permit released %+v, want the reserved %+v", got, o)
	}
	if permits[0].Coid() != o.ClientOrderID() {
		t.Fatalf("permit coid %q, want %q", permits[0].Coid(),
			o.ClientOrderID())
	}

	// And the row it rests on is there.
	row, ok, err := s.Reader().OwnedOrder(o.ClientOrderID())
	if err != nil || !ok {
		t.Fatalf("owned_order after commit: ok=%v err=%v", ok, err)
	}
	if row.Bound {
		t.Fatalf("a reservation was bound before any order id was learned: %+v",
			row)
	}
	if row.RunID != "runa" || row.Ticker != "KXTEST-A" || row.Role != "adding" ||
		row.PriceCents != 50 {
		t.Fatalf("reservation row %+v", row)
	}

	// A forged permit licenses nothing.
	if _, err := (DispatchPermit{}).Order(); err == nil {
		t.Fatal("the zero DispatchPermit released an order")
	}
}

// TestFailedBindingNeverEntersCommittedOwnership is H-ORD-9's third answer.
//
// A binding that has not committed -- or that never will -- makes its order id
// UNCLASSIFIABLE. It must not read as owned, and it must not read as foreign
// either: "foreign" declares a third party is trading the account, latches a
// global stop, and would here be produced by a write that failed.
//
// `M-HS-BINDCACHE` updates the in-memory index at submission, which turns an
// intention into a fact and survives the intention failing.
func TestFailedBindingNeverEntersCommittedOwnership(t *testing.T) {
	s := tempStore(t)
	begin(t, s, "runa", 1_700_000_000_000)

	// A coid that was never reserved. The binding cannot succeed.
	coid, err := rest.Coid("runa", 0, quote.SideYes, 7)
	if err != nil {
		t.Fatalf("coid: %v", err)
	}
	if _, err := s.BindOrder(coid, "ord-never", 1_700_000_001_000); err != nil {
		t.Fatalf("BindOrder: %v", err)
	}

	// In flight: unclassifiable, not owned.
	owned, err := s.Ownership().OwnsOrders([]string{"ord-never"})
	if err == nil {
		t.Fatalf("an uncommitted binding classified order ord-never as %v; "+
			"H-ORD-9 answers from the DURABLE ledger, and an in-flight "+
			"intention is exactly the in-memory fallback it forbids", owned)
	}

	results := pump(t, s)
	failed := false
	for _, r := range results {
		if r.Kind == KindBindOrder && r.Err != nil {
			failed = true
		}
	}
	if !failed {
		t.Fatalf("binding a coid that was never reserved succeeded: %+v",
			results)
	}

	// Failed: still unclassifiable, still not owned.
	if owned, err := s.Ownership().OwnsOrders([]string{"ord-never"}); err == nil {
		t.Fatalf("a failed binding classified order ord-never as %v; the "+
			"ledger does not know whether it is ours, and both answers it "+
			"could give are wrong", owned)
	}
	if _, ok := s.Ownership().Bound("ord-never"); ok {
		t.Fatal("a failed binding entered the committed ownership index")
	}
	if row, ok, err := s.Reader().OwnedOrder(coid); err != nil || ok {
		t.Fatalf("a failed binding left a row: ok=%v row=%+v err=%v",
			ok, row, err)
	}
}

// TestOwnershipSurvivesRunsAndTerminalOrders is H-ORD-9's "never by heuristic,
// never by run_id".
//
// An order placed by a previous incarnation is still ours, and an order that has
// since gone terminal is still ours: `owned_order` rows are never deleted and
// are never filtered by the current run. The test spans three opens of one
// database on purpose -- the index is rebuilt from disk each time, and the
// property has to survive that rebuild rather than a process's memory.
//
// `M-HS-OWNRUN` scopes the load to the most recent run, which makes every fill
// from before the last restart foreign: a SEV1 and a global stop fired by
// starting up correctly.
func TestOwnershipSurvivesRunsAndTerminalOrders(t *testing.T) {
	dbPath, logPath := paths(t)

	// --- run A --------------------------------------------------------------
	s1 := openAt(t, dbPath, logPath)
	h1 := begin(t, s1, "runa", 1_700_000_000_000)
	oa := order(t, "runa", 1, quote.SideYes)
	reserve(t, s1, h1, oa, quote.RoleAdding, 1_700_000_001_000)
	bind(t, s1, oa.ClientOrderID(), "ord-a", 1_700_000_002_000)
	if err := s1.Close(); err != nil {
		t.Fatalf("close run A: %v", err)
	}

	// --- run B, later ------------------------------------------------------
	s2 := openAt(t, dbPath, logPath)
	h2 := begin(t, s2, "runb", 1_700_000_100_000)
	ob := order(t, "runb", 1, quote.SideNo)
	reserve(t, s2, h2, ob, quote.RoleReducing, 1_700_000_101_000)
	bind(t, s2, ob.ClientOrderID(), "ord-b", 1_700_000_102_000)
	if err := s2.Close(); err != nil {
		t.Fatalf("close run B: %v", err)
	}

	// --- run C, before it has even begun -----------------------------------
	s3 := openAt(t, dbPath, logPath)
	t.Cleanup(func() { s3.Close() })

	got, err := s3.Ownership().OwnsOrders([]string{"ord-a", "ord-b", "ord-x"})
	if err != nil {
		t.Fatalf("OwnsOrders: %v", err)
	}
	want := []bool{true, true, false}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("OwnsOrders = %v, want %v. An order from a previous "+
			"incarnation is still ours: H-ORD-9 classifies against the durable "+
			"ledger, never by run_id, and a run-scoped index makes every fill "+
			"from before the last restart a SEV1 foreign fill", got, want)
	}

	// Both rows survive, with their bindings intact.
	rows, err := s3.Reader().OwnedOrders()
	if err != nil {
		t.Fatalf("read owned_order: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("owned_order holds %d rows, want 2; rows are never deleted",
			len(rows))
	}
	for _, r := range rows {
		if !r.Bound {
			t.Fatalf("row %+v lost its binding across a reopen", r)
		}
	}
	if rows[0].RunID != "runa" || rows[1].RunID != "runb" {
		t.Fatalf("rows %+v", rows)
	}

	// The length contract holds for an empty batch too.
	empty, err := s3.Ownership().OwnsOrders(nil)
	if err != nil || len(empty) != 0 {
		t.Fatalf("empty batch: %v %v", empty, err)
	}
}

// TestOurFillFirstObserverWinsAcrossRuns is H-ORD-6's literal `INSERT OR
// IGNORE`.
//
// A fill seen live and re-read by a later run's backfill walk is the same fill.
// Letting the backfill overwrite it relabels a live observation as history --
// and `backfilled` is the field that says whether we were watching when it
// happened. `M-HS-FILLREPLACE` makes the last writer win.
func TestOurFillFirstObserverWinsAcrossRuns(t *testing.T) {
	dbPath, logPath := paths(t)

	s1 := openAt(t, dbPath, logPath)
	h1 := begin(t, s1, "runa", 1_700_000_000_000)
	oa := order(t, "runa", 1, quote.SideYes)
	reserve(t, s1, h1, oa, quote.RoleAdding, 1_700_000_001_000)
	bind(t, s1, oa.ClientOrderID(), "ord-a", 1_700_000_002_000)

	live := fill("trade-1", "ord-a")
	if _, err := s1.RecordFill(h1, live, 1_700_000_003_000, false); err != nil {
		t.Fatalf("RecordFill: %v", err)
	}
	for _, r := range pump(t, s1) {
		if !r.OK() {
			t.Fatalf("first fill failed: %v", r.Err)
		}
	}
	if err := s1.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// A later run walks `backfill_h` of history and sees the same trade.
	s2 := openAt(t, dbPath, logPath)
	t.Cleanup(func() { s2.Close() })
	h2 := begin(t, s2, "runb", 1_700_000_100_000)
	again := fill("trade-1", "ord-a")
	again.Price4 = 9999
	if _, err := s2.RecordFill(h2, again, 1_700_000_101_000, true); err != nil {
		t.Fatalf("RecordFill (backfill): %v", err)
	}
	for _, r := range pump(t, s2) {
		if !r.OK() {
			t.Fatalf("backfilled fill failed: %v", r.Err)
		}
	}

	row, ok, err := s2.Reader().Fill("trade-1")
	if err != nil || !ok {
		t.Fatalf("read fill: ok=%v err=%v", ok, err)
	}
	if row.FirstRunID != "runa" {
		t.Fatalf("first_run_id is %q, want runa: the FIRST observer wins",
			row.FirstRunID)
	}
	if row.Backfilled {
		t.Fatal("a live observation was relabelled as backfilled history by a " +
			"later run's walk; `backfilled` says whether we were watching when " +
			"the fill happened, and the second reader was not")
	}
	if row.FirstSeenMs != 1_700_000_003_000 {
		t.Fatalf("first_seen_ms %d, want the first sighting", row.FirstSeenMs)
	}
	if row.Price4 != 5000 {
		t.Fatalf("price4 %d, want the first observer's 5000", row.Price4)
	}
	if row.ExchangeTsMs != live.ExchangeTsMs {
		t.Fatalf("exchange_ts_ms %d, want %d", row.ExchangeTsMs,
			live.ExchangeTsMs)
	}
}
