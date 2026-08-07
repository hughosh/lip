package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"lip/harness/cfg"
	"lip/harness/hstore"
	"lip/harness/quote"
	"lip/harness/rest"
	"lip/harness/risk"
)

// This file is D3: the single REST writer, and NOTHING ELSE.
//
// D3, H-ORD-6 and "ONE REST writer" are the same requirement written three
// times, and one goroutine is how it is enforced rather than remembered. That
// goroutine owns the two-stage commit end to end -- reserve, await the
// COMMITTED permit, create, bind -- and there is deliberately no second path in
// this package that reaches `rest.Client.Create` or `rest.Client.Cancel`.
//
// # What this goroutine deliberately does NOT own
//
// Not `quote.Queue`, not `quote.Capacity`, not `risk.Portfolio`, not
// `core.Rig`, not `wsx.Gate`. Those belong to the OWNER goroutine, and the
// division is not stylistic:
//
//   - none of them carries a mutex. `Queue`'s own doc says so -- "safety here
//     is single-writer, and the owner goroutine is that writer" (H-TOP-4) --
//     and `quote.Grant` states the division from the other side: "It is
//     returned rather than applied because this package owns no state that
//     ticks. cmd/harness owns the workers and the bucket and calls
//     Capacity.Take." Workers, plural, executing while the owner accounts.
//
//   - `rest.Create` blocks for up to `restTimeout` (10 s) and repeats the SAME
//     coid up to `retry_same_coid_max` times, so a write can hold its goroutine
//     for half a minute. An owner parked inside one is an owner that has
//     stopped publishing `risk.Snapshot`, and `owner_stall_s` is 3 s -- so
//     every single order the harness placed would raise SEV1 `OWNER_STALLED`.
//     A chronic false SEV1 is how a real one stops being read, which is
//     probebot.py's observable reached by a new route.
//
// So the seam is two channels of plain values. The owner decides WHAT to write
// (dequeue, classify, price, size, mint the coid, charge the capacity); this
// goroutine decides nothing and executes exactly one write at a time.
//
// # The one thing that must not move
//
// `BindOrder` is submitted INLINE, immediately after the create returns and
// before the outcome is handed back. See `placeWrite`.

// ---------------------------------------------------------------------------
// The seam
// ---------------------------------------------------------------------------

// writeRequest is one unit of work the owner hands to the REST writer.
//
// It carries a BUILT order and never the ingredients for one. §6.6's queue
// holds intents re-evaluated at dequeue, so the decision of what to send is
// taken on the owner goroutine against conditions that are current; by the time
// a request reaches this channel that decision has been made and this goroutine
// may not revisit it. In particular the coid is already minted, because
// H-ORD-2b's same-coid recovery requires the id to be a function of the run and
// a sequence the owner controls -- two writers minting coids from one run id
// would collide, and H-ORD-1 permits at most one order per coid.
type writeRequest struct {
	// IDs is every `quote.Intent` this single write discharges, echoed back on
	// the result so the owner can call `AckPlace`, `ConfirmAbsent` or `Drop`
	// against them WITHOUT having to remember what it sent.
	IDs []uint64

	Market string
	Side   quote.Side
	Role   quote.Role
	// Op is which of the two writes this is. There is no amend (H-ORD-3).
	Op quote.Op

	// Grant is the capacity the owner already charged with `Capacity.Take`. It
	// is echoed back untouched; this goroutine never applies it, because the
	// capacity value lives on the owner. `writeResult.Sent` is what the owner
	// needs to decide whether the token is refundable.
	Grant quote.Grant

	// Order is the placement, built and validated by the owner, already
	// carrying its coid. OpPlace only.
	Order rest.CreateOrder

	// Orders is the resting orders a cancel targets, from the last COMPLETE
	// orders walk. OpCancel only, and legitimately EMPTY: H-ORD-4's sweep then
	// issues no DELETE and performs only the verifying read, which is still the
	// read that turns absence into a fact (H-FAIL-3).
	Orders []rest.Order
}

// writeResult is one executed write, reported back to the owner.
//
// Everything the owner has to act on is here, and the fields are separated by
// what they license rather than by where they came from.
type writeResult struct {
	// Req is the request, echoed whole. The owner matches on Req.IDs.
	Req writeRequest

	// Create is the create's outcome. Zero for a cancel.
	//
	// The owner reads `Outcome.Exists()` for H-Q-9a's `AckPlace`, `Filled` for
	// §8.2's `Portfolio.ApplyAck`, `MaxLive` for the aggregate caps, and
	// `ReconcileNow()` for §7.2 clause 2's immediate reconciliation.
	Create rest.CreateResult

	// Sweep is the cancel's outcome. Zero for a place.
	Sweep rest.SweepResult

	// Bound reports that `BindOrder` was SUBMITTED for `Create.OrderID`. It is
	// not a claim of durability -- the binding commits on the store's writer and
	// its terminal outcome arrives as a `hstore.Result` -- it is the claim that
	// stage two was submitted on this write's own code path and not deferred.
	Bound bool

	// Absent is H-FAIL-3's licence and the ONLY thing that may be passed to
	// `Queue.ConfirmAbsent`.
	//
	// It is true only when a COMPLETE verifying read found nothing of ours
	// resting on (Market, Side): `SweepResult.Clean` for the requested orders,
	// AND no `OtherOurs` on that side. Anything less -- a 2xx, a `reduced_by`
	// equal to what we asked for, our own belief that we sent a DELETE -- is a
	// cancel REQUEST, and a cancel-requested order is still live and fillable.
	Absent bool

	// Sent reports that at least one WRITE left the process. The verifying read
	// of a sweep is not a write and is not counted here: the §16 bucket paces
	// writes, and charging it for reads would let a sweep eat the reducer's
	// share. The owner uses it to decide whether the token is refundable.
	Sent bool

	// Anomalies is EVERY anomaly this write produced, and it is the complete
	// set: it already contains `Create.Anomalies` and `Sweep.Anomalies`. The
	// owner raises this slice and nothing else -- raising the nested ones as
	// well would double-count §13.2's per-(class,ticker) suppression.
	Anomalies []risk.Anomaly

	// Err is why the write did not happen, when it did not happen. A refusal is
	// not a failure of the exchange: the commonest value here is H-STORE-3
	// revoking adding authority, which is a correct and expected outcome.
	Err error
}

// ---------------------------------------------------------------------------
// The writer goroutine
// ---------------------------------------------------------------------------

// permitWait bounds how long a placement may hold this goroutine waiting for
// its reservation to commit.
//
// It exists because the writer must stay available to CANCEL. A store whose
// writer has stalled retries the reservation forever -- correctly, the record
// is evidence -- and a dispatcher parked on that retry is a dispatcher that
// cannot issue the one write that only ever reduces exposure (I1), for as long
// as the disk is unhappy.
//
// It is `restTimeout` and not a §16 knob, for the same reason `restTimeout` is
// not one, and it is the same value deliberately: one placement then costs at
// most two of these end to end, and a placement whose ownership row could not
// commit within it is a placement whose price §6.6 would re-derive anyway.
// It comfortably exceeds `hstore`'s 5 s writer-progress bound, so a transient
// failure that recovers is not abandoned for having been slow once.
const permitWait = restTimeout

// dispatchLoop is the single REST writer (D3). It returns when ctx is done or
// when the owner closes `in`.
//
// One request at a time, in order, with no internal concurrency. That is the
// pilot's `dispatchWorkers` = 1 made structural: `Capacity.BusyGeneral` counts
// what is in flight, and a writer that fanned out would have to invent a worker
// pool the owner is not accounting for.
//
// `reserves` is how H-ORD-6's permit reaches this goroutine, and its direction
// is the important part. `hstore.Store.TakeResults` drains the WHOLE batch
// under one mutex, so two goroutines calling it would each silently take the
// other's results -- and the two losses are not symmetric. A dispatcher that
// swallowed somebody else's terminal result would delete a SEV1 that
// `reject.go` argues is observable exactly once, per record, from `Result.Err`.
// So the result loop is the SOLE consumer of the FIFO and forwards a COPY of
// every reservation outcome here; nothing on this side takes ownership of
// anything, and a result that is not ours is simply ignored.
//
// It carries the whole `hstore.Result` and not a bare permit, because a
// reservation that FAILS produces no permit at all: a permit-only channel would
// leave this goroutine waiting out `permitWait` on the one path where the answer
// is already known.
//
// It never sends on `out` without also honouring ctx, because the owner can be
// gone: a writer blocked forever on an unread result channel is a writer that
// cannot be stopped, and `hstore.Shutdown` refuses to close over records this
// goroutine still holds.
func (r *rig) dispatchLoop(ctx context.Context, in <-chan writeRequest,
	reserves <-chan hstore.Result, out chan<- writeResult) {

	for {
		select {
		case <-ctx.Done():
			return
		case req, ok := <-in:
			if !ok {
				return
			}
			res := r.executeWrite(ctx, req, reserves)
			select {
			case out <- res:
			case <-ctx.Done():
				return
			}
		}
	}
}

// executeWrite performs exactly one write and classifies it.
func (r *rig) executeWrite(ctx context.Context, req writeRequest,
	reserves <-chan hstore.Result) writeResult {

	if req.Op == quote.OpCancel {
		return r.cancelWrite(ctx, req)
	}
	return r.placeWrite(ctx, req, reserves)
}

// ---------------------------------------------------------------------------
// H-ORD-6 — the two-stage commit
// ---------------------------------------------------------------------------

// placeWrite is H-ORD-6 end to end:
//
//	RESERVE -> await the COMMIT -> permit.Order() -> create -> BIND, inline
//
// Every arrow is load-bearing and none of them may be reordered.
func (r *rig) placeWrite(ctx context.Context, req writeRequest,
	reserves <-chan hstore.Result) writeResult {

	res := writeResult{Req: req}

	coid := req.Order.ClientOrderID()
	if coid == "" {
		res.Err = errors.New("a placement reached the REST writer with no " +
			"client order id; the coid IS the ownership record (H-ORD-1) and " +
			"the owner mints it, so an empty one is a request that was never " +
			"built rather than one that may be sent")
		return res
	}
	// The owner builds the order and the ticker is its own field, so a
	// disagreement between the two means the request was assembled from two
	// different dispatches. Placing it would reserve ownership under one
	// market's coid and rest the order in another.
	if req.Order.Ticker() != req.Market {
		res.Err = fmt.Errorf("the request names market %s but its order is for "+
			"%s; the ownership record and the order would describe different "+
			"markets", req.Market, req.Order.Ticker())
		return res
	}

	// (1) STAGE ONE. This returns a RECEIPT and not a permit, and that is the
	// entire mechanism: there is no path from "I want to place an order" to "I
	// may place this order" that does not pass through a SQLite commit.
	rcpt, err := r.store.ReserveOrder(r.run, req.Order, req.Role, r.ex.NowMs())
	if err != nil {
		res.Err = fmt.Errorf("the ownership reservation for %s could not be "+
			"submitted, so H-ORD-6 forbids dispatching it: %w", coid, err)
		res.Anomalies = append(res.Anomalies, risk.Anomaly{
			Class: "RESERVATION_NOT_SUBMITTED", Sev: risk.SEV1, Ticker: req.Market,
			Text: fmt.Sprintf("the coid reservation for %s was refused by the "+
				"store (%v); no order was placed, because an order whose "+
				"ownership is unrecorded produces a fill H-ORD-9 must classify "+
				"as foreign", coid, err),
		})
		return res
	}

	// (2) The commit, AWAITED. This is the barrier, and there is no way to
	// shorten it: the permit is issued by a committed transaction and by
	// nothing else, which is what makes the argument `Create` needs
	// unobtainable any other way.
	permit, err := awaitPermit(ctx, rcpt, reserves)
	if err != nil {
		res.Err = fmt.Errorf("the coid reservation for %s never became "+
			"durable: %w", coid, err)
		res.Anomalies = append(res.Anomalies, risk.Anomaly{
			Class: hstore.RecordRejectedClass, Sev: risk.SEV1, Ticker: req.Market,
			Text: fmt.Sprintf("no dispatch permit was issued for %s (%v); "+
				"H-ORD-6's permit exists only because a reservation committed, "+
				"so the order is not placed", coid, err),
		})
		return res
	}

	// (3) H-STORE-3, enforced by the only thing that can enforce it.
	//
	// An ADDING order is refused while storage is unhealthy: its ownership
	// record could not be written, and an unrecorded order produces a fill
	// H-ORD-9 must classify as foreign. A REDUCING order is deliberately
	// unaffected -- it stays dispatchable after a later storage failure,
	// because the alternative is a disk problem that strands inventory, and
	// every stop path in this system stops adding and none stops reducing (I1).
	body, err := permit.Order()
	if err != nil {
		res.Err = err
		res.Anomalies = append(res.Anomalies, risk.Anomaly{
			Class: "ADDING_REFUSED", Sev: risk.SEV2, Ticker: req.Market,
			Text: fmt.Sprintf("the %s placement %s was not dispatched: %v",
				req.Role, permit.Coid(), err),
		})
		// The reservation COMMITTED, so this coid sits in `Ownership`'s
		// unresolved set and every unrecognised order id on the account defers
		// on it. Nothing was sent, so no order id will ever bind to it -- the
		// exchange never took the coid. Recording that conclusion is what
		// drains the set; leaving it would make one refused placement defer
		// every fill for the life of the deployment, which converts H-ORD-9's
		// repair from "no false foreign" into "no foreign ever".
		res.Anomalies = append(res.Anomalies,
			r.abandonReservation(permit.Coid(), req.Market)...)
		return res
	}

	// (4) The write.
	create := r.api.Create(ctx, body, r.cfg.Params)
	res.Create = create
	res.Sent = create.Attempts > 0
	res.Anomalies = append(res.Anomalies, create.Anomalies...)

	// (5) THE BINDING, INLINE, BEFORE THIS FUNCTION RETURNS.
	//
	// This is the single most important line in the file, and its value is in
	// WHEN it runs rather than in what it does. Between the ack and the
	// committed binding the order id is not in `Ownership.byOrder`, so every
	// fill on OUR OWN order classifies UNRESOLVED and `risk.ApplyFills` defers
	// it -- `q_local` lags the account for exactly as long as the gap lasts,
	// and past 120 s the deferral escalates to SEV2 `FILL_UNCLASSIFIABLE`.
	//
	// `wsx.bindListedOrders` is the SAFETY NET for a binding that was lost, and
	// it is one poll late BY CONSTRUCTION: it can only learn an order id from
	// the next complete orders walk, and only while the order is still listed.
	// Batching this, deferring it to the next poll, or "letting the walk pick it
	// up" makes the net the mechanism -- and the net does not catch an order
	// that filled and went terminal inside one poll interval.
	//
	// The receipt is dropped deliberately, for the reason `BindListedOrder`
	// gives: there is no completion this goroutine is entitled to wait for, and
	// a writer that blocked on the disk here would stall every cancel behind it.
	// A terminal outcome still arrives, through the result loop, as
	// `hstore.Rejections`' SEV1.
	if create.OrderID != "" {
		if _, berr := r.store.BindOrder(create.Coid, create.OrderID,
			r.ex.NowMs()); berr != nil {

			res.Anomalies = append(res.Anomalies, risk.Anomaly{
				Class: "ORDER_BINDING_NOT_SUBMITTED", Sev: risk.SEV1,
				Ticker: req.Market,
				Text: fmt.Sprintf("order %s acknowledged coid %s but the "+
					"binding could not be submitted (%v); until a walk rebinds "+
					"it every fill on that order defers, and after 120s the "+
					"deferral escalates to FILL_UNCLASSIFIABLE",
					create.OrderID, create.Coid, berr),
			})
		} else {
			res.Bound = true
		}
	}

	// (6) A definite rejection resolves the reservation from the other end.
	//
	// `CreateRejected` is the exchange answering, and its answer is no: no
	// order exists, so the coid was never taken and nothing will ever bind to
	// it. This is the ONLY create outcome that licenses abandonment. An UNKNOWN
	// does not -- we do not know the order is absent, and "we could not find it"
	// has never been evidence of anything (H-ORD-2a) -- and a 409 positively
	// identifies an order that DID land.
	if create.Outcome == rest.CreateRejected {
		res.Anomalies = append(res.Anomalies,
			r.abandonReservation(create.Coid, req.Market)...)
	}
	return res
}

// awaitPermit blocks until THIS reservation's row commits, or says why it never
// will.
//
// The match is on the RECEIPT SEQUENCE and on nothing else. A receipt is issued
// by one submission and sequence numbers begin at 1, so it identifies exactly
// one record; matching on the coid instead would match a reservation for the
// same coid made by a different submission -- which is precisely what an H-ORD-1
// collision is, and it is the case where the two results say opposite things.
//
// Everything that is not ours is IGNORED, not stashed. This channel carries
// copies: the result loop has already run the batch through
// `hstore.Rejections`, so there is nothing here to take ownership of and
// nothing to hand back.
func awaitPermit(ctx context.Context, rcpt hstore.Receipt,
	reserves <-chan hstore.Result) (hstore.DispatchPermit, error) {

	timer := time.NewTimer(permitWait)
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			return hstore.DispatchPermit{}, ctx.Err()

		case <-timer.C:
			return hstore.DispatchPermit{}, fmt.Errorf("no outcome for "+
				"reservation #%d within %v; the store has neither committed it "+
				"nor given up on it, and this goroutine has to stay available "+
				"to cancel (I1). The reservation stays outstanding, so a fill "+
				"on an order we did not send still cannot read as foreign",
				rcpt.Seq(), permitWait)

		case res, ok := <-reserves:
			if !ok {
				return hstore.DispatchPermit{}, errors.New("the store's result " +
					"loop has stopped forwarding reservation outcomes; no " +
					"permit can be issued from now on, and H-ORD-6 forbids " +
					"dispatching without one")
			}
			if res.Receipt.Seq() != rcpt.Seq() {
				continue
			}
			if res.Err != nil {
				return hstore.DispatchPermit{}, res.Err
			}
			p, valid := res.Permit()
			if !valid {
				return hstore.DispatchPermit{}, errors.New("the reservation " +
					"committed without issuing a dispatch permit; there is no " +
					"way to construct one outside hstore, and dispatching " +
					"without it is the H-ORD-6 violation the type exists to " +
					"make unexpressible")
			}
			return p, nil
		}
	}
}

// abandonReservation records the conclusion that the exchange never took a coid.
//
// It is stage two's OTHER terminal and it is the drain on `Ownership`'s
// unresolved set. Something has to drain it: while the set is non-empty an
// order id absent from the ledger reads as UNRESOLVED rather than foreign, so
// one reservation that never reached the exchange defers every unrecognised
// fill for the lifetime of the deployment.
//
// It is called ONLY where the coid provably was not taken -- a placement
// refused before dispatch, or a definite 4xx -- and never on an UNKNOWN.
func (r *rig) abandonReservation(coid, ticker string) []risk.Anomaly {
	if _, err := r.store.ResolveReservationAbandoned(coid,
		r.ex.NowMs()); err != nil {

		return []risk.Anomaly{{
			Class: "RESERVATION_NOT_RESOLVED", Sev: risk.SEV2, Ticker: ticker,
			Text: fmt.Sprintf("coid %s was never taken by the exchange but the "+
				"abandonment could not be submitted (%v); the reservation stays "+
				"outstanding, so every unrecognised fill on the account keeps "+
				"deferring until a walk resolves it", coid, err),
		}}
	}
	return nil
}

// ---------------------------------------------------------------------------
// H-ORD-4 — a cancel is not a fact until a complete read says so
// ---------------------------------------------------------------------------

// cancelWrite cancels and then VERIFIES, per H-ORD-4 and H-FAIL-3.
//
// It takes no permit and reserves nothing. H-ORD-6's two-stage commit exists
// because an order we placed and did not record is a fill we cannot classify; a
// cancel creates no such obligation, and gating it on the store would be the
// disk stopping the one write that can only reduce exposure (I1).
func (r *rig) cancelWrite(ctx context.Context, req writeRequest) writeResult {
	res := writeResult{Req: req}

	sweep := r.api.CancelAndSweep(ctx, req.Market, req.Orders)
	res.Sweep = sweep
	res.Sent = len(sweep.Cancels) > 0
	res.Anomalies = append(res.Anomalies, sweep.Anomalies...)

	// `Clean` says the REQUESTED orders are gone. `Queue.ConfirmAbsent`'s gate
	// is stronger and is keyed on (market, side): nothing of OURS rests there.
	// `OtherOurs` is exactly the difference -- every other `lipH-` order the
	// verifying read found in this ticker, reported and deliberately NOT
	// cancelled, because on a halt that set contains the reducing quote and A4
	// requires it be kept alive. The conjunction is the complete-read statement
	// H-FAIL-3 demands, and it is the only thing that opens a
	// cancel-confirm-place's second leg.
	res.Absent = sweep.Clean && !restsOn(sweep.OtherOurs, req.Side)
	return res
}

// restsOn reports whether any of our orders still rests on this side.
func restsOn(orders []rest.Order, side quote.Side) bool {
	for _, o := range orders {
		if o.Side == side {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// §6.6's token bucket — the refill half of quote.Capacity
// ---------------------------------------------------------------------------
//
// `quote` is pure by construction (H-TOP-3), so `Capacity.Take` only ever
// decrements and something outside it has to put the writes back. These three
// functions are that something. They hold NO state and start NO goroutine: the
// carry is handed in and handed back, so the owner keeps it next to the
// capacity it belongs to and a simulator replaying a tape gets the same
// arithmetic it got last time.

// refillWrites accrues `elapsed` worth of the §16 write budget into c.
//
// `elapsed` is a MONOTONIC difference and must never be a wall-clock one. F21's
// clock step makes a wall interval negative, and a bucket that refilled by a
// negative amount would hand §16's rate limit to whatever the NTP daemon last
// did.
//
// `carry` is the accrued fraction of a write from the previous call, returned
// updated. It exists so that a rate the caller's tick interval does not divide
// evenly still averages out instead of truncating part of the budget away on
// every pass.
//
// THE RESERVE IS FILLED FIRST. H-QUE-3 holds a share of the write budget for P1
// at all times because a reducing-side write is the exit; a refill that topped
// the general pool up first would let a requote storm spend every accrued token
// before the exit's share came back -- H-QUE-3 inverted at the refill instead of
// at the admission. It costs nothing when nothing is starving, because the two
// ceilings are disjoint and sum to `write_burst`.
func refillWrites(c quote.Capacity, p cfg.Params, elapsed time.Duration,
	carry float64) (quote.Capacity, float64) {

	reserved := quote.ReserveWrites(p.WriteBurst)
	general := p.WriteBurst - reserved
	if general < 0 {
		general = 0
	}
	if elapsed <= 0 || p.WriteRate <= 0 {
		// A non-advancing or backwards reading accrues nothing. The elapsed
		// value is monotonic by contract; treating a violation of that contract
		// as a large refill is the one outcome worth ruling out.
		return clampWrites(c, p), carry
	}

	carry += p.WriteRate * elapsed.Seconds()
	// An idle hour must not become an hour's worth of writes. `write_burst` IS
	// the ceiling on a burst, so the accrual is clamped to the total before any
	// of it is spent -- otherwise a harness that sat through a quiet market
	// would wake holding a budget §16 never authorised.
	if total := float64(general + reserved); carry > total {
		carry = total
	}
	whole := int(carry)
	carry -= float64(whole)

	for i := 0; i < whole; i++ {
		switch {
		case c.ReservedTokens < reserved:
			c.ReservedTokens++
		case c.Tokens < general:
			c.Tokens++
		default:
			// Both pools are at their ceiling. The remaining accrual is
			// DISCARDED rather than banked: a bucket that carried it would
			// spend the whole idle period as a burst the instant the first
			// token was taken.
			return clampWrites(c, p), 0
		}
	}
	return clampWrites(c, p), carry
}

// releaseWrite returns the worker slot a grant occupied, and returns the token
// too when no request left the process.
//
// The worker is released because with one REST writer at most one write is ever
// in flight: occupancy is real while `Create` or `CancelAndSweep` is running and
// is honestly zero the instant it returns. Modelling it as permanently idle
// would make `Capacity.Admit`'s worker arm dead code and quietly delete
// H-QUE-3's worker reserve.
//
// The TOKEN comes back only when `sent` is false. The bucket paces writes
// against the exchange, and charging for a write that never happened -- an
// H-STORE-3 refusal, a request the owner rebuilt -- would let a store outage
// spend the reducer's share of the budget on nothing at all.
func releaseWrite(c quote.Capacity, p cfg.Params, g quote.Grant,
	sent bool) quote.Capacity {

	if g.ReservedWorker {
		if c.BusyReserved > 0 {
			c.BusyReserved--
		}
	} else if c.BusyGeneral > 0 {
		c.BusyGeneral--
	}
	if !sent {
		switch {
		case g.BypassBucket:
			// P0 spent no local token by construction (§6.6).
		case g.ReservedToken:
			c.ReservedTokens++
		default:
			c.Tokens++
		}
	}
	return clampWrites(c, p)
}

// clampWrites holds both pools inside their §16 ceilings and above zero.
//
// It is applied on the way out of every function here rather than trusted to
// the arithmetic above it, because the ceilings are the whole content of
// `write_burst` and there is no reading of §6.6 in which exceeding one is
// recoverable by a later subtraction.
func clampWrites(c quote.Capacity, p cfg.Params) quote.Capacity {
	reserved := quote.ReserveWrites(p.WriteBurst)
	general := p.WriteBurst - reserved
	if general < 0 {
		general = 0
	}
	if c.ReservedTokens > reserved {
		c.ReservedTokens = reserved
	}
	if c.ReservedTokens < 0 {
		c.ReservedTokens = 0
	}
	if c.Tokens > general {
		c.Tokens = general
	}
	if c.Tokens < 0 {
		c.Tokens = 0
	}
	return c
}

// confidence: high
