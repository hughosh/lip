package lifecycle

import (
	"context"
	"testing"

	"lip/harness/rest"
)

// TestASightingClearsTheMissCounterEvenWithoutAnOrderID pins the ordering
// inside the resolving walk's bind loop.
//
// The walk decides two different things about a listed coid, and they rest on
// different evidence:
//
//   - whether to BIND it, which needs an order id, because a binding with an
//     empty one classifies nothing (H-ORD-9 classifies fills BY order id); and
//   - whether it was SEEN, which needs only the coid, because the exchange
//     listing a coid at all is the proof that it took the order.
//
// Conflating them puts the miss-counter reset behind the order-id guard, and
// the result is a run of "consecutive" misses that is not consecutive: the
// sighting suppresses the increment (the coid is in `listed`) but does not
// clear the two misses before it, so ONE further miss reaches
// `resolveConfirmAttempts` and abandons.
//
// That matters because abandonment is not a retry. `ResolveReservationAbandoned`
// is permanent, the store refuses to bind an abandoned coid afterwards, and the
// reservation exists precisely because H-ORD-6 committed it BEFORE dispatch. So
// the failure is an order of ours, which the exchange listed, recorded forever
// as one it never took -- decided on evidence the sighting had already refuted.
//
// H-ORD-2a is the same rule from the other side: absence is not evidence in
// either direction, and it certainly does not outrank a positive
// identification we already hold.
func TestASightingClearsTheMissCounterEvenWithoutAnOrderID(t *testing.T) {
	const coid = "lipH-abandon-me"

	src := newResolveSource()
	resolver := resolveOutstanding(coid)
	s := newStartupResolving(t, &recordingLatch{}, src, ownsAll(), keepAll(),
		newSweeper(true), resolver, "M")

	// Two complete walks that do not mention it at all.
	for i := 1; i <= 2; i++ {
		src.unfiltered = resolveListing()
		if at := s.Step(context.Background(), startupNow); at.Err != nil {
			t.Fatalf("miss pass %d: %v", i, at.Err)
		}
		if n := len(resolver.abandoned); n != 0 {
			t.Fatalf("abandoned after %d miss(es): %v; %d consecutive "+
				"complete misses are required", i, resolver.abandoned, 3)
		}
	}

	// The exchange now lists it, but with NO order id. Nothing can be bound
	// from this, and nothing should be: the only claim it supports is that the
	// exchange has heard of this coid. That claim is enough to reset the run.
	src.unfiltered = resolveListing(rest.Order{
		ClientOrderID: coid, Ticker: "M", Status: rest.StatusExecuted, Ours: true,
	})
	if at := s.Step(context.Background(), startupNow); at.Err != nil {
		t.Fatalf("sighting pass: %v", at.Err)
	}
	if n := len(resolver.bound); n != 0 {
		t.Fatalf("bound %v from a listing with no order id; a binding with an "+
			"empty order id classifies nothing", resolver.bound)
	}
	if n := len(resolver.abandoned); n != 0 {
		t.Fatalf("abandoned %v on the very walk that SAW the coid",
			resolver.abandoned)
	}

	// One more miss. That is one, not three: the sighting reset the run.
	src.unfiltered = resolveListing()
	if at := s.Step(context.Background(), startupNow); at.Err != nil {
		t.Fatalf("post-sighting miss pass: %v", at.Err)
	}
	if n := len(resolver.abandoned); n != 0 {
		t.Fatalf("reservation %s was abandoned after miss, miss, SIGHTING, "+
			"miss -- which is one consecutive miss, not %d. The exchange "+
			"listed this coid, so it took the order; abandoning it writes a "+
			"permanent record that it never did, and the store will refuse to "+
			"bind it ever after (abandoned: %v)",
			coid, resolveConfirmAttempts, resolver.abandoned)
	}
}
