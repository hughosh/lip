package main

import (
	"context"
	"os"
	"sync"
	"testing"
	"time"

	"lip/harness/num"
	"lip/harness/quote"
	"lip/harness/rest"
	"lip/harness/risk"
	"lip/harness/wsx"
)

type pausedPositionSource struct {
	src     wsx.PortfolioSource
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (s *pausedPositionSource) Fills(ctx context.Context, ticker string, since time.Time) rest.FillsResult {
	return s.src.Fills(ctx, ticker, since)
}
func (s *pausedPositionSource) Orders(ctx context.Context, ticker, status string) rest.OrdersResult {
	return s.src.Orders(ctx, ticker, status)
}
func (s *pausedPositionSource) Positions(ctx context.Context) rest.PositionsResult {
	s.once.Do(func() { close(s.started) })
	select {
	case <-s.release:
	case <-ctx.Done():
	}
	return s.src.Positions(ctx)
}

// A single real poller cycle over the fake REST transport establishes all
// three A13 truths. Unlike a hand-built PortfolioRead, it cannot forge an
// actionable gate and it exercises the owner's complete-walk consumer.
func reducerConfirmationPoll(t *testing.T, g *gateFailOwner) {
	t.Helper()
	p, err := wsx.NewPoller(g.h.rig.api, g.h.clk, time.Hour)
	if err != nil {
		t.Fatalf("new poller: %v", err)
	}
	ctx, cancel := context.WithCancel(g.h.ctx)
	defer cancel()
	out := make(chan wsx.PortfolioRead, 1)
	done := make(chan error, 1)
	go func() { done <- p.Run(ctx, g.tokens, out) }()
	select {
	case read := <-out:
		g.o.applyRead(read)
	case <-time.After(5 * time.Second):
		t.Fatal("fake REST portfolio walk did not complete")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("fake REST poller did not stop")
	}
}

// A sweep proves the old reducer absent, but a fill immediately before DELETE
// may have changed q. The owner must wait for a new complete portfolio cycle
// before replacing it; then it must size from the newly observed position.
func TestConfirmedAbsentReducerWaitsForFreshPosition(t *testing.T) {
	one := num.QtyFromFloat(1)
	twelve := num.QtyFromFloat(12)
	for _, tc := range []struct {
		name  string
		q     num.Qty
		side  quote.Side
		price int
	}{
		{"long YES reduces on NO", one, quote.SideNo, 55},
		{"short YES reduces on YES", -one, quote.SideYes, 40},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := newGateFailOwner(t, tc.q)
			const oldID = "EX-OVERSIZED-EXIT"
			coid, err := rest.Coid("SEAMPRIOR", 0, tc.side, 1)
			if err != nil {
				t.Fatalf("old order coid: %v", err)
			}
			wireSide, wirePrice := string(rest.Bid), "0.4000"
			if tc.side == quote.SideNo {
				wireSide, wirePrice = string(rest.Ask), "0.4500"
			}
			g.h.ex.mu.Lock()
			g.h.ex.resting = []map[string]any{seamRestingOrder(seamCreate{
				Ticker: seamTicker, WireSide: wireSide, Price: wirePrice,
				Count: twelve.Wire(), Coid: coid, OrderID: oldID,
			})}
			g.h.ex.mu.Unlock()
			g.h.ex.setPosition(seamTicker, tc.q.Wire())
			reducerConfirmationPoll(t, g)
			if !g.h.rig.gate.Actionable(seamTicker, g.h.clk.Now()) {
				t.Fatal("opening fake REST walk did not make the book actionable")
			}
			g.snapshot(gateFailThinBook())
			if !g.stopped(gateFailDebounce) {
				t.Fatal("gate failure did not put the market into REDUCING")
			}
			writes := make(chan writeRequest, 1)
			g.o.pump(gateFailDebounce, writes)
			var cancel writeRequest
			select {
			case cancel = <-writes:
			default:
				t.Fatal("oversized reducer was not selected for cancellation")
			}
			if cancel.Op != quote.OpCancel || cancel.Side != tc.side {
				t.Fatalf("selected %s/%s, want cancel on %s", cancel.Op,
					cancel.Side, tc.side)
			}
			g.o.applyWriteResult(writeResult{
				Req: cancel, Sent: true, Absent: true,
				Sweep: rest.SweepResult{
					Walk: rest.Walk{Outcome: rest.WalkComplete}, Clean: true,
				},
			})

			// No fabricated post-sweep orders walk: the stale order is still in
			// Portfolio. Only the independent verifying read retired its id.
			if got := g.h.rig.pf.LiveOrders(); len(got) != 1 || got[0].OrderID != oldID {
				t.Fatalf("the test lost its stale portfolio order: %+v", got)
			}
			if got := g.o.atRisk(tc.side); got != 0 {
				t.Fatalf("confirmed-absent reducer still contributes %s to the cap", got.Wire())
			}
			for _, e := range g.o.exposures() {
				if e.Ticker == seamTicker && e.Reducing != 0 {
					t.Fatalf("confirmed-absent reducer still reserves %s capital", e.Reducing)
				}
			}
			g.o.pump(gateFailDebounce, writes)
			select {
			case req := <-writes:
				t.Fatalf("stale q dispatched %s before a fresh positions walk", req.Op)
			default:
			}
			if !g.o.awaitCancelTruth {
				t.Fatal("cancel did not hold placement for position reconciliation")
			}

			// The exchange has no old resting reducer now. Its next complete
			// positions walk still reports this test's q, so a reducer is due.
			g.h.ex.mu.Lock()
			g.h.ex.resting = nil
			g.h.ex.mu.Unlock()
			g.h.clk.Advance(time.Millisecond)
			reducerConfirmationPoll(t, g)
			if g.o.awaitCancelTruth {
				t.Fatal("fresh fills/orders/positions did not release placement")
			}
			g.o.evaluate(gateFailDebounce)
			g.o.pump(gateFailDebounce, writes)
			var place writeRequest
			select {
			case place = <-writes:
			default:
				t.Fatal("confirmed reducer replacement did not dispatch after fresh position truth")
			}
			if place.Op != quote.OpPlace || place.Side != tc.side {
				t.Fatalf("replacement dispatched %s/%s, want place on %s", place.Op, place.Side, tc.side)
			}
			if place.Order.Count() != one {
				t.Fatalf("replacement size %s, want |q| = %s", place.Order.Count().Wire(), one.Wire())
			}
			for _, a := range g.h.takeRaised() {
				if a.Class == "WRITE_NOT_BUILDABLE" {
					t.Fatalf("the confirmed replacement raised WRITE_NOT_BUILDABLE: %+v", a)
				}
			}

			g.o.applyWriteResult(writeResult{
				Req: place, Sent: true, Bound: true,
				Create: rest.CreateResult{
					Outcome: rest.CreateAcked, Coid: place.Order.ClientOrderID(),
					OrderID: "EX-REPLACEMENT", MaxLive: one, Remaining: one,
				},
			})
			if g.o.cancelConfirmed[tc.side] {
				t.Fatal("the new create did not clear the side-wide cancellation latch")
			}
			if got := g.o.atRisk(tc.side); got != one {
				t.Fatalf("replacement ACK resurrected the old reducer: aggregate %s, want %s", got.Wire(), one.Wire())
			}
			g.o.evaluate(gateFailDebounce)
			if got := seamCancelsOn(g.o.r.queue, tc.side); len(got) != 0 {
				t.Fatalf("replacement ACK triggered %d further reducing-side cancels before the next walk", len(got))
			}
		})
	}
}

// The exchange can fill a reducer just before DELETE and still report a clean
// cancel sweep. Exercise the real fake-REST cancel and verifying read so the
// owner sees exactly that result, with no manufactured Absent flag.
func TestFillBeforeReducerCancelCannotPlaceFromStaleQ(t *testing.T) {
	one := num.QtyFromFloat(1)
	twelve := num.QtyFromFloat(12)
	for _, tc := range []struct {
		name          string
		before, after num.Qty
	}{
		{"filled flat", one, 0},
		{"partly reduced", num.QtyFromFloat(2), one},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := newGateFailOwner(t, tc.before)
			const oldID = "EX-FILL-RACE-EXIT"
			coid, err := rest.Coid("SEAMPRIOR", 0, quote.SideNo, 1)
			if err != nil {
				t.Fatal(err)
			}
			g.h.seamOwnedOrderOn(oldID, 7, quote.SideNo, 55, twelve)
			g.h.ex.mu.Lock()
			g.h.ex.resting = []map[string]any{seamRestingOrder(seamCreate{
				Ticker: seamTicker, WireSide: string(rest.Ask), Price: "0.4500",
				Count: twelve.Wire(), Coid: coid, OrderID: oldID,
			})}
			g.h.ex.mu.Unlock()
			g.h.ex.setPosition(seamTicker, tc.before.Wire())
			reducerConfirmationPoll(t, g)
			g.snapshot(gateFailThinBook())
			if !g.stopped(gateFailDebounce) {
				t.Fatal("did not enter REDUCING")
			}
			writes := make(chan writeRequest, 1)
			g.o.pump(gateFailDebounce, writes)
			var cancel writeRequest
			select {
			case cancel = <-writes:
			default:
				t.Fatal("oversized reducer was not selected for cancellation")
			}
			if cancel.Op != quote.OpCancel || cancel.Side != quote.SideNo {
				t.Fatalf("selected %+v, want NO cancel", cancel)
			}

			// One NO fill moves the actual YES position before DELETE. The
			// owner's q is intentionally still from the earlier portfolio poll.
			g.h.ex.mu.Lock()
			g.h.ex.positions[seamTicker] = tc.after.Wire()
			g.h.ex.fills = append(g.h.ex.fills, seamMakerFill(
				"FILL-RACE-EXIT", oldID, "no", "0.4500", "0.5500", "1.00", 2))
			for _, order := range g.h.ex.resting {
				if order["order_id"] == oldID {
					order["remaining_count"] = "11.00"
				}
			}
			g.h.ex.mu.Unlock()
			if got := g.h.rig.pf.Q(seamTicker); got != tc.before {
				t.Fatalf("owner q refreshed prematurely: %s", got.Wire())
			}
			res := g.h.rig.cancelWrite(g.h.ctx, cancel)
			if res.Err != nil || !res.Absent {
				t.Fatalf("cancel sweep absent=%v err=%v sweep=%+v", res.Absent, res.Err, res.Sweep)
			}
			g.o.applyWriteResult(res)
			g.o.pump(gateFailDebounce, writes)
			select {
			case req := <-writes:
				t.Fatalf("stale q dispatched %+v before fill/position truth", req)
			default:
			}
			if got := g.h.ex.createCount(); got != 0 {
				t.Fatalf("exchange received %d creates before position truth", got)
			}
			g.h.clk.Advance(time.Millisecond)
			reducerConfirmationPoll(t, g)
			if got := g.h.rig.pf.Q(seamTicker); got != tc.after {
				t.Fatalf("reconciled q=%s, want %s", got.Wire(), tc.after.Wire())
			}
			g.o.evaluate(gateFailDebounce)
			g.o.pump(gateFailDebounce, writes)
			if tc.after == 0 {
				select {
				case req := <-writes:
					t.Fatalf("flat position dispatched %+v after reconciliation", req)
				default:
				}
				return
			}
			select {
			case req := <-writes:
				if req.Op != quote.OpPlace || req.Side != quote.SideNo ||
					req.Order.Count() != tc.after {
					t.Fatalf("reconciled replacement %+v, want NO/%s", req, tc.after.Wire())
				}
			default:
				t.Fatal("reconciled nonflat position did not get a reducer")
			}
		})
	}
}

func TestPreCancelStartedPortfolioReadCannotReleaseReducer(t *testing.T) {
	one := num.QtyFromFloat(1)
	g := newGateFailOwner(t, one)
	const oldID = "EX-OLDER-READ"
	g.h.installResting(risk.LiveOrder{
		OrderID: oldID, Ticker: seamTicker, Side: quote.SideNo,
		Price4: num.Price4FromCents(55), Remaining: one,
	})
	// Establish the current generation and A13 truth normally.
	g.h.ex.setPosition(seamTicker, one.Wire())
	reducerConfirmationPoll(t, g)
	src := &pausedPositionSource{
		src: g.h.rig.api, started: make(chan struct{}), release: make(chan struct{}),
	}
	p, err := wsx.NewPoller(src, g.h.clk, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(g.h.ctx)
	defer cancel()
	out := make(chan wsx.PortfolioRead, 1)
	done := make(chan error, 1)
	g.tokens <- g.o.reconcileToken
	go func() { done <- p.Run(ctx, g.tokens, out) }()
	select {
	case <-src.started:
	case <-time.After(5 * time.Second):
		t.Fatal("older portfolio read did not start")
	}
	g.o.applyWriteResult(writeResult{
		Req: writeRequest{Market: seamTicker, Side: quote.SideNo,
			Op: quote.OpCancel, Orders: []rest.Order{{
				OrderID: oldID, Ticker: seamTicker, Side: quote.SideNo,
			}}},
		Absent: true, Sent: true,
		Sweep: rest.SweepResult{Walk: rest.Walk{Outcome: rest.WalkComplete}, Clean: true},
	})
	g.h.clk.Advance(time.Millisecond)
	close(src.release)
	var read wsx.PortfolioRead
	select {
	case read = <-out:
	case <-time.After(5 * time.Second):
		t.Fatal("older portfolio read did not complete")
	}
	if read.CompletedAt().Mono <= g.o.cancelTruthAfter ||
		read.StartedAt(wsx.TruthPositions).Mono > g.o.cancelTruthAfter {
		t.Fatalf("test did not cross the cancel with a pre-started read: start=%v end=%v cancel=%v",
			read.StartedAt(wsx.TruthPositions).Mono, read.CompletedAt().Mono, g.o.cancelTruthAfter)
	}
	g.o.applyRead(read)
	if !g.o.awaitCancelTruth || g.o.truthKnown() {
		t.Fatal("pre-cancel-started read released placement or drain truth")
	}
	if _, suppressed := g.o.absentOrders[oldID]; !suppressed {
		t.Fatal("older orders walk resurrected the order after its cancel sweep")
	}
	writes := make(chan writeRequest, 1)
	g.o.evaluate(gateFailDebounce)
	g.o.pump(gateFailDebounce, writes)
	select {
	case req := <-writes:
		t.Fatalf("older in-flight read released placement: %+v", req)
	default:
	}
	// Pending reconciliation blocks placements, not risk-reducing cancels.
	g.h.installResting(risk.LiveOrder{
		OrderID: "EX-OTHER-ADDING", Ticker: seamTicker, Side: quote.SideYes,
		Price4: num.Price4FromCents(40), Remaining: one,
	})
	g.o.enqueueCancel(gateFailDebounce, quote.SideYes, quote.RoleAdding)
	g.o.pump(gateFailDebounce, writes)
	select {
	case req := <-writes:
		if req.Op != quote.OpCancel || req.Side != quote.SideYes {
			t.Fatalf("pending reconciliation dispatched %+v, want adding-side cancel", req)
		}
	default:
		t.Fatal("pending reconciliation blocked an adding-side cancel")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("paused poller did not stop")
	}
}

func TestFlatToFillCancelCannotDeclareDrainedBeforeReconciliation(t *testing.T) {
	one := num.QtyFromFloat(1)
	g := newGateFailOwner(t, 0)
	g.h.ex.setPosition(seamTicker, "0.00")
	reducerConfirmationPoll(t, g)
	const oldID = "EX-FLAT-TO-FILL"
	g.h.seamOwnedOrderOn(oldID, 8, quote.SideYes, 40, one)
	g.o.pending["flat-to-fill"] = pendingOrder{
		side: quote.SideYes, cents: 40, qty: one, acked: true, id: oldID,
	}
	// The adding order fills and disappears immediately before its DELETE.
	// The owner's last complete position is still flat, and this ACKed order
	// has never appeared in Portfolio.LiveOrders.
	g.h.ex.mu.Lock()
	g.h.ex.positions[seamTicker] = one.Wire()
	g.h.ex.fills = append(g.h.ex.fills, seamMakerFill(
		"FLAT-TO-FILL", oldID, "yes", "0.4000", "0.6000", "1.00", 2))
	g.h.ex.mu.Unlock()
	res := g.h.rig.cancelWrite(g.h.ctx, writeRequest{
		Market: seamTicker, Side: quote.SideYes, Op: quote.OpCancel,
		Orders: []rest.Order{{OrderID: oldID, Ticker: seamTicker, Side: quote.SideYes}},
	})
	if res.Err != nil || !res.Absent {
		t.Fatalf("cancel sweep absent=%v err=%v", res.Absent, res.Err)
	}
	g.o.applyWriteResult(res)
	if len(g.o.pending) != 0 || g.o.anyInventory() || g.o.anyLiveOrder() {
		t.Fatal("test did not create a locally flat and quiet account")
	}
	facts := g.o.globalFacts()
	if facts.RiskKnown || facts.TruthReadable {
		t.Fatal("cancel/fill race claimed known risk before a fresh position walk")
	}
	facts.State = quote.WindingDown
	if next, _ := quote.NextGlobal(facts); next != quote.WindingDown {
		t.Fatalf("flat-looking cancel/fill race transitioned to %s", next)
	}
	g.h.clk.Advance(time.Millisecond)
	reducerConfirmationPoll(t, g)
	if got := g.h.rig.pf.Q(seamTicker); got != one {
		t.Fatalf("fresh reconciliation q=%s, want filled +1", got.Wire())
	}
}

// Startup adoption seeds a local portfolio before any live endpoint truth has
// landed. A halt in that interval cannot claim the account is known-flat just
// because its last adopted quantities happen to be zero.
func TestMissingFirstPortfolioWalkCannotAuthorizeDrain(t *testing.T) {
	g := newGateFailOwner(t, 0)
	for _, kind := range []wsx.Truth{
		wsx.TruthFills, wsx.TruthOrders, wsx.TruthPositions,
	} {
		if age := g.h.rig.gate.TruthAge(kind, g.h.clk.Now()); age != -1 {
			t.Fatalf("test unexpectedly has %v truth at age %v", kind, age)
		}
	}
	facts := g.o.globalFacts()
	if facts.RiskKnown || facts.TruthReadable || facts.Reconciled {
		t.Fatalf("missing live portfolio truth claimed known risk: %+v", facts)
	}
	facts.State = quote.WindingDown
	if next, _ := quote.NextGlobal(facts); next != quote.WindingDown {
		t.Fatalf("missing portfolio truth drained a halted account: %s", next)
	}
	g.o.applySignal(os.Interrupt)
	if g.o.global != quote.WindingDown {
		t.Fatalf("SIGINT before first portfolio walk moved global to %s, want WINDING_DOWN", g.o.global)
	}
}

// lip-opm: truth ages run on the monotonic clock, which stands still while the
// host sleeps, so reads from before an observation gap would look seconds old.
// The gap retires them: truth, the drain and the turnover proofs wait for a read
// started after it. A reconnect retires nothing, because REST ages are real.
func TestObservationGapRetiresPreGapPortfolioTruth(t *testing.T) {
	g := newGateFailOwner(t, 0)
	g.h.ex.setPosition(seamTicker, "0.00")
	reducerConfirmationPoll(t, g)
	if !g.o.truthKnown() {
		t.Fatal("the opening walk did not establish truth")
	}
	g.disconnect(false)
	g.connect()
	if !g.o.truthKnown() {
		t.Fatal("a reconnect retired portfolio truth")
	}
	for len(g.tokens) > 0 { // the reconnect's tokens; the gap offers its own
		<-g.tokens
	}

	wall, mono := g.h.clk.wallMs(), g.h.clk.monoNow()
	g.o.observeSleep(wall, mono, g.tokens)
	g.o.observeSleep(wall+(10*time.Minute).Milliseconds(), mono+time.Millisecond, g.tokens)
	if g.o.truthKnown() {
		t.Fatal("pre-sleep portfolio truth still counts as known after the gap")
	}
	for _, k := range []wsx.Truth{wsx.TruthFills, wsx.TruthOrders, wsx.TruthPositions} {
		if age := g.h.rig.gate.TruthAge(k, g.h.clk.Now()); age >= 0 {
			t.Fatalf("%v truth reads %v old after the gap; turnover proofs would accept it", k, age)
		}
	}
	facts := g.o.globalFacts()
	facts.State = quote.WindingDown
	if next, _ := quote.NextGlobal(facts); next != quote.WindingDown {
		t.Fatalf("a flat account drained from pre-sleep truth: %s", next)
	}

	g.h.clk.Advance(time.Millisecond)
	reducerConfirmationPoll(t, g)
	if !g.o.truthKnown() {
		t.Fatal("a complete read started after the gap did not restore truth")
	}
}

// A cancel sweep proves resting absence, not that an ambiguous create never
// reached the exchange. Its maximum possibly-live quantity stays in every cap.
func TestConfirmedAbsenceDoesNotReleaseUnknownCreate(t *testing.T) {
	one := num.QtyFromFloat(1)
	g := newGateFailOwner(t, one)
	const oldID = "EX-OLD-EXIT"
	g.h.installResting(risk.LiveOrder{
		OrderID: oldID, Ticker: seamTicker, Side: quote.SideNo,
		Price4: num.Price4FromCents(55), Remaining: one,
	}, risk.LiveOrder{
		OrderID: "EX-OTHER-SIDE", Ticker: seamTicker, Side: quote.SideYes,
		Price4: num.Price4FromCents(40), Remaining: one,
	}, risk.LiveOrder{
		OrderID: "EX-OTHER-MARKET", Ticker: "OTHER", Side: quote.SideNo,
		Price4: num.Price4FromCents(55), Remaining: one,
	})
	g.o.pending["unresolved"] = pendingOrder{
		side: quote.SideNo, cents: 55, qty: one,
	}
	g.o.applyWriteResult(writeResult{
		Req: writeRequest{
			Market: seamTicker, Side: quote.SideNo, Op: quote.OpCancel,
			Orders: []rest.Order{{OrderID: oldID, Ticker: seamTicker, Side: quote.SideNo}},
		},
		Absent: true, Sent: true,
		Sweep: rest.SweepResult{Walk: rest.Walk{Outcome: rest.WalkComplete}, Clean: true},
	})
	if got := g.o.atRisk(quote.SideNo); got != one {
		t.Fatalf("UNKNOWN create's maximum possibly-live size became %s, want %s", got.Wire(), one.Wire())
	}
	if got, _ := g.o.targetSize(quote.SideNo, quote.RoleReducing); got != 0 {
		t.Fatalf("replacement was sized at %s despite an unresolved create at the cap", got.Wire())
	}
	if got := g.o.atRisk(quote.SideYes); got != one {
		t.Fatalf("confirmed NO absence changed the YES side aggregate to %s", got.Wire())
	}
	foundOther := false
	for _, e := range g.o.exposures() {
		if e.Ticker == "OTHER" {
			foundOther = e.Adding > 0
		}
	}
	if !foundOther {
		t.Fatal("confirmed absence in the selected market removed another market's exposure")
	}
}

func TestConfirmedAbsenceRetiresNamedAckBeforePortfolioWalk(t *testing.T) {
	one := num.QtyFromFloat(1)
	g := newGateFailOwner(t, one)
	const orderID = "EX-ACKED-EXIT"
	g.o.pending["acked-coid"] = pendingOrder{
		side: quote.SideNo, cents: 55, qty: one, acked: true, id: orderID,
	}
	if got := g.o.atRisk(quote.SideNo); got != one {
		t.Fatalf("ACKed order did not enter the pre-walk cap: %s", got.Wire())
	}
	g.o.applyWriteResult(writeResult{
		Req: writeRequest{
			Market: seamTicker, Side: quote.SideNo, Op: quote.OpCancel,
			Orders: []rest.Order{{OrderID: orderID, Ticker: seamTicker, Side: quote.SideNo}},
		},
		Absent: true, Sent: true,
		Sweep: rest.SweepResult{Walk: rest.Walk{Outcome: rest.WalkComplete}, Clean: true},
	})
	if got := g.o.atRisk(quote.SideNo); got != 0 {
		t.Fatalf("verified-absent ACKed order still blocks the reducer: %s", got.Wire())
	}
	if len(g.o.pending) != 0 {
		t.Fatalf("named ACK remained pending after verified absence: %+v", g.o.pending)
	}
}

func TestCompleteOrdersWalkRestoresPositiveOrderEvidence(t *testing.T) {
	one := num.QtyFromFloat(1)
	g := newGateFailOwner(t, -one)
	const orderID = "EX-RETURNED-EXIT"
	old := risk.LiveOrder{
		OrderID: orderID, Ticker: seamTicker, Side: quote.SideYes,
		Price4: num.Price4FromCents(40), Remaining: one,
	}
	g.h.installResting(old)
	g.o.applyWriteResult(writeResult{
		Req: writeRequest{
			Market: seamTicker, Side: quote.SideYes, Op: quote.OpCancel,
			Orders: []rest.Order{{OrderID: orderID, Ticker: seamTicker, Side: quote.SideYes}},
		},
		Absent: true, Sent: true,
		Sweep: rest.SweepResult{Walk: rest.Walk{Outcome: rest.WalkComplete}, Clean: true},
	})
	if got := g.o.atRisk(quote.SideYes); got != 0 {
		t.Fatalf("the confirmed-absent order contributes %s before the next walk", got.Wire())
	}
	coid, err := rest.Coid("SEAMPRIOR", 0, quote.SideYes, 1)
	if err != nil {
		t.Fatalf("returned order coid: %v", err)
	}
	g.h.ex.mu.Lock()
	g.h.ex.resting = []map[string]any{seamRestingOrder(seamCreate{
		Ticker: seamTicker, WireSide: string(rest.Bid), Price: "0.4000",
		Count: one.Wire(), Coid: coid, OrderID: orderID,
	})}
	g.h.ex.mu.Unlock()
	g.h.ex.setPosition(seamTicker, (-one).Wire())
	g.h.clk.Advance(time.Millisecond)
	reducerConfirmationPoll(t, g)
	if got := g.o.atRisk(quote.SideYes); got != one {
		t.Fatalf("complete walk positively listed the order, but aggregate is %s, want %s", got.Wire(), one.Wire())
	}
	if _, suppressed := g.o.absentOrders[orderID]; suppressed {
		t.Fatal("the complete orders walk left positive order evidence suppressed")
	}
}
