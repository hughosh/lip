package lifecycle

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"lip/harness/quote"
	"lip/harness/risk"
)

// TestMalformedOrUnreadableLatchFailsClosed is H-HALT-4's asymmetry.
//
// Exactly one condition means "no halt is latched": the file does not exist.
// Every other condition -- unreadable, empty, truncated, null, wrong version, no
// trigger, unknown fields -- means latched, because the question the file answers
// is "did a previous incarnation stop?" and a file we cannot read does not
// answer it.
//
// `M-L-LATCHFAIL` treats a malformed or unreadable latch as clear. Under it, the
// HR-009 sequence resumes: a taker fill latches WINDING_DOWN, a panic kills the
// process mid-write, launchd restarts it, the short file reads as clear, and
// flat markets resume adding with no operator ever having seen the halt.
func TestMalformedOrUnreadableLatchFailsClosed(t *testing.T) {
	cases := []struct {
		name    string
		content string
		// present is what Load must report.
		present bool
		wantErr bool
	}{
		{"absent", "", false, false},
		{"empty file", "", true, true},
		{"truncated json", `{"version":1,"trig`, true, true},
		{"json null", `null`, true, true},
		{"empty object", `{}`, true, true},
		{"wrong version", `{"version":9,"trigger":"taker_fill","ts_ms":1}`, true, true},
		{"no trigger", `{"version":1,"trigger":"","ts_ms":1}`, true, true},
		{"zero timestamp", `{"version":1,"trigger":"taker_fill","ts_ms":0}`, true, true},
		{"unknown field", `{"version":1,"trigger":"t","ts_ms":1,"x":2}`, true, true},
		{"valid", `{"version":1,"trigger":"taker_fill","ts_ms":17,"market":"M"}`, true, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "harness.halt")
			if tc.name != "absent" {
				if err := os.WriteFile(path, []byte(tc.content), 0o600); err != nil {
					t.Fatalf("seed: %v", err)
				}
			}
			l, err := NewFileLatch(path)
			if err != nil {
				t.Fatalf("NewFileLatch: %v", err)
			}
			_, present, err := l.Load()
			if present != tc.present {
				t.Fatalf("present = %v, want %v", present, tc.present)
			}
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}

			// The controller's verdict is what actually matters: anything other
			// than a clean absence must bootstrap LATCHED and block adding.
			ctrl, eff, cerr := NewGlobalController(l)
			if cerr != nil {
				t.Fatalf("NewGlobalController: %v", cerr)
			}
			wantLatched := tc.present
			if eff.Latched != wantLatched {
				t.Fatalf("bootstrap Latched = %v, want %v", eff.Latched, wantLatched)
			}
			if !ctrl.Latched() && wantLatched {
				t.Fatal("controller does not report itself latched")
			}
			if tc.wantErr {
				if !eff.BlockAdding || !eff.RetryLatch {
					t.Fatalf("an uninterpretable latch did not block adding "+
						"(BlockAdding=%v RetryLatch=%v)",
						eff.BlockAdding, eff.RetryLatch)
				}
				if !hasClass(eff.Anomalies, "LATCH_INVALID") {
					t.Fatalf("no LATCH_INVALID; got %v", classesOf(eff.Anomalies))
				}
				if sev, _ := sevOf(eff.Anomalies, "LATCH_INVALID"); sev != risk.SEV1 {
					t.Fatalf("LATCH_INVALID severity %v, want SEV1", sev)
				}
			}

			// A latched bootstrap forces WINDING_DOWN out of NextGlobal even
			// from a state that otherwise reconciles into RUNNING (A14).
			dec := ctrl.Decide(quote.GlobalInput{
				State: quote.Starting, TruthReadable: true, Reconciled: true,
			}, StopCause{})
			if wantLatched {
				if dec.State != quote.WindingDown || dec.Trigger != quote.GTLatch {
					t.Fatalf("latched bootstrap produced %v/%v, want "+
						"WINDING_DOWN/GTLatch", dec.State, dec.Trigger)
				}
			} else if dec.State != quote.Running {
				t.Fatalf("clear bootstrap produced %v, want RUNNING", dec.State)
			}
		})
	}
}

// TestLatchNeverOverwritesFirstCauseAndHasNoClearAPI is the other half of
// H-HALT-4: "the harness never self-clears it. Clearing is the operator action
// of §10.4."
//
// Two properties, and they fail differently. Overwriting loses the reason: a
// SIGTERM sent while winding down from a taker fill would replace the most
// serious stop condition in the system with the most mundane one, and the
// operator investigating at 02:00 would find "sigterm". A clear method loses the
// halt entirely, and it would be reachable from any code path that believed it
// had a good reason.
func TestLatchNeverOverwritesFirstCauseAndHasNoClearAPI(t *testing.T) {
	l := tempLatch(t)

	first := LatchRecord{Version: 1, Trigger: "taker_fill", TsMillis: 100, Market: "A"}
	if durable, err := l.Ensure(first); err != nil || !durable {
		t.Fatalf("first Ensure: durable=%v err=%v", durable, err)
	}

	second := LatchRecord{Version: 1, Trigger: "sigterm", TsMillis: 900, Market: "B"}
	if durable, err := l.Ensure(second); err != nil || !durable {
		t.Fatalf("second Ensure: durable=%v err=%v", durable, err)
	}

	got, present, err := l.Load()
	if err != nil || !present {
		t.Fatalf("Load after two Ensures: present=%v err=%v", present, err)
	}
	if got != first {
		t.Fatalf("the latch now reads %+v; the FIRST durable cause must win, "+
			"or a mundane later trigger erases the reason the harness stopped",
			got)
	}

	// Mode is 0600: the latch is a safety record, not something a stray process
	// gets to rewrite.
	fi, err := os.Stat(l.Path())
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("latch mode %v, want 0600", fi.Mode().Perm())
	}

	// No exported escape hatch, on the concrete type OR on the interface.
	forbidden := []string{"clear", "remove", "reset", "delete", "unlatch",
		"overwrite", "resume", "settrue", "setfalse", "truncate"}
	for _, typ := range []reflect.Type{
		reflect.TypeOf(FileLatch{}), reflect.TypeOf(&FileLatch{}),
		reflect.TypeOf((*LatchStore)(nil)).Elem(),
	} {
		for i := 0; i < typ.NumMethod(); i++ {
			name := strings.ToLower(typ.Method(i).Name)
			for _, bad := range forbidden {
				if strings.Contains(name, bad) {
					t.Fatalf("%s exports %s: H-HALT-4 says the harness never "+
						"self-clears the latch, and a method that can is that "+
						"sentence deleted", typ, typ.Method(i).Name)
				}
			}
		}
	}
}

// TestGlobalStopIsDurableBeforePublicationAndSurvivesRestart is the
// write-before-transition ordering, and the restart that ordering exists for.
//
// `M20` pretends the Ensure succeeded without writing. Under it the in-memory
// transition still happens, the harness still stops adding, everything looks
// right -- and the next incarnation finds no file and resumes quoting. The only
// thing that catches it is asserting against a SECOND controller built over the
// same disk.
func TestGlobalStopIsDurableBeforePublicationAndSurvivesRestart(t *testing.T) {
	l := tempLatch(t)
	ctrl, boot, err := NewGlobalController(l)
	if err != nil {
		t.Fatalf("NewGlobalController: %v", err)
	}
	if boot.Latched {
		t.Fatal("a fresh directory bootstrapped as latched")
	}

	in := quote.GlobalInput{State: quote.Running}
	cause := StopCause{Trigger: "taker_fill", Market: "M", TsMillis: 4242}
	dec := ctrl.Decide(in, cause)

	if !dec.Committed {
		t.Fatalf("the stop was not committed: %v", classesOf(dec.Anomalies))
	}
	if dec.State != quote.WindingDown {
		t.Fatalf("state %v, want WINDING_DOWN", dec.State)
	}

	// The record is on disk NOW, before anything downstream published the
	// transition.
	rec, present, err := l.Load()
	if err != nil || !present {
		t.Fatalf("the latch is not durable after a committed stop: "+
			"present=%v err=%v", present, err)
	}
	if rec.Trigger != "taker_fill" || rec.Market != "M" || rec.TsMillis != 4242 {
		t.Fatalf("latch record %+v does not carry the trigger, market and "+
			"timestamp H-HALT-4 requires", rec)
	}

	// The restart. A brand new controller over the same path -- this is the
	// process launchd brought back after the panic.
	l2, err := NewFileLatch(l.Path())
	if err != nil {
		t.Fatalf("NewFileLatch: %v", err)
	}
	ctrl2, boot2, err := NewGlobalController(l2)
	if err != nil {
		t.Fatalf("restart NewGlobalController: %v", err)
	}
	if !boot2.Latched {
		t.Fatal("the restarted process did not see the latch: this is HR-009 " +
			"exactly -- the halt self-clears because the supervision policy the " +
			"spec mandates erased the safety state the spec mandates")
	}

	// And it enters WINDING_DOWN directly from STARTING, ahead of the
	// reconciliation that would otherwise produce RUNNING.
	dec2 := ctrl2.Decide(quote.GlobalInput{
		State: quote.Starting, TruthReadable: true, Reconciled: true,
	}, StopCause{})
	if dec2.State != quote.WindingDown || dec2.Trigger != quote.GTLatch {
		t.Fatalf("the restarted process produced %v/%v from a complete "+
			"reconciliation, want WINDING_DOWN/GTLatch",
			dec2.State, dec2.Trigger)
	}
}

// TestLatchWriteFailureBlocksAddingWithoutTransitioningOrStoppingTheExit is I1
// applied to a disk failure.
//
// The response to "we could not write the latch" is to stop ADDING and to stop
// nothing else -- and specifically NOT to take the in-memory transition, because
// a process sitting in WINDING_DOWN believing it latched is a process whose
// restart resumes quoting.
func TestLatchWriteFailureBlocksAddingWithoutTransitioningOrStoppingTheExit(t *testing.T) {
	for _, tc := range []struct {
		name  string
		store *recordingLatch
	}{
		{"write error", &recordingLatch{writeErr: os.ErrPermission}},
		{"reported not durable", &recordingLatch{notDurable: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctrl, _, err := NewGlobalController(tc.store)
			if err != nil {
				t.Fatalf("NewGlobalController: %v", err)
			}
			in := quote.GlobalInput{State: quote.Running}
			dec := ctrl.Decide(in, StopCause{
				Trigger: "pnl_kill", TsMillis: 7,
			})

			if dec.Committed {
				t.Fatal("a failed latch write reported the stop as committed")
			}
			if dec.State != quote.Running {
				t.Fatalf("state %v: the in-memory transition must NOT be taken "+
					"when the latch did not reach disk, or a restart resumes "+
					"quoting from a stop nobody recorded", dec.State)
			}
			if !dec.BlockAdding || !dec.RetryLatch {
				t.Fatalf("BlockAdding=%v RetryLatch=%v, want both true",
					dec.BlockAdding, dec.RetryLatch)
			}
			if !hasClass(dec.Anomalies, "LATCH_WRITE_FAILED") {
				t.Fatalf("no LATCH_WRITE_FAILED; got %v", classesOf(dec.Anomalies))
			}
			if ctrl.Latched() {
				t.Fatal("the controller latched itself on a write that failed")
			}
		})
	}
}

// TestMalformedStopCauseIsNotSilentlyDropped guards the other direction: a
// caller that asks to stop with an unusable cause must not keep quoting.
func TestMalformedStopCauseIsNotSilentlyDropped(t *testing.T) {
	store := &recordingLatch{}
	ctrl, _, err := NewGlobalController(store)
	if err != nil {
		t.Fatalf("NewGlobalController: %v", err)
	}
	dec := ctrl.Decide(quote.GlobalInput{State: quote.Running},
		StopCause{Trigger: "", TsMillis: 5})

	if dec.Committed || !dec.BlockAdding || !dec.RetryLatch {
		t.Fatalf("a stop with no trigger was not treated as an unlatched stop: "+
			"%+v", dec)
	}
	if len(store.ensures) != 0 {
		t.Fatalf("an invalid cause reached the disk: %+v", store.ensures)
	}
}
