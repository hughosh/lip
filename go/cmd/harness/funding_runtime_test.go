package main

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"lip/harness/num"
	"lip/harness/quote"
	"lip/harness/rest"
	"lip/harness/risk"
)

func TestFundingCashGuardReservesUnknownAndNeverUsesPositionAsCash(t *testing.T) {
	g := capitalOwner(t, num.QtyFromFloat(1), 100)
	g.o.r.cfg.CapitalSource = "selected_shard_balance"
	g.o.r.cfg.Funding = &fundingEvidence{}
	g.o.r.funding = new(atomic.Pointer[fundingObservation])
	g.o.r.funding.Store(&fundingObservation{available: num.MoneyFromDollars(.60), at: g.o.r.ex.Mono(), valid: true})
	reduce := capitalOrder(t, g, quote.SideNo, quote.RoleReducing, 55, 1, 220)
	if err := g.o.checkPlacementCapital(reduce); err != nil {
		t.Fatalf("held position was subtracted from available cash: %v", err)
	}
	unknown := capitalOrder(t, g, quote.SideYes, quote.RoleAdding, 40, 1, 221)
	g.o.pending[unknown.Order.ClientOrderID()] = pendingOrder{side: unknown.Side, cents: 40, qty: unknown.Order.Count()}
	if err := g.o.checkPlacementCapital(reduce); err == nil {
		t.Fatal("unknown commitment not reserved from cash")
	}
	delete(g.o.pending, unknown.Order.ClientOrderID())
	g.o.r.funding.Store(&fundingObservation{available: num.MoneyFromDollars(.54), at: g.o.r.ex.Mono(), valid: true})
	if err := g.o.checkPlacementCapital(reduce); err == nil {
		t.Fatal("reducer exceeded real cash despite ample configured capital")
	}
}

func TestFundingRecoveryBlocksAddsButPreservesReducerAndCancellation(t *testing.T) {
	g := capitalOwner(t, num.QtyFromFloat(1), 100)
	g.o.r.cfg.CapitalSource = "selected_shard_balance"
	g.o.r.cfg.Funding = &fundingEvidence{RecoveryOnly: true}
	g.o.r.funding = new(atomic.Pointer[fundingObservation])
	g.o.r.funding.Store(&fundingObservation{available: num.MoneyFromDollars(10), at: g.o.r.ex.Mono(), valid: true})
	add := capitalOrder(t, g, quote.SideYes, quote.RoleAdding, 40, 1, 222)
	reduce := capitalOrder(t, g, quote.SideNo, quote.RoleReducing, 55, 1, 223)
	if err := g.o.checkPlacementCapital(add); err == nil {
		t.Fatal("recovery admitted adding")
	}
	if err := g.o.checkPlacementCapital(reduce); err != nil {
		t.Fatal(err)
	}
	g.o.r.funding.Store(&fundingObservation{})
	if err := g.o.checkPlacementCapital(reduce); err == nil {
		t.Fatal("failed cash observation admitted a create")
	}
	if err := g.o.checkPlacementCapital(writeRequest{Op: quote.OpCancel}); err != nil {
		t.Fatalf("failed cash blocked cancel: %v", err)
	}
}

func TestFundingStaleObservationCannotAuthorizeCreate(t *testing.T) {
	g := capitalOwner(t, 0, 100)
	g.o.r.cfg.CapitalSource = "selected_shard_balance"
	g.o.r.cfg.Funding = &fundingEvidence{}
	g.o.r.funding = new(atomic.Pointer[fundingObservation])
	g.o.r.funding.Store(&fundingObservation{available: num.MoneyFromDollars(100), at: g.o.r.ex.Mono() - 2*g.o.p.TruthMaxAge, valid: true})
	add := capitalOrder(t, g, quote.SideYes, quote.RoleAdding, 40, 1, 224)
	if err := g.o.checkPlacementCapital(add); err == nil {
		t.Fatal("stale funding authorized adding")
	}
}

func TestFundingSourceRefreshDoesNotRaiseRunCapAndFailureInvalidatesCash(t *testing.T) {
	script := &fundingScript{t: t, available: "9858"}
	latest := new(atomic.Pointer[fundingObservation])
	s := &fundedPortfolioSource{Client: rest.NewClient(script), ticker: "T", index: 2, mono: func() time.Duration { return time.Second }, latest: latest}
	if _, err := s.Balance(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f := latest.Load(); f == nil || !f.valid || f.available != num.MoneyFromDollars(98.58) {
		t.Fatalf("missing scoped cash: %+v", f)
	}
	s.index = 3
	if _, err := s.Balance(context.Background()); err == nil {
		t.Fatal("changed shard accepted")
	}
	if latest.Load().valid {
		t.Fatal("failed refresh retained authority")
	}
}

func TestFundingHealthUsesTimeAfterConcurrentCashPublication(t *testing.T) {
	g := capitalOwner(t, 0, 100)
	g.o.r.cfg.CapitalSource = "selected_shard_balance"
	g.o.r.funding = new(atomic.Pointer[fundingObservation])
	ownerTick := g.o.r.ex.Mono()
	// A successful balance read can publish after the owner sampled its tick
	// time, but before evaluate loads the immutable cash observation.
	g.h.clk.Advance(time.Millisecond)
	g.o.r.funding.Store(&fundingObservation{
		available: num.MoneyFromDollars(100), at: g.o.r.ex.Mono(), valid: true,
	})
	g.o.evaluate(ownerTick)
	if g.o.global != quote.Running || g.h.latchTrigger() != "" {
		t.Fatalf("fresh concurrently published cash stopped owner: state=%s latch=%q", g.o.global, g.h.latchTrigger())
	}
}

func TestFundingHealthStillStopsInvalidStaleAndFutureCash(t *testing.T) {
	for _, kind := range []string{"missing", "invalid", "stale", "future"} {
		t.Run(kind, func(t *testing.T) {
			g := capitalOwner(t, 0, 100)
			g.o.r.cfg.CapitalSource = "selected_shard_balance"
			g.o.r.funding = new(atomic.Pointer[fundingObservation])
			now := g.o.r.ex.Mono()
			f := &fundingObservation{available: num.MoneyFromDollars(100), at: now, valid: true}
			switch kind {
			case "missing":
				f = nil
			case "invalid":
				f.valid = false
			case "stale":
				f.at = now - g.o.p.TruthMaxAge - time.Nanosecond
			case "future":
				f.at = now + time.Nanosecond
			}
			g.o.r.funding.Store(f)
			g.o.evaluate(now)
			if g.o.global == quote.Running || g.h.latchTrigger() != "funding_unavailable" {
				t.Fatalf("%s cash did not durably stop owner: state=%s latch=%q", kind, g.o.global, g.h.latchTrigger())
			}
			found := false
			for _, a := range g.h.takeRaised() {
				if a.Class == "FUNDING_UNAVAILABLE" && a.Sev == risk.SEV1 {
					found = true
				}
			}
			if !found {
				t.Fatal("funding stop did not raise an immediate SEV1 diagnostic")
			}
		})
	}
}

func TestFundingHealthDiagnosticDoesNotFloodWhenLatchWriteFails(t *testing.T) {
	h := newSeamHarness(t, seamOptions{LatchDirMissing: true})
	o := h.ownerFor()
	o.r.cfg.CapitalSource = "selected_shard_balance"
	o.r.funding = new(atomic.Pointer[fundingObservation])
	for i := 0; i < 20; i++ {
		o.checkFundingHealth()
	}
	if !o.stopHeld || o.global != quote.Running {
		t.Fatal("fixture did not preserve an undurable stop while adding is blocked")
	}
	count := 0
	for _, a := range h.takeRaised() {
		if a.Class == "FUNDING_UNAVAILABLE" {
			count++
			if a.Sev != risk.SEV1 {
				t.Fatal("funding diagnostic lost immediate severity")
			}
		}
	}
	if count != 1 {
		t.Fatalf("%d funding diagnostics during one held stop; want one", count)
	}
}
