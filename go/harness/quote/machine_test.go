package quote

import (
	"testing"
	"time"

	"lip/harness/cfg"
	"lip/harness/num"
)

// mkt builds a MarketInput for a selected, running, unstopped market with no
// close in sight.
func mkt(st MarketState, q num.Qty) MarketInput {
	return MarketInput{
		State:      st,
		Q:          q,
		Global:     Running,
		Selected:   true,
		UntilClose: 24 * time.Hour,
		HasClose:   true,
	}
}

// TestMarketLadder walks §5.2's inventory transitions.
func TestMarketLadder(t *testing.T) {
	p := cfg.Default() // inv_soft 3, inv_hard 7

	for _, tc := range []struct {
		name string
		in   MarketInput
		want MarketState
		why  string
	}{
		{"idle selects into quoting", mkt(Idle, 0), Quoting,
			"selection is the only way in"},
		{"quoting stays under inv_soft", mkt(Quoting, qty(2)), Quoting,
			"inside the tolerance band, symmetric at S"},
		{"quoting at exactly inv_soft", mkt(Quoting, qty(3)), Quoting,
			"§5.2's edge is |q| > inv_soft, so exactly inv_soft is still " +
				"QUOTING -- the same inclusive boundary size_A uses, and the " +
				"two must agree or the taper engages in a state that quotes S"},
		{"quoting skews past inv_soft", mkt(Quoting, qty(3.01)), Skewed,
			"one quantum past the boundary is past it (H-CO-4a)"},
		{"skewed falls back", mkt(Skewed, qty(2)), Quoting,
			"|q| fell back under inv_soft"},
		{"skewed at exactly inv_hard", mkt(Skewed, qty(7)), Skewed,
			"the edge is |q| > inv_hard, and size_A is exactly 0 here -- the " +
				"market is still SKEWED with a zero-sized adding quote"},
		{"skewed reduces past inv_hard", mkt(Skewed, qty(7.01)), Reducing,
			"one quantum past inv_hard"},
		{"quoting can reach reducing directly", mkt(Quoting, qty(10)), Reducing,
			"a single fill can take |q| from under inv_soft to over inv_hard. " +
				"§5.2's diagram routes REDUCING through SKEWED; the prose says " +
				"'|q| > inv_hard', and a machine that insisted on the drawn " +
				"path would keep quoting S on both sides at ten contracts"},
		{"reducing holds above zero", mkt(Reducing, qty(0.01)), Reducing,
			"REDUCING leaves only at EXACTLY zero. One quantum is not zero, " +
				"and resuming here would replace the exit with a quote on the " +
				"other side while a position is still open"},
		{"reducing holds under inv_hard", mkt(Reducing, qty(6)), Reducing,
			"not 'under inv_hard' -- a market that reduced from 10 to 6 has " +
				"six contracts it did not want"},
		{"reducing exits at flat", mkt(Reducing, 0), Idle,
			"§5.2's q == 0 edge back to IDLE"},
		{"short side is symmetric", mkt(Quoting, qty(-10)), Reducing,
			"the ladder is on |q|"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, trig := NextMarket(tc.in, p)
			if got != tc.want {
				t.Fatalf("%s at q = %s -> %s, want %s (trigger %s) -- %s",
					tc.in.State, tc.in.Q.Wire(), got, tc.want, trig, tc.why)
			}
		})
	}
}

// TestReducingReachesIdleOnlyAtExactZero is the H-CO-4a regression at the
// transition it was gating.
//
// Fills of 0.10, 0.20 and −0.30 accumulated as float64 leave a residue near
// 5.6e-17. The market is economically flat, but `q != 0` is TRUE, so REDUCING
// never reaches IDLE -- and because H-HALT-3 exits only when every market is
// flat or closed, the process never exits either.
func TestReducingReachesIdleOnlyAtExactZero(t *testing.T) {
	p := cfg.Default()

	var q num.Qty
	for _, f := range []float64{0.10, 0.20, -0.30} {
		q += num.QtyFromFloat(f)
	}
	got, trig := NextMarket(mkt(Reducing, q), p)
	if got != Idle {
		t.Fatalf("a net-zero fill sequence left the market in %s at q = %s: "+
			"REDUCING never reaches IDLE and SIGTERM never sees a drained "+
			"market (H-CO-4a, H-HALT-3)", got, q.Wire())
	}
	if trig != MTFlat {
		t.Errorf("trigger = %s, want %s", trig, MTFlat)
	}
}

// TestMarketStopKeepsTheExit is the inversion, per market.
//
// "Note what is absent: there is no HALTED per-market state that cancels
// everything. A market-level halt trigger sends the market to REDUCING, which
// keeps the exit alive. That is the inversion."
//
// Every stop condition, at a position, must land in REDUCING -- not IDLE, not
// CLOSED, not any state whose sizes are empty. This is the shape of the defect
// that cost real money.
func TestMarketStopKeepsTheExit(t *testing.T) {
	p := cfg.Default()

	for _, tc := range []struct {
		name  string
		apply func(*MarketInput)
		trig  MarketTrigger
	}{
		{"market stop trigger", func(in *MarketInput) { in.Stop = true }, MTStop},
		{"program end_date", func(in *MarketInput) { in.ProgramEnded = true },
			MTProgramEnd},
		{"global winding down",
			func(in *MarketInput) { in.Global = WindingDown }, MTGlobalStop},
		{"global drained",
			func(in *MarketInput) { in.Global = Drained }, MTGlobalStop},
		{"global starting",
			func(in *MarketInput) { in.Global = Starting }, MTGlobalStop},
		{"global unknown risk",
			func(in *MarketInput) { in.Global = UnknownRisk }, MTGlobalStop},
		{"deselected with inventory",
			func(in *MarketInput) { in.Selected = false }, MTDeselected},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// From every state that can hold a position.
			for _, from := range []MarketState{Idle, Quoting, Skewed, Reducing} {
				in := mkt(from, qty(5))
				tc.apply(&in)
				got, trig := NextMarket(in, p)
				if got != Reducing {
					t.Fatalf("%s with a 5-contract position went to %s, want "+
						"REDUCING: a stop that removes the exit and keeps the "+
						"risk is probebot.py's defect", from, got)
				}
				if from != Reducing && trig != tc.trig {
					t.Errorf("from %s: trigger = %s, want %s", from, trig, tc.trig)
				}
			}
		})
	}
}

// TestStoppedFlatMarketCannotResumeQuoting is the second load-bearing ordering.
//
// A flat market under an active stop condition goes to IDLE -- there is nothing
// to reduce. The hazard is the next tick: IDLE's only exit is QUOTING, and if
// that edge is guarded only by `Selected`, a wedged feed that happens to be
// flat resumes adding into the exact condition that stopped it, one selection
// tick later. The guard belongs on the edge, not in whoever calls the selector.
func TestStoppedFlatMarketCannotResumeQuoting(t *testing.T) {
	p := cfg.Default()

	for _, tc := range []struct {
		name  string
		apply func(*MarketInput)
	}{
		{"feed wedged", func(in *MarketInput) { in.Stop = true }},
		{"program ended", func(in *MarketInput) { in.ProgramEnded = true }},
		{"global winding down", func(in *MarketInput) { in.Global = WindingDown }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := mkt(Reducing, 0)
			tc.apply(&in)

			got, _ := NextMarket(in, p)
			if got != Idle {
				t.Fatalf("a flat stopped market went to %s, want IDLE", got)
			}

			// The next tick, still stopped, still selected.
			in.State = Idle
			if got, _ := NextMarket(in, p); got != Idle {
				t.Fatalf("a flat stopped market in IDLE went to %s: it is "+
					"still selected and the stop condition is still live, so "+
					"this resumes adding into the condition that stopped it",
					got)
			}

			// And it recovers once the condition clears.
			clear := mkt(Idle, 0)
			if got, _ := NextMarket(clear, p); got != Quoting {
				t.Fatalf("a flat market with nothing stopping it went to %s, "+
					"want QUOTING -- the guard must not be a one-way door",
					got)
			}
		})
	}
}

// TestCloseLeadOutranksTheInventoryTriggers is H-CLOSE-2 against §5.2's ladder.
//
// A market that is both over inv_hard and inside its close lead is SETTLING.
// Both states cancel the adding side and rest a capped reducer, so the
// difference looks cosmetic -- but only SETTLING carries H-CLOSE-3's final
// cancel, and a market that stayed in REDUCING through the close would rest
// orders into it.
func TestCloseLeadOutranksTheInventoryTriggers(t *testing.T) {
	p := cfg.Default() // close_lead 1h

	for _, from := range []MarketState{Idle, Quoting, Skewed, Reducing} {
		for _, q := range []num.Qty{0, qty(2), qty(5), qty(50), qty(-50)} {
			in := mkt(from, q)
			in.UntilClose = 30 * time.Minute // inside the lead
			in.Stop = true                   // and stopped, for good measure

			got, trig := NextMarket(in, p)
			if got != Settling {
				t.Fatalf("%s at q = %s, 30 minutes from close, went to %s: "+
					"H-CLOSE-2 enters SETTLING at close_time − close_lead and "+
					"nothing outranks it except the close itself",
					from, q.Wire(), got)
			}
			if from != Settling && trig != MTCloseLead {
				t.Errorf("trigger = %s, want %s", trig, MTCloseLead)
			}
		}
	}

	// Exactly at the lead, inclusive.
	in := mkt(Quoting, qty(5))
	in.UntilClose = p.CloseLead
	if got, _ := NextMarket(in, p); got != Settling {
		t.Errorf("at exactly close_lead the market was %s, want SETTLING", got)
	}
	// One nanosecond outside it, not yet.
	in.UntilClose = p.CloseLead + time.Nanosecond
	if got, _ := NextMarket(in, p); got != Skewed {
		t.Errorf("outside the lead the market was %s, want the ordinary "+
			"ladder to govern", got)
	}
}

// TestUnknownCloseTimeDoesNotSettle pins the HasClose flag.
//
// A market whose close_time has not been read is not a market closing in zero
// seconds, and it is not a market closing in a year either. A zero-valued
// duration read as "now" would put every market into SETTLING at startup,
// before the first schedule poll -- which is M13's observable produced by a
// missing flag rather than by a missing placement.
func TestUnknownCloseTimeDoesNotSettle(t *testing.T) {
	p := cfg.Default()

	in := mkt(Quoting, qty(5))
	in.HasClose, in.UntilClose = false, 0

	if got, _ := NextMarket(in, p); got != Skewed {
		t.Fatalf("a market with no known close_time went to %s: an unread "+
			"schedule is not a close at t = 0", got)
	}
}

// TestTradingCloseIsObservedNotComputed is H-CLOSE-4.
//
// can_close_early markets settle before close_time, so the arithmetic is not
// authoritative. CLOSED is entered on the observation, and it is terminal.
func TestTradingCloseIsObservedNotComputed(t *testing.T) {
	p := cfg.Default()

	in := mkt(Quoting, qty(5))
	in.UntilClose = 10 * time.Hour // nowhere near the lead
	in.TradingClosed = true

	got, trig := NextMarket(in, p)
	if got != Closed {
		t.Fatalf("an observed close with ten hours on the clock left the "+
			"market in %s: H-CLOSE-4 says the lead is not a guarantee", got)
	}
	if trig != MTTradingClose {
		t.Errorf("trigger = %s, want %s", trig, MTTradingClose)
	}

	// Terminal: nothing reopens a settled market, whatever else is true.
	reopen := mkt(Closed, qty(5))
	reopen.TradingClosed = false
	if got, _ := NextMarket(reopen, p); got != Closed {
		t.Errorf("a CLOSED market moved to %s", got)
	}
}

// TestSettlingLeavesOnlyIfTheScheduleMoves covers the reverse edge, and it is
// the HQL-004 regression.
//
// H-CLOSE-0 treats close_time as not static. If it moves back out beyond the
// lead, a market left in SETTLING would have its adding side abandoned
// indefinitely on the strength of a deadline that no longer exists.
//
// An earlier version answered that with a "conservative" path returning
// REDUCING for any nonzero position. That was worse, not safer, and the second
// tick is what shows it: REDUCING leaves only at exactly zero, so q = +5 --
// below inv_hard = 7, ordinary SKEWED inventory -- was trapped one-sided for
// the whole remaining five hours, at roughly two thirds of the market's reward
// rate. The comment claiming it would "re-earn its adding side on the next
// tick" was false, and the original test never took that tick.
func TestSettlingLeavesOnlyIfTheScheduleMoves(t *testing.T) {
	p := cfg.Default()

	for _, tc := range []struct {
		name string
		q    num.Qty
		want MarketState
		why  string
	}{
		{"ordinary skewed inventory", qty(5), Skewed,
			"inv_soft < 5 < inv_hard is SKEWED: adding side tapered, exit " +
				"capped. REDUCING here would be a one-sided market for as " +
				"long as the exit takes to fill"},
		{"inside the tolerance band", qty(2), Quoting,
			"|q| <= inv_soft quotes S on both sides"},
		{"flat", 0, Quoting,
			"nothing held, nothing stopping it: the ordinary ladder selects it"},
		{"past inv_hard", qty(10), Reducing,
			"the ladder still reduces what it should"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := mkt(Settling, tc.q)
			in.UntilClose = 5 * time.Hour // the close moved out

			got, _ := NextMarket(in, p)
			if got != tc.want {
				t.Fatalf("a SETTLING market at q = %s whose close moved five "+
					"hours out went to %s, want %s -- %s",
					tc.q.Wire(), got, tc.want, tc.why)
			}

			// The second tick, which is where the trap showed. Whatever state
			// it reached must be stable, not a staging post it cannot leave.
			in.State = got
			if again, _ := NextMarket(in, p); again != tc.want {
				t.Fatalf("the market moved on from %s to %s on the next tick",
					got, again)
			}
		})
	}
}

// TestGlobalStartingReadsTheLatchFirst is H-HALT-4 and the HR-009 sequence.
//
// A taker fill latches global WINDING_DOWN -- the most serious stop condition
// in the system. An unrelated panic kills the process. launchd KeepAlive
// restarts it. Without this edge, STARTING has no notion of a prior halt and
// flat markets resume adding: the halt self-clears with no operator action,
// because the supervision policy the spec mandates erased the safety state the
// spec mandates.
func TestGlobalStartingReadsTheLatchFirst(t *testing.T) {
	in := GlobalInput{
		State:         Starting,
		Latched:       true,
		TruthReadable: true,
		Reconciled:    true, // everything else says "go"
	}
	got, trig := NextGlobal(in)
	if got != WindingDown {
		t.Fatalf("STARTING with a durable latch went to %s, want WINDING_DOWN: "+
			"the latch is read before anything else, and only an operator "+
			"clears it (§10.4)", got)
	}
	if trig != GTLatch {
		t.Errorf("trigger = %s, want %s", trig, GTLatch)
	}

	// A14: a latch on disk implies WINDING_DOWN or DRAINED. If one appears
	// while RUNNING, the latch wins -- it is the durable record.
	if got, _ := NextGlobal(GlobalInput{State: Running, Latched: true}); got != WindingDown {
		t.Errorf("RUNNING with a latch on disk stayed %s: A14 makes the "+
			"latch and the in-memory state disagreeing an invariant "+
			"violation, and the file is authoritative because SQLite may be "+
			"the thing that failed", got)
	}
}

// TestGlobalNeverReturnsToRunning is §5.1's one-way door.
//
// "There is no transition from WINDING_DOWN back to RUNNING without an
// operator action (§10.4)." This function offers none, and the sweep proves it
// over every combination of inputs rather than over the ones anyone thought of.
func TestGlobalNeverReturnsToRunning(t *testing.T) {
	for _, latched := range []bool{false, true} {
		for _, truth := range []bool{false, true} {
			for _, recon := range []bool{false, true} {
				for _, stop := range []bool{false, true} {
					for _, inv := range []bool{false, true} {
						for _, from := range []GlobalState{WindingDown, Drained} {
							in := GlobalInput{
								State: from, Latched: latched,
								TruthReadable: truth, Reconciled: recon,
								Stop: stop, AnyInventory: inv,
							}
							if got, trig := NextGlobal(in); got == Running {
								t.Fatalf("%s -> RUNNING (trigger %s) with "+
									"%+v: the harness never self-clears a "+
									"global halt", from, trig, in)
							}
						}
					}
				}
			}
		}
	}
}

// TestGlobalUnknownRiskNeedsBothConditions.
//
// "It never places, never exits, and never decays into RUNNING without a
// complete successful reconciliation." UNKNOWN_RISK is the state for "we may
// hold inventory and cannot see it", and leaving it on a partial read is
// exactly the thing it exists to refuse.
func TestGlobalUnknownRiskNeedsBothConditions(t *testing.T) {
	for _, tc := range []struct {
		truth, recon bool
		want         GlobalState
	}{
		{false, false, UnknownRisk},
		{true, false, UnknownRisk},
		{false, true, UnknownRisk},
		{true, true, Running},
	} {
		in := GlobalInput{
			State: UnknownRisk, TruthReadable: tc.truth, Reconciled: tc.recon,
		}
		if got, _ := NextGlobal(in); got != tc.want {
			t.Errorf("UNKNOWN_RISK with truth=%v reconciled=%v -> %s, want %s",
				tc.truth, tc.recon, got, tc.want)
		}
	}

	// And it honours the latch on the way out, rather than reaching RUNNING
	// through the recovery path.
	in := GlobalInput{
		State: UnknownRisk, TruthReadable: true, Reconciled: true, Latched: true,
	}
	if got, _ := NextGlobal(in); got != WindingDown {
		t.Errorf("UNKNOWN_RISK recovered to %s with a latch set, want "+
			"WINDING_DOWN -- §5.1 recovers 'to the state the latch implies'",
			got)
	}
}

// TestGlobalNoRunningToUnknownRiskEdge pins a deliberate omission.
//
// UNKNOWN_RISK reads as the natural home for "position polls are failing", but
// it places nothing AND reduces nothing -- §5.1 gives it "no reducing quotes,
// q is unknown, so no reducer can be sized". Entering it from RUNNING would
// cancel a reducer already sized correctly from the last good read, in response
// to a read failure. H-FAIL-4 and A13 handle stale truth the right way instead:
// dispatch stops, resting orders stay, and the exit survives the outage.
func TestGlobalNoRunningToUnknownRiskEdge(t *testing.T) {
	in := GlobalInput{State: Running, TruthReadable: false, Reconciled: false}
	got, _ := NextGlobal(in)
	if got == UnknownRisk {
		t.Fatal("RUNNING moved to UNKNOWN_RISK on a failed truth read: that " +
			"state sizes no reducer, so a read failure would cancel an exit " +
			"we had already sized correctly")
	}
	if got != Running {
		t.Errorf("RUNNING moved to %s on a failed truth read, want to stay "+
			"RUNNING with dispatch gated by H-FAIL-4", got)
	}
}

// TestGlobalDrainAndReturn covers both ends of DRAINED.
func TestGlobalDrainAndReturn(t *testing.T) {
	got, trig := NextGlobal(GlobalInput{State: WindingDown, AnyInventory: false})
	if got != Drained || trig != GTDrained {
		t.Errorf("WINDING_DOWN with everything flat -> %s (%s), want DRAINED",
			got, trig)
	}
	if got, _ := NextGlobal(GlobalInput{State: WindingDown, AnyInventory: true}); got != WindingDown {
		t.Error("WINDING_DOWN drained with inventory still open")
	}

	// Inventory reappearing under DRAINED -- a fill reported late, a poll
	// disagreeing, an adoption at restart -- returns to the state whose job is
	// keeping a reducing quote alive.
	got, trig = NextGlobal(GlobalInput{State: Drained, AnyInventory: true})
	if got != WindingDown {
		t.Fatalf("DRAINED with inventory stayed %s: DRAINED rests no reducer, "+
			"so this abandons the position (I1)", got)
	}
	if trig != GTInventory {
		t.Errorf("trigger = %s, want %s", trig, GTInventory)
	}
}

// TestGlobalStopIsOnlyEverOneWay sweeps every input from RUNNING.
func TestGlobalStopIsOnlyEverOneWay(t *testing.T) {
	in := GlobalInput{State: Running, TruthReadable: true, Reconciled: true, Stop: true}
	got, trig := NextGlobal(in)
	if got != WindingDown {
		t.Fatalf("a global stop trigger left the state at %s", got)
	}
	if trig != GTStop {
		t.Errorf("trigger = %s, want %s", trig, GTStop)
	}
	// And WINDING_DOWN never adds, whatever the market machine is told.
	if WindingDown.AddsRisk() {
		t.Error("WINDING_DOWN.AddsRisk() is true")
	}
	for _, g := range []GlobalState{Starting, UnknownRisk, WindingDown, Drained} {
		if g.AddsRisk() {
			t.Errorf("%s.AddsRisk() is true: I1 permits adding in RUNNING only", g)
		}
	}
	if !Running.AddsRisk() {
		t.Error("RUNNING.AddsRisk() is false")
	}
}

// TestDueCloseActionsRunInLeadOrder is H-CLOSE-0's catch-up, and the HQL-001
// regression.
//
// HR-017: at schedule_poll_s = 300 with final_lead = 60s, a close_time that
// moves from 17:00 to 12:03 at 12:00:01 is next observed at 12:05 -- after the
// close. Neither the close lead nor the final cancel ever ran. When a newly
// observed schedule is already inside both leads, both actions are due.
//
// **They are returned in LEAD order -- close lead first.** An earlier version
// returned the final cancel first, reading "with the final cancel taking
// precedence over everything else" as an ordering instruction. Run that way the
// catch-up cancels everything, confirms it absent, and then the close-lead step
// enters SETTLING -- at which point the next sizing pass sees a SETTLING market
// with q != 0, asks for the capped reducer, and places an order into the final
// minute that the final cancel had just removed. The final cancel was undone by
// the step after it.
//
// This test pins the ORDER; TestNothingRestsIntoTheClose pins the END STATE,
// which is the property that actually matters and which the old test never
// checked.
func TestDueCloseActionsRunInLeadOrder(t *testing.T) {

	p := cfg.Default() // close_lead 1h, final_lead 60s

	for _, tc := range []struct {
		name  string
		until time.Duration
		want  []CloseAction
	}{
		{"far out", 4 * time.Hour, nil},
		{"inside the close lead", 30 * time.Minute, []CloseAction{ActionCloseLead}},
		{"at the close lead", p.CloseLead, []CloseAction{ActionCloseLead}},
		{"inside the final lead", 30 * time.Second,
			[]CloseAction{ActionCloseLead, ActionFinalCancel}},
		{"at the final lead", p.FinalLead,
			[]CloseAction{ActionCloseLead, ActionFinalCancel}},
		{"already past the close", -time.Minute,
			[]CloseAction{ActionCloseLead, ActionFinalCancel}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := DueCloseActions(tc.until, true, p)
			if len(got) != len(tc.want) {
				t.Fatalf("DueCloseActions(%v) = %v, want %v", tc.until, got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("DueCloseActions(%v)[%d] = %s, want %s -- the "+
						"actions run in lead order, so the close lead does "+
						"not re-place what the final cancel just removed",
						tc.until, i, got[i], tc.want[i])
				}
			}
		})
	}

	// An unread schedule schedules nothing. A zero duration read as "now"
	// would fire the final cancel on every market before the first poll.
	if got := DueCloseActions(0, false, p); got != nil {
		t.Errorf("DueCloseActions with no known close_time = %v, want none", got)
	}
}

// TestNothingRestsIntoTheClose is H-CLOSE-3's end state, and the finding the
// old close tests could not have caught.
//
// "At close_time − final_lead, cancel everything in that market and verify with
// a sweep. Nothing of ours rests into the close."
//
// The old pair of tests checked two cheap halves that were each correct:
// DueCloseActions returned both actions, and SizesFor(Settling, q != 0) always
// emitted the capped reducer. Their COMPOSITION violated the rule -- the final
// cancel confirmed every order absent, and the very next sizing pass asked for
// the reducer again. Neither test could see it, because neither looked at what
// was resting afterwards.
//
// This one asserts the end state: past final_lead, in every state, at every
// position, the sizing pass requests nothing and reports the cancel obligation
// instead.
func TestNothingRestsIntoTheClose(t *testing.T) {
	p := cfg.Default() // close_lead 1h, final_lead 60s

	for _, until := range []time.Duration{p.FinalLead, 30 * time.Second, 0, -time.Minute} {
		past := PastFinalLead(until, true, p)
		if !past {
			t.Fatalf("PastFinalLead(%v) = false, but final_lead is %v",
				until, p.FinalLead)
		}
		for _, st := range []MarketState{Idle, Quoting, Skewed, Reducing, Settling} {
			for _, q := range []num.Qty{0, qty(0.01), qty(5), qty(-12), qty(50)} {
				s := SizesFor(SizeInput{
					State: st, Q: q, Funded: qty(1000), PastFinalLead: past,
				}, p)

				if s.HasAdd || s.HasReduce {
					t.Fatalf("%s at q = %s, %v from close, still wants to "+
						"quote (add %v/%s, reduce %v/%s): H-CLOSE-3 says "+
						"nothing of ours rests into the close, and A4's "+
						"obligation lapses by its own terms -- 'the exemption "+
						"applies only after final_lead, after trading close, "+
						"or at q = 0'",
						st, q.Wire(), until,
						s.HasAdd, s.Add.Wire(), s.HasReduce, s.Reduce.Wire())
				}
				if !s.FinalCancel {
					t.Fatalf("%s at q = %s past final_lead did not report the "+
						"cancel obligation: quoting nothing and HAVING nothing "+
						"resting are different claims, and only the second is "+
						"H-CLOSE-3", st, q.Wire())
				}
			}
		}
	}

	// Inside the close lead but not yet past final_lead, the exit is still
	// required. This is H-CLOSE-2a, and it is the boundary M13 lives on: the
	// exemption begins at final_lead, never at SETTLING entry.
	s := SizesFor(SizeInput{
		State: Settling, Q: qty(12), Funded: qty(1000),
		PastFinalLead: PastFinalLead(30*time.Minute, true, p),
	}, p)
	if !s.HasReduce || s.Reduce != qty(12) {
		t.Fatalf("a SETTLING market 30 minutes from close sized its exit at "+
			"%s (has %v), want 12.00: until final_lead a SETTLING market with "+
			"q != 0 MUST have a reducing quote resting or an in-flight intent "+
			"to place one. M13 -- cancel everything on entry to SETTLING and "+
			"never place the capped reducer -- is probebot.py's exact defect "+
			"confined to the close window, and it passed every gate in an "+
			"earlier version of §17", s.Reduce.Wire(), s.HasReduce)
	}
	if s.FinalCancel {
		t.Error("the cancel obligation was reported an hour early, which is " +
			"M13 arriving through the latch instead of through the sizing")
	}

	// An unread schedule does not latch. A zero duration read as "now" would
	// silence every market before the first schedule poll.
	if PastFinalLead(0, false, p) {
		t.Error("PastFinalLead latched on a market whose close_time has not " +
			"been read: an unread schedule is not a close at t = 0")
	}
}

// TestDrainRequiresOrdersConfirmedAbsent is the HQL-002 regression.
//
// §5.1 draws WINDING_DOWN → DRAINED at "all flat". Flat is necessary and not
// sufficient: H-FAIL-3 says an order we have merely requested a cancel for is
// live and fillable until the exchange confirms it absent, so an account that
// reads flat while its cancels are in flight is one ignored cancel away from
// being long again -- and DRAINED rests no reducer.
//
// The sequence is ordinary, not exotic: SIGTERM enters WINDING_DOWN, cancels
// are dispatched, and inventory reads zero before any of their responses land.
// Declaring the drain complete there is a stop condition that adds risk after
// announcing it has stopped.
func TestDrainRequiresOrdersConfirmedAbsent(t *testing.T) {
	for _, tc := range []struct {
		name      string
		inventory bool
		liveOrder bool
		want      GlobalState
	}{
		{"flat and clean", false, false, Drained},
		{"flat with cancels in flight", false, true, WindingDown},
		{"inventory open", true, false, WindingDown},
		{"both", true, true, WindingDown},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, _ := NextGlobal(GlobalInput{
				State:        WindingDown,
				AnyInventory: tc.inventory,
				AnyLiveOrder: tc.liveOrder,
			})
			if got != tc.want {
				t.Fatalf("WINDING_DOWN with inventory=%v liveOrder=%v -> %s, "+
					"want %s: a flat account with 12 YES resting at 50c is not "+
					"drained, it is one ignored cancel away from q = +12",
					tc.inventory, tc.liveOrder, got, tc.want)
			}
		})
	}

	// And an order reappearing under DRAINED brings the reducer back, for the
	// same reason it blocks the drain in the first place: it is fillable.
	got, trig := NextGlobal(GlobalInput{State: Drained, AnyLiveOrder: true})
	if got != WindingDown || trig != GTInventory {
		t.Errorf("DRAINED with a live order -> %s (%s), want WINDING_DOWN",
			got, trig)
	}
}

// TestLatchOutranksEveryOtherRule is the HQL-005 regression.
//
// A14: a global halt latch on disk implies the global state is WINDING_DOWN or
// DRAINED. An earlier version checked the latch inside each state's own
// branch, and UNKNOWN_RISK returned early on an unreadable truth read before
// ever reaching it -- so a process that took a SIGTERM while its position reads
// were failing sat in UNKNOWN_RISK with a durable stop on disk that no state
// audit or heartbeat would report.
//
// Nothing is traded away by the correction: neither state places anything. But
// WINDING_DOWN is the one that says so out loud, and it is the one with no path
// back to RUNNING without an operator (§10.4).
func TestLatchOutranksEveryOtherRule(t *testing.T) {
	for _, from := range []GlobalState{Starting, UnknownRisk, Running} {
		for _, truth := range []bool{false, true} {
			for _, recon := range []bool{false, true} {
				for _, stop := range []bool{false, true} {
					in := GlobalInput{
						State: from, Latched: true,
						TruthReadable: truth, Reconciled: recon, Stop: stop,
					}
					got, trig := NextGlobal(in)
					if got != WindingDown {
						t.Fatalf("%s with a durable latch, truth=%v "+
							"reconciled=%v stop=%v -> %s, want WINDING_DOWN "+
							"(A14)", from, truth, recon, stop, got)
					}
					if trig != GTLatch {
						t.Errorf("trigger = %s, want %s", trig, GTLatch)
					}
				}
			}
		}
	}

	// A latch does not drag DRAINED backwards -- DRAINED already satisfies A14,
	// and re-entering WINDING_DOWN from it on the strength of the latch alone
	// would make the drain unreachable while the latch is set, which is always.
	if got, _ := NextGlobal(GlobalInput{State: Drained, Latched: true}); got != Drained {
		t.Errorf("DRAINED with a latch -> %s: A14 admits WINDING_DOWN OR "+
			"DRAINED, and the latch is never self-cleared, so a latch that "+
			"forced WINDING_DOWN would make the drain permanently unreachable",
			got)
	}
}
