package main

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"lip/harness/cfg"
	"lip/harness/rest"
)

// The injected doer stands in for the authenticated exchange transport. Its
// unscoped balance is real account money, but none is on the market's shard.
func TestStartupWrongShardCashCannotFundSelectedMarket(t *testing.T) {
	base := &fundingScript{t: t}
	var selectedReads, unscopedReads int
	doer := dispatchDoerFunc(func(ctx context.Context, req rest.Request) (rest.Response, error) {
		if req.Method != "GET" {
			t.Fatalf("startup funding wrote: %+v", req)
		}
		if req.Path == "/portfolio/balance" {
			if req.Query.Get("subaccount") != "0" {
				return rest.Response{}, fmt.Errorf("balance left primary subaccount: %+v", req)
			}
			switch req.Query.Get("exchange_index") {
			case "2":
				selectedReads++
				return rest.Response{Status: 200, Body: []byte(`{"balance":0,"balance_dollars":"0.00"}`)}, nil
			case "":
				unscopedReads++
				return rest.Response{Status: 200, Body: []byte(`{"balance":20000,"balance_dollars":"200.00"}`)}, nil
			default:
				return rest.Response{}, fmt.Errorf("unexpected balance shard: %+v", req)
			}
		}
		return base.Do(ctx, req)
	})
	c := config{Params: cfg.Default(), Ticker: "T", CapitalSource: "selected_shard_balance"}
	got, err := resolveFundingCap(context.Background(), c, doer)
	if err == nil || !strings.Contains(err.Error(), "selected shard has no effective capital") {
		t.Fatalf("wrong-shard cash authorized startup: funding=%+v cap=%s err=%v", got.Funding, got.Params.CapitalMax, err)
	}
	if selectedReads != 1 || unscopedReads != 0 {
		t.Fatalf("funding queried wrong balance scope: selected=%d unscoped=%d", selectedReads, unscopedReads)
	}
}
