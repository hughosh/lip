package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"lip/harness/cfg"
	"lip/harness/lifecycle"
	"lip/harness/num"
	"lip/harness/quote"
	"lip/harness/rest"
	"lip/harness/risk"
	"lip/harness/wsx"
)

// This file is the OWNER GOROUTINE and the loops that feed it.
//
// # Why there is exactly one owner
//
// Three of the objects it touches carry no mutex, and none of them is an
// oversight. `quote.Queue`: "It carries no mutex, exactly as core carries none
// (H-TOP-4): safety here is single-writer, and the owner goroutine is that
// writer." `core.Rig`: the measurement rig makes the same assumption, one owner
// selecting over the frame channel. `risk.Portfolio`: pure, no clock, no
// goroutine. So the book, the position model, the queue and the capacity are all
// owned HERE, by one goroutine, and every other goroutine in this process
// reaches them through a channel or not at all.
//
// # What the other goroutines are, and why each is separate
//
//   - the STORE WRITER (`hstore.Store.Run`) -- one connection, one goroutine.
//   - the MONITOR -- I2. It holds an `atomic.Pointer` and nothing else, so no
//     decision taken here can stop it. That is the fix for probebot.py's
//     defect: 2 snapshots over 6.14 hours while 61 contracts sat naked.
//   - the WS SUPERVISOR -- owns the socket's whole lifetime and its own derived
//     contexts, so a socket failure cannot reach anything else (H-FAIL-2).
//   - the PORTFOLIO POLLER -- a separate goroutine on a separate context from
//     any session, for the same reason. It keeps polling through an outage.
//   - the DISPATCHER -- the single REST writer (D3, H-ORD-6). It is separate
//     from the owner because `rest.Create` blocks for up to `restTimeout` and
//     retries the same coid up to `retry_same_coid_max` times: an owner parked
//     inside one is an owner not publishing snapshots, and `owner_stall_s` is
//     3 seconds, so every order placed would raise a SEV1 `OWNER_STALLED`. A
//     chronic false SEV1 is how a real one stops being read.
//   - the RESULT LOOP -- drains `hstore` outcomes and the anomaly buffer.
//
// The owner therefore never blocks on I/O. Everything it does is arithmetic
// over state it already holds, plus a channel send.

// ownerTick is the floor cadence for re-evaluation in a quiet market.
//
// It is `debounce_s`, deliberately. §6.5's two brakes are both "has this
// condition held for `debounce_s`?", and a re-evaluation interval coarser than
// the debounce makes the debounce longer than §16 says it is -- silently, and
// only in the markets quiet enough that nothing else woke the loop. A busy
// market wakes it far more often, on every delivered frame.
const ownerTick = 250 * time.Millisecond

// eventBuffer and readBuffer size the hand-offs into the owner.
//
// Both are small on purpose. A deep buffer in front of a single consumer does
// not make it faster, it makes it LATE: the owner would work through a queue of
// book states that are no longer true, and §6.5 would requote against a touch
// that has already moved. `wsx.Poller` already coalesces its own overruns for
// exactly this reason -- "a queue of overdue polls against a slow endpoint is
// how a five-second cadence becomes a five-minute backlog of answers about the
// past" -- and the same argument applies one layer up.
const (
	eventBuffer = 64
	readBuffer  = 2
	// scheduleBuffer is one. A schedule read is a REPLACEMENT, not an event:
	// the newest answer supersedes every older one completely, so a queue of
	// them is a queue of answers about the past -- and §9's whole point is that
	// the schedule moves.
	scheduleBuffer = 1
)

// owner is the state exactly one goroutine may touch.
type owner struct {
	r *rig
	// sd is the stop path: signals, the A9 state_event mirror, the drain and the
	// result loop. It is a collaborator rather than a set of methods here
	// because `os/signal.Notify` and `os.Exit` are the two things this file must
	// not be able to reach by accident.
	sd *shutdown
	p  cfg.Params

	global quote.GlobalState
	market quote.MarketState

	// --- the §12 cause that is not on disk yet -------------------------------
	//
	// `lifecycle` answers every uncertain stop path with two fields, and both
	// are instructions to this loop rather than diagnostics: "BlockAdding is
	// I1's response to every uncertainty in this file... cancels, reducing
	// quotes, position polling, reconciliation and monitoring all continue while
	// it is set", and "RetryLatch asks the caller to CommitStop again"
	// (global.go:50-58, :82-89).
	//
	// Before lip-vxo neither was read anywhere in this package. `requestStop`
	// returned on a failed write, and whether the stop was ever tried again
	// depended on whether the ORIGINAL condition happened to recur -- and all
	// three of its call sites are event-driven: a portfolio read (`applyRead`),
	// a gate tick (`evaluate`) and an ack that carried a fill
	// (`applyWriteResult`). A taker fill does not recur. So one transient EIO
	// could lose a global stop for the life of the process, with the harness
	// still adding and nothing saying so.

	// stopCause is the FIRST §12 cause whose durable write has not succeeded,
	// and stopHeld is whether one is held at all.
	//
	// The first cause is kept and later ones are discarded, matching
	// `FileLatch.Ensure`'s first-writer-wins and for the reason it gives: "the
	// first durable cause is the one the operator investigates, and a later,
	// more mundane trigger -- a SIGTERM sent while winding down from a taker
	// fill -- must not overwrite the reason the harness stopped".
	//
	// `BlockAdding` and `RetryLatch` are held as ONE condition and not two,
	// because `lifecycle` produces them as one: every path that sets either sets
	// both (global.go:173-174, :213, :239, :320-321). Splitting them here would
	// admit a state this codebase never emits -- adding blocked with nothing to
	// retry -- and that state is a wedge with no exit.
	stopCause lifecycle.StopCause
	stopHeld  bool
	// stopPinged stops the retry raising its SEV1 four times a second into a
	// 256-slot buffer. The first failure is raised in full; the retries are
	// silent, and `releaseStop` says when it ended. Same argument as
	// `reducerCancelPinged`: a standing condition is worth saying once.
	stopPinged  bool
	stopRetries uint64

	// signalCause is a SIGTERM or SIGINT whose durable write failed, held
	// SEPARATELY from `stopCause` and for a different reason.
	//
	// `stopCause` is what gets written; this is what gets a DRAIN PERMIT once
	// something is written. The two cannot be one field: a taker fill may
	// already be held when the signal arrives, and first-writer-wins would then
	// discard the signal's cause -- correctly, because the taker fill is the
	// reason the operator investigates, and incorrectly for the exit, because
	// the operator still asked for the process to end.
	//
	// Without this, `SignalController.Handle` issues no permit on a failed write
	// (drain.go), `onSignal` falls back to `BeginUnplanned` (shutdown.go), and
	// `DrainTracker` upgrades an unplanned drain only when handed a valid permit
	// -- which nothing in the retry path ever mints. H-HALT-3's SIGTERM would
	// stop adding, wind down, reach flat and then idle forever.
	signalCause lifecycle.StopCause
	signalHeld  bool

	// --- §6.5's two clocks, per side ---------------------------------------
	//
	// They are separate measurements and not one reused twice.
	// `RequoteInput.StrandedFor` says why: "Debouncing the stranded brake
	// against TouchHeld would let a touch that flickers every 200ms reset the
	// timer forever, so the one rule written to rescue an order nobody is
	// trading against would be defeated by exactly the churn that stranded it."

	// touchPrice is the external touch this side was last seen at, and
	// touchSince is when it arrived there. H-Q-7 debounces against this.
	touchPrice [2]int
	touchFound [2]bool
	touchSince [2]time.Duration

	// strandedSince is when our order first went `stale_bid_ticks` or more from
	// the touch, and strandedNow whether it still is. H-Q-8 debounces against
	// the CONDITION's age, not the price's.
	strandedSince [2]time.Duration
	strandedNow   [2]bool

	// --- the write path ----------------------------------------------------

	capacity quote.Capacity
	capAt    time.Duration
	// capCarry is the fractional token accrual `refillWrites` carries between
	// calls. `write_rate` is 5/s and the owner ticks at 250 ms, so every single
	// tick accrues 1.25 tokens: dropping the fraction would round the §16 write
	// rate down to 4/s and nothing would say so.
	capCarry float64
	coidSeq  uint64

	// inflight is the one write the dispatcher is executing. At most one,
	// because there is one dispatcher: D3's "ONE REST writer" is a goroutine
	// count, and this field is what makes it observable from here.
	inflight *writeRequest

	// pending is every create this process has made that the orders walk has
	// not listed back yet -- unknown AND acked alike. See `pendingOrder`.
	//
	// An entry leaves ONLY when a complete orders walk LISTS its coid, at which
	// point the order is in `Portfolio.LiveOrders` and counting it here as well
	// would double it. Absence from a walk is never the trigger: H-ORD-2a
	// deleted the exhaustive-read proof of a negative, and "the order is not in
	// the list" is that proof wearing a different hat.
	pending map[string]pendingOrder

	// --- §9's schedule, as READ ---------------------------------------------
	//
	// `harness-spec.md` §9 opens with a source table naming where each of these
	// comes from: `market.close_time` from `/markets/{ticker}` ("trading stops;
	// position settles") and `program.end_date` from
	// `/incentive_programs?status=active` ("reward accrual stops"). It adds that
	// "`close_time` may fall before or after `end_date`. Neither is assumed
	// static" -- which is why these are polled at `schedule_poll_s` rather than
	// read once, and why none of them is a config field.
	//
	// Every one of them starts UNKNOWN, and unknown is not "far away".
	// `quote.MarketInput.HasClose` is explicit about the difference: "a market
	// whose close_time we do not know is one we cannot enforce a lead on, and
	// H-CLOSE-0 makes that the caller's problem to escalate rather than this
	// function's to assume away."
	closeAt  time.Time
	hasClose bool
	// canCloseEarly is H-CLOSE-4's flag, and on this exchange it is the common
	// case rather than the exception -- 192 of 200 active LIP programmes on
	// 2026-08-07. It selects the operator's early backoff, not a refusal.
	canCloseEarly bool
	// tradingClosed is the close OBSERVED. §5.2 is precise that this, and not
	// the arithmetic, is authoritative: a `can_close_early` market settles
	// before `close_time` and no lead computed from `close_time` sees it coming.
	tradingClosed bool
	// scheduleRead is when a COMPLETE schedule read last landed. H-CLOSE-0
	// requires the schedule be sampled at least twice within the lead it
	// enforces, so a schedule that has stopped arriving is itself a condition.
	scheduleRead   time.Duration
	scheduleEver   bool
	schedulePinged bool

	// --- what the loops have established -----------------------------------

	// reduceNoted is the reduce requests observed SINCE THE LAST EVALUATION, and
	// it is cleared at the end of every one.
	//
	// It is not a latch, and an earlier version of this field being one was a
	// wedge: `wsx.Gate` already holds the sticky reduce -- `Gate.Reducing`
	// answers it and the gate is what clears it when the condition that set it
	// goes away -- so a second latch here could only ever be an unclearable
	// copy. One disconnect would have sent this market to REDUCING for the life
	// of the process, and the socket coming back would not have brought it out.
	//
	// What the field IS for is the reduce requests that do NOT come from the
	// gate: H-POS-2's position drift, arriving through `ApplyPortfolio`. Those
	// need no latch of their own, because `quote.NextMarket` holds REDUCING
	// itself until `q` reaches zero AND nothing is stopping the market.
	reduceNoted bool

	// reducerCancelPinged stops a refused reducing-side cancel from producing
	// one anomaly per 250 ms tick. The condition is a programming error rather
	// than a market event, so it is worth saying once and worth not saying four
	// times a second into a 256-slot buffer.
	reducerCancelPinged [2]bool

	snapSeq uint64
}

// pendingOrder is one create this process has made that the orders walk has not
// listed back yet, held for its risk contribution.
//
// It covers BOTH outcomes that leave quantity live, and the second one is the
// one an earlier revision of this file missed:
//
//   - UNKNOWN, where H-ORD-2 clause 6 keeps the maximum possibly-live quantity
//     in every cap until the order is positively resolved;
//   - ACKED, where the order demonstrably exists and simply is not in
//     `risk.Portfolio` yet, because `Portfolio.ReplaceOrders` is wholesale from
//     a complete orders walk (H-POS-4) and the next one is up to
//     `position_poll_s` away.
//
// Leaving the acked case out is not a reporting gap, it is a size failure.
// `restingOn` feeds `RequoteInput.HasOurs` and `OurSize`, so an acked order the
// aggregate cannot see makes the side read EMPTY -- and §6.5's answer to an
// empty side is presence restoration, every tick, with no debounce (a presence
// gap is revenue, so `Decide` deliberately does not wait). Measured on the
// composed harness: one +8 position produced ten identical 8-contract exits in
// about three seconds, bounded only by `write_burst`, none cancelled and none
// filled -- 80 contracts of reducer against 8 contracts of inventory. H-Q-5a
// caps a reducer at |q| and A12 says no fill sequence may change the sign of q
// via a reducer; that sequence can.
type pendingOrder struct {
	side  quote.Side
	cents int
	// qty is what this order may have live: `CreateResult.MaxLive`, which is
	// the requested count for anything that is not a definite rejection. It is
	// the conservative figure on purpose -- H-Q-5b counts RESTING + SENDING +
	// UNKNOWN + unconfirmed-cancel, and every cap is evaluated against that.
	qty num.Qty
	at  time.Duration
	// acked distinguishes the two cases for ESCALATION only. Both occupy the
	// aggregate identically; only an unresolved one is worth waking an operator
	// about, because an acked order that has not appeared in a walk yet is
	// simply younger than the poll.
	acked  bool
	pinged bool
}

func newOwner(r *rig, sd *shutdown) *owner {
	o := &owner{
		r:        r,
		sd:       sd,
		p:        r.cfg.Params,
		global:   quote.Starting,
		market:   quote.Idle,
		capacity: r.cap,
		pending:  make(map[string]pendingOrder),
	}

	if r.boot.BlockAdding || r.boot.RetryLatch {
		// `rig.boot`'s two answers, honoured from the first tick rather than
		// held as diagnostics -- which is what the field's own comment asks for.
		//
		// The latch READ failed, so `NewGlobalController` bootstrapped LATCHED
		// and asked for the write to be retried. There is no cause to retry it
		// with, because nothing in this process decided to stop: this one is
		// synthesised for exactly that, and it is not a fiction -- the harness
		// really is halting, and it is halting because the latch could not be
		// read.
		//
		// `Ensure` is first-writer-wins, so if a record IS on disk this retry
		// does not overwrite the reason the previous incarnation stopped; it
		// completes the parent-directory sync that the failed read could not
		// confirm had ever happened. If nothing is on disk it writes one, and
		// a halt this process is already in becomes a halt that survives it.
		o.holdStop(lifecycle.StopCause{
			Trigger: "latch_unreadable", TsMillis: r.ex.NowMs(),
		})
	}
	return o
}

// serve is the process. It performs §7.5, installs the adoption, starts every
// other goroutine and then owns the loop until the context ends.
//
// Named `serve` rather than `run` because `rig.run` is §15's run handle -- the
// licence every record beneath it requires -- and a method shadowing it would
// make `r.run` mean two different things one keystroke apart.
func (r *rig) serve(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	sd := newShutdown(r)
	o := newOwner(r, sd)

	// The monitor starts FIRST, before the startup walk and before anything can
	// place an order. A5's obligation is to have a sample for every selected
	// market at every tick, and the interval this harness most needs observed is
	// the one where it is deciding what it inherited.
	monTick := time.NewTicker(time.Second)
	defer monTick.Stop()
	go r.mon.run(ctx, monTick.C)

	// The result loop starts before the first submission that is not the run
	// row, so no record's outcome sits unclaimed. It also drains `r.deferred` --
	// results that arrived while construction was waiting for the run handle.
	go func() {
		if err := sd.runResults(ctx); err != nil && ctx.Err() == nil {
			r.anom.raise(risk.Anomaly{
				Class: "RESULT_LOOP_EXIT", Sev: risk.SEV1,
				Text: fmt.Sprintf("the store result loop returned %v; from here "+
					"no lost record raises its SEV1 and no anomaly reaches "+
					"either journal", err),
			})
		}
	}()

	adoption, err := o.startup(ctx)
	if err != nil {
		return err
	}
	o.install(adoption)

	events := make(chan wsx.Event, eventBuffer)
	reads := make(chan wsx.PortfolioRead, readBuffer)
	tokens := make(chan wsx.ReconcileToken, 1)
	writes := make(chan writeRequest)
	results := make(chan writeResult, 1)

	go func() {
		// The supervisor never returns because of a socket; only the process
		// context ends it. Its error is therefore always the context's.
		if err := r.sup.Run(ctx, nil, events); err != nil && ctx.Err() == nil {
			r.anom.raise(risk.Anomaly{
				Class: "WS_SUPERVISOR_EXIT", Sev: risk.SEV1,
				Text: fmt.Sprintf("the websocket supervisor returned %v while "+
					"the process context was still live; the book feed is gone "+
					"and every market is non-actionable until it comes back", err),
			})
		}
	}()
	go func() {
		if err := r.poll.Run(ctx, tokens, reads); err != nil && ctx.Err() == nil {
			r.anom.raise(risk.Anomaly{
				Class: "PORTFOLIO_POLLER_EXIT", Sev: risk.SEV1,
				Text: fmt.Sprintf("the portfolio poller returned %v; position "+
					"truth stops advancing from here, so it ages out and "+
					"H-FAIL-4 stops all dispatch, which is the correct end "+
					"state but not a recoverable one", err),
			})
		}
	}()
	// The dispatcher is handed the RESERVATION OUTCOMES it needs and never the
	// store's FIFO itself. `runResults` is the sole consumer of `TakeResults`,
	// because it is the only thing that must see every result: a second consumer
	// racing it would sometimes take a terminal outcome belonging to another
	// record, and that record's SEV1 `STORE_RECORD_REJECTED` would never be
	// raised -- `reject.go`'s whole argument is that the loss is observable
	// exactly once, per record, from `Result.Err`.
	go r.dispatchLoop(ctx, writes, sd.Permits(), results)

	// §9's schedule poll. It is a SEPARATE goroutine for the same reason the
	// portfolio poller is: it is a REST read that must keep answering while the
	// socket is down, and it must not be able to park the owner. Its cadence is
	// `schedule_poll_s`, which `cfg.Validate` has already asserted is at least
	// twice per `final_lead` -- H-CLOSE-0's sampling rule, checked as arithmetic
	// rather than discovered at the close (HR-017).
	schedules := make(chan rest.ScheduleResult, scheduleBuffer)
	go r.scheduleLoop(ctx, schedules)

	// The drain runs on its own goroutine, fed observations by this one.
	//
	// It is separate because it is the only code in this process that may end
	// it, and the observation it acts on has to be an ANSWER: `TruthKnown`,
	// `AnyInventory` and `AnyLiveOrder` are all owner state, and a drain that
	// reached for them itself would be reading a position model with no mutex
	// from a second goroutine. So the owner supplies the facts and the drain
	// supplies the authority, and neither can perform the other's half.
	observations := make(chan lifecycle.DrainObservation, 1)
	go func() {
		if err := sd.drainLoop(ctx, observations); err != nil && ctx.Err() == nil {
			r.anom.raise(risk.Anomaly{
				Class: "DRAIN_LOOP_EXIT", Sev: risk.SEV1,
				Text: fmt.Sprintf("the drain loop returned %v; H-HALT-3's "+
					"escalation has stopped and no exit can now be authorised, "+
					"so a wound-down harness will idle rather than finish", err),
			})
		}
	}()

	signals, stopSignals := installSignals()
	defer stopSignals()

	tick := time.NewTicker(ownerTick)
	defer tick.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()

		case ev := <-events:
			o.applyEvent(ev, tokens)

		case read := <-reads:
			o.applyRead(read)

		case res := <-results:
			o.applyWriteResult(res)

		case sch := <-schedules:
			o.applySchedule(sch)

		case sig := <-signals:
			o.applySignal(sig)

		case <-tick.C:
		}

		now := r.ex.Mono()
		o.evaluate(now)
		o.pump(now, writes)
		o.publish(now)
		o.offerDrain(observations)
	}
}

// applySignal hands one signal to the controller and folds back what it decided.
//
// This function does not exit, does not cancel a context and does not stop a
// loop. H-HALT-3: SIGTERM enters WINDING_DOWN and keeps running until drained,
// and `drain_timeout_h` escalates rather than exiting. The exit, if it ever
// happens, is authorised by the drain and by nothing else.
func (o *owner) applySignal(sig os.Signal) {
	eff := o.sd.onSignal(sig, quote.GlobalInput{
		State:         o.global,
		Latched:       o.r.ctrl.Latched(),
		TruthReadable: o.truthKnown(),
		Reconciled:    true,
		Stop:          true,
		RiskKnown:     true,
		AnyInventory:  o.anyInventory(),
		AnyLiveOrder:  o.anyLiveOrder(),
	})
	if !eff.Recognised {
		// Not ours. It stops nothing and blocks nothing -- a controller that
		// acted on any signal would act on SIGWINCH.
		return
	}
	if !eff.Decision.Committed {
		// H-HALT-3's stop was decided and could not be made durable.
		// `SignalController.Handle` commits before it advances, so an
		// uncommitted decision here IS a failed latch write, and its
		// `BlockAdding`/`RetryLatch` are the same two answers `commitStop`
		// honours. Honouring them matters MORE here than anywhere else: a
		// signal is delivered once. There is no standing condition left to
		// recur, so a SIGTERM dropped at this line is a SIGTERM this process
		// never acts on -- while `SignalController.Handle`'s own contract
		// promises that on a failed write "the harness still stops adding and
		// still drains". The drain has already begun (`onSignal`); this is the
		// stopping-adding half, and the retry that makes it durable.
		o.holdStop(eff.Cause)
		o.holdSignal(eff.Cause)
		// Already reported. `onSignal` raised this commit's own SEV1 through
		// `eff.Anomalies`, and the first retry would otherwise raise the same
		// failed write a second time.
		o.stopPinged = true
		return
	}
	o.global = eff.Decision.State
}

// offerDrain hands this tick's facts to the drain without ever blocking.
//
// A dropped observation is not a lost one: the next tick produces another 250 ms
// later, and the drain's escalation is paced in hours. Blocking here would park
// the owner on the one goroutine whose job is to outlive it.
func (o *owner) offerDrain(observations chan<- lifecycle.DrainObservation) {
	select {
	case observations <- lifecycle.DrainObservation{
		TruthKnown:   o.truthKnown(),
		AnyInventory: o.anyInventory(),
		AnyLiveOrder: o.anyLiveOrder(),
	}:
	default:
	}
}

// startup is §7.5, retried indefinitely with backoff.
//
// There is no bounded-attempt exit and the absence is the rule: "a bounded retry
// that eventually stops is a process sitting next to inventory it decided not to
// look at again." The only thing that ends this loop is a complete adoption or
// the process context.
func (o *owner) startup(ctx context.Context) (lifecycle.Adoption, error) {
	for {
		att := o.r.start.Step(ctx, time.Now())
		o.r.anom.raiseAll(att.Anomalies)
		o.sd.mirrorGlobal(o.global, att.Decision)
		if att.Decision.Committed {
			o.global = att.Decision.State
		}

		if att.Adoption != nil {
			return att.Adoption, nil
		}
		if !att.Retry {
			// `Attempt.Retry` is documented as ALWAYS true on failure, so this
			// branch is unreachable by construction. It is written anyway
			// because the alternative to reaching it is spinning on a
			// coordinator that has told us to stop, and a busy loop against a
			// failing exchange is worse than a refusal.
			return nil, fmt.Errorf("startup declined to retry: %w", att.Err)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(att.After):
		}
	}
}

// install replaces the placeholder model with what §7.5 actually read.
//
// Everything here comes from the Adoption and nothing is derived a second time.
// The portfolio is seeded from the exchange with every kept order installed; the
// market states are the adoption's, so a held market starts REDUCING (§7.5 step
// 6) rather than being re-derived from a `q` this function would have to guess
// the provenance of.
func (o *owner) install(a lifecycle.Adoption) {
	o.r.pf = a.Portfolio()
	o.global = o.r.start.State()
	if st, ok := a.States()[o.r.cfg.Ticker]; ok {
		o.market = st
	}
	o.r.anom.raiseAll(a.Anomalies())

	sum := a.Summary()
	o.r.anom.raise(risk.Anomaly{
		Class: "STARTUP", Sev: risk.SEV3, Ticker: o.r.cfg.Ticker,
		Text: fmt.Sprintf("run %s adopted %d order(s), cancelled %d stale, saw "+
			"%d foreign order(s) and %d foreign fill(s); balance %s, managed %v, "+
			"excluded %v, reducing %v. Global state is %s and %s is %s",
			o.r.runID, sum.AdoptedOrders, sum.CancelledStale, sum.ForeignOrders,
			sum.ForeignFills, sum.Balance, sum.Managed, sum.Excluded,
			sum.Reducing, o.global, o.r.cfg.Ticker, o.market),
	})
}

// applyEvent folds one websocket event into the gate and the book.
//
// `core.Rig.Handle` is reached ONLY through `Gate.ApplyFrame`, and never called
// directly. The gate owns delivery: it decides whether a frame reaches the book
// at all (a granularity frame or an undeliverable one does not), it quarantines
// the book when the handler refuses, and `FrameEffects.Delivered` is its REPORT
// of what it did rather than permission for the caller to do it afterwards.
func (o *owner) applyEvent(ev wsx.Event, tokens chan<- wsx.ReconcileToken) {
	switch ev.Kind {
	case wsx.EventConnected:
		eff := o.r.gate.OnConnect(ev.At)
		o.r.anom.raiseAll(eff.Anomalies)
		// A new generation invalidates every read taken under the old one, and
		// the token is what says so. Delivering it triggers an IMMEDIATE poll,
		// which is H-ORD-5's reconciliation and is what unlocks placement.
		o.offerToken(tokens, eff.Token)
		o.resnapshot(eff.Resnapshot)

	case wsx.EventDisconnected:
		eff := o.r.gate.ApplyDisconnect(ev.At, ev.Clean)
		o.r.anom.raiseAll(eff.Anomalies)
		if eff.ResetBooks {
			// Every level carried across the gap may already be wrong, and a
			// trade arriving before the replacement snapshot would be
			// attributed to it. `core` discards five pieces of state together
			// and dropping any one of them fails silently, which is why this is
			// its call and not a field-by-field reset here.
			o.r.book.ResetOnReconnect()
		}
		// The token is offered on DISCONNECT as well as on connect. The poller's
		// own contract says why: "on disconnect it is the first reading of an
		// account nobody is watching over the socket any more."
		o.offerToken(tokens, eff.Token)

	case wsx.EventDisconnectReduce:
		// F4. `lip-0qj` is exactly this relay: the supervisor observes the
		// outage crossing `disconnect_reduce_s` and the gate is what turns it
		// into a REDUCING market. Nothing else forwards it.
		eff := o.r.gate.NoteDisconnectSustained(ev.Down, ev.At)
		o.r.anom.raiseAll(eff.Anomalies)
		o.noteReduce(eff.Reduce)
		o.resnapshot(eff.Resnapshot)

	case wsx.EventFrame:
		info := wsx.InspectFrame(ev.Frame)
		frame := ev.Frame
		eff := o.r.gate.ApplyFrame(info, func() error {
			return o.r.book.Handle(frame)
		}, ev.At)
		o.r.anom.raiseAll(eff.Anomalies)
		o.noteReduce(eff.Reduce)
		// `core.Rig` detects its own subscription-wide sequence gaps and asks
		// for a resnapshot through `NeedsResnapshot`; the gate asks separately
		// through `FrameEffects`. Both are honoured, and the gate is told about
		// core's gap so the two do not disagree about whether the book is
		// trustworthy.
		if o.r.book.NeedsResnapshot {
			o.r.book.NeedsResnapshot = false
			gapEff := o.r.gate.NoteSeqGap(ev.At)
			o.r.anom.raiseAll(gapEff.Anomalies)
			o.noteReduce(gapEff.Reduce)
			o.resnapshot(true)
		}
		o.resnapshot(eff.Resnapshot)
	}
}

// offerToken hands a reconcile token to the poller without ever blocking.
//
// The channel holds one. A token that cannot be delivered because an earlier one
// is still queued is not lost information: the poller reads the LATEST token and
// polls immediately on it, and two generations' worth of "reconcile now" is one
// reconciliation. Blocking here would park the owner on a goroutine it is
// supposed to be independent of.
func (o *owner) offerToken(tokens chan<- wsx.ReconcileToken, tok wsx.ReconcileToken) {
	if !tok.Valid() {
		return
	}
	select {
	case tokens <- tok:
	default:
	}
}

// resnapshot asks the exchange to re-send the book.
//
// The command channel to the live session is deliberately not wired: this
// harness subscribes one market, and `wsx.Supervisor.Run` re-subscribes on every
// reconnect, which is the only path that produces a snapshot the exchange will
// actually send after a gap. What a resnapshot request would buy on a LIVE
// socket is a faster recovery from a sequence gap; what it costs is a second
// writer to the socket. The gate already quarantines the book until a snapshot
// arrives, so the slow path is safe rather than merely tolerable.
//
// This is recorded as an anomaly rather than silently skipped, because a market
// that stays quarantined until the next reconnect is a market that is not
// quoting, and the operator should be able to see why.
func (o *owner) resnapshot(want bool) {
	if !want {
		return
	}
	o.r.anom.raise(risk.Anomaly{
		Class: "RESNAPSHOT_DEFERRED", Sev: risk.SEV2, Ticker: o.r.cfg.Ticker,
		Text: "the gate asked for a book resnapshot; this build recovers by " +
			"reconnect rather than by an in-session update_subscription, so " +
			"the book stays quarantined and the market non-actionable until " +
			"the socket cycles. No placement decision is taken from a " +
			"quarantined book (A13, H-FAIL-5)",
	})
}

// scheduleLoop reads §9's schedule on `schedule_poll_s`, forever.
//
// It polls IMMEDIATELY on entry and then on the interval. The first read is the
// one that lets the market quote at all -- until it lands there is no close to
// enforce a lead against -- so waiting a full interval for it would mean every
// start of this process spends `schedule_poll_s` stopped for a reason that has
// nothing to do with the market.
//
// A failed read is not retried faster and is not escalated here. The owner owns
// that judgement: it holds the previous reading, it knows how old it is, and
// `scheduleStale` is where a reading stops being usable. A poller that decided
// on its own that a schedule was too old would be a second opinion about the
// same fact.
func (r *rig) scheduleLoop(ctx context.Context, out chan<- rest.ScheduleResult) {
	t := time.NewTicker(r.cfg.Params.SchedulePoll)
	defer t.Stop()

	for {
		res := r.api.Schedule(ctx, r.cfg.Ticker)
		select {
		case out <- res:
		case <-ctx.Done():
			return
		default:
			// The owner has not taken the previous one. Drop THIS one rather
			// than blocking: the reads are replacements, so the owner is about
			// to consume an answer at most one interval older than this, and a
			// poller parked on a send is a poller that has stopped observing.
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// applySchedule folds one complete schedule read into the owner.
//
// A read that did not land changes NOTHING -- not the close, not the flags, not
// the freshness clock. That last one is the important part: refreshing the clock
// on a failed read would report the schedule current while holding an answer
// nobody re-established, which is precisely the shape `wsx.applyFills` refuses
// for the same reason ("the walk itself completed, but no fill in it could be
// attributed, and reporting the endpoint current on that basis leaves A13
// authorising placement from a read we could not interpret").
func (o *owner) applySchedule(res rest.ScheduleResult) {
	o.r.anom.raiseAll(res.Anomalies)
	if !res.Observed() {
		return
	}

	if res.TradingClosed && !o.tradingClosed {
		// H-CLOSE-4, OBSERVED. This is the one close the arithmetic cannot see
		// coming, and until this read existed nothing in this binary could
		// produce the flag at all.
		o.r.anom.raise(risk.Anomaly{
			Class: "TRADING_CLOSED", Sev: risk.SEV2, Ticker: o.r.cfg.Ticker,
			Text: fmt.Sprintf("%s reports status %q: trading has stopped. This "+
				"is the close OBSERVED rather than computed, which is the only "+
				"way a can_close_early market's close is ever seen (H-CLOSE-4)",
				o.r.cfg.Ticker, res.Status),
		})
	}

	o.closeAt, o.hasClose = res.CloseTime, res.HasClose
	o.canCloseEarly = res.CanCloseEarly
	o.tradingClosed = res.TradingClosed
	o.scheduleRead = o.r.ex.Mono()
	o.scheduleEver = true
}

// untilClose is `close_time - now`, and whether we know it at all.
//
// The second result is `quote.MarketInput.HasClose` and it is not a
// convenience. An unread schedule is NOT a close far away: §5.2 treats an
// unknown close as a lead that cannot be enforced and escalates, whereas a
// zero `time.Time` differenced against now is a close roughly two thousand
// years in the past, which reads as "settle immediately".
func (o *owner) untilClose() (time.Duration, bool) {
	if !o.hasClose || o.scheduleStale() {
		return 0, false
	}
	return time.Until(o.closeAt), true
}

// scheduleStale is H-CLOSE-0 stated as an expiry on the reading itself.
//
// A close_time we read once and have not refreshed is not a fact about the
// market, it is a fact about the market AS OF THEN -- and §9 is explicit that
// "neither is assumed static". HR-017 is that difference costing a close:
//
//	at schedule_poll_s = 300 with final_lead = 60s, a close_time that moves
//	from 17:00 to 12:03 at 12:00:01 is next observed at 12:05 -- after the
//	close. Neither the close lead nor the final cancel ever ran.
//
// The threshold is not a taste. H-CLOSE-0's rule is "a lead cannot be enforced
// unless the schedule is read at least twice within it", which `cfg.Validate`
// already enforces on the CONFIGURATION by refusing `schedule_poll_s * 2 >
// final_lead`. The same rule applied to a READING says a sample older than
// `final_lead` cannot enforce `final_lead`, whatever value it carries. So past
// that age the schedule stops being usable and `HasClose` goes false -- which
// stops the adding side through `closeUnknown` and leaves the exit alive.
//
// This is the direction that matters. An ABSENT schedule is conspicuous; a
// STALE one looks exactly like a good one, and every lead computed from it
// reads as enforced.
func (o *owner) scheduleStale() bool {
	if !o.scheduleEver {
		return true
	}
	return o.r.ex.Mono()-o.scheduleRead > o.p.FinalLead
}

// untilCloseOrZero adapts `untilClose` to `PastFinalLead`'s argument pair.
//
// `PastFinalLead(untilClose, hasClose, p)` returns false when `hasClose` is
// false, which is the correct reading: a market whose close we have not read is
// not past its final lead, it is a market whose lead cannot be enforced -- and
// §5.2 escalates that separately rather than acting on it here.
func (o *owner) untilCloseOrZero() (time.Duration, bool, cfg.Params) {
	u, has := o.untilClose()
	return u, has, o.p
}

// closeUnknown stops the market while §9's schedule has not been read.
//
// H-CLOSE-0 hands this case to the caller rather than resolving it: "a market
// whose close_time we do not know is one we cannot enforce a lead on, and
// H-CLOSE-0 makes that the caller's problem to escalate rather than this
// function's to assume away." This is the caller, and stopping is what
// escalating means here.
//
// It stops ADDING and nothing else -- §5.2's response to a market-scoped stop
// is REDUCING, so the exit stays alive and the position stays managed. The
// alternative, quoting on, means resting a new adding order into a market that
// may close before the next poll, with no lead having been enforced and no
// H-CLOSE-3 final cancel having run. HR-017 is that failure with a slow poll;
// this is the same failure with no poll at all.
//
// It is normal for a few seconds at startup, before the first schedule read
// lands, so it is reported only once it has persisted past the interval that
// was supposed to refresh it.
func (o *owner) closeUnknown(hasClose bool) bool {
	if hasClose {
		// The schedule is current again. Re-arm, so a second outage is reported
		// as loudly as the first -- a once-per-process alert about a recurring
		// condition is an alert that describes the first hour of a deployment
		// and nothing after it.
		o.schedulePinged = false
		return false
	}
	if !o.schedulePinged {
		o.schedulePinged = true
		age, known := o.scheduleAge()
		o.r.anom.raise(risk.Anomaly{
			Class: "SCHEDULE_UNUSABLE", Sev: risk.SEV1, Ticker: o.r.cfg.Ticker,
			Text: fmt.Sprintf("%s has no usable close_time (%s). No close lead "+
				"can be enforced from a schedule this process does not hold, "+
				"and H-CLOSE-3's final cancel cannot run either, so the market "+
				"is STOPPED: the adding side comes off and is confirmed absent, "+
				"and the capped reducer stays (§5.2, A8, I1). §9 reads it from "+
				"/markets/{ticker} every schedule_poll_s %v",
				o.r.cfg.Ticker, scheduleWhy(age, known, o.p.FinalLead),
				o.p.SchedulePoll),
		})
	}
	return true
}

// scheduleAge is how long since a complete schedule read, if there has been one.
func (o *owner) scheduleAge() (time.Duration, bool) {
	if !o.scheduleEver {
		return 0, false
	}
	return o.r.ex.Mono() - o.scheduleRead, true
}

// scheduleWhy distinguishes the two ways a schedule is unusable, because they
// want different responses from an operator: one is an endpoint that has never
// answered, the other is one that has stopped.
func scheduleWhy(age time.Duration, known bool, bound time.Duration) string {
	if !known {
		return "no schedule read has ever completed"
	}
	return fmt.Sprintf("the last complete read was %v ago, past final_lead %v "+
		"-- H-CLOSE-0 requires the schedule be sampled at least twice within "+
		"the lead it enforces, so a reading older than the lead cannot enforce "+
		"it whatever value it carries (HR-017)",
		age.Truncate(time.Second), bound)
}

// earlyCloseDue is the operator's H-CLOSE-4 rule, and it is the one deviation
// in this file from §16's numbers.
//
// §16's `close_lead` is 1h, cut from 4h by HR-011 to bound how long a resting
// reducer faces a stale book. That trade is made against a close we can see
// coming. `can_close_early` markets are the ones we cannot: they settle on
// external information at a moment no schedule predicts, and 192 of the 200
// active LIP programmes carry the flag -- so this is the ordinary case, not the
// exception, and "prefer against selecting them" (H-CLOSE-4) does not scale to
// a universe that is 96% early-closeable.
//
// The operator's decision is therefore to stop ADDING four hours out in those
// markets rather than one. It is expressed as a market-scoped `Stop` and not as
// a modified `close_lead`, and the difference matters in three ways:
//
//   - §5.2's response to a stop is REDUCING -- "adding side cancelled,
//     confirmed absent; capped reducer rests" -- which is exactly the intent.
//     `MarketInput.Stop` is explicit that a stop "sends the market to REDUCING,
//     never to a state that cancels everything, which is the inversion the
//     whole design turns on." The EXIT stays alive. Nothing here cancels it.
//   - `close_lead` and `final_lead` are untouched, so SETTLING still begins at
//     `close_time - close_lead` and H-CLOSE-3's final cancel still runs at
//     `final_lead`. This composes with §9 rather than replacing it.
//   - §16 stays the table the `run` row records verbatim. A per-deployment
//     backoff in `cfg.Params` would be a spec deviation dressed as
//     configuration, which `params_test.go` exists to refuse.
//
// It returns false when the close is unknown. That is not an oversight: an
// unread schedule already produces `HasClose = false`, which §5.2 escalates on
// its own, and manufacturing a stop from a number we do not have would silence
// that escalation with an answer.
func (o *owner) earlyCloseDue(untilClose time.Duration, hasClose bool) bool {
	if !hasClose || !o.canCloseEarly || o.r.cfg.EarlyCloseLead <= 0 {
		return false
	}
	return untilClose <= o.r.cfg.EarlyCloseLead
}

func (o *owner) noteReduce(tickers []string) {
	for _, t := range tickers {
		if t == o.r.cfg.Ticker {
			o.reduceNoted = true
		}
	}
}

// applyRead folds one poll cycle into the gate and the position model.
//
// The argument order is `wsx`'s and must not be rearranged: orders, then fills,
// then positions. Orders first so an unresolved reservation is bound before this
// same cycle's fills are classified against the ledger; positions LAST because
// they overwrite (H-POS-1), and applying them first double-counts every fill
// that landed inside the cycle.
func (o *owner) applyRead(read wsx.PortfolioRead) {
	now := o.r.ex.Mono()
	eff := wsx.ApplyPortfolio(o.r.gate, o.r.pf, o.r.store.Ownership(), o.r.store,
		read, risk.Live, now, o.p)

	o.r.anom.raiseAll(eff.Anomalies)
	o.noteReduce(eff.Reduce)

	for _, f := range eff.OwnedFill {
		if _, err := o.r.store.RecordFill(o.r.run, f, o.r.ex.NowMs(), false); err != nil {
			o.r.anom.raise(risk.Anomaly{
				Class: "FILL_NOT_RECORDED", Sev: risk.SEV1, Ticker: f.Ticker,
				Text: fmt.Sprintf("our fill %s on order %s could not be "+
					"submitted to the store (%v); H-ORD-6 makes trade_id the "+
					"join against rig.db, and a fill that is not recorded is "+
					"one no later analysis can find", f.TradeID, f.OrderID, err),
			})
		}
	}
	if eff.Stop {
		o.requestStop("portfolio_read", "")
	}
	if eff.Applied[wsx.TruthOrders] {
		// A complete orders walk is the ONLY thing that resolves an unknown
		// create, and it resolves it positively: the coid is listed, so the
		// order exists and is now in `Portfolio.LiveOrders`. Absence is not
		// evidence in either direction (H-ORD-2a), so nothing is cleared for
		// not appearing.
		o.clearListed()
	}
}

// clearListed drops pending creates the resting-order walk has since listed.
//
// Listing the coid is the ONLY release. It is a positive fact -- the exchange
// named the order -- and it is also the moment the quantity appears in
// `Portfolio.LiveOrders`, so releasing it here is exactly what stops the same
// order being counted in two places at once.
func (o *owner) clearListed() {
	if len(o.pending) == 0 {
		return
	}
	listed := make(map[string]struct{})
	for _, lo := range o.r.pf.LiveOrders() {
		if bound, ok := o.r.store.Ownership().Bound(lo.OrderID); ok {
			listed[bound] = struct{}{}
		}
	}
	for coid := range o.pending {
		if _, seen := listed[coid]; seen {
			delete(o.pending, coid)
		}
	}
}

// evaluate is one pass of §5.2, §6.2 and §6.5 over the one market.
func (o *owner) evaluate(now time.Duration) {
	// `RetryLatch`, honoured, and FIRST: a held cause that becomes durable on
	// this tick publishes its WINDING_DOWN on this tick, and one that does not
	// keeps `stopHeld` set for the `MarketInput.Stop` term below. Driving it
	// from here rather than from the triggering event is the point -- every
	// caller of `requestStop` is event-driven, and a taker fill does not recur.
	o.retryStop()

	// §5.1's OTHER edges, driven from the tick rather than from a stop.
	//
	// Before lip-xdq `advance` had exactly ONE caller -- the stop funnel -- so
	// the global state moved only on the tick a §12 cause was made durable.
	// WINDING_DOWN -> DRAINED and DRAINED -> WINDING_DOWN are defined in the
	// machine and nothing ever asked for them. So a harness that wound down and
	// reduced to flat never published DRAINED: every snapshot, every §14 report
	// and the whole A9 history said it was still winding down, and `GTInventory`
	// -- the edge that exists because "DRAINED rests no reducer" -- was
	// unreachable in production, because the state it leaves was never entered.
	//
	// It runs BEFORE `NextMarket` below so the market is placed from this tick's
	// global state rather than the last one's.
	//
	// It cannot enable adding, which is what makes it safe to run unconditionally:
	// `GlobalState.AddsRisk` is `g == Running`, and of the edges reachable from
	// the states this loop is ever in, none ends in RUNNING. `Stop` is
	// deliberately false -- the stop funnel is the only thing entitled to assert
	// one, and `Advance` refuses an unlatched `Stop` anyway.
	//
	// `Reconciled` is unread in RUNNING, WINDING_DOWN and DRAINED, which is
	// every state reachable here: `install` seeds the owner from
	// `Startup.State()`, and a Step that returns an Adoption has already been
	// taken to RUNNING or, if latched, to WINDING_DOWN. It is supplied as an
	// honest answer rather than a literal because §5.1's UNKNOWN_RISK rule --
	// "never decays into RUNNING without a complete successful reconciliation"
	// -- is exactly what fresh truth on all three walks attests.
	o.advance(o.globalFacts())

	// The non-gate reduce requests are consumed by THIS evaluation and cleared
	// on the way out, on every exit including the final-cancel one. Anything
	// that must outlive a tick is held by the thing that owns it -- the gate for
	// a disconnect or a book quarantine, `quote.NextMarket`'s own state for
	// inventory and drift -- and never by a second copy here.
	defer func() { o.reduceNoted = false }()

	tick := o.r.gate.Tick(o.r.ex.Clock.Now())
	o.r.anom.raiseAll(tick.Anomalies)
	o.noteReduce(tick.Reduce)
	if tick.Stop {
		o.requestStop("gate", o.r.cfg.Ticker)
	}

	ticker := o.r.cfg.Ticker
	q := o.r.pf.Q(ticker)
	untilClose, hasClose := o.untilClose()

	next, trig := quote.NextMarket(quote.MarketInput{
		State:  o.market,
		Q:      q,
		Global: o.global,
		// Selected is §10's decision and it is "the only way into QUOTING". The
		// pilot's one market is selected by the operator, so the only question
		// left here is whether this process may add at all -- which is the
		// global state. `Stop` carries every reason the MARKET may not, and
		// §5.2's IDLE -> QUOTING edge already requires `!Stop`, so folding a
		// reduce condition into `Selected` as well would be the same rule
		// applied twice and clearable in only one of the two places.
		Selected: o.global == quote.Running,
		// The gate owns the sticky reduce. `Gate.Reducing` is the latch and the
		// gate is what clears it; `reduceNoted` carries only this evaluation's
		// non-gate requests, principally H-POS-2's drift.
		// The gate owns the sticky reduce; `reduceNoted` carries this
		// evaluation's non-gate requests, principally H-POS-2's drift. The last
		// term is the operator's H-CLOSE-4 backoff -- see `earlyCloseDue`.
		//
		// The FIRST term is `BlockAdding`, honoured. A §12 cause that has
		// been decided and could not be made durable stops adding HERE, which
		// is the one place §5.2 stops it: REDUCING cancels the adding side and
		// keeps the capped exit resting, and a flat market goes to IDLE, whose
		// own guard is what keeps it from resuming. The global state is
		// deliberately NOT moved with it -- publishing WINDING_DOWN on a stop
		// that is not on disk is HR-009 -- so the market stops while the state
		// waits for the latch.
		//
		// It goes through `Stop` and not through `Selected`, because §5.2's
		// IDLE -> QUOTING edge already requires `!Stop`: expressing it in both
		// would be one rule clearable in only one of the two places, and
		// MTDeselected would label the A9 row an operator selection decision
		// rather than a stop.
		Stop: o.stopHeld || o.reduceNoted || o.r.gate.Reducing(ticker) ||
			!o.r.gate.Connected() || o.earlyCloseDue(untilClose, hasClose) ||
			o.closeUnknown(hasClose),
		// ProgramEnded is H-CLOSE-1 and is NOT wired. It is a literal false and
		// not an owner field, deliberately: a field nothing ever writes reads
		// as wired to everyone downstream, and this file argues everywhere else
		// that an assertion resting on a constructor existing is a comment.
		//
		// What it needs is not `program.end_date` read once. The live universe
		// on 2026-08-07 shows LIP programmes are short REWARD PERIODS inside a
		// market's life -- one sampled programme ran 21:45Z to 22:00Z on a
		// market closing over a day later -- so `end_date` passing is a gap
		// between periods, not the end of the incentive. H-CLOSE-1's condition
		// is "this ticker is in no active programme any more", which is a poll
		// of `/incentive_programs?status=active` and a membership test, and
		// `feed.Universe` cannot answer it (it returns ticker and target size
		// and is hash-pinned).
		//
		// Left false because the cost is ECONOMIC and not safety. False keeps
		// the adding quote alive, so the failure is quoting while unpaid --
		// unremunerated risk on an S=1 canary. Every rule that bounds actual
		// exposure is wired: the close lead, the operator's early-close
		// backoff, the inventory ladder, the gate, and the capital caps. The
		// opposite error would be worse: reading a between-periods gap as "the
		// programme ended" stops quoting in a market that is still paying.
		ProgramEnded:  false,
		UntilClose:    untilClose,
		HasClose:      hasClose,
		TradingClosed: o.tradingClosed,
	}, o.p)
	if next != o.market {
		o.sd.mirrorMarket(ticker, o.market, next, trig)
		o.market = next
	}

	sz := quote.SizesFor(quote.SizeInput{
		State:         o.market,
		Q:             q,
		Funded:        o.fundedReducer(q),
		PastFinalLead: quote.PastFinalLead(untilClose, hasClose, o.p),
	}, o.p)

	if sz.FinalCancel {
		// H-CLOSE-3: cancel everything in this market and verify with a sweep.
		// Nothing of ours rests into the close. This is an obligation to ACT and
		// not merely a refusal to place, which is why it is a separate branch
		// from a state that happens to quote nothing.
		o.enqueueCancel(now, quote.SideYes, quote.RoleAdding)
		o.enqueueCancel(now, quote.SideNo, quote.RoleAdding)
		return
	}

	reducing, held := quote.ReducingSide(q)
	actionable := o.r.gate.Actionable(ticker, o.r.ex.Clock.Now())

	for _, side := range []quote.Side{quote.SideYes, quote.SideNo} {
		role := quote.RoleAdding
		target, wanted := sz.Add, sz.HasAdd
		if held && side == reducing {
			role, target, wanted = quote.RoleReducing, sz.Reduce, sz.HasReduce
		} else if held && side != reducing {
			target, wanted = sz.Add, sz.HasAdd
		}

		if !wanted {
			// The state rests nothing on this side. If something of ours is
			// there, it comes off -- A8 for REDUCING's adding side, and the
			// same act for every other state that quotes nothing.
			if o.atRisk(side) > 0 {
				o.enqueueCancel(now, side, role)
			}
			continue
		}
		if !actionable {
			// A13 / H-FAIL-4 / H-FAIL-5: no PLACEMENT decision may be taken
			// from a quarantined or stale book. Cancels are unaffected and were
			// handled above, which is the asymmetry every stop path in this
			// system has (I1).
			continue
		}
		o.decideSide(now, side, role, target)
	}
}

// decideSide runs §6.5 for one side and queues what it asks for.
func (o *owner) decideSide(now time.Duration, side quote.Side, role quote.Role,
	target num.Qty) {

	book := o.r.book.Book(o.r.cfg.Ticker)
	if book == nil {
		return
	}
	levels := book.Yes()
	if side == quote.SideNo {
		levels = book.No()
	}

	ours, price, has := o.restingOn(side)
	ext := quote.ExternalBest(levels, ours)
	o.noteTouch(now, side, ext)

	otherPrice, hasOther := o.bestOn(side.Opposite())

	in := quote.RequoteInput{
		Side:        side,
		Role:        role,
		OurPrice:    price,
		OurSize:     o.atRisk(side),
		HasOurs:     has,
		Ext:         ext,
		TouchHeld:   o.touchHeld(now, side),
		StrandedFor: o.strandedFor(now, side),
		OtherPrice:  otherPrice,
		HasOther:    hasOther,
		Size:        target,
		// Headroom is read only when place-then-cancel is permitted, and it is
		// not. Left at zero so that a future build turning H-Q-9 on has to
		// supply it deliberately rather than inherit a stale value.
		Headroom: 0,
		// H-Q-9 stays OFF for the pilot: cancel-confirm-place everywhere
		// (pilot-plan §2.7, §7.1). This is the zero value and is written
		// explicitly anyway, because the flag is the single line between the
		// pilot's simple invariant and two of our orders live at once.
		AllowPlaceThenCancel: false,
	}

	rq := quote.Decide(in, o.p)
	if !rq.Move {
		return
	}
	if has && rq.Price == price {
		// Already there. `Decide` returns early on this, but the check is
		// repeated because `Move` with an unchanged price would enqueue a write
		// that cancels an order to replace it with itself.
		return
	}

	reason := quote.ReasonRequote
	switch {
	case role == quote.RoleReducing:
		reason = quote.ReasonReduce
	case rq.Trigger == quote.TriggerAbsent:
		reason = quote.ReasonPresence
	}

	kind := rq.Kind
	if !has {
		kind = quote.KindPlace
	}
	o.enqueue(now, quote.Intent{
		Market: o.r.cfg.Ticker, Side: side, Role: role,
		Kind: kind, Reason: reason,
	})
}

// enqueueCancel queues the retirement of a side.
//
// A standalone cancel is legal on the ADDING side only (H-QUE-2): a
// reducing-side cancel removes the exit, so it is only ever the first leg of a
// cancel-confirm-place. A caller asking to retire a reducing side outright is
// asking for something §6.6 does not have a row for, so it is refused loudly
// rather than downgraded into a shape the queue would accept.
func (o *owner) enqueueCancel(now time.Duration, side quote.Side, role quote.Role) {
	if role == quote.RoleReducing {
		if !o.reducerCancelPinged[side] {
			o.reducerCancelPinged[side] = true
			o.r.anom.raise(risk.Anomaly{
				Class: "REDUCER_CANCEL_REFUSED", Sev: risk.SEV2,
				Ticker: o.r.cfg.Ticker,
				Text: fmt.Sprintf("a standalone cancel was wanted for the %s "+
					"reducing side; H-QUE-2 has no row for one, because "+
					"cancelling the exit is not work this harness does on its "+
					"own account. Nothing was queued, and this is reported once "+
					"per side rather than once per tick", side),
			})
		}
		return
	}
	o.enqueue(now, quote.Intent{
		Market: o.r.cfg.Ticker, Side: side, Role: role,
		Kind: quote.KindCancel, Reason: quote.ReasonCancel,
	})
}

// enqueue admits one intent, refusing duplicates of work already pending.
//
// §6.6's queue holds INTENTS re-evaluated at dequeue, not requests, so a second
// intent for a side that already has one queued is not additional information --
// it is the same decision taken again from the same state, and admitting it
// would let a quiet market accumulate one entry per owner tick.
func (o *owner) enqueue(now time.Duration, in quote.Intent) {
	for _, pending := range o.r.queue.Pending() {
		if pending.Market == in.Market && pending.Side == in.Side &&
			pending.Op() == in.Op() {
			return
		}
	}
	if _, err := o.r.queue.Enqueue(now, in); err != nil {
		o.r.anom.raise(risk.Anomaly{
			Class: "INTENT_REFUSED", Sev: risk.SEV2, Ticker: in.Market,
			Text: fmt.Sprintf("the write queue refused a %s/%s intent: %v",
				in.Kind, in.Reason, err),
		})
	}
}

// pump refills the write budget and hands at most one write to the dispatcher.
//
// At most one, because there is one dispatcher goroutine and it is synchronous:
// D3's "ONE REST writer" is what makes `AllowPlaceThenCancel = false`
// enforceable rather than aspirational, and a second concurrent write is the
// shape that would quietly undo it.
func (o *owner) pump(now time.Duration, writes chan<- writeRequest) {
	o.capacity, o.capCarry = refillWrites(o.capacity, o.p, now-o.capAt, o.capCarry)
	o.capAt = now

	if o.inflight != nil {
		return
	}
	d, ok := o.r.queue.Dequeue(o.conditions(now), o.capacity)
	if !ok {
		return
	}

	req, err := o.build(d)
	if err != nil {
		// The dispatch cannot be built, so the intents it would have discharged
		// are dropped rather than left to be re-selected forever at a class that
		// keeps rising. Dropping is §6.6's own answer to an intent whose
		// condition no longer holds, and "we cannot express this write" is a
		// condition that will not hold on the next tick either.
		for _, id := range d.IDs {
			o.r.queue.Drop(id)
		}
		o.r.anom.raise(risk.Anomaly{
			Class: "WRITE_NOT_BUILDABLE", Sev: risk.SEV2, Ticker: d.Market,
			Text: fmt.Sprintf("a %s on %s/%s was selected but could not be "+
				"built: %v", d.Op, d.Market, d.Side, err),
		})
		return
	}

	// The capacity is charged BEFORE the send and never after. `Capacity.Take`
	// is the caller's half of H-QUE-3, and charging it on completion would let
	// the same token fund every write issued while one was in flight.
	o.capacity = o.capacity.Take(d.Grant)
	select {
	case writes <- req:
		o.inflight = &req
	default:
		// The dispatcher did not take it. Nothing left the process, so the token
		// comes back with the worker slot -- and the intents are RELEASED rather
		// than left queued, because `Dequeue` has already advanced a dependent
		// one to stage-first-sent and nothing but a confirmation moves it from
		// there. §6.5 re-decides on the next tick.
		o.capacity = releaseWrite(o.capacity, o.p, d.Grant, false)
		for _, id := range d.IDs {
			o.r.queue.Drop(id)
		}
	}
}

// conditions is §6.6's re-evaluation, rebuilt at every dequeue and cached
// nowhere. A condition that was true 30 seconds ago is exactly what "not
// pre-built requests" is guarding against.
func (o *owner) conditions(now time.Duration) quote.Conditions {
	q := o.r.pf.Q(o.r.cfg.Ticker)
	return quote.Conditions{
		Now:    now,
		Global: o.global,
		AboveSoft: map[string]bool{
			o.r.cfg.Ticker: q.Abs() > o.p.InvSoft,
		},
		Valid: func(in quote.Intent) bool {
			// A placement is still wanted only while the market may place at
			// all. A cancel is always still wanted: it can only reduce
			// exposure, and an intent to remove an order does not stop being
			// true because the book moved.
			if in.Op() == quote.OpCancel {
				return true
			}
			return o.r.gate.Actionable(in.Market, o.r.ex.Clock.Now())
		},
	}
}

// build turns a selected dispatch into the concrete write.
//
// The coid is minted HERE, in the owner, and not in the dispatcher. H-ORD-1's
// determinism is what makes H-ORD-2b's same-coid recovery possible at all, and
// the sequence it counts is owner state: a counter living in the goroutine that
// also blocks on the network would advance on retries it did not author.
func (o *owner) build(d quote.Dispatch) (writeRequest, error) {
	req := writeRequest{
		IDs: append([]uint64(nil), d.IDs...), Market: d.Market,
		Side: d.Side, Role: d.Role, Op: d.Op, Grant: d.Grant,
	}

	if d.Op == quote.OpCancel {
		for _, lo := range o.r.pf.LiveOrders() {
			if lo.Ticker != d.Market || lo.Side != d.Side {
				continue
			}
			cents, exact := rest.CentsExact(lo.Price4)
			if !exact {
				cents = 0
			}
			req.Orders = append(req.Orders, rest.Order{
				OrderID: lo.OrderID, Ticker: lo.Ticker, Side: lo.Side,
				Price4: lo.Price4, PriceCents: cents, Fractional: !exact,
				Remaining: lo.Remaining, Status: rest.StatusResting,
			})
		}
		if len(req.Orders) == 0 {
			return writeRequest{}, fmt.Errorf("nothing of ours rests on %s/%s",
				d.Market, d.Side)
		}
		return req, nil
	}

	price, ok := o.targetPrice(d.Side)
	if !ok {
		return writeRequest{}, fmt.Errorf("no external touch to place against")
	}
	size, bound := o.targetSize(d.Side, d.Role)
	if size <= 0 {
		return writeRequest{}, fmt.Errorf("the side is already carrying its "+
			"aggregate target, so the remainder is %s", size.Wire())
	}

	o.coidSeq++
	// The pilot quotes ONE market, so the market index is 0. It is written as a
	// named constant rather than a bare literal because `ParseCoid` reads it
	// back as the market this order belongs to, and a second market added
	// without giving it a distinct index would attribute both to the first.
	coid, err := rest.Coid(o.r.runID, pilotMarketIdx, d.Side, o.coidSeq)
	if err != nil {
		return writeRequest{}, err
	}
	order, err := rest.NewCreateOrder(d.Market, d.Side, price, size, bound, coid)
	if err != nil {
		return writeRequest{}, err
	}
	req.Order = order
	return req, nil
}

// pilotMarketIdx is the `%03d` field of H-ORD-1's coid. See `build`.
const pilotMarketIdx = 0

// targetPrice re-reads the touch at dispatch rather than carrying the price the
// decision was taken at.
//
// §6.6 is explicit that the queue holds intents and not requests, and a price
// captured at enqueue is the request half of exactly that. The book has moved by
// the time a write is admitted through the rate limiter, and placing at the old
// touch is how an order arrives already behind.
func (o *owner) targetPrice(side quote.Side) (int, bool) {
	book := o.r.book.Book(o.r.cfg.Ticker)
	if book == nil {
		return 0, false
	}
	levels := book.Yes()
	if side == quote.SideNo {
		levels = book.No()
	}
	ours, _, _ := o.restingOn(side)
	ext := quote.ExternalBest(levels, ours)
	if !ext.Found || !quote.ValidPrice(ext.Price) {
		return 0, false
	}

	price := ext.Price
	if other, has := o.bestOn(side.Opposite()); has {
		max, ok := quote.MaxOpposite(other)
		if !ok {
			return 0, false
		}
		if price > max {
			// H-CO-6. `Decide` already clamps a REDUCING side here and blocks an
			// adding one outright; this repeats the bound at the last moment
			// before the bytes are built, because the opposite side may have
			// been placed between the decision and the dispatch.
			price = max
		}
	}
	if !quote.ValidPrice(price) {
		return 0, false
	}
	return price, true
}

// targetSize is the REMAINDER: the aggregate the side should carry, less what is
// already working on it (H-Q-5b).
//
// The second result is H-CO-4b's `derivedFrom` bound -- |q| for a reducer,
// size_A for an adding quote -- which `rest.NewCreateOrder` validates the count
// against. Passing it rather than zero is what makes an over-sized reducer a
// construction error instead of a placed order.
func (o *owner) targetSize(side quote.Side, role quote.Role) (num.Qty, num.Qty) {
	q := o.r.pf.Q(o.r.cfg.Ticker)
	sz := quote.SizesFor(quote.SizeInput{
		State:         o.market,
		Q:             q,
		Funded:        o.fundedReducer(q),
		PastFinalLead: quote.PastFinalLead(o.untilCloseOrZero()),
	}, o.p)

	target, bound := sz.Add, quote.SizeA(q, o.p)
	if role == quote.RoleReducing {
		target, bound = sz.Reduce, q.Abs()
	}
	remainder := target - o.atRisk(side)
	if remainder < 0 {
		remainder = 0
	}
	if remainder > bound {
		remainder = bound
	}
	return remainder, bound
}

// fundedReducer converts capital into the contract count §6.2's size_R caps
// against.
//
// It sizes against `now` -- what can be funded without cancelling anything --
// and never against `afterCuts`. `ReducingBudget` states the reason: capital
// tied up in an adding order is not free until the exchange confirms the cancel,
// and a reducer sized against the post-cut figure before the cancels land gets
// an `insufficient_balance` reject, which H-CAP-5 makes a correctness failure
// and a global WINDING_DOWN rather than a market condition.
func (o *owner) fundedReducer(q num.Qty) num.Qty {
	reducing, held := quote.ReducingSide(q)
	if !held {
		return 0
	}
	// Priced at the BOOK's touch, not at our own resting price.
	//
	// `funded` bounds the aggregate the exit may carry, so the question it
	// answers is "how many contracts can this capital buy at the price the exit
	// would be placed at" -- and §6.5 places at the touch. Pricing it off our
	// own resting order would size the exit against a price we are no longer
	// quoting at the moment the touch has moved, which is exactly when the
	// reducer is being resized.
	var price int
	{
		book := o.r.book.Book(o.r.cfg.Ticker)
		if book == nil {
			return 0
		}
		levels := book.Yes()
		if reducing == quote.SideNo {
			levels = book.No()
		}
		best, _, ok := levels.Max()
		if !ok {
			return 0
		}
		price = best
	}
	now, _ := risk.ReducingBudget(o.exposures(), o.p)
	return risk.FundedContracts(now, num.Price4FromCents(price))
}

// exposures is the committed-collateral picture §10.2's caps are measured
// against.
//
// Held positions are valued at SETTLEMENT ($1.00), for the reason `policy.go`
// gives at length: the positions endpoint answers with the position and not what
// it cost, and overstating what a position ties up can only shrink a budget,
// whereas understating it produces the `insufficient_balance` reject H-CAP-5
// treats as a correctness failure.
func (o *owner) exposures() []risk.Exposure {
	by := make(map[string]*risk.Exposure)
	get := func(t string) *risk.Exposure {
		e, ok := by[t]
		if !ok {
			e = &risk.Exposure{Ticker: t}
			by[t] = e
		}
		return e
	}
	for t, q := range o.r.pf.Positions() {
		get(t).Position += risk.SideCost(q, risk.SettlementPrice4)
	}
	for _, lo := range o.r.pf.LiveOrders() {
		e := get(lo.Ticker)
		reducing, held := quote.ReducingSide(o.r.pf.Q(lo.Ticker))
		if held && lo.Side == reducing {
			e.Reducing += risk.SideCost(lo.Remaining, lo.Price4)
			continue
		}
		e.Adding += risk.SideCost(lo.Remaining, lo.Price4)
	}
	out := make([]risk.Exposure, 0, len(by))
	for _, e := range by {
		out = append(out, *e)
	}
	return out
}

// restingOn is our own aggregate presence on one side, in the shape
// `ExternalBest` subtracts.
//
// It returns the AGGREGATE (H-Q-5b): exchange-confirmed resting, plus the write
// currently in flight, plus every unresolved create's maximum possibly-live
// quantity. Leaving any of the three out would leave that size in the external
// touch, and we would then chase ourselves (H-Q-10).
func (o *owner) restingOn(side quote.Side) ([]quote.Resting, int, bool) {
	byPrice := make(map[int]num.Qty)
	for _, lo := range o.r.pf.LiveOrders() {
		if lo.Ticker != o.r.cfg.Ticker || lo.Side != side {
			continue
		}
		cents, exact := rest.CentsExact(lo.Price4)
		if !exact {
			// H-CO-3a. A resting price that is not a whole cent cannot be
			// subtracted from a book indexed by whole cents, and guessing which
			// level it belongs to would delete somebody else's liquidity.
			continue
		}
		byPrice[cents] += lo.Remaining
	}
	if o.inflight != nil && o.inflight.Market == o.r.cfg.Ticker &&
		o.inflight.Side == side && o.inflight.Op == quote.OpPlace {
		byPrice[o.inflight.Order.PriceCents()] += o.inflight.Order.Count()
	}
	for _, u := range o.pending {
		if u.side == side {
			byPrice[u.cents] += u.qty
		}
	}

	out := make([]quote.Resting, 0, len(byPrice))
	best, has := 0, false
	for price, size := range byPrice {
		if size <= 0 {
			continue
		}
		out = append(out, quote.Resting{Price: price, Size: size})
		if !has || price > best {
			best, has = price, true
		}
	}
	return out, best, has
}

// bestOn is our own best price on one side, for H-CO-6.
func (o *owner) bestOn(side quote.Side) (int, bool) {
	_, price, has := o.restingOn(side)
	return price, has
}

// atRisk is the aggregate every size cap is evaluated against (H-Q-5b, A11).
func (o *owner) atRisk(side quote.Side) num.Qty {
	ours, _, _ := o.restingOn(side)
	var total num.Qty
	for _, r := range ours {
		total += r.Size
	}
	return total
}

// noteTouch advances §6.5's two clocks for one side.
func (o *owner) noteTouch(now time.Duration, side quote.Side, ext quote.External) {
	i := int(side)

	if !ext.Found {
		o.touchFound[i] = false
		o.strandedNow[i] = false
		return
	}
	if !o.touchFound[i] || o.touchPrice[i] != ext.Price {
		o.touchPrice[i], o.touchFound[i], o.touchSince[i] = ext.Price, true, now
	}

	_, price, has := o.restingOn(side)
	if !has {
		o.strandedNow[i] = false
		return
	}
	away, _ := quote.BehindBy(price, ext)
	if away < 0 {
		away = -away
	}
	stranded := away >= o.p.StaleBidTicks
	if stranded && !o.strandedNow[i] {
		// The CONDITION's age, started when it began rather than when the price
		// last moved. A touch that flickers every 200ms must not reset this.
		o.strandedSince[i] = now
	}
	o.strandedNow[i] = stranded
}

func (o *owner) touchHeld(now time.Duration, side quote.Side) time.Duration {
	i := int(side)
	if !o.touchFound[i] {
		return 0
	}
	return now - o.touchSince[i]
}

func (o *owner) strandedFor(now time.Duration, side quote.Side) time.Duration {
	i := int(side)
	if !o.strandedNow[i] {
		return 0
	}
	return now - o.strandedSince[i]
}

// applyWriteResult folds the dispatcher's answer back into the queue and the
// risk model.
func (o *owner) applyWriteResult(res writeResult) {
	o.inflight = nil
	o.r.anom.raiseAll(res.Anomalies)
	// The worker slot always comes back; the TOKEN comes back only when nothing
	// left the process. A write refused by H-STORE-3 or rebuilt by the owner
	// spent no exchange write, and charging the §16 bucket for it would let a
	// store outage spend the reducer's share of the budget on nothing at all.
	o.capacity = releaseWrite(o.capacity, o.p, res.Req.Grant, res.Sent)

	// A WRITE THAT DID NOT COMPLETE MUST RELEASE ITS INTENTS, and this is the
	// least obvious rule in the file.
	//
	// `Queue.commit` advances a DEPENDENT intent's first leg to `StageFirstSent`
	// at DEQUEUE -- "that one line is H-Q-9a: dispatch is not confirmation" --
	// and leaves it in the queue. `Intent.Dispatchable()` is false at that stage,
	// and `Conditions.stillWanted` deliberately never drops it ("a write cannot
	// be un-sent"). So an intent whose write did not reach a terminal answer is
	// not merely delayed: it occupies its (market, side) FOREVER, it can never be
	// selected again, and `enqueue`'s duplicate check then refuses every
	// replacement for that side. The market stops quoting and nothing says so.
	//
	// Dropping is safe precisely because it is not a retreat: `evaluate` re-runs
	// §6.5 on the next tick from current conditions and re-enqueues if the
	// decision still holds, which is what §6.6 means by holding intents rather
	// than requests.
	if res.Err != nil {
		for _, id := range res.Req.IDs {
			o.r.queue.Drop(id)
		}
		o.r.anom.raise(risk.Anomaly{
			Class: "WRITE_FAILED", Sev: risk.SEV2, Ticker: res.Req.Market,
			Text: fmt.Sprintf("the %s on %s/%s did not complete (%v); its "+
				"intents are released so the side is not wedged at "+
				"stage-first-sent, and §6.5 re-decides on the next tick",
				res.Req.Op, res.Req.Market, res.Req.Side, res.Err),
		})
		return
	}

	if res.Req.Op == quote.OpCancel {
		if res.Absent {
			// H-ORD-4 and H-FAIL-3. `Absent` is the ONLY thing that may reach
			// `ConfirmAbsent`, and `dispatch.go` is strict about what earns it:
			// a COMPLETE verifying read that found nothing of ours resting on
			// this (market, side). A 2xx is a cancel REQUEST, and a
			// cancel-requested order is still live and still fillable.
			o.r.queue.ConfirmAbsent(res.Req.Market, res.Req.Side)
			return
		}
		// Not verified absent. The intents are RELEASED for the reason above:
		// a cancel-confirm-place whose cancel did not confirm sits at
		// stage-first-sent, and only `ConfirmAbsent` moves it -- which is
		// exactly what did not happen. Left in place it would hold this side
		// against every future intent for the life of the process.
		//
		// The orders themselves stay in the risk model regardless. H-FAIL-3: a
		// cancel we requested and did not see confirmed is a live, fillable
		// order, and it keeps its quantity in every aggregate cap until the
		// exchange says otherwise. Releasing the INTENT and keeping the ORDER
		// are the two halves of the same reading.
		for _, id := range res.Req.IDs {
			o.r.queue.Drop(id)
		}
		o.r.anom.raise(risk.Anomaly{
			Class: "CANCEL_UNVERIFIED", Sev: risk.SEV2, Ticker: res.Req.Market,
			Text: fmt.Sprintf("the cancel of %d order(s) on %s/%s did not come "+
				"back verified absent (%s); H-FAIL-3 keeps every one of them in "+
				"the risk model and in every aggregate cap until the exchange "+
				"confirms it gone, and §6.5 re-decides the side on the next tick",
				len(res.Req.Orders), res.Req.Market, res.Req.Side,
				res.Sweep.Outcome),
		})
		return
	}

	create := res.Create

	// EVERY create that may have left quantity live enters the aggregate HERE,
	// before anything else reads it, and it stays until a complete orders walk
	// lists the coid back.
	//
	// Both outcomes, not just the unknown one. `CreateRejected` is the single
	// case with nothing live -- `MaxLive` is zero only there -- so the test is
	// on the quantity rather than on the outcome, and an acked order is carried
	// for exactly the same reason an unknown one is: `Portfolio.ReplaceOrders`
	// is wholesale from a complete walk (H-POS-4), so until the next one lands
	// this order is in no aggregate the sizing can see.
	//
	// H-ORD-2 clause 6 is the unknown half: "an UNKNOWN order's maximum
	// possibly-live quantity STAYS in the risk model and in every aggregate cap
	// (H-Q-5b, H-CAP-7) until it is positively resolved."
	if create.MaxLive > 0 {
		o.pending[create.Coid] = pendingOrder{
			side:  res.Req.Side,
			cents: res.Req.Order.PriceCents(),
			qty:   create.MaxLive,
			at:    o.r.ex.Mono(),
			acked: create.Outcome.Exists(),
		}
	}

	switch {
	case create.Outcome.Exists():
		// H-Q-9a: the leg gate opens on the ACK and not on the dispatch. For a
		// cancel-confirm-place this is the terminal leg; for a first placement
		// it discharges the intent.
		for _, id := range res.Req.IDs {
			o.r.queue.AckPlace(id)
		}
		if create.Filled > 0 {
			// §8.2. The ack's own fill count moves `q` now rather than waiting
			// for the fills walk, and `ApplyAck` is what keeps the two from
			// double-counting when the walk catches up.
			fe := o.r.pf.ApplyAck(risk.AckFill{
				OrderID: create.OrderID, Ticker: res.Req.Market,
				Side: res.Req.Side, Filled: create.Filled,
			})
			o.r.anom.raiseAll(fe.Anomalies)
			if fe.Stop {
				o.requestStop("ack_fill", res.Req.Market)
			}
		}
		if !res.Bound && create.OrderID != "" {
			// The binding is `dispatch.go`'s to submit, inline, on the
			// `CreateResult`. If it did not happen the reservation stays
			// unresolved, and while it does every fill on this order DEFERS
			// rather than applying -- past 120 s that is a SEV2
			// `FILL_UNCLASSIFIABLE` per trade, repeated. The `wsx` order-walk
			// rebind will close it one poll later, which is the safety net and
			// not the normal path.
			o.r.anom.raise(risk.Anomaly{
				Class: "BINDING_NOT_SUBMITTED", Sev: risk.SEV1,
				Ticker: res.Req.Market,
				Text: fmt.Sprintf("order %s acked for coid %s but no binding "+
					"was submitted on the dispatch path; our own fills on it "+
					"defer until the next orders walk rebinds it",
					create.OrderID, create.Coid),
			})
		}

	case create.Outcome == rest.CreateRejected:
		// A definite rejection: nothing exists and nothing is at risk. The
		// intents are dropped rather than left to age into a higher class,
		// because the write was made and answered -- re-selecting it would send
		// the same rejected order again.
		for _, id := range res.Req.IDs {
			o.r.queue.Drop(id)
		}

	default:
		// CreateUnknown. The intents are dropped for the same reason a rejection
		// drops them -- the write happened -- but the order may exist, so it
		// stays in `unresolved` above and keeps occupying the aggregate. §7.2's
		// resolution is the next complete orders walk listing the coid.
		for _, id := range res.Req.IDs {
			o.r.queue.Drop(id)
		}
	}

	if create.ReconcileNow() {
		// §7.2 clause 2. There is no way to force the poller off-cadence from
		// here -- it polls on its own timer and on a reconcile token, and a
		// token is a connection generation rather than a request. Recorded so
		// the delay is visible: the resolution is at most one `position_poll_s`
		// away, and the quantity stays in every cap until it arrives.
		o.r.anom.raise(risk.Anomaly{
			Class: "RECONCILE_DEFERRED", Sev: risk.SEV2, Ticker: res.Req.Market,
			Text: fmt.Sprintf("create %s ended %s and §7.2 asks for an "+
				"immediate reconciliation; this build waits for the next "+
				"portfolio poll (at most %v), and the order's maximum "+
				"possibly-live quantity stays in every aggregate cap until then",
				create.Coid, create.Outcome, o.p.PositionPoll),
		})
	}
	o.escalateUnresolved()
}

// escalateUnresolved is §7.2's `unknown_ping_s`.
//
// One anomaly per coid, not one per tick: an order stuck for an hour is one
// condition an operator needs told about once, and repeating it every 250 ms is
// how the alert that matters gets filtered out. The entry itself is NOT dropped
// -- H-ORD-2 clause 6 keeps it in every cap until it is positively resolved, and
// growing quiet about it does not resolve it.
func (o *owner) escalateUnresolved() {
	now := o.r.ex.Mono()
	for coid, u := range o.pending {
		// An ACKED order waiting for its first walk is not an anomaly, it is
		// younger than one `position_poll_s`. Only an order whose existence was
		// never established is worth waking an operator about.
		if u.acked || u.pinged || now-u.at < o.p.UnknownPing {
			continue
		}
		u.pinged = true
		o.pending[coid] = u
		o.r.anom.raise(risk.Anomaly{
			Class: "ORDER_UNKNOWN", Sev: risk.SEV2, Ticker: o.r.cfg.Ticker,
			Text: fmt.Sprintf("coid %s has been unresolved for %v (past "+
				"unknown_ping_s %v): %s contracts may be live on %s and are "+
				"still held against every aggregate cap. Only a complete orders "+
				"walk listing the coid resolves it -- absence is not evidence "+
				"in either direction (H-ORD-2a)", coid,
				(now - u.at).Truncate(time.Second), o.p.UnknownPing,
				u.qty.Wire(), u.side),
		})
	}
}

// requestStop asks the coordinator for a durable global stop. It is the entry
// point every §12 trigger in this file uses, and it never drops a cause.
func (o *owner) requestStop(trigger, market string) {
	o.commitStop(lifecycle.StopCause{
		Trigger: trigger, Market: market, TsMillis: o.r.ex.NowMs(),
	})
}

// commitStop is the ONE place a §12 cause is written to the durable latch, and
// so it is the one place "what happens when the write fails" is implemented.
// Both the event-driven triggers and the per-tick retry come through here.
//
// It goes through `CommitStop` and then `Advance`, in that order and never the
// other way round: H-HALT-4 requires the latch reach disk BEFORE the in-memory
// state changes, and the two calls were split precisely so that neither can be
// induced to do the other's job by a malformed argument.
func (o *owner) commitStop(cause lifecycle.StopCause) {
	if o.stopHeld {
		// A second, different trigger arriving while the first is still not on
		// disk. The HELD cause is the one written; this one is discarded, and
		// discarded silently, because the conditions that reach here are
		// standing ones -- the gate's stop is sticky and re-requests on every
		// tick -- and one anomaly per 250 ms is how the SEV1 that matters gets
		// filtered out.
		cause = o.stopCause
	}

	commit := o.r.ctrl.CommitStop(cause)
	if !commit.Durable || commit.BlockAdding || commit.RetryLatch {
		// I1's answers, HONOURED rather than observed. The disjunction is the
		// conservative reading: any one of the three means this stop is not on
		// disk, and nothing publishable comes of it.
		//
		// The stop is NOT published -- publishing a halt we failed to record is
		// the HR-009 sequence with the harness having been TOLD the write
		// failed. `BlockAdding` takes effect in `evaluate`, which is the one
		// place §5.2 stops adding, and stops nothing else: the adding side is
		// cancelled and confirmed absent, the capped reducer keeps resting, and
		// the poller, the reconciliation and the monitor never paused.
		// `RetryLatch` is why the cause is kept rather than returned from.
		if !o.stopPinged {
			o.stopPinged = true
			o.r.anom.raiseAll(commit.Anomalies)
		}
		o.holdStop(cause)
		return
	}
	o.r.anom.raiseAll(commit.Anomalies)
	o.releaseStop()

	// `Stop` is the ONLY thing this differs from an ordinary tick by, and it is
	// what makes the A9 row say `global_stop` instead of `halt_latch`: it is the
	// input `NextGlobal` reads to tell a cause committed by THIS process from a
	// latch inherited from a previous one.
	in := o.globalFacts()
	in.Stop = true
	o.advance(in)
}

// globalFacts is what this owner can attest about §5.1's inputs, in one place so
// that the stop path and the ordinary tick cannot disagree about them.
//
// Every field was a hardcoded literal at the stop call site before lip-xdq --
// including `State`, which was left at the zero value and therefore said
// STARTING on every §12 trigger. That was survivable only because A14 fired
// first and forced WINDING_DOWN regardless; it meant the machine was being
// handed a state the process had not been in for hours, and any rule that came
// to depend on it would have been reading garbage. `RiskKnown` and
// `TruthReadable` were literal `true` for the same reason and with a sharper
// edge: they gate the drain, and asserting them is `M-L-FALSEFLAT` -- "absence
// of evidence drains".
func (o *owner) globalFacts() quote.GlobalInput {
	known := o.truthKnown()
	return quote.GlobalInput{
		State:         o.global,
		TruthReadable: known,
		Reconciled:    known,
		RiskKnown:     known,
		AnyInventory:  o.anyInventory(),
		AnyLiveOrder:  o.anyLiveOrder(),
	}
}

// holdStop retains a cause whose durable write did not succeed, so that retrying
// it is this loop's OBLIGATION rather than a property of whether the condition
// that produced it happens to recur.
//
// It is also reached from two places that never call `CommitStop` themselves:
// `newOwner`, for the bootstrap's own unreadable latch, and `applySignal`, where
// `SignalController.Handle` did the commit and a signal that arrived once has no
// condition left to recur at all.
func (o *owner) holdStop(cause lifecycle.StopCause) {
	if o.stopHeld {
		return
	}
	o.stopCause, o.stopHeld = cause, true
}

// holdSignal retains an operator signal whose stop is not durable yet, so the
// exit authority it carries can be granted once something is. See `signalCause`.
func (o *owner) holdSignal(cause lifecycle.StopCause) {
	if o.signalHeld {
		return
	}
	o.signalCause, o.signalHeld = cause, true
}

// releaseStop ends the hold, and tells the operator it ended.
//
// The SEV1 that opened it said the harness had decided to stop and could not
// record it. Someone who was told that needs telling that the record now exists,
// because the two facts have opposite operational meanings and only the second
// one licenses the state that is about to be published.
func (o *owner) releaseStop() {
	if !o.stopHeld {
		return
	}
	o.r.anom.raise(risk.Anomaly{
		Class: "LATCH_WRITE_RECOVERED", Sev: risk.SEV2,
		Ticker: o.stopCause.Market,
		Text: fmt.Sprintf("a durable halt latch is now CONFIRMED after %d "+
			"retried tick(s), so the global state it justifies may be "+
			"published. The cause this process held was %q; the latch is "+
			"first-writer-wins and `Ensure` guarantees only that a record -- "+
			"this one OR AN EARLIER INCARNATION'S -- is on disk, so read the "+
			"file for the authoritative reason rather than assuming it is this "+
			"trigger. Adding was blocked for the whole gap and cancelling, "+
			"reducing, polling, reconciling and monitoring never stopped (I1)",
			o.stopRetries, o.stopCause.Trigger),
	})
	o.stopCause, o.stopHeld = lifecycle.StopCause{}, false
	o.stopPinged, o.stopRetries = false, 0
	o.confirmSignalDrain()
}

// confirmSignalDrain grants the exit authority a signal earned but could not be
// given, now that a durable stop exists.
//
// It runs on the RELEASE and not on the signal, because the permit attests
// durability and there was none to attest when the signal arrived. `BeginPlanned`
// upgrades the unplanned drain `onSignal` already started rather than restarting
// its escalation clock, which is what `DrainTracker.start` documents for an
// operator who signals a harness already winding down from another cause.
func (o *owner) confirmSignalDrain() {
	if !o.signalHeld {
		return
	}
	cause := o.signalCause
	o.signalCause, o.signalHeld = lifecycle.StopCause{}, false
	o.sd.confirmSignalDrain(cause)
}

// retryStop is `RetryLatch`, honoured: the held cause is written again on every
// owner tick until it is durable.
//
// It is driven from `evaluate`, which runs on EVERY iteration of the run loop --
// the 250 ms tick and every event alike -- and that is the whole point. A failed
// latch write is a transient disk condition far more often than a permanent one,
// and the cost of retrying is nothing.
func (o *owner) retryStop() {
	if !o.stopHeld {
		return
	}
	o.stopRetries++
	o.commitStop(o.stopCause)
}

// advance is the one FUNCTION in this process that moves the global state, and
// that is H-HALT-2's property rather than a per-tick budget: "there is exactly
// one place in the codebase that transitions the global state... Both write a
// `state_event` row with the trigger. A grep for those two functions enumerates
// every stop path in the system."
//
// It is called more than once on some ticks, and that is worth writing down
// rather than glossing: the stop funnel calls it on the tick a §12 cause becomes
// durable, and `evaluate` calls it on every tick for §5.1's other edges. The
// second call is a no-op by construction -- `NextGlobal` returns `GTNone`
// beside an unchanged state and `mirrorGlobal` refuses to record one -- so
// nothing is published twice.
//
// The comment this replaces claimed it was "the ONE call per tick that can move
// the global state (§3.8)". That was already untrue when it was written --
// `requestStop` has three event-driven callers -- and §3.8 is a section
// `harness-spec.md` does not contain. See lip-4nb for the two other files
// carrying the same dangling citation.
func (o *owner) advance(in quote.GlobalInput) {
	dec := o.r.ctrl.Advance(in)
	o.r.anom.raiseAll(dec.Anomalies)
	if !dec.Committed {
		return
	}
	o.sd.mirrorGlobal(o.global, dec)
	o.global = dec.State
}

func (o *owner) anyInventory() bool {
	for _, q := range o.r.pf.Positions() {
		if q != 0 {
			return true
		}
	}
	return false
}

// anyLiveOrder is H-FAIL-3's definition, and the three terms are not
// interchangeable: RESTING is the confirmed walk, SENDING is the write in
// flight, and UNKNOWN is a create whose outcome was never resolved. DRAINED
// requires all three to be empty, because "an account that is flat but still has
// fillable orders on the book is not drained: it is one ignored cancel away from
// being long again."
func (o *owner) anyLiveOrder() bool {
	return len(o.r.pf.LiveOrders()) > 0 || o.inflight != nil ||
		len(o.pending) > 0
}

// truthKnown is whether all three portfolio reads are inside `truth_max_age_s`.
//
// A drain authorised from stale truth is a drain authorised from the last
// reading before the outage, and H-FAIL-4 exists because that reading looks
// exactly like a good one.
func (o *owner) truthKnown() bool {
	now := o.r.ex.Clock.Now()
	for _, k := range []wsx.Truth{wsx.TruthOrders, wsx.TruthFills, wsx.TruthPositions} {
		if o.r.gate.TruthAge(k, now) > o.p.TruthMaxAge {
			return false
		}
	}
	return true
}

// publish is H-TOP-5: the owner stamps a fresh Seq on every tick, and the
// monitor's whole stall detector is built on that number advancing.
//
// It publishes UNCONDITIONALLY, at the end of every tick, including ticks that
// decided nothing and ticks taken in a halted state. That is the point: I2 makes
// the monitor unstoppable but does not make it truthful, and an owner that
// stopped publishing while continuing to run would leave the monitor
// re-reporting a snapshot that was true once. A5 would pass at every tick while
// real inventory grew unobserved.
func (o *owner) publish(now time.Duration) {
	o.snapSeq++
	ticker := o.r.cfg.Ticker
	book := o.r.book.Book(ticker)

	snap := &risk.Snapshot{
		Seq:       o.snapSeq,
		PubMono:   now,
		PubWallMs: o.r.ex.NowMs(),
		Global:    o.global,
	}

	m := risk.MarketSnap{
		Ticker:         ticker,
		State:          o.market,
		Q:              o.r.pf.Q(ticker),
		Selected:       true,
		BookActionable: o.r.gate.Actionable(ticker, o.r.ex.Clock.Now()),
	}
	if book != nil {
		// Gated: the market's own qualifying walk fails on either side. Reward
		// for a gated interval is zero for every participant, so it is excluded
		// from the uptime denominator rather than counted as our downtime
		// (H-Q-4).
		m.Gated = book.Qualifies() == 0
		for _, side := range []quote.Side{quote.SideYes, quote.SideNo} {
			levels := book.Yes()
			if side == quote.SideNo {
				levels = book.No()
			}
			ours, price, has := o.restingOn(side)
			ext := quote.ExternalBest(levels, ours)
			s := risk.SideSnap{AtRisk: o.atRisk(side)}
			if has {
				s.PriceCents = price
			}
			for _, r := range ours {
				if r.Price == price {
					s.Resting = r.Size
				}
			}
			if ext.Found {
				s.TouchCents = ext.Price
				s.FieldScore = ext.Size
			}
			m.Sides[side] = s
		}
	}
	snap.Markets = []risk.MarketSnap{m}
	o.r.snap.Store(snap)
}

// confidence: high
