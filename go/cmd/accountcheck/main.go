// accountcheck is an attended, read-only account scope probe. Its output is a
// small allowlist of values; neither response bodies nor transport errors escape.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"math/big"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"lip/feed"
	"lip/harness/rest"
)

type requestLog struct {
	At            string            `json:"at"`
	Path          string            `json:"path"`
	Query         map[string]string `json:"query,omitempty"`
	Status        int               `json:"status,omitempty"`
	DurationMS    float64           `json:"duration_ms"`
	ResponseBytes int               `json:"response_bytes"`
}
type guarded struct {
	next  rest.Doer
	calls []requestLog
}

func (g *guarded) Do(ctx context.Context, r rest.Request) (rest.Response, error) {
	if r.Method != "GET" || len(r.Body) != 0 {
		return rest.Response{}, errors.New("accountcheck refused a non-GET or body-bearing request")
	}
	at := time.Now().UTC().Format(time.RFC3339Nano)
	started := time.Now()
	x, e := g.next.Do(ctx, r)
	duration := time.Since(started)
	q := map[string]string{}
	for _, key := range []string{"exchange_index", "subaccount", "status", "min_ts"} {
		if v := r.Query.Get(key); v != "" {
			q[key] = v
		}
	}
	if len(q) == 0 {
		q = nil
	}
	g.calls = append(g.calls, requestLog{At: at, Path: r.Path, Query: q, Status: x.Status,
		DurationMS: float64(duration) / float64(time.Millisecond), ResponseBytes: len(x.Body)})
	if e != nil {
		return rest.Response{}, errors.New("transport failed")
	}
	return x, nil
}

type status struct {
	Outcome    string `json:"outcome"`
	Pages      int    `json:"pages,omitempty"`
	Count      int    `json:"count,omitempty"`
	HTTPStatus int    `json:"http_status,omitempty"`
}
type balance struct {
	Subaccount    int    `json:"subaccount"`
	ExchangeIndex int    `json:"exchange_index"`
	Balance       string `json:"balance"`
	UpdatedTS     string `json:"updated_ts,omitempty"`
}
type position struct {
	Subaccount int    `json:"subaccount"`
	Ticker     string `json:"ticker"`
	Quantity   string `json:"quantity"`
}
type order struct {
	ID            string `json:"order_id"`
	Ticker        string `json:"ticker"`
	Subaccount    string `json:"subaccount_number,omitempty"`
	ExchangeIndex string `json:"exchange_index,omitempty"`
	Side          string `json:"side,omitempty"`
	Remaining     string `json:"remaining_count,omitempty"`
	CreatedTime   string `json:"created_time,omitempty"`
}
type report struct {
	StartedAt             string            `json:"started_at"`
	EndedAt               string            `json:"ended_at"`
	Requests              []requestLog      `json:"requests"`
	Aggregate             status            `json:"aggregate_balance_status"`
	AggregateBalance      string            `json:"aggregate_balance,omitempty"`
	Enumeration           status            `json:"subaccount_balances_status"`
	Balances              []balance         `json:"subaccount_balances,omitempty"`
	SelectedTicker        string            `json:"selected_ticker,omitempty"`
	Market                status            `json:"market_status,omitempty"`
	ExchangeIndex         *int              `json:"exchange_index,omitempty"`
	SelectedBalance       status            `json:"selected_balance_status,omitempty"`
	SelectedBalanceCents  string            `json:"selected_balance,omitempty"`
	Funding               status            `json:"market_funding_status,omitempty"`
	FundingAvailableMicro string            `json:"market_funding_available_micro,omitempty"`
	FundingObservedAt     string            `json:"market_funding_observed_at,omitempty"`
	FundingUpdatedTS      int64             `json:"market_funding_updated_ts,omitempty"`
	Fills                 status            `json:"fills_status,omitempty"`
	FillRows              []fillRow         `json:"fills,omitempty"`
	MinTSProbes           []minTSProbe      `json:"min_ts_probes,omitempty"`
	Orders                status            `json:"resting_orders_status"`
	OpenOrders            []order           `json:"open_orders,omitempty"`
	Positions             map[string]status `json:"positions_by_subaccount"`
	NonzeroPositions      []position        `json:"nonzero_positions,omitempty"`
	AccountScopeComplete  bool              `json:"account_scope_complete"`
	Flat                  bool              `json:"flat"`
	ProductionDecoder     map[string]status `json:"production_decoder"`
}
type fillRow struct {
	FillID  string `json:"fill_id"`
	TradeID string `json:"trade_id"`
	OrderID string `json:"order_id"`
	Side    string `json:"side"`
	Action  string `json:"action"`
	// Raw direction keys, unnormalised. rest.readSide treats book_side as
	// authoritative over side ("ask" is a NO bid whatever side claims), so
	// evidence keeps all three rather than side alone.
	BookSide    string `json:"book_side"`
	OutcomeSide string `json:"outcome_side"`
	Ticker      string `json:"ticker"`
	CreatedTime string `json:"created_time"`
	TS          int64  `json:"ts"`
	Count       string `json:"count"`
	YesPrice    string `json:"yes_price_dollars,omitempty"`
	NoPrice     string `json:"no_price_dollars,omitempty"`
	FeeCost     string `json:"fee_cost"`
	IsTaker     bool   `json:"is_taker"`
}
type minTSProbe struct {
	MinTS             int64  `json:"min_ts"`
	Status            status `json:"status"`
	Returned          int    `json:"returned"`
	ExpectedInclusive int    `json:"expected_inclusive"`
	ExpectedExclusive int    `json:"expected_exclusive"`
	MatchesInclusive  bool   `json:"matches_inclusive"`
	MatchesExclusive  bool   `json:"matches_exclusive"`
}

func field(m map[string]json.RawMessage, k string) string {
	b := m[k]
	if len(b) == 0 || string(b) == "null" {
		return ""
	}
	if b[0] == '"' {
		var s string
		if json.Unmarshal(b, &s) != nil {
			return ""
		}
		return s
	}
	if b[0] == '{' || b[0] == '[' {
		return ""
	}
	return string(b)
}
func obj(b []byte) (map[string]json.RawMessage, bool) {
	var m map[string]json.RawMessage
	e := json.Unmarshal(b, &m)
	return m, e == nil && m != nil
}
func integer(s string) (int, bool) { v, e := strconv.Atoi(s); return v, e == nil && v >= 0 }
func decimal(s string) bool {
	if s == "" {
		return false
	}
	_, ok := new(big.Rat).SetString(s)
	return ok
}
func safeTicker(s string) bool {
	if s == "" || len(s) > 200 {
		return false
	}
	for _, c := range s {
		if !strings.ContainsRune("ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_.", c) {
			return false
		}
	}
	return true
}
func single(ctx context.Context, d rest.Doer, path string, q url.Values) (map[string]json.RawMessage, status) {
	x, e := d.Do(ctx, rest.Request{Method: "GET", Path: path, Query: q})
	if e != nil {
		return nil, status{Outcome: "transport_failed"}
	}
	if x.Status != 200 {
		return nil, status{Outcome: "http_error", HTTPStatus: x.Status}
	}
	m, ok := obj(x.Body)
	if !ok {
		return nil, status{Outcome: "malformed"}
	}
	return m, status{Outcome: "complete", Count: 1}
}
func walk(ctx context.Context, c *rest.Client, ep rest.Endpoint, q url.Values, key string) ([]json.RawMessage, status) {
	w := c.Walk(ctx, ep, q)
	s := status{Outcome: w.Outcome.String(), Pages: w.Pages}
	if !w.Replaces() {
		return nil, s
	}
	recs := w.Records(key)
	s.Count = len(recs)
	return recs, s
}
func enumerate(m map[string]json.RawMessage) ([]balance, []int, bool) {
	raw, ok := m["subaccount_balances"]
	if !ok || string(raw) == "null" {
		return nil, nil, false
	}
	var rows []json.RawMessage
	if json.Unmarshal(raw, &rows) != nil || len(rows) == 0 {
		return nil, nil, false
	}
	seen := map[int]bool{0: true}
	out := make([]balance, 0, len(rows))
	for _, b := range rows {
		r, ok := obj(b)
		if !ok {
			return nil, nil, false
		}
		sub, ok1 := integer(field(r, "subaccount_number"))
		idx, ok2 := integer(field(r, "exchange_index"))
		v := field(r, "balance")
		if !ok1 || !ok2 || !decimal(v) {
			return nil, nil, false
		}
		seen[sub] = true
		out = append(out, balance{Subaccount: sub, ExchangeIndex: idx, Balance: v, UpdatedTS: field(r, "updated_ts")})
	}
	subs := make([]int, 0, len(seen))
	for n := range seen {
		subs = append(subs, n)
	}
	sort.Ints(subs)
	return out, subs, true
}
func parsePositions(sub int, market, event []json.RawMessage) ([]position, bool) {
	for _, b := range event {
		m, ok := obj(b)
		if !ok || !decimal(field(m, "event_exposure_dollars")) {
			return nil, false
		}
		exposure, _ := new(big.Rat).SetString(field(m, "event_exposure_dollars"))
		if exposure.Sign() != 0 {
			return nil, false
		}
	}
	out := []position{}
	for _, b := range market {
		m, ok := obj(b)
		if !ok {
			return nil, false
		}
		t := field(m, "ticker")
		q := field(m, "position_fp")
		if !safeTicker(t) || !decimal(q) {
			return nil, false
		}
		rat, _ := new(big.Rat).SetString(q)
		if rat.Sign() != 0 {
			out = append(out, position{Subaccount: sub, Ticker: t, Quantity: q})
		}
	}
	return out, true
}
func parseOrders(rows []json.RawMessage) ([]order, bool) {
	out := make([]order, 0, len(rows))
	for _, b := range rows {
		m, ok := obj(b)
		if !ok {
			return nil, false
		}
		id := field(m, "order_id")
		t := field(m, "ticker")
		if t == "" {
			t = field(m, "market_ticker")
		}
		if id == "" || !safeTicker(t) || field(m, "status") != "resting" {
			return nil, false
		}
		q := field(m, "remaining_count_fp")
		if q == "" {
			q = field(m, "remaining_count")
		}
		if !decimal(q) {
			return nil, false
		}
		out = append(out, order{ID: id, Ticker: t, Subaccount: field(m, "subaccount_number"), ExchangeIndex: field(m, "exchange_index"), Side: field(m, "side"), Remaining: q, CreatedTime: field(m, "created_time")})
	}
	return out, true
}
func parseFills(rows []json.RawMessage) ([]fillRow, bool) {
	out := make([]fillRow, 0, len(rows))
	for _, b := range rows {
		m, ok := obj(b)
		if !ok {
			return nil, false
		}
		var taker *bool
		if json.Unmarshal(m["is_taker"], &taker) != nil || taker == nil {
			return nil, false
		}
		created := field(m, "created_time")
		_, err := time.Parse(time.RFC3339Nano, created)
		ts, tsErr := strconv.ParseInt(field(m, "ts"), 10, 64)
		if err != nil || tsErr != nil || ts < 0 || field(m, "fill_id") == "" || field(m, "trade_id") == "" || !safeTicker(field(m, "ticker")) {
			return nil, false
		}
		count := field(m, "count_fp")
		if count == "" {
			count = field(m, "count")
		}
		if !decimal(count) || !decimal(field(m, "fee_cost")) {
			return nil, false
		}
		out = append(out, fillRow{FillID: field(m, "fill_id"), TradeID: field(m, "trade_id"), OrderID: field(m, "order_id"), Side: field(m, "side"), Action: field(m, "action"), BookSide: field(m, "book_side"), OutcomeSide: field(m, "outcome_side"), Ticker: field(m, "ticker"), CreatedTime: created, TS: ts, Count: count, YesPrice: field(m, "yes_price_dollars"), NoPrice: field(m, "no_price_dollars"), FeeCost: field(m, "fee_cost"), IsTaker: *taker})
	}
	return out, true
}
func sameIDs(actual []fillRow, full []fillRow, boundary int64, inclusive bool) bool {
	want := map[string]int{}
	for _, f := range full {
		if f.TS > boundary || (inclusive && f.TS == boundary) {
			want[f.FillID]++
		}
	}
	for _, f := range actual {
		want[f.FillID]--
	}
	for _, n := range want {
		if n != 0 {
			return false
		}
	}
	return true
}
func probeFills(ctx context.Context, c *rest.Client, r *report) {
	rows, s := walk(ctx, c, rest.EpFills, nil, "fills")
	r.Fills = s
	if s.Outcome != "complete" {
		return
	}
	var ok bool
	r.FillRows, ok = parseFills(rows)
	if !ok {
		r.Fills.Outcome = "malformed"
		r.FillRows = nil
		return
	}
	if len(r.FillRows) == 0 {
		return
	}
	min, max := r.FillRows[0].TS, r.FillRows[0].TS
	for _, f := range r.FillRows {
		if f.TS < min {
			min = f.TS
		}
		if f.TS > max {
			max = f.TS
		}
	}
	boundaries := []int64{min, min + (max-min)/2, max}
	seen := map[int64]bool{}
	for _, base := range boundaries {
		for _, offset := range []int64{-1, 0, 1} {
			bound := base + offset
			if bound < 0 || seen[bound] {
				continue
			}
			seen[bound] = true
			filtered, s := walk(ctx, c, rest.EpFills, url.Values{"min_ts": {strconv.FormatInt(bound, 10)}}, "fills")
			p := minTSProbe{MinTS: bound, Status: s}
			if s.Outcome == "complete" {
				parsed, valid := parseFills(filtered)
				if !valid {
					p.Status.Outcome = "malformed"
				} else {
					p.Returned = len(parsed)
					p.MatchesInclusive = sameIDs(parsed, r.FillRows, bound, true)
					p.MatchesExclusive = sameIDs(parsed, r.FillRows, bound, false)
				}
			}
			for _, f := range r.FillRows {
				if f.TS >= bound {
					p.ExpectedInclusive++
				}
				if f.TS > bound {
					p.ExpectedExclusive++
				}
			}
			r.MinTSProbes = append(r.MinTSProbes, p)
		}
	}
}
func run(ctx context.Context, d rest.Doer, ticker string, withFills ...bool) report {
	r := report{StartedAt: time.Now().UTC().Format(time.RFC3339Nano), Positions: map[string]status{}, ProductionDecoder: map[string]status{}}
	g := &guarded{next: d}
	c := rest.NewClient(g)
	aggregate, as := single(ctx, g, "/portfolio/balance", nil)
	r.Aggregate = as
	if as.Outcome == "complete" {
		v := field(aggregate, "balance")
		if !decimal(v) {
			r.Aggregate.Outcome = "malformed"
		} else {
			r.AggregateBalance = v
		}
	}
	em, es := single(ctx, g, "/portfolio/subaccounts/balances", nil)
	r.Enumeration = es
	var subs []int
	enumOK := false
	if es.Outcome == "complete" {
		var bs []balance
		bs, subs, enumOK = enumerate(em)
		if !enumOK {
			r.Enumeration.Outcome = "malformed"
		} else {
			r.Enumeration.Count = len(bs)
			r.Balances = bs
		}
	}
	if ticker != "" {
		r.SelectedTicker = ticker
		if !safeTicker(ticker) {
			r.Market = status{Outcome: "invalid_ticker"}
		} else {
			mm, ms := single(ctx, g, "/markets/"+ticker, nil)
			r.Market = ms
			if ms.Outcome == "complete" {
				inner, ok := obj(mm["market"])
				if !ok {
					r.Market.Outcome = "malformed"
				} else {
					idx, ok := integer(field(inner, "exchange_index"))
					if !ok || field(inner, "ticker") != ticker {
						r.Market.Outcome = "malformed"
					} else {
						r.ExchangeIndex = &idx
						funding, err := c.MarketFunding(ctx, ticker)
						if err != nil {
							r.Funding = status{Outcome: "failed"}
						} else {
							r.Funding = status{Outcome: "complete", Count: 1}
							r.FundingAvailableMicro = strconv.FormatInt(int64(funding.Available), 10)
							r.FundingObservedAt = funding.ObservedAt.UTC().Format(time.RFC3339Nano)
							r.FundingUpdatedTS = funding.UpdatedTS
						}
						q := url.Values{"subaccount": {"0"}, "exchange_index": {strconv.Itoa(idx)}}
						bm, ss := single(ctx, g, "/portfolio/balance", q)
						r.SelectedBalance = ss
						if ss.Outcome == "complete" {
							v := field(bm, "balance")
							if !decimal(v) {
								r.SelectedBalance.Outcome = "malformed"
							} else {
								r.SelectedBalanceCents = v
							}
						}
					}
				}
			}
		}
	}
	rows, os := walk(ctx, c, rest.EpOrders, url.Values{"status": {rest.StatusResting}}, "orders")
	r.Orders = os
	if os.Outcome == "complete" {
		var ok bool
		r.OpenOrders, ok = parseOrders(rows)
		if !ok {
			r.Orders.Outcome = "malformed"
			r.OpenOrders = nil
		}
	}
	positionsOK := enumOK
	if enumOK {
		for _, sub := range subs {
			q := url.Values{"subaccount": {strconv.Itoa(sub)}}
			w := c.Walk(ctx, rest.EpPositions, q)
			s := status{Outcome: w.Outcome.String(), Pages: w.Pages}
			if w.Replaces() {
				s.Count = len(w.Records("market_positions")) + len(w.Records("event_positions"))
				p, ok := parsePositions(sub, w.Records("market_positions"), w.Records("event_positions"))
				if !ok {
					s.Outcome = "malformed"
					positionsOK = false
				} else {
					r.NonzeroPositions = append(r.NonzeroPositions, p...)
				}
			} else {
				positionsOK = false
			}
			r.Positions[strconv.Itoa(sub)] = s
		}
	}
	// Production decoders are separately observed. Their refusal cannot rewrite
	// what the raw scope probe actually saw.
	po := c.Orders(ctx, "", rest.StatusResting)
	r.ProductionDecoder["orders"] = status{Outcome: po.Outcome.String(), Pages: po.Pages, Count: len(po.Orders)}
	pp := c.Positions(ctx)
	r.ProductionDecoder["primary_positions"] = status{Outcome: pp.Outcome.String(), Pages: pp.Pages, Count: len(pp.ByTicker)}
	if len(withFills) != 0 && withFills[0] {
		probeFills(ctx, c, &r)
	}
	r.AccountScopeComplete = enumOK && positionsOK && r.Orders.Outcome == "complete" && r.Aggregate.Outcome == "complete"
	r.Flat = r.AccountScopeComplete && len(r.NonzeroPositions) == 0 && len(r.OpenOrders) == 0
	r.Requests = g.calls
	r.EndedAt = time.Now().UTC().Format(time.RFC3339Nano)
	return r
}
func main() {
	out := flag.String("out", "", "report JSON path (required)")
	ticker := flag.String("ticker", "", "selected market ticker")
	fills := flag.Bool("fills", false, "probe complete fills and min_ts boundaries")
	flag.Parse()
	if *out == "" {
		fmt.Fprintln(os.Stderr, "-out is required")
		os.Exit(2)
	}
	signer, e := feed.NewSigner()
	if e != nil {
		fmt.Fprintln(os.Stderr, "could not load credentials")
		os.Exit(1)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	r := run(ctx, rest.NewHTTPDoer(signer, 15*time.Second), *ticker, *fills)
	b, e := json.MarshalIndent(r, "", "  ")
	if e != nil {
		fmt.Fprintln(os.Stderr, "could not encode report")
		os.Exit(1)
	}
	if e = os.WriteFile(*out, append(b, '\n'), 0o600); e != nil {
		fmt.Fprintln(os.Stderr, "could not write report")
		os.Exit(1)
	}
	fmt.Fprintln(os.Stderr, "accountcheck report written")
}

// confidence: high
