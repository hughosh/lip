package lifecycle

import (
	"errors"
	"fmt"

	"lip/harness/quote"
	"lip/harness/risk"
)

// StopCause is why a durable global stop is being requested: H-HALT-4's
// "trigger, timestamp and market".
//
// `Market` is allowed to be empty. `pnl_kill`, a foreign fill and a SIGTERM are
// account-wide and name no market; a taker fill and a hard position drift do.
type StopCause struct {
	Trigger  string
	Market   string
	TsMillis int64
}

func (c StopCause) record() LatchRecord {
	return LatchRecord{
		Version:  latchVersion,
		Trigger:  c.Trigger,
		TsMillis: c.TsMillis,
		Market:   c.Market,
	}
}

// GlobalDecision is one adjudicated global-state decision.
//
// `State` and `Trigger` are `quote.NextGlobal`'s verbatim output. They are NOT
// this type's opinion: H-HALT-2 says there is exactly one place the global state
// transitions, and this package is not it. What this type adds is the ORDER --
// whether the latch reached disk before the transition became publishable.
type GlobalDecision struct {
	State   quote.GlobalState
	Trigger quote.GlobalTrigger

	// Committed means a transition was returned FOR PUBLICATION: the durable
	// state on disk already justifies it, either because the latch was written
	// during this call or because it was already there, or because no stop was
	// involved at all.
	//
	// False means no such transition is being returned. `State` is then the
	// input state -- the caller publishes nothing, blocks adding, and retries.
	Committed bool

	// BlockAdding is I1's response to every uncertainty in this file. It is
	// deliberately the ONLY prohibition: cancels, reducing quotes, position
	// polling, reconciliation and monitoring all continue while it is set.
	BlockAdding bool

	// RetryLatch asks the caller to call Decide again with a well-formed cause.
	// A failed latch write is a transient disk condition far more often than a
	// permanent one, and the cost of retrying is nothing.
	RetryLatch bool

	Anomalies []risk.Anomaly
}

// BootstrapEffects is what reading the latch at construction produced. It is
// returned FROM THE CONSTRUCTOR, not from a method, because the read has to have
// happened before anything else can.
type BootstrapEffects struct {
	// Latched is what NextGlobal will be fed as GlobalInput.Latched for the rest
	// of the process's life. It never goes back to false: H-HALT-4's "the
	// harness never self-clears it".
	Latched     bool
	BlockAdding bool
	RetryLatch  bool
	Anomalies   []risk.Anomaly
}

// GlobalController is the write-before-transition ordering of H-HALT-4, and it
// is a CHOKE POINT rather than a helper.
//
// # The problem it solves
//
// H-HALT-4 requires the latch be written "before the in-memory state changes".
// `quote.NextGlobal` is pure and takes `Latched` as an INPUT -- it cannot write
// anything, and making it able to would end H-HALT-2's property that one pure
// function is the whole of the state machine. So the ordering has to live
// somewhere that both calls `NextGlobal` and owns the disk, and every path that
// can stop the harness has to go through it.
//
// # Why "goes through it" is not enough on its own
//
// An earlier version of this type performed the write-then-transition sequence
// correctly whenever it was handed a cause -- and passed `GlobalInput` straight
// through when it was not. `NextGlobal`'s RUNNING rule is `if in.Stop { return
// WindingDown, GTStop }`, so a caller that set `Stop` on the input and supplied
// no cause got a committed WINDING_DOWN with nothing on disk. That is every
// §12 trigger -- `inv_kill`, `pnl_kill`, a taker fill, `insufficient_balance`,
// hard position drift, the `harness.stop` sentinel -- one forgotten argument
// away from a halt that a panic and `launchd KeepAlive` erase completely.
//
// So the rule is stated over the OUTPUT, not over the arguments: this type never
// returns a publishable transition into WINDING_DOWN unless the durable state on
// disk already justifies it. `Stop` without a cause is refused.
type GlobalController struct {
	store   LatchStore
	latched bool
	booted  bool
}

// NewGlobalController reads the latch IMMEDIATELY, and that immediacy is the
// mechanism rather than a convention.
//
// H-HALT-4: "`STARTING` reads the latch **before any placement**". The stronger
// property available here is that it reads the latch before any REST call at
// all, because `NewStartup` (adopt.go) cannot be constructed without a
// controller that has already bootstrapped. There is no ordering comment to
// obey: a `Startup` that could issue the first `GET /portfolio/positions` before
// the latch had been consulted is not constructible.
//
// A read error does not fail construction. Failing construction would leave the
// process with no controller at all, which is a process that cannot latch the
// stop it is in the middle of discovering. It bootstraps LATCHED instead, with
// SEV1 and a retry request.
func NewGlobalController(store LatchStore) (*GlobalController, BootstrapEffects, error) {
	if store == nil {
		return nil, BootstrapEffects{}, errors.New("no latch store: the " +
			"durable halt latch has no in-memory fallback, because a latch that " +
			"does not outlive the process is not a latch")
	}
	c := &GlobalController{store: store, booted: true}

	_, present, err := store.Load()
	eff := BootstrapEffects{Latched: present}
	if err != nil {
		// Present-but-uninterpretable, or unreadable. Latched, loudly.
		eff.Latched = true
		eff.BlockAdding = true
		eff.RetryLatch = true
		eff.Anomalies = append(eff.Anomalies, latchInvalid(err))
	}
	c.latched = eff.Latched
	return c, eff, nil
}

// Latched is what GlobalInput.Latched must be set to. It is monotone: once true,
// forever true for this process.
func (c *GlobalController) Latched() bool { return c.latched }

// Decide is the whole protocol, in the order H-HALT-4 fixes.
//
// A zero `StopCause` means "no stop is being requested BY THIS CALL", which is
// how the ordinary STARTING -> RUNNING and WINDING_DOWN -> DRAINED edges are
// taken. It is not permission to stop without a cause: see `in.Stop` below.
//
// When a stop IS requested the sequence is:
//
//  1. validate the cause -- a latch with no trigger is useless to the operator;
//  2. Ensure it durable;
//  3. set the cached Latched input;
//  4. call quote.NextGlobal;
//  5. only then return a publishable transition.
//
// Step 3 before step 4 is why a stop discovered during STARTING cannot flicker
// through RUNNING. Without it, a foreign fill found during reconciliation would
// hand `NextGlobal` a STARTING input with `Reconciled` true, get RUNNING back,
// and the market would be quotable for exactly as long as it took the next tick
// to notice. With it, the input carries `Latched=true` and A14's rule fires
// first, producing WINDING_DOWN/GTLatch directly.
func (c *GlobalController) Decide(in quote.GlobalInput, cause StopCause) GlobalDecision {
	in.Latched = c.latched

	if cause == (StopCause{}) {
		// No cause supplied. If the INPUT nonetheless asks to stop, and nothing
		// durable on disk already justifies stopping, this is the causeless stop
		// described on the type: refuse it rather than publish a halt that a
		// restart erases.
		if in.Stop && !c.latched {
			return c.refuse(in, "", risk.Anomaly{
				Class: "STOP_CAUSE_MISSING", Sev: risk.SEV1,
				Text: "a global stop was requested through GlobalInput.Stop " +
					"with no StopCause, so there is nothing to write to the " +
					"durable halt latch and the stop would not survive a " +
					"restart; adding is blocked and the caller must retry with " +
					"the trigger, timestamp and market H-HALT-4 requires",
			})
		}
		st, tr := quote.NextGlobal(in)
		return GlobalDecision{State: st, Trigger: tr, Committed: true}
	}

	rec := cause.record()
	if err := validateCause(rec); err != nil {
		// A malformed cause is a programming error, and the response is NOT to
		// drop the stop. Adding is blocked and the caller is asked to retry with
		// a well-formed cause; the harness does not keep quoting because the
		// thing that wanted it to stop filled the struct in wrong.
		return c.refuse(in, cause.Market, risk.Anomaly{
			Class: "LATCH_WRITE_FAILED", Sev: risk.SEV1, Ticker: cause.Market,
			Text: fmt.Sprintf("a global stop was requested with a cause that "+
				"cannot be latched, so the stop is not durable and adding is "+
				"blocked until it is: %v", err),
		})
	}

	durable, err := c.store.Ensure(rec)
	if err != nil || !durable {
		// The disk failed. I1 decides the rest: this stops ADDING and stops
		// nothing else. Cancels, the reducing quote, position polling,
		// reconciliation and the monitor all continue, because a harness that
		// stopped managing its inventory because it could not write a file has
		// converted a storage failure into an unobserved position.
		//
		// The in-memory transition is NOT returned. Returning it would leave the
		// process in WINDING_DOWN believing it had latched, and a restart would
		// find no file and resume adding -- HR-009 exactly, with the harness
		// having been told the write failed.
		if err == nil {
			err = errors.New("the latch store reported the record as not durable")
		}
		return c.refuse(in, cause.Market, risk.Anomaly{
			Class: "LATCH_WRITE_FAILED", Sev: risk.SEV1, Ticker: cause.Market,
			Text: fmt.Sprintf("the durable halt latch could not be written for "+
				"trigger %q, so this stop would not survive a restart; adding "+
				"is blocked and the write is retried, while cancelling, "+
				"reducing, reconciling and monitoring continue: %v",
				cause.Trigger, err),
		})
	}

	c.latched = true
	in.Latched = true
	st, tr := quote.NextGlobal(in)
	return GlobalDecision{State: st, Trigger: tr, Committed: true}
}

// refuse is the one shape every uncertainty in this file resolves to: no
// transition published, adding blocked, the write retried, everything else
// still running.
func (c *GlobalController) refuse(in quote.GlobalInput, market string,
	anom risk.Anomaly) GlobalDecision {

	return GlobalDecision{
		State:       in.State,
		Trigger:     quote.GTNone,
		Committed:   false,
		BlockAdding: true,
		RetryLatch:  true,
		Anomalies:   []risk.Anomaly{anom},
	}
}

// confidence: high
