package main

import (
	"context"
	"errors"
	"math"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"lip/harness/num"
	"lip/harness/quote"
	"lip/harness/rest"
	"lip/harness/risk"
	"lip/harness/wsx"
)

type readSource struct {
	fills, orders, positions atomic.Int32
	fillErr                  error
}

func (s *readSource) Fills(context.Context, string, time.Time) rest.FillsResult {
	s.fills.Add(1)
	if s.fillErr != nil {
		return rest.FillsResult{Walk: rest.Walk{Outcome: rest.WalkFailed, Err: s.fillErr}}
	}
	return rest.FillsResult{Walk: rest.Walk{Outcome: rest.WalkComplete}}
}
func (s *readSource) Orders(context.Context, string, string) rest.OrdersResult {
	s.orders.Add(1)
	return rest.OrdersResult{Walk: rest.Walk{Outcome: rest.WalkComplete}}
}
func (s *readSource) Positions(context.Context) rest.PositionsResult {
	s.positions.Add(1)
	return rest.PositionsResult{Walk: rest.Walk{Outcome: rest.WalkComplete}}
}

func TestPortfolioReadBackoffKeepsEndpointsIndependentAndFailedWalk(t *testing.T) {
	var now time.Duration
	throttle := &rest.RateLimitError{HasDelay: true, Delay: 4 * time.Second}
	src := &readSource{fillErr: throttle}
	w := &portfolioThrottleSource{src: src, mono: func() time.Duration { return now }}
	if r := w.Fills(context.Background(), "", time.Time{}); r.Replaces() || !errors.Is(r.Err, throttle) {
		t.Fatalf("429 must be a failed walk: %+v", r)
	}
	now = time.Second
	if r := w.Fills(context.Background(), "", time.Time{}); r.Replaces() || r.Err == nil {
		t.Fatalf("waiting for retry must be a failed walk: %+v", r)
	}
	if got := src.fills.Load(); got != 1 {
		t.Fatalf("read reissued before Retry-After: %d", got)
	}
	if !w.Orders(context.Background(), "", rest.StatusResting).Replaces() ||
		!w.Positions(context.Background()).Replaces() {
		t.Fatal("fills throttle blocked independent truth reads")
	}
	now = 4 * time.Second
	src.fillErr = nil
	if !w.Fills(context.Background(), "", time.Time{}).Replaces() || src.fills.Load() != 2 {
		t.Fatal("fills intent was lost after Retry-After")
	}
}

func TestReadBackoffWaitCancellationAndMalformedRetryAfter(t *testing.T) {
	var b readBackoff
	// A malformed Retry-After has already been parsed as HasDelay=false. Its
	// fallback is bounded jitter, not an unbounded or zero-delay loop.
	b.observe(0, &rest.RateLimitError{RetryAfter: "nonsense"})
	if b.until < 500*time.Millisecond || b.until > time.Second {
		t.Fatalf("malformed Retry-After fallback %v", b.until)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	if err := b.wait(ctx, func() time.Duration { return 0 }); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled wait: %v", err)
	}
	if time.Since(start) > 100*time.Millisecond {
		t.Fatal("cancelled retry slept through its deadline")
	}
	b.observe(0, nil)
	if b.until != 0 || b.n != 0 {
		t.Fatalf("non-429 did not reset retry sequence: %+v", b)
	}
	b.observe(time.Second, &rest.RateLimitError{HasDelay: true, Delay: time.Duration(math.MaxInt64)})
	if b.until != time.Duration(math.MaxInt64) {
		t.Fatalf("huge Retry-After wrapped deadline: %v", b.until)
	}
}

func TestOrderbookReadRetryHonorsTimeoutWithoutSecondRequest(t *testing.T) {
	var calls atomic.Int32
	api := rest.NewClient(dispatchDoerFunc(func(context.Context, rest.Request) (rest.Response, error) {
		calls.Add(1)
		return rest.Response{Status: 429, Header: http.Header{"Retry-After": []string{"2"}}}, nil
	}))
	var b readBackoff
	mono := func() time.Duration { return 0 }
	first := b.orderbook(context.Background(), api, dispatchTicker, mono)
	var throttle *rest.RateLimitError
	if first.Read() || !errors.As(first.Err, &throttle) || b.until != 2*time.Second {
		t.Fatalf("first orderbook 429: result=%+v until=%v", first, b.until)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	second := b.orderbook(ctx, api, dispatchTicker, mono)
	if second.Read() || !errors.Is(second.Err, context.DeadlineExceeded) {
		t.Fatalf("timed out retry must fail without a book: %+v", second)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("retry sent before Retry-After: %d calls", got)
	}
}

func TestThrottledPlacementRetryRechecksStopRoleSizeAndSource(t *testing.T) {
	r, _ := newDispatchRig(t, &fakeExchange{})
	r.ex.Clock = wsx.NewSystemClock()
	gate, err := wsx.NewGate([]string{dispatchTicker}, r.cfg.Params)
	if err != nil {
		t.Fatal(err)
	}
	r.gate = gate
	o := newOwner(r, nil)
	adding := placement(t, r, quote.SideYes, quote.RoleAdding, 40, 1)
	o.global = quote.Starting
	if o.retryPlacementAllowed(0, adding) {
		t.Fatal("retry placed through startup stop")
	}
	o.global = quote.Running
	o.market = quote.Reducing
	if o.retryPlacementAllowed(0, adding) {
		t.Fatal("adding retry placed while market only reduces")
	}
	reducing := placement(t, r, quote.SideNo, quote.RoleReducing, 40, 2)
	r.pf.ReplacePositions(map[string]num.Qty{dispatchTicker: 50}, true, r.cfg.Params)
	if o.retryPlacementAllowed(0, reducing) {
		t.Fatal("reducer retry exceeded current inventory")
	}
	r.pf.ReplacePositions(map[string]num.Qty{dispatchTicker: 200}, true, r.cfg.Params)
	if o.retryPlacementAllowed(0, reducing) {
		t.Fatal("retry used stale or unavailable pricing source")
	}
}

func TestRead429SharesSustainedWarningAndCleanRestore(t *testing.T) {
	r, _ := newDispatchRig(t, &fakeExchange{})
	var now time.Duration
	r.ex.Mono = func() time.Duration { return now }
	o := newOwner(r, nil)
	throttle := &rest.RateLimitError{}
	o.noteRESTThrottle(throttle)
	now = 59 * time.Second
	o.noteRESTError(throttle)
	now = 60 * time.Second
	o.updateThrottle(now)
	select {
	case a := <-r.anom.ch:
		if a.Class != "RATE_LIMIT_SUSTAINED" || a.Sev != risk.SEV2 {
			t.Fatalf("wrong sustained warning: %+v", a)
		}
	default:
		t.Fatal("sustained read 429 did not raise SEV2")
	}
	o.updateThrottle(61 * time.Second)
	select {
	case a := <-r.anom.ch:
		t.Fatalf("duplicate warning: %+v", a)
	default:
	}
	now = 119 * time.Second
	o.updateThrottle(now)
	if o.throttled {
		t.Fatal("write rate did not restore after 60 clean seconds")
	}
}

func TestReducerRetryCountsItsOwnUnknownOnceAndKeepsReservedCancel(t *testing.T) {
	g := newGateFailOwner(t, num.QtyFromFloat(1))
	h, o := g.h, g.o
	h.ex.setPosition(seamTicker, "1.00")
	reducerConfirmationPoll(t, g)
	coid, _ := rest.Coid("RETRY", 0, quote.SideNo, 1)
	body, err := rest.NewCreateOrder(seamTicker, quote.SideNo, 55, num.QtyFromFloat(1), num.QtyFromFloat(1), coid)
	if err != nil {
		t.Fatal(err)
	}
	req := writeRequest{Market: seamTicker, Side: quote.SideNo, Role: quote.RoleReducing, Op: quote.OpPlace, Order: body}
	o.pending[coid] = pendingOrder{side: quote.SideNo, cents: 55, qty: body.Count()}
	if !o.retryPlacementAllowed(h.clk.Now().Mono, req) {
		t.Fatal("same coid maximum counted twice; reducer cannot retry")
	}
	o.capacity = quote.NewCapacity(2, 2)
	o.capacity.BusyGeneral = 1
	o.retries = []writeRequest{{Market: seamTicker, Side: quote.SideNo, Role: quote.RoleReducing, Op: quote.OpCancel, IDs: []uint64{1}}}
	out := make(chan writeRequest, 1)
	if !o.pumpRetry(h.clk.Now().Mono, out) {
		t.Fatal("reducer cancellation lost retry")
	}
	select {
	case got := <-out:
		if !got.Grant.ReservedWorker {
			t.Fatal("reducer cancel retry lost P1 reservation")
		}
	default:
		t.Fatal("reducer cancellation stranded behind busy general slot")
	}
}

func TestOwnerThrottleHalvesActualRefillAndRestoresIt(t *testing.T) {
	g := newGateFailOwner(t, 0)
	o := g.o
	// Keep the queue empty so this measures actual owner refill, including carry.
	for _, in := range o.r.queue.Pending() {
		o.r.queue.Drop(in.ID)
	}
	o.capacity = quote.NewCapacity(2, 2)
	o.capacity.Tokens, o.capacity.ReservedTokens = 0, 0
	o.capAt = 0
	o.noteRESTThrottle(&rest.RateLimitError{})
	o.pump(400*time.Millisecond, make(chan writeRequest))
	if o.capacity.Tokens+o.capacity.ReservedTokens != 1 {
		t.Fatalf("throttled refill=%+v, want one token at rate2.5/s", o.capacity)
	}
	o.updateThrottle(60 * time.Second)
	o.capacity.Tokens, o.capacity.ReservedTokens = 0, 0
	o.capAt = 60 * time.Second
	o.capCarry = 0
	o.pump(60*time.Second+400*time.Millisecond, make(chan writeRequest))
	if o.capacity.Tokens+o.capacity.ReservedTokens != 2 {
		t.Fatalf("restored refill=%+v, want two tokens at rate5/s", o.capacity)
	}
}
