package lifecycle

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"lip/harness/quote"
	"lip/harness/rest"
)

// TestStartupOwnsStateAcrossRetrySequence is the STATEFULNESS of §7.5's
// coordinator, asserted over a whole sequence rather than over one call.
//
// # Why a sequence and not a single Step
//
// `Startup` was changed from a function taking `GlobalInput` into a stateful
// coordinator that owns the global state privately, and the reason was that the
// startup transaction had no owner: the caller manufactured the input, so it
// also manufactured `STARTING`, and a caller one forgotten field away from
// handing a latched process STARTING is H-HALT-4's failure mode with extra
// steps. Ownership is only meaningful ACROSS calls -- a coordinator that
// re-derived its state from scratch on every Step would satisfy every
// single-call assertion in this package and still have no memory at all.
//
// So this test drives ONE coordinator through five Steps and compares the whole
// trace: three consecutive failed positions walks, the third of which is §7.5's
// `startup_retries` threshold and produces UNKNOWN_RISK through §5.1's
// `!TruthReadable` rule; then a complete pass that recovers to RUNNING through
// `GTReconciled`; then one more failure, which is the step that proves the
// coordinator remembered RUNNING rather than rebuilding STARTING.
//
// That last step is the load-bearing one. `M-L-STATERESET` puts `s.state` back
// to `quote.Starting` at the top of every Step, and it is invisible for the
// first four: STARTING and a sub-threshold failure both produce STARTING,
// STARTING and UNKNOWN_RISK both produce UNKNOWN_RISK on the third, and both
// STARTING and UNKNOWN_RISK produce RUNNING on a complete pass (§5.1 draws the
// reconciled edge from both). Only a failure taken from RUNNING distinguishes
// them: the honest coordinator holds RUNNING (§5.1's RUNNING rule transitions
// only on `Stop`), while the mutant reports STARTING -- a process that has
// already reconciled and quoted announcing that it is still starting up, which
// is the state H-ORD-5 uses to decide nothing may be placed.
//
// The backoff ladder is asserted in the same trace because it reads the same
// private field. §7.5 retries indefinitely with backoff, and the ladder is per
// CONSECUTIVE failure -- `M-L-STATERESET`'s sibling failure mode is a counter
// that survives a success, which would have the harness waiting 8s before its
// next attempt after a pass that worked perfectly.
func TestStartupOwnsStateAcrossRetrySequence(t *testing.T) {
	src := okSource()

	// Kept side by side so the sequence below reads as "the endpoint went down
	// and came back", which is the failure §7.5's retry rule is written for.
	// Capturing the healthy result rather than rebuilding it keeps the recovery
	// step identical to the source every other startup test uses.
	healthy := src.positions
	broken := rest.PositionsResult{Walk: failedWalk("503")}

	s := newStartup(t, &recordingLatch{}, src, ownsAll(), keepAll(),
		newSweeper(true))

	if got := s.State(); got != quote.Starting {
		t.Fatalf("a freshly constructed Startup reports %v, want STARTING: "+
			"H-ORD-5 places nothing until reconciliation succeeds, and the "+
			"state that says so is the constructor's, not a caller's", got)
	}

	// step is one entry in the trace. `coordinator` is `s.State()` read
	// immediately after the Step and `decision` is `at.Decision.State`; they are
	// separate fields so that a coordinator whose published decision and whose
	// own memory have drifted apart fails the comparison rather than being
	// papered over by asserting only one of them. A divergence there is a
	// process acting on a state it never told anyone about.
	type step struct {
		n           int
		what        string
		decision    quote.GlobalState
		coordinator quote.GlobalState
		trigger     quote.GlobalTrigger
		after       time.Duration
		retry       bool
		adopted     bool
		sev1        bool
	}

	render := func(trace []step) string {
		lines := make([]string, 0, len(trace))
		for _, e := range trace {
			lines = append(lines, fmt.Sprintf(
				"    %d %-26s decision=%v coordinator=%v trigger=%v after=%v "+
					"retry=%v adopted=%v STARTUP_RECONCILE_FAILED=%v",
				e.n, e.what, e.decision, e.coordinator, e.trigger, e.after,
				e.retry, e.adopted, e.sev1))
		}
		return strings.Join(lines, "\n")
	}

	// The threshold is written as a LITERAL 3 -- the third row is the first
	// UNKNOWN_RISK -- rather than being derived from `startupFailureThreshold`.
	// A table computed from the constant would move with `M-L-RETRYTHRESH`; §7.5
	// says three, so the table says three.
	want := []step{
		{1, "positions walk fails", quote.Starting, quote.Starting,
			quote.GTNone, 1 * time.Second, true, false, false},
		{2, "positions walk fails", quote.Starting, quote.Starting,
			quote.GTNone, 2 * time.Second, true, false, false},
		{3, "third failure: threshold", quote.UnknownRisk, quote.UnknownRisk,
			quote.GTTruthFailed, 4 * time.Second, true, false, true},
		{4, "complete pass recovers", quote.Running, quote.Running,
			quote.GTReconciled, 0, false, true, false},
		{5, "failure from RUNNING", quote.Running, quote.Running,
			quote.GTNone, 1 * time.Second, true, false, false},
	}
	// Which Steps see a broken positions endpoint. Only the fourth is healthy,
	// so the recovery is a genuine complete walk and the fifth failure is a
	// fresh outage rather than a continuation of the first one.
	failing := []bool{true, true, true, false, true}

	got := make([]step, 0, len(failing))
	for i, fails := range failing {
		if fails {
			src.positions = broken
		} else {
			src.positions = healthy
		}

		at := s.Step(context.Background(), startupNow)

		got = append(got, step{
			n:           i + 1,
			what:        want[i].what,
			decision:    at.Decision.State,
			coordinator: s.State(),
			trigger:     at.Decision.Trigger,
			after:       at.After,
			retry:       at.Retry,
			adopted:     at.Adoption != nil,
			sev1:        hasClass(at.Anomalies, "STARTUP_RECONCILE_FAILED"),
		})

		// Asserted inline as well as in the trace, because these two are the
		// properties §7.5 states as absolutes and a reader should not have to
		// diff a table to find out one of them broke.
		if fails && !at.Retry {
			t.Fatalf("step %d did not ask to be retried; §7.5 retries "+
				"indefinitely with backoff and there is deliberately no "+
				"exhausted, terminal or gave-up outcome -- a bounded retry is a "+
				"process sitting next to inventory it decided not to look at "+
				"again", i+1)
		}
		if s.State() != at.Decision.State {
			t.Fatalf("step %d: the coordinator holds %v while it published %v; "+
				"`advance` is the only writer of the state and it writes only a "+
				"committed decision, so these cannot legitimately differ -- a "+
				"coordinator acting on a state it did not publish is one whose "+
				"heartbeat, audit and adding-authority checks disagree with "+
				"each other", i+1, s.State(), at.Decision.State)
		}
	}

	if !reflect.DeepEqual(got, want) {
		t.Fatalf("the startup state trace differs.\ngot:\n%s\nwant:\n%s\n"+
			"The trace is compared whole rather than step by step because the "+
			"property under test is that ONE coordinator carried its state, "+
			"its consecutive-failure count and its backoff ladder across every "+
			"call; any single row of it can be produced by a coordinator with "+
			"no memory at all.", render(got), render(want))
	}

	// UNKNOWN_RISK begins on EXACTLY the third failure. The trace above already
	// pins every row, but stating the boundary separately is what makes the
	// failure message name the mutation: too early is a harness that stops
	// quoting on a single flaky read, too late is a harness that keeps calling
	// its position knowledge good after it has demonstrably failed to read it
	// three times running.
	first := -1
	for _, e := range got {
		if e.decision == quote.UnknownRisk {
			first = e.n
			break
		}
	}
	if first != 3 {
		t.Fatalf("UNKNOWN_RISK first appeared at step %d, want step 3: §7.5's "+
			"`startup_retries` is 3, and H-ORD-5a chooses UNKNOWN_RISK over "+
			"WINDING_DOWN precisely because winding down would have to size a "+
			"reducer from the q that just failed to read.\ntrace:\n%s",
			first, render(got))
	}

	// The recovery edge is §5.1's, not an assignment. `Attempt` carries no
	// independent state field for exactly this reason: an earlier version had
	// one, which let the startup path manufacture STARTING and UNKNOWN_RISK
	// directly and hand a latched process STARTING -- the thing H-HALT-4 exists
	// to prevent. So RUNNING has to arrive with a committed decision carrying
	// GTReconciled, and it has to arrive from UNKNOWN_RISK, which is the edge
	// §5.1 draws only for a complete successful reconciliation.
	recovery := got[3]
	if recovery.trigger != quote.GTReconciled {
		t.Fatalf("the recovering step reached %v under trigger %v, want RUNNING "+
			"under `reconciled`: §5.1's UNKNOWN_RISK rule requires BOTH truth "+
			"readable and reconciliation complete, and a RUNNING that arrived "+
			"under any other trigger was assigned rather than adjudicated",
			recovery.decision, recovery.trigger)
	}
	if !recovery.adopted {
		t.Fatalf("the recovering step reached RUNNING with no Adoption; the " +
			"Adoption is the licence to leave STARTING, and a state machine " +
			"that publishes RUNNING without one has licensed quoting on a walk " +
			"that produced no portfolio")
	}
}

// TestStartupPublicSurfaceCannotAcceptOrResetGlobalInput is the STRUCTURAL half
// of the same argument, and it is stated over the type rather than over a
// behaviour because behaviour cannot rule out an API that nobody happens to
// call yet.
//
// # What is being forbidden and why
//
// `Startup` owns the global state privately. That ownership is worth nothing if
// any exported method accepts a `quote.GlobalInput` or a `quote.GlobalState`,
// because two distinct disasters follow immediately:
//
//   - A caller that can SUPPLY the state can hand a latched process STARTING.
//     H-HALT-4's whole mechanism is that a durable latch outlives the process
//     and forces WINDING_DOWN before anything is placed; §5.1 enforces it from
//     `in.State`, so a caller that chooses `in.State` chooses whether the latch
//     is honoured. That is the HR-009 sequence -- a taker fill latches, a panic
//     kills the process, `launchd KeepAlive` restarts it -- with the harness
//     restoring itself to STARTING on purpose.
//
//   - A caller that can supply a `GlobalInput` supplies a ZERO one by default,
//     and a zero `GlobalInput` describes an account nobody read exactly as it
//     describes a flat, quiet one. DRAINED is the single state §5.1 reaches
//     from two falses (`!AnyInventory && !AnyLiveOrder`), so the forgotten
//     field is not a harmless no-op: it is the harness announcing its inventory
//     is gone on the strength of a read that never happened, and DRAINED rests
//     no reducer -- the position it did not look at is now unmanaged.
//
// This is why `startingInput()` was deleted rather than kept as a convenience,
// and why `Step` derives every §5.1 fact from the pass that actually ran.
//
// `M-L-STATEFORGE` adds the setter or the input-taking entry point back --
// `SetState`, `Reset`, `StepWith(ctx, in, now)`, a restored `Run(ctx, in,
// now)`. No behavioural test can catch that, because the mutant's existing
// paths still behave: the damage is done by the door being there at all.
// Reflection over the exported surface is the only assertion that fails on the
// door's existence.
//
// `State() quote.GlobalState` is the ONE permitted appearance. It is read-only
// getter -- reading the state cannot forge it -- and the getter is what every
// caller uses instead of holding its own copy, so forbidding it outright would
// push callers back to tracking the state themselves, which is the shape this
// whole change removed.
func TestStartupPublicSurfaceCannotAcceptOrResetGlobalInput(t *testing.T) {
	var (
		inputType = reflect.TypeOf(quote.GlobalInput{})
		stateType = reflect.TypeOf(quote.GlobalState(0))
		startup   = reflect.TypeOf(&Startup{})
	)

	// A pointer to either type is the same door with one more level of
	// indirection, and a setter is at least as likely to be written
	// `*quote.GlobalState` as by value, so both are unwrapped before comparison.
	bare := func(t reflect.Type) reflect.Type {
		for t.Kind() == reflect.Pointer {
			t = t.Elem()
		}
		return t
	}

	// Without this the test passes vacuously against a Startup whose methods
	// were renamed or removed: an empty surface accepts no GlobalInput either.
	// The two methods named here are the whole intended API.
	for _, name := range []string{"Step", "State"} {
		if _, ok := startup.MethodByName(name); !ok {
			t.Fatalf("*Startup has no exported %s method, so this test is "+
				"asserting a rule about a surface that no longer exists; §7.5's "+
				"coordinator is driven by Step and read by State, and if either "+
				"was renamed this check must be renamed with it rather than "+
				"left passing against nothing", name)
		}
	}

	sawStateGetter := false
	for i := 0; i < startup.NumMethod(); i++ {
		m := startup.Method(i)
		sig := m.Type

		// In(0) on a method obtained from a type is the receiver.
		for j := 1; j < sig.NumIn(); j++ {
			switch bare(sig.In(j)) {
			case inputType:
				t.Errorf("exported method %s accepts a quote.GlobalInput "+
					"(argument %d of %v). §5.1's inputs may not come from a "+
					"caller: a zero GlobalInput describes an account nobody "+
					"read exactly as it describes a flat, quiet one, and "+
					"DRAINED is the one state reached by two falses -- so the "+
					"forgotten field is the harness declaring its inventory "+
					"gone on a read that never happened, with no reducer left "+
					"resting against the position it did not look at. Every "+
					"fact Step needs is derived from the walk that actually "+
					"ran.", m.Name, j, sig)
			case stateType:
				t.Errorf("exported method %s accepts a quote.GlobalState "+
					"(argument %d of %v). A caller that can supply or reset the "+
					"global state can hand a LATCHED process STARTING, which "+
					"is precisely what H-HALT-4's durable latch exists to make "+
					"impossible: §5.1 enforces the latch from the state it is "+
					"given, so choosing that state chooses whether the halt "+
					"survives the restart. The state is private and `advance` "+
					"is its only writer.", m.Name, j, sig)
			}
		}

		for j := 0; j < sig.NumOut(); j++ {
			out := bare(sig.Out(j))
			if out == inputType {
				t.Errorf("exported method %s returns a quote.GlobalInput "+
					"(result %d of %v). Handing a caller the input struct "+
					"invites it to amend one field and pass it back, which is "+
					"the supply-your-own-state hole restated as a round trip; "+
					"`startingInput()` was deleted for this reason and nothing "+
					"may reintroduce it as a method.", m.Name, j, sig)
				continue
			}
			if out != stateType {
				continue
			}
			// The one permitted appearance, and permitted only in the exact
			// read-only shape: no arguments, one result. A `State(...)` that
			// took anything would be a setter wearing the getter's name.
			if m.Name != "State" || sig.NumIn() != 1 || sig.NumOut() != 1 {
				t.Errorf("exported method %s returns a quote.GlobalState in "+
					"signature %v; the only permitted appearance of that type "+
					"on this surface is `State() quote.GlobalState`, a "+
					"read-only getter. Any other shape either sets the state "+
					"or returns it alongside something a caller can feed back, "+
					"and a caller that can reset the state can hand a latched "+
					"process STARTING.", m.Name, sig)
				continue
			}
			sawStateGetter = true
		}
	}

	if !sawStateGetter {
		t.Errorf("no `State() quote.GlobalState` getter was found on *Startup. " +
			"It is required, not merely tolerated: it is what lets callers read " +
			"the coordinator's state instead of keeping their own copy, and a " +
			"caller keeping its own copy is the shape that made the caller " +
			"manufacture GlobalInput in the first place.")
	}
}
