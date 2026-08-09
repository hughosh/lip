package lifecycle

import (
	"context"

	"lip/harness/num"
	"lip/harness/quote"
	"lip/harness/rest"
	"lip/harness/risk"
)

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
	//
	// It is a RECOMMENDATION, not an instruction. The lifecycle layer may
	// override it into a cancellation when adding authority has been revoked --
	// see `AddingPermitted` below and lip-eyq §4.
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
	// Selected is the EFFECTIVE selection set for this run: the operator's set
	// with every foreign-excluded ticker already removed.
	//
	// It is the effective set rather than the raw one because a policy handed
	// the raw set is being asked to validate an order against markets startup
	// has already decided are not ours to newly quote -- and the policy is
	// `lip-3af`'s, so this package cannot assume it will subtract `Excluded`
	// itself. `M-L-EXCLUDESELECT` passes the raw set.
	Selected []string
	// Excluded names the tickers barred from NEW selection by foreign activity
	// (F15). It is supplied ALONGSIDE the already-filtered `Selected` because
	// exclusion and selection answer different questions: a market can be
	// excluded and still hold inventory that must be reduced (H-SEL-11), and a
	// policy sizing a reducer needs to know which case it is in.
	Excluded []string
	// AddingPermitted is whether this process may rest an ADDING order at all.
	//
	// It is the lifecycle's answer, not the policy's, and it is false for a
	// loaded or newly committed latch, for a failed latch persistence, and for
	// any halted coordinator state. Its zero value is false, so a policy
	// constructed against a zero-valued facts struct is told it may not add --
	// which is the conservative reading of every uncertainty in this package.
	AddingPermitted bool
	// OwnedResting is every `lipH-` order on the account, including the one
	// being decided. H-ORD-5c's rules are aggregate -- capital, per-market size
	// -- so a policy that could only see one order at a time could not apply
	// them.
	OwnedResting []rest.Order
	Balance      num.Money
}

func (f AdoptionFacts) clone() AdoptionFacts {
	out := AdoptionFacts{
		Positions:       make(map[string]num.Qty, len(f.Positions)),
		Selected:        append([]string(nil), f.Selected...),
		Excluded:        append([]string(nil), f.Excluded...),
		AddingPermitted: f.AddingPermitted,
		OwnedResting:    append([]rest.Order(nil), f.OwnedResting...),
		Balance:         f.Balance,
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
//
// What this package does NOT delegate is lifecycle authority. A policy that
// returns `AdoptionKeep` for an adding order while the process is halted or the
// ticker is excluded is overridden into a verified cancellation (lip-eyq §4),
// because whether we may add at all is not a question about the order.
type AdoptionPolicy interface {
	DecideAdopted(ctx context.Context, order rest.Order,
		facts AdoptionFacts) (AdoptionDecision, error)
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
	// StartupTrades is the trade id of every fill this startup saw on the
	// account, across every complete fills walk it made and BEFORE `backfill_h`
	// filtered any of them out. Identity only.
	//
	// It is the boundary between the history this process INHERITED and the
	// activity it CAUSED, and it is what a rung policy that reacts to a first
	// live fill must consult before reacting. It is deliberately wider than
	// `OwnedFills`: that is the `backfill_h` slice of the fills that are ours,
	// while this is every trade the walk carried -- ours, foreign, and older
	// than the window alike. A trade outside `backfill_h` never reaches the
	// position model at startup, so `seenTrade` does not know it, and the first
	// live poll -- which asks for all history with a zero `since` -- would
	// otherwise present it as brand new on every single restart.
	StartupTrades() map[string]struct{}
	// Kept is every adopted `lipH-` order that survived H-ORD-5c.
	Kept() []rest.Order
	// Foreign is every resting order on the account that is not ours. Reported,
	// never cancelled.
	Foreign() []rest.Order
	// Managed is H-ORD-5b's union: effective selection, plus held, plus
	// owned-resting.
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
	// the record, not as work for the caller to remember. It spans every pass
	// this Step made, including rewalks whose adoption was discarded.
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
	baseline  map[string]struct{}
	kept      []rest.Order
	foreign   []rest.Order
	managed   []string
	exclude   []string
	states    map[string]quote.MarketState
	summary   StartupSummary
	causes    []StopCause
	anomalies []risk.Anomaly

	// anyInventory and anyLiveOrder are §5.1's two risk flags, derived HERE from
	// what this pass actually read: the seeded positions and the final kept
	// orders. They are unexported and have no accessor, because they are the
	// coordinator's evidence for its own Advance and not a fact any caller is
	// entitled to supply. `M-L-FALSEFLAT` is what happens when the drain stops
	// requiring them to be answers.
	anyInventory bool
	anyLiveOrder bool
}

func (a *adoption) isAdoption() {}

func (a *adoption) Portfolio() *risk.Portfolio { return a.portfolio }
func (a *adoption) Balance() num.Money         { return a.balance }

func (a *adoption) OwnedFills() []rest.Fill {
	return append([]rest.Fill(nil), a.ownFills...)
}

// StartupTrades copies, like every other accessor here and for the same
// reason: a caller holding the boundary must not be able to move it.
func (a *adoption) StartupTrades() map[string]struct{} {
	return freezeBaseline(a.baseline)
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

// confidence: high
