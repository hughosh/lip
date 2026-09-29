package rest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"lip/harness/risk"
)

// lip-kaf. The 2026-09-28 first stage raised six SEV1 SWEEP_INCOMPLETE records,
// and every one of them came 0.27-1.2 s AFTER the exchange's own terminal time
// for the order it named. Three consecutive YES sweeps, finishing 0.28, 0.68 and
// 1.08 s after that order's `last_update_time`, still found it in a complete
// `status=resting` read. The DELETE had worked; the resting LIST had not caught
// up. Each sweep's two rounds take about 0.4 s, so a list that lags a cancel by
// longer than that fails every sweep.
//
// These tests pin both halves. An order the exchange reports terminal, as its
// own record, is not "live and fillable" and must not page SEV1 (H-FAIL-3:
// confirmed gone "by response or by sweep"). And nothing short of that report
// -- absence from a named read, a 404, an error, a record for another order, a
// resting record -- may retire a requested order the list still shows.

const lagTicker = "KXLAG-26OCT08-T110"

// lagOrder is the exchange's own record for one order.
type lagOrder struct {
	id, coid, side string
	yes4, no4      string
	remaining      string // while it rested
	status         string
	created        string
	updated        string
	// staleReads is how many further `status=resting` list reads still show
	// this order after the exchange cancelled it.
	staleReads int
}

func (o *lagOrder) record(ticker, status, remaining, updated string) map[string]any {
	book := string(Bid)
	if o.side == "no" {
		book = string(Ask)
	}
	return map[string]any{
		"order_id":           o.id,
		"client_order_id":    o.coid,
		"ticker":             ticker,
		"side":               "yes",
		"book_side":          book,
		"outcome_side":       o.side,
		"yes_price_dollars":  o.yes4,
		"no_price_dollars":   o.no4,
		"remaining_count_fp": remaining,
		"status":             status,
		"created_time":       o.created,
		"last_update_time":   updated,
	}
}

// lagExchange is a stateful exchange whose resting-orders LIST lags its order
// records. A cancelled order keeps appearing in `status=resting` reads for
// `lag` further reads; a named-order GET answers from the record itself.
type lagExchange struct {
	t      *testing.T
	orders []*lagOrder
	// lag is the staleReads a successful cancel installs.
	lag int
	// staleStatus renders a stale listing with the record's CURRENT status (the
	// filter lagged, the record did not). Otherwise it is rendered exactly as it
	// rested.
	staleStatus bool
	// ignoreDeletes answers every DELETE 200 and cancels nothing: the order
	// really does persist.
	ignoreDeletes bool
	// named overrides the named-order GET answer.
	named func(id string) (Response, error)

	deletes []string
	gets    []string
	lists   int
}

const lagCancelledAt = "2026-09-28T20:19:22.315385Z"

func newLagExchange(t *testing.T, lag int) *lagExchange {
	return &lagExchange{t: t, lag: lag, orders: []*lagOrder{
		{id: "oY", coid: "lipH-run1-000-yes-00000001", side: "yes",
			yes4: "0.2300", no4: "0.7700", remaining: "12.00",
			status: StatusResting, created: "2026-09-28T19:33:06.843172Z",
			updated: "2026-09-28T19:33:06.843172Z"},
		{id: "oN", coid: "lipH-run1-000-no-00000002", side: "no",
			yes4: "0.6100", no4: "0.3900", remaining: "12.00",
			status: StatusResting, created: "2026-09-28T19:33:06.942929Z",
			updated: "2026-09-28T19:33:06.942929Z"},
	}}
}

func (x *lagExchange) find(id string) *lagOrder {
	for _, o := range x.orders {
		if o.id == id {
			return o
		}
	}
	return nil
}

// fresh is the named-order GET body for o, from the record itself.
func (x *lagExchange) fresh(o *lagOrder) Response {
	rem := o.remaining
	if o.status != StatusResting {
		rem = "0.00"
	}
	body, err := json.Marshal(map[string]any{
		"order": o.record(lagTicker, o.status, rem, o.updated)})
	if err != nil {
		panic(err)
	}
	return Response{Status: 200, Body: body}
}

func (x *lagExchange) Do(_ context.Context, req Request) (Response, error) {
	switch {
	case req.Method == "DELETE" &&
		strings.HasPrefix(req.Path, "/portfolio/events/orders/"):
		id := strings.TrimPrefix(req.Path, "/portfolio/events/orders/")
		if got := req.Query.Get("market_ticker"); got != lagTicker {
			x.t.Errorf("DELETE %s routed by market_ticker %q", id, got)
		}
		x.deletes = append(x.deletes, id)
		o := x.find(id)
		if o == nil || o.status != StatusResting {
			return Response{Status: 404,
				Body: []byte(`{"error":{"code":"not_found"}}`)}, nil
		}
		if x.ignoreDeletes {
			return Response{Status: 200, Body: cancelBody(id, o.coid, "0.00")}, nil
		}
		reduced := o.remaining
		o.status, o.updated, o.staleReads = StatusCanceled, lagCancelledAt, x.lag
		return Response{Status: 200, Body: cancelBody(id, o.coid, reduced)}, nil

	case req.Method == "GET" && req.Path == EpOrders.Path:
		if req.Query.Get("status") != StatusResting ||
			req.Query.Get("ticker") != lagTicker {
			x.t.Errorf("unexpected orders filter %v", req.Query)
		}
		x.lists++
		items := []any{}
		for _, o := range x.orders {
			switch {
			case o.status == StatusResting:
				items = append(items,
					o.record(lagTicker, StatusResting, o.remaining, o.updated))
			case o.staleReads > 0:
				o.staleReads--
				if x.staleStatus {
					items = append(items,
						o.record(lagTicker, o.status, "0.00", o.updated))
				} else {
					items = append(items,
						o.record(lagTicker, StatusResting, o.remaining, o.created))
				}
			}
		}
		return jsonPage(EpOrders, "", map[string][]any{"orders": items}), nil

	case req.Method == "GET" && strings.HasPrefix(req.Path, EpOrders.Path+"/"):
		id := strings.TrimPrefix(req.Path, EpOrders.Path+"/")
		x.gets = append(x.gets, id)
		if x.named != nil {
			return x.named(id)
		}
		o := x.find(id)
		if o == nil {
			return Response{Status: 404,
				Body: []byte(`{"error":{"code":"not_found"}}`)}, nil
		}
		return x.fresh(o), nil
	}
	x.t.Errorf("lagExchange got an unscripted %s %s", req.Method, req.Path)
	return Response{}, fmt.Errorf("unscripted %s %s", req.Method, req.Path)
}

func lagRequest(x *lagExchange, id string) []Order {
	o := x.find(id)
	return []Order{{OrderID: o.id, ClientOrderID: o.coid, Ticker: lagTicker,
		Ours: true}}
}

func sweepSEV1(res SweepResult) bool {
	for _, a := range res.Anomalies {
		if a.Class == "SWEEP_INCOMPLETE" && a.Sev == risk.SEV1 {
			return true
		}
	}
	return false
}

// sweepPendings counts the non-paging SEV3 SWEEP_PENDING anomalies.
func sweepPendings(res SweepResult) int {
	n := 0
	for _, a := range res.Anomalies {
		if a.Class == "SWEEP_PENDING" {
			if a.Sev != risk.SEV3 {
				panic(fmt.Sprintf("SWEEP_PENDING at %v, want SEV3", a.Sev))
			}
			n++
		}
	}
	return n
}

// sweepClock is a deterministic clock for the page bound.
type sweepClock struct{ now time.Time }

func (k *sweepClock) Now() time.Time          { return k.now }
func (k *sweepClock) advance(d time.Duration) { k.now = k.now.Add(d) }

func clockedClient(d Doer) (*Client, *sweepClock) {
	k := &sweepClock{now: time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)}
	c := NewClient(d)
	c.Now = k.Now
	return c, k
}

func ids(orders []Order) []string {
	out := make([]string, 0, len(orders))
	for _, o := range orders {
		out = append(out, o.OrderID)
	}
	return out
}

// onlyNamed fails when a DELETE or named GET reached an order outside `want`.
func onlyNamed(t *testing.T, what string, got []string, want ...string) {
	t.Helper()
	allowed := map[string]bool{}
	for _, w := range want {
		allowed[w] = true
	}
	for _, id := range got {
		if !allowed[id] {
			t.Fatalf("%s reached %s, which this sweep was not asked to cancel "+
				"(got %v); the opposite side's order is kept alive and "+
				"reported, never cancelled or re-examined (A4, M2)", what, id, got)
		}
	}
}

// The six-SEV1 reproducer, two-sided: a market stop cancels YES, then NO, while
// the list still shows each cancelled order for longer than a sweep lasts.
func TestSweepRetiresAListLaggedCancelOnTheExchangesTerminalAnswer(t *testing.T) {
	x := newLagExchange(t, 3)
	c := NewClient(x)

	yes := c.CancelAndSweep(context.Background(), lagTicker, lagRequest(x, "oY"))
	if sweepSEV1(yes) || !yes.Clean {
		t.Fatalf("the exchange's own record says oY is %s (updated %s), yet the "+
			"sweep reported clean=%v, still resting %v, SEV1 %v.\n\n"+
			"The complete list still showed oY because it lags the cancel "+
			"(lip-kaf: six such SEV1s on 2026-09-28, each 0.27-1.2 s after the "+
			"order's terminal time). An order the exchange reports cancelled "+
			"is not live and fillable (H-FAIL-3)",
			x.find("oY").status, x.find("oY").updated, yes.Clean,
			ids(yes.StillResting), sweepSEV1(yes))
	}
	if got := ids(yes.OtherOurs); len(got) != 1 || got[0] != "oN" {
		t.Fatalf("the NO order still rests and must be reported, got %v", got)
	}
	onlyNamed(t, "a YES-sweep DELETE", x.deletes, "oY")
	onlyNamed(t, "a YES-sweep named read", x.gets, "oY")
	if len(x.gets) > 1 {
		t.Fatalf("oY was read by name %d times in one sweep, want at most one",
			len(x.gets))
	}

	// The NO side, while oY is still in the lagging list.
	x.deletes, x.gets = nil, nil
	no := c.CancelAndSweep(context.Background(), lagTicker, lagRequest(x, "oN"))
	if sweepSEV1(no) || !no.Clean {
		t.Fatalf("NO sweep: clean=%v still=%v SEV1=%v, although the exchange "+
			"reports oN %s", no.Clean, ids(no.StillResting), sweepSEV1(no),
			x.find("oN").status)
	}
	onlyNamed(t, "a NO-sweep DELETE", x.deletes, "oN")
	onlyNamed(t, "a NO-sweep named read", x.gets, "oN")

	// Clearance: once the list catches up it shows neither.
	x.lag = 0
	for _, o := range x.orders {
		o.staleReads = 0
	}
	read := c.Orders(context.Background(), lagTicker, StatusResting)
	if !read.Replaces() || len(read.Orders) != 0 {
		t.Fatalf("after the lag the complete list should be empty: %v %v",
			read.Outcome, ids(read.Orders))
	}
}

// A lag inside the sweep's own retry needs no named read: the retry's list is
// already current.
func TestSweepListLagShorterThanTheRetryNeedsNoNamedRead(t *testing.T) {
	x := newLagExchange(t, 1)
	res := NewClient(x).CancelAndSweep(context.Background(), lagTicker,
		lagRequest(x, "oY"))
	if !res.Clean || sweepSEV1(res) {
		t.Fatalf("clean=%v SEV1=%v", res.Clean, sweepSEV1(res))
	}
	if len(x.gets) != 0 {
		t.Fatalf("named reads %v; the retry's complete list already showed oY "+
			"gone", x.gets)
	}
	if res.Rounds != 2 {
		t.Fatalf("rounds %d, want 2", res.Rounds)
	}
}

// The filter lagged but the record did not: the listed record itself says
// cancelled. That is the exchange's own terminal answer about that order.
func TestSweepListedTerminalRecordIsNotResting(t *testing.T) {
	x := newLagExchange(t, 5)
	x.staleStatus = true
	res := NewClient(x).CancelAndSweep(context.Background(), lagTicker,
		lagRequest(x, "oY"))
	if !res.Clean || sweepSEV1(res) {
		t.Fatalf("the requested order was listed with status %q, which is not "+
			"resting; clean=%v still=%v SEV1=%v", StatusCanceled, res.Clean,
			ids(res.StillResting), sweepSEV1(res))
	}
	if len(x.gets) != 0 {
		t.Fatalf("named reads %v for an order whose own listed record is "+
			"already terminal", x.gets)
	}
}

// Persistence control: the exchange keeps the order resting. Every read says
// so, and the sweep must still page SEV1.
func TestSweepEscalatesAnOrderTheExchangeStillCallsResting(t *testing.T) {
	x := newLagExchange(t, 0)
	x.ignoreDeletes = true
	c, clk := clockedClient(x)
	first := c.CancelAndSweep(context.Background(), lagTicker, lagRequest(x, "oY"))
	if first.Clean || sweepSEV1(first) || sweepPendings(first) != 1 {
		t.Fatalf("first sweep: clean=%v SEV1=%v pending=%d", first.Clean,
			sweepSEV1(first), sweepPendings(first))
	}
	clk.advance(sweepPageBound)
	res := c.CancelAndSweep(context.Background(), lagTicker, lagRequest(x, "oY"))
	if res.Clean || !sweepSEV1(res) {
		t.Fatalf("oY really is resting; clean=%v SEV1=%v", res.Clean,
			sweepSEV1(res))
	}
	if got := ids(res.StillResting); len(got) != 1 || got[0] != "oY" {
		t.Fatalf("still resting %v, want [oY]", got)
	}
	onlyNamed(t, "a DELETE", x.deletes, "oY")
	onlyNamed(t, "a named read", x.gets, "oY")
}

// Terminal statuses the exchange uses for an order that no longer rests.
func TestSweepConfirmationAcceptsTheExchangesTerminalStatuses(t *testing.T) {
	for _, status := range []string{StatusCanceled, StatusExecuted} {
		t.Run(status, func(t *testing.T) {
			x := newLagExchange(t, 5)
			x.named = func(id string) (Response, error) {
				o := x.find(id)
				body, _ := json.Marshal(map[string]any{
					"order": o.record(lagTicker, status, "0.00", lagCancelledAt)})
				return Response{Status: 200, Body: body}, nil
			}
			res := NewClient(x).CancelAndSweep(context.Background(), lagTicker,
				lagRequest(x, "oY"))
			if !res.Clean || sweepSEV1(res) {
				t.Fatalf("a named %s record with nothing remaining is the "+
					"exchange confirming the order gone; clean=%v SEV1=%v",
					status, res.Clean, sweepSEV1(res))
			}
		})
	}
}

// Nothing but a terminal answer for THAT order in THIS market retires it.
func TestSweepConfirmationAcceptsOnlyATerminalAnswerForThatOrder(t *testing.T) {
	record := func(x *lagExchange, mutate func(map[string]any)) func(string) (Response, error) {
		return func(id string) (Response, error) {
			rec := x.find(id).record(lagTicker, StatusCanceled, "0.00", lagCancelledAt)
			mutate(rec)
			body, _ := json.Marshal(map[string]any{"order": rec})
			return Response{Status: 200, Body: body}, nil
		}
	}
	status := func(code int, body string) func(*lagExchange) func(string) (Response, error) {
		return func(*lagExchange) func(string) (Response, error) {
			return func(string) (Response, error) {
				return Response{Status: code, Body: []byte(body)}, nil
			}
		}
	}
	cases := map[string]func(*lagExchange) func(string) (Response, error){
		"404":          status(404, `{"error":{"code":"not_found"}}`),
		"500":          status(500, `{"error":{"code":"internal"}}`),
		"429":          status(429, `{"error":{"code":"too_many_requests"}}`),
		"undecodable":  status(200, `not json`),
		"null order":   status(200, `{"order":null}`),
		"no order key": status(200, `{"cursor":"","orders":[]}`),
		"transport error": func(*lagExchange) func(string) (Response, error) {
			return func(string) (Response, error) {
				return Response{}, errors.New("connection reset by peer")
			}
		},
		"resting": func(x *lagExchange) func(string) (Response, error) {
			return record(x, func(r map[string]any) {
				r["status"], r["remaining_count_fp"] = StatusResting, "12.00"
			})
		},
		"cancelled with quantity remaining": func(x *lagExchange) func(string) (Response, error) {
			return record(x, func(r map[string]any) { r["remaining_count_fp"] = "3.00" })
		},
		"no remaining field": func(x *lagExchange) func(string) (Response, error) {
			return record(x, func(r map[string]any) { delete(r, "remaining_count_fp") })
		},
		"another order": func(x *lagExchange) func(string) (Response, error) {
			return record(x, func(r map[string]any) { r["order_id"] = "oOther" })
		},
		"another market": func(x *lagExchange) func(string) (Response, error) {
			return record(x, func(r map[string]any) { r["ticker"] = "KXOTHER-26OCT08-T1" })
		},
		"empty status": func(x *lagExchange) func(string) (Response, error) {
			return record(x, func(r map[string]any) { r["status"] = "" })
		},
		"unknown status": func(x *lagExchange) func(string) (Response, error) {
			return record(x, func(r map[string]any) { r["status"] = "pending" })
		},
		"misspelt status": func(x *lagExchange) func(string) (Response, error) {
			return record(x, func(r map[string]any) { r["status"] = "cancelled" })
		},
	}
	for name, answer := range cases {
		t.Run(name, func(t *testing.T) {
			// lag 5 outlasts both sweeps' four list reads.
			x := newLagExchange(t, 5)
			x.named = answer(x)
			c, clk := clockedClient(x)
			first := c.CancelAndSweep(context.Background(), lagTicker,
				lagRequest(x, "oY"))
			if first.Clean || sweepSEV1(first) || sweepPendings(first) != 1 ||
				len(first.StillResting) != 1 {
				t.Fatalf("first sweep on a named answer of %q: clean=%v SEV1=%v "+
					"pending=%d still=%v", name, first.Clean, sweepSEV1(first),
					sweepPendings(first), ids(first.StillResting))
			}
			clk.advance(sweepPageBound)
			res := c.CancelAndSweep(context.Background(), lagTicker,
				lagRequest(x, "oY"))
			if res.Clean || !sweepSEV1(res) {
				t.Fatalf("a named answer of %q is not the exchange confirming oY "+
					"gone, and the list still shows it; clean=%v SEV1=%v",
					name, res.Clean, sweepSEV1(res))
			}
			if got := ids(res.StillResting); len(got) != 1 || got[0] != "oY" {
				t.Fatalf("still resting %v, want [oY]", got)
			}
		})
	}
}

// lip-9tt. After our own DELETE answered 200 with reduced_by equal to the whole
// order, production showed the complete resting list AND the named read stale
// for about 1.4 s, and each lag paged a SEV1 that cleared itself on the next
// tick. The fix defers only the PAGE to sweepPageBound; the verdict -- not
// Clean, the order in StillResting -- never waits and never expires.

// unverifiedDoer answers every `status=resting` list read 500 while `fail` is
// set, so the verifying read is incomplete (SWEEP_UNVERIFIED).
func unverifiedDoer(x *lagExchange, fail *bool) Doer {
	return doerFunc(func(ctx context.Context, req Request) (Response, error) {
		if *fail && req.Method == "GET" && req.Path == EpOrders.Path {
			return Response{Status: 500, Body: []byte(`{}`)}, nil
		}
		return x.Do(ctx, req)
	})
}

func sweepUnverified(res SweepResult) bool {
	for _, a := range res.Anomalies {
		if a.Class == "SWEEP_UNVERIFIED" && a.Sev == risk.SEV1 {
			return true
		}
	}
	return false
}

// (a) The first sweep that leaves an order unconfirmed does not page; it says
// the order is live and the sweep is not clean.
func TestSweepFirstUnconfirmedSweepIsPendingNotPaged(t *testing.T) {
	x := newLagExchange(t, 0)
	x.ignoreDeletes = true
	c, _ := clockedClient(x)
	res := c.CancelAndSweep(context.Background(), lagTicker, lagRequest(x, "oY"))
	if sweepSEV1(res) || sweepPendings(res) != 1 {
		t.Fatalf("SEV1=%v pending=%d, want no SEV1 and one SEV3 SWEEP_PENDING: %+v",
			sweepSEV1(res), sweepPendings(res), res.Anomalies)
	}
	if res.Clean {
		t.Fatal("a pending sweep is not clean: oY is live and fillable (H-FAIL-3)")
	}
	if got := ids(res.StillResting); len(got) != 1 || got[0] != "oY" {
		t.Fatalf("still resting %v, want [oY]", got)
	}
	for _, a := range res.Anomalies {
		if a.Class == "SWEEP_PENDING" && (!strings.Contains(a.Text, "live and fillable") ||
			!strings.Contains(a.Text, "NOT clean") ||
			!strings.Contains(a.Text, "SWEEP_INCOMPLETE")) {
			t.Fatalf("pending text must say live, not clean and when it pages: %q", a.Text)
		}
	}
}

// (b) The same order, still unconfirmed, stays SEV3 below the bound and pages
// SEV1 at it.
func TestSweepPagesAStillUnconfirmedOrderAtTheBound(t *testing.T) {
	x := newLagExchange(t, 0)
	x.ignoreDeletes = true
	c, clk := clockedClient(x)
	sweep := func() SweepResult {
		return c.CancelAndSweep(context.Background(), lagTicker, lagRequest(x, "oY"))
	}
	if res := sweep(); sweepSEV1(res) || sweepPendings(res) != 1 {
		t.Fatalf("t=0: SEV1=%v pending=%d", sweepSEV1(res), sweepPendings(res))
	}
	clk.advance(sweepPageBound - time.Millisecond)
	if res := sweep(); sweepSEV1(res) || sweepPendings(res) != 1 || res.Clean ||
		len(res.StillResting) != 1 {
		t.Fatalf("bound-1ms: SEV1=%v pending=%d clean=%v still=%v, want SEV3 only",
			sweepSEV1(res), sweepPendings(res), res.Clean, ids(res.StillResting))
	}
	clk.advance(time.Millisecond)
	res := sweep()
	if !sweepSEV1(res) || sweepPendings(res) != 0 || res.Clean {
		t.Fatalf("at the bound: SEV1=%v pending=%d clean=%v, want SEV1 "+
			"SWEEP_INCOMPLETE", sweepSEV1(res), sweepPendings(res), res.Clean)
	}
	if got := ids(res.StillResting); len(got) != 1 || got[0] != "oY" {
		t.Fatalf("still resting %v, want [oY]", got)
	}
	for _, a := range res.Anomalies {
		if a.Class == "SWEEP_INCOMPLETE" && !strings.Contains(a.Text, "unconfirmed for 5s") {
			t.Fatalf("the page must carry the oldest unconfirmed age: %q", a.Text)
		}
	}
}

// (c) The exchange's own record retires the order on a later sweep, which ends
// its episode: a new unconfirmed episode for the same id starts at SEV3 again.
func TestSweepRetirementByNamedReadEndsTheEpisode(t *testing.T) {
	x := newLagExchange(t, 100) // the list lags for the whole test
	retired := false
	x.named = func(id string) (Response, error) {
		o := x.find(id)
		if retired {
			return x.fresh(o), nil
		}
		body, _ := json.Marshal(map[string]any{
			"order": o.record(lagTicker, StatusResting, o.remaining, o.created)})
		return Response{Status: 200, Body: body}, nil
	}
	c, clk := clockedClient(x)
	sweep := func() SweepResult {
		return c.CancelAndSweep(context.Background(), lagTicker, lagRequest(x, "oY"))
	}
	if res := sweep(); sweepSEV1(res) || sweepPendings(res) != 1 {
		t.Fatalf("t=0: SEV1=%v pending=%d", sweepSEV1(res), sweepPendings(res))
	}
	clk.advance(3 * time.Second)
	retired = true
	if res := sweep(); !res.Clean || sweepSEV1(res) || sweepPendings(res) != 0 ||
		len(res.Confirmations) == 0 {
		t.Fatalf("t=3s: the named read retires oY; clean=%v SEV1=%v pending=%d "+
			"confirmations=%d", res.Clean, sweepSEV1(res), sweepPendings(res),
			len(res.Confirmations))
	}
	clk.advance(3 * time.Second) // 6 s after the first episode began
	retired = false
	res := sweep()
	if sweepSEV1(res) || sweepPendings(res) != 1 || res.Clean {
		t.Fatalf("t=6s: a NEW episode must start at SEV3, not inherit t=0's age; "+
			"SEV1=%v pending=%d clean=%v", sweepSEV1(res), sweepPendings(res),
			res.Clean)
	}
}

// (d) An incomplete verifying read neither starts nor ends an episode.
func TestSweepUnverifiedReadNeitherStartsNorEndsAnEpisode(t *testing.T) {
	t.Run("does not start", func(t *testing.T) {
		x := newLagExchange(t, 0)
		x.ignoreDeletes = true
		fail := true
		c, clk := clockedClient(unverifiedDoer(x, &fail))
		if res := c.CancelAndSweep(context.Background(), lagTicker,
			lagRequest(x, "oY")); !sweepUnverified(res) || res.Clean {
			t.Fatalf("want SWEEP_UNVERIFIED, got %+v", res.Anomalies)
		}
		clk.advance(sweepPageBound)
		fail = false
		res := c.CancelAndSweep(context.Background(), lagTicker, lagRequest(x, "oY"))
		if sweepSEV1(res) || sweepPendings(res) != 1 {
			t.Fatalf("an unverified sweep started an episode: SEV1=%v pending=%d",
				sweepSEV1(res), sweepPendings(res))
		}
	})
	t.Run("does not end", func(t *testing.T) {
		x := newLagExchange(t, 0)
		x.ignoreDeletes = true
		fail := false
		c, clk := clockedClient(unverifiedDoer(x, &fail))
		sweep := func() SweepResult {
			return c.CancelAndSweep(context.Background(), lagTicker, lagRequest(x, "oY"))
		}
		if res := sweep(); sweepPendings(res) != 1 {
			t.Fatalf("t=0: pending=%d", sweepPendings(res))
		}
		clk.advance(3 * time.Second)
		fail = true
		if res := sweep(); !sweepUnverified(res) || res.Clean {
			t.Fatalf("t=3s: want SWEEP_UNVERIFIED, got %+v", res.Anomalies)
		}
		clk.advance(3 * time.Second)
		fail = false
		res := sweep()
		if !sweepSEV1(res) || sweepPendings(res) != 0 {
			t.Fatalf("t=6s: the unverified sweep ended t=0's episode; SEV1=%v "+
				"pending=%d", sweepSEV1(res), sweepPendings(res))
		}
	})
}

// (e) A clean sweep ends the episode of every order it was asked to cancel.
func TestSweepCleanSweepEndsEveryRequestedEpisode(t *testing.T) {
	x := newLagExchange(t, 0)
	x.ignoreDeletes = true
	c, clk := clockedClient(x)
	both := append(lagRequest(x, "oY"), lagRequest(x, "oN")...)
	sweep := func() SweepResult {
		return c.CancelAndSweep(context.Background(), lagTicker, both)
	}
	if res := sweep(); sweepPendings(res) != 1 || len(res.StillResting) != 2 {
		t.Fatalf("t=0: pending=%d still=%v", sweepPendings(res), ids(res.StillResting))
	}
	clk.advance(3 * time.Second)
	x.ignoreDeletes = false
	if res := sweep(); !res.Clean || sweepPendings(res) != 0 || sweepSEV1(res) {
		t.Fatalf("t=3s: clean=%v pending=%d SEV1=%v", res.Clean,
			sweepPendings(res), sweepSEV1(res))
	}
	c.unconfirmedMu.Lock()
	left := len(c.unconfirmed)
	c.unconfirmedMu.Unlock()
	if left != 0 {
		t.Fatalf("%d episodes survived a clean sweep of both orders", left)
	}
	// Both rest again and will not die: a fresh episode, not t=0's.
	clk.advance(3 * time.Second)
	for _, o := range x.orders {
		o.status = StatusResting
	}
	x.ignoreDeletes = true
	if res := sweep(); sweepSEV1(res) || sweepPendings(res) != 1 {
		t.Fatalf("t=6s: SEV1=%v pending=%d, want a new SEV3 episode",
			sweepSEV1(res), sweepPendings(res))
	}
}
