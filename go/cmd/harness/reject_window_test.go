package main

import (
	"context"
	"fmt"
	"testing"
	"time"

	"lip/harness/num"
	"lip/harness/quote"
	"lip/harness/rest"
	"lip/harness/risk"
	"lip/harness/wsx"
)

func rejectResult(coid string) rest.CreateResult {
	return rest.CreateResult{Coid: coid, Attempts: 2, Outcome: rest.CreateRejected,
		Status: 400, RejectReason: "some_exchange_code"}
}

func queuedAnomalySevs(s *anomalySink, class string) []risk.Severity {
	var out []risk.Severity
	for len(s.ch) > 0 {
		a := <-s.ch
		if a.Class == class {
			out = append(out, a.Sev)
		}
	}
	return out
}

func TestRejectWindowExactThresholdUniqueSentAndMarketScoped(t *testing.T) {
	var w rejectWindow
	for i := 0; i < 44; i++ {
		_, crossed := w.record("A", rest.CreateResult{Coid: fmt.Sprintf("ack-%d", i), Outcome: rest.CreateAcked, Status: 200}, true)
		if crossed {
			t.Fatal("threshold fired before 50 outcomes")
		}
	}
	for i := 0; i < 5; i++ {
		accepted, crossed := w.record("A", rejectResult(fmt.Sprintf("reject-%d", i)), true)
		if !accepted || crossed {
			t.Fatal("five rejects before/at the boundary crossed the strict threshold")
		}
	}
	if _, crossed := w.record("A", rest.CreateResult{Coid: "fiftieth", Outcome: rest.CreateAcked, Status: 200}, true); crossed {
		t.Fatal("5 of 50 is exactly 10%, not over it")
	}
	if accepted, crossed := w.record("A", rejectResult("reject-0"), true); accepted || crossed {
		t.Fatal("the same coid's attempts were counted twice")
	}
	if accepted, crossed := w.record("A", rejectResult("unsent"), false); accepted || crossed {
		t.Fatal("an unsent refusal entered the window")
	}
	if _, crossed := w.record("B", rejectResult("other-market"), true); crossed {
		t.Fatal("B's first rejection inherited A's history")
	}
	if _, crossed := w.record("A", rejectResult("sixth-reject"), true); !crossed {
		t.Fatal("6 of the rolling last 50 did not cross the threshold")
	}
	if w.markets["A"].rejects != 6 || w.markets["B"].rejects != 1 {
		t.Fatalf("markets were mixed: A=%d B=%d", w.markets["A"].rejects, w.markets["B"].rejects)
	}
}

func TestRejectWindowUnknownAnd429DoNotBecomeDefiniteRejects(t *testing.T) {
	var w rejectWindow
	for i := 0; i < 44; i++ {
		w.record("A", rest.CreateResult{Coid: fmt.Sprintf("ok-%d", i), Outcome: rest.CreateAcked, Status: 200}, true)
	}
	for i := 0; i < 5; i++ {
		w.record("A", rejectResult(fmt.Sprintf("reject-%d", i)), true)
	}
	for _, result := range []rest.CreateResult{
		{Coid: "ambiguous", Outcome: rest.CreateUnknown, Status: 503, RejectReason: "some_exchange_code"},
		{Coid: "throttled", Outcome: rest.CreateUnknown, Status: 429, RejectReason: "some_exchange_code"},
		{Coid: "no-code", Outcome: rest.CreateRejected, Status: 400},
		{Coid: "local", Outcome: rest.CreateRejected, Status: 0, RejectReason: "some_exchange_code"},
	} {
		if _, crossed := w.record("A", result, true); crossed {
			t.Fatalf("uncertain or unparseable outcome crossed F9: %+v", result)
		}
	}
	if w.markets["A"].rejects != 5 {
		t.Fatalf("uncertain outcomes counted as rejects: %d", w.markets["A"].rejects)
	}
}

func TestRejectWindowUpgradesSameCoidWithoutAddingAnotherOrder(t *testing.T) {
	var w rejectWindow
	for i := 0; i < 49; i++ {
		w.record("A", rest.CreateResult{Coid: fmt.Sprintf("ok-%d", i), Outcome: rest.CreateAcked, Status: 200}, true)
	}
	if accepted, crossed := w.record("A", rest.CreateResult{Coid: "retried", Outcome: rest.CreateUnknown, Status: 429}, true); !accepted || crossed {
		t.Fatal("429 did not occupy its single sent-create slot")
	}
	if accepted, crossed := w.record("A", rejectResult("retried"), true); !accepted || crossed {
		t.Fatal("definite retry result did not upgrade the existing slot")
	}
	if m := w.markets["A"]; m.n != 50 || m.rejects != 1 {
		t.Fatalf("retry counted as another order: n=%d rejects=%d", m.n, m.rejects)
	}
	if accepted, crossed := w.record("A", rejectResult("retried"), true); accepted || crossed {
		t.Fatal("replayed terminal result was counted again")
	}
}

func TestOwnerCountsSent429BeforeRetryEarlyReturn(t *testing.T) {
	g := newGateFailOwner(t, 0)
	req := placement(t, g.h.rig, quote.SideYes, quote.RoleAdding, 40, 1)
	req.attempts = 1
	coid := req.Order.ClientOrderID()
	g.o.applyWriteResult(writeResult{Req: req, Sent: true, Create: rest.CreateResult{
		Coid: coid, Outcome: rest.CreateUnknown, Status: 429,
		Err: &rest.RateLimitError{}, MaxLive: req.Order.Count(),
	}})
	m := g.o.rejects.markets[req.Market]
	if m == nil || m.n != 1 || m.rejects != 0 || len(g.o.retries) != 1 {
		t.Fatalf("429 did not enter the one-slot window before retry: window=%+v retries=%d", m, len(g.o.retries))
	}
}

func TestOwnerRejectWindowMovesSelectedMarketToReducing(t *testing.T) {
	g := newGateFailOwner(t, num.QtyFromFloat(1))
	for i := 0; i < 44; i++ {
		g.o.observeRejectResponse(seamTicker, rest.CreateResult{Coid: fmt.Sprintf("ok-%d", i), Outcome: rest.CreateAcked, Status: 200}, true)
	}
	for i := 0; i < 5; i++ {
		g.o.observeRejectResponse(seamTicker, rejectResult(fmt.Sprintf("reject-%d", i)), true)
	}
	g.o.observeRejectResponse(seamTicker, rest.CreateResult{Coid: "fiftieth", Outcome: rest.CreateAcked, Status: 200}, true)
	if g.h.rig.gate.Reducing(seamTicker) || g.o.reduceNoted {
		t.Fatal("the owner reduced at exactly five rejects")
	}
	g.o.observeRejectResponse(seamTicker, rejectResult("sixth"), true)
	if !g.h.rig.gate.Reducing(seamTicker) || !g.o.reduceNoted {
		t.Fatal("the F9 threshold did not reach the gate and owner")
	}
	g.o.evaluate(g.h.clk.Now().Mono)
	if g.o.market != quote.Reducing {
		t.Fatalf("owner market=%v, want REDUCING", g.o.market)
	}
	if sevs := queuedAnomalySevs(g.h.anom, "ORDER_REJECT_RATE"); len(sevs) != 1 || sevs[0] != risk.SEV2 {
		t.Fatalf("F9 alerts=%v, want one SEV2", sevs)
	}
}

func TestPostOnlyCrossForcesActualRESTReadThroughF5(t *testing.T) {
	g := newGateFailOwner(t, num.QtyFromFloat(0))
	h, o := g.h, g.o
	doer := &f5Doer{base: h.rig.api.Doer, yesSize: "19.00"}
	h.rig.api.Doer = doer
	requests := make(chan wsx.CrossCheckRequest, 1)
	results := make(chan crossCheckResult, 1)
	o.crossChecks = requests
	ctx, cancel := context.WithCancel(h.ctx)
	done := make(chan struct{})
	go func() { defer close(done); h.rig.crossCheckLoop(ctx, requests, results) }()
	defer func() { cancel(); <-done }()
	if !h.rig.gate.BookCurrent(seamTicker) {
		t.Fatal("setup: no current websocket book")
	}
	result := rest.CreateResult{Coid: "first", Outcome: rest.CreateRejected, Status: 400, RejectReason: "post_only_would_cross"}
	o.observeRejectResponse(seamTicker, result, true)
	if h.rig.gate.Actionable(seamTicker, h.clk.Now()) {
		t.Fatal("F11 left placement licensed while its cross-check was outstanding")
	}
	select {
	case read := <-results:
		if !read.request.Token.Valid() || read.request.Ticker != seamTicker || read.book.Outcome != rest.OrderbookRead {
			t.Fatalf("F11 did not execute a valid REST orderbook read: %+v", read)
		}
		commands := make(chan wsx.Command, 1)
		o.wsCommands = commands
		o.applyCrossCheck(read)
		if !h.rig.gate.F5Quarantined(seamTicker) || !h.rig.gate.Reducing(seamTicker) || !o.reduceNoted {
			t.Fatal("F5 size disagreement did not quarantine and reduce the market")
		}
		if len(commands) != 1 || (<-commands).Kind != wsx.CmdResnapshot {
			t.Fatal("F5 mismatch did not request owner recovery")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("F11 did not issue the REST read immediately")
	}
	if doer.count() != 1 {
		t.Fatalf("REST orderbook reads=%d, want one", doer.count())
	}
	result.Coid = "second"
	o.observeRejectResponse(seamTicker, result, true)
	if sevs := queuedAnomalySevs(h.anom, "POST_ONLY_WOULD_CROSS_REPEATED"); len(sevs) != 1 || sevs[0] != risk.SEV2 {
		t.Fatalf("repeated post_only alarm=%v, want one SEV2", sevs)
	}
	if doer.count() != 1 {
		t.Fatal("quarantined book triggered another cross-check")
	}
}
