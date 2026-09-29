package main

import (
	"lip/harness/quote"
	"lip/harness/wsx"
)

// A complete orders walk can beat the create worker's result. Preserve its
// positive coid identity even while the durable binding is still committing,
// so the same order does not appear twice as RESTING and SENDING/UNKNOWN.
// This map is accounting evidence only; it never grants a write permit.
func (o *owner) noteListedBindings(bindings []wsx.Binding) {
	next := make(map[string]string)
	for _, order := range o.r.pf.LiveOrders() {
		if coid := o.listedBindings[order.OrderID]; coid != "" {
			next[order.OrderID] = coid
		}
		if coid, ok := o.r.store.Ownership().Bound(order.OrderID); ok {
			next[order.OrderID] = coid
		}
	}
	for _, binding := range bindings {
		next[binding.OrderID] = binding.Coid
	}
	o.listedBindings = next
}

func (o *owner) hasListedCreate(coid, market string, side quote.Side) bool {
	if coid == "" {
		return false
	}
	for _, order := range o.r.pf.LiveOrders() {
		if order.Ticker != market || order.Side != side {
			continue
		}
		if _, absent := o.absentOrders[order.OrderID]; absent {
			continue
		}
		bound := o.listedBindings[order.OrderID]
		if bound == "" {
			bound, _ = o.r.store.Ownership().Bound(order.OrderID)
		}
		if bound == coid {
			return true
		}
	}
	return false
}

// confidence: high
