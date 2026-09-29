package main

import (
	"strings"
	"testing"

	"lip/harness/num"
	"lip/harness/quote"
	"lip/harness/risk"
)

func TestFirstCompletePositionDisagreementIsDurablyAttributable(t *testing.T) {
	g := newGateFailOwner(t, num.QtyFromFloat(1))
	h, o := g.h, g.o
	h.ex.setPosition(seamTicker, "1.00")
	reducerConfirmationPoll(t, g)
	h.takeRaised()
	h.ex.setPosition(seamTicker, "1.05")
	driftPollAgain(t, g)
	o.evaluate(h.clk.Now().Mono)
	if o.market == quote.Reducing || o.global != quote.Running {
		t.Fatal("first disagreement changed state before the sustained threshold")
	}
	o.sd.handleAnomalies()
	h.await("first position discrepancy in durable journal", func() bool {
		return len(h.anomalySevs("POSITION_DISAGREEMENT")) == 1
	})
	rows, err := h.rig.store.Reader().PendingAnomalies()
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if row.Class != "POSITION_DISAGREEMENT" {
			continue
		}
		if row.Sev != risk.SEV3 || row.Ticker != seamTicker ||
			!strings.Contains(row.Text, "q_local=1.00 q_exch=1.05 delta=-0.05") {
			t.Fatalf("unattributable position discrepancy: %+v", row)
		}
	}

	h.ex.breakPositions(true)
	h.ex.setPosition(seamTicker, "1.10")
	driftPollAgain(t, g)
	for _, a := range h.takeRaised() {
		if a.Class == "POSITION_DISAGREEMENT" {
			t.Fatalf("partial read was recorded as authoritative: %+v", a)
		}
	}
	if h.rig.pf.Q(seamTicker) != num.QtyFromFloat(1.05) {
		t.Fatal("partial read overwrote the last authoritative position")
	}
}
