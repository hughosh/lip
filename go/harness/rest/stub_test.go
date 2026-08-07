package rest

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
)

// scriptedDoer is the scenario exchange's ancestor: a Doer that answers from a
// script rather than from a network.
//
// It is deliberately a *transport*, not a fake exchange API. Every guard in
// this package is a guard against something that arrives as bytes and a status
// code — a rewound page that is HTTP 200, a 409 that means "your order landed",
// a response that never arrives at all. A fake that returned typed values would
// make all of them untestable, and pilot-plan §3's scenario exchange plugs in
// at exactly this seam.
type scriptedDoer struct {
	t *testing.T
	// handle answers request number n (0-based). Returning a non-nil error is
	// how a scenario drops a response — the ambiguous outcome of H-ORD-2.
	handle func(n int, req Request) (Response, error)
	calls  []Request
}

func (d *scriptedDoer) Do(_ context.Context, req Request) (Response, error) {
	n := len(d.calls)
	d.calls = append(d.calls, req)
	if d.handle == nil {
		d.t.Fatalf("scriptedDoer got an unscripted call: %+v", req)
	}
	return d.handle(n, req)
}

// Calls returns the requests received so far.
func (d *scriptedDoer) Calls() []Request { return d.calls }

// jsonPage builds an HTTP 200 page body for `ep` from an item map and a cursor.
// The cursor is written under the endpoint's OWN field name, so a test that
// mis-declares the endpoint reproduces M22 exactly.
//
// It fills in every declared item array the caller did not supply with an EMPTY
// array, because that is the measured shape: every page V1.8a sampled carried
// its cursor key and all of its declared arrays, e.g. positions' terminal page
// was exactly {cursor, event_positions, market_positions}. A fixture that
// omitted one would be testing against a response the exchange does not send,
// and `decodePage` now rejects that shape as malformed.
//
// A test that WANTS a malformed page builds the body literally instead.
func jsonPage(ep Endpoint, cursor string, items map[string][]any) Response {
	body := map[string]any{ep.CursorField: cursor}
	for _, k := range ep.ItemKeys {
		body[k] = []any{}
	}
	for k, v := range items {
		body[k] = v
	}
	raw, err := json.Marshal(body)
	if err != nil {
		panic(err)
	}
	return Response{Status: 200, Body: raw}
}

// order is a minimal resting-order record shaped like the real payload.
func order(id, coid, ticker, side string, price float64, count string) map[string]any {
	return map[string]any{
		"order_id":          id,
		"client_order_id":   coid,
		"ticker":            ticker,
		"side":              side,
		"yes_price_dollars": fmt.Sprintf("%.4f", price),
		"remaining_count":   count,
		"status":            StatusResting,
	}
}

// fill is a minimal fill record carrying both identities V1.8 confirmed.
func fill(fillID, tradeID, orderID, ticker string, isTaker bool) map[string]any {
	return map[string]any{
		"fill_id":           fillID,
		"trade_id":          tradeID,
		"order_id":          orderID,
		"ticker":            ticker,
		"market_ticker":     ticker,
		"is_taker":          isTaker,
		"count_fp":          "1.00",
		"fee_cost":          "0.00",
		"side":              "yes",
		"yes_price_dollars": "0.5800",
	}
}

// position is a minimal market_positions record.
func position(ticker string, qty string) map[string]any {
	return map[string]any{"ticker": ticker, "position": qty}
}
