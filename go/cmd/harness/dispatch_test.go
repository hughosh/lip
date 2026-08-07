package main

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"lip/harness/cfg"
	"lip/harness/hstore"
	"lip/harness/num"
	"lip/harness/quote"
	"lip/harness/rest"
	"lip/harness/risk"
)

// These tests are about the SEAMS and nothing else.
//
// Every part joined here is already unit-tested and mutation-guarded in its own
// package: `rest.Create`'s 409 handling, `hstore`'s commit ordering, `quote`'s
// capacity arithmetic. Re-testing any of them here would produce green that
// means nothing. What has never been exercised is the JOIN -- whether the
// reservation really commits before the request leaves, whether the binding
// really goes out on this write's own code path, whether an H-STORE-3 refusal
// really stops an adding order and really does not stop a reducing one.
//
// So the store is REAL SQLite under t.TempDir(). A fake store would pass all of
// these: `DispatchPermit` can only be issued by a committed `hstore` transaction
// and that is the property under test, so there is nothing to substitute.

const dispatchTicker = "KXTESTMARKET-26AUG07-T1"

// dispatchRunID is a coid-safe run id: alphanumeric, no "-" separator.
const dispatchRunID = "20260807T120000ABCDE"

// dispatchMarketIdx is the coid's %03d market field. The pilot is one
// operator-chosen market (pilot-plan §7.1), so the owner mints every coid at
// index 0 and these tests do the same.
const dispatchMarketIdx = 0

// ---------------------------------------------------------------------------
// The fake exchange
// ---------------------------------------------------------------------------

// fakeExchange is a `rest.Doer`, which is to say a TRANSPORT and not a typed
// fake of the API.
//
// The seam is at the status code and the raw bytes for the reason `rest.Doer`
// gives: every failure this path exists to survive -- a 409 that means "your
// order landed", a response that never arrives -- is invisible above that line.
type fakeExchange struct {
	mu      sync.Mutex
	calls   []rest.Request
	creates []string
	deletes []string

	// onCreate runs on the WRITER goroutine at the instant the create request
	// is about to be sent, and it is where the H-ORD-6 ordering is asserted:
	// whatever it observes, it observes strictly before the order exists.
	onCreate func(coid string)

	// createStatus is the status a create is answered with. 0 means 200.
	createStatus int
	// createBody overrides the acknowledgement body when non-nil.
	createBody []byte

	// resting is what the verifying orders walk reports.
	resting []map[string]any
}

func (f *fakeExchange) Do(_ context.Context, req rest.Request) (rest.Response, error) {
	f.mu.Lock()
	f.calls = append(f.calls, req)
	f.mu.Unlock()

	switch {
	case req.Method == "POST" && req.Path == "/portfolio/events/orders":
		return f.create(req)
	case req.Method == "DELETE":
		return f.cancel(req)
	case req.Method == "GET" && req.Path == "/portfolio/orders":
		return f.orders()
	}
	return rest.Response{}, fmt.Errorf("fakeExchange got an unscripted %s %s",
		req.Method, req.Path)
}

func (f *fakeExchange) create(req rest.Request) (rest.Response, error) {
	var body struct {
		Ticker        string `json:"ticker"`
		Count         string `json:"count"`
		ClientOrderID string `json:"client_order_id"`
	}
	if err := json.Unmarshal(req.Body, &body); err != nil {
		return rest.Response{}, err
	}

	f.mu.Lock()
	f.creates = append(f.creates, body.ClientOrderID)
	hook := f.onCreate
	status, custom := f.createStatus, f.createBody
	f.mu.Unlock()

	if hook != nil {
		hook(body.ClientOrderID)
	}
	if status != 0 {
		if custom == nil {
			custom = []byte(`{"error":{"code":"bad_request"}}`)
		}
		return rest.Response{Status: status, Body: custom}, nil
	}

	// The measured flat CreateOrderV2Response. Every field `parseAck` requires
	// is present, because a response missing one is UNKNOWN rather than acked
	// and would silently turn a placement test into a reconciliation test.
	ack, err := json.Marshal(map[string]any{
		"order_id":        "EX-" + body.ClientOrderID,
		"client_order_id": body.ClientOrderID,
		"remaining_count": body.Count,
		"fill_count":      "0.00",
		"ts_ms":           1,
	})
	if err != nil {
		return rest.Response{}, err
	}
	return rest.Response{Status: 200, Body: ack}, nil
}

func (f *fakeExchange) cancel(req rest.Request) (rest.Response, error) {
	f.mu.Lock()
	f.deletes = append(f.deletes, req.Path)
	f.mu.Unlock()
	body, err := json.Marshal(map[string]any{
		"order_id": req.Path, "client_order_id": "", "reduced_by": "1.00",
		"ts_ms": 1,
	})
	if err != nil {
		return rest.Response{}, err
	}
	return rest.Response{Status: 200, Body: body}, nil
}

func (f *fakeExchange) orders() (rest.Response, error) {
	f.mu.Lock()
	items := make([]any, 0, len(f.resting))
	for _, o := range f.resting {
		items = append(items, o)
	}
	f.mu.Unlock()
	// A terminal page: empty cursor, and every declared item array present.
	body, err := json.Marshal(map[string]any{"cursor": "", "orders": items})
	if err != nil {
		return rest.Response{}, err
	}
	return rest.Response{Status: 200, Body: body}, nil
}

func (f *fakeExchange) createdCoids() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.creates...)
}

func (f *fakeExchange) deleteCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.deletes)
}

// restingOrder is a resting-order record shaped like the real payload.
func restingOrder(orderID, coid, side string, price float64) map[string]any {
	return map[string]any{
		"order_id":          orderID,
		"client_order_id":   coid,
		"ticker":            dispatchTicker,
		"side":              side,
		"yes_price_dollars": fmt.Sprintf("%.4f", price),
		"remaining_count":   "1.00",
		"status":            rest.StatusResting,
	}
}

// ---------------------------------------------------------------------------
// A rig with a real store
// ---------------------------------------------------------------------------

// newDispatchRig builds the minimum rig the REST writer touches, over a REAL
// store in t.TempDir(), and returns it with the reservation-result channel the
// writer awaits its permits on.
//
// The writer goroutine is started and torn down with the test; `rig.db` and
// every other database in the tree is off limits.
func newDispatchRig(t *testing.T, ex rest.Doer) (*rig, <-chan hstore.Result) {
	t.Helper()

	dir := t.TempDir()
	store, err := hstore.Open(hstore.StoreConfig{
		DBPath:         filepath.Join(dir, "harness.db"),
		AnomalyLogPath: filepath.Join(dir, "anomaly.jsonl"),
	})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		store.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
		if cerr := store.Close(); cerr != nil {
			t.Logf("closing the test store: %v", cerr)
		}
	})

	p := cfg.Default()
	start := time.Now()
	nowMs := func() int64 { return time.Now().UnixMilli() }

	rcpt, err := store.BeginRun(dispatchRunID, nowMs(), p)
	if err != nil {
		t.Fatalf("begin run: %v", err)
	}
	run, deferred, err := awaitRunHandle(ctx, store, rcpt)
	if err != nil {
		t.Fatalf("await run handle: %v", err)
	}
	if len(deferred) != 0 {
		t.Fatalf("the run row's commit carried %d foreign results; nothing "+
			"else had been submitted", len(deferred))
	}

	r := &rig{
		cfg:   config{Params: p, Ticker: dispatchTicker},
		store: store,
		run:   run,
		runID: dispatchRunID,
		api:   rest.NewClient(ex),
		anom:  newAnomalySink(),
		pf:    risk.NewPortfolio(),
		queue: quote.NewQueue(p.MaxQueueAge),
		cap:   quote.NewCapacity(dispatchWorkers, p.WriteBurst),
		ex: exchange{
			NowMs: nowMs,
			Mono:  func() time.Duration { return time.Since(start) },
		},
	}
	// The result loop must already be the sole consumer before anything is
	// reserved: started later, it would race `awaitPermit` for the FIFO, which
	// is the exact defect the single-consumer rule exists to remove.
	return r, forwardReserves(t, ctx, store)
}

// forwardReserves stands in for `shutdown.go`'s result loop.
//
// It is the SOLE consumer of the store's result FIFO -- `TakeResults` drains
// the whole batch under one mutex, so a second consumer silently takes the
// first one's results -- and it forwards a COPY of every reservation outcome to
// whoever is awaiting a permit. Everything else is dropped here rather than
// asserted on, because what the production loop does with the rest
// (`hstore.Rejections`) is `shutdown.go`'s test to write, not this one's.
func forwardReserves(t *testing.T, ctx context.Context,
	store *hstore.Store) <-chan hstore.Result {

	t.Helper()
	// Buffered so a test that never awaits a permit -- a cancel, a seeded
	// collision -- cannot wedge this goroutine.
	out := make(chan hstore.Result, 64)
	go func() {
		for {
			for _, res := range store.TakeResults() {
				if res.Kind != hstore.KindReserveOrder {
					continue
				}
				select {
				case out <- res:
				case <-ctx.Done():
					return
				}
			}
			select {
			case <-ctx.Done():
				return
			case <-store.Wake():
			}
		}
	}()
	return out
}

// placement builds the request an owner would hand over: a fully built,
// coid-bearing order.
func placement(t *testing.T, r *rig, side quote.Side, role quote.Role,
	priceCents int, seq uint64) writeRequest {

	t.Helper()
	coid, err := rest.Coid(r.runID, dispatchMarketIdx, side, seq)
	if err != nil {
		t.Fatalf("coid: %v", err)
	}
	count := num.QtyFromFloat(1)
	order, err := rest.NewCreateOrder(dispatchTicker, side, priceCents,
		count, count, coid)
	if err != nil {
		t.Fatalf("build order: %v", err)
	}
	return writeRequest{
		IDs: []uint64{seq}, Market: dispatchTicker, Side: side, Role: role,
		Op: quote.OpPlace, Order: order,
	}
}

// waitFor polls a condition for up to a second. Every wait here is on the
// STORE's writer goroutine committing, never on the dispatcher, so a timeout is
// a real failure and not a slow machine.
func waitFor(t *testing.T, why string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", why)
}

// ---------------------------------------------------------------------------
// H-ORD-6 — no dispatch without a committed permit
// ---------------------------------------------------------------------------

// TestNoOrderIsSentWithoutACommittedPermit is the barrier, asserted from the
// only side that can prove it: the wire.
//
// The reservation is made permanently unwritable -- the coid is already
// reserved with different content, which `sqliteBackend.reserveOrder` refuses
// as an H-ORD-1 collision -- so no permit is ever issued. If the writer treated
// `ReserveOrder`'s RECEIPT as licence, or reserved and dispatched
// concurrently, the exchange would see a POST. It must see nothing at all.
func TestNoOrderIsSentWithoutACommittedPermit(t *testing.T) {
	ex := &fakeExchange{}
	r, reserves := newDispatchRig(t, ex)
	req := placement(t, r, quote.SideYes, quote.RoleAdding, 42, 1)

	// Reserve the same coid first, with different content. Its row commits and
	// owns the coid; the dispatcher's reservation is then a collision.
	clash, err := rest.NewCreateOrder(dispatchTicker, quote.SideYes, 43,
		num.QtyFromFloat(1), num.QtyFromFloat(1), req.Order.ClientOrderID())
	if err != nil {
		t.Fatalf("build clashing order: %v", err)
	}
	if _, err := r.store.ReserveOrder(r.run, clash, quote.RoleAdding,
		r.ex.NowMs()); err != nil {
		t.Fatalf("seed reservation: %v", err)
	}
	waitFor(t, "the seed reservation to commit", func() bool {
		return r.store.Ownership().UnresolvedCount() == 1
	})

	res := r.executeWrite(context.Background(), req, reserves)

	if res.Err == nil {
		t.Fatalf("the reservation could never commit, yet the write reported " +
			"no error; H-ORD-6's permit is issued by a committed row and by " +
			"nothing else")
	}
	if got := ex.createdCoids(); len(got) != 0 {
		t.Fatalf("an order was sent without a committed reservation: %v.\n"+
			"This is H-ORD-6: an order whose ownership is unrecorded produces "+
			"a fill H-ORD-9 must classify as FOREIGN, which is a SEV1 and a "+
			"durable global stop declared about our own order", got)
	}
	if res.Bound {
		t.Fatalf("a binding was submitted for an order that was never sent")
	}
	if len(res.Anomalies) == 0 {
		t.Fatalf("a reservation that will never be written is a record that is " +
			"GONE and it raised no anomaly")
	}
}

// TestAPermitIsMatchedByReceiptAndNotByCoid is the identity half of the wait.
//
// A receipt names one SUBMISSION. Matching on the coid instead would accept a
// result belonging to a different submission that happens to name the same coid
// -- which is exactly what an H-ORD-1 collision is, and it is the one case where
// the two results say opposite things: one committed, the other was permanently
// rejected. A writer that took the wrong one would dispatch on a permit its own
// reservation never earned.
func TestAPermitIsMatchedByReceiptAndNotByCoid(t *testing.T) {
	one := num.QtyFromFloat(1)
	coid, err := rest.Coid(dispatchRunID, dispatchMarketIdx, quote.SideYes, 1)
	if err != nil {
		t.Fatalf("coid: %v", err)
	}

	ex := &fakeExchange{}
	r, reserves := newDispatchRig(t, ex)

	// A reservation for the SAME coid, committed by somebody else. Its result
	// carries a valid permit and a different receipt.
	twin, err := rest.NewCreateOrder(dispatchTicker, quote.SideYes, 42, one, one, coid)
	if err != nil {
		t.Fatalf("build twin: %v", err)
	}
	if _, err := r.store.ReserveOrder(r.run, twin, quote.RoleAdding,
		r.ex.NowMs()); err != nil {
		t.Fatalf("seed reservation: %v", err)
	}
	seed := <-reserves
	if _, ok := seed.Permit(); !ok {
		t.Fatalf("the seeded reservation did not commit: %v", seed.Err)
	}

	// Now a second submission of the same coid with different content. It is an
	// H-ORD-1 collision and is permanently rejected, so this write must fail --
	// even though a permit for that coid has already gone past on the channel.
	clash := placement(t, r, quote.SideYes, quote.RoleAdding, 43, 1)
	if clash.Order.ClientOrderID() != coid {
		t.Fatalf("the two orders do not share a coid; the test proves nothing")
	}
	res := r.executeWrite(context.Background(), clash, reserves)

	if res.Err == nil {
		t.Fatalf("a colliding reservation was permitted to dispatch on another " +
			"submission's permit")
	}
	if got := ex.createdCoids(); len(got) != 0 {
		t.Fatalf("an order was sent on a permit its reservation never earned: %v",
			got)
	}
}

// ---------------------------------------------------------------------------
// The binding
// ---------------------------------------------------------------------------

// TestBindingIsSubmittedOnTheAckAndNotAtTheNextPoll is the single most
// important assertion in this file.
//
// Two things are checked, and neither is about `BindOrder` itself:
//
//  1. at the instant the create request is sent, the reservation is ALREADY in
//     the durable unresolved set -- `Ownership.reserveCommitted` runs only
//     after the transaction commits, so this is the commit-before-dispatch
//     ordering observed from the wire;
//
//  2. the binding becomes durable with NO second dispatch, NO order walk and
//     NO poll of any kind in this test. Nothing else in this process could have
//     submitted it. A writer that batched the binding, or left it to
//     `wsx.bindListedOrders`, would leave the order id unbound here forever --
//     and in production it would leave every fill on our own order deferred
//     until the deferral escalated to SEV2 `FILL_UNCLASSIFIABLE`.
func TestBindingIsSubmittedOnTheAckAndNotAtTheNextPoll(t *testing.T) {
	ex := &fakeExchange{}
	r, reserves := newDispatchRig(t, ex)
	req := placement(t, r, quote.SideYes, quote.RoleAdding, 42, 1)
	coid := req.Order.ClientOrderID()

	var committedFirst bool
	ex.onCreate = func(sent string) {
		if sent != coid {
			t.Errorf("create carried coid %q, want %q", sent, coid)
		}
		_, committedFirst = r.store.Ownership().Unresolved()[coid]
	}

	res := r.executeWrite(context.Background(), req, reserves)
	if res.Err != nil {
		t.Fatalf("place: %v", res.Err)
	}
	if !committedFirst {
		t.Fatalf("the order for %s was sent while its reservation was NOT in "+
			"the durable unresolved set; the coid enters that set only when the "+
			"transaction commits, so the order was dispatched before it was "+
			"recorded (H-ORD-6)", coid)
	}
	if !res.Bound {
		t.Fatalf("the create acknowledged order %s and no binding was "+
			"submitted", res.Create.OrderID)
	}

	orderID := res.Create.OrderID
	waitFor(t, "the binding to commit", func() bool {
		bound, ok := r.store.Ownership().Bound(orderID)
		return ok && bound == coid
	})
	if n := r.store.Ownership().UnresolvedCount(); n != 0 {
		t.Fatalf("the binding committed but %d reservation(s) are still "+
			"outstanding; a committed binding resolves its own reservation in "+
			"the same instant, and while the set is non-empty every "+
			"unrecognised fill on the account defers", n)
	}
}

// TestDefiniteRejectionAbandonsTheReservation drains the other terminal.
//
// A definite 4xx is the exchange answering, and its answer is no: the coid was
// never taken. Leaving the reservation outstanding would make one rejected
// placement defer every unrecognised fill for the life of the deployment, which
// turns H-ORD-9's repair from "no false foreign" into "no foreign ever".
func TestDefiniteRejectionAbandonsTheReservation(t *testing.T) {
	ex := &fakeExchange{createStatus: 400}
	r, reserves := newDispatchRig(t, ex)

	res := r.executeWrite(context.Background(),
		placement(t, r, quote.SideYes, quote.RoleAdding, 42, 1), reserves)
	if res.Create.Outcome != rest.CreateRejected {
		t.Fatalf("outcome = %s, want REJECTED", res.Create.Outcome)
	}
	if res.Bound {
		t.Fatalf("a rejected create bound an order id")
	}
	waitFor(t, "the abandonment to commit", func() bool {
		return r.store.Ownership().UnresolvedCount() == 0
	})
}

// TestAnUnknownCreateLeavesTheReservationOutstanding is the converse, and it is
// the direction that costs money to get wrong.
//
// "We could not find it" has never been evidence of anything (H-ORD-2a). An
// UNKNOWN create may have landed, so abandoning its reservation would let a
// later fill on that very order read as FOREIGN.
func TestAnUnknownCreateLeavesTheReservationOutstanding(t *testing.T) {
	// A 2xx with a body `parseAck` cannot accept: the create stays UNKNOWN at
	// full size and is same-coid recoverable.
	ex := &fakeExchange{createStatus: 200, createBody: []byte(`{}`)}
	r, reserves := newDispatchRig(t, ex)

	res := r.executeWrite(context.Background(),
		placement(t, r, quote.SideYes, quote.RoleAdding, 42, 1), reserves)
	if res.Create.Outcome != rest.CreateUnknown {
		t.Fatalf("outcome = %s, want UNKNOWN", res.Create.Outcome)
	}
	if !res.Create.ReconcileNow() {
		t.Fatalf("an UNKNOWN create did not ask for reconciliation (§7.2)")
	}
	if got := r.store.Ownership().UnresolvedCount(); got != 1 {
		t.Fatalf("unresolved reservations = %d, want 1: an UNKNOWN create is "+
			"not evidence that the coid was never taken, and abandoning it "+
			"would let a fill on that order classify as foreign", got)
	}
}

// ---------------------------------------------------------------------------
// H-STORE-3 and I1
// ---------------------------------------------------------------------------

// TestUnhealthyStoreRefusesAddingAndNotReducing is H-STORE-3 joined to I1.
//
// The store is broken in the way that revokes adding authority without
// refusing submissions -- a permanently rejected record, which latches a sticky
// fault -- and then two placements are dispatched through the real pipeline.
// The adding one must not reach the exchange, because its ownership record
// could not be written and an unrecorded order produces a fill H-ORD-9 must
// call foreign. The reducing one must, because every stop path in this system
// stops adding risk and none of them stops reducing it (I1): a disk problem
// that stranded inventory would be the failure mode this harness exists to not
// have.
func TestUnhealthyStoreRefusesAddingAndNotReducing(t *testing.T) {
	ex := &fakeExchange{}
	r, reserves := newDispatchRig(t, ex)

	breakStore(t, r)

	adding := placement(t, r, quote.SideYes, quote.RoleAdding, 42, 10)
	addRes := r.executeWrite(context.Background(), adding, reserves)
	if addRes.Err == nil {
		t.Fatalf("an ADDING order was dispatched while storage was unhealthy; " +
			"H-STORE-3 revokes adding authority precisely because the ownership " +
			"record of an order placed now cannot be written")
	}
	if got := ex.createdCoids(); len(got) != 0 {
		t.Fatalf("an ADDING order reached the exchange while storage was "+
			"unhealthy: %v", got)
	}

	reducing := placement(t, r, quote.SideNo, quote.RoleReducing, 42, 11)
	redRes := r.executeWrite(context.Background(), reducing, reserves)
	if redRes.Err != nil {
		t.Fatalf("a REDUCING order was refused by an unhealthy store: %v.\n"+
			"I1: every stop path stops ADDING risk; none stops reducing it. A "+
			"store fault that blocked the exit would strand inventory",
			redRes.Err)
	}
	got := ex.createdCoids()
	if len(got) != 1 || got[0] != reducing.Order.ClientOrderID() {
		t.Fatalf("the exchange saw %v, want exactly the reducing order %s",
			got, reducing.Order.ClientOrderID())
	}
}

// breakStore latches a sticky permanent fault: adding revoked, submissions
// still accepted, later records still committed.
//
// The mechanism is an H-ORD-1 collision -- one coid reserved twice with
// different content -- because that is a PERMANENT error, and only a permanent
// error revokes adding without also refusing the reservations this test needs
// to keep committing. A transient failure would recover; a closed store would
// refuse `ReserveOrder` outright and the permit path would never be reached,
// which would test a different rule.
func breakStore(t *testing.T, r *rig) {
	t.Helper()
	coid, err := rest.Coid(r.runID, dispatchMarketIdx, quote.SideYes, 999)
	if err != nil {
		t.Fatalf("coid: %v", err)
	}
	one := num.QtyFromFloat(1)
	first, err := rest.NewCreateOrder(dispatchTicker, quote.SideYes, 40, one, one, coid)
	if err != nil {
		t.Fatalf("build order: %v", err)
	}
	second, err := rest.NewCreateOrder(dispatchTicker, quote.SideYes, 41, one, one, coid)
	if err != nil {
		t.Fatalf("build order: %v", err)
	}
	if _, err := r.store.ReserveOrder(r.run, first, quote.RoleAdding,
		r.ex.NowMs()); err != nil {
		t.Fatalf("first reservation: %v", err)
	}
	if _, err := r.store.ReserveOrder(r.run, second, quote.RoleAdding,
		r.ex.NowMs()); err != nil {
		t.Fatalf("colliding reservation: %v", err)
	}
	waitFor(t, "storage to become unhealthy", func() bool {
		return !r.store.Health().AllowsAdding()
	})
}

// ---------------------------------------------------------------------------
// H-ORD-4 / H-FAIL-3 — a cancel is not a fact until a read says so
// ---------------------------------------------------------------------------

// TestAbsenceIsClaimedOnlyFromACompleteRead is `Queue.ConfirmAbsent`'s licence.
//
// The DELETE is answered 2xx in both halves. What differs is what the VERIFYING
// read then reports, and that is the only thing allowed to decide: H-FAIL-3 is
// explicit that "off" means exchange-confirmed absent and never
// cancel-requested.
func TestAbsenceIsClaimedOnlyFromACompleteRead(t *testing.T) {
	target := rest.Order{OrderID: "EX-1", Ticker: dispatchTicker,
		Side: quote.SideYes, Remaining: num.QtyFromFloat(1)}
	req := writeRequest{
		IDs: []uint64{1}, Market: dispatchTicker, Side: quote.SideYes,
		Role: quote.RoleAdding, Op: quote.OpCancel,
		Orders: []rest.Order{target},
	}
	ourCoid, err := rest.Coid(dispatchRunID, dispatchMarketIdx, quote.SideYes, 7)
	if err != nil {
		t.Fatalf("coid: %v", err)
	}

	t.Run("clean", func(t *testing.T) {
		ex := &fakeExchange{}
		r, reserves := newDispatchRig(t, ex)
		res := r.executeWrite(context.Background(), req, reserves)
		if !res.Sweep.Clean {
			t.Fatalf("sweep was not clean against an empty book")
		}
		if !res.Absent {
			t.Fatalf("a complete read found nothing of ours resting and " +
				"absence was still not claimed; the replacement leg of a " +
				"cancel-confirm-place can never open")
		}
		if !res.Sent || ex.deleteCount() != 1 {
			t.Fatalf("expected exactly one DELETE, got %d (sent=%v)",
				ex.deleteCount(), res.Sent)
		}
	})

	t.Run("another of ours still rests on that side", func(t *testing.T) {
		// Not the requested order -- `CancelAndSweep` reports it as OtherOurs
		// and deliberately does not cancel it (A4 keeps the reducer alive). It
		// is still one of ours resting on this side, so absence is FALSE.
		ex := &fakeExchange{resting: []map[string]any{
			restingOrder("EX-9", ourCoid, "yes", 0.42),
		}}
		r, reserves := newDispatchRig(t, ex)
		res := r.executeWrite(context.Background(), req, reserves)
		if !res.Sweep.Clean {
			t.Fatalf("the REQUESTED order was gone, so the sweep is clean")
		}
		if res.Absent {
			t.Fatalf("absence was claimed while %d of our orders rested on "+
				"that side; `ConfirmAbsent` would open a replacement leg on "+
				"top of a live order", len(res.Sweep.OtherOurs))
		}
	})

	t.Run("the requested order still rests", func(t *testing.T) {
		ex := &fakeExchange{resting: []map[string]any{
			restingOrder("EX-1", ourCoid, "yes", 0.42),
		}}
		r, reserves := newDispatchRig(t, ex)
		res := r.executeWrite(context.Background(), req, reserves)
		if res.Sweep.Clean || res.Absent {
			t.Fatalf("the exchange still lists the cancelled order and the " +
				"sweep reported it gone; an unverified cancel is a LIVE order " +
				"(H-FAIL-3)")
		}
	})
}

// TestACancelTakesNoPermitAndReservesNothing is I1 stated as an absence.
//
// H-ORD-6's two-stage commit exists because an order we placed and did not
// record is a fill we cannot classify. A cancel creates no such obligation, so
// gating it on the store would be the disk stopping the one write that can only
// reduce exposure.
func TestACancelTakesNoPermitAndReservesNothing(t *testing.T) {
	ex := &fakeExchange{}
	r, reserves := newDispatchRig(t, ex)
	breakStore(t, r)

	before := r.store.Ownership().UnresolvedCount()
	res := r.executeWrite(context.Background(), writeRequest{
		IDs: []uint64{1}, Market: dispatchTicker, Side: quote.SideYes,
		Role: quote.RoleAdding, Op: quote.OpCancel,
		Orders: []rest.Order{{OrderID: "EX-1", Ticker: dispatchTicker,
			Side: quote.SideYes, Remaining: num.QtyFromFloat(1)}},
	}, reserves)
	if res.Err != nil {
		t.Fatalf("a cancel was refused by a broken store: %v", res.Err)
	}
	if ex.deleteCount() != 1 {
		t.Fatalf("DELETEs = %d, want 1", ex.deleteCount())
	}
	if got := r.store.Ownership().UnresolvedCount(); got != before {
		t.Fatalf("a cancel reserved a coid: unresolved went %d -> %d",
			before, got)
	}
}

// ---------------------------------------------------------------------------
// §6.6's token bucket
// ---------------------------------------------------------------------------

// TestRefillStopsAtTheCeilingAndTheReserveFillsFirst is the whole of the refill
// contract.
//
// `write_burst` IS the ceiling on a burst. A bucket that accrued past it would
// hand the exchange a burst §16 never authorised, and a bucket that banked an
// idle period would deliver that burst the instant the first token was taken.
func TestRefillStopsAtTheCeilingAndTheReserveFillsFirst(t *testing.T) {
	p := cfg.Default()
	reserved := quote.ReserveWrites(p.WriteBurst)
	general := p.WriteBurst - reserved

	// Drained completely, then handed an hour.
	c := quote.Capacity{Workers: 2, ReservedWorkers: 1}
	c, carry := refillWrites(c, p, time.Hour, 0)

	if c.Tokens != general || c.ReservedTokens != reserved {
		t.Fatalf("an hour of refill produced tokens=%d reserved=%d, want %d/%d "+
			"-- write_burst is the ceiling and nothing accrues past it",
			c.Tokens, c.ReservedTokens, general, reserved)
	}
	if carry != 0 {
		t.Fatalf("carry = %v after both pools filled; a banked idle period is "+
			"a burst waiting to be spent", carry)
	}

	// A second hour changes nothing.
	full := c
	c, _ = refillWrites(c, p, time.Hour, 0)
	if c != full {
		t.Fatalf("a full bucket refilled further: %+v -> %+v", full, c)
	}

	// The reserve is filled FIRST. H-QUE-3 holds a share of the budget for P1
	// at all times because a reducing-side write is the exit; a refill that
	// topped the general pool up first would let a requote storm spend every
	// accrued token before the exit's share came back.
	drained := quote.Capacity{Workers: 2, ReservedWorkers: 1}
	one := time.Duration(float64(time.Second) / p.WriteRate)
	drained, _ = refillWrites(drained, p, one, 0)
	if drained.ReservedTokens != 1 || drained.Tokens != 0 {
		t.Fatalf("the first accrued write went to tokens=%d reserved=%d; "+
			"H-QUE-3's reserve is refilled first",
			drained.Tokens, drained.ReservedTokens)
	}

	// A fractional accrual is carried rather than truncated away.
	c = quote.Capacity{Workers: 2, ReservedWorkers: 1}
	carry = 0
	half := time.Duration(float64(time.Second) / p.WriteRate / 2)
	c, carry = refillWrites(c, p, half, carry)
	if c.ReservedTokens != 0 || c.Tokens != 0 {
		t.Fatalf("half a token period granted a whole write")
	}
	c, carry = refillWrites(c, p, half, carry)
	if c.ReservedTokens != 1 {
		t.Fatalf("two half-periods did not add up to one write (carry=%v); a "+
			"bucket that truncates loses part of the §16 budget on every pass",
			carry)
	}

	// Backwards time accrues nothing. F21's clock step is why elapsed is
	// monotonic; a negative refill would hand the rate limit to the NTP daemon.
	before := c
	c, _ = refillWrites(c, p, -time.Hour, 0)
	if c.Tokens != before.Tokens || c.ReservedTokens != before.ReservedTokens {
		t.Fatalf("a backwards interval changed the bucket: %+v -> %+v",
			before, c)
	}
}

// TestReleaseReturnsTheWorkerAndOnlyRefundsUnsentWrites is the other half.
//
// The worker comes back because with one REST writer occupancy is real while
// the request is running and honestly zero afterwards. The TOKEN comes back
// only when nothing was sent: charging for a write that never happened would
// let a store outage spend the reducer's share of the budget on nothing.
func TestReleaseReturnsTheWorkerAndOnlyRefundsUnsentWrites(t *testing.T) {
	p := cfg.Default()
	full := quote.NewCapacity(dispatchWorkers, p.WriteBurst)

	for _, tc := range []struct {
		name  string
		grant quote.Grant
		sent  bool
	}{
		{"general, sent", quote.Grant{}, true},
		{"general, unsent", quote.Grant{}, false},
		{"reserved, sent", quote.Grant{ReservedWorker: true, ReservedToken: true}, true},
		{"reserved, unsent", quote.Grant{ReservedWorker: true, ReservedToken: true}, false},
		{"P0 bypass, unsent", quote.Grant{BypassBucket: true}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			charged := full.Take(tc.grant)
			back := releaseWrite(charged, p, tc.grant, tc.sent)

			if back.BusyGeneral != 0 || back.BusyReserved != 0 {
				t.Fatalf("workers still busy after the write returned: "+
					"general=%d reserved=%d", back.BusyGeneral, back.BusyReserved)
			}
			if err := back.Validate(); err != nil {
				t.Fatalf("release produced a capacity H-QUE-3 refuses: %v", err)
			}
			switch {
			case tc.sent:
				if back.Tokens+back.ReservedTokens !=
					charged.Tokens+charged.ReservedTokens {
					t.Fatalf("a sent write got its token back: %d -> %d",
						charged.Tokens+charged.ReservedTokens,
						back.Tokens+back.ReservedTokens)
				}
			default:
				if back.Tokens != full.Tokens ||
					back.ReservedTokens != full.ReservedTokens {
					t.Fatalf("an unsent write did not restore the bucket: "+
						"%d/%d, want %d/%d", back.Tokens, back.ReservedTokens,
						full.Tokens, full.ReservedTokens)
				}
			}
		})
	}
}

// TestReleaseNeverExceedsTheCeiling guards the refund path against the one
// mistake that would matter: handing back a token that was never spent.
func TestReleaseNeverExceedsTheCeiling(t *testing.T) {
	p := cfg.Default()
	full := quote.NewCapacity(dispatchWorkers, p.WriteBurst)
	// A grant that was never charged, released as unsent. The refund must not
	// push either pool past write_burst's share of it.
	back := releaseWrite(full, p, quote.Grant{}, false)
	if back.Tokens != full.Tokens || back.ReservedTokens != full.ReservedTokens {
		t.Fatalf("a refund pushed a full bucket past its ceiling: %d/%d -> %d/%d",
			full.Tokens, full.ReservedTokens, back.Tokens, back.ReservedTokens)
	}
}

// ---------------------------------------------------------------------------
// The loop
// ---------------------------------------------------------------------------

// TestDispatchLoopExecutesOneWriteAtATime is the D3 shape.
//
// It is not a concurrency test -- there is nothing to race, which is the point.
// It asserts that the goroutine consumes requests in order, answers each on the
// result channel, and returns when the owner closes the input rather than
// leaking.
func TestDispatchLoopExecutesOneWriteAtATime(t *testing.T) {
	ex := &fakeExchange{}
	r, reserves := newDispatchRig(t, ex)

	in := make(chan writeRequest)
	out := make(chan writeResult)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		r.dispatchLoop(ctx, in, reserves, out)
	}()

	var want []string
	for seq := uint64(1); seq <= 3; seq++ {
		req := placement(t, r, quote.SideYes, quote.RoleAdding, 40+int(seq), seq)
		want = append(want, req.Order.ClientOrderID())
		in <- req
		res := <-out
		if res.Err != nil {
			t.Fatalf("write %d: %v", seq, res.Err)
		}
		if len(res.Req.IDs) != 1 || res.Req.IDs[0] != seq {
			t.Fatalf("the result did not echo the request's intent ids: %v",
				res.Req.IDs)
		}
		if !res.Bound {
			t.Fatalf("write %d acknowledged and did not bind", seq)
		}
	}

	close(in)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatalf("the writer did not return when the owner closed its input")
	}

	got := ex.createdCoids()
	if len(got) != len(want) {
		t.Fatalf("the exchange saw %d creates, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("create %d carried %q, want %q; the writer reordered its "+
				"input", i, got[i], want[i])
		}
	}
}
