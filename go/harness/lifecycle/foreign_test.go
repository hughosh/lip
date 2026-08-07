package lifecycle

import (
	"context"
	"reflect"
	"testing"

	"lip/harness/num"
	"lip/harness/quote"
	"lip/harness/rest"
	"lip/harness/risk"
)

// TestStartupForeignOrderIsExcludedAndNeverCancelled is §7.5 step 5's
// not-ours branch, and F15.
//
// > coid does not match → **not ours.** Do not cancel it. Ping `SEV2`
// > (`FOREIGN_ORDER`) and exclude that market from selection [...]
//
// Never cancelled, in either phase: "cancelling someone else's -- or a previous
// incarnation's -- orders is a destructive act taken on incomplete information".
// `M-L-FOREIGNCANCEL` includes a foreign order in the cancel group, which is the
// harness acting on somebody else's risk on the strength of a coid it did not
// recognise.
func TestStartupForeignOrderIsExcludedAndNeverCancelled(t *testing.T) {
	src := okSource()
	src.orders = rest.OrdersResult{Walk: completeWalk(), Orders: []rest.Order{
		ourOrder("mine", "A"),
		foreignOrder("theirs", "B"),
	}}
	policy := &fixedPolicy{decision: AdoptionCancel}
	sweeper := newSweeperOn(src, true)

	s := newStartup(t, &recordingLatch{}, src, ownsAll(), policy, sweeper, "A", "B")
	at := s.Step(context.Background(), startupNow)
	if at.Err != nil {
		t.Fatalf("Run: %v", at.Err)
	}

	// The policy is a cancel-everything policy, so if a foreign order ever
	// reached it, the sweeper would have been asked to cancel it.
	for _, o := range policy.seen {
		if !o.Ours {
			t.Fatalf("the adoption policy was shown foreign order %s; there is "+
				"no reading of §7.5 under which \"cancel\" is an available "+
				"answer for an order that is not ours", o.OrderID)
		}
	}
	for _, o := range sweeper.swept() {
		if o.OrderID == "theirs" {
			t.Fatal("a foreign order was cancelled at startup")
		}
	}

	if got := at.Adoption.Excluded(); !reflect.DeepEqual(got, []string{"B"}) {
		t.Fatalf("excluded %v, want [B]", describe(got))
	}
	foreign := at.Adoption.Foreign()
	if len(foreign) != 1 || foreign[0].OrderID != "theirs" {
		t.Fatalf("foreign orders %v, want exactly theirs", foreign)
	}
	if sev, ok := sevOf(at.Adoption.Anomalies(), "FOREIGN_ORDER"); !ok ||
		sev != risk.SEV2 {
		t.Fatalf("startup FOREIGN_ORDER severity %v (present=%v), want SEV2: a "+
			"pre-existing order on an account we are adopting is not a live "+
			"third party", sev, ok)
	}

	// A pre-existing foreign order alone is NOT a global stop.
	if len(at.Adoption.Causes()) != 0 {
		t.Fatalf("a pre-existing foreign order latched a global stop: %v",
			at.Adoption.Causes())
	}
}

// TestForeignOnlyTickerWithInventoryStaysManagedAndReducing is H-SEL-11 beating
// F15 where they collide.
//
// > H-SEL-11 wins over F15 on the exclusion question: a market with `q != 0`
// > stays under management in `REDUCING`. Exclusion applies to *new selection*,
// > not to abandoning inventory.
//
// This is HR-024's first collision, and getting it wrong in the other direction
// abandons a position because somebody left an unrelated order in the same
// market.
func TestForeignOnlyTickerWithInventoryStaysManagedAndReducing(t *testing.T) {
	src := okSource()
	src.positions = rest.PositionsResult{
		Walk:     completeWalk(),
		ByTicker: map[string]num.Qty{"CONTESTED": num.QtyFromFloat(5)},
	}
	src.orders = rest.OrdersResult{Walk: completeWalk(), Orders: []rest.Order{
		foreignOrder("theirs", "CONTESTED"),
	}}

	s := newStartup(t, &recordingLatch{}, src, ownsAll(), keepAll(),
		newSweeper(true))
	at := s.Step(context.Background(), startupNow)
	if at.Err != nil {
		t.Fatalf("Run: %v", at.Err)
	}

	if got := at.Adoption.Excluded(); !reflect.DeepEqual(got, []string{"CONTESTED"}) {
		t.Fatalf("excluded %v, want [CONTESTED]", describe(got))
	}
	if got := at.Adoption.Managed(); !reflect.DeepEqual(got, []string{"CONTESTED"}) {
		t.Fatalf("managed %v: exclusion applies to NEW SELECTION, not to "+
			"abandoning inventory", describe(got))
	}
	if st := at.Adoption.States()["CONTESTED"]; st != quote.Reducing {
		t.Fatalf("state %v, want REDUCING", st)
	}
}

// TestLiveForeignActivityRequestsDurableGlobalStop is H-ORD-9's asymmetry across
// the startup boundary.
//
// > **Foreign activity appearing *after* startup latches global
// > `WINDING_DOWN`** (H-ORD-9), because by then it is not a pre-existing
// > condition we adopted but a live third party trading the account we are
// > modelling.
//
// And for fills there is no startup exemption AT ALL: a resting order tells us
// someone placed it; a fill tells us someone is trading the account whose
// position our model describes, and "that model is now unreliable everywhere,
// not in one market".
//
// `M-L-FOREIGNLIVE` treats a live foreign order as a startup-only exclusion,
// which leaves the harness quoting alongside a third party.
func TestLiveForeignActivityRequestsDurableGlobalStop(t *testing.T) {
	guard, err := NewForeignGuard(ownsAll("mine"))
	if err != nil {
		t.Fatalf("NewForeignGuard: %v", err)
	}
	orders := []rest.Order{foreignOrder("theirs", "M")}

	// The zero Phase is invalid and must be treated as live, not as startup: a
	// three-valued phase whose zero value was permissive would make the
	// permissive reading the default for every caller that forgot to set it.
	for _, phase := range []Phase{PhaseLive, PhaseUnset} {
		eff, err := guard.Classify(phase, orders, nil, 99)
		if err != nil {
			t.Fatalf("phase %v: classify: %v", phase, err)
		}
		if !eff.Stop() {
			t.Fatalf("phase %v: a foreign order after startup did not request a "+
				"global stop", phase)
		}
		if sev, _ := sevOf(eff.Anomalies, "FOREIGN_ORDER"); sev != risk.SEV1 {
			t.Fatalf("phase %v: FOREIGN_ORDER severity %v, want SEV1", phase, sev)
		}
		if len(eff.ForeignOrders) != 1 {
			t.Fatalf("phase %v: foreign orders %v", phase, eff.ForeignOrders)
		}
		// Still not cancelled -- stopping our own additions is an action on our
		// risk; cancelling their order is an action on theirs.
		if len(eff.Exclude) != 0 {
			t.Fatalf("phase %v: a live foreign order was handled as a startup "+
				"exclusion (%v) instead of a global stop", phase, eff.Exclude)
		}
	}

	// A foreign FILL is global in both phases, including startup.
	fills := []rest.Fill{makerFill("t1", "not-ours", "M")}
	for _, phase := range []Phase{PhaseStartup, PhaseLive} {
		eff, err := guard.Classify(phase, nil, fills, 99)
		if err != nil {
			t.Fatalf("phase %v: classify: %v", phase, err)
		}
		if !eff.Stop() {
			t.Fatalf("phase %v: a foreign fill did not request a global stop",
				phase)
		}
		if sev, _ := sevOf(eff.Anomalies, "FOREIGN_FILL"); sev != risk.SEV1 {
			t.Fatalf("phase %v: FOREIGN_FILL severity %v, want SEV1", phase, sev)
		}
		if len(eff.OwnedFills) != 0 {
			t.Fatalf("phase %v: a foreign fill entered the owned set", phase)
		}
		if len(eff.ForeignFills) != 1 {
			t.Fatalf("phase %v: foreign fills %v", phase, eff.ForeignFills)
		}
	}

	// A fill in the ledger is ours and moves nothing global by itself.
	own, err := guard.Classify(PhaseLive, nil,
		[]rest.Fill{makerFill("t2", "mine", "M")}, 99)
	if err != nil {
		t.Fatalf("classify: %v", err)
	}
	if own.Stop() {
		t.Fatalf("an owned maker fill requested a global stop: %v",
			classesOf(own.Anomalies))
	}
	if len(own.OwnedFills) != 1 {
		t.Fatalf("owned fills %v, want one", own.OwnedFills)
	}
}

// TestForeignFillAtStartupLatchesBeforeReconciliationCanExposeRunning is the
// ordering half of the same rule.
//
// A stop discovered DURING startup must be latched before the reconciliation
// that would otherwise hand NextGlobal a STARTING input with Reconciled true and
// get RUNNING back. With the latch set first, A14 fires ahead of every other rule
// and the answer is WINDING_DOWN/GTLatch -- so the market is never quotable, not
// even for one tick.
func TestForeignFillAtStartupLatchesBeforeReconciliationCanExposeRunning(t *testing.T) {
	store := &recordingLatch{}
	src := okSource()
	src.fills = rest.FillsResult{Walk: completeWalk(),
		Fills: []rest.Fill{makerFill("t1", "not-ours", "M")}}

	ctrl, _, err := NewGlobalController(store)
	if err != nil {
		t.Fatalf("NewGlobalController: %v", err)
	}
	guard, err := NewForeignGuard(ownsAll())
	if err != nil {
		t.Fatalf("NewForeignGuard: %v", err)
	}
	s, err := NewStartup(ctrl, src, guard, keepAll(), newSweeper(true),
		&stubResolver{}, testParams(), nil)
	if err != nil {
		t.Fatalf("NewStartup: %v", err)
	}

	at := s.Step(context.Background(), startupNow)
	if at.Err != nil {
		t.Fatalf("Run: %v", at.Err)
	}
	causes := at.Adoption.Causes()
	if len(causes) == 0 {
		t.Fatal("a foreign fill in the startup history produced no durable stop cause")
	}

	// The reconciliation completed, so without the latch this input produces
	// RUNNING.
	in := quote.GlobalInput{
		State: quote.Starting, TruthReadable: true, Reconciled: true,
	}
	if st, _ := quote.NextGlobal(in); st != quote.Running {
		t.Fatalf("the control input produces %v without a latch; this test "+
			"would prove nothing", st)
	}

	if c := ctrl.CommitStop(causes[0]); !c.Durable {
		t.Fatalf("the startup stop was not made durable: %v", classesOf(c.Anomalies))
	}
	dec := ctrl.Advance(in)
	if !dec.Committed {
		t.Fatalf("the startup stop was not committed: %v", classesOf(dec.Anomalies))
	}
	if dec.State != quote.WindingDown || dec.Trigger != quote.GTLatch {
		t.Fatalf("a foreign fill found during STARTING produced %v/%v; the "+
			"market would be quotable until the next tick noticed",
			dec.State, dec.Trigger)
	}
	if len(store.ensures) == 0 || store.ensures[0].Trigger != "foreign_fill" {
		t.Fatalf("the latch was not written with the foreign-fill trigger: %v",
			store.ensures)
	}
}

// TestOwnershipLookupIsRequiredEvenWithNoFills is H-ORD-9's "never by
// heuristic".
//
// An empty fills walk is the state every fixture and every first run is in, so a
// nil-tolerant constructor would be exercised constantly and would fail only on
// the day it mattered.
func TestOwnershipLookupIsRequiredEvenWithNoFills(t *testing.T) {
	if _, err := NewForeignGuard(nil); err == nil {
		t.Fatal("NewForeignGuard accepted a nil ownership lookup: the " +
			"heuristic it degrades to is \"no fills are foreign\", which is the " +
			"hole H-ORD-9 exists to close")
	}
}
