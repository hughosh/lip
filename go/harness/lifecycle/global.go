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

	// RetryLatch asks the caller to CommitStop again with a well-formed cause
	// before advancing. A failed latch write is a transient disk condition far
	// more often than a permanent one, and the cost of retrying is nothing.
	RetryLatch bool

	Anomalies []risk.Anomaly
}

// StopCommit is what CommitStop did to the DURABLE latch, and nothing else.
//
// Splitting this out of GlobalDecision is the whole point of lip-eyq's boundary
// change. The previous API fused two different authorities into one call:
// "make this cause durable" and "tell me the next global state". Fusing them
// meant the ordinary STARTING -> RUNNING edge had to be expressed as a stop with
// an empty cause, so a zero StopCause acquired a second meaning -- "no stop is
// being requested" -- on a type whose only job is to describe a stop. A caller
// that forgot an argument got that meaning by accident.
//
// Separated, each call says one thing. CommitStop cannot transition anything and
// Advance cannot write anything, so neither can be induced to do the other's job
// by a malformed argument.
type StopCommit struct {
	// Durable is the only success condition: the cause reached disk and the
	// cached latch is now set. A caller may not publish a halted state without
	// it.
	Durable bool

	// BlockAdding is I1's response to every uncertainty here, and is
	// deliberately the ONLY prohibition: cancels, reducing quotes, position
	// polling, reconciliation and monitoring all continue while it is set.
	BlockAdding bool

	// RetryLatch asks the caller to commit again. A failed latch write is a
	// transient disk condition far more often than a permanent one.
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
//
// lip-eyq made that structural rather than defensive. The one call became two --
// `CommitStop` writes and `Advance` transitions -- so the two authorities cannot
// be confused by an argument at all: a call that writes cannot return a state,
// and a call that returns a state cannot write. `Advance` additionally refuses a
// halted state the caller merely ASSERTS, because nothing this controller
// produces can be halted without a committed cause.
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

// CommitStop makes a cause DURABLE and updates the cached latch. It transitions
// nothing and returns no state: that is Advance's job, and the separation is
// what stops a forgotten argument from producing an uncommitted halt.
//
// H-HALT-4 fixes the order, and the order is across the two calls rather than
// inside one: the latch reaches disk here, and only a later Advance can publish
// the transition it justifies. A caller that commits and then never advances has
// a durable stop that the next process start will read -- which is the correct
// failure direction. A caller that advances without committing gets refused.
//
// Setting the cached latch here, before any Advance, is why a stop discovered
// during STARTING cannot flicker through RUNNING. Without it, a foreign fill
// found during reconciliation would hand NextGlobal a STARTING input with
// Reconciled true, get RUNNING back, and the market would be quotable for
// exactly as long as it took the next tick to notice. With it, the next Advance
// carries Latched=true and A14's rule fires first, producing
// WINDING_DOWN/GTLatch directly.
//
// A zero StopCause is not special-cased. It has no Trigger, so validateCause
// rejects it like any other malformed cause -- the sentinel meaning is gone.
func (c *GlobalController) CommitStop(cause StopCause) StopCommit {
	rec := cause.record()
	if err := validateCause(rec); err != nil {
		// A malformed cause is a programming error, and the response is NOT to
		// drop the stop. Adding is blocked and the caller is asked to retry with
		// a well-formed cause; the harness does not keep quoting because the
		// thing that wanted it to stop filled the struct in wrong.
		return StopCommit{
			BlockAdding: true, RetryLatch: true,
			Anomalies: []risk.Anomaly{{
				Class: "LATCH_WRITE_FAILED", Sev: risk.SEV1, Ticker: cause.Market,
				Text: fmt.Sprintf("a global stop was requested with a cause that "+
					"cannot be latched, so the stop is not durable and adding is "+
					"blocked until it is: %v", err),
			}},
		}
	}

	durable, err := c.store.Ensure(rec)
	if err != nil || !durable {
		// The disk failed. I1 decides the rest: this stops ADDING and stops
		// nothing else. Cancels, the reducing quote, position polling,
		// reconciliation and the monitor all continue, because a harness that
		// stopped managing its inventory because it could not write a file has
		// converted a storage failure into an unobserved position.
		//
		// The cached latch is NOT set. Setting it would leave the process
		// believing it had latched, and a restart would find no file and resume
		// adding -- HR-009 exactly, with the harness having been told the write
		// failed.
		if err == nil {
			err = errors.New("the latch store reported the record as not durable")
		}
		return StopCommit{
			BlockAdding: true, RetryLatch: true,
			Anomalies: []risk.Anomaly{{
				Class: "LATCH_WRITE_FAILED", Sev: risk.SEV1, Ticker: cause.Market,
				Text: fmt.Sprintf("the durable halt latch could not be written for "+
					"trigger %q, so this stop would not survive a restart; adding "+
					"is blocked and the write is retried, while cancelling, "+
					"reducing, reconciling and monitoring continue: %v",
					cause.Trigger, err),
			}},
		}
	}

	c.latched = true
	return StopCommit{Durable: true}
}

// Advance injects the cached latch and calls quote.NextGlobal. It writes
// nothing: if a transition needs a durable justification, CommitStop must
// already have supplied it.
//
// Two inputs are refused, and both are refusals of a state the caller is
// ASSERTING rather than one the machine produced:
//
//  1. `Stop` with nothing latched. This is the causeless stop: NextGlobal's
//     RUNNING rule is `if in.Stop { return WindingDown, GTStop }`, so honouring
//     it would publish a halt with nothing on disk -- one that a panic and
//     `launchd KeepAlive` erase completely. Every §12 trigger is one forgotten
//     CommitStop away from that.
//
//  2. A purported existing state of WINDING_DOWN or DRAINED with nothing
//     latched. Nothing Advance produced can be in either state unlatched --
//     rule 1 is the only door into WINDING_DOWN and it requires the latch -- so
//     such an input did not come from this machine. Accepting it would let a
//     caller walk a forged WINDING_DOWN to DRAINED and have the harness declare
//     its drain complete with no durable record that it ever stopped. DRAINED
//     rests no reducer, so the forged path ends with an unmanaged position and
//     nothing on disk to explain it.
//
// Both refusals block adding and publish nothing, per I1.
func (c *GlobalController) Advance(in quote.GlobalInput) GlobalDecision {
	in.Latched = c.latched

	if !c.latched {
		if in.Stop {
			return c.refuse(in, "", risk.Anomaly{
				Class: "STOP_CAUSE_MISSING", Sev: risk.SEV1,
				Text: "a global stop was requested through GlobalInput.Stop " +
					"with nothing committed to the durable halt latch, so the " +
					"stop would not survive a restart; adding is blocked and " +
					"the caller must CommitStop with the trigger, timestamp " +
					"and market H-HALT-4 requires before advancing",
			})
		}
		if in.State == quote.WindingDown || in.State == quote.Drained {
			return c.refuse(in, "", risk.Anomaly{
				Class: "GLOBAL_STATE_UNJUSTIFIED", Sev: risk.SEV1,
				Text: fmt.Sprintf("the caller asserted global state %s with "+
					"nothing on the durable halt latch; no state this "+
					"controller produced can be halted without a committed "+
					"cause, so this input did not come from the state machine "+
					"and advancing it could drain a halt that was never "+
					"recorded; adding is blocked and nothing is published",
					in.State),
			})
		}
	}

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
