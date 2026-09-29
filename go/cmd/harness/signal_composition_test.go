package main

import (
	"context"
	"os"
	"sync"
	"syscall"
	"testing"
	"time"

	"lip/harness/quote"
)

func TestComposedOperatorSignalKeepsServiceUntilFlatUnrestedExit(t *testing.T) {
	for _, sig := range []os.Signal{syscall.SIGINT, syscall.SIGTERM} {
		t.Run(sig.String(), func(t *testing.T) {
			h := newSeamHarness(t, seamOptions{
				Positions: map[string]string{seamTicker: "1.00"}})
			// Let a complete portfolio walk list the reducer before its create
			// acknowledgement returns. Both views describe the same live order;
			// the signal must not cause an oversized-reducer cancel from counting
			// them twice.
			created := make(chan seamCreate, 1)
			releaseCreate := make(chan struct{})
			var releaseOnce sync.Once
			release := func() { releaseOnce.Do(func() { close(releaseCreate) }) }
			defer release()
			h.ex.onCreate = func(idx int, c seamCreate) {
				if idx != 0 {
					return
				}
				h.ex.mu.Lock()
				h.ex.resting = append(h.ex.resting, seamRestingOrder(c))
				h.ex.mu.Unlock()
				created <- c
				<-releaseCreate
			}
			ctx, cancel := context.WithCancel(h.ctx)
			defer cancel()
			exits := make(chan int, 1)
			signals := make(chan os.Signal, 1)
			sd := newShutdown(h.rig)
			sd.exit = func(code int) { exits <- code; cancel() }
			h.serveCancel, h.serveDone = cancel, make(chan struct{})
			go func() { h.serveErr = h.rig.serveWithShutdown(ctx, sd, signals); close(h.serveDone) }()
			t.Cleanup(h.stopServe)
			h.awaitActionable()
			var reducer seamCreate
			select {
			case reducer = <-created:
			case <-time.After(seamBudget):
				t.Fatal("no reducer create before signal")
			}
			h.await("capped reducer resting before signal", func() bool { return h.ex.restingCount() > 0 })
			h.clk.Advance(h.cfg.Params.PositionPoll)
			h.await("reducer positively bound by a complete portfolio walk", func() bool {
				coid, ok := h.rig.store.Ownership().Bound(reducer.OrderID)
				return ok && coid == reducer.Coid
			})
			h.awaitTicks(2)
			m, _ := h.market()
			if got, want := m.Sides[quote.SideNo].AtRisk, m.Q; got != want {
				t.Fatalf("listed reducer and its in-flight create counted more than once: at-risk=%s q=%s",
					got.Wire(), want.Wire())
			}
			if ids := h.ex.deletedIDs(); len(ids) != 0 || h.ex.restingCount() != 1 {
				t.Fatalf("listed in-flight reducer cancelled before signal: deletes=%v resting=%d",
					ids, h.ex.restingCount())
			}
			release()
			signals <- sig
			want := "sigterm"
			if sig == syscall.SIGINT {
				want = "sigint"
			}
			h.await("signal latch durable", func() bool { return h.latchTrigger() == want })
			h.awaitTicks(3)
			if h.snapshot().Global != quote.WindingDown {
				t.Fatalf("global=%s", h.snapshot().Global)
			}
			h.await("reducer remains available after planned stop", func() bool {
				return h.ex.restingCount() > 0
			})
			select {
			case code := <-exits:
				t.Fatalf("exited %d while inventory remains", code)
			default:
			}
			// The fake exchange explicitly reports the reducer filled and flat.
			// Only a complete new portfolio cycle can turn this into exit authority.
			h.ex.mu.Lock()
			h.ex.positions[seamTicker] = "0.00"
			h.ex.resting = nil
			h.ex.mu.Unlock()
			h.clk.Advance(h.cfg.Params.PositionPoll)
			select {
			case code := <-exits:
				if code != 0 {
					t.Fatalf("exit code=%d", code)
				}
			case <-time.After(seamBudget):
				t.Fatal("did not exit after complete flat/unrested truth")
			}
		})
	}
}
