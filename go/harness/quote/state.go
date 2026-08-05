// Package quote is the pure per-market state machine, skew function and order
// priority queue of harness-spec.md §5, §6.
//
// Pure by the same rule that makes core pure (port-spec §3): no clock, no I/O,
// no goroutines. This is not stylistic. It is what lets §14's simulator replay
// a tape and produce a deterministic, comparable trace. Anything needing a
// clock lives in cmd/harness and is passed in.
package quote

// GlobalState is harness-spec.md §5.1.
//
// Note what is absent: there is no HALTED state, and the word "halt" does not
// appear as an identifier anywhere in this package (H-HALT-1). `halt()` is the
// name that invited the semantics that lost the money -- it cancelled resting
// orders, never flattened, and broke the observer out of its own loop in the
// same control-flow iteration.
type GlobalState uint8

const (
	// Starting reads the durable halt latch before anything else (H-HALT-4)
	// and does not place a single order until reconciliation succeeds
	// (H-ORD-5).
	Starting GlobalState = iota

	// UnknownRisk is "we may hold inventory and cannot see it". Its only job
	// is to keep trying to find out, forever, while placing nothing.
	//
	// It is deliberately NOT WindingDown (H-ORD-5a). WindingDown's entire
	// purpose is keeping a reducing quote alive, and sizing a reducer requires
	// knowing q -- which side to quote and how much. Entered from a failed
	// position read it has no q, so it can do nothing at all, and would simply
	// sit there with inventory it could not see.
	UnknownRisk

	Running

	// WindingDown is not "stopped". It is the state the probe should have
	// entered and did not: adding quotes off permanently, reducing quotes
	// live, monitoring full, process alive. There is no transition back to
	// Running without an operator action (§10.4).
	WindingDown

	// Drained does not exit the process. It idles, keeps monitoring and keeps
	// heartbeating. Exiting on drain would mean the one moment the harness is
	// safe is also the moment it stops being able to tell you anything.
	Drained
)

func (g GlobalState) String() string {
	switch g {
	case Starting:
		return "STARTING"
	case UnknownRisk:
		return "UNKNOWN_RISK"
	case Running:
		return "RUNNING"
	case WindingDown:
		return "WINDING_DOWN"
	case Drained:
		return "DRAINED"
	}
	return "INVALID"
}

// AddsRisk reports whether new inventory-taking quotes are permitted.
//
// I1: every stop path stops ADDING risk. No stop path stops reducing it, and
// no stop path stops watching it. So this is the only predicate any stop
// condition is allowed to consult -- there is deliberately no corresponding
// "may reduce" or "may monitor" predicate, because the answer to both is
// unconditionally yes and a predicate would invite a caller to check it.
func (g GlobalState) AddsRisk() bool { return g == Running }

// MarketState is harness-spec.md §5.2.
type MarketState uint8

const (
	Idle MarketState = iota
	Quoting
	Skewed

	// Reducing is one-sided: the adding side is cancelled and
	// exchange-confirmed absent (H-FAIL-3), the reducing side rests at the
	// touch capped at |q| (H-Q-5a).
	//
	// A market-level halt trigger sends the market HERE rather than to any
	// state that cancels everything. That inversion is the whole design.
	Reducing

	// Settling is entered at close_time - close_lead. It is NOT an exemption
	// from having an exit (H-CLOSE-2a): until final_lead a Settling market
	// with q != 0 must still have a reducing quote resting or an in-flight
	// intent to place one.
	//
	// Mutation M13 -- cancel everything on entry to Settling and never place
	// the capped reducer -- is probebot.py's exact defect confined to the
	// close window, and it passed every gate in an earlier version of §17.
	Settling

	Closed
)

func (m MarketState) String() string {
	switch m {
	case Idle:
		return "IDLE"
	case Quoting:
		return "QUOTING"
	case Skewed:
		return "SKEWED"
	case Reducing:
		return "REDUCING"
	case Settling:
		return "SETTLING"
	case Closed:
		return "CLOSED"
	}
	return "INVALID"
}

// AddsRisk reports whether this market state may rest an adding quote.
func (m MarketState) AddsRisk() bool { return m == Quoting || m == Skewed }

// confidence: high
