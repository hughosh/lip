package lifecycle

import (
	"testing"
	"time"

	"lip/harness/risk"
)

var sleepEpoch = time.Unix(1_700_000_000, 0).UTC()

// TestClockDivergenceForcesResnapshotAndReconcile is F7.
//
// > **Host sleep / process stall** | per tick, compare wall-clock delta against
// > monotonic delta; divergence > 5s | treat the gap as downtime, force
// > resnapshot + `RECONCILE_NOW`, record the interval in `uptime` | `SEV2`
//
// The failure this detects is the one in the operator's own notes: the iMac
// idle-sleeps, and from inside the process nothing happened -- every monotonic
// timer agrees no time passed, so the book's staleness clock, the truth-age
// check and the reconnect backoff all read as fresh across a gap of hours.
//
// `M-L-SLEEP` disables the response.
func TestClockDivergenceForcesResnapshotAndReconcile(t *testing.T) {
	cases := []struct {
		name           string
		wallAdvance    time.Duration
		monoAdvance    time.Duration
		wantDetected   bool
		wantDowntime   time.Duration
		wantResnapshot bool
	}{
		{"ordinary tick", time.Second, time.Second, false, 0, false},
		{"small drift under the threshold", 3 * time.Second, time.Second, false, 0, false},
		{"exactly five seconds does not fire", 6 * time.Second, time.Second, false, 0, false},
		{"just past five seconds", 6*time.Second + time.Millisecond, time.Second,
			true, 5*time.Second + time.Millisecond, true},
		{"a two hour sleep", 2 * time.Hour, time.Second,
			true, 2*time.Hour - time.Second, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var d SleepDetector

			// The first sample is the baseline and reports nothing; treating the
			// process's own start as a gap would fire F7 on every launch.
			if eff := d.Observe(sleepEpoch.UnixMilli(), 0); eff.Detected {
				t.Fatal("the baseline sample fired F7")
			}

			eff := d.Observe(sleepEpoch.Add(tc.wallAdvance).UnixMilli(), tc.monoAdvance)
			if eff.Detected != tc.wantDetected {
				t.Fatalf("Detected = %v, want %v (wall +%v, mono +%v)",
					eff.Detected, tc.wantDetected, tc.wallAdvance, tc.monoAdvance)
			}
			if !tc.wantDetected {
				if len(eff.Anomalies) != 0 {
					t.Fatalf("anomalies on a normal tick: %v",
						classesOf(eff.Anomalies))
				}
				return
			}
			if eff.Downtime != tc.wantDowntime {
				t.Fatalf("Downtime = %v, want %v", eff.Downtime, tc.wantDowntime)
			}
			if !eff.Resnapshot || !eff.ReconcileNow {
				t.Fatalf("Resnapshot=%v ReconcileNow=%v: the book is stale by at "+
					"least the gap and the position may have moved",
					eff.Resnapshot, eff.ReconcileNow)
			}
			if sev, ok := sevOf(eff.Anomalies, "HOST_SLEEP_OR_STALL"); !ok ||
				sev != risk.SEV2 {
				t.Fatalf("HOST_SLEEP_OR_STALL sev %v present %v, want SEV2",
					sev, ok)
			}
			if d.Downtime() != tc.wantDowntime {
				t.Fatalf("accumulated downtime %v, want %v",
					d.Downtime(), tc.wantDowntime)
			}
		})
	}
}

// TestBackwardWallStepInvalidatesTruthWithoutNegativeUptime is the other
// direction: an NTP step, a manual clock change, a VM restored from a snapshot.
//
// It is not a sleep and produces no downtime, but it invalidates every
// wall-stamped freshness comparison in the system just as thoroughly. Recording
// it as negative downtime would produce an uptime fraction above 1.0, which is
// the kind of number that gets explained away rather than investigated.
func TestBackwardWallStepInvalidatesTruthWithoutNegativeUptime(t *testing.T) {
	var d SleepDetector
	d.Observe(sleepEpoch.UnixMilli(), 0)

	eff := d.Observe(sleepEpoch.Add(-time.Hour).UnixMilli(), time.Second)
	if !eff.Detected {
		t.Fatal("an hour-long backward wall step went unreported")
	}
	if eff.Downtime != 0 || d.Downtime() != 0 {
		t.Fatalf("a backward step recorded %v of downtime (total %v); an "+
			"uptime fraction above 1.0 gets explained away rather than "+
			"investigated", eff.Downtime, d.Downtime())
	}
	if eff.Gap != 0 {
		t.Fatalf("Gap = %v on a backward step, want 0", eff.Gap)
	}
	if !eff.Resnapshot || !eff.ReconcileNow {
		t.Fatalf("Resnapshot=%v ReconcileNow=%v: every wall-stamped freshness "+
			"comparison is now unreliable", eff.Resnapshot, eff.ReconcileNow)
	}

	// A small backward step is ordinary NTP discipline and is not an event.
	var d2 SleepDetector
	d2.Observe(sleepEpoch.UnixMilli(), 0)
	if eff := d2.Observe(sleepEpoch.Add(-2*time.Second).UnixMilli(), time.Second); eff.Detected {
		t.Fatal("a three-second backward drift fired F7")
	}
}

// TestSleepDetectorDoesNotReachIntoTheWebsocketGate keeps the effects a RELAY
// rather than a call.
//
// `lip-3af` relays `Resnapshot` and `ReconcileNow` into `wsx`. A detector that
// held a *wsx.Gate would need a method to call on it, and every method that
// forces a resnapshot from outside the gate is a widening of the surface
// `wsx/seal_test.go` exists to keep narrow.
func TestSleepDetectorDoesNotReachIntoTheWebsocketGate(t *testing.T) {
	var d SleepDetector
	d.Observe(sleepEpoch.UnixMilli(), 0)
	eff := d.Observe(sleepEpoch.Add(time.Hour).UnixMilli(), time.Second)

	// The effect is data. Nothing was invoked to produce it, and the caller is
	// free to ignore it -- which is what makes it testable without a socket.
	if !eff.Resnapshot {
		t.Fatal("a one-hour gap did not request a resnapshot")
	}
	if eff.Downtime <= 0 {
		t.Fatalf("downtime %v", eff.Downtime)
	}
}
