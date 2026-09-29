package main

import (
	"fmt"

	"lip/harness/num"
	"lip/harness/quote"
	"lip/harness/risk"
)

// checkPlacementCapital runs on the owner after the final price and count are
// known, before handoff. A successful handoff enters inflight before the owner
// selects another request, so every worker spends the same remaining budget.
func (o *owner) checkPlacementCapital(req writeRequest) error {
	if req.Op != quote.OpPlace {
		return nil
	}
	if o.r.cfg.Turnover {
		if req.Market != req.Order.Ticker() || req.Side != req.Order.Side() {
			return fmt.Errorf("turnover dispatch identity mismatch")
		}
		plan, err := o.buildTurnoverPlan()
		if err != nil {
			return fmt.Errorf("turnover dispatch accounting: %w", err)
		}
		if req.Role == quote.RoleAdding && (plan.AddingStopped || !o.isSelected(req.Market)) {
			return fmt.Errorf("turnover adding authority unavailable")
		}
	}
	ex := o.exposures()
	// Retrying an UNKNOWN coid cannot create a second order. Remove only its
	// existing reservation, then test its complete body against the budget.
	var reserved num.Money
	if pending, ok := o.pending[req.Order.ClientOrderID()]; ok &&
		!o.hasListedCreate(req.Order.ClientOrderID(), req.Market, req.Side) &&
		req.Market == o.pendingTicker(pending) && pending.side == req.Side &&
		pending.cents == req.Order.PriceCents() && pending.qty == req.Order.Count() {
		reserved = risk.SideCost(pending.qty, num.Price4FromCents(pending.cents))
	}
	if reserved > 0 {
		for i := range ex {
			if ex[i].Ticker != req.Market {
				continue
			}
			reducing, held := quote.ReducingSide(o.r.pf.Q(req.Market))
			if held && req.Side == reducing {
				ex[i].Reducing -= reserved
			} else {
				ex[i].Adding -= reserved
			}
		}
	}
	var budget num.Money
	if req.Role == quote.RoleReducing {
		budget, _ = risk.ReducingBudget(ex, o.p)
	} else {
		params := o.p
		if o.r.cfg.Turnover && o.r.cfg.CapitalCeiling != nil {
			params.CapitalMax = min(params.CapitalMax, *o.r.cfg.CapitalCeiling)
		}
		budget = risk.AddingBudget(req.Market, ex, o.capitalReducerNeed(), params)
	}
	cost := risk.SideCost(req.Order.Count(), num.Price4FromCents(req.Order.PriceCents()))
	if err := o.checkObservedFunding(req, ex, cost); err != nil {
		return err
	}
	if cost > budget {
		return fmt.Errorf("placement capital: %s requires %s, available %s", req.Role, cost, budget)
	}
	return nil
}

// capitalReducerNeed withholds capital for held contracts that do not yet have
// a reducing order. Existing reducers already consume exposure; counting their
// quantity here again would prevent adding even after the exit is fully funded.
func (o *owner) capitalReducerNeed() num.Money {
	var need num.Money
	for ticker, q := range o.r.pf.Positions() {
		side, held := quote.ReducingSide(q)
		if !held {
			continue
		}
		var working num.Qty
		price4 := int64(risk.SettlementPrice4)
		if ticker == o.ticker() || o.r.cfg.Turnover && o.manages(ticker) {
			restore := o.marketContext(ticker)
			working = o.atRisk(side)
			if price, ok := o.f5TargetPrice(side, quote.RoleReducing); ok {
				price4 = num.Price4FromCents(price)
			}
			restore()
		} else {
			// This pilot owns one ticker. Other known positions still take
			// global priority; without their book, reserve settlement value.
			for _, lo := range o.r.pf.LiveOrders() {
				if lo.Ticker == ticker && lo.Side == side {
					if _, absent := o.absentOrders[lo.OrderID]; !absent {
						working += lo.Remaining
					}
				}
			}
		}
		if remaining := q.Abs() - working; remaining > 0 {
			need += risk.SideCost(remaining, price4)
		}
	}
	return need
}

// confidence: high
