package qual

import (
	"errors"
	"math"
	"strings"
	"testing"
	"time"
)

func assessmentRequirements() Requirements {
	return Requirements{
		MinimumElapsed:                     4 * time.Hour,
		ExpectedMonitorCadence:             30 * time.Minute,
		ExpectedPortfolioCadence:           30 * time.Minute,
		MinimumMonitorFreshRatio:           0.8,
		MinimumPortfolioFreshCompleteRatio: 0.9,
		RequiredForcedEvents:               []string{"clean_disconnect"},
		RequiredStateEvents:                []string{"QUARANTINED", "RUNNING"},
		RequiredAnomalyEvents:              []string{"OWNER_STALLED"},
		RequiredHeartbeatEvents:            []string{"sent"},
	}
}

func validAssessmentEvidence() Evidence {
	ended := testStart.Add(4 * time.Hour)
	finalized := ended
	fingerprint := WouldWriteFingerprint{
		Kind: WriteCreate, Ticker: "TEST-26AUG11", Side: "yes",
		Price: "45", Quantity: "1", State: "ADDING",
	}
	return Evidence{
		Metadata:  testMetadata(),
		CreatedAt: testStart,
		UpdatedAt: ended,
		Segments: []ProcessSegment{{
			ID: "process-1", RunID: "run-1", PID: 101, StartedAt: testStart,
			EndedAt: &ended, EndReason: "finalized", ActiveNanos: int64(4 * time.Hour),
		}},
		HTTP: []HTTPCount{{
			HTTPKey: HTTPKey{Method: "GET", Endpoint: "/portfolio/orders"}, Count: 20,
		}},
		WouldWrites: []WouldWriteEpisode{{
			Fingerprint: fingerprint, SegmentID: "process-1",
			FirstAt: testStart.Add(time.Hour), LastAt: testStart.Add(2 * time.Hour),
			Observations: 10,
		}},
		Monitor: FreshnessCounters{
			Checks: 10, Fresh: 8, Stale: 2,
			LastCheckAt: ended, LastFreshAt: ended,
		},
		Portfolio: PortfolioCounters{
			Walks: 10, Fresh: 10, Stale: 0, Complete: 9, Incomplete: 1,
			FreshComplete: 9, LastWalkAt: ended, LastFreshAt: ended,
			LastCompleteAt: ended,
		},
		Events: []EventCounter{
			{Category: EventAnomaly, Name: "OWNER_STALLED", Count: 1, FirstAt: ended, LastAt: ended},
			{Category: EventForced, Name: "clean_disconnect", Count: 1, FirstAt: ended, LastAt: ended},
			{Category: EventHeartbeat, Name: "sent", Count: 4, FirstAt: testStart, LastAt: ended},
			{Category: EventState, Name: "QUARANTINED", Count: 1, FirstAt: ended, LastAt: ended},
			{Category: EventState, Name: "RUNNING", Count: 2, FirstAt: testStart, LastAt: ended},
		},
		FinalizedAt: &finalized,
	}
}

func failureCodes(result Assessment) map[FailureCode]int {
	out := make(map[FailureCode]int)
	for _, failure := range result.Failures {
		out[failure.Code]++
	}
	return out
}

func TestAssessQualifiesAtExactBoundaries(t *testing.T) {
	result, err := Assess(validAssessmentEvidence(), assessmentRequirements())
	if err != nil {
		t.Fatal(err)
	}
	if !result.Qualified || len(result.Failures) != 0 {
		t.Fatalf("boundary evidence did not qualify: %+v", result)
	}
	if result.Elapsed != 4*time.Hour || result.MonitorFreshRatio != 0.8 ||
		result.PortfolioFreshCompleteRatio != 0.9 || result.BelowGuardNonGET != 0 ||
		result.WouldWriteEpisodes != 1 || result.WouldWriteObservations != 10 ||
		result.ExpectedMonitorChecks != 8 || result.ExpectedPortfolioWalks != 8 {
		t.Fatalf("assessment measurements wrong: %+v", result)
	}
}

func TestAssessRejectsEachIncompleteCondition(t *testing.T) {
	tests := []struct {
		name string
		edit func(*Evidence)
		code FailureCode
	}{
		{"live metadata", func(e *Evidence) { e.Metadata.Live = true }, FailureLive},
		{"not finalized", func(e *Evidence) { e.FinalizedAt = nil }, FailureNotFinalized},
		{"unlinked segment", func(e *Evidence) { e.Segments[0].RunID = "" }, FailureUnlinkedSegment},
		{"open segment", func(e *Evidence) {
			e.Segments[0].EndedAt = nil
			e.Segments[0].EndReason = ""
		}, FailureOpenSegment},
		{"below guard POST", func(e *Evidence) {
			e.HTTP = append(e.HTTP, HTTPCount{
				HTTPKey: HTTPKey{Method: "POST", Endpoint: "/portfolio/events/orders"}, Count: 2,
			})
		}, FailureBelowGuardNonGET},
		{"no would write", func(e *Evidence) { e.WouldWrites = nil }, FailureNoWouldWrite},
		{"monitor missing", func(e *Evidence) { e.Monitor = FreshnessCounters{} }, FailureMonitorMissing},
		{"monitor below ratio", func(e *Evidence) {
			e.Monitor.Fresh, e.Monitor.Stale = 7, 3
		}, FailureMonitorRatio},
		{"portfolio missing", func(e *Evidence) { e.Portfolio = PortfolioCounters{} }, FailurePortfolioMissing},
		{"portfolio below ratio", func(e *Evidence) {
			e.Portfolio.Complete, e.Portfolio.Incomplete, e.Portfolio.FreshComplete = 8, 2, 8
		}, FailurePortfolioRatio},
		{"too short", func(e *Evidence) {
			short := e.CreatedAt.Add(4*time.Hour - time.Nanosecond)
			e.FinalizedAt, e.Segments[0].EndedAt, e.UpdatedAt = &short, &short, short
			e.Segments[0].ActiveNanos = int64(4*time.Hour - time.Nanosecond)
		}, FailureTooShort},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			evidence := validAssessmentEvidence()
			tc.edit(&evidence)
			result, err := Assess(evidence, assessmentRequirements())
			if err != nil {
				t.Fatalf("Assess: %v", err)
			}
			if result.Qualified || failureCodes(result)[tc.code] == 0 {
				t.Fatalf("result = %+v, want failure %q", result, tc.code)
			}
		})
	}
}

func TestAssessReportsEveryMissingRequiredEventDeterministically(t *testing.T) {
	evidence := validAssessmentEvidence()
	evidence.Events = nil
	result, err := Assess(evidence, assessmentRequirements())
	if err != nil {
		t.Fatal(err)
	}
	if result.Qualified || failureCodes(result)[FailureRequiredEventMissing] != 5 {
		t.Fatalf("missing event failures = %+v", result.Failures)
	}
	want := []struct {
		category EventCategory
		name     string
	}{
		{EventAnomaly, "OWNER_STALLED"},
		{EventForced, "clean_disconnect"},
		{EventHeartbeat, "sent"},
		{EventState, "QUARANTINED"},
		{EventState, "RUNNING"},
	}
	var got []struct {
		category EventCategory
		name     string
	}
	for _, failure := range result.Failures {
		if failure.Code == FailureRequiredEventMissing {
			got = append(got, struct {
				category EventCategory
				name     string
			}{failure.Category, failure.Name})
		}
	}
	if len(got) != len(want) {
		t.Fatalf("missing events = %#v, want %#v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("missing events = %#v, want %#v", got, want)
		}
	}
}

func TestAssessElapsedIsSummedProcessTimeNotWallTime(t *testing.T) {
	evidence := validAssessmentEvidence()
	firstEnd := testStart.Add(time.Hour)
	secondStart := testStart.Add(5 * time.Hour)
	secondEnd := testStart.Add(6 * time.Hour)
	evidence.Segments = []ProcessSegment{
		{ID: "process-1", RunID: "run-1", PID: 101, StartedAt: testStart, EndedAt: &firstEnd, EndReason: "restart", ActiveNanos: int64(time.Hour)},
		{ID: "process-2", RunID: "run-2", PID: 202, StartedAt: secondStart, EndedAt: &secondEnd, EndReason: "finalized", ActiveNanos: int64(time.Hour)},
	}
	evidence.UpdatedAt = secondEnd
	evidence.FinalizedAt = &secondEnd
	result, err := Assess(evidence, assessmentRequirements())
	if err != nil {
		t.Fatal(err)
	}
	if result.Elapsed != 2*time.Hour || result.Qualified ||
		failureCodes(result)[FailureTooShort] != 1 {
		t.Fatalf("six wall hours with two process hours = %+v, want elapsed=2h and too-short", result)
	}
}

func TestAssessCountsPersistedActiveTimeAcrossHistoricalUncleanRestart(t *testing.T) {
	evidence := validAssessmentEvidence()
	secondStart := testStart.Add(3 * time.Hour)
	secondEnd := secondStart.Add(time.Hour)
	evidence.Segments = []ProcessSegment{
		{ID: "process-1", RunID: "run-1", PID: 101, StartedAt: testStart, ActiveNanos: int64(3 * time.Hour)},
		{ID: "process-2", RunID: "run-2", PID: 202, StartedAt: secondStart, EndedAt: &secondEnd, EndReason: "finalized", ActiveNanos: int64(time.Hour)},
	}
	evidence.UpdatedAt = secondEnd
	evidence.FinalizedAt = &secondEnd
	result, err := Assess(evidence, assessmentRequirements())
	if err != nil {
		t.Fatal(err)
	}
	if !result.Qualified || result.Elapsed != 4*time.Hour || result.UncleanSegments != 1 ||
		failureCodes(result)[FailureOpenSegment] != 0 {
		t.Fatalf("recovered unclean restart assessment = %+v, want qualified, elapsed=4h, unclean=1", result)
	}
}

func TestAssessRejectsUnlinkedHistoricalSegmentWithoutCountingRestart(t *testing.T) {
	evidence := validAssessmentEvidence()
	secondStart := testStart.Add(3 * time.Hour)
	secondEnd := secondStart.Add(4 * time.Hour)
	evidence.Segments = []ProcessSegment{
		{ID: "process-1", PID: 101, StartedAt: testStart, ActiveNanos: int64(3 * time.Hour)},
		{ID: "process-2", RunID: "run-2", PID: 202, StartedAt: secondStart,
			EndedAt: &secondEnd, EndReason: "finalized", ActiveNanos: int64(4 * time.Hour)},
	}
	evidence.UpdatedAt = secondEnd
	evidence.FinalizedAt = &secondEnd

	result, err := Assess(evidence, assessmentRequirements())
	if err != nil {
		t.Fatal(err)
	}
	if result.Qualified || failureCodes(result)[FailureUnlinkedSegment] != 1 ||
		result.UncleanSegments != 0 || result.Elapsed != 4*time.Hour ||
		result.ExpectedMonitorChecks != 8 || result.ExpectedPortfolioWalks != 8 {
		t.Fatalf("unlinked pre-run segment affected qualification authority: %+v", result)
	}
}

func TestAssessCadenceDenominatorPenalizesFourHoursOfSilence(t *testing.T) {
	evidence := validAssessmentEvidence()
	evidence.Monitor = FreshnessCounters{
		Checks: 1, Fresh: 1, LastCheckAt: testStart, LastFreshAt: testStart,
	}
	evidence.Portfolio = PortfolioCounters{
		Walks: 1, Fresh: 1, Complete: 1, FreshComplete: 1,
		LastWalkAt: testStart, LastFreshAt: testStart, LastCompleteAt: testStart,
	}
	requirements := assessmentRequirements()
	requirements.ExpectedMonitorCadence = time.Hour
	requirements.ExpectedPortfolioCadence = time.Hour
	result, err := Assess(evidence, requirements)
	if err != nil {
		t.Fatal(err)
	}
	codes := failureCodes(result)
	if result.Qualified || result.ExpectedMonitorChecks != 4 ||
		result.ExpectedPortfolioWalks != 4 || result.MonitorFreshRatio != 0.25 ||
		result.PortfolioFreshCompleteRatio != 0.25 ||
		codes[FailureMonitorRatio] != 1 || codes[FailurePortfolioRatio] != 1 {
		t.Fatalf("one callback followed by four active hours = %+v", result)
	}
}

func TestAssessUsesMonotonicActiveDurationNotWallTimestamps(t *testing.T) {
	evidence := validAssessmentEvidence()
	// The wall stamps claim a full day, but the persisted monotonic process
	// duration says two hours. A wall step cannot manufacture qualification.
	wallEnd := testStart.Add(24 * time.Hour)
	evidence.Segments[0].EndedAt = &wallEnd
	evidence.Segments[0].ActiveNanos = int64(2 * time.Hour)
	evidence.FinalizedAt = &wallEnd
	evidence.UpdatedAt = wallEnd
	result, err := Assess(evidence, assessmentRequirements())
	if err != nil {
		t.Fatal(err)
	}
	if result.Elapsed != 2*time.Hour || failureCodes(result)[FailureTooShort] != 1 {
		t.Fatalf("assessment trusted wall time over monotonic active time: %+v", result)
	}
}

func TestAssessRejectsInvalidRequirementsAndCorruptEvidence(t *testing.T) {
	tests := []struct {
		name string
		edit func(*Requirements)
	}{
		{"zero duration", func(r *Requirements) { r.MinimumElapsed = 0 }},
		{"zero monitor cadence", func(r *Requirements) { r.ExpectedMonitorCadence = 0 }},
		{"zero portfolio cadence", func(r *Requirements) { r.ExpectedPortfolioCadence = 0 }},
		{"zero monitor ratio", func(r *Requirements) { r.MinimumMonitorFreshRatio = 0 }},
		{"monitor ratio above one", func(r *Requirements) { r.MinimumMonitorFreshRatio = 1.01 }},
		{"monitor ratio NaN", func(r *Requirements) { r.MinimumMonitorFreshRatio = math.NaN() }},
		{"zero portfolio ratio", func(r *Requirements) { r.MinimumPortfolioFreshCompleteRatio = 0 }},
		{"portfolio ratio above one", func(r *Requirements) { r.MinimumPortfolioFreshCompleteRatio = 1.01 }},
		{"portfolio ratio NaN", func(r *Requirements) { r.MinimumPortfolioFreshCompleteRatio = math.NaN() }},
		{"empty event", func(r *Requirements) { r.RequiredForcedEvents = []string{" "} }},
		{"duplicate event", func(r *Requirements) { r.RequiredStateEvents = []string{"RUNNING", "RUNNING"} }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			requirements := assessmentRequirements()
			tc.edit(&requirements)
			if _, err := Assess(validAssessmentEvidence(), requirements); err == nil {
				t.Fatal("Assess accepted invalid requirements")
			}
		})
	}

	corrupt := validAssessmentEvidence()
	corrupt.Monitor.Checks++ // totals no longer add up
	_, err := Assess(corrupt, assessmentRequirements())
	if !errors.Is(err, ErrCorrupt) {
		t.Fatalf("corrupt evidence error = %v, want ErrCorrupt", err)
	}
}

func TestAssessmentDoesNotClaimExternalDeadmanProof(t *testing.T) {
	result, err := Assess(validAssessmentEvidence(), assessmentRequirements())
	if err != nil || !result.Qualified {
		t.Fatalf("valid assessment: result=%+v err=%v", result, err)
	}
	for _, failure := range result.Failures {
		if strings.Contains(strings.ToLower(failure.Detail), "deadman") ||
			strings.Contains(strings.ToLower(failure.Detail), "dead-man") {
			t.Fatalf("local assessment invented external dead-man evidence: %+v", failure)
		}
	}
}
