package qual

import (
	"strings"
	"testing"
	"time"
)

func validQ01Evidence() Evidence {
	firstActive := 10 * time.Minute
	secondActive := 10 * time.Minute
	secondStart := testStart.Add(firstActive)
	ended := secondStart.Add(secondActive)
	finalized := ended
	fingerprint := WouldWriteFingerprint{
		Kind: WriteCreate, Ticker: "TEST-26AUG11", Side: "yes",
		Price: "45", Quantity: "1", State: "ADDING",
	}
	e := Evidence{
		Metadata:  testMetadata(),
		CreatedAt: testStart,
		UpdatedAt: ended,
		Segments: []ProcessSegment{
			{
				ID: "process-1", RunID: "run-1", PID: 101, StartedAt: testStart,
				ActiveNanos: int64(firstActive),
			},
			{
				ID: "process-2", RunID: "run-2", PID: 202, StartedAt: secondStart,
				EndedAt: &ended, EndReason: "finalized", ActiveNanos: int64(secondActive),
			},
		},
		// The two views as a correctly composed chain produces them: the reads
		// are seen on BOTH sides of the guard, and the refused write is seen
		// only above it. HTTP is a subset of AttemptedHTTP by construction --
		// see `assertTransportWasCounted`, which is what caught `lip-b0t`.
		AttemptedHTTP: []HTTPCount{
			{HTTPKey: HTTPKey{Method: "GET", Endpoint: "/portfolio/orders"}, Count: 20},
			{HTTPKey: HTTPKey{Method: "POST", Endpoint: "/portfolio/events/orders"}, Count: 1},
		},
		HTTP: []HTTPCount{{
			HTTPKey: HTTPKey{Method: "GET", Endpoint: "/portfolio/orders"}, Count: 20,
		}},
		WouldWrites: []WouldWriteEpisode{{
			Fingerprint: fingerprint, SegmentID: "process-2",
			FirstAt: secondStart, LastAt: ended, Observations: 10,
		}},
		Events: []EventCounter{
			{Category: EventHeartbeat, Name: "deadman_checkin_sent", Count: 4, FirstAt: testStart, LastAt: ended},
			{Category: EventHeartbeat, Name: "sent", Count: 4, FirstAt: testStart, LastAt: ended},
			{Category: EventState, Name: "websocket:connected", Count: 2, FirstAt: testStart, LastAt: ended},
			{Category: EventState, Name: "websocket:disconnected", Count: 1, FirstAt: secondStart, LastAt: secondStart},
		},
		FinalizedAt: &finalized,
	}
	setQ01Durations(&e, firstActive, secondActive)
	return e
}

func setQ01Durations(e *Evidence, durations ...time.Duration) {
	var monitor, portfolio uint64
	for i, active := range durations {
		e.Segments[i].ActiveNanos = int64(active)
		m := expectedCallbacks(active, q01MonitorCadence)
		p := expectedCallbacks(active, q01PortfolioCadence)
		e.Segments[i].MonitorSlots = MonitorSlotCoverage{Observed: m, Fresh: m}
		e.Segments[i].PortfolioSlots = PortfolioSlotCoverage{
			Observed: p, FreshComplete: p,
		}
		monitor += m
		portfolio += p
	}
	e.Monitor = FreshnessCounters{
		Checks: monitor, Fresh: monitor,
		LastCheckAt: e.UpdatedAt, LastFreshAt: e.UpdatedAt,
	}
	e.Portfolio = PortfolioCounters{
		Walks: portfolio, Fresh: portfolio, Complete: portfolio,
		FreshComplete: portfolio, LastWalkAt: e.UpdatedAt,
		LastFreshAt: e.UpdatedAt, LastCompleteAt: e.UpdatedAt,
	}
}

func q01FailureCodes(result Q01LocalAssessment) map[FailureCode]int {
	out := make(map[FailureCode]int)
	for _, failure := range result.Failures {
		out[failure.Code]++
	}
	return out
}

func removeQ01Event(e *Evidence, category EventCategory, name string) {
	out := e.Events[:0]
	for _, event := range e.Events {
		if event.Category != category || event.Name != name {
			out = append(out, event)
		}
	}
	e.Events = out
}

func TestAssessQ01LocalPassesExactTwentyMinuteRestartBoundary(t *testing.T) {
	result, err := AssessQ01Local(validQ01Evidence())
	if err != nil {
		t.Fatal(err)
	}
	if !result.LocalRequirementsMet || len(result.Failures) != 0 {
		t.Fatalf("exact q01 boundary did not pass locally: %+v", result)
	}
	if result.Scope != Q01LocalScope || result.Elapsed != 20*time.Minute ||
		result.HistoricalUncleanSegments != 1 ||
		result.ExpectedMonitorSlots != 20*60 ||
		result.FreshMonitorSlots != result.ExpectedMonitorSlots ||
		result.ExpectedPortfolioSlots != 20*60/5 ||
		result.FreshCompletePortfolioSlots != result.ExpectedPortfolioSlots {
		t.Fatalf("q01 local measurements = %+v", result)
	}
	if len(result.ExternalOutstanding) != len(q01ExternalOutstanding) {
		t.Fatalf("external outstanding = %v", result.ExternalOutstanding)
	}
	for _, item := range result.ExternalOutstanding {
		if !strings.Contains(item, "provenance") &&
			!strings.Contains(item, "provider") {
			t.Fatalf("external item does not name unproved evidence: %q", item)
		}
	}
}

func TestAssessQ01LocalRejectsOneNanosecondBelowTwentyMinutes(t *testing.T) {
	e := validQ01Evidence()
	setQ01Durations(&e, 10*time.Minute, 10*time.Minute-time.Nanosecond)
	result, err := AssessQ01Local(e)
	if err != nil {
		t.Fatal(err)
	}
	if result.LocalRequirementsMet || q01FailureCodes(result)[FailureTooShort] != 1 {
		t.Fatalf("sub-twenty-minute q01 passed: %+v", result)
	}
}

func TestAssessQ01LocalNinetyNinePercentSlotBoundary(t *testing.T) {
	boundary := validQ01Evidence()
	// Monitor: 1,188/1,200 is exactly 99%. Portfolio: ceil(99% of
	// 240)=238; the missing two slots are silence, not bad observed walks.
	boundary.Segments[1].MonitorSlots.Fresh = 588
	boundary.Monitor.Fresh = 1188
	boundary.Monitor.Stale = 12
	boundary.Segments[1].PortfolioSlots.Observed = 118
	boundary.Segments[1].PortfolioSlots.FreshComplete = 118
	boundary.Portfolio = PortfolioCounters{
		Walks: 238, Fresh: 238, Complete: 238, FreshComplete: 238,
		LastWalkAt: boundary.UpdatedAt, LastFreshAt: boundary.UpdatedAt,
		LastCompleteAt: boundary.UpdatedAt,
	}
	result, err := AssessQ01Local(boundary)
	if err != nil || !result.LocalRequirementsMet {
		t.Fatalf("fixed 99%% boundary did not pass: result=%+v err=%v", result, err)
	}

	monitorBelow := boundary
	monitorBelow.Segments = append([]ProcessSegment(nil), boundary.Segments...)
	monitorBelow.Segments[1].MonitorSlots.Fresh--
	monitorBelow.Monitor.Fresh--
	monitorBelow.Monitor.Stale++
	result, err = AssessQ01Local(monitorBelow)
	if err != nil || result.LocalRequirementsMet ||
		q01FailureCodes(result)[FailureQ01MonitorSlots] != 1 {
		t.Fatalf("below-99%% monitor slots passed: result=%+v err=%v", result, err)
	}

	portfolioBelow := boundary
	portfolioBelow.Segments = append([]ProcessSegment(nil), boundary.Segments...)
	portfolioBelow.Segments[1].PortfolioSlots.Observed--
	portfolioBelow.Segments[1].PortfolioSlots.FreshComplete--
	portfolioBelow.Portfolio.Walks--
	portfolioBelow.Portfolio.Fresh--
	portfolioBelow.Portfolio.Complete--
	portfolioBelow.Portfolio.FreshComplete--
	result, err = AssessQ01Local(portfolioBelow)
	if err != nil || result.LocalRequirementsMet ||
		q01FailureCodes(result)[FailureQ01PortfolioSlots] != 1 {
		t.Fatalf("below-99%% portfolio slots passed: result=%+v err=%v", result, err)
	}
}

func TestAssessQ01LocalAboveFortyFiveMinutesIsDeviationOnly(t *testing.T) {
	e := validQ01Evidence()
	setQ01Durations(&e, 20*time.Minute, 25*time.Minute+time.Second)
	result, err := AssessQ01Local(e)
	if err != nil {
		t.Fatal(err)
	}
	if !result.LocalRequirementsMet || len(result.Failures) != 0 ||
		len(result.ProtocolDeviations) != 1 ||
		result.ProtocolDeviations[0].Code != DeviationQ01AboveTargetWindow {
		t.Fatalf("above-forty-five-minute assessment = %+v", result)
	}
}

func TestAssessQ01LocalBurstThenSilenceCannotFillSlots(t *testing.T) {
	e := validQ01Evidence()
	e.Segments[0].MonitorSlots = MonitorSlotCoverage{Observed: 1, Fresh: 1}
	e.Segments[1].MonitorSlots = MonitorSlotCoverage{}
	result, err := AssessQ01Local(e)
	if err != nil {
		t.Fatal(err)
	}
	if result.LocalRequirementsMet ||
		q01FailureCodes(result)[FailureQ01MonitorSlots] != 1 ||
		result.FreshMonitorSlots != 1 || result.ExpectedMonitorSlots != 20*60 {
		t.Fatalf("burst stood in for silent monitor slots: %+v", result)
	}
}

func TestCallerSelectedRequirementsCannotWeakenQ01(t *testing.T) {
	e := validQ01Evidence()
	e.Segments[0].MonitorSlots = MonitorSlotCoverage{Observed: 1, Fresh: 1}
	e.Segments[1].MonitorSlots = MonitorSlotCoverage{}

	generic, err := Assess(e, Requirements{
		MinimumElapsed:                     time.Millisecond,
		ExpectedMonitorCadence:             24 * time.Hour,
		ExpectedPortfolioCadence:           24 * time.Hour,
		MinimumMonitorFreshRatio:           0.000001,
		MinimumPortfolioFreshCompleteRatio: 0.000001,
	})
	if err != nil || !generic.Qualified {
		t.Fatalf("generic low policy should demonstrate caller control: result=%+v err=%v",
			generic, err)
	}
	fixed, err := AssessQ01Local(e)
	if err != nil {
		t.Fatal(err)
	}
	if fixed.LocalRequirementsMet || q01FailureCodes(fixed)[FailureQ01MonitorSlots] != 1 {
		t.Fatalf("caller-selected generic policy weakened fixed q01: %+v", fixed)
	}
}

func TestAssessQ01LocalRejectsUnexpectedSEV1AndAnomalyDrop(t *testing.T) {
	for _, name := range []string{"SEV1:OWNER_STALLED", "ANOMALY_DROPPED", "SEV2:ANOMALY_DROPPED"} {
		t.Run(name, func(t *testing.T) {
			e := validQ01Evidence()
			e.Events = append(e.Events, EventCounter{
				Category: EventAnomaly, Name: name, Count: 1,
				FirstAt: testStart, LastAt: testStart,
			})
			result, err := AssessQ01Local(e)
			if err != nil {
				t.Fatal(err)
			}
			if result.LocalRequirementsMet ||
				q01FailureCodes(result)[FailureQ01UnexpectedAnomaly] != 1 {
				t.Fatalf("forbidden anomaly %q passed: %+v", name, result)
			}
		})
	}
}

func TestAssessQ01LocalRequiresAttemptsDecisionsHeartbeatsAndDisconnect(t *testing.T) {
	tests := []struct {
		name string
		edit func(*Evidence)
		code FailureCode
	}{
		{"attempt evidence", func(e *Evidence) { e.AttemptedHTTP = nil }, FailureQ01AttemptEvidence},
		{"would-write", func(e *Evidence) { e.WouldWrites = nil }, FailureNoWouldWrite},
		{"heartbeat sent", func(e *Evidence) { removeQ01Event(e, EventHeartbeat, "sent") }, FailureQ01HeartbeatMissing},
		{"deadman check-in", func(e *Evidence) { removeQ01Event(e, EventHeartbeat, "deadman_checkin_sent") }, FailureQ01HeartbeatMissing},
		{"disconnect", func(e *Evidence) { removeQ01Event(e, EventState, "websocket:disconnected") }, FailureQ01DisconnectMissing},
		{"reconnect", func(e *Evidence) {
			for i := range e.Events {
				if e.Events[i].Category == EventState && e.Events[i].Name == "websocket:connected" {
					e.Events[i].Count = 1
				}
			}
		}, FailureQ01DisconnectMissing},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e := validQ01Evidence()
			tc.edit(&e)
			result, err := AssessQ01Local(e)
			if err != nil {
				t.Fatal(err)
			}
			if result.LocalRequirementsMet || q01FailureCodes(result)[tc.code] == 0 {
				t.Fatalf("missing %s passed: %+v", tc.name, result)
			}
		})
	}
}

func TestAssessQ01LocalAllowsNoAboveGuardNonGETOnCleanAccount(t *testing.T) {
	e := validQ01Evidence()
	e.AttemptedHTTP = []HTTPCount{{
		HTTPKey: HTTPKey{Method: "GET", Endpoint: "/portfolio/orders"}, Count: 20,
	}}
	result, err := AssessQ01Local(e)
	if err != nil {
		t.Fatal(err)
	}
	if !result.LocalRequirementsMet || result.AboveGuardNonGET != 0 {
		t.Fatalf("clean-account q01 required an unreachable non-GET attempt: %+v", result)
	}
}

// The `lip-b0t` detector, run against the shape attempt 1's bundle actually had.
//
// A REST client composed without the guard shows up as an endpoint that reached
// the transport and was never observed above it. The assessment has to fail on
// that rather than merely note it: `AboveGuardNonGET` is computed from
// AttemptedHTTP, so an unguarded client's writes would be reported as zero
// writes attempted, and the artifact whose purpose is to prove no write escaped
// would be certifying a path it cannot see.
func TestAssessQ01LocalRejectsTransportNotCountedAboveTheGuard(t *testing.T) {
	for _, tc := range []struct {
		name      string
		attempted []HTTPCount
	}{
		{
			// Exactly q01 attempt 1: /incentive_programs in `http`, absent
			// from `attempted_http`.
			name: "endpoint absent above the guard",
			attempted: []HTTPCount{
				{HTTPKey: HTTPKey{Method: "GET", Endpoint: "/portfolio/orders"}, Count: 20},
				{HTTPKey: HTTPKey{Method: "POST", Endpoint: "/portfolio/events/orders"}, Count: 1},
			},
		},
		{
			// The partial bypass: the endpoint IS counted above, but by fewer
			// calls than reached the wire, so some of them were issued on a
			// second client. Presence alone would miss this.
			name: "endpoint undercounted above the guard",
			attempted: []HTTPCount{
				{HTTPKey: HTTPKey{Method: "GET", Endpoint: "/incentive_programs"}, Count: 2},
				{HTTPKey: HTTPKey{Method: "GET", Endpoint: "/portfolio/orders"}, Count: 20},
				{HTTPKey: HTTPKey{Method: "POST", Endpoint: "/portfolio/events/orders"}, Count: 1},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := validQ01Evidence()
			e.HTTP = append(e.HTTP, HTTPCount{
				HTTPKey: HTTPKey{Method: "GET", Endpoint: "/incentive_programs"},
				Count:   4,
			})
			e.AttemptedHTTP = tc.attempted
			result, err := AssessQ01Local(e)
			if err != nil {
				t.Fatal(err)
			}
			if result.LocalRequirementsMet ||
				q01FailureCodes(result)[FailureQ01UncountedTransport] == 0 {
				t.Fatalf("a bundle recording transport that never crossed the "+
					"write guard was accepted: %+v", result)
			}
		})
	}
}

func TestAssessQ01LocalRejectsObservedBadPortfolioWalk(t *testing.T) {
	e := validQ01Evidence()
	e.Portfolio.Walks++
	e.Portfolio.Stale++
	e.Portfolio.Incomplete++
	result, err := AssessQ01Local(e)
	if err != nil {
		t.Fatal(err)
	}
	if result.LocalRequirementsMet ||
		q01FailureCodes(result)[FailureQ01PortfolioBadWalk] != 1 {
		t.Fatalf("observed bad portfolio walk passed: %+v", result)
	}
}
