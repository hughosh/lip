package main

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"lip/harness/num"
	"lip/harness/quote"
	"lip/harness/rest"
	"lip/harness/wsx"
)

func f5OwnerCheck(t *testing.T) (*seamHarness, *owner, wsx.CrossCheckRequest) {
	t.Helper()
	h := newSeamHarness(t, seamOptions{})
	o := h.ownerFor()
	h.connectGate()
	frame, err := h.ws.frame("orderbook_snapshot", seamBook())
	if err != nil {
		t.Fatal(err)
	}
	o.applyEvent(wsx.Event{Kind: wsx.EventFrame, Frame: frame, At: h.clk.Now()}, make(chan wsx.ReconcileToken, 1))
	if !h.rig.gate.BookCurrent(seamTicker) {
		t.Fatal("snapshot was not accepted")
	}
	h.clk.Advance(h.cfg.Params.Quiet + time.Second)
	checks := h.rig.gate.Tick(h.clk.Now()).CrossCheck
	if len(checks) != 1 || checks[0].Refresh {
		t.Fatalf("F5 checks = %+v", checks)
	}
	return h, o, checks[0]
}

func f5Book(yesSize string) rest.OrderbookResult {
	return rest.OrderbookResult{Outcome: rest.OrderbookRead, Ticker: seamTicker,
		Yes: []rest.BookLevel{{Cents: 40, Size: func() float64 {
			if yesSize == "19.00" {
				return 19
			}
			return 20
		}(), Wire: [2]string{"0.4000", yesSize}}},
		No: []rest.BookLevel{{Cents: 55, Size: 20, Wire: [2]string{"0.5500", "20.00"}}}}
}

func TestOwnerF5CrossCheckAgreeAndSizeDisagree(t *testing.T) {
	t.Run("agree resets silence without reducing", func(t *testing.T) {
		h, o, req := f5OwnerCheck(t)
		o.applyCrossCheck(crossCheckResult{request: req, started: h.clk.Now().Mono, book: f5Book("20.00")})
		if h.rig.gate.F5Quarantined(seamTicker) || h.rig.gate.Reducing(seamTicker) {
			t.Fatal("agreement reduced or quarantined the market")
		}
		if h.rig.gate.Tick(h.clk.Now()).CrossCheck != nil {
			t.Fatal("agreement did not reset the quiet clock")
		}
	})
	t.Run("size-only disagreement retains REST and requests snapshot", func(t *testing.T) {
		h, o, req := f5OwnerCheck(t)
		commands := make(chan wsx.Command, 1)
		o.wsCommands = commands
		o.applyCrossCheck(crossCheckResult{request: req, started: h.clk.Now().Mono, book: f5Book("19.00")})
		if !h.rig.gate.F5Quarantined(seamTicker) || !h.rig.gate.Reducing(seamTicker) || !o.reduceNoted {
			t.Fatal("size-only disagreement did not reach owner REDUCING relay")
		}
		if len(commands) != 1 || (<-commands).Kind != wsx.CmdResnapshot {
			t.Fatal("no immediate in-session resnapshot")
		}
		ws := h.rig.book.Book(seamTicker)
		size, _ := ws.Yes().Get(40)
		if size != 19 {
			t.Fatalf("diagnostic replacement size=%v, want 19", size)
		}
		retained, ok := o.restReducer.Snapshot(seamTicker, ws.Target, h.clk.Now().Mono, h.cfg.Params.Quiet)
		if !ok {
			t.Fatal("independent REST reducer book was not retained")
		}
		size, _ = retained.Yes().Get(40)
		if size != 19 {
			t.Fatalf("retained REST size=%v, want 19", size)
		}
		// A duplicate response cannot install new depth or renew REST authority.
		o.applyCrossCheck(crossCheckResult{request: req, started: h.clk.Now().Mono, book: f5Book("20.00")})
		retained, ok = o.restReducer.Snapshot(seamTicker, ws.Target, h.clk.Now().Mono, h.cfg.Params.Quiet)
		if !ok {
			t.Fatal("duplicate revoked retained source")
		}
		size, _ = retained.Yes().Get(40)
		if size != 19 {
			t.Fatalf("stale result changed retained size to %v", size)
		}
		if book, allowed := o.pricingBook(quote.RoleAdding); allowed || book != nil {
			t.Fatal("F5 allowed adding from a quarantined book")
		}
	})
}

func TestOwnerResnapshotUsesOnlyCurrentBookSubscription(t *testing.T) {
	h := newSeamHarness(t, seamOptions{})
	o := h.ownerFor()
	commands := make(chan wsx.Command, 4)
	o.wsCommands = commands
	tokens := make(chan wsx.ReconcileToken, 4)
	frame := func(raw string) {
		t.Helper()
		o.applyEvent(wsx.Event{Kind: wsx.EventFrame, Frame: []byte(raw), At: h.clk.Now()}, tokens)
	}
	o.applyEvent(wsx.Event{Kind: wsx.EventConnected, At: h.clk.Now()}, tokens)
	if len(commands) != 0 {
		t.Fatal("startup sent get_snapshot before a subscription SID existed")
	}
	o.requestF5Resnapshot()
	if len(commands) != 0 || !o.f5ResnapshotPending {
		t.Fatal("unknown book SID did not defer the recovery request")
	}
	frame(`{"type":"subscribed","id":2,"msg":{"channel":"trade","sid":9}}`)
	frame(`{"type":"subscribed","id":1,"msg":{"channel":"trade","sid":9}}`)
	if len(commands) != 0 {
		t.Fatal("trade acknowledgement became a book snapshot target")
	}
	frame(`{"type":"subscribed","id":1,"msg":{"channel":"orderbook_delta","sid":7}}`)
	if len(commands) != 1 {
		t.Fatal("book acknowledgement did not release deferred recovery")
	}
	if got := (<-commands).Sids; len(got) != 1 || got[0] != 7 {
		t.Fatalf("first recovery sids=%v, want [7]", got)
	}
	// core now knows both IDs. Its Sids() is deliberately not a command target.
	frame(`{"type":"trade","sid":9,"seq":1,"msg":{"market_ticker":"OTHER"}}`)
	o.requestF5Resnapshot()
	if got := (<-commands).Sids; len(got) != 1 || got[0] != 7 {
		t.Fatalf("mixed trade/book recovery sids=%v, want [7]", got)
	}
	bookFrame := func(typ string, seq int64, msg map[string]any) {
		t.Helper()
		b, err := json.Marshal(map[string]any{
			"type": typ, "sid": 7, "seq": seq, "msg": msg,
		})
		if err != nil {
			t.Fatal(err)
		}
		o.applyEvent(wsx.Event{Kind: wsx.EventFrame, Frame: b, At: h.clk.Now()}, tokens)
	}
	bookFrame("orderbook_snapshot", 1, seamBook())
	bookFrame("orderbook_delta", 3, map[string]any{
		"market_ticker": seamTicker, "side": "yes", "price_dollars": "0.4000", "delta_fp": "1.00",
	})
	if len(commands) != 1 {
		t.Fatalf("gap queued %d snapshot commands, want 1", len(commands))
	}
	if got := (<-commands).Sids; len(got) != 1 || got[0] != 7 {
		t.Fatalf("gap recovery sids=%v, want [7]", got)
	}
	o.applyEvent(wsx.Event{Kind: wsx.EventDisconnected, At: h.clk.Now(), Clean: true}, tokens)
	o.applyEvent(wsx.Event{Kind: wsx.EventConnected, At: h.clk.Now()}, tokens)
	o.requestF5Resnapshot()
	if len(commands) != 0 {
		t.Fatal("reconnect reused the old connection's book SID")
	}
	frame(`{"type":"subscribed","id":1,"msg":{"channel":"orderbook_delta","sid":11}}`)
	if got := (<-commands).Sids; len(got) != 1 || got[0] != 11 {
		t.Fatalf("reconnected recovery sids=%v, want [11]", got)
	}
}

func TestOwnerCanLearnBookSIDFromAcceptedInitialSnapshot(t *testing.T) {
	h := newSeamHarness(t, seamOptions{})
	o := h.ownerFor()
	commands := make(chan wsx.Command, 1)
	o.wsCommands = commands
	tokens := make(chan wsx.ReconcileToken, 1)
	o.applyEvent(wsx.Event{Kind: wsx.EventConnected, At: h.clk.Now()}, tokens)
	o.requestF5Resnapshot()
	frame, err := h.ws.frame("orderbook_snapshot", seamBook())
	if err != nil {
		t.Fatal(err)
	}
	o.applyEvent(wsx.Event{Kind: wsx.EventFrame, Frame: frame, At: h.clk.Now()}, tokens)
	if got := (<-commands).Sids; len(got) != 1 || got[0] != 1 {
		t.Fatalf("snapshot-learned recovery sids=%v, want [1]", got)
	}
}

// f5Doer adds only the endpoint under test to the existing seam exchange.
type f5Doer struct {
	base    rest.Doer
	mu      sync.Mutex
	yesSize string
	reads   int
}

func (d *f5Doer) Do(ctx context.Context, req rest.Request) (rest.Response, error) {
	if req.Method == "GET" && req.Path == "/markets/"+seamTicker+"/orderbook" {
		d.mu.Lock()
		d.reads++
		size := d.yesSize
		d.mu.Unlock()
		if size == "" {
			size = "20.00"
		}
		return seamJSON(map[string]any{"orderbook_fp": map[string]any{
			"yes_dollars": [][]string{{"0.4000", size}},
			"no_dollars":  [][]string{{"0.5500", "20.00"}},
		}})
	}
	return d.base.Do(ctx, req)
}
func (d *f5Doer) count() int { d.mu.Lock(); defer d.mu.Unlock(); return d.reads }

func advanceF5QuietWithLiveSocket(t *testing.T, h *seamHarness) {
	t.Helper()
	// Non-book frames reset the socket read deadline without resetting F5's
	// per-market book silence clock.
	for i := 0; i < 13; i++ {
		if i == 5 || i == 10 {
			frame, err := h.ws.frame("subscription_ack", map[string]any{})
			if err != nil {
				t.Fatal(err)
			}
			h.ws.frames <- frame
		}
		h.clk.Advance(h.cfg.Params.PositionPoll)
		h.awaitTicks(2)
	}
}

func TestServeF5QuietAgreeKeepsQuoting(t *testing.T) {
	h := newSeamHarness(t, seamOptions{})
	doer := &f5Doer{base: h.rig.api.Doer}
	h.rig.api.Doer = doer
	h.rig.cfg.Params.SchedulePoll = 100 * time.Millisecond
	h.start()
	h.awaitActionable()
	advanceF5QuietWithLiveSocket(t, h)
	h.await("a real F5 REST orderbook read", func() bool { return doer.count() > 0 })
	h.await("agreement to restore actionability", func() bool {
		m, ok := h.market()
		return ok && m.BookActionable
	})
	m, _ := h.market()
	if m.State != quote.Quoting {
		t.Fatalf("an agreeing quiet market is %s, want QUOTING", m.State)
	}
}

// This composition keeps the owner on one goroutine, but uses the real
// portfolio poller, write dispatcher, reserve/commit permit and SQLite store.
// Existing exchange orders make the adding sweep observable without relying
// on startup timing to place both sides first.
func TestOwnerF5ReducerSweepResizeRefreshAndRecovery(t *testing.T) {
	g := newGateFailOwner(t, num.QtyFromFloat(2))
	h, o := g.h, g.o
	for i, side := range []quote.Side{quote.SideYes, quote.SideNo} {
		id, price, qty := "F5-ADDING", 40, num.QtyFromFloat(1)
		wireSide, wirePrice := string(rest.Bid), "0.4000"
		if side == quote.SideNo {
			id, price, qty = "F5-REDUCER", 55, num.QtyFromFloat(2)
			wireSide, wirePrice = string(rest.Ask), "0.4500"
		}
		h.seamOwnedOrderOn(id, uint64(i+1), side, price, qty)
		coid, _ := rest.Coid("SEAMPRIOR", 0, side, uint64(i+1))
		h.ex.mu.Lock()
		h.ex.resting = append(h.ex.resting, seamRestingOrder(seamCreate{
			Ticker: seamTicker, WireSide: wireSide, Price: wirePrice,
			Count: qty.Wire(), Coid: coid, OrderID: id,
		}))
		h.ex.mu.Unlock()
	}
	h.ex.setPosition(seamTicker, "2.00")
	h.clk.Advance(h.cfg.Params.Quiet + time.Second)
	reducerConfirmationPoll(t, g)
	checks := h.rig.gate.Tick(h.clk.Now()).CrossCheck
	if len(checks) != 1 {
		t.Fatalf("quiet checks=%+v", checks)
	}
	commands := make(chan wsx.Command, 1)
	o.wsCommands = commands
	o.applyCrossCheck(crossCheckResult{request: checks[0], started: h.clk.Now().Mono, book: f5Book("19.00")})
	if len(commands) != 1 || (<-commands).Kind != wsx.CmdResnapshot {
		t.Fatal("mismatch did not request immediate snapshot")
	}

	ctx, cancel := context.WithCancel(h.ctx)
	resultsDone := make(chan struct{})
	go func() { defer close(resultsDone); _ = o.sd.runResults(ctx) }()
	writes, results := make(chan writeRequest, 4), make(chan writeResult, 4)
	dispatchDone := make(chan struct{})
	go func() { defer close(dispatchDone); h.rig.dispatchLoop(ctx, writes, o.sd.Permits(), results) }()
	defer func() { cancel(); <-dispatchDone; <-resultsDone }()
	step := func() writeResult {
		t.Helper()
		o.pump(h.clk.Now().Mono, writes)
		select {
		case res := <-results:
			if res.Err != nil {
				t.Fatalf("dispatcher: %v", res.Err)
			}
			o.applyWriteResult(res)
			return res
		case <-time.After(3 * time.Second):
			t.Fatal("expected write was not dispatched")
		}
		return writeResult{}
	}
	o.evaluate(h.clk.Now().Mono)
	if o.market != quote.Reducing {
		t.Fatalf("mismatch market=%v", o.market)
	}
	res := step()
	if res.Req.Op != quote.OpCancel || res.Req.Side != quote.SideYes || !res.Absent {
		t.Fatalf("adding was not swept: %+v", res)
	}
	h.ex.mu.Lock()
	keptReducer := len(h.ex.resting) == 1 && h.ex.resting[0]["order_id"] == "F5-REDUCER"
	h.ex.mu.Unlock()
	if !keptReducer {
		t.Fatal("adding sweep did not preserve exactly the resting reducer")
	}

	// A subsequent positions walk shrinks |q|. The REST refresh has a new
	// NO touch, deliberately different from the quarantined diagnostic book.
	h.clk.Advance(time.Millisecond)
	h.ex.setPosition(seamTicker, "1.00")
	reducerConfirmationPoll(t, g)
	o.evaluate(h.clk.Now().Mono)
	res = step()
	if res.Req.Op != quote.OpCancel || res.Req.Side != quote.SideNo || !res.Absent {
		t.Fatalf("oversized reducer not swept: %+v", res)
	}
	h.clk.Advance(time.Millisecond)
	reducerConfirmationPoll(t, g)
	o.evaluate(h.clk.Now().Mono) // queue replacement while the REST source is fresh
	queuedReplacement := false
	for _, in := range h.rig.queue.Pending() {
		queuedReplacement = queuedReplacement || (in.Side == quote.SideNo && in.Role == quote.RoleReducing && in.Op() == quote.OpPlace)
	}
	if !queuedReplacement {
		t.Fatal("fresh REST did not queue a reducer replacement before expiry")
	}
	if _, ok := o.pricingBook(quote.RoleReducing); !ok {
		t.Fatal("fresh REST reducer was not actionable at decision")
	}

	// It becomes stale between decision and dispatch. Fresh portfolio truth
	// cannot make a stale REST price usable.
	h.clk.Advance(h.cfg.Params.Quiet + time.Second)
	o.offerToken(g.tokens, o.reconcileToken)
	reducerConfirmationPoll(t, g)
	o.pump(h.clk.Now().Mono, writes)
	if len(o.inflight) != 0 || h.ex.createCount() != 0 {
		t.Fatal("stale REST dispatched a queued replacement")
	}
	if _, ok := o.pricingBook(quote.RoleReducing); ok {
		t.Fatal("stale REST source remained actionable")
	}
	checks = h.rig.gate.Tick(h.clk.Now()).CrossCheck
	if len(checks) != 1 || !checks[0].Refresh {
		t.Fatalf("refresh checks=%+v", checks)
	}
	refreshed := f5Book("19.00")
	refreshed.No = []rest.BookLevel{{Cents: 53, Size: 20, Wire: [2]string{"0.5300", "20.00"}}}
	o.applyCrossCheck(crossCheckResult{request: checks[0], started: h.clk.Now().Mono, book: refreshed})
	if _, ok := o.pricingBook(quote.RoleAdding); ok {
		t.Fatal("refresh enabled adding")
	}
	o.evaluate(h.clk.Now().Mono)
	res = step()
	if res.Req.Op != quote.OpPlace || res.Req.Side != quote.SideNo || res.Req.Order.Count() != num.QtyFromFloat(1) || !res.Bound {
		t.Fatalf("replacement was not a committed one-contract reducer: %+v", res)
	}
	creates := h.ex.allCreates()
	if len(creates) != 1 || creates[0].Price != "0.4700" || !creates[0].PostOnly {
		t.Fatalf("replacement did not use refreshed REST NO=53: %+v", creates)
	}

	// All truth above predates this snapshot but follows the mismatch, so it
	// satisfies the reconciliation half. The snapshot is independently required.
	if !h.rig.gate.F5Quarantined(seamTicker) {
		t.Fatal("portfolio truth alone lifted quarantine")
	}
	g.snapshot(gateFailQualifyingBook())
	if h.rig.gate.F5Quarantined(seamTicker) {
		t.Fatal("fresh snapshot plus new portfolio truth did not recover")
	}
	o.expireRESTIfRecovered()
	if _, ok := o.restReducer.Snapshot(seamTicker, seamTarget, h.clk.Now().Mono, h.cfg.Params.Quiet); ok {
		t.Fatal("recovery retained REST authority")
	}
}

func TestOwnerF5SnapshotRequiresEveryNewPortfolioWalk(t *testing.T) {
	for _, missing := range []string{"fills", "orders", "positions"} {
		t.Run(missing, func(t *testing.T) {
			g := newGateFailOwner(t, num.QtyFromFloat(1))
			h, o := g.h, g.o
			h.ex.setPosition(seamTicker, "1.00")
			h.clk.Advance(h.cfg.Params.Quiet + time.Second)
			reducerConfirmationPoll(t, g)
			checks := h.rig.gate.Tick(h.clk.Now()).CrossCheck
			if len(checks) != 1 {
				t.Fatalf("quiet checks=%+v", checks)
			}
			o.applyCrossCheck(crossCheckResult{request: checks[0], started: h.clk.Now().Mono, book: f5Book("19.00")})
			g.snapshot(gateFailQualifyingBook())
			if !h.rig.gate.F5Quarantined(seamTicker) {
				t.Fatal("snapshot plus pre-mismatch truth lifted F5")
			}
			base := h.rig.api.Doer
			h.rig.api.Doer = dispatchDoerFunc(func(ctx context.Context, req rest.Request) (rest.Response, error) {
				if req.Method == "GET" && req.Path == "/portfolio/"+missing {
					return rest.Response{Status: 200, Body: []byte(`{}`)}, nil // incomplete walk, never truth
				}
				return base.Do(ctx, req)
			})
			h.clk.Advance(time.Millisecond)
			reducerConfirmationPoll(t, g) // mismatch offered the token
			if !h.rig.gate.F5Quarantined(seamTicker) || h.rig.gate.Actionable(seamTicker, h.clk.Now()) {
				t.Fatalf("snapshot and two new endpoints bypassed missing %s", missing)
			}
			h.rig.api.Doer = base
			h.clk.Advance(time.Millisecond)
			g.tokens <- o.reconcileToken
			reducerConfirmationPoll(t, g)
			if h.rig.gate.F5Quarantined(seamTicker) || !h.rig.gate.Actionable(seamTicker, h.clk.Now()) {
				t.Fatal("snapshot and all three new endpoint walks did not recover")
			}
		})
	}
}
