package main

import (
	"context"
	"fmt"
	"math"
	"time"

	"lip/harness/num"
	"lip/harness/rest"
	"lip/harness/risk"
)

// fundingEvidence is the frozen startup basis for the selected-shard cap.
// RecoveryOnly means an inherited commitment was seen and additions must be
// disabled while the ordinary cancel and reducer path remains available.
type fundingEvidence struct {
	Ticker        string
	ExchangeIndex int
	Subaccount    int
	Available     num.Money
	Committed     num.Money
	Effective     num.Money
	// EntryCeiling remains visible when recovery must keep already-committed
	// collateral in the reducer model above that configured entry ceiling.
	EntryCeiling *num.Money
	ObservedAt   time.Time
	RecoveryOnly bool
}

type getOnlyDoer struct{ next rest.Doer }

func (d getOnlyDoer) Do(ctx context.Context, req rest.Request) (rest.Response, error) {
	if req.Method != "GET" {
		return rest.Response{}, fmt.Errorf("funding preflight permits GET only: %s", req.Method)
	}
	return d.next.Do(ctx, req)
}

// resolveFundingCap reads complete account truth through the production doer.
// Its wrapper makes a new REST client incapable of writing even if the
// preflight is extended later. The resulting cap is fixed for this run.
func resolveFundingCap(ctx context.Context, c config, doer rest.Doer) (config, error) {
	if c.CapitalSource != "selected_shard_balance" {
		return c, nil
	}
	if c.Turnover {
		return resolveTurnoverFundingCap(ctx, c, doer)
	}
	if doer == nil {
		return c, fmt.Errorf("selected shard funding has no doer")
	}
	client := rest.NewClient(getOnlyDoer{next: doer})
	funding, err := client.MarketFunding(ctx, c.Ticker)
	if err != nil {
		return c, fmt.Errorf("selected shard funding: %w", err)
	}
	positions := client.AccountPositions(ctx)
	if !positions.Replaces() {
		return c, fmt.Errorf("selected shard positions incomplete: %v", positions.Err)
	}
	orders := client.Orders(ctx, "", rest.StatusResting)
	if !orders.Replaces() {
		return c, fmt.Errorf("account resting orders incomplete: %v", orders.Err)
	}
	if age := time.Since(funding.ObservedAt); age < 0 || age > c.Params.TruthMaxAge {
		return c, fmt.Errorf("selected shard funding aged %v during account walk", age)
	}
	var committed num.Money
	addFace := func(q num.Qty) error {
		v := int64(q)
		if v < 0 {
			if v == math.MinInt64 {
				return fmt.Errorf("position quantity overflow")
			}
			v = -v
		}
		if v > (math.MaxInt64-int64(committed))/risk.SettlementPrice4 {
			return fmt.Errorf("account commitment overflow")
		}
		committed += num.Money(v * risk.SettlementPrice4)
		return nil
	}
	for ticker, q := range positions.ByTicker {
		if q != 0 && ticker != c.Ticker {
			return c, refuse("account holds nonselected market %s; CR-2 management is required before starting this one-market runtime", ticker)
		}
		if err := addFace(q); err != nil {
			return c, err
		}
	}
	for _, order := range orders.Orders {
		if order.Ticker != c.Ticker {
			return c, refuse("account has nonselected market order on %s; cancel/reconcile that account activity before this one-market runtime", order.Ticker)
		}
		if err := addFace(order.Remaining); err != nil {
			return c, err
		}
	}
	if int64(funding.Available) > math.MaxInt64-int64(committed) {
		return c, fmt.Errorf("selected shard capital overflow")
	}
	effective := funding.Available + committed
	// An inherited commitment is a recovery operation. Avoid a sizing gate
	// that could prohibit reducing it, but require the runtime to prohibit ADD.
	recovery := committed > 0 || len(orders.Orders) > 0
	if !recovery && c.CapitalCeiling != nil && effective > *c.CapitalCeiling {
		effective = *c.CapitalCeiling
	}
	if effective <= 0 {
		return c, refuse("selected shard has no effective capital")
	}
	c.Params.CapitalMax = effective
	if err := c.Params.Validate(); err != nil {
		return c, &refusal{err: fmt.Errorf("selected shard effective capital: %w", err)}
	}
	if !recovery {
		if err := risk.CheckFundable(c.Params); err != nil {
			return c, &refusal{err: fmt.Errorf("selected shard cannot fund configured sizing: %w", err)}
		}
	}
	c.Funding = &fundingEvidence{Ticker: funding.Ticker,
		ExchangeIndex: funding.ExchangeIndex, Subaccount: funding.Subaccount,
		Available: funding.Available, Committed: committed, Effective: effective,
		EntryCeiling: c.CapitalCeiling, ObservedAt: funding.ObservedAt,
		RecoveryOnly: recovery}
	return c, nil
}

// confidence: high
