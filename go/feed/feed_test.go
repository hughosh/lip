package feed

import (
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/coder/websocket"
)

// --- P27: the subscription shapes -------------------------------------------
//
// These matter because getting them wrong changes NO fill row. The trade filter
// in particular looks like an obvious tidy-up: adding market_tickers to it would
// produce an identical `fill` table while silently ending the whole-exchange
// capture that stats["trades"] and the 98.6% discard rate are defined against.

func decode(t *testing.T, build func() ([]byte, error)) map[string]any {
	t.Helper()
	b, err := build()
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func params(t *testing.T, m map[string]any) map[string]any {
	t.Helper()
	p, ok := m["params"].(map[string]any)
	if !ok {
		t.Fatalf("no params object in %v", m)
	}
	return p
}

func channels(t *testing.T, p map[string]any) []string {
	t.Helper()
	raw, ok := p["channels"].([]any)
	if !ok {
		t.Fatalf("no channels array in %v", p)
	}
	out := make([]string, len(raw))
	for i, c := range raw {
		out[i], _ = c.(string)
	}
	return out
}

func TestP27_DeltaSubscriptionIsFilteredToTheUniverse(t *testing.T) {
	m := decode(t, func() ([]byte, error) { return subscribeDelta([]string{"AAA", "BBB"}) })

	if m["id"] != float64(1) {
		t.Errorf("id = %v, want 1", m["id"])
	}
	if m["cmd"] != "subscribe" {
		t.Errorf("cmd = %v, want subscribe", m["cmd"])
	}
	p := params(t, m)
	if got := channels(t, p); len(got) != 1 || got[0] != "orderbook_delta" {
		t.Errorf("channels = %v, want [orderbook_delta]", got)
	}
	tk, ok := p["market_tickers"].([]any)
	if !ok || len(tk) != 2 {
		t.Fatalf("market_tickers = %v, want the 2 subscribed tickers", p["market_tickers"])
	}
	if tk[0] != "AAA" || tk[1] != "BBB" {
		t.Errorf("market_tickers = %v, want [AAA BBB] in the order given", tk)
	}
}

func TestP27_TradeSubscriptionHasNoMarketFilter(t *testing.T) {
	m := decode(t, subscribeTrade)

	if m["id"] != float64(2) {
		t.Errorf("id = %v, want 2", m["id"])
	}
	p := params(t, m)
	if got := channels(t, p); len(got) != 1 || got[0] != "trade" {
		t.Errorf("channels = %v, want [trade]", got)
	}
	if _, present := p["market_tickers"]; present {
		t.Error("the trade subscription carries a market filter. It must not: one " +
			"subscription yields the whole exchange tape, which core.Rig filters " +
			"down, and the 98.6% discard rate is the standing check that this is " +
			"what we actually subscribed to. See port-spec.md P27.")
	}
}

func TestP27_ResnapshotRequestShape(t *testing.T) {
	m := decode(t, func() ([]byte, error) {
		return resnapshotRequest([]int64{9, 4}, []string{"AAA"})
	})

	if m["id"] != float64(3) {
		t.Errorf("id = %v, want 3", m["id"])
	}
	if m["cmd"] != "update_subscription" {
		t.Errorf("cmd = %v, want update_subscription", m["cmd"])
	}
	p := params(t, m)
	if p["action"] != "get_snapshot" {
		t.Errorf("action = %v, want get_snapshot", p["action"])
	}
	sids, ok := p["sids"].([]any)
	if !ok || len(sids) != 2 || sids[0] != float64(9) || sids[1] != float64(4) {
		t.Errorf("sids = %v, want [9 4] in first-seen order", p["sids"])
	}
	if tk, ok := p["market_tickers"].([]any); !ok || len(tk) != 1 {
		t.Errorf("market_tickers = %v, want the full ticker list", p["market_tickers"])
	}
}

// An empty sid map must serialise as [] rather than null: Python sends
// list(self.seq), which is [] when the map is empty.
func TestP27_ResnapshotWithNoSidsSendsEmptyArray(t *testing.T) {
	b, err := resnapshotRequest(nil, []string{"AAA"})
	if err != nil {
		t.Fatal(err)
	}
	var m struct {
		Params struct {
			Sids *[]int64 `json:"sids"`
		} `json:"params"`
	}
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	if m.Params.Sids == nil {
		t.Fatal("sids serialised as null, want []")
	}
	if len(*m.Params.Sids) != 0 {
		t.Errorf("sids = %v, want []", *m.Params.Sids)
	}
}

// --- P25a: a clean close is not the reconnect path --------------------------
//
// websockets' Connection.__aiter__ is
//
//	try:  while True: yield await self.recv()
//	except ConnectionClosedOK: return
//
// so close code 1000/1001 ends rig.py:505's `async for` NORMALLY. The `try`
// completes, the `except (ConnectionClosed, OSError)` at rig.py:548 never runs,
// and the loop reconnects with no commit, no backoff and NO ResetOnReconnect —
// carrying the old books, history, refs, seq and stale set across.
//
// Misclassifying either direction changes rows: treating a clean close as an
// error wipes state Python keeps, and treating an error close as clean keeps
// state Python wipes.
func TestP25a_CleanCloseClassification(t *testing.T) {
	clean := []websocket.StatusCode{
		websocket.StatusNormalClosure, // 1000
		websocket.StatusGoingAway,     // 1001
	}
	for _, s := range clean {
		err := fmt.Errorf("read: %w", websocket.CloseError{Code: s})
		if !IsCleanClose(err) {
			t.Errorf("close code %d classified as an error; Python's iterator "+
				"exits normally on it, so no reset must happen", s)
		}
	}

	dirty := []websocket.StatusCode{
		websocket.StatusAbnormalClosure, // 1006 — what a dropped TCP gives
		websocket.StatusProtocolError,   // 1002
		websocket.StatusInternalError,   // 1011
		websocket.StatusPolicyViolation, // 1008
		websocket.StatusMessageTooBig,   // 1009
		websocket.StatusServiceRestart,  // 1012
		websocket.StatusTryAgainLater,   // 1013
	}
	for _, s := range dirty {
		err := fmt.Errorf("read: %w", websocket.CloseError{Code: s})
		if IsCleanClose(err) {
			t.Errorf("close code %d classified as clean; it raises "+
				"ConnectionClosedError in Python, which DOES reset state", s)
		}
	}

	// A plain transport error is not a clean close either.
	if IsCleanClose(errors.New("connection reset by peer")) {
		t.Error("a bare transport error was classified as a clean close")
	}
	if !IsCleanClose(ErrCleanClose) {
		t.Error("ErrCleanClose must classify as clean")
	}
}
