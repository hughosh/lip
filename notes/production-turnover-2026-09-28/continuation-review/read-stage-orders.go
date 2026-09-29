// Read the three actual first-stage order IDs. GET-only; no writer configuration.
// Endpoint reference: https://docs.kalshi.com/api-reference/orders/get-order
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"lip/feed"
	"lip/harness/rest"
	"os"
	"path/filepath"
	"time"
)

func main() {
	out := flag.String("out", "", "new absolute evidence file")
	flag.Parse()
	if !filepath.IsAbs(*out) {
		fmt.Fprintln(os.Stderr, "absolute -out required")
		os.Exit(2)
	}
	file, err := os.OpenFile(*out, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		fmt.Fprintln(os.Stderr, "output unavailable")
		os.Exit(1)
	}
	defer file.Close()
	signer, err := feed.NewSigner()
	if err != nil {
		fmt.Fprintln(os.Stderr, "credentials unavailable")
		os.Exit(1)
	}
	client := rest.NewHTTPDoer(signer, 15*time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	rows := []map[string]any{}
	ok := true
	for _, id := range []string{"01a0e981-b150-714a-b910-47ca43522e5b", "01a0e981-b150-7a9c-81d5-c6fcc7baa657", "01a0e9ac-14e0-7144-9953-d1f111fe220d"} {
		path := "/portfolio/orders/" + id
		row := map[string]any{"at_utc": time.Now().UTC().Format(time.RFC3339Nano), "method": "GET", "path": path, "requested_order_id": id}
		response, callErr := client.Do(ctx, rest.Request{Method: "GET", Path: path})
		row["http_status"] = response.Status
		var body struct {
			Order map[string]json.RawMessage `json:"order"`
		}
		if callErr != nil || response.Status != 200 || json.Unmarshal(response.Body, &body) != nil || body.Order == nil {
			row["outcome"] = "failed"
			ok = false
		} else {
			var actualID, ticker string
			json.Unmarshal(body.Order["order_id"], &actualID)
			json.Unmarshal(body.Order["ticker"], &ticker)
			if actualID != id || ticker != "KXBROSFT-26OCT08-T110" {
				row["outcome"] = "identity_mismatch"
				ok = false
			} else {
				row["outcome"] = "complete"
			}
			order := map[string]json.RawMessage{}
			for _, key := range []string{"order_id", "client_order_id", "ticker", "outcome_side", "book_side", "type", "status", "yes_price_dollars", "no_price_dollars", "fill_count_fp", "remaining_count_fp", "initial_count_fp", "taker_fill_cost_dollars", "maker_fill_cost_dollars", "taker_fees_dollars", "maker_fees_dollars", "side", "action", "created_time", "last_update_time", "self_trade_prevention_type", "cancel_order_on_pause", "subaccount_number", "exchange_index", "post_only"} {
				if value, exists := body.Order[key]; exists {
					order[key] = value
				}
			}
			row["order"] = order
		}
		rows = append(rows, row)
	}
	encoder := json.NewEncoder(file)
	encoder.SetIndent("", "  ")
	if encoder.Encode(map[string]any{"read_only": true, "scope": "three named owned orders only; not an account-wide flatness read", "complete": ok, "ended_at_utc": time.Now().UTC().Format(time.RFC3339Nano), "orders": rows}) != nil {
		os.Exit(1)
	}
	if err := file.Sync(); err != nil {
		os.Exit(1)
	}
	if !ok {
		fmt.Fprintln(os.Stderr, "order read incomplete; receipt retained")
		os.Exit(1)
	}
	fmt.Println("Three named order GET receipts written")
}
