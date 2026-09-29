package rest

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

type accountFixture struct {
	balances Response
	pages    map[string]Response
	calls    []Request
}

func (f *accountFixture) Do(_ context.Context, r Request) (Response, error) {
	f.calls = append(f.calls, r)
	if r.Method != "GET" {
		return Response{}, fmt.Errorf("unexpected method %s", r.Method)
	}
	if r.Path == subaccountBalancesPath {
		return f.balances, nil
	}
	if r.Path != EpPositions.Path || r.Query.Get("subaccount") == "" || r.Query.Has("exchange_index") {
		return Response{}, fmt.Errorf("unscoped positions request: %+v", r)
	}
	p, ok := f.pages[r.Query.Get("subaccount")+"/"+r.Query.Get("cursor")]
	if !ok {
		return Response{}, fmt.Errorf("unexpected page: %+v", r)
	}
	return p, nil
}
func accountOK(s string) Response { return Response{Status: 200, Body: []byte(s)} }

func TestAccountPositionsCompleteAllSubaccounts(t *testing.T) {
	f := &accountFixture{balances: accountOK(`{"subaccount_balances":[{"subaccount_number":1,"exchange_index":0},{"subaccount_number":0,"exchange_index":0},{"subaccount_number":1,"exchange_index":2}]}`), pages: map[string]Response{
		"0/":     accountOK(`{"cursor":"next","market_positions":[],"event_positions":[]}`),
		"0/next": accountOK(`{"cursor":"","market_positions":[{"ticker":"A","position_fp":"2.00"}],"event_positions":[]}`),
		"1/":     accountOK(`{"cursor":"","market_positions":[],"event_positions":[]}`),
	}}
	r := NewClient(f).AccountPositions(context.Background())
	if !r.Replaces() || r.ByTicker["A"] == 0 || r.Pages != 3 || len(f.calls) != 4 {
		t.Fatalf("result=%+v calls=%+v", r, f.calls)
	}
}

func TestAccountPositionsRejectsUnknownEnumeration(t *testing.T) {
	cases := []Response{
		accountOK(`{}`), accountOK(`{"subaccount_balances":null}`), accountOK(`{"subaccount_balances":[]}`), accountOK(`{"subaccount_balances":{}}`),
		accountOK(`{"subaccount_balances":[{"subaccount_number":1,"exchange_index":0}]}`),
		accountOK(`{"subaccount_balances":[{"subaccount_number":null,"exchange_index":0}]}`),
		accountOK(`{"cursor":"next","subaccount_balances":[{"subaccount_number":0,"exchange_index":0}]}`),
		{Status: 403, Body: []byte(`{"message":"restricted"}`)},
	}
	for i, b := range cases {
		f := &accountFixture{balances: b}
		r := NewClient(f).AccountPositions(context.Background())
		if r.Replaces() || r.Err == nil || r.ByTicker != nil || len(f.calls) != 1 {
			t.Fatalf("case %d: result=%+v calls=%+v", i, r, f.calls)
		}
	}
}

func TestAccountPositionsRefusesOffsettingNonprimaryExposure(t *testing.T) {
	f := &accountFixture{balances: accountOK(`{"subaccount_balances":[{"subaccount_number":0,"exchange_index":0},{"subaccount_number":1,"exchange_index":0}]}`), pages: map[string]Response{
		"0/": accountOK(`{"cursor":"","market_positions":[{"ticker":"A","position_fp":"2.00"}],"event_positions":[]}`),
		"1/": accountOK(`{"cursor":"","market_positions":[{"ticker":"A","position_fp":"-2.00"}],"event_positions":[]}`),
	}}
	r := NewClient(f).AccountPositions(context.Background())
	if r.Replaces() || r.ByTicker != nil || r.Err == nil || !strings.Contains(r.Err.Error(), "subaccount 1") {
		t.Fatalf("result=%+v", r)
	}
}

func TestAccountPositionsAbandonsPartialWalk(t *testing.T) {
	f := &accountFixture{balances: accountOK(`{"subaccount_balances":[{"subaccount_number":0,"exchange_index":0},{"subaccount_number":1,"exchange_index":0}]}`), pages: map[string]Response{
		"0/":     accountOK(`{"cursor":"","market_positions":[],"event_positions":[]}`),
		"1/":     accountOK(`{"cursor":"next","market_positions":[],"event_positions":[]}`),
		"1/next": {Status: 500, Body: []byte(`{"message":"failed"}`)},
	}}
	r := NewClient(f).AccountPositions(context.Background())
	if r.Replaces() || r.ByTicker != nil || r.Err == nil {
		t.Fatalf("result=%+v", r)
	}
}

func TestAccountPositionsRefusesDuplicateTickerAcrossShards(t *testing.T) {
	f := &accountFixture{balances: accountOK(`{"subaccount_balances":[{"subaccount_number":0,"exchange_index":0},{"subaccount_number":0,"exchange_index":1}]}`), pages: map[string]Response{
		"0/": accountOK(`{"cursor":"","market_positions":[{"ticker":"A","exchange_index":0,"position_fp":"2.00"},{"ticker":"A","exchange_index":1,"position_fp":"-2.00"}],"event_positions":[]}`),
	}}
	r := NewClient(f).AccountPositions(context.Background())
	if r.Replaces() || r.ByTicker != nil || r.Err == nil {
		t.Fatalf("result=%+v", r)
	}
}

func TestAccountPositionsCannotHidePrimaryEventExposure(t *testing.T) {
	for _, exposure := range []string{`"1.0000"`, `null`, `"unknown"`} {
		f := &accountFixture{balances: accountOK(`{"subaccount_balances":[{"subaccount_number":0,"exchange_index":0}]}`), pages: map[string]Response{
			"0/": accountOK(`{"cursor":"","market_positions":[],"event_positions":[{"event_ticker":"E","event_exposure_dollars":` + exposure + `}]}`),
		}}
		if r := NewClient(f).AccountPositions(context.Background()); r.Replaces() {
			t.Fatalf("event exposure %s became a flat account", exposure)
		}
	}
}

func TestAccountPositionsAllowsHistoricalZeroEventButDoesNotNetSubaccounts(t *testing.T) {
	f := &accountFixture{balances: accountOK(`{"subaccount_balances":[{"subaccount_number":0,"exchange_index":0},{"subaccount_number":1,"exchange_index":0}]}`), pages: map[string]Response{
		"0/": accountOK(`{"cursor":"","market_positions":[],"event_positions":[{"event_ticker":"E","event_exposure_dollars":"0.0000"}]}`),
		"1/": accountOK(`{"cursor":"","market_positions":[],"event_positions":[{"event_ticker":"F","event_exposure_dollars":"0.0000"}]}`),
	}}
	if r := NewClient(f).AccountPositions(context.Background()); !r.Replaces() {
		t.Fatal(r.Err)
	}
}
