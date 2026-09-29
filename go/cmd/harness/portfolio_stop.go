package main

import (
	"fmt"

	"lip/harness/risk"
	"lip/harness/wsx"
)

// The pilot defers dense position_poll telemetry, but every authoritative
// discrepancy above tolerance remains attributable in the durable journal.
// Partial and old-generation walks never reach this consumer.
func (o *owner) recordPositionDisagreements(records []risk.PollRecord) {
	for _, rec := range records {
		if rec.Delta.Abs() <= o.p.PosDriftTol {
			continue
		}
		o.r.anom.raise(risk.Anomaly{
			Class: "POSITION_DISAGREEMENT", Sev: risk.SEV3, Ticker: rec.Ticker,
			Text: fmt.Sprintf("complete position poll: q_local=%s q_exch=%s delta=%s", rec.QLocal.Wire(), rec.QExch.Wire(), rec.Delta.Wire()),
		})
	}
}

// stopForPortfolio preserves a named hard-drift cause in the durable latch.
// Other portfolio stops retain the existing generic cause until they have a
// distinct halt-table trigger of their own.
func (o *owner) stopForPortfolio(eff wsx.PortfolioEffects) {
	if !eff.Stop {
		return
	}
	for _, anomaly := range eff.Anomalies {
		if anomaly.Class == "POSITION_DRIFT" && anomaly.Sev == risk.SEV1 {
			o.requestStop("position_drift", anomaly.Ticker)
			return
		}
	}
	o.requestStop("portfolio_read", "")
}

// confidence: high
