package lifecycle

import (
	"context"
	"errors"
	"testing"

	"lip/harness/rest"
	"lip/harness/risk"
)

// ---------------------------------------------------------------------------
// A source that can tell startup's TWO orders walks apart
// ---------------------------------------------------------------------------

// resolveSource answers the two orders reads a startup pass makes differently,
// which `fakeSource` cannot: it returns the same slice whatever `status` it is
// handed.
//
// That distinction is the entire subject of this file. §7.5 step 2 reads
// `status=resting`, because an order that is not resting can be neither adopted
// nor cancelled. lip-eyq's reservation-resolving walk reads UNFILTERED -- no
// status at all -- because `ClientOrderID` is present on TERMINAL orders too,
// and a coid whose order has since executed or been cancelled is exactly the
// coid that must be BOUND rather than abandoned. A resting-only resolving walk
// would abandon every reservation whose order filled, which is a permanent
// record made against a live order.
//
// With a source that cannot express "listed by the unfiltered walk, absent from
// the resting one", that bug passes the suite. So this one can.
//
// It also counts the unfiltered reads, so "no outstanding reservations means no
// walk at all" is assertable rather than assumed.
type resolveSource struct {
	*fakeSource
	// unfiltered is what `Orders(ctx, "", "")` returns. Tests reassign it
	// between Steps to model a coid appearing or vanishing from the listing.
	unfiltered rest.OrdersResult
	// unfilteredCalls counts the resolving walks, including the ones that are
	// supposed never to happen.
	unfilteredCalls int
}

func newResolveSource() *resolveSource {
	return &resolveSource{fakeSource: okSource(), unfiltered: resolveListing()}
}

func (s *resolveSource) Orders(ctx context.Context, ticker,
	status string) rest.OrdersResult {

	// Recorded on the embedded source so the §7.5 read-ORDER assertions in the
	// rest of the package keep working against this type unchanged.
	s.calls = append(s.calls, call{what: "orders", ticker: ticker, status: status})
	if status != "" {
		return s.orders
	}
	s.unfilteredCalls++
	return s.unfiltered
}

// resolveListing is a COMPLETE unfiltered listing of exactly these orders.
//
// The EMPTY one is the case that matters most: a complete walk that mentions a
// coid nowhere is the only evidence there is that the exchange never took it,
// and H-PAGE-1 is why the walk has to be complete for that reading to hold.
func resolveListing(orders ...rest.Order) rest.OrdersResult {
	return rest.OrdersResult{Walk: completeWalk(), Orders: orders}
}

// resolveTerminalOrder is an order the exchange accepted and has since CLOSED.
//
// `Status` is deliberately NOT `rest.StatusResting`. H-ORD-6 commits the
// reservation before dispatch, so the crash window it covers is most often
// survived by an order that went on to fill -- and the exchange still lists it,
// coid and all, once it is terminal. A drain that only counted resting orders
// would treat this as an order the exchange never saw.
func resolveTerminalOrder(coid, orderID, ticker string) rest.Order {
	return rest.Order{
		OrderID: orderID, ClientOrderID: coid, Ticker: ticker,
		Status: rest.StatusExecuted, Ours: true,
	}
}

// resolveOutstanding is a resolver holding these coids as reservations that were
// committed and never resolved either way: what a SIGKILL between H-ORD-6's two
// commits leaves behind, and what nothing but this walk can drain.
func resolveOutstanding(coids ...string) *stubResolver {
	m := make(map[string]struct{}, len(coids))
	for _, c := range coids {
		m[c] = struct{}{}
	}
	return &stubResolver{outstanding: m}
}

// restingWalks counts the §7.5 step-2 reads, so a test asserting the resolving
// walk did NOT happen can still show that the pass read orders at all.
func restingWalks(src *resolveSource) int {
	n := 0
	for _, c := range src.calls {
		if c.what == "orders" && c.status == rest.StatusResting {
			n++
		}
	}
	return n
}

// ---------------------------------------------------------------------------
// The ordinary path: nothing outstanding, so nothing to resolve
// ---------------------------------------------------------------------------

// TestStartupMakesNoResolvingWalkWhenNoReservationIsOutstanding pins the cost
// of the drain on the path every startup actually takes.
//
// An unfiltered `GET /portfolio/orders` lists TERMINAL orders, which is every
// order the account has ever had inside the exchange's retention window. It is
// by far the most expensive read in the pass and it is paginated, so a version
// that ran it unconditionally would make every clean restart pay for a recovery
// that is not happening -- and would give H-PAGE-1 a fresh way to fail a startup
// that had nothing to resolve.
//
// The rule is therefore not "the walk is cheap", it is "the walk does not
// happen". `UnresolvedReservations()` empty means no walk at all.
func TestStartupMakesNoResolvingWalkWhenNoReservationIsOutstanding(t *testing.T) {
	src := newResolveSource()
	// A resolver with a nil outstanding set: the ledger has no reservation it
	// has neither bound nor abandoned, which is what every startup that did not
	// crash mid-dispatch looks like.
	resolver := &stubResolver{}
	s := newStartupResolving(t, &recordingLatch{}, src, ownsAll(), keepAll(),
		newSweeper(true), resolver)

	at := s.Step(context.Background(), startupNow)
	if at.Err != nil {
		t.Fatalf("the ordinary startup pass failed: %v", at.Err)
	}
	if at.Adoption == nil {
		t.Fatal("the ordinary startup pass returned no Adoption, so the " +
			"fixture is not exercising a complete reconciliation and the " +
			"assertions below would hold for a pass that did nothing")
	}
	if src.unfilteredCalls != 0 {
		t.Fatalf("startup made %d UNFILTERED orders walk(s) with no outstanding "+
			"reservation. That walk lists every terminal order on the account "+
			"and is the most expensive read in the pass; it exists only to drain "+
			"reservations, and with none outstanding there is nothing to drain",
			src.unfilteredCalls)
	}
	if got := restingWalks(src); got != 1 {
		t.Fatalf("the pass made %d resting-orders walk(s), want 1: the "+
			"unfiltered-walk assertion above only means something if §7.5's own "+
			"reads ran (calls %v)", got, describe(src.sequence()))
	}
	if len(resolver.bound) != 0 || len(resolver.abandoned) != 0 {
		t.Fatalf("startup wrote to the ownership ledger with nothing "+
			"outstanding: bound %v, abandoned %v", resolver.bound,
			resolver.abandoned)
	}
}

// ---------------------------------------------------------------------------
// Binding: the coid the exchange did take
// ---------------------------------------------------------------------------

// TestStartupBindsAListedTerminalReservationAndDoesNotAbandonIt is the half of
// the drain that must never fire the other way round.
//
// The scenario is H-ORD-6's crash window survived by a WINNING order: the
// reservation committed, the order was dispatched, the process died before the
// binding, and the order then FILLED. The exchange still lists it -- terminal,
// with our coid on it -- so the reservation is resolvable, and the only correct
// resolution is to bind it to the order id it actually became.
//
// Abandoning it instead would be the serious error, and it is not recoverable:
// abandonment is a permanent record saying the exchange never took this coid, so
// the fill on that order would afterwards read as a stranger's and latch a
// durable, operator-only WINDING_DOWN over our own trade (H-ORD-9, and the
// finding `TestStartupDoesNotLatchForeignFillWhileReservationsUnresolved` pins).
func TestStartupBindsAListedTerminalReservationAndDoesNotAbandonIt(t *testing.T) {
	const coid = "lipH-crashed-before-binding"
	const orderID = "ord-that-went-on-to-fill"

	src := newResolveSource()
	// Listed ONLY by the unfiltered walk, and terminal. The resting walk
	// (`src.orders`) stays empty, so nothing here can be explained by the order
	// still being open.
	src.unfiltered = resolveListing(resolveTerminalOrder(coid, orderID, "M"))
	resolver := resolveOutstanding(coid)

	s := newStartupResolving(t, &recordingLatch{}, src, ownsAll(), keepAll(),
		newSweeper(true), resolver)
	at := s.Step(context.Background(), startupNow)

	if src.unfilteredCalls != 1 {
		t.Fatalf("startup made %d unfiltered orders walk(s) with a reservation "+
			"outstanding, want exactly 1", src.unfilteredCalls)
	}
	want := coid + "|" + orderID
	if len(resolver.bound) != 1 || resolver.bound[0] != want {
		t.Fatalf("bound = %v, want [%s]: the exchange listed our coid on a "+
			"TERMINAL order, which is the exchange saying it took the order, and "+
			"binding it to that order id is the only resolution that keeps the "+
			"fill ours", resolver.bound, want)
	}
	if len(resolver.abandoned) != 0 {
		t.Fatalf("startup abandoned %v -- a reservation the exchange LISTED. "+
			"Abandonment is permanent and says the exchange never took the coid; "+
			"here it would disown the order's own fill and latch a durable "+
			"operator-only stop over our own trade", resolver.abandoned)
	}
	if hasClass(at.Anomalies, "ORDER_BINDING_NOT_SUBMITTED") {
		t.Fatalf("a binding that succeeded raised ORDER_BINDING_NOT_SUBMITTED "+
			"(anomalies %v)", classesOf(at.Anomalies))
	}
	if left := resolver.UnresolvedReservations(); len(left) != 0 {
		t.Fatalf("the reservation is still outstanding after a successful "+
			"binding: %v. Nothing else drains the set, so startup would defer "+
			"every unrecognised fill forever", left)
	}
	if at.Err != nil || at.Adoption == nil {
		t.Fatalf("startup did not conclude after the drain emptied the "+
			"unresolved set (err %v, adoption %v). Terminating is the entire "+
			"point of lip-eyq: H-ORD-5a's 'keep trying' is only livable because "+
			"something eventually resolves the reservation", at.Err, at.Adoption)
	}
}

// ---------------------------------------------------------------------------
// Abandonment: the coid the exchange never took
// ---------------------------------------------------------------------------

// TestStartupAbandonsOnlyAfterThreeConsecutiveCompleteMisses is the other half,
// and the delay is the safety.
//
// A coid absent from ONE complete unfiltered listing is not proof the exchange
// never took it. H-ORD-2a is the reason: an order can be accepted and not yet
// visible to the listing endpoint, so a single miss can be a race with the
// exchange's own read-after-write rather than an absence. Abandoning on that
// evidence writes a permanent "never taken" record about a live order, and every
// fill it later produces reads as a stranger's.
//
// So `resolveConfirmAttempts` complete walks must all miss it, CONSECUTIVELY,
// before the abandonment is submitted. This test drives the passes one at a time
// and asserts nothing is abandoned until the last one -- a version that
// abandoned on the first miss would satisfy an end-state-only assertion.
func TestStartupAbandonsOnlyAfterThreeConsecutiveCompleteMisses(t *testing.T) {
	const coid = "lipH-never-reached-the-exchange"

	// The threshold is part of the RULE, not a tuning knob, so it is pinned here
	// rather than only read. The loops below are written in terms of the
	// constant for legibility, and a suite that did nothing else would follow a
	// weakening down to 1 without a murmur -- which is abandon-on-first-miss,
	// the exact behaviour these tests exist to forbid.
	if resolveConfirmAttempts != 3 {
		t.Fatalf("resolveConfirmAttempts is %d, want 3: lowering it trades a "+
			"permanent, unrecoverable record against a possibly-live order for "+
			"a slightly faster startup", resolveConfirmAttempts)
	}

	src := newResolveSource()
	// A COMPLETE listing that mentions nothing. Complete is what makes the
	// absence evidence at all.
	src.unfiltered = resolveListing()
	resolver := resolveOutstanding(coid)

	s := newStartupResolving(t, &recordingLatch{}, src, ownsAll(), keepAll(),
		newSweeper(true), resolver)

	for pass := 1; pass < resolveConfirmAttempts; pass++ {
		at := s.Step(context.Background(), startupNow)
		if len(resolver.abandoned) != 0 {
			t.Fatalf("pass %d of %d abandoned %v. One complete listing that "+
				"omits a coid is not proof the exchange never took it "+
				"(H-ORD-2a); abandonment is permanent, so it waits for %d "+
				"consecutive misses", pass, resolveConfirmAttempts,
				resolver.abandoned, resolveConfirmAttempts)
		}
		if hasClass(at.Anomalies, "RESERVATION_ABANDONED") {
			t.Fatalf("pass %d raised RESERVATION_ABANDONED before the "+
				"confirmations were in: %v", pass, classesOf(at.Anomalies))
		}
	}

	at := s.Step(context.Background(), startupNow)
	if len(resolver.abandoned) != 1 || resolver.abandoned[0] != coid {
		t.Fatalf("after %d consecutive complete misses abandoned = %v, want "+
			"[%s]. Nothing else drains a reservation the exchange never took, "+
			"so leaving it outstanding makes every unrecognised fill defer "+
			"forever and startup never concludes (H-ORD-5a)",
			resolveConfirmAttempts, resolver.abandoned, coid)
	}
	if !hasClass(at.Anomalies, "RESERVATION_ABANDONED") {
		t.Fatalf("the abandonment was silent: %v. A permanent record that the "+
			"exchange never took one of our coids is an operator-visible event",
			classesOf(at.Anomalies))
	}
	if sev, ok := sevOf(at.Anomalies, "RESERVATION_ABANDONED"); !ok ||
		sev != risk.SEV2 {

		t.Fatalf("RESERVATION_ABANDONED is %v, want SEV2", sev)
	}
	if len(resolver.bound) != 0 {
		t.Fatalf("a coid absent from every listing was BOUND: %v", resolver.bound)
	}
	if src.unfilteredCalls != resolveConfirmAttempts {
		t.Fatalf("the drain made %d unfiltered walks over %d passes; each pass "+
			"must re-read, because the confirmation counts INDEPENDENT listings "+
			"and not repeats of one cached answer", src.unfilteredCalls,
			resolveConfirmAttempts)
	}
}

// TestStartupResetsAReservationsMissCounterWhenTheCoidReappears is why the
// counter is described as CONSECUTIVE and not as a tally.
//
// A cumulative counter is the plausible wrong implementation, and it is wrong in
// the dangerous direction. Consider a coid the exchange has, listed
// intermittently -- a replica lagging, a page boundary moving under a rewalk,
// H-ORD-2a's accept-then-appear window straddling a restart. Every miss it ever
// suffers accumulates, so after enough restarts a LIVE order's reservation is
// abandoned on the strength of misses separated by listings that proved the
// order was there all along.
//
// The rule is that a coid the exchange demonstrably still knows about starts
// over from zero. Here the coid is missed twice, listed once, and then missed
// twice again: a tally would abandon on the fourth pass, and the correct
// implementation abandons on the sixth.
func TestStartupResetsAReservationsMissCounterWhenTheCoidReappears(t *testing.T) {
	const coid = "lipH-intermittently-listed"
	const orderID = "ord-real-all-along"

	src := newResolveSource()
	src.unfiltered = resolveListing()
	resolver := resolveOutstanding(coid)

	s := newStartupResolving(t, &recordingLatch{}, src, ownsAll(), keepAll(),
		newSweeper(true), resolver)

	// Passes 1 and 2: absent. One short of the confirmation threshold.
	for pass := 1; pass < resolveConfirmAttempts; pass++ {
		s.Step(context.Background(), startupNow)
		if len(resolver.abandoned) != 0 {
			t.Fatalf("pass %d abandoned early: %v", pass, resolver.abandoned)
		}
	}

	// Pass 3: the exchange lists it. This is the evidence that resets the
	// counter -- the coid is one the exchange took after all.
	src.unfiltered = resolveListing(resolveTerminalOrder(coid, orderID, "M"))
	s.Step(context.Background(), startupNow)
	if len(resolver.abandoned) != 0 {
		t.Fatalf("the pass that LISTED the coid abandoned it: %v",
			resolver.abandoned)
	}
	if len(resolver.bound) != 1 {
		t.Fatalf("bound = %v, want the one listed coid", resolver.bound)
	}

	// Re-arm the outstanding set. `stubResolver.BindListedOrder` removes the
	// coid on success, and the subject of this test is the harness's per-coid
	// MISS COUNTER rather than the ledger's bookkeeping: the counter must have
	// been cleared by the listing, and that is only observable if the coid is
	// still outstanding to be missed again. The ledger legitimately reports a
	// reservation as outstanding after a submitted binding, because the binding
	// is not visible until it commits -- which is the same asynchrony §7.5's
	// unresolved branch is written around.
	resolver.outstanding[coid] = struct{}{}
	src.unfiltered = resolveListing()

	// Passes 4 and 5: absent again. A cumulative tally reaches
	// `resolveConfirmAttempts` on pass 4 and abandons a coid the exchange listed
	// one pass earlier.
	for pass := resolveConfirmAttempts + 1; pass < 2*resolveConfirmAttempts; pass++ {
		s.Step(context.Background(), startupNow)
		if len(resolver.abandoned) != 0 {
			t.Fatalf("pass %d abandoned %v. Its misses were separated by a "+
				"listing that showed the exchange DOES have this coid, so the "+
				"count restarts there; a lifetime tally abandons a live "+
				"reservation on the strength of intermittent visibility",
				pass, resolver.abandoned)
		}
	}

	// Pass 6: the third CONSECUTIVE miss since the listing.
	s.Step(context.Background(), startupNow)
	if len(resolver.abandoned) != 1 || resolver.abandoned[0] != coid {
		t.Fatalf("abandoned = %v after %d consecutive misses following the "+
			"listing, want [%s]. Resetting the counter must not disable the "+
			"drain -- a counter that never reaches the threshold again leaves "+
			"the reservation outstanding forever and startup never concludes",
			resolver.abandoned, resolveConfirmAttempts, coid)
	}
}

// ---------------------------------------------------------------------------
// Incomplete listings prove nothing
// ---------------------------------------------------------------------------

// TestStartupAbandonsNothingWhenTheResolvingWalkIsIncomplete is H-PAGE-1 applied
// to the one record that cannot be taken back.
//
// H-PAGE-1's rule is that an incomplete read is STALE, NEVER EMPTY. Everywhere
// else in the harness that costs a retry. Here it would cost an order: a
// truncated listing omits coids the exchange holds, and reading that omission as
// absence writes a permanent "never taken" record against a live reservation,
// after which the order's fills read as a stranger's and latch a durable stop.
//
// So an incomplete resolving walk abandons NOTHING, increments NOTHING, and
// fails the pass into §7.5's indefinite retry. The pass is driven more times
// than `resolveConfirmAttempts` because a version that counted incomplete walks
// as misses would look correct on one pass and abandon on the third.
func TestStartupAbandonsNothingWhenTheResolvingWalkIsIncomplete(t *testing.T) {
	const coid = "lipH-unknown-because-unread"

	src := newResolveSource()
	src.unfiltered = rest.OrdersResult{Walk: failedWalk("page 2 timed out")}
	resolver := resolveOutstanding(coid)
	store := &recordingLatch{}

	s := newStartupResolving(t, store, src, ownsAll(), keepAll(),
		newSweeper(true), resolver)

	for pass := 1; pass <= resolveConfirmAttempts+1; pass++ {
		at := s.Step(context.Background(), startupNow)
		if at.Err == nil {
			t.Fatalf("pass %d concluded on a truncated resolving walk. The "+
				"reservation is still unresolved, so any fill on an "+
				"unrecognised order id is still neither ours nor foreign and "+
				"this pass has not reconciled", pass)
		}
		if !at.Retry {
			t.Fatalf("pass %d did not ask to retry. §7.5 retries indefinitely "+
				"with backoff; there is no terminal outcome, and a drain that "+
				"gave up would leave the reservation outstanding forever", pass)
		}
		if len(resolver.abandoned) != 0 {
			t.Fatalf("pass %d abandoned %v on an INCOMPLETE listing. H-PAGE-1: "+
				"a truncated read is stale, never empty -- it cannot show that "+
				"the exchange never took a coid, and abandonment is permanent",
				pass, resolver.abandoned)
		}
		if len(resolver.bound) != 0 {
			t.Fatalf("pass %d bound %v from a walk that never returned its "+
				"orders", pass, resolver.bound)
		}
	}

	if len(store.ensures) != 0 {
		t.Fatalf("a failed read wrote %d latch record(s): %+v. An unreadable "+
			"listing is a truth failure, not a discovered stop cause, and the "+
			"latch it would write is durable and operator-only",
			len(store.ensures), store.ensures)
	}
}

// ---------------------------------------------------------------------------
// A binding that could not be submitted
// ---------------------------------------------------------------------------

// TestStartupKeepsAReservationOutstandingWhenTheBindingCannotBeSubmitted covers
// the drain failing at the write.
//
// The exchange has answered: it listed the coid, so the reservation is ours and
// belongs to that order id. Only the ledger write failed. The two things that
// must NOT happen are the two convenient ones -- treating the reservation as
// resolved because we know the answer (the answer is not durable, so a restart
// does not have it), or counting the coid as missed and eventually abandoning it
// (it was LISTED; the exchange took it).
//
// So the reservation stays outstanding, the failure is raised as SEV2
// `ORDER_BINDING_NOT_SUBMITTED`, and startup keeps deferring rather than
// concluding anything about unrecognised fills. The later passes here are the
// load-bearing part: a listed-but-unbound coid must not drift into abandonment
// however long the ledger stays broken.
func TestStartupKeepsAReservationOutstandingWhenTheBindingCannotBeSubmitted(
	t *testing.T) {

	const coid = "lipH-listed-but-unwritable"
	const orderID = "ord-known-to-the-exchange"

	src := newResolveSource()
	src.unfiltered = resolveListing(resolveTerminalOrder(coid, orderID, "M"))
	resolver := resolveOutstanding(coid)
	resolver.bindErr = errors.New("ownership store: disk full")

	s := newStartupResolving(t, &recordingLatch{}, src, ownsAll(), keepAll(),
		newSweeper(true), resolver)
	at := s.Step(context.Background(), startupNow)

	if !hasClass(at.Anomalies, "ORDER_BINDING_NOT_SUBMITTED") {
		t.Fatalf("a binding failure was silent: %v. The reservation is still "+
			"outstanding and every unrecognised fill keeps deferring, so a "+
			"startup that cannot progress must say why", classesOf(at.Anomalies))
	}
	if sev, ok := sevOf(at.Anomalies, "ORDER_BINDING_NOT_SUBMITTED"); !ok ||
		sev != risk.SEV2 {

		t.Fatalf("ORDER_BINDING_NOT_SUBMITTED is %v, want SEV2", sev)
	}
	if len(resolver.bound) != 0 {
		t.Fatalf("bound = %v after the submission failed; nothing was written",
			resolver.bound)
	}
	left := resolver.UnresolvedReservations()
	if _, still := left[coid]; !still {
		t.Fatalf("the reservation was dropped from the outstanding set after a "+
			"FAILED binding (%v). The binding exists only in memory, so a "+
			"restart would find an unbound reservation again -- and in the "+
			"meantime a fill on %s would read as a stranger's and latch a "+
			"durable operator-only stop", left, orderID)
	}

	// The ledger stays broken and the coid stays listed. It must never be
	// abandoned: the drain's two ends answer different questions, and "the write
	// failed" is not "the exchange never took it".
	for pass := 2; pass <= resolveConfirmAttempts+1; pass++ {
		s.Step(context.Background(), startupNow)
		if len(resolver.abandoned) != 0 {
			t.Fatalf("pass %d abandoned %v -- a coid the unfiltered walk LISTED "+
				"on every pass. A permanent 'the exchange never took this' "+
				"record produced by our own failure to write a binding disowns "+
				"a real order", pass, resolver.abandoned)
		}
	}
}
