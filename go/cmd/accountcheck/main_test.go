package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"lip/harness/rest"
)

type fixture struct {
	t         *testing.T
	calls     []rest.Request
	enum      string
	positions map[string]string
	fills     string
}

func (f *fixture) Do(_ context.Context, r rest.Request) (rest.Response, error) {
	f.calls = append(f.calls, r)
	body := "{}"
	switch r.Path {
	case "/portfolio/balance":
		body = `{"balance":100}`
	case "/portfolio/subaccounts/balances":
		body = f.enum
	case "/portfolio/orders":
		body = `{"orders":[],"cursor":""}`
	case "/portfolio/positions":
		body = f.positions[r.Query.Get("subaccount")]
		if body == "" {
			body = `{"market_positions":[],"event_positions":[],"cursor":""}`
		}
	case "/portfolio/fills":
		body = f.fills
		if body == "" {
			body = `{"fills":[],"cursor":""}`
		}
	case "/markets/TEST":
		body = `{"market":{"ticker":"TEST","exchange_index":1}}`
	default:
		f.t.Fatalf("unexpected path %s", r.Path)
	}
	return rest.Response{Status: 200, Body: []byte(body)}, nil
}
func TestGuardRefusesWritesBeforeTransport(t *testing.T) {
	f := &fixture{t: t}
	g := &guarded{next: f}
	for _, m := range []string{"POST", "DELETE", "PATCH"} {
		_, e := g.Do(context.Background(), rest.Request{Method: m, Path: "/portfolio/orders"})
		if e == nil {
			t.Fatalf("%s accepted", m)
		}
	}
	if len(f.calls) != 0 {
		t.Fatal("write reached transport")
	}
}
func TestMissingEnumerationCannotReportFlat(t *testing.T) {
	f := &fixture{t: t, enum: `{}`}
	r := run(context.Background(), f, "")
	if r.AccountScopeComplete || r.Flat {
		t.Fatalf("incomplete enumeration called flat: %+v", r)
	}
}
func TestPositionsEnumerateEverySubaccount(t *testing.T) {
	f := &fixture{t: t, enum: `{"subaccount_balances":[{"subaccount_number":0,"exchange_index":0,"balance":100},{"subaccount_number":2,"exchange_index":1,"balance":50}]}`, positions: map[string]string{"2": `{"market_positions":[{"ticker":"TWO","position_fp":"1.00"}],"event_positions":[],"cursor":""}`}}
	r := run(context.Background(), f, "")
	if !r.AccountScopeComplete || r.Flat || len(r.NonzeroPositions) != 1 || r.NonzeroPositions[0].Subaccount != 2 {
		t.Fatalf("wrong scope: %+v", r)
	}
	seen := map[string]bool{}
	for _, c := range f.calls {
		if c.Path == "/portfolio/positions" {
			seen[c.Query.Get("subaccount")] = true
			if c.Query.Has("exchange_index") {
				t.Fatal("positions restricted to shard")
			}
		}
	}
	if !seen["0"] || !seen["2"] {
		t.Fatalf("subaccount reads missing: %v", seen)
	}
}
func TestNullOrMalformedPositionCannotReportFlat(t *testing.T) {
	for _, body := range []string{`{"market_positions":[{"ticker":"TWO","position_fp":null}],"event_positions":[],"cursor":""}`, `{"market_positions":[{"ticker":"TWO","position_fp":"bad"}],"event_positions":[],"cursor":""}`} {
		f := &fixture{t: t, enum: `{"subaccount_balances":[{"subaccount_number":0,"exchange_index":0,"balance":100}]}`, positions: map[string]string{"0": body}}
		r := run(context.Background(), f, "")
		if r.Flat || r.AccountScopeComplete {
			t.Fatalf("bad position called flat: %+v", r)
		}
	}
}
func TestReportDoesNotCopyRawError(t *testing.T) {
	f := &fixture{t: t, enum: `{"balances":null,"secret":"DO_NOT_EMIT"}`}
	r := run(context.Background(), f, "")
	b, e := json.Marshal(r)
	if e != nil {
		t.Fatal(e)
	}
	if strings.Contains(string(b), "DO_NOT_EMIT") {
		t.Fatal("raw field leaked")
	}
}
func TestHistoricalZeroEventPositionIsFlat(t *testing.T) {
	f := &fixture{t: t, enum: `{"subaccount_balances":[{"subaccount_number":0,"exchange_index":0,"balance":"100.00"}]}`, positions: map[string]string{"0": `{"market_positions":[],"event_positions":[{"event_ticker":"OLD","event_exposure_dollars":"0.00"}],"cursor":""}`}}
	r := run(context.Background(), f, "")
	if !r.AccountScopeComplete || !r.Flat {
		t.Fatalf("zero historical event refused: %+v", r)
	}
	f.positions["0"] = `{"market_positions":[],"event_positions":[{"event_ticker":"OLD","event_exposure_dollars":"1.00"}],"cursor":""}`
	r = run(context.Background(), f, "")
	if r.AccountScopeComplete || r.Flat {
		t.Fatalf("nonzero event called flat: %+v", r)
	}
}
func TestFundingUsesTypedReaderAndLogsScopedQuery(t *testing.T) {
	f := &fixture{t: t, enum: `{"subaccount_balances":[{"subaccount_number":0,"exchange_index":1,"balance":"1.00"}]}`}
	r := run(context.Background(), f, "TEST")
	if r.Funding.Outcome != "complete" || r.FundingAvailableMicro != "1000000" || r.ExchangeIndex == nil || *r.ExchangeIndex != 1 {
		t.Fatalf("funding: %+v", r)
	}
	found := false
	for _, call := range r.Requests {
		if call.Path == "/portfolio/balance" && call.Query["exchange_index"] == "1" && call.Query["subaccount"] == "0" {
			found = true
		}
		if _, ok := call.Query["cursor"]; ok {
			t.Fatal("cursor leaked")
		}
	}
	if !found {
		t.Fatal("scoped balance query omitted")
	}
}
func TestFillsProbeReportsFilterObservation(t *testing.T) {
	f := &fixture{t: t, enum: `{"subaccount_balances":[{"subaccount_number":0,"exchange_index":0,"balance":100}]}`, fills: `{"fills":[{"fill_id":"F1","trade_id":"T1","ticker":"ABC","created_time":"2026-09-26T00:00:00Z","ts":1790380800,"count_fp":"1.00","fee_cost":"0.001000","is_taker":false}],"cursor":""}`}
	r := run(context.Background(), f, "", true)
	if r.Fills.Outcome != "complete" || len(r.FillRows) != 1 || len(r.MinTSProbes) != 3 {
		t.Fatalf("fill probe: %+v", r)
	}
	if got := r.FillRows[0]; got.OrderID != "" || got.Side != "" || got.Action != "" || got.BookSide != "" || got.OutcomeSide != "" {
		t.Fatalf("absent attribution fields must stay empty: %+v", got)
	}
	var atBoundary *minTSProbe
	for i := range r.MinTSProbes {
		if r.MinTSProbes[i].MinTS == r.FillRows[0].TS {
			atBoundary = &r.MinTSProbes[i]
		}
	}
	if atBoundary == nil || !atBoundary.MatchesInclusive || atBoundary.MatchesExclusive {
		t.Fatalf("boundary comparison: %+v", atBoundary)
	}
}
func TestParseFillsCarriesOrderAttribution(t *testing.T) {
	base := `"fill_id":"F1","trade_id":"T1","ticker":"ABC","created_time":"2026-09-26T00:00:00Z","ts":1790380800,"count_fp":"1.00","fee_cost":"0.001000","is_taker":false`
	rows, ok := parseFills([]json.RawMessage{
		json.RawMessage(`{` + base + `,"order_id":"O1","side":"no","action":"sell","book_side":"ask","outcome_side":"no"}`),
		json.RawMessage(`{` + base + `}`),
	})
	if !ok || len(rows) != 2 {
		t.Fatalf("parseFills: ok=%v rows=%+v", ok, rows)
	}
	if got := rows[0]; got.OrderID != "O1" || got.Side != "no" || got.Action != "sell" || got.BookSide != "ask" || got.OutcomeSide != "no" {
		t.Fatalf("attribution not carried: %+v", got)
	}
	if got := rows[1]; got.OrderID != "" || got.Side != "" || got.Action != "" {
		t.Fatalf("absent attribution fields must stay empty: %+v", got)
	}
	b, err := json.Marshal(rows)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"order_id":"O1","side":"no","action":"sell","book_side":"ask","outcome_side":"no"`, `"order_id":"","side":"","action":"","book_side":"","outcome_side":""`} {
		if !strings.Contains(string(b), want) {
			t.Fatalf("evidence JSON missing %s: %s", want, b)
		}
	}
}
