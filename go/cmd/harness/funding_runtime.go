package main

import (
	"context"
	"fmt"
	"sync/atomic"
	"time"

	"lip/harness/num"
	"lip/harness/quote"
	"lip/harness/rest"
	"lip/harness/risk"
)

// fundingObservation is immutable after publication. Cash observations never
// increase the run's frozen capital limit or enter trading P&L.
type fundingObservation struct {
	available num.Money
	at        time.Duration
	valid     bool
	// Turnover publishes a complete, immutable market identity and scoped
	// balance set in the same atomic observation.
	byTicker map[string]int
	byShard  map[int]num.Money
}

// fundedPortfolioSource keeps the selected cash scope and account-wide position
// scope on every startup and periodic read, not only on the initial preflight.
type fundedPortfolioSource struct {
	*rest.Client
	ticker string
	index  int
	mono   func() time.Duration
	latest *atomic.Pointer[fundingObservation]
}

func (s *fundedPortfolioSource) Positions(ctx context.Context) rest.PositionsResult {
	return s.Client.AccountPositions(ctx)
}

func (s *fundedPortfolioSource) Balance(ctx context.Context) (rest.Balance, error) {
	f, err := s.Client.MarketFunding(ctx, s.ticker)
	if err != nil || f.ExchangeIndex != s.index {
		s.latest.Store(&fundingObservation{})
		if err == nil {
			err = fmt.Errorf("selected market exchange index changed")
		}
		return rest.Balance{}, err
	}
	s.latest.Store(&fundingObservation{available: f.Available, at: s.mono(), valid: true})
	return rest.Balance{Cents: int64(f.Available) / 10_000}, nil
}

// checkObservedFunding is an additional cash guard, independent of the fixed
// capital/reserve model. Conservatively reserve ALL possibly live orders from
// the last observed cash, even if the exchange has already deducted them. This
// can undersize; it cannot release unknown orders or spend position value as cash.
// Cancellation never needs cash evidence. Reducers regain capacity after adds
// are confirmed absent and cash is freshly observed.
func (o *owner) checkObservedFunding(req writeRequest, ex []risk.Exposure, cost num.Money) error {
	if o.r.cfg.CapitalSource != "selected_shard_balance" {
		return nil
	}
	if o.r.cfg.Funding == nil || o.r.funding == nil {
		return fmt.Errorf("placement funding: missing authenticated funding basis")
	}
	if o.r.cfg.Turnover {
		return o.checkTurnoverObservedFunding(req, ex, cost)
	}
	if o.r.cfg.Funding.RecoveryOnly && req.Role != quote.RoleReducing {
		return fmt.Errorf("placement funding: inherited exposure permits recovery only")
	}
	f := o.r.funding.Load()
	now := o.r.ex.Mono()
	if f == nil || !f.valid || now < f.at || now-f.at > o.p.TruthMaxAge {
		return fmt.Errorf("placement funding: selected-shard cash is unavailable or stale")
	}
	available := f.available
	for _, e := range ex {
		available -= e.Adding + e.Reducing
	}
	if cost > available {
		return fmt.Errorf("placement funding: cost %s exceeds conservative selected-shard cash %s", cost, available)
	}
	return nil
}

func (o *owner) recordFundingLimits() {
	if o.r.cfg.Turnover {
		o.recordTurnoverFundingLimits()
		return
	}
	f := o.r.cfg.Funding
	if f == nil {
		return
	}
	o.r.anom.raise(risk.Anomaly{Class: "FUNDING_LIMITS", Sev: risk.SEV3, Ticker: f.Ticker,
		Text: fmt.Sprintf("run=%s source=selected_shard_balance subaccount=%d exchange_index=%d observed_at=%s cash=%s recovery_commitments=%s capital_max=%s reserve=%.6f adding_ceiling=%s s=%s s_max=%s inv_soft=%s inv_hard=%s inv_kill=%s pnl_kill=%s recovery_only=%t cash_poll=%s cash_max_age=%s; periodic cash never raises capital; open-order cash reservation is conservative",
			o.r.runID, f.Subaccount, f.ExchangeIndex, f.ObservedAt.UTC().Format(time.RFC3339Nano), f.Available, f.Committed,
			o.p.CapitalMax, o.p.CapitalReserve, num.Money(float64(o.p.CapitalMax)*(1-o.p.CapitalReserve)),
			o.p.S.Wire(), o.p.SMax.Wire(), o.p.InvSoft.Wire(), o.p.InvHard.Wire(), o.p.InvKill.Wire(), o.p.PnLKill, f.RecoveryOnly, o.p.PositionPoll, o.p.TruthMaxAge)})
	o.r.anom.raise(risk.Anomaly{Class: "FUNDING_EXACT", Sev: risk.SEV3, Ticker: f.Ticker,
		Text: fmt.Sprintf("cash_micro_usd=%d commitments_micro_usd=%d capital_micro_usd=%d reserve_micro_usd=%d entry_ceiling_micro_usd=%v", f.Available, f.Committed, o.p.CapitalMax, num.Money(float64(o.p.CapitalMax)*o.p.CapitalReserve), fundingCeilingValue(f.EntryCeiling))})
	if f.RecoveryOnly {
		o.requestStop("funding_recovery", f.Ticker)
	}
}

func fundingCeilingValue(v *num.Money) any {
	if v == nil {
		return "none"
	}
	return int64(*v)
}

// A missing cash observation removes adding authority, including resting adds.
// The durable stop still permits cancels and funded, quantity-capped reducers.
func (o *owner) checkFundingHealth() {
	if o.r.cfg.CapitalSource != "selected_shard_balance" || o.global == quote.WindingDown || o.global == quote.Drained {
		return
	}
	var f *fundingObservation
	if o.r.funding != nil {
		f = o.r.funding.Load()
	}
	// The balance reader publishes concurrently with the owner. Sampling after
	// the load prevents a successful new observation from appearing to be in
	// the future relative to an earlier owner tick. Genuine future/stale cash
	// remains invalid, and the loaded observation stays immutable.
	now := o.r.ex.Mono()
	if f != nil && f.valid && now >= f.at && now-f.at <= o.p.TruthMaxAge {
		return
	}
	// A failed latch write keeps the owner running with a held stop. Diagnose
	// that stop once, while continuing the durable-latch retry on every tick.
	if !o.fundingStopPinged {
		o.fundingStopPinged = true
		reason := "missing observation"
		if f != nil {
			reason = fmt.Sprintf("valid=%t age=%s maximum_age=%s", f.valid, now-f.at, o.p.TruthMaxAge)
		}
		o.r.anom.raise(risk.Anomaly{Class: "FUNDING_UNAVAILABLE", Sev: risk.SEV1, Ticker: o.r.cfg.Ticker,
			Text: "selected-shard cash cannot authorize adding: " + reason + "; durable stop retains cancellation, observation and freshly funded reduction"})
	}
	o.requestStop("funding_unavailable", o.r.cfg.Ticker)
}

// confidence: high
