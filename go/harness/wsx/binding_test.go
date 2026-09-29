package wsx

import (
	"errors"
	"testing"
	"time"

	"lip/harness/num"
	"lip/harness/quote"
	"lip/harness/rest"
	"lip/harness/risk"
)

func TestUnresolvedFillEscalatesAfterElapsedTimeAcrossWallSteps(t *testing.T) {
	p := testParams()
	g, err := NewGate([]string{fxTicker}, p)
	if err != nil {
		t.Fatal(err)
	}
	pf := risk.NewPortfolio()
	tok := g.OnConnect(at(0)).Token
	fill := restFill("clock-fill", "pending-order", fxTicker,
		quote.SideYes, 1, "0.0000", false)
	own := ledger{unresolved: map[string]bool{"pending-order": true}}
	readAt := func(stamp Stamp, seq uint64) PortfolioRead {
		return newRead(tok, stamp, seq,
			completePositions(map[string]num.Qty{}), completeOrders(nil),
			completeFills([]rest.Fill{fill}))
	}
	check := func(stamp Stamp, seq uint64, want bool) {
		t.Helper()
		eff := ApplyPortfolio(g, pf, own, nil, readAt(stamp, seq), risk.Live,
			stamp.Mono, p)
		if got := hasAnomaly(eff.Anomalies, "FILL_UNCLASSIFIABLE"); got != want {
			t.Fatalf("at mono=%v wall=%d, escalation=%t, want %t: %v",
				stamp.Mono, stamp.WallMs, got, want, classes(eff.Anomalies))
		}
	}
	check(at(0), 1, false)
	forward := at(5)
	forward.WallMs += time.Hour.Milliseconds()
	check(forward, 2, false)
	backward := at(119)
	backward.WallMs -= time.Hour.Milliseconds()
	check(backward, 3, false)
	check(at(120), 4, true)
}

// TestPortfolioPollBindsListedOrdersBeforeClassifyingFills is the live half of
// the F2 repair: the poll cycle does not merely tolerate an unresolved
// reservation, it CLOSES it.
//
// The exchange is already telling us the missing fact. `GET /portfolio/orders`
// returns every resting order with its `client_order_id`, so an order dispatched
// under a reservation whose binding was lost -- to a crash, to a store that was
// refusing writes -- is sitting in the walk we read every five seconds with both
// halves of the binding attached. Not writing it back means the reservation
// stays outstanding until `lip-eyq`'s startup walk runs, which on a process that
// does not restart is never; and while it is outstanding every unrecognised fill
// on the whole account defers.
//
// Three things are pinned, and the second is the one the ordering exists for:
//
//   - the binding is submitted, for the coid the LEDGER says is outstanding and
//     for no other;
//   - it is submitted BEFORE the fills in the same cycle are classified;
//   - the fill in that same cycle still defers, because the binding is durable-
//     asynchronous and H-ORD-9 answers from committed rows only. One cycle of
//     latency, and no false foreign at any point in it.
//
// `M-W-NOBIND` deletes the submission.
func TestPortfolioPollBindsListedOrdersBeforeClassifyingFills(t *testing.T) {
	p := testParams()
	g, err := NewGate([]string{fxTicker}, p)
	if err != nil {
		t.Fatal(err)
	}
	pf := risk.NewPortfolio()
	tok := g.OnConnect(at(0)).Token

	const (
		coid    = "lipH-abc-0-Y-7"
		orderID = "ord-recovered"
	)

	// The ledger: one reservation outstanding, nothing bound. So the order id
	// the exchange is about to report is unrecognised, and a fill on it is
	// unresolved rather than foreign.
	own := ledger{unresolved: map[string]bool{orderID: true}}
	bind := newBinder(coid)

	read := newRead(tok, at(0), 1,
		completePositions(map[string]num.Qty{}),
		completeOrders([]rest.Order{
			{
				OrderID: orderID, ClientOrderID: coid, Ticker: fxTicker,
				Side: quote.SideYes, Price4: cents4(42),
				Remaining: contracts(5), Ours: true,
			},
			// A resting order that is NOT one of our outstanding reservations.
			// It must not be bound: `bindOrder` would be told to attach an
			// exchange order id to a reservation this store has no row for.
			{
				OrderID: "ord-somebody-else", ClientOrderID: "someone-elses-coid",
				Ticker: fxTicker, Side: quote.SideNo, Price4: cents4(20),
				Remaining: contracts(1),
			},
		}),
		completeFills([]rest.Fill{
			restFill("t1", orderID, fxTicker, quote.SideYes, 5, "0.0000", false),
		}))

	eff := ApplyPortfolio(g, pf, own, bind, read, risk.Live, at(0).Mono, p)

	// --- the binding was submitted, and only for the outstanding coid -------
	if len(bind.submitted) != 1 {
		t.Fatalf("the orders walk submitted %d binding(s), want 1: %+v. The "+
			"exchange reported both halves of the binding this ledger is "+
			"missing, and a walk that reads them and does not write them back "+
			"leaves the reservation outstanding for the life of the process",
			len(bind.submitted), bind.submitted)
	}
	if got := bind.submitted[0]; got.Coid != coid || got.OrderID != orderID {
		t.Fatalf("bound %+v, want coid %s -> order %s", got, coid, orderID)
	}
	if len(eff.Bound) != 1 || eff.Bound[0].OrderID != orderID {
		t.Fatalf("Bound = %+v, want the one submitted binding", eff.Bound)
	}

	// --- the SAME cycle's fill defers; it is never foreign ------------------
	if len(eff.DeferredFill) != 1 || eff.DeferredFill[0].TradeID != "t1" {
		t.Fatalf("DeferredFill = %+v, want the fill on the order we just "+
			"bound. The binding is not durable yet, so H-ORD-9 cannot call it "+
			"ours -- but it must not call it foreign either", eff.DeferredFill)
	}
	if len(eff.OwnedFill) != 0 {
		t.Fatalf("a fill was applied from an UNCOMMITTED binding: %+v. That is "+
			"the in-memory fallback H-ORD-9 forbids", eff.OwnedFill)
	}
	if eff.Stop {
		t.Fatal("the cycle requested a global stop; nothing foreign happened, " +
			"and the fill that could not be attributed is our own")
	}
	if hasAnomaly(eff.Anomalies, "FOREIGN_FILL") {
		t.Fatalf("FOREIGN_FILL raised on the cycle that bound the order: %v",
			classes(eff.Anomalies))
	}

	// --- fills truth still refreshed ----------------------------------------
	//
	// The exchange read succeeded. Withholding the stamp because an attribution
	// is pending would age truth out and stop dispatch over the normal state of
	// a ledger with a reservation in flight.
	if !eff.Applied[TruthFills] || !eff.Applied[TruthOrders] {
		t.Fatalf("a complete cycle did not refresh truth: %v", eff.Applied)
	}

	// --- next cycle, the binding has committed ------------------------------
	settled := ledger{ours: map[string]bool{orderID: true}}
	bind.unresolved = map[string]struct{}{}
	read2 := newRead(tok, at(5), 2,
		completePositions(map[string]num.Qty{}),
		completeOrders([]rest.Order{{
			OrderID: orderID, ClientOrderID: coid, Ticker: fxTicker,
			Side: quote.SideYes, Price4: cents4(42), Remaining: contracts(5),
			Ours: true,
		}}),
		completeFills([]rest.Fill{
			restFill("t1", orderID, fxTicker, quote.SideYes, 5, "0.0000", false),
		}))

	eff2 := ApplyPortfolio(g, pf, settled, bind, read2, risk.Live, at(5).Mono, p)
	if len(eff2.OwnedFill) != 1 {
		t.Fatalf("the deferred fill was not applied on the cycle after its "+
			"binding committed: Owned = %+v, Deferred = %+v. The fills walk "+
			"re-offers it every poll, and a deferral that was marked seen is a "+
			"fill no later poll can apply", eff2.OwnedFill, eff2.DeferredFill)
	}
	if len(bind.submitted) != 1 {
		t.Fatalf("a resolved reservation was bound again: %+v. The ledger's "+
			"unresolved set is what gates the submission, and re-submitting "+
			"for a coid it no longer lists means the gate is not being read",
			bind.submitted)
	}
	// `q` is deliberately not asserted here. `applyPositions` runs last and
	// OVERWRITES from the exchange (H-POS-1), so the observable for "the fill
	// was applied" is `OwnedFill` -- which is also the set that reaches
	// `our_fill`. Asserting q would be asserting what the positions fixture
	// says, not what the fills path did.
}

// TestPortfolioPollSurvivesABinderThatRefuses pins the failure posture.
//
// A store that will not take the binding is already a SEV1 through store
// health. Letting it also halt the position model would couple the two in the
// direction this package refuses everywhere else -- and nothing is lost by
// carrying on, because the reservation simply stays outstanding and the fill
// keeps deferring instead of reading as foreign.
func TestPortfolioPollSurvivesABinderThatRefuses(t *testing.T) {
	p := testParams()
	g, err := NewGate([]string{fxTicker}, p)
	if err != nil {
		t.Fatal(err)
	}
	pf := risk.NewPortfolio()
	tok := g.OnConnect(at(0)).Token

	const coid = "lipH-abc-0-Y-7"
	bind := newBinder(coid)
	bind.err = errors.New("the store is not accepting records")

	read := newRead(tok, at(0), 1,
		completePositions(map[string]num.Qty{}),
		completeOrders([]rest.Order{{
			OrderID: "ord-recovered", ClientOrderID: coid, Ticker: fxTicker,
			Side: quote.SideYes, Price4: cents4(42), Remaining: contracts(5),
			Ours: true,
		}}),
		completeFills([]rest.Fill{
			restFill("t1", "ord-recovered", fxTicker, quote.SideYes, 5,
				"0.0000", false),
		}))

	own := ledger{unresolved: map[string]bool{"ord-recovered": true}}
	eff := ApplyPortfolio(g, pf, own, bind, read, risk.Live, at(0).Mono, p)

	if !hasAnomaly(eff.Anomalies, "ORDER_BINDING_NOT_SUBMITTED") {
		t.Fatalf("a refused binding was silent: %v", classes(eff.Anomalies))
	}
	if eff.Stop {
		t.Fatal("a refused binding stopped the harness; the store failing is " +
			"already SEV1 through health, and the fill it could not bind is " +
			"still safely deferring")
	}
	if len(eff.Bound) != 0 {
		t.Fatalf("a refused binding was reported as submitted: %+v", eff.Bound)
	}
	if len(eff.DeferredFill) != 1 {
		t.Fatalf("DeferredFill = %+v; a binding that could not be submitted "+
			"leaves the fill exactly where it was", eff.DeferredFill)
	}
	if !eff.Applied[TruthOrders] {
		t.Fatal("a refused binding withheld the orders freshness stamp; the " +
			"orders WALK was complete and is what that stamp is about")
	}
}

func hasAnomaly(as []risk.Anomaly, class string) bool {
	for _, a := range as {
		if a.Class == class {
			return true
		}
	}
	return false
}
