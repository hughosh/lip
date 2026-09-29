package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"lip/harness/num"
	"lip/harness/quote"
	"lip/harness/rest"
	"lip/harness/risk"
	"lip/harness/wsx"
)

// The account pages and writes still go through seamExchange. This adapter
// supplies only the public market and shard reads the turnover path adds.
type turnoverCompositionExchange struct {
	base               *seamExchange
	mu                 sync.Mutex
	phase              int
	target             string
	reads              map[string]int
	terminals          []map[string]any
	terminalDeliveries int
}

func (x *turnoverCompositionExchange) setPhase(phase int) {
	x.mu.Lock()
	x.phase = phase
	x.mu.Unlock()
}

// setTarget changes every program's Target Size, which makes the owner rebuild
// core and refresh the universe with the same tickers.
func (x *turnoverCompositionExchange) setTarget(fp string) {
	x.mu.Lock()
	x.target = fp
	x.mu.Unlock()
}

func (x *turnoverCompositionExchange) readCount(path string) int {
	x.mu.Lock()
	defer x.mu.Unlock()
	return x.reads[path]
}

func (x *turnoverCompositionExchange) Do(ctx context.Context, req rest.Request) (rest.Response, error) {
	x.mu.Lock()
	if x.reads == nil {
		x.reads = map[string]int{}
	}
	x.reads[req.Path]++
	phase := x.phase
	target := x.target
	if target == "" {
		target = "10.00"
	}
	x.mu.Unlock()
	if req.Method == "GET" {
		switch {
		case req.Path == "/portfolio/subaccounts/balances":
			if len(req.Query) != 0 {
				return rest.Response{}, fmt.Errorf("scoped subaccount enumeration: %v", req.Query)
			}
			return seamJSON(map[string]any{"subaccount_balances": []any{
				map[string]any{"subaccount_number": 0, "exchange_index": 2},
			}})
		case req.Path == rest.EpOrders.Path && req.Query.Get("status") == "":
			// A complete resting page is not a terminal receipt. The separate
			// all-status production poll receives positively executed identities.
			x.base.mu.Lock()
			x.base.ordersWalks++
			orders := make([]any, 0, len(x.base.resting))
			for _, order := range x.base.resting {
				orders = append(orders, order)
			}
			x.base.mu.Unlock()
			x.mu.Lock()
			for _, order := range x.terminals {
				orders = append(orders, order)
				x.terminalDeliveries++
			}
			x.mu.Unlock()
			return seamJSON(map[string]any{"orders": orders, "cursor": ""})
		case req.Path == rest.EpPrograms.Path:
			if req.Query.Get("status") != "active" || req.Query.Get("type") != "liquidity" {
				return rest.Response{}, fmt.Errorf("unfiltered program walk: %v", req.Query)
			}
			now := time.Now().UTC()
			program := func(ticker string, pool int) map[string]any {
				return map[string]any{"id": "program-" + ticker, "market_ticker": ticker,
					"target_size_fp": target, "period_reward": pool,
					"discount_factor_bps": 5000,
					"start_date":          now.Add(-time.Minute).Format(time.RFC3339),
					"end_date":            now.Add(4 * time.Hour).Format(time.RFC3339)}
			}
			programs := []any{program(seamTicker, 2_000_000), program(turnoverOtherTicker, 1_000_000)}
			if phase != 0 {
				programs = []any{program(turnoverOtherTicker, 2_000_000)}
			}
			return seamJSON(map[string]any{"incentive_programs": programs, "next_cursor": ""})
		case strings.HasSuffix(req.Path, "/orderbook"):
			if req.Path != "/markets/"+seamTicker+"/orderbook" && req.Path != "/markets/"+turnoverOtherTicker+"/orderbook" {
				return rest.Response{}, fmt.Errorf("unexpected selection book %s", req.Path)
			}
			return seamJSON(map[string]any{"orderbook_fp": map[string]any{
				"yes_dollars": [][]string{{"0.4000", "20.00"}},
				"no_dollars":  [][]string{{"0.5500", "20.00"}},
			}})
		case req.Path == "/markets/"+seamTicker || req.Path == "/markets/"+turnoverOtherTicker:
			ticker := strings.TrimPrefix(req.Path, "/markets/")
			return seamJSON(map[string]any{"market": map[string]any{
				"ticker": ticker, "exchange_index": 2, "status": "active", "result": "",
				"close_time":      time.Now().Add(72 * time.Hour).UTC().Format(time.RFC3339),
				"can_close_early": false,
			}})
		case req.Path == "/portfolio/balance" && req.Query.Get("subaccount") == "0" && req.Query.Get("exchange_index") == "2":
			return x.base.balancePage()
		}
	}
	if req.Method == "DELETE" && strings.HasPrefix(req.Path, "/portfolio/events/orders/") {
		id := strings.TrimPrefix(req.Path, "/portfolio/events/orders/")
		x.base.mu.Lock()
		var terminal map[string]any
		for _, order := range x.base.resting {
			if order["order_id"] != id {
				continue
			}
			terminal = make(map[string]any, len(order))
			for key, value := range order {
				terminal[key] = value
			}
			terminal["status"] = rest.StatusCanceled
			terminal["remaining_count"] = "0.00"
		}
		x.base.mu.Unlock()
		response, err := x.base.Do(ctx, req)
		if err == nil && response.Status == 200 && terminal != nil {
			x.mu.Lock()
			x.terminals = append(x.terminals, terminal)
			x.mu.Unlock()
		}
		return response, err
	}
	return x.base.Do(ctx, req)
}

const turnoverOtherTicker = "KXSEAM-26AUG08-T2"

func turnoverMarket(s *risk.Snapshot, ticker string) (risk.MarketSnap, bool) {
	if s != nil {
		for _, m := range s.Markets {
			if m.Ticker == ticker {
				return m, true
			}
		}
	}
	return risk.MarketSnap{}, false
}

// startTurnoverCompositionFeed responds only to subscriptions actually issued by
// the production supervisor. A stopped incarnation retains no fixture goroutine.
func startTurnoverCompositionFeed(t *testing.T, h *seamHarness, x *turnoverCompositionExchange) (func(string, func() bool), func()) {
	socket := h.ws
	feedCtx, stopFeed := context.WithCancel(context.Background())
	done := make(chan struct{})
	stop := func() { stopFeed(); <-done }
	t.Cleanup(stop)
	feedErr := make(chan error, 1)
	t.Cleanup(func() {
		if !t.Failed() {
			return
		}
		s := h.snapshot()
		t.Logf("turnover diagnostic: latch=%s anomalies=%v", h.latchTrigger(), h.anomalyClasses())
		if s != nil {
			t.Logf("turnover diagnostic: seq=%d global=%v markets=%+v", s.Seq, s.Global, s.Markets)
		}
		sent := socket.subscriptions()
		t.Logf("turnover diagnostic: websocket writes=%d A schedule reads=%d B schedule reads=%d creates=%d", len(sent),
			x.readCount("/markets/"+seamTicker), x.readCount("/markets/"+turnoverOtherTicker), h.ex.createCount())
		if len(sent) > 5 {
			sent = sent[len(sent)-5:]
		}
		for _, b := range sent {
			t.Logf("turnover diagnostic: recent websocket write=%s", b)
		}
	})
	go func() {
		defer close(done)
		tick := time.NewTicker(5 * time.Millisecond)
		defer tick.Stop()
		seen := 0
		snapshots := 0
		for {
			select {
			case <-feedCtx.Done():
				return
			case <-tick.C:
				for sent := socket.subscriptions(); seen < len(sent); seen++ {
					var cmd struct {
						Cmd    string `json:"cmd"`
						Params struct {
							Channels      []string `json:"channels"`
							Action        string   `json:"action"`
							MarketTickers []string `json:"market_tickers"`
						} `json:"params"`
					}
					if err := json.Unmarshal(sent[seen], &cmd); err != nil {
						feedErr <- fmt.Errorf("decode websocket command: %w", err)
						return
					}
					bookSubscribe := cmd.Cmd == "subscribe" && len(cmd.Params.Channels) == 1 && cmd.Params.Channels[0] == "orderbook_delta"
					resnapshot := cmd.Cmd == "update_subscription" && cmd.Params.Action == "get_snapshot"
					if !bookSubscribe && !resnapshot {
						continue
					}
					for _, ticker := range cmd.Params.MarketTickers {
						if ticker != seamTicker && ticker != turnoverOtherTicker {
							feedErr <- fmt.Errorf("unexpected book snapshot ticker %q", ticker)
							return
						}
						snapshots++
						if snapshots > 64 {
							feedErr <- fmt.Errorf("websocket fixture received over 64 book snapshot requests")
							return
						}
						book := seamBook()
						book["market_ticker"] = ticker
						frame, err := socket.frame("orderbook_snapshot", book)
						if err != nil {
							feedErr <- err
							return
						}
						select {
						case socket.frames <- frame:
						case <-feedCtx.Done():
							return
						}
					}
				}
			}
		}
	}()
	lastClock := time.Now()
	await := func(why string, cond func() bool) {
		h.await(why, func() bool {
			if elapsed := time.Since(lastClock); elapsed >= 25*time.Millisecond {
				h.clk.Advance(elapsed)
				lastClock = time.Now()
			}
			select {
			case err := <-feedErr:
				t.Fatalf("turnover websocket fixture: %v", err)
			default:
			}
			return cond()
		})
	}
	return await, stop
}

// This crosses funding preflight, real startup adoption, selection GETs, the
// owner event loop, the websocket subscription, schedule reads, reducer writes,
// SQLite ownership binding, and monitor publication. A selected-only loop drops
// A after B wins and fails several independent assertions below.
func TestComposedTurnoverKeepsHeldOldMarketWhenSelectionMoves(t *testing.T) {
	h := newSeamHarness(t, seamOptions{SkipRig: true, ListCreated: true,
		Positions: map[string]string{seamTicker: "1.00"}})
	h.cfg.Turnover = true
	h.cfg.Candidates = []string{seamTicker, turnoverOtherTicker}
	h.cfg.CapitalSource = "selected_shard_balance"
	h.cfg.Params.NMarkets = 1
	h.cfg.Params.SchedulePoll = 100 * time.Millisecond
	h.cfg.Params.Reselect = 100 * time.Millisecond
	h.cfg.Params.CapitalMax = num.MoneyFromDollars(1000)
	x := &turnoverCompositionExchange{base: h.ex}
	h.xch.Doer = x
	var err error
	h.cfg, err = resolveFundingCap(h.ctx, h.cfg, x)
	if err != nil {
		t.Fatalf("turnover funding preflight: %v", err)
	}
	if len(h.cfg.ShardFunding) != 2 || h.cfg.Funding == nil {
		t.Fatalf("turnover funding evidence: %+v", h.cfg.ShardFunding)
	}
	// newSeamHarness seeded a one-market snapshot. A turnover target refresh
	// reconnects the socket, so answer the filtered book subscription for each
	// generation and any explicit snapshot request. Trade subscriptions have no
	// book snapshot response. Sequence numbers stay monotone across fake socket
	// generations; the production book decoder starts a new sequence on each.
	<-h.ws.frames
	h.anom = newAnomalySink()
	h.rig, err = newRig(h.ctx, h.cfg, false, h.xch, seamAlertFactory, h.anom, nil)
	if err != nil {
		t.Fatalf("newRig turnover: %v", err)
	}
	t.Cleanup(h.closeRig)
	h.start()
	await, _ := startTurnoverCompositionFeed(t, h, x)
	await("initial A selection and held reducer", func() bool {
		a, aOK := turnoverMarket(h.snapshot(), seamTicker)
		return aOK && a.Selected && a.Q == num.QtyFromFloat(1) && a.State == quote.Reducing
	})
	await("A's subscribed book, schedule and reducer", func() bool {
		a, ok := turnoverMarket(h.snapshot(), seamTicker)
		if !ok || !a.BookActionable || x.readCount("/markets/"+seamTicker) == 0 {
			return false
		}
		for _, c := range h.ex.allCreates() {
			if c.Ticker == seamTicker && c.WireSide == "ask" && c.Count == "1.00" {
				return true
			}
		}
		return false
	})
	x.setPhase(1) // A's program ended; B is now the sole eligible candidate.
	await("B selected while A remains a managed reducer", func() bool {
		s := h.snapshot()
		a, aOK := turnoverMarket(s, seamTicker)
		b, bOK := turnoverMarket(s, turnoverOtherTicker)
		return aOK && bOK && !a.Selected && b.Selected &&
			a.Q == num.QtyFromFloat(1) && a.State == quote.Reducing &&
			x.readCount("/markets/"+seamTicker) > 1
	})
	if got := h.rig.scheduleTickers(); len(got) != 2 || got[0] != seamTicker || got[1] != turnoverOtherTicker {
		t.Fatalf("managed schedule universe after rotation: %v", got)
	}
	if got := h.rig.sup.Tickers(); len(got) != 2 || got[0] != seamTicker || got[1] != turnoverOtherTicker {
		t.Fatalf("book subscription lost A after rotation: %v", got)
	}
	before := h.snapSeq()
	await("monitor samples both markets after rotation", func() bool {
		last := h.rig.last.Load()
		return h.snapSeq() > before && last != nil && !last.Stale &&
			len(last.Samples) == 2 && last.Samples[0].SourceSeq >= before
	})
	for _, c := range h.ex.allCreates() {
		if c.Ticker == seamTicker && (c.WireSide != "ask" || c.Count != "1.00") {
			t.Fatalf("old-market reducer added risk or exceeded held quantity: %+v", c)
		}
	}
	// F15 may stop the account, but it never gives us ownership of another
	// trader's order. Keep A's reduction obligation visible through that stop.
	h.ex.mu.Lock()
	h.ex.resting = append(h.ex.resting, map[string]any{
		"order_id": "foreign-turnover", "client_order_id": "manual-order",
		"ticker": turnoverOtherTicker, "book_side": "bid", "status": "resting",
		"yes_price_dollars": "0.4000", "no_price_dollars": "0.6000",
		"remaining_count_fp": "1.00", "fill_count_fp": "0.00",
		"initial_count_fp": "1.00",
	})
	h.ex.mu.Unlock()
	h.clk.Advance(h.cfg.Params.PositionPoll)
	await("foreign order causes the account stop", func() bool {
		return h.latchTrigger() == "foreign_order"
	})
	h.awaitTicks(2)
	for _, id := range h.ex.deletedIDs() {
		if id == "foreign-turnover" {
			t.Fatal("rotation cancelled a foreign order")
		}
	}
	a, ok := turnoverMarket(h.snapshot(), seamTicker)
	if !ok || a.Q != num.QtyFromFloat(1) || a.State != quote.Reducing {
		t.Fatalf("account stop lost old-market reducer: %+v present=%t", a, ok)
	}
}

// turnoverFailingDialer refuses every dial while failing is set.
type turnoverFailingDialer struct {
	base    wsx.Dialer
	failing atomic.Bool
	refused atomic.Int32
}

func (d *turnoverFailingDialer) Dial(ctx context.Context, url string, h http.Header) (wsx.Socket, error) {
	if d.failing.Load() {
		d.refused.Add(1)
		return nil, errors.New("scripted websocket dial failure")
	}
	return d.base.Dial(ctx, url, h)
}

// A universe refresh retires the socket generation. If the re-dial then fails,
// the poller must already hold a token for the new generation: portfolio truth
// keeps flowing while the socket is down (portfolio.go), and only a new
// connection and snapshot make a book actionable again.
func TestComposedTurnoverUniverseRefreshKeepsPortfolioTruthWhileRedialFails(t *testing.T) {
	h := newSeamHarness(t, seamOptions{SkipRig: true, ReadOnly: true})
	h.cfg.Turnover = true
	h.cfg.Candidates = []string{seamTicker, turnoverOtherTicker}
	h.cfg.CapitalSource = "selected_shard_balance"
	h.cfg.Params.NMarkets = 1
	h.cfg.Params.SchedulePoll = 100 * time.Millisecond
	h.cfg.Params.Reselect = 100 * time.Millisecond
	h.cfg.Params.CapitalMax = num.MoneyFromDollars(1000)
	// The outage below outlasts truth_max_age with F4 pushed past it, so the
	// only thing under test is whether portfolio reads still apply. The
	// balance loop ticks on real time, so a short real cadence keeps
	// selected-shard cash fresh while virtual time follows real time.
	h.cfg.Params.PositionPoll = 20 * time.Millisecond
	h.cfg.Params.TruthMaxAge = time.Second
	h.cfg.Params.DisconnectReduce = 10 * time.Minute
	x := &turnoverCompositionExchange{base: h.ex}
	h.xch.Doer = x
	dialer := &turnoverFailingDialer{base: h.ws}
	h.xch.Dialer = dialer
	var err error
	h.cfg, err = resolveFundingCap(h.ctx, h.cfg, x)
	if err != nil {
		t.Fatalf("turnover funding preflight: %v", err)
	}
	<-h.ws.frames
	h.anom = newAnomalySink()
	h.rig, err = newRig(h.ctx, h.cfg, false, h.xch, seamAlertFactory, h.anom, nil)
	if err != nil {
		t.Fatalf("newRig turnover: %v", err)
	}
	t.Cleanup(h.closeRig)
	h.start()
	await, _ := startTurnoverCompositionFeed(t, h, x)
	await("A selected with an actionable book and fresh truth", func() bool {
		s := h.snapshot()
		a, ok := turnoverMarket(s, seamTicker)
		return ok && a.Selected && a.BookActionable && s.Account.TruthFresh
	})

	staleBefore := len(h.anomalySevs("RECONCILE_TOKEN_STALE"))
	dialer.failing.Store(true)
	x.setTarget("20.00")
	await("the target refresh dropped the socket and the re-dial failed", func() bool {
		a, ok := turnoverMarket(h.snapshot(), seamTicker)
		return dialer.refused.Load() > 0 && ok && !a.BookActionable
	})
	down := h.clk.monoNow()
	await("an outage past truth_max_age and a refused retry", func() bool {
		return h.clk.monoNow()-down > h.cfg.Params.TruthMaxAge+5*h.cfg.Params.PositionPoll &&
			dialer.refused.Load() >= 2
	})
	h.awaitTicks(2)
	if latch := h.latchTrigger(); latch != "" {
		t.Fatalf("the outage stopped the account (%s); anomalies=%v", latch, h.anomalyClasses())
	}
	s := h.snapshot()
	if !s.Account.TruthFresh {
		t.Fatalf("portfolio truth aged out while the re-dial failed; anomalies=%v",
			h.anomalyClasses())
	}
	for _, m := range s.Markets {
		if m.BookActionable {
			t.Fatalf("%s is actionable without a new connection and snapshot", m.Ticker)
		}
	}
	// Only reads already buffered or in flight at the refresh may be stale.
	if got := len(h.anomalySevs("RECONCILE_TOKEN_STALE")) - staleBefore; got > readBuffer+1 {
		t.Fatalf("%d RECONCILE_TOKEN_STALE rows during the outage, want at most %d",
			got, readBuffer+1)
	}

	dialer.failing.Store(false)
	await("a new connection and snapshot make A actionable again", func() bool {
		a, ok := turnoverMarket(h.snapshot(), seamTicker)
		return ok && a.BookActionable
	})
}

// fillReducer changes only the fake exchange. It models an execution record,
// the exact order remainder, and authoritative account inventory atomically;
// the owner must learn every fact through its ordinary REST readers.
func (x *turnoverCompositionExchange) fillReducer(t *testing.T, c seamCreate, quantity, remaining, position, tradeID string, tsMs int64) {
	t.Helper()
	if c.Ticker != seamTicker || c.WireSide != string(rest.Ask) {
		t.Fatalf("fixture was asked to fill something other than A's reducer: %+v", c)
	}
	yes, err := rest.ParsePrice4(c.Price)
	if err != nil {
		t.Fatalf("reducer execution price: %v", err)
	}
	no := 10000 - yes
	fill := seamMakerFill(tradeID, c.OrderID, "no", c.Price,
		fmt.Sprintf("%d.%04d", no/10000, no%10000), quantity, tsMs)
	fill["fee_cost"] = "0.0000"
	x.base.mu.Lock()
	defer x.base.mu.Unlock()
	found := false
	kept := make([]map[string]any, 0, len(x.base.resting))
	for _, old := range x.base.resting {
		if old["order_id"] != c.OrderID {
			kept = append(kept, old)
			continue
		}
		found = true
		order := make(map[string]any, len(old))
		for key, value := range old {
			order[key] = value
		}
		order["remaining_count"] = remaining
		if remaining != "0.00" {
			kept = append(kept, order)
			continue
		}
		order["status"] = rest.StatusExecuted
		x.mu.Lock()
		x.terminals = append(x.terminals, order)
		x.mu.Unlock()
	}
	if !found {
		t.Fatalf("cannot execute unlisted reducer %s", c.OrderID)
	}
	x.base.resting = kept
	x.base.fills = append(x.base.fills, fill)
	x.base.positions[c.Ticker] = position
}

// Restart deliberately removes A from the configured candidate list. Its
// remaining inventory and owned order must nevertheless be reconstructed from
// durable ownership plus complete account truth, reduced through the production
// writer, and included in the eventual account-wide drain decision.
func TestComposedTurnoverRestartReconstructsOldMarketReducesAndExits(t *testing.T) {
	h := newSeamHarness(t, seamOptions{SkipRig: true, ListCreated: true,
		Positions: map[string]string{seamTicker: "1.00"}})
	h.cfg.Turnover = true
	h.cfg.Candidates = []string{seamTicker, turnoverOtherTicker}
	h.cfg.CapitalSource = "selected_shard_balance"
	h.cfg.Params.NMarkets = 1
	h.cfg.Params.SchedulePoll = 100 * time.Millisecond
	h.cfg.Params.Reselect = 100 * time.Millisecond
	h.cfg.Params.PositionPoll = 100 * time.Millisecond
	h.cfg.Params.CapitalMax = num.MoneyFromDollars(1000)
	x := &turnoverCompositionExchange{base: h.ex}
	h.xch.Doer = x
	var err error
	h.cfg, err = resolveFundingCap(h.ctx, h.cfg, x)
	if err != nil {
		t.Fatalf("first-incarnation funding preflight: %v", err)
	}
	<-h.ws.frames // Every dynamic generation is answered from its subscription.
	h.anom = newAnomalySink()
	h.rig, err = newRig(h.ctx, h.cfg, false, h.xch, seamAlertFactory, h.anom, nil)
	if err != nil {
		t.Fatalf("first turnover incarnation: %v", err)
	}
	t.Cleanup(h.closeRig)
	h.start()
	await, stopFeed := startTurnoverCompositionFeed(t, h, x)
	await("A selected and production reducer listed", func() bool {
		a, ok := turnoverMarket(h.snapshot(), seamTicker)
		return ok && a.Selected && a.Q == num.QtyFromFloat(1) &&
			a.State == quote.Reducing && a.BookActionable && h.ex.restingCount() == 1
	})
	first, ok := h.ex.createAt(0)
	if !ok || first.Ticker != seamTicker || first.WireSide != string(rest.Ask) || first.Count != "1.00" {
		t.Fatalf("first-incarnation reducer: %+v present=%t", first, ok)
	}
	await("A reducer durable ownership binding", func() bool {
		coid, bound := h.rig.store.Ownership().Bound(first.OrderID)
		return bound && coid == first.Coid
	})
	x.setPhase(1)
	await("A to B selection before restart", func() bool {
		a, aOK := turnoverMarket(h.snapshot(), seamTicker)
		b, bOK := turnoverMarket(h.snapshot(), turnoverOtherTicker)
		return aOK && bOK && !a.Selected && b.Selected &&
			a.Q == num.QtyFromFloat(1) && a.State == quote.Reducing
	})
	h.seamCreateStop()
	await("durable stop while A still needs reduction", func() bool {
		a, ok := turnoverMarket(h.snapshot(), seamTicker)
		return h.latchTrigger() == "harness_stop" && h.snapshot().Global == quote.WindingDown &&
			ok && a.Q == num.QtyFromFloat(1) && a.State == quote.Reducing
	})
	stopFeed()
	h.stopServe()
	if err := h.rig.close(context.Background()); err != nil {
		t.Fatalf("close interrupted first incarnation: %v", err)
	}
	if err := os.Remove(h.cfg.Paths.Stop); err != nil {
		t.Fatalf("remove already-committed stop request: %v", err)
	}

	// The exchange executes part of the old reducer while the client is down.
	// On restart A is neither the configured seed nor an approved candidate.
	const historicalTrade = "SEAM-TURNOVER-DOWN-FILL"
	x.fillReducer(t, first, "0.25", "0.75", "0.75", historicalTrade, h.clk.wallMs())
	h.cfg.Ticker = turnoverOtherTicker
	h.cfg.Candidates = []string{turnoverOtherTicker}
	h.cfg, err = resolveFundingCap(h.ctx, h.cfg, x)
	if err != nil {
		t.Fatalf("restart funding preflight with old-market exposure: %v", err)
	}
	if _, present := h.cfg.ShardFunding[seamTicker]; !present {
		t.Fatal("restart funding omitted A because it is no longer a candidate")
	}
	readsBeforeRefusal := h.ex.ordersWalkCount()
	refused, err := seamNewRig(t, h.ctx, h.cfg, false, h.xch)
	if err == nil {
		if refused != nil {
			_ = refused.close(context.Background())
		}
		t.Fatal("unattended turnover restart crossed the durable halt latch")
	}
	if !strings.Contains(err.Error(), "-resume") || h.ex.ordersWalkCount() != readsBeforeRefusal {
		t.Fatalf("restart refusal=%v; orders walks changed from %d to %d", err,
			readsBeforeRefusal, h.ex.ordersWalkCount())
	}
	h.ws = newSeamDialer()
	h.xch.Dialer = h.ws
	h.anom = newAnomalySink()
	h.rig, err = newRig(h.ctx, h.cfg, true, h.xch, seamAlertFactory, h.anom, nil)
	if err != nil {
		t.Fatalf("attended turnover restart: %v", err)
	}
	createsAtRestart := h.ex.createCount()
	h.start()
	await, _ = startTurnoverCompositionFeed(t, h, x)
	await("restart reconstructs old A beside newly selected B", func() bool {
		s := h.snapshot()
		a, aOK := turnoverMarket(s, seamTicker)
		b, bOK := turnoverMarket(s, turnoverOtherTicker)
		return s != nil && s.Global == quote.WindingDown && aOK && bOK &&
			!a.Selected && b.Selected && a.Q == num.QtyFromFloat(.75) &&
			a.State == quote.Reducing && a.BookActionable
	})
	await("restart commits the attributable historical reducer fill", func() bool {
		_, found, err := h.rig.store.Reader().Fill(historicalTrade)
		return err == nil && found
	})
	row, found, err := h.rig.store.Reader().Fill(historicalTrade)
	if err != nil || !found || !row.Backfilled || row.OrderID != first.OrderID ||
		row.Count != num.QtyFromFloat(.25) || row.Fee != 0 {
		t.Fatalf("reconstructed historical fill=%+v present=%t err=%v", row, found, err)
	}
	if coid, bound := h.rig.store.Ownership().Bound(first.OrderID); !bound || coid != first.Coid {
		t.Fatalf("old-market ownership after restart=%q present=%t", coid, bound)
	}
	if got := h.latchTrigger(); got != "harness_stop" {
		t.Fatalf("restart cleared or replaced the durable stop: %q", got)
	}
	for _, universe := range [][]string{h.rig.scheduleTickers(), h.rig.sup.Tickers()} {
		sort.Strings(universe)
		if len(universe) != 2 || universe[0] != seamTicker || universe[1] != turnoverOtherTicker {
			t.Fatalf("restart old-market schedule/subscription universe=%v", universe)
		}
	}
	priorWalks := h.ex.ordersWalkCount()
	h.clk.Advance(h.cfg.Params.PositionPoll)
	await("fresh post-restart account reconciliation", func() bool {
		return h.ex.ordersWalkCount() > priorWalks
	})
	h.awaitTicks(2)
	a, ok := turnoverMarket(h.snapshot(), seamTicker)
	if !ok || a.Q != num.QtyFromFloat(.75) || a.State != quote.Reducing {
		t.Fatalf("historical fill replay double-counted A's reduction: %+v present=%t", a, ok)
	}
	for _, c := range h.ex.allCreates()[createsAtRestart:] {
		q, err := num.ParseQty(c.Count)
		if err != nil || c.Ticker != seamTicker || c.WireSide != string(rest.Ask) || q > num.QtyFromFloat(.75) {
			t.Fatalf("halted restart created an adding or oversized order: %+v err=%v", c, err)
		}
	}

	// Finish the exact inherited reducer. Omission from a resting walk is not
	// enough: the fake serves its positive terminal record on the separate
	// all-status poll and the owner must publish zero at-risk size as well as q.
	const finalTrade = "SEAM-TURNOVER-FINAL-FILL"
	x.fillReducer(t, first, "0.75", "0.00", "0.00", finalTrade, h.clk.wallMs())
	h.clk.Advance(h.cfg.Params.PositionPoll)
	await("final reducer fill committed with its fee", func() bool {
		row, found, err := h.rig.store.Reader().Fill(finalTrade)
		return err == nil && found && row.OrderID == first.OrderID &&
			row.Count == num.QtyFromFloat(.75) && row.Fee == 0
	})
	await("authoritative zero exposure and terminal obligations permit drain", func() bool {
		s := h.snapshot()
		if s == nil || s.Global != quote.Drained {
			return false
		}
		for _, m := range s.Markets {
			if m.Q != 0 || m.Sides[0].AtRisk != 0 || m.Sides[1].AtRisk != 0 {
				return false
			}
		}
		x.mu.Lock()
		defer x.mu.Unlock()
		return x.terminalDeliveries > 0
	})
	select {
	case code := <-h.serveExits:
		t.Fatalf("latched turnover drained and exited %d without the operator signal", code)
	default:
	}
	h.serveSignals <- syscall.SIGTERM
	select {
	case code := <-h.serveExits:
		if code != 0 {
			t.Fatalf("turnover clean exit code=%d", code)
		}
	case <-time.After(seamBudget):
		t.Fatal("turnover drain never reached the production exit callback")
	}
	h.stopServe()
	// These are fresh unfiltered transport reads after the process has stopped,
	// not a conclusion inferred from the last owner or monitor publication.
	positions := h.rig.api.AccountPositions(h.ctx)
	orders := h.rig.api.Orders(h.ctx, "", rest.StatusResting)
	if !positions.Replaces() || !orders.Replaces() || len(orders.Orders) != 0 {
		t.Fatalf("final account-wide proof: positions=%+v orders=%+v", positions, orders)
	}
	for ticker, q := range positions.ByTicker {
		if q != 0 {
			t.Fatalf("clean exit left authoritative position %s=%s", ticker, q.Wire())
		}
	}
}

func TestComposedTurnoverRestartRetainsOmittedBoundOldOrder(t *testing.T) {
	h := newSeamHarness(t, seamOptions{ListCreated: true})
	h.installOwnedOrder("omitted-bound-old-A", 933)
	if err := h.rig.close(context.Background()); err != nil {
		t.Fatal(err)
	}
	h.cfg.Turnover = true
	h.cfg.Ticker = turnoverOtherTicker
	h.cfg.Candidates = []string{turnoverOtherTicker}
	h.cfg.CapitalSource = "selected_shard_balance"
	h.cfg.Params.NMarkets = 1
	h.cfg.Params.SchedulePoll = 100 * time.Millisecond
	h.cfg.Params.PositionPoll = 100 * time.Millisecond
	h.cfg.Params.Reselect = 100 * time.Millisecond
	x := &turnoverCompositionExchange{base: h.ex, phase: 1}
	h.xch.Doer = x
	var err error
	h.cfg, err = resolveFundingCap(h.ctx, h.cfg, x)
	if err != nil {
		t.Fatal(err)
	}
	h.ws = newSeamDialer()
	h.xch.Dialer = h.ws
	h.anom = newAnomalySink()
	h.rig, err = newRig(h.ctx, h.cfg, false, h.xch, seamAlertFactory, h.anom, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(h.rig.turnoverInherited) != 1 || h.rig.turnoverInherited[0].OrderID != "omitted-bound-old-A" {
		t.Fatalf("restart lost omitted durable obligation: %+v", h.rig.turnoverInherited)
	}
	h.start()
	await, _ := startTurnoverCompositionFeed(t, h, x)
	await("old omitted obligation reaches production owned-only cancellation", func() bool {
		for _, id := range h.ex.deletedIDs() {
			if id == "omitted-bound-old-A" {
				return true
			}
		}
		return false
	})
	if _, ok := turnoverMarket(h.snapshot(), seamTicker); !ok {
		t.Fatal("old omitted order market disappeared from monitor")
	}
}

// confidence: low
