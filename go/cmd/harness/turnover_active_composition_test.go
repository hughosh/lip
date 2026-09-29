package main

import (
	"fmt"
	"testing"
	"time"

	"lip/harness/num"
	"lip/harness/quote"
	"lip/harness/rest"
	"lip/harness/risk"
)

// This scenario begins with an empty fake account so neither market's funding
// is recovery-only. It must actually dispatch adding orders on A and then B;
// merely changing a selected flag cannot satisfy it.
func TestComposedTurnoverDispatchesNewMarketWhileReducingOwnedOldFill(t *testing.T) {
	h := newSeamHarness(t, seamOptions{SkipRig: true, ListCreated: true})
	h.cfg.Turnover = true
	h.cfg.Candidates = []string{seamTicker, turnoverOtherTicker}
	h.cfg.CapitalSource = "selected_shard_balance"
	h.cfg.Params.NMarkets = 1
	h.cfg.Params.S = num.QtyFromFloat(1)
	h.cfg.Params.SMax = num.QtyFromFloat(1)
	h.cfg.Params.SchedulePoll = 100 * time.Millisecond
	h.cfg.Params.Reselect = 100 * time.Millisecond
	h.cfg.Params.PositionPoll = 100 * time.Millisecond
	x := &turnoverCompositionExchange{base: h.ex}
	h.xch.Doer = x
	var err error
	h.cfg, err = resolveFundingCap(h.ctx, h.cfg, x)
	if err != nil {
		t.Fatalf("flat turnover funding preflight: %v", err)
	}
	for ticker, basis := range h.cfg.ShardFunding {
		if basis.RecoveryOnly {
			t.Fatalf("flat-account fixture unexpectedly restricted %s to recovery", ticker)
		}
	}
	<-h.ws.frames
	h.anom = newAnomalySink()
	h.rig, err = newRig(h.ctx, h.cfg, false, h.xch, seamAlertFactory, h.anom, nil)
	if err != nil {
		t.Fatalf("active turnover composition: %v", err)
	}
	t.Cleanup(h.closeRig)
	h.start()
	feedAwait, _ := startTurnoverCompositionFeed(t, h, x)
	await := feedAwait
	var filled seamCreate
	await("A selected with two actually dispatched adding sides", func() bool {
		s := h.snapshot()
		a, ok := turnoverMarket(s, seamTicker)
		if s == nil || s.Global != quote.Running || !ok || !a.Selected || a.Q != 0 || a.State != quote.Quoting {
			return false
		}
		seen := map[string]bool{}
		for _, c := range h.ex.allCreates() {
			if c.Ticker != seamTicker || c.Count != "1.00" {
				continue
			}
			row, exists, err := h.rig.store.Reader().OwnedOrder(c.Coid)
			if err != nil || !exists || !row.Bound || row.OrderID != c.OrderID || row.Role != quote.RoleAdding.String() {
				continue
			}
			seen[c.WireSide] = true
			if c.WireSide == string(rest.Bid) {
				filled = c
			}
		}
		return seen[string(rest.Bid)] && seen[string(rest.Ask)] && h.ex.restingCount() == 2
	})
	for _, c := range h.ex.allCreates() {
		if c.Ticker == turnoverOtherTicker {
			t.Fatalf("B dispatched before its program was selected: %+v", c)
		}
	}

	// Inject exchange truth only. The coid and order ID were minted, reserved,
	// dispatched and bound by the production writer above, not seeded by this
	// test. Account reads must attribute this partial YES fill and its fee.
	const tradeID = "SEAM-ACTIVE-TURNOVER-OWNED-FILL"
	yes4, err := rest.ParsePrice4(filled.Price)
	if err != nil {
		t.Fatalf("filled adding price: %v", err)
	}
	no4 := 10000 - yes4
	fill := seamMakerFill(tradeID, filled.OrderID, "yes", filled.Price,
		fmt.Sprintf("%d.%04d", no4/10000, no4%10000), "0.25", h.clk.wallMs())
	fill["client_order_id"] = filled.Coid
	fill["fee_cost"] = "0.0000"
	h.ex.mu.Lock()
	changed := false
	for i, order := range h.ex.resting {
		if order["order_id"] != filled.OrderID || order["client_order_id"] != filled.Coid {
			continue
		}
		remainder := make(map[string]any, len(order))
		for key, value := range order {
			remainder[key] = value
		}
		remainder["remaining_count"] = "0.75"
		remainder["fill_count"] = "0.25"
		h.ex.resting[i] = remainder
		changed = true
	}
	if changed {
		h.ex.positions[seamTicker] = "0.25"
		h.ex.fills = append(h.ex.fills, fill)
	}
	h.ex.mu.Unlock()
	if !changed {
		t.Fatal("the production adding order disappeared before the simulated execution")
	}
	x.setPhase(1)
	h.clk.Advance(h.cfg.Params.PositionPoll)
	await("owned partial fill attributed before old-market reduction", func() bool {
		row, present, err := h.rig.store.Reader().Fill(tradeID)
		a, ok := turnoverMarket(h.snapshot(), seamTicker)
		return err == nil && present && row.OrderID == filled.OrderID &&
			row.Ticker == seamTicker && row.Count == num.QtyFromFloat(.25) &&
			row.Fee == 0 && ok && a.Q == num.QtyFromFloat(.25)
	})
	await("B adds while A is retained as a bounded reducer", func() bool {
		s := h.snapshot()
		a, aOK := turnoverMarket(s, seamTicker)
		b, bOK := turnoverMarket(s, turnoverOtherTicker)
		if s == nil || s.Global != quote.Running || !aOK || !bOK || a.Selected || !b.Selected ||
			a.Q != num.QtyFromFloat(.25) || a.State != quote.Reducing || !a.BookActionable ||
			b.Q != 0 || b.State != quote.Quoting || !b.BookActionable ||
			a.Sides[quote.SideYes].AtRisk != 0 || a.Sides[quote.SideNo].AtRisk != num.QtyFromFloat(.25) {
			return false
		}
		bSides := map[string]bool{}
		reducer := false
		for _, c := range h.ex.allCreates() {
			row, present, err := h.rig.store.Reader().OwnedOrder(c.Coid)
			if err != nil || !present || !row.Bound || row.OrderID != c.OrderID {
				continue
			}
			if c.Ticker == turnoverOtherTicker && c.Count == "1.00" && row.Role == quote.RoleAdding.String() {
				bSides[c.WireSide] = true
			}
			if c.Ticker == seamTicker && c.WireSide == string(rest.Ask) && c.Count == "0.25" && row.Role == quote.RoleReducing.String() {
				reducer = true
			}
		}
		return reducer && bSides[string(rest.Bid)] && bSides[string(rest.Ask)] &&
			seamContains(h.ex.deletedIDs(), filled.OrderID)
	})
	createsAfterRotation := h.ex.createCount()
	before := h.snapSeq()
	await("monitor publishes both active B and retained A", func() bool {
		last := h.rig.last.Load()
		return h.snapSeq() > before && last != nil && !last.Stale && len(last.Samples) == 2 &&
			last.Samples[0].SourceSeq >= before && last.Account.TruthFresh && last.Account.TradingPnL.Evaluable &&
			len(last.Account.Exposures) == 2 && risk.Deployed(last.Account.Exposures) > 0 && risk.Deployed(last.Account.Exposures) <= last.Account.CapitalMax
	})
	for _, c := range h.ex.allCreates()[createsAfterRotation:] {
		if c.Ticker != seamTicker {
			continue
		}
		qty, err := num.ParseQty(c.Count)
		if err != nil || c.WireSide != string(rest.Ask) || qty > num.QtyFromFloat(.25) {
			t.Fatalf("deselected A dispatched adding or oversized risk: %+v err=%v", c, err)
		}
	}
	if got := h.latchTrigger(); got != "" {
		t.Fatalf("normal active turnover stopped the account: %s", got)
	}

	// Another trader's order appears only after active turnover was proved.
	// Its presence must stop account adding without conferring cancellation
	// authority, while our old A inventory remains managed.
	const foreignID = "foreign-active-turnover"
	h.ex.mu.Lock()
	h.ex.resting = append(h.ex.resting, map[string]any{
		"order_id": foreignID, "client_order_id": "manual-active-turnover",
		"ticker": turnoverOtherTicker, "side": "yes", "status": rest.StatusResting,
		"yes_price_dollars": "0.4000", "remaining_count": "1.00",
	})
	h.ex.mu.Unlock()
	h.clk.Advance(h.cfg.Params.PositionPoll)
	await("foreign activity stops adding but preserves A reduction", func() bool {
		s := h.snapshot()
		a, ok := turnoverMarket(s, seamTicker)
		return h.latchTrigger() == "foreign_order" && s != nil && s.Global == quote.WindingDown &&
			ok && a.Q == num.QtyFromFloat(.25) && a.State == quote.Reducing
	})
	await("owned B adding orders cancelled after the foreign stop", func() bool {
		b, ok := turnoverMarket(h.snapshot(), turnoverOtherTicker)
		return ok && b.Sides[0].AtRisk == 0 && b.Sides[1].AtRisk == 0
	})
	if seamContains(h.ex.deletedIDs(), foreignID) {
		t.Fatal("active turnover cancelled a foreign order")
	}
}

// confidence: low
