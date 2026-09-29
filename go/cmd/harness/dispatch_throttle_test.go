package main

import (
	"context"
	"fmt"
	"lip/harness/quote"
	"lip/harness/rest"
	"sync/atomic"
	"testing"
	"time"
)

type dispatchDoerFunc func(context.Context, rest.Request) (rest.Response, error)

func (f dispatchDoerFunc) Do(ctx context.Context, req rest.Request) (rest.Response, error) {
	return f(ctx, req)
}

func TestConcurrentReservationsRouteToTheirOwnWorkers(t *testing.T) {
	entered := make(chan string, 2)
	release := make(chan struct{})
	ex := &fakeExchange{onCreate: func(coid string) { entered <- coid; <-release }}
	r, reserves := newDispatchRig(t, ex)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	in := make(chan writeRequest, 2)
	out := make(chan writeResult, 2)
	done := make(chan struct{})
	go func() { defer close(done); r.dispatchLoop(ctx, in, reserves, out) }()
	in <- placement(t, r, quote.SideYes, quote.RoleAdding, 40, 1)
	in <- placement(t, r, quote.SideNo, quote.RoleReducing, 40, 2)
	// Both commit permits must arrive while neither transport can finish.
	for i := 0; i < 2; i++ {
		select {
		case <-entered:
		case <-time.After(2 * time.Second):
			close(release)
			t.Fatal("concurrent reservation lost its permit")
		}
	}
	close(release)
	for i := 0; i < 2; i++ {
		select {
		case res := <-out:
			if res.Err != nil || !res.Bound {
				t.Fatalf("result %+v", res)
			}
		case <-time.After(time.Second):
			t.Fatal("missing write result")
		}
	}
	close(in)
	<-done
}

func TestP1DispatchUnderCancelStormHungGeneralAnd429(t *testing.T) {
	generalEntered := make(chan struct{}, 1)
	var posts atomic.Int32
	ex := dispatchDoerFunc(func(ctx context.Context, req rest.Request) (rest.Response, error) {
		if req.Method == "DELETE" {
			generalEntered <- struct{}{}
			<-ctx.Done()
			return rest.Response{}, ctx.Err()
		}
		posts.Add(1)
		return rest.Response{Status: 429, Body: []byte(`{"error":{"code":"rate_limited"}}`)}, nil
	})
	r, reserves := newDispatchRig(t, ex)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	in := make(chan writeRequest, 2)
	out := make(chan writeResult, 2)
	done := make(chan struct{})
	go func() { defer close(done); r.dispatchLoop(ctx, in, reserves, out) }()
	defer func() { cancel(); <-done }()
	cp := quote.NewCapacity(2, 2)
	general, _ := cp.Admit(quote.P0)
	cp = cp.Take(general)
	in <- writeRequest{IDs: []uint64{100}, Market: dispatchTicker, Op: quote.OpCancel, Grant: general, Orders: []rest.Order{{OrderID: "hung", Ticker: dispatchTicker}}}
	<-generalEntered
	// Each loop replenishes the P0 storm before selecting the waiting reducer.
	// The general worker never completes; only the reserved worker can run.
	original := placement(t, r, quote.SideNo, quote.RoleReducing, 40, 1)
	for round := 0; round < 3; round++ {
		q := quote.NewQueue(time.Second)
		for j := 0; j < 64; j++ {
			_, err := q.Enqueue(0, quote.Intent{Market: fmt.Sprintf("storm-%d", j), Side: quote.SideYes, Role: quote.RoleAdding, Kind: quote.KindCancel, Reason: quote.ReasonCancel})
			if err != nil {
				t.Fatal(err)
			}
		}
		_, err := q.Enqueue(0, quote.Intent{Market: dispatchTicker, Side: quote.SideNo, Role: quote.RoleReducing, Kind: quote.KindPlace, Reason: quote.ReasonReduce})
		if err != nil {
			t.Fatal(err)
		}
		d, ok := q.Dequeue(quote.Conditions{Global: quote.Running, AboveSoft: map[string]bool{dispatchTicker: true}}, cp)
		if !ok || !d.Grant.ReservedWorker || d.Market != dispatchTicker {
			t.Fatalf("P1 starved: %+v %v", d, ok)
		}
		req := original
		req.Grant = d.Grant
		cp = cp.Take(d.Grant)
		in <- req
		select {
		case res := <-out:
			if res.Create.Status != 429 || res.Create.MaxLive != original.Order.Count() {
				t.Fatalf("throttle lost unknown exposure: %+v", res)
			}
			if res.Req.Order.ClientOrderID() != original.Order.ClientOrderID() {
				t.Fatal("new coid on retry")
			}
			original = res.Req
			cp = releaseWrite(cp, r.cfg.Params, res.Req.Grant, res.Sent)
			// Refill after backoff without releasing the hung general slot.
			cp.ReservedTokens = 1
		case <-time.After(2 * time.Second):
			t.Fatal("P1 failed to dispatch while general transport hung")
		}
	}
	if posts.Load() != 3 {
		t.Fatalf("attempts %d", posts.Load())
	}
}

func TestThrottleBackoffReleasesSlotKeepsExposureAndRestoresRate(t *testing.T) {
	r, _ := newDispatchRig(t, &fakeExchange{})
	var now time.Duration
	r.ex.Mono = func() time.Duration { return now }
	o := newOwner(r, nil)
	req := placement(t, r, quote.SideYes, quote.RoleAdding, 40, 1)
	grant, _ := o.capacity.Admit(quote.P3)
	req.Grant = grant
	req.attempts = 1
	o.capacity = o.capacity.Take(grant)
	o.inflight = []*writeRequest{&req}
	o.applyWriteResult(writeResult{Req: req, Sent: true, Create: rest.CreateResult{Status: 429, Err: &rest.RateLimitError{}, MaxLive: req.Order.Count()}})
	if len(o.inflight) != 0 || o.capacity.BusyGeneral != 0 || len(o.retries) != 1 {
		t.Fatal("429 held a slot or lost retry")
	}
	if o.atRisk(req.Side) != req.Order.Count() {
		t.Fatal("unknown maximum lost")
	}
	if got := o.exposures(); len(got) != 1 || got[0].Adding <= 0 {
		t.Fatalf("collateral lost %+v", got)
	}
	if !o.throttled {
		t.Fatal("rate did not halve")
	}
	ch := make(chan writeRequest, 1)
	if o.pumpRetry(0, ch) {
		t.Fatal("backoff dispatched immediately")
	}
	now = 60 * time.Second
	o.updateThrottle(now)
	if o.throttled {
		t.Fatal("rate did not restore after clean interval")
	}
	for n := 0; n < 10; n++ {
		lo := retryDelay(n, &rest.RateLimitError{}, 0)
		hi := retryDelay(n, &rest.RateLimitError{}, 1)
		if lo < time.Second/2 || hi > 64*time.Second || hi != lo*2 {
			t.Fatalf("jitter bounds %v %v", lo, hi)
		}
	}
}

func TestAllInflightCreatesReserveCapital(t *testing.T) {
	r, _ := newDispatchRig(t, &fakeExchange{})
	o := newOwner(r, nil)
	first := placement(t, r, quote.SideYes, quote.RoleAdding, 40, 1)
	second := placement(t, r, quote.SideNo, quote.RoleAdding, 50, 2)
	o.inflight = []*writeRequest{&first, &second}
	both := o.exposures()
	o.inflight = o.inflight[:1]
	one := o.exposures()
	if len(both) != 1 || len(one) != 1 || both[0].Adding <= one[0].Adding {
		t.Fatalf("inflight collateral not aggregated: both %+v one %+v", both, one)
	}
}

func TestThrottleThenRejectDoesNotResolveEarlierUnknown(t *testing.T) {
	ex := &fakeExchange{createStatus: 429}
	r, reserves := newDispatchRig(t, ex)
	req := placement(t, r, quote.SideYes, quote.RoleAdding, 40, 1)
	first := routedWrite(t, r, reserves, req)
	if first.Req.permit == nil || first.Req.attempts != 1 {
		t.Fatal("retry lacks durable permit/attempt history")
	}
	ex.mu.Lock()
	ex.createStatus = 400
	ex.mu.Unlock()
	second := routedWrite(t, r, reserves, first.Req)
	if second.Create.Outcome != rest.CreateUnknown || second.Create.MaxLive != req.Order.Count() {
		t.Fatalf("later reject erased earlier unknown %+v", second.Create)
	}
	if second.Req.attempts != 2 {
		t.Fatal("retry reset attempt budget")
	}
}
