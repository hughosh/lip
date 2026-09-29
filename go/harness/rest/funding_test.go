package rest

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"lip/harness/num"
)

func TestMarketFundingScoped(t *testing.T) {
	const ticker = "TEST-26SEP26"
	d := &scriptedDoer{t: t}
	d.handle = func(n int, req Request) (Response, error) {
		if req.Method != "GET" {
			t.Fatalf("write attempted: %+v", req)
		}
		switch n {
		case 0:
			if req.Path != "/markets/"+ticker || len(req.Query) != 0 {
				t.Fatalf("market request: %+v", req)
			}
			return Response{Status: 200, Body: []byte(`{"market":{"ticker":"TEST-26SEP26","exchange_index":0}}`)}, nil
		case 1:
			if req.Path != "/portfolio/balance" || req.Query.Get("subaccount") != "0" || req.Query.Get("exchange_index") != "0" || len(req.Query) != 2 {
				t.Fatalf("balance must explicitly scope shard zero: %+v", req)
			}
			return Response{Status: 200, Body: []byte(`{"balance":56,"balance_dollars":"0.560001","updated_ts":1,"balance_breakdown":[{"exchange_index":1,"balance":"99.000000"},{"exchange_index":0,"balance":"0.560001"}]}`)}, nil
		default:
			t.Fatalf("extra request: %+v", req)
			return Response{}, nil
		}
	}
	before := time.Now()
	f, err := NewClient(d).MarketFunding(context.Background(), ticker)
	if err != nil {
		t.Fatal(err)
	}
	if f.Ticker != ticker || f.ExchangeIndex != 0 || f.Subaccount != 0 || f.Available != num.Money(560001) || f.UpdatedTS != 1 {
		t.Fatalf("wrong scoped funding: %+v", f)
	}
	if f.ObservedAt.Before(before) || f.ObservedAt.After(time.Now()) {
		t.Fatalf("observation timestamp not local completion: %v", f.ObservedAt)
	}
}

func TestMarketFundingWrongShardAggregateRefused(t *testing.T) {
	d := &scriptedDoer{t: t}
	d.handle = func(n int, req Request) (Response, error) {
		if n == 0 {
			return Response{Status: 200, Body: []byte(`{"market":{"ticker":"T","exchange_index":2}}`)}, nil
		}
		if req.Query.Get("exchange_index") != "2" || req.Query.Get("subaccount") != "0" {
			t.Fatalf("unscoped query: %+v", req)
		}
		return Response{Status: 200, Body: []byte(`{"balance":10000,"balance_dollars":"100.00","balance_breakdown":[{"exchange_index":1,"balance":"100.00"},{"exchange_index":2,"balance":"0.00"}]}`)}, nil
	}
	if _, err := NewClient(d).MarketFunding(context.Background(), "T"); err == nil {
		t.Fatal("aggregate money on another shard funded selected shard")
	}
}

func TestMarketFundingInvalidFields(t *testing.T) {
	tests := []struct{ name, market, balance string }{
		{"market mismatch", `{"market":{"ticker":"OTHER","exchange_index":1}}`, ""},
		{"missing index", `{"market":{"ticker":"T"}}`, ""},
		{"null index", `{"market":{"ticker":"T","exchange_index":null}}`, ""},
		{"negative index", `{"market":{"ticker":"T","exchange_index":-1}}`, ""},
		{"index overflow", `{"market":{"ticker":"T","exchange_index":9223372036854775808}}`, ""},
		{"missing balance", `{"market":{"ticker":"T","exchange_index":1}}`, `{}`},
		{"null balance", `{"market":{"ticker":"T","exchange_index":1}}`, `{"balance":null}`},
		{"negative balance", `{"market":{"ticker":"T","exchange_index":1}}`, `{"balance":-1}`},
		{"overflow balance", `{"market":{"ticker":"T","exchange_index":1}}`, `{"balance":922337203685478}`},
		{"dollars mismatch", `{"market":{"ticker":"T","exchange_index":1}}`, `{"balance":2,"balance_dollars":"1.00"}`},
		{"fraction overflow", `{"market":{"ticker":"T","exchange_index":1}}`, `{"balance":1,"balance_dollars":"0.0100001"}`},
		{"null breakdown", `{"market":{"ticker":"T","exchange_index":1}}`, `{"balance":1,"balance_breakdown":null}`},
		{"missing selected row", `{"market":{"ticker":"T","exchange_index":1}}`, `{"balance":1,"balance_breakdown":[{"exchange_index":2,"balance":"0.01"}]}`},
		{"negative updated timestamp", `{"market":{"ticker":"T","exchange_index":1}}`, `{"balance":1,"updated_ts":-1}`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			d := &scriptedDoer{t: t}
			d.handle = func(n int, _ Request) (Response, error) {
				if n == 0 {
					return Response{Status: 200, Body: []byte(tc.market)}, nil
				}
				return Response{Status: 200, Body: []byte(tc.balance)}, nil
			}
			if _, err := NewClient(d).MarketFunding(context.Background(), "T"); err == nil {
				t.Fatal("malformed funding accepted")
			}
		})
	}
}

func TestMarketFunding429AndNoRawBody(t *testing.T) {
	for _, stage := range []int{0, 1} {
		d := &scriptedDoer{t: t}
		d.handle = func(n int, _ Request) (Response, error) {
			if n == 0 && stage == 1 {
				return Response{Status: 200, Body: []byte(`{"market":{"ticker":"T","exchange_index":0}}`)}, nil
			}
			return Response{Status: 429, Body: []byte("private-body-sentinel")}, nil
		}
		_, err := NewClient(d).MarketFunding(context.Background(), "T")
		var limited *RateLimitError
		if !errors.As(err, &limited) || strings.Contains(err.Error(), "private-body-sentinel") {
			t.Fatalf("stage %d: want sanitized rate limit, got %v", stage, err)
		}
	}
}
