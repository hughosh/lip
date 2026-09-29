package main

import (
	"context"
	"encoding/json"
	"testing"

	"lip/harness/hstore"
	"lip/harness/num"
	"lip/harness/quote"
	"lip/harness/rest"
)

type turnoverInheritedScript struct {
	t      *testing.T
	rows   []map[string]any
	status int
	calls  int
}

func (s *turnoverInheritedScript) Do(_ context.Context, req rest.Request) (rest.Response, error) {
	s.calls++
	if req.Method != "GET" || req.Path != rest.EpOrders.Path || req.Query.Get("ticker") != "" || req.Query.Get("status") != "" {
		s.t.Fatalf("inheritance did not use the complete read-only all-status scope: %+v", req)
	}
	status := s.status
	if status == 0 {
		status = 200
	}
	body, err := json.Marshal(map[string]any{"cursor": "", "orders": s.rows})
	if err != nil {
		s.t.Fatal(err)
	}
	return rest.Response{Status: status, Body: body}, nil
}

func inheritedReservation(t *testing.T, bound bool) (*rig, hstore.OwnedOrderRow, *turnoverInheritedScript) {
	t.Helper()
	script := &turnoverInheritedScript{t: t, rows: []map[string]any{}}
	r, _ := newDispatchRig(t, script)
	req := placement(t, r, quote.SideYes, quote.RoleAdding, 40, 901)
	if _, err := r.store.ReserveOrder(r.run, req.Order, req.Role, r.ex.NowMs()); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the inherited reservation to commit", func() bool {
		return r.store.Ownership().UnresolvedCount() == 1
	})
	if bound {
		if err := r.store.BindListedOrder(req.Order.ClientOrderID(), "old-owned", r.ex.NowMs()); err != nil {
			t.Fatal(err)
		}
		waitFor(t, "the inherited binding to commit", func() bool {
			_, ok := r.store.Ownership().Bound("old-owned")
			return ok
		})
	}
	row, ok, err := r.store.Reader().OwnedOrder(req.Order.ClientOrderID())
	if err != nil || !ok {
		t.Fatalf("durable reservation missing: ok=%t err=%v", ok, err)
	}
	return r, row, script
}

func inheritedWire(row hstore.OwnedOrderRow, status, remaining string) map[string]any {
	return map[string]any{
		"order_id": "old-owned", "client_order_id": row.Coid,
		"ticker": row.Ticker, "side": row.Side, "yes_price_dollars": "0.4000",
		"remaining_count_fp": remaining, "status": status,
	}
}

func TestTurnoverInheritedOmissionRetainsBoundAndUnboundMaximum(t *testing.T) {
	for _, bound := range []bool{false, true} {
		t.Run(map[bool]string{false: "unbound", true: "bound"}[bound], func(t *testing.T) {
			r, row, _ := inheritedReservation(t, bound)
			got, err := readTurnoverInherited(context.Background(), r.store, r.api)
			if err != nil || len(got) != 1 || got[0] != row {
				t.Fatalf("complete omission changed possible obligation: got=%+v err=%v", got, err)
			}
		})
	}
}

func TestTurnoverInheritedPositiveTerminalsBindAndRetireOnlyMatchingIdentity(t *testing.T) {
	for _, status := range []string{rest.StatusExecuted, rest.StatusCanceled} {
		t.Run(status, func(t *testing.T) {
			r, row, script := inheritedReservation(t, false)
			script.rows = []map[string]any{inheritedWire(row, status, "0.00")}
			got, err := readTurnoverInherited(context.Background(), r.store, r.api)
			if err != nil || len(got) != 0 {
				t.Fatalf("matching terminal did not release inherited maximum: %+v %v", got, err)
			}
			waitFor(t, "the positive terminal binding to commit", func() bool {
				coid, ok := r.store.Ownership().Bound("old-owned")
				return ok && coid == row.Coid
			})
		})
	}
}

func TestTurnoverInheritedRestingReadRetainsReservationMaximum(t *testing.T) {
	r, row, script := inheritedReservation(t, true)
	script.rows = []map[string]any{inheritedWire(row, rest.StatusResting, "0.25")}
	got, err := readTurnoverInherited(context.Background(), r.store, r.api)
	if err != nil || len(got) != 1 || got[0].Count != num.QtyFromFloat(1) {
		t.Fatalf("resting observation prematurely shrank reserved maximum: %+v %v", got, err)
	}
}

func TestTurnoverInheritedIdentityConflictFailsBeforeAnyRelease(t *testing.T) {
	cases := map[string]func(map[string]any){
		"coid":     func(m map[string]any) { m["client_order_id"] = "foreign" },
		"order_id": func(m map[string]any) { m["order_id"] = "other-order" },
		"ticker":   func(m map[string]any) { m["ticker"] = "OTHER" },
		"side":     func(m map[string]any) { m["side"] = "no" },
		"price":    func(m map[string]any) { m["yes_price_dollars"] = "0.4100" },
		"oversize": func(m map[string]any) { m["remaining_count_fp"] = "1.01" },
		"negative": func(m map[string]any) { m["remaining_count_fp"] = "-0.01" },
		"nonzero":  func(m map[string]any) { m["remaining_count_fp"] = "0.01" },
		"status":   func(m map[string]any) { m["status"] = "unknown" },
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			r, row, script := inheritedReservation(t, true)
			wire := inheritedWire(row, rest.StatusExecuted, "0.00")
			change(wire)
			script.rows = []map[string]any{wire}
			if got, err := readTurnoverInherited(context.Background(), r.store, r.api); err == nil || got != nil {
				t.Fatalf("conflicting %s authorized inheritance: %+v %v", name, got, err)
			}
		})
	}
}

func TestTurnoverInheritedIncompleteWalkAndDuplicateCannotRelease(t *testing.T) {
	for _, duplicate := range []bool{false, true} {
		t.Run(map[bool]string{false: "failed_walk", true: "duplicate"}[duplicate], func(t *testing.T) {
			r, row, script := inheritedReservation(t, false)
			wire := inheritedWire(row, rest.StatusExecuted, "0.00")
			script.rows = []map[string]any{wire}
			if duplicate {
				script.rows = append(script.rows, wire)
			} else {
				script.status = 500
			}
			if _, err := readTurnoverInherited(context.Background(), r.store, r.api); err == nil {
				t.Fatal("incomplete or duplicate evidence released a reservation")
			}
			if _, bound := r.store.Ownership().Bound("old-owned"); bound {
				t.Fatal("failed observation submitted a binding")
			}
		})
	}
}

func TestTurnoverInheritedIgnoresForeignAndAbandonedRows(t *testing.T) {
	r, row, script := inheritedReservation(t, false)
	foreign := inheritedWire(row, rest.StatusExecuted, "0.00")
	foreign["client_order_id"] = "foreign"
	foreign["order_id"] = "foreign-order"
	script.rows = []map[string]any{foreign}
	got, err := readTurnoverInherited(context.Background(), r.store, r.api)
	if err != nil || len(got) != 1 || got[0] != row {
		t.Fatalf("foreign order changed durable ownership: %+v %v", got, err)
	}
	if _, err := r.store.ResolveReservationAbandoned(row.Coid, r.ex.NowMs()); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the test abandonment to commit", func() bool {
		return r.store.Ownership().UnresolvedCount() == 0
	})
	calls := script.calls
	got, err = readTurnoverInherited(context.Background(), r.store, nil)
	if err != nil || len(got) != 0 || script.calls != calls {
		t.Fatalf("abandoned-only store needed account access: %+v %v", got, err)
	}
}

func TestTurnoverResolverRefusesOmissionAbandonment(t *testing.T) {
	r, row, _ := inheritedReservation(t, false)
	resolver := turnoverReservationResolver{r.store}
	if err := resolver.AbandonListedReservation(row.Coid, r.ex.NowMs()); err == nil {
		t.Fatal("CR2 resolver authorized omission-based abandonment")
	}
	if _, retained := resolver.UnresolvedReservations()[row.Coid]; !retained {
		t.Fatal("refused abandonment lost the durable unresolved identity")
	}
}

// confidence: low
