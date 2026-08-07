package risk

import (
	"fmt"
	"sort"

	"lip/harness/cfg"
	"lip/harness/num"
	"lip/harness/quote"
)

// ---------------------------------------------------------------------------
// §8 — the position model
// ---------------------------------------------------------------------------

// OwnershipLookup answers H-ORD-9's only question: is this `order_id` one the
// harness has ever sent?
//
// It is an interface and not a map because the answer has to come from the
// DURABLE ledger in `harness.db`, which covers every coid the harness has ever
// sent INCLUDING terminal orders, and which outlives the process. `lip-6w5`
// implements it against storage; this package never learns how.
//
// There is deliberately no in-memory fallback. A process that has forgotten
// which orders are its own cannot classify a fill, and the failure mode of
// guessing is the one H-ORD-9 was written to close: a manual taker fill either
// falsely trips F14's global halt or silently leaves a hole in the ownership
// record, and nothing in the data distinguishes the two.
//
// # Why the seam is a BATCH, and why it can fail
//
// It answers about a whole walk at once because `ApplyFills` must classify every
// unseen fill before it mutates anything: a walk applied half-way leaves
// `q_local` describing a prefix of the account's history while the dedup set
// claims all of it, and no later poll repairs that. A per-fill lookup that could
// fail in the middle makes that atomicity impossible to state.
//
// And it returns an ERROR because the durable ledger can be UNAVAILABLE.
// "Unavailable" is not "not ours". A boolean seam has nowhere to put the
// difference, so the unavailable case silently becomes the foreign case -- which
// is SEV1, a global stop, and a claim that a third party is trading the account,
// produced by a database that would not open. The returned slice must match the
// input length; on error nothing may be inferred about any id in the batch.
//
// # Why the per-id answer is three-valued and not two
//
// H-ORD-6 reserves the coid durably BEFORE the order is dispatched, and the
// exchange order id is learned afterwards. Between those two commits the ledger
// holds a reservation with no order id: the order may exist, and a fill on it
// carries an id the ledger has never seen. A two-valued answer has to call that
// FOREIGN, which is H-ORD-9's forbidden catastrophe -- SEV1, global stop and a
// durable, operator-only WINDING_DOWN latch -- produced by our own order.
//
// So "unknown to the ledger" and "known not to be ours" are different answers,
// and only the second one is FOREIGN.
type OwnershipLookup interface {
	OwnsOrders(orderIDs []string) ([]Ownership, error)
}

// Ownership is the ledger's answer about one order id.
//
// The ZERO VALUE is `OwnershipUnresolved`, and that is the whole reason the type
// exists rather than a pair of bools. Every un-set element of a slice, every
// fake in a test that forgot a case, and every future third state defaults to
// the answer that DEFERS. `OwnershipForeign` is the one that latches a global
// stop, so it is the one nobody gets by accident.
type Ownership uint8

const (
	// OwnershipUnresolved is "the ledger cannot conclude yet". The order id is
	// not bound, and there is at least one reservation outstanding that could
	// still turn out to be it. Callers must neither apply nor disown the fill:
	// it is offered again on the next walk, and `lip-eyq`'s startup walk is what
	// eventually resolves every reservation.
	OwnershipUnresolved Ownership = iota
	// OwnershipOurs is a COMMITTED binding in the durable ledger.
	OwnershipOurs
	// OwnershipForeign is conclusive: the id is not bound and no outstanding
	// reservation could become it. Someone else is trading the account.
	OwnershipForeign
)

func (o Ownership) String() string {
	switch o {
	case OwnershipOurs:
		return "ours"
	case OwnershipForeign:
		return "foreign"
	}
	return "unresolved"
}

// FillEvent is one fill from the authoritative record, `GET /portfolio/fills`,
// converted at the edge into this package's units.
//
// `TradeID` and not `FillID` is the identity, because H-ORD-6 makes `trade_id`
// `our_fill`'s primary key and the join against `rig.db` -- it is the one
// identity the public `trade` stream also carries.
type FillEvent struct {
	TradeID string
	OrderID string
	Ticker  string
	Side    quote.Side
	Price4  int64
	Count   num.Qty
	Fee     num.Money
	IsTaker bool
	// ExchangeTsMs is when the exchange says the fill happened, in Unix
	// milliseconds. It is `our_fill.exchange_ts_ms` (H-ORD-6) and it is the
	// EXCHANGE's clock, never ours: the row is joined against `rig.db`'s public
	// trade stream, and ordering our record of a trade by when we happened to
	// read it would put our own fills in the wrong place in the tape.
	ExchangeTsMs int64
}

// AckFill is the fill quantity an order acknowledgement reported.
//
// `Filled` is CUMULATIVE for that order, not an increment: the exchange reports
// the order's total filled count each time it describes the order, and two acks
// for the same order are two views of one running total. Treating them as
// increments double-counts every re-read of the same order, which is a position
// error in the direction of believing we hold more than we do -- and it is the
// direction that sizes a reducer too large and inverts the sign of `q`.
type AckFill struct {
	OrderID string
	Ticker  string
	Side    quote.Side
	Filled  num.Qty
}

// LiveOrder is one resting order in this package's terms.
type LiveOrder struct {
	OrderID   string
	Ticker    string
	Side      quote.Side
	Price4    int64
	Remaining num.Qty
}

// ReconcileMode distinguishes the startup walk from steady-state polling.
type ReconcileMode uint8

const (
	// Seed is the startup reconciliation. The exchange has just told us the
	// position directly (`GET /portfolio/positions`), so the historical fills
	// that produced it must NOT be replayed on top of it -- that would count
	// every one of them twice. Their identities and their safety classification
	// are still recorded: a taker fill in our history is a fact about the
	// harness whenever it happened.
	Seed ReconcileMode = iota

	// Live is steady state, where a newly observed fill is news and moves q.
	Live
)

func (m ReconcileMode) String() string {
	if m == Seed {
		return "seed"
	}
	return "live"
}

// orderState is what the model remembers about one order id.
//
// The two cumulative counters are the whole mechanism by which an ack and the
// authoritative fill that corroborates it produce ONE position change rather
// than two. See applyOrder.
type orderState struct {
	ticker string
	side   quote.Side
	// ackCum is the highest cumulative filled count any acknowledgement has
	// reported for this order.
	ackCum num.Qty
	// fillCum is the total count of authoritative fills recorded for this order.
	fillCum num.Qty
	// applied is the magnitude already contributed to q by this order.
	applied num.Qty
}

// Portfolio is `q_local` and the order model of §8.2, kept pure.
//
// It reads no clock, performs no I/O and starts no goroutine. Freshness,
// polling cadence and the decision to act on any effect it returns belong to
// the caller; this type only says what the arithmetic is.
type Portfolio struct {
	q      map[string]num.Qty
	orders map[string]*orderState
	// seenTrade is H-ORD-6's dedup set, keyed by trade_id. The fills endpoint is
	// walked in full on every poll, so the same fill is delivered again on every
	// poll forever; without this the position would grow without bound while
	// every individual read was correct.
	seenTrade map[string]struct{}
	// deferredAt is the poll-clock reading at which each unresolved trade_id
	// was FIRST deferred. It is not durable and does not need to be: a restart
	// re-offers the fill, re-defers it, and restarts the 120s clock, which is
	// the right behaviour -- the operator is being told "this has been stuck for
	// two minutes", and after a restart it has not.
	deferredAt map[string]int64
	// deferralEscalated is the trade_ids the SEV2 has already been raised for,
	// so a fill stuck for an hour produces one anomaly and not one per poll.
	deferralEscalated map[string]struct{}
	// resting is the last complete order walk, by order id.
	resting map[string]LiveOrder
	// driftStreak counts consecutive polls whose disagreement exceeded
	// pos_drift_tol, per ticker. H-POS-2's rule is "two consecutive polls", so a
	// single agreeing poll has to clear it.
	driftStreak map[string]int
}

func NewPortfolio() *Portfolio {
	return &Portfolio{
		q:                 make(map[string]num.Qty),
		orders:            make(map[string]*orderState),
		seenTrade:         make(map[string]struct{}),
		deferredAt:        make(map[string]int64),
		deferralEscalated: make(map[string]struct{}),
		resting:           make(map[string]LiveOrder),
		driftStreak:       make(map[string]int),
	}
}

// NewSeededPortfolio is §7.5 step 1: a fresh portfolio holding exactly the
// position the exchange just reported, and nothing else.
//
// It exists because startup adoption cannot use `ReplacePositions`. That
// function's job is to compare a poll against what we already believed and to
// react to the disagreement -- drift records, `driftStreak`, `POSITION_DRIFT`,
// `Reduce`, `Stop`. At startup there is no prior belief to disagree with: `q` is
// empty because the process just began, so every non-zero position the exchange
// reports would read as a full-magnitude delta. On any account holding more than
// `pos_drift_hard`, that is an immediate SEV1 and a global stop fired by the act
// of starting up correctly -- and H-ORD-5's whole point is that adopting an
// existing position is the NORMAL path after a SIGKILL, not an anomaly.
//
// So this seeds and does not compare: no records, no streaks, no anomalies, no
// stop. It also replays nothing. §7.5 reads positions BEFORE fills precisely
// because the exchange has already told us where the position ended up; the
// historical fills that produced it are recorded for their identities and their
// safety classification (`risk.Seed`), never applied on top of the answer.
//
// Flat entries are dropped rather than stored as zero, so `Positions()` and the
// managed-set union see the same markets a steady-state poll would.
func NewSeededPortfolio(positions map[string]num.Qty) *Portfolio {
	p := NewPortfolio()
	for t, q := range positions {
		if q != 0 {
			p.q[t] = q
		}
	}
	return p
}

// Q is `q_local` for one market. Absent means flat.
func (p *Portfolio) Q(ticker string) num.Qty { return p.q[ticker] }

// Positions returns a copy of every non-flat market.
func (p *Portfolio) Positions() map[string]num.Qty {
	out := make(map[string]num.Qty, len(p.q))
	for t, q := range p.q {
		if q != 0 {
			out[t] = q
		}
	}
	return out
}

// LiveOrders returns a copy of the last complete resting-order walk, ordered by
// order id so that callers and tests see a stable sequence.
func (p *Portfolio) LiveOrders() []LiveOrder {
	out := make([]LiveOrder, 0, len(p.resting))
	for _, o := range p.resting {
		out = append(out, o)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].OrderID < out[j].OrderID })
	return out
}

// ---------------------------------------------------------------------------
// The ack / fill reconciliation
// ---------------------------------------------------------------------------

// FillEffects is what applying acknowledgements or fills produced.
type FillEffects struct {
	Anomalies []Anomaly
	// Stop requests global WINDING_DOWN. It is a REQUEST and not an action:
	// §5.1's NextGlobal is the only place the global state changes (H-HALT-2).
	Stop bool
	// Owned is the subset of the offered fills that the ledger claims, in the
	// order offered. `hstore` writes these to `our_fill`; `lip-gp8` reads their
	// cash flows. A taker fill of ours appears here as well: it happened, and
	// the record of it is the evidence.
	Owned []FillEvent
	// Foreign is the subset the ledger CONCLUSIVELY disclaims: not bound, and
	// no reservation outstanding that could still become it.
	Foreign []FillEvent
	// Deferred is the subset the ledger could not conclude about. They were not
	// applied, not marked seen and not disclaimed, and the next walk offers them
	// again. It is reported so the caller can see that a poll which changed
	// nothing was still doing something -- an empty `Owned` with an empty
	// `Deferred` and an empty `Foreign` is a quiet account, and an empty `Owned`
	// with a full `Deferred` is a ledger with work outstanding.
	//
	// Note what carrying deferrals does NOT do: it does not set `Incomplete`.
	// The exchange read succeeded and the fills endpoint IS current; the fills
	// in it simply have not been attributed yet. Withholding the freshness
	// stamp would age truth out and stop dispatch over a condition that is
	// normal for a second or two after every reservation.
	Deferred []FillEvent
	// Incomplete says the batch was NOT classified and nothing was applied:
	// not q, not the order counters, not the dedup set. The caller must not
	// refresh fills truth from a result carrying it -- an endpoint reported as
	// current on the strength of a read nothing could be attributed from is how
	// A13 keeps authorising placement through a ledger outage.
	Incomplete bool
}

// ApplyAck records an order acknowledgement's cumulative fill count.
//
// This is the reaction-speed half of §8.2: it moves `q_local` the moment the
// write path hears that something filled, without waiting for the next fills
// poll. It is not truth -- H-POS-1's overwrite is -- and it is not durable.
func (p *Portfolio) ApplyAck(a AckFill) FillEffects {
	var eff FillEffects
	if a.OrderID == "" {
		eff.Anomalies = append(eff.Anomalies, Anomaly{
			Class: "ACK_WITHOUT_ORDER_ID", Sev: SEV2, Ticker: a.Ticker,
			Text: "an acknowledgement reported a fill count with no order_id, " +
				"so it cannot be reconciled against the fill that will " +
				"corroborate it and would double-count q",
		})
		return eff
	}
	st, ok := p.order(a.OrderID, a.Ticker, a.Side, &eff)
	if !ok {
		eff.Stop = true
		return eff
	}
	if a.Filled > st.ackCum {
		st.ackCum = a.Filled
	}
	p.settle(st)
	return eff
}

// unclassifiedFillEscalateMs is how long a fill may stay unresolved before the
// operator is told about it.
//
// Deferral is the correct answer to "the ledger has not concluded yet", and it
// is also a perfectly quiet way to never apply a fill again. §7.2's
// `unknown_ping_s` is the same shape and the same 120 seconds: an UNKNOWN state
// is legitimate briefly and is a fault if it persists, so the protocol is to
// keep the honest answer and raise the volume rather than to invent a decision.
//
// The escalation is SEV2 and not SEV1, and it never converts to a
// classification. A fill this process cannot attribute is not evidence that a
// third party is trading the account -- that is precisely what it fails to
// establish -- and firing F14's global stop on an unresolved question is the
// original defect wearing a timer.
const unclassifiedFillEscalateMs = 120_000

// ApplyFills applies one complete authoritative fills walk.
//
// Every fill is CONVERTED and CLASSIFIED before any of them is applied, which
// is why this takes a slice and not one fill at a time. Half-applying a walk
// leaves `q_local` describing a prefix of the account's history while the
// dedup set claims the whole of it, and no later poll can repair that: the
// skipped fills are already marked seen.
//
// `nowMs` is the CALLER's poll clock in Unix milliseconds -- the wall stamp of
// the walk being applied, not `time.Now()`. It is used for one thing: deciding
// when a fill has been unresolved for too long. Reading the clock here would put
// a real timer inside the position model, and the deferral deadline would then
// be the one deadline in this package a test cannot drive.
func (p *Portfolio) ApplyFills(fills []FillEvent, own OwnershipLookup,
	mode ReconcileMode, nowMs int64) FillEffects {

	var eff FillEffects
	if own == nil {
		// Fail closed. A nil lookup is not "nothing is ours", it is "the ledger
		// is unavailable", and the two produce opposite classifications of the
		// same fill.
		eff.Anomalies = append(eff.Anomalies, Anomaly{
			Class: "OWNERSHIP_LEDGER_UNAVAILABLE", Sev: SEV1,
			Text: "no ownership ledger was supplied, so no fill can be " +
				"classified as ours or foreign; H-ORD-9 forbids the inference " +
				"that would otherwise fill the gap",
		})
		eff.Stop = true
		eff.Incomplete = true
		return eff
	}

	// --- Pass 1: what is new, WITHOUT marking anything seen -----------------
	//
	// The dedup set is not touched here. If the classification below fails, the
	// walk must be re-offerable in full, and a fill already marked seen is a
	// fill no later poll will ever apply.
	unseen := make([]FillEvent, 0, len(fills))
	batch := make(map[string]struct{}, len(fills))
	for _, f := range fills {
		if f.TradeID == "" {
			eff.Anomalies = append(eff.Anomalies, Anomaly{
				Class: "FILL_WITHOUT_TRADE_ID", Sev: SEV1, Ticker: f.Ticker,
				Text: "a fill arrived with no trade_id, so it can be neither " +
					"deduplicated nor joined to rig.db (H-ORD-6)",
			})
			eff.Stop = true
			continue
		}
		if _, seen := p.seenTrade[f.TradeID]; seen {
			continue
		}
		// A walk that repeats a trade_id within itself is deduplicated here
		// too, for the same reason the persistent set exists: the second copy
		// would otherwise be applied on top of the first.
		if _, dup := batch[f.TradeID]; dup {
			continue
		}
		batch[f.TradeID] = struct{}{}
		unseen = append(unseen, f)
	}

	// --- Pass 2: classify the WHOLE batch against the durable ledger --------
	orderIDs := make([]string, len(unseen))
	for i, f := range unseen {
		orderIDs[i] = f.OrderID
	}
	owned, err := own.OwnsOrders(orderIDs)
	if err != nil || len(owned) != len(unseen) {
		// H-ORD-9's third answer. Nothing is applied, nothing is marked seen,
		// and the batch is reported incomplete so the caller does not refresh
		// fills truth from it. Treating this as "none of them are ours" is
		// `M-HS-LOOKUPFAIL`: it manufactures a SEV1 foreign-fill stop out of a
		// storage outage, and it does so on a walk it has already consumed.
		eff.Anomalies = append(eff.Anomalies, Anomaly{
			Class: "OWNERSHIP_LEDGER_UNAVAILABLE", Sev: SEV1,
			Text: fmt.Sprintf("the ownership ledger could not classify %d "+
				"fill(s) (%v); nothing in this walk was applied and nothing "+
				"was marked seen, because H-ORD-9 has no in-memory fallback "+
				"and \"unavailable\" is not \"foreign\"", len(unseen),
				lookupErr(err, len(owned), len(unseen))),
		})
		eff.Stop = true
		eff.Incomplete = true
		return eff
	}

	// --- Pass 3: apply, now that every fill has an answer -------------------
	for i, f := range unseen {
		if owned[i] == OwnershipUnresolved {
			// DEFER. Not applied, not marked seen, not foreign.
			//
			// The ledger holds a reservation that could still turn out to be
			// this order (H-ORD-6's window, which spans a crash), so there is no
			// answer yet -- and the two available guesses are both damaging in
			// ways no later poll repairs. Guessing OURS books a stranger's
			// contracts into q. Guessing FOREIGN latches a durable, operator-only
			// WINDING_DOWN on our own order, which is the F2 catastrophe.
			//
			// Leaving it OUT of `seenTrade` is the load-bearing half:
			// `GET /portfolio/fills` re-offers the whole walk every poll, so a
			// fill that is not marked seen comes back and is reclassified once
			// the binding commits. `M-R-INDETSEEN` marks it seen anyway, which
			// turns "wait for the answer" into "silently drop the fill" -- the
			// contracts are held, q never learns, and nothing is ever logged.
			eff.Deferred = append(eff.Deferred, f)
			if first, dup := p.deferredAt[f.TradeID]; !dup {
				p.deferredAt[f.TradeID] = nowMs
			} else if nowMs-first >= unclassifiedFillEscalateMs {
				if _, told := p.deferralEscalated[f.TradeID]; !told {
					p.deferralEscalated[f.TradeID] = struct{}{}
					eff.Anomalies = append(eff.Anomalies, Anomaly{
						Class: "FILL_UNCLASSIFIABLE", Sev: SEV2, Ticker: f.Ticker,
						Text: fmt.Sprintf("fill %s on order %s has been "+
							"unresolved for %dms: the ownership ledger holds a "+
							"reservation that has never been bound or "+
							"abandoned, so the fill can be neither applied nor "+
							"declared foreign. It is still being deferred -- "+
							"this is a report, not a decision -- and it stays "+
							"deferred until the startup rebind walk resolves "+
							"the reservation", f.TradeID, f.OrderID,
							nowMs-first),
					})
				}
			}
			continue
		}

		p.seenTrade[f.TradeID] = struct{}{}
		delete(p.deferredAt, f.TradeID)
		delete(p.deferralEscalated, f.TradeID)

		if owned[i] == OwnershipForeign {
			// H-ORD-9. A foreign fill does not enter our_fill, does not trigger
			// F14, and does trigger SEV1 plus global WINDING_DOWN -- someone
			// else is trading the account our position model describes, so that
			// model is now unreliable EVERYWHERE and not in one market.
			eff.Foreign = append(eff.Foreign, f)
			eff.Anomalies = append(eff.Anomalies, Anomaly{
				Class: "FOREIGN_FILL", Sev: SEV1, Ticker: f.Ticker,
				Text: fmt.Sprintf("fill %s on order %s is not in the ownership "+
					"ledger and no reservation is outstanding that could "+
					"become it; the account is being traded by something other "+
					"than this harness and every position it models is now "+
					"suspect", f.TradeID, f.OrderID),
			})
			eff.Stop = true
			continue
		}

		eff.Owned = append(eff.Owned, f)

		st, ok := p.order(f.OrderID, f.Ticker, f.Side, &eff)
		if !ok {
			eff.Stop = true
			continue
		}
		st.fillCum += f.Count
		if mode == Seed {
			// Record the identity, keep the counters consistent, and leave q
			// alone: the exchange has already told us the position these fills
			// produced. Advancing `applied` in lockstep is what stops a LIVE
			// fill on the same order later re-applying the historical quantity.
			if st.fillCum > st.applied {
				st.applied = st.fillCum
			}
		} else {
			p.settle(st)
		}

		// H-ORD-8 and its independent corroborator S2. Evaluated AFTER the
		// position update, deliberately: the contracts exist whatever the fill
		// says about how they were acquired, and a reducer sized from a q that
		// omitted them is the second failure on top of the first.
		if f.IsTaker || f.Fee > 0 {
			eff.Anomalies = append(eff.Anomalies, Anomaly{
				Class: "TAKER_FILL", Sev: SEV1, Ticker: f.Ticker,
				Text: fmt.Sprintf("our fill %s reports is_taker=%v and fee %s; "+
					"H-Q-3 has been violated by something -- a post_only that "+
					"did not take effect, a marketable price, or an API change",
					f.TradeID, f.IsTaker, f.Fee),
			})
			eff.Stop = true
		}
	}
	return eff
}

// lookupErr describes why a batch classification is unusable.
//
// A malformed response -- the right kind of answer, of the wrong length -- is
// treated exactly like an error, and named separately so the anomaly text says
// which happened. A short slice read positionally would attribute one fill's
// ownership to another, which is the same wrong answer as an outage with none of
// the warning.
func lookupErr(err error, got, want int) error {
	if err != nil {
		return err
	}
	return fmt.Errorf("the ownership lookup answered about %d order(s) when "+
		"asked about %d; a positional answer of the wrong length attributes "+
		"one fill's ownership to another", got, want)
}

// order fetches or creates the per-order record, refusing a contradiction about
// either the market or the direction.
//
// An order that acknowledges as a YES bid and then fills as a NO bid is not a
// bookkeeping wrinkle: it is the exchange and this process disagreeing about
// which direction our own risk points, and applying either reading would move
// `q` the wrong way by twice the fill. The event is dropped and the caller is
// told to stop adding.
//
// The ticker is the same failure on the other axis, and it is refused the same
// way rather than resolved. `settle` moves the order's REMEMBERED ticker, so a
// contradicting event books contracts against the market this process first
// saw while the exchange holds them on the one it just named: two tickers'
// `q` are wrong at once, and H-POS-1's poll then reports drift on both with
// nothing to say which reading was the mistake. Storing the ticker and never
// comparing it is what made that silent.
func (p *Portfolio) order(orderID, ticker string, side quote.Side,
	eff *FillEffects) (*orderState, bool) {

	st, ok := p.orders[orderID]
	if !ok {
		st = &orderState{ticker: ticker, side: side}
		p.orders[orderID] = st
		return st, true
	}
	if st.ticker != ticker {
		eff.Anomalies = append(eff.Anomalies, Anomaly{
			Class: "ORDER_TICKER_CONFLICT", Sev: SEV1, Ticker: ticker,
			Text: fmt.Sprintf("order %s was first seen on market %s and has "+
				"now been reported on market %s; the market our own risk sits "+
				"in is in dispute and applying either reading moves q on a "+
				"market the exchange does not agree we traded",
				orderID, st.ticker, ticker),
		})
		return nil, false
	}
	if st.side != side {
		eff.Anomalies = append(eff.Anomalies, Anomaly{
			Class: "ORDER_SIDE_CONFLICT", Sev: SEV1, Ticker: ticker,
			Text: fmt.Sprintf("order %s was first seen on the %s side and has "+
				"now been reported on the %s side; the direction of our own "+
				"risk is in dispute and no reading of it is safe to apply",
				orderID, st.side, side),
		})
		return nil, false
	}
	return st, true
}

// settle brings q into line with the best evidence for one order.
//
// The order has contributed `applied` contracts to q so far. The best evidence
// for what it has actually filled is the LARGER of the cumulative ack and the
// cumulative authoritative fills -- an ack that has not yet been corroborated,
// or fills that arrived before any ack described them. Taking the maximum is
// what makes ack-before-fill and fill-before-ack produce the same position:
// whichever arrives second finds `applied` already at or above its own figure
// and moves nothing.
//
// It is also why a LOWER cumulative ack is not a retraction. Acks are a running
// total, so a smaller one is a stale view of the same order, and treating it as
// a correction would silently un-fill contracts we hold.
func (p *Portfolio) settle(st *orderState) {
	want := st.ackCum
	if st.fillCum > want {
		want = st.fillCum
	}
	if want <= st.applied {
		return
	}
	delta := want - st.applied
	st.applied = want
	// §8.1: q is signed, YES-positive. A YES bid that fills makes us long YES; a
	// NO bid that fills makes us long NO, which is the same number with the
	// other sign.
	if st.side == quote.SideYes {
		p.q[st.ticker] += delta
	} else {
		p.q[st.ticker] -= delta
	}
}

// ---------------------------------------------------------------------------
// H-POS-1 / H-POS-2 — the authoritative overwrite
// ---------------------------------------------------------------------------

// PollRecord is one row of §15's `position_poll`, written on EVERY poll
// including agreements -- a table with only disagreements in it cannot
// distinguish "no drift" from "not polling" (H-POS-2).
type PollRecord struct {
	Ticker string
	QLocal num.Qty
	QExch  num.Qty
	Delta  num.Qty
	Agreed bool
}

// PositionEffects is what one positions poll produced.
type PositionEffects struct {
	// Applied is false when the walk was incomplete. Nothing changed.
	Applied   bool
	Records   []PollRecord
	Anomalies []Anomaly
	// Reduce names the markets whose sustained disagreement sends them to
	// REDUCING.
	Reduce []string
	// Stop requests global WINDING_DOWN.
	Stop bool
}

// ReplacePositions is H-POS-1: on every poll, `q_local := q_exch`.
//
// `complete` is the caller's report of H-PAGE-1's walk outcome. An incomplete
// walk changes NOTHING -- not q, not the drift streak, not a record. "Stale,
// never empty" is the whole of H-PAGE-1, and the failure it guards against is
// specific: a truncated positions walk that reads as "flat everywhere" is the
// one answer that lets REDUCING reach IDLE, lets the reducer be cancelled, and
// lets the process believe it has nothing to manage.
//
// The overwrite covers the UNION of the markets we thought we held and the
// markets the exchange returned. Iterating only the response would leave a
// market we believe we are long in but the exchange has since settled or
// liquidated sitting at its old non-zero q forever, with a reducer sized
// against inventory that does not exist.
func (p *Portfolio) ReplacePositions(exch map[string]num.Qty, complete bool,
	prm cfg.Params) PositionEffects {

	var eff PositionEffects
	if !complete {
		eff.Anomalies = append(eff.Anomalies, Anomaly{
			Class: "POSITION_WALK_INCOMPLETE", Sev: SEV2,
			Text: "the positions walk did not complete, so q_local is " +
				"unchanged and is now the older of two readings; H-PAGE-1 " +
				"makes an incomplete read stale, never empty",
		})
		return eff
	}
	eff.Applied = true

	tickers := make([]string, 0, len(p.q)+len(exch))
	seen := make(map[string]struct{}, len(p.q)+len(exch))
	for t := range p.q {
		if _, ok := seen[t]; !ok {
			seen[t] = struct{}{}
			tickers = append(tickers, t)
		}
	}
	for t := range exch {
		if _, ok := seen[t]; !ok {
			seen[t] = struct{}{}
			tickers = append(tickers, t)
		}
	}
	sort.Strings(tickers)

	for _, t := range tickers {
		local := p.q[t]
		// Absence from a COMPLETE result means flat. That inference is only
		// available because the walk completed; it is exactly what `complete`
		// buys.
		remote := exch[t]
		delta := local - remote

		eff.Records = append(eff.Records, PollRecord{
			Ticker: t, QLocal: local, QExch: remote,
			Delta: delta, Agreed: delta == 0,
		})

		// The exchange is authoritative and overwrites, in every case --
		// including the cases below that also raise an anomaly. `q_local` exists
		// so we can react between polls, not so we can argue with the exchange.
		if remote == 0 {
			delete(p.q, t)
		} else {
			p.q[t] = remote
		}

		mag := delta.Abs()
		switch {
		case mag > prm.PosDriftHard:
			p.driftStreak[t] = 0
			eff.Anomalies = append(eff.Anomalies, Anomaly{
				Class: "POSITION_DRIFT", Sev: SEV1, Ticker: t,
				Text: fmt.Sprintf("q_local %s and q_exch %s disagree by %s, "+
					"past pos_drift_hard %s on a single poll; our model of our "+
					"own risk is wrong, so stop adding to it",
					local.Wire(), remote.Wire(), mag.Wire(),
					prm.PosDriftHard.Wire()),
			})
			eff.Stop = true
		case mag > prm.PosDriftTol:
			p.driftStreak[t]++
			if p.driftStreak[t] >= 2 {
				eff.Anomalies = append(eff.Anomalies, Anomaly{
					Class: "POSITION_DRIFT", Sev: SEV2, Ticker: t,
					Text: fmt.Sprintf("q_local %s and q_exch %s have disagreed "+
						"by more than pos_drift_tol %s for %d consecutive "+
						"polls", local.Wire(), remote.Wire(),
						prm.PosDriftTol.Wire(), p.driftStreak[t]),
				})
				eff.Reduce = append(eff.Reduce, t)
			}
		default:
			// One poll inside tolerance clears the streak. H-POS-2 says "two
			// CONSECUTIVE polls", and a counter that never resets would
			// eventually reduce every market that ever had a fill land between
			// two polls -- which is the normal case it explicitly calls normal.
			p.driftStreak[t] = 0
		}
	}
	return eff
}

// ---------------------------------------------------------------------------
// H-POS-4 — resting orders
// ---------------------------------------------------------------------------

// OrdersEffects is what one resting-order walk produced.
type OrdersEffects struct {
	// Applied is false when the walk was incomplete. The order model is
	// unchanged.
	Applied bool
	// Foreign is every resting order on the account that is not ours. They are
	// REPORTED and never cancelled here: cancelling an order we do not
	// understand is an action on someone else's risk, and the lifecycle decides
	// what a foreign order means depending on whether we are still STARTING.
	Foreign   []LiveOrder
	Anomalies []Anomaly
}

// ReplaceOrders is H-POS-4: the resting-order map is replaced wholesale, but
// only on a complete walk.
//
// The same asymmetry as H-POS-1 and for a sharper reason: an incomplete orders
// walk that retired a live order from our model would leave collateral
// committed and a fillable order on the book that no cap accounts for, while
// H-FAIL-3 is explicit that "off" means exchange-confirmed absent and not "we
// could not read it".
func (p *Portfolio) ReplaceOrders(ours, foreign []LiveOrder,
	complete bool) OrdersEffects {

	var eff OrdersEffects
	if !complete {
		eff.Anomalies = append(eff.Anomalies, Anomaly{
			Class: "ORDER_WALK_INCOMPLETE", Sev: SEV2,
			Text: "the orders walk did not complete, so the resting-order " +
				"model is unchanged; an order we merely failed to read is " +
				"still live and still fillable (H-FAIL-3)",
		})
		return eff
	}
	eff.Applied = true

	next := make(map[string]LiveOrder, len(ours))
	for _, o := range ours {
		next[o.OrderID] = o
	}
	p.resting = next

	eff.Foreign = append(eff.Foreign, foreign...)
	for _, o := range foreign {
		eff.Anomalies = append(eff.Anomalies, Anomaly{
			Class: "FOREIGN_ORDER", Sev: SEV2, Ticker: o.Ticker,
			Text: fmt.Sprintf("resting order %s on the account carries no "+
				"coid of ours; it is not cancelled here, because what a "+
				"foreign order means depends on whether this process is still "+
				"adopting a pre-existing account", o.OrderID),
		})
	}
	return eff
}

// confidence: high
