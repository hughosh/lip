package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"lip/harness/cfg"
	"lip/harness/num"
	"lip/harness/quote"
	"lip/harness/rest"
	"lip/harness/risk"
)

// startupFailureThreshold is §7.5's `startup_retries` (3).
//
// It is a private constant and NOT a `cfg.Params` field. §16's parameter table
// does not list it, and `cfg.Params` is derived from that table field-for-field
// -- `params_test.go` asserts the struct against a fixed map, so adding a knob
// here would be a spec deviation dressed as configuration.
//
// It is also not a tuning knob in substance. What follows the third failure is
// UNKNOWN_RISK, which places nothing and retries forever; the number chooses how
// long we tolerate a flaky read before saying so out loud, not how much risk we
// take. It is pinned by BEHAVIOUR in
// `TestUnknownRiskBeginsOnThirdFailureAndRetriesForever`, which writes the
// literal 3, so `M-L-RETRYTHRESH` cannot pass by editing one literal in two
// places.
const startupFailureThreshold = 3

// maxSweepRewalks bounds the cancel-then-rewalk loop of §7.5.
//
// H-ORD-5c can cancel adopted orders, and an adoption may not be built from the
// position read taken BEFORE that cancellation -- an order that filled while it
// was being cancelled leaves `q` stale. So a pass that cancelled anything
// discards its result and rewalks.
//
// Termination does not depend on this bound: `CancelAndSweep` returns `Clean`
// only when a COMPLETE verifying read found none of the requested orders
// resting, and an order the exchange has confirmed absent cannot come back. An
// honest source therefore cancels nothing on the rewalk. The bound exists
// because an exchange that keeps re-reporting orders it confirmed gone is
// lying about the one fact this procedure rests on, and an unbounded loop in a
// startup path is a hang rather than an error.
const maxSweepRewalks = 3

// resolveConfirmAttempts is how many COMPLETE unfiltered orders walks must fail
// to mention a coid before its reservation is declared abandoned.
//
// It exists because of a direct tension with H-ORD-2a, stated at
// `rest/write.go:307`: "A complete walk that does NOT find the coid does not
// downgrade the outcome. The order landed; it is simply no longer open. Absence
// is not evidence in either direction." Abandoning a reservation on one missing
// coid is precisely absence-as-evidence, and the record it writes is permanent
// -- `ResolveReservationAbandoned` refuses a coid that later binds.
//
// What makes repetition admissible where a single read is not: the walk is
// UNFILTERED, so it lists terminal orders too. A coid the exchange ever accepted
// appears in it whatever became of the order. Missing from three consecutive
// complete walks is therefore not "the order closed"; it is "the exchange has no
// record of this coid", which is what H-ORD-6's crash-before-dispatch leaves
// behind and is the only thing abandonment is for.
//
// Something must drain the set or the first reservation that never reached the
// exchange defers every unrecognised fill for the lifetime of the deployment,
// which converts F2's repair from "no false foreign" into "no foreign ever".
const resolveConfirmAttempts = 3

// startupBackoff is the retry ladder in seconds: 1, 2, 4, 8, 16, 32, 60, then 60
// forever. It matches `wsx`'s reconnect ladder because the failure it paces --
// an endpoint that is not answering -- is the same failure.
var startupBackoff = [...]time.Duration{
	1 * time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second,
	16 * time.Second, 32 * time.Second, 60 * time.Second,
}

func startupBackoffAt(consecutiveFailures int) time.Duration {
	i := consecutiveFailures - 1
	if i < 0 {
		i = 0
	}
	if i >= len(startupBackoff) {
		return startupBackoff[len(startupBackoff)-1]
	}
	return startupBackoff[i]
}

// PortfolioSource is the four reads of §7.5 steps 1-4. `*rest.Client` satisfies
// it; the scenario exchange and the tests substitute for it.
type PortfolioSource interface {
	Positions(ctx context.Context) rest.PositionsResult
	Orders(ctx context.Context, ticker, status string) rest.OrdersResult
	Fills(ctx context.Context, ticker string, since time.Time) rest.FillsResult
	Balance(ctx context.Context) (rest.Balance, error)
}

// CancelSweeper is H-ORD-4's verified cancel. `*rest.Client` satisfies it.
type CancelSweeper interface {
	CancelAndSweep(ctx context.Context, ticker string,
		orders []rest.Order) rest.SweepResult
}

// ReservationResolver is the ownership ledger's write half, as narrowly as this
// package needs it, and it is what makes `adopt`'s refusal-to-conclude
// TERMINATE.
//
// `ForeignEffects.Unresolved` is the ledger saying "this fill is not bound and I
// hold a reservation that could still turn out to be it". Startup must not
// conclude anything is foreign while that set is non-empty -- doing so latches a
// durable, operator-only global stop about our own order. But nothing drains the
// set on its own: H-ORD-6 commits the reservation before dispatch, so a crash
// between the two leaves a reservation no ACK will ever resolve, and a startup
// that merely waits for it waits forever.
//
// So this is the drain, from both ends. Bind what the unfiltered orders walk can
// still identify; abandon what the exchange demonstrably never took.
//
// It is `*hstore.Store` in production. It is stated as an interface here for the
// same reason `wsx.OrderBinder` is: reading ownership is something every fill
// path does, writing a resolution is something exactly one walk does, and one
// interface carrying both would hand the write to every caller that only needed
// the read.
type ReservationResolver interface {
	// UnresolvedReservations is the coids with a committed reservation and no
	// committed resolution, as a copy.
	UnresolvedReservations() map[string]struct{}
	// BindListedOrder submits the coid -> order-id binding. It returns when the
	// record is QUEUED, not when it is durable.
	BindListedOrder(coid, orderID string, boundMs int64) error
	// AbandonListedReservation submits the abandonment of a coid the exchange
	// never took. Queued, not durable, for the same reason.
	AbandonListedReservation(coid string, abandonedMs int64) error
}

// Startup runs §7.5, and it is a STATEFUL COORDINATOR rather than a function.
//
// # Why it owns the global state instead of receiving it
//
// The previous shape was `Run(ctx, quote.GlobalInput, now)`: the caller
// manufactured the input connecting this procedure to the state machine. That
// left the startup transaction without an owner. Causes were discovered here and
// committed only after the whole fallible walk finished; only this type knew the
// positions and resting orders, yet the caller supplied `AnyInventory` and
// `AnyLiveOrder`; and every one of the 25 startup tests reset the missing state
// through the same zero-valued helper, so no production code ever assigned
// either risk field.
//
// A zero-valued `GlobalInput` describes an account nobody read exactly as it
// describes a flat, quiet one, and `DRAINED` is the one state reached by two
// falses. The caller was therefore one forgotten field away from the harness
// announcing its inventory was gone on the strength of a read that never
// happened -- and DRAINED rests no reducer, so the position it did not look at
// is now unmanaged.
//
// So the state is PRIVATE, it starts at `STARTING`, and it is updated only from
// a decision the controller committed. A caller cannot inject it, reset it or
// replay it, because there is no exported surface that accepts one.
type Startup struct {
	ctrl     *GlobalController
	src      PortfolioSource
	guard    *ForeignGuard
	policy   AdoptionPolicy
	sweep    CancelSweeper
	resolver ReservationResolver
	params   cfg.Params

	// selected is the operator's selection set. It may be empty; H-ORD-5b's
	// union does not require it to be non-empty, and an account being adopted
	// with no selection is exactly the WINDING_DOWN restart case.
	selected []string

	// consecutiveFailures counts attempts since the last complete one.
	consecutiveFailures int

	// state is this coordinator's global state. Private, `STARTING` at
	// construction, and written in exactly one place: `advance`, from a
	// committed decision. `M-L-STATERESET` puts it back to STARTING on every
	// Step and `M-L-STATEFORGE` adds a setter.
	state quote.GlobalState

	// causes is every durable stop this coordinator has discovered across every
	// attempt, deduplicated by trigger and market.
	//
	// It persists across attempts because a rewalk DISCARDS its adoption: a pass
	// that cancelled something rewalks from a fresh read, and a foreign fill it
	// found on the way is not un-found by the discard. Committing is immediate
	// and durable, so the ledger is already correct; this is what keeps the
	// returned Adoption's `Causes()` honest about a stop discovered two rewalks
	// ago. `M-L-CAUSEFAIL` postpones the commitment to final success instead.
	causes []StopCause
	seen   map[string]struct{}

	// missedCoid counts CONSECUTIVE complete unfiltered walks that did not
	// mention each still-outstanding coid. See `resolveConfirmAttempts`.
	missedCoid map[string]int

	// pendingCause is the FIRST cause a walk discovered and could not make
	// durable, retained ACROSS attempts and retried before the next walk.
	//
	// Retaining it is not belt-and-braces, and `BlockAdding` is the benign half
	// here -- nothing places before an Adoption exists. `RetryLatch` is the half
	// that bites. `commitCause` records a cause only AFTER a successful write,
	// and a failed one sends the whole Step back to the caller to retry with a
	// FRESH CLOCK -- so Step 3's `since := now.Add(-s.params.Backfill)`
	// recomputes a moving boundary on every attempt. A fill discovered near the
	// edge of `backfill_h` whose latch write failed can fall outside the window
	// on the next attempt and never be discovered again, and the cause is then
	// lost for exactly lip-vxo's reason: the retry was left to whether the
	// condition happened to recur.
	//
	// The FIRST cause is kept, matching `FileLatch.Ensure`'s first-writer-wins.
	pendingCause StopCause
	pendingHeld  bool

	// baseline is the union of the trade ids of every fill every COMPLETE
	// fills walk of this startup has seen, taken BEFORE `backfill_h` filtered
	// any of them out. It is identity and nothing else.
	//
	// It is the boundary between "history" and "live", and it is drawn on
	// IDENTITY AND PHASE rather than on a timestamp because no timestamp
	// available here is sound. Step 3 asks for `backfill_h` of history while
	// the live poll asks for all of it with a zero `since`; the filter is
	// applied client-side after a complete walk, and a malformed `ts` bypasses
	// it deliberately. So a trade the exchange stamped last week can be absent
	// from the startup window and present in the first live walk, and an
	// adoption-time cut-off would call it new.
	//
	// It accumulates across ATTEMPTS for the same reason `causes` does: a pass
	// that cancelled something rewalks from a fresh read and discards its
	// adoption, but a trade it saw is not un-seen by the discard, and every
	// pass of this Step ran before the adoption that ends it. Anything observed
	// here predates the licence to leave STARTING, which is exactly what makes
	// it history.
	baseline map[string]struct{}
}

// NewStartup requires every collaborator. A nil one fails construction.
//
// The controller argument is not decoration: taking a *GlobalController that can
// only be produced by NewGlobalController -- which performs the latch read
// itself -- means there is no ordering to remember. `TestLatchReadPrecedesEveryPortfolioRequest`
// asserts the consequence, and `M-L-BOOTORDER` breaks it on purpose.
func NewStartup(ctrl *GlobalController, src PortfolioSource, guard *ForeignGuard,
	policy AdoptionPolicy, sweep CancelSweeper, resolver ReservationResolver,
	params cfg.Params, selected []string) (*Startup, error) {

	if ctrl == nil || !ctrl.booted {
		return nil, errors.New("no bootstrapped global controller: H-HALT-4 " +
			"requires the durable latch be read before any placement, and " +
			"requiring a booted controller here makes it read before the first " +
			"portfolio request rather than merely before the first order")
	}
	if src == nil {
		return nil, errors.New("no portfolio source")
	}
	if guard == nil {
		return nil, errors.New("no foreign-activity guard: H-ORD-9's " +
			"classification is not optional on a dedicated account, it is the " +
			"assertion that makes the account dedicated")
	}
	if policy == nil {
		return nil, errors.New("no adoption policy: H-ORD-5c validates every " +
			"adopted order against current selection, close, size, price and " +
			"capital rules, and there is deliberately no keep-by-default path")
	}
	if sweep == nil {
		return nil, errors.New("no cancel sweeper: H-ORD-5c cancels and SWEEPS " +
			"an invalid adopted order, and H-FAIL-3 makes an unverified cancel " +
			"a live order")
	}
	if resolver == nil {
		// Required rather than nil-tolerant, unlike `wsx.OrderBinder`. A nil
		// binder there degrades to resolving reservations more slowly while the
		// process keeps running; a nil resolver HERE degrades to never leaving
		// STARTING, because the one thing that drains `Unresolved` is gone and
		// classification is forbidden to conclude while it is non-empty. An
		// optional collaborator whose absence is an unannounced permanent hang
		// is not optional.
		return nil, errors.New("no reservation resolver: startup may not " +
			"conclude any fill is foreign while the ownership ledger holds " +
			"reservations it has neither bound nor abandoned, and this walk is " +
			"the only thing that drains that set; without it STARTING is never " +
			"left rather than left too early")
	}
	if err := params.Validate(); err != nil {
		return nil, err
	}
	return &Startup{
		ctrl: ctrl, src: src, guard: guard, policy: policy, sweep: sweep,
		resolver: resolver, params: params,
		selected:   append([]string(nil), selected...),
		state:      quote.Starting,
		seen:       make(map[string]struct{}),
		missedCoid: make(map[string]int),
		baseline:   make(map[string]struct{}),
	}, nil
}

// State is the coordinator's current global state. Read-only by construction:
// the field is unexported and nothing in this package's exported surface assigns
// it from an argument.
func (s *Startup) State() quote.GlobalState { return s.state }

// Attempt is one pass through §7.5. Its outcome is either a complete Adoption or
// an error; there is no third value.
type Attempt struct {
	// Adoption is non-nil only on a complete pass whose every durable cause was
	// already committed.
	Adoption Adoption
	// Err is why this pass did not complete.
	Err error
	// Decision is the adjudicated global-state decision, produced by the
	// controller. There is deliberately no independently assigned state field:
	// an earlier version returned its own `State`, which meant the startup path
	// manufactured `STARTING` and `UNKNOWN_RISK` directly instead of going
	// through §5.1 -- so a latched process could be handed STARTING by the very
	// procedure H-HALT-4 exists to gate.
	Decision GlobalDecision
	// Retry is ALWAYS true on failure. §7.5: "retries indefinitely with
	// backoff". There is no exhausted, terminal or gave-up outcome, and the
	// absence is the rule -- a bounded retry that eventually stops is a process
	// sitting next to inventory it decided not to look at again.
	Retry bool
	// After is how long to wait before the next attempt.
	After     time.Duration
	Anomalies []risk.Anomaly
}

// walkResult distinguishes the three ways a pass ends, because they call for
// three different inputs to §5.1 and an earlier version collapsed two of them.
type walkResult struct {
	ad    *adoption
	anoms []risk.Anomaly
	// latchFailed means a discovered cause could not be made durable. It is NOT
	// an ordinary error: the reads all worked, so truth is readable and this
	// must not decay to UNKNOWN_RISK, but no Adoption may be returned either.
	latchFailed bool
	err         error
}

// Step performs one attempt and adjudicates its outcome through the controller.
//
// It takes no GlobalInput. Every fact §5.1 needs is derived here, from what this
// pass actually established:
//
//   - `TruthReadable` is whether the reads worked, not whether a caller thought
//     they did.
//   - `Reconciled` is a COMPLETE pass, so §5.1 -- not an assignment -- is what
//     takes STARTING to RUNNING.
//   - `AnyInventory` comes from the seeded positions and `AnyLiveOrder` from the
//     final kept orders, both from this pass.
//   - `RiskKnown` is set by exactly one shape of pass: complete, final, and
//     having cancelled nothing. Anything else leaves it false, so an incomplete
//     or latched attempt stays WINDING_DOWN rather than reporting a false drain.
func (s *Startup) Step(ctx context.Context, now time.Time) Attempt {
	// A cause an EARLIER attempt discovered and could not make durable is
	// retried first, and no walk happens until it is on disk. The order is the
	// rule: this walk would recompute `backfill_h` from a fresh clock, so a
	// cause left to be re-discovered by it may have aged out of the window that
	// discovered it. See `pendingCause`.
	if anoms, ok := s.retryPending(); !ok {
		return s.latchBlocked(walkResult{anoms: anoms})
	}

	res := s.walk(ctx, now)

	switch {
	case res.latchFailed:
		return s.latchBlocked(res)
	case res.err != nil:
		return s.fail(res.anoms, res.err)
	}

	s.consecutiveFailures = 0
	res.ad.causes = s.carriedCauses()

	// The ONE Advance of this Step (§3.8). The pass is complete and cancelled
	// nothing, so its own reads describe the account as it is now and the risk
	// flags are answers rather than zeroes.
	dec := s.advance(quote.GlobalInput{
		TruthReadable: true,
		Reconciled:    true,
		RiskKnown:     true,
		AnyInventory:  res.ad.anyInventory,
		AnyLiveOrder:  res.ad.anyLiveOrder,
	})
	res.ad.anomalies = append(res.ad.anomalies, dec.Anomalies...)
	return Attempt{
		Adoption:  res.ad,
		Decision:  dec,
		Anomalies: append(res.anoms, dec.Anomalies...),
	}
}

// latchBlocked is §3.6: a cause was discovered and could not be made durable.
//
// Leaving STARTING on the strength of a stop we failed to record is the HR-009
// sequence with the harness having been TOLD the write failed, so there is no
// Adoption. But the reads themselves worked, so this is not a truth failure and
// must not decay into UNKNOWN_RISK -- and the cancellation work the pass already
// did stands, because cancelling is safe on facts we hold and a harness that
// stopped managing inventory because it could not write a file has converted a
// storage failure into an unobserved position.
func (s *Startup) latchBlocked(res walkResult) Attempt {
	s.consecutiveFailures++
	dec := s.advance(quote.GlobalInput{
		TruthReadable: true,
		Reconciled:    false,
	})
	dec.BlockAdding = true
	dec.RetryLatch = true
	return Attempt{
		Err: errors.New("a startup stop cause could not be made durable, so " +
			"this reconciliation does not license leaving STARTING; cancelling, " +
			"reducing, reconciling and monitoring continue and the write is " +
			"retried"),
		Decision:  dec,
		Retry:     true,
		After:     startupBackoffAt(s.consecutiveFailures),
		Anomalies: append(res.anoms, dec.Anomalies...),
	}
}

// fail applies §7.5's retry semantics, and reaches UNKNOWN_RISK through §5.1
// rather than by assigning it.
//
// The route is `GlobalInput.TruthReadable`. §5.1's STARTING rule is "if
// !TruthReadable -> UNKNOWN_RISK/GTTruthFailed", so the third consecutive
// failure sets that input to false and the state machine produces the state.
// Before the threshold, truth is reported readable and STARTING holds itself
// (the input is not `Reconciled`, so §5.1 returns the same state).
func (s *Startup) fail(anoms []risk.Anomaly, err error) Attempt {
	s.consecutiveFailures++

	// H-ORD-5a. NOT WindingDown: winding down exists to keep a REDUCING quote
	// alive, and sizing a reducer needs a q we just failed to read. UNKNOWN_RISK's
	// only job is to keep trying to find out, forever, while placing nothing.
	dec := s.advance(quote.GlobalInput{
		TruthReadable: s.consecutiveFailures < startupFailureThreshold,
		Reconciled:    false,
	})

	at := Attempt{
		Err:       err,
		Decision:  dec,
		Retry:     true,
		After:     startupBackoffAt(s.consecutiveFailures),
		Anomalies: anoms,
	}
	if s.consecutiveFailures >= startupFailureThreshold {
		at.Anomalies = append(at.Anomalies, risk.Anomaly{
			Class: "STARTUP_RECONCILE_FAILED", Sev: risk.SEV1,
			Text: fmt.Sprintf("startup reconciliation has failed %d consecutive "+
				"times, so the harness enters %v: it places nothing, it does "+
				"not exit, and it retries indefinitely with backoff until a "+
				"complete read succeeds (%v)",
				s.consecutiveFailures, at.Decision.State, err),
		})
	}
	at.Anomalies = append(at.Anomalies, at.Decision.Anomalies...)
	return at
}

// advance is the ONLY writer of s.state, and it writes only what the controller
// committed.
//
// The caller supplies the facts it established; the state itself is this
// coordinator's and is never an argument. An uncommitted decision leaves the
// state untouched, because a refusal publishes nothing.
func (s *Startup) advance(in quote.GlobalInput) GlobalDecision {
	in.State = s.state
	dec := s.ctrl.Advance(in)
	if dec.Committed {
		s.state = dec.State
	}
	return dec
}

// commitCause makes one discovered cause durable IMMEDIATELY and records it for
// the eventual Adoption.
//
// Immediacy is the rule §3.3-3.5 exists to state. The previous shape returned
// causes on the Adoption for the caller to latch afterwards, so the foreign-fill
// test proved only that a diligent caller COULD do the right thing -- and every
// fallible step between discovery and the end of the walk was a chance for the
// cause to be lost. A foreign fill found during reconciliation must not be
// returnable as a to-do item.
//
// Returns false when the cause could not be made durable. The caller keeps
// working -- cancelling is still safe -- but may not return an Adoption.
func (s *Startup) commitCause(c StopCause) ([]risk.Anomaly, bool) {
	sc := s.ctrl.CommitStop(c)
	if !sc.Durable {
		// `RetryLatch`, honoured across attempts rather than within one. See
		// `pendingCause`: the next walk recomputes a moving backfill boundary,
		// so a cause left to be re-discovered may not be discoverable.
		if !s.pendingHeld {
			s.pendingCause, s.pendingHeld = c, true
		}
		return sc.Anomalies, false
	}
	key := c.Trigger + "\x00" + c.Market
	if _, dup := s.seen[key]; !dup {
		s.seen[key] = struct{}{}
		s.causes = append(s.causes, c)
	}
	return sc.Anomalies, true
}

// retryPending writes the held cause again, and reports whether a walk may
// proceed. It is a no-op when nothing is held.
//
// The hold is cleared BEFORE the attempt so that `commitCause` treats this as an
// ordinary commit: on success it records the cause in `causes` exactly as the
// walk that discovered it would have, and on failure it holds the same cause
// again. Nothing else re-derives it, which is the point -- the cause outlives
// the walk that found it.
//
// A successful commit returns no anomalies (`StopCommit{Durable: true}` carries
// none), so there is nothing to thread into the Attempt on that path.
func (s *Startup) retryPending() ([]risk.Anomaly, bool) {
	if !s.pendingHeld {
		return nil, true
	}
	c := s.pendingCause
	s.pendingHeld = false
	return s.commitCause(c)
}

func (s *Startup) carriedCauses() []StopCause {
	return append([]StopCause(nil), s.causes...)
}

// walk runs §7.5 steps 1-7, rewalking after any pass that cancelled.
func (s *Startup) walk(ctx context.Context, now time.Time) walkResult {
	var anoms []risk.Anomaly
	totalCancelled := 0
	for round := 0; round <= maxSweepRewalks; round++ {
		res := s.attempt(ctx, now)
		anoms = append(anoms, res.anoms...)
		if res.latchFailed || res.err != nil {
			// No adoption in either case, and the walk-level accumulator is what
			// the caller reports: a rewalk's anomalies are this Step's too.
			return walkResult{
				anoms: anoms, latchFailed: res.latchFailed, err: res.err,
			}
		}
		if res.cancelled == 0 {
			// The summary reports what this STARTUP cancelled, across every
			// pass, not what its final pass did -- which is always zero.
			res.ad.summary.CancelledStale = totalCancelled
			res.ad.anomalies = anoms
			return walkResult{ad: res.ad, anoms: anoms}
		}
		totalCancelled += res.cancelled
		// This pass cancelled orders and its position read predates them. An
		// order that filled while it was being cancelled would leave `q` stale,
		// and `q` is what sizes every reducer from here on -- so the result is
		// discarded and all four reads run again. The failure counter is NOT
		// touched: this is a successful pass doing more work, not a failure.
		// Any cause it discovered is already durable and already recorded on the
		// coordinator, so the discard loses nothing (§3.7).
		anoms = append(anoms, risk.Anomaly{
			Class: "STARTUP_REWALK", Sev: risk.SEV2,
			Text: fmt.Sprintf("%d invalid adopted order(s) were cancelled and "+
				"swept clean, so the four startup reads are being run again: "+
				"the position read that preceded the cancellation cannot "+
				"license quoting, because an order that filled while it was "+
				"being cancelled leaves q stale", res.cancelled),
		})
	}
	return walkResult{
		anoms: anoms,
		err: fmt.Errorf("startup still had orders to cancel after %d clean "+
			"sweeps; a verified sweep confirms the exchange no longer shows "+
			"them resting, so an order that keeps reappearing means the reads "+
			"contradict the sweeps", maxSweepRewalks),
	}
}

// passResult is one pass's outcome, including how many orders it cancelled.
type passResult struct {
	ad          *adoption
	anoms       []risk.Anomaly
	cancelled   int
	latchFailed bool
	err         error
}

// attempt is one pass through §7.5 steps 1-9.
//
// The steps run in the order §7.5 and lip-eyq §3 fix between them, and the order
// is load bearing in five places:
//
//   - positions BEFORE fills, because the exchange has already told us where the
//     position ended up. `risk.Seed` then records the historical fills' identities
//     and their taker classification without replaying them onto that answer.
//   - the BALANCE ERROR IS RETAINED, not returned. It is a read failure like any
//     other, but returning at it skips classification entirely -- and a foreign
//     fill is a durable global stop whether or not the capital model could be
//     seeded in the same pass. Classification runs on the raw orders and fills,
//     which are already complete by then; only the conversion and the policy need
//     the balance, and both come after.
//   - the RESOLVE WALK before classification, so the ledger is as drained as this
//     pass can make it before anything is called foreign.
//   - CLASSIFICATION before CONVERSION, because conversion can fail on a fill we
//     were never going to apply. A foreign fill whose `fee_cost` is missing must
//     still be a `FOREIGN_FILL` and a durable stop; converting first turns it
//     into a generic retryable error, and after `backfill_h` it stops being
//     reported at all.
//   - the policy and the sweep BEFORE the result exists, because H-ORD-5c
//     validates "before leaving STARTING".
//
// No failure returns a partial adoption. An incomplete walk, a decode error, a
// policy error, an unavailable ledger or an unclean sweep all return an error
// and leave the adoption nil -- because the only thing this object is FOR is
// licensing the exit from STARTING, and a partial one licenses it on partial
// evidence. H-PAGE-1's "stale, never empty" is the same argument: a truncated
// positions walk that reads as flat everywhere is the single most dangerous
// wrong answer these endpoints can give.
func (s *Startup) attempt(ctx context.Context, now time.Time) passResult {
	var anoms []risk.Anomaly
	fail := func(err error) passResult {
		return passResult{anoms: anoms, err: err}
	}

	// --- Step 1: positions, UNFILTERED -------------------------------------
	//
	// H-ORD-5 step 1: "including markets not in the selection set". A filtered
	// read is structurally incapable of answering the question, and the answer
	// it gives instead is "flat" for every market we hold but did not ask about.
	pos := s.src.Positions(ctx)
	anoms = append(anoms, pos.Anomalies...)
	if !pos.Replaces() {
		return fail(fmt.Errorf("the startup positions walk ended %s: "+
			"H-PAGE-1 makes an incomplete read stale and never empty, and this "+
			"one would otherwise seed q as flat everywhere (%v)",
			pos.Outcome, pos.Err))
	}

	// --- Step 2: resting orders, UNFILTERED --------------------------------
	orders := s.src.Orders(ctx, "", rest.StatusResting)
	anoms = append(anoms, orders.Anomalies...)
	if !orders.Replaces() {
		return fail(fmt.Errorf("the startup resting-orders walk ended "+
			"%s: an order we merely failed to read is still live and still "+
			"fillable (H-FAIL-3) (%v)", orders.Outcome, orders.Err))
	}

	// --- Step 3: fills back backfill_h -------------------------------------
	since := now.Add(-s.params.Backfill)
	fills := s.src.Fills(ctx, "", since)
	anoms = append(anoms, fills.Anomalies...)
	if !fills.Replaces() {
		return fail(fmt.Errorf("the startup fills walk ended %s: "+
			"H-PAGE-1a records that H-ORD-8's taker detector silently stops "+
			"working the moment our fills exceed one page, so it rides on the "+
			"complete walk and nothing else (%v)", fills.Outcome, fills.Err))
	}
	// The baseline is taken from the COMPLETE walk and from `AllTradeIDs`, so
	// it spans the whole account history the exchange returned and not the
	// `backfill_h` slice of it `fills.Fills` was cut down to. Recorded here,
	// before anything below can fail this pass: a trade this process has
	// already seen during STARTING is history whether or not this attempt goes
	// on to produce an adoption.
	for _, id := range fills.AllTradeIDs {
		s.baseline[id] = struct{}{}
	}

	// --- Step 4: balance, whose error is RETAINED (§3.2) --------------------
	bal, balErr := s.src.Balance(ctx)
	// Balance is integer cents; Money is 1e-6 USD.
	balance := num.Money(bal.Cents) * 10_000

	// --- Step 5: drain the unresolved set BEFORE anything is called foreign -
	resolveAnoms, err := s.resolveReservations(ctx, now)
	anoms = append(anoms, resolveAnoms...)
	if err != nil {
		return fail(err)
	}

	// --- Step 6: classify foreign activity, BEFORE converting anything -----
	fe, err := s.guard.Classify(PhaseStartup, orders.Orders, fills.Fills,
		now.UnixMilli())
	if err != nil {
		// An unclassifiable walk. NOT a completed reconciliation, and not a set
		// of foreign fills either: §7.5's licence to leave STARTING rests on
		// having attributed every fill on the account, and a ledger that cannot
		// answer has attributed none of them. This retries with backoff like
		// any other incomplete read.
		return fail(fmt.Errorf("startup could not classify the "+
			"account's fills against the ownership ledger: %w", err))
	}
	if len(fe.Unresolved) > 0 {
		// The ledger still holds reservations it has neither bound nor
		// abandoned. The resolve walk above has submitted what it could, but
		// those records are durable-ASYNCHRONOUS: a binding submitted this pass
		// is not visible to `OwnsOrders` until it commits, so the first pass
		// after a crash legitimately lands here and the next one does not.
		//
		// This is the SAME posture as an unavailable ledger above -- no
		// adoption, no latch, retry with backoff -- for a different reason: not
		// that the store is broken, but that it has outstanding work, which
		// after a SIGKILL between H-ORD-6's two commits is what a CORRECT ledger
		// looks like. Concluding anyway would mean either adopting fills that
		// might be a stranger's or declaring our own dispatched order foreign,
		// and the second one latches a durable global stop only an operator can
		// clear.
		return fail(fmt.Errorf("startup found %d fill(s) the "+
			"ownership ledger cannot yet attribute: it holds reservations that "+
			"have been neither bound nor abandoned, so those fills are neither "+
			"ours nor foreign and this walk has not reconciled",
			len(fe.Unresolved)))
	}
	anoms = append(anoms, fe.Anomalies...)

	// §3.3: the foreign-fill cause is durable BEFORE the balance error is
	// consulted and before any conversion is attempted. Both of those can fail,
	// and a stop that exists only in a local slice when they do is a stop that
	// never happened.
	latchFailed := false
	for _, c := range fe.Causes {
		causeAnoms, ok := s.commitCause(c)
		anoms = append(anoms, causeAnoms...)
		if !ok {
			latchFailed = true
		}
	}

	// §3.4: known owned-taker evidence, from the RAW `is_taker` flag, committed
	// before the fee conversion that can fail on it. H-ORD-8's two detectors
	// fail for different reasons; this is the one that needs no arithmetic, so
	// it must not be held hostage to the one that does.
	for _, f := range fe.OwnedFills {
		if !f.IsTaker {
			continue
		}
		causeAnoms, ok := s.commitCause(StopCause{
			Trigger:  "startup_fill_history",
			Market:   f.Ticker,
			TsMillis: now.UnixMilli(),
		})
		anoms = append(anoms, causeAnoms...)
		if !ok {
			latchFailed = true
		}
	}

	// The retained balance error is consulted HERE: classification is done and
	// every cause it found is on disk.
	if balErr != nil {
		return passResult{anoms: anoms, latchFailed: latchFailed,
			err: fmt.Errorf("the startup balance read failed, so "+
				"the capital model cannot be seeded and H-CAP-8's fundability "+
				"check would run against a zero: %w", balErr)}
	}

	ownedResting := orders.Ours()

	// --- Step 7: convert ONLY the fills that are ours ----------------------
	//
	// `rest` accepts an absent `fee_cost`, and a fill we are never going to
	// apply must not be able to fail the whole startup as a conversion error.
	ownedConverted, err := ConvertFills(fe.OwnedFills)
	if err != nil {
		return passResult{anoms: anoms, latchFailed: latchFailed,
			err: fmt.Errorf("a startup fill of ours did not convert: %w", err)}
	}

	portfolio := risk.NewSeededPortfolio(pos.ByTicker)
	fx := portfolio.ApplyFills(ownedConverted, s.guard.own, risk.Seed,
		now.UnixMilli())
	anoms = append(anoms, fx.Anomalies...)
	if fx.Stop {
		// A taker fill in our own history, or a side contradiction. H-ORD-8: it
		// is a fact about the harness whenever it happened. §3.5: committed the
		// moment reconciliation discovers it.
		causeAnoms, ok := s.commitCause(StopCause{
			Trigger:  "startup_fill_history",
			TsMillis: now.UnixMilli(),
		})
		anoms = append(anoms, causeAnoms...)
		if !ok {
			latchFailed = true
		}
	}

	// --- Step 8: the policy, with LIFECYCLE AUTHORITY AHEAD OF IT ----------
	//
	// §4. `Selected` handed to the policy is the EFFECTIVE selection -- the
	// operator's set minus what foreign activity excluded -- because a policy
	// given the raw set is being asked to validate against markets this startup
	// has already decided are not ours to newly quote. `M-L-EXCLUDESELECT`
	// passes the raw set.
	excluded := make(map[string]struct{}, len(fe.Exclude))
	for _, t := range fe.Exclude {
		excluded[t] = struct{}{}
	}
	effective := make([]string, 0, len(s.selected))
	for _, t := range s.selected {
		if _, bad := excluded[t]; !bad {
			effective = append(effective, t)
		}
	}
	addingPermitted := s.addingPermitted(latchFailed)

	facts := AdoptionFacts{
		Positions:       pos.ByTicker,
		Selected:        effective,
		Excluded:        fe.Exclude,
		AddingPermitted: addingPermitted,
		OwnedResting:    ownedResting,
		Balance:         balance,
	}
	var kept []rest.Order
	cancelByTicker := make(map[string][]rest.Order)
	for _, o := range ownedResting {
		d, err := s.policy.DecideAdopted(ctx, o, facts.clone())
		if err != nil {
			return passResult{anoms: anoms, latchFailed: latchFailed,
				err: fmt.Errorf("the adoption policy failed on order %s: "+
					"H-ORD-5c validates before leaving STARTING, so an "+
					"undecided order is not an order we may quote alongside: %w",
					o.OrderID, err)}
		}
		switch d {
		case AdoptionKeep:
			// The policy may keep an order the LIFECYCLE may not. Adding
			// authority is not the policy's to grant: a globally halted process
			// or an excluded ticker revokes it, and the policy is `lip-3af`'s
			// and cannot be assumed to know either fact. A reducer is
			// untouched -- including in an excluded market with inventory,
			// because exclusion bars NEW quoting and never abandons a position.
			// `M-L-STOPKEEP` keeps the revoked adding order anyway.
			_, tickerExcluded := excluded[o.Ticker]
			if (!addingPermitted || tickerExcluded) &&
				orderIsAdding(pos.ByTicker[o.Ticker], o.Side) {

				cancelByTicker[o.Ticker] = append(cancelByTicker[o.Ticker], o)
				anoms = append(anoms, risk.Anomaly{
					Class: "ADOPTION_KEEP_REVOKED", Sev: risk.SEV2,
					Ticker: o.Ticker,
					Text: fmt.Sprintf("the adoption policy kept adding order "+
						"%s, but adding authority is revoked here (adding "+
						"permitted %v, ticker excluded %v), so it is cancelled "+
						"and swept instead: keeping it would resume adding "+
						"under a halt or in a market an unexplained order made "+
						"untrustworthy", o.OrderID, addingPermitted,
						tickerExcluded),
				})
				continue
			}
			kept = append(kept, o)
		case AdoptionCancel:
			cancelByTicker[o.Ticker] = append(cancelByTicker[o.Ticker], o)
		default:
			return passResult{anoms: anoms, latchFailed: latchFailed,
				err: fmt.Errorf("the adoption policy returned %s for order %s; "+
					"there is no keep-by-default path, because a policy that "+
					"falls through is exactly the prior incarnation's order "+
					"persisting unexamined that H-ORD-5c forbids", d, o.OrderID)}
		}
	}

	// --- Step 9: cancel and VERIFY, grouped by ticker ----------------------
	cancelled := 0
	for _, t := range sortedKeys(cancelByTicker) {
		res := s.sweep.CancelAndSweep(ctx, t, cancelByTicker[t])
		anoms = append(anoms, res.Anomalies...)
		if !res.Clean {
			return passResult{anoms: anoms, latchFailed: latchFailed,
				err: fmt.Errorf("the startup cancel sweep for %s did not come "+
					"back clean (walk %s, %d still resting): H-FAIL-3 makes a "+
					"cancel-requested order live and fillable until the "+
					"exchange confirms it absent, so this adoption cannot "+
					"complete", t, res.Outcome, len(res.StillResting))}
		}
		cancelled += len(cancelByTicker[t])
	}

	// §3.6: the cancellation above ran because facts sufficed for it. The
	// Adoption does not, and never will on this pass -- a licence to leave
	// STARTING may not rest on a stop that is not on disk.
	if latchFailed {
		return passResult{anoms: anoms, cancelled: cancelled, latchFailed: true}
	}
	if cancelled > 0 {
		// The caller rewalks. Nothing below would be built from a current read.
		return passResult{anoms: anoms, cancelled: cancelled}
	}

	// --- Step 10: the complete result --------------------------------------
	//
	// This pass cancelled nothing, so `pos`, `orders` and `fills` all describe
	// the account as it is now.
	//
	// Every kept order is installed into the portfolio's resting-order model.
	// Without this, an adopted, exchange-fillable order exists in `Kept()` and
	// nowhere the risk model can see it -- `AnyLiveOrder` reads false, §5.1
	// declares the account DRAINED, and H-FAIL-3's "one ignored cancel away from
	// being long again" is exactly the position we are in. `foreign` is passed
	// empty on purpose: no foreign or cancelled order enters our model, and
	// `ForeignGuard` has already raised the anomaly for them.
	oe := portfolio.ReplaceOrders(keptAsLive(kept), nil, true)
	anoms = append(anoms, oe.Anomalies...)
	if !oe.Applied {
		return fail(errors.New("the adopted resting orders were not " +
			"installed into the portfolio"))
	}

	held := portfolio.Positions()
	// §4: Managed is built from the EFFECTIVE selection, the positions and the
	// final kept orders.
	managed := managedSet(effective, held, kept)

	states := make(map[string]quote.MarketState, len(managed))
	var reducing []string
	anyInventory := false
	for _, t := range managed {
		// §7.5 step 6: "Any market with q != 0 enters at REDUCING, not QUOTING,
		// regardless of size. Adopted inventory is inventory whose provenance we
		// do not know." Including markets outside the selection set: H-SEL-11
		// beats F15, and a market we hold is never abandoned for not being
		// selected.
		if held[t] != 0 {
			states[t] = quote.Reducing
			reducing = append(reducing, t)
		} else {
			states[t] = quote.Idle
		}
	}
	// AnyInventory is derived from the SEEDED positions rather than from
	// `managed`, because a market can hold inventory without being managed only
	// if the union above is wrong -- and this flag is the one that blocks a
	// false DRAINED, so it reads the source rather than a derived set.
	for _, q := range held {
		if q != 0 {
			anyInventory = true
			break
		}
	}

	ad := &adoption{
		portfolio: portfolio,
		balance:   balance,
		ownFills:  fe.OwnedFills,
		// FROZEN here, by copy. The accepted adoption carries the boundary its
		// own startup drew, so a consumer cannot be handed a set that a later
		// pass of a coordinator it does not know about went on to widen.
		baseline:     freezeBaseline(s.baseline),
		kept:         kept,
		foreign:      fe.ForeignOrders,
		managed:      managed,
		exclude:      fe.Exclude,
		states:       states,
		anomalies:    anoms,
		anyInventory: anyInventory,
		anyLiveOrder: len(kept) > 0,
		summary: StartupSummary{
			Positions:      held,
			Balance:        balance,
			AdoptedOrders:  len(kept),
			CancelledStale: 0,
			ForeignOrders:  len(fe.ForeignOrders),
			ForeignFills:   len(fe.ForeignFills),
			OwnedFills:     len(fe.OwnedFills),
			Managed:        managed,
			Excluded:       fe.Exclude,
			Reducing:       reducing,
		},
	}
	return passResult{ad: ad, anoms: anoms}
}

// freezeBaseline copies the accumulated identity set. See `Startup.baseline`.
func freezeBaseline(in map[string]struct{}) map[string]struct{} {
	out := make(map[string]struct{}, len(in))
	for id := range in {
		out[id] = struct{}{}
	}
	return out
}

// addingPermitted is §4's authority question, answered by the LIFECYCLE.
//
// It is a whitelist rather than a blacklist so that its zero-value behaviour and
// its behaviour for any state added later are both `false`. Adding is the one
// thing every uncertainty in this package prohibits; a new global state that
// silently inherited permission to add would invert that.
func (s *Startup) addingPermitted(latchFailed bool) bool {
	if latchFailed || s.ctrl.Latched() {
		return false
	}
	switch s.state {
	case quote.Starting, quote.Running:
		return true
	}
	return false
}

// orderIsAdding is §4's per-order half of the same question.
//
// At q == 0 there is no reducing side, so BOTH sides are adding and every
// resting order in the market is one. Otherwise the adding side is
// `quote.AddingSide`'s, and the order is adding exactly when it is on it.
func orderIsAdding(q num.Qty, side quote.Side) bool {
	a, ok := quote.AddingSide(q)
	if !ok {
		return true
	}
	return side == a
}

// resolveReservations is lip-eyq's drain on `ForeignEffects.Unresolved`, and it
// is what makes `adopt`'s refusal-to-conclude terminate.
//
// It walks `GET /portfolio/orders` UNFILTERED -- no status -- because
// `Order.ClientOrderID` is present for TERMINAL orders too. That is the whole
// mechanism: a coid the exchange ever accepted appears in this walk whatever
// became of the order, so the walk can distinguish "our order that has since
// closed" from "a coid the exchange never saw". A resting-only walk cannot, and
// would abandon every reservation whose order filled.
//
// It costs nothing on the ordinary path: with no outstanding reservations there
// is no walk at all.
func (s *Startup) resolveReservations(ctx context.Context,
	now time.Time) ([]risk.Anomaly, error) {

	outstanding := s.resolver.UnresolvedReservations()
	if len(outstanding) == 0 {
		// Nothing outstanding, so nothing can be stale either. Clearing the miss
		// counters here keeps `resolveConfirmAttempts` a count of CONSECUTIVE
		// misses rather than a lifetime tally.
		if len(s.missedCoid) > 0 {
			s.missedCoid = make(map[string]int)
		}
		return nil, nil
	}

	var anoms []risk.Anomaly
	walk := s.src.Orders(ctx, "", "")
	anoms = append(anoms, walk.Anomalies...)
	if !walk.Replaces() {
		// An INCOMPLETE walk proves nothing about any coid. Treating a missing
		// coid as absent here would abandon a live reservation on the strength
		// of a truncated page, which is H-PAGE-1's "stale, never empty" applied
		// to the one record that cannot be taken back.
		return anoms, fmt.Errorf("the startup reservation-resolving orders "+
			"walk ended %s: an incomplete listing cannot show that the "+
			"exchange never took a coid, and abandoning a reservation is "+
			"permanent (%v)", walk.Outcome, walk.Err)
	}

	// Bind every outstanding coid the exchange can still identify.
	listed := make(map[string]struct{}, len(walk.Orders))
	for _, o := range walk.Orders {
		if o.ClientOrderID == "" {
			continue
		}
		listed[o.ClientOrderID] = struct{}{}
		if _, wanted := outstanding[o.ClientOrderID]; !wanted {
			continue
		}
		// The counter is cleared on the SIGHTING, before the order id is
		// examined, and the ordering here is load bearing.
		//
		// The exchange listing this coid AT ALL is the proof that it took the
		// order, and that is the exact fact abandonment must never contradict.
		// Clearing only when an order id is also present makes a coid listed
		// with a blank one count as seen -- `listed` is already set, so no miss
		// is recorded -- while leaving its previous misses standing. The run of
		// "consecutive" misses is then not consecutive at all: two misses, a
		// sighting, one more miss, and the reservation is abandoned on evidence
		// the sighting refuted. `ResolveReservationAbandoned` is permanent and
		// the store refuses to bind afterwards, so that is an order of ours
		// declared un-dispatchable forever on a walk that saw it.
		delete(s.missedCoid, o.ClientOrderID)
		if o.OrderID == "" {
			continue
		}
		if err := s.resolver.BindListedOrder(o.ClientOrderID, o.OrderID,
			now.UnixMilli()); err != nil {

			anoms = append(anoms, risk.Anomaly{
				Class: "ORDER_BINDING_NOT_SUBMITTED", Sev: risk.SEV2,
				Ticker: o.Ticker,
				Text: fmt.Sprintf("startup recognised order %s as our "+
					"unresolved reservation %s but the binding could not be "+
					"submitted (%v); the reservation stays outstanding, so "+
					"fills on this order keep deferring rather than reading as "+
					"foreign", o.OrderID, o.ClientOrderID, err),
			})
		}
	}

	// Abandon what the exchange demonstrably never took -- but only after
	// `resolveConfirmAttempts` consecutive COMPLETE walks have failed to mention
	// it. See the constant for why one walk is not enough.
	for coid := range outstanding {
		if _, seen := listed[coid]; seen {
			continue
		}
		s.missedCoid[coid]++
		if s.missedCoid[coid] < resolveConfirmAttempts {
			continue
		}
		if err := s.resolver.AbandonListedReservation(coid,
			now.UnixMilli()); err != nil {

			anoms = append(anoms, risk.Anomaly{
				Class: "RESERVATION_NOT_ABANDONED", Sev: risk.SEV2,
				Text: fmt.Sprintf("reservation %s was absent from %d "+
					"consecutive complete order listings, so the exchange never "+
					"took it, but the abandonment could not be submitted (%v); "+
					"it stays outstanding and keeps deferring every "+
					"unrecognised fill", coid, s.missedCoid[coid], err),
			})
			continue
		}
		delete(s.missedCoid, coid)
		anoms = append(anoms, risk.Anomaly{
			Class: "RESERVATION_ABANDONED", Sev: risk.SEV2,
			Text: fmt.Sprintf("reservation %s was absent from %d consecutive "+
				"complete order listings, which list terminal orders too, so "+
				"the exchange never took it: H-ORD-6 commits the reservation "+
				"before dispatch and this is what a crash between the two "+
				"leaves behind; it is recorded abandoned so it stops deferring "+
				"every unrecognised fill", coid, resolveConfirmAttempts),
		})
	}
	return anoms, nil
}

// keptAsLive converts adopted orders into the risk package's terms.
func keptAsLive(kept []rest.Order) []risk.LiveOrder {
	out := make([]risk.LiveOrder, 0, len(kept))
	for _, o := range kept {
		out = append(out, risk.LiveOrder{
			OrderID:   o.OrderID,
			Ticker:    o.Ticker,
			Side:      o.Side,
			Price4:    o.Price4,
			Remaining: o.Remaining,
		})
	}
	return out
}

// managedSet is H-ORD-5b: "the union of every selected market, every market with
// a non-zero position, and every market with a resting, sending or unknown
// `lipH-` order, irrespective of LIP program status, selection rank, or
// core.Rig universe membership. The harness subscribes to whatever it holds."
//
// It is built from the KEPT orders, not from every owned resting order: an order
// H-ORD-5c cancelled and swept is confirmed absent from the exchange, and
// subscribing to a market on the strength of an order that no longer exists
// would manage a market for no reason.
//
// Note what is NOT subtracted: an excluded ticker. F15 excludes from new
// SELECTION -- which is why the `selected` argument is the EFFECTIVE set with
// exclusions already removed -- while H-SEL-11 keeps a market with inventory
// under management. A foreign-only ticker is excluded and unmanaged; a ticker
// with both a foreign order and inventory of ours is excluded AND managed AND
// reducing.
func managedSet(selected []string, held map[string]num.Qty,
	kept []rest.Order) []string {

	seen := make(map[string]struct{})
	add := func(t string) {
		if t == "" {
			return
		}
		seen[t] = struct{}{}
	}
	for _, t := range selected {
		add(t)
	}
	for t, q := range held {
		if q != 0 {
			add(t)
		}
	}
	for _, o := range kept {
		add(o.Ticker)
	}

	out := make([]string, 0, len(seen))
	for t := range seen {
		out = append(out, t)
	}
	sort.Strings(out)
	return out
}

func sortedKeys(m map[string][]rest.Order) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// ConvertFills is the edge conversion into `risk`'s units, and it runs only
// over fills the ownership ledger has already claimed as ours.
//
// A missing `fee_cost` is a hard error rather than a zero, for the reason `wsx`
// gives at the same boundary: maker fees are $0.00 (S2), so a non-zero fee is a
// taker fill whatever `is_taker` claims. The two detectors of H-ORD-8 fail for
// different reasons, and a fee we cannot read is one of the two witnesses going
// silent -- at startup, over up to 24 hours of history we did not observe.
//
// Exported because the owner records `Adoption.OwnedFills()` as `our_fill` rows
// with `backfilled = true`, and that is the same conversion this Step already
// made -- writing a second copy of it in `cmd/harness` would put the fee
// arithmetic that corroborates H-ORD-8 in two places that could drift.
func ConvertFills(fills []rest.Fill) ([]risk.FillEvent, error) {
	out := make([]risk.FillEvent, 0, len(fills))
	for _, f := range fills {
		if f.FeeCost == "" {
			return nil, fmt.Errorf("fill %s has no fee_cost; S2's independent "+
				"corroborator of H-ORD-8 cannot be evaluated without it",
				f.TradeID)
		}
		fee4, err := rest.ParsePrice4(f.FeeCost)
		if err != nil {
			return nil, fmt.Errorf("fill %s has fee_cost %q: %w",
				f.TradeID, f.FeeCost, err)
		}
		out = append(out, risk.FillEvent{
			TradeID:      f.TradeID,
			OrderID:      f.OrderID,
			Ticker:       f.Ticker,
			Side:         f.Side,
			Price4:       f.Price4,
			Count:        f.Count,
			Fee:          num.Money(fee4 * 100),
			IsTaker:      f.IsTaker,
			ExchangeTsMs: f.TsMillis,
		})
	}
	return out, nil
}

// confidence: high
