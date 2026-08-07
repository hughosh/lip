package hstore

import (
	"reflect"
	"testing"

	"lip/harness/quote"
	"lip/harness/rest"
	"lip/harness/risk"
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
// The two cases now answer differently, and the difference is deliberate. A
// binding IN FLIGHT is `OwnershipUnresolved`: the store is fine, it has not
// concluded, and the caller defers. A binding the writer has GIVEN UP on is a
// whole-batch error: the store is faulty, and nothing it says about any id in
// the batch can be relied on. Both are "unclassifiable"; only one of them is a
// reason to distrust the other answers in the same walk.
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

	// In flight: unresolved. Not owned, and not foreign either.
	owned, err := s.Ownership().OwnsOrders([]string{"ord-never"})
	if err != nil {
		t.Fatalf("an in-flight binding poisoned the whole batch (%v); a "+
			"submitted binding is a store working normally, and the walk it "+
			"arrived in is still classifiable", err)
	}
	if len(owned) != 1 || owned[0] != risk.OwnershipUnresolved {
		t.Fatalf("an uncommitted binding classified order ord-never as %v, "+
			"want [unresolved]; H-ORD-9 answers from the DURABLE ledger, so an "+
			"in-flight intention is not ownership -- and it is not evidence of "+
			"a third party either", owned)
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

	// Both reservations were bound before their runs closed, so the ledger has
	// accounted for every order it could have created and `ord-x` is
	// CONCLUSIVELY foreign. That precondition is asserted rather than assumed:
	// with a reservation still outstanding the third answer would legitimately
	// be `unresolved`, and this test would then be pinning the run-scoping
	// property through a code path that never reaches the foreign arm.
	if n := s3.Ownership().UnresolvedCount(); n != 0 {
		t.Fatalf("%d reservation(s) outstanding after two runs that bound "+
			"every order they reserved; the foreign answer below would be "+
			"deferred rather than conclusive", n)
	}
	got, err := s3.Ownership().OwnsOrders([]string{"ord-a", "ord-b", "ord-x"})
	if err != nil {
		t.Fatalf("OwnsOrders: %v", err)
	}
	want := []risk.Ownership{risk.OwnershipOurs, risk.OwnershipOurs,
		risk.OwnershipForeign}
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

// TestDivergentDuplicateTradeIsRefusedNotSwallowed is the other half of
// H-ORD-6, and the half a bare `INSERT OR IGNORE` cannot express.
//
// "First observer wins" is a rule about the OBSERVATION columns -- which run
// saw it, when, and whether it was live or backfilled. It is not a rule about
// the exchange facts, and it must not be used as one: `INSERT OR IGNORE`
// cannot tell "the same fill again" from "a different account of the same
// trade id", and it discards the second silently in both cases. A trade that
// comes back with another price or another count is the exchange contradicting
// itself, or this process reading the wrong field, and either way it is the
// evidence trail disagreeing with itself about money. Swallowed, it leaves the
// database asserting one price and the account holding another, with nothing
// anywhere recording that both were seen.
//
// So: identical on the exchange facts is idempotent, divergent is PERMANENT,
// and the observation columns are excluded from the comparison entirely.
//
// `M-HS-FILLBLINDDUP` restores the blind duplicate.
func TestDivergentDuplicateTradeIsRefusedNotSwallowed(t *testing.T) {
	s := tempStore(t)
	h := begin(t, s, "runa", 1_700_000_000_000)
	o := order(t, "runa", 1, quote.SideYes)
	reserve(t, s, h, o, quote.RoleAdding, 1_700_000_001_000)
	bind(t, s, o.ClientOrderID(), "ord-a", 1_700_000_002_000)

	live := fill("trade-1", "ord-a")
	if _, err := s.RecordFill(h, live, 1_700_000_003_000, false); err != nil {
		t.Fatalf("RecordFill: %v", err)
	}
	for _, r := range pump(t, s) {
		if !r.OK() {
			t.Fatalf("the first sighting failed: %v", r.Err)
		}
	}

	// The SAME facts again, from a backfill walk: idempotent, and the store is
	// still healthy afterwards. Asserted, not assumed -- a rule that refused
	// this too would make every restart's backfill a permanent fault.
	same := fill("trade-1", "ord-a")
	if _, err := s.RecordFill(h, same, 1_700_000_004_000, true); err != nil {
		t.Fatalf("RecordFill (identical): %v", err)
	}
	for _, r := range pump(t, s) {
		if !r.OK() {
			t.Fatalf("an identical re-observation of trade-1 was rejected: %v",
				r.Err)
		}
	}
	if hl := s.Health(); !hl.Healthy() {
		t.Fatalf("an identical duplicate made the store unhealthy: %+v", hl)
	}

	// A different price for the same trade id: two irreconcilable accounts of
	// one exchange fact.
	other := fill("trade-1", "ord-a")
	other.Price4 = 9999
	rcpt, err := s.RecordFill(h, other, 1_700_000_005_000, false)
	if err != nil {
		t.Fatalf("RecordFill (divergent): %v", err)
	}
	var outcome Result
	var seen bool
	for _, r := range pump(t, s) {
		if r.Receipt.Seq() == rcpt.Seq() {
			outcome, seen = r, true
		}
	}
	if !seen {
		t.Fatal("the divergent duplicate produced no result at all")
	}
	if outcome.Err == nil {
		t.Fatal("a trade id that came back with a DIFFERENT price was accepted " +
			"as already recorded; the database now asserts one price and the " +
			"account holds another, and nothing anywhere records that both " +
			"were seen")
	}

	// The first observer's row is untouched: a refusal is not a rewrite.
	row, ok, err := s.Reader().Fill("trade-1")
	if err != nil || !ok {
		t.Fatalf("read fill: ok=%v err=%v", ok, err)
	}
	if row.Price4 != 5000 {
		t.Fatalf("price4 is %d after a refused divergent duplicate, want the "+
			"first observer's 5000", row.Price4)
	}
	if row.FirstRunID != "runa" || row.Backfilled {
		t.Fatalf("the observation columns moved: run=%q backfilled=%v",
			row.FirstRunID, row.Backfilled)
	}

	// And the contradiction is a permanent fault, not a retry: this store has
	// a hole in it and never reports healthy again.
	if hl := s.Health(); hl.Healthy() || hl.AllowsAdding() {
		t.Fatalf("a permanently rejected fill left the store reporting %+v", hl)
	}
}

// TestOurFillFirstObserverWinsAcrossRuns is H-ORD-6's observation columns.
//
// A fill seen live and re-read by a later run's backfill walk is the same fill.
// Letting the backfill overwrite it relabels a live observation as history --
// and `backfilled` is the field that says whether we were watching when it
// happened. The exchange facts are identical here on purpose: a divergence in
// THOSE is a permanent refusal, which is
// `TestDivergentDuplicateTradeIsRefusedNotSwallowed`. This test is about the
// three columns that record who saw it and when.
//
// `M-HS-FILLREPLACE` makes the last writer win.
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
	// The same exchange facts, which is what a re-read of the same trade
	// actually looks like. Only the OBSERVATION differs: a later run, a later
	// sighting, and `backfilled` true.
	again := fill("trade-1", "ord-a")
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
	if row.ExchangeTsMs != live.ExchangeTsMs {
		t.Fatalf("exchange_ts_ms %d, want %d", row.ExchangeTsMs,
			live.ExchangeTsMs)
	}
}
