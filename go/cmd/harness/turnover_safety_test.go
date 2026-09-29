package main

import (
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"lip/harness/num"
	"lip/harness/qual"
	"lip/harness/quote"
	"lip/harness/rest"
	"lip/harness/risk"
	"lip/harness/wsx"
)

func turnoverOwnerForSafety(t *testing.T, q num.Qty) *gateFailOwner {
	t.Helper()
	g := capitalOwner(t, 0, 120)
	if q != 0 {
		g.h.installPosition(q)
	}
	o := g.o
	o.r.cfg.Turnover = true
	o.r.cfg.CapitalSource = "selected_shard_balance"
	o.r.cfg.Funding = &fundingEvidence{Ticker: seamTicker, ExchangeIndex: 2}
	o.r.cfg.Candidates = []string{seamTicker, turnoverOtherTicker}
	o.r.cfg.ShardFunding = map[string]fundingEvidence{
		seamTicker:          {Ticker: seamTicker, ExchangeIndex: 2, Effective: num.MoneyFromDollars(20)},
		turnoverOtherTicker: {Ticker: turnoverOtherTicker, ExchangeIndex: 3, Effective: num.MoneyFromDollars(100)},
	}
	o.r.funding = new(atomic.Pointer[fundingObservation])
	o.r.funding.Store(&fundingObservation{valid: true, at: g.o.r.ex.Mono(), byTicker: map[string]int{seamTicker: 2, turnoverOtherTicker: 3}, byShard: map[int]num.Money{2: num.MoneyFromDollars(2), 3: num.MoneyFromDollars(100)}})
	o.markets = map[string]*marketRuntime{seamTicker: {}, turnoverOtherTicker: {market: quote.Idle, programMembership: programMembership{ticker: turnoverOtherTicker}}}
	o.saveMarket()
	o.targets = map[string]float64{seamTicker: seamTarget, turnoverOtherTicker: seamTarget}
	return g
}
func TestTurnoverReducerSizesFromItsOwnShardCash(t *testing.T) {
	g := turnoverOwnerForSafety(t, num.QtyFromFloat(10))
	got := g.o.fundedReducer(num.QtyFromFloat(10))
	want := risk.FundedContracts(num.MoneyFromDollars(2), num.Price4FromCents(seamNoTouch))
	if got != want || got <= 0 || got >= num.QtyFromFloat(10) {
		t.Fatalf("shard-constrained partial reducer=%s want=%s", got.Wire(), want.Wire())
	}
	// An existing compliant partial exit must not be cancelled merely because
	// the cash it occupies is conservatively reserved from the observed balance.
	g.o.pending["held-partial"] = pendingOrder{ticker: seamTicker, side: quote.SideNo, cents: seamNoTouch, qty: got, acked: true, id: "held"}
	if funded := g.o.fundedReducer(num.QtyFromFloat(10)); funded < got {
		t.Fatalf("funding shrank an already compliant exit: %s < %s", funded.Wire(), got.Wire())
	}
}
func TestTurnoverPendingOrderIdentityDoesNotLeakAcrossMarkets(t *testing.T) {
	g := turnoverOwnerForSafety(t, 0)
	g.o.pending["old-unknown"] = pendingOrder{ticker: seamTicker, side: quote.SideYes, cents: 40, qty: num.QtyFromFloat(3), id: "old-owned", acked: true}
	restore := g.o.marketContext(turnoverOtherTicker)
	if got := g.o.atRisk(quote.SideYes); got != 0 {
		t.Fatalf("A pending contaminated B side quantity: %s", got.Wire())
	}
	if orders := g.o.unlistedOn(turnoverOtherTicker, quote.SideYes); len(orders) != 0 {
		t.Fatalf("A order entered B cancel list: %+v", orders)
	}
	restore()
	if got := g.o.atRisk(quote.SideYes); got != num.QtyFromFloat(3) {
		t.Fatalf("old obligation lost: %s", got.Wire())
	}
}
func TestTurnoverResnapshotIncludesOldManagedMarket(t *testing.T) {
	g := turnoverOwnerForSafety(t, 0)
	out := make(chan wsx.Command, 4)
	g.o.wsCommands = out
	g.o.bookSID = 1
	g.o.resnapshot(true)
	seen := map[string]bool{}
	for len(out) > 0 {
		cmd := <-out
		for _, ticker := range cmd.Tickers {
			seen[ticker] = true
		}
	}
	if !seen[seamTicker] || !seen[turnoverOtherTicker] {
		t.Fatalf("resnapshot did not include all managed books: %v", seen)
	}
}

// Adding a market through Gate.AddMarkets retires the connection generation, so
// refreshUniverse must hand the poller a token for the new one; otherwise every
// read is stale until a re-dial succeeds. The composed test
// TestComposedTurnoverUniverseRefreshKeepsPortfolioTruthWhileRedialFails covers
// the target-refresh path.
func TestTurnoverUniverseGrowthKeepsPortfolioReadsApplying(t *testing.T) {
	g := turnoverOwnerForSafety(t, 0)
	select {
	case <-g.tokens: // the fixture's connect token, never taken by a poller
	default:
	}
	gen := g.o.r.gate.Generation()
	g.o.refreshUniverse() // the gate learns B here
	if g.o.r.gate.Generation() == gen {
		t.Fatal("adding a market did not retire the connection generation")
	}
	if len(g.tokens) != 1 {
		t.Fatal("refreshUniverse offered the poller no token for the new generation")
	}
	reducerConfirmationPoll(t, g)
	if !g.o.truthKnown() {
		t.Fatal("a portfolio read under the growth token did not apply")
	}
	if g.o.r.gate.Actionable(seamTicker, g.h.clk.Now()) {
		t.Fatal("the growth token made a market actionable without a new connection")
	}
}

// H-ORD-4b is account-wide by decision (lip-mgh). A clean sweep in B proves B's
// order absent, not unfilled, and the budget that sizes A's reducer sums
// exposure across markets, so A waits for the post-sweep cycle as well.
func TestCancelSweepInOneMarketHoldsPlacementsInEveryMarket(t *testing.T) {
	one := num.QtyFromFloat(1)
	g := turnoverOwnerForSafety(t, one)
	g.h.ex.setPosition(seamTicker, one.Wire())
	reducerConfirmationPoll(t, g)
	if !g.h.rig.gate.Actionable(seamTicker, g.h.clk.Now()) {
		t.Fatal("A's book is not actionable after the opening walk")
	}
	// Only B's cancel result arrives; B needs no book or position of its own.
	g.o.applyWriteResult(writeResult{
		Req: writeRequest{Op: quote.OpCancel, Market: turnoverOtherTicker, Side: quote.SideYes,
			Orders: []rest.Order{{OrderID: "B-SWEPT", Ticker: turnoverOtherTicker, Side: quote.SideYes}}},
		Sent: true, Absent: true,
		Sweep: rest.SweepResult{Walk: rest.Walk{Outcome: rest.WalkComplete}, Clean: true},
	})
	writes := make(chan writeRequest, 4)
	now := g.h.clk.monoNow()
	g.o.evaluate(now)
	g.o.pump(now, writes)
	select {
	case req := <-writes:
		t.Fatalf("%s %s/%s dispatched while B's sweep awaited fresh truth", req.Market, req.Op, req.Role)
	default:
	}
	if g.o.truthKnown() {
		t.Fatal("the drain may treat truth as known while B's sweep awaits it")
	}

	// A cycle whose three walks start after the sweep releases every market.
	g.h.clk.Advance(time.Millisecond)
	reducerConfirmationPoll(t, g)
	now = g.h.clk.monoNow()
	g.o.evaluate(now)
	g.o.pump(now, writes)
	select {
	case req := <-writes:
		if req.Market != seamTicker || req.Op != quote.OpPlace || req.Role != quote.RoleReducing {
			t.Fatalf("after fresh truth dispatched %s %s/%s, want A's reducer", req.Market, req.Op, req.Role)
		}
	default:
		t.Fatal("A's reducer stayed held after the post-sweep portfolio cycle")
	}
}
func TestTurnoverCrossCheckBackpressureRetainsRequests(t *testing.T) {
	g := turnoverOwnerForSafety(t, 0)
	out := make(chan wsx.CrossCheckRequest, 1)
	g.o.crossChecks = out
	rows := []wsx.CrossCheckRequest{{Ticker: "A"}, {Ticker: "B"}, {Ticker: "C"}}
	g.o.requestCrossChecks(rows)
	if len(g.o.crossCheckPending) != 2 {
		t.Fatalf("queued request was treated as failed evidence: %d", len(g.o.crossCheckPending))
	}
	if got := (<-out).Ticker; got != "A" {
		t.Fatal(got)
	}
	g.o.flushCrossChecks()
	if got := (<-out).Ticker; got != "B" {
		t.Fatal(got)
	}
	g.o.flushCrossChecks()
	if got := (<-out).Ticker; got != "C" {
		t.Fatal(got)
	}
}
func TestTurnoverCompleteOmissionRetainsOwnedObligation(t *testing.T) {
	g := turnoverOwnerForSafety(t, 0)
	g.o.pending["unknown-A"] = pendingOrder{ticker: seamTicker, side: quote.SideYes, cents: 40, qty: num.QtyFromFloat(1), at: g.o.r.ex.Mono(), id: "known-id", acked: true}
	g.o.clearListed()
	g.o.applyTurnoverTerminals(turnoverTerminalRead{orders: rest.OrdersResult{Walk: rest.Walk{Outcome: rest.WalkComplete}}, started: g.o.r.ex.Mono()})
	if g.o.pending["unknown-A"].qty != num.QtyFromFloat(1) || !g.o.anyLiveOrder() {
		t.Fatal("omission released possibly-live owned exposure")
	}
}
func TestTurnoverSelectionExpiresWithoutFreshEvidence(t *testing.T) {
	g := turnoverOwnerForSafety(t, 0)
	g.o.selected = map[string]bool{seamTicker: true}
	g.o.turnoverReady = true
	g.o.turnoverAccountingReady = true
	g.o.selectionAt = g.o.r.ex.Mono()
	if !g.o.isSelected(seamTicker) {
		t.Fatal("fresh selected market unavailable")
	}
	g.h.clk.Advance(g.o.p.TruthMaxAge + time.Millisecond)
	if g.o.isSelected(seamTicker) {
		t.Fatal("stale selection retained adding authority")
	}
}

func TestTurnoverPositiveTerminalWaitsForInflightResult(t *testing.T) {
	g := turnoverOwnerForSafety(t, 0)
	g.h.installOwnedOrder("terminal-owned", 911)
	coid, _ := g.o.r.store.Ownership().Bound("terminal-owned")
	order, err := rest.NewCreateOrder(seamTicker, quote.SideYes, 40, num.QtyFromFloat(1), num.QtyFromFloat(1), coid)
	if err != nil {
		t.Fatal(err)
	}
	g.o.pending[coid] = pendingOrder{ticker: seamTicker, side: quote.SideYes, cents: 40, qty: num.QtyFromFloat(1), at: g.o.r.ex.Mono(), acked: true, id: "terminal-owned"}
	g.o.inflight = []*writeRequest{{Market: seamTicker, Side: quote.SideYes, Op: quote.OpPlace, Order: order}}
	read := turnoverTerminalRead{started: g.o.r.ex.Mono(), orders: rest.OrdersResult{Walk: rest.Walk{Outcome: rest.WalkComplete}, Orders: []rest.Order{{OrderID: "terminal-owned", ClientOrderID: coid, Ticker: seamTicker, Side: quote.SideYes, Price4: 4000, Status: rest.StatusExecuted}}}}
	g.o.applyTurnoverTerminals(read)
	if _, ok := g.o.pending[coid]; !ok {
		t.Fatal("terminal retired while delayed create result could resurrect it")
	}
	g.o.inflight = nil
	g.o.applyTurnoverTerminals(read)
	if _, ok := g.o.pending[coid]; ok {
		t.Fatal("positive bound terminal did not retire obligation")
	}
}

func TestTurnoverUnboundPositiveTerminalBindsBeforeRetirement(t *testing.T) {
	g := turnoverOwnerForSafety(t, 0)
	req := capitalOrder(t, g, quote.SideYes, quote.RoleAdding, 40, 1, 912)
	coid := req.Order.ClientOrderID()
	if _, err := g.o.r.store.ReserveOrder(g.o.r.run, req.Order, req.Role, g.o.r.ex.NowMs()); err != nil {
		t.Fatal(err)
	}
	g.h.await("unbound reservation committed", func() bool { _, ok := g.o.r.store.Ownership().Unresolved()[coid]; return ok })
	g.o.pending[coid] = pendingOrder{ticker: seamTicker, side: quote.SideYes, cents: 40, qty: num.QtyFromFloat(1), at: g.o.r.ex.Mono()}
	read := turnoverTerminalRead{started: g.o.r.ex.Mono(), orders: rest.OrdersResult{Walk: rest.Walk{Outcome: rest.WalkComplete}, Orders: []rest.Order{{OrderID: "immediate-fill", ClientOrderID: coid, Ticker: seamTicker, Side: quote.SideYes, Price4: 4000, Status: rest.StatusExecuted}}}}
	g.o.applyTurnoverTerminals(read)
	if _, ok := g.o.pending[coid]; !ok {
		t.Fatal("released before binding committed")
	}
	g.h.await("terminal binding committed", func() bool { got, ok := g.o.r.store.Ownership().Bound("immediate-fill"); return ok && got == coid })
	g.o.applyTurnoverTerminals(read)
	if _, ok := g.o.pending[coid]; ok {
		t.Fatal("unbound terminal never retired after durable binding")
	}
}

func TestTurnoverForeignPrefixIsNotOwnedSweepEvidence(t *testing.T) {
	g := turnoverOwnerForSafety(t, 0)
	foreign := rest.Order{OrderID: "foreign", ClientOrderID: "lip-foreign-shaped", Ticker: seamTicker, Side: quote.SideYes, Price4: 4000, Remaining: num.QtyFromFloat(1)}
	if g.o.acceptTurnoverOwnedSighting(foreign) {
		t.Fatal("prefix-shaped foreign order admitted as owned")
	}
}

func TestTurnoverMonitorPreservesAggregateAndInvalidatesStalledPnL(t *testing.T) {
	snapshot := &risk.Snapshot{Seq: 1, Account: risk.AccountSnapshot{TruthFresh: true, CapitalMax: num.MoneyFromDollars(2), Exposures: []risk.Exposure{{Ticker: "A", Position: num.MoneyFromDollars(1)}, {Ticker: "B", Adding: num.MoneyFromDollars(.5)}}, TradingPnL: risk.PnLResult{Evaluable: true, Total: num.MoneyFromDollars(.1)}}}
	var monitor risk.MonitorState
	fresh := monitor.Step(0, snapshot, time.Second)
	if !fresh.Account.TruthFresh || !fresh.Account.TradingPnL.Evaluable || risk.Deployed(fresh.Account.Exposures) != num.MoneyFromDollars(1.5) {
		t.Fatalf("aggregate publication lost: %+v", fresh.Account)
	}
	stale := monitor.Step(2*time.Second, snapshot, time.Second)
	if !stale.Stale || stale.Account.TruthFresh || stale.Account.TradingPnL.Evaluable {
		t.Fatalf("stalled aggregate claimed current truth: %+v", stale)
	}
	if !snapshot.Account.TruthFresh || !snapshot.Account.TradingPnL.Evaluable {
		t.Fatal("monitor mutated owner publication")
	}
}

// lip-d3e: an unresolved create is reported under its own market, not the
// owner's active one. A legacy pending order with no ticker is the base market.
func TestTurnoverOrderUnknownNamesThePendingOrdersOwnMarket(t *testing.T) {
	g := turnoverOwnerForSafety(t, 0)
	g.h.takeRaised()
	at := g.o.r.ex.Mono()
	g.o.pending["B-UNRESOLVED"] = pendingOrder{ticker: turnoverOtherTicker, side: quote.SideYes, cents: 40, qty: num.QtyFromFloat(1), at: at}
	g.o.pending["A-UNRESOLVED"] = pendingOrder{side: quote.SideNo, cents: 55, qty: num.QtyFromFloat(1), at: at}
	g.h.clk.Advance(g.o.p.UnknownPing + time.Second)
	g.o.escalateUnresolved()
	got := map[string]string{}
	for _, a := range g.h.takeRaised() {
		for _, coid := range []string{"B-UNRESOLVED", "A-UNRESOLVED"} {
			if a.Class == "ORDER_UNKNOWN" && strings.Contains(a.Text, "coid "+coid+" ") {
				got[coid] = a.Ticker
			}
		}
	}
	if got["B-UNRESOLVED"] != turnoverOtherTicker || got["A-UNRESOLVED"] != seamTicker {
		t.Fatalf("ORDER_UNKNOWN tickers = %v, want B's under %s and A's under %s",
			got, turnoverOtherTicker, seamTicker)
	}
}

// lip-d3e: read-only turnover evidence fingerprints a would-write with the
// state of the market it writes to, not the owner's active market.
func TestTurnoverWouldWriteFingerprintCarriesItsOwnMarketState(t *testing.T) {
	g := turnoverOwnerForSafety(t, 0)
	g.o.r.cfg.Live = false
	qrec, err := qual.Open(filepath.Join(t.TempDir(), "qualification.json"),
		qual.Metadata{
			SchemaVersion: qual.SchemaVersion, ConfigHash: "sha256:would-write-market",
			BinaryIdentity: "sha256:test", Ticker: seamTicker, Rung: "canary",
			Live: false,
		}, qual.SegmentStart{
			ID: "would-write-market", PID: os.Getpid(), StartedAt: time.Now().UTC(),
		})
	if err != nil {
		t.Fatalf("qual.Open: %v", err)
	}
	g.o.r.qual = qrec
	bState := g.o.markets[turnoverOtherTicker].market
	if bState == g.o.market {
		t.Fatalf("fixture: A and B are both %s, so the fingerprint cannot tell them apart", bState)
	}
	g.h.installResting(risk.LiveOrder{OrderID: "B-RESTING", Ticker: turnoverOtherTicker,
		Side: quote.SideYes, Price4: 4000, Remaining: num.QtyFromFloat(1)})
	now := g.h.clk.monoNow()
	if _, err := g.o.r.queue.Enqueue(now, quote.Intent{Market: turnoverOtherTicker,
		Side: quote.SideYes, Role: quote.RoleAdding, Kind: quote.KindCancel,
		Reason: quote.ReasonCancel}); err != nil {
		t.Fatalf("enqueue B cancel: %v", err)
	}
	g.o.pump(now, make(chan writeRequest, 1))
	var episodes []qual.WouldWriteEpisode
	for _, e := range qrec.Snapshot().WouldWrites {
		if e.Fingerprint.Ticker == turnoverOtherTicker {
			episodes = append(episodes, e)
		}
	}
	if len(episodes) != 1 {
		t.Fatalf("B would-write episodes = %+v, want one", qrec.Snapshot().WouldWrites)
	}
	if state := episodes[0].Fingerprint.State; !strings.Contains(state, ";market="+bState.String()+";") {
		t.Fatalf("B's would-write state %q does not carry B's market state %s", state, bState)
	}
}

// confidence: high
