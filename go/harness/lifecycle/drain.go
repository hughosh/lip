package lifecycle

import (
	"errors"
	"fmt"
	"os"
	"syscall"
	"time"

	"lip/harness/cfg"
	"lip/harness/quote"
	"lip/harness/risk"
)

// DrainPermit is authority to end the process once it is genuinely drained.
//
// Every field is private and the zero value is invalid, so a permit cannot be
// written down -- only obtained. It is the same argument as `wsx.ReconcileToken`
// and `rest.WalkUnset`: the thing that licenses an irreversible act must not be
// constructible by the code that wants the act performed.
//
// The act here is the most irreversible one the harness has. H-FAIL-1 lists
// exactly two process exits, and the only one this code can cause is "operator
// SIGINT/SIGTERM (which enters `WINDING_DOWN` and keeps running until drained,
// then exits)". A permit is issued only when that signal's stop has been
// DURABLY committed -- because a process that exits on a stop it failed to
// record is a process `launchd KeepAlive` restarts into a clean directory, where
// it resumes adding.
type DrainPermit struct {
	valid    bool
	trigger  string
	tsMillis int64
}

// Valid reports whether this permit was issued rather than declared.
func (p DrainPermit) Valid() bool { return p.valid }

// Trigger is which signal obtained it, for the operator's record.
func (p DrainPermit) Trigger() string { return p.trigger }

// SignalEffects is what one signal produced.
type SignalEffects struct {
	// Recognised is false for a signal this controller does not act on. An
	// unrecognised signal changes NOTHING -- it does not stop, does not drain,
	// and does not block adding.
	Recognised bool
	// Decision is the adjudicated transition, straight from the controller. The
	// signal handler does not transition anything itself (H-HALT-2).
	Decision GlobalDecision
	// Permit is valid ONLY when the signal was recognised AND its stop reached
	// the disk.
	Permit    DrainPermit
	Anomalies []risk.Anomaly
}

// SignalController turns a signal into a DURABLE stop. It does not register for
// signals, and it does not exit.
//
// `os/signal.Notify` lives in `lip-3af`, and so does `os.Exit`. Both are here as
// an INJECTED `Handle(sig)` instead, because a controller that owned the
// registration would only be testable by sending real signals to the test
// binary, and a rule as load-bearing as "SIGTERM does not exit" needs to be
// checkable in an ordinary unit test as well as in the subprocess one.
type SignalController struct {
	ctrl *GlobalController
}

// NewSignalController binds the handler to the choke point.
//
// It takes the controller rather than returning a cause for someone else to
// commit, because "someone else remembers to commit it" is precisely the
// property that cannot be asserted. The handler that grants exit authority and
// the code that writes the latch are the same call.
func NewSignalController(ctrl *GlobalController) (*SignalController, error) {
	if ctrl == nil || !ctrl.booted {
		return nil, errors.New("no bootstrapped global controller: a signal " +
			"handler that can authorise a process exit without a committed " +
			"durable stop is a handler that exits into a supervisor which " +
			"immediately resumes quoting")
	}
	return &SignalController{ctrl: ctrl}, nil
}

// Handle is H-HALT-3.
//
// > SIGTERM does not exit. It sets `WINDING_DOWN`, keeps the process alive, and
// > exits only when every market is flat or closed. An operator who genuinely
// > wants the process gone with inventory open must SIGKILL, which is an
// > explicit and recoverable act (§7.5), not an accident.
//
// This deliberately diverges from `cmd/rig`, where SIGTERM rolls the transaction
// back to reproduce Python's unhandled-signal behaviour. That faithfulness rule
// binds the rig, not the harness: the harness has inventory and the rig does
// not.
//
// SIGINT is treated identically. §12's table gives them one row -- "SIGINT /
// SIGTERM | global | off everywhere | live | full | alive until drained" -- and
// an operator who typed Ctrl-C into a terminal has not consented to abandoning a
// position either.
//
// The permit is issued last and only on `Committed`. If the latch write fails,
// the harness still stops adding and still drains -- it simply may never END,
// which is the correct direction: an undrained process that keeps reducing and
// monitoring is safe, and an exited one that resumes on restart is not.
func (c *SignalController) Handle(sig os.Signal, in quote.GlobalInput,
	wallMillis int64, mono time.Duration) SignalEffects {

	var eff SignalEffects
	name := ""
	switch sig {
	case syscall.SIGTERM:
		name = "sigterm"
	case syscall.SIGINT:
		name = "sigint"
	default:
		// Not ours. Deliberately inert: a controller that stopped the harness on
		// any signal would stop it on SIGWINCH.
		return eff
	}

	eff.Recognised = true
	in.Stop = true
	cause := StopCause{Trigger: name, TsMillis: wallMillis}
	eff.Decision = c.ctrl.Decide(in, cause)
	eff.Anomalies = append(eff.Anomalies, eff.Decision.Anomalies...)
	eff.Anomalies = append(eff.Anomalies, risk.Anomaly{
		Class: "SIGNAL_DRAIN", Sev: risk.SEV2,
		Text: fmt.Sprintf("%s received after %s of uptime with inventory=%v and "+
			"live orders=%v; the process does NOT exit. It stops adding, keeps "+
			"the reducing quote alive, keeps monitoring, and exits only once "+
			"every market is flat and no order of ours rests (H-HALT-3). "+
			"Durable stop committed: %v",
			name, mono, in.AnyInventory, in.AnyLiveOrder, eff.Decision.Committed),
	})

	if eff.Decision.Committed {
		eff.Permit = DrainPermit{valid: true, trigger: name, tsMillis: wallMillis}
	}
	return eff
}

// DrainObservation is what the drain tracker is told each tick.
type DrainObservation struct {
	// TruthKnown is whether the portfolio reads that establish flatness are
	// CURRENT. A drain authorised from stale truth is a drain authorised from
	// the last reading before the outage, and H-FAIL-4 exists because that
	// reading looks exactly like a good one.
	TruthKnown bool
	// AnyInventory is q != 0 anywhere.
	AnyInventory bool
	// AnyLiveOrder is H-FAIL-3's definition: RESTING, SENDING, UNKNOWN, or
	// cancel-requested but not yet exchange-confirmed absent.
	AnyLiveOrder bool
}

// DrainEffects is one drain tick.
type DrainEffects struct {
	// ExitAuthorised is the ONLY thing in this package that permits a process
	// exit, and it is true only when a valid permit was presented, truth is
	// known, inventory is flat, and nothing of ours rests. `lip-3af` performs
	// the exit.
	ExitAuthorised bool
	Anomalies      []risk.Anomaly
}

// DrainTracker is H-HALT-3's escalation, and the shape of it is HR-009's fix.
//
// > **`drain_timeout_h` (12h) escalates; it does not exit.** At the timeout the
// > harness pings `SEV1` and keeps pinging on an escalating cadence -- it does
// > not terminate with `q != 0`.
//
// > Red-team HR-009: the earlier rule permitted exit after 12 hours **with
// > inventory open**. That is I1 and H-FAIL-1 contradicted in the parameter
// > table -- a timer that eventually does the exact thing the whole document
// > forbids. A drain timeout is evidence the operator is needed, not authority
// > to abandon.
//
// So there is no path from the timeout to `ExitAuthorised`. The timeout's only
// output is an anomaly, and the cadence of that anomaly gets FASTER, not
// quieter: an operator who has not responded in twelve hours is an operator the
// alert is failing to reach.
type DrainTracker struct {
	p       cfg.Params
	started bool
	// permit is the issued authority, held rather than a bool. An earlier
	// version took `planned bool`, which meant any caller could grant
	// planned-exit authority with a literal `true` -- including for a stop whose
	// latch write had just failed.
	permit DrainPermit
	begin  time.Duration
	// nextPing is when the next DRAIN_TIMEOUT escalation is due, measured on the
	// monotonic clock.
	nextPing time.Duration
	// interval is the current escalation gap. It halves each time.
	interval time.Duration
}

// NewDrainTracker validates the parameters it paces against.
func NewDrainTracker(p cfg.Params) (*DrainTracker, error) {
	if err := p.Validate(); err != nil {
		return nil, err
	}
	return &DrainTracker{p: p}, nil
}

// BeginPlanned starts an operator-initiated drain, which is the only kind that
// can ever end in a process exit.
//
// It refuses a permit it did not see issued. The zero `DrainPermit` is invalid
// and cannot be made valid from outside this package.
func (d *DrainTracker) BeginPlanned(permit DrainPermit, mono time.Duration) error {
	if !permit.Valid() {
		return errors.New("planned drain refused: the permit is not valid. A " +
			"process exit is authorised only by a signal whose durable stop " +
			"reached the disk, because exiting on a stop that was not recorded " +
			"hands launchd a clean directory to resume quoting from")
	}
	d.start(permit, mono)
	return nil
}

// BeginUnplanned starts the drain that follows every OTHER global stop -- a
// taker fill, `pnl_kill`, a foreign fill, a hard drift.
//
// It escalates identically and can never authorise an exit. §5.1: "DRAINED does
// not exit the process; it idles and keeps monitoring." Exiting on those would
// mean the one moment the harness is safe is also the moment it stops being able
// to tell you anything, and it would hand `launchd KeepAlive` a process to
// restart into a latched state forever.
func (d *DrainTracker) BeginUnplanned(mono time.Duration) {
	d.start(DrainPermit{}, mono)
}

func (d *DrainTracker) start(permit DrainPermit, mono time.Duration) {
	if d.started {
		// A second signal during a drain does not restart the escalation clock.
		// An impatient operator pressing Ctrl-C again would otherwise push the
		// first SEV1 back by another `drain_timeout_h`.
		//
		// It CAN upgrade an unplanned drain to a planned one: an operator who
		// signals a harness already winding down from a taker fill has asked for
		// the process to end once it is safe to.
		if permit.Valid() && !d.permit.Valid() {
			d.permit = permit
		}
		return
	}
	d.started = true
	d.permit = permit
	d.begin = mono
	d.interval = d.p.DrainTimeout
	d.nextPing = mono + d.p.DrainTimeout
}

// Started reports whether a drain is in progress.
func (d *DrainTracker) Started() bool { return d.started }

// Observe is one drain tick.
//
// Exit requires all four of: a valid permit, truth we can currently read, and
// nothing left that can become a position. The last is H-FAIL-3 and §5.1's
// DRAINED rule: "an account that is flat but still has fillable orders on the
// book is not drained: it is one ignored cancel away from being long again."
func (d *DrainTracker) Observe(obs DrainObservation, mono time.Duration) DrainEffects {
	var eff DrainEffects
	if !d.started {
		return eff
	}

	if d.permit.Valid() && obs.TruthKnown && !obs.AnyInventory && !obs.AnyLiveOrder {
		eff.ExitAuthorised = true
		return eff
	}

	// Not drained. Escalate on the cadence, forever, and never authorise
	// anything by doing so.
	for d.nextPing <= mono {
		eff.Anomalies = append(eff.Anomalies, risk.Anomaly{
			Class: "DRAIN_TIMEOUT", Sev: risk.SEV1,
			Text: fmt.Sprintf("the drain has run %s (past drain_timeout_h %s) "+
				"with truth_known=%v inventory=%v live_orders=%v; the harness "+
				"does NOT exit with q != 0 -- it keeps the reducing quote alive, "+
				"keeps monitoring, and escalates on a shortening cadence "+
				"because a drain timeout is evidence the operator is needed, "+
				"not authority to abandon (H-HALT-3, HR-009)",
				mono-d.begin, d.p.DrainTimeout, obs.TruthKnown,
				obs.AnyInventory, obs.AnyLiveOrder),
		})
		d.interval = nextEscalation(d.interval, d.p.DrainTimeout)
		d.nextPing += d.interval
	}
	return eff
}

// nextEscalation halves the gap, with a floor of min(1h, drain_timeout_h).
//
// The floor is min-with-the-timeout rather than a flat hour so that a
// deliberately short `drain_timeout_h` -- which is how the drills and the
// scenario exchange exercise this -- cannot produce a cadence LONGER than the
// timeout that triggered it.
func nextEscalation(current, timeout time.Duration) time.Duration {
	floor := time.Hour
	if timeout < floor {
		floor = timeout
	}
	next := current / 2
	if next < floor {
		next = floor
	}
	return next
}

// confidence: high
