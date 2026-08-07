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

// ---------------------------------------------------------------------------
// lip-eyq §4 -- LIFECYCLE AUTHORITY AHEAD OF POLICY
// ---------------------------------------------------------------------------
//
// The rule these tests pin is a division of labour, not a validation rule.
// `AdoptionPolicy` is `lip-3af`'s, and it answers "is this order valid under
// current selection, close, size, price and capital rules". It does NOT answer
// "may this process rest an adding order at all" -- that is a fact about the
// lifecycle (a halt latch on disk, a failed latch write, a market F15 excluded)
// which the policy cannot be assumed to know and has no way to discover.
//
// So `AdoptionKeep` is a RECOMMENDATION. When adding authority has been revoked
// the lifecycle overrides it into a verified cancellation. Getting this backwards
// is the failure H-HALT-4 exists to prevent -- a latched process that resumes
// adding because the injected policy said the order looked fine -- and it is
// invisible to every test that only exercises the policy.
//
// The mirror rule is that a REDUCER is untouched. F15 bars NEW quoting in an
// excluded market; H-SEL-11 keeps a market we hold under management in REDUCING.
// Cancelling the exit because somebody else left an unrelated order in the same
// market converts an exclusion into an abandoned position.

// authoritySideOrder is `ourOrder` with an explicit side.
//
// The side is what decides adding from reducing once q != 0, and `ourOrder`
// leaves it at the zero value (`quote.SideYes`), which is the ADDING side at
// q > 0. Naming it here means these tests state which role each order plays
// rather than inheriting one from a zero value.
func authoritySideOrder(id, ticker string, side quote.Side) rest.Order {
	o := ourOrder(id, ticker)
	o.Side = side
	return o
}

// authorityIDs lists order ids, so an assertion can say which orders survived
// rather than how many.
func authorityIDs(orders []rest.Order) []string {
	out := make([]string, 0, len(orders))
	for _, o := range orders {
		out = append(out, o.OrderID)
	}
	return out
}

// authorityCount counts anomalies of one class. Which alarm fired matters, and
// so does how often: one revocation for two revoked orders would mean the
// operator is told about half of what was cancelled.
func authorityCount(anoms []risk.Anomaly, class string) int {
	n := 0
	for _, a := range anoms {
		if a.Class == class {
			n++
		}
	}
	return n
}

// authorityFactsRecorder keeps every `AdoptionFacts` the policy was actually
// handed, WITHOUT copying them.
//
// Not copying is the point. `AdoptionFacts.clone()` is what stands between
// `lip-3af`'s policy and the structures this package is in the middle of
// building, and a recorder that deep-copied on the way in would prove the
// defensive copy exists by making one itself.
type authorityFactsRecorder struct {
	decision AdoptionDecision
	facts    []AdoptionFacts
	orders   []rest.Order
}

func (p *authorityFactsRecorder) DecideAdopted(ctx context.Context, o rest.Order,
	f AdoptionFacts) (AdoptionDecision, error) {

	p.orders = append(p.orders, o)
	p.facts = append(p.facts, f)
	return p.decision, nil
}

// TestRevokedAddingOrderIsCancelledWhileTheReducerInTheSameExcludedMarketSurvives
// is lip-eyq §4's central case, and it is the one where the two halves of the
// rule point in opposite directions inside a single market.
//
// A foreign order rests in CONTESTED, so F15 excludes the market from new
// selection. We hold q = +5 there, so §6.2 designates `no` as the reducing side
// and `yes` as the adding side. Two of our orders are resting, one on each side,
// and the injected policy keeps everything.
//
// The adding one must go: keeping it would resume adding in a market an
// unexplained order has already made untrustworthy, which is what the exclusion
// was for. The reducing one must stay: H-SEL-11 beats F15 on the other half, and
// exclusion "applies to new selection, not to abandoning inventory" -- so the
// market is excluded AND managed AND reducing, all three at once. Cancelling the
// reducer would leave the position with no exit while the harness reported it as
// handled.
//
// `M-L-STOPKEEP` preserves the revoked adding order: the policy's `AdoptionKeep`
// is honoured and the adder rests on. Under it the adder appears in `Kept()`,
// nothing is swept, and no `ADOPTION_KEEP_REVOKED` is raised, so the operator is
// not told either.
func TestRevokedAddingOrderIsCancelledWhileTheReducerInTheSameExcludedMarketSurvives(t *testing.T) {
	q := num.QtyFromFloat(5)

	// Establish the premise from §6.2 rather than assuming it. If `yes` were not
	// the adding side at q > 0 this test would be asserting the opposite rule
	// while still passing, and the two orders below would have swapped roles.
	if a, ok := quote.AddingSide(q); !ok || a != quote.SideYes {
		t.Fatalf("AddingSide(+5) = %v/%v, want yes/true: this test labels its "+
			"two orders from §6.2's R and A, and would be vacuous otherwise",
			a, ok)
	}

	src := okSource()
	src.positions = rest.PositionsResult{
		Walk:     completeWalk(),
		ByTicker: map[string]num.Qty{"CONTESTED": q},
	}
	src.orders = rest.OrdersResult{Walk: completeWalk(), Orders: []rest.Order{
		authoritySideOrder("adder", "CONTESTED", quote.SideYes),
		authoritySideOrder("reducer", "CONTESTED", quote.SideNo),
		// The foreign order is what excludes the market (F15). It is never
		// cancelled -- cancelling an order we did not place is a destructive act
		// on incomplete information (§11).
		foreignOrder("theirs", "CONTESTED"),
	}}

	sweep := newSweeperOn(src, true)
	s := newStartup(t, &recordingLatch{}, src, ownsAll(), keepAll(), sweep,
		"CONTESTED")
	at := s.Step(context.Background(), startupNow)
	if at.Err != nil {
		t.Fatalf("Step: %v", at.Err)
	}
	if at.Adoption == nil {
		t.Fatal("no adoption: the reads all completed and every cancel swept " +
			"clean, so this pass licenses leaving STARTING")
	}

	if got := authorityIDs(at.Adoption.Kept()); !reflect.DeepEqual(got,
		[]string{"reducer"}) {

		t.Fatalf("kept %v, want [reducer]: the adding order is cancelled because "+
			"adding authority is revoked in an excluded market, and the reducer "+
			"stays because H-SEL-11 keeps a market we hold under management -- "+
			"exclusion bars new quoting, it does not abandon a position",
			describe(got))
	}
	if got := authorityIDs(sweep.swept()); !reflect.DeepEqual(got,
		[]string{"adder"}) {

		t.Fatalf("swept %v, want [adder]: H-FAIL-3 makes a cancel-requested "+
			"order live and fillable until the exchange confirms it absent, so a "+
			"revoked order must be cancelled AND verified, and the reducer must "+
			"not be swept at all", describe(got))
	}

	if n := authorityCount(at.Anomalies, "ADOPTION_KEEP_REVOKED"); n != 1 {
		t.Fatalf("%d ADOPTION_KEEP_REVOKED anomalies among %v, want exactly 1: "+
			"the lifecycle overriding the injected policy is not a routine "+
			"cancellation and the operator is told which order it was",
			n, classesOf(at.Anomalies))
	}
	if sev, ok := sevOf(at.Anomalies, "ADOPTION_KEEP_REVOKED"); !ok ||
		sev != risk.SEV2 {

		t.Fatalf("ADOPTION_KEEP_REVOKED severity %v (present=%v), want SEV2",
			sev, ok)
	}
	// The Adoption carries it too. `Attempt.Anomalies` is this Step's; the
	// Adoption is what the summary and the operator ping are built from, and a
	// revocation visible in only one of them is a revocation half reported.
	if !hasClass(at.Adoption.Anomalies(), "ADOPTION_KEEP_REVOKED") {
		t.Fatalf("the adoption's anomalies %v carry no ADOPTION_KEEP_REVOKED",
			classesOf(at.Adoption.Anomalies()))
	}

	// Excluded AND managed AND reducing -- HR-024's collision, resolved in
	// H-SEL-11's favour on the management question and F15's on the selection
	// question.
	if got := at.Adoption.Excluded(); !reflect.DeepEqual(got,
		[]string{"CONTESTED"}) {

		t.Fatalf("excluded %v, want [CONTESTED]: an account with unexplained "+
			"orders on it is an account whose position model we cannot trust",
			describe(got))
	}
	if got := at.Adoption.Managed(); !reflect.DeepEqual(got,
		[]string{"CONTESTED"}) {

		t.Fatalf("managed %v, want [CONTESTED]: we hold q = +5 there, and a "+
			"market we hold is never abandoned for being excluded", describe(got))
	}
	if st := at.Adoption.States()["CONTESTED"]; st != quote.Reducing {
		t.Fatalf("state %v, want REDUCING: §7.5 step 6 enters any market with "+
			"q != 0 at REDUCING regardless of size, because adopted inventory is "+
			"inventory whose provenance we do not know", st)
	}

	if n := at.Adoption.Summary().CancelledStale; n != 1 {
		t.Fatalf("summary reports %d cancelled, want 1: the §7.5 step 7 ping "+
			"reports what this STARTUP cancelled across every pass, and the "+
			"final pass always cancels nothing", n)
	}
}

// TestAtFlatPositionEveryRestingOrderInAnExcludedMarketIsAddingAndNoneSurvives
// is the q == 0 half of §6.2's adding/reducing split, which has no reducing side
// at all.
//
// `quote.AddingSide(0)` returns `(SideYes, false)`: the false is the caller's
// signal that there is no opposite to name, because a flat market has no exit to
// keep alive. `orderIsAdding` reads that as BOTH sides adding, which is the only
// safe reading -- an order on a flat market cannot be reducing anything, so
// calling one of the two sides a reducer would exempt a genuinely risk-adding
// order from every revocation in this file.
//
// Defaulting the other way is the subtle version: treating the `false` as "yes
// is the reducing side" would silently preserve exactly half of the orders in
// every flat excluded market.
//
// `M-L-STOPKEEP` preserves the revoked adding orders, and here that is all of
// them: an excluded market keeps two live quotes and the harness carries on
// adding in the one market it has already decided it cannot trust.
func TestAtFlatPositionEveryRestingOrderInAnExcludedMarketIsAddingAndNoneSurvives(t *testing.T) {
	// The premise, stated: at q == 0 there is no reducing side.
	if _, ok := quote.AddingSide(0); ok {
		t.Fatal("AddingSide(0) named a side: §6.2 has no reducing side at q == " +
			"0, and this test exists because that is the case a caller defaults " +
			"through")
	}

	src := okSource() // positions are empty, so q == 0 everywhere
	src.orders = rest.OrdersResult{Walk: completeWalk(), Orders: []rest.Order{
		authoritySideOrder("mine-yes", "FLAT", quote.SideYes),
		authoritySideOrder("mine-no", "FLAT", quote.SideNo),
		foreignOrder("theirs", "FLAT"),
	}}

	sweep := newSweeperOn(src, true)
	s := newStartup(t, &recordingLatch{}, src, ownsAll(), keepAll(), sweep,
		"FLAT")
	at := s.Step(context.Background(), startupNow)
	if at.Err != nil {
		t.Fatalf("Step: %v", at.Err)
	}
	if at.Adoption == nil {
		t.Fatal("no adoption")
	}

	if got := at.Adoption.Kept(); len(got) != 0 {
		t.Fatalf("kept %v, want nothing: with no position there is no reducing "+
			"side, so both of our orders are adding and adding authority is "+
			"revoked in an excluded market", describe(authorityIDs(got)))
	}
	if got := authorityIDs(sweep.swept()); !reflect.DeepEqual(got,
		[]string{"mine-no", "mine-yes"}) && !reflect.DeepEqual(got,
		[]string{"mine-yes", "mine-no"}) {

		t.Fatalf("swept %v, want both of our orders: a revoked order is "+
			"cancelled and verified, never merely dropped from the model, "+
			"because H-FAIL-3 leaves an unverified cancel live and fillable",
			describe(got))
	}
	if n := authorityCount(at.Anomalies, "ADOPTION_KEEP_REVOKED"); n != 2 {
		t.Fatalf("%d ADOPTION_KEEP_REVOKED anomalies among %v, want 2 -- one "+
			"per revoked order, because an operator told about one of two "+
			"cancellations is being told the market was half handled",
			n, classesOf(at.Anomalies))
	}

	if got := at.Adoption.Excluded(); !reflect.DeepEqual(got, []string{"FLAT"}) {
		t.Fatalf("excluded %v, want [FLAT]", describe(got))
	}
	// Nothing held and nothing kept, so H-SEL-11 has nothing to save: the
	// market drops out of management entirely. This is the OTHER side of the
	// first test's collision, and it is why exclusion is not simply ignored.
	if got := at.Adoption.Managed(); len(got) != 0 {
		t.Fatalf("managed %v, want nothing: FLAT is excluded from selection, "+
			"holds no inventory and has no kept order of ours, so H-ORD-5b's "+
			"union is empty and there is nothing to subscribe to", describe(got))
	}
	if st, ok := at.Adoption.States()["FLAT"]; ok {
		t.Fatalf("FLAT has state %v, but it is not managed; a state for an "+
			"unmanaged market is a market the evaluator will size", st)
	}
	if n := at.Adoption.Summary().CancelledStale; n != 2 {
		t.Fatalf("summary reports %d cancelled, want 2", n)
	}
}

// TestAdoptionPolicyReceivesEffectiveSelectionAndExclusionsAsDefensiveCopies is
// §4's contract with `lip-3af`, from the policy's side of the boundary.
//
// Three separate claims, and each one fails differently:
//
//  1. `Selected` is the EFFECTIVE selection -- the operator's set MINUS what
//     foreign activity excluded. A policy handed the raw set is being asked to
//     validate an order against markets startup has already decided are not ours
//     to newly quote, and the policy cannot be assumed to subtract `Excluded`
//     itself because it is another unit's code.
//  2. `Excluded` is supplied ALONGSIDE the filtered selection, because the two
//     answer different questions: a market can be excluded and still hold
//     inventory that must be reduced (H-SEL-11), and a policy sizing a reducer
//     needs to know which case it is in. Filtering `Selected` without naming what
//     was removed would destroy that information.
//  3. Both slices are DEFENSIVE COPIES, per call. `Excluded` aliases the guard's
//     own exclusion list and that same list is what the Adoption reports, so
//     without the copy a policy -- or anything holding what a policy was handed
//     -- can rewrite the exclusion set of the startup that produced it.
//
// `M-L-EXCLUDESELECT` passes the raw selection set, so claim 1 fails: the policy
// is told SHUNNED is selectable while the harness has already excluded it.
func TestAdoptionPolicyReceivesEffectiveSelectionAndExclusionsAsDefensiveCopies(t *testing.T) {
	src := okSource()
	src.orders = rest.OrdersResult{Walk: completeWalk(), Orders: []rest.Order{
		ourOrder("o1", "OPEN"),
		ourOrder("o2", "OPEN"),
		foreignOrder("theirs", "SHUNNED"),
	}}

	rec := &authorityFactsRecorder{decision: AdoptionKeep}
	// The operator selected both markets. Foreign activity in SHUNNED is what
	// takes it out of the effective set; nothing in the operator's configuration
	// mentions it.
	s := newStartup(t, &recordingLatch{}, src, ownsAll(), rec, newSweeper(true),
		"OPEN", "SHUNNED")
	at := s.Step(context.Background(), startupNow)
	if at.Err != nil {
		t.Fatalf("Step: %v", at.Err)
	}
	if at.Adoption == nil {
		t.Fatal("no adoption")
	}

	if len(rec.facts) != 2 {
		t.Fatalf("the policy was consulted %d time(s) for 2 owned resting "+
			"orders; H-ORD-5c has no keep-by-default path, so every adopted "+
			"order is decided", len(rec.facts))
	}
	for i, f := range rec.facts {
		if !reflect.DeepEqual(f.Selected, []string{"OPEN"}) {
			t.Fatalf("call %d saw Selected %v, want [OPEN]: SHUNNED is barred "+
				"from new selection by F15, and a policy handed the raw set "+
				"would validate this order against a market startup has already "+
				"decided is not ours to newly quote", i, describe(f.Selected))
		}
		if !reflect.DeepEqual(f.Excluded, []string{"SHUNNED"}) {
			t.Fatalf("call %d saw Excluded %v, want [SHUNNED]: the filtered "+
				"selection alone cannot tell a policy whether it is sizing a "+
				"reducer in an excluded market (H-SEL-11) or looking at a market "+
				"nobody selected", i, describe(f.Excluded))
		}
	}

	// Now the defensive copy, from both directions. Writing through what the
	// FIRST call was handed must not be visible to the second call, and must not
	// be visible in the Adoption -- whose `Excluded()` reads the guard's list
	// that `AdoptionFacts.Excluded` was built from.
	rec.facts[0].Selected[0] = "FORGED"
	rec.facts[0].Excluded[0] = "FORGED"

	if got := rec.facts[1].Selected; !reflect.DeepEqual(got, []string{"OPEN"}) {
		t.Fatalf("after the first call's facts were rewritten the second call's "+
			"Selected reads %v, want [OPEN]: the facts are cloned per call, so "+
			"one order's decision cannot rewrite the next one's premises",
			describe(got))
	}
	if got := rec.facts[1].Excluded; !reflect.DeepEqual(got,
		[]string{"SHUNNED"}) {

		t.Fatalf("after the first call's facts were rewritten the second call's "+
			"Excluded reads %v, want [SHUNNED]", describe(got))
	}
	if got := at.Adoption.Excluded(); !reflect.DeepEqual(got,
		[]string{"SHUNNED"}) {

		t.Fatalf("the adoption's excluded set reads %v after a policy's facts "+
			"were rewritten, want [SHUNNED]: the policy is another unit's code "+
			"and this package cannot know what it does with what it is handed",
			describe(got))
	}
	if got := at.Adoption.Managed(); !reflect.DeepEqual(got, []string{"OPEN"}) {
		t.Fatalf("managed %v, want [OPEN]", describe(got))
	}
}

// TestLatchedControllerRevokesAdoptionKeepForAnAddingOrderWithNoExclusionAnywhere
// separates the two independent revocations of §4.
//
// The previous tests revoke through EXCLUSION. This one revokes through the
// LATCH, on an account with no foreign activity at all and a market the operator
// selected: nothing here is excluded, and the policy keeps everything. The only
// reason the adding order may not stay is that `harness.halt` was already on disk
// when this process booted.
//
// That is the HR-009 sequence with a restart in the middle. A taker fill latches
// WINDING_DOWN, a panic kills the process, `launchd KeepAlive` restarts it, and
// startup adopts the orders the previous incarnation left resting. H-HALT-4 says
// the halt does not self-clear -- but if adoption honours the policy's `keep`,
// the harness comes back with live adding quotes and the latch has been reduced
// to a state label. `AddingPermitted` is false for a loaded latch precisely so
// that this cannot happen, and its zero value is false so that any uncertainty
// added later defaults the same way.
//
// The reducer still stays. WINDING_DOWN exists to keep a reducing quote alive;
// a halt that cancelled the exit would be a stop condition that increases risk.
//
// `M-L-STOPKEEP` preserves the revoked adding order, which is this exact
// self-clearing halt: a latched harness resting adding quotes.
func TestLatchedControllerRevokesAdoptionKeepForAnAddingOrderWithNoExclusionAnywhere(t *testing.T) {
	// A latch already on disk when this process starts, exactly as a restart
	// after a committed stop finds it.
	store := &recordingLatch{
		present: true,
		rec:     LatchRecord{Version: 1, Trigger: "taker_fill", TsMillis: 1},
	}

	q := num.QtyFromFloat(3)
	src := okSource()
	src.positions = rest.PositionsResult{
		Walk:     completeWalk(),
		ByTicker: map[string]num.Qty{"HELD": q},
	}
	// No foreign order anywhere: nothing on this account is excluded, so the
	// only revocation available is the latch's.
	src.orders = rest.OrdersResult{Walk: completeWalk(), Orders: []rest.Order{
		authoritySideOrder("adder", "HELD", quote.SideYes),
		authoritySideOrder("reducer", "HELD", quote.SideNo),
	}}

	sweep := newSweeperOn(src, true)
	s := newStartup(t, store, src, ownsAll(), keepAll(), sweep, "HELD")
	at := s.Step(context.Background(), startupNow)
	if at.Err != nil {
		t.Fatalf("Step: %v", at.Err)
	}
	if at.Adoption == nil {
		t.Fatal("no adoption: a latched process still reconciles, and a halt " +
			"that refused to read the account would be halting next to " +
			"inventory it declined to look at")
	}

	// Non-vacuity: the controller really did boot latched, and §5.1 really did
	// produce WINDING_DOWN from it. Without this the test could pass against a
	// process that was never latched at all.
	if at.Decision.State != quote.WindingDown ||
		at.Decision.Trigger != quote.GTLatch {

		t.Fatalf("decision %v/%v, want WINDING_DOWN/GTLatch: this test's whole "+
			"premise is a process that booted with a latch on disk",
			at.Decision.State, at.Decision.Trigger)
	}
	if got := at.Adoption.Excluded(); len(got) != 0 {
		t.Fatalf("excluded %v, want nothing: this revocation must come from the "+
			"latch alone, or the test proves nothing the exclusion tests did not",
			describe(got))
	}

	if got := authorityIDs(at.Adoption.Kept()); !reflect.DeepEqual(got,
		[]string{"reducer"}) {

		t.Fatalf("kept %v, want [reducer]: H-HALT-4's latch does not self-clear, "+
			"so a restarted process may not resume adding on the strength of an "+
			"injected policy that cannot see the halt -- while WINDING_DOWN "+
			"exists to keep the reducing quote alive", describe(got))
	}
	if got := authorityIDs(sweep.swept()); !reflect.DeepEqual(got,
		[]string{"adder"}) {

		t.Fatalf("swept %v, want [adder]", describe(got))
	}
	if n := authorityCount(at.Anomalies, "ADOPTION_KEEP_REVOKED"); n != 1 {
		t.Fatalf("%d ADOPTION_KEEP_REVOKED anomalies among %v, want 1",
			n, classesOf(at.Anomalies))
	}
	if sev, ok := sevOf(at.Anomalies, "ADOPTION_KEEP_REVOKED"); !ok ||
		sev != risk.SEV2 {

		t.Fatalf("ADOPTION_KEEP_REVOKED severity %v (present=%v), want SEV2",
			sev, ok)
	}

	if got := at.Adoption.Managed(); !reflect.DeepEqual(got, []string{"HELD"}) {
		t.Fatalf("managed %v, want [HELD]: a halted harness still manages the "+
			"market it holds, or the position it is winding down is unobserved",
			describe(got))
	}
	if st := at.Adoption.States()["HELD"]; st != quote.Reducing {
		t.Fatalf("state %v, want REDUCING", st)
	}
}

// TestManagedIsBuiltFromTheEffectiveSelectionSoAnExcludedEmptyTickerIsUnmanaged
// is §4's last clause: `Managed` is the union of the EFFECTIVE selection, the
// positions and the FINAL KEPT orders.
//
// Each of the three terms is doing work, and this test isolates the first. Two
// markets are selected; a foreign order rests in one of them, which excludes it
// (F15). That excluded market holds nothing and has no order of ours, so neither
// of the other two terms can put it back -- and it must drop out of management
// entirely.
//
// This is where exclusion actually bites. `TestRevokedAdding...` shows an
// excluded market staying managed because H-SEL-11's inventory clause rescues
// it; if `Managed` were built from the RAW selection, that test would still pass
// and exclusion would have no observable effect on management anywhere. Managing
// a market we have decided not to quote means subscribing to it, evaluating it
// and reporting it, and the whole point of F15 is that we do not.
//
// `M-L-EXCLUDESELECT` uses the raw selection set, so SHUNNED stays managed --
// the harness reports it as under management while refusing to quote it, and the
// exclusion becomes a label on an otherwise unchanged market.
func TestManagedIsBuiltFromTheEffectiveSelectionSoAnExcludedEmptyTickerIsUnmanaged(t *testing.T) {
	src := okSource() // nothing held anywhere
	src.orders = rest.OrdersResult{Walk: completeWalk(), Orders: []rest.Order{
		// The only order on the account is somebody else's, in SHUNNED. We keep
		// nothing, so the kept-orders term of H-ORD-5b's union is empty too.
		foreignOrder("theirs", "SHUNNED"),
	}}

	s := newStartup(t, &recordingLatch{}, src, ownsAll(), keepAll(),
		newSweeper(true), "KEPT", "SHUNNED")
	at := s.Step(context.Background(), startupNow)
	if at.Err != nil {
		t.Fatalf("Step: %v", at.Err)
	}
	if at.Adoption == nil {
		t.Fatal("no adoption")
	}

	if got := at.Adoption.Excluded(); !reflect.DeepEqual(got,
		[]string{"SHUNNED"}) {

		t.Fatalf("excluded %v, want [SHUNNED]: the test's premise is that "+
			"foreign activity excluded a market the operator selected",
			describe(got))
	}
	if got := at.Adoption.Managed(); !reflect.DeepEqual(got, []string{"KEPT"}) {
		t.Fatalf("managed %v, want [KEPT]: SHUNNED is selected but excluded, "+
			"holds no inventory and has no kept order of ours, so nothing in "+
			"H-ORD-5b's union reaches it -- managing it would subscribe to and "+
			"evaluate a market F15 has barred from new selection", describe(got))
	}
	if _, ok := at.Adoption.States()["SHUNNED"]; ok {
		t.Fatalf("SHUNNED has an initial market state despite being unmanaged; " +
			"states are built from the managed set and an entry here is a " +
			"market the evaluator will size")
	}
	if st := at.Adoption.States()["KEPT"]; st != quote.Idle {
		t.Fatalf("state of KEPT is %v, want IDLE: it is selected, effective and "+
			"flat", st)
	}

	// The §7.5 step 7 ping reports the same union, not a differently derived
	// one. A summary that named SHUNNED as managed would tell the operator the
	// opposite of what the harness is doing.
	sum := at.Adoption.Summary()
	if !reflect.DeepEqual(sum.Managed, []string{"KEPT"}) {
		t.Fatalf("summary managed %v, want [KEPT]", describe(sum.Managed))
	}
	if !reflect.DeepEqual(sum.Excluded, []string{"SHUNNED"}) {
		t.Fatalf("summary excluded %v, want [SHUNNED]", describe(sum.Excluded))
	}
	if len(sum.Reducing) != 0 {
		t.Fatalf("summary reducing %v, want nothing held", describe(sum.Reducing))
	}
}
