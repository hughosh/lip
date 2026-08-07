package wsx

import (
	"context"
	"errors"
	"testing"
	"time"

	"lip/harness/num"
	"lip/harness/quote"
	"lip/harness/rest"
	"lip/harness/risk"
)

// TestPortfolioPollRunsImmediatelyThenEveryFiveSecondsDuringWSOutage is H-POS-1's
// cadence and H-FAIL-2's independence, together.
//
// Three properties:
//
//   - a new reconciliation token polls IMMEDIATELY. H-ORD-5's reconciliation is
//     what unlocks placement after a reconnect, so waiting up to a full interval
//     for it leaves every market non-actionable for that long, every time.
//   - the cadence continues WHILE THE SOCKET IS DOWN. That is the whole point:
//     position truth during an outage is the only thing that can say whether
//     the inventory nobody is watching is moving. The poller in this test never
//     sees a second token and never stops.
//   - a cycle that overruns the interval coalesces into ONE immediate catch-up
//     rather than a queue. At a five-second cadence a backlog of overdue polls
//     is a backlog of answers about the past, which is not monitoring.
func TestPortfolioPollRunsImmediatelyThenEveryFiveSecondsDuringWSOutage(t *testing.T) {
	clk := newFakeClock()
	src := newScriptedPortfolio()
	p := testParams()
	pol, err := NewPoller(src, clk, p.PositionPoll)
	if err != nil {
		t.Fatal(err)
	}

	tokens := make(chan ReconcileToken, 1)
	out := make(chan PortfolioRead, 32)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- pol.Run(ctx, tokens, out) }()

	clk.BlockUntilTimers(1)

	g, err := NewGate([]string{fxTicker}, p)
	if err != nil {
		t.Fatal(err)
	}
	tok := g.OnConnect(clk.Now()).Token

	tokens <- tok
	first := <-out
	if first.Seq() != 1 {
		t.Fatalf("first poll seq = %d, want 1", first.Seq())
	}
	if first.CompletedAt().Mono != 0 {
		t.Fatalf("the token did not poll immediately: first poll at %v",
			first.CompletedAt().Mono)
	}

	// No further tokens: the socket is down for the rest of this test.
	for i := 2; i <= 4; i++ {
		clk.Advance(p.PositionPoll)
		read := <-out
		if read.Seq() != uint64(i) {
			t.Fatalf("poll %d arrived with seq %d", i, read.Seq())
		}
		want := time.Duration(i-1) * p.PositionPoll
		if read.CompletedAt().Mono != want {
			t.Fatalf("poll %d at %v, want %v -- the cadence must not stop "+
				"because the websocket did", i, read.CompletedAt().Mono, want)
		}
	}

	// A cycle that overruns the interval. The source advances the clock by
	// more than one interval while it is answering, for that cycle only.
	src.setDelay(clk, 7*time.Second)
	clk.Advance(p.PositionPoll)
	slow := <-out
	if slow.Seq() != 5 {
		t.Fatalf("slow poll seq = %d, want 5", slow.Seq())
	}

	// Exactly one catch-up, with no further clock movement.
	catchUp := <-out
	if catchUp.Seq() != 6 {
		t.Fatalf("catch-up seq = %d, want 6", catchUp.Seq())
	}
	if catchUp.CompletedAt().Mono != slow.CompletedAt().Mono {
		t.Fatalf("the catch-up poll waited %v; a cycle that overran its "+
			"interval owes exactly one immediate tick",
			catchUp.CompletedAt().Mono-slow.CompletedAt().Mono)
	}
	select {
	case extra := <-out:
		t.Fatalf("a second catch-up poll ran (seq %d); the missed ticks "+
			"queued instead of coalescing", extra.Seq())
	default:
	}

	cancel()
	if err := <-done; err == nil {
		t.Fatal("Run returned nil after its context was cancelled")
	}
}

// TestIncompletePortfolioReadPreservesStateAndFreshness is H-PAGE-1 applied to
// all three endpoints at once, and the independence between them.
//
// "Stale, never empty." An incomplete walk leaves the previous reading in place
// AND leaves that endpoint's freshness clock where it was, so a persistent
// failure ages the endpoint out and stops placement rather than silently
// serving an old answer as a current one.
//
// The three endpoints are applied INDEPENDENTLY. A failed orders walk must not
// withhold a good positions walk: they answer different questions and fail for
// different reasons, and coupling them lets one flaky endpoint stop all
// placement.
func TestIncompletePortfolioReadPreservesStateAndFreshness(t *testing.T) {
	p := testParams()
	g, err := NewGate([]string{fxTicker}, p)
	if err != nil {
		t.Fatal(err)
	}
	pf := risk.NewPortfolio()
	own := owns("ord-1")
	tok := g.OnConnect(at(0)).Token

	good := newRead(tok, at(0), 1,
		completePositions(map[string]num.Qty{fxTicker: contracts(3)}),
		completeOrders([]rest.Order{{
			OrderID: "ord-1", Ticker: fxTicker, Side: quote.SideYes,
			Price4: cents4(42), Remaining: contracts(5), Ours: true,
		}}),
		completeFills([]rest.Fill{
			restFill("t1", "ord-1", fxTicker, quote.SideYes, 3, "0.0000", false),
		}))
	eff := ApplyPortfolio(g, pf, own, nil, good, risk.Seed, at(0).Mono, p)
	for k := Truth(0); k < truthCount; k++ {
		if !eff.Applied[k] {
			t.Fatalf("%v was not applied from a complete read", k)
		}
	}
	if got := pf.Q(fxTicker); got != contracts(3) {
		t.Fatalf("q = %s, want 3.00", got.Wire())
	}
	if len(pf.LiveOrders()) != 1 {
		t.Fatalf("live orders = %v", pf.LiveOrders())
	}

	// Now only the ORDERS walk fails, and the exchange reports a different
	// position. Positions and fills must still land.
	partial := newRead(tok, at(5), 2,
		completePositions(map[string]num.Qty{fxTicker: contracts(4)}),
		rest.OrdersResult{Walk: failedWalk("transport reset")},
		completeFills(nil))
	eff = ApplyPortfolio(g, pf, own, nil, partial, risk.Live, at(5).Mono, p)

	if !eff.Applied[TruthPositions] || !eff.Applied[TruthFills] {
		t.Fatalf("a failed orders walk withheld the other two endpoints: %v",
			eff.Applied)
	}
	if eff.Applied[TruthOrders] {
		t.Fatal("a failed orders walk was applied")
	}
	if got := pf.Q(fxTicker); got != contracts(4) {
		t.Fatalf("q = %s, want the exchange's 4.00 -- a failed ORDERS walk "+
			"says nothing about the position", got.Wire())
	}
	if got := pf.LiveOrders(); len(got) != 1 || got[0].OrderID != "ord-1" {
		t.Fatalf("live orders = %v, want the previous reading preserved; an "+
			"order we merely failed to read is still live and still fillable",
			got)
	}
	if !hasClass(eff.Anomalies, "PORTFOLIO_READ_INCOMPLETE") {
		t.Fatalf("a failed walk was silent: %v", classesOf(eff.Anomalies))
	}

	// The orders freshness clock did NOT advance, so it will age out while the
	// other two stay current.
	if age := g.TruthAge(TruthOrders, at(5)); age != 5*time.Second {
		t.Fatalf("orders truth age = %v, want 5s (unchanged since the last "+
			"SUCCESS at t=0)", age)
	}
	if age := g.TruthAge(TruthPositions, at(5)); age != 0 {
		t.Fatalf("positions truth age = %v, want 0", age)
	}

	// A read carrying a token from a RETIRED generation is discarded wholesale
	// -- not even its timestamp is credited. Two generation changes have
	// happened since `tok` was issued (the disconnect and the reconnect), so it
	// is two behind.
	g.ApplyDisconnect(at(6), false)
	g.OnConnect(at(7))
	late := good
	late.completedAt = at(8)
	late.seq = 3
	lateEff := ApplyPortfolio(g, pf, own, nil, late, risk.Live, at(8).Mono, p)
	if !lateEff.Stale {
		t.Fatal("a read carrying a pre-disconnect token was applied")
	}
	for k := Truth(0); k < truthCount; k++ {
		if lateEff.Applied[k] {
			t.Fatalf("%v was applied from a stale read", k)
		}
	}
	if !hasClass(lateEff.Anomalies, "RECONCILE_TOKEN_STALE") {
		t.Fatalf("a stale read was silent: %v", classesOf(lateEff.Anomalies))
	}
}

// TestAnyStalePortfolioEndpointStopsAllPlacementButNotCancel is A13 and
// H-FAIL-4.
//
// ANY of the three, not just positions. Positions answering while fills have
// been erroring for two minutes is a state in which we know our size and not
// what produced it, and a single "portfolio is fresh" flag reports that as
// healthy -- which is the shape of every monitor that has ever gone quiet
// while the risk was live.
//
// What stops is PLACEMENT. Cancelling does not, and must not: every stop path
// stops adding risk, none stops reducing it (I1). A gate that withheld cancels
// when truth went stale would leave orders resting precisely because we had
// stopped being able to see them.
func TestAnyStalePortfolioEndpointStopsAllPlacementButNotCancel(t *testing.T) {
	p := testParams()
	stale := p.TruthMaxAge + time.Second

	for _, k := range []Truth{TruthPositions, TruthOrders, TruthFills} {
		t.Run(k.String(), func(t *testing.T) {
			g, err := NewGate([]string{fxTicker}, p)
			if err != nil {
				t.Fatal(err)
			}
			tok := g.OnConnect(at(0)).Token
			g.ApplyFrame(snapFrame(fxTicker), okHandle, at(0))
			for j := Truth(0); j < truthCount; j++ {
				g.noteTruth(j, tok, at(0))
			}

			now := Stamp{Mono: stale}
			// Refresh every endpoint EXCEPT k.
			for j := Truth(0); j < truthCount; j++ {
				if j != k {
					g.noteTruth(j, tok, now)
				}
			}
			g.ApplyFrame(deltaFrame(fxTicker), okHandle, now)

			if g.Actionable(fxTicker, now) {
				t.Fatalf("placement is licensed while %v truth is %v old, "+
					"past truth_max_age_s %v", k, stale, p.TruthMaxAge)
			}
			if !g.CancelPermitted(fxTicker, now) {
				t.Fatalf("stale %v truth withheld a CANCEL; every stop path "+
					"stops adding risk and none stops reducing it (I1)", k)
			}

			// Refreshing the missing one restores placement, which is what
			// makes the previous assertion about THIS endpoint and not about
			// something else being wrong.
			g.noteTruth(k, tok, now)
			if !g.Actionable(fxTicker, now) {
				t.Fatalf("refreshing %v did not restore placement", k)
			}
		})
	}

	// Cancels survive every other reason the gate closes, too.
	g, err := NewGate([]string{fxTicker}, p)
	if err != nil {
		t.Fatal(err)
	}
	if !g.CancelPermitted(fxTicker, at(0)) {
		t.Fatal("a never-connected gate withheld a cancel; H-CLOSE-3's final " +
			"cancel and every stranded-order sweep run through this path")
	}
	connectAndReconcile(g, fxTicker, at(0))
	g.ApplyDisconnect(at(1), false)
	if !g.CancelPermitted(fxTicker, at(1)) {
		t.Fatal("a disconnected gate withheld a cancel")
	}
}

// TestFillConversionFailureDiscardsTheWholeWalk is the "convert every fill
// before applying any" rule.
//
// `fee_cost` is H-ORD-8's INDEPENDENT corroborator: maker fees are $0.00 (S2),
// so a non-zero fee is a taker fill whatever `is_taker` claims, and the two
// detectors fail for different reasons. A fee we cannot read is one of the two
// witnesses to the most expensive bug in the system going silent.
//
// Half-applying the walk would be worse than discarding it: q would describe a
// prefix of the account's history while the dedup set claimed the whole of it,
// and no later poll could repair that.
func TestFillConversionFailureDiscardsTheWholeWalk(t *testing.T) {
	p := testParams()
	for _, tc := range []struct {
		name string
		fee  string
	}{
		{"missing fee_cost", ""},
		{"malformed fee_cost", "not-a-number"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g, err := NewGate([]string{fxTicker}, p)
			if err != nil {
				t.Fatal(err)
			}
			pf := risk.NewPortfolio()
			tok := g.OnConnect(at(0)).Token

			read := newRead(tok, at(0), 1, completePositions(nil), completeOrders(nil), completeFills([]rest.Fill{
				restFill("t1", "o", fxTicker, quote.SideYes, 2, "0.0000", false),
				restFill("t2", "o", fxTicker, quote.SideYes, 2, tc.fee, false),
			}))
			eff := ApplyPortfolio(g, pf, owns("o"), nil, read, risk.Live, at(0).Mono, p)

			if eff.Applied[TruthFills] {
				t.Fatal("a fills walk with an unconvertible record was applied")
			}
			if !eff.Stop {
				t.Fatal("an unconvertible fee did not request a global stop")
			}
			if !hasClass(eff.Anomalies, "FILL_UNCONVERTIBLE") {
				t.Fatalf("anomalies = %v", classesOf(eff.Anomalies))
			}
			if got := pf.Q(fxTicker); got != 0 {
				t.Fatalf("q = %s; the GOOD fill in the same walk was applied, "+
					"so the walk was half-applied and t1 is now marked seen "+
					"forever", got.Wire())
			}
			if age := g.TruthAge(TruthFills, at(0)); age != -1 {
				t.Fatalf("the fills freshness clock advanced on a walk that "+
					"was not applied (age %v)", age)
			}
		})
	}

	// A nil ownership ledger fails closed and, critically, does NOT refresh the
	// fills freshness clock. "The ledger is unavailable" and "nothing is ours"
	// are opposite classifications of the same fill, and a refreshed clock
	// would report the endpoint as current while nothing it returned could be
	// attributed -- so A13 would keep authorising placement on a read nobody
	// could interpret. There is deliberately no in-memory fallback.
	t.Run("nil ownership ledger", func(t *testing.T) {
		g, err := NewGate([]string{fxTicker}, p)
		if err != nil {
			t.Fatal(err)
		}
		pf := risk.NewPortfolio()
		tok := g.OnConnect(at(0)).Token
		g.ApplyFrame(snapFrame(fxTicker), okHandle, at(0))

		read := newRead(tok, at(0), 1, completePositions(nil), completeOrders(nil), completeFills([]rest.Fill{
			restFill("t1", "o", fxTicker, quote.SideYes, 2, "0.0000", false),
		}))
		eff := ApplyPortfolio(g, pf, nil, nil, read, risk.Live, at(0).Mono, p)

		if eff.Applied[TruthFills] {
			t.Fatal("a fills walk was applied with no ownership ledger")
		}
		if !eff.Stop {
			t.Fatal("a missing ownership ledger did not request a global stop")
		}
		if !hasClass(eff.Anomalies, "OWNERSHIP_LEDGER_UNAVAILABLE") {
			t.Fatalf("anomalies = %v", classesOf(eff.Anomalies))
		}
		if age := g.TruthAge(TruthFills, at(0)); age != -1 {
			t.Fatalf("fills truth was refreshed by an uninterpretable read "+
				"(age %v); it must be left to age out", age)
		}
		if got := pf.Q(fxTicker); got != 0 {
			t.Fatalf("q = %s; no fill may be attributed without the ledger",
				got.Wire())
		}
		// Positions and orders were readable and are unaffected: the three
		// endpoints stay independent even here.
		if !eff.Applied[TruthPositions] || !eff.Applied[TruthOrders] {
			t.Fatalf("a missing ledger withheld the other two endpoints: %v",
				eff.Applied)
		}
		if g.Actionable(fxTicker, at(0)) {
			t.Fatal("placement is licensed while fills have never reconciled")
		}
	})

	// A well-formed non-zero fee converts and trips the taker detector.
	g, err := NewGate([]string{fxTicker}, p)
	if err != nil {
		t.Fatal(err)
	}
	pf := risk.NewPortfolio()
	tok := g.OnConnect(at(0)).Token
	read := newRead(tok, at(0), 1, completePositions(nil), completeOrders(nil), completeFills([]rest.Fill{
		restFill("t1", "o", fxTicker, quote.SideYes, 2, "0.0100", false),
	}))
	eff := ApplyPortfolio(g, pf, owns("o"), nil, read, risk.Live, at(0).Mono, p)
	if !eff.Stop || !hasClass(eff.Anomalies, "TAKER_FILL") {
		t.Fatalf("a fill with a positive fee did not trip S2's corroborator: "+
			"stop=%v classes=%v", eff.Stop, classesOf(eff.Anomalies))
	}
}

// TestPortfolioTruthAppliesDuringWebsocketOutage is the repair for the defect
// that mattered most in this unit, and it is the probe's failure reached
// through the freshness machinery rather than through a `break`.
//
// A disconnect retires every snapshot and every reconciliation of the dead
// connection, by advancing the generation. The poller, however, is a SEPARATE
// transport (H-FAIL-2) and keeps running. If the generation bump left it
// holding only the dead connection's token, every read it completed for the
// length of the outage would be discarded as stale: requests going out,
// answers coming back, and `q` frozen at whatever it was when the socket
// dropped -- while resting orders kept filling against that frozen number.
//
// So the disconnect issues a token of its own. Reads under it update positions,
// orders, fills and their clocks. They cannot make anything actionable, because
// Actionable needs `connected` and a snapshot from this generation, and an
// outage has neither.
func TestPortfolioTruthAppliesDuringWebsocketOutage(t *testing.T) {
	p := testParams()
	g, err := NewGate([]string{fxTicker}, p)
	if err != nil {
		t.Fatal(err)
	}
	pf := risk.NewPortfolio()
	own := owns("ord-1")

	// Connected, reconciled, quoting.
	connTok := connectAndReconcile(g, fxTicker, at(0))
	if !g.Actionable(fxTicker, at(0)) {
		t.Fatal("setup: the market should be actionable")
	}

	// The socket drops. The disconnect hands back a usable token.
	de := g.ApplyDisconnect(at(10), false)
	if !de.Token.Valid() {
		t.Fatal("a disconnect issued no reconciliation token, so every " +
			"portfolio read taken during the outage would be discarded and " +
			"position monitoring would be blind for its whole duration")
	}
	if g.Actionable(fxTicker, at(10)) {
		t.Fatal("the market stayed actionable through a disconnect")
	}

	// A fill lands during the outage and the exchange reports the new position.
	outage := newRead(de.Token, at(15), 2,
		completePositions(map[string]num.Qty{fxTicker: contracts(12)}),
		completeOrders(nil),
		completeFills([]rest.Fill{
			restFill("t1", "ord-1", fxTicker, quote.SideYes, 12, "0.0000", false),
		}))
	eff := ApplyPortfolio(g, pf, own, nil, outage, risk.Live, at(15).Mono, p)

	if eff.Stale {
		t.Fatal("a read taken under the DISCONNECT token was discarded; the " +
			"poller would then be running blind for the whole outage")
	}
	for k := Truth(0); k < truthCount; k++ {
		if !eff.Applied[k] {
			t.Fatalf("%v was not applied during the outage: %v", k, eff.Applied)
		}
	}
	if got := pf.Q(fxTicker); got != contracts(12) {
		t.Fatalf("q = %s during the outage, want the exchange's 12.00 -- this "+
			"is the number a reducer would be sized from", got.Wire())
	}
	if age := g.TruthAge(TruthPositions, at(15)); age != 0 {
		t.Fatalf("positions truth age = %v, want 0: the read happened", age)
	}

	// It bought monitoring, not permission.
	if g.Actionable(fxTicker, at(15)) {
		t.Fatal("reconciling during an outage re-authorised placement; there " +
			"is no connection and no snapshot on this generation")
	}

	// The dead connection's token is still rejected, so the fix did not simply
	// stop checking.
	late := newRead(connTok, at(16), 3,
		completePositions(map[string]num.Qty{fxTicker: contracts(99)}),
		completeOrders(nil), completeFills(nil))
	lateEff := ApplyPortfolio(g, pf, own, nil, late, risk.Live, at(16).Mono, p)
	if !lateEff.Stale {
		t.Fatal("a read carrying the DEAD connection's token was applied")
	}
	if got := pf.Q(fxTicker); got != contracts(12) {
		t.Fatalf("q = %s; a pre-disconnect read overwrote it", got.Wire())
	}

	// Reconnecting retires the outage token too: a fresh reconciliation is
	// required before anything is actionable again.
	ce := g.OnConnect(at(20))
	stillOutage := newRead(de.Token, at(21), 4,
		completePositions(map[string]num.Qty{fxTicker: contracts(1)}),
		completeOrders(nil), completeFills(nil))
	if e := ApplyPortfolio(g, pf, own, nil, stillOutage, risk.Live, at(21).Mono, p); !e.Stale {
		t.Fatal("the outage token survived the reconnect")
	}
	g.ApplyFrame(snapFrame(fxTicker), okHandle, at(21))
	fresh := newRead(ce.Token, at(21), 5,
		completePositions(map[string]num.Qty{fxTicker: contracts(12)}),
		completeOrders(nil), completeFills(nil))
	ApplyPortfolio(g, pf, own, nil, fresh, risk.Live, at(21).Mono, p)
	if !g.Actionable(fxTicker, at(21)) {
		t.Fatal("a complete post-reconnect reconciliation did not unlock the " +
			"market")
	}
}

// TestPortfolioPollReadsFillsBeforeAuthoritativePosition pins the read order.
//
// Positions is authoritative and overwrites (H-POS-1), so it has to be the
// NEWEST observation in the cycle as well as the most trusted one. Read first,
// a fill landing between it and the fills walk is applied on top of a figure
// that already contains it.
func TestPortfolioPollReadsFillsBeforeAuthoritativePosition(t *testing.T) {
	clk := newFakeClock()
	src := newScriptedPortfolio()
	p := testParams()
	pol, err := NewPoller(src, clk, p.PositionPoll)
	if err != nil {
		t.Fatal(err)
	}
	out := make(chan PortfolioRead, 4)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = pol.Run(ctx, nil, out) }()
	clk.BlockUntilTimers(1)

	clk.Advance(p.PositionPoll)
	<-out

	got := src.callOrder()
	want := []string{"fills", "orders", "positions"}
	if len(got) != len(want) {
		t.Fatalf("endpoint call order = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("endpoint call order = %v, want %v -- positions is "+
				"authoritative and must be the LAST observation of the cycle, "+
				"or the fills walked before it are applied on top of a figure "+
				"that already contains them", got, want)
		}
	}
}

// TestAuthoritativePositionIsFinalAfterSameCycleFill is the arithmetic the read
// order exists to protect, and the loss it prevents is a sign flip.
//
// A new owned YES fill of 12 arrives in the same cycle in which the exchange
// reports q = +12. They are the SAME twelve contracts. Applying the fill after
// the authoritative figure gives q = +24; a reducer sized from 24 sells 24
// against a real position of 12 and leaves us at −12 -- the sign-flipping exit
// H-Q-5a exists to forbid, produced by bookkeeping rather than by sizing.
func TestAuthoritativePositionIsFinalAfterSameCycleFill(t *testing.T) {
	p := testParams()
	g, err := NewGate([]string{fxTicker}, p)
	if err != nil {
		t.Fatal(err)
	}
	pf := risk.NewPortfolio()
	tok := connectAndReconcile(g, fxTicker, at(0))

	read := newRead(tok, at(5), 1,
		completePositions(map[string]num.Qty{fxTicker: contracts(12)}),
		completeOrders(nil),
		completeFills([]rest.Fill{
			restFill("t1", "ord-1", fxTicker, quote.SideYes, 12, "0.0000", false),
		}))
	eff := ApplyPortfolio(g, pf, owns("ord-1"), nil, read, risk.Live, at(5).Mono, p)

	if got := pf.Q(fxTicker); got != contracts(12) {
		t.Fatalf("q = %s, want 12.00. The fill and the position are the same "+
			"twelve contracts; 24.00 means the fill was applied on top of an "+
			"authoritative figure that already contained it, and a 24-lot "+
			"reducer would flip a real +12 to -12", got.Wire())
	}
	if len(eff.Records) != 1 {
		t.Fatalf("records = %v, want one poll record", eff.Records)
	}
	if !eff.Records[0].Agreed {
		t.Fatalf("the poll record disagrees (%+v); with the fill applied "+
			"first, q_local and q_exch describe the same twelve contracts and "+
			"must agree", eff.Records[0])
	}
	if len(eff.Reduce) != 0 || eff.Stop {
		t.Fatalf("a correctly ordered cycle raised a drift response: "+
			"reduce=%v stop=%v", eff.Reduce, eff.Stop)
	}
}

// TestEndpointTruthAgeStartsWhenItsWalkStarts is H-FAIL-4 measured against the
// evidence rather than against the batch that carried it.
//
// A completed cursor walk is a consistent suffix of the account as of the
// moment it STARTED. The fills walk scales with account history (H-POS-3) and
// is the slow one, so stamping the cycle's three results with its finish time
// can overstate the currency of the fills by the whole duration of the walk --
// and truth_max_age_s would then authorise placement from evidence older than
// the limit it exists to enforce.
func TestEndpointTruthAgeStartsWhenItsWalkStarts(t *testing.T) {
	p := testParams()
	g, err := NewGate([]string{fxTicker}, p)
	if err != nil {
		t.Fatal(err)
	}
	pf := risk.NewPortfolio()
	tok := connectAndReconcile(g, fxTicker, at(0))

	// The fills walk starts at t=0 and takes longer than truth_max_age_s. The
	// orders and positions walks run after it and are current.
	slow := p.TruthMaxAge + 10*time.Second
	done := Stamp{Mono: slow}
	read := newStaggeredRead(tok, 1,
		at(0),            // fills started here
		done, done, done, // orders, positions, completion
		completePositions(map[string]num.Qty{fxTicker: contracts(1)}),
		completeOrders(nil), completeFills(nil))

	ApplyPortfolio(g, pf, owns(), nil, read, risk.Live, done.Mono, p)

	if age := g.TruthAge(TruthFills, done); age != slow {
		t.Fatalf("fills truth age = %v, want %v -- it must be measured from "+
			"when the fills walk STARTED, not from when the cycle finished",
			age, slow)
	}
	if age := g.TruthAge(TruthPositions, done); age != 0 {
		t.Fatalf("positions truth age = %v, want 0", age)
	}
	if g.Actionable(fxTicker, done) {
		t.Fatalf("placement is licensed from a fills walk that began %v ago, "+
			"past truth_max_age_s %v; the cycle's completion stamp made stale "+
			"evidence look current", slow, p.TruthMaxAge)
	}

	// A cycle whose fills walk is fast leaves everything current, so the
	// assertion above is about the stamp and not about something else.
	g2, err := NewGate([]string{fxTicker}, p)
	if err != nil {
		t.Fatal(err)
	}
	tok2 := connectAndReconcile(g2, fxTicker, at(0))
	fast := newStaggeredRead(tok2, 1, at(0), at(0), at(0), at(0),
		completePositions(map[string]num.Qty{fxTicker: contracts(1)}),
		completeOrders(nil), completeFills(nil))
	ApplyPortfolio(g2, risk.NewPortfolio(), owns(), nil, fast, risk.Live, at(0).Mono, p)
	if !g2.Actionable(fxTicker, at(0)) {
		t.Fatal("a cycle with three current walks did not license placement")
	}
}

// TestOwnershipLookupFailureAppliesNothingAndRefreshesNoTruth is H-ORD-9's
// third answer, end to end.
//
// The durable ownership ledger can say "ours", "not ours", or "I cannot say",
// and only the third is new. It must not become "not ours": that classification
// declares a third party is trading the account, latches a SEV1 and a global
// stop, and would here be manufactured by a database that would not open. It
// must not become "ours" either. So the walk applies NOTHING -- not q, not the
// order counters, not the dedup set -- and the fills freshness clock does not
// advance, which leaves H-FAIL-4 to age the endpoint out and stop dispatch. That
// is the correct end state for a ledger outage.
//
// The dedup set is the subtle half. A fill marked seen during the outage is a
// fill NO later poll will ever apply, so a lookup failure that consumed the walk
// would leave a permanent hole in the position that no repair can find.
//
// `M-HS-LOOKUPFAIL` turns the error into all-foreign and keeps applying.
func TestOwnershipLookupFailureAppliesNothingAndRefreshesNoTruth(t *testing.T) {
	p := testParams()
	g, err := NewGate([]string{fxTicker}, p)
	if err != nil {
		t.Fatal(err)
	}
	pf := risk.NewPortfolio()
	tok := g.OnConnect(at(0)).Token

	fills := completeFills([]rest.Fill{
		restFill("t1", "ord-1", fxTicker, quote.SideYes, 3, "0.0000", false),
		restFill("t2", "ord-2", fxTicker, quote.SideYes, 2, "0.0000", false),
	})

	// The ledger is unavailable.
	broken := ledger{ours: map[string]bool{"ord-1": true},
		err: errors.New("harness.db: unable to open database file")}
	read := newRead(tok, at(0), 1,
		completePositions(map[string]num.Qty{fxTicker: contracts(0)}),
		completeOrders(nil), fills)
	eff := ApplyPortfolio(g, pf, broken, nil, read, risk.Live, at(0).Mono, p)

	if eff.Applied[TruthFills] {
		t.Fatal("the fills endpoint was reported current after a walk whose " +
			"every fill was unclassifiable; A13 would keep authorising " +
			"placement on the strength of a read nothing could be attributed " +
			"from")
	}
	if !eff.Stop {
		t.Fatal("an unavailable ownership ledger did not request a global stop")
	}
	if len(eff.OwnedFill) != 0 || len(eff.Foreign) != 0 {
		t.Fatalf("an unavailable ledger classified something: owned=%v "+
			"foreign=%v", eff.OwnedFill, eff.Foreign)
	}
	if !hasClass(eff.Anomalies, "OWNERSHIP_LEDGER_UNAVAILABLE") {
		t.Fatalf("anomalies %v, want OWNERSHIP_LEDGER_UNAVAILABLE",
			classes(eff.Anomalies))
	}
	if hasClass(eff.Anomalies, "FOREIGN_FILL") {
		t.Fatalf("a storage outage was reported as a FOREIGN_FILL, which " +
			"asserts a third party is trading the account")
	}
	if got := pf.Q(fxTicker); got != 0 {
		t.Fatalf("q moved during a ledger outage: %s", got.Wire())
	}

	// The ledger comes back. Nothing was consumed, so the same walk now
	// classifies in full -- which is only possible because no trade_id was
	// marked seen.
	good := ledger{ours: map[string]bool{"ord-1": true}}
	read2 := newRead(tok, at(1), 2,
		completePositions(map[string]num.Qty{fxTicker: contracts(3)}),
		completeOrders(nil), fills)
	eff = ApplyPortfolio(g, pf, good, nil, read2, risk.Live, at(1).Mono, p)

	if !eff.Applied[TruthFills] {
		t.Fatal("the recovered walk was not applied")
	}
	if len(eff.OwnedFill) != 1 || eff.OwnedFill[0].TradeID != "t1" {
		t.Fatalf("owned fills after recovery %v, want exactly t1 -- a fill "+
			"marked seen during the outage could never be applied again",
			eff.OwnedFill)
	}
	if !hasClass(eff.Anomalies, "FOREIGN_FILL") {
		t.Fatalf("t2 is genuinely not in the ledger and was not reported "+
			"foreign: %v", classes(eff.Anomalies))
	}
}

func classes(anoms []risk.Anomaly) []string {
	out := make([]string, 0, len(anoms))
	for _, a := range anoms {
		out = append(out, a.Class)
	}
	return out
}
