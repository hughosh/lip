package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"sync/atomic"
	"syscall"

	"lip/harness/hstore"
	"lip/harness/lifecycle"
	"lip/harness/quote"
	"lip/harness/risk"
)

// This file is the one place in the process that registers for a signal and the
// one place that can end it.
//
// `lifecycle` says so in its own words -- "`os/signal.Notify` lives in
// `lip-3af`, and so does `os.Exit`" -- and it says it while deliberately not
// doing either, so that H-HALT-3 could be asserted by an ordinary unit test
// instead of only by killing a subprocess. Everything that rule forbids is
// therefore forbidden HERE or nowhere:
//
// > SIGTERM does not exit. It sets `WINDING_DOWN`, keeps the process alive, and
// > exits only when every market is flat or closed. An operator who genuinely
// > wants the process gone with inventory open must SIGKILL, which is an
// > explicit and recoverable act (§7.5), not an accident.
//
// There is exactly one `os.Exit` reachable from this file, it is behind
// `DrainEffects.ExitAuthorised`, and `ExitAuthorised` is true only with a valid
// permit AND current truth AND flat inventory AND nothing of ours resting.
// `drain_timeout_h` reaches none of it: HR-009 removed the twelve-hour exit
// because "a drain timeout is evidence the operator is needed, not authority to
// abandon", so the timeout's only output is a SEV1 on a shortening cadence.
//
// The other two loops here exist because two packages finished with no
// production caller at all. `hstore.RecordGlobalState` had zero (A9's audit
// trail was a constructor and a table and nothing that wrote to it), and
// `hstore.Rejections` had zero outside its own tests (a record the store
// accepted and then lost was, in production, silent).

// permitBuffer is the depth of the hand-off from the result loop to the
// dispatcher.
//
// It is not sized against throughput. With `dispatchWorkers` = 1 and §16's token
// bucket the number of reservations outstanding at any instant is small, so 64
// is not a queue -- it is headroom past which the dispatcher goroutine is
// provably wedged. The send is non-blocking BECAUSE of that: this loop is the
// only thing draining rejections and anomalies, and a result loop parked on a
// channel send is a store failure nobody is told about.
const permitBuffer = 64

// The §13.2 classes this file introduces. Each names a way the audit trail, and
// not the account, is what broke.
const (
	// stateEventUnrecordedClass is A9's mirror failing at submission. The
	// transition still happened and the FILE latch still holds it; what is lost
	// is the row an operator reconstructs the run from.
	stateEventUnrecordedClass = "STATE_EVENT_UNRECORDED"
	// anomalyDroppedClass is `anomalySink` having overflowed. A dropped anomaly
	// that is not reported is the monitor's evidence disappearing at exactly the
	// moment there was too much of it.
	anomalyDroppedClass = "ANOMALY_DROPPED"
	// permitStrandedClass is a committed coid reservation whose permit never
	// reached a dispatcher.
	permitStrandedClass = "DISPATCH_PERMIT_STRANDED"
	// drainRefusedClass is `BeginPlanned` rejecting the permit it was handed.
	drainRefusedClass = "DRAIN_BEGIN_REFUSED"
	// stopRefusedClass is an authorised exit the store would not let happen.
	stopRefusedClass = "ORDERLY_STOP_REFUSED"
)

// shutdown owns the process's stop half: the signal registration, the drain
// tick, A9's mirror, and the loop that drains `hstore`'s results.
//
// There is exactly ONE of these per process, and the id counters are why: an
// `anomaly_id` or an `event_id` is a PRIMARY KEY, and a duplicate is a
// permanentError -- a record the writer refuses forever, which is the record
// being lost by the machinery that exists to stop records being lost. One
// instance plus a run id that is unique per run makes a collision impossible
// rather than unlikely.
type shutdown struct {
	r *rig

	// exit is `os.Exit`, injected.
	//
	// The reason is not symmetry with `exchange`'s no-production-default rule --
	// that rule is about a field which reaches the live account when a caller
	// forgets it, and this one reaches nothing. It is that the failure directions
	// are opposite. A test of "SIGTERM does not exit" that called the real
	// `os.Exit` would take the test binary with it and report nothing; a `nil`
	// here would leave the process ALIVE, which is H-HALT-3's own preferred
	// direction. So this is the one collaborator whose absence is safe.
	exit func(int)

	// permits is the hand-off to `dispatch.go`.
	//
	// A channel and not a callback, because the dispatcher is a goroutine and D3
	// makes it the ONE goroutine entitled to touch the exchange write path. A
	// callback would run the dispatcher's code on this loop's goroutine, which is
	// the single-writer property lost to a convenience.
	permits chan hstore.Result

	// events and anomalies mint the two primary keys. Atomic because `stop` may
	// sweep from the caller's goroutine while the result loop is still running.
	events    atomic.Uint64
	anomalies atomic.Uint64
}

func newShutdown(r *rig) *shutdown {
	return &shutdown{
		r:       r,
		exit:    os.Exit,
		permits: make(chan hstore.Result, permitBuffer),
	}
}

// Permits is the receive end for the dispatcher.
func (s *shutdown) Permits() <-chan hstore.Result { return s.permits }

// ---------------------------------------------------------------------------
// 1. Signals
// ---------------------------------------------------------------------------

// installSignals registers for SIGINT and SIGTERM and returns the channel and
// its deregistration.
//
// Both, and not SIGTERM alone. §12's table gives them one row -- "SIGINT /
// SIGTERM | global | off everywhere | live | full | alive until drained" -- and
// an operator who typed Ctrl-C into a terminal has not consented to abandoning a
// position either. Nothing else is registered: a process that stopped on any
// signal would stop on SIGWINCH.
//
// The channel is buffered because `os/signal` drops a signal it cannot deliver
// without blocking, and the one it would drop is the operator's stop.
func installSignals() (<-chan os.Signal, func()) {
	ch := make(chan os.Signal, 2)
	signal.Notify(ch, syscall.SIGINT, syscall.SIGTERM)
	return ch, func() { signal.Stop(ch) }
}

// onSignal is H-HALT-3's handler. It returns; it does not exit.
//
// `in` is the run loop's `quote.GlobalInput` -- the current global state and the
// current risk flags -- because this file does not own the portfolio and must
// not invent an answer about it. `SignalController.Handle` commits the durable
// stop and then advances, in that order, and the permit it may issue is the only
// authority a later drain tick can present.
//
// Three outcomes, and the middle one is the one worth naming:
//
//   - unrecognised: nothing happens at all;
//   - recognised, latch write FAILED: no permit, so the drain begins UNPLANNED.
//     The harness stops adding and keeps reducing, and it may never end. That is
//     the correct direction -- an undrained process that keeps reducing and
//     monitoring is safe, and one that exits on a stop it failed to record is a
//     process `launchd KeepAlive` restarts into a clean directory, where it
//     resumes adding (HR-009). A later successful `CommitStop` upgrades the
//     drain in place, which is exactly what `DrainTracker.start` is for;
//   - recognised and committed: the permit begins a PLANNED drain, the only kind
//     that can ever end in a process exit.
func (s *shutdown) onSignal(sig os.Signal, in quote.GlobalInput) lifecycle.SignalEffects {
	from := in.State
	mono := s.r.ex.Mono()

	eff := s.r.sigs.Handle(sig, in, s.r.ex.NowMs(), mono)
	s.r.anom.raiseAll(eff.Anomalies)
	if !eff.Recognised {
		return eff
	}

	// A9 before the drain: the transition is what the operator reads afterwards,
	// and recording it is not allowed to depend on what the tracker then does.
	s.mirrorGlobal(from, eff.Decision)

	if !eff.Permit.Valid() {
		s.r.drain.BeginUnplanned(mono)
		return eff
	}
	if err := s.r.drain.BeginPlanned(eff.Permit, mono); err != nil {
		// Unreachable through this path -- `Handle` issues a permit only on
		// `Committed` -- and raised rather than ignored because if it ever does
		// happen the harness has a signal it believes it honoured and a drain that
		// never started, which reads from outside exactly like a healthy wind-down.
		s.r.anom.raise(risk.Anomaly{
			Class: drainRefusedClass, Sev: risk.SEV1,
			Text: fmt.Sprintf("the drain tracker refused the permit issued for "+
				"%s, so no drain is in progress and nothing will ever escalate "+
				"about it: %v", eff.Permit.Trigger(), err),
		})
		// Still begin one. A drain that cannot exit is strictly better than no
		// drain at all, because the escalation is what reaches the operator.
		s.r.drain.BeginUnplanned(mono)
	}
	return eff
}

// confirmSignalDrain upgrades the unplanned drain a failed signal started, once
// its stop has actually been made durable.
//
// The permit is minted by `lifecycle` and never here. `DrainPermit`'s fields are
// private precisely so that "the thing that licenses an irreversible act must
// not be constructible by the code that wants the act performed" -- and a
// process exit is that act. This function can ask for one and can be refused.
func (s *shutdown) confirmSignalDrain(cause lifecycle.StopCause) {
	permit := s.r.sigs.Confirm(cause)
	if !permit.Valid() {
		s.r.anom.raise(risk.Anomaly{
			Class: drainRefusedClass, Sev: risk.SEV1,
			Text: fmt.Sprintf("the durable stop for %q is on disk, but no drain "+
				"permit was issued for it, so this process has a signal it "+
				"honoured and a drain that can never authorise an exit; it will "+
				"wind down, reach flat and then idle rather than finish "+
				"(H-HALT-3)", cause.Trigger),
		})
		return
	}
	if err := s.r.drain.BeginPlanned(permit, s.r.ex.Mono()); err != nil {
		s.r.anom.raise(risk.Anomaly{
			Class: drainRefusedClass, Sev: risk.SEV1,
			Text: fmt.Sprintf("the drain tracker refused the permit re-issued "+
				"for %s after its durable stop landed, so the wind-down cannot "+
				"end: %v", permit.Trigger(), err),
		})
	}
}

// ---------------------------------------------------------------------------
// 2. The drain
// ---------------------------------------------------------------------------

// observeDrain is one drain tick, and the seam the exit decision is made at.
//
// It takes the `DrainObservation` rather than building one. `TruthKnown`,
// `AnyInventory` and `AnyLiveOrder` are answers about the portfolio and the
// order gate, both of which the run loop owns on its own goroutine; a shutdown
// path that reached for them would be reading `risk.Portfolio` from a second
// goroutine, and `harness/quote` and `harness/risk` hold no mutex by design.
// Worse, it would have to invent a value when the read was unavailable -- and
// `DrainObservation.TruthKnown` exists precisely because "a drain authorised
// from stale truth is a drain authorised from the last reading before the
// outage, and H-FAIL-4 exists because that reading looks exactly like a good
// one."
//
// The returned bool is `DrainEffects.ExitAuthorised` and nothing else. It is
// never true from a timeout, never true from a signal alone, and never true from
// an unplanned drain.
func (s *shutdown) observeDrain(obs lifecycle.DrainObservation) bool {
	eff := s.r.drain.Observe(obs, s.r.ex.Mono())
	s.r.anom.raiseAll(eff.Anomalies)
	return eff.ExitAuthorised
}

// drainLoop ticks the tracker from the run loop's observations and performs the
// ONE authorised exit.
//
// An exit that the store refuses is NOT forced. `hstore.Shutdown` returns its
// reason and changes nothing -- the writer is not cancelled and the store is not
// closed -- so the loop raises SEV1 and keeps ticking. The account is already
// flat and unrested by the time this is reachable, so staying up costs nothing
// and tearing down would convert records that are still being retried into
// records that are permanently lost.
//
// A closed observation channel ends the loop WITHOUT exiting, for the same
// reason: the absence of an observation is not an observation.
func (s *shutdown) drainLoop(ctx context.Context, obs <-chan lifecycle.DrainObservation) error {
	for {
		var o lifecycle.DrainObservation
		select {
		case <-ctx.Done():
			return nil
		case got, open := <-obs:
			if !open {
				return nil
			}
			o = got
		}

		if !s.observeDrain(o) {
			continue
		}
		if err := s.stop(ctx); err != nil {
			s.r.anom.raise(risk.Anomaly{
				Class: stopRefusedClass, Sev: risk.SEV1,
				Text: fmt.Sprintf("the drain is complete and the exit is "+
					"authorised, but the orderly stop was refused and NOTHING has "+
					"been torn down: %v. The process stays up, still retrying, "+
					"because forcing past this turns records the store has "+
					"accepted into records that are permanently lost", err),
			})
			continue
		}
		s.exit(0)
		return nil
	}
}

// stop is the orderly stop, in `hstore.Shutdown`'s order.
//
// The final sweep runs FIRST and is not optional. `Store.Close` discards
// `results`, so a terminal outcome published between the last loop pass and the
// teardown is a lost record whose SEV1 is lost with it -- the one class of
// evidence a shutdown is most likely to be about. Sweeping here submits those
// anomalies while the store still accepts submissions, and `close`'s `Drain` is
// then what makes them durable before the writer is stopped.
//
// The refusal is returned unchanged. `rig.close` is deliberate about this and so
// is this wrapper: a store that cannot drain is a store still retrying records it
// has ACCEPTED, and a caller that cannot stop cleanly is told rather than
// discovering afterwards that it stopped destructively.
func (s *shutdown) stop(ctx context.Context) error {
	s.handleResults(s.r.store.TakeResults())
	s.handleAnomalies()
	return s.r.close(ctx)
}

// ---------------------------------------------------------------------------
// 3. A9 — the state_event mirror
// ---------------------------------------------------------------------------

// mirrorGlobal writes A9's row for one §5.1 transition.
//
// `hstore.RecordGlobalState` had no production caller before this. A9 requires
// every transition to write a row carrying its trigger, and until now the
// requirement was satisfied by a constructor existing -- `state.go` says so
// itself: "Claiming A9 here on the strength of a constructor existing is how an
// assertion becomes a comment."
//
// Three properties, in the order they matter:
//
//  1. **A failure here does not block the transition.** The FILE latch is
//     authoritative (H-HALT-4: "The file is authoritative if it and the database
//     disagree, because SQLite may be the thing that failed"), and this row is
//     the audit trail, not the safety mechanism. So the error is forwarded to the
//     anomaly sink and nothing is returned upward. A harness that refused to wind
//     down because it could not write a history row would be a harness that a
//     full disk keeps quoting.
//  2. **Only a Committed decision is recorded.** A refused `Advance` published
//     no transition -- `GlobalDecision.State` is then the INPUT state and the
//     caller "publishes nothing, blocks adding, and retries" -- so recording it
//     would put a transition in the audit trail that the harness never made, and
//     an operator reconstructing why it stopped adding would find a halt that
//     never happened.
//  3. **Only a real change is recorded.** `NextGlobal` returns the same state
//     with `GTNone` to mean nothing happened; `NewGlobalStateEvent` rejects both,
//     and so does the table's CHECK. Filtering here keeps the ordinary
//     no-op tick from producing a submission error on every pass.
func (s *shutdown) mirrorGlobal(from quote.GlobalState, d lifecycle.GlobalDecision) {
	if !d.Committed || from == d.State {
		return
	}
	id := s.nextEventID()
	_, err := s.r.store.RecordGlobalState(s.r.run, id, s.r.ex.NowMs(), from,
		d.State, d.Trigger)
	if err == nil {
		return
	}
	s.r.anom.raise(risk.Anomaly{
		Class: stateEventUnrecordedClass, Sev: risk.SEV1,
		Text: fmt.Sprintf("the global transition %s -> %s (%s) HAPPENED and is "+
			"in force, but its A9 state_event row was refused at submission and "+
			"the run's own history now has a gap where it is: %v",
			from, d.State, d.Trigger, err),
	})
}

// mirrorMarket writes A9's row for one §5.2 transition.
//
// Same three rules as `mirrorGlobal`, with the trigger supplied directly because
// `quote.NextMarket` is called by the run loop rather than adjudicated through a
// controller: there is no durable latch on a market state, so there is nothing
// for a `Committed` flag to mean and the caller passes what the machine returned.
func (s *shutdown) mirrorMarket(ticker string, from, to quote.MarketState,
	trigger quote.MarketTrigger) {

	if from == to || trigger == quote.MTNone {
		return
	}
	id := s.nextEventID()
	_, err := s.r.store.RecordMarketState(s.r.run, id, s.r.ex.NowMs(), ticker,
		from, to, trigger)
	if err == nil {
		return
	}
	s.r.anom.raise(risk.Anomaly{
		Class: stateEventUnrecordedClass, Sev: risk.SEV1, Ticker: ticker,
		Text: fmt.Sprintf("the %s transition %s -> %s (%s) HAPPENED and is in "+
			"force, but its A9 state_event row was refused at submission and the "+
			"run's own history now has a gap where it is: %v",
			ticker, from, to, trigger, err),
	})
}

// nextEventID and nextAnomalyID mint the two primary keys.
//
// The run id prefix is what makes them unique across restarts sharing one
// database, and `rest.ValidRunID` forbids "-" inside a run id, so the separator
// cannot be ambiguous. A counter and not randomness: the ids are greppable
// against the run, and there is no failure path to handle at the point of use.
func (s *shutdown) nextEventID() string {
	return s.r.runID + "-e" + strconv.FormatUint(s.events.Add(1), 10)
}

func (s *shutdown) nextAnomalyID() string {
	return s.r.runID + "-a" + strconv.FormatUint(s.anomalies.Add(1), 10)
}

// ---------------------------------------------------------------------------
// 4. The result loop
// ---------------------------------------------------------------------------

// runResults drains `TakeResults` and the anomaly sink until the context ends.
//
// It starts from `rig.deferred`, which is every result that arrived while
// construction was waiting for the run handle. Those are carried rather than
// dropped for one reason: a terminal result is a record that is GONE, and
// dropping one because it arrived early would lose exactly the evidence that
// says the store was already failing before the harness started.
//
// The select waits on BOTH wakeups. `Store.Wake()` is capacity one and says only
// "there is something to take", and the anomaly sink is a separate producer that
// no store activity is obliged to follow -- a monitor raising SEV1s at 1 Hz
// against a completely idle store would otherwise sit unwritten until the next
// submission happened to wake this loop.
func (s *shutdown) runResults(ctx context.Context) error {
	deferred := s.r.deferred
	s.r.deferred = nil
	s.handleResults(deferred)

	for {
		s.handleResults(s.r.store.TakeResults())
		s.handleAnomalies()

		select {
		case <-ctx.Done():
			// One last pass. Anything published or raised between the pass above
			// and the cancellation is still evidence, and this is the last moment
			// the store will accept it.
			s.handleResults(s.r.store.TakeResults())
			s.handleAnomalies()
			return nil
		case <-s.r.store.Wake():
		case a := <-s.r.anom.ch:
			s.submitAnomaly(a)
		}
	}
}

// handleResults is `hstore.Rejections` given its first production caller, plus
// the dispatcher hand-off.
//
// The SEV1 is derived from `Result.Err` and NEVER from `Health()`, and
// `reject.go` gives the whole argument: a sticky `fault` is one string for the
// whole store, overwritten by each rejection, so two lost records read as one
// condition naming whichever happened last; it has no EDGE, so an alert built on
// it either fires once forever or repeats on every poll; and it misses the
// writer-exit path entirely, where records are terminally failed with no
// permanent fault latched at all. `Err != nil` is terminal by construction --
// the writer publishes an error only after it has stopped retrying -- so it
// occurs exactly once per record that was lost, with that record's identity.
//
// Submitting the anomaly to the store that just lost a record is not circular.
// H-STORE-3 revokes ADDING and nothing else, `RecordAnomaly` is accepted while
// the store is unhealthy on purpose, and §13.1's text journal is a second durable
// copy written even when SQLite is the thing that failed.
func (s *shutdown) handleResults(batch []hstore.Result) {
	if len(batch) == 0 {
		return
	}
	for _, a := range hstore.Rejections(batch) {
		s.submitAnomaly(a)
	}
	for _, res := range batch {
		// EVERY reservation outcome is forwarded, not only the ones that issued
		// a permit. A reservation that FAILED produces no permit at all, and a
		// dispatcher told only about successes would sit on its receipt until
		// its context expired -- on the one path where the answer is already
		// known. `dispatch.go` matches on `Receipt.Seq()`, so it takes what is
		// its own and ignores the rest.
		if res.Kind != hstore.KindReserveOrder {
			continue
		}
		select {
		case s.permits <- res:
		default:
			// The dispatcher is not consuming. Dropping the outcome leaves the
			// coid reserved and unresolved forever, and `Ownership` defers on an
			// unresolved reservation -- so every order id we do not recognise
			// stops being evidence of a third party for the life of the process.
			s.submitAnomaly(risk.Anomaly{
				Class: permitStrandedClass, Sev: risk.SEV1,
				Text: fmt.Sprintf("the reservation outcome for record #%d could "+
					"not be handed to the dispatcher (%d already queued), so a "+
					"durably reserved order will never be sent and its "+
					"reservation will never resolve; §7.5 defers on an "+
					"unresolved reservation, so foreign-order detection is "+
					"degraded until this run ends",
					res.Receipt.Seq(), permitBuffer),
			})
		}
	}
}

// handleAnomalies drains the sink and reports what it could not hold.
//
// The drop report is synthesised HERE and submitted directly rather than raised,
// because raising it into a sink that is by definition full is how the report
// about dropping becomes a drop. `anomalySink` counts every one it could not
// take -- "the drop is COUNTED and reported rather than silent" -- and this is
// the code that makes that sentence true.
func (s *shutdown) handleAnomalies() {
	for {
		select {
		case a := <-s.r.anom.ch:
			s.submitAnomaly(a)
		default:
			if n := s.r.anom.takeDropped(); n > 0 {
				s.submitAnomaly(risk.Anomaly{
					Class: anomalyDroppedClass, Sev: risk.SEV1,
					Text: fmt.Sprintf("%d anomal(ies) were raised and could not be "+
						"taken: the %d-slot hand-off between the monitor, the poll "+
						"loop and this writer was full, or the record was malformed. "+
						"The buffer drops rather than blocking because a monitor that "+
						"could block on a disk is a monitor that a disk can stop "+
						"(I2, H-STORE-2), and a burst that overflowed is visible here "+
						"in the same journal as the anomalies that caused it",
						n, anomalyBuffer),
				})
			}
			return
		}
	}
}

// submitAnomaly mints the id and queues one §13.1 record.
//
// A submission failure goes to stderr and NOT back into the sink. The sink is
// drained by this function's own caller, so raising here would re-deliver the
// same failing anomaly on the next pass, fail again, and spin -- an unbounded
// loop produced by the error handling of the error path. Stderr is what
// `launchd` captures, and it is the only channel left once the store refuses
// records at all.
func (s *shutdown) submitAnomaly(a risk.Anomaly) {
	id := s.nextAnomalyID()
	if _, err := s.r.store.RecordAnomaly(s.r.run, id, a, s.r.ex.NowMs()); err != nil {
		ticker := a.Ticker
		if ticker == "" {
			ticker = "account"
		}
		fmt.Fprintf(os.Stderr, "harness: anomaly %s (%s, sev%d, %s) could not be "+
			"submitted to either journal and is now only in this line: %s (%v)\n",
			id, a.Class, a.Sev, ticker, a.Text, err)
	}
}

// confidence: high
