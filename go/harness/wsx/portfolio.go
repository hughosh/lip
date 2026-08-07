package wsx

import (
	"context"
	"fmt"
	"time"

	"lip/harness/cfg"
	"lip/harness/num"
	"lip/harness/rest"
	"lip/harness/risk"
)

// PortfolioSource is the three authoritative reads, at exactly the shape
// `rest.Client` already provides them.
//
// It is an interface so the poller can be driven by the scenario exchange, and
// the methods are the concrete ones rather than a tidied abstraction so that
// `*rest.Client` satisfies it without an adapter. An adapter is a place for a
// filter argument to be quietly changed, and two of the three arguments below
// are ones whose wrong value returns an empty list rather than an error.
type PortfolioSource interface {
	Positions(ctx context.Context) rest.PositionsResult
	Orders(ctx context.Context, ticker, status string) rest.OrdersResult
	Fills(ctx context.Context, ticker string, since time.Time) rest.FillsResult
}

// PortfolioRead is one complete poll cycle's three results.
//
// Every field is private. The type is a RECORD of what three walks returned and
// when each of them started looking, and a caller that could assemble one could
// assemble a complete-looking reconciliation out of nothing -- which is the
// same escape hatch as an exported `noteTruth`, reached through a struct
// literal. `NewPoller().Run` is the only thing that builds one.
type PortfolioRead struct {
	token ReconcileToken
	seq   uint64

	// The three results, in the order they are read and applied.
	fills     rest.FillsResult
	orders    rest.OrdersResult
	positions rest.PositionsResult

	// Per-endpoint START stamps, taken immediately BEFORE each walk begins.
	//
	// The start and not the finish, and this is a correctness property rather
	// than a rounding preference. A completed cursor walk is a consistent
	// suffix of the account as of the moment it STARTED; everything that
	// happened during the walk is, at best, partially represented in it. The
	// fills walk scales with account history (H-POS-3) and is the slow one, so
	// stamping it with the cycle's finish can overstate its currency by the
	// whole duration of the walk -- and `truth_max_age_s` would then authorise
	// placement from evidence older than the limit it exists to enforce.
	fillsAt     Stamp
	ordersAt    Stamp
	positionsAt Stamp

	// completedAt is diagnostics and cadence only. It is deliberately NOT a
	// freshness input.
	completedAt Stamp
}

// Seq is the cycle number, for diagnostics and for a test that needs to assert
// a missed tick coalesced rather than queued.
func (r PortfolioRead) Seq() uint64 { return r.seq }

// CompletedAt is when the cycle finished. Diagnostics only: no freshness
// decision may be taken from it.
func (r PortfolioRead) CompletedAt() Stamp { return r.completedAt }

// Poller reads the three portfolio endpoints on a fixed cadence.
//
// It is a SEPARATE goroutine with a SEPARATE context from any websocket
// session, and that is H-FAIL-2 made structural: REST and the websocket are
// distinct transports, so a socket failure must not stop position monitoring.
// The probe's defect was an observer that stopped; the cheapest way to be sure
// this one does not is for the thing that fails to have no way of reaching it.
type Poller struct {
	src      PortfolioSource
	clk      Clock
	interval time.Duration
}

// NewPoller builds the portfolio poller.
func NewPoller(src PortfolioSource, clk Clock, interval time.Duration) (*Poller, error) {
	if src == nil {
		return nil, fmt.Errorf("no portfolio source")
	}
	if clk == nil {
		return nil, fmt.Errorf("no clock")
	}
	if interval <= 0 {
		return nil, fmt.Errorf("poll interval %v must be positive", interval)
	}
	return &Poller{src: src, clk: clk, interval: interval}, nil
}

// Run polls until its own context is cancelled.
//
// `tokens` delivers a new ReconcileToken whenever the connection generation
// changes -- on reconnect AND on disconnect. Receiving one triggers an
// IMMEDIATE poll. On reconnect that is H-ORD-5's reconciliation, which is what
// unlocks placement again; on disconnect it is the first reading of an account
// nobody is watching over the socket any more.
//
// Between tokens it polls every `interval`. It keeps polling while the socket
// is down; that is the entire point. Position truth during an outage is the
// only thing that can tell us whether the inventory we cannot watch is moving.
//
// # Read order: fills, orders, positions
//
// Positions is read LAST, and the order is load-bearing rather than
// aesthetic. `GET /portfolio/positions` is the authoritative q (H-POS-1) and
// `ApplyPortfolio` lets it overwrite. If it were read first, a fill that landed
// between it and the fills walk would be applied ON TOP of a q that already
// contained it: a real +12 becomes a local +24, and a reducer sized from 24
// flips the actual position to −12. Reading it last makes it the newest
// observation in the cycle as well as the authoritative one, so overwriting
// with it can only ever move q towards the exchange.
//
// Polls do not overlap. A cycle that outruns the interval does not stack a
// second cycle behind it: the missed tick is coalesced into ONE immediate next
// poll. A queue of overdue polls against a slow endpoint is how a five-second
// cadence becomes a five-minute backlog of answers about the past.
func (p *Poller) Run(ctx context.Context, tokens <-chan ReconcileToken,
	out chan<- PortfolioRead) error {

	timer := p.clk.NewTimer(p.interval)
	defer timer.Stop()

	var (
		tok ReconcileToken
		seq uint64
	)

	// poll runs one cycle and rearms the timer.
	//
	// The rearm is what makes a slow cycle coalesce instead of queue. A cycle
	// that overran the interval owes exactly ONE tick, so the next fire is
	// immediate and the one after that is a full interval later. Rearming with
	// a fixed interval instead would let a chronically slow endpoint halve the
	// real cadence silently; queueing the missed ticks would build a backlog of
	// answers about the past, which at a 5s cadence is how position monitoring
	// stops being monitoring.
	poll := func() error {
		started := p.clk.Now().Mono
		seq++
		read := PortfolioRead{token: tok, seq: seq}

		read.fillsAt = p.clk.Now()
		read.fills = p.src.Fills(ctx, "", time.Time{})
		read.ordersAt = p.clk.Now()
		read.orders = p.src.Orders(ctx, "", rest.StatusResting)
		read.positionsAt = p.clk.Now()
		read.positions = p.src.Positions(ctx)
		read.completedAt = p.clk.Now()

		wait := p.interval - (read.completedAt.Mono - started)
		if wait < 0 {
			wait = 0
		}
		timer.Reset(wait)

		select {
		case out <- read:
		case <-ctx.Done():
			return ctx.Err()
		}
		return nil
	}

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()

		case t, ok := <-tokens:
			if !ok {
				// The supervisor is gone. Keep polling: an unattended process
				// that has lost its socket still holds inventory, and this is
				// the only remaining witness to it.
				tokens = nil
				continue
			}
			tok = t
			if err := poll(); err != nil {
				return err
			}

		case <-timer.C():
			if err := poll(); err != nil {
				return err
			}
		}
	}
}

// PortfolioEffects is what applying one poll cycle produced.
type PortfolioEffects struct {
	// Applied names the endpoints whose result was complete and was used. An
	// endpoint absent from here changed nothing and did not refresh its
	// freshness clock.
	Applied [truthCount]bool
	// Stale is true when the token did not match the current connection, in
	// which case NOTHING was applied.
	Stale bool

	Records   []risk.PollRecord
	OwnedFill []risk.FillEvent
	// DeferredFill is the fills the ownership ledger could not conclude about.
	// They were not applied and not marked seen, and the next poll offers them
	// again. Reported and not swallowed: a cycle whose fills all deferred looks
	// identical to a quiet account from the outside, and the difference is a
	// ledger with unresolved reservations in it.
	DeferredFill []risk.FillEvent
	Foreign      []risk.LiveOrder
	// Bound is the coid -> order-id bindings this cycle SUBMITTED from the
	// orders walk, in walk order. Submitted, not committed.
	Bound     []Binding
	Reduce    []string
	Stop      bool
	Anomalies []risk.Anomaly
}

// Binding is one coid the orders walk recognised as an unresolved reservation of
// ours, and the exchange order id it was bound to.
type Binding struct {
	Coid    string
	OrderID string
}

func (e *PortfolioEffects) merge(anoms []risk.Anomaly) {
	e.Anomalies = append(e.Anomalies, anoms...)
}

// OrderBinder is the ownership ledger's WRITE half, as narrowly as this package
// can state it: the reservations still outstanding, and a way to bind one.
//
// It exists because a resting order the exchange lists carries the coid that
// reserved it, and the ledger may be missing exactly that binding -- H-ORD-6
// commits the reservation before dispatch, so a crash between the two leaves a
// live order whose id this process does not recognise. The orders walk is
// already reading the one fact that closes the gap; not writing it back means
// waiting for `lip-eyq`'s startup walk to run, which on a process that never
// restarts is never.
//
// It is deliberately not `risk.OwnershipLookup`. Reading ownership is something
// every fill path does; writing a binding is something exactly one walk does,
// and a single interface carrying both would hand the write to every caller that
// only needed the read. It is also allowed to be nil, unlike the lookup: a
// deployment with no binder classifies fills exactly as before and merely
// resolves reservations more slowly, whereas a deployment with no lookup cannot
// classify at all.
type OrderBinder interface {
	// UnresolvedReservations is the coids with a committed reservation and no
	// committed resolution, as a copy.
	UnresolvedReservations() map[string]struct{}
	// BindListedOrder submits the coid -> order-id binding. It returns when the
	// record is QUEUED, not when it is durable.
	BindListedOrder(coid, orderID string, boundMs int64) error
}

// ApplyPortfolio folds one poll cycle into the gate and the position model.
//
// `now` is the monotonic reading at APPLICATION time. It is used for one thing:
// asking whether truth is stale RIGHT NOW, after the cycle may have sat in a
// channel. Freshness itself is stamped from each endpoint's own start stamp.
//
// # Order: orders, then fills, then positions
//
// Positions stays LAST for the reason it always did. It is authoritative and it
// overwrites (H-POS-1); applying it before the fills means applying the fills on
// top of a figure that already contains them, which double-counts every fill
// that landed inside the cycle.
//
// Orders now goes FIRST, ahead of fills, and that swap is the second half of the
// F2 repair. The orders walk is where an unresolved reservation gets bound, and
// the fills walk is where an unrecognised order id gets classified: running them
// the other way round classifies against a ledger this same cycle was about to
// complete. It buys a bounded latency rather than a guarantee -- the binding is
// durable-asynchronous, so a fill in the same cycle still reads as unresolved
// and defers one poll -- and one deferred poll is the whole cost of never
// declaring our own order foreign.
//
// The swap is safe because the two do not interact through position state.
// `ReplaceOrders` replaces the resting-order map and touches neither `q` nor any
// order's cumulative counters; and even if it did, `settle` moves `q` by the
// MAXIMUM of the cumulative ack and the cumulative fills, so whichever of the
// two arrives second finds `applied` already at or above its own figure and
// moves nothing.
//
// # The three endpoints are applied INDEPENDENTLY
//
// A failed orders walk does not withhold a good positions walk: they answer
// different questions, they fail for different reasons, and coupling them means
// one flaky endpoint takes down the freshness of the other two and stops all
// placement.
func ApplyPortfolio(g *Gate, pf *risk.Portfolio, own risk.OwnershipLookup,
	bind OrderBinder, read PortfolioRead, mode risk.ReconcileMode,
	now time.Duration, p cfg.Params) PortfolioEffects {

	var eff PortfolioEffects

	// A read issued for a generation that is no longer current describes the
	// account across a gap. It is discarded WHOLESALE -- not partially
	// credited, not used for its timestamp -- because a half-accepted
	// reconciliation produces a state that looks reconciled.
	//
	// Note what this does NOT reject: a read taken under the token issued AT
	// disconnect. That token's generation is the current one for the whole
	// outage, so portfolio truth keeps flowing while the socket is down. It
	// cannot make anything actionable -- Actionable needs `connected` and a
	// snapshot from this generation, and an outage has neither.
	if !read.token.Valid() || read.token.gen != g.gen {
		eff.Stale = true
		eff.merge([]risk.Anomaly{{
			Class: "RECONCILE_TOKEN_STALE", Sev: risk.SEV2,
			Text: fmt.Sprintf("a portfolio read for connection generation %d "+
				"arrived at generation %d; it describes the account on the "+
				"far side of a disconnect and cannot be applied",
				read.token.gen, g.gen),
		}})
		return eff
	}

	applyOrders(g, pf, bind, read, &eff)
	applyFills(g, pf, own, read, mode, &eff)
	applyPositions(g, pf, read, p, &eff)

	// The caller's staleness question is asked from `now`, not from when the
	// endpoints answered, so a cycle that sat in the channel does not read as
	// fresher than it is.
	for k := Truth(0); k < truthCount; k++ {
		if !g.truthOK[k] || g.truthGen[k] != g.gen {
			continue
		}
		if now-g.truthAt[k] > p.TruthMaxAge {
			eff.merge([]risk.Anomaly{{
				Class: "TRUTH_STALE", Sev: risk.SEV2,
				Text: fmt.Sprintf("%s truth is %v old, past truth_max_age_s "+
					"%v; no placement decision may be taken from it (A13, "+
					"H-FAIL-4). Cancels are unaffected", k,
					now-g.truthAt[k], p.TruthMaxAge),
			}})
		}
	}
	return eff
}

// applyFills is step 2. EVERY fill is converted before ANY is applied.
//
// A conversion failure half-way through would leave q describing a prefix of
// the walk while the dedup set claims the whole of it, and no later poll can
// repair that: the skipped fills are already marked seen.
func applyFills(g *Gate, pf *risk.Portfolio, own risk.OwnershipLookup,
	read PortfolioRead, mode risk.ReconcileMode, eff *PortfolioEffects) {

	switch {
	case !read.fills.Replaces():
		eff.merge(walkAnomaly("fills", read.fills.Walk))

	case own == nil:
		// Fail closed, and do NOT advance the fills freshness clock. A nil
		// lookup is not "nothing is ours", it is "the durable ownership ledger
		// is unavailable" -- and the two produce opposite classifications of
		// the same fill. Refreshing the clock here would report the endpoint as
		// current while nothing it returned could be attributed, so A13 would
		// keep authorising placement on the strength of a read we could not
		// interpret. There is deliberately no in-memory fallback to reach for.
		eff.merge([]risk.Anomaly{{
			Class: "OWNERSHIP_LEDGER_UNAVAILABLE", Sev: risk.SEV1,
			Text: "the fills walk completed but no ownership ledger was " +
				"supplied, so no fill can be classified as ours or foreign; " +
				"the result is discarded and fills truth is left to age out",
		}})
		eff.Stop = true

	default:
		events, err := convertFills(read.fills.Fills)
		if err != nil {
			eff.merge([]risk.Anomaly{{
				Class: "FILL_UNCONVERTIBLE", Sev: risk.SEV1,
				Text: fmt.Sprintf("a fill in an otherwise complete walk could "+
					"not be converted (%v); the whole result is discarded "+
					"rather than partially applied, and the fills freshness "+
					"clock does not advance", err),
			}})
			eff.Stop = true
			return
		}
		fe := pf.ApplyFills(events, own, mode, read.fillsAt.WallMs)
		eff.OwnedFill = append(eff.OwnedFill, fe.Owned...)
		eff.DeferredFill = append(eff.DeferredFill, fe.Deferred...)
		eff.merge(fe.Anomalies)
		eff.Stop = eff.Stop || fe.Stop
		if fe.Incomplete {
			// The durable ownership ledger could not classify the batch, so
			// NOTHING was applied and nothing was marked seen. Do not refresh
			// fills truth: the walk itself completed, but no fill in it could
			// be attributed, and reporting the endpoint current on that basis
			// leaves A13 authorising placement from a read we could not
			// interpret. Left un-refreshed, it ages out and H-FAIL-4 stops
			// dispatch, which is the correct end state for an outage.
			return
		}
		eff.Applied[TruthFills] = true
		g.noteTruth(TruthFills, read.token, read.fillsAt)
	}
}

// applyOrders is step 1. H-POS-4: wholesale, and only on a complete walk.
//
// It also BINDS. Every listed order is checked against the ledger's unresolved
// reservations, and a match is submitted as a coid -> order-id binding before
// this cycle's fills are classified.
//
// # The ledger decides, not ParseCoid
//
// The binding is offered only for coids the LEDGER says are outstanding, and
// never for a coid that merely looks like ours. H-ORD-9's whole content is that
// ownership is a durable fact and not a string match: a coid parses the same
// whether we reserved it or a stranger copied the format, and `bindOrder` would
// then be told to attach an exchange order id to a reservation this store has no
// row for. `o.Ours` is not consulted here for the same reason -- it is the
// parse, and the parse is what H-ORD-9 refuses to classify from.
func applyOrders(g *Gate, pf *risk.Portfolio, bind OrderBinder,
	read PortfolioRead, eff *PortfolioEffects) {

	if !read.orders.Replaces() {
		eff.merge(walkAnomaly("orders", read.orders.Walk))
		return
	}
	bindListedOrders(bind, read, eff)
	ours := make([]risk.LiveOrder, 0, len(read.orders.Orders))
	foreign := make([]risk.LiveOrder, 0)
	for _, o := range read.orders.Orders {
		lo := risk.LiveOrder{
			OrderID: o.OrderID, Ticker: o.Ticker, Side: o.Side,
			Price4: o.Price4, Remaining: o.Remaining,
		}
		if o.Ours {
			ours = append(ours, lo)
		} else {
			foreign = append(foreign, lo)
		}
	}
	oe := pf.ReplaceOrders(ours, foreign, true)
	eff.Foreign = append(eff.Foreign, oe.Foreign...)
	eff.merge(oe.Anomalies)
	eff.merge(read.orders.Anomalies)
	eff.Applied[TruthOrders] = true
	g.noteTruth(TruthOrders, read.token, read.ordersAt)
}

// bindListedOrders closes the H-ORD-6 window for every order the exchange is
// still listing.
//
// A submission failure is SEV2 and does not stop anything. Nothing is lost by
// it: the reservation stays unresolved, fills on that order keep deferring
// rather than reading as foreign, the next poll re-offers the same order, and
// `lip-eyq`'s startup walk covers the orders that have since gone terminal.
// Failing louder would mean a store that is refusing writes -- already a SEV1
// through store health -- also halting the position model, which is the coupling
// this package avoids everywhere else.
//
// `M-W-NOBIND` deletes the submission. The ledger then never learns the order
// id from the one endpoint that reports it, so the reservation stays outstanding
// for the life of the process and every fill on the account defers forever.
func bindListedOrders(bind OrderBinder, read PortfolioRead,
	eff *PortfolioEffects) {

	if bind == nil {
		return
	}
	unresolved := bind.UnresolvedReservations()
	if len(unresolved) == 0 {
		return
	}
	for _, o := range read.orders.Orders {
		if o.ClientOrderID == "" || o.OrderID == "" {
			continue
		}
		if _, outstanding := unresolved[o.ClientOrderID]; !outstanding {
			continue
		}
		if err := bind.BindListedOrder(o.ClientOrderID, o.OrderID,
			read.ordersAt.WallMs); err != nil {

			eff.merge([]risk.Anomaly{{
				Class: "ORDER_BINDING_NOT_SUBMITTED", Sev: risk.SEV2,
				Ticker: o.Ticker,
				Text: fmt.Sprintf("the resting-order walk recognised order %s "+
					"as our unresolved reservation %s but the binding could "+
					"not be submitted (%v); the reservation stays outstanding, "+
					"so fills on this order keep deferring rather than reading "+
					"as foreign", o.OrderID, o.ClientOrderID, err),
			}})
			continue
		}
		eff.Bound = append(eff.Bound,
			Binding{Coid: o.ClientOrderID, OrderID: o.OrderID})
	}
}

// applyPositions is step 3, and it is LAST because it overwrites.
//
// H-POS-1: on every poll, `q_local := q_exch`. That is only safe as the final
// word of the cycle. Applied before the fills, it is a figure the fills are
// then added to a second time.
func applyPositions(g *Gate, pf *risk.Portfolio, read PortfolioRead,
	p cfg.Params, eff *PortfolioEffects) {

	if !read.positions.Replaces() {
		eff.merge(walkAnomaly("positions", read.positions.Walk))
		return
	}
	pe := pf.ReplacePositions(read.positions.ByTicker, true, p)
	eff.Records = append(eff.Records, pe.Records...)
	eff.merge(pe.Anomalies)
	eff.Reduce = append(eff.Reduce, pe.Reduce...)
	eff.Stop = eff.Stop || pe.Stop
	eff.Applied[TruthPositions] = true
	g.noteTruth(TruthPositions, read.token, read.positionsAt)
}

// convertFills turns the wire's fills into the position model's units.
//
// `fee_cost` is parsed rather than ignored because it is H-ORD-8's INDEPENDENT
// corroborator: maker fees are $0.00 (S2), so a non-zero fee is a taker fill
// whatever `is_taker` claims, and the two detectors fail for different reasons.
// A fee we cannot read is therefore not a cosmetic gap -- it is one of the two
// witnesses to the most expensive bug in the system going silent.
func convertFills(fills []rest.Fill) ([]risk.FillEvent, error) {
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
			TradeID: f.TradeID,
			OrderID: f.OrderID,
			Ticker:  f.Ticker,
			Side:    f.Side,
			Price4:  f.Price4,
			Count:   f.Count,
			// Money is 1e-6 dollars and a Price4 is 1e-4 dollars.
			Fee:          num.Money(fee4 * 100),
			IsTaker:      f.IsTaker,
			ExchangeTsMs: f.TsMillis,
		})
	}
	return out, nil
}

func walkAnomaly(name string, w rest.Walk) []risk.Anomaly {
	anoms := append([]risk.Anomaly(nil), w.Anomalies...)
	return append(anoms, risk.Anomaly{
		Class: "PORTFOLIO_READ_INCOMPLETE", Sev: risk.SEV2,
		Text: fmt.Sprintf("the %s walk ended %s (%v); the previous reading is "+
			"kept and its freshness clock does NOT advance, so this endpoint "+
			"will age out if the failure persists", name, w.Outcome, w.Err),
	})
}

// confidence: high
