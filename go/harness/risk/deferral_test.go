package risk

import (
	"testing"

	"lip/harness/quote"
)

// TestUnresolvedFillIsDeferredNotSeenNotForeign is the consumer half of
// FINDING 2, and the assertion that matters is the one about `seenTrade`.
//
// When the ledger cannot conclude, there are three things this must NOT do, and
// each of them is unrecoverable in a different way:
//
//   - apply it: a stranger's contracts enter q, and H-POS-1's overwrite will
//     then read as drift on a market we do not hold;
//   - disown it: `SEV1 FOREIGN_FILL`, a global stop and a durable WINDING_DOWN
//     latch, declared about our own dispatched order;
//   - mark it seen: the quietest of the three and the worst. `GET
//     /portfolio/fills` re-offers the whole walk every poll, so the dedup set is
//     the ONLY thing standing between a deferral and a permanent silent drop.
//     A fill marked seen while unresolved is never applied, never reported and
//     never looked at again -- the contracts are held, q never learns, and no
//     anomaly is ever raised.
//
// So the test defers a fill and then answers the same fill again with a ledger
// that has since committed the binding. `M-R-INDETSEEN` marks it seen on the
// first pass, and the second pass then finds nothing to do.
func TestUnresolvedFillIsDeferredNotSeenNotForeign(t *testing.T) {
	const tk = "KXTEST-D"
	p := NewPortfolio()

	f := FillEvent{TradeID: "t1", OrderID: "ord-dispatched", Ticker: tk,
		Side: quote.SideYes, Count: contracts(5)}

	// --- poll 1: the binding has not committed yet --------------------------
	eff := p.ApplyFills([]FillEvent{f}, unresolves("ord-dispatched"), Live,
		pollMs)

	if len(eff.Deferred) != 1 || eff.Deferred[0].TradeID != "t1" {
		t.Fatalf("Deferred = %+v, want the one unresolved fill", eff.Deferred)
	}
	if len(eff.Owned) != 0 {
		t.Fatalf("an unresolved fill was applied as ours: %+v", eff.Owned)
	}
	if len(eff.Foreign) != 0 {
		t.Fatalf("an unresolved fill was disowned: %+v. That is a SEV1, a "+
			"global stop and a durable WINDING_DOWN latch produced by our own "+
			"order -- the exact catastrophe H-ORD-9 forbids", eff.Foreign)
	}
	if eff.Stop {
		t.Fatal("an unresolved fill requested a global stop; the ledger has " +
			"not concluded anything, and H-ORD-5a's response to not knowing " +
			"is to keep trying")
	}
	if hasClass(eff.Anomalies, "FOREIGN_FILL") {
		t.Fatalf("FOREIGN_FILL raised for an unresolved fill: %v",
			anomalyClasses(eff.Anomalies))
	}
	if len(eff.Anomalies) != 0 {
		t.Fatalf("a deferral inside the escalation window is not an event; "+
			"anomalies = %v", anomalyClasses(eff.Anomalies))
	}
	if eff.Incomplete {
		t.Fatal("a walk that deferred was reported Incomplete. The EXCHANGE " +
			"read succeeded and the fills endpoint is current; withholding " +
			"the freshness stamp would age truth out and stop dispatch over a " +
			"condition that is normal for a second after every reservation")
	}
	if got := p.Q(tk); got != 0 {
		t.Fatalf("q = %s after deferring the only fill, want 0", got.Wire())
	}

	// --- poll 2: the same walk, and the binding has now committed -----------
	//
	// The fills endpoint returns the identical trade again, because it always
	// does. This is the whole point of not marking it seen.
	eff2 := p.ApplyFills([]FillEvent{f}, owns("ord-dispatched"), Live, pollMs)

	if len(eff2.Owned) != 1 {
		t.Fatalf("the re-offered fill was not applied once its binding "+
			"committed (Owned = %+v, Deferred = %+v). A deferred fill that was "+
			"marked seen is a fill no later poll can ever apply: the contracts "+
			"are held and q never learns", eff2.Owned, eff2.Deferred)
	}
	if len(eff2.Deferred) != 0 {
		t.Fatalf("still deferred after the ledger answered: %+v", eff2.Deferred)
	}
	if got := p.Q(tk); got != contracts(5) {
		t.Fatalf("q = %s after the resolved fill applied, want 5.00",
			got.Wire())
	}

	// --- poll 3: and now the ordinary dedup takes over ----------------------
	p.ApplyFills([]FillEvent{f}, owns("ord-dispatched"), Live, pollMs)
	if got := p.Q(tk); got != contracts(5) {
		t.Fatalf("q = %s after the walk re-delivered an applied fill, want "+
			"5.00; deferral must not have disabled trade_id dedup", got.Wire())
	}
}

// TestDeferredFillEscalatesSev2AfterDeadline is the other half of the same
// decision: deferring is honest, and deferring silently forever is not.
//
// §7.2 handles an UNKNOWN market state the same way -- legitimate briefly, a
// fault if it persists -- and the response there is to raise the volume rather
// than to invent a decision. So after `unclassifiedFillEscalateMs` the operator
// is told, ONCE per trade_id, and the fill goes on being deferred.
//
// The two things this pins that a looser test would not:
//
//   - it does NOT convert. No SEV1, no foreign, no stop, not ever. A timer that
//     eventually calls the fill foreign is FINDING 2 with a delay.
//   - it fires once. A fill stuck for an hour at a 5s cadence would otherwise
//     produce 720 anomalies, and §13's bucket is not what should be absorbing
//     that.
func TestDeferredFillEscalatesSev2AfterDeadline(t *testing.T) {
	const tk = "KXTEST-E"
	p := NewPortfolio()
	l := unresolves("ord-stuck")
	f := FillEvent{TradeID: "t1", OrderID: "ord-stuck", Ticker: tk,
		Side: quote.SideYes, Count: contracts(2)}

	// First sighting: the clock starts here, and starting it is not an event.
	if eff := p.ApplyFills([]FillEvent{f}, l, Live, pollMs); len(eff.Anomalies) != 0 {
		t.Fatalf("the first deferral raised %v", anomalyClasses(eff.Anomalies))
	}

	// One millisecond inside the window. Still nothing.
	eff := p.ApplyFills([]FillEvent{f}, l, Live,
		pollMs+unclassifiedFillEscalateMs-1)
	if len(eff.Anomalies) != 0 {
		t.Fatalf("a deferral %dms old raised %v; the window is %dms",
			unclassifiedFillEscalateMs-1, anomalyClasses(eff.Anomalies),
			unclassifiedFillEscalateMs)
	}
	if len(eff.Deferred) != 1 {
		t.Fatalf("Deferred = %+v inside the window", eff.Deferred)
	}

	// On the deadline.
	eff = p.ApplyFills([]FillEvent{f}, l, Live, pollMs+unclassifiedFillEscalateMs)
	if !hasClass(eff.Anomalies, "FILL_UNCLASSIFIABLE") {
		t.Fatalf("a fill unresolved for %dms raised %v, want "+
			"FILL_UNCLASSIFIABLE: deferring is the right answer and deferring "+
			"in silence is how a held position stays invisible",
			unclassifiedFillEscalateMs, anomalyClasses(eff.Anomalies))
	}
	if sev := sevOf(t, eff.Anomalies, "FILL_UNCLASSIFIABLE"); sev != SEV2 {
		t.Fatalf("FILL_UNCLASSIFIABLE is %v, want SEV2. SEV1 is for a third "+
			"party trading the account, and an unattributed fill is precisely "+
			"the failure to establish that", sev)
	}
	if len(eff.Foreign) != 0 || eff.Stop {
		t.Fatalf("the deadline converted the fill: Foreign = %+v, Stop = %v. "+
			"A timer that eventually calls an unresolved fill foreign is the "+
			"original defect with a delay on it", eff.Foreign, eff.Stop)
	}
	if len(eff.Deferred) != 1 {
		t.Fatalf("the escalated fill stopped being deferred: %+v", eff.Deferred)
	}
	if len(eff.Owned) != 0 {
		t.Fatalf("the deadline applied the fill as ours: %+v", eff.Owned)
	}

	// Long past it: reported once, still deferred.
	eff = p.ApplyFills([]FillEvent{f}, l, Live,
		pollMs+10*unclassifiedFillEscalateMs)
	if hasClass(eff.Anomalies, "FILL_UNCLASSIFIABLE") {
		t.Fatalf("FILL_UNCLASSIFIABLE raised a second time for the same "+
			"trade_id; at a 5s cadence a fill stuck for an hour would produce "+
			"720 of them (%v)", anomalyClasses(eff.Anomalies))
	}
	if len(eff.Deferred) != 1 {
		t.Fatalf("the fill stopped being deferred after escalating: %+v",
			eff.Deferred)
	}

	// And resolving it applies the fill and clears the escalation, so a LATER
	// unresolved fill on the same trade id would be reported afresh.
	eff = p.ApplyFills([]FillEvent{f}, owns("ord-stuck"), Live,
		pollMs+11*unclassifiedFillEscalateMs)
	if len(eff.Owned) != 1 {
		t.Fatalf("the escalated fill was not applied once the ledger "+
			"answered: %+v", eff)
	}
	if got := p.Q(tk); got != contracts(2) {
		t.Fatalf("q = %s, want 2.00", got.Wire())
	}
}
