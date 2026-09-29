package main

import (
	"context"
	"fmt"
	"time"

	"lip/harness/num"
	"lip/harness/quote"
	"lip/harness/rest"
	"lip/harness/turnover"
	"lip/harness/wsx"
)

// The production owner uses the tested turnover planner for the final account-
// wide union and frozen caps. The existing hstore reservation-before-dispatch
// barrier remains the durable source; startup reconstructs from that ledger and
// complete exchange truth, never a second independently committed ownership DB.
func (o *owner) buildTurnoverPlan() (*turnover.Plan, error) {
	now := o.r.ex.Mono()
	proof := func(k wsx.Truth) turnover.Proof {
		age := o.r.gate.TruthAge(k, o.r.ex.Clock.Now())
		return turnover.Proof{Complete: age >= 0 && age <= o.p.TruthMaxAge, At: now - age}
	}
	in := turnover.Input{Now: now, MaxAge: o.p.TruthMaxAge, Selected: sortedSelection(o.selected), Positions: o.r.pf.Positions(),
		PositionsProof: proof(wsx.TruthPositions), OrdersProof: proof(wsx.TruthOrders), OwnershipProof: proof(wsx.TruthOrders),
		PendingProof: turnover.Proof{Complete: true, At: now}, AggregateCap: o.p.CapitalMax, Previous: o.turnoverPlan,
		FrozenCaps: map[turnover.Shard]num.Money{}, Cash: map[turnover.Shard]turnover.Cash{}}
	if o.r.funding == nil {
		return nil, fmt.Errorf("turnover funding publisher absent")
	}
	funding := o.r.funding.Load()
	if funding == nil || !funding.valid {
		return nil, fmt.Errorf("turnover funding unavailable")
	}
	for _, basis := range o.r.cfg.ShardFunding {
		in.FrozenCaps[turnover.Shard{ExchangeIndex: basis.ExchangeIndex, Subaccount: basis.Subaccount}] = basis.Effective
	}
	for t, index := range funding.byTicker {
		in.Identities = append(in.Identities, turnover.Identity{Ticker: t, Shard: turnover.Shard{ExchangeIndex: index}, Proof: turnover.Proof{Complete: true, At: funding.at}})
	}
	for index, cash := range funding.byShard {
		in.Cash[turnover.Shard{ExchangeIndex: index}] = turnover.Cash{Available: cash, Proof: turnover.Proof{Complete: true, At: funding.at}}
	}
	orders := map[string]turnover.Order{}
	add := func(coid, t string, side quote.Side, qty num.Qty, price4 int64) error {
		if coid == "" {
			return fmt.Errorf("owned obligation on %s lacks durable identity", t)
		}
		if qty <= 0 {
			return nil
		}
		row := turnover.Order{ID: coid, Ticker: t, Side: side, Quantity: qty, Price4: price4, Ownership: turnover.Owned}
		if old, ok := orders[coid]; ok {
			if old.Ticker != t || old.Side != side || old.Price4 != price4 {
				return fmt.Errorf("owned identity conflicts")
			}
			if old.Quantity > row.Quantity {
				row.Quantity = old.Quantity
			}
		}
		orders[coid] = row
		return nil
	}
	for _, lo := range o.r.pf.LiveOrders() {
		if _, absent := o.absentOrders[lo.OrderID]; absent {
			continue
		}
		coid := o.listedBindings[lo.OrderID]
		if coid == "" {
			coid, _ = o.r.store.Ownership().Bound(lo.OrderID)
		}
		if err := add(coid, lo.Ticker, lo.Side, lo.Remaining, lo.Price4); err != nil {
			return nil, err
		}
	}
	for coid, p := range o.pending {
		if o.hasListedCreate(coid, o.pendingTicker(p), p.side) {
			continue
		}
		if err := add(coid, o.pendingTicker(p), p.side, p.qty, num.Price4FromCents(p.cents)); err != nil {
			return nil, err
		}
	}
	for _, req := range o.inflight {
		if req.Op != quote.OpPlace || o.hasListedCreate(req.Order.ClientOrderID(), req.Market, req.Side) {
			continue
		}
		if err := add(req.Order.ClientOrderID(), req.Market, req.Side, req.Order.Count(), num.Price4FromCents(req.Order.PriceCents())); err != nil {
			return nil, err
		}
	}
	for _, row := range orders {
		in.Orders = append(in.Orders, row)
	}
	if o.turnoverPlan != nil {
		for _, prior := range o.turnoverPlan.Owned {
			if _, live := orders[prior.ID]; !live {
				if at, ok := o.turnoverResolved[prior.ID]; ok {
					in.Resolved = append(in.Resolved, turnover.Resolution{ID: prior.ID, Proof: turnover.Proof{Complete: true, At: at}})
				}
			}
		}
	}
	return turnover.Build(in)
}
func (o *owner) updateTurnoverPlan() {
	if !o.r.cfg.Turnover {
		return
	}
	plan, err := o.buildTurnoverPlan()
	o.turnoverAccountingReady = err == nil
	if err != nil {
		return
	}
	o.turnoverPlan = plan
	for _, m := range plan.Markets {
		o.addManaged(m.Ticker)
	}
}

// Listed owned identities remain in the owner's obligation map across complete
// resting-walk omissions. They are counted only once, and leave only on a named
// confirmed cancel or a positive terminal all-status observation.
func (o *owner) rememberTurnoverOrders() {
	if !o.r.cfg.Turnover {
		return
	}
	for _, lo := range o.r.pf.LiveOrders() {
		if _, absent := o.absentOrders[lo.OrderID]; absent {
			continue
		}
		coid := o.listedBindings[lo.OrderID]
		if coid == "" {
			coid, _ = o.r.store.Ownership().Bound(lo.OrderID)
		}
		cents, exact := rest.CentsExact(lo.Price4)
		if coid == "" || !exact {
			o.requestStop("turnover_order_identity", lo.Ticker)
			continue
		}
		o.pending[coid] = pendingOrder{ticker: lo.Ticker, side: lo.Side, cents: cents, qty: lo.Remaining, at: o.r.ex.Mono(), acked: true, id: lo.OrderID}
	}
}
func (o *owner) resolveTurnoverOrder(coid string) {
	if !o.r.cfg.Turnover {
		return
	}
	if o.turnoverResolved == nil {
		o.turnoverResolved = map[string]time.Duration{}
	}
	o.turnoverResolved[coid] = o.r.ex.Mono()
	delete(o.pending, coid)
}

type turnoverTerminalRead struct {
	orders   rest.OrdersResult
	started  time.Duration
	revision uint64
}

// This loop resolves positive terminals only. An empty complete all-status walk
// cannot release a possible order. It never issues cancellation or placement.
func (r *rig) turnoverTerminalLoop(ctx context.Context, out chan<- turnoverTerminalRead) {
	if !r.cfg.Turnover {
		return
	}
	timer := time.NewTicker(r.cfg.Params.PositionPoll)
	defer timer.Stop()
	for {
		started := r.ex.Mono()
		// No owner state is read here; the revision is not a book authority. Timing
		// plus immutable identity validation below protects a terminal observation.
		readCtx, cancel := context.WithTimeout(ctx, r.cfg.Params.TruthMaxAge)
		orders := r.api.Orders(readCtx, "", "")
		cancel()
		select {
		case out <- turnoverTerminalRead{orders: orders, started: started}:
		case <-ctx.Done():
			return
		}
		select {
		case <-timer.C:
		case <-ctx.Done():
			return
		}
	}
}
func (o *owner) applyTurnoverTerminals(read turnoverTerminalRead) {
	if !o.r.cfg.Turnover {
		return
	}
	o.noteRESTError(read.orders.Err)
	now := o.r.ex.Mono()
	if !read.orders.Replaces() || read.started > now || now-read.started > o.p.TruthMaxAge {
		return
	}
	resolved := false
	for _, ord := range read.orders.Orders {
		if ord.Status != rest.StatusExecuted && ord.Status != rest.StatusCanceled {
			continue
		}
		p, ok := o.pending[ord.ClientOrderID]
		if !ok || read.started < p.at || ord.OrderID == "" || (p.id != "" && p.id != ord.OrderID) || o.pendingTicker(p) != ord.Ticker || p.side != ord.Side || num.Price4FromCents(p.cents) != ord.Price4 {
			continue
		}
		// Wait for the worker result before retiring this coid: a delayed ACK
		// must not resurrect an already terminal obligation or replay its fill.
		if o.turnoverCreateInFlight(ord.ClientOrderID) {
			continue
		}
		// Only an exact reservation identity above may bind an unbound terminal.
		// Binding is asynchronous; retain the obligation until it commits.
		if bound, ok := o.r.store.Ownership().Bound(ord.OrderID); !ok || bound != ord.ClientOrderID {
			if !ok {
				_ = o.r.store.BindListedOrder(ord.ClientOrderID, ord.OrderID, o.r.ex.NowMs())
			}
			continue
		}
		o.resolveTurnoverOrder(ord.ClientOrderID)
		o.absentOrders[ord.OrderID] = struct{}{}
		resolved = true
	}
	if resolved {
		o.awaitCancelTruth = true
		o.cancelTruthAfter = now
		o.offerToken(o.reconcileOut, o.reconcileToken)
	}
}

func (o *owner) turnoverCreateInFlight(coid string) bool {
	for _, req := range o.inflight {
		if req.Op == quote.OpPlace && req.Order.ClientOrderID() == coid {
			return true
		}
	}
	return false
}

// confidence: low

// Prefix-shaped orders are not ownership evidence. A verifying sweep may learn
// a previously unknown exchange ID only for an existing immutable reservation.
func (o *owner) acceptTurnoverOwnedSighting(ord rest.Order) bool {
	if coid, ok := o.r.store.Ownership().Bound(ord.OrderID); ok {
		return ord.ClientOrderID == "" || ord.ClientOrderID == coid
	}
	p, ok := o.pending[ord.ClientOrderID]
	if !ok || o.pendingTicker(p) != ord.Ticker || p.side != ord.Side || num.Price4FromCents(p.cents) != ord.Price4 || (p.id != "" && p.id != ord.OrderID) {
		return false
	}
	if err := o.r.store.BindListedOrder(ord.ClientOrderID, ord.OrderID, o.r.ex.NowMs()); err != nil {
		return false
	}
	p.id = ord.OrderID
	p.acked = true
	o.pending[ord.ClientOrderID] = p
	return true
}

// confidence: low
