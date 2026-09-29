package rest

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
)

const subaccountBalancesPath = "/portfolio/subaccounts/balances"

// SubaccountNumbers enumerates every subaccount reported by the account-wide
// balances endpoint. A malformed or restricted response is never interpreted
// as a primary-only account.
func (c *Client) SubaccountNumbers(ctx context.Context) ([]int, error) {
	resp, err := c.Doer.Do(ctx, Request{Method: "GET", Path: subaccountBalancesPath})
	if err != nil {
		return nil, fmt.Errorf("subaccount enumeration: %w", err)
	}
	if resp.Status != 200 {
		if resp.Status == 429 {
			resp.Body = nil
			return nil, fmt.Errorf("subaccount enumeration: %w", rateLimitError(resp))
		}
		return nil, fmt.Errorf("subaccount enumeration: HTTP %d", resp.Status)
	}
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(resp.Body, &envelope); err != nil || envelope == nil {
		return nil, fmt.Errorf("subaccount enumeration: invalid response: %v", err)
	}
	if _, paginated := envelope["cursor"]; paginated {
		return nil, fmt.Errorf("subaccount enumeration: unexpected cursor; completeness unknown")
	}
	raw, ok := envelope["subaccount_balances"]
	if !ok || isJSONNull(raw) {
		return nil, fmt.Errorf("subaccount enumeration: missing subaccount_balances")
	}
	var rows []json.RawMessage
	if err := json.Unmarshal(raw, &rows); err != nil || rows == nil || len(rows) == 0 {
		return nil, fmt.Errorf("subaccount enumeration: invalid subaccount_balances: %v", err)
	}
	seen := make(map[int]bool)
	for i, row := range rows {
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(row, &fields); err != nil || fields == nil {
			return nil, fmt.Errorf("subaccount enumeration: row %d invalid: %v", i, err)
		}
		numberRaw, ok := fields["subaccount_number"]
		if !ok || isJSONNull(numberRaw) {
			return nil, fmt.Errorf("subaccount enumeration: row %d has no subaccount_number", i)
		}
		var number int
		if err := json.Unmarshal(numberRaw, &number); err != nil || number < 0 || number > 63 {
			return nil, fmt.Errorf("subaccount enumeration: row %d has invalid subaccount_number", i)
		}
		// Balances are one row per exchange shard. Validate shard identity but
		// deduplicate by subaccount because positions omit the shard filter.
		shardRaw, ok := fields["exchange_index"]
		if !ok || isJSONNull(shardRaw) {
			return nil, fmt.Errorf("subaccount enumeration: row %d has no exchange_index", i)
		}
		var shard int
		if err := json.Unmarshal(shardRaw, &shard); err != nil || shard < 0 {
			return nil, fmt.Errorf("subaccount enumeration: row %d has invalid exchange_index", i)
		}
		seen[number] = true
	}
	if !seen[0] {
		return nil, fmt.Errorf("subaccount enumeration: primary subaccount 0 absent; completeness unknown")
	}
	numbers := make([]int, 0, len(seen))
	for number := range seen {
		numbers = append(numbers, number)
	}
	sort.Ints(numbers)
	return numbers, nil
}

// AccountPositions reads all enumerated subaccounts across every exchange
// shard. The harness operates in a dedicated-primary envelope: exposure in a
// numbered subaccount is an error, not a position that can net with primary.
func (c *Client) AccountPositions(ctx context.Context) PositionsResult {
	numbers, err := c.SubaccountNumbers(ctx)
	if err != nil {
		return failedAccountPositions(0, err)
	}
	var primary PositionsResult
	pages := 0
	for _, number := range numbers {
		scoped := NewClient(subaccountPositionsDoer{base: c.Doer, number: number})
		result := scoped.Positions(ctx)
		pages += result.Pages
		if !result.Replaces() {
			result.Pages = pages
			if result.Err != nil {
				result.Err = fmt.Errorf("subaccount %d positions: %w", number, result.Err)
			}
			return result
		}
		// The production decoder maps records by ticker. If an all-shard
		// response repeats one ticker, that map would overwrite a shard's
		// exposure. Refuse replacement until shard aggregation is explicit.
		seenTicker := make(map[string]bool)
		for _, raw := range result.Records("market_positions") {
			var fields map[string]json.RawMessage
			if err := json.Unmarshal(raw, &fields); err != nil {
				return failedAccountPositions(pages, fmt.Errorf("subaccount %d market position: %w", number, err))
			}
			ticker := scalar(fields["ticker"])
			if seenTicker[ticker] {
				return failedAccountPositions(pages, fmt.Errorf("subaccount %d has multiple shard positions for %s; aggregation is unresolved", number, ticker))
			}
			seenTicker[ticker] = true
		}
		for _, raw := range result.Records("event_positions") {
			fields, err := fundingObject(raw)
			if err != nil {
				return failedAccountPositions(pages, fmt.Errorf("subaccount %d malformed event position", number))
			}
			exposure, err := fundingMoney(fields["event_exposure_dollars"])
			if err != nil {
				return failedAccountPositions(pages, fmt.Errorf("subaccount %d event exposure unknown", number))
			}
			if exposure == 0 {
				continue
			}
			event := scalar(fields["event_ticker"])
			represented := false
			for ticker, q := range result.ByTicker {
				if event != "" && q != 0 && strings.HasPrefix(ticker, event+"-") {
					represented = true
				}
			}
			if !represented {
				return failedAccountPositions(pages, fmt.Errorf("subaccount %d event exposure has no matching held market", number))
			}
		}
		if number == 0 {
			primary = result
			continue
		}
		for ticker, q := range result.ByTicker {
			if q != 0 {
				return failedAccountPositions(pages, fmt.Errorf("subaccount %d holds %s position; dedicated-primary envelope refuses account position replacement", number, ticker))
			}
		}
	}
	primary.Pages = pages
	return primary
}

func failedAccountPositions(pages int, err error) PositionsResult {
	return PositionsResult{Walk: Walk{Outcome: WalkFailed, Pages: pages, Err: err}}
}

type subaccountPositionsDoer struct {
	base   Doer
	number int
}

func (d subaccountPositionsDoer) Do(ctx context.Context, req Request) (Response, error) {
	if req.Method != "GET" || req.Path != EpPositions.Path {
		return Response{}, fmt.Errorf("subaccount positions scope used for unexpected request %s %s", req.Method, req.Path)
	}
	q := url.Values{}
	for key, values := range req.Query {
		if key == "exchange_index" || key == "subaccount" {
			return Response{}, fmt.Errorf("subaccount positions scope received conflicting filter %q", key)
		}
		q[key] = append([]string(nil), values...)
	}
	q.Set("subaccount", strconv.Itoa(d.number))
	req.Query = q
	return d.base.Do(ctx, req)
}

// confidence: high
