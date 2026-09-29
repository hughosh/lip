package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"lip/harness/hstore"
	"lip/harness/num"
	"lip/harness/ping"
	"lip/harness/quote"
)

type restartHeartbeatRecorder struct{ seen chan ping.Heartbeat }

func (r *restartHeartbeatRecorder) Step(_ context.Context, nowMs int64,
	hb ping.Heartbeat) ping.Effects {
	select {
	case r.seen <- hb:
	default:
	}
	return ping.Effects{NextStepMs: nowMs + int64(time.Minute/time.Millisecond)}
}

// A runtime can disappear after committing a stop while its exchange order and
// inventory remain. The second rig must reconstruct both from the same durable
// store and the same external account, rather than from first-run memory.
func TestComposedAbruptStopRestartAdoptsOwnedReducerAndHistoricalFill(t *testing.T) {
	h := newSeamHarness(t, seamOptions{
		ListCreated: true,
		Positions:   map[string]string{seamTicker: "1.00"},
	})
	h.start()
	h.awaitActionable()
	h.await("first-run reducer on the exchange", func() bool {
		return h.ex.restingCount() == 1
	})
	first, ok := h.ex.createAt(0)
	if !ok || first.WireSide != "ask" {
		t.Fatalf("first-run reducer=%+v present=%v", first, ok)
	}
	h.await("first-run ownership binding committed", func() bool {
		coid, bound := h.rig.store.Ownership().Bound(first.OrderID)
		return bound && coid == first.Coid
	})

	h.seamCreateStop()
	h.await("owner committed stop before interruption", func() bool {
		return h.latchTrigger() == "harness_stop"
	})
	h.awaitGlobalEvent("RUNNING->WINDING_DOWN/global_stop")
	h.await("halted owner still holds inventory", func() bool {
		s := h.snapshot()
		m, ok := h.market()
		return s != nil && s.Global == quote.WindingDown && ok &&
			m.Q == num.QtyFromFloat(1) && m.State == quote.Reducing
	})

	// Cancel the runtime without a planned shutdown signal. The resource close
	// stands in for process teardown and releases the instance lock; it does not
	// touch the latch, store path, or external exchange state.
	h.stopServe()
	if err := h.rig.close(context.Background()); err != nil {
		t.Fatalf("closing interrupted incarnation: %v", err)
	}
	if err := os.Remove(h.cfg.Paths.Stop); err != nil {
		t.Fatalf("removing stop request after it was committed: %v", err)
	}
	if h.ex.restingCount() != 1 {
		t.Fatalf("the exchange lost the inherited order at interruption")
	}

	// This fill happens while the client is down. The exchange's authoritative
	// position already includes it, and the full fills walk will report it on
	// every later poll. The 1.00 reducer remains listed until reconciliation.
	const tradeID = "SEAM-RESTART-HISTORY-1"
	h.ex.addFill(map[string]any{
		"fill_id": "SEAM-RESTART-FILL-1", "trade_id": tradeID,
		"order_id": first.OrderID, "ticker": seamTicker, "side": "no",
		"yes_price_dollars": "0.4500", "no_price_dollars": "0.5500",
		"count": "0.25", "is_taker": false, "fee_cost": "0.0000",
		"ts": fmt.Sprintf("%d", h.clk.wallMs()),
	})
	h.ex.setPosition(seamTicker, "0.75")
	readsBeforeRefusal := h.ex.ordersWalkCount()
	createsBeforeRestart := h.ex.createCount()

	refused, err := seamNewRig(t, h.ctx, h.cfg, false, h.xch)
	if err == nil {
		if refused != nil {
			_ = refused.close(context.Background())
		}
		t.Fatal("unattended restart crossed a durable halt latch")
	}
	if !strings.Contains(err.Error(), "-resume") || h.ex.ordersWalkCount() != readsBeforeRefusal {
		t.Fatalf("restart refusal=%v; exchange order walks=%d, want %d",
			err, h.ex.ordersWalkCount(), readsBeforeRefusal)
	}
	if got := h.latchTrigger(); got != "harness_stop" {
		t.Fatalf("refusal changed durable stop to %q", got)
	}

	// A fresh websocket subscription is part of a new process; the REST account
	// and its accepted coids stay on the same fake exchange.
	h.ws = newSeamDialer()
	h.xch.Dialer = h.ws
	frame, err := h.ws.frame("orderbook_snapshot", seamBook())
	if err != nil {
		t.Fatal(err)
	}
	h.ws.frames <- frame
	recorder := &restartHeartbeatRecorder{seen: make(chan ping.Heartbeat, 16)}
	makeAlerts := func(*hstore.Reader, *hstore.Store,
		time.Duration) (alertStepper, error) {
		return recorder, nil
	}
	restarted, err := newRig(h.ctx, h.cfg, true, h.xch,
		makeAlerts, newAnomalySink(), nil)
	if err != nil {
		t.Fatalf("authorized restart after refused start: %v", err)
	}
	h.rig = restarted
	h.start()
	h.awaitActionable()
	h.await("historical fill adopted into the durable ledger", func() bool {
		_, found, err := h.rig.store.Reader().Fill(tradeID)
		return err == nil && found
	})
	h.await("halted restarted owner publishes inherited inventory", func() bool {
		s := h.snapshot()
		m, ok := h.market()
		return s != nil && s.Global == quote.WindingDown && ok &&
			m.Q == num.QtyFromFloat(.75) && m.State == quote.Reducing
	})
	row, found, err := h.rig.store.Reader().Fill(tradeID)
	if err != nil || !found || !row.Backfilled || row.OrderID != first.OrderID ||
		row.Count != num.QtyFromFloat(.25) {
		t.Fatalf("inherited owned fill row=%+v found=%v err=%v", row, found, err)
	}
	if coid, bound := h.rig.store.Ownership().Bound(first.OrderID); !bound || coid != first.Coid {
		t.Fatalf("restart ownership binding=%q present=%v", coid, bound)
	}
	if got := h.latchTrigger(); got != "harness_stop" {
		t.Fatalf("authorized restart cleared stop: %q", got)
	}

	// Force another complete poll of the same historical fill. The exchange's
	// position is authoritative; replaying the fill must not subtract it again.
	priorWalks := h.ex.ordersWalkCount()
	h.clk.Advance(h.cfg.Params.PositionPoll)
	h.await("post-restart portfolio poll", func() bool {
		return h.ex.ordersWalkCount() > priorWalks
	})
	h.awaitTicks(2)
	m, ok := h.market()
	if !ok || m.Q != num.QtyFromFloat(.75) || m.State != quote.Reducing {
		t.Fatalf("replayed historical fill changed q/state: %+v present=%v", m, ok)
	}
	if h.ex.createCount() < createsBeforeRestart || h.ex.restingCount() == 0 {
		t.Fatalf("reducer unavailable: creates=%d resting=%d",
			h.ex.createCount(), h.ex.restingCount())
	}
	for _, c := range h.ex.allCreates()[createsBeforeRestart:] {
		if c.WireSide != "ask" {
			t.Fatalf("restart placed an adding order under durable stop: %+v", c)
		}
	}
	before := h.snapSeq()
	h.awaitTicks(4)
	h.await("monitor samples restarted owner publication", func() bool {
		last := h.rig.last.Load()
		return last != nil && !last.Stale && len(last.Samples) == 1 &&
			last.Samples[0].SourceSeq >= before &&
			last.Samples[0].Snap.Q == num.QtyFromFloat(.75)
	})
	if h.snapSeq() <= before {
		t.Fatal("owner publication stopped after restart")
	}
	heartbeat := h.rig.heartbeat()
	if heartbeat.Global != quote.WindingDown || len(heartbeat.Markets) != 1 ||
		heartbeat.Markets[0].State != quote.Reducing ||
		!heartbeat.Markets[0].Q.Known ||
		heartbeat.Markets[0].Q.V != num.QtyFromFloat(.75) ||
		!heartbeat.SourceStale.Known || heartbeat.SourceStale.V {
		t.Fatalf("post-restart heartbeat=%+v", heartbeat)
	}
	h.rig.wakeAlerts()
	h.await("alert loop receives the post-restart heartbeat", func() bool {
		for {
			select {
			case hb := <-recorder.seen:
				if hb.Global == quote.WindingDown && len(hb.Markets) == 1 &&
					hb.Markets[0].State == quote.Reducing &&
					hb.Markets[0].Q.Known &&
					hb.Markets[0].Q.V == num.QtyFromFloat(.75) {
					return true
				}
			default:
				return false
			}
		}
	})
	if s := h.snapshot(); s.Global != quote.WindingDown {
		t.Fatalf("global state after polling=%s", s.Global)
	}
	if !seamContains(h.anomalyClasses(), "HARNESS_STOP_REQUESTED") {
		t.Fatal("committed stop anomaly disappeared across restart")
	}
}
