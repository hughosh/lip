package main

import (
	"testing"

	"lip/harness/quote"
	"lip/harness/risk"
)

func TestLiveForeignOrderStopsGloballyWithoutCancellingForeignExposure(t *testing.T) {
	for _, ticker := range []string{seamTicker, "OTHER-MARKET"} {
		t.Run(ticker, func(t *testing.T) {
			h := newSeamHarness(t, seamOptions{ListCreated: true,
				Positions: map[string]string{seamTicker: "1.00"}})
			h.start()
			h.awaitActionable()
			h.await("our reducer resting", func() bool { return h.ex.restingCount() > 0 })
			if got := h.latchTrigger(); got != "" {
				t.Fatalf("stopped before foreign activity: %s", got)
			}
			h.ex.mu.Lock()
			h.ex.resting = append(h.ex.resting, map[string]any{
				"order_id": "foreign-live", "client_order_id": "manual-order",
				"ticker": ticker, "book_side": "bid", "status": "resting",
				"yes_price_dollars": "0.4000", "no_price_dollars": "0.6000",
				"remaining_count_fp": "1.00", "fill_count_fp": "0.00",
				"initial_count_fp": "1.00",
			})
			h.ex.mu.Unlock()
			h.clk.Advance(h.cfg.Params.PositionPoll)
			h.await("foreign activity durable global stop", func() bool { return h.latchTrigger() == "foreign_order" })
			h.awaitGlobalEvent("RUNNING->WINDING_DOWN/global_stop")
			h.awaitTicks(3)
			if h.snapshot().Global != quote.WindingDown {
				t.Fatalf("global=%v", h.snapshot().Global)
			}
			h.await("foreign-order anomaly", func() bool { return len(h.anomalySevs("FOREIGN_ORDER")) > 0 })
			for _, sev := range h.anomalySevs("FOREIGN_ORDER") {
				if sev != risk.SEV2 {
					t.Fatalf("F15 severity=%v", sev)
				}
			}
			// The global stop leaves the account monitor running while our
			// reducer manages the existing position.
			walks := h.ex.ordersWalkCount()
			h.clk.Advance(h.cfg.Params.PositionPoll)
			h.await("orders monitor after global stop", func() bool {
				return h.ex.ordersWalkCount() > walks
			})
			m, ok := h.market()
			if !ok || m.State != quote.Reducing || m.Q == 0 {
				t.Fatalf("reducer state lost: %+v", m)
			}
			for _, id := range h.ex.deletedIDs() {
				if id == "foreign-live" {
					t.Fatal("cancelled another trader's order")
				}
			}
		})
	}
}
