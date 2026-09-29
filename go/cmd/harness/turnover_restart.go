package main

import (
	"context"
	"fmt"
	"time"

	"lip/harness/hstore"
	"lip/harness/num"
	"lip/harness/quote"
	"lip/harness/rest"
)

// readTurnoverInherited reconstructs every durable possible obligation before
// the owner starts. A complete omission cannot retire a bound or unbound order.
// Only a positive, identity-matching terminal can remove the reservation's
// maximum quantity; the later lifecycle walk supplies authoritative holdings.
func readTurnoverInherited(ctx context.Context, store *hstore.Store, api *rest.Client) ([]hstore.OwnedOrderRow, error) {
	if store == nil {
		return nil, fmt.Errorf("turnover inheritance has no durable store")
	}
	rows, err := store.Reader().OwnedOrders()
	if err != nil {
		return nil, fmt.Errorf("turnover inherited reservations: %w", err)
	}
	active := make([]hstore.OwnedOrderRow, 0, len(rows))
	byCoid := map[string]int{}
	byOrder := map[string]int{}
	for _, row := range rows {
		if row.Abandoned {
			continue
		}
		if row.Coid == "" || row.Ticker == "" || row.Count <= 0 || row.PriceCents <= 0 || row.PriceCents >= 100 ||
			(row.Side != quote.SideYes.String() && row.Side != quote.SideNo.String()) || row.Bound != (row.OrderID != "") {
			return nil, fmt.Errorf("turnover inherited reservation %q has invalid identity", row.Coid)
		}
		if _, duplicate := byCoid[row.Coid]; duplicate {
			return nil, fmt.Errorf("turnover inherited coid %q is duplicated", row.Coid)
		}
		if row.Bound {
			if _, duplicate := byOrder[row.OrderID]; duplicate {
				return nil, fmt.Errorf("turnover inherited order %q is duplicated", row.OrderID)
			}
			byOrder[row.OrderID] = len(active)
		}
		byCoid[row.Coid] = len(active)
		active = append(active, row)
	}
	if len(active) == 0 {
		return nil, nil
	}
	if api == nil {
		return nil, fmt.Errorf("turnover inheritance has no account reader")
	}
	walk := api.Orders(ctx, "", "")
	if !walk.Replaces() {
		return nil, fmt.Errorf("turnover inherited all-status orders incomplete: %s: %v", walk.Outcome, walk.Err)
	}
	seen := map[int]rest.Order{}
	seenOrder := map[string]bool{}
	terminal := map[int]bool{}
	for _, order := range walk.Orders {
		if seenOrder[order.OrderID] {
			return nil, fmt.Errorf("turnover inherited order ID %q appears more than once", order.OrderID)
		}
		seenOrder[order.OrderID] = true
		i, matchesCoid := byCoid[order.ClientOrderID]
		j, matchesOrder := byOrder[order.OrderID]
		if !matchesCoid && !matchesOrder {
			continue // Foreign identities never grant an ownership or cancel right.
		}
		if !matchesCoid || (matchesOrder && i != j) {
			return nil, fmt.Errorf("turnover inherited order %q conflicts with durable coid", order.OrderID)
		}
		row := active[i]
		if _, duplicate := seen[i]; duplicate {
			return nil, fmt.Errorf("turnover inherited coid %q appears more than once", row.Coid)
		}
		if order.OrderID == "" || (row.Bound && row.OrderID != order.OrderID) || row.Ticker != order.Ticker ||
			row.Side != order.Side.String() || num.Price4FromCents(row.PriceCents) != order.Price4 ||
			order.Remaining < 0 || order.Remaining > row.Count {
			return nil, fmt.Errorf("turnover inherited coid %q conflicts with durable order body", row.Coid)
		}
		switch order.Status {
		case rest.StatusCanceled, rest.StatusExecuted:
			if order.Remaining != 0 {
				return nil, fmt.Errorf("turnover inherited terminal %q still reports remaining quantity", row.Coid)
			}
			terminal[i] = true
		case rest.StatusResting:
			if order.Remaining == 0 {
				return nil, fmt.Errorf("turnover inherited resting order %q has zero quantity", row.Coid)
			}
		default:
			return nil, fmt.Errorf("turnover inherited order %q has unsupported status %q", row.Coid, order.Status)
		}
		seen[i] = order
	}
	// Validate the whole observation before submitting any binding. Submission
	// is not durability: lifecycle must still wait for the unresolved index to
	// clear before it can classify fills and finish startup.
	for i, order := range seen {
		if !active[i].Bound {
			if err := store.BindListedOrder(active[i].Coid, order.OrderID, time.Now().UnixMilli()); err != nil {
				return nil, fmt.Errorf("turnover inherited binding %q: %w", active[i].Coid, err)
			}
		}
	}
	inherited := make([]hstore.OwnedOrderRow, 0, len(active))
	for i, row := range active {
		if !terminal[i] {
			// Preserve the reservation maximum even for a smaller resting read;
			// authoritative live adoption subsequently replaces that quantity.
			inherited = append(inherited, row)
		}
	}
	return inherited, nil
}

// CR2 has no omission-based abandonment. A reservation may have been sent
// before its process died, so repeated absent walks still retain the obligation.
// BindListedOrder and UnresolvedReservations are promoted from the real store.
type turnoverReservationResolver struct{ *hstore.Store }

func (turnoverReservationResolver) AbandonListedReservation(coid string, _ int64) error {
	return fmt.Errorf("turnover reservation %q has no positive terminal evidence; omission cannot abandon it", coid)
}

// confidence: low
