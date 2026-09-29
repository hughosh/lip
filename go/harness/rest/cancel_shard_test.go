package rest

import (
	"context"
	"strings"
	"testing"
)

// shardAwareCancelDoer models the documented routing default: order_id alone
// reaches shard 0, while market_ticker auto-routes to the market's shard.
type shardAwareCancelDoer struct {
	t            *testing.T
	orderResting bool
	ignoreFirst  bool
	deleteCalls  int
	verifyReads  int
	routedShards []int
}

func (d *shardAwareCancelDoer) Do(_ context.Context, req Request) (Response, error) {
	switch req.Method {
	case "DELETE":
		d.deleteCalls++
		if req.Query.Has("exchange_index") {
			d.t.Fatalf("cancel set exchange_index=%q; expected ticker-based "+
				"auto-routing with that parameter omitted", req.Query.Get("exchange_index"))
		}
		shard := 0
		if ticker := req.Query.Get("market_ticker"); ticker != "" {
			if ticker != "KXTENNIS-26SEP26-PLAYERA-PLAYERB" {
				d.t.Fatalf("cancel routed using unexpected market_ticker %q", ticker)
			}
			shard = 3
		}
		d.routedShards = append(d.routedShards, shard)
		if shard == 3 {
			if !d.ignoreFirst || d.deleteCalls > 1 {
				d.orderResting = false
			}
			return Response{Status: 200,
				Body: cancelBody("order-1", "lipH-run1-000-yes-00000001", "1.00")}, nil
		}
		// The targeted order belongs to shard 3. A shard-0 lookup does not
		// remove it; preserve that fact so the sweep's complete read can see it.
		return Response{Status: 404,
			Body: []byte(`{"error":{"code":"order_not_found"}}`)}, nil
	case "GET":
		d.verifyReads++
		if req.Query.Get("ticker") != "KXTENNIS-26SEP26-PLAYERA-PLAYERB" {
			d.t.Fatalf("verifying walk ticker = %q, want target market",
				req.Query.Get("ticker"))
		}
		if d.orderResting {
			return jsonPage(EpOrders, "", map[string][]any{
				"orders": {order("order-1", "lipH-run1-000-yes-00000001",
					"KXTENNIS-26SEP26-PLAYERA-PLAYERB", "yes", 0.58, "1.00")},
			}), nil
		}
		return jsonPage(EpOrders, "", map[string][]any{"orders": {}}), nil
	default:
		d.t.Fatalf("unexpected request: %s %s", req.Method, req.Path)
		return Response{}, nil
	}
}

func TestCancelSweepAutoRoutesByMarketAndVerifiesAbsence(t *testing.T) {
	const ticker = "KXTENNIS-26SEP26-PLAYERA-PLAYERB"
	d := &shardAwareCancelDoer{t: t, orderResting: true, ignoreFirst: true}
	c := NewClient(d)

	res := c.CancelAndSweep(context.Background(), ticker,
		[]Order{{OrderID: "order-1", Ticker: ticker, Ours: true}})

	if !res.Clean || !res.Replaces() {
		t.Fatalf("the market-routed cancel must be confirmed absent by a complete "+
			"orders walk: clean=%v outcome=%s err=%v still=%+v",
			res.Clean, res.Outcome, res.Err, res.StillResting)
	}
	if d.deleteCalls != 2 || d.verifyReads != 2 || res.Rounds != 2 {
		t.Fatalf("sweep used %d DELETE(s), %d verifying read(s), %d round(s); "+
			"want cancel→complete read that still sees the order→routed retry→"+
			"complete absent read", d.deleteCalls, d.verifyReads, res.Rounds)
	}
	if len(d.routedShards) != 2 || d.routedShards[0] != 3 || d.routedShards[1] != 3 {
		t.Fatalf("cancel routed to shards %v, want both attempts on shard 3 via "+
			"market_ticker; an order_id-only DELETE defaults to shard 0 and leaves "+
			"this order live", d.routedShards)
	}
	if len(res.Cancels) != 2 || res.Cancels[0].Outcome != CancelAccepted ||
		res.Cancels[1].Outcome != CancelAccepted {
		t.Fatalf("cancel results = %+v, want two accepted DELETEs", res.Cancels)
	}
}

func TestCancelForMarketRefusesMissingOrInvalidTickerBeforeWrite(t *testing.T) {
	for _, ticker := range []string{"", "T1/../T2", "T1?exchange_index=0"} {
		t.Run(strings.ReplaceAll(ticker, "/", "_"), func(t *testing.T) {
			d := &shardAwareCancelDoer{t: t, orderResting: true}
			res := NewClient(d).cancelForMarket(context.Background(), ticker, "order-1")
			if res.Outcome != CancelRejected || res.Sent || res.Err == nil {
				t.Fatalf("invalid routed cancel result = %+v; want local rejection", res)
			}
			if d.deleteCalls != 0 {
				t.Fatalf("invalid ticker reached the transport %d time(s)", d.deleteCalls)
			}
		})
	}
}
