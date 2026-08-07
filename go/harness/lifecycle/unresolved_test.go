package lifecycle

import (
	"context"
	"testing"

	"lip/harness/rest"
)

// TestStartupDoesNotLatchForeignFillWhileReservationsUnresolved is adversarial
// FINDING 2 at full length, ending where it does the damage.
//
// The sequence: H-ORD-6 commits a reservation, the order is dispatched, and the
// process is SIGKILLed before the binding commits -- the exact crash the
// two-stage commit exists to survive. The order fills. We come back up, and
// §7.5's startup walk reads a fill whose order id is nowhere in the ledger.
//
// What used to happen next is the whole finding. The unbound reservation was
// invisible, the fill classified FOREIGN, and startup committed a `foreign_fill`
// stop cause -- which is a DURABLE, operator-only WINDING_DOWN latch. Nothing in
// the harness can clear it. Every subsequent restart reads the latch and goes
// straight back to WINDING_DOWN, so an unattended process that did exactly the
// right thing takes itself off the market permanently, on the strength of its
// own order.
//
// This test pins both layers, because the classification being right is not
// enough if the caller then treats it like a conclusion:
//
//   - `Classify` routes the fill to `Unresolved` and produces NO cause, NO
//     anomaly and NO exclusion;
//   - `Startup.Run` does not conclude. No adoption, no latch, retry -- the same
//     posture as an unavailable ledger, which is H-ORD-5a's "keep trying".
//
// `M-L-INDETLATCH` folds the unresolved case back into the foreign one.
func TestStartupDoesNotLatchForeignFillWhileReservationsUnresolved(t *testing.T) {
	const orderID = "ord-dispatched-never-bound"

	// --- layer 1: the classification ----------------------------------------
	guard, err := NewForeignGuard(unresolvedFor(orderID))
	if err != nil {
		t.Fatalf("NewForeignGuard: %v", err)
	}
	fills := []rest.Fill{makerFill("t1", orderID, "M")}

	for _, phase := range []Phase{PhaseStartup, PhaseLive} {
		eff, err := guard.Classify(phase, nil, fills, 99)
		if err != nil {
			t.Fatalf("phase %v: classify: %v", phase, err)
		}
		if len(eff.Unresolved) != 1 || eff.Unresolved[0].TradeID != "t1" {
			t.Fatalf("phase %v: Unresolved = %+v, want the one fill the ledger "+
				"cannot attribute", phase, eff.Unresolved)
		}
		if len(eff.ForeignFills) != 0 {
			t.Fatalf("phase %v: a fill on our own dispatched-but-unbound order "+
				"was disowned: %+v", phase, eff.ForeignFills)
		}
		if eff.Stop() {
			t.Fatalf("phase %v: an unresolved fill requested a global stop "+
				"(%+v). That stop is DURABLE and operator-only: it survives "+
				"every restart, and here it would have been produced by our "+
				"own order", phase, eff.Causes)
		}
		if len(eff.Anomalies) != 0 {
			t.Fatalf("phase %v: an unresolved fill produced %v; it is not an "+
				"event, it is the absence of one", phase,
				classesOf(eff.Anomalies))
		}
		if len(eff.OwnedFills) != 0 {
			t.Fatalf("phase %v: an unresolved fill was adopted as ours: %+v. "+
				"The other guess is just as wrong -- it books a position we "+
				"may not hold", phase, eff.OwnedFills)
		}
	}

	// A ledger that HAS concluded still latches. Without this the test would
	// pass just as well against a guard that never latches at all, which is the
	// rule being disabled rather than corrected.
	conclusive, err := NewForeignGuard(ownsAll())
	if err != nil {
		t.Fatalf("NewForeignGuard: %v", err)
	}
	eff, err := conclusive.Classify(PhaseStartup, nil, fills, 99)
	if err != nil {
		t.Fatalf("classify: %v", err)
	}
	if !eff.Stop() || len(eff.ForeignFills) != 1 {
		t.Fatalf("a CONCLUSIVELY foreign fill did not request a global stop "+
			"(causes %+v, foreign %+v); H-ORD-9's detection of a third party "+
			"trading the account must still work", eff.Causes, eff.ForeignFills)
	}

	// --- layer 2: what startup does with it ---------------------------------
	store := &recordingLatch{}
	src := okSource()
	src.fills = rest.FillsResult{Walk: completeWalk(), Fills: fills}

	s := newStartup(t, store, src, unresolvedFor(orderID), keepAll(),
		newSweeper(true))
	at := s.Step(context.Background(), startupNow)

	if at.Adoption != nil {
		t.Fatal("startup concluded while the ownership ledger held " +
			"unattributed fills. §7.5's licence to leave STARTING rests on " +
			"having attributed EVERY fill on the account, and these were not " +
			"attributed either way")
	}
	if at.Err == nil {
		t.Fatal("startup reported no error and no adoption")
	}
	if !at.Retry {
		t.Fatal("startup did not ask to retry; §7.5 retries indefinitely with " +
			"backoff, and the reservations are drained by lip-eyq's walk on a " +
			"later attempt")
	}
	if len(store.ensures) != 0 {
		t.Fatalf("startup wrote %d latch record(s) over an unresolved fill: "+
			"%+v. This is the finding: a durable WINDING_DOWN that only a "+
			"human can clear, produced by our own dispatched order",
			len(store.ensures), store.ensures)
	}
}
