package lifecycle

import (
	"fmt"
	"time"

	"lip/harness/risk"
)

// sleepDivergence is F7's threshold: "per tick, compare wall-clock delta against
// monotonic delta; divergence > 5s".
//
// Strictly greater. Exactly five seconds does not fire, because the spec wrote
// `>` and a boundary that drifts by one comparison operator is a boundary the
// negative control cannot pin.
const sleepDivergence = 5 * time.Second

// SleepEffects is one paired-sample observation.
type SleepEffects struct {
	// Detected is true when wall and monotonic disagreed by more than the
	// threshold.
	Detected bool
	// Gap is how much wall time the monotonic clock did not account for. It is
	// never negative: see the backward-step case in Observe.
	Gap time.Duration
	// Resnapshot asks the websocket layer for a full book resnapshot. This type
	// does not call wsx and does not hold a Gate -- `lip-3af` relays it. A
	// detector that reached into the gate would widen a surface `seal_test.go`
	// exists to keep narrow.
	Resnapshot bool
	// ReconcileNow asks the portfolio poller for an immediate complete walk.
	ReconcileNow bool
	// Downtime is the interval to record in `uptime`. Durable accounting is
	// `lip-6w5`.
	Downtime  time.Duration
	Anomalies []risk.Anomaly
}

// SleepDetector is F7, and it is the reason the Mac running this is not a
// silently unreliable host.
//
// The observed failure is in the operator's own notes: the iMac idle-sleeps, and
// a sleep voids everything above it -- the socket is dead, the book is stale by
// however long the lid was down, and every timer that measured the interval on a
// monotonic clock believes no time passed. Nothing in the process notices,
// because from the process's point of view nothing happened.
//
// H-DEP-3's `caffeinate -is` is the prevention. This is the DETECTION, and both
// are needed: the assertion lives exactly as long as the process, so it covers
// nothing during the window between a crash and `launchd` restarting, and it
// does not cover a stall that is not a sleep at all -- a stopped world, a
// suspended container, a debugger.
type SleepDetector struct {
	started bool
	// lastWallMillis is a wall reading as an INTEGER, not a time.Time. See
	// Observe.
	lastWallMillis int64
	lastMono       time.Duration
	totalDowntime  time.Duration
}

// Observe takes one paired reading of the wall clock and a monotonic source.
//
// # Why the wall reading is an int64 and not a time.Time
//
// `time.Now()` returns a Time carrying BOTH a wall reading and a monotonic
// reading, and `t.Sub(u)` uses the monotonic readings whenever both operands
// have one. So the obvious signature -- `Observe(wall time.Time, mono
// time.Duration)` -- computes `wall.Sub(s.lastWall)` as a MONOTONIC delta, and
// this entire detector compares the monotonic clock against itself. The
// divergence is then ~0 across a two-hour sleep and F7 can never fire.
//
// That is not a hypothetical: it is what an earlier version of this file did,
// and its tests passed, because `time.Unix(...)` and `.Add(...)` produce Times
// with NO monotonic reading and `Sub` correctly falls back to wall arithmetic
// for those. The bug was invisible to every test and present in every
// production call.
//
// An int64 of Unix milliseconds cannot carry a hidden monotonic reading, so the
// two sources are structurally independent and the subtraction below is
// unambiguously wall-minus-monotonic. `lip-3af` supplies
// `time.Now().UnixMilli()` and its own monotonic source.
//
// The first sample establishes the baseline and reports nothing; there is no
// previous pair to difference against, and treating the process's own start as a
// gap would fire F7 on every launch.
func (s *SleepDetector) Observe(wallMillis int64, mono time.Duration) SleepEffects {
	var eff SleepEffects
	if !s.started {
		s.started = true
		s.lastWallMillis = wallMillis
		s.lastMono = mono
		return eff
	}

	wallDelta := time.Duration(wallMillis-s.lastWallMillis) * time.Millisecond
	monoDelta := mono - s.lastMono
	s.lastWallMillis = wallMillis
	s.lastMono = mono

	divergence := wallDelta - monoDelta
	if divergence < 0 {
		// The wall clock went BACKWARDS relative to the monotonic one -- an NTP
		// step, a manual clock change, a VM restored from a snapshot. It is not
		// a sleep and it produces no downtime, but it invalidates truth in the
		// same way: every wall-stamped freshness comparison in the system just
		// became meaningless, so the book is resnapshotted and the portfolio
		// reconciled.
		//
		// Downtime stays zero rather than going negative. A negative interval
		// added to `uptime` produces an uptime fraction above 1.0, which is the
		// kind of number that gets explained away rather than investigated.
		if -divergence > sleepDivergence {
			eff.Detected = true
			eff.Resnapshot = true
			eff.ReconcileNow = true
			eff.Anomalies = append(eff.Anomalies, risk.Anomaly{
				Class: "HOST_SLEEP_OR_STALL", Sev: risk.SEV2,
				Text: fmt.Sprintf("the wall clock moved %s while the monotonic "+
					"clock moved %s -- a backward wall step of %s. No downtime "+
					"is recorded, but every wall-stamped freshness comparison "+
					"is now unreliable, so the book is resnapshotted and the "+
					"portfolio reconciled", wallDelta, monoDelta, -divergence),
			})
		}
		return eff
	}

	if divergence <= sleepDivergence {
		return eff
	}

	s.totalDowntime += divergence
	eff.Detected = true
	eff.Gap = divergence
	eff.Resnapshot = true
	eff.ReconcileNow = true
	eff.Downtime = divergence
	eff.Anomalies = append(eff.Anomalies, risk.Anomaly{
		Class: "HOST_SLEEP_OR_STALL", Sev: risk.SEV2,
		Text: fmt.Sprintf("the wall clock advanced %s while the monotonic clock "+
			"advanced %s: %s of wall time passed that this process did not "+
			"observe. The book is stale by at least that much and the position "+
			"may have moved, so the interval is recorded as downtime, the book "+
			"is resnapshotted and the portfolio is reconciled now (F7)",
			wallDelta, monoDelta, divergence),
	})
	return eff
}

// Downtime is the total unobserved wall time this detector has accumulated.
func (s *SleepDetector) Downtime() time.Duration { return s.totalDowntime }

// confidence: high
