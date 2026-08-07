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

// AdoptionDecision is what §7.5 step 5 and H-ORD-5c decided about one adopted
// `lipH-` order.
//
// The zero value is INVALID. H-ORD-5c says adoption "is not a licence for a
// prior incarnation's orders to persist unexamined", and a zero value meaning
// "keep" would give exactly that licence to every policy that returned early,
// hit a branch it did not cover, or was replaced by a stub. There is no
// keep-by-default path in this package.
type AdoptionDecision uint8

const (
	AdoptionUnset AdoptionDecision = iota
	// AdoptionKeep adopts the order into the state machine at its stated price
	// and size (§7.5 step 5).
	AdoptionKeep
	// AdoptionCancel is H-ORD-5c: the order is invalid under current selection,
	// close, size, price or capital rules, so it is cancelled and swept.
	AdoptionCancel
)

func (d AdoptionDecision) String() string {
	switch d {
	case AdoptionKeep:
		return "keep"
	case AdoptionCancel:
		return "cancel"
	}
	return "unset"
}

// AdoptionFacts is everything the policy is allowed to see, in defensive copies.
//
// Copies rather than the live structures because the policy is `lip-3af`'s and
// this package cannot know what it does. A policy handed the real map could
// mutate the position we are in the middle of seeding, and the resulting `q`
// would be neither the exchange's answer nor a detectable corruption.
type AdoptionFacts struct {
	// Positions is q per market as the exchange just reported it.
	Positions map[string]num.Qty
	// Selected is the operator's selection set for this run.
	Selected []string
	// OwnedResting is every `lipH-` order on the account, including the one
	// being decided. H-ORD-5c's rules are aggregate -- capital, per-market size
	// -- so a policy that could only see one order at a time could not apply
	// them.
	OwnedResting []rest.Order
	Balance      num.Money
}

func (f AdoptionFacts) clone() AdoptionFacts {
	out := AdoptionFacts{
		Positions:    make(map[string]num.Qty, len(f.Positions)),
		Selected:     append([]string(nil), f.Selected...),
		OwnedResting: append([]rest.Order(nil), f.OwnedResting...),
		Balance:      f.Balance,
	}
	for t, q := range f.Positions {
		out.Positions[t] = q
	}
	return out
}

// AdoptionPolicy is H-ORD-5c's validation, and it lives in `lip-3af`.
//
// This package deliberately does not implement it. Deciding whether an adopted
// order is valid requires the selection rules, the close schedule, the size and
// price rules and the capital model -- four subsystems whose wiring is the unit
// after this one. An interim "keep everything" implementation here would be
// H-ORD-5c deleted, and it would pass every gate in this package.
type AdoptionPolicy interface {
	DecideAdopted(ctx context.Context, order rest.Order,
		facts AdoptionFacts) (AdoptionDecision, error)
}

// CancelSweeper is H-ORD-4's verified cancel. `*rest.Client` satisfies it.
type CancelSweeper interface {
	CancelAndSweep(ctx context.Context, ticker string,
		orders []rest.Order) rest.SweepResult
}

// StartupSummary is §7.5 step 7: "a `STARTUP` ping summarising adopted position
// and orders".
type StartupSummary struct {
	Positions      map[string]num.Qty
	Balance        num.Money
	AdoptedOrders  int
	CancelledStale int
	ForeignOrders  int
	ForeignFills   int
	OwnedFills     int
	Managed        []string
	Excluded       []string
	Reducing       []string
}

func (s StartupSummary) clone() StartupSummary {
	out := s
	out.Positions = make(map[string]num.Qty, len(s.Positions))
	for t, q := range s.Positions {
		out.Positions[t] = q
	}
	out.Managed = append([]string(nil), s.Managed...)
	out.Excluded = append([]string(nil), s.Excluded...)
	out.Reducing = append([]string(nil), s.Reducing...)
	return out
}

// Adoption is the COMPLETE result of one successful startup, and it is an
// INTERFACE with an unexported method rather than a struct.
//
// This object is the licence to leave STARTING. Obtaining one honestly costs
// four complete walks, an ownership classification, a policy pass over every
// adopted order, a verified clean sweep and a rewalk. Every field of the
// previous struct version was private and it was STILL forgeable, because Go
// permits `&lifecycle.Adoption{}` from any package as long as no unexported
// field is named -- and the zero value asserts a flat account with nothing
// resting and nothing foreign, which is the single most dangerous thing this
// type can claim.
//
// An interface with an unexported method cannot be implemented outside this
// package and has no non-nil zero value. `Attempt.Adoption` being nil is then
// the only thing "no licence" can look like.
type Adoption interface {
	// Portfolio is `q` seeded from the exchange (§7.5 step 1), with every kept
	// order installed as resting.
	Portfolio() *risk.Portfolio
	// Balance seeds the capital model (§7.5 step 4).
	Balance() num.Money
	// OwnedFills is the `backfill_h` history that belongs to us (§7.5 step 3).
	OwnedFills() []rest.Fill
	// Kept is every adopted `lipH-` order that survived H-ORD-5c.
	Kept() []rest.Order
	// Foreign is every resting order on the account that is not ours. Reported,
	// never cancelled.
	Foreign() []rest.Order
	// Managed is H-ORD-5b's union: selected, plus held, plus owned-resting.
	Managed() []string
	// Excluded names tickers barred from NEW selection (F15). A ticker can be
	// both excluded and managed; H-SEL-11 wins over F15 where they collide.
	Excluded() []string
	// States is the initial per-market state. Every market with q != 0 is
	// REDUCING (§7.5 step 6).
	States() map[string]quote.MarketState
	// Summary is §7.5 step 7's `STARTUP` ping content. Delivery is `lip-6w5`.
	Summary() StartupSummary
	// Causes is every durable global stop this adoption discovered. They have
	// ALREADY been committed before this object existed; they are exposed for
	// the record, not as work for the caller to remember.
	Causes() []StopCause
	// Anomalies is everything the operator is told about this startup.
	Anomalies() []risk.Anomaly

	// isAdoption cannot be implemented outside this package. It is the whole
	// seal.
	isAdoption()
}

// adoption is the only implementation.
type adoption struct {
	portfolio *risk.Portfolio
	balance   num.Money
	ownFills  []rest.Fill
	kept      []rest.Order
	foreign   []rest.Order
	managed   []string
	exclude   []string
	states    map[string]quote.MarketState
	summary   StartupSummary
	causes    []StopCause
	anomalies []risk.Anomaly
}

func (a *adoption) isAdoption() {}

func (a *adoption) Portfolio() *risk.Portfolio { return a.portfolio }
func (a *adoption) Balance() num.Money         { return a.balance }

func (a *adoption) OwnedFills() []rest.Fill {
	return append([]rest.Fill(nil), a.ownFills...)
}

func (a *adoption) Kept() []rest.Order { return append([]rest.Order(nil), a.kept...) }

func (a *adoption) Foreign() []rest.Order {
	return append([]rest.Order(nil), a.foreign...)
}

func (a *adoption) Managed() []string  { return append([]string(nil), a.managed...) }
func (a *adoption) Excluded() []string { return append([]string(nil), a.exclude...) }

func (a *adoption) States() map[string]quote.MarketState {
	out := make(map[string]quote.MarketState, len(a.states))
	for t, s := range a.states {
		out[t] = s
	}
	return out
}

// Summary deep-copies. The previous version returned the struct by value, which
// copies the ints and ALIASES every map and slice inside it -- so a reporting
// consumer holding a summary could rewrite the managed set of the adoption that
// produced it.
func (a *adoption) Summary() StartupSummary { return a.summary.clone() }

func (a *adoption) Causes() []StopCause { return append([]StopCause(nil), a.causes...) }

func (a *adoption) Anomalies() []risk.Anomaly {
	return append([]risk.Anomaly(nil), a.anomalies...)
}

// Startup runs §7.5. It is constructible only from a bootstrapped
// GlobalController, which is what makes the latch read precede every REST call.
type Startup struct {
	ctrl   *GlobalController
	src    PortfolioSource
	guard  *ForeignGuard
	policy AdoptionPolicy
	sweep  CancelSweeper
	params cfg.Params

	// selected is the operator's selection set. It may be empty; H-ORD-5b's
	// union does not require it to be non-empty, and an account being adopted
	// with no selection is exactly the WINDING_DOWN restart case.
	selected []string

	// consecutiveFailures counts attempts since the last complete one.
	consecutiveFailures int
}

// NewStartup requires every collaborator. A nil one fails construction.
//
// The controller argument is not decoration: taking a *GlobalController that can
// only be produced by NewGlobalController -- which performs the latch read
// itself -- means there is no ordering to remember. `TestLatchReadPrecedesEveryPortfolioRequest`
// asserts the consequence, and `M-L-BOOTORDER` breaks it on purpose.
func NewStartup(ctrl *GlobalController, src PortfolioSource, guard *ForeignGuard,
	policy AdoptionPolicy, sweep CancelSweeper, params cfg.Params,
	selected []string) (*Startup, error) {

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
	if err := params.Validate(); err != nil {
		return nil, err
	}
	return &Startup{
		ctrl: ctrl, src: src, guard: guard, policy: policy, sweep: sweep,
		params: params, selected: append([]string(nil), selected...),
	}, nil
}

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

// Run performs one attempt and adjudicates its outcome through the controller.
//
// `in` is the CURRENT global input. It is passed through to `quote.NextGlobal`
// by way of `GlobalController.Decide`, so a latch already on disk produces
// WINDING_DOWN/GTLatch (A14) whatever this procedure would otherwise have
// concluded -- including on a failing attempt, which is why a latched startup
// failure cannot regress to STARTING or UNKNOWN_RISK.
func (s *Startup) Run(ctx context.Context, in quote.GlobalInput,
	now time.Time) Attempt {

	ad, anoms, err := s.walk(ctx, now)
	if err != nil {
		return s.fail(in, anoms, err)
	}

	// Every cause this startup discovered is committed HERE, before the licence
	// is handed out. An earlier version returned the causes on the Adoption for
	// a caller to latch later; the foreign-fill test then proved only that a
	// diligent caller COULD do the right thing, which is not a property of the
	// code. A foreign fill found during reconciliation must not be returnable as
	// a to-do item.
	dec := s.commit(in, ad.causes)
	if !dec.Committed {
		// A cause was found and could not be made durable. No licence: leaving
		// STARTING on the strength of a stop we failed to record is the HR-009
		// sequence with the harness having been told the write failed.
		s.consecutiveFailures++
		return Attempt{
			Err: errors.New("a startup stop cause could not be made durable, " +
				"so this reconciliation does not license leaving STARTING"),
			Decision:  dec,
			Retry:     true,
			After:     startupBackoffAt(s.consecutiveFailures),
			Anomalies: append(anoms, dec.Anomalies...),
		}
	}

	s.consecutiveFailures = 0
	ad.anomalies = append(ad.anomalies, dec.Anomalies...)
	return Attempt{
		Adoption:  ad,
		Decision:  dec,
		Anomalies: append(anoms, dec.Anomalies...),
	}
}

// commit makes every discovered cause durable, then advances the machine ONCE.
//
// The one-Advance-per-step rule (lip-eyq §3.8) is not tidiness. The previous
// shape called Decide once per cause and fed each returned state back in as the
// next call's input, so N causes produced N transitions and the intermediate
// ones were publishable. A second cause arriving in the same walk therefore had
// to be adjudicated against a state the first cause had already moved -- and if
// the second failed to commit, the caller had already been handed a transition
// justified by a latch set that was never completed.
//
// Committing every cause first collapses that: the durable record is the whole
// set, and exactly one transition is derived from it. With no causes at all the
// Advance still happens -- with a complete reconciliation, which is what takes
// STARTING to RUNNING through §5.1 rather than by assignment.
func (s *Startup) commit(in quote.GlobalInput, causes []StopCause) GlobalDecision {
	in.TruthReadable = true
	in.Reconciled = true

	var anoms []risk.Anomaly
	for _, c := range causes {
		sc := s.ctrl.CommitStop(c)
		anoms = append(anoms, sc.Anomalies...)
		if !sc.Durable {
			// I1: the cause is not on disk, so no transition is publishable and
			// no Adoption may be returned. Cancelling, reducing, reconciling and
			// monitoring all continue; the caller retries the write.
			return GlobalDecision{
				State: in.State, Trigger: quote.GTNone,
				Committed: false, BlockAdding: true, RetryLatch: true,
				Anomalies: anoms,
			}
		}
	}

	dec := s.ctrl.Advance(in)
	dec.Anomalies = append(anoms, dec.Anomalies...)
	return dec
}

// fail applies §7.5's retry semantics, and reaches UNKNOWN_RISK through §5.1
// rather than by assigning it.
//
// The route is `GlobalInput.TruthReadable`. §5.1's STARTING rule is "if
// !TruthReadable -> UNKNOWN_RISK/GTTruthFailed", so the third consecutive
// failure sets that input to false and the state machine produces the state.
// Before the threshold, truth is reported readable and STARTING holds itself
// (the input is not `Reconciled`, so §5.1 returns the same state).
func (s *Startup) fail(in quote.GlobalInput, anoms []risk.Anomaly,
	err error) Attempt {

	s.consecutiveFailures++

	// H-ORD-5a. NOT WindingDown: winding down exists to keep a REDUCING quote
	// alive, and sizing a reducer needs a q we just failed to read. UNKNOWN_RISK's
	// only job is to keep trying to find out, forever, while placing nothing.
	in.TruthReadable = s.consecutiveFailures < startupFailureThreshold
	in.Reconciled = false

	at := Attempt{
		Err:       err,
		Decision:  s.ctrl.Advance(in),
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

// walk runs §7.5 steps 1-7, rewalking after any pass that cancelled.
func (s *Startup) walk(ctx context.Context, now time.Time) (*adoption,
	[]risk.Anomaly, error) {

	var anoms []risk.Anomaly
	totalCancelled := 0
	for round := 0; round <= maxSweepRewalks; round++ {
		ad, passAnoms, cancelled, err := s.attempt(ctx, now)
		anoms = append(anoms, passAnoms...)
		if err != nil {
			return nil, anoms, err
		}
		if cancelled == 0 {
			// The summary reports what this STARTUP cancelled, across every
			// pass, not what its final pass did -- which is always zero.
			ad.summary.CancelledStale = totalCancelled
			ad.anomalies = anoms
			return ad, anoms, nil
		}
		totalCancelled += cancelled
		// This pass cancelled orders and its position read predates them. An
		// order that filled while it was being cancelled would leave `q` stale,
		// and `q` is what sizes every reducer from here on -- so the result is
		// discarded and all four reads run again. The failure counter is NOT
		// touched: this is a successful pass doing more work, not a failure.
		anoms = append(anoms, risk.Anomaly{
			Class: "STARTUP_REWALK", Sev: risk.SEV2,
			Text: fmt.Sprintf("%d invalid adopted order(s) were cancelled and "+
				"swept clean, so the four startup reads are being run again: "+
				"the position read that preceded the cancellation cannot "+
				"license quoting, because an order that filled while it was "+
				"being cancelled leaves q stale", cancelled),
		})
	}
	return nil, anoms, fmt.Errorf("startup still had orders to cancel after %d "+
		"clean sweeps; a verified sweep confirms the exchange no longer shows "+
		"them resting, so an order that keeps reappearing means the reads "+
		"contradict the sweeps", maxSweepRewalks)
}

// attempt is one pass through §7.5 steps 1-7.
//
// The steps run in the order §7.5 fixes, and the order is load bearing in three
// places:
//
//   - positions BEFORE fills, because the exchange has already told us where the
//     position ended up. `risk.Seed` then records the historical fills' identities
//     and their taker classification without replaying them onto that answer.
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
func (s *Startup) attempt(ctx context.Context, now time.Time) (*adoption,
	[]risk.Anomaly, int, error) {

	var anoms []risk.Anomaly

	// --- Step 1: positions, UNFILTERED -------------------------------------
	//
	// H-ORD-5 step 1: "including markets not in the selection set". A filtered
	// read is structurally incapable of answering the question, and the answer
	// it gives instead is "flat" for every market we hold but did not ask about.
	pos := s.src.Positions(ctx)
	anoms = append(anoms, pos.Anomalies...)
	if !pos.Replaces() {
		return nil, anoms, 0, fmt.Errorf("the startup positions walk ended %s: "+
			"H-PAGE-1 makes an incomplete read stale and never empty, and this "+
			"one would otherwise seed q as flat everywhere (%v)",
			pos.Outcome, pos.Err)
	}

	// --- Step 2: resting orders, UNFILTERED --------------------------------
	orders := s.src.Orders(ctx, "", rest.StatusResting)
	anoms = append(anoms, orders.Anomalies...)
	if !orders.Replaces() {
		return nil, anoms, 0, fmt.Errorf("the startup resting-orders walk ended "+
			"%s: an order we merely failed to read is still live and still "+
			"fillable (H-FAIL-3) (%v)", orders.Outcome, orders.Err)
	}

	// --- Step 3: fills back backfill_h -------------------------------------
	since := now.Add(-s.params.Backfill)
	fills := s.src.Fills(ctx, "", since)
	anoms = append(anoms, fills.Anomalies...)
	if !fills.Replaces() {
		return nil, anoms, 0, fmt.Errorf("the startup fills walk ended %s: "+
			"H-PAGE-1a records that H-ORD-8's taker detector silently stops "+
			"working the moment our fills exceed one page, so it rides on the "+
			"complete walk and nothing else (%v)", fills.Outcome, fills.Err)
	}

	// --- Step 4: balance ---------------------------------------------------
	bal, err := s.src.Balance(ctx)
	if err != nil {
		return nil, anoms, 0, fmt.Errorf("the startup balance read failed, so "+
			"the capital model cannot be seeded and H-CAP-8's fundability check "+
			"would run against a zero: %w", err)
	}
	// Balance is integer cents; Money is 1e-6 USD.
	balance := num.Money(bal.Cents) * 10_000

	// --- Step 5: classify foreign activity, BEFORE converting anything -----
	fe, err := s.guard.Classify(PhaseStartup, orders.Orders, fills.Fills,
		now.UnixMilli())
	if err != nil {
		// An unclassifiable walk. NOT a completed reconciliation, and not a set
		// of foreign fills either: §7.5's licence to leave STARTING rests on
		// having attributed every fill on the account, and a ledger that cannot
		// answer has attributed none of them. This retries with backoff like
		// any other incomplete read.
		return nil, anoms, 0, fmt.Errorf("startup could not classify the "+
			"account's fills against the ownership ledger: %w", err)
	}
	if len(fe.Unresolved) > 0 {
		// The ledger holds reservations it has neither bound nor abandoned, and
		// fills on the account that could belong to any of them. This is the
		// SAME posture as an unavailable ledger above -- no adoption, no latch,
		// retry with backoff -- for a different reason: not that the store is
		// broken, but that it has outstanding work, which after a SIGKILL
		// between H-ORD-6's two commits is what a CORRECT ledger looks like.
		//
		// §7.5's licence to leave STARTING rests on having attributed every fill
		// on the account, and these are not attributed. Concluding anyway would
		// mean either adopting fills that might be a stranger's or declaring our
		// own dispatched order foreign, and the second one latches a durable
		// global stop that only an operator can clear. `lip-eyq` owns the walk
		// that drains this set -- rebind by coid over the unfiltered orders
		// listing, abandon what the exchange never saw -- and until it runs this
		// retries, which is H-ORD-5a's "keep trying".
		return nil, anoms, 0, fmt.Errorf("startup found %d fill(s) the "+
			"ownership ledger cannot yet attribute: it holds reservations that "+
			"have been neither bound nor abandoned, so those fills are neither "+
			"ours nor foreign and this walk has not reconciled",
			len(fe.Unresolved))
	}
	anoms = append(anoms, fe.Anomalies...)
	causes := append([]StopCause(nil), fe.Causes...)
	ownedResting := orders.Ours()

	// --- Step 6: convert ONLY the fills that are ours ----------------------
	//
	// `rest` accepts an absent `fee_cost`, and a fill we are never going to
	// apply must not be able to fail the whole startup as a conversion error.
	ownedConverted, err := convertStartupFills(fe.OwnedFills)
	if err != nil {
		return nil, anoms, 0, fmt.Errorf("a startup fill of ours did not "+
			"convert: %w", err)
	}

	portfolio := risk.NewSeededPortfolio(pos.ByTicker)
	fx := portfolio.ApplyFills(ownedConverted, s.guard.own, risk.Seed,
		now.UnixMilli())
	anoms = append(anoms, fx.Anomalies...)
	if fx.Stop {
		// A taker fill in our own history, or a side contradiction. H-ORD-8: it
		// is a fact about the harness whenever it happened.
		causes = append(causes, StopCause{
			Trigger:  "startup_fill_history",
			TsMillis: now.UnixMilli(),
		})
	}

	// --- Step 7: run the policy over every OWNED resting order -------------
	//
	// Foreign orders are never passed to the policy. The policy's job is to
	// decide whether to keep or cancel, and there is no reading of §7.5 under
	// which "cancel" is an available answer for an order that is not ours.
	facts := AdoptionFacts{
		Positions:    pos.ByTicker,
		Selected:     s.selected,
		OwnedResting: ownedResting,
		Balance:      balance,
	}
	var kept []rest.Order
	cancelByTicker := make(map[string][]rest.Order)
	for _, o := range ownedResting {
		d, err := s.policy.DecideAdopted(ctx, o, facts.clone())
		if err != nil {
			return nil, anoms, 0, fmt.Errorf("the adoption policy failed on "+
				"order %s: H-ORD-5c validates before leaving STARTING, so an "+
				"undecided order is not an order we may quote alongside: %w",
				o.OrderID, err)
		}
		switch d {
		case AdoptionKeep:
			kept = append(kept, o)
		case AdoptionCancel:
			cancelByTicker[o.Ticker] = append(cancelByTicker[o.Ticker], o)
		default:
			return nil, anoms, 0, fmt.Errorf("the adoption policy returned %s "+
				"for order %s; there is no keep-by-default path, because a "+
				"policy that falls through is exactly the prior incarnation's "+
				"order persisting unexamined that H-ORD-5c forbids", d, o.OrderID)
		}
	}

	// --- Step 8: cancel and VERIFY, grouped by ticker ----------------------
	cancelled := 0
	for _, t := range sortedKeys(cancelByTicker) {
		res := s.sweep.CancelAndSweep(ctx, t, cancelByTicker[t])
		anoms = append(anoms, res.Anomalies...)
		if !res.Clean {
			return nil, anoms, 0, fmt.Errorf("the startup cancel sweep for %s "+
				"did not come back clean (walk %s, %d still resting): H-FAIL-3 "+
				"makes a cancel-requested order live and fillable until the "+
				"exchange confirms it absent, so this adoption cannot complete",
				t, res.Outcome, len(res.StillResting))
		}
		cancelled += len(cancelByTicker[t])
	}
	if cancelled > 0 {
		// The caller rewalks. Nothing below would be built from a current read.
		return nil, anoms, cancelled, nil
	}

	// --- Step 9: the complete result ---------------------------------------
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
		return nil, anoms, 0, errors.New("the adopted resting orders were not " +
			"installed into the portfolio")
	}

	held := portfolio.Positions()
	managed := managedSet(s.selected, held, kept)

	states := make(map[string]quote.MarketState, len(managed))
	var reducing []string
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

	ad := &adoption{
		portfolio: portfolio,
		balance:   balance,
		ownFills:  fe.OwnedFills,
		kept:      kept,
		foreign:   fe.ForeignOrders,
		managed:   managed,
		exclude:   fe.Exclude,
		states:    states,
		causes:    causes,
		anomalies: anoms,
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
	return ad, anoms, 0, nil
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
// SELECTION; H-SEL-11 keeps a market with inventory under management. A
// foreign-only ticker is excluded and unmanaged; a ticker with both a foreign
// order and inventory of ours is excluded AND managed AND reducing.
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

// convertStartupFills is the edge conversion into `risk`'s units, and it runs
// only over fills the ownership ledger has already claimed as ours.
//
// A missing `fee_cost` is a hard error rather than a zero, for the reason `wsx`
// gives at the same boundary: maker fees are $0.00 (S2), so a non-zero fee is a
// taker fill whatever `is_taker` claims. The two detectors of H-ORD-8 fail for
// different reasons, and a fee we cannot read is one of the two witnesses going
// silent -- at startup, over up to 24 hours of history we did not observe.
func convertStartupFills(fills []rest.Fill) ([]risk.FillEvent, error) {
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
