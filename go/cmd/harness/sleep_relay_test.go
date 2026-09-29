package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"lip/harness/risk"
)

// TestOwnerSleepRelay runs the real owner, socket supervisor, portfolio poller
// and store writer against the seam exchange. The virtual wall steps while its
// monotonic reading does not: deriving both deltas from one clock, or leaving
// SleepDetector unconstructed in serve, fails every response below.
func TestOwnerSleepRelay(t *testing.T) {
	h := newSeamHarness(t, seamOptions{})
	h.start()
	h.awaitActionable()
	h.awaitTicks(1) // The owner has established F7's baseline sample.
	priorWalks := h.ex.ordersWalkCount()
	priorCommands := len(h.ws.subscriptions())
	if priorWalks == 0 {
		t.Fatal("the startup reconciliation never reached the exchange")
	}

	h.clk.mu.Lock()
	h.clk.wall += int64((7 * time.Second) / time.Millisecond)
	h.clk.mu.Unlock()

	h.await("F7 to quarantine the accepted book", func() bool {
		m, ok := h.market()
		return ok && !m.BookActionable
	})
	h.await("F7 resnapshot command to reach the socket", func() bool {
		return len(h.ws.subscriptions()) > priorCommands
	})
	h.await("F7 reconcile token to start a new complete portfolio walk", func() bool {
		return h.ex.ordersWalkCount() > priorWalks
	})
	h.await("F7 downtime interval to reach the durable anomaly store", func() bool {
		return len(h.anomalySevs("HOST_SLEEP_OR_STALL")) > 0
	})

	cmds := h.ws.subscriptions()[priorCommands:]
	found := false
	for _, raw := range cmds {
		var msg struct {
			Cmd    string `json:"cmd"`
			Params struct {
				Action  string   `json:"action"`
				Tickers []string `json:"market_tickers"`
			} `json:"params"`
		}
		if err := json.Unmarshal(raw, &msg); err != nil {
			t.Fatalf("socket command: %v", err)
		}
		if msg.Cmd == "update_subscription" && msg.Params.Action == "get_snapshot" &&
			len(msg.Params.Tickers) == 1 && msg.Params.Tickers[0] == seamTicker {
			found = true
		}
	}
	if !found {
		t.Fatalf("F7 sent no real get_snapshot command: %s", cmds)
	}

	rows, err := h.rig.store.Reader().PendingAnomalies()
	if err != nil {
		t.Fatalf("read durable anomalies: %v", err)
	}
	found = false
	for _, a := range rows {
		if a.Class != "HOST_SLEEP_OR_STALL" {
			continue
		}
		if a.Sev != risk.SEV2 || !strings.Contains(a.Text, "wall_ms=[") ||
			!strings.Contains(a.Text, "duration=7s") ||
			!strings.Contains(a.Text, "cumulative=7s") {
			t.Fatalf("F7 durable downtime evidence: %+v", a)
		}
		found = true
	}
	if !found {
		t.Fatal("HOST_SLEEP_OR_STALL was not persisted")
	}
}
