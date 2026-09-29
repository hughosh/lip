package main

import (
	"context"
	"testing"

	"lip/harness/cfg"
	"lip/harness/num"
	"lip/harness/quote"
	"lip/harness/rest"
)

func TestScenarioExchangeCoidIdempotencySurvivesClientRestart(t *testing.T) {
	ex := newSeamExchange()
	ex.listCreated = true
	coid, _ := rest.Coid("SCENARIO", 0, quote.SideYes, 1)
	body, err := rest.NewCreateOrder(seamTicker, quote.SideYes, 40, num.QtyFromFloat(1), num.QtyFromFloat(1), coid)
	if err != nil {
		t.Fatal(err)
	}
	first := rest.NewClient(ex).Create(context.Background(), body, cfg.Default())
	if first.Outcome != rest.CreateAcked {
		t.Fatalf("first=%+v", first)
	}
	// New client, same exchange ledger: a retry cannot create a second order.
	second := rest.NewClient(ex).Create(context.Background(), body, cfg.Default())
	if second.Outcome != rest.CreateAlreadyExists || second.OrderID != first.OrderID {
		t.Fatalf("duplicate=%+v", second)
	}
	if ex.createCount() != 2 || ex.restingCount() != 1 {
		t.Fatalf("attempts=%d resting=%d", ex.createCount(), ex.restingCount())
	}
	// A public market frame is never evidence of a private fill. The scenario
	// ledger changes only through explicit fixture input (positions/fills).
	if got := ex.positions[seamTicker]; got != "" {
		t.Fatalf("invented private fill=%s", got)
	}
}
