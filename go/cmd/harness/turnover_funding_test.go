package main

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"lip/harness/cfg"
	"lip/harness/num"
	"lip/harness/quote"
	"lip/harness/rest"
	"lip/harness/risk"
)

type turnoverFundingScript struct {
	t        *testing.T
	index    map[string]int
	balances map[int]int
	position string
	calls    []rest.Request
	onCall   func()
}

func (s *turnoverFundingScript) Do(_ context.Context, r rest.Request) (rest.Response, error) {
	if s.onCall != nil {
		s.onCall()
	}
	s.calls = append(s.calls, r)
	if r.Method != "GET" {
		s.t.Fatalf("turnover funding attempted write: %+v", r)
	}
	response := func(body string) (rest.Response, error) {
		return rest.Response{Status: 200, Body: []byte(body)}, nil
	}
	if strings.HasPrefix(r.Path, "/markets/") {
		ticker := strings.TrimPrefix(r.Path, "/markets/")
		index, ok := s.index[ticker]
		if !ok {
			return rest.Response{}, fmt.Errorf("unexpected market %s", ticker)
		}
		return response(fmt.Sprintf(`{"market":{"ticker":%q,"exchange_index":%d}}`, ticker, index))
	}
	switch r.Path {
	case "/portfolio/subaccounts/balances":
		return response(`{"subaccount_balances":[{"subaccount_number":0,"exchange_index":2},{"subaccount_number":0,"exchange_index":3}]}`)
	case rest.EpPositions.Path:
		if r.Query.Get("subaccount") != "0" {
			s.t.Fatalf("positions left primary account: %+v", r)
		}
		if s.position == "" {
			return response(`{"cursor":"","market_positions":[],"event_positions":[]}`)
		}
		return response(fmt.Sprintf(`{"cursor":"","market_positions":[{"ticker":%q,"position_fp":"2.00"}],"event_positions":[]}`, s.position))
	case rest.EpOrders.Path:
		if r.Query.Get("status") != rest.StatusResting || r.Query.Get("ticker") != "" {
			s.t.Fatalf("orders omitted account scope: %+v", r)
		}
		return response(`{"cursor":"","orders":[]}`)
	case "/portfolio/balance":
		if r.Query.Get("subaccount") != "0" {
			s.t.Fatalf("balance left primary account: %+v", r)
		}
		var index int
		if _, err := fmt.Sscanf(r.Query.Get("exchange_index"), "%d", &index); err != nil {
			return rest.Response{}, err
		}
		return response(fmt.Sprintf(`{"balance":%d}`, s.balances[index]))
	}
	return rest.Response{}, fmt.Errorf("unexpected funding read: %+v", r)
}

func TestTurnoverFundingObservationKeepsScanStartAge(t *testing.T) {
	now := time.Second
	script := &turnoverFundingScript{t: t, index: map[string]int{"A": 2, "B": 2}, balances: map[int]int{2: 10000}, onCall: func() { now += time.Second }}
	latest := new(atomic.Pointer[fundingObservation])
	source := &multiFundingSource{Client: rest.NewClient(script), evidence: map[string]fundingEvidence{"A": {ExchangeIndex: 2}, "B": {ExchangeIndex: 2}}, mono: func() time.Duration { return now }, maxAge: time.Minute, latest: latest}
	if _, err := source.Balance(context.Background()); err != nil {
		t.Fatal(err)
	}
	if latest.Load().at != time.Second || now <= latest.Load().at {
		t.Fatalf("slow scan renewed old cash: now=%v at=%v", now, latest.Load().at)
	}
}

func TestTurnoverFundingDeduplicatesShardAndRetainsInheritedExposure(t *testing.T) {
	s := &turnoverFundingScript{t: t, index: map[string]int{"A": 2, "B": 2, "C": 3, "OLD": 3}, balances: map[int]int{2: 10000, 3: 5000}, position: "OLD"}
	c := config{Params: cfg.Default(), Ticker: "A", Candidates: []string{"A", "B", "C"}, Turnover: true, CapitalSource: "selected_shard_balance"}
	got, err := resolveFundingCap(context.Background(), c, s)
	if err != nil {
		t.Fatal(err)
	}
	if got.Params.CapitalMax != num.MoneyFromDollars(152) || len(got.ShardFunding) != 4 {
		t.Fatalf("shard cash double counted or inherited exposure lost: cap=%s evidence=%+v", got.Params.CapitalMax, got.ShardFunding)
	}
	if got.ShardFunding["A"].Effective != num.MoneyFromDollars(100) || got.ShardFunding["C"].Effective != num.MoneyFromDollars(52) || !got.ShardFunding["OLD"].RecoveryOnly {
		t.Fatalf("incorrect frozen shard evidence: %+v", got.ShardFunding)
	}
}

func TestTurnoverFundingCeilingIsAggregateAcrossShards(t *testing.T) {
	s := &turnoverFundingScript{t: t, index: map[string]int{"A": 2, "C": 3}, balances: map[int]int{2: 1000, 3: 1000}}
	ceiling := num.MoneyFromDollars(10)
	c := config{Params: cfg.Default(), Ticker: "A", Candidates: []string{"A", "C"}, Turnover: true, CapitalSource: "selected_shard_balance", CapitalCeiling: &ceiling}
	c.Params.NMarkets = 1
	c.Params.S = num.QtyFromFloat(1)
	got, err := resolveFundingCap(context.Background(), c, s)
	if err != nil {
		t.Fatal(err)
	}
	if got.Params.CapitalMax != ceiling || got.ShardFunding["A"].Effective != ceiling || got.ShardFunding["C"].Effective != ceiling {
		t.Fatalf("ceiling applied once per shard: cap=%s basis=%+v", got.Params.CapitalMax, got.ShardFunding)
	}
}

func TestTurnoverFundingGuardIsolatesCashAndRejectsUnadmittedIdentity(t *testing.T) {
	g := capitalOwner(t, 0, 100)
	g.o.r.cfg.CapitalSource = "selected_shard_balance"
	g.o.r.cfg.Turnover = true
	g.o.r.cfg.Funding = &fundingEvidence{Ticker: "A", ExchangeIndex: 2}
	g.o.r.cfg.ShardFunding = map[string]fundingEvidence{
		"A": {Ticker: "A", ExchangeIndex: 2, Effective: num.MoneyFromDollars(100)},
		"B": {Ticker: "B", ExchangeIndex: 2, Effective: num.MoneyFromDollars(100)},
		"C": {Ticker: "C", ExchangeIndex: 3, Effective: num.MoneyFromDollars(50)},
	}
	g.o.r.funding = new(atomic.Pointer[fundingObservation])
	g.o.r.funding.Store(&fundingObservation{valid: true, at: g.o.r.ex.Mono(), byTicker: map[string]int{"A": 2, "B": 2, "C": 3}, byShard: map[int]num.Money{2: num.MoneyFromDollars(50), 3: num.MoneyFromDollars(1)}})
	request := writeRequest{Market: "A", Role: quote.RoleAdding}
	ex := []risk.Exposure{{Ticker: "C", Adding: num.MoneyFromDollars(45)}}
	if err := g.o.checkObservedFunding(request, ex, num.MoneyFromDollars(10)); err != nil {
		t.Fatalf("other shard cash/order contaminated A: %v", err)
	}
	ex = append(ex, risk.Exposure{Ticker: "B", Adding: num.MoneyFromDollars(45)})
	if err := g.o.checkObservedFunding(request, ex, num.MoneyFromDollars(10)); err == nil {
		t.Fatal("same-shard order did not reserve A cash")
	}
	if err := g.o.checkObservedFunding(writeRequest{Market: "UNKNOWN", Role: quote.RoleAdding}, nil, num.MoneyFromDollars(1)); err == nil {
		t.Fatal("unadmitted market received another shard cash")
	}
	g.o.r.funding.Store(&fundingObservation{valid: true, at: g.o.r.ex.Mono(), byTicker: map[string]int{"A": 2, "B": 2, "C": 3}, byShard: map[int]num.Money{2: num.MoneyFromDollars(1000), 3: num.MoneyFromDollars(1)}})
	if err := g.o.checkObservedFunding(request, []risk.Exposure{{Ticker: "B", Adding: num.MoneyFromDollars(95)}}, num.MoneyFromDollars(10)); err == nil {
		t.Fatal("fresh cash raised frozen shard cap")
	}
	if err := g.o.checkObservedFunding(writeRequest{Market: "A", Role: quote.RoleReducing}, []risk.Exposure{{Ticker: "B", Position: num.MoneyFromDollars(95)}}, num.MoneyFromDollars(10)); err == nil {
		t.Fatal("reducer bypassed frozen shard cap")
	}
	if err := g.o.checkObservedFunding(request, []risk.Exposure{{Ticker: "UNMAPPED", Adding: num.MoneyFromDollars(1)}}, num.MoneyFromDollars(1)); err == nil {
		t.Fatal("unknown live order identity was ignored")
	}
}

func TestTurnoverFundingPollInvalidatesMovedMarketIdentity(t *testing.T) {
	script := &turnoverFundingScript{t: t, index: map[string]int{"A": 2, "B": 2}, balances: map[int]int{2: 10000, 3: 5000}}
	latest := new(atomic.Pointer[fundingObservation])
	source := &multiFundingSource{Client: rest.NewClient(script), evidence: map[string]fundingEvidence{"A": {ExchangeIndex: 2}, "B": {ExchangeIndex: 2}}, mono: func() time.Duration { return time.Second }, maxAge: time.Minute, latest: latest}
	if _, err := source.Balance(context.Background()); err != nil || latest.Load() == nil || !latest.Load().valid {
		t.Fatalf("initial poll failed: %v %+v", err, latest.Load())
	}
	script.index["OLD"] = 2
	script.position = "OLD"
	if _, err := source.Balance(context.Background()); err != nil || latest.Load().byTicker["OLD"] != 2 {
		t.Fatalf("new same-shard position was not read before reducer funding: %v %+v", err, latest.Load())
	}
	script.index["B"] = 3
	if _, err := source.Balance(context.Background()); err == nil || latest.Load().valid {
		t.Fatalf("moved market retained cash authority: %v %+v", err, latest.Load())
	}
}
