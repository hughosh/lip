package qual

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
)

// Requirements is the explicit local evidence contract for one qualification
// rung.  Ratios must be in (0, 1], and MinimumElapsed must be positive, so a
// zero-value Requirements cannot accidentally qualify a run.
//
// Required event names prove only that this process observed and recorded the
// named events.  In particular, no field here claims that an external dead-man
// provider fired its missed-heartbeat alarm; that proof is external evidence
// and must remain an operator-reviewed prerequisite outside Assess.
type Requirements struct {
	MinimumElapsed                     time.Duration
	ExpectedMonitorCadence             time.Duration
	ExpectedPortfolioCadence           time.Duration
	MinimumMonitorFreshRatio           float64
	MinimumPortfolioFreshCompleteRatio float64
	RequiredForcedEvents               []string
	RequiredStateEvents                []string
	RequiredAnomalyEvents              []string
	RequiredHeartbeatEvents            []string
}

// FailureCode is a stable machine-readable reason a structurally valid bundle
// is not qualified.
type FailureCode string

const (
	FailureLive                 FailureCode = "live_metadata"
	FailureNotFinalized         FailureCode = "not_finalized"
	FailureUnlinkedSegment      FailureCode = "unlinked_segment"
	FailureOpenSegment          FailureCode = "open_segment"
	FailureBelowGuardNonGET     FailureCode = "below_guard_non_get"
	FailureNoWouldWrite         FailureCode = "no_would_write"
	FailureMonitorMissing       FailureCode = "monitor_missing"
	FailureMonitorRatio         FailureCode = "monitor_fresh_ratio"
	FailurePortfolioMissing     FailureCode = "portfolio_missing"
	FailurePortfolioRatio       FailureCode = "portfolio_fresh_complete_ratio"
	FailureTooShort             FailureCode = "elapsed_too_short"
	FailureRequiredEventMissing FailureCode = "required_event_missing"
)

// AssessmentFailure is one detailed reason Qualified is false.  Category and
// Name are set for missing events; Detail is always suitable for an operator
// report but is not intended as a machine key.
type AssessmentFailure struct {
	Code     FailureCode   `json:"code"`
	Category EventCategory `json:"category,omitempty"`
	Name     string        `json:"name,omitempty"`
	Detail   string        `json:"detail"`
}

// Assessment contains both the decision and the measurements behind it.
// Qualified is true if and only if Failures is empty.
type Assessment struct {
	Qualified                   bool          `json:"qualified"`
	Elapsed                     time.Duration `json:"elapsed"`
	MonitorFreshRatio           float64       `json:"monitor_fresh_ratio"`
	PortfolioFreshCompleteRatio float64       `json:"portfolio_fresh_complete_ratio"`
	ExpectedMonitorChecks       uint64        `json:"expected_monitor_checks"`
	ExpectedPortfolioWalks      uint64        `json:"expected_portfolio_walks"`
	BelowGuardNonGET            uint64        `json:"below_guard_non_get"`
	WouldWriteEpisodes          uint64        `json:"would_write_episodes"`
	WouldWriteObservations      uint64        `json:"would_write_observations"`
	// UncleanSegments counts historical process segments without an end
	// marker. The final/current segment is FailureOpenSegment instead.
	UncleanSegments uint64              `json:"unclean_segments"`
	Failures        []AssessmentFailure `json:"failures"`
}

// Assess evaluates a detached evidence snapshot without modifying or
// finalizing it.  Invalid Requirements and structurally corrupt Evidence return
// an error.  Valid evidence that is merely incomplete returns a nil error and a
// detailed Assessment with Qualified=false.
func Assess(e Evidence, requirements Requirements) (Assessment, error) {
	required, err := validateRequirements(requirements)
	if err != nil {
		return Assessment{}, err
	}

	// Live is a qualification failure, not structural corruption.  Validate a
	// read-only copy so the result can say exactly why an otherwise valid bundle
	// is refused instead of returning a generic decode/shape error.
	structural := e
	structural.Metadata.Live = false
	if err := validateEvidence(structural); err != nil {
		return Assessment{}, err
	}

	result := Assessment{Failures: make([]AssessmentFailure, 0)}
	fail := func(code FailureCode, detail string) {
		result.Failures = append(result.Failures, AssessmentFailure{
			Code: code, Detail: detail,
		})
	}

	if e.Metadata.Live {
		fail(FailureLive, "metadata says live=true; the read-only qualification format cannot qualify an armed run")
	}
	if e.FinalizedAt == nil {
		fail(FailureNotFinalized, "the evidence bundle has not been finalized")
	}
	for i, segment := range e.Segments {
		if strings.TrimSpace(segment.RunID) == "" {
			result.Failures = append(result.Failures, AssessmentFailure{
				Code: FailureUnlinkedSegment, Name: segment.ID,
				Detail: fmt.Sprintf("process segment %q is not linked to a committed run",
					segment.ID),
			})
			// A process that failed before run creation is neither qualification
			// time nor evidence of q01's forced restart.
			continue
		}
		// Every durable monotonic checkpoint is eligible qualification time,
		// including the tail of a process that was intentionally SIGKILLed.
		// Its absent wall-clock end records the forced restart; it does not erase
		// active time already persisted before the kill.
		active := time.Duration(segment.ActiveNanos)
		result.Elapsed += active
		result.ExpectedMonitorChecks += expectedCallbacks(active,
			requirements.ExpectedMonitorCadence)
		result.ExpectedPortfolioWalks += expectedCallbacks(active,
			requirements.ExpectedPortfolioCadence)
		if segment.EndedAt == nil {
			if i == len(e.Segments)-1 {
				// The final segment must close before the bundle can qualify.  An
				// earlier missing end is different: it is the durable evidence of
				// an unclean restart, which q01 deliberately forces.  Preserve and
				// count it, but let explicit restart/state event requirements decide
				// whether the expected recovery was demonstrated.
				result.Failures = append(result.Failures, AssessmentFailure{
					Code: FailureOpenSegment, Name: segment.ID,
					Detail: fmt.Sprintf("final process segment %q has no recorded end", segment.ID),
				})
			} else {
				result.UncleanSegments++
			}
		}
	}
	monitorDenominator := maxUint64(e.Monitor.Checks, result.ExpectedMonitorChecks)
	portfolioDenominator := maxUint64(e.Portfolio.Walks, result.ExpectedPortfolioWalks)
	result.MonitorFreshRatio = ratio(e.Monitor.Fresh, monitorDenominator)
	result.PortfolioFreshCompleteRatio = ratio(e.Portfolio.FreshComplete,
		portfolioDenominator)

	var nonGET []string
	for _, count := range e.HTTP {
		if count.Method == "GET" {
			continue
		}
		result.BelowGuardNonGET += count.Count
		nonGET = append(nonGET, fmt.Sprintf("%s %s=%d", count.Method, count.Endpoint, count.Count))
	}
	if result.BelowGuardNonGET > 0 {
		sort.Strings(nonGET)
		fail(FailureBelowGuardNonGET, fmt.Sprintf(
			"%d non-GET request(s) crossed the write guard: %s",
			result.BelowGuardNonGET, strings.Join(nonGET, ", ")))
	}

	result.WouldWriteEpisodes = uint64(len(e.WouldWrites)) +
		e.WouldWriteOverflow.Episodes
	for _, episode := range e.WouldWrites {
		result.WouldWriteObservations += episode.Observations
	}
	result.WouldWriteObservations += e.WouldWriteOverflow.Observations
	if result.WouldWriteObservations == 0 {
		fail(FailureNoWouldWrite, "no would-write decision was observed; the write guard was not exercised by a real decision")
	}

	if e.Monitor.Checks == 0 {
		fail(FailureMonitorMissing, "no monitor freshness samples were recorded")
	} else if result.MonitorFreshRatio < requirements.MinimumMonitorFreshRatio {
		fail(FailureMonitorRatio, fmt.Sprintf(
			"monitor fresh ratio %.6f is below required %.6f (%d/%d; %d expected at %s cadence)",
			result.MonitorFreshRatio, requirements.MinimumMonitorFreshRatio,
			e.Monitor.Fresh, monitorDenominator, result.ExpectedMonitorChecks,
			requirements.ExpectedMonitorCadence))
	}
	if e.Portfolio.Walks == 0 {
		fail(FailurePortfolioMissing, "no portfolio walks were recorded")
	} else if result.PortfolioFreshCompleteRatio < requirements.MinimumPortfolioFreshCompleteRatio {
		fail(FailurePortfolioRatio, fmt.Sprintf(
			"portfolio fresh+complete ratio %.6f is below required %.6f (%d/%d; %d expected at %s cadence)",
			result.PortfolioFreshCompleteRatio,
			requirements.MinimumPortfolioFreshCompleteRatio,
			e.Portfolio.FreshComplete, portfolioDenominator,
			result.ExpectedPortfolioWalks, requirements.ExpectedPortfolioCadence))
	}
	if e.FinalizedAt != nil && result.Elapsed < requirements.MinimumElapsed {
		fail(FailureTooShort, fmt.Sprintf("elapsed %s is below required %s",
			result.Elapsed, requirements.MinimumElapsed))
	}

	eventCounts := make(map[eventKey]uint64, len(e.Events))
	for _, event := range e.Events {
		eventCounts[eventKey{category: event.Category, name: event.Name}] = event.Count
	}
	for _, event := range required {
		if eventCounts[event.key] != 0 {
			continue
		}
		result.Failures = append(result.Failures, AssessmentFailure{
			Code: FailureRequiredEventMissing, Category: event.key.category,
			Name: event.key.name,
			Detail: fmt.Sprintf("required %s event %q was not recorded",
				event.key.category, event.key.name),
		})
	}

	result.Qualified = len(result.Failures) == 0
	return result, nil
}

type requiredEvent struct{ key eventKey }

func validateRequirements(r Requirements) ([]requiredEvent, error) {
	if r.MinimumElapsed <= 0 {
		return nil, fmt.Errorf("minimum elapsed duration %s is not positive", r.MinimumElapsed)
	}
	if r.ExpectedMonitorCadence <= 0 {
		return nil, fmt.Errorf("expected monitor cadence %s is not positive",
			r.ExpectedMonitorCadence)
	}
	if r.ExpectedPortfolioCadence <= 0 {
		return nil, fmt.Errorf("expected portfolio cadence %s is not positive",
			r.ExpectedPortfolioCadence)
	}
	if math.IsNaN(r.MinimumMonitorFreshRatio) ||
		r.MinimumMonitorFreshRatio <= 0 || r.MinimumMonitorFreshRatio > 1 {
		return nil, fmt.Errorf("minimum monitor fresh ratio %v is outside (0, 1]",
			r.MinimumMonitorFreshRatio)
	}
	if math.IsNaN(r.MinimumPortfolioFreshCompleteRatio) ||
		r.MinimumPortfolioFreshCompleteRatio <= 0 || r.MinimumPortfolioFreshCompleteRatio > 1 {
		return nil, fmt.Errorf("minimum portfolio fresh+complete ratio %v is outside (0, 1]",
			r.MinimumPortfolioFreshCompleteRatio)
	}

	var out []requiredEvent
	seen := make(map[eventKey]bool)
	add := func(category EventCategory, names []string) error {
		for _, raw := range names {
			name := strings.TrimSpace(raw)
			if name == "" {
				return fmt.Errorf("required %s event name is empty", category)
			}
			key := eventKey{category: category, name: name}
			if seen[key] {
				return fmt.Errorf("required %s event %q is duplicated", category, name)
			}
			seen[key] = true
			out = append(out, requiredEvent{key: key})
		}
		return nil
	}
	for _, item := range []struct {
		category EventCategory
		names    []string
	}{
		{EventForced, r.RequiredForcedEvents},
		{EventState, r.RequiredStateEvents},
		{EventAnomaly, r.RequiredAnomalyEvents},
		{EventHeartbeat, r.RequiredHeartbeatEvents},
	} {
		if err := add(item.category, item.names); err != nil {
			return nil, err
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].key.category != out[j].key.category {
			return out[i].key.category < out[j].key.category
		}
		return out[i].key.name < out[j].key.name
	})
	return out, nil
}

func ratio(numerator, denominator uint64) float64 {
	if denominator == 0 {
		return 0
	}
	return float64(numerator) / float64(denominator)
}

func expectedCallbacks(active, cadence time.Duration) uint64 {
	if active <= 0 {
		return 0
	}
	// Ceiling division: an eligible partial cadence interval still owes one
	// callback. Both values are positive and time.Duration is int64, so
	// (active-1)/cadence+1 cannot overflow.
	return uint64((active-1)/cadence + 1)
}

func maxUint64(a, b uint64) uint64 {
	if a > b {
		return a
	}
	return b
}

// confidence: high
