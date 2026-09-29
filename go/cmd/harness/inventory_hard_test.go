package main

import (
	"context"
	"testing"
	"time"

	"lip/harness/num"
	"lip/harness/quote"
	"lip/harness/rest"
)

// Cross the hard threshold by less than pos_drift_hard on a single poll,
// isolating the inventory relay from both the drift ladder and inv_kill.
func TestInventoryHardBreachSweepsAddingAndKeepsCappedReducerAndMonitor(t *testing.T) {
	g := newGateFailOwner(t, num.QtyFromFloat(6))
	h, o := g.h, g.o
	o.p.S = num.QtyFromFloat(12)
	for i, side := range []quote.Side{quote.SideYes, quote.SideNo} {
		id, price, qty := "HARD-ADD", 40, num.QtyFromFloat(1)
		wireSide, wirePrice := string(rest.Bid), "0.4000"
		if side == quote.SideNo {
			id, price, qty = "HARD-EXIT", 55, num.QtyFromFloat(6)
			wireSide, wirePrice = string(rest.Ask), "0.4500"
		}
		h.seamOwnedOrderOn(id, uint64(i+1), side, price, qty)
		coid, _ := rest.Coid("SEAMPRIOR", 0, side, uint64(i+1))
		h.ex.mu.Lock()
		h.ex.resting = append(h.ex.resting, seamRestingOrder(seamCreate{Ticker: seamTicker, WireSide: wireSide, Price: wirePrice, Count: qty.Wire(), Coid: coid, OrderID: id}))
		h.ex.mu.Unlock()
	}
	h.ex.setPosition(seamTicker, "6.00")
	reducerConfirmationPoll(t, g)
	o.evaluate(h.clk.Now().Mono)
	if o.market != quote.Skewed {
		t.Fatalf("control state=%v", o.market)
	}
	// Remove the control's harmless resize intents; no control write was sent.
	for _, in := range h.rig.queue.Pending() {
		h.rig.queue.Drop(in.ID)
	}
	h.ex.setPosition(seamTicker, "7.01")
	h.clk.Advance(time.Millisecond)
	o.offerToken(g.tokens, o.reconcileToken)
	reducerConfirmationPoll(t, g)
	o.evaluate(h.clk.Now().Mono)
	if o.market != quote.Reducing || o.global != quote.Running || h.latchTrigger() != "" {
		t.Fatalf("hard breach state=%v global=%v latch=%v", o.market, o.global, h.latchTrigger())
	}
	ctx, cancel := context.WithCancel(h.ctx)
	done := make(chan struct{})
	go func() { defer close(done); _ = o.sd.runResults(ctx) }()
	writes, results := make(chan writeRequest, 2), make(chan writeResult, 2)
	dispatched := make(chan struct{})
	go func() { defer close(dispatched); h.rig.dispatchLoop(ctx, writes, o.sd.Permits(), results) }()
	defer func() { cancel(); <-dispatched; <-done }()
	o.pump(h.clk.Now().Mono, writes)
	select {
	case res := <-results:
		if res.Req.Op != quote.OpCancel || res.Req.Side != quote.SideYes || !res.Absent {
			t.Fatalf("adding sweep=%+v", res)
		}
		o.applyWriteResult(res)
	case <-time.After(3 * time.Second):
		t.Fatal("adding sweep not dispatched")
	}
	h.ex.mu.Lock()
	if len(h.ex.resting) != 1 || h.ex.resting[0]["order_id"] != "HARD-EXIT" {
		t.Fatalf("reducer lost: %+v", h.ex.resting)
	}
	h.ex.mu.Unlock()
	if got := o.atRisk(quote.SideNo); got <= 0 || got > num.QtyFromFloat(7.01) {
		t.Fatalf("uncapped reducer=%v", got)
	}
	walks := h.ex.ordersWalkCount()
	h.clk.Advance(time.Millisecond)
	o.offerToken(g.tokens, o.reconcileToken)
	reducerConfirmationPoll(t, g)
	o.publish(h.clk.Now().Mono)
	if h.ex.ordersWalkCount() <= walks || h.snapshot() == nil {
		t.Fatal("portfolio and monitor failed to continue")
	}
	ticks, monitored := make(chan time.Time, 1), make(chan struct{})
	go func() { defer close(monitored); h.rig.mon.run(ctx, ticks) }()
	ticks <- time.Now()
	h.await("monitor sample while reducing", func() bool { return h.rig.last.Load() != nil })
	if hb := h.rig.heartbeat(); hb.Global != quote.Running || len(hb.Markets) != 1 || hb.Markets[0].State != quote.Reducing {
		t.Fatalf("heartbeat lost the stopped market: %+v", hb)
	}
	// The runtime selects one market. Its shared global state remains usable
	// by an unaffected market's state machine; no global stop leaks from this
	// per-market inventory threshold.
	other, _ := quote.NextMarket(quote.MarketInput{State: quote.Quoting,
		Global: o.global, Selected: true, HasClose: true, UntilClose: 24 * time.Hour}, o.p)
	if other != quote.Quoting {
		t.Fatalf("unaffected market state=%v", other)
	}
	cancel()
	<-monitored
}
