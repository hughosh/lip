package main

import (
	"context"
	"sync"
	"testing"

	"lip/harness/num"
	"lip/harness/quote"
	"lip/harness/rest"
	"lip/harness/risk"
)

type balanceRejectExchange struct {
	base     *seamExchange
	mu       sync.Mutex
	rejected bool
}

func (f *balanceRejectExchange) Do(ctx context.Context, req rest.Request) (rest.Response, error) {
	f.mu.Lock()
	reject := req.Method == "POST" && !f.rejected
	if reject {
		f.rejected = true
	}
	f.mu.Unlock()
	if reject {
		return rest.Response{Status: 400, Body: []byte(`{"error":{"code":"insufficient_balance"}}`)}, nil
	}
	return f.base.Do(ctx, req)
}

func TestInsufficientBalanceReachesDurableGlobalStopAndKeepsReducerLive(t *testing.T) {
	for _, position := range []string{"1.00", "-1.00"} {
		t.Run(position, func(t *testing.T) {
			h := newSeamHarness(t, seamOptions{SkipRig: true, ListCreated: true,
				Positions: map[string]string{seamTicker: position}})
			h.xch.Doer = &balanceRejectExchange{base: h.ex}
			h.anom = newAnomalySink()
			var err error
			h.rig, err = newRig(h.ctx, h.cfg, false, h.xch, seamAlertFactory, h.anom, h.qual)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(h.closeRig)
			h.start()
			h.await("insufficient balance durable cause", func() bool { return h.latchTrigger() == "insufficient_balance" })
			h.awaitGlobalEvent("RUNNING->WINDING_DOWN/global_stop")
			h.await("replacement reducer after rejected create", func() bool { return h.ex.createCount() > 0 })
			h.await("SEV1 recorded", func() bool { return len(h.anomalySevs("INSUFFICIENT_BALANCE")) > 0 })
			if sevs := h.anomalySevs("INSUFFICIENT_BALANCE"); len(sevs) != 1 || sevs[0] != risk.SEV1 {
				t.Fatalf("severity: %v", sevs)
			}
			h.awaitTicks(3)
			if h.snapshot().Global != quote.WindingDown {
				t.Fatalf("global=%v", h.snapshot().Global)
			}
			m, ok := h.market()
			if !ok || m.State != quote.Reducing {
				t.Fatalf("market=%+v", m)
			}
			for _, c := range h.ex.allCreates() {
				p, ours := rest.ParseCoid(c.Coid)
				want := quote.SideNo
				if position == "-1.00" {
					want = quote.SideYes
				}
				if !ours || p.Side != want || c.Count != "1.00" || !c.PostOnly {
					t.Fatalf("unsafe post-stop create: %+v", c)
				}
			}
		})
	}
}

func TestInsufficientBalanceRequiresDefiniteStructuredReject(t *testing.T) {
	for _, result := range []rest.CreateResult{
		{Outcome: rest.CreateUnknown, RejectReason: "insufficient_balance", MaxLive: num.QtyFromFloat(1)},
		{Outcome: rest.CreateRejected, RejectReason: "rate_limit_exceeded"},
		{Outcome: rest.CreateRejected},
	} {
		h := newSeamHarness(t, seamOptions{})
		o := h.ownerFor()
		o.stopForInsufficientBalance(seamTicker, result)
		if got := h.latchTrigger(); got != "" {
			t.Fatalf("unrelated outcome latched %q", got)
		}
	}
}
