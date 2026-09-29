package wsx

import (
	"encoding/json"
	"fmt"
)

// The three command ids. They are fixed, not sequential, because the exchange
// echoes the id back on its acknowledgement and a subscription confirmation for
// id 1 is the only way to know the FILTERED channel is the one that came up.
//
// The asymmetry between the two subscriptions is deliberate and is not a
// tidy-up opportunity: `orderbook_delta` is filtered to our universe, `trade`
// is not filtered at all. One `trade` subscription yields the whole exchange
// tape, which `core.Rig` filters down. Adding a market filter would change no
// row and would silently stop the capture being a whole-exchange capture.
const (
	idSubscribeDelta = 1
	idSubscribeTrade = 2
	idResnapshot     = 3
)

// subscribeDelta is the filtered orderbook subscription. The payload shape is
// the one `feed` already verified against the live exchange; it is reproduced
// rather than imported because `feed` is hash-pinned and its Conn deliberately
// carries no keepalive.
func subscribeDelta(tickers []string) ([]byte, error) {
	if err := ValidateTickers(tickers); err != nil {
		return nil, err
	}
	return json.Marshal(map[string]any{
		"id": idSubscribeDelta, "cmd": "subscribe",
		"params": map[string]any{
			"channels":       []string{"orderbook_delta"},
			"market_tickers": tickers,
		},
	})
}

func subscribeTrade() ([]byte, error) {
	return json.Marshal(map[string]any{
		"id": idSubscribeTrade, "cmd": "subscribe",
		"params": map[string]any{"channels": []string{"trade"}},
	})
}

// resnapshotRequest asks the exchange to re-send a book snapshot on exactly
// one orderbook_delta subscription. Trade subscriptions cannot be snapshotted.
func resnapshotRequest(sids []int64, tickers []string) ([]byte, error) {
	if err := ValidateTickers(tickers); err != nil {
		return nil, err
	}
	if len(sids) != 1 || sids[0] <= 0 {
		return nil, fmt.Errorf("get_snapshot requires exactly one positive orderbook subscription ID")
	}
	return json.Marshal(map[string]any{
		"id": idResnapshot, "cmd": "update_subscription",
		"params": map[string]any{
			"sids":           sids,
			"action":         "get_snapshot",
			"market_tickers": tickers,
		},
	})
}

// ValidateTickers rejects the two subscription arguments that fail silently.
//
// An empty list subscribes to nothing and the exchange acknowledges it happily,
// so the socket comes up, the pings answer, the gate waits forever for a
// snapshot that was never going to arrive, and every market stays
// non-actionable with no error anywhere. A duplicate ticker is subscribed
// twice: every delta for it arrives twice, and `core.Rig` applies both, so the
// book is silently wrong in the direction of double depth.
//
// Order is preserved by the caller and asserted here only for the properties
// above; the exchange does not care, but a stable order makes the request
// byte-comparable in tests.
func ValidateTickers(tickers []string) error {
	if len(tickers) == 0 {
		return fmt.Errorf("no market tickers: an empty subscription is " +
			"accepted by the exchange and delivers nothing, which is " +
			"indistinguishable from a quiet market")
	}
	seen := make(map[string]struct{}, len(tickers))
	for i, t := range tickers {
		if t == "" {
			return fmt.Errorf("market ticker %d is empty", i)
		}
		if _, dup := seen[t]; dup {
			return fmt.Errorf("market ticker %q appears more than once; the "+
				"exchange would deliver each of its deltas twice and the book "+
				"would carry double depth with no error anywhere", t)
		}
		seen[t] = struct{}{}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Inbound frame shapes
// ---------------------------------------------------------------------------
//
// Only the fields this package needs to CLASSIFY and VALIDATE a frame are
// decoded here. Everything else stays as raw bytes and is handed to
// `core.Rig.Handle` unmodified: `core` is the differentially tested surface,
// and re-deriving a book field here would put a second, untested decoder on
// the path that matters.

type envelope struct {
	Type string          `json:"type"`
	Sid  *int64          `json:"sid"`
	Seq  *int64          `json:"seq"`
	Msg  json.RawMessage `json:"msg"`
}

type snapshotMsg struct {
	MarketTicker string      `json:"market_ticker"`
	YesLevels    [][2]string `json:"yes_dollars_fp"`
	NoLevels     [][2]string `json:"no_dollars_fp"`
}

type deltaMsg struct {
	MarketTicker string  `json:"market_ticker"`
	PriceDollars *string `json:"price_dollars"`
}

type tradeMsg struct {
	MarketTicker *string `json:"market_ticker"`
}

// confidence: high
