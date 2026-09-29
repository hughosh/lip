package main

import (
	"context"
	"fmt"
	"math"
	"sort"
	"sync/atomic"
	"time"

	"lip/harness/num"
	"lip/harness/quote"
	"lip/harness/rest"
	"lip/harness/risk"
)

// resolveTurnoverFundingCap admits the configured universe and every inherited
// exposure against complete account truth. Cash is counted once per exchange
// shard; a second market on that shard cannot manufacture a second budget.
func resolveTurnoverFundingCap(ctx context.Context, c config, doer rest.Doer) (config, error) {
	if doer == nil {
		return c, fmt.Errorf("turnover funding has no doer")
	}
	client := rest.NewClient(getOnlyDoer{next: doer})
	positions := client.AccountPositions(ctx)
	if !positions.Replaces() {
		return c, fmt.Errorf("turnover account positions incomplete: %v", positions.Err)
	}
	orders := client.Orders(ctx, "", rest.StatusResting)
	if !orders.Replaces() {
		return c, fmt.Errorf("turnover account resting orders incomplete: %v", orders.Err)
	}
	known := map[string]bool{}
	for _, ticker := range c.Candidates {
		known[ticker] = true
	}
	known[c.Ticker] = true
	for ticker, qty := range positions.ByTicker {
		if qty != 0 {
			known[ticker] = true
		}
	}
	for _, order := range orders.Orders {
		known[order.Ticker] = true
	}
	tickers := make([]string, 0, len(known))
	for ticker := range known {
		tickers = append(tickers, ticker)
	}
	sort.Strings(tickers)
	byTicker := make(map[string]rest.Funding, len(tickers))
	byShard := map[int]num.Money{}
	for _, ticker := range tickers {
		funding, err := client.MarketFunding(ctx, ticker)
		if err != nil {
			return c, fmt.Errorf("turnover funding %s: %w", ticker, err)
		}
		if funding.Ticker != ticker || funding.Subaccount != 0 {
			return c, fmt.Errorf("turnover funding %s returned unadmitted identity", ticker)
		}
		byTicker[ticker] = funding
		if available, ok := byShard[funding.ExchangeIndex]; !ok || funding.Available < available {
			byShard[funding.ExchangeIndex] = funding.Available
		}
	}
	for _, funding := range byTicker {
		if age := time.Since(funding.ObservedAt); age < 0 || age > c.Params.TruthMaxAge {
			return c, fmt.Errorf("turnover funding %s aged %v during account walk", funding.Ticker, age)
		}
	}
	commitment := map[int]num.Money{}
	hasOrder := map[int]bool{}
	addCommitment := func(ticker string, qty num.Qty) error {
		funding, ok := byTicker[ticker]
		if !ok {
			return fmt.Errorf("turnover exposure %s has no authoritative shard", ticker)
		}
		v := int64(qty)
		if v < 0 {
			if v == math.MinInt64 {
				return fmt.Errorf("turnover position quantity overflow")
			}
			v = -v
		}
		index := funding.ExchangeIndex
		if v > (math.MaxInt64-int64(commitment[index]))/risk.SettlementPrice4 {
			return fmt.Errorf("turnover shard commitment overflow")
		}
		commitment[index] += num.Money(v * risk.SettlementPrice4)
		return nil
	}
	for ticker, qty := range positions.ByTicker {
		if qty != 0 {
			if err := addCommitment(ticker, qty); err != nil {
				return c, err
			}
		}
	}
	for _, order := range orders.Orders {
		if err := addCommitment(order.Ticker, order.Remaining); err != nil {
			return c, err
		}
		hasOrder[byTicker[order.Ticker].ExchangeIndex] = true
	}
	shardCap := map[int]num.Money{}
	var aggregate num.Money
	var recovery bool
	for index, available := range byShard {
		committed := commitment[index]
		if int64(available) > math.MaxInt64-int64(committed) {
			return c, fmt.Errorf("turnover shard %d capital overflow", index)
		}
		effective := available + committed
		if committed > 0 || hasOrder[index] {
			recovery = true
		}
		shardCap[index] = effective
		if int64(aggregate) > math.MaxInt64-int64(effective) {
			return c, fmt.Errorf("turnover aggregate capital overflow")
		}
		aggregate += effective
	}
	// Shard ceilings bound each placement's scope; the aggregate ceiling is
	// enforced once over their union, not allocated to a permanently selected
	// ticker. Thus rotation can use any admitted funded shard without minting
	// aggregate capital. Recovery retains capital to reduce inherited exposure;
	// adding separately applies EntryCeiling at final dispatch.
	if !recovery && c.CapitalCeiling != nil && aggregate > *c.CapitalCeiling {
		aggregate = *c.CapitalCeiling
		for index, cap := range shardCap {
			shardCap[index] = min(cap, aggregate)
		}
	}

	if aggregate <= 0 {
		return c, refuse("turnover shards have no effective capital")
	}
	c.Params.CapitalMax = aggregate
	if err := c.Params.Validate(); err != nil {
		return c, &refusal{err: fmt.Errorf("turnover effective capital: %w", err)}
	}
	if !recovery {
		if err := risk.CheckFundable(c.Params); err != nil {
			return c, &refusal{err: fmt.Errorf("turnover shards cannot fund configured sizing: %w", err)}
		}
	}
	c.ShardFunding = make(map[string]fundingEvidence, len(byTicker))
	for ticker, funding := range byTicker {
		index := funding.ExchangeIndex
		c.ShardFunding[ticker] = fundingEvidence{
			Ticker: ticker, ExchangeIndex: index, Subaccount: funding.Subaccount,
			Available: byShard[index], Committed: commitment[index], Effective: shardCap[index],
			EntryCeiling: c.CapitalCeiling, ObservedAt: funding.ObservedAt,
			RecoveryOnly: commitment[index] > 0 || hasOrder[index],
		}
	}
	selected := c.ShardFunding[c.Ticker]
	c.Funding = &selected
	return c, nil
}

// multiFundingSource keeps account positions complete and publishes cash only
// after every admitted and newly exposed market has a fresh authoritative
// identity. A partial walk invalidates the previous cash observation.
type multiFundingSource struct {
	*rest.Client
	evidence         map[string]fundingEvidence
	inheritedTickers []string
	mono             func() time.Duration
	maxAge           time.Duration
	latest           *atomic.Pointer[fundingObservation]
}

func (s *multiFundingSource) Positions(ctx context.Context) rest.PositionsResult {
	return s.Client.AccountPositions(ctx)
}

func (s *multiFundingSource) Balance(ctx context.Context) (rest.Balance, error) {
	started := s.mono()
	invalidate := func(err error) (rest.Balance, error) {
		s.latest.Store(&fundingObservation{})
		return rest.Balance{}, err
	}
	positions := s.Client.AccountPositions(ctx)
	if !positions.Replaces() {
		return invalidate(fmt.Errorf("turnover funding positions incomplete: %v", positions.Err))
	}
	orders := s.Client.Orders(ctx, "", rest.StatusResting)
	if !orders.Replaces() {
		return invalidate(fmt.Errorf("turnover funding orders incomplete: %v", orders.Err))
	}
	tickers := map[string]bool{}
	for _, ticker := range s.inheritedTickers {
		tickers[ticker] = true
	}
	frozenShards := map[int]bool{}
	for ticker, f := range s.evidence {
		tickers[ticker] = true
		frozenShards[f.ExchangeIndex] = true
	}
	for ticker, qty := range positions.ByTicker {
		if qty != 0 {
			tickers[ticker] = true
		}
	}
	for _, order := range orders.Orders {
		tickers[order.Ticker] = true
	}
	names := make([]string, 0, len(tickers))
	for ticker := range tickers {
		names = append(names, ticker)
	}
	sort.Strings(names)
	byTicker := make(map[string]int, len(names))
	byShard := map[int]num.Money{}
	var oldest time.Time
	for _, ticker := range names {
		f, err := s.Client.MarketFunding(ctx, ticker)
		if err != nil {
			return invalidate(fmt.Errorf("turnover funding %s: %w", ticker, err))
		}
		if f.Ticker != ticker || f.Subaccount != 0 {
			return invalidate(fmt.Errorf("turnover funding %s has unadmitted identity", ticker))
		}
		if fixed, ok := s.evidence[ticker]; ok && fixed.ExchangeIndex != f.ExchangeIndex {
			return invalidate(fmt.Errorf("turnover funding %s changed exchange shard", ticker))
		}
		// Discovery can support a reducer only on an already frozen shard.
		// A new shard cannot silently enlarge this run's capital limit.
		if !frozenShards[f.ExchangeIndex] {
			return invalidate(fmt.Errorf("turnover funding %s belongs to unfunded shard", ticker))
		}
		byTicker[ticker] = f.ExchangeIndex
		if oldest.IsZero() || f.ObservedAt.Before(oldest) {
			oldest = f.ObservedAt
		}
		if prior, ok := byShard[f.ExchangeIndex]; !ok || f.Available < prior {
			byShard[f.ExchangeIndex] = f.Available
		}
	}
	if age := time.Since(oldest); s.maxAge <= 0 || age < 0 || age > s.maxAge {
		return invalidate(fmt.Errorf("turnover funding observation aged %v during account walk", age))
	}
	var cents int64
	for _, cash := range byShard {
		shardCents := int64(cash) / 10_000
		if shardCents > math.MaxInt64-cents {
			return invalidate(fmt.Errorf("turnover funding balance overflow"))
		}
		cents += shardCents
	}
	s.latest.Store(&fundingObservation{at: started, valid: true, byTicker: byTicker, byShard: byShard})
	return rest.Balance{Cents: cents}, nil
}

func (o *owner) checkTurnoverObservedFunding(req writeRequest, ex []risk.Exposure, cost num.Money) error {
	f := o.r.funding.Load()
	now := o.r.ex.Mono()
	if f == nil || !f.valid || now < f.at || now-f.at > o.p.TruthMaxAge {
		return fmt.Errorf("placement funding: turnover shard cash is unavailable or stale")
	}
	index, ok := f.byTicker[req.Market]
	if !ok {
		return fmt.Errorf("placement funding: %s has no fresh shard identity", req.Market)
	}
	frozen := false
	for _, basis := range o.r.cfg.ShardFunding {
		if basis.ExchangeIndex == index {
			frozen = true
			break
		}
	}
	if !frozen {
		return fmt.Errorf("placement funding: %s shard has no frozen capital", req.Market)
	}
	if basis, admitted := o.r.cfg.ShardFunding[req.Market]; admitted {
		if basis.ExchangeIndex != index || basis.Subaccount != 0 {
			return fmt.Errorf("placement funding: %s shard identity changed", req.Market)
		}
		if basis.RecoveryOnly && req.Role != quote.RoleReducing {
			return fmt.Errorf("placement funding: inherited shard exposure permits recovery only")
		}
	} else if req.Role != quote.RoleReducing {
		return fmt.Errorf("placement funding: %s was not admitted for adding", req.Market)
	}
	available, ok := f.byShard[index]
	if !ok {
		return fmt.Errorf("placement funding: %s shard has no fresh cash", req.Market)
	}
	for _, e := range ex {
		if e.Adding == 0 && e.Reducing == 0 {
			continue
		}
		otherIndex, known := f.byTicker[e.Ticker]
		if !known {
			return fmt.Errorf("placement funding: live order on %s has no fresh shard identity", e.Ticker)
		}
		if otherIndex == index {
			available -= e.Adding + e.Reducing
		}
	}
	if cost > available {
		return fmt.Errorf("placement funding: cost %s exceeds conservative shard %d cash %s", cost, index, available)
	}
	var deployed num.Money
	for _, e := range ex {
		otherIndex, known := f.byTicker[e.Ticker]
		if !known && e.Total() != 0 {
			return fmt.Errorf("placement funding: exposure on %s has no fresh shard identity", e.Ticker)
		}
		if known && otherIndex == index {
			deployed += e.Total()
		}
	}
	var shardCap num.Money
	for _, basis := range o.r.cfg.ShardFunding {
		if basis.ExchangeIndex == index {
			shardCap = basis.Effective
			break
		}
	}
	if deployed > shardCap || cost > shardCap-deployed {
		return fmt.Errorf("placement funding: cost %s exceeds frozen shard %d cap %s after %s deployed", cost, index, shardCap, deployed)
	}
	return nil
}

func (o *owner) recordTurnoverFundingLimits() {
	indices := map[int]fundingEvidence{}
	for _, f := range o.r.cfg.ShardFunding {
		indices[f.ExchangeIndex] = f
	}
	shards := make([]int, 0, len(indices))
	for index := range indices {
		shards = append(shards, index)
	}
	sort.Ints(shards)
	for _, index := range shards {
		f := indices[index]
		o.r.anom.raise(risk.Anomaly{Class: "FUNDING_LIMITS", Sev: risk.SEV3, Ticker: f.Ticker,
			Text: fmt.Sprintf("run=%s source=selected_shard_balance turnover=true subaccount=%d exchange_index=%d observed_at=%s cash_micro_usd=%d commitments_micro_usd=%d frozen_shard_cap_micro_usd=%d aggregate_cap_micro_usd=%d recovery_only=%t; cash polling cannot raise a frozen shard cap",
				o.r.runID, f.Subaccount, index, f.ObservedAt.UTC().Format(time.RFC3339Nano), f.Available, f.Committed, f.Effective, o.p.CapitalMax, f.RecoveryOnly)})
	}
}

// Remaining local cash and frozen collateral both constrain sizing, not just
// dispatch rejection. Existing compliant reducers remain part of the target.
func (o *owner) turnoverPlacementBudget(ticker string) num.Money {
	if o.r.funding == nil {
		return 0
	}
	f := o.r.funding.Load()
	now := o.r.ex.Mono()
	if f == nil || !f.valid || now < f.at || now-f.at > o.p.TruthMaxAge {
		return 0
	}
	index, ok := f.byTicker[ticker]
	if !ok {
		return 0
	}
	cash, ok := f.byShard[index]
	if !ok {
		return 0
	}
	var cap num.Money
	admitted := false
	for _, b := range o.r.cfg.ShardFunding {
		if b.ExchangeIndex == index {
			cap = b.Effective
			admitted = true
			break
		}
	}
	if !admitted {
		return 0
	}
	for _, e := range o.exposures() {
		shard, known := f.byTicker[e.Ticker]
		if !known && e.Total() != 0 {
			return 0
		}
		if known && shard == index {
			cash -= e.Adding + e.Reducing
			cap -= e.Total()
		}
	}
	return max(num.Money(0), min(cash, cap))
}

// confidence: low

// confidence: low
