package main

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"lip/harness/lifecycle"
	"lip/harness/num"
	"lip/harness/quote"
	"lip/harness/rest"
	"lip/harness/risk"
	"lip/harness/wsx"
)

// lip-14o: the phantom reducer.
//
// Candidate-6's R2 stage (2026-09-30, KXDANCINGWITHTHESTARS-26DEC31-EFRE) held
// q = -12 for 20 min 48 s with no reducer resting and nothing alarming. The
// sequence: a reducer was placed, then requoted under §6.5 before the first
// 5 s orders walk had listed it. The cancel leg named the order from
// `o.pending` (`unlistedOn`), the DELETE answered 200, and the verifying read
// still listed the order inside Kalshi's ~1.5 s list lag -- so the sweep was
// not Absent. The owner released the intents and, correctly under H-FAIL-3,
// kept the pending entry. The next complete walk omitted the cancelled order,
// which under H-ORD-2a retires nothing. From there the entry was a phantom:
// `restingOn` counted it, so the reducing side read as already carrying |q|;
// `Decide` saw "ours at the touch" and returned no move; `escalateUnresolved`
// skipped it as acked; and nothing re-swept a side the state still WANTED
// (`evaluateMarket` re-swept only sides it was turning off). H-ORD-4 says the
// opposite: an order unconfirmed after the retry "stays live, fillable and in
// the cancellation obligation, and the next tick sweeps it again". That is
// what this file asserts, end to end, against the real cancel sweep, the real
// poller cycle and the real drain tracker.
//
// The first two tests share the trap: the reducer comes back within one
// `position_poll_s`, and a planned SIGTERM drain can still complete, because
// `anyLiveOrder` counts `len(o.pending)` and a phantom would otherwise hold
// the process at DRAIN_TIMEOUT for ever. The remaining three are the holes an
// adversarial review of the fix found: an obligation left with nothing in the
// model to name, a verifying read that named nothing, and an UNKNOWN create
// whose only identification is the coid a read listed.

// lagDoer is the seam exchange with Kalshi's cancel lag in front of it: for an
// id the test marks as lagging, DELETE answers 200 and then 404 (the measured
// pair) while the resting list and the named read keep showing the order
// resting, until `clear` ends the lag. After `clear` the exchange has no such
// open order, so a later DELETE still answers 404. Everything else passes
// through.
type lagDoer struct {
	inner *seamExchange
	mu    sync.Mutex
	// lagging is each id whose cancel the list has not caught up with, with
	// the number of DELETEs it has answered.
	lagging map[string]int
	// cancelled is every id whose lag has ended: gone from the list, and
	// 404 to any further DELETE.
	cancelled map[string]bool
	// named records every GET /portfolio/orders/{id}, in order.
	named []string
}

func (d *lagDoer) lag(id string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.lagging == nil {
		d.lagging = make(map[string]int)
	}
	d.lagging[id] = 0
}

// clear ends the lag: the exchange's list and record now agree the order is
// gone.
func (d *lagDoer) clear(id string) {
	d.mu.Lock()
	delete(d.lagging, id)
	if d.cancelled == nil {
		d.cancelled = make(map[string]bool)
	}
	d.cancelled[id] = true
	d.mu.Unlock()
	d.inner.mu.Lock()
	kept := d.inner.resting[:0]
	for _, o := range d.inner.resting {
		if o["order_id"] != id {
			kept = append(kept, o)
		}
	}
	d.inner.resting = kept
	d.inner.mu.Unlock()
}

func (d *lagDoer) record(id string) map[string]any {
	d.inner.mu.Lock()
	defer d.inner.mu.Unlock()
	for _, o := range d.inner.resting {
		if o["order_id"] == id {
			rec := make(map[string]any, len(o))
			for k, v := range o {
				rec[k] = v
			}
			return rec
		}
	}
	return nil
}

func (d *lagDoer) Do(ctx context.Context, req rest.Request) (rest.Response, error) {
	const cancelPrefix = "/portfolio/events/orders/"
	const namedPrefix = "/portfolio/orders/"
	notFound := rest.Response{Status: 404,
		Body: []byte(`{"error":{"code":"not_found"}}`)}
	switch {
	case req.Method == "DELETE" && strings.HasPrefix(req.Path, cancelPrefix):
		id := strings.TrimPrefix(req.Path, cancelPrefix)
		d.mu.Lock()
		n, lagging := d.lagging[id]
		if lagging {
			d.lagging[id] = n + 1
		}
		gone := d.cancelled[id]
		d.mu.Unlock()
		if gone {
			d.inner.mu.Lock()
			d.inner.deletes = append(d.inner.deletes, id)
			d.inner.mu.Unlock()
			return notFound, nil
		}
		if !lagging {
			return d.inner.Do(ctx, req)
		}
		d.inner.mu.Lock()
		d.inner.deletes = append(d.inner.deletes, id)
		d.inner.mu.Unlock()
		if n > 0 {
			// The second DELETE of an order the exchange already cancelled:
			// 404, as production measured (lip-kaf, lip-9tt).
			return notFound, nil
		}
		rec := d.record(id)
		reduced := "0.00"
		if rec != nil {
			reduced, _ = rec["remaining_count"].(string)
		}
		body, err := json.Marshal(map[string]any{
			"order_id": id, "client_order_id": "", "reduced_by": reduced, "ts_ms": 1,
		})
		if err != nil {
			return rest.Response{}, err
		}
		return rest.Response{Status: 200, Body: body}, nil

	case req.Method == "GET" && strings.HasPrefix(req.Path, namedPrefix):
		id := strings.TrimPrefix(req.Path, namedPrefix)
		d.mu.Lock()
		d.named = append(d.named, id)
		d.mu.Unlock()
		rec := d.record(id)
		if rec == nil {
			return notFound, nil
		}
		body, err := json.Marshal(map[string]any{"order": rec})
		if err != nil {
			return rest.Response{}, err
		}
		return rest.Response{Status: 200, Body: body}, nil
	}
	return d.inner.Do(ctx, req)
}

// flakyDoer fails the next N complete-list reads (GET /portfolio/orders) with
// a 503, which is how a verifying read ends INCOMPLETE (SWEEP_UNVERIFIED).
type flakyDoer struct {
	inner    rest.Doer
	mu       sync.Mutex
	failList int
}

func (d *flakyDoer) Do(ctx context.Context, req rest.Request) (rest.Response, error) {
	if req.Method == "GET" && req.Path == "/portfolio/orders" {
		d.mu.Lock()
		fail := d.failList > 0
		if fail {
			d.failList--
		}
		d.mu.Unlock()
		if fail {
			return rest.Response{Status: 503,
				Body: []byte(`{"error":{"code":"unavailable"}}`)}, nil
		}
	}
	return d.inner.Do(ctx, req)
}

func (d *flakyDoer) failNext(n int) {
	d.mu.Lock()
	d.failList = n
	d.mu.Unlock()
}

// phantomFixture is the owner-level fixture plus the lagging transport and
// every anomaly the owner has raised, drained from the sink after each tick
// because nothing else consumes it here.
type phantomFixture struct {
	g      *gateFailOwner
	x      *lagDoer
	fl     *flakyDoer
	raised []risk.Anomaly
}

func (f *phantomFixture) collect() {
	f.raised = append(f.raised, f.g.h.takeRaised()...)
}

func (f *phantomFixture) count(class string) int {
	f.collect()
	n := 0
	for _, a := range f.raised {
		if a.Class == class {
			n++
		}
	}
	return n
}

// tick is one owner iteration at `now` with the dispatcher replaced by the
// test: a cancel goes through the REAL `cancelWrite` against the lagging
// exchange, and a placement is acknowledged unfilled and listed by the
// exchange, exactly as a resting order of ours would be before its first walk.
// It returns the placement it acknowledged, if any.
func (f *phantomFixture) tick(t *testing.T, now time.Duration, orderID string) (writeRequest, bool) {
	t.Helper()
	return f.tickWith(t, now, orderID, false)
}

// tickUnknown is `tick` with the placement ending UNKNOWN: no id, the
// transport timed out, and the exchange DID take the order and lists it under
// our coid. The coid is reserved first, as `dispatch.go` does before the
// POST, so a later walk can bind the listed order to it.
func (f *phantomFixture) tickUnknown(t *testing.T, now time.Duration, orderID string) (writeRequest, bool) {
	t.Helper()
	return f.tickWith(t, now, orderID, true)
}

func (f *phantomFixture) tickWith(t *testing.T, now time.Duration, orderID string,
	unknown bool) (writeRequest, bool) {

	t.Helper()
	g, x := f.g, f.x
	writes := make(chan writeRequest, 4)
	g.o.evaluate(now)
	g.o.pump(now, writes)
	var placed writeRequest
	var any bool
	for {
		var req writeRequest
		select {
		case req = <-writes:
		default:
			f.collect()
			return placed, any
		}
		switch req.Op {
		case quote.OpCancel:
			g.o.applyWriteResult(g.h.rig.cancelWrite(g.h.ctx, req))
		case quote.OpPlace:
			count := req.Order.Count()
			create := rest.CreateResult{
				Outcome: rest.CreateAcked, Coid: req.Order.ClientOrderID(),
				OrderID: orderID, MaxLive: count, Remaining: count,
			}
			if unknown {
				if _, err := g.h.rig.store.ReserveOrder(g.h.rig.run, req.Order,
					req.Role, g.h.clk.wallMs()); err != nil {
					t.Fatalf("reserving %s: %v", req.Order.ClientOrderID(), err)
				}
				create = rest.CreateResult{
					Outcome: rest.CreateUnknown, Coid: req.Order.ClientOrderID(),
					Attempts: 1, MaxLive: count, Err: errors.New("timeout"),
				}
			}
			g.o.applyWriteResult(writeResult{
				Req: req, Sent: true, Bound: true, Create: create,
			})
			wireSide, yesCents, err := rest.ToWire(req.Side, req.Order.PriceCents())
			if err != nil {
				t.Fatalf("wire side for %+v: %v", req, err)
			}
			x.inner.mu.Lock()
			x.inner.resting = append(x.inner.resting, seamRestingOrder(seamCreate{
				Ticker: seamTicker, WireSide: string(wireSide), Price: rest.PriceWire(yesCents),
				Count: count.Wire(), Coid: req.Order.ClientOrderID(),
				OrderID: orderID,
			}))
			x.inner.mu.Unlock()
			placed, any = req, true
		default:
			t.Fatalf("unexpected %s dispatched", req.Op)
		}
	}
}

// poll is one real poller cycle started strictly after `now`. It is the 5 s
// cadence tick: a fresh `wsx.Poller` fires on a reconcile token or on its own
// timer, so the owner's current token is handed to it when nothing else has.
func (f *phantomFixture) poll(t *testing.T) time.Duration {
	t.Helper()
	f.g.h.clk.Advance(time.Millisecond)
	select {
	case f.g.tokens <- f.g.o.reconcileToken:
	default:
	}
	reducerConfirmationPoll(t, f.g)
	f.collect()
	return f.g.h.clk.monoNow()
}

// recover runs owner ticks at 250 ms for up to one `position_poll_s`, running
// a fresh poller cycle whenever the owner holds placement for cancel truth,
// and returns the replacement reducer it acknowledged.
func (f *phantomFixture) recover(t *testing.T, now time.Duration,
	orderID string) (writeRequest, time.Duration, bool) {

	t.Helper()
	ticks := int(f.g.o.p.PositionPoll / (250 * time.Millisecond))
	for i := 0; i < ticks; i++ {
		f.g.h.clk.Advance(250 * time.Millisecond)
		now = f.g.h.clk.monoNow()
		if placed, ok := f.tick(t, now, orderID); ok {
			return placed, now, true
		}
		if f.g.o.awaitCancelTruth {
			now = f.poll(t)
		}
	}
	return writeRequest{}, now, false
}

// requote moves the YES touch one tick above our 40c reducer and ticks until
// the owner has sent a cancel for it, which it must do within the debounce.
func (f *phantomFixture) requote(t *testing.T, now time.Duration) time.Duration {
	t.Helper()
	h := f.g.h
	f.g.snapshot([][]string{{"0.4100", "5.00"}, {"0.4000", "20.00"}},
		[][]string{{"0.5500", "20.00"}})
	deletesBefore := h.ex.deleteCount()
	for i := 0; i < 4; i++ {
		h.clk.Advance(250 * time.Millisecond)
		now = h.clk.monoNow()
		if _, placedAgain := f.tick(t, now, "EX-UNEXPECTED"); placedAgain {
			t.Fatal("a second reducer was placed while the first was unconfirmed")
		}
		if h.ex.deleteCount() > deletesBefore {
			return now
		}
	}
	t.Fatalf("the requote never cancelled the resting reducer; queue=%v",
		h.rig.queue.Pending())
	return now
}

// bindPlaced makes the acknowledged order OURS in the durable ledger, the way
// `dispatch.go` submits the binding inline on the ack. Without it a fill on the
// order is FOREIGN (H-ORD-9) and the test would be exercising the wrong path.
func bindPlaced(t *testing.T, h *seamHarness, req writeRequest, orderID string) {
	t.Helper()
	coid := req.Order.ClientOrderID()
	if _, err := h.rig.store.ReserveOrder(h.rig.run, req.Order, req.Role,
		h.clk.wallMs()); err != nil {
		t.Fatalf("reserving %s: %v", coid, err)
	}
	if _, err := h.rig.store.BindOrder(coid, orderID, h.clk.wallMs()); err != nil {
		t.Fatalf("binding %s -> %s: %v", coid, orderID, err)
	}
	h.await("the ownership binding to commit", func() bool {
		got, ok := h.rig.store.Ownership().Bound(orderID)
		return ok && got == coid
	})
}

func restingYesOrders(h *seamHarness) int {
	h.ex.mu.Lock()
	defer h.ex.mu.Unlock()
	n := 0
	for _, o := range h.ex.resting {
		if o["side"] == "yes" {
			n++
		}
	}
	return n
}

// newPhantomFixture is an owner over the real gate, book, queue, portfolio and
// `cancelWrite`, short 12 YES with a qualifying book whose YES touch is 40c,
// made actionable by a real opening poller cycle. The model position is
// seeded the way a complete positions walk would have, so the opening cycle
// agrees with it rather than reading a hard drift.
func newPhantomFixture(t *testing.T) (*phantomFixture, time.Duration) {
	t.Helper()
	twelve := num.QtyFromFloat(12)
	h := newSeamHarness(t, seamOptions{SkipRig: true})
	x := &lagDoer{inner: h.ex}
	fl := &flakyDoer{inner: x}
	h.xch.Doer = fl
	h.anom = newAnomalySink()
	r, err := newRig(h.ctx, h.cfg, false, h.xch, seamAlertFactory, h.anom, h.qual)
	if err != nil {
		t.Fatalf("newRig: %v", err)
	}
	h.rig = r
	t.Cleanup(h.closeRig)

	g := &gateFailOwner{h: h, o: h.ownerFor(), tokens: make(chan wsx.ReconcileToken, 1)}
	g.o.closeAt = time.Now().Add(24 * time.Hour)
	g.o.hasClose = true
	g.o.scheduleEver = true
	g.connect()
	f := &phantomFixture{g: g, x: x, fl: fl}

	g.snapshot(gateFailQualifyingBook())
	h.installPosition(-twelve)
	h.ex.setPosition(seamTicker, (-twelve).Wire())
	reducerConfirmationPoll(t, g)
	if !h.rig.gate.Actionable(seamTicker, h.clk.Now()) {
		t.Fatal("the opening fake REST walk did not make the book actionable")
	}
	return f, h.clk.monoNow()
}

// phantomReducer drives the owner into the trap and returns it there: q = -12
// in REDUCING, the YES reducer acked and listed, requoted before any walk,
// its cancel sweep unverified inside the list lag (held across three ticks),
// the touch back at the phantom's price, and then one complete cycle that
// omits the cancelled order. Nothing is fabricated on the owner: the create
// ack is the dispatcher's ordinary answer, the sweeps are real, and the walks
// are real poller cycles over the fake REST transport.
func phantomReducer(t *testing.T) (*phantomFixture, time.Duration) {
	t.Helper()
	twelve := num.QtyFromFloat(12)
	f, now := newPhantomFixture(t)
	g, h := f.g, f.g.h

	const phantomID = "EX-PHANTOM-EXIT"
	placed, ok := f.tick(t, now, phantomID)
	if !ok || placed.Side != quote.SideYes || placed.Role != quote.RoleReducing ||
		placed.Order.Count() != twelve {
		t.Fatalf("the reducer was not placed: ok=%v %+v market=%s", ok, placed, g.o.market)
	}
	if g.o.market != quote.Reducing {
		t.Fatalf("market is %s, want REDUCING at q = -12", g.o.market)
	}
	p, pending := g.o.pending[placed.Order.ClientOrderID()]
	if !pending || !p.acked || p.id != phantomID {
		t.Fatalf("acked reducer is not pending its first walk: %+v", g.o.pending)
	}

	// §6.5 before the first walk: an outside YES bid appears one tick above
	// ours and holds for the debounce. The owner must requote, and the cancel
	// leg can only name the order from `pending`. The lag then holds across
	// two more ticks, so more than one sweep comes back unverified.
	f.x.lag(phantomID)
	now = f.requote(t, now)
	deletes := h.ex.deleteCount()
	for i := 0; i < 2; i++ {
		h.clk.Advance(250 * time.Millisecond)
		now = h.clk.monoNow()
		if _, placedAgain := f.tick(t, now, "EX-UNEXPECTED"); placedAgain {
			t.Fatal("a reducer was placed while the first was unconfirmed")
		}
	}
	if h.ex.deleteCount() < deletes+1 {
		t.Fatalf("the unverified side was not swept again while the touch was "+
			"away: %d DELETEs", h.ex.deleteCount())
	}
	if n := f.count("CANCEL_UNVERIFIED"); n < 1 {
		t.Fatal("the lagged sweep did not report CANCEL_UNVERIFIED")
	}
	if _, still := g.o.pending[placed.Order.ClientOrderID()]; !still {
		t.Fatal("H-FAIL-3: the unverified cancel must keep the order in the risk model")
	}
	if got := g.o.atRisk(quote.SideYes); got != twelve {
		t.Fatalf("unverified reducer at risk %s, want %s", got.Wire(), twelve.Wire())
	}

	// The flicker ends: the outside bid is gone and the YES touch is back at
	// 40c, where the phantom's own record says it rests. With the touch away
	// §6.5 itself would re-sweep on the next tick (that is how production
	// escaped at 21:46:36Z); with the touch back `Decide` sees our order at
	// the touch and asks for nothing, and the same holds for a touch that
	// moved in our favour.
	g.snapshot(gateFailQualifyingBook())

	// The lag ends. The next complete cycle omits the cancelled order, and by
	// H-ORD-2a that omission retires nothing. This is the trap.
	f.x.clear(phantomID)
	h.clk.Advance(1500 * time.Millisecond)
	now = f.poll(t)
	if _, still := g.o.pending[placed.Order.ClientOrderID()]; !still {
		t.Fatal("absence from a complete walk retired the pending entry (H-ORD-2a)")
	}
	if got := h.rig.pf.LiveOrders(); len(got) != 0 {
		t.Fatalf("the omitting walk still lists %+v", got)
	}
	return f, now
}

func TestUnverifiedReducerRequoteIsResweptAndTheReducerReturns(t *testing.T) {
	twelve := num.QtyFromFloat(12)
	f, now := phantomReducer(t)
	g := f.g

	const replacementID = "EX-REPLACEMENT-EXIT"
	placed, _, ok := f.recover(t, now, replacementID)
	if !ok {
		t.Fatalf("no reducer was placed within one position_poll_s of the "+
			"omitting walk: q=%s pending=%+v live=%d sweptOrders=%d; the "+
			"unverified cancel left a phantom that §6.5 reads as our order "+
			"at the touch (lip-14o, H-ORD-4)", g.h.rig.pf.Q(seamTicker).Wire(),
			g.o.pending, len(g.h.rig.pf.LiveOrders()), len(g.o.sweptOrders))
	}
	if placed.Side != quote.SideYes || placed.Role != quote.RoleReducing ||
		placed.Order.Count() != twelve {
		t.Fatalf("replacement %+v, want a YES reducer of %s", placed, twelve.Wire())
	}
	// The phantom is gone from the model by exchange confirmation, and only
	// the replacement is carried.
	if n := len(g.o.pending); n != 1 {
		t.Fatalf("%d pending entries after the replacement, want only the "+
			"replacement: %+v", n, g.o.pending)
	}
	if got := g.o.atRisk(quote.SideYes); got != twelve {
		t.Fatalf("YES at risk %s after the replacement, want exactly |q| = %s "+
			"(H-Q-5a)", got.Wire(), twelve.Wire())
	}
	// One SEV2 per episode, not one per re-sweep: three sweeps came back
	// unverified and the operator is told once that a cancel is unconfirmed;
	// SWEEP_PENDING/SWEEP_INCOMPLETE carry the rest.
	if n := f.count("CANCEL_UNVERIFIED"); n != 1 {
		t.Fatalf("CANCEL_UNVERIFIED raised %d times for one episode, want 1", n)
	}
	if n := f.count("WRITE_NOT_BUILDABLE"); n != 0 {
		t.Fatalf("WRITE_NOT_BUILDABLE raised %d times", n)
	}
}

func TestPlannedDrainCompletesThroughAPhantomReducer(t *testing.T) {
	twelve := num.QtyFromFloat(12)
	f, now := phantomReducer(t)
	g := f.g
	h := g.h

	// The operator asks for a planned exit while the phantom is in place.
	g.o.applySignal(syscall.SIGTERM)
	f.collect()
	if g.o.global != quote.WindingDown {
		t.Fatalf("SIGTERM left the harness %s, want WINDING_DOWN", g.o.global)
	}
	if got := h.latchTrigger(); got != "sigterm" {
		t.Fatalf("latch trigger %q, want sigterm", got)
	}
	drain := h.rig.drain
	observe := func() lifecycle.DrainEffects {
		return drain.Observe(lifecycle.DrainObservation{
			TruthKnown: g.o.truthKnown(), AnyInventory: g.o.anyInventory(),
			AnyLiveOrder: g.o.anyLiveOrder(),
		}, h.clk.monoNow())
	}
	if eff := observe(); eff.ExitAuthorised {
		t.Fatal("the drain authorised an exit with q = -12")
	}

	// The exit must come back under WINDING_DOWN exactly as under RUNNING:
	// I1 keeps the reducer alive through every stop.
	const replacementID = "EX-DRAIN-EXIT"
	placed, now, ok := f.recover(t, now, replacementID)
	if !ok {
		t.Fatalf("no reducer was placed within one position_poll_s under the "+
			"drain: pending=%+v; the drain would sit at DRAIN_TIMEOUT for "+
			"ever (lip-14o)", g.o.pending)
	}
	bindPlaced(t, h, placed, replacementID)

	// The replacement fills in full before its first walk lists it. The
	// account is flat; the exchange lists nothing of ours.
	h.ex.mu.Lock()
	h.ex.positions[seamTicker] = "0.00"
	h.ex.fills = append(h.ex.fills, seamMakerFill("PHANTOM-DRAIN-FILL",
		replacementID, "yes", "0.4000", "0.6000", twelve.Wire(), h.clk.wallMs()))
	kept := h.ex.resting[:0]
	for _, o := range h.ex.resting {
		if o["order_id"] != replacementID {
			kept = append(kept, o)
		}
	}
	h.ex.resting = kept
	h.ex.mu.Unlock()
	now = f.poll(t)
	if got := h.rig.pf.Q(seamTicker); got != 0 {
		t.Fatalf("q=%s after the fill walk, want flat", got.Wire())
	}

	// Flat and winding down: whatever the model still carries for the filled
	// order must be confirmed gone by the exchange, and then the planned
	// drain must authorise the exit.
	ticks := int(g.o.p.PositionPoll / (250 * time.Millisecond))
	authorised := false
	for i := 0; i < ticks && !authorised; i++ {
		h.clk.Advance(250 * time.Millisecond)
		now = h.clk.monoNow()
		if _, placedAgain := f.tick(t, now, "EX-UNEXPECTED"); placedAgain {
			t.Fatal("a flat account placed an order under WINDING_DOWN")
		}
		if g.o.awaitCancelTruth {
			now = f.poll(t)
		}
		authorised = observe().ExitAuthorised
	}
	if !authorised {
		t.Fatalf("the planned drain never authorised the exit: truth=%v "+
			"inventory=%v live=%v pending=%+v live_orders=%d global=%s",
			g.o.truthKnown(), g.o.anyInventory(), g.o.anyLiveOrder(),
			g.o.pending, len(h.rig.pf.LiveOrders()), g.o.global)
	}
	if n := len(g.o.pending); n != 0 {
		t.Fatalf("exit authorised with %d pending entries: %+v", n, g.o.pending)
	}
	_ = now
}

// A LISTED reducer is requoted; its DELETE takes, but the verifying read is
// INCOMPLETE (SWEEP_UNVERIFIED), so nothing enters `sweptOrders`. The touch
// returns and the next complete walk omits the order: the model now holds
// nothing of ours on the side while H-ORD-4 still keeps it in the
// obligation. The latch must license the read-only sweep that discharges it,
// rather than refuse the cancel as unbuildable on every tick and never decide
// the exit again (adversarial review finding 1).
func TestUnverifiedCancelFromAnIncompleteReadIsDischargedByItsOwnRead(t *testing.T) {
	twelve := num.QtyFromFloat(12)
	f, now := newPhantomFixture(t)
	g, h := f.g, f.g.h

	const listedID = "EX-LISTED-EXIT"
	placed, ok := f.tick(t, now, listedID)
	if !ok || placed.Side != quote.SideYes || placed.Order.Count() != twelve {
		t.Fatalf("the reducer was not placed: ok=%v %+v", ok, placed)
	}
	bindPlaced(t, h, placed, listedID)
	now = f.poll(t)
	if n := len(g.o.pending); n != 0 {
		t.Fatalf("still pending after the listing walk: %+v", g.o.pending)
	}
	if n := len(h.rig.pf.LiveOrders()); n != 1 {
		t.Fatalf("%d live orders after the listing walk, want 1", n)
	}

	// The requote's DELETE lands at the exchange; its verifying read is a 503.
	f.fl.failNext(1)
	now = f.requote(t, now)
	if n := f.count("SWEEP_UNVERIFIED"); n != 1 {
		t.Fatalf("SWEEP_UNVERIFIED raised %d times, want 1", n)
	}
	if n := f.count("CANCEL_UNVERIFIED"); n != 1 {
		t.Fatalf("CANCEL_UNVERIFIED raised %d times, want 1", n)
	}

	// The touch is back, and the next complete walk omits the cancelled
	// order. Nothing of ours is left in the model.
	g.snapshot(gateFailQualifyingBook())
	h.clk.Advance(1500 * time.Millisecond)
	now = f.poll(t)
	if n := len(h.rig.pf.LiveOrders()); n != 0 {
		t.Fatalf("%d live orders after the omitting walk, want 0", n)
	}
	if got := g.o.atRisk(quote.SideYes); got != 0 {
		t.Fatalf("YES at risk %s after the omitting walk, want 0", got.Wire())
	}

	placed, _, ok = f.recover(t, now, "EX-REPLACEMENT-EXIT")
	if !ok {
		t.Fatalf("no reducer within one position_poll_s of the omitting walk: "+
			"pending=%+v live=%d queue=%v WRITE_NOT_BUILDABLE=%d", g.o.pending,
			len(h.rig.pf.LiveOrders()), h.rig.queue.Pending(),
			f.count("WRITE_NOT_BUILDABLE"))
	}
	if placed.Order.Count() != twelve {
		t.Fatalf("replacement %+v, want |q| = %s", placed, twelve.Wire())
	}
	if n := f.count("WRITE_NOT_BUILDABLE"); n != 0 {
		t.Fatalf("the read-only sweep was refused %d time(s) as unbuildable", n)
	}
	if n := f.count("CANCEL_UNVERIFIED"); n != 1 {
		t.Fatalf("CANCEL_UNVERIFIED raised %d times for one episode, want 1", n)
	}
}

// An UNKNOWN create -- no id, the transport timed out -- that the exchange
// took and lists at the touch, correctly sized. A one-tick flicker makes §6.5
// requote; the cancel leg has nothing to name, so it is a verifying read
// alone, which finds the order (`OtherOurs`) and comes back not Absent. The
// touch then returns. A read that named nothing sent no DELETE and must not
// open an obligation: the next ticks must not cancel a correctly sized live
// exit that §6.5 no longer wants moved, and the next walk must list it into
// the model (adversarial review finding 2).
func TestAVerifyingReadThatNamedNothingDoesNotCancelTheExitOnAFlicker(t *testing.T) {
	twelve := num.QtyFromFloat(12)
	f, now := newPhantomFixture(t)
	g, h := f.g, f.g.h

	const unknownID = "EX-UNKNOWN-EXIT"
	placed, ok := f.tickUnknown(t, now, unknownID)
	if !ok || placed.Side != quote.SideYes || placed.Order.Count() != twelve {
		t.Fatalf("the reducer was not placed: ok=%v %+v", ok, placed)
	}
	p, pending := g.o.pending[placed.Order.ClientOrderID()]
	if !pending || p.acked || p.id != "" {
		t.Fatalf("UNKNOWN create is not pending without an id: %+v", g.o.pending)
	}

	// The flicker: the verifying read finds the order and nothing is deleted.
	g.snapshot([][]string{{"0.4100", "5.00"}, {"0.4000", "20.00"}},
		[][]string{{"0.5500", "20.00"}})
	deletesBefore := h.ex.deleteCount()
	for i := 0; i < 4 && f.count("CANCEL_UNVERIFIED") == 0; i++ {
		h.clk.Advance(250 * time.Millisecond)
		now = h.clk.monoNow()
		if _, placedAgain := f.tick(t, now, "EX-UNEXPECTED"); placedAgain {
			t.Fatal("a reducer was placed over the unresolved create")
		}
	}
	if n := f.count("CANCEL_UNVERIFIED"); n != 1 {
		t.Fatalf("the verifying read did not come back unverified: %d", n)
	}
	if got := h.ex.deleteCount(); got != deletesBefore {
		t.Fatalf("the verifying read sent %d DELETE(s)", got-deletesBefore)
	}
	if !g.o.sweptOn(quote.SideYes) {
		t.Fatal("the verifying read did not record the order it found")
	}

	// The touch is back where our order rests. One poll interval of ticks.
	g.snapshot(gateFailQualifyingBook())
	ticks := int(g.o.p.PositionPoll / (250 * time.Millisecond))
	for i := 0; i < ticks; i++ {
		h.clk.Advance(250 * time.Millisecond)
		now = h.clk.monoNow()
		if _, placedAgain := f.tick(t, now, "EX-UNEXPECTED"); placedAgain {
			t.Fatal("a reducer was placed over the resting exit")
		}
	}
	if got := h.ex.deleteCount(); got != deletesBefore {
		t.Fatalf("the correctly sized live exit was DELETEd %d time(s) after a "+
			"read that named nothing", got-deletesBefore)
	}
	if n := restingYesOrders(h); n != 1 {
		t.Fatalf("the exchange rests %d YES orders of ours, want 1", n)
	}
	// The walk lists it under the reserved coid: the create is resolved
	// positively and the exit is in the model as a listed order.
	now = f.poll(t)
	if len(g.o.pending) != 0 || len(h.rig.pf.LiveOrders()) != 1 {
		t.Fatalf("pending=%+v live=%d after the listing walk, want the exit "+
			"listed and nothing pending", g.o.pending, len(h.rig.pf.LiveOrders()))
	}
	if got := g.o.atRisk(quote.SideYes); got != twelve {
		t.Fatalf("YES at risk %s, want %s", got.Wire(), twelve.Wire())
	}
	_ = now
}

// The same UNKNOWN create, but the touch STAYS away, so §6.5 keeps wanting
// the move: the next cancel names the order from `sweptOrders` -- with the
// coid the read listed -- and the sweep confirms it absent. The only
// identification of that order is its coid, and that is positive on both
// ends: listed resting by a complete read, then confirmed cancelled by one.
// The id-less entry must be retired and the replacement placed; before, it
// stood at |q| for ever and blocked the side and the drain.
func TestACancelledUnknownCreateIsRetiredByTheCoidAReadListed(t *testing.T) {
	twelve := num.QtyFromFloat(12)
	f, now := newPhantomFixture(t)
	g, h := f.g, f.g.h

	const unknownID = "EX-UNKNOWN-EXIT"
	placed, ok := f.tickUnknown(t, now, unknownID)
	if !ok || placed.Order.Count() != twelve {
		t.Fatalf("the reducer was not placed: ok=%v %+v", ok, placed)
	}
	coid := placed.Order.ClientOrderID()

	// The touch moves away and stays there. The first cancel leg names
	// nothing and only reads; the next one names what that read found.
	g.snapshot([][]string{{"0.4100", "5.00"}, {"0.4000", "20.00"}},
		[][]string{{"0.5500", "20.00"}})
	deletesBefore := h.ex.deleteCount()
	for i := 0; i < 8 && h.ex.deleteCount() == deletesBefore; i++ {
		h.clk.Advance(250 * time.Millisecond)
		now = h.clk.monoNow()
		if _, placedAgain := f.tick(t, now, "EX-UNEXPECTED"); placedAgain {
			t.Fatal("a reducer was placed over the unresolved create")
		}
	}
	if h.ex.deleteCount() == deletesBefore {
		t.Fatalf("the order the read found was never cancelled; queue=%v",
			h.rig.queue.Pending())
	}
	if _, still := g.o.pending[coid]; still {
		t.Fatalf("the cancelled create is still pending after the clean sweep "+
			"that named its coid: %+v", g.o.pending)
	}
	if n := restingYesOrders(h); n != 0 {
		t.Fatalf("the exchange still rests %d YES orders of ours", n)
	}

	replacement, _, ok := f.recover(t, now, "EX-REPLACEMENT-EXIT")
	if !ok {
		t.Fatalf("no replacement within one position_poll_s: pending=%+v "+
			"atRisk=%s anyLiveOrder=%v", g.o.pending,
			g.o.atRisk(quote.SideYes).Wire(), g.o.anyLiveOrder())
	}
	if replacement.Side != quote.SideYes || replacement.Order.Count() != twelve {
		t.Fatalf("replacement %+v, want a YES reducer of %s", replacement, twelve.Wire())
	}
	if n := len(g.o.pending); n != 1 {
		t.Fatalf("%d pending entries, want only the replacement: %+v", n, g.o.pending)
	}
}
