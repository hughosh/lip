package qual

import (
	"fmt"
	"math"
	"strings"
	"time"
)

const (
	q01MinimumActive     = 20 * time.Minute
	q01TargetMaximum     = 45 * time.Minute
	q01MonitorCadence    = time.Second
	q01PortfolioCadence  = 5 * time.Second
	q01MinimumPercentage = uint64(99)

	// Q01LocalScope is deliberately not named "qualified": this assessor can
	// prove only facts inside the preserved local bundle.
	Q01LocalScope = "local_evidence_only"
)

const (
	FailureQ01RestartMissing      FailureCode = "q01_restart_missing"
	FailureQ01AttemptEvidence     FailureCode = "q01_attempt_evidence_missing"
	FailureQ01MonitorSlots        FailureCode = "q01_monitor_slot_coverage"
	FailureQ01PortfolioSlots      FailureCode = "q01_portfolio_slot_coverage"
	FailureQ01PortfolioBadWalk    FailureCode = "q01_portfolio_bad_walk"
	FailureQ01HeartbeatMissing    FailureCode = "q01_heartbeat_missing"
	FailureQ01DisconnectMissing   FailureCode = "q01_disconnect_cycle_missing"
	FailureQ01UnexpectedAnomaly   FailureCode = "q01_unexpected_anomaly"
	FailureQ01UncountedTransport  FailureCode = "q01_uncounted_transport"
	DeviationQ01AboveTargetWindow             = "q01_above_target_window"
)

// Q01ProtocolDeviation is visible operator-review material that does not erase
// otherwise valid evidence. The revised event test targets a stop by 45
// minutes, while 20 minutes is the minimum active observation threshold.
type Q01ProtocolDeviation struct {
	Code   string `json:"code"`
	Detail string `json:"detail"`
}

// Q01LocalAssessment never claims that the whole q01 promotion is complete.
// LocalRequirementsMet covers only the evidence bundle. ExternalOutstanding is
// always populated with facts this process cannot prove about itself.
type Q01LocalAssessment struct {
	Scope                       string                 `json:"scope"`
	LocalRequirementsMet        bool                   `json:"local_requirements_met"`
	Elapsed                     time.Duration          `json:"elapsed"`
	AboveGuardNonGET            uint64                 `json:"above_guard_non_get"`
	BelowGuardNonGET            uint64                 `json:"below_guard_non_get"`
	WouldWriteObservations      uint64                 `json:"would_write_observations"`
	HistoricalUncleanSegments   uint64                 `json:"historical_unclean_segments"`
	ExpectedMonitorSlots        uint64                 `json:"expected_monitor_slots"`
	ObservedMonitorSlots        uint64                 `json:"observed_monitor_slots"`
	FreshMonitorSlots           uint64                 `json:"fresh_monitor_slots"`
	MonitorFreshSlotRatio       float64                `json:"monitor_fresh_slot_ratio"`
	ExpectedPortfolioSlots      uint64                 `json:"expected_portfolio_slots"`
	ObservedPortfolioSlots      uint64                 `json:"observed_portfolio_slots"`
	FreshCompletePortfolioSlots uint64                 `json:"fresh_complete_portfolio_slots"`
	PortfolioFreshCompleteRatio float64                `json:"portfolio_fresh_complete_slot_ratio"`
	ProtocolDeviations          []Q01ProtocolDeviation `json:"protocol_deviations"`
	ExternalOutstanding         []string               `json:"external_outstanding"`
	Failures                    []AssessmentFailure    `json:"failures"`
}

var q01ExternalOutstanding = [...]string{
	"real-account, real-feed, launchd, and caffeinate provenance",
	"operator provenance and timestamps for forced qualification actions",
	"alert-provider receipt and proof that a missed-heartbeat alarm fired",
}

// AssessQ01Local applies the fixed local portion of pilot-plan q01. It accepts
// no Requirements argument: a caller cannot shorten the 20-minute run, slow
// the expected cadences, lower 99%, or change the required events.
func AssessQ01Local(e Evidence) (Q01LocalAssessment, error) {
	result := Q01LocalAssessment{
		Scope:               Q01LocalScope,
		Failures:            make([]AssessmentFailure, 0),
		ProtocolDeviations:  make([]Q01ProtocolDeviation, 0),
		ExternalOutstanding: append([]string(nil), q01ExternalOutstanding[:]...),
	}

	structural := e
	structural.Metadata.Live = false
	if err := validateEvidence(structural); err != nil {
		return Q01LocalAssessment{}, err
	}

	fail := func(code FailureCode, detail string) {
		result.Failures = append(result.Failures, AssessmentFailure{
			Code: code, Detail: detail,
		})
	}
	if e.Metadata.Live {
		fail(FailureLive, "metadata says live=true; q01 local evidence must be structurally read-only")
	}
	if e.FinalizedAt == nil {
		fail(FailureNotFinalized, "the q01 evidence bundle has not been finalized")
	}

	restartObserved := false
	for i, segment := range e.Segments {
		linked := strings.TrimSpace(segment.RunID) != ""
		if !linked {
			result.Failures = append(result.Failures, AssessmentFailure{
				Code: FailureUnlinkedSegment, Name: segment.ID,
				Detail: fmt.Sprintf("process segment %q is not linked to a committed run",
					segment.ID),
			})
			continue
		}

		active := time.Duration(segment.ActiveNanos)
		result.Elapsed += active
		monitorExpected := expectedCallbacks(active, q01MonitorCadence)
		portfolioExpected := expectedCallbacks(active, q01PortfolioCadence)
		if segment.MonitorSlots.Observed > monitorExpected ||
			segment.MonitorSlots.Fresh > monitorExpected {
			return Q01LocalAssessment{}, fmt.Errorf("%w: segment %q monitor slot coverage exceeds %d expected slots",
				ErrCorrupt, segment.ID, monitorExpected)
		}
		if segment.PortfolioSlots.Observed > portfolioExpected ||
			segment.PortfolioSlots.FreshComplete > portfolioExpected {
			return Q01LocalAssessment{}, fmt.Errorf("%w: segment %q portfolio slot coverage exceeds %d expected slots",
				ErrCorrupt, segment.ID, portfolioExpected)
		}
		result.ExpectedMonitorSlots += monitorExpected
		result.ObservedMonitorSlots += segment.MonitorSlots.Observed
		result.FreshMonitorSlots += segment.MonitorSlots.Fresh
		result.ExpectedPortfolioSlots += portfolioExpected
		result.ObservedPortfolioSlots += segment.PortfolioSlots.Observed
		result.FreshCompletePortfolioSlots += segment.PortfolioSlots.FreshComplete

		if segment.EndedAt == nil && i < len(e.Segments)-1 {
			result.HistoricalUncleanSegments++
			for _, later := range e.Segments[i+1:] {
				if strings.TrimSpace(later.RunID) != "" {
					restartObserved = true
					break
				}
			}
		}
	}
	last := e.Segments[len(e.Segments)-1]
	if last.EndedAt == nil {
		result.Failures = append(result.Failures, AssessmentFailure{
			Code: FailureOpenSegment, Name: last.ID,
			Detail: fmt.Sprintf("final process segment %q has no recorded end", last.ID),
		})
	}
	if !restartObserved {
		fail(FailureQ01RestartMissing,
			"no linked historical unclean segment is followed by a linked resumed segment")
	}
	if result.Elapsed < q01MinimumActive {
		fail(FailureTooShort, fmt.Sprintf("linked monotonic active time %s is below fixed q01 minimum %s",
			result.Elapsed, q01MinimumActive))
	}
	if result.Elapsed > q01TargetMaximum {
		result.ProtocolDeviations = append(result.ProtocolDeviations, Q01ProtocolDeviation{
			Code: DeviationQ01AboveTargetWindow,
			Detail: fmt.Sprintf("linked monotonic active time %s exceeded the q01 stop target %s",
				result.Elapsed, q01TargetMaximum),
		})
	}

	result.AboveGuardNonGET = nonGETTotal(e.AttemptedHTTP)
	result.BelowGuardNonGET = nonGETTotal(e.HTTP)
	if e.AttemptedHTTP == nil {
		fail(FailureQ01AttemptEvidence,
			"above-guard attempted-method evidence is absent; an empty observed list must be encoded explicitly")
	}
	assertTransportWasCounted(e, fail)
	if result.BelowGuardNonGET != 0 {
		fail(FailureBelowGuardNonGET, fmt.Sprintf(
			"%d non-GET request(s) crossed the write guard", result.BelowGuardNonGET))
	}
	for _, episode := range e.WouldWrites {
		result.WouldWriteObservations = saturatingAdd(result.WouldWriteObservations,
			episode.Observations)
	}
	result.WouldWriteObservations = saturatingAdd(result.WouldWriteObservations,
		e.WouldWriteOverflow.Observations)
	if result.WouldWriteObservations == 0 {
		fail(FailureNoWouldWrite,
			"no would-write decision was observed from the real read-only decision path")
	}

	result.MonitorFreshSlotRatio = ratio(result.FreshMonitorSlots,
		result.ExpectedMonitorSlots)
	if !meetsQ01Percentage(result.FreshMonitorSlots, result.ExpectedMonitorSlots) {
		fail(FailureQ01MonitorSlots, fmt.Sprintf(
			"fresh monitor slots %d/%d are below fixed q01 minimum 99%% (%d distinct slots observed)",
			result.FreshMonitorSlots, result.ExpectedMonitorSlots,
			result.ObservedMonitorSlots))
	}
	result.PortfolioFreshCompleteRatio = ratio(result.FreshCompletePortfolioSlots,
		result.ExpectedPortfolioSlots)
	if !meetsQ01Percentage(result.FreshCompletePortfolioSlots,
		result.ExpectedPortfolioSlots) {
		fail(FailureQ01PortfolioSlots, fmt.Sprintf(
			"fresh+complete portfolio slots %d/%d are below fixed q01 minimum 99%% (%d distinct slots observed)",
			result.FreshCompletePortfolioSlots, result.ExpectedPortfolioSlots,
			result.ObservedPortfolioSlots))
	}
	if e.Portfolio.Stale != 0 || e.Portfolio.Incomplete != 0 {
		fail(FailureQ01PortfolioBadWalk, fmt.Sprintf(
			"q01 observed %d stale and %d incomplete portfolio walk(s)",
			e.Portfolio.Stale, e.Portfolio.Incomplete))
	}

	events := make(map[eventKey]uint64, len(e.Events))
	for _, event := range e.Events {
		key := eventKey{category: event.Category, name: event.Name}
		events[key] = event.Count
		if event.Category == EventAnomaly &&
			(strings.HasPrefix(event.Name, "SEV1:") || anomalyDroppedName(event.Name)) {
			result.Failures = append(result.Failures, AssessmentFailure{
				Code: FailureQ01UnexpectedAnomaly, Category: event.Category,
				Name:   event.Name,
				Detail: fmt.Sprintf("q01 recorded forbidden anomaly %q", event.Name),
			})
		}
	}
	for _, name := range []string{"sent", "deadman_checkin_sent"} {
		if events[eventKey{category: EventHeartbeat, name: name}] == 0 {
			result.Failures = append(result.Failures, AssessmentFailure{
				Code: FailureQ01HeartbeatMissing, Category: EventHeartbeat, Name: name,
				Detail: fmt.Sprintf("required local heartbeat event %q was not recorded", name),
			})
		}
	}
	connected := events[eventKey{category: EventState, name: "websocket:connected"}]
	disconnected := events[eventKey{category: EventState, name: "websocket:disconnected"}]
	if connected < 2 || disconnected < 1 {
		fail(FailureQ01DisconnectMissing, fmt.Sprintf(
			"websocket cycle has connected=%d disconnected=%d; q01 requires at least 2 and 1",
			connected, disconnected))
	}

	result.LocalRequirementsMet = len(result.Failures) == 0
	return result, nil
}

// assertTransportWasCounted checks that HTTP is a SUBSET of AttemptedHTTP.
//
// The two views are the same requests seen from either side of `WriteGuard`, so
// attempted is a superset of transported BY CONSTRUCTION: the only thing that
// can appear above and not below is a write the guard refused, and nothing at
// all can appear below without having passed through above. A key present in
// HTTP and missing from AttemptedHTTP therefore does not mean a miscount, it
// means that request never crossed the guard -- there is a second client on the
// transport.
//
// That is `lip-b0t`, measured from q01 attempt 1's own bundle: /incentive_programs
// had count 4 under `http` and no `attempted_http` entry at all, because the
// startup universe read was issued on a `rest.Client` composed over the raw
// doer. The consequence is not merely a gap in the counts. `AboveGuardNonGET`
// is computed from AttemptedHTTP, so a non-GET on a bypassing client would be
// reported as ZERO writes attempted while a write had in fact been transmitted
// -- the artifact whose entire purpose is to prove no write escaped, blind over
// exactly the path that could emit one.
//
// It lives in the assessment rather than only in a unit test because it is
// checkable against a bundle that already exists. A unit test proves today's
// composition; this proves the composition that actually ran, which is what
// stops a THIRD client reintroducing the hole silently.
func assertTransportWasCounted(e Evidence, fail func(FailureCode, string)) {
	attempted := make(map[HTTPKey]uint64, len(e.AttemptedHTTP))
	for _, count := range e.AttemptedHTTP {
		attempted[count.HTTPKey] = count.Count
	}
	for _, count := range e.HTTP {
		above, counted := attempted[count.HTTPKey]
		if !counted {
			fail(FailureQ01UncountedTransport, fmt.Sprintf(
				"%s %s reached the transport %d time(s) and was never observed "+
					"above the write guard; a request below the guard that is "+
					"absent above it did not cross the guard at all, so there "+
					"is a REST client in this process that H-VER-1 does not sit "+
					"under", count.Method, count.Endpoint, count.Count))
			continue
		}
		if above < count.Count {
			fail(FailureQ01UncountedTransport, fmt.Sprintf(
				"%s %s reached the transport %d time(s) but was observed only "+
					"%d time(s) above the write guard; the attempted view is a "+
					"superset of the transported one unless some of those calls "+
					"were issued on a client that bypasses the guard",
				count.Method, count.Endpoint, count.Count, above))
		}
	}
}

func nonGETTotal(counts []HTTPCount) uint64 {
	var total uint64
	for _, count := range counts {
		if count.Method != "GET" {
			total = saturatingAdd(total, count.Count)
		}
	}
	return total
}

func saturatingAdd(a, b uint64) uint64 {
	if math.MaxUint64-a < b {
		return math.MaxUint64
	}
	return a + b
}

func meetsQ01Percentage(numerator, denominator uint64) bool {
	if denominator == 0 {
		return false
	}
	// A time.Duration contains fewer than 10^10 one-second slots, so this
	// multiplication is far below uint64's bound even at its maximum duration.
	required := (denominator*q01MinimumPercentage + 99) / 100
	return numerator >= required
}

func anomalyDroppedName(name string) bool {
	return name == "ANOMALY_DROPPED" || strings.HasSuffix(name, ":ANOMALY_DROPPED")
}

// confidence: high
