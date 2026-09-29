package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"lip/harness/hstore"
	"lip/harness/rest"
	"lip/harness/risk"
)

type balanceSource interface {
	Balance(context.Context) (rest.Balance, error)
}

type balanceRecorder interface {
	RecordBalancePoll(hstore.RunHandle, int64, int64) (hstore.Receipt, error)
}

// balanceTelemetry records cash observations. In selected-shard mode its source
// also publishes a separate cash guard; it never changes the fixed run capital
// limit or treats a balance change as trading P&L.
type balanceTelemetry struct {
	source   balanceSource
	recorder balanceRecorder
	run      hstore.RunHandle
	interval time.Duration
	nowMs    func() int64
	mono     func() time.Duration
	backoff  readBackoff
}

func (p *balanceTelemetry) pollOnce(ctx context.Context) error {
	readCtx, cancel := context.WithTimeout(ctx, restTimeout)
	defer cancel()
	if err := p.backoff.wait(readCtx, p.mono); err != nil {
		return err
	}
	balance, err := p.source.Balance(readCtx)
	p.backoff.observe(p.mono(), err)
	if err != nil {
		return fmt.Errorf("balance telemetry read: %w", err)
	}
	if _, err := p.recorder.RecordBalancePoll(p.run, p.nowMs(), balance.Cents); err != nil {
		return fmt.Errorf("balance telemetry store: %w", err)
	}
	return nil
}

func (p *balanceTelemetry) runLoop(ctx context.Context, report func(error)) {
	if p.interval <= 0 {
		report(fmt.Errorf("balance telemetry interval %s is not positive", p.interval))
		return
	}
	t := time.NewTicker(p.interval)
	defer t.Stop()
	for {
		if err := p.pollOnce(ctx); err != nil && ctx.Err() == nil {
			report(err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (r *rig) balanceLoop(ctx context.Context, throttles chan<- *rest.RateLimitError) {
	source := balanceSource(r.api)
	interval := r.cfg.Params.BalancePoll
	if r.funding != nil {
		source = r.portfolio
		interval = r.cfg.Params.PositionPoll
	}
	p := balanceTelemetry{
		source: source, recorder: r.store, run: r.run,
		interval: interval,
		nowMs:    r.ex.NowMs, mono: r.ex.Mono,
	}
	p.runLoop(ctx, func(err error) {
		var throttle *rest.RateLimitError
		if errors.As(err, &throttle) {
			select {
			case throttles <- throttle:
			case <-ctx.Done():
				return
			}
		}
		r.anom.raise(risk.Anomaly{
			Class: "BALANCE_POLL_FAILED", Sev: risk.SEV2,
			Text: err.Error(),
		})
	})
}

// confidence: high
