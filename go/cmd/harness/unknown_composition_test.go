package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"lip/harness/num"
	"lip/harness/quote"
	"lip/harness/rest"
	"lip/harness/risk"
)

// unknownTransport loses every create answer after the request has crossed the
// transport boundary. The original guard remains in front of the seam exchange
// for all other requests.
type unknownTransport struct {
	base  rest.Doer
	mu    sync.Mutex
	coids []string
}

// acceptedUnknownTransport lets the exchange accept the create (and its
// idempotent retries), then loses each answer on the way back to the client.
type acceptedUnknownTransport struct {
	base rest.Doer
}

func (d acceptedUnknownTransport) Do(ctx context.Context, req rest.Request) (rest.Response, error) {
	resp, err := d.base.Do(ctx, req)
	if err != nil || req.Method != "POST" || req.Path != "/portfolio/events/orders" {
		return resp, err
	}
	return rest.Response{}, context.DeadlineExceeded
}

func (d *unknownTransport) Do(ctx context.Context, req rest.Request) (rest.Response, error) {
	if req.Method != "POST" || req.Path != "/portfolio/events/orders" {
		return d.base.Do(ctx, req)
	}
	var body struct {
		Coid string `json:"client_order_id"`
	}
	if err := json.Unmarshal(req.Body, &body); err != nil {
		return rest.Response{}, err
	}
	d.mu.Lock()
	d.coids = append(d.coids, body.Coid)
	d.mu.Unlock()
	return rest.Response{}, context.DeadlineExceeded
}

func (d *unknownTransport) calls() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.coids...)
}

func unknownAnomalies(as []risk.Anomaly) []risk.Anomaly {
	var out []risk.Anomaly
	for _, a := range as {
		if a.Class == "ORDER_UNKNOWN" {
			out = append(out, a)
		}
	}
	return out
}

func TestUnknownCreateComposesThroughDispatcherStoreAndOwner(t *testing.T) {
	g := newGateFailOwner(t, num.QtyFromFloat(1))
	h, o := g.h, g.o
	h.ex.setPosition(seamTicker, "1.00")
	reducerConfirmationPoll(t, g)
	o.reconcileOut = g.tokens
	select {
	case <-g.tokens:
		t.Fatal("stale reconciliation token before UNKNOWN create")
	default:
	}
	d := &unknownTransport{base: h.rig.api.Doer}
	h.rig.api.Doer = d

	ctx, cancel := context.WithCancel(h.ctx)
	resultsDone := make(chan struct{})
	go func() { defer close(resultsDone); _ = o.sd.runResults(ctx) }()
	writes, results := make(chan writeRequest, 4), make(chan writeResult, 4)
	dispatchDone := make(chan struct{})
	go func() { defer close(dispatchDone); h.rig.dispatchLoop(ctx, writes, o.sd.Permits(), results) }()
	defer cancel()

	o.evaluate(h.clk.Now().Mono)
	o.pump(h.clk.Now().Mono, writes)
	var res writeResult
	select {
	case res = <-results:
	case <-time.After(3 * time.Second):
		t.Fatal("owner did not dispatch the placement")
	}
	if res.Err != nil {
		t.Fatalf("dispatch: %v", res.Err)
	}
	cancel()
	<-dispatchDone
	<-resultsDone
	if res.Req.Op != quote.OpPlace || res.Req.Role != quote.RoleAdding || res.Req.Side != quote.SideYes {
		t.Fatalf("wrong owner request: %+v", res.Req)
	}
	if res.Create.Outcome != rest.CreateUnknown || !res.Sent || !res.Create.ReconcileNow() {
		t.Fatalf("timeout lost ambiguity or reconciliation: %+v", res)
	}
	if res.Create.Attempts != o.p.RetrySameCoidMax {
		t.Fatalf("attempts=%d, max=%d", res.Create.Attempts, o.p.RetrySameCoidMax)
	}
	coids := d.calls()
	if len(coids) != o.p.RetrySameCoidMax {
		t.Fatalf("wire creates=%v", coids)
	}
	for _, coid := range coids {
		if coid != res.Create.Coid {
			t.Fatalf("retry minted fresh coid: %v", coids)
		}
	}

	o.applyWriteResult(res)
	select {
	case tok := <-g.tokens:
		if !tok.Valid() || tok != o.reconcileToken {
			t.Fatalf("UNKNOWN create requested stale reconciliation: %+v", tok)
		}
	default:
		t.Fatal("UNKNOWN create did not request immediate reconciliation")
	}
	if got := h.rig.store.Ownership().UnresolvedCount(); got != 1 {
		t.Fatalf("unresolved reservations=%d", got)
	}
	if got := o.atRisk(quote.SideYes); got != num.QtyFromFloat(1) {
		t.Fatalf("UNKNOWN quantity at risk=%s", got.Wire())
	}
	if remaining, _ := o.targetSize(quote.SideYes, quote.RoleAdding); remaining != 0 {
		t.Fatalf("adding cap offered %s on top of UNKNOWN", remaining.Wire())
	}
	exposure := o.exposures()
	if len(exposure) != 1 || exposure[0].Adding != risk.SideCost(num.QtyFromFloat(1), num.Price4FromCents(res.Req.Order.PriceCents())) {
		t.Fatalf("UNKNOWN missing from capital exposure: %+v", exposure)
	}
	if got := unknownAnomalies(h.takeRaised()); len(got) != 0 {
		t.Fatalf("ORDER_UNKNOWN before unknown_ping_s: %+v", got)
	}

	// A complete, empty real portfolio walk cannot prove the coid absent.
	h.clk.Advance(o.p.PositionPoll)
	g.tokens <- o.reconcileToken
	reducerConfirmationPoll(t, g)
	if got := o.atRisk(quote.SideYes); got != num.QtyFromFloat(1) {
		t.Fatalf("empty walk erased UNKNOWN: %s", got.Wire())
	}
	deadline := o.pending[res.Create.Coid].at + o.p.UnknownPing
	g.at(deadline - time.Nanosecond)
	o.evaluate(h.clk.Now().Mono)
	if got := unknownAnomalies(h.takeRaised()); len(got) != 0 {
		t.Fatalf("early ORDER_UNKNOWN: %+v", got)
	}
	h.clk.Advance(time.Nanosecond)
	o.evaluate(h.clk.Now().Mono)
	got := unknownAnomalies(h.takeRaised())
	if len(got) != 1 || got[0].Sev != risk.SEV2 {
		t.Fatalf("at unknown_ping_s, ORDER_UNKNOWN=%+v", got)
	}
	h.clk.Advance(time.Second)
	o.evaluate(h.clk.Now().Mono)
	if got := unknownAnomalies(h.takeRaised()); len(got) != 0 {
		t.Fatalf("repeated ORDER_UNKNOWN: %+v", got)
	}
	o.pump(h.clk.Now().Mono, writes)
	for len(writes) != 0 {
		next := <-writes
		if next.Op == quote.OpPlace && next.Side == res.Req.Side {
			t.Fatalf("owner minted another order on UNKNOWN side: %s after %s", next.Order.ClientOrderID(), res.Create.Coid)
		}
	}
	if got := d.calls(); len(got) != o.p.RetrySameCoidMax {
		t.Fatalf("fresh create after UNKNOWN: %v", got)
	}
}

func TestAcceptedCreateWithLostAnswersResolvesFromPortfolioWithoutDuplicate(t *testing.T) {
	g := newGateFailOwner(t, num.QtyFromFloat(1))
	h, o := g.h, g.o
	h.ex.listCreated = true
	h.ex.setPosition(seamTicker, "1.00")
	reducerConfirmationPoll(t, g)
	o.reconcileOut = g.tokens
	select {
	case <-g.tokens:
		t.Fatal("stale reconciliation token before create")
	default:
	}
	h.rig.api.Doer = acceptedUnknownTransport{base: h.rig.api.Doer}

	ctx, cancel := context.WithCancel(h.ctx)
	resultsDone := make(chan struct{})
	go func() { defer close(resultsDone); _ = o.sd.runResults(ctx) }()
	writes, results := make(chan writeRequest, 4), make(chan writeResult, 4)
	dispatchDone := make(chan struct{})
	go func() { defer close(dispatchDone); h.rig.dispatchLoop(ctx, writes, o.sd.Permits(), results) }()
	defer func() { cancel(); <-dispatchDone; <-resultsDone }()

	o.evaluate(h.clk.Now().Mono)
	o.pump(h.clk.Now().Mono, writes)
	var res writeResult
	select {
	case res = <-results:
	case <-time.After(3 * time.Second):
		t.Fatal("owner did not dispatch the placement")
	}
	if res.Err != nil || res.Create.Outcome != rest.CreateUnknown || !res.Sent ||
		!res.Create.ReconcileNow() || res.Create.Attempts != o.p.RetrySameCoidMax {
		t.Fatalf("lost accepted answers were not UNKNOWN after same-coid retries: %+v", res)
	}
	if res.Req.Op != quote.OpPlace || res.Req.Role != quote.RoleAdding ||
		res.Req.Side != quote.SideYes {
		t.Fatalf("wrong owner request: %+v", res.Req)
	}
	creates := h.ex.allCreates()
	if len(creates) != o.p.RetrySameCoidMax || h.ex.restingCount() != 1 {
		t.Fatalf("exchange attempts=%v, resting=%d", creates, h.ex.restingCount())
	}
	for _, c := range creates {
		if c.Coid != res.Create.Coid || c.OrderID != creates[0].OrderID {
			t.Fatalf("retry changed exchange identity: %+v", creates)
		}
	}
	h.ex.mu.Lock()
	exchangePosition := h.ex.positions[seamTicker]
	h.ex.mu.Unlock()
	if got := exchangePosition; got != "1.00" {
		t.Fatalf("create invented a private fill: position=%s", got)
	}
	if got := h.rig.store.Ownership().UnresolvedCount(); got != 1 {
		t.Fatalf("accepted UNKNOWN missing durable reservation: %d", got)
	}

	o.applyWriteResult(res)
	select {
	case tok := <-g.tokens:
		if !tok.Valid() || tok != o.reconcileToken {
			t.Fatalf("UNKNOWN requested stale reconciliation: %+v", tok)
		}
	default:
		t.Fatal("UNKNOWN did not request immediate reconciliation")
	}
	if got := o.atRisk(quote.SideYes); got != num.QtyFromFloat(1) {
		t.Fatalf("UNKNOWN exposure=%s", got.Wire())
	}
	if remaining, _ := o.targetSize(quote.SideYes, quote.RoleAdding); remaining != 0 {
		t.Fatalf("adding cap offered %s over accepted UNKNOWN", remaining.Wire())
	}
	// The next complete walk sees the real resting order and submits its
	// binding. Wait for that SQLite commit before checking owner release.
	h.clk.Advance(o.p.PositionPoll)
	g.tokens <- o.reconcileToken
	reducerConfirmationPoll(t, g)
	waitFor(t, "listed order binding to commit", func() bool {
		coid, ok := h.rig.store.Ownership().Bound(creates[0].OrderID)
		return ok && coid == res.Create.Coid
	})
	if got := h.rig.store.Ownership().UnresolvedCount(); got != 0 {
		t.Fatalf("listed order left unresolved reservation: %d", got)
	}
	// The binding may commit just after applyRead. A second complete walk
	// transfers the same quantity from pending into the portfolio's live set.
	g.tokens <- o.reconcileToken
	reducerConfirmationPoll(t, g)
	if _, still := o.pending[res.Create.Coid]; still {
		t.Fatalf("listed coid remains pending: %s", res.Create.Coid)
	}
	live := h.rig.pf.LiveOrders()
	if len(live) != 1 || live[0].OrderID != creates[0].OrderID ||
		o.atRisk(quote.SideYes) != num.QtyFromFloat(1) {
		t.Fatalf("reconciled order=%+v, exposure=%s", live, o.atRisk(quote.SideYes).Wire())
	}
	if q := h.rig.pf.Q(seamTicker); q != num.QtyFromFloat(1) {
		t.Fatalf("portfolio inferred a private fill: q=%s", q.Wire())
	}
	o.evaluate(h.clk.Now().Mono)
	o.pump(h.clk.Now().Mono, writes)
	for len(writes) != 0 {
		next := <-writes
		if next.Op == quote.OpPlace && next.Side == quote.SideYes {
			t.Fatalf("owner minted duplicate adding order: %s", next.Order.ClientOrderID())
		}
	}
	if h.ex.createCount() != o.p.RetrySameCoidMax || h.ex.restingCount() != 1 {
		t.Fatalf("reconciliation changed exchange orders: attempts=%d resting=%d",
			h.ex.createCount(), h.ex.restingCount())
	}
	if got := unknownAnomalies(h.takeRaised()); len(got) != 0 {
		t.Fatalf("resolved UNKNOWN raised ORDER_UNKNOWN: %+v", got)
	}
	// A sustained book failure leaves the owner in REDUCING with the inventory
	// cap intact after the ambiguous create is resolved.
	start := h.clk.Now().Mono
	g.snapshot(gateFailThinBook())
	if !g.stopped(start + gateFailDebounce) {
		t.Fatal("sustained gate failure did not enter reducing state")
	}
	if o.market != quote.Reducing {
		t.Fatalf("inventory lost reducing state: %s", o.market)
	}
	if remaining, bound := o.targetSize(quote.SideNo, quote.RoleReducing); bound != num.QtyFromFloat(1) || remaining > bound {
		t.Fatalf("reducer size=%s cap=%s, inventory=1.00", remaining.Wire(), bound.Wire())
	}
	o.publish(h.clk.Now().Mono)
	snap := h.rig.snap.Load()
	if snap == nil || len(snap.Markets) != 1 || snap.Markets[0].Q != num.QtyFromFloat(1) ||
		snap.Markets[0].Sides[quote.SideYes].AtRisk != num.QtyFromFloat(1) {
		t.Fatalf("published state lost inventory or resting exposure: %+v", snap)
	}
	monitored := make(chan risk.StepResult, 1)
	monitor := newMonitor(h.rig.snap, o.p.OwnerStall, func() time.Duration {
		return h.clk.Now().Mono
	}, func(_ time.Duration, step risk.StepResult) { monitored <- step })
	tick := make(chan time.Time, 1)
	monitorDone := make(chan struct{})
	monitorCtx, stopMonitor := context.WithCancel(h.ctx)
	go func() { defer close(monitorDone); monitor.run(monitorCtx, tick) }()
	tick <- time.Now()
	select {
	case step := <-monitored:
		if step.Stale || len(step.Samples) != 1 ||
			step.Samples[0].Snap.Q != num.QtyFromFloat(1) {
			t.Fatalf("monitor lost reconciled inventory: %+v", step)
		}
	case <-time.After(time.Second):
		t.Fatal("monitor did not sample resolved UNKNOWN")
	}
	stopMonitor()
	<-monitorDone
	hb := h.rig.heartbeat()
	if len(hb.Markets) != 1 || hb.Markets[0].State != quote.Reducing ||
		!hb.Markets[0].Q.Known || hb.Markets[0].Q.V != num.QtyFromFloat(1) {
		t.Fatalf("heartbeat lost reconciled inventory: %+v", hb)
	}
}

func TestGuardedUnsentCreateDoesNotEnterUnknownRecovery(t *testing.T) {
	g := newGateFailOwner(t, num.QtyFromFloat(1))
	h, o := g.h, g.o
	h.ex.setPosition(seamTicker, "1.00")
	reducerConfirmationPoll(t, g)
	// Keep the real guard and remove its sentinel after rig construction.
	if err := os.Remove(h.cfg.Paths.LiveOK); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(h.ctx)
	resultsDone := make(chan struct{})
	go func() { defer close(resultsDone); _ = o.sd.runResults(ctx) }()
	writes, results := make(chan writeRequest, 4), make(chan writeResult, 4)
	dispatchDone := make(chan struct{})
	go func() { defer close(dispatchDone); h.rig.dispatchLoop(ctx, writes, o.sd.Permits(), results) }()
	defer func() { cancel(); <-dispatchDone; <-resultsDone }()
	o.evaluate(h.clk.Now().Mono)
	o.pump(h.clk.Now().Mono, writes)
	select {
	case res := <-results:
		var refused *rest.WriteRefused
		if !errors.As(res.Create.Err, &refused) || res.Sent || res.Create.Outcome != rest.CreateRejected || res.Create.Attempts != 0 || res.Create.MaxLive != 0 {
			t.Fatalf("guarded refusal entered UNKNOWN recovery: %+v", res)
		}
		o.applyWriteResult(res)
	case <-time.After(3 * time.Second):
		t.Fatal("owner did not dispatch guarded placement")
	}
	if len(o.pending) != 0 || o.atRisk(quote.SideYes) != 0 || len(unknownAnomalies(h.takeRaised())) != 0 {
		t.Fatal("unsent refusal retained unknown exposure or raised ORDER_UNKNOWN")
	}
	h.clk.Advance(o.p.UnknownPing)
	o.evaluate(h.clk.Now().Mono)
	if got := unknownAnomalies(h.takeRaised()); len(got) != 0 {
		t.Fatalf("unsent refusal escalated after unknown_ping_s: %+v", got)
	}
}
