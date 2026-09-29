package main

import (
	"strings"
	"testing"

	"lip/harness/num"
	"lip/harness/quote"
	"lip/harness/rest"
	"lip/harness/risk"
)

func capitalOwner(t *testing.T, q num.Qty, cap float64) *gateFailOwner {
	t.Helper()
	g := newGateFailOwner(t, q)
	g.o.p.S = num.QtyFromFloat(1)
	g.o.p.CapitalMax = num.MoneyFromDollars(cap)
	g.o.p.CapitalReserve = 0
	g.o.p.NMarkets = 1
	g.o.p.Concentration = 2
	return g
}

func capitalOrder(t *testing.T, g *gateFailOwner, side quote.Side, role quote.Role, cents int, qty float64, seq uint64) writeRequest {
	t.Helper()
	coid, err := rest.Coid(g.h.rig.runID, pilotMarketIdx, side, seq)
	if err != nil {
		t.Fatal(err)
	}
	count := num.QtyFromFloat(qty)
	order, err := rest.NewCreateOrder(seamTicker, side, cents, count, count, coid)
	if err != nil {
		t.Fatal(err)
	}
	return writeRequest{Market: seamTicker, Side: side, Role: role, Op: quote.OpPlace, Order: order}
}

func TestCapitalDispatchSecondOppositeBuildCannotSpendInflightBudget(t *testing.T) {
	g := capitalOwner(t, 0, .70)
	first, err := g.o.build(quote.Dispatch{Market: seamTicker, Side: quote.SideYes, Role: quote.RoleAdding, Op: quote.OpPlace})
	if err != nil {
		t.Fatalf("first funded build: %v", err)
	}
	g.o.inflight = append(g.o.inflight, &first)
	_, err = g.o.build(quote.Dispatch{Market: seamTicker, Side: quote.SideNo, Role: quote.RoleAdding, Op: quote.OpPlace})
	if err == nil || !strings.Contains(err.Error(), "placement capital") {
		t.Fatalf("second opposite build must count first worker's %.2f collateral: %v", risk.Deployed(g.o.exposures()).Dollars(), err)
	}
}

func TestFinalBuildCannotCrossAnyPossiblyLiveOppositeOrder(t *testing.T) {
	for _, state := range []string{"inflight", "unknown", "unconfirmed_cancel"} {
		t.Run(state, func(t *testing.T) {
			g := capitalOwner(t, 0, 100)
			reducerConfirmationPoll(t, g)
			opposite := capitalOrder(t, g, quote.SideNo, quote.RoleAdding, 65, 1, 101)
			switch state {
			case "inflight":
				g.o.inflight = []*writeRequest{&opposite}
			case "unknown":
				g.o.pending[opposite.Order.ClientOrderID()] = pendingOrder{side: quote.SideNo, cents: 65, qty: num.QtyFromFloat(1)}
			case "unconfirmed_cancel":
				g.h.installResting(risk.LiveOrder{OrderID: "late-opposite", Ticker: seamTicker, Side: quote.SideNo, Price4: 6500, Remaining: num.QtyFromFloat(1)})
				g.o.inflight = []*writeRequest{{Market: seamTicker, Side: quote.SideNo, Role: quote.RoleAdding, Op: quote.OpCancel}}
			}
			req, err := g.o.build(quote.Dispatch{Market: seamTicker, Side: quote.SideYes, Role: quote.RoleAdding, Op: quote.OpPlace})
			if err != nil {
				t.Fatalf("a funded, noncrossing lower price should remain available: %v", err)
			}
			if req.Order.PriceCents() != 34 || quote.Crosses(req.Order.PriceCents(), 65) {
				t.Fatalf("final build crosses %s opposite exposure: YES=%d NO=65", state, req.Order.PriceCents())
			}
		})
	}
}

func TestCapitalDispatchUnknownAndUnconfirmedCancelStayCharged(t *testing.T) {
	for _, state := range []string{"unknown", "cancel_inflight", "resting"} {
		t.Run(state, func(t *testing.T) {
			g := capitalOwner(t, 0, .70)
			first := capitalOrder(t, g, quote.SideYes, quote.RoleAdding, 40, 1, 21)
			if state == "unknown" {
				g.o.pending[first.Order.ClientOrderID()] = pendingOrder{side: first.Side, cents: 40, qty: first.Order.Count()}
			} else {
				g.h.installResting(risk.LiveOrder{OrderID: "capital-order", Ticker: seamTicker, Side: quote.SideYes, Price4: 4000, Remaining: first.Order.Count()})
				if state == "cancel_inflight" {
					cancel := writeRequest{Market: seamTicker, Side: quote.SideYes, Role: quote.RoleAdding, Op: quote.OpCancel}
					g.o.inflight = []*writeRequest{&cancel}
					g.o.cancelConfirmed[quote.SideYes] = false
				}
			}
			candidate := capitalOrder(t, g, quote.SideNo, quote.RoleAdding, 55, 1, 22)
			if err := g.o.checkPlacementCapital(candidate); err == nil {
				t.Fatal("possibly-live collateral released before confirmed absence")
			}
		})
	}
}

func TestListedCreateReservesCapitalOnceBeforeAndAfterLateWorkerResult(t *testing.T) {
	g := capitalOwner(t, num.QtyFromFloat(1), 2)
	g.h.seamOwnedOrderOn("listed-exit", 111, quote.SideNo, 55, num.QtyFromFloat(1))
	coid, ok := g.h.rig.store.Ownership().Bound("listed-exit")
	if !ok {
		t.Fatal("fixture did not commit ownership")
	}
	order, err := rest.NewCreateOrder(seamTicker, quote.SideNo, 55, num.QtyFromFloat(1), num.QtyFromFloat(1), coid)
	if err != nil {
		t.Fatal(err)
	}
	req := writeRequest{Market: seamTicker, Side: quote.SideNo, Role: quote.RoleReducing, Op: quote.OpPlace, Order: order}
	g.h.installResting(risk.LiveOrder{OrderID: "listed-exit", Ticker: seamTicker, Side: quote.SideNo, Price4: 5500, Remaining: num.QtyFromFloat(1)})
	g.o.inflight = []*writeRequest{&req}
	check := func() {
		t.Helper()
		if got := risk.Deployed(g.o.exposures()); got != num.MoneyFromDollars(1.55) {
			t.Fatalf("listed one-contract position and exit counted twice: %s", got)
		}
		if got := g.o.atRisk(quote.SideNo); got != num.QtyFromFloat(1) {
			t.Fatalf("listed exit quantity=%s", got.Wire())
		}
	}
	check()
	g.o.applyWriteResult(writeResult{Req: req, Sent: true, Bound: true, Create: rest.CreateResult{
		Outcome: rest.CreateAcked, Coid: coid, OrderID: "listed-exit", MaxLive: num.QtyFromFloat(1),
	}})
	check()
	if _, remains := g.o.pending[coid]; remains || g.o.retryPlacementAllowed(0, req) {
		t.Fatal("late worker result reopened an already positively resolved create")
	}
}

func TestCapitalDispatchUnknownRetryCountsSameCoidOnce(t *testing.T) {
	g := capitalOwner(t, 0, .50)
	// Retry authority also needs independently reconciled portfolio truth.
	reducerConfirmationPoll(t, g)
	req := capitalOrder(t, g, quote.SideYes, quote.RoleAdding, 40, 1, 25)
	g.o.pending[req.Order.ClientOrderID()] = pendingOrder{side: req.Side, cents: 40, qty: req.Order.Count()}
	g.o.inflight = []*writeRequest{&req}
	if got := risk.Deployed(g.o.exposures()); got != num.MoneyFromDollars(.40) {
		t.Fatalf("same coid charged twice: %s", got)
	}
	if err := g.o.checkPlacementCapital(req); err != nil {
		t.Fatalf("funded same-coid retry refused: %v", err)
	}
	if !g.o.retryPlacementAllowed(0, req) {
		t.Fatal("funded same-coid retry lost placement authority")
	}
	g.o.p.CapitalMax = num.MoneyFromDollars(.30)
	if err := g.o.checkPlacementCapital(req); err == nil {
		t.Fatal("retry bypassed newly insufficient capital")
	}
	if g.o.retryPlacementAllowed(0, req) {
		t.Fatal("retry authority bypassed final capital check")
	}
}

func TestCapitalDispatchReducerUsesReserveAndIgnoresConcentration(t *testing.T) {
	g := capitalOwner(t, num.QtyFromFloat(1), 1.60)
	g.o.p.CapitalReserve = .50
	g.o.p.Concentration = .10
	reducer := capitalOrder(t, g, quote.SideNo, quote.RoleReducing, 55, 1, 30)
	if err := g.o.checkPlacementCapital(reducer); err != nil {
		t.Fatalf("funded exit lost reserve/concentration exemption: %v", err)
	}
	adding := capitalOrder(t, g, quote.SideYes, quote.RoleAdding, 40, 1, 31)
	if err := g.o.checkPlacementCapital(adding); err == nil {
		t.Fatal("adding spent reducer reserve")
	}
}

func TestCapitalDispatchAddingHonorsConcentrationAndOutstandingReducer(t *testing.T) {
	t.Run("reserve", func(t *testing.T) {
		g := capitalOwner(t, 0, 1)
		g.o.p.CapitalReserve = .50
		req := capitalOrder(t, g, quote.SideNo, quote.RoleAdding, 55, 1, 34)
		if err := g.o.checkPlacementCapital(req); err == nil {
			t.Fatal("adding consumed the unallocated capital reserve")
		}
	})
	t.Run("concentration", func(t *testing.T) {
		g := capitalOwner(t, 0, 10)
		g.o.p.Concentration = .03 // Thirty cents locally, ten dollars globally.
		req := capitalOrder(t, g, quote.SideYes, quote.RoleAdding, 40, 1, 35)
		if err := g.o.checkPlacementCapital(req); err == nil {
			t.Fatal("adding ignored per-market concentration")
		}
	})
	t.Run("outstanding_reducer", func(t *testing.T) {
		g := capitalOwner(t, num.QtyFromFloat(1), 1.70)
		req := capitalOrder(t, g, quote.SideYes, quote.RoleAdding, 40, 1, 36)
		if err := g.o.checkPlacementCapital(req); err == nil {
			t.Fatal("adding consumed the unfunded reducer's 55 cents")
		}
	})
}

func TestCapitalDispatchChecksActualLatePrice(t *testing.T) {
	g := capitalOwner(t, 0, .45)
	d := quote.Dispatch{Market: seamTicker, Side: quote.SideYes, Role: quote.RoleAdding, Op: quote.OpPlace}
	if _, err := g.o.build(d); err != nil {
		t.Fatalf("40-cent build should fit: %v", err)
	}
	g.h.installBook([][]string{{"0.6000", "20.00"}}, [][]string{{"0.3500", "20.00"}})
	if _, err := g.o.build(d); err == nil || !strings.Contains(err.Error(), "placement capital") {
		t.Fatalf("late 60-cent price must exceed remaining 45 cents: %v", err)
	}
}
