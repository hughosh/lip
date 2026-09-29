package rest

import (
	"context"
	"fmt"
	"reflect"
	"testing"
	"time"
)

func traced(c *Client) *[]SweepTrace {
	var got []SweepTrace
	c.SweepTrace = func(t SweepTrace) { got = append(got, t) }
	return &got
}

// The trace keeps exactly what lip-kaf found missing: the requested ids, each
// DELETE's answer, each verifying read's timing and contents, and the named
// read that decided the verdict.
func TestSweepTraceRecordsEveryDeleteReadAndNamedRead(t *testing.T) {
	x := newLagExchange(t, 3)
	c := NewClient(x)
	got := traced(c)

	res := c.CancelAndSweep(context.Background(), lagTicker, lagRequest(x, "oY"))
	if len(*got) != 1 {
		t.Fatalf("%d traces for one sweep, want 1", len(*got))
	}
	tr := (*got)[0]

	if tr.Ticker != lagTicker || !reflect.DeepEqual(tr.Requested, []string{"oY"}) {
		t.Fatalf("ticker %q requested %v", tr.Ticker, tr.Requested)
	}
	if tr.Verdict != "confirmed" || !res.Clean {
		t.Fatalf("verdict %q clean %v", tr.Verdict, res.Clean)
	}
	if len(tr.Rounds) != 2 {
		t.Fatalf("%d rounds traced, want 2", len(tr.Rounds))
	}
	first, retry := tr.Rounds[0], tr.Rounds[1]
	if len(first.Deletes) != 1 || first.Deletes[0].Status != 200 ||
		first.Deletes[0].Outcome != "ACCEPTED" ||
		first.Deletes[0].ReducedBy != "12.00" || !first.Deletes[0].Sent {
		t.Fatalf("first DELETE %+v", first.Deletes)
	}
	if len(retry.Deletes) != 1 || retry.Deletes[0].Status != 404 ||
		retry.Deletes[0].Outcome != "GONE" {
		t.Fatalf("retry DELETE %+v", retry.Deletes)
	}
	for i, r := range tr.Rounds {
		if r.Read.Outcome != "complete" || r.Read.Listed != 2 {
			t.Fatalf("round %d read %+v", i, r.Read)
		}
		var sawStale bool
		for _, o := range r.Read.Orders {
			if o.OrderID == "oY" {
				// The listing is the order as it rested: its update time is its
				// creation, not the cancel. That is what shows the list stale.
				sawStale = o.Requested && o.Status == StatusResting &&
					o.Remaining == "12.00" && o.LastUpdate == x.find("oY").created
			}
			if o.OrderID == "oN" && (o.Requested || !o.Ours) {
				t.Fatalf("round %d: oN traced as %+v", i, o)
			}
		}
		if !sawStale {
			t.Fatalf("round %d did not keep oY's stale listing: %+v", i,
				r.Read.Orders)
		}
	}
	if len(tr.Confirms) != 1 {
		t.Fatalf("confirms %+v", tr.Confirms)
	}
	cf := tr.Confirms[0]
	if cf.OrderID != "oY" || cf.Status != 200 || !cf.Retired || cf.Record == nil ||
		cf.Record.Status != StatusCanceled || cf.Record.Remaining != "0.00" ||
		cf.Record.LastUpdate != lagCancelledAt {
		t.Fatalf("confirm %+v record %+v", cf, cf.Record)
	}
	if len(tr.StillResting) != 0 || !reflect.DeepEqual(tr.OtherOurs, []string{"oN"}) {
		t.Fatalf("still %v other %v", tr.StillResting, tr.OtherOurs)
	}

	// Wall times, in order and in UTC, so they compare with the exchange's.
	seq := []time.Time{tr.Started}
	for _, r := range tr.Rounds {
		for _, d := range r.Deletes {
			seq = append(seq, d.Started, d.Ended)
		}
		seq = append(seq, r.Read.Started, r.Read.Ended)
	}
	seq = append(seq, cf.Started, cf.Ended, tr.Ended)
	for i, ts := range seq {
		if ts.IsZero() || ts.Location() != time.UTC {
			t.Fatalf("time %d is %v", i, ts)
		}
		if i > 0 && ts.Before(seq[i-1]) {
			t.Fatalf("time %d (%v) precedes time %d (%v)", i, ts, i-1, seq[i-1])
		}
	}
}

// Every way a sweep ends is traced exactly once, with its own verdict.
func TestSweepTraceVerdictOnEveryReturn(t *testing.T) {
	cases := map[string]struct {
		setup   func(x *lagExchange) Doer
		verdict string
		rounds  int
	}{
		"clean": {setup: func(x *lagExchange) Doer { return x },
			verdict: "clean", rounds: 1},
		"incomplete": {setup: func(x *lagExchange) Doer {
			x.ignoreDeletes = true
			return x
		}, verdict: "incomplete", rounds: 2},
		"throttled": {setup: func(x *lagExchange) Doer {
			return doerFunc(func(ctx context.Context, req Request) (Response, error) {
				if req.Method == "DELETE" {
					return Response{Status: 429,
						Body: []byte(`{"error":{"code":"too_many_requests"}}`)}, nil
				}
				return x.Do(ctx, req)
			})
		}, verdict: "throttled", rounds: 1},
		"unverified": {setup: func(x *lagExchange) Doer {
			return doerFunc(func(ctx context.Context, req Request) (Response, error) {
				if req.Method == "GET" && req.Path == EpOrders.Path {
					return Response{Status: 500, Body: []byte(`{}`)}, nil
				}
				return x.Do(ctx, req)
			})
		}, verdict: "unverified", rounds: 1},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			x := newLagExchange(t, 0)
			c := NewClient(tc.setup(x))
			got := traced(c)
			c.CancelAndSweep(context.Background(), lagTicker, lagRequest(x, "oY"))
			if len(*got) != 1 {
				t.Fatalf("%d traces, want 1", len(*got))
			}
			if tr := (*got)[0]; tr.Verdict != tc.verdict || len(tr.Rounds) != tc.rounds {
				t.Fatalf("verdict %q rounds %d, want %q %d", tr.Verdict,
					len(tr.Rounds), tc.verdict, tc.rounds)
			}
		})
	}
}

// The trace observes. The same exchange gives the same result with or without
// it.
func TestSweepTraceDoesNotChangeTheResult(t *testing.T) {
	for _, lag := range []int{0, 1, 3} {
		for _, persist := range []bool{false, true} {
			run := func(withTrace bool) SweepResult {
				x := newLagExchange(t, lag)
				x.ignoreDeletes = persist
				c := NewClient(x)
				if withTrace {
					traced(c)
				}
				return c.CancelAndSweep(context.Background(), lagTicker,
					lagRequest(x, "oY"))
			}
			plain, withTrace := run(false), run(true)
			if !reflect.DeepEqual(plain, withTrace) {
				t.Fatalf("lag %d persist %v: result differs with the trace on:\n"+
					"%+v\n%+v", lag, persist, plain, withTrace)
			}
		}
	}
}

// Each read keeps at most traceOrderCap records but counts them all.
func TestSweepTraceIsBounded(t *testing.T) {
	x := newLagExchange(t, 0)
	for i := 0; i < traceOrderCap+8; i++ {
		x.orders = append(x.orders, &lagOrder{
			id: fmt.Sprintf("o%03d", i), coid: fmt.Sprintf("lipH-run1-000-yes-%08d", 100+i),
			side: "yes", yes4: "0.2000", no4: "0.8000", remaining: "1.00",
			status: StatusResting, created: "2026-09-28T19:00:00Z",
			updated: "2026-09-28T19:00:00Z"})
	}
	c := NewClient(x)
	got := traced(c)
	c.CancelAndSweep(context.Background(), lagTicker, lagRequest(x, "oY"))
	read := (*got)[0].Rounds[0].Read
	if len(read.Orders) != traceOrderCap || read.Listed != traceOrderCap+8+1 {
		t.Fatalf("kept %d of %d listed", len(read.Orders), read.Listed)
	}
}

type doerFunc func(context.Context, Request) (Response, error)

func (f doerFunc) Do(ctx context.Context, req Request) (Response, error) {
	return f(ctx, req)
}
