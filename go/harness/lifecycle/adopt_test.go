package lifecycle

import (
	"context"
	"errors"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"lip/harness/num"
	"lip/harness/quote"
	"lip/harness/rest"
	"lip/harness/risk"
)

var startupNow = time.Unix(1_700_000_000, 0).UTC()

// TestLatchReadPrecedesEveryPortfolioRequest is H-HALT-4's "STARTING reads the
// latch before any placement", strengthened to "before any REST call".
//
// The strengthening is structural rather than a comment to obey: `NewStartup`
// takes a *GlobalController, and the only way to get one is `NewGlobalController`,
// which performs the read itself. So there is no constructible Startup that has
// not already consulted the latch.
//
// `M-L-BOOTORDER` performs the first REST read before the latch load. This test
// catches it by asserting the store's load count is already non-zero at the
// moment zero endpoints have been called.
func TestLatchReadPrecedesEveryPortfolioRequest(t *testing.T) {
	store := &recordingLatch{}
	src := okSource()

	s := newStartup(t, store, src, ownsAll(), keepAll(), newSweeper(true))

	if store.loads == 0 {
		t.Fatal("the latch had not been read by the time a Startup existed")
	}
	if len(src.calls) != 0 {
		t.Fatalf("a portfolio request was issued during construction: %v",
			describe(src.sequence()))
	}

	// And a Startup cannot be assembled around a controller that never
	// bootstrapped -- the zero value is refused.
	guard, err := NewForeignGuard(ownsAll())
	if err != nil {
		t.Fatalf("NewForeignGuard: %v", err)
	}
	if _, err := NewStartup(&GlobalController{}, src, guard, keepAll(),
		newSweeper(true), &stubResolver{}, testParams(), nil); err == nil {
		t.Fatal("NewStartup accepted a controller that had not read the latch")
	}
	if _, err := NewStartup(nil, src, guard, keepAll(), newSweeper(true),
		&stubResolver{}, testParams(), nil); err == nil {
		t.Fatal("NewStartup accepted a nil controller")
	}

	// Now run, and confirm the first thing that touches the network is step 1.
	at := s.Step(context.Background(), startupNow)
	if at.Err != nil {
		t.Fatalf("Run: %v", at.Err)
	}
	if src.calls[0].what != "positions" {
		t.Fatalf("first endpoint call was %q, want positions", src.calls[0].what)
	}
}

// TestStartupReadsPositionsOrdersFillsBalanceInOrder is §7.5 steps 1-4, "in this
// order".
//
// Positions BEFORE fills is the load-bearing pair. The exchange has already told
// us where the position ended up; replaying the historical fills that produced it
// on top of that answer counts every one of them twice. `M-L-ADOPTORDER` swaps
// orders and positions, and `M-L-SEEDLIVE` attacks the same property from the
// other side.
//
// The filters are asserted too: both walks are UNFILTERED. §7.5 step 1 says
// "including markets not in the selection set", and H-ORD-5b makes the managed
// set the union of selected, held and owned-resting -- so a ticker-filtered read
// is structurally incapable of answering the question, and the answer it gives
// instead is "flat" for every market we hold but did not ask about.
func TestStartupReadsPositionsOrdersFillsBalanceInOrder(t *testing.T) {
	src := okSource()
	s := newStartup(t, &recordingLatch{}, src, ownsAll(), keepAll(),
		newSweeper(true), "SELECTED-A")

	at := s.Step(context.Background(), startupNow)
	if at.Err != nil {
		t.Fatalf("Run: %v", at.Err)
	}

	want := []string{"positions", "orders", "fills", "balance"}
	if got := src.sequence(); !reflect.DeepEqual(got, want) {
		t.Fatalf("startup read order %v, want %v", describe(got), describe(want))
	}

	for _, c := range src.calls {
		if c.what == "orders" {
			if c.ticker != "" {
				t.Fatalf("the resting-orders walk was filtered to %q; H-ORD-5b's "+
					"managed set includes markets outside the selection", c.ticker)
			}
			if c.status != rest.StatusResting {
				t.Fatalf("the orders walk asked for status %q, want %q; the "+
					"endpoint answers \"nothing\" to an unrecognised value, which "+
					"reads exactly like \"no orders rest\"",
					c.status, rest.StatusResting)
			}
		}
		if c.what == "fills" {
			if c.ticker != "" {
				t.Fatalf("the fills walk was filtered to %q", c.ticker)
			}
			wantSince := startupNow.Add(-testParams().Backfill)
			if !c.since.Equal(wantSince) {
				t.Fatalf("fills walked back to %v, want %v (backfill_h)",
					c.since, wantSince)
			}
		}
	}
}

// TestStartupNeverLicensesBeforeFourReadsPolicyAndCleanSweeps is H-ORD-5:
// "Reconcile before quoting. Always. No exceptions."
//
// An Adoption is the licence to leave STARTING, so every way the procedure can
// fail must produce NO adoption rather than a partial one. `M11` marks startup
// complete after positions and before the remaining reads, the policy and the
// sweep; under it a truncated fills walk, a failed balance read or an unclean
// sweep all still license quoting.
func TestStartupNeverLicensesBeforeFourReadsPolicyAndCleanSweeps(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*fakeSource)
		policy  *fixedPolicy
		sweeper *recordingSweeper
	}{
		{
			name: "positions walk incomplete",
			mutate: func(s *fakeSource) {
				s.positions = rest.PositionsResult{Walk: failedWalk("truncated")}
			},
		},
		{
			name: "orders walk incomplete",
			mutate: func(s *fakeSource) {
				s.orders = rest.OrdersResult{Walk: failedWalk("truncated")}
			},
		},
		{
			name: "fills walk incomplete",
			mutate: func(s *fakeSource) {
				s.fills = rest.FillsResult{Walk: failedWalk("truncated")}
			},
		},
		{
			name:   "balance read failed",
			mutate: func(s *fakeSource) { s.balErr = errors.New("503") },
		},
		{
			name: "fill with no fee_cost",
			mutate: func(s *fakeSource) {
				f := makerFill("t1", "o1", "M")
				f.FeeCost = ""
				s.fills = rest.FillsResult{Walk: completeWalk(),
					Fills: []rest.Fill{f}}
			},
		},
		{
			name: "policy errored",
			mutate: func(s *fakeSource) {
				s.orders = rest.OrdersResult{Walk: completeWalk(),
					Orders: []rest.Order{ourOrder("o1", "M")}}
			},
			policy: &fixedPolicy{err: errors.New("selection unavailable")},
		},
		{
			name: "policy returned the zero decision",
			mutate: func(s *fakeSource) {
				s.orders = rest.OrdersResult{Walk: completeWalk(),
					Orders: []rest.Order{ourOrder("o1", "M")}}
			},
			policy: &fixedPolicy{decision: AdoptionUnset},
		},
		{
			name: "sweep came back unclean",
			mutate: func(s *fakeSource) {
				s.orders = rest.OrdersResult{Walk: completeWalk(),
					Orders: []rest.Order{ourOrder("o1", "M")}}
			},
			policy:  &fixedPolicy{decision: AdoptionCancel},
			sweeper: newSweeper(false),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			src := okSource()
			tc.mutate(src)
			policy := tc.policy
			if policy == nil {
				policy = keepAll()
			}
			sweeper := tc.sweeper
			if sweeper == nil {
				sweeper = newSweeper(true)
			}
			s := newStartup(t, &recordingLatch{}, src, ownsAll("o1"), policy,
				sweeper)

			at := s.Step(context.Background(), startupNow)
			if at.Err == nil {
				t.Fatal("the attempt reported success")
			}
			if at.Adoption != nil {
				t.Fatal("a failed attempt returned an Adoption: that object IS " +
					"the licence to leave STARTING, and a partial one licenses " +
					"quoting on partial evidence")
			}
			if !at.Retry {
				t.Fatal("a failed attempt did not ask to be retried; §7.5 " +
					"retries indefinitely with backoff and has no terminal " +
					"outcome")
			}
			if at.Decision.State != quote.Starting {
				t.Fatalf("the first failure moved the global state to %v, want "+
					"STARTING", at.Decision.State)
			}
		})
	}
}

// TestUnknownRiskBeginsOnThirdFailureAndRetriesForever is §7.5's retry rule and
// H-ORD-5a's choice of state.
//
// > If any of steps 1-4 fails after `startup_retries` (3), the harness enters
// > **`UNKNOWN_RISK`**, pings `SEV1`, and **retries indefinitely with backoff**.
// > It does **not** proceed to quote against an unknown position, and it does
// > **not** exit.
//
// UNKNOWN_RISK and not WINDING_DOWN, per H-ORD-5a: winding down exists to keep a
// REDUCING quote alive, and sizing a reducer requires knowing q -- which is
// exactly what just failed to read.
//
// `M-L-RETRYTHRESH` moves the threshold to 1 and `M-L-RETRYEXIT` stops retrying
// at the threshold. The second is the more dangerous of the two: a bounded retry
// that eventually gives up is a process sitting next to inventory it decided not
// to look at again.
func TestUnknownRiskBeginsOnThirdFailureAndRetriesForever(t *testing.T) {
	src := okSource()
	src.positions = rest.PositionsResult{Walk: failedWalk("503")}
	s := newStartup(t, &recordingLatch{}, src, ownsAll(), keepAll(),
		newSweeper(true))

	wantBackoff := []time.Duration{
		1 * time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second,
		16 * time.Second, 32 * time.Second, 60 * time.Second, 60 * time.Second,
		60 * time.Second,
	}

	for i, wantAfter := range wantBackoff {
		at := s.Step(context.Background(), startupNow)
		attempt := i + 1

		if at.Err == nil || at.Adoption != nil {
			t.Fatalf("attempt %d succeeded against a failing positions walk",
				attempt)
		}
		if !at.Retry {
			t.Fatalf("attempt %d did not ask to be retried; §7.5 retries "+
				"indefinitely and there is no exhausted outcome", attempt)
		}
		if at.After != wantAfter {
			t.Fatalf("attempt %d backoff %v, want %v", attempt, at.After, wantAfter)
		}

		// The threshold is written as a LITERAL 3 here, not as
		// `startupFailureThreshold`. Referencing the constant would make this
		// test move with it, and `M-L-RETRYTHRESH` -- which edits the constant --
		// would pass unnoticed. §7.5 says three, so the test says three.
		switch {
		case attempt < 3:
			if at.Decision.State != quote.Starting {
				t.Fatalf("attempt %d moved to %v before the threshold, want "+
					"STARTING", attempt, at.Decision.State)
			}
			if hasClass(at.Anomalies, "STARTUP_RECONCILE_FAILED") {
				t.Fatalf("attempt %d raised STARTUP_RECONCILE_FAILED before the "+
					"third consecutive failure", attempt)
			}
		default:
			if at.Decision.State != quote.UnknownRisk {
				t.Fatalf("attempt %d state %v, want UNKNOWN_RISK -- NOT "+
					"WINDING_DOWN, which would have to size a reducer from the "+
					"q that just failed to read (H-ORD-5a)", attempt, at.Decision.State)
			}
			if !hasClass(at.Anomalies, "STARTUP_RECONCILE_FAILED") {
				t.Fatalf("attempt %d raised no STARTUP_RECONCILE_FAILED; got %v",
					attempt, classesOf(at.Anomalies))
			}
		}
	}

	// A later complete attempt leaves UNKNOWN_RISK, and it leaves it through
	// quote.NextGlobal rather than through anything in this package.
	src.positions = rest.PositionsResult{
		Walk: completeWalk(), ByTicker: map[string]num.Qty{},
	}
	at := s.Step(context.Background(), startupNow)
	if at.Err != nil || at.Adoption == nil {
		t.Fatalf("the recovering attempt failed: %v", at.Err)
	}

	ctrl, _, err := NewGlobalController(&recordingLatch{})
	if err != nil {
		t.Fatalf("NewGlobalController: %v", err)
	}
	dec := ctrl.Advance(quote.GlobalInput{
		State: quote.UnknownRisk, TruthReadable: true, Reconciled: true,
	})
	if dec.State != quote.Running || dec.Trigger != quote.GTReconciled {
		t.Fatalf("recovery produced %v/%v, want RUNNING/GTReconciled",
			dec.State, dec.Trigger)
	}

	// And the counter reset, so the next failure starts the ladder over.
	//
	// The STATE does not go back to STARTING, and that is the coordinator owning
	// it rather than a regression. This assertion used to read `!= quote.Starting`,
	// which was only ever satisfied because `startingInput()` re-asserted
	// STARTING on every call: the caller manufactured the state and the machine
	// dutifully agreed. A process that has completed a reconciliation is RUNNING,
	// and one later failed read does not un-reconcile it -- §5.1 gives RUNNING no
	// edge out but a stop. Regressing to STARTING would re-run adoption against
	// an account we have already adopted.
	//
	// The reset actually under test is the FAILURE COUNTER's, and the backoff
	// ladder below is what witnesses it.
	src.positions = rest.PositionsResult{Walk: failedWalk("503")}
	next := s.Step(context.Background(), startupNow)
	if next.Decision.State != quote.Running {
		t.Fatalf("a failed read after a complete reconciliation produced %v, "+
			"want RUNNING", next.Decision.State)
	}
	if next.After != 1*time.Second {
		t.Fatalf("after a success the backoff ladder did not reset: %v", next.After)
	}
}

// TestStartupSeedsExchangePositionWithoutReplayingHistoricalFills is §7.5's
// reason for reading positions before fills, and `risk.Seed`'s reason for
// existing.
//
// The exchange reports q = 3. Our 24h history contains the three fills that
// produced it. Applying them on top of the seeded position gives 6 -- a position
// error in the direction of believing we hold more than we do, which sizes a
// reducer too large and can invert the sign of q. `M-L-SEEDLIVE` does exactly
// that by passing `risk.Live`.
//
// The identities are still recorded: a taker fill in our history is a fact about
// the harness whenever it happened.
func TestStartupSeedsExchangePositionWithoutReplayingHistoricalFills(t *testing.T) {
	src := okSource()
	src.positions = rest.PositionsResult{
		Walk:     completeWalk(),
		ByTicker: map[string]num.Qty{"HELD": num.QtyFromFloat(3)},
	}
	src.fills = rest.FillsResult{Walk: completeWalk(), Fills: []rest.Fill{
		makerFill("t1", "o1", "HELD"),
		makerFill("t2", "o1", "HELD"),
		makerFill("t3", "o1", "HELD"),
	}}

	s := newStartup(t, &recordingLatch{}, src, ownsAll("o1"), keepAll(),
		newSweeper(true))
	at := s.Step(context.Background(), startupNow)
	if at.Err != nil {
		t.Fatalf("Run: %v", at.Err)
	}

	got := at.Adoption.Portfolio().Q("HELD")
	want := num.QtyFromFloat(3)
	if got != want {
		t.Fatalf("q = %s after adoption, want %s: the exchange had already "+
			"reported the position these fills produced, so replaying them "+
			"counts every one of them twice", got.Wire(), want.Wire())
	}

	if n := len(at.Adoption.OwnedFills()); n != 3 {
		t.Fatalf("owned fills = %d, want 3: the identities and the taker "+
			"classification are recorded even though q does not move", n)
	}

	// The identities were recorded, which is what stops the SECOND count. The
	// fills endpoint is walked in full on every poll, so these same three fills
	// are delivered again on the very next steady-state cycle -- in `risk.Live`
	// mode, where a newly observed fill is news and moves q. Seeding having
	// recorded their trade ids is the only thing between that and q = 6.
	again, err := ConvertFills(src.fills.Fills)
	if err != nil {
		t.Fatalf("ConvertFills: %v", err)
	}
	at.Adoption.Portfolio().ApplyFills(again, ownsAll("o1"), risk.Live, startupNow.UnixMilli())
	if got := at.Adoption.Portfolio().Q("HELD"); got != want {
		t.Fatalf("q = %s after the next live poll redelivered the same three "+
			"fills, want %s: seeding must record their identities, not just "+
			"decline to apply them", got.Wire(), want.Wire())
	}
}

// TestStartupManagedSetIncludesSelectionPositionsAndOwnedOrders is H-ORD-5b.
//
// > Markets under management are the **union** of: every selected market, every
// > market with a non-zero position, and every market with a resting, sending or
// > unknown `lipH-` order, irrespective of LIP program status, selection rank, or
// > `core.Rig` universe membership. The harness subscribes to whatever it holds.
//
// And §7.5 step 6: every market with q != 0 enters at REDUCING, not QUOTING,
// regardless of size -- including the ones outside the selection set, because
// H-SEL-11 beats F15 and a market we hold is never abandoned for not being
// selected.
func TestStartupManagedSetIncludesSelectionPositionsAndOwnedOrders(t *testing.T) {
	src := okSource()
	src.positions = rest.PositionsResult{
		Walk: completeWalk(),
		ByTicker: map[string]num.Qty{
			"HELD-UNSELECTED": num.QtyFromFloat(2),
			"FLAT":            0,
		},
	}
	src.orders = rest.OrdersResult{Walk: completeWalk(), Orders: []rest.Order{
		ourOrder("o1", "RESTING-UNSELECTED"),
	}}

	s := newStartup(t, &recordingLatch{}, src, ownsAll("o1"), keepAll(),
		newSweeper(true), "SELECTED")
	at := s.Step(context.Background(), startupNow)
	if at.Err != nil {
		t.Fatalf("Run: %v", at.Err)
	}

	want := []string{"HELD-UNSELECTED", "RESTING-UNSELECTED", "SELECTED"}
	if got := at.Adoption.Managed(); !reflect.DeepEqual(got, want) {
		t.Fatalf("managed set %v, want %v", describe(got), describe(want))
	}

	states := at.Adoption.States()
	if states["HELD-UNSELECTED"] != quote.Reducing {
		t.Fatalf("a market we hold but did not select entered %v, want "+
			"REDUCING: adopted inventory is inventory whose provenance we do "+
			"not know", states["HELD-UNSELECTED"])
	}
	if states["SELECTED"] == quote.Reducing {
		t.Fatal("a flat selected market entered REDUCING")
	}
	if _, ok := states["FLAT"]; ok {
		t.Fatal("a market the exchange reported flat entered the managed set " +
			"through the position union")
	}
}

// TestInvalidAdoptedOrdersRequireVerifiedCleanSweep is H-ORD-5c.
//
// > Before leaving `STARTING`, every adopted `lipH-` order that is invalid under
// > current selection, close, size, price or capital rules is **cancelled and
// > swept** (H-ORD-4). Adoption preserves orders we would place today; it is not
// > a licence for a prior incarnation's orders to persist unexamined.
//
// Swept and not merely cancelled: H-FAIL-3 makes a cancel-requested order live
// and fillable until the exchange confirms it absent. `M-L-SWEEP` ignores
// `SweepResult.Clean`, which turns the verification back into a hope.
func TestInvalidAdoptedOrdersRequireVerifiedCleanSweep(t *testing.T) {
	src := okSource()
	src.orders = rest.OrdersResult{Walk: completeWalk(), Orders: []rest.Order{
		ourOrder("keep-1", "A"),
		ourOrder("stale-1", "B"),
		ourOrder("stale-2", "B"),
	}}

	// A policy that keeps one market's orders and cancels the other's.
	policy := &perOrderPolicy{decide: func(o rest.Order) AdoptionDecision {
		if o.Ticker == "B" {
			return AdoptionCancel
		}
		return AdoptionKeep
	}}
	sweeper := newSweeperOn(src, true)

	s := newStartup(t, &recordingLatch{}, src, ownsAll(), policy, sweeper, "A", "B")
	at := s.Step(context.Background(), startupNow)
	if at.Err != nil {
		t.Fatalf("Run: %v", at.Err)
	}

	if got := len(sweeper.requests["B"]); got != 2 {
		t.Fatalf("swept %d orders in B, want 2", got)
	}
	if _, swept := sweeper.requests["A"]; swept {
		t.Fatal("the kept market was swept")
	}
	kept := at.Adoption.Kept()
	if len(kept) != 1 || kept[0].OrderID != "keep-1" {
		t.Fatalf("kept %v, want exactly keep-1", kept)
	}
	if at.Adoption.Summary().CancelledStale != 2 {
		t.Fatalf("summary reports %d cancelled, want 2",
			at.Adoption.Summary().CancelledStale)
	}

	// After the rewalk, the swept orders are gone from the account and only the
	// kept one is still resting -- which is what makes the rewalk safe to run.
	if n := len(src.orders.Orders); n != 1 || src.orders.Orders[0].OrderID != "keep-1" {
		t.Fatalf("after the sweep the account still shows %d orders (%v); a "+
			"clean sweep means a complete verifying read found none of them "+
			"resting", n, src.orders.Orders)
	}

	// The unclean case is the one that matters, and it must not complete. It
	// gets its OWN orders -- the source above has been swept.
	unclean := newSweeper(false)
	freshOrders := rest.OrdersResult{Walk: completeWalk(), Orders: []rest.Order{
		ourOrder("stale-1", "B"),
	}}
	s2 := newStartup(t, &recordingLatch{}, okSourceWith(freshOrders), ownsAll(),
		policy, unclean, "A", "B")
	at2 := s2.Step(context.Background(), startupNow)
	if at2.Err == nil || at2.Adoption != nil {
		t.Fatal("an unclean sweep still licensed the exit from STARTING: " +
			"H-FAIL-3 makes those orders live and fillable")
	}

	// It must fail on the SWEEP, immediately -- not eventually, by exhausting
	// the rewalk bound.
	//
	// Those two outcomes are both "an error", and an assertion that only checks
	// `Err != nil` cannot tell them apart. `M-L-SWEEP` ignores `res.Clean`, and
	// under it the unclean sweep is accepted, the pass reports orders cancelled,
	// and the rewalk loop runs until `maxSweepRewalks` gives up -- which still
	// returns an error, so the mutation SURVIVED a version of this test that
	// asked no more than that.
	//
	// Counting the cancel attempts discriminates: refusing an unclean sweep
	// makes exactly one, and swallowing it makes one per rewalk.
	if n := len(unclean.requests["B"]); n != 1 {
		t.Fatalf("the unclean sweep produced %d cancel attempts, want exactly "+
			"1: an unverified cancel must fail the adoption where it happens, "+
			"not be retried until a loop bound turns it into a different "+
			"error", n)
	}
	if !strings.Contains(at2.Err.Error(), "did not come back clean") {
		t.Fatalf("the failure was not attributed to the unclean sweep: %v",
			at2.Err)
	}
}

// perOrderPolicy decides per order.
type perOrderPolicy struct {
	decide func(rest.Order) AdoptionDecision
}

func (p *perOrderPolicy) DecideAdopted(ctx context.Context, o rest.Order,
	f AdoptionFacts) (AdoptionDecision, error) {
	return p.decide(o), nil
}

func okSourceWith(orders rest.OrdersResult) *fakeSource {
	s := okSource()
	s.orders = orders
	return s
}

// TestAdoptionFactsAreDefensiveCopies keeps `lip-3af`'s policy from reaching
// into the position we are in the middle of seeding.
func TestAdoptionFactsAreDefensiveCopies(t *testing.T) {
	src := okSource()
	src.positions = rest.PositionsResult{
		Walk:     completeWalk(),
		ByTicker: map[string]num.Qty{"M": num.QtyFromFloat(4)},
	}
	src.orders = rest.OrdersResult{Walk: completeWalk(),
		Orders: []rest.Order{ourOrder("o1", "M")}}

	vandal := &perOrderPolicyMutating{}
	s := newStartup(t, &recordingLatch{}, src, ownsAll(), vandal,
		newSweeper(true), "M")
	at := s.Step(context.Background(), startupNow)
	if at.Err != nil {
		t.Fatalf("Run: %v", at.Err)
	}
	if got := at.Adoption.Portfolio().Q("M"); got != num.QtyFromFloat(4) {
		t.Fatalf("q = %s after a policy mutated its facts, want 4.00; the "+
			"resulting position would be neither the exchange's answer nor a "+
			"detectable corruption", got.Wire())
	}
}

type perOrderPolicyMutating struct{}

func (p *perOrderPolicyMutating) DecideAdopted(ctx context.Context, o rest.Order,
	f AdoptionFacts) (AdoptionDecision, error) {
	f.Positions["M"] = num.QtyFromFloat(999)
	f.Positions["INVENTED"] = num.QtyFromFloat(7)
	return AdoptionKeep, nil
}

// TestTheStartupBaselineIsPreFilterAndSpansEveryAttempt pins the two properties
// a rung policy that stops at its first LIVE fill depends on, and it exists
// because getting either wrong is silent.
//
// PRE-FILTER. Step 3 asks for `backfill_h` of history, `rest.Fills` walks the
// account in full and applies that window client-side, and the live poll asks
// for everything with a zero `since`. So a trade older than the window is
// dropped from what startup seeds -- `seenTrade` never hears of it -- and the
// first live walk offers it as brand new. A baseline built from the FILTERED
// slice would not contain it, and the harness would react to last week's trade
// on every restart.
//
// ACROSS ATTEMPTS. §7.5 retries, and a pass that cancels anything discards its
// adoption and rewalks. Every one of those passes ran before the licence to
// leave STARTING existed, so everything they saw is history -- and a baseline
// reset per attempt would forget the fills that the attempt which happened to
// fail was the only one to see.
func TestTheStartupBaselineIsPreFilterAndSpansEveryAttempt(t *testing.T) {
	src := okSource()
	// The first pass sees `t-old` and `t-window`, but only `t-window` survives
	// the backfill filter -- exactly what `rest.Fills` produces for a trade
	// older than the window.
	src.fills = rest.FillsResult{
		Walk:        completeWalk(),
		Fills:       []rest.Fill{makerFill("t-window", "o1", "HELD")},
		AllTradeIDs: []string{"t-old", "t-window"},
	}
	// ...and it fails after the fills walk. The balance error is retained
	// rather than returned, so classification still runs, and the pass ends
	// without an adoption.
	src.balErr = errors.New("balance unavailable")

	s := newStartup(t, &recordingLatch{}, src, ownsAll("o1"), keepAll(),
		newSweeper(true))
	first := s.Step(context.Background(), startupNow)
	if first.Adoption != nil {
		t.Fatalf("the first pass produced an adoption despite a failed balance " +
			"read; this test needs a pass whose evidence is kept and whose " +
			"result is discarded")
	}

	// The second pass sees a different account: the old trades are gone from
	// what the endpoint returns, and a new one has appeared.
	src.balErr = nil
	src.fills = rest.FillsResult{
		Walk:        completeWalk(),
		Fills:       []rest.Fill{makerFill("t-later", "o1", "HELD")},
		AllTradeIDs: []string{"t-later"},
	}
	second := s.Step(context.Background(), startupNow)
	if second.Err != nil {
		t.Fatalf("the second pass: %v", second.Err)
	}

	got := second.Adoption.StartupTrades()
	for _, id := range []string{"t-old", "t-window", "t-later"} {
		if _, ok := got[id]; !ok {
			t.Fatalf("the startup baseline is %v and does not contain %q.\n\n"+
				"It must be the union of every complete fills walk this startup "+
				"made, taken before `backfill_h` filtered any record out. A "+
				"trade this process saw while STARTING is history whether or "+
				"not the attempt that saw it went on to produce an adoption, "+
				"and whether or not the window it asked for reached back far "+
				"enough to seed it", keysOf(got), id)
		}
	}
	if len(got) != 3 {
		t.Fatalf("the startup baseline is %v, want exactly the three trade ids "+
			"the two walks carried", keysOf(got))
	}

	// And it is FROZEN. A caller holding the boundary must not be able to move
	// it, for the same reason every other accessor on this interface copies.
	delete(got, "t-old")
	if _, ok := second.Adoption.StartupTrades()["t-old"]; !ok {
		t.Fatal("deleting from the returned map changed the adoption's own " +
			"baseline; the boundary between inherited history and live " +
			"activity is not something a consumer may edit")
	}
}

// TestTheAdoptedPortfolioCarriesTheBaseline is `lip-da6`, and it is the wiring
// the previous test's boundary is useless without.
//
// `StartupTrades()` being correct buys nothing on its own. The portfolio the
// adoption hands to the live poller is the thing that decides whether a fill
// older than `backfill_h` moves `q`, and if the baseline is not installed at the
// moment that portfolio is seeded then the FIRST live poll -- which asks with a
// zero `since` and so sees everything -- replays the whole of the account's
// out-of-window history onto a position the exchange had already reported.
//
// The assertion is deliberately made through the returned portfolio rather than
// by inspecting a field: it is exactly what `cmd/harness` does with it at
// `run.go`'s `install`, and a baseline that is correct everywhere except in the
// object that gets used is the defect this test exists to catch.
// `M-L-NOBASELINE` seeds without it.
func TestTheAdoptedPortfolioCarriesTheBaseline(t *testing.T) {
	src := okSource()
	// The exchange reports the position `t-old` already produced, and reports
	// the trade only in the PRE-FILTER identity list -- which is what
	// `rest.Fills` returns for a fill older than the window.
	src.positions = rest.PositionsResult{
		Walk: completeWalk(), ByTicker: map[string]num.Qty{"M": num.QtyFromFloat(6)},
	}
	src.fills = rest.FillsResult{
		Walk:        completeWalk(),
		Fills:       nil,
		AllTradeIDs: []string{"t-old"},
	}

	s := newStartup(t, &recordingLatch{}, src, ownsAll("o1"), keepAll(),
		newSweeper(true), "M")
	at := s.Step(context.Background(), startupNow)
	if at.Err != nil {
		t.Fatalf("Step: %v", at.Err)
	}

	pf := at.Adoption.Portfolio()
	if got := pf.Q("M"); got != num.QtyFromFloat(6) {
		t.Fatalf("seeded q = %s, want 6.00", got.Wire())
	}

	// The first live poll, asking with a zero `since`, offers the fill the
	// startup window never asked for.
	eff := pf.ApplyFills([]risk.FillEvent{{
		TradeID: "t-old", OrderID: "o1", Ticker: "M", Side: quote.SideYes,
		Price4: 5000, Count: num.QtyFromFloat(6),
	}}, ownsAll("o1"), risk.Live, startupNow.UnixMilli())

	if got := pf.Q("M"); got != num.QtyFromFloat(6) {
		t.Fatalf("q = %s after the first live poll, want 6.00 unchanged.\n\n"+
			"The adopted portfolio was seeded without the startup baseline, so "+
			"a fill older than backfill_h -- invisible to §7.5 and brand new to "+
			"a live walk that asks for all history -- was replayed on top of "+
			"the position the exchange had already reported. q_local is now "+
			"history-plus-position while q_exch is the position, and above "+
			"pos_drift_hard that is a SEV1 POSITION_DRIFT and a durable "+
			"WINDING_DOWN the harness manufactured by restarting", got.Wire())
	}
	if len(eff.Owned) != 0 {
		t.Fatalf("inherited history was reported as live: %d owned fill(s), "+
			"which H-ORD-6 makes a permanently mislabelled our_fill row",
			len(eff.Owned))
	}
	if len(eff.OwnedBackfilled) != 1 {
		t.Fatalf("owned backfilled = %d, want 1", len(eff.OwnedBackfilled))
	}
}

func keysOf(m map[string]struct{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
