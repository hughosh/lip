package lifecycle

import (
	"context"
	"errors"
	"testing"
	"time"

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
	// absent fee_cost, and `ConvertFills` makes it a hard error because
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

// TestAStartupCauseSurvivesAFailedWriteAndTheBackfillWindowMovingPast is the
// `Startup` half of lip-vxo, and the reason `BlockAdding` and `RetryLatch` are
// not equally benign during STARTING.
//
// `BlockAdding` is benign here: nothing places before an Adoption exists.
// `RetryLatch` is not, and the mechanism is the moving window. `commitCause`
// records a cause only AFTER a successful write, so a failed one leaves nothing
// behind; the pass then returns no Adoption and the caller retries the whole
// Step -- with a fresh clock, so Step 3 recomputes `since :=
// now.Add(-backfill_h)` every time. A fill discovered near the edge of that
// window whose latch write failed is simply not reported by the next walk, and
// the harness adopts a clean account and resumes adding next to a stop it had
// already decided to take.
//
// So the cause is RETAINED and rewritten before the next walk, and this test
// makes the second walk return nothing at all: the only thing that can put the
// trigger on disk is the retained cause.
func TestAStartupCauseSurvivesAFailedWriteAndTheBackfillWindowMovingPast(
	t *testing.T) {
	latch := &recordingLatch{writeErr: errors.New("EIO on the halt latch")}
	src := okSource()
	src.fills = rest.FillsResult{Walk: completeWalk(), Fills: []rest.Fill{{
		FillID: "f-1", TradeID: "t-1", OrderID: "ord-1", Ticker: "M",
		Price4: 5000, Count: num.QtyFromFloat(1),
		IsTaker: true, FeeCost: "0.01", TsMillis: 1,
	}}}
	s := newStartup(t, latch, src, ownsAll("ord-1"), keepAll(),
		newSweeper(true), "M")

	first := s.Step(context.Background(), startupNow)

	if first.Adoption != nil {
		t.Fatal("a pass whose stop cause could not be made durable returned " +
			"an Adoption; leaving STARTING on a stop we failed to record is " +
			"the HR-009 sequence with the harness having been TOLD the write " +
			"failed")
	}
	if !first.Retry {
		t.Fatal("the pass stopped retrying")
	}
	if len(latch.ensures) == 0 {
		t.Fatal("no write was even attempted on the first pass")
	}
	attempted := latch.ensures[0].Trigger
	if attempted == "" {
		t.Fatal("the attempted cause has no trigger")
	}
	if latch.present {
		t.Fatal("the latch reports a record present after a failing disk; " +
			"this test asserts nothing unless the first write really failed")
	}

	// The disk recovers, and the walk that discovered the cause can no longer
	// discover it: `backfill_h` has rolled past the fill. This is the ONLY
	// thing the second pass is missing, and without a retained cause it is
	// everything.
	latch.writeErr = nil
	src.fills = rest.FillsResult{Walk: completeWalk()}

	second := s.Step(context.Background(), startupNow.Add(time.Hour))

	if !latch.present {
		t.Fatalf("nothing reached the durable halt latch on the second pass. "+
			"The fill that produced cause %q is outside the backfill window "+
			"now and will never be reported again, so a cause that is not "+
			"RETAINED across the failed write is a global stop this process "+
			"decided to take and then lost -- and the account it adopts looks "+
			"clean", attempted)
	}
	if latch.rec.Trigger != attempted {
		t.Fatalf("the latch records trigger %q, want the retained %q",
			latch.rec.Trigger, attempted)
	}

	// And the retained cause is on the Adoption, not merely on disk: §7.5's
	// consumer reads `Causes()` to know why it is starting halted.
	if second.Adoption == nil {
		t.Fatalf("the second pass returned no Adoption (err=%v) even though "+
			"its cause is now durable and its walk was clean", second.Err)
	}
	var found bool
	for _, c := range second.Adoption.Causes() {
		if c.Trigger == attempted {
			found = true
		}
	}
	if !found {
		t.Fatalf("the Adoption carries %d cause(s), none with trigger %q; a "+
			"cause that reached disk but not the Adoption is one the run loop "+
			"never hears about", len(second.Adoption.Causes()), attempted)
	}
}
