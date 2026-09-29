package main

import (
	"fmt"
	"time"

	"lip/harness/num"
	"lip/harness/risk"
)

// inventoryStuckTracker belongs to the owner goroutine. A position is evidence
// for F16 only after a complete walk was applied on the current connection
// generation. Incomplete walks leave the previous episode intact; they never
// establish recovery or refresh the evidence used to emit an alert.
type inventoryStuckTracker struct {
	positions map[string]num.Qty
	gen       uint64
	known     bool
	episodes  map[string]inventoryStuckEpisode
}

type inventoryStuckEpisode struct {
	since  time.Duration
	pinged bool
}

// observe replaces the position truth from one accepted complete walk. An
// omitted market is flat, so a complete omission can end an episode. The first
// accepted breach in this process starts its clock, including after restart;
// no duration from a previous process is inferred from an adopted position.
func (s *inventoryStuckTracker) observe(now time.Duration, gen uint64,
	positions map[string]num.Qty, hard num.Qty) {
	if s.episodes == nil {
		s.episodes = make(map[string]inventoryStuckEpisode)
	}
	for ticker := range s.episodes {
		if positions[ticker].Abs() <= hard {
			delete(s.episodes, ticker)
		}
	}
	s.positions = make(map[string]num.Qty, len(positions))
	for ticker, q := range positions {
		s.positions[ticker] = q
		if q.Abs() > hard {
			if _, active := s.episodes[ticker]; !active {
				s.episodes[ticker] = inventoryStuckEpisode{since: now}
			}
		}
	}
	s.gen = gen
	s.known = true
}

// step emits exactly once after the configured duration is exceeded. A stale
// position or old generation cannot authorize an alert, but neither clears an
// episode: only a later complete position at or below hard can do that.
func (s *inventoryStuckTracker) step(ticker string, now time.Duration, gen uint64,
	age, maxAge, stuck time.Duration) (risk.Anomaly, bool) {
	if !s.known || s.gen != gen || age < 0 || age > maxAge {
		return risk.Anomaly{}, false
	}
	ep, active := s.episodes[ticker]
	if !active || ep.pinged || now-ep.since <= stuck {
		return risk.Anomaly{}, false
	}
	ep.pinged = true
	s.episodes[ticker] = ep
	return risk.Anomaly{
		Class: "INVENTORY_STUCK", Sev: risk.SEV2, Ticker: ticker,
		Text: fmt.Sprintf("position %s has remained beyond inv_hard for %v, "+
			"longer than stuck_s %v; the market is already REDUCING",
			s.positions[ticker].Wire(), now-ep.since, stuck),
	}, true
}

// confidence: high
