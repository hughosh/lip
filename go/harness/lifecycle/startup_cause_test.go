package lifecycle

import (
	"context"
	"testing"

	"lip/harness/num"
	"lip/harness/rest"
)

// TestStartupCommitsKnownTakerEvidenceBeforeFeeConversion is lip-eyq §3.4.
//
// H-ORD-8 has TWO independent detectors for a taker fill: the exchange's
// `is_taker` flag, and S2's corroborator that a non-zero fee cannot belong to a
// maker fill. They fail for different reasons, and the second one needs
// arithmetic on `fee_cost` that a missing or malformed field defeats.
//
// So the order matters. `is_taker` is a fact the moment the fills walk lands --
// it needs no conversion and cannot fail -- while the fee path can throw on the
// very same fill. Committing the durable stop only after the conversion means a
// fill the exchange EXPLICITLY FLAGGED as a taker produces nothing at all when
// its fee is unreadable: the pass returns a generic retryable error, the stop is
// never latched, and after `backfill_h` rolls past it the fill stops being
// reported at all. The harness then resumes adding next to a taker fill it was
// told about and forgot.
//
// This is why the commitment is immediate and why it is driven off the RAW
// flag. The pass still FAILS here -- a fill of ours that will not convert is not
// a completed reconciliation and returns no Adoption -- but it fails with the
// stop already on disk.
//
// `M-L-CAUSEFAIL` postpones the commitment past the conversion.
func TestStartupCommitsKnownTakerEvidenceBeforeFeeConversion(t *testing.T) {
	latch := &recordingLatch{}
	src := okSource()
	// Ours, flagged taker, and deliberately unconvertible: `rest` accepts an
	// absent fee_cost, and `convertStartupFills` makes it a hard error because
	// S2's corroborator cannot be evaluated without it.
	src.fills = rest.FillsResult{Walk: completeWalk(), Fills: []rest.Fill{{
		FillID: "f-1", TradeID: "t-1", OrderID: "ord-1", Ticker: "M",
		Price4: 5000, Count: num.QtyFromFloat(1),
		IsTaker: true, FeeCost: "", TsMillis: 1,
	}}}
	s := newStartup(t, latch, src, ownsAll("ord-1"), keepAll(),
		newSweeper(true), "M")

	at := s.Step(context.Background(), startupNow)

	// The conversion failed, so there is no licence to leave STARTING.
	if at.Err == nil {
		t.Fatal("a fill of ours with no fee_cost converted; S2's corroborator " +
			"of H-ORD-8 cannot be evaluated without it, so it must be a hard " +
			"error rather than a zero fee")
	}
	if at.Adoption != nil {
		t.Fatal("a pass whose fee conversion failed returned an Adoption; the " +
			"only thing an Adoption is for is licensing the exit from STARTING")
	}
	if !at.Retry {
		t.Fatal("the pass stopped retrying")
	}

	// ...and the stop is on disk ANYWAY, committed before the conversion that
	// failed. This is the whole property.
	var triggers []string
	for _, r := range latch.ensures {
		triggers = append(triggers, r.Trigger)
		if r.Trigger == "startup_fill_history" {
			if r.Market != "M" {
				t.Fatalf("the taker cause named market %q, want M; H-HALT-4's "+
					"record is trigger, timestamp AND market", r.Market)
			}
			return
		}
	}
	t.Fatalf("a fill EXPLICITLY flagged is_taker did not latch a durable "+
		"startup_fill_history stop before the fee conversion failed on it; "+
		"the latch store recorded %v. H-ORD-8 is a fact about the harness "+
		"whenever it happened, and a stop that waits on arithmetic that "+
		"throws is a stop that never happens", triggers)
}
