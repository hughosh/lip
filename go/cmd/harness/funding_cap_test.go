package main

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"lip/harness/cfg"
	"lip/harness/num"
	"lip/harness/rest"
)

type fundingScript struct {
	t         *testing.T
	available string
	position  string
	order     bool
	partial   bool
	calls     []rest.Request
}

func (s *fundingScript) Do(_ context.Context, r rest.Request) (rest.Response, error) {
	s.calls = append(s.calls, r)
	if r.Method != "GET" {
		s.t.Fatalf("preflight wrote: %+v", r)
	}
	response := func(body string) (rest.Response, error) { return rest.Response{Status: 200, Body: []byte(body)}, nil }
	switch r.Path {
	case "/markets/T":
		return response(`{"market":{"ticker":"T","exchange_index":2}}`)
	case "/portfolio/balance":
		if r.Query.Get("exchange_index") != "2" || r.Query.Get("subaccount") != "0" {
			s.t.Fatalf("unscoped funding: %+v", r)
		}
		return response(fmt.Sprintf(`{"balance":%s}`, s.available))
	case "/portfolio/subaccounts/balances":
		return response(`{"subaccount_balances":[{"subaccount_number":0,"exchange_index":2}]}`)
	case rest.EpPositions.Path:
		if r.Query.Get("subaccount") != "0" {
			s.t.Fatalf("unscoped positions: %+v", r)
		}
		if s.position != "" {
			return response(fmt.Sprintf(`{"cursor":"","market_positions":[{"ticker":"T","position_fp":"%s"}],"event_positions":[]}`, s.position))
		}
		return response(`{"cursor":"","market_positions":[],"event_positions":[]}`)
	case rest.EpOrders.Path:
		if r.Query.Get("status") != rest.StatusResting || r.Query.Get("ticker") != "" {
			s.t.Fatalf("incomplete order filter: %+v", r)
		}
		if s.partial {
			return rest.Response{Status: 500}, nil
		}
		if s.order {
			return response(`{"cursor":"","orders":[{"order_id":"o1","client_order_id":"lipH-run1-000-yes-00000001","ticker":"T","book_side":"bid","yes_price_dollars":"0.5000","remaining_count_fp":"3.00","status":"resting"}]}`)
		}
		return response(`{"cursor":"","orders":[]}`)
	}
	return rest.Response{}, fmt.Errorf("unexpected request: %+v", r)
}

func TestSelectedShardCapUsesCashWithoutDefaultHundredClip(t *testing.T) {
	c := config{Params: cfg.Default(), Ticker: "T", CapitalSource: "selected_shard_balance"}
	s := &fundingScript{t: t, available: "20000"}
	got, err := resolveFundingCap(context.Background(), c, s)
	if err != nil {
		t.Fatal(err)
	}
	if got.Params.CapitalMax != num.MoneyFromDollars(200) || got.Funding == nil || got.Funding.RecoveryOnly || got.Funding.ExchangeIndex != 2 || len(s.calls) != 5 {
		t.Fatalf("wrong cap/evidence: %+v calls=%d", got.Funding, len(s.calls))
	}
	ceiling := num.MoneyFromDollars(120)
	c.CapitalCeiling = &ceiling
	got, err = resolveFundingCap(context.Background(), c, &fundingScript{t: t, available: "20000"})
	if err != nil || got.Params.CapitalMax != ceiling {
		t.Fatalf("ceiling: %+v %v", got.Funding, err)
	}
}

func TestSelectedShardRecoveryPreservesCommitmentForReducer(t *testing.T) {
	c := config{Params: cfg.Default(), Ticker: "T", CapitalSource: "selected_shard_balance"}
	ceiling := num.MoneyFromDollars(1)
	c.CapitalCeiling = &ceiling
	s := &fundingScript{t: t, available: "100", position: "2.00"}
	got, err := resolveFundingCap(context.Background(), c, s)
	if err != nil {
		t.Fatal(err)
	}
	if got.Funding == nil || !got.Funding.RecoveryOnly || got.Funding.Committed != num.MoneyFromDollars(2) || got.Params.CapitalMax != num.MoneyFromDollars(3) || got.Funding.EntryCeiling == nil || *got.Funding.EntryCeiling != ceiling {
		t.Fatalf("recovery cap: %+v", got.Funding)
	}
}

func TestSelectedShardRestingOrderTriggersRecovery(t *testing.T) {
	c := config{Params: cfg.Default(), Ticker: "T", CapitalSource: "selected_shard_balance"}
	got, err := resolveFundingCap(context.Background(), c, &fundingScript{t: t, available: "0", order: true})
	if err != nil {
		t.Fatal(err)
	}
	if !got.Funding.RecoveryOnly || got.Funding.Committed != num.MoneyFromDollars(3) {
		t.Fatalf("resting order omitted from recovery: %+v", got.Funding)
	}
}

func TestSelectedShardRequiresCompleteAccountTruth(t *testing.T) {
	c := config{Params: cfg.Default(), Ticker: "T", CapitalSource: "selected_shard_balance"}
	_, err := resolveFundingCap(context.Background(), c, &fundingScript{t: t, available: "20000", partial: true})
	if err == nil || !strings.Contains(err.Error(), "orders incomplete") {
		t.Fatalf("partial truth accepted: %v", err)
	}
}

func TestFundingPreflightStructurallyGetOnly(t *testing.T) {
	d := getOnlyDoer{next: &fundingScript{t: t}}
	if _, err := d.Do(context.Background(), rest.Request{Method: "POST", Path: "/portfolio/orders"}); err == nil {
		t.Fatal("write passed GET-only preflight")
	}
}

func TestSelectedShardRefusesUnmanageableOtherMarket(t *testing.T) {
	c := config{Params: cfg.Default(), Ticker: "T", CapitalSource: "selected_shard_balance"}
	base := &fundingScript{t: t, available: "9858"}
	d := dispatchDoerFunc(func(ctx context.Context, r rest.Request) (rest.Response, error) {
		if r.Path == rest.EpPositions.Path {
			return rest.Response{Status: 200, Body: []byte(`{"cursor":"","market_positions":[{"ticker":"OTHER","position_fp":"1.00"}],"event_positions":[]}`)}, nil
		}
		return base.Do(ctx, r)
	})
	if _, err := resolveFundingCap(context.Background(), c, d); err == nil || !strings.Contains(err.Error(), "nonselected") {
		t.Fatalf("unmanageable position accepted: %v", err)
	}
}
