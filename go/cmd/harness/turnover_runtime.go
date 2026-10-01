package main

import (
	"context"
	"fmt"
	"lip/core"
	"lip/harness/lifecycle"
	"lip/harness/quote"
	"lip/harness/rest"
	"lip/harness/risk"
	"sort"
	"time"
)

// Market policy is local to the owner goroutine. Account portfolio, pending
// reservations, capacity, cancel-truth barrier, stop latch and P&L stay shared.
// Explicit context save/restore also keeps legacy one-market callers intact.
type marketRuntime struct {
	index               int
	market              quote.MarketState
	programMembership   programMembership
	restReducer         rest.ReducerBook
	f5ResnapshotPending bool
	touchPrice          [2]int
	touchFound          [2]bool
	touchSince          [2]time.Duration
	strandedSince       [2]time.Duration
	strandedNow         [2]bool
	closeAt             time.Time
	hasClose            bool
	canCloseEarly       bool
	tradingClosed       bool
	scheduleRead        time.Duration
	scheduleEver        bool
	schedulePinged      bool
	reduceNoted         bool
	reducerCancelPinged [2]bool
	cancelConfirmed     [2]bool
	cancelUnverified    [2]bool
	cancelUnverifiedAt  [2]time.Duration
	resweptAt           [2]time.Duration
}

func (o *owner) ticker() string {
	if o.activeTicker != "" {
		return o.activeTicker
	}
	return o.r.cfg.Ticker
}
func (o *owner) pendingTicker(p pendingOrder) string {
	if p.ticker != "" {
		return p.ticker
	}
	return o.r.cfg.Ticker
}
func (o *owner) manages(t string) bool {
	if o.markets == nil {
		return t == o.r.cfg.Ticker
	}
	_, ok := o.markets[t]
	return ok
}
func (o *owner) managedTickers() []string {
	if o.markets == nil {
		return []string{o.r.cfg.Ticker}
	}
	out := make([]string, 0, len(o.markets))
	for t := range o.markets {
		out = append(out, t)
	}
	sort.Strings(out)
	return out
}
func (o *owner) isSelected(t string) bool {
	if !o.r.cfg.Turnover {
		return t == o.r.cfg.Ticker
	}
	return o.turnoverReady && o.turnoverAccountingReady && o.selected[t] && o.r.ex.Mono() >= o.selectionAt && o.r.ex.Mono()-o.selectionAt <= o.p.TruthMaxAge
}
func (o *owner) saveMarket() {
	if o.markets == nil {
		return
	}
	m := o.markets[o.ticker()]
	if m == nil {
		return
	}
	m.market = o.market
	m.programMembership = o.programMembership
	m.restReducer = o.restReducer
	m.f5ResnapshotPending = o.f5ResnapshotPending
	m.touchPrice = o.touchPrice
	m.touchFound = o.touchFound
	m.touchSince = o.touchSince
	m.strandedSince = o.strandedSince
	m.strandedNow = o.strandedNow
	m.closeAt = o.closeAt
	m.hasClose = o.hasClose
	m.canCloseEarly = o.canCloseEarly
	m.tradingClosed = o.tradingClosed
	m.scheduleRead = o.scheduleRead
	m.scheduleEver = o.scheduleEver
	m.schedulePinged = o.schedulePinged
	m.reduceNoted = o.reduceNoted
	m.reducerCancelPinged = o.reducerCancelPinged
	m.cancelConfirmed = o.cancelConfirmed
	m.cancelUnverified = o.cancelUnverified
	m.cancelUnverifiedAt = o.cancelUnverifiedAt
	m.resweptAt = o.resweptAt
}
func (o *owner) loadMarket(t string) {
	m := o.markets[t]
	if m == nil {
		return
	}
	o.activeTicker = t
	o.market = m.market
	o.programMembership = m.programMembership
	o.restReducer = m.restReducer
	o.f5ResnapshotPending = m.f5ResnapshotPending
	o.touchPrice = m.touchPrice
	o.touchFound = m.touchFound
	o.touchSince = m.touchSince
	o.strandedSince = m.strandedSince
	o.strandedNow = m.strandedNow
	o.closeAt = m.closeAt
	o.hasClose = m.hasClose
	o.canCloseEarly = m.canCloseEarly
	o.tradingClosed = m.tradingClosed
	o.scheduleRead = m.scheduleRead
	o.scheduleEver = m.scheduleEver
	o.schedulePinged = m.schedulePinged
	o.reduceNoted = m.reduceNoted
	o.reducerCancelPinged = m.reducerCancelPinged
	o.cancelConfirmed = m.cancelConfirmed
	o.cancelUnverified = m.cancelUnverified
	o.cancelUnverifiedAt = m.cancelUnverifiedAt
	o.resweptAt = m.resweptAt
}
func (o *owner) marketContext(t string) func() {
	if o.markets == nil || t == o.ticker() || !o.manages(t) {
		return func() {}
	}
	before := o.ticker()
	o.saveMarket()
	o.loadMarket(t)
	return func() { o.saveMarket(); o.loadMarket(before) }
}
func (o *owner) invalidateMarketBooks() {
	o.restReducer.Invalidate()
	o.f5ResnapshotPending = false
	for _, m := range o.markets {
		m.restReducer.Invalidate()
		m.f5ResnapshotPending = false
	}
}
func (o *owner) installTurnover(a lifecycle.Adoption) {
	if !o.r.cfg.Turnover {
		return
	}
	o.markets = map[string]*marketRuntime{o.r.cfg.Ticker: {}}
	o.selected = map[string]bool{}
	o.targets = map[string]float64{o.r.cfg.Ticker: o.r.ex.Target}
	o.saveMarket()
	// Funding preflight and lifecycle adoption independently retain old exposure.
	// All approved candidates may be observed, but only selection grants adds.
	want := map[string]bool{}
	for _, t := range o.r.cfg.Candidates {
		want[t] = true
	}
	for t := range o.r.cfg.ShardFunding {
		want[t] = true
	}
	for _, t := range a.Summary().Managed {
		want[t] = true
	}
	for t := range want {
		o.addManaged(t)
	}
	for t, state := range a.States() {
		if m := o.markets[t]; m != nil {
			m.market = state
		}
	}
	o.loadMarket(o.r.cfg.Ticker)
	for _, row := range o.r.turnoverInherited {
		side := quote.SideYes
		if row.Side == "no" {
			side = quote.SideNo
		}
		o.addManaged(row.Ticker)
		o.pending[row.Coid] = pendingOrder{ticker: row.Ticker, side: side, cents: row.PriceCents, qty: row.Count, at: o.r.ex.Mono(), acked: row.Bound, id: row.OrderID}
	}
	o.rememberTurnoverOrders()
	o.refreshUniverse()
}
func (o *owner) addManaged(t string) bool {
	if t == "" || o.markets == nil || o.manages(t) {
		return false
	}
	o.markets[t] = &marketRuntime{index: len(o.markets), market: quote.Reducing, programMembership: programMembership{ticker: t, ended: true}}
	// A recovery-only book must exist even after its LIP program disappears.
	// Target 1 permits observing a mark; it never grants selection or an add.
	o.targets[t] = 1
	return true
}
func (o *owner) discoverManaged() {
	if !o.r.cfg.Turnover {
		return
	}
	changed := false
	for t, q := range o.r.pf.Positions() {
		if q != 0 {
			changed = o.addManaged(t) || changed
		}
	}
	for _, ord := range o.r.pf.LiveOrders() {
		changed = o.addManaged(ord.Ticker) || changed
	}
	for _, p := range o.pending {
		changed = o.addManaged(o.pendingTicker(p)) || changed
	}
	for _, req := range o.inflight {
		changed = o.addManaged(req.Market) || changed
	}
	if changed {
		o.refreshUniverse()
	}
}
func (o *owner) refreshUniverse() {
	tickers := o.managedTickers()
	tok, err := o.r.gate.AddMarkets(o.r.ex.Clock.Now(), tickers)
	if err != nil {
		o.requestStop("turnover_universe", "")
		return
	}
	// Portfolio reads continue under the new generation while the socket
	// re-dials, and a stop requested below still needs them to drain.
	o.offerToken(o.reconcileOut, tok)
	if err := o.r.sup.AddMarkets(tickers); err != nil {
		o.requestStop("turnover_subscription", "")
		return
	}
	o.r.book = core.NewRig(noopSink{}, o.targets)
	o.invalidateMarketBooks()
	o.bookSID = 0
	held := append([]string(nil), tickers...)
	o.r.marketUniverse.Store(&held)
	o.r.anom.raise(risk.Anomaly{Class: "TURNOVER_MANAGED", Sev: risk.SEV3, Text: fmt.Sprintf("managed=%v universe_revision=%d; all books require a new generation", tickers, o.r.gate.UniverseRevision())})
}
func (r *rig) scheduleTickers() []string {
	if p := r.marketUniverse.Load(); p != nil {
		return append([]string(nil), (*p)...)
	}
	return []string{r.cfg.Ticker}
}

// Context cancellation bounds every network wait; each market keeps its own
// schedule observation. A full result queue cannot erase an old-market update.
func (r *rig) turnoverScheduleLoop(ctx context.Context, out chan<- rest.ScheduleResult) {
	timer := time.NewTicker(r.cfg.Params.SchedulePoll)
	defer timer.Stop()
	backoff := map[string]*readBackoff{}
	for {
		for _, t := range r.scheduleTickers() {
			if backoff[t] == nil {
				backoff[t] = &readBackoff{}
			}
			readCtx, cancel := context.WithTimeout(ctx, restTimeout)
			res := backoff[t].schedule(readCtx, r.api, t, r.ex.Mono)
			cancel()
			select {
			case out <- res:
			case <-ctx.Done():
				return
			}
		}
		select {
		case <-timer.C:
		case <-ctx.Done():
			return
		}
	}
}

// confidence: low

// Selection results carry the request's monotonic start. A long read cannot
// certify current eligibility merely by finishing just before dispatch.
type turnoverSelectionRead struct {
	result  turnoverSelectionResult
	started time.Duration
}

func (r *rig) turnoverSelectionLoop(ctx context.Context, out chan<- turnoverSelectionRead) {
	if !r.cfg.Turnover {
		return
	}
	timer := time.NewTicker(r.cfg.Params.SchedulePoll)
	defer timer.Stop()
	for {
		started := r.ex.Mono()
		readCtx, cancel := context.WithTimeout(ctx, r.cfg.Params.TruthMaxAge)
		result := readTurnoverCandidates(readCtx, r.api, r.cfg.Candidates, float64(r.cfg.Params.S)/100, time.UnixMilli(r.ex.NowMs()), r.cfg.Params.MaxTenor)
		cancel()
		select {
		case out <- turnoverSelectionRead{result, started}:
		case <-ctx.Done():
			return
		}
		select {
		case <-timer.C:
		case <-ctx.Done():
			return
		}
	}
}
func (o *owner) applyTurnoverSelection(result turnoverSelectionResult, started time.Duration) {
	if !o.r.cfg.Turnover {
		return
	}
	now := o.r.ex.Mono()
	if !result.Complete || result.Err != nil || started > now || now-started > o.p.TruthMaxAge || (o.selectionSeen && started < o.selectionAt) {
		o.turnoverReady = false
		o.r.anom.raise(risk.Anomaly{Class: "TURNOVER_SELECTION_UNAVAILABLE", Sev: risk.SEV2, Text: fmt.Sprintf("selection did not replace: complete=%t error=%v age=%s", result.Complete, result.Err, now-started)})
		return
	}
	// A complete result must cover the exact configured scope once. Data from a
	// different candidate set cannot silently grant authority to an extra ticker.
	allowed := map[string]bool{}
	for _, t := range o.r.cfg.Candidates {
		allowed[t] = true
	}
	rows := map[string]turnoverCandidate{}
	for _, c := range result.Candidates {
		if !allowed[c.Ticker] {
			o.turnoverReady = false
			return
		}
		if _, dup := rows[c.Ticker]; dup {
			o.turnoverReady = false
			return
		}
		rows[c.Ticker] = c
	}
	if len(rows) != len(allowed) {
		o.turnoverReady = false
		return
	}
	previous := []string{}
	for t, selected := range o.selected {
		if selected && rows[t].Eligible {
			previous = append(previous, t)
		}
	}
	sort.Strings(previous)
	chosen := previous
	if !o.selectionSeen || now-o.selectionRankAt >= o.p.Reselect || len(previous) < len(o.selected) || len(previous) < o.p.NMarkets {
		chosen = chooseTurnoverSelection(result.Candidates, previous, o.p.NMarkets, o.p.Hysteresis)
		o.selectionRankAt = now
	}
	next := map[string]bool{}
	for _, t := range chosen {
		next[t] = true
	}
	changed := fmt.Sprint(sortedSelection(o.selected)) != fmt.Sprint(chosen)
	o.selected = next
	o.selectionSeen = true
	o.selectionAt = started
	o.turnoverReady = true
	targetChanged := false
	for t, c := range rows {
		if c.Target > 0 && o.targets[t] != c.Target {
			o.targets[t] = c.Target
			targetChanged = true
		}
		restore := o.marketContext(t)
		o.programMembership.ended = c.ProgramID == "" || !time.UnixMilli(o.r.ex.NowMs()).Before(c.ProgramEnd)
		restore()
	}
	if targetChanged {
		o.offerToken(o.reconcileOut, o.r.gate.RefreshUniverse(o.r.ex.Clock.Now()))
		o.r.sup.RefreshUniverse()
		o.r.book = core.NewRig(noopSink{}, o.targets)
		o.invalidateMarketBooks()
		o.bookSID = 0
	}
	if changed {
		o.r.anom.raise(risk.Anomaly{Class: "TURNOVER_SELECTION", Sev: risk.SEV3, Text: fmt.Sprintf("selected=%v managed=%v; rotation preserves held and possibly-live obligations", chosen, o.managedTickers())})
	}
}
func sortedSelection(set map[string]bool) []string {
	out := []string{}
	for t, v := range set {
		if v {
			out = append(out, t)
		}
	}
	sort.Strings(out)
	return out
}

func (o *owner) marketIndex(ticker string) int {
	if o.markets != nil {
		if m := o.markets[ticker]; m != nil {
			return m.index
		}
	}
	return pilotMarketIdx
}

// confidence: low
