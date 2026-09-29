package main

import (
	"testing"

	"lip/harness/num"
	"lip/harness/quote"
)

// The attended sizing rung permits S up to 12 but must retain the durable
// first-fill stop. A one-quantum position stays below the ordinary inventory
// thresholds, so this exercises the rung's own stop policy.
func TestTheSizingRungLatchesOnFirstPosition(t *testing.T) {
	h := newSeamHarness(t, seamOptions{Rung: "sizing"})
	h.start()
	h.awaitActionable()
	if h.cfg.Params.S != num.QtyFromFloat(12) {
		t.Fatalf("sizing S is %v, want 12", h.cfg.Params.S)
	}
	if got := h.latchTrigger(); got != "" {
		t.Fatalf("sizing rung latched before a position appeared: %q", got)
	}

	h.ex.setPosition(seamTicker, "0.01")
	h.clk.Advance(h.cfg.Params.PositionPoll)
	h.awaitGlobalEvent("RUNNING->WINDING_DOWN/global_stop")
	if got := h.latchTrigger(); got != "canary_position_nonzero" {
		t.Fatalf("sizing rung first position latched with cause %q", got)
	}
	if state := h.snapshot().Global; state != quote.WindingDown {
		t.Fatalf("sizing rung global state is %s, want WINDING_DOWN", state)
	}
}
