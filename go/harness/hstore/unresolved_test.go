package hstore

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	"lip/harness/quote"
	"lip/harness/rest"
	"lip/harness/risk"
)

// abandon submits and drains a reservation abandonment.
func abandon(t *testing.T, s *Store, coid string, ms int64) []Result {
	t.Helper()
	if _, err := s.ResolveReservationAbandoned(coid, ms); err != nil {
		t.Fatalf("ResolveReservationAbandoned(%s): %v", coid, err)
	}
	return pump(t, s)
}

// firstErr returns the first failed result, so a test can assert on WHY a
// record was refused rather than only that something was.
func firstErr(results []Result) error {
	for _, r := range results {
		if r.Err != nil {
			return r.Err
		}
	}
	return nil
}

// TestReservedUnboundCoidSurvivesReopenAsUnresolved is FINDING 2, at the exact
// point it does its damage: the first `Open` after a crash.
//
// H-ORD-6 commits the reservation BEFORE the order is dispatched and learns the
// exchange order id afterwards. A SIGKILL between those two commits -- which is
// the crash the two-stage design exists to survive -- leaves a durable
// reservation with no order id, and a live order on the exchange whose id this
// process has never seen. The next poll's fill on that order carries an id
// absent from the ledger.
//
// The old index could not represent that state. `loadBindings` selected
// `order_id IS NOT NULL`, nothing else seeded the index at `Open`, and an
// unknown id therefore answered a plain `false`: FOREIGN. Which is
// `SEV1 FOREIGN_FILL`, a global stop, and a durable operator-only WINDING_DOWN
// latch -- H-ORD-9's forbidden catastrophe, produced by our own order.
//
// So the property is about what survives the reopen. Not that the running
// process remembers its reservation -- it does not exist any more -- but that
// the DATABASE does, and that the rebuilt index reads it.
//
// `M-HS-OWNNULLSKIP` restores the `order_id IS NOT NULL` filter.
func TestReservedUnboundCoidSurvivesReopenAsUnresolved(t *testing.T) {
	dbPath, logPath := paths(t)

	// --- the run that died between the two commits --------------------------
	s1 := openAt(t, dbPath, logPath)
	h1 := begin(t, s1, "runa", 1_700_000_000_000)
	o := order(t, "runa", 1, quote.SideYes)
	permit := reserve(t, s1, h1, o, quote.RoleAdding, 1_700_000_001_000)
	coid := permit.Coid()

	// The dispatch happened -- the permit is the licence for it -- and the
	// binding did not. Nothing else is written.
	if s1.Ownership().UnresolvedCount() != 1 {
		t.Fatalf("a committed reservation did not open the unresolved window; "+
			"count = %d, want 1. The permit that authorises the dispatch and "+
			"the state that makes the resulting order id unrecognisable-but-"+
			"ours are the same event", s1.Ownership().UnresolvedCount())
	}
	if err := s1.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// --- the process that comes back up -------------------------------------
	s2 := openAt(t, dbPath, logPath)
	t.Cleanup(func() { s2.Close() })

	if n := s2.Ownership().UnresolvedCount(); n != 1 {
		t.Fatalf("after reopening the same database the unresolved "+
			"reservation count is %d, want 1: the row is on disk with a NULL "+
			"order_id, and an Open that does not read it starts life believing "+
			"it has no orders outstanding", n)
	}
	if _, outstanding := s2.Ownership().Unresolved()[coid]; !outstanding {
		t.Fatalf("coid %s is not in the reopened unresolved set %v", coid,
			s2.Ownership().Unresolved())
	}

	// The fill arrives on the order id we never learned.
	got, err := s2.Ownership().OwnsOrders([]string{"ord-dispatched-never-bound"})
	if err != nil {
		t.Fatalf("OwnsOrders: %v", err)
	}
	if len(got) != 1 || got[0] != risk.OwnershipUnresolved {
		t.Fatalf("a fill on our own dispatched-but-unbound order classified as "+
			"%v, want [unresolved]. Answering `foreign` here is a SEV1, a "+
			"global stop and a durable WINDING_DOWN latch that only an "+
			"operator can clear, declared about an order this ledger itself "+
			"authorised", got)
	}

	// The row is intact and is still in neither terminal state.
	row, ok, err := s2.Reader().OwnedOrder(coid)
	if err != nil || !ok {
		t.Fatalf("owned_order(%s): ok=%v err=%v", coid, ok, err)
	}
	if row.Bound || row.Abandoned {
		t.Fatalf("the reservation resolved itself across the crash: %+v", row)
	}

	// And binding it late -- which is what `lip-eyq`'s walk does -- closes the
	// window rather than leaving it open forever.
	bind(t, s2, coid, "ord-dispatched-never-bound", 1_700_000_010_000)
	if n := s2.Ownership().UnresolvedCount(); n != 0 {
		t.Fatalf("a committed binding left %d reservation(s) outstanding; the "+
			"coid is resolved, and leaving it in the set defers every "+
			"unrecognised fill forever", n)
	}
	got, err = s2.Ownership().OwnsOrders([]string{"ord-dispatched-never-bound"})
	if err != nil {
		t.Fatalf("OwnsOrders after the late bind: %v", err)
	}
	if len(got) != 1 || got[0] != risk.OwnershipOurs {
		t.Fatalf("after the durable binding the order classified as %v, want "+
			"[ours]", got)
	}
}

// TestOwnsOrdersConclusiveForeignRequiresNoUnresolvedReservations pins the
// OTHER half, and it is the half that keeps H-ORD-9 a rule with consequences.
//
// Deferring is safe and it is also a way to never answer. If an unrecognised id
// deferred unconditionally there would be no foreign fill ever, and F14's
// detection of a third party trading the account would be dead code. So
// "foreign" has to remain reachable, and the condition for reaching it is
// precise: the ledger has accounted for every order it could have created, i.e.
// the unresolved set is EMPTY.
//
// The test drives one store across that boundary with the SAME unknown order id
// throughout, so the only thing that changes between the two answers is whether
// a reservation is outstanding.
//
// `M-HS-OWNCONCLUSIVE` drops the deferral arm, which is FINDING 2 restored.
func TestOwnsOrdersConclusiveForeignRequiresNoUnresolvedReservations(t *testing.T) {
	s := tempStore(t)
	h := begin(t, s, "runa", 1_700_000_000_000)

	const stranger = "ord-not-ours"

	// --- an empty ledger is already conclusive ------------------------------
	//
	// Asserted first so the FOREIGN answer below is known to be reachable in
	// this fixture at all. A test whose "conclusive" case is unreachable would
	// pass while asserting nothing.
	got, err := s.Ownership().OwnsOrders([]string{stranger})
	if err != nil {
		t.Fatalf("OwnsOrders on an empty ledger: %v", err)
	}
	if got[0] != risk.OwnershipForeign {
		t.Fatalf("with no reservations outstanding an unknown order id "+
			"classified as %v, want [foreign]; a ledger that has accounted for "+
			"every order it ever created and does not recognise this one is "+
			"looking at somebody else's fill", got)
	}

	// --- one outstanding reservation defers the SAME id ---------------------
	oa := order(t, "runa", 1, quote.SideYes)
	pa := reserve(t, s, h, oa, quote.RoleAdding, 1_700_000_001_000)
	ob := order(t, "runa", 2, quote.SideNo)
	pb := reserve(t, s, h, ob, quote.RoleReducing, 1_700_000_002_000)

	got, err = s.Ownership().OwnsOrders([]string{stranger})
	if err != nil {
		t.Fatalf("OwnsOrders with reservations outstanding: %v", err)
	}
	if got[0] != risk.OwnershipUnresolved {
		t.Fatalf("with 2 reservations outstanding an unknown order id "+
			"classified as %v, want [unresolved]. Either of those coids may "+
			"already be a live order on the exchange under an id we have not "+
			"learned, so this id is not evidence of a third party", got)
	}

	// --- resolving ONE of two is not enough ---------------------------------
	bind(t, s, pa.Coid(), "ord-a", 1_700_000_003_000)
	got, err = s.Ownership().OwnsOrders([]string{stranger})
	if err != nil {
		t.Fatalf("OwnsOrders with one reservation left: %v", err)
	}
	if got[0] != risk.OwnershipUnresolved {
		t.Fatalf("with 1 reservation still outstanding the unknown id "+
			"classified as %v, want [unresolved]; the gate is the whole set "+
			"being empty, not most of it", got)
	}

	// --- resolving the last one restores the conclusion ---------------------
	bind(t, s, pb.Coid(), "ord-b", 1_700_000_004_000)
	if n := s.Ownership().UnresolvedCount(); n != 0 {
		t.Fatalf("%d reservation(s) outstanding after binding both", n)
	}
	got, err = s.Ownership().OwnsOrders([]string{stranger})
	if err != nil {
		t.Fatalf("OwnsOrders after both bindings: %v", err)
	}
	if got[0] != risk.OwnershipForeign {
		t.Fatalf("with every reservation resolved the unknown id classified "+
			"as %v, want [foreign]. If this stayed unresolved, H-ORD-9's "+
			"foreign-fill detection would be unreachable and F14 would never "+
			"fire for a real third party", got)
	}

	// Our own bound orders are unaffected by any of it.
	mine, err := s.Ownership().OwnsOrders([]string{"ord-a", "ord-b"})
	if err != nil {
		t.Fatalf("OwnsOrders on our own orders: %v", err)
	}
	if mine[0] != risk.OwnershipOurs || mine[1] != risk.OwnershipOurs {
		t.Fatalf("our own bound orders classified as %v", mine)
	}
}

// TestAbandonedReservationRestoresConclusiveClassification is the drain.
//
// Binding is not the only way a reservation ends. An order the exchange refused,
// or one we crashed before sending, produces a coid that will NEVER have an
// order id -- and with no way to record that, the first such reservation would
// defer every unrecognised fill for the life of the deployment. That converts
// this repair from "no false foreign" into "no foreign at all", which is the
// same rule being disabled from the other side.
//
// `abandoned_ms` is that record, and the test also pins the three refusals that
// keep it from being usable as an eraser: a bound coid cannot be abandoned, an
// abandoned coid cannot be bound, and an unknown coid cannot be either.
func TestAbandonedReservationRestoresConclusiveClassification(t *testing.T) {
	dbPath, logPath := paths(t)
	s := openAt(t, dbPath, logPath)
	h := begin(t, s, "runa", 1_700_000_000_000)

	const stranger = "ord-not-ours"

	oa := order(t, "runa", 1, quote.SideYes)
	pa := reserve(t, s, h, oa, quote.RoleAdding, 1_700_000_001_000)

	got, err := s.Ownership().OwnsOrders([]string{stranger})
	if err != nil {
		t.Fatalf("OwnsOrders: %v", err)
	}
	if got[0] != risk.OwnershipUnresolved {
		t.Fatalf("an unknown id with a reservation outstanding classified as "+
			"%v, want [unresolved]", got)
	}

	// The exchange never took it.
	if err := firstErr(abandon(t, s, pa.Coid(), 1_700_000_005_000)); err != nil {
		t.Fatalf("abandoning an outstanding reservation failed: %v", err)
	}
	if n := s.Ownership().UnresolvedCount(); n != 0 {
		t.Fatalf("%d reservation(s) outstanding after abandonment, want 0", n)
	}
	got, err = s.Ownership().OwnsOrders([]string{stranger})
	if err != nil {
		t.Fatalf("OwnsOrders after abandonment: %v", err)
	}
	if got[0] != risk.OwnershipForeign {
		t.Fatalf("after the last reservation was abandoned an unknown id "+
			"classified as %v, want [foreign]; without a drain the first "+
			"reservation the exchange refused would defer every fill forever",
			got)
	}

	// Idempotent: `lip-eyq`'s walk re-runs on every startup.
	if err := firstErr(abandon(t, s, pa.Coid(), 1_700_000_006_000)); err != nil {
		t.Fatalf("re-abandoning an abandoned reservation was refused: %v", err)
	}

	// A bound coid cannot be abandoned.
	ob := order(t, "runa", 2, quote.SideNo)
	pb := reserve(t, s, h, ob, quote.RoleReducing, 1_700_000_007_000)
	bind(t, s, pb.Coid(), "ord-b", 1_700_000_008_000)
	if _, err := s.ResolveReservationAbandoned(pb.Coid(), 1_700_000_009_000); err != nil {
		t.Fatalf("submit: %v", err)
	}
	err = firstErr(pump(t, s))
	if err == nil || !strings.Contains(err.Error(), "ord-b") {
		t.Fatalf("abandoning a BOUND coid was accepted (%v); the exchange "+
			"demonstrably took that order, and a ledger that recorded "+
			"otherwise would classify its own fills as foreign", err)
	}

	// An abandoned coid cannot be bound.
	if _, err := s.BindOrder(pa.Coid(), "ord-late", 1_700_000_010_000); err != nil {
		t.Fatalf("submit: %v", err)
	}
	err = firstErr(pump(t, s))
	if err == nil || !strings.Contains(err.Error(), "abandoned") {
		t.Fatalf("binding an ABANDONED coid was accepted (%v); the ledger "+
			"already concluded the exchange never took it, and both records "+
			"cannot be true", err)
	}

	// An unknown coid is neither.
	unknown, err := rest.Coid("runa", 0, quote.SideYes, 99)
	if err != nil {
		t.Fatalf("coid: %v", err)
	}
	if _, err := s.ResolveReservationAbandoned(unknown, 1_700_000_011_000); err != nil {
		t.Fatalf("submit: %v", err)
	}
	if err := firstErr(pump(t, s)); err == nil {
		t.Fatal("abandoning a coid with no reservation was accepted; it " +
			"records a conclusion about a dispatch this store has no evidence " +
			"of")
	}

	// The abandonment is DURABLE, not a memory of this process. `lip-eyq`'s
	// walk runs after a restart, and a resolution that did not survive one
	// would have the whole account deferring again on the next boot.
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	s2 := openAt(t, dbPath, logPath)
	t.Cleanup(func() { s2.Close() })

	row, ok, err := s2.Reader().OwnedOrder(pa.Coid())
	if err != nil || !ok {
		t.Fatalf("owned_order(%s): ok=%v err=%v", pa.Coid(), ok, err)
	}
	if !row.Abandoned || row.AbandonedMs != 1_700_000_005_000 {
		t.Fatalf("the abandonment did not survive the reopen with its FIRST "+
			"timestamp: %+v", row)
	}
	if n := s2.Ownership().UnresolvedCount(); n != 0 {
		t.Fatalf("%d reservation(s) outstanding after reopening a ledger whose "+
			"only reservations were bound or abandoned", n)
	}
	got, err = s2.Ownership().OwnsOrders([]string{stranger})
	if err != nil {
		t.Fatalf("OwnsOrders after reopen: %v", err)
	}
	if got[0] != risk.OwnershipForeign {
		t.Fatalf("after a reopen the unknown id classified as %v, want "+
			"[foreign]", got)
	}
}

// legacySchemaV1 is the exact `owned_order` this schema replaced: no
// `abandoned_ms`, so a reservation that was never bound is indistinguishable
// from one the exchange refused. The other four tables are unchanged, which is
// what makes the refusal about the VERSION rather than about a table set that
// happens not to match.
const legacySchemaV1 = `
CREATE TABLE run (
    run_id      TEXT    PRIMARY KEY,
    started_ms  INTEGER NOT NULL,
    config_json BLOB    NOT NULL
);
CREATE TABLE owned_order (
    coid        TEXT    PRIMARY KEY,
    run_id      TEXT    NOT NULL REFERENCES run(run_id),
    reserved_ms INTEGER NOT NULL,
    ticker      TEXT    NOT NULL,
    side        TEXT    NOT NULL,
    role        TEXT    NOT NULL,
    price_cents INTEGER NOT NULL,
    count_q     INTEGER NOT NULL,
    order_id    TEXT    UNIQUE,
    bound_ms    INTEGER,
    CHECK ((order_id IS NULL) = (bound_ms IS NULL))
);
CREATE TABLE our_fill (
    trade_id       TEXT    PRIMARY KEY,
    first_run_id   TEXT    NOT NULL REFERENCES run(run_id),
    first_seen_ms  INTEGER NOT NULL,
    backfilled     INTEGER NOT NULL,
    order_id       TEXT    NOT NULL REFERENCES owned_order(order_id),
    ticker         TEXT    NOT NULL,
    side           TEXT    NOT NULL,
    price4         INTEGER NOT NULL,
    count_q        INTEGER NOT NULL,
    fee_micros     INTEGER NOT NULL,
    is_taker       INTEGER NOT NULL,
    exchange_ts_ms INTEGER NOT NULL
);
CREATE TABLE state_event (
    event_id   TEXT    PRIMARY KEY,
    run_id     TEXT    NOT NULL REFERENCES run(run_id),
    ts_ms      INTEGER NOT NULL,
    scope      TEXT    NOT NULL,
    ticker     TEXT    NOT NULL,
    from_state TEXT    NOT NULL,
    to_state   TEXT    NOT NULL,
    "trigger"  TEXT    NOT NULL
);
CREATE TABLE anomaly (
    anomaly_id       TEXT    PRIMARY KEY,
    run_id           TEXT    NOT NULL REFERENCES run(run_id),
    class            TEXT    NOT NULL,
    sev              INTEGER NOT NULL,
    ticker           TEXT    NOT NULL,
    text             TEXT    NOT NULL,
    first_ms         INTEGER NOT NULL,
    journaled_ms     INTEGER,
    delivered_ms     INTEGER,
    attempts         INTEGER NOT NULL,
    last_attempt_ms  INTEGER,
    suppressed_count INTEGER NOT NULL
);
PRAGMA user_version=1;
`

// TestOpenRefusesTheVersionOneSchemaItReplaced is the explicit half of the
// schema bump.
//
// `inspectDatabase`'s default arm already rejects any version it does not know,
// so this branch is not what makes a v1 file safe -- it is what makes the
// refusal LEGIBLE. The generic message says "this process does not know what
// its owned_order rows mean", which is true and useless: the operator's next
// question is whether their data can be migrated, and the answer is that there
// is no migration and none is owed, because nothing was ever deployed on v1.
//
// The substantive claim underneath is the one the message has to carry. In v1 a
// reservation with a NULL `order_id` could be either of two things -- still
// outstanding, or one the exchange refused -- and the whole F2 repair turns on
// telling them apart. Reading a v1 file would therefore have to guess, and both
// guesses are the defect: guessing "outstanding" defers every unrecognised fill
// forever, guessing "refused" declares our own order foreign.
func TestOpenRefusesTheVersionOneSchemaItReplaced(t *testing.T) {
	dir := t.TempDir()
	legacy := filepath.Join(dir, "harness.db")

	raw, err := sql.Open("sqlite", legacy)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(legacySchemaV1); err != nil {
		t.Fatalf("seed the v1 database: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}
	before := journalModeAt(t, legacy)

	st, err := Open(StoreConfig{DBPath: legacy,
		AnomalyLogPath: filepath.Join(dir, "a.jsonl")})
	if err == nil {
		st.Close()
		t.Fatalf("Open accepted a version-%d database. Its owned_order has no "+
			"abandoned_ms, so an unbound reservation in it is ambiguous "+
			"between still-outstanding and never-taken -- and the ownership "+
			"answer for an unrecognised order id depends on which it was",
			legacySchemaVersion)
	}
	// The message has to be actionable, not merely correct.
	for _, want := range []string{legacy, "abandoned_ms", "no migration"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("the v1 refusal does not mention %q, so the operator "+
				"cannot tell which file to move aside or why: %v", want, err)
		}
	}

	// And, as with any other rejected file, it was not written to on the way.
	if after := journalModeAt(t, legacy); after != before {
		t.Fatalf("the v1 database's journal mode was changed from %q to %q by "+
			"an Open that then rejected it", before, after)
	}
	var version int64
	ro, err := sql.Open("sqlite", "file:"+legacy+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	if err := ro.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != legacySchemaVersion {
		t.Fatalf("the rejected file's user_version was changed to %d", version)
	}
}
