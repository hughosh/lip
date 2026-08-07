package hstore

import (
	"errors"
	"fmt"

	"lip/harness/quote"
)

// ---------------------------------------------------------------------------
// A9 — state_event
// ---------------------------------------------------------------------------
//
// A9 requires every state transition to write a row carrying the trigger. This
// unit provides the RECORD and its constructors; it deliberately does not call
// `NextGlobal` or `NextMarket` and does not claim A9 is integrated. Integration
// is two other beads: `lip-eyq` enqueues the startup transitions before it
// publishes its decision, and `lip-3af` enqueues every live transition before it
// publishes one. Claiming A9 here on the strength of a constructor existing is
// how an assertion becomes a comment.

const (
	scopeGlobal = "global"
	scopeMarket = "market"
)

// StateEvent is one immutable A9 row.
//
// Every field is unexported and there is no setter. The invariants -- a REAL
// transition, and a trigger that is not `none` -- are checked once, by the
// constructor, and cannot be edited back out afterwards. A struct with exported
// fields would let a caller build the row the CHECK constraints exist to reject
// and then be surprised at the writer.
type StateEvent struct {
	eventID string
	tsMs    int64
	scope   string
	ticker  string
	from    string
	to      string
	trigger string
}

// Valid reports whether this event came from a constructor.
func (e StateEvent) Valid() bool {
	return e.eventID != "" && e.scope != "" && e.trigger != "" &&
		e.from != e.to
}

// EventID is the row's primary key.
func (e StateEvent) EventID() string { return e.eventID }

// TsMillis is when the transition happened.
func (e StateEvent) TsMillis() int64 { return e.tsMs }

// Scope is "global" or "market".
func (e StateEvent) Scope() string { return e.scope }

// Ticker is the market, or "" for a global transition.
func (e StateEvent) Ticker() string { return e.ticker }

// From is the state left.
func (e StateEvent) From() string { return e.from }

// To is the state entered.
func (e StateEvent) To() string { return e.to }

// Trigger is A9's reason, and it is never "none".
func (e StateEvent) Trigger() string { return e.trigger }

// StateEventRow is a `state_event` row as read back, including its run.
type StateEventRow struct {
	EventID string
	RunID   string
	TsMs    int64
	Scope   string
	Ticker  string
	From    string
	To      string
	Trigger string
}

// NewGlobalStateEvent records one §5.1 transition.
//
// It rejects an unchanged state and a `GTNone` trigger, and the second is the
// one that matters. `NextGlobal` returns `GTNone` alongside the SAME state --
// that pair is its way of saying nothing happened. A row carrying it is
// therefore either a transition whose reason was lost or a non-transition
// recorded as one, and both make the state history unreadable in the same
// direction: an operator reconstructing why the harness stopped adding finds a
// change with no cause.
func NewGlobalStateEvent(eventID string, tsMs int64, from, to quote.GlobalState,
	trigger quote.GlobalTrigger) (StateEvent, error) {

	if eventID == "" {
		return StateEvent{}, errors.New("a state event with no id")
	}
	if tsMs <= 0 {
		return StateEvent{}, errors.New("a state event with no timestamp")
	}
	if from.String() == "INVALID" || to.String() == "INVALID" {
		return StateEvent{}, fmt.Errorf("global transition %v -> %v names a "+
			"state §5.1 does not define", from, to)
	}
	if from == to {
		return StateEvent{}, fmt.Errorf("global state %v did not change, so "+
			"there is no transition to record", from)
	}
	if trigger == quote.GTNone {
		return StateEvent{}, fmt.Errorf("global transition %v -> %v carries "+
			"trigger `none`: A9 requires the reason, and NextGlobal returns "+
			"GTNone only when nothing happened", from, to)
	}
	return StateEvent{
		eventID: eventID,
		tsMs:    tsMs,
		scope:   scopeGlobal,
		ticker:  "",
		from:    from.String(),
		to:      to.String(),
		trigger: trigger.String(),
	}, nil
}

// NewMarketStateEvent records one §5.2 transition.
func NewMarketStateEvent(eventID string, tsMs int64, ticker string,
	from, to quote.MarketState, trigger quote.MarketTrigger) (StateEvent, error) {

	if eventID == "" {
		return StateEvent{}, errors.New("a state event with no id")
	}
	if tsMs <= 0 {
		return StateEvent{}, errors.New("a state event with no timestamp")
	}
	if ticker == "" {
		return StateEvent{}, errors.New("a market transition with no ticker " +
			"cannot be attributed to a market, and §5.2's states are per-market")
	}
	if from.String() == "INVALID" || to.String() == "INVALID" {
		return StateEvent{}, fmt.Errorf("market transition %v -> %v names a "+
			"state §5.2 does not define", from, to)
	}
	if from == to {
		return StateEvent{}, fmt.Errorf("market %s state %v did not change, so "+
			"there is no transition to record", ticker, from)
	}
	if trigger == quote.MTNone {
		return StateEvent{}, fmt.Errorf("market %s transition %v -> %v carries "+
			"trigger `none`: A9 requires the reason, and NextMarket returns "+
			"MTNone only when nothing happened", ticker, from, to)
	}
	return StateEvent{
		eventID: eventID,
		tsMs:    tsMs,
		scope:   scopeMarket,
		ticker:  ticker,
		from:    from.String(),
		to:      to.String(),
		trigger: trigger.String(),
	}, nil
}

// confidence: high
