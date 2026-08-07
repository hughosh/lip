package lifecycle

import (
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"testing"
	"time"

	"lip/harness/quote"
	"lip/harness/risk"
)

// sigtermChildEnv marks the re-executed child of the subprocess test.
const sigtermChildEnv = "LIP_LIFECYCLE_SIGTERM_CHILD"

// aliveMarker is the continuation marker. Its ABSENCE is what catches `M3`.
const aliveMarker = "ALIVE_AFTER_SIGTERM_WITH_INVENTORY"

// TestSIGTERMWithInventoryOutlivesSignalAndDrainTimeout is H-HALT-3, tested the
// only way an "it does not exit" rule can honestly be tested.
//
// > SIGTERM does not exit. It sets `WINDING_DOWN`, keeps the process alive, and
// > exits only when every market is flat or closed.
//
// `M3` is "`os.Exit` on SIGTERM instead of draining", and it is invisible to an
// in-process assertion: a mutation that exits the process cannot be observed by
// a check that runs after it. So this re-executes the test binary, delivers a
// REAL SIGTERM to the child, and asserts on a marker the child prints AFTERWARDS.
// Under the mutation the child dies inside the handler, prints nothing, and the
// parent fails on the missing marker rather than on an exit code -- `os.Exit(0)`
// exits zero and would otherwise read as success.
//
// The child also drives the drain tracker far past `drain_timeout_h` with
// inventory open, which is HR-009's rule:
//
// > Red-team HR-009: the earlier rule permitted exit after 12 hours **with
// > inventory open**. That is I1 and H-FAIL-1 contradicted in the parameter
// > table -- a timer that eventually does the exact thing the whole document
// > forbids. A drain timeout is evidence the operator is needed, not authority
// > to abandon.
func TestSIGTERMWithInventoryOutlivesSignalAndDrainTimeout(t *testing.T) {
	if os.Getenv(sigtermChildEnv) == "1" {
		sigtermChild()
		return
	}

	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	cmd := exec.Command(exe,
		"-test.run=^TestSIGTERMWithInventoryOutlivesSignalAndDrainTimeout$",
		"-test.v")
	cmd.Env = append(os.Environ(), sigtermChildEnv+"=1")
	out, err := cmd.CombinedOutput()
	text := string(out)

	if err != nil {
		t.Fatalf("the child did not survive its own SIGTERM: %v\n%s", err, text)
	}
	if !strings.Contains(text, aliveMarker) {
		t.Fatalf("the child did not reach the continuation marker %q after "+
			"SIGTERM. H-HALT-3 makes SIGTERM non-terminal: it sets "+
			"WINDING_DOWN, keeps the process alive, and exits only when every "+
			"market is flat or closed.\n%s", aliveMarker, text)
	}
	for _, bad := range []string{"EXIT_AUTHORISED_WITH_INVENTORY",
		"NO_ESCALATION", "BAD_EFFECTS", "SIGNAL_NOT_DELIVERED", "NO_PERMIT"} {
		if strings.Contains(text, bad) {
			t.Fatalf("child reported %s\n%s", bad, text)
		}
	}
}

// sigtermChild runs in the re-executed binary. It sends itself a real SIGTERM,
// handles it, and then keeps going.
func sigtermChild() {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGTERM)
	defer signal.Stop(ch)

	if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
		fmt.Println("SIGNAL_NOT_DELIVERED", err)
		return
	}

	var sig os.Signal
	select {
	case sig = <-ch:
	case <-time.After(10 * time.Second):
		fmt.Println("SIGNAL_NOT_DELIVERED timeout")
		return
	}

	store := &recordingLatch{}
	ctrl, _, err := NewGlobalController(store)
	if err != nil {
		fmt.Println("BAD_EFFECTS", err)
		return
	}
	sc, err := NewSignalController(ctrl)
	if err != nil {
		fmt.Println("BAD_EFFECTS", err)
		return
	}

	in := quote.GlobalInput{
		State: quote.Running, AnyInventory: true, AnyLiveOrder: true,
	}
	eff := sc.Handle(sig, in, 1_700_000_000_000, 5*time.Minute)
	if !eff.Recognised || !eff.Decision.Committed ||
		eff.Decision.State != quote.WindingDown {
		fmt.Printf("BAD_EFFECTS %+v\n", eff)
		return
	}
	if !eff.Permit.Valid() || eff.Permit.Trigger() != "sigterm" {
		fmt.Println("NO_PERMIT")
		return
	}

	p := testParams()
	d, err := NewDrainTracker(p)
	if err != nil {
		fmt.Println("BAD_EFFECTS", err)
		return
	}
	if err := d.BeginPlanned(eff.Permit, 0); err != nil {
		fmt.Println("NO_PERMIT", err)
		return
	}

	// Far past drain_timeout_h, still holding inventory.
	de := d.Observe(DrainObservation{
		TruthKnown: true, AnyInventory: true, AnyLiveOrder: true,
	}, 100*p.DrainTimeout)
	if de.ExitAuthorised {
		fmt.Println("EXIT_AUTHORISED_WITH_INVENTORY")
		return
	}
	if !hasClass(de.Anomalies, "DRAIN_TIMEOUT") {
		fmt.Println("NO_ESCALATION")
		return
	}

	fmt.Println(aliveMarker)
}

// TestSignalCannotAuthoriseExitUntilLatchIsDurable is R4's whole point.
//
// The permit is not a bool a caller sets; it is issued by the handler and only
// when `Decide` reported the stop COMMITTED. A process that exits on a stop it
// failed to record is a process `launchd KeepAlive` restarts into a clean
// directory, where it resumes adding.
//
// `M-L-PLANFORGE` grants the permit before latch commitment.
func TestSignalCannotAuthoriseExitUntilLatchIsDurable(t *testing.T) {
	drained := DrainObservation{TruthKnown: true}

	t.Run("latch write fails", func(t *testing.T) {
		store := &recordingLatch{writeErr: os.ErrPermission}
		permit, dec := grantedPermit(t, store)

		if dec.Committed {
			t.Fatal("a failed latch write reported the stop committed")
		}
		if permit.Valid() {
			t.Fatal("a permit was issued for a stop that never reached disk")
		}

		d, err := NewDrainTracker(testParams())
		if err != nil {
			t.Fatalf("NewDrainTracker: %v", err)
		}
		if err := d.BeginPlanned(permit, 0); err == nil {
			t.Fatal("BeginPlanned accepted an invalid permit")
		}
		// It never began, so nothing can be authorised.
		if eff := d.Observe(drained, time.Hour); eff.ExitAuthorised {
			t.Fatal("exit was authorised from a refused permit")
		}
	})

	t.Run("permit cannot be declared", func(t *testing.T) {
		d, err := NewDrainTracker(testParams())
		if err != nil {
			t.Fatalf("NewDrainTracker: %v", err)
		}
		// The zero value is the only DrainPermit an external caller can write
		// down, and it is invalid.
		if err := d.BeginPlanned(DrainPermit{}, 0); err == nil {
			t.Fatal("BeginPlanned accepted the zero permit")
		}
	})

	t.Run("committed stop issues a usable permit", func(t *testing.T) {
		permit, dec := grantedPermit(t, &recordingLatch{})
		if !dec.Committed || !permit.Valid() {
			t.Fatalf("a clean latch write produced committed=%v valid=%v",
				dec.Committed, permit.Valid())
		}
		d, err := NewDrainTracker(testParams())
		if err != nil {
			t.Fatalf("NewDrainTracker: %v", err)
		}
		if err := d.BeginPlanned(permit, 0); err != nil {
			t.Fatalf("BeginPlanned: %v", err)
		}
		if eff := d.Observe(drained, time.Second); !eff.ExitAuthorised {
			t.Fatal("a genuinely drained, genuinely permitted process was not " +
				"authorised to exit")
		}
	})
}

// TestDrainAuthorisesExitOnlyWhenFlatKnownAndUnrested is the positive half.
//
// Exit requires all of: an issued permit, truth we can currently read, and
// nothing left that can become a position. The last is §5.1's DRAINED rule --
// "an account that is flat but still has fillable orders on the book is not
// drained: it is one ignored cancel away from being long again".
//
// `M-L-DRAINEXIT` authorises exit when the timeout expires with inventory.
func TestDrainAuthorisesExitOnlyWhenFlatKnownAndUnrested(t *testing.T) {
	p := testParams()
	cases := []struct {
		name    string
		planned bool
		obs     DrainObservation
		want    bool
	}{
		{"flat, known, unrested, permitted", true,
			DrainObservation{TruthKnown: true}, true},
		{"inventory open", true,
			DrainObservation{TruthKnown: true, AnyInventory: true}, false},
		{"live order resting", true,
			DrainObservation{TruthKnown: true, AnyLiveOrder: true}, false},
		{"truth unreadable", true,
			DrainObservation{}, false},
		{"unplanned stop, otherwise drained", false,
			DrainObservation{TruthKnown: true}, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d, err := NewDrainTracker(p)
			if err != nil {
				t.Fatalf("NewDrainTracker: %v", err)
			}
			if tc.planned {
				permit, _ := grantedPermit(t, &recordingLatch{})
				if err := d.BeginPlanned(permit, 0); err != nil {
					t.Fatalf("BeginPlanned: %v", err)
				}
			} else {
				d.BeginUnplanned(0)
			}

			// Immediately, and then far past the timeout. Neither may change the
			// answer in the direction of exiting.
			for _, mono := range []time.Duration{0, 100 * p.DrainTimeout} {
				got := d.Observe(tc.obs, mono)
				if got.ExitAuthorised != tc.want {
					t.Fatalf("at mono=%v ExitAuthorised = %v, want %v; a drain "+
						"timeout is evidence the operator is needed, not "+
						"authority to abandon (HR-009)",
						mono, got.ExitAuthorised, tc.want)
				}
			}
		})
	}
}

// TestDrainEscalationShortensAndNeverStops is the cadence.
//
// > At the timeout the harness pings `SEV1` and keeps pinging on an escalating
// > cadence -- it does not terminate with `q != 0`.
//
// Escalating means the gap gets SHORTER. An operator who has not responded in
// twelve hours is an operator the alert is failing to reach, and a constant or
// lengthening cadence is the alarm quietly agreeing to be ignored.
func TestDrainEscalationShortensAndNeverStops(t *testing.T) {
	p := testParams()
	d, err := NewDrainTracker(p)
	if err != nil {
		t.Fatalf("NewDrainTracker: %v", err)
	}
	permit, _ := grantedPermit(t, &recordingLatch{})
	if err := d.BeginPlanned(permit, 0); err != nil {
		t.Fatalf("BeginPlanned: %v", err)
	}
	obs := DrainObservation{TruthKnown: true, AnyInventory: true}

	// Nothing before the timeout.
	if eff := d.Observe(obs, p.DrainTimeout-time.Second); len(eff.Anomalies) != 0 {
		t.Fatalf("escalated before drain_timeout_h: %v", classesOf(eff.Anomalies))
	}

	// First ping AT the timeout, and it is SEV1.
	first := d.Observe(obs, p.DrainTimeout)
	if len(first.Anomalies) != 1 {
		t.Fatalf("at the timeout: %d anomalies, want 1", len(first.Anomalies))
	}
	if sev, _ := sevOf(first.Anomalies, "DRAIN_TIMEOUT"); sev != risk.SEV1 {
		t.Fatalf("DRAIN_TIMEOUT severity %v, want SEV1", sev)
	}

	// The gaps thereafter halve, with a floor of one hour, and they keep
	// arriving for as long as the risk does.
	var fireTimes []time.Duration
	for mono := p.DrainTimeout; mono <= p.DrainTimeout+40*time.Hour; mono += time.Minute {
		if eff := d.Observe(obs, mono); len(eff.Anomalies) > 0 {
			for range eff.Anomalies {
				fireTimes = append(fireTimes, mono)
			}
		}
	}
	if len(fireTimes) < 4 {
		t.Fatalf("only %d further escalations in 40h; the cadence stopped",
			len(fireTimes))
	}
	prevGap := time.Duration(0)
	for i := 1; i < len(fireTimes); i++ {
		gap := fireTimes[i] - fireTimes[i-1]
		if prevGap != 0 && gap > prevGap {
			t.Fatalf("escalation gap grew from %v to %v; the cadence must "+
				"shorten, not lengthen", prevGap, gap)
		}
		if gap < time.Hour-time.Minute {
			t.Fatalf("escalation gap %v fell below the one-hour floor", gap)
		}
		prevGap = gap
	}
}

// TestUnplannedDrainEscalatesForeverAndNeverExits is §5.1's DRAINED rule.
//
// A taker fill, `pnl_kill`, a foreign fill or a hard drift all enter
// WINDING_DOWN and then DRAINED and STAY there. Exiting on those would mean the
// one moment the harness is safe is also the moment it stops being able to tell
// you anything.
func TestUnplannedDrainEscalatesForeverAndNeverExits(t *testing.T) {
	p := testParams()
	d, err := NewDrainTracker(p)
	if err != nil {
		t.Fatalf("NewDrainTracker: %v", err)
	}
	d.BeginUnplanned(0)

	// Completely drained, and still not authorised.
	if eff := d.Observe(DrainObservation{TruthKnown: true}, p.DrainTimeout); eff.ExitAuthorised {
		t.Fatal("a non-signal global stop authorised a process exit")
	}
	// And it still escalates while risk remains.
	eff := d.Observe(DrainObservation{TruthKnown: true, AnyInventory: true},
		3*p.DrainTimeout)
	if !hasClass(eff.Anomalies, "DRAIN_TIMEOUT") {
		t.Fatalf("an unplanned drain stopped escalating: %v",
			classesOf(eff.Anomalies))
	}
}

// TestUnrecognisedSignalIsInert keeps the handler from stopping the harness on
// a SIGWINCH.
func TestUnrecognisedSignalIsInert(t *testing.T) {
	store := &recordingLatch{}
	ctrl, _, err := NewGlobalController(store)
	if err != nil {
		t.Fatalf("NewGlobalController: %v", err)
	}
	sc, err := NewSignalController(ctrl)
	if err != nil {
		t.Fatalf("NewSignalController: %v", err)
	}
	eff := sc.Handle(syscall.SIGWINCH, quote.GlobalInput{State: quote.Running},
		1, time.Second)
	if eff.Recognised || eff.Permit.Valid() || len(eff.Anomalies) != 0 {
		t.Fatalf("SIGWINCH produced %+v, want an inert result", eff)
	}
	if len(store.ensures) != 0 {
		t.Fatalf("SIGWINCH wrote a latch: %+v", store.ensures)
	}
}

// TestSIGINTIsTreatedAsSIGTERM is §12's table, which gives them one row.
//
// An operator who typed Ctrl-C into a terminal has not consented to abandoning a
// position either.
func TestSIGINTIsTreatedAsSIGTERM(t *testing.T) {
	store := &recordingLatch{}
	ctrl, _, err := NewGlobalController(store)
	if err != nil {
		t.Fatalf("NewGlobalController: %v", err)
	}
	sc, err := NewSignalController(ctrl)
	if err != nil {
		t.Fatalf("NewSignalController: %v", err)
	}
	in := quote.GlobalInput{State: quote.Running, AnyInventory: true}
	eff := sc.Handle(syscall.SIGINT, in, 42, time.Minute)
	if !eff.Recognised || !eff.Permit.Valid() {
		t.Fatalf("SIGINT produced %+v", eff)
	}
	if eff.Decision.State != quote.WindingDown {
		t.Fatalf("SIGINT produced state %v", eff.Decision.State)
	}
	if len(store.ensures) != 1 || store.ensures[0].Trigger != "sigint" ||
		store.ensures[0].TsMillis != 42 {
		t.Fatalf("SIGINT latched %+v", store.ensures)
	}
}

// TestSecondSignalDoesNotRestartTheEscalationClock keeps an impatient operator
// from pushing the first SEV1 back by another drain_timeout_h.
func TestSecondSignalDoesNotRestartTheEscalationClock(t *testing.T) {
	p := testParams()
	d, err := NewDrainTracker(p)
	if err != nil {
		t.Fatalf("NewDrainTracker: %v", err)
	}
	permit, _ := grantedPermit(t, &recordingLatch{})
	if err := d.BeginPlanned(permit, 0); err != nil {
		t.Fatalf("BeginPlanned: %v", err)
	}
	if err := d.BeginPlanned(permit, p.DrainTimeout/2); err != nil {
		t.Fatalf("second BeginPlanned: %v", err)
	}

	obs := DrainObservation{TruthKnown: true, AnyInventory: true}
	if eff := d.Observe(obs, p.DrainTimeout); len(eff.Anomalies) == 0 {
		t.Fatal("a second Ctrl-C pushed the first SEV1 past drain_timeout_h")
	}
}

// TestSignalDuringAnUnplannedDrainCanStillAuthoriseTheExit covers the operator
// who signals a harness already winding down from a taker fill.
//
// The drain is already running and its clock must not restart, but the operator
// HAS now asked for the process to end once it is safe to.
func TestSignalDuringAnUnplannedDrainCanStillAuthoriseTheExit(t *testing.T) {
	d, err := NewDrainTracker(testParams())
	if err != nil {
		t.Fatalf("NewDrainTracker: %v", err)
	}
	d.BeginUnplanned(0)

	permit, _ := grantedPermit(t, &recordingLatch{})
	if err := d.BeginPlanned(permit, time.Minute); err != nil {
		t.Fatalf("BeginPlanned: %v", err)
	}
	eff := d.Observe(DrainObservation{TruthKnown: true}, 2*time.Minute)
	if !eff.ExitAuthorised {
		t.Fatal("an operator signal during an unplanned drain did not grant " +
			"exit authority once the account was genuinely drained")
	}
}
