package main

import (
	"context"
	"errors"
	"time"

	"lip/harness/rest"
)

// programMembership is the owner's last complete answer to whether the selected
// market belongs to the active liquidity-program set. Reward periods can roll
// over while the market stays active, so end_date is deliberately not an input.
// Failed, rewound, and unpopulated walks cannot assert absence or presence.
type programMembership struct {
	ticker string
	ended  bool
}

func (m *programMembership) apply(res rest.ProgramsResult) {
	if !res.Replaces() {
		return
	}
	_, active := res.ByTarget[m.ticker]
	m.ended = !active
}

// programLoop owns its read backoff and performs at most one complete-or-failed
// walk per cadence. A per-walk deadline prevents a slow paginated read from
// indefinitely postponing the next observation; Walk returns no partial set.
// The channel holds only a replacement, so a busy owner cannot accumulate old
// membership snapshots.
func (r *rig) programLoop(ctx context.Context, out chan<- rest.ProgramsResult) {
	interval := r.cfg.Params.SchedulePoll
	t := time.NewTicker(interval)
	defer t.Stop()
	var backoff readBackoff

	for {
		readCtx, cancel := context.WithTimeout(ctx, 2*interval)
		res := backoff.programs(readCtx, r.api, r.ex.Mono)
		cancel()
		var throttle *rest.RateLimitError
		if errors.As(res.Err, &throttle) {
			// The owner accounts for every 429 in the shared write throttle.
			// Dropping this hand-off could make that accounting falsely clean.
			select {
			case out <- res:
			case <-ctx.Done():
				return
			}
		} else {
			select {
			case out <- res:
			case <-ctx.Done():
				return
			default:
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// applyProgramMembership runs only on the owner goroutine. A failed walk leaves
// its last complete answer in place; a later complete walk may establish either
// absence or renewed membership after a reward-period rollover.
func (o *owner) applyProgramMembership(res rest.ProgramsResult) {
	o.noteRESTError(res.Err)
	o.r.anom.raiseAll(res.Anomalies)
	if o.r.cfg.Turnover {
		for _, t := range o.managedTickers() {
			restore := o.marketContext(t)
			o.programMembership.apply(res)
			if res.Replaces() && o.programMembership.ended {
				delete(o.selected, t)
			}
			restore()
		}
	} else {
		o.programMembership.apply(res)
	}
}

// confidence: high
