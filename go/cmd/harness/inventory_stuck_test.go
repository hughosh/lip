package main

import (
	"testing"
	"time"

	"lip/harness/num"
	"lip/harness/quote"
	"lip/harness/risk"
)

func TestInventoryStuckTrackerBoundariesAndEpisodes(t *testing.T) {
	hard := num.QtyFromFloat(7)
	limit := 10 * time.Second
	maxAge := 20 * time.Second
	for _, tc := range []struct {
		name string
		q    num.Qty
	}{
		{"long YES", num.QtyFromFloat(7.01)},
		{"long NO", num.QtyFromFloat(-7.01)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var s inventoryStuckTracker
			read := func(at time.Duration, q num.Qty) {
				s.observe(at, 3, map[string]num.Qty{"A": q}, hard)
			}
			step := func(at, age time.Duration, gen uint64) bool {
				a, ok := s.step("A", at, gen, age, maxAge, limit)
				if ok && (a.Class != "INVENTORY_STUCK" || a.Sev != risk.SEV2 || a.Ticker != "A") {
					t.Fatalf("wrong F16 anomaly: %+v", a)
				}
				return ok
			}
			read(0, tc.q)
			if step(limit-time.Nanosecond, 0, 3) || step(limit, 0, 3) {
				t.Fatal("F16 fired before elapsed time was strictly greater than stuck_s")
			}
			if !step(limit+time.Nanosecond, 0, 3) || step(2*limit, 0, 3) {
				t.Fatal("F16 did not fire exactly once after stuck_s")
			}
			// Incomplete/stale truth does not clear a fired episode. The next
			// accepted high poll is a continuation, not a second episode.
			if step(3*limit, maxAge+time.Nanosecond, 3) || step(3*limit, 0, 4) {
				t.Fatal("stale truth or an old generation fired F16")
			}
			read(4*limit, tc.q)
			if step(4*limit, 0, 3) {
				t.Fatal("a continuing breach fired twice")
			}
			read(5*limit, hard)
			if step(5*limit, 0, 3) {
				t.Fatal("exactly inv_hard did not end the episode")
			}
			read(6*limit, tc.q)
			if step(7*limit, 0, 3) || !step(7*limit+time.Nanosecond, 0, 3) {
				t.Fatal("recovery did not rearm a second full episode")
			}
		})
	}
}

func TestInventoryStuckTrackerRequiresCompleteCurrentTruthAndMonotonicTime(t *testing.T) {
	hard := num.QtyFromFloat(7)
	high := num.QtyFromFloat(7.01)
	limit := 10 * time.Second
	var s inventoryStuckTracker
	if _, ok := s.step("A", 11*time.Second, 1, 0, time.Minute, limit); ok {
		t.Fatal("a restart inherited an episode without a complete walk")
	}
	s.observe(0, 1, map[string]num.Qty{"A": hard}, hard)
	if _, ok := s.step("A", 11*time.Second, 1, 0, time.Minute, limit); ok {
		t.Fatal("the exact threshold began an episode")
	}
	s.observe(20*time.Second, 1, map[string]num.Qty{"A": high}, hard)
	// A wall-clock step does not affect this monotonic API. A stale or
	// previous-generation read cannot authorize the alert at either boundary.
	for _, in := range []struct {
		at  time.Duration
		gen uint64
		age time.Duration
	}{
		{31 * time.Second, 2, 0},
		{31 * time.Second, 1, -1},
		{31 * time.Second, 1, time.Minute + 1},
	} {
		if _, ok := s.step("A", in.at, in.gen, in.age, time.Minute, limit); ok {
			t.Fatalf("F16 fired without current fresh truth: %+v", in)
		}
	}
	// The first complete walk on generation 2 continues the old episode;
	// no incomplete or stale observation claimed a recovery.
	s.observe(32*time.Second, 2, map[string]num.Qty{"A": high}, hard)
	if _, ok := s.step("A", 32*time.Second, 2, 0, time.Minute, limit); !ok {
		t.Fatal("a current complete walk did not reveal the sustained breach")
	}
	// A complete omission is an authoritative flat observation and ends it.
	s.observe(33*time.Second, 2, nil, hard)
	s.observe(34*time.Second, 2, map[string]num.Qty{"A": high}, hard)
	if _, ok := s.step("A", 35*time.Second, 2, 0, time.Minute, limit); ok {
		t.Fatal("a complete flat observation did not reset the clock")
	}
}

func TestInventoryStuckTrackerIsPerMarketAndIgnoresWallStep(t *testing.T) {
	hard := num.QtyFromFloat(7)
	limit := 10 * time.Second
	clk := newSeamClock()
	var s inventoryStuckTracker
	s.observe(clk.monoNow(), 1, map[string]num.Qty{
		"A": num.QtyFromFloat(7.01), "B": num.QtyFromFloat(-7),
	}, hard)
	clk.mu.Lock()
	clk.wall += int64(time.Hour / time.Millisecond)
	clk.mu.Unlock()
	if _, ok := s.step("A", clk.monoNow(), 1, 0, time.Minute, limit); ok {
		t.Fatal("a forward wall step shortened stuck_s")
	}
	clk.Advance(limit)
	if _, ok := s.step("A", clk.monoNow(), 1, 0, time.Minute, limit); ok {
		t.Fatal("F16 fired at exactly stuck_s after a wall step")
	}
	clk.Advance(time.Nanosecond)
	if _, ok := s.step("A", clk.monoNow(), 1, 0, time.Minute, limit); !ok {
		t.Fatal("the monotonic duration did not fire F16")
	}
	if _, ok := s.step("B", clk.monoNow(), 1, 0, time.Minute, limit); ok {
		t.Fatal("the other market at exactly inv_hard inherited A's episode")
	}
}

// This reaches F16 through a real owner loop, accepted poller reads and the
// durable anomaly journal. The short configured duration is deliberately far
// from the §16 default, so ignoring Params.Stuck cannot pass this test.
func TestComposedInventoryStuckUsesConfiguredDurationAndRearms(t *testing.T) {
	h := newSeamHarness(t, seamOptions{
		ReadOnly:  true,
		Positions: map[string]string{seamTicker: "7.01"},
	})
	h.rig.cfg.Params.Stuck = time.Second
	h.start()
	h.awaitActionable()
	if m, ok := h.market(); !ok || m.State != quote.Reducing {
		t.Fatalf("an initial 7.01 position should be REDUCING: %+v", m)
	}

	h.clk.Advance(time.Second)
	h.awaitTicks(2)
	if got := h.anomalySevs("INVENTORY_STUCK"); len(got) != 0 {
		t.Fatalf("F16 fired at exactly stuck_s: %v", got)
	}
	h.clk.Advance(time.Nanosecond)
	h.await("one durable F16 SEV2 just beyond stuck_s", func() bool {
		return len(h.anomalySevs("INVENTORY_STUCK")) == 1
	})
	if got := h.anomalySevs("INVENTORY_STUCK"); got[0] != risk.SEV2 {
		t.Fatalf("F16 severity = %v, want SEV2", got)
	}
	h.clk.Advance(time.Second)
	h.awaitTicks(2)
	if got := h.anomalySevs("INVENTORY_STUCK"); len(got) != 1 {
		t.Fatalf("one breach emitted %d F16 anomalies, want one", len(got))
	}
	if got := h.latchTrigger(); got != "" {
		t.Fatalf("informational F16 caused a global halt: %q", got)
	}

	// A complete read at the exact hard threshold resets the episode. The
	// fake poller is the only source of this truth; owner state is untouched.
	h.ex.setPosition(seamTicker, "7.00")
	h.clk.Advance(h.cfg.Params.PositionPoll)
	h.await("a complete threshold position poll", func() bool {
		m, ok := h.market()
		return ok && m.Q == num.QtyFromFloat(7)
	})
	h.ex.setPosition(seamTicker, "-7.01")
	h.clk.Advance(h.cfg.Params.PositionPoll)
	h.await("a complete negative breach position poll", func() bool {
		m, ok := h.market()
		return ok && m.Q == num.QtyFromFloat(-7.01)
	})
	h.clk.Advance(time.Second + time.Nanosecond)
	h.await("a second F16 episode", func() bool {
		return len(h.anomalySevs("INVENTORY_STUCK")) == 2
	})
	if got := h.anomalySevs("INVENTORY_STUCK"); got[1] != risk.SEV2 {
		t.Fatalf("second episode severity = %v, want SEV2", got)
	}
}
