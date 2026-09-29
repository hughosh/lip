package main

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"lip/harness/num"
	"lip/harness/quote"
	"lip/harness/rest"
	"lip/harness/risk"
)

// namedReadExchange is dispatch_test.go's fakeExchange plus the named-order
// GET. Its `resting` list never changes, which is the point here: it keeps
// listing orders the exchange's own records have already retired -- the
// lagging list lip-kaf observed on 2026-09-28 -- while the named read answers
// from those records.
type namedReadExchange struct {
	*fakeExchange
	mu sync.Mutex
	// status is what the named read reports for each order id.
	status map[string]string
	reads  []string
}

func (x *namedReadExchange) Do(ctx context.Context, req rest.Request) (rest.Response, error) {
	if req.Method != "GET" || !strings.HasPrefix(req.Path, "/portfolio/orders/") {
		return x.fakeExchange.Do(ctx, req)
	}
	id := strings.TrimPrefix(req.Path, "/portfolio/orders/")
	x.mu.Lock()
	x.reads = append(x.reads, id)
	status, known := x.status[id]
	x.mu.Unlock()
	var rec map[string]any
	for _, o := range x.fakeExchange.resting {
		if o["order_id"] == id {
			rec = make(map[string]any, len(o))
			for k, v := range o {
				rec[k] = v
			}
		}
	}
	if !known || rec == nil {
		return rest.Response{Status: 404,
			Body: []byte(`{"error":{"code":"not_found"}}`)}, nil
	}
	rec["status"] = status
	if status != rest.StatusResting {
		rec["remaining_count"] = "0.00"
	}
	body, err := json.Marshal(map[string]any{"order": rec})
	if err != nil {
		return rest.Response{}, err
	}
	return rest.Response{Status: 200, Body: body}, nil
}

func (x *namedReadExchange) namedReads() []string {
	x.mu.Lock()
	defer x.mu.Unlock()
	return append([]string(nil), x.reads...)
}

func sweepPending(res writeResult) bool {
	for _, a := range res.Anomalies {
		if a.Class == "SWEEP_PENDING" && a.Sev == risk.SEV3 {
			return true
		}
	}
	return false
}

func sweepPaged(res writeResult) bool {
	for _, a := range res.Anomalies {
		if a.Class == "SWEEP_INCOMPLETE" && a.Sev == risk.SEV1 {
			return true
		}
	}
	return false
}

// A market stop cancels both sides of one market. The list lags both cancels,
// and each side's verdict must follow the exchange's own record for the order
// that side asked about -- never the other side's.
func TestCancelWriteFollowsTheExchangesRecordThroughAListLagOnBothSides(t *testing.T) {
	const yesCoid = "lipH-run1-000-yes-00000001"
	const noCoid = "lipH-run1-000-no-00000002"
	cancelOf := func(side quote.Side, id string) writeRequest {
		return writeRequest{
			IDs: []uint64{1}, Market: dispatchTicker, Side: side,
			Role: quote.RoleAdding, Op: quote.OpCancel,
			Orders: []rest.Order{{OrderID: id, Ticker: dispatchTicker,
				Side: side, Remaining: num.QtyFromFloat(1)}},
		}
	}
	newExchange := func(yesStatus, noStatus string) *namedReadExchange {
		return &namedReadExchange{
			fakeExchange: &fakeExchange{resting: []map[string]any{
				restingOrder("EX-Y", yesCoid, "yes", 0.23),
				restingOrder("EX-N", noCoid, "no", 0.39),
			}},
			status: map[string]string{"EX-Y": yesStatus, "EX-N": noStatus},
		}
	}

	t.Run("both cancelled, both lagging", func(t *testing.T) {
		ex := newExchange(rest.StatusCanceled, rest.StatusCanceled)
		r, reserves := newDispatchRig(t, ex)

		yes := routedWrite(t, r, reserves, cancelOf(quote.SideYes, "EX-Y"))
		if !yes.Absent || sweepPaged(yes) {
			t.Fatalf("YES: the exchange's record says EX-Y is cancelled; "+
				"absent=%v SEV1=%v clean=%v still=%d. A lagging list is not "+
				"a live order (lip-kaf, H-FAIL-3)", yes.Absent, sweepPaged(yes),
				yes.Sweep.Clean, len(yes.Sweep.StillResting))
		}
		no := routedWrite(t, r, reserves, cancelOf(quote.SideNo, "EX-N"))
		if !no.Absent || sweepPaged(no) {
			t.Fatalf("NO: absent=%v SEV1=%v; EX-Y still listed on the YES "+
				"side must not decide the NO side", no.Absent, sweepPaged(no))
		}
		if !yes.Sent || !no.Sent || yes.Err != nil || no.Err != nil {
			t.Fatalf("sent %v/%v err %v/%v", yes.Sent, no.Sent, yes.Err, no.Err)
		}
		for _, id := range ex.namedReads() {
			if id != "EX-Y" && id != "EX-N" {
				t.Fatalf("named read of %s", id)
			}
		}
	})

	t.Run("one side really rests", func(t *testing.T) {
		ex := newExchange(rest.StatusCanceled, rest.StatusResting)
		r, reserves := newDispatchRig(t, ex)

		yes := routedWrite(t, r, reserves, cancelOf(quote.SideYes, "EX-Y"))
		if !yes.Absent || sweepPaged(yes) {
			t.Fatalf("YES: absent=%v SEV1=%v", yes.Absent, sweepPaged(yes))
		}
		// lip-9tt: the page waits for the bound; the verdict does not.
		now := time.Unix(1_790_000_000, 0)
		r.api.Now = func() time.Time { return now }
		no := routedWrite(t, r, reserves, cancelOf(quote.SideNo, "EX-N"))
		if no.Absent || sweepPaged(no) || !sweepPending(no) {
			t.Fatalf("NO first sweep: the exchange's own record says EX-N "+
				"still rests; absent=%v SEV1=%v pending=%v. It is live and "+
				"fillable, and pages only once unconfirmed for the bound (H-ORD-4)",
				no.Absent, sweepPaged(no), sweepPending(no))
		}
		if got := no.Sweep.StillResting; len(got) != 1 || got[0].OrderID != "EX-N" {
			t.Fatalf("NO still resting %+v, want EX-N", got)
		}
		now = now.Add(5 * time.Second)
		no = routedWrite(t, r, reserves, cancelOf(quote.SideNo, "EX-N"))
		if no.Absent || !sweepPaged(no) {
			t.Fatalf("NO at the bound: EX-N still rests; absent=%v SEV1=%v. "+
				"That order is live and fillable and the sweep must page (H-ORD-4)",
				no.Absent, sweepPaged(no))
		}
		if got := no.Sweep.StillResting; len(got) != 1 || got[0].OrderID != "EX-N" {
			t.Fatalf("NO still resting at the bound %+v, want EX-N", got)
		}
	})
}
