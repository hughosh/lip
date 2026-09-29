package main

import (
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"lip/harness/num"
	"lip/harness/quote"
	"lip/harness/rest"
	"lip/harness/risk"
	"lip/harness/wsx"
)

// These tests are H-Q-4a: "after gate_fail_debounce_s (30s) of continuous gate
// failure, cancel and exchange-confirm every ADDING order. If q != 0, keep only
// the aggregate-capped reducer (H-Q-5a). Re-enter only from a fresh, complete,
// qualifying book."
//
// # What the rule is for, and what it replaced
//
// Red-team HR-012 deleted the earlier rule ("do not cancel what is already
// resting") and said why: reward in a gated-out interval is ZERO FOR EVERY
// PARTICIPANT, but our orders stayed live and fillable, taking directional
// inventory during an interval that could not pay -- and H-Q-4's gated-out
// accounting excludes the interval from our uptime denominator, so the loss was
// invisible in the metric. The debounce is the part of the old rule that was
// worth keeping: churn protection against transient blips.
//
// So the shape is a three-part obligation and every part can fail on its own:
//
//	 t < start          nothing; the book qualifies
//	 start .. start+D   place NOTHING, cancel NOTHING     (H-Q-4 + the debounce)
//	 start+D onwards    cancel and SWEEP every adding order, keep only the
//	                    aggregate-capped reducer          (H-Q-4a)
//
// # Why the interval is timed from BOOK evidence
//
// `wsx.Gate.BookCurrent` exists for this and is deliberately not `Actionable`.
// A13's predicate folds in portfolio freshness; if the episode were timed from
// it, a stale positions walk would CLEAR the episode, and the adding order this
// rule exists to retire would rest through both conditions at once. The
// converse also matters: A13 still owns DISPATCH authority, so clearing an
// episode never resumes adding on its own (H-FAIL-4, H-ORD-5).
//
// # Why the composed fixture drives `serve`
//
// `Actionable` cannot be forged. `Gate.noteTruth` is private precisely so that
// no test can manufacture a complete reconciliation -- "an exported version is a
// SetActionable(true) with a longer name" -- so the only way to reach a market
// that may really place is to run the real startup walk, the real poller and the
// real dispatcher against the fake exchange. The owner-level tests below take
// the other half: they drive `applyEvent` and `evaluate` directly, where
// `Actionable` is false and CANCELS are therefore the observable, which is I1's
// asymmetry and exactly the half H-Q-4a's expiry turns on.

// gateFailDebounce is the configured interval these tests run against.
//
// It is a NON-DEFAULT literal on purpose, and it is distinct from every other
// duration that could plausibly be substituted for it:
//
//   - not `gate_fail_debounce_s`'s own default of 30s, so a build that ignored
//     the configuration and hardcoded §16's number never reaches the response
//     inside any of these tests;
//   - not H-Q-7's `debounce_s` of 250ms, which is the requote debounce and is
//     also the owner's tick, so a build that reached for the nearest available
//     duration fires four times too early and the just-below assertions catch it;
//   - a whole number of the owner's 250ms ticks, so "exactly at the boundary" is
//     an instant the loop really evaluates at rather than one it steps over.
const gateFailDebounce = 4 * time.Second

// gateFailS is the quote size, one contract.
//
// Not cosmetic. `quote.ExternalBest` subtracts our own resting size from the
// level it sits on (H-Q-10), so a fixture that quotes 12 contracts into a level
// carrying 2 leaves NO external touch on that side -- and then "nothing was
// placed" would be attributable to the missing touch rather than to the gate.
// At S = 1 every book below keeps a positive external touch on both sides in
// both the qualifying and the non-qualifying state, so the gate is the only
// thing that can explain a difference.
var gateFailS = num.QtyFromFloat(1)

// The two books. They differ ONLY in the depth of the NO side, and the touch
// prices are identical in both, so nothing §6.5 reads changes when the market
// stops qualifying -- no requote is triggered, no touch is lost, and the
// qualifying walk is the single variable.
//
// `seamTarget` is 10 contracts. 20 on both sides qualifies; 2 on the no side
// does not, and `core.Book.Qualifies` clears the whole walk when either side
// fails to accumulate to target.
func gateFailQualifyingBook() (yes, no [][]string) {
	return [][]string{{"0.4000", "20.00"}}, [][]string{{"0.5500", "20.00"}}
}

func gateFailThinBook() (yes, no [][]string) {
	return [][]string{{"0.4000", "20.00"}}, [][]string{{"0.5500", "2.00"}}
}

// ---------------------------------------------------------------------------
// The fixture
// ---------------------------------------------------------------------------

// newGateFailHarness is `newSeamHarness` with two things it has no option for:
// a configured `gate_fail_debounce_s` and S = 1.
//
// It builds the fakes with `SkipRig` and then constructs the REAL `newRig` over
// the mutated config, so what the rig receives is the configuration this test
// chose. That is the point of the whole exercise: `cfg.Params.GateFailDebounce`
// had no production reader at all before `lip-732`, and a test that asserted
// against a hardcoded 30s could not tell a wired parameter from an ignored one.
func newGateFailHarness(t *testing.T, opt seamOptions) *seamHarness {
	t.Helper()

	opt.SkipRig = true
	h := newSeamHarness(t, opt)

	h.cfg.Params.GateFailDebounce = gateFailDebounce
	h.cfg.Params.S = gateFailS
	if err := h.cfg.Params.Validate(); err != nil {
		t.Fatalf("the gate-failure fixture's own configuration is invalid: %v", err)
	}

	h.anom = newAnomalySink()
	r, err := newRig(h.ctx, h.cfg, opt.Resume, h.xch, seamAlertFactory, h.anom,
		h.qual)
	if err != nil {
		t.Fatalf("newRig: %v", err)
	}
	h.rig = r
	t.Cleanup(h.closeRig)

	// The parameter really reached the rig. Everything below reads it through
	// `owner.p`, which is copied from here, so a fixture that silently kept the
	// default would make every timing assertion below assert nothing.
	if got := h.rig.cfg.Params.GateFailDebounce; got != gateFailDebounce {
		t.Fatalf("the rig holds gate_fail_debounce_s = %v, want the configured "+
			"%v; the parameter is not reaching the composition root at all",
			got, gateFailDebounce)
	}
	return h
}

// gateFailBook pushes one orderbook snapshot into the live socket.
func (h *seamHarness) gateFailBook(yes, no [][]string) {
	h.t.Helper()
	frame, err := h.ws.frame("orderbook_snapshot", map[string]any{
		"market_ticker":  seamTicker,
		"ts_ms":          1,
		"yes_dollars_fp": yes,
		"no_dollars_fp":  no,
	})
	if err != nil {
		h.t.Fatalf("building a snapshot frame: %v", err)
	}
	h.ws.frames <- frame
}

// gateFailAwaitGated waits for the published snapshot to report the market's own
// qualifying walk as failing, or as passing.
//
// `risk.MarketSnap.Gated` is `book.Qualifies() == 0` published by the owner, so
// waiting on it establishes that the frame reached core, that core accepted it,
// and that the owner has run at least one evaluation since -- which is exactly
// the state the episode is folded in from.
func (h *seamHarness) gateFailAwaitGated(want bool) {
	h.t.Helper()
	h.await(fmt.Sprintf("the published market to report Gated = %t", want),
		func() bool {
			m, ok := h.market()
			return ok && m.Gated == want
		})
}

// gateFailCreateOn is the create this run made on one side, by H-ORD-1's coid.
func gateFailCreateOn(h *seamHarness, side quote.Side) (seamCreate, bool) {
	for _, c := range h.ex.allCreates() {
		p, ours := rest.ParseCoid(c.Coid)
		if ours && p.Side == side {
			return c, true
		}
	}
	return seamCreate{}, false
}

func gateFailDeleted(h *seamHarness, orderID string) bool {
	for _, id := range h.ex.deletedIDs() {
		if id == orderID {
			return true
		}
	}
	return false
}

// gateFailStepWall moves the WALL clock and leaves the monotonic clock alone.
//
// `seamClock.Advance` deliberately moves both, because that is what the passage
// of time does. F21 is the other thing: an NTP correction, a manual `date`, a
// VM resuming with a rewritten wall clock. There is no fixture method for it
// because nothing in the harness is supposed to care, and this test exists to
// prove that H-Q-4a's interval is one of the things that does not.
func gateFailStepWall(c *seamClock, d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.wall += int64(d / time.Millisecond)
}

// ---------------------------------------------------------------------------
// 1. The composed lifecycle: just below, exactly at, and past the debounce
// ---------------------------------------------------------------------------

// TestSustainedGateFailureCancelsTheAddingSideAtTheConfiguredDebounce is
// H-Q-4a end to end, through the real owner queue, the real dispatcher, the fake
// REST exchange and H-ORD-4's verifying orders walk.
//
// The market is FLAT, and that is what the composed fixture can hold in QUOTING
// rather than a convenience. §5.2's REDUCING "leaves only at exactly zero", and
// a composed run starts with §9's schedule unread -- `closeUnknown` stops the
// market on the first tick, so any harness seeded with inventory is in REDUCING
// before the schedule lands and stays there for the life of the process, with
// no adding order for this rule to retire. The three-sign mapping is therefore
// asserted at the owner level, where the schedule is in place before the first
// evaluation: `TestTheStoppedMarketKeepsTheCappedExitOnEitherSign`.
//
// What this one proves is the wiring, which no owner-level test can: the
// configured `gate_fail_debounce_s` reaches the production consumer, and at the
// far end of it a real DELETE and a real verifying orders walk reach a real
// exchange.
//
// The three timing checkpoints are the falsification target. Just below the
// configured debounce nothing may move, at exactly the debounce the response
// must have begun on THAT evaluation rather than one tick later, and above it
// the state is preserved rather than re-litigated. A build that used H-Q-7's
// 250ms fails the first; a build that kept §16's 30s default, or compared with a
// strict `>`, fails the second -- the monotonic clock here is virtual and does
// not advance on its own, so a threshold that is never reached is never reached
// at all.
func TestSustainedGateFailureCancelsTheAddingSideAtTheConfiguredDebounce(
	t *testing.T) {

	h := newGateFailHarness(t, seamOptions{ListCreated: true})
	h.start()

	h.awaitActionable()

	// The steady state this rule acts on: a QUOTING market with our own orders
	// resting on both sides of a book that qualifies.
	const wantOrders = 2
	h.await("the market to reach QUOTING", func() bool {
		m, ok := h.market()
		return ok && m.State == quote.Quoting
	})
	h.await("both sides to be resting on the exchange", func() bool {
		return h.ex.restingCount() >= wantOrders
	})
	if m, _ := h.market(); m.Gated {
		t.Fatalf("the opening book is already reported as gated; the whole " +
			"test measures the transition into that state")
	}

	yes, ok := gateFailCreateOn(h, quote.SideYes)
	if !ok {
		t.Fatal("no create was made on the YES side")
	}
	no, ok := gateFailCreateOn(h, quote.SideNo)
	if !ok {
		t.Fatal("no create was made on the NO side")
	}

	createsBefore := h.ex.createCount()
	deletesBefore := h.ex.deleteCount()

	// --- the book stops qualifying ------------------------------------------
	//
	// Same prices, less depth on the no side. Nothing §6.5 reads has changed,
	// so anything that happens from here is the qualifying walk and nothing
	// else.
	h.gateFailBook(gateFailThinBook())
	h.gateFailAwaitGated(true)

	// A SECOND non-qualifying snapshot inside the window, and this one moves
	// the external YES touch a tick AGAINST our resting order.
	//
	// Two things at once, both of them load-bearing:
	//
	//   - it is what §6.5 requotes on (H-Q-6), and H-Q-9 is off for the pilot,
	//     so the write it produces is a cancel-confirm-place whose FIRST LEG IS
	//     A CANCEL. `Conditions.Valid` deliberately never drops a cancel -- "it
	//     can only reduce exposure" -- so the only thing standing between a
	//     non-qualifying book and a requote cancel is `evaluate` declining to
	//     decide the side at all. That is the half of the debounce HR-012 kept:
	//     churn on transient blips, and this window is exactly a blip.
	//   - it is a continuing failure, which must NOT restart the interval. If
	//     the start were re-stamped here the deadline below would never arrive.
	h.gateFailBook([][]string{{"0.4100", "20.00"}},
		[][]string{{"0.5500", "2.00"}})
	h.await("the owner to observe the moved external touch", func() bool {
		m, ok := h.market()
		return ok && m.Sides[quote.SideYes].TouchCents == 41
	})

	// --- start + D - one tick: NOTHING moves --------------------------------
	//
	// Split in two so that H-Q-7's 250ms requote debounce is satisfied WELL
	// inside the window: a requote that had not yet cleared its own brake would
	// make "no cancel went out" true for the wrong reason.
	h.clk.Advance(h.cfg.Params.Debounce + ownerTick)
	h.awaitTicks(3)
	h.clk.Advance(gateFailDebounce - 2*ownerTick - h.cfg.Params.Debounce)
	h.awaitTicks(3)

	if got := h.ex.deleteCount(); got != deletesBefore {
		t.Fatalf("%d cancel(s) reached the exchange %v into a %v debounce, want "+
			"none.\n\nHR-012 kept the debounce for exactly this: \"the debounce "+
			"is what the original rule was really protecting: churn on transient "+
			"blips\". A book that recovers inside the interval must cost nothing "+
			"at all, and a cancel here is a presence gap and a write we paid for",
			got-deletesBefore, gateFailDebounce-ownerTick, gateFailDebounce)
	}
	if got := h.ex.createCount(); got != createsBefore {
		t.Fatalf("%d placement(s) reached the exchange from a NON-QUALIFYING "+
			"book.\n\nH-Q-4: \"the snapshot is excluded for everyone (S3's gate) "+
			"and our presence earns nothing. Do not place.\" That applies from the "+
			"first frame -- the debounce delays the CANCEL, not the refusal to add",
			got-createsBefore)
	}
	if got := h.ex.restingCount(); got < wantOrders {
		t.Fatalf("%d of our orders rest inside the debounce, want %d; resting "+
			"orders are preserved through the window", got, wantOrders)
	}
	if m, _ := h.market(); m.State != quote.Quoting {
		t.Fatalf("the market is %s inside the debounce, want QUOTING; the "+
			"market-scoped stop belongs at the far end of the interval and not "+
			"inside it", m.State)
	}

	// --- exactly start + D: the response begins ------------------------------
	h.clk.Advance(ownerTick)
	h.await("both adding orders to be cancelled at the exchange", func() bool {
		return gateFailDeleted(h, yes.OrderID) && gateFailDeleted(h, no.OrderID)
	})
	h.await("the market to reach IDLE", func() bool {
		m, ok := h.market()
		return ok && m.State == quote.Idle
	})
	h.await("both sides to be off the exchange, verified by the sweep's own "+
		"resting-orders walk", func() bool { return h.ex.restingCount() == 0 })

	// The SEV2 the operator reads, once per episode.
	h.await("the sustained gate failure to be recorded", func() bool {
		return seamContains(h.anomalyClasses(), "BOOK_GATE_SUSTAINED")
	})

	// --- strictly above D: preserved, not re-litigated -----------------------
	deletesAtStop := h.ex.deleteCount()
	createsAtStop := h.ex.createCount()
	walksAtStop := h.ex.ordersWalkCount()
	h.clk.Advance(ownerTick)
	h.awaitTicks(6)

	if got := h.ex.createCount(); got != createsAtStop {
		t.Fatalf("%d placement(s) went out after the stop. The adding side is "+
			"CANCELLED and exchange-confirmed absent (A8, H-FAIL-3); a market "+
			"that re-adds one tick later has not stopped adding",
			got-createsAtStop)
	}
	if h.ex.restingCount() != 0 {
		t.Fatalf("%d of our orders rest above the debounce in a FLAT market, "+
			"want none", h.ex.restingCount())
	}
	if grew := h.ex.deleteCount() - deletesAtStop; grew != 0 {
		t.Fatalf("%d further DELETE(s) went out over six ticks after both sides "+
			"had already been confirmed absent by a complete verifying read.\n\n"+
			"H-FAIL-3 makes exchange-confirmed absence the end of the "+
			"obligation, in both directions. `risk.Portfolio.LiveOrders` is "+
			"replaced wholesale from a complete ORDERS WALK (H-POS-4) and the "+
			"next one is up to position_poll_s away, so a build that re-derives "+
			"the obligation from `atRisk` alone reissues the cancel on every one "+
			"of the twenty ticks in between -- each a DELETE for an id the "+
			"exchange no longer has, plus a full verifying walk, out of the §16 "+
			"write budget the reducer shares", grew)
	}
	if grew := h.ex.ordersWalkCount() - walksAtStop; grew > 1 {
		t.Fatalf("%d further complete orders walks were performed in six ticks "+
			"on a market with nothing of ours resting; a sweep is a write plus a "+
			"full cursor walk and neither is free", grew)
	}
}

// ---------------------------------------------------------------------------
// 2. The sign mapping, and the exit that survives it
// ---------------------------------------------------------------------------

// TestTheStoppedMarketKeepsTheCappedExitOnEitherSign is H-Q-4a's second
// sentence: "if q != 0, keep only the aggregate-capped reducer (H-Q-5a)."
//
// The composed test above is flat, because §5.2's REDUCING "leaves only at
// exactly zero" and a composed run is in REDUCING before §9's schedule lands.
// This one places the schedule before the first evaluation, so the market really
// is QUOTING with inventory -- the state in which there is both an adding order
// to retire and an exit to preserve.
//
// The mapping is §6.2's, and getting it backwards does not merely fail to exit,
// it DOUBLES the position: reducing side R = "no" if q > 0 and "yes" if q < 0,
// adding side A = the opposite. So the three rows below are the whole rule, and
// the flat one is the case with no exit at all -- IDLE, resting nothing, its own
// guard on the IDLE -> QUOTING edge keeping it from resuming.
//
// Both timing ends are asserted per sign, because "the adding side is off" is
// satisfied trivially by a build that cancels on the first thin frame -- which
// is the churn HR-012 kept the debounce for.
func TestTheStoppedMarketKeepsTheCappedExitOnEitherSign(t *testing.T) {
	one := num.QtyFromFloat(1)

	cases := []struct {
		name       string
		q          num.Qty
		addSide    quote.Side
		reduceSide quote.Side
		hasExit    bool
		wantState  quote.MarketState
	}{
		{
			name: "long yes: the no exit survives", q: one,
			addSide: quote.SideYes, reduceSide: quote.SideNo, hasExit: true,
			wantState: quote.Reducing,
		},
		{
			name: "flat: nothing rests and nothing replaces it", q: 0,
			addSide: quote.SideYes, reduceSide: quote.SideNo, hasExit: false,
			wantState: quote.Idle,
		},
		{
			name: "short yes: the yes exit survives", q: -one,
			addSide: quote.SideNo, reduceSide: quote.SideYes, hasExit: true,
			wantState: quote.Reducing,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := newGateFailOwner(t, tc.q)

			// One of ours on each side, the way a complete orders walk reports
			// them (H-POS-4). With q != 0 one of the two IS the exit.
			g.h.installResting(
				risk.LiveOrder{
					OrderID: "EX-YES", Ticker: seamTicker, Side: quote.SideYes,
					Price4: num.Price4FromCents(40), Remaining: one,
				},
				risk.LiveOrder{
					OrderID: "EX-NO", Ticker: seamTicker, Side: quote.SideNo,
					Price4: num.Price4FromCents(55), Remaining: one,
				},
			)

			g.snapshot(gateFailThinBook())

			// Just below: neither side is touched. A cancel here is the churn
			// the debounce exists to prevent, and on the reducing side it would
			// be A4's exit removed for a book that may recover in a second.
			if g.stopped(gateFailDebounce - ownerTick) {
				t.Fatalf("the market stopped %v into a %v debounce",
					gateFailDebounce-ownerTick, gateFailDebounce)
			}
			if n := g.h.rig.queue.Len(); n != 0 {
				t.Fatalf("%d intent(s) were queued inside the debounce, want none "+
					"on either side", n)
			}

			// Exactly at the boundary.
			if !g.stopped(gateFailDebounce) {
				t.Fatalf("the market is %s after %v of continuous failure, want "+
					"%s", g.o.market, gateFailDebounce, tc.wantState)
			}
			if g.o.market != tc.wantState {
				t.Fatalf("the market is %s holding q = %s, want %s. §5.2 answers "+
					"a market-scoped stop with REDUCING when there is anything to "+
					"reduce and IDLE when there is not", g.o.market, tc.q.Wire(),
					tc.wantState)
			}

			if adds := seamCancelsOn(g.h.rig.queue, tc.addSide); len(adds) != 1 {
				t.Fatalf("%d cancel intent(s) for the %s ADDING side, want 1.\n\n"+
					"H-Q-4a: \"cancel and exchange-confirm every ADDING order\". A8 "+
					"says no market in REDUCING has an adding-side order resting "+
					"or in flight, and H-FAIL-3 makes \"off\" mean "+
					"exchange-confirmed absent rather than unrefreshed",
					len(adds), tc.addSide)
			} else if adds[0].Role != quote.RoleAdding {
				t.Fatalf("the %s cancel is classified %s, want adding; H-QUE-2 "+
					"classes a reducing-side cancel P1 and refuses a standalone "+
					"one outright", tc.addSide, adds[0].Role)
			}

			if !tc.hasExit {
				// A flat market has no reducing side at all, so BOTH sides are
				// adding and both come off.
				if other := seamCancelsOn(g.h.rig.queue,
					tc.addSide.Opposite()); len(other) != 1 {

					t.Fatalf("%d cancel intent(s) for the %s side of a FLAT "+
						"market, want 1: with q == 0 there is no exit to keep and "+
						"every order of ours is an adding order",
						len(other), tc.addSide.Opposite())
				}
				return
			}

			if exits := seamCancelsOn(g.h.rig.queue, tc.reduceSide); len(exits) != 0 {
				t.Fatalf("%d cancel intent(s) for the %s REDUCING side.\n\n§5.2: "+
					"\"there is no HALTED per-market state that cancels "+
					"everything. A market-level halt trigger sends the market to "+
					"REDUCING, which keeps the exit alive. That is the "+
					"inversion.\" Cancelling the exit because the BOOK stopped "+
					"qualifying strands the position it was protecting, in a "+
					"market that by construction has thin depth on one side",
					len(exits), tc.reduceSide)
			}
			if got := g.o.atRisk(tc.reduceSide); got != one {
				t.Fatalf("the exit's aggregate is %s, want %s; nothing here may "+
					"retire it from the risk model", got.Wire(), one.Wire())
			}
			// And it is still capped. `min(|q|, S_max, funded)` is one contract
			// and one contract is already working, so the remainder is zero --
			// H-Q-5a's "the reducer never overshoots flat", evaluated against the
			// H-Q-5b aggregate rather than against any single order.
			remainder, bound := g.o.targetSize(tc.reduceSide, quote.RoleReducing)
			if remainder != 0 {
				t.Fatalf("the stopped market would place a further %s contracts "+
					"on a reducing side already carrying its whole |q| cap of %s",
					remainder.Wire(), one.Wire())
			}
			if bound != tc.q.Abs() {
				t.Fatalf("the reducer's bound is %s, want |q| = %s",
					bound.Wire(), tc.q.Abs().Wire())
			}
		})
	}
}

// ---------------------------------------------------------------------------
// 2. A placement decided before the failure cannot escape after it
// ---------------------------------------------------------------------------

// TestAQueuedPlacementCannotEscapeIntoANonQualifyingBook closes the hole that is
// exactly one dequeue wide.
//
// §6.6 holds INTENTS re-evaluated at dequeue and not pre-built requests, and
// `owner.conditions` is where that re-evaluation happens. Stopping `evaluate`
// from ENQUEUING new work is only half of H-Q-4's "do not place": a placement
// decided while the book still qualified sits in the queue until the write
// budget and the single dispatcher get to it, and it is priced at dispatch from
// whatever book is current THEN. If the re-evaluation does not consult the
// episode, that write goes out into an interval that pays nobody.
//
// The fixture holds the FIRST create in flight at the exchange, which is what
// makes this deterministic rather than a race: `pump` hands out at most one
// write because there is one dispatcher (D3), so while the first is parked the
// second side's intent is queued, decided, and cannot have been selected. The
// non-qualifying book is therefore guaranteed to arrive before it is.
//
// The control is the whole test. "One create was made" is trivially true of a
// harness that only ever wanted one, so the same fixture is run with a book that
// KEEPS qualifying and must produce two.
func TestAQueuedPlacementCannotEscapeIntoANonQualifyingBook(t *testing.T) {
	for _, tc := range []struct {
		name string
		// thin is whether the book stops qualifying while the first create is
		// parked at the exchange.
		thin       bool
		wantCreate int
	}{
		{
			name: "control: the book keeps qualifying", thin: false,
			wantCreate: 2,
		},
		{
			name: "the book stops qualifying under the queued intent", thin: true,
			wantCreate: 1,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// FLAT, for the reason the test above is flat: a composed run seeded
			// with inventory is in REDUCING before §9's schedule lands and never
			// leaves it, so only one side is ever quoted and there is no second
			// intent to queue.
			h := newGateFailHarness(t, seamOptions{ListCreated: true})

			// The hold. Armed before `serve` starts, entered by the dispatcher on
			// the first create, released by the test. `beforeDo` runs on the
			// writer goroutine and must not call testing methods, so it only
			// signals and blocks.
			var armed atomic.Bool
			armed.Store(true)
			entered := make(chan struct{}, 1)
			release := make(chan struct{})
			h.ex.beforeDo = func(req rest.Request) {
				if req.Method != "POST" || !armed.CompareAndSwap(true, false) {
					return
				}
				entered <- struct{}{}
				<-release
			}
			t.Cleanup(func() {
				select {
				case <-release:
				default:
					close(release)
				}
			})

			h.start()

			select {
			case <-entered:
			case <-time.After(seamBudget):
				t.Fatalf("no create reached the exchange within %v; a QUOTING "+
					"market with a qualifying book quotes both sides, and without "+
					"the first one parked there is no queued second one to block",
					seamBudget)
			}

			// One write is in flight, so `pump` returns early on every tick and
			// the other side's intent sits in the queue exactly as §6.6 says it
			// does: an intent, not a request.
			if tc.thin {
				h.gateFailBook(gateFailThinBook())
				h.gateFailAwaitGated(true)
			}

			close(release)

			// The parked create completes, the dispatcher frees up, and the
			// queued intent is re-evaluated at dequeue.
			h.await("the parked create to be acknowledged and rest",
				func() bool { return h.ex.restingCount() >= 1 })
			h.awaitTicks(8)

			if got := h.ex.createCount(); got != tc.wantCreate {
				var last string
				if c, ok := h.ex.createAt(got - 1); ok {
					last = fmt.Sprintf(" (last: %s at %s)", c.Coid, c.Price)
				}
				t.Fatalf("%d create(s) reached the exchange, want %d%s.\n\n"+
					"§6.6 holds intents \"re-evaluated at dequeue\" and "+
					"`owner.conditions` is that re-evaluation. A placement decided "+
					"from a book that qualified and dispatched into one that does "+
					"not is H-Q-4's refusal arriving one dequeue too late: the "+
					"snapshot is excluded for everyone, the interval pays nobody, "+
					"and the order is live and fillable through all of it",
					got, tc.wantCreate, last)
			}
			if m, _ := h.market(); m.State != quote.Quoting {
				t.Fatalf("the market is %s, want QUOTING: this test runs entirely "+
					"INSIDE the debounce, and if the market has already stopped "+
					"then the count above is attributable to §5.2's REDUCING "+
					"rather than to the queue's re-evaluation", m.State)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// The owner-level fixture
// ---------------------------------------------------------------------------

// gateFailOwner is an owner driven directly, with the real gate, book, queue and
// portfolio underneath it.
//
// `Actionable` is false here and stays false, because `Gate.noteTruth` is
// private and no test may forge a reconciliation. That makes CANCELS the
// observable, which is I1's asymmetry -- "every stop path in this system stops
// ADDING risk; none of them stops reducing it and none of them stops watching
// it" -- and cancels are precisely what H-Q-4a's expiry produces.
type gateFailOwner struct {
	h      *seamHarness
	o      *owner
	tokens chan wsx.ReconcileToken
}

func newGateFailOwner(t *testing.T, q num.Qty) *gateFailOwner {
	t.Helper()
	h := newGateFailHarness(t, seamOptions{})
	g := &gateFailOwner{
		h: h, o: h.ownerFor(), tokens: make(chan wsx.ReconcileToken, 1),
	}

	// What a complete §9 schedule read leaves behind, far outside `close_lead`
	// and not an early-closing market.
	//
	// It is not decoration. `closeUnknown` makes an UNREAD schedule a
	// market-scoped stop -- "a market whose close_time we do not know is one we
	// cannot enforce a lead on" -- and a market already stopped for that reason
	// is one in which H-Q-4a's own stop is unobservable. §5.2's stop term is a
	// disjunction, so every other disjunct has to be false for these tests to be
	// about the gate at all.
	g.o.closeAt = time.Now().Add(24 * time.Hour)
	g.o.hasClose = true
	g.o.scheduleEver = true

	g.connect()
	if !q.IsFlat() {
		h.installPosition(q)
	}

	// The control: with a qualifying book and nothing else wrong, this market
	// QUOTES. Without it every assertion below could pass on a fixture that was
	// stopped for a reason nothing in these tests touches.
	g.snapshot(gateFailQualifyingBook())
	if g.stopped(0) {
		t.Fatalf("the fixture's market is %s before any gate failure at all; "+
			"§5.2's stop term is a disjunction and something other than H-Q-4a "+
			"is asserting one, so nothing below would be attributable to the "+
			"qualifying walk", g.o.market)
	}
	return g
}

// connect is `wsx.EventConnected` through the owner's own handler, so the
// generation advances exactly as it does in production.
func (g *gateFailOwner) connect() {
	g.h.t.Helper()
	g.o.applyEvent(wsx.Event{
		Kind: wsx.EventConnected, At: g.h.clk.Now(),
	}, g.tokens)
	if !g.h.rig.gate.Connected() {
		g.h.t.Fatal("the gate is not connected after EventConnected")
	}
}

func (g *gateFailOwner) disconnect(clean bool) {
	g.h.t.Helper()
	g.o.applyEvent(wsx.Event{
		Kind: wsx.EventDisconnected, At: g.h.clk.Now(), Clean: clean,
	}, g.tokens)
}

// frame pushes one already-built frame through `applyEvent`, which is the same
// path `serve` uses: `InspectFrame`, `Gate.ApplyFrame` with core's handler, the
// sequence-gap relay, and then H-Q-4a's episode.
func (g *gateFailOwner) frame(raw []byte) {
	g.h.t.Helper()
	g.o.applyEvent(wsx.Event{
		Kind: wsx.EventFrame, At: g.h.clk.Now(), Frame: raw,
	}, g.tokens)
}

func (g *gateFailOwner) snapshot(yes, no [][]string) {
	g.h.t.Helper()
	raw, err := g.h.ws.frame("orderbook_snapshot", map[string]any{
		"market_ticker":  seamTicker,
		"ts_ms":          1,
		"yes_dollars_fp": yes,
		"no_dollars_fp":  no,
	})
	if err != nil {
		g.h.t.Fatalf("building a snapshot frame: %v", err)
	}
	g.frame(raw)
}

// delta moves one level, which is how a thin book recovers without a new
// snapshot. `core` applies it against the book the current generation already
// holds.
func (g *gateFailOwner) delta(side string, price, deltaFP string) {
	g.h.t.Helper()
	raw, err := g.h.ws.frame("orderbook_delta", map[string]any{
		"market_ticker": seamTicker,
		"ts_ms":         1,
		"side":          side,
		"price_dollars": price,
		"delta_fp":      deltaFP,
	})
	if err != nil {
		g.h.t.Fatalf("building a delta frame: %v", err)
	}
	g.frame(raw)
}

// qualifies is what H-Q-4 actually gates on, read from core's own book.
func (g *gateFailOwner) qualifies() bool {
	b := g.h.rig.book.Book(seamTicker)
	return b != nil && b.Qualifies() != 0
}

// at moves the injected monotonic clock to an absolute reading.
//
// Absolute rather than relative because every assertion in this file is stated
// against the episode's own start, and a fixture that took deltas would make an
// off-by-one-tick in the TEST indistinguishable from the off-by-one-tick in the
// implementation it is looking for.
//
// Frames are stamped from this clock -- `applyEvent` passes `Event.At` straight
// through -- so moving it before delivering a frame is how these tests place an
// event at an instant rather than merely asserting about one.
func (g *gateFailOwner) at(mono time.Duration) {
	g.h.t.Helper()
	now := g.h.clk.monoNow()
	if mono < now {
		g.h.t.Fatalf("the monotonic clock cannot go backwards: %v is before "+
			"the current %v. F21 is a WALL step and `gateFailStepWall` is how "+
			"this file performs one", mono, now)
	}
	g.h.clk.Advance(mono - now)
}

// stopped moves the clock to `mono`, runs one real evaluation there, and reports
// whether §5.2 has taken the market out of its quoting states.
//
// It asserts on the MARKET STATE and not on any field of the episode. The state
// is what cancels the adding side and what keeps the exit alive, so it is the
// thing H-Q-4a is actually about; an episode that opened and never reached §5.2
// would satisfy any assertion on a timer and none on the behaviour.
func (g *gateFailOwner) stopped(mono time.Duration) bool {
	g.h.t.Helper()
	g.at(mono)
	g.o.evaluate(g.h.clk.monoNow())
	return g.o.market == quote.Reducing || g.o.market == quote.Idle
}

// ---------------------------------------------------------------------------
// 3. What may and may not clear the episode
// ---------------------------------------------------------------------------

// TestOnlyAnAcceptedCurrentQualifyingBookClearsTheGateFailureEpisode is the
// re-entry half of H-Q-4a: "re-enter only from a fresh, complete, qualifying
// book."
//
// Every candidate below is something that arrives while the market is failing
// and that a looser implementation would treat as recovery. None of them is
// evidence about depth:
//
//   - a frame that never decoded, so nothing reached the book;
//   - a frame core REFUSED, which leaves the book exactly as it was -- and
//     which additionally quarantines the market (`quarantineRejectedBook`);
//   - a fractional book price, H-CO-3a's structural violation, which never
//     reaches core at all;
//   - a qualifying book for a DIFFERENT market, which the unfiltered `trade`
//     subscription makes an ordinary occurrence rather than an exotic one;
//   - a disconnect and a reconnect, which change the generation and retire every
//     snapshot -- and must neither clear the episode nor restart its clock;
//   - a qualifying DELTA onto a generation that has not been snapshotted, which
//     is an unknown base plus a known edit and is not a book.
//
// The positive control at the end is what stops the whole test from passing on a
// build where NOTHING ever clears the episode.
func TestOnlyAnAcceptedCurrentQualifyingBookClearsTheGateFailureEpisode(
	t *testing.T) {

	// past is comfortably beyond the configured debounce, so "still stopped"
	// means the episode survived rather than merely that it had not expired.
	past := gateFailDebounce + time.Second

	cases := []struct {
		name string
		// arrive is the candidate recovery event.
		arrive func(g *gateFailOwner)
		// wantCleared is whether the episode may end because of it.
		wantCleared bool
		why         string
	}{
		{
			name: "an undecodable frame",
			arrive: func(g *gateFailOwner) {
				g.frame([]byte(`{"type":"orderbook_snapshot","sid":1,"seq":99`))
			},
			why: "the frame never decoded, so nothing reached the book and " +
				"nothing about any depth is known",
		},
		{
			name: "a frame core refused",
			arrive: func(g *gateFailOwner) {
				// A well-formed envelope whose sizes core cannot parse. The
				// handler returns an error, so `FrameEffects.Delivered` is
				// false and the book is untouched.
				g.snapshot([][]string{{"0.4000", "not-a-size"}},
					[][]string{{"0.5500", "20.00"}})
			},
			why: "core rejects the WHOLE frame, leaving the book exactly as it " +
				"was; certifying it would be quoting from a book core declined " +
				"to believe",
		},
		{
			name: "a fractional book price",
			arrive: func(g *gateFailOwner) {
				g.snapshot([][]string{{"0.4050", "20.00"}},
					[][]string{{"0.5500", "20.00"}})
			},
			why: "H-CO-3a: a resting level off the cent grid never reaches " +
				"core, and it quarantines the market rather than recovering it",
		},
		{
			name: "a qualifying book for another market",
			arrive: func(g *gateFailOwner) {
				raw, err := g.h.ws.frame("orderbook_snapshot", map[string]any{
					"market_ticker":  "KXOTHER-26AUG08-T1",
					"ts_ms":          1,
					"yes_dollars_fp": [][]string{{"0.4000", "500.00"}},
					"no_dollars_fp":  [][]string{{"0.5500", "500.00"}},
				})
				if err != nil {
					g.h.t.Fatalf("building a foreign snapshot: %v", err)
				}
				g.frame(raw)
			},
			why: "the subscription carries the whole exchange tape; another " +
				"market's depth says nothing about ours",
		},
		{
			name: "a disconnect and a reconnect",
			arrive: func(g *gateFailOwner) {
				g.disconnect(true)
				g.connect()
			},
			why: "a generation change retires every snapshot (H-FAIL-5). A " +
				"socket coming back is evidence about the socket, and a market " +
				"that flapped every few seconds would otherwise never complete " +
				"one continuous interval",
		},
		{
			name: "a qualifying delta with no snapshot on this generation",
			arrive: func(g *gateFailOwner) {
				g.disconnect(false)
				g.connect()
				// Enough size to qualify, applied to a generation that has
				// received no snapshot.
				g.delta("no", "0.5500", "500.00")
			},
			why: "a delta is an increment against a book we have decided not " +
				"to trust; replaying increments onto it cannot make it right",
		},
		{
			name: "a fresh complete qualifying snapshot",
			arrive: func(g *gateFailOwner) {
				g.snapshot(gateFailQualifyingBook())
			},
			wantCleared: true,
			why: "H-Q-4a's own re-entry condition, and the only one. Without " +
				"this case the whole test would pass on a build where nothing " +
				"ever ends an episode",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := newGateFailOwner(t, num.QtyFromFloat(1))

			// A qualifying book, then a thin one. The episode opens at mono 0.
			g.snapshot(gateFailQualifyingBook())
			if !g.qualifies() {
				t.Fatal("the opening book does not qualify; the fixture's " +
					"target and depths disagree")
			}
			g.snapshot(gateFailThinBook())
			if g.qualifies() {
				t.Fatal("the thin book still qualifies; there is no failure " +
					"to time")
			}
			if g.stopped(0) {
				t.Fatal("the market stopped on the FIRST non-qualifying frame; " +
					"H-Q-4a debounces, and cancelling on a transient blip is " +
					"the churn HR-012 kept the interval for")
			}

			tc.arrive(g)

			if got := g.stopped(past); got == tc.wantCleared {
				verb := "did not clear"
				if tc.wantCleared {
					verb = "cleared"
				}
				t.Fatalf("%v after the episode opened the market is stopped = "+
					"%t, and the episode %s.\n\n%s", past, got, verb, tc.why)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// 4. Recovery resets the interval; a recovery at the deadline wins
// ---------------------------------------------------------------------------

// TestARecoveryResetsTheDebounceAndOneAtTheDeadlineWins is the "CONTINUOUS" in
// "continuous gate failure", from both directions.
//
// The first half is the ordinary case an off-by-one gets wrong: a book that
// recovers three seconds into a four-second interval and then fails again has
// not been failing continuously for four seconds at t = 4s, and cancelling there
// would retire an adding order for an interval that was interrupted. The second
// failure is entitled to a whole new debounce.
//
// The second half is the boundary itself. A qualifying book arriving at exactly
// the old deadline wins, because the failure was NOT continuous through that
// instant -- the frame is evidence about the market at that moment and the
// deadline is a statement about the interval that ended there. Getting this
// backwards costs an order in the one case where the market has already
// recovered.
func TestARecoveryResetsTheDebounceAndOneAtTheDeadlineWins(t *testing.T) {
	t.Run("a recovery restarts the interval from scratch", func(t *testing.T) {
		g := newGateFailOwner(t, num.QtyFromFloat(1))
		g.snapshot(gateFailQualifyingBook())
		g.snapshot(gateFailThinBook())

		// Three quarters of the way through, the book recovers.
		mid := gateFailDebounce - time.Second
		if g.stopped(mid) {
			t.Fatalf("the market stopped %v into a %v debounce", mid,
				gateFailDebounce)
		}
		g.snapshot(gateFailQualifyingBook())
		if g.stopped(gateFailDebounce) {
			t.Fatalf("the market stopped at %v, the ORIGINAL deadline, after "+
				"the book recovered at %v. H-Q-4a's interval is CONTINUOUS "+
				"failure; an interval with a qualifying book in the middle of "+
				"it is two shorter intervals", gateFailDebounce, mid)
		}

		// It fails again, at the instant the old deadline would have fired. The
		// new episode starts HERE and gets the whole interval, not the
		// remainder of the old one.
		restart := gateFailDebounce
		g.at(restart)
		g.snapshot(gateFailThinBook())
		if g.stopped(restart + gateFailDebounce - ownerTick) {
			t.Fatalf("the second failure stopped the market %v after it began, "+
				"inside its own %v debounce; the new episode inherited the old "+
				"one's clock", gateFailDebounce-ownerTick, gateFailDebounce)
		}
		if !g.stopped(restart + gateFailDebounce) {
			t.Fatalf("the second failure did not stop the market after a full "+
				"%v of continuous failure", gateFailDebounce)
		}
	})

	t.Run("a recovery at the exact deadline wins", func(t *testing.T) {
		g := newGateFailOwner(t, num.QtyFromFloat(1))
		g.snapshot(gateFailQualifyingBook())
		g.snapshot(gateFailThinBook())

		// The frame lands AT the deadline instant, before the evaluation at
		// that instant. The failure was therefore not continuous THROUGH it.
		g.at(gateFailDebounce)
		g.snapshot(gateFailQualifyingBook())
		if g.stopped(gateFailDebounce) {
			t.Fatalf("a qualifying book that arrived at exactly %v -- the "+
				"deadline instant -- did not save the adding order. The "+
				"interval is a statement about failure that was continuous "+
				"through that instant, and this one was not: the market is "+
				"qualifying right now and the harness is about to cancel into "+
				"a book that pays", gateFailDebounce)
		}
	})

	t.Run("a continuing failure never moves its own start", func(t *testing.T) {
		g := newGateFailOwner(t, num.QtyFromFloat(1))
		g.snapshot(gateFailQualifyingBook())
		g.snapshot(gateFailThinBook())

		// A busy market publishes constantly. Every one of these frames is
		// accepted, current and non-qualifying, arrives at a LATER instant than
		// the one before it, and not one of them may push the deadline out --
		// otherwise the rule is unreachable in exactly the market it matters in.
		for i := 1; i <= 8; i++ {
			g.at(time.Duration(i) * 400 * time.Millisecond)
			g.delta("no", "0.5500", "0.01")
			g.delta("no", "0.5500", "-0.01")
			if g.qualifies() {
				t.Fatalf("frame %d recovered the book; these deltas are meant "+
					"to leave the qualifying walk failing", i)
			}
		}
		if !g.stopped(gateFailDebounce) {
			t.Fatalf("sixteen accepted non-qualifying frames later the market "+
				"is still quoting at %v.\n\nH-Q-4a times CONTINUOUS failure. "+
				"An episode whose start is re-stamped by every frame never "+
				"completes in a market that publishes four times a second, "+
				"which is every market this harness would ever be paid in",
				gateFailDebounce)
		}
	})
}

// ---------------------------------------------------------------------------
// 5. F21 -- the clock step
// ---------------------------------------------------------------------------

// TestAWallClockStepDoesNotMoveTheGateFailureInterval is F21 applied to H-Q-4a.
//
// §16's table calls the parameter `gate_fail_debounce_s` and F21 is "clock step:
// wall vs monotonic". An interval derived from the wall clock is one an NTP
// correction can settle: a step forward retires a resting adding order
// instantly, from a market that has been failing for two seconds; a step
// backward holds one live through a gated-out interval of unbounded length. Both
// of those are silent, and neither leaves a trace an operator could read
// afterwards.
//
// The monotonic clock does not move in this test at all until the last two
// assertions, which is what makes the wall steps attributable.
func TestAWallClockStepDoesNotMoveTheGateFailureInterval(t *testing.T) {
	g := newGateFailOwner(t, num.QtyFromFloat(1))
	g.snapshot(gateFailQualifyingBook())
	g.snapshot(gateFailThinBook())

	wallBefore := g.h.clk.wallMs()

	// An hour forward. Nothing at all has elapsed.
	gateFailStepWall(g.h.clk, time.Hour)
	if g.stopped(0) {
		t.Fatalf("a one-hour WALL step retired the adding order from a market "+
			"that had been failing for zero monotonic time. F21 recomputes "+
			"deadlines that are wall-anchored -- a close lead is one -- and "+
			"H-Q-4a's is not one of them: it is a DURATION of continuous "+
			"failure, and a duration measured against a clock that can be "+
			"rewritten is not a duration (wall moved %dms)",
			g.h.clk.wallMs()-wallBefore)
	}

	// And an hour back, past where it started.
	gateFailStepWall(g.h.clk, -2*time.Hour)
	if g.stopped(gateFailDebounce - ownerTick) {
		t.Fatalf("a backward WALL step stopped the market before %v of "+
			"monotonic failure had elapsed", gateFailDebounce)
	}
	if !g.stopped(gateFailDebounce) {
		t.Fatalf("after two wall steps totalling an hour backwards, %v of "+
			"MONOTONIC failure no longer stops the market. The wall clock has "+
			"no vote here in either direction", gateFailDebounce)
	}
}

// ---------------------------------------------------------------------------
// 6. A13 is not bypassed by the episode clearing
// ---------------------------------------------------------------------------

// TestAGateFailureResetNeverBypassesA13 is the rule that keeps `BookCurrent`
// from becoming a second, weaker `Actionable`.
//
// The episode is book evidence and the book alone; DISPATCH authority stays with
// A13, which is connected, unquarantined, snapshotted on THIS generation, AND
// positions, orders and fills each reconciled after it and inside
// `truth_max_age_s`. So a market can be perfectly qualifying and still place
// nothing, and after a reconnect it MUST -- H-ORD-5 reserves that window for
// reconciliation and H-FAIL-4 refuses to resume from stale truth.
//
// The sequence here is the dangerous one: fail, disconnect, reconnect, and
// recover the book. Everything about the depth now looks healthy. The portfolio
// still describes the account on the far side of a gap.
func TestAGateFailureResetNeverBypassesA13(t *testing.T) {
	g := newGateFailOwner(t, num.QtyFromFloat(1))
	g.snapshot(gateFailQualifyingBook())
	g.snapshot(gateFailThinBook())

	g.disconnect(false)
	g.connect()
	g.snapshot(gateFailQualifyingBook())

	if !g.h.rig.gate.BookCurrent(seamTicker) {
		t.Fatal("a snapshot accepted on the current generation is not reported " +
			"as a current book; the episode below could not have cleared for " +
			"the reason this test is about")
	}
	if g.h.rig.gate.Actionable(seamTicker, g.h.clk.Now()) {
		t.Fatal("A13 is satisfied with no portfolio reconciliation on this " +
			"generation at all. `Gate.noteTruth` is private precisely so that " +
			"cannot happen, and if it has happened this test proves nothing")
	}

	// The episode really did clear -- otherwise "nothing was placed" would be
	// attributable to H-Q-4a rather than to A13.
	if g.stopped(gateFailDebounce * 2) {
		t.Fatalf("the market is still stopped %v after a fresh, complete, "+
			"qualifying snapshot; H-Q-4a's re-entry condition has been met",
			gateFailDebounce*2)
	}
	if places := seamPlaces(g.h.rig.queue); len(places) != 0 {
		t.Fatalf("%d placement intent(s) were queued from a qualifying book "+
			"whose portfolio has not reconciled since the reconnect.\n\nA13: "+
			"\"no placement decision is taken from a book that is quarantined "+
			"or stale, OR from portfolio truth older than truth_max_age_s\". "+
			"H-ORD-5 reserves the window between reconnect and complete "+
			"reconciliation, and a book recovering is not a position being "+
			"re-read", len(places))
	}
}

// ---------------------------------------------------------------------------
// 7. The adding order that no orders walk has listed yet
// ---------------------------------------------------------------------------

// TestAnUnlistedAddingOrderIsStillCancelledAtExpiry is the bookkeeping half of
// H-Q-4a, and it is the half that made the rule a no-op in the window it is most
// likely to fire in.
//
// `risk.Portfolio.LiveOrders` is replaced WHOLESALE from a complete orders walk
// (H-POS-4), and the next walk is up to `position_poll_s` away. Between two
// walks an order this process created -- acked, or UNKNOWN, or simply young --
// is in `pending`, therefore in `atRisk`, therefore in every aggregate cap
// (H-Q-5b, H-ORD-2 clause 6). It was in no cancel REQUEST: `build` looked only
// at `LiveOrders`, found nothing, and returned "nothing of ours rests on this
// side". The intents were dropped, a `WRITE_NOT_BUILDABLE` SEV2 was raised, and
// the order stayed live and fillable through the gated-out interval while the
// harness believed it had asked for it back.
//
// Both resolutions are asserted, because they are different code paths:
//
//   - an ACKED create carries an `order_id`, so the very next cancel names it;
//   - an UNKNOWN create carries none (H-ORD-2a: absence is not evidence, and
//     nothing may be inferred), so the cancel goes out with an EMPTY target
//     list, H-ORD-4's verifying read finds the order, and the tick after that
//     names it.
func TestAnUnlistedAddingOrderIsStillCancelledAtExpiry(t *testing.T) {
	const orderID = "EX-UNLISTED-YES"

	cases := []struct {
		name string
		// acked is whether the create's acknowledgement carried an order id.
		acked bool
		// wantFirst is whether the FIRST cancel built after expiry can already
		// name the order.
		wantFirst bool
	}{
		{
			name: "an acked create the walk has not listed", acked: true,
			wantFirst: true,
		},
		{
			name:  "an UNKNOWN create, discovered by the verifying read",
			acked: false, wantFirst: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := newGateFailOwner(t, num.QtyFromFloat(1))
			g.snapshot(gateFailQualifyingBook())

			// One adding order on the YES side, created by this process and
			// listed by no walk. This is `applyWriteResult`'s own path, not a
			// field poked from the test.
			coid, err := rest.Coid(g.h.rig.runID, pilotMarketIdx,
				quote.SideYes, 1)
			if err != nil {
				t.Fatalf("building a coid: %v", err)
			}
			order, err := rest.NewCreateOrder(seamTicker, quote.SideYes, 40,
				gateFailS, gateFailS, coid)
			if err != nil {
				t.Fatalf("building the create: %v", err)
			}
			res := writeResult{
				Req: writeRequest{
					Market: seamTicker, Side: quote.SideYes,
					Role: quote.RoleAdding, Op: quote.OpPlace, Order: order,
				},
				Create: rest.CreateResult{
					Outcome: rest.CreateUnknown, Coid: coid,
					MaxLive: gateFailS,
				},
			}
			if tc.acked {
				res.Create.Outcome = rest.CreateAcked
				res.Create.OrderID = orderID
				res.Create.Remaining = gateFailS
			}
			g.o.applyWriteResult(res)

			if got := g.o.atRisk(quote.SideYes); got != gateFailS {
				t.Fatalf("the unlisted create contributes %s to the YES "+
					"aggregate, want %s. H-ORD-2 clause 6 keeps an order's "+
					"maximum possibly-live quantity in the risk model and in "+
					"every aggregate cap until it is POSITIVELY resolved",
					got.Wire(), gateFailS.Wire())
			}

			// The book fails, and the debounce runs out.
			g.snapshot(gateFailThinBook())
			if !g.stopped(gateFailDebounce) {
				t.Fatalf("the market did not stop after %v of continuous "+
					"failure", gateFailDebounce)
			}

			writes := make(chan writeRequest, 1)
			g.o.pump(gateFailDebounce, writes)
			var req writeRequest
			select {
			case req = <-writes:
			default:
				t.Fatalf("no write was dispatched after H-Q-4a's expiry with " +
					"an adding order live on the YES side.\n\nA8 requires that " +
					"no market in REDUCING has an adding-side order resting or " +
					"in flight, and H-FAIL-3 makes \"off\" mean " +
					"exchange-confirmed absent. Neither is satisfied by a " +
					"harness that queued the obligation and could not express " +
					"the write")
			}
			if req.Op != quote.OpCancel || req.Side != quote.SideYes {
				t.Fatalf("the dispatched write is a %s on %s, want a cancel on "+
					"the YES adding side", req.Op, req.Side)
			}

			named := false
			for _, o := range req.Orders {
				if o.OrderID == orderID {
					named = true
				}
			}
			if named != tc.wantFirst {
				t.Fatalf("the first cancel after expiry names order %q = %t, "+
					"want %t (targets: %d).\n\nAn ACKED create's id is known "+
					"immediately and must be cancelled by name. An UNKNOWN "+
					"one has no id anywhere (H-ORD-2a), so the cancel must go "+
					"out with an EMPTY target list -- `writeRequest.Orders` "+
					"documents that as legitimate, because H-ORD-4's sweep then "+
					"\"issues no DELETE and performs only the verifying read, "+
					"which is still the read that turns absence into a fact\"",
					orderID, named, tc.wantFirst, len(req.Orders))
			}
			if tc.wantFirst {
				return
			}

			// The UNKNOWN half. The verifying read finds the order; nothing of
			// ours was in the REQUESTED set, so it comes back as `OtherOurs`,
			// which `rest.CancelAndSweep` reports and deliberately does not
			// cancel -- on a halt that set contains the reducing quote and A4
			// requires it stay alive. Deciding which of them to retire is the
			// owner's job, per side, from §5.2.
			g.o.applyWriteResult(writeResult{
				Req: req,
				Sweep: rest.SweepResult{
					Walk:  rest.Walk{Outcome: rest.WalkComplete},
					Clean: true,
					OtherOurs: []rest.Order{{
						OrderID: orderID, Ticker: seamTicker,
						Side: quote.SideYes, Price4: num.Price4FromCents(40),
						PriceCents: 40, Remaining: gateFailS,
						Status: rest.StatusResting, Ours: true,
					}},
				},
			})

			if g.o.r.queue.Len() != 0 {
				t.Fatalf("%d intent(s) survived a sweep that found one of ours "+
					"still resting on the cancelled side", g.o.r.queue.Len())
			}

			g.o.evaluate(gateFailDebounce)
			g.o.pump(gateFailDebounce, writes)
			select {
			case req = <-writes:
			default:
				t.Fatalf("no second cancel was dispatched after the verifying " +
					"read reported one of ours still resting on the adding " +
					"side. H-FAIL-3: only exchange-confirmed ABSENCE ends the " +
					"obligation, and this read said the opposite")
			}
			named = false
			for _, o := range req.Orders {
				if o.OrderID == orderID {
					named = true
				}
			}
			if !named {
				t.Fatalf("the cancel built after the sweep discovered order %q "+
					"does not name it (targets: %d). An order the exchange has "+
					"shown us resting is positively identified; declining to "+
					"cancel it because no PORTFOLIO walk has listed it yet is "+
					"the same order left live for another `position_poll_s`",
					orderID, len(req.Orders))
			}
		})
	}
}

// ---------------------------------------------------------------------------
// 8. An ambiguous cancel retires nothing
// ---------------------------------------------------------------------------

// TestAnAmbiguousCancelDoesNotRetireTheAddingOrder is H-FAIL-3 at H-Q-4a's
// expiry: "off means exchange-confirmed absent, never cancel-requested".
//
// The three ways a DELETE can end without establishing anything are all here as
// one shape, because the harness's answer to all three is identical: the sweep's
// verifying read did not complete, or completed and still found the order. In
// neither case may the quantity leave the risk model, in neither case may the
// side be treated as clear, and in both cases the obligation to cancel is still
// outstanding on the next tick.
//
// `rest.CancelAndSweep` owns the retry -- one round, then a SEV1 -- and that
// escalation is asserted to still be there, because the tempting simplification
// is to treat a sweep that has already retried as done.
func TestAnAmbiguousCancelDoesNotRetireTheAddingOrder(t *testing.T) {
	const orderID = "EX-AMBIGUOUS-YES"

	cases := []struct {
		name  string
		sweep rest.SweepResult
		why   string
	}{
		{
			name: "the verifying read never completed",
			sweep: rest.SweepResult{
				Walk: rest.Walk{Outcome: rest.WalkFailed},
				Anomalies: []risk.Anomaly{{
					Class: "SWEEP_UNVERIFIED", Sev: risk.SEV1,
					Ticker: seamTicker,
					Text: "cancel sweep could not be verified: the " +
						"resting-orders read did not complete",
				}},
			},
			why: "an incomplete read tells us nothing; it cannot retire an " +
				"order from our model and it certainly cannot declare a sweep " +
				"clean",
		},
		{
			name: "the order is still resting after the retry",
			sweep: rest.SweepResult{
				Walk:   rest.Walk{Outcome: rest.WalkComplete},
				Rounds: 2,
				StillResting: []rest.Order{{
					OrderID: orderID, Ticker: seamTicker, Side: quote.SideYes,
					Price4: num.Price4FromCents(40), PriceCents: 40,
					Remaining: gateFailS, Status: rest.StatusResting, Ours: true,
				}},
				Anomalies: []risk.Anomaly{{
					Class: "SWEEP_INCOMPLETE", Sev: risk.SEV1,
					Ticker: seamTicker,
					Text: "cancel sweep still finds 1 of our orders resting " +
						"after 2 rounds; they are live and fillable",
				}},
			},
			why: "the exchange says the order is there. Nothing about a DELETE " +
				"we sent outranks a complete read that found it resting",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := newGateFailOwner(t, num.QtyFromFloat(1))
			g.snapshot(gateFailQualifyingBook())
			g.h.installResting(risk.LiveOrder{
				OrderID: orderID, Ticker: seamTicker, Side: quote.SideYes,
				Price4: num.Price4FromCents(40), Remaining: gateFailS,
			})

			g.snapshot(gateFailThinBook())
			if !g.stopped(gateFailDebounce) {
				t.Fatalf("the market did not stop after %v of continuous "+
					"failure", gateFailDebounce)
			}

			writes := make(chan writeRequest, 1)
			g.o.pump(gateFailDebounce, writes)
			var req writeRequest
			select {
			case req = <-writes:
			default:
				t.Fatal("no cancel was dispatched at H-Q-4a's expiry")
			}

			// The ambiguous answer. `Absent` is false, which is the only thing
			// `dispatch.go` ever lets through to `Queue.ConfirmAbsent`.
			g.o.applyWriteResult(writeResult{
				Req: req, Sweep: tc.sweep, Sent: true,
				Anomalies: tc.sweep.Anomalies,
			})

			if got := g.o.atRisk(quote.SideYes); got != gateFailS {
				t.Fatalf("the adding side's aggregate is %s after an ambiguous "+
					"cancel, want %s.\n\n%s. H-FAIL-3 keeps every one of them "+
					"in the risk model and in every aggregate cap until the "+
					"exchange confirms it gone", got.Wire(), gateFailS.Wire(),
					tc.why)
			}

			raised := g.h.takeRaised()
			if !seamContains(classesIn(raised), "CANCEL_UNVERIFIED") {
				t.Fatalf("an ambiguous cancel raised %v, with no "+
					"CANCEL_UNVERIFIED among them; the operator has no way to "+
					"see that an adding order is live in a market the harness "+
					"has stopped adding in", classesIn(raised))
			}
			for _, a := range tc.sweep.Anomalies {
				if !seamContains(classesIn(raised), a.Class) {
					t.Fatalf("the sweep's own %s (%s) was not raised; "+
						"`writeResult.Anomalies` is the complete set and the "+
						"owner raises it, so a lost one is an escalation that "+
						"never happened", a.Class, a.Sev)
				}
			}

			// The obligation is still outstanding, and the next tick reissues
			// it. Re-entry is blocked in the meantime: the adding side is not
			// replaced while the market is stopped.
			g.o.evaluate(gateFailDebounce + ownerTick)
			if len(seamCancelsOn(g.o.r.queue, quote.SideYes)) == 0 {
				t.Fatalf("no cancel intent was re-queued after an ambiguous "+
					"answer.\n\n%s -- and an obligation that is dropped "+
					"because the first attempt was ambiguous is an order left "+
					"resting through the whole gated-out interval", tc.why)
			}
			if places := seamPlaces(g.o.r.queue); len(places) != 0 {
				t.Fatalf("%d placement intent(s) were queued while an adding "+
					"order was still unaccounted for on a stopped market (A8)",
					len(places))
			}
		})
	}
}

// classesIn is the anomaly classes of a slice, for assertion messages that name
// what actually happened rather than what did not.
func classesIn(as []risk.Anomaly) []string {
	out := make([]string, 0, len(as))
	for _, a := range as {
		out = append(out, a.Class)
	}
	return out
}

// ---------------------------------------------------------------------------
// 9. The reducer stays aggregate-capped through the stop
// ---------------------------------------------------------------------------

// TestTheReducerAggregateStaysCappedThroughAGateFailureStop is H-Q-5a and H-Q-5b
// asserted at the one moment they are easiest to lose: the market has just been
// stopped, the adding side is being retired, and §5.2 is asking for the exit to
// be present.
//
// H-Q-5b counts `RESTING + SENDING + UNKNOWN + unconfirmed-cancel` on that side
// of that market, never a single order. HR-004 is what happens when it does not:
// with q = +100 a second 200-contract reducer was permitted because the
// momentary doubled size was exactly `S_max`, and if both filled `q` went to
// -300. H-Q-5a's cap at |q| is the other half -- a reducer sized above |q| is a
// reducing quote of |q| plus an ADDING quote on the opposite side, which is
// precisely what a stopped market exists to switch off.
//
// The three quantities below are the three the aggregate must see and the ones a
// naive implementation counts only one of.
func TestTheReducerAggregateStaysCappedThroughAGateFailureStop(t *testing.T) {
	// q = +2, so the reducing side is NO and the cap is exactly two contracts.
	held := num.QtyFromFloat(2)
	one := num.QtyFromFloat(1)

	g := newGateFailOwner(t, held)
	g.snapshot(gateFailQualifyingBook())

	// One contract confirmed resting by a complete walk, and one contract in an
	// UNKNOWN create that no walk has listed. Together they are already the
	// whole cap.
	g.h.installResting(risk.LiveOrder{
		OrderID: "EX-EXIT-RESTING", Ticker: seamTicker, Side: quote.SideNo,
		Price4: num.Price4FromCents(55), Remaining: one,
	})
	coid, err := rest.Coid(g.h.rig.runID, pilotMarketIdx, quote.SideNo, 1)
	if err != nil {
		t.Fatalf("building a coid: %v", err)
	}
	order, err := rest.NewCreateOrder(seamTicker, quote.SideNo, 55, one, held,
		coid)
	if err != nil {
		t.Fatalf("building the create: %v", err)
	}
	g.o.applyWriteResult(writeResult{
		Req: writeRequest{
			Market: seamTicker, Side: quote.SideNo, Role: quote.RoleReducing,
			Op: quote.OpPlace, Order: order,
		},
		Create: rest.CreateResult{
			Outcome: rest.CreateUnknown, Coid: coid, MaxLive: one,
		},
	})

	if got := g.o.atRisk(quote.SideNo); got != held {
		t.Fatalf("the reducing side's aggregate is %s, want %s = RESTING (%s) "+
			"+ UNKNOWN (%s). H-Q-5b: every size limit applies to the SUM, "+
			"never to a single order in isolation", got.Wire(), held.Wire(),
			one.Wire(), one.Wire())
	}

	g.snapshot(gateFailThinBook())
	if !g.stopped(gateFailDebounce) {
		t.Fatalf("the market did not stop after %v of continuous failure",
			gateFailDebounce)
	}
	if g.o.market != quote.Reducing {
		t.Fatalf("a stopped market holding %s is %s, want REDUCING; a flat "+
			"market goes to IDLE and this one is not flat", held.Wire(),
			g.o.market)
	}

	// The remainder is what `build` would place, and it is ZERO: the side is
	// already carrying its whole aggregate target.
	remainder, bound := g.o.targetSize(quote.SideNo, quote.RoleReducing)
	if remainder != 0 {
		t.Fatalf("the reducing side would place a further %s contracts on top "+
			"of an aggregate that is already at the |q| cap of %s.\n\nHR-004: "+
			"\"two individually-capped reducers that both fill flip the "+
			"position\", and A12 says no fill sequence may change the sign of "+
			"q via a reducer", remainder.Wire(), held.Wire())
	}
	if bound != held.Abs() {
		t.Fatalf("the reducer's H-CO-4b bound is %s, want |q| = %s; "+
			"`rest.NewCreateOrder` validates the count against it, and a wrong "+
			"bound makes an oversized reducer a placed order instead of a "+
			"construction error", bound.Wire(), held.Abs().Wire())
	}
	if cancels := seamCancelsOn(g.o.r.queue, quote.SideNo); len(cancels) != 0 {
		t.Fatalf("%d cancel intent(s) were queued for the REDUCING side at "+
			"H-Q-4a's expiry. H-Q-4a keeps the exit: \"if q != 0, keep only "+
			"the aggregate-capped reducer\", and H-QUE-2 has no row for a "+
			"standalone reducing-side cancel at all", len(cancels))
	}
}

// ---------------------------------------------------------------------------
// 10. An OVERSIZED reducer is re-capped by SIZE, not by price
// ---------------------------------------------------------------------------

// TestAnOversizedReducerIsRecappedAtTheGateFailureStop is H-Q-4a's second
// sentence at the one place it is easiest to lose: the reducer is AT THE TOUCH,
// so §6.5's price-only `quote.Decide` reports no move and leaves it exactly where
// it is -- twelve contracts of exit against one contract of inventory.
//
// H-Q-5a caps the reducer at |q| over the H-Q-5b aggregate, and a reducer above
// |q| is a reducing quote of |q| plus an ADDING quote on the opposite side (A11,
// A12): a full fill of the twelve carries q = +1 to q = -11, the position flip a
// stopped market exists to prevent. Only a SIZE check sees it while the price is
// unchanged. H-QUE-2 forbids a standalone reducing-side cancel, so the retirement
// is ONE dependent cancel-confirm-place: cancel every possibly-live reducer,
// confirm it absent (H-ORD-4, H-FAIL-3), then rebuild exactly the capped
// remainder -- and no replacement dispatches until the sweep confirms absence, so
// none overlaps a reducer still resting.
//
// Both signs, because getting §6.2's mapping backwards (R = "no" for q > 0, "yes"
// for q < 0) does not merely fail to cap, it doubles down on the wrong side. And
// the interval is timed to the stop: strictly before the threshold the oversized
// reducer is left exactly where it is, because this is H-Q-4a's response to a
// SUSTAINED stop and not churn on a transient blip (HR-012).
func TestAnOversizedReducerIsRecappedAtTheGateFailureStop(t *testing.T) {
	one := num.QtyFromFloat(1)
	twelve := num.QtyFromFloat(12)

	cases := []struct {
		name       string
		q          num.Qty
		reduceSide quote.Side
		addSide    quote.Side
		cents      int
		orderID    string
	}{
		{
			name: "long yes: the oversized NO exit is capped", q: one,
			reduceSide: quote.SideNo, addSide: quote.SideYes,
			cents: 55, orderID: "EX-BIG-NO",
		},
		{
			name: "short yes: the oversized YES exit is capped", q: -one,
			reduceSide: quote.SideYes, addSide: quote.SideNo,
			cents: 40, orderID: "EX-BIG-YES",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := newGateFailOwner(t, tc.q)

			// One reducer, twelve contracts, resting at the touch the way a
			// complete orders walk reports it (H-POS-4). |q| is one, so this is
			// eleven contracts over the cap and the price has not moved.
			g.h.installResting(risk.LiveOrder{
				OrderID: tc.orderID, Ticker: seamTicker, Side: tc.reduceSide,
				Price4: num.Price4FromCents(tc.cents), Remaining: twelve,
			})
			if got := g.o.atRisk(tc.reduceSide); got != twelve {
				t.Fatalf("the reducing side's aggregate is %s before the stop, "+
					"want %s", got.Wire(), twelve.Wire())
			}

			g.snapshot(gateFailThinBook())

			// Strictly before the threshold the market has NOT stopped: the adding
			// side is still quoting, because the debounce is the churn protection
			// HR-012 kept for a transient blip.
			if g.stopped(gateFailDebounce - ownerTick) {
				t.Fatalf("the market stopped %v into a %v debounce",
					gateFailDebounce-ownerTick, gateFailDebounce)
			}
			// The aggregate overhang, however, is corrected at every tick. A12 is
			// invariant: a reducer eleven contracts over |q| is a reducing quote of
			// |q| plus an adding quote on the opposite side WHATEVER the gate is
			// doing, so the recap is independent of the gate-failure stop
			// (H-Q-5b). One dependent cancel-confirm-place on the reducing side,
			// no adding-side churn, and no placement until the cancel confirms.
			if got := seamCancelsOn(g.o.r.queue, tc.reduceSide); len(got) != 1 {
				t.Fatalf("%d cancel-bearing intent(s) on the %s reducing side inside "+
					"the debounce, want exactly 1; the overhang is an A12 violation "+
					"corrected at every tick, not a book blip the debounce absorbs",
					len(got), tc.reduceSide)
			}
			if got := seamCancelsOn(g.o.r.queue, tc.addSide); len(got) != 0 {
				t.Fatalf("%d cancel(s) on the %s adding side inside the debounce, "+
					"want none; the adding side is protected by the debounce until "+
					"the threshold", len(got), tc.addSide)
			}
			if places := seamPlaces(g.o.r.queue); len(places) != 0 {
				t.Fatalf("%d placement(s) queued inside the debounce before the "+
					"reducer was confirmed absent, want none", len(places))
			}

			// Exactly at the threshold: one dependent cancel-confirm-place, even
			// though the PRICE has not moved.
			if !g.stopped(gateFailDebounce) {
				t.Fatalf("the market did not stop after %v of continuous failure",
					gateFailDebounce)
			}
			if g.o.market != quote.Reducing {
				t.Fatalf("a stopped market holding %s is %s, want REDUCING",
					tc.q.Wire(), g.o.market)
			}

			retire := seamCancelsOn(g.o.r.queue, tc.reduceSide)
			if len(retire) != 1 {
				t.Fatalf("%d cancel-bearing intent(s) on the %s reducing side, "+
					"want exactly 1.\n\nThe reducer is at the touch and oversized; "+
					"§6.5's price-only Decide reports no move, so only an aggregate "+
					"SIZE check retires the overhang (H-Q-5a, H-Q-5b)",
					len(retire), tc.reduceSide)
			}
			ret := retire[0]
			if ret.Kind != quote.KindCancelConfirmPlace {
				t.Fatalf("the reducer retirement is a %s, want %s; H-QUE-2 has no "+
					"row for a standalone reducing-side cancel, so it is the first "+
					"leg of a dependent cancel-confirm-place", ret.Kind,
					quote.KindCancelConfirmPlace)
			}
			if ret.Reason != quote.ReasonReduce || ret.Role != quote.RoleReducing {
				t.Fatalf("the retirement is %s/%s, want %s/%s", ret.Role, ret.Reason,
					quote.RoleReducing, quote.ReasonReduce)
			}
			if places := seamPlaces(g.o.r.queue); len(places) != 0 {
				t.Fatalf("%d placement intent(s) were queued before the reducer was "+
					"confirmed absent; no replacement may overlap a possibly-live "+
					"reducer (H-Q-5a)", len(places))
			}
			if adds := seamCancelsOn(g.o.r.queue, tc.addSide); len(adds) != 0 {
				t.Fatalf("%d cancel(s) on the %s adding side, want none; nothing of "+
					"ours rests there to churn", len(adds), tc.addSide)
			}

			// The cancel dispatches FIRST and names the oversized order.
			writes := make(chan writeRequest, 1)
			g.o.pump(gateFailDebounce, writes)
			var req writeRequest
			select {
			case req = <-writes:
			default:
				t.Fatal("no write was dispatched for the oversized reducer at the " +
					"gate-failure stop")
			}
			if req.Op != quote.OpCancel || req.Side != tc.reduceSide {
				t.Fatalf("the dispatched write is a %s on %s, want a cancel on the "+
					"%s reducing side", req.Op, req.Side, tc.reduceSide)
			}
			named := false
			for _, o := range req.Orders {
				if o.OrderID == tc.orderID {
					named = true
				}
			}
			if !named {
				t.Fatalf("the cancel does not name the oversized order %q "+
					"(targets: %d); the first leg must retire the twelve contracts "+
					"already resting", tc.orderID, len(req.Orders))
			}

			// Repeated evaluations while the cancel is in flight do not duplicate
			// it: `enqueue`'s (market, side) dedup catches the still-queued first
			// leg, whose Op is a cancel, and no replacement escapes meanwhile.
			g.o.evaluate(gateFailDebounce)
			g.o.evaluate(gateFailDebounce)
			if n := g.o.r.queue.Len(); n != 1 {
				t.Fatalf("%d intent(s) after two further evaluations, want 1; a "+
					"cancel already in flight must not be re-queued as a second "+
					"cancellation for the same overhang", n)
			}
			if places := seamPlaces(g.o.r.queue); len(places) != 0 {
				t.Fatalf("%d placement intent(s) while the cancel is unconfirmed, "+
					"want none: no replacement before exchange-confirmed absence",
					len(places))
			}

			// The verifying read confirms the side absent, but it carries no
			// position/fill truth. A fill just before DELETE may have changed q,
			// so replacement waits for a subsequent complete portfolio cycle.
			g.o.applyWriteResult(writeResult{
				Req: req,
				Sweep: rest.SweepResult{
					Walk: rest.Walk{Outcome: rest.WalkComplete}, Clean: true,
				},
				Absent: true, Sent: true,
			})
			if !g.o.cancelConfirmed[tc.reduceSide] {
				t.Fatal("the reducing side is not recorded confirmed-absent after " +
					"a complete verifying read found nothing of ours resting")
			}
			if places := seamPlaces(g.o.r.queue); len(places) != 0 || !g.o.awaitCancelTruth {
				t.Fatalf("%d placement leg(s) after absence with pending position truth, "+
					"want none until a fresh complete poll",
					len(places))
			}
			if again := seamCancelsOn(g.o.r.queue, tc.reduceSide); len(again) != 0 {
				t.Fatalf("%d cancel(s) still queued on the reducing side after "+
					"absence, want 0", len(again))
			}

			// Model the next complete orders walk listing the cancelled order gone.
			// Target arithmetic now sizes the remainder against a clean
			// aggregate: exactly one contract -- |q| -- never the twelve.
			g.h.installResting()
			if got := g.o.atRisk(tc.reduceSide); got != 0 {
				t.Fatalf("the reducing side's aggregate is %s after the walk listed "+
					"the cancelled order gone, want 0", got.Wire())
			}
			remainder, bound := g.o.targetSize(tc.reduceSide, quote.RoleReducing)
			if remainder != one {
				t.Fatalf("the replacement reducer would be %s contracts, want "+
					"exactly %s = |q|; the retirement must rebuild the capped "+
					"target and not the twelve it cancelled (H-Q-5a)",
					remainder.Wire(), one.Wire())
			}
			if bound != tc.q.Abs() {
				t.Fatalf("the reducer's H-CO-4b bound is %s, want |q| = %s",
					bound.Wire(), tc.q.Abs().Wire())
			}
		})
	}
}

// TestTheSettlingMarketRecapsAnOversizedReducerWithoutAGateFailure is lip-732's
// falsification target: the aggregate cap is INDEPENDENT of the gate.
//
// §10.3 runs S = 12, and an ordinary legal fill can leave q = +/-1. H-CLOSE-2
// then enters SETTLING at close_time - close_lead while `gateStopped` is FALSE --
// nothing on the gate-failure path runs here at all. SizesFor caps the reducer
// at |q| = 1, yet §6.5's price-only Decide leaves the at-touch 12-contract order
// UNCHANGED whatever its size (`requote.go` returns no move for OurPrice at the
// touch). If the aggregate check were still conditioned on `gateStopped`, that
// order would remain designated as the reducer -- a reducing quote of one plus
// an eleven-contract ADDING quote on the opposite side (A12) -- and the sole
// one-contract exit H-Q-5b requires would never be placed. The prior rounds'
// suite exercised only `gateStopped == true`, so it survived exactly that.
//
// Both signs, because §6.2 chooses the reducing side from the sign of q and a
// rule written on one is not the same rule on the other.
func TestTheSettlingMarketRecapsAnOversizedReducerWithoutAGateFailure(t *testing.T) {
	one := num.QtyFromFloat(1)
	twelve := num.QtyFromFloat(12)

	cases := []struct {
		name       string
		q          num.Qty
		reduceSide quote.Side
		addSide    quote.Side
		cents      int
		orderID    string
	}{
		{
			name: "long yes: the oversized NO exit is capped in SETTLING", q: one,
			reduceSide: quote.SideNo, addSide: quote.SideYes,
			cents: 55, orderID: "EX-SET-BIG-NO",
		},
		{
			name: "short yes: the oversized YES exit is capped in SETTLING", q: -one,
			reduceSide: quote.SideYes, addSide: quote.SideNo,
			cents: 40, orderID: "EX-SET-BIG-YES",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := newGateFailOwner(t, tc.q)

			// The qualifying book is still current and no gate-failure episode is
			// open, so `gateStopped` is false throughout. The ONLY thing that
			// changes is the close, moved inside close_lead (1h) but outside
			// final_lead (60s): H-CLOSE-2's entry into SETTLING, which still
			// requires a live exit (H-CLOSE-2a). `untilClose` reads the wall clock,
			// so a fixed 30-minute offset holds for the life of the test.
			g.o.closeAt = time.Now().Add(30 * time.Minute)
			if !g.qualifies() {
				t.Fatal("the qualifying book is no longer current; this case must " +
					"run with gateStopped false")
			}

			// One reducer, twelve contracts, resting at the touch the way a
			// complete orders walk reports it (H-POS-4). |q| is one, so this is
			// eleven contracts over the cap and the price has not moved.
			g.h.installResting(risk.LiveOrder{
				OrderID: tc.orderID, Ticker: seamTicker, Side: tc.reduceSide,
				Price4: num.Price4FromCents(tc.cents), Remaining: twelve,
			})
			if got := g.o.atRisk(tc.reduceSide); got != twelve {
				t.Fatalf("the reducing side's aggregate is %s before SETTLING, "+
					"want %s", got.Wire(), twelve.Wire())
			}

			// Enter SETTLING. The market was QUOTING; the close-lead crossing takes
			// it to SETTLING on this evaluation (NextMarket checks the lead before
			// the stop path), and the SAME evaluation caps the reducer -- because
			// price-unchanged is exactly when §6.5 cannot.
			g.at(gateFailDebounce)
			now := g.h.clk.monoNow()
			g.o.evaluate(now)
			if g.o.market != quote.Settling {
				t.Fatalf("a market inside close_lead holding %s is %s, want "+
					"SETTLING (H-CLOSE-2)", tc.q.Wire(), g.o.market)
			}

			retire := seamCancelsOn(g.o.r.queue, tc.reduceSide)
			if len(retire) != 1 {
				t.Fatalf("%d cancel-bearing intent(s) on the %s reducing side, "+
					"want exactly 1.\n\nThe reducer is at the touch and oversized, "+
					"and gateStopped is FALSE; §6.5's price-only Decide reports no "+
					"move, so only an aggregate SIZE check -- INDEPENDENT of the "+
					"gate -- retires the overhang (H-Q-5a, H-Q-5b)", len(retire),
					tc.reduceSide)
			}
			ret := retire[0]
			if ret.Kind != quote.KindCancelConfirmPlace {
				t.Fatalf("the reducer retirement is a %s, want %s; H-QUE-2 has no "+
					"row for a standalone reducing-side cancel, so it is the first "+
					"leg of a dependent cancel-confirm-place", ret.Kind,
					quote.KindCancelConfirmPlace)
			}
			if ret.Reason != quote.ReasonReduce || ret.Role != quote.RoleReducing {
				t.Fatalf("the retirement is %s/%s, want %s/%s", ret.Role, ret.Reason,
					quote.RoleReducing, quote.ReasonReduce)
			}
			if places := seamPlaces(g.o.r.queue); len(places) != 0 {
				t.Fatalf("%d placement intent(s) were queued before the reducer was "+
					"confirmed absent; no replacement may overlap a possibly-live "+
					"reducer (H-Q-5a)", len(places))
			}
			if adds := seamCancelsOn(g.o.r.queue, tc.addSide); len(adds) != 0 {
				t.Fatalf("%d cancel(s) on the %s adding side, want none; nothing of "+
					"ours rests there", len(adds), tc.addSide)
			}

			// The cancel dispatches FIRST and names the oversized order.
			writes := make(chan writeRequest, 1)
			g.o.pump(now, writes)
			var req writeRequest
			select {
			case req = <-writes:
			default:
				t.Fatal("no write was dispatched for the oversized reducer on entry " +
					"to SETTLING")
			}
			if req.Op != quote.OpCancel || req.Side != tc.reduceSide {
				t.Fatalf("the dispatched write is a %s on %s, want a cancel on the "+
					"%s reducing side", req.Op, req.Side, tc.reduceSide)
			}
			named := false
			for _, o := range req.Orders {
				if o.OrderID == tc.orderID {
					named = true
				}
			}
			if !named {
				t.Fatalf("the cancel does not name the oversized order %q "+
					"(targets: %d); the first leg must retire the twelve contracts "+
					"already resting", tc.orderID, len(req.Orders))
			}

			// Repeated evaluations while the cancel is in flight do not duplicate
			// it, and no replacement escapes before absence is confirmed.
			g.o.evaluate(now)
			g.o.evaluate(now)
			if n := g.o.r.queue.Len(); n != 1 {
				t.Fatalf("%d intent(s) after two further evaluations, want 1; a "+
					"cancel already in flight must not be re-queued as a second "+
					"cancellation for the same overhang", n)
			}
			if places := seamPlaces(g.o.r.queue); len(places) != 0 {
				t.Fatalf("%d placement intent(s) while the cancel is unconfirmed, "+
					"want none: no replacement before exchange-confirmed absence",
					len(places))
			}

			// The verifying read confirms absence, but a possible fill before
			// DELETE means the replacement waits for fresh positions and fills.
			g.o.applyWriteResult(writeResult{
				Req: req,
				Sweep: rest.SweepResult{
					Walk: rest.Walk{Outcome: rest.WalkComplete}, Clean: true,
				},
				Absent: true, Sent: true,
			})
			if !g.o.cancelConfirmed[tc.reduceSide] {
				t.Fatal("the reducing side is not recorded confirmed-absent after " +
					"a complete verifying read found nothing of ours resting")
			}
			if places := seamPlaces(g.o.r.queue); len(places) != 0 || !g.o.awaitCancelTruth {
				t.Fatalf("%d placement leg(s) after absence with pending position truth, "+
					"want none until a fresh complete poll",
					len(places))
			}

			// Model the next complete orders walk listing the cancelled order gone.
			// Target arithmetic now sizes the remainder against a clean
			// aggregate: exactly one contract -- |q| -- never the twelve.
			g.h.installResting()
			if got := g.o.atRisk(tc.reduceSide); got != 0 {
				t.Fatalf("the reducing side's aggregate is %s after the walk listed "+
					"the cancelled order gone, want 0", got.Wire())
			}
			remainder, bound := g.o.targetSize(tc.reduceSide, quote.RoleReducing)
			if remainder != one {
				t.Fatalf("the replacement reducer would be %s contracts, want "+
					"exactly %s = |q|; the retirement must rebuild the capped "+
					"target and not the twelve it cancelled (H-Q-5a)",
					remainder.Wire(), one.Wire())
			}
			if bound != tc.q.Abs() {
				t.Fatalf("the reducer's H-CO-4b bound is %s, want |q| = %s",
					bound.Wire(), tc.q.Abs().Wire())
			}

			// Neither A13 nor the post-cancel position/fill reconciliation has
			// licensed placement in this owner-level fixture. The dependent leg
			// was discarded, and a later fresh poll must rederive any replacement.
			drain := make(chan writeRequest, 1)
			g.o.pump(now, drain)
			select {
			case <-drain:
				t.Fatal("the replacement placement dispatched from a non-actionable " +
					"book; A13 withholds every placement decision here")
			default:
			}
			if n := g.o.r.queue.Len(); n != 0 {
				t.Fatalf("%d intent(s) remain while the replacement awaits fresh "+
					"position truth, want 0", n)
			}

			// The replacement rests the way a complete walk reports the
			// acknowledged one-contract exit. From here the market is CONVERGED:
			// the reducing side carries exactly |q|, so `atRisk == target` and the
			// recap never re-enters. No cancel/place churn against the §16 budget,
			// and the sole live order is the capped exit.
			g.h.installResting(risk.LiveOrder{
				OrderID: tc.orderID + "-CAP", Ticker: seamTicker,
				Side: tc.reduceSide, Price4: num.Price4FromCents(tc.cents),
				Remaining: one,
			})
			g.o.cancelConfirmed[tc.reduceSide] = false
			g.o.evaluate(now)
			if g.o.market != quote.Settling {
				t.Fatalf("the market left SETTLING once the exit was capped, now %s",
					g.o.market)
			}
			if got := g.o.atRisk(tc.reduceSide); got != one {
				t.Fatalf("the reducing side's aggregate is %s, want the sole capped "+
					"exit of %s", got.Wire(), one.Wire())
			}
			if again := seamCancelsOn(g.o.r.queue, tc.reduceSide); len(again) != 0 {
				t.Fatalf("%d further cancel(s) on the reducing side once it carries "+
					"exactly |q|, want 0; atRisk == target, so the recap must not "+
					"re-enter and churn", len(again))
			}
			if places := seamPlaces(g.o.r.queue); len(places) != 0 {
				t.Fatalf("%d placement(s) once the capped exit rests, want none; a "+
					"compliant reducer is left alone", len(places))
			}

			// A full fill lands EXACTLY flat and cannot flip the sign: the exit is
			// |q| and no more (H-Q-5a), so q goes from +/-1 to 0, never to the
			// opposite sign. With nothing left to reduce the exit is retired and
			// nothing is placed on the opposite side.
			g.h.installPosition(num.QtyFromFloat(0))
			g.h.installResting()
			g.o.evaluate(now)
			if r, ok := quote.ReducingSide(g.o.r.pf.Q(seamTicker)); ok {
				t.Fatalf("a flat market still names a reducing side (%s); a "+
					"|q|-capped exit cannot overshoot zero", r)
			}
			if places := seamPlaces(g.o.r.queue); len(places) != 0 {
				t.Fatalf("%d placement(s) after the exit fully filled to flat, "+
					"want none; there is no inventory left to reduce", len(places))
			}
		})
	}
}

// TestAnAbnormalDisconnectDoesNotCancelTheCompliantExit is I1 against H-Q-5b's
// own recap: the aggregate size check must never remove the exit BECAUSE the
// harness went blind.
//
// The mechanism it pins is a composition, and every link is load-bearing.
// `ResetOnReconnect` -- which `ApplyDisconnect` requests for an ABNORMAL close
// and only an abnormal one (`ResetBooks: !clean`) -- empties every level.
// `fundedReducer` reads the reducing side's touch from exactly those levels and
// answers 0 when there is none. `SizeR` caps `Reduce` at `funded`, so `target`
// becomes 0, while `SizesFor` still reports `HasReduce` for any q != 0 -- so
// `wanted` stays true and the `!wanted` branch never sees this. A resting exit
// sized at EXACTLY the quantized |q| -- compliant, the one thing A4 requires us
// to keep -- then satisfies `atRisk(side) > target` as 1 > 0, and the recap sits
// deliberately ABOVE the `!actionable` guard so its cancel leg is dispatched
// even from a book nothing may be placed against. The exit would be deleted at
// the moment of blindness and the replacement leg held by `Conditions.Valid`
// until resnapshot: a stop path that stops reducing risk, which is precisely
// what I1 forbids.
//
// The exit here is deliberately NOT oversized. An oversized reducer is retired
// by this branch on its own merits, so a 12-contract fixture could not tell
// "retired the overhang" from "cancelled because target collapsed to zero". At
// exactly |q| the ONLY thing that can produce a reducing-side cancel is the
// collapse. The clean-close case is the control: it keeps the levels, so
// `target` is |q|, `atRisk == target`, and the same evaluation is silent for the
// ordinary reason.
//
// The adding side is not asserted on. A stopped market with inventory is
// REDUCING, and A8 requires its adding side to come off; that cancel is correct
// and unrelated.
func TestAnAbnormalDisconnectDoesNotCancelTheCompliantExit(t *testing.T) {
	one := num.QtyFromFloat(1)

	for _, tc := range []struct {
		name       string
		q          num.Qty
		reduceSide quote.Side
		cents      int
		orderID    string
	}{
		{
			name: "long yes: the compliant NO exit survives the gap", q: one,
			reduceSide: quote.SideNo, cents: 55, orderID: "EX-DISC-NO",
		},
		{
			name: "short yes: the compliant YES exit survives the gap", q: -one,
			reduceSide: quote.SideYes, cents: 40, orderID: "EX-DISC-YES",
		},
	} {
		for _, clean := range []bool{false, true} {
			class := "abnormal"
			if clean {
				class = "clean"
			}
			t.Run(tc.name+", "+class, func(t *testing.T) {
				g := newGateFailOwner(t, tc.q)

				// The exit a complete orders walk reports (H-POS-4), sized at the
				// cap rather than over it.
				g.h.installResting(risk.LiveOrder{
					OrderID: tc.orderID, Ticker: seamTicker, Side: tc.reduceSide,
					Price4: num.Price4FromCents(tc.cents), Remaining: one,
				})
				if got := g.o.atRisk(tc.reduceSide); got != one {
					t.Fatalf("the reducing side's aggregate is %s, want %s; this "+
						"case is only about a COMPLIANT exit", got.Wire(), one.Wire())
				}

				// Control: while the book is still readable the recap is silent,
				// so anything seen after the gap is attributable to the gap.
				g.at(gateFailDebounce)
				g.o.evaluate(g.h.clk.monoNow())
				if n := len(seamCancelsOn(g.o.r.queue, tc.reduceSide)); n != 0 {
					t.Fatalf("%d reducing-side cancel(s) BEFORE any disconnect; "+
						"the fixture is already retiring a compliant exit, so "+
						"nothing below would be attributable to the gap", n)
				}

				g.disconnect(clean)
				g.o.evaluate(g.h.clk.monoNow())

				if n := len(seamCancelsOn(g.o.r.queue, tc.reduceSide)); n != 0 {
					t.Fatalf("%d reducing-side cancel(s) after a %s disconnect, "+
						"want 0.\n\nThe exit is exactly the quantized |q| = %s, "+
						"which A4 requires us to KEEP. An emptied book makes "+
						"`fundedReducer` 0 and `SizeR` caps `target` at 0, so the "+
						"H-Q-5b recap reads a compliant exit as pure overshoot and "+
						"cancels it -- above the `!actionable` guard, with the "+
						"replacement held until resnapshot. I1: no stop path may "+
						"stop reducing risk.", n, class, tc.q.Abs().Wire())
				}
			})
		}
	}
}
