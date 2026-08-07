package lifecycle

import (
	"errors"
	"fmt"
	"sort"

	"lip/harness/rest"
	"lip/harness/risk"
)

// Phase is whether this process is still adopting a pre-existing account or is
// in steady state. It is the whole of H-ORD-9's distinction, and the same
// foreign order means opposite things on either side of it.
//
// The zero value is INVALID and is treated as `live`. A three-valued phase whose
// zero value was `startup` would make the permissive reading the default for
// every caller that forgot to set it, and the permissive reading is the one that
// keeps quoting while a third party trades the account.
type Phase uint8

const (
	// PhaseUnset is the zero value. Classified as live, loudly.
	PhaseUnset Phase = iota
	// PhaseStartup is §7.5 step 5: what we find on the account is a
	// pre-existing condition we are adopting, not something that happened to us.
	PhaseStartup
	// PhaseLive is steady state. Anything foreign appearing now is a third party
	// trading the account our position model describes.
	PhaseLive
)

func (p Phase) String() string {
	switch p {
	case PhaseStartup:
		return "startup"
	case PhaseLive:
		return "live"
	}
	return "unset"
}

// ForeignEffects is one classification pass over an orders walk and a fills
// walk.
type ForeignEffects struct {
	// OwnedFills is every fill whose order id is in the durable ledger. It is
	// what may enter `our_fill`; a foreign fill never does (H-ORD-9).
	OwnedFills []rest.Fill
	// ForeignFills is reported so the operator can see what the third party did.
	ForeignFills []rest.Fill
	// ForeignOrders is reported and, at startup, excluded. It is NEVER cancelled
	// -- see the doc on Classify.
	ForeignOrders []rest.Order
	// Exclude names the tickers barred from NEW SELECTION. H-SEL-11 wins over
	// F15 on the other half: a market with q != 0 stays under management in
	// REDUCING. Exclusion is about what we newly quote, not about abandoning
	// inventory.
	Exclude []string
	// Causes are the durable global stops this pass requires. They go through
	// GlobalController.Decide; this type never transitions anything.
	Causes    []StopCause
	Anomalies []risk.Anomaly
}

// Stop reports whether this pass requires a durable global stop.
func (e ForeignEffects) Stop() bool { return len(e.Causes) > 0 }

// ForeignGuard is H-ORD-9's classification, and its constructor is where the
// "assertion, not a model" argument is enforced.
type ForeignGuard struct {
	own risk.OwnershipLookup
}

// NewForeignGuard requires a real ownership ledger, and requires it even on an
// account that currently has no fills.
//
// H-ORD-9: "Fills are classified by `order_id` against that ledger -- never by
// heuristic, never by `run_id`." An optional lookup is a heuristic with a nil
// check in front of it, and the heuristic it degrades to is "no fills are
// foreign", which is the exact hole H-ORD-9 was written to close: a manual taker
// fill either falsely trips F14 or silently leaves a gap, and nothing in the
// data distinguishes the two.
//
// Requiring it when the walk is empty matters more than requiring it when the
// walk is full. An empty walk is the state every test fixture and every first
// run is in, so a nil-tolerant constructor would be exercised constantly and
// would fail only on the day it mattered.
func NewForeignGuard(own risk.OwnershipLookup) (*ForeignGuard, error) {
	if own == nil {
		return nil, errors.New("no ownership lookup: H-ORD-9 classifies fills " +
			"against a durable ledger and has no in-memory fallback, and a nil " +
			"lookup degrades to \"nothing is foreign\", which is the hole the " +
			"rule exists to close")
	}
	return &ForeignGuard{own: own}, nil
}

// Classify splits an orders walk and a fills walk into ours and not-ours, and
// says what each side means in this phase.
//
// # Orders are classified by coid; fills are classified by the ledger
//
// That split is the spec's, not a convenience. §7.5 step 5 classifies resting
// orders by whether the coid matches `lipH-*`, because a resting order is
// something we can look at. H-ORD-9 classifies fills by `order_id` against the
// durable ledger "never by heuristic, never by `run_id`", because a fill is
// history and the order that produced it may be long gone -- including orders
// from a previous incarnation, which a run-scoped coid check would call foreign.
//
// # Foreign orders are never cancelled here, in either phase
//
// F15's response is "exclude that market from selection; do **not** cancel it",
// and §11's commentary is explicit that "cancelling someone else's -- or a
// previous incarnation's -- orders is a destructive act taken on incomplete
// information". Note that the LIVE case latches a global stop and still does not
// cancel. Those are not in tension: stopping our own additions is an action on
// our risk, and cancelling an order we did not place is an action on someone
// else's.
//
// # A foreign fill is global, in both phases
//
// H-ORD-9 gives foreign fills no startup exemption, and the reason is that a
// fill is not a pre-existing condition we can adopt the way a resting order is.
// A resting order sitting on the account tells us someone placed it; a fill tells
// us someone is TRADING the account whose position our model describes, and that
// model "is now unreliable everywhere, not in one market".
//
// # An unavailable ledger is an ERROR, not a classification
//
// The durable ledger has three answers -- ours, not ours, and "I cannot say" --
// and the third one used to have nowhere to go. Returning it as "not ours"
// declares a third party is trading the account and latches a global stop, on
// the strength of a database that would not open; returning it as "ours" adopts
// a stranger's position. So it is neither: this returns an error, the pass
// produces NO effects, and §7.5 treats it as an unclassifiable walk that has not
// reconciled. `M-HS-LOOKUPFAIL` takes the other road.
func (g *ForeignGuard) Classify(phase Phase, orders []rest.Order,
	fills []rest.Fill, tsMillis int64) (ForeignEffects, error) {

	// The ledger is consulted FIRST, for the whole walk, before any effect
	// exists. A failure half way through would leave the caller holding a
	// partial classification that looks like a complete one.
	orderIDs := make([]string, len(fills))
	for i, f := range fills {
		orderIDs[i] = f.OrderID
	}
	owned, err := g.own.OwnsOrders(orderIDs)
	if err != nil {
		return ForeignEffects{}, fmt.Errorf("the ownership ledger could not "+
			"classify this walk's %d fill(s), so it is neither a set of "+
			"foreign fills nor a completed reconciliation; H-ORD-9 classifies "+
			"against the durable ledger and has no fallback: %w",
			len(fills), err)
	}
	if len(owned) != len(fills) {
		return ForeignEffects{}, fmt.Errorf("the ownership ledger answered "+
			"about %d order(s) when asked about %d; a positional answer of the "+
			"wrong length attributes one fill's ownership to another",
			len(owned), len(fills))
	}

	var eff ForeignEffects
	excluded := make(map[string]struct{})

	for _, o := range orders {
		if o.Ours {
			continue
		}
		eff.ForeignOrders = append(eff.ForeignOrders, o)

		if phase == PhaseStartup {
			// Pre-existing. SEV2, excluded from new selection, not cancelled,
			// and NOT a global stop: an account we are adopting is allowed to
			// have had things on it before we arrived.
			if _, dup := excluded[o.Ticker]; !dup {
				excluded[o.Ticker] = struct{}{}
			}
			eff.Anomalies = append(eff.Anomalies, risk.Anomaly{
				Class: "FOREIGN_ORDER", Sev: risk.SEV2, Ticker: o.Ticker,
				Text: fmt.Sprintf("resting order %s carries no coid of ours and "+
					"was already on the account at startup; it is not cancelled, "+
					"and %s is excluded from new selection because an account "+
					"with unexplained orders on it is an account whose position "+
					"model we cannot trust", o.OrderID, o.Ticker),
			})
			continue
		}

		// Live, or an unset phase treated as live. This appeared while we were
		// running: a third party is trading the account.
		eff.Anomalies = append(eff.Anomalies, risk.Anomaly{
			Class: "FOREIGN_ORDER", Sev: risk.SEV1, Ticker: o.Ticker,
			Text: fmt.Sprintf("resting order %s carries no coid of ours and "+
				"appeared after startup (phase %s), so it is not a pre-existing "+
				"condition we adopted but a live third party trading the account "+
				"we are modelling; it is not cancelled, and the harness stops "+
				"adding globally", o.OrderID, phase),
		})
		eff.Causes = append(eff.Causes, StopCause{
			Trigger:  "foreign_order",
			Market:   o.Ticker,
			TsMillis: tsMillis,
		})
	}

	for i, f := range fills {
		if owned[i] {
			eff.OwnedFills = append(eff.OwnedFills, f)
			continue
		}
		eff.ForeignFills = append(eff.ForeignFills, f)
		eff.Anomalies = append(eff.Anomalies, risk.Anomaly{
			Class: "FOREIGN_FILL", Sev: risk.SEV1, Ticker: f.Ticker,
			Text: fmt.Sprintf("fill %s on order %s is not in the ownership "+
				"ledger, so it does not enter our_fill and does not trigger "+
				"F14; someone else is trading the account our position model "+
				"describes, and that model is now unreliable everywhere rather "+
				"than in one market", f.TradeID, f.OrderID),
		})
		eff.Causes = append(eff.Causes, StopCause{
			Trigger:  "foreign_fill",
			Market:   f.Ticker,
			TsMillis: tsMillis,
		})
	}

	eff.Exclude = make([]string, 0, len(excluded))
	for t := range excluded {
		eff.Exclude = append(eff.Exclude, t)
	}
	sort.Strings(eff.Exclude)
	return eff, nil
}

// confidence: high
