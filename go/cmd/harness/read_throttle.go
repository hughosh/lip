package main

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"sync"
	"time"

	"lip/harness/rest"
	"lip/harness/wsx"
)

// readBackoff belongs to one read endpoint. It never holds a write dispatcher
// slot. A successful or non-429 answer resets the exponential sequence.
type readBackoff struct {
	until time.Duration
	n     int
}

func (b *readBackoff) observe(now time.Duration, err error) {
	var throttle *rest.RateLimitError
	if !errors.As(err, &throttle) {
		b.n, b.until = 0, 0
		return
	}
	b.until = retryDeadline(now, retryDelay(b.n, throttle, rand.Float64()))
	b.n++
}

// wait is used only by dedicated read goroutines. The first 429 is returned to
// the owner immediately; a subsequent intent waits here, outside the write pool.
func (b *readBackoff) wait(ctx context.Context, mono func() time.Duration) error {
	delay := b.until - mono()
	if delay <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return ctx.Err()
	}
}

func (b *readBackoff) schedule(ctx context.Context, api *rest.Client, ticker string,
	mono func() time.Duration) rest.ScheduleResult {
	if err := b.wait(ctx, mono); err != nil {
		return rest.ScheduleResult{Ticker: ticker, ScheduleRead: rest.ScheduleRead{
			Outcome: rest.ScheduleFailed, Err: err}}
	}
	res := api.Schedule(ctx, ticker)
	b.observe(mono(), res.Err)
	return res
}

func (b *readBackoff) orderbook(ctx context.Context, api *rest.Client, ticker string,
	mono func() time.Duration) rest.OrderbookResult {
	r, _ := b.orderbookAt(ctx, api, ticker, mono)
	return r
}

// orderbookAt stamps the actual GET start, after any rate-limit wait. F5 uses
// this stamp when deciding how long a retained REST source may authorize a
// reducer; a pre-wait timestamp would age a new book before it was read.
func (b *readBackoff) orderbookAt(ctx context.Context, api *rest.Client, ticker string,
	mono func() time.Duration) (rest.OrderbookResult, time.Duration) {
	if err := b.wait(ctx, mono); err != nil {
		return rest.OrderbookResult{Ticker: ticker, Outcome: rest.OrderbookFailed, Err: err}, 0
	}
	started := mono()
	res := api.Orderbook(ctx, ticker)
	b.observe(mono(), res.Err)
	return res, started
}

func (b *readBackoff) programs(ctx context.Context, api *rest.Client,
	mono func() time.Duration) rest.ProgramsResult {
	if err := b.wait(ctx, mono); err != nil {
		return rest.ProgramsResult{Walk: rest.Walk{Outcome: rest.WalkFailed, Err: err}}
	}
	res := api.Programs(ctx)
	b.observe(mono(), res.Err)
	return res
}

// portfolioThrottleSource tracks the three endpoints independently. A 429 on a
// long fills walk cannot keep orders or positions from being read. Until its
// own retry deadline, an endpoint reports a failed walk, never a complete empty
// one. The poller's next cycle retains the read intent.
type portfolioThrottleSource struct {
	src  wsx.PortfolioSource
	mono func() time.Duration
	mu   sync.Mutex
	fill readBackoff
	ord  readBackoff
	pos  readBackoff
}

func (s *portfolioThrottleSource) ready(b *readBackoff) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.mono() >= b.until
}

func (s *portfolioThrottleSource) saw(b *readBackoff, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b.observe(s.mono(), err)
}

func (s *portfolioThrottleSource) Fills(ctx context.Context, ticker string, since time.Time) rest.FillsResult {
	if !s.ready(&s.fill) {
		return rest.FillsResult{Walk: rest.Walk{Outcome: rest.WalkFailed,
			Err: fmt.Errorf("fills read awaiting rate-limit retry")}}
	}
	r := s.src.Fills(ctx, ticker, since)
	s.saw(&s.fill, r.Err)
	return r
}

func (s *portfolioThrottleSource) Orders(ctx context.Context, ticker, status string) rest.OrdersResult {
	if !s.ready(&s.ord) {
		return rest.OrdersResult{Walk: rest.Walk{Outcome: rest.WalkFailed,
			Err: fmt.Errorf("orders read awaiting rate-limit retry")}}
	}
	r := s.src.Orders(ctx, ticker, status)
	s.saw(&s.ord, r.Err)
	return r
}

func (s *portfolioThrottleSource) Positions(ctx context.Context) rest.PositionsResult {
	if !s.ready(&s.pos) {
		return rest.PositionsResult{Walk: rest.Walk{Outcome: rest.WalkFailed,
			Err: fmt.Errorf("positions read awaiting rate-limit retry")}}
	}
	r := s.src.Positions(ctx)
	s.saw(&s.pos, r.Err)
	return r
}

// confidence: high
