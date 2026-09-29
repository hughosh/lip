package main

import (
	"context"
	"testing"
	"time"

	"lip/harness/lifecycle"
	"lip/harness/num"
	"lip/harness/quote"
	"lip/harness/rest"
)

// These are actual owned orders in SQLite and on the fake exchange. The
// portfolio walk has to recognize their coids; no gate truth is forged.
func driftRestingOrders(t *testing.T, g *gateFailOwner) {
	t.Helper()
	for i, side := range []quote.Side{quote.SideYes, quote.SideNo} {
		id, price, wireSide, wirePrice := "DRIFT-ADD", 40, string(rest.Bid), "0.4000"
		if side == quote.SideNo {
			id, price, wireSide, wirePrice = "DRIFT-EXIT", 55, string(rest.Ask), "0.4500"
		}
		g.h.seamOwnedOrderOn(id, uint64(i+1), side, price, num.QtyFromFloat(1))
		coid, err := rest.Coid("SEAMPNL0", 0, side, uint64(i+1))
		if err != nil {
			t.Fatal(err)
		}
		g.h.ex.mu.Lock()
		g.h.ex.resting = append(g.h.ex.resting, seamRestingOrder(seamCreate{
			Ticker: seamTicker, WireSide: wireSide, Price: wirePrice,
			Count: "1.00", Coid: coid, OrderID: id,
		}))
		g.h.ex.mu.Unlock()
	}
}

func driftPollAgain(t *testing.T, g *gateFailOwner) {
	t.Helper()
	g.h.clk.Advance(time.Millisecond)
	g.o.offerToken(g.tokens, g.o.reconcileToken)
	reducerConfirmationPoll(t, g)
}

func driftAssertReducer(t *testing.T, g *gateFailOwner) {
	t.Helper()
	h, o := g.h, g.o
	if o.market != quote.Reducing || o.atRisk(quote.SideNo) <= 0 {
		t.Fatalf("drift state=%v, reducing exposure=%v", o.market, o.atRisk(quote.SideNo))
	}
	ctx, cancel := context.WithCancel(h.ctx)
	defer cancel()
	resultsDone := make(chan struct{})
	go func() { defer close(resultsDone); _ = o.sd.runResults(ctx) }()
	writes, results := make(chan writeRequest, 2), make(chan writeResult, 2)
	dispatchDone := make(chan struct{})
	go func() { defer close(dispatchDone); h.rig.dispatchLoop(ctx, writes, o.sd.Permits(), results) }()
	defer func() { cancel(); <-dispatchDone; <-resultsDone }()
	o.pump(h.clk.Now().Mono, writes)
	select {
	case res := <-results:
		if res.Err != nil || res.Req.Op != quote.OpCancel ||
			res.Req.Side != quote.SideYes || !res.Absent {
			t.Fatalf("adding sweep=%+v", res)
		}
		o.applyWriteResult(res)
	case <-time.After(3 * time.Second):
		t.Fatal("drift did not dispatch adding cancellation")
	}
	h.ex.mu.Lock()
	keptExit := len(h.ex.resting) == 1 && h.ex.resting[0]["order_id"] == "DRIFT-EXIT"
	h.ex.mu.Unlock()
	if !keptExit {
		t.Fatal("drift sweep did not preserve the reducer")
	}
}

func TestComposedToleranceDriftReducesOnlyAfterTwoCompletePolls(t *testing.T) {
	g := newGateFailOwner(t, num.QtyFromFloat(1))
	h, o := g.h, g.o
	driftRestingOrders(t, g)
	h.ex.setPosition(seamTicker, "1.00")
	reducerConfirmationPoll(t, g)
	o.evaluate(h.clk.Now().Mono)
	if o.market == quote.Reducing || o.global != quote.Running {
		t.Fatalf("agreement control market=%v global=%v", o.market, o.global)
	}

	// The first complete discrepancy overwrites q_local. The next one must
	// therefore move again; repeating 1.05 would be an agreement, not a streak.
	h.ex.setPosition(seamTicker, "1.05")
	driftPollAgain(t, g)
	o.evaluate(h.clk.Now().Mono)
	if o.market == quote.Reducing || o.global != quote.Running || h.latchTrigger() != "" {
		t.Fatalf("one tolerance drift market=%v global=%v latch=%q", o.market, o.global, h.latchTrigger())
	}

	// A failed positions page may neither provide the second discrepancy nor
	// turn the old position into flat. Its following complete walk can still
	// supply the second consecutive *complete* drift observation.
	h.ex.breakPositions(true)
	h.ex.setPosition(seamTicker, "1.10")
	driftPollAgain(t, g)
	if q := h.rig.pf.Q(seamTicker); q != num.QtyFromFloat(1.05) {
		t.Fatalf("incomplete poll changed q_local to %v", q)
	}
	o.evaluate(h.clk.Now().Mono)
	if o.market == quote.Reducing || h.latchTrigger() != "" {
		t.Fatalf("incomplete poll market=%v latch=%q", o.market, h.latchTrigger())
	}
	h.ex.breakPositions(false)
	driftPollAgain(t, g)
	o.evaluate(h.clk.Now().Mono)
	if o.market != quote.Reducing || o.global != quote.Running || h.latchTrigger() != "" {
		t.Fatalf("two complete drifts market=%v global=%v latch=%q", o.market, o.global, h.latchTrigger())
	}
	if q := h.rig.pf.Q(seamTicker); q != num.QtyFromFloat(1.10) {
		t.Fatalf("complete poll did not overwrite q_local: %v", q)
	}
	driftAssertReducer(t, g)
	before := h.ex.ordersWalkCount()
	driftPollAgain(t, g)
	o.publish(h.clk.Now().Mono)
	if h.ex.ordersWalkCount() <= before || h.snapshot() == nil {
		t.Fatal("portfolio poller or snapshot monitor stopped after REDUCING")
	}
}

func TestComposedHardDriftLatchesSpecificCauseAndKeepsReducer(t *testing.T) {
	g := newGateFailOwner(t, num.QtyFromFloat(1))
	h, o := g.h, g.o
	driftRestingOrders(t, g)
	h.ex.setPosition(seamTicker, "1.00")
	reducerConfirmationPoll(t, g)
	if got := h.latchTrigger(); got != "" {
		t.Fatalf("agreement control already latched %q", got)
	}

	// 6.01 remains below inv_hard=7 and inv_kill=18; the only global
	// candidate from this poll is the single 5.01 hard position drift.
	h.ex.setPosition(seamTicker, "6.01")
	driftPollAgain(t, g)
	o.evaluate(h.clk.Now().Mono)
	if o.global != quote.WindingDown || o.market != quote.Reducing {
		t.Fatalf("hard drift global=%v market=%v", o.global, o.market)
	}
	if got := h.latchTrigger(); got != "position_drift" {
		t.Fatalf("hard drift durable trigger=%q, want position_drift", got)
	}
	latch, err := lifecycle.NewFileLatch(h.cfg.Paths.Latch)
	if err != nil {
		t.Fatal(err)
	}
	rec, present, err := latch.Load()
	if err != nil || !present || rec.Market != seamTicker {
		t.Fatalf("hard drift latch present=%v record=%+v err=%v", present, rec, err)
	}
	h.awaitGlobalEvent("RUNNING->WINDING_DOWN/global_stop")
	driftAssertReducer(t, g)
	before := h.ex.ordersWalkCount()
	driftPollAgain(t, g)
	o.publish(h.clk.Now().Mono)
	if h.ex.ordersWalkCount() <= before || h.snapshot() == nil || o.global != quote.WindingDown {
		t.Fatal("portfolio monitor stopped or durable global halt cleared")
	}
}
