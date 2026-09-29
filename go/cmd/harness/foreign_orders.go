package main

import (
	"fmt"

	"lip/harness/lifecycle"
	"lip/harness/rest"
	"lip/harness/risk"
)

// Live foreign orders invalidate the dedicated-account assumption globally.
// The portfolio layer has already emitted F15's SEV2 FOREIGN_ORDER. Retain that
// specified order severity; the durable latch carries the mandatory global stop.
// Startup still uses PhaseStartup, and none of these orders enters our queue.
func (o *owner) stopForForeignOrders(orders []risk.LiveOrder) {
	if len(orders) == 0 {
		return
	}
	wire := make([]rest.Order, len(orders))
	for i, ord := range orders {
		wire[i] = rest.Order{OrderID: ord.OrderID, Ticker: ord.Ticker,
			Side: ord.Side, Price4: ord.Price4, Remaining: ord.Remaining}
	}
	eff, err := o.r.guard.Classify(lifecycle.PhaseLive, wire, nil, o.r.ex.NowMs())
	if err != nil {
		o.r.anom.raise(risk.Anomaly{Class: "FOREIGN_CLASSIFICATION_FAILED", Sev: risk.SEV1,
			Text: fmt.Sprintf("live foreign order classification failed: %v", err)})
		o.requestStop("foreign_classification_failed", "")
		return
	}
	// The guard's orders-only anomalies duplicate the portfolio's FOREIGN_ORDER
	// records; use its phase-sensitive causes, not a second severity for one fact.
	for _, cause := range eff.Causes {
		o.commitStop(cause)
	}
}

// confidence: high
