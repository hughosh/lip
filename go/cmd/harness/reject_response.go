package main

import (
	"lip/harness/rest"
	"lip/harness/risk"
)

// F10 is a global correctness stop. Use the structured definite-reject code;
// a local refusal, ambiguous response, or free-form message is not this fact.
func (o *owner) stopForInsufficientBalance(market string, result rest.CreateResult) {
	if result.Outcome != rest.CreateRejected || result.RejectReason != "insufficient_balance" {
		return
	}
	o.r.anom.raise(risk.Anomaly{
		Class: "INSUFFICIENT_BALANCE", Sev: risk.SEV1, Ticker: market,
		Text: "the exchange rejected a create for insufficient_balance; capital accounting is no longer trustworthy, so adding stops globally while reduction and observation continue",
	})
	o.requestStop("insufficient_balance", market)
}

// confidence: high
