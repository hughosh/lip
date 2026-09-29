package main

import (
	"context"
	"syscall"
	"testing"
	"time"

	"lip/harness/lifecycle"
	"lip/harness/num"
	"lip/harness/quote"
)

// A SIGTERM enters the real drain through the owner. A civil clock correction
// must neither authorise an inventory-bearing exit nor consume its escalation
// interval. The tracker receives paired-clock readings from the composed rig.
func TestOwnerDrainTimeoutSurvivesWallClockCorrections(t *testing.T) {
	g := newGateFailOwner(t, num.QtyFromFloat(1))
	g.o.applySignal(syscall.SIGTERM)
	if g.o.global != quote.WindingDown || !g.h.rig.drain.Started() {
		t.Fatalf("SIGTERM did not begin a winding-down drain: %s", g.o.global)
	}
	g.h.takeRaised()
	obs := lifecycle.DrainObservation{TruthKnown: true, AnyInventory: true}
	check := func(want int) {
		t.Helper()
		if g.o.sd.observeDrain(obs) {
			t.Fatal("drain authorised exit with inventory open")
		}
		if got := countClass(g.h.takeRaised(), "DRAIN_TIMEOUT"); got != want {
			t.Fatalf("at mono=%v wall=%d: %d timeout alerts, want %d",
				g.h.clk.monoNow(), g.h.clk.wallMs(), got, want)
		}
	}
	gateFailStepWall(g.h.clk, time.Hour)
	check(0)
	gateFailStepWall(g.h.clk, -2*time.Hour)
	check(0)
	g.at(g.o.p.DrainTimeout - time.Millisecond)
	check(0)
	g.at(g.o.p.DrainTimeout)
	check(1)
}

// The close lead is a civil-time deadline. The owner must recalculate it from
// the current wall reading on each evaluation, including after a correction.
func TestOwnerCloseLeadRecomputesAfterWallClockCorrections(t *testing.T) {
	g := newGateFailOwner(t, num.QtyFromFloat(1))
	driftRestingOrders(t, g)
	g.h.ex.setPosition(seamTicker, "1.00")
	reducerConfirmationPoll(t, g)
	g.o.closeAt = time.UnixMilli(g.h.clk.wallMs()).Add(2 * g.o.p.CloseLead)
	if until, ok := g.o.untilClose(); !ok || until != 2*g.o.p.CloseLead {
		t.Fatalf("initial close lead: until=%v known=%t", until, ok)
	}
	gateFailStepWall(g.h.clk, -time.Hour)
	g.o.evaluate(g.h.clk.monoNow())
	if until, ok := g.o.untilClose(); !ok || until != 3*time.Hour {
		t.Fatalf("backward correction did not recompute close: until=%v known=%t", until, ok)
	}
	if g.o.market != quote.Quoting {
		t.Fatalf("backward correction moved market to %s before the close lead", g.o.market)
	}
	gateFailStepWall(g.h.clk, 2*time.Hour+time.Minute)
	g.o.evaluate(g.h.clk.monoNow())
	if until, ok := g.o.untilClose(); !ok || until != g.o.p.CloseLead-time.Minute {
		t.Fatalf("forward correction did not recompute close: until=%v known=%t", until, ok)
	}
	if g.o.market != quote.Settling {
		t.Fatalf("forward correction crossed close lead but market is %s", g.o.market)
	}
	// The clock correction must reach the real cancel/confirm boundary before
	// final_lead; a state label alone would leave the adding order fillable.
	ctx, cancel := context.WithCancel(g.h.ctx)
	resultsDone := make(chan struct{})
	go func() { defer close(resultsDone); _ = g.o.sd.runResults(ctx) }()
	writes, results := make(chan writeRequest, 2), make(chan writeResult, 2)
	dispatchDone := make(chan struct{})
	go func() { defer close(dispatchDone); g.h.rig.dispatchLoop(ctx, writes, g.o.sd.Permits(), results) }()
	defer func() { cancel(); <-dispatchDone; <-resultsDone }()
	g.o.pump(g.h.clk.Now().Mono, writes)
	select {
	case result := <-results:
		if result.Req.Op != quote.OpCancel || result.Req.Side != quote.SideYes || !result.Absent {
			t.Fatalf("close-lead adding sweep: %+v", result)
		}
		g.o.applyWriteResult(result)
	case <-time.After(3 * time.Second):
		t.Fatal("wall-clock correction did not dispatch the adding sweep")
	}
	g.h.ex.mu.Lock()
	kept := len(g.h.ex.resting) == 1 && g.h.ex.resting[0]["order_id"] == "DRIFT-EXIT"
	g.h.ex.mu.Unlock()
	if !kept || g.o.atRisk(quote.SideNo) != num.QtyFromFloat(1) {
		t.Fatal("clock catch-up lost or oversized the funded reducer")
	}
	driftPollAgain(t, g)
	g.o.publish(g.h.clk.Now().Mono)
	ticks, monitored := make(chan time.Time, 1), make(chan struct{})
	go func() { defer close(monitored); g.h.rig.mon.run(ctx, ticks) }()
	ticks <- time.Now()
	g.h.await("monitor after clock catch-up", func() bool { return g.h.rig.last.Load() != nil })
	if hb := g.h.rig.heartbeat(); len(hb.Markets) != 1 || hb.Markets[0].State != quote.Settling {
		t.Fatalf("clock catch-up heartbeat=%+v", hb)
	}
	cancel()
	<-monitored
}
