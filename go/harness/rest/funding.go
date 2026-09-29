package rest

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/url"
	"strconv"
	"strings"
	"time"

	"lip/harness/num"
)

// Funding is a completed read of cash available to subaccount 0 on the
// matching engine that owns Ticker. UpdatedTS is the balance ledger's last
// mutation, not the observation time; use ObservedAt for read freshness.
type Funding struct {
	Ticker        string
	ExchangeIndex int
	Subaccount    int
	Available     num.Money
	ObservedAt    time.Time
	UpdatedTS     int64
}

// MarketFunding resolves a market's authoritative exchange index before asking
// for that index's balance. An unscoped account balance cannot fund a market.
func (c *Client) MarketFunding(ctx context.Context, ticker string) (Funding, error) {
	if err := validateTicker(ticker); err != nil {
		return Funding{}, fmt.Errorf("market funding: %w", err)
	}
	market, err := c.Doer.Do(ctx, Request{Method: "GET", Path: "/markets/" + ticker})
	if err != nil {
		return Funding{}, fmt.Errorf("market funding market read: %w", err)
	}
	if err := fundingStatus("market", market); err != nil {
		return Funding{}, err
	}
	m, err := fundingObject(market.Body)
	if err != nil {
		return Funding{}, fmt.Errorf("market funding market: %w", err)
	}
	obj, err := fundingObjectField(m, "market")
	if err != nil {
		return Funding{}, fmt.Errorf("market funding market: %w", err)
	}
	var returned string
	if err := json.Unmarshal(obj["ticker"], &returned); err != nil || returned != ticker {
		return Funding{}, fmt.Errorf("market funding: market ticker mismatch or missing ticker")
	}
	index, err := fundingNonnegativeInt(obj, "exchange_index")
	if err != nil || index > int64(math.MaxInt) {
		return Funding{}, fmt.Errorf("market funding: invalid market exchange_index")
	}
	q := url.Values{}
	q.Set("subaccount", "0")
	q.Set("exchange_index", strconv.FormatInt(index, 10))
	resp, err := c.Doer.Do(ctx, Request{Method: "GET", Path: "/portfolio/balance", Query: q})
	if err != nil {
		return Funding{}, fmt.Errorf("market funding balance read: %w", err)
	}
	if err := fundingStatus("balance", resp); err != nil {
		return Funding{}, err
	}
	// Stamp the completed response, not the ledger's last mutation.
	observed := time.Now()
	b, err := fundingObject(resp.Body)
	if err != nil {
		return Funding{}, fmt.Errorf("market funding balance: %w", err)
	}
	cents, err := fundingNonnegativeInt(b, "balance")
	if err != nil || cents > math.MaxInt64/10_000 {
		return Funding{}, fmt.Errorf("market funding: invalid scoped balance cents")
	}
	available := num.Money(cents * 10_000)
	if raw, ok := b["balance_dollars"]; ok {
		available, err = fundingMoney(raw)
		if err != nil || int64(available)/10_000 != cents {
			return Funding{}, fmt.Errorf("market funding: scoped balance_dollars contradicts balance")
		}
	}
	var updated int64
	if _, ok := b["updated_ts"]; ok {
		updated, err = fundingNonnegativeInt(b, "updated_ts")
		if err != nil {
			return Funding{}, fmt.Errorf("market funding: invalid updated_ts")
		}
	}
	if raw, ok := b["balance_breakdown"]; ok {
		var rows []json.RawMessage
		if err := json.Unmarshal(raw, &rows); err != nil || rows == nil {
			return Funding{}, fmt.Errorf("market funding: invalid balance_breakdown")
		}
		matches := 0
		for _, row := range rows {
			item, err := fundingObject(row)
			if err != nil {
				return Funding{}, fmt.Errorf("market funding: invalid balance_breakdown row")
			}
			rowIndex, err := fundingNonnegativeInt(item, "exchange_index")
			if err != nil {
				return Funding{}, fmt.Errorf("market funding: invalid balance_breakdown exchange_index")
			}
			if rowIndex != index {
				continue
			}
			matches++
			rowMoney, err := fundingMoney(item["balance"])
			if err != nil || rowMoney != available {
				return Funding{}, fmt.Errorf("market funding: selected shard breakdown contradicts scoped balance")
			}
		}
		if matches != 1 {
			return Funding{}, fmt.Errorf("market funding: selected shard breakdown missing or duplicated")
		}
	}
	return Funding{Ticker: ticker, ExchangeIndex: int(index), Subaccount: 0,
		Available: available, ObservedAt: observed, UpdatedTS: updated}, nil
}

func fundingStatus(stage string, resp Response) error {
	if resp.Status == 200 {
		return nil
	}
	if resp.Status == 429 {
		// Preserve retry guidance while keeping private response bytes out of
		// error text (RateLimitError otherwise includes the body).
		resp.Body = nil
		return fmt.Errorf("market funding %s: %w", stage, rateLimitError(resp))
	}
	return fmt.Errorf("market funding %s: HTTP %d", stage, resp.Status)
}

func fundingObject(raw []byte) (map[string]json.RawMessage, error) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil || obj == nil {
		return nil, fmt.Errorf("invalid JSON object")
	}
	return obj, nil
}

func fundingObjectField(obj map[string]json.RawMessage, key string) (map[string]json.RawMessage, error) {
	return fundingObject(obj[key])
}

func fundingNonnegativeInt(obj map[string]json.RawMessage, key string) (int64, error) {
	raw, ok := obj[key]
	if !ok || len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return 0, fmt.Errorf("missing %s", key)
	}
	var n int64
	if err := json.Unmarshal(raw, &n); err != nil || n < 0 {
		return 0, fmt.Errorf("invalid %s", key)
	}
	return n, nil
}

// fundingMoney parses exact, nonnegative decimal dollars into micro-dollars.
// It rejects precision beyond the internal scale and integer overflow.
func fundingMoney(raw json.RawMessage) (num.Money, error) {
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return 0, fmt.Errorf("missing dollars")
	}
	var s string
	if raw[0] == '"' {
		if err := json.Unmarshal(raw, &s); err != nil {
			return 0, fmt.Errorf("invalid dollars")
		}
	} else {
		s = string(raw)
	}
	parts := strings.Split(s, ".")
	if len(parts) > 2 || len(parts[0]) == 0 || len(parts[0]) > 19 {
		return 0, fmt.Errorf("invalid dollars")
	}
	for _, part := range parts {
		for _, ch := range part {
			if ch < '0' || ch > '9' {
				return 0, fmt.Errorf("invalid dollars")
			}
		}
	}
	if len(parts) == 2 && (len(parts[1]) == 0 || len(parts[1]) > 6) {
		return 0, fmt.Errorf("invalid dollar precision")
	}
	whole, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || whole > math.MaxInt64/num.MoneyScale {
		return 0, fmt.Errorf("dollars overflow")
	}
	units := whole * num.MoneyScale
	if len(parts) == 2 {
		frac := parts[1] + strings.Repeat("0", 6-len(parts[1]))
		f, _ := strconv.ParseInt(frac, 10, 64)
		if f > math.MaxInt64-units {
			return 0, fmt.Errorf("dollars overflow")
		}
		units += f
	}
	return num.Money(units), nil
}

// confidence: high
