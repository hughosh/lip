package rest

import "time"

// SweepTrace is one CancelAndSweep as this side of the wire saw it: every
// DELETE, every verifying read and every named read, with wall times.
//
// It exists because lip-kaf could not adjudicate six live SEV1s without the
// requested ids, each DELETE's answer and each verifying read's timing and
// contents, and the run had kept none of them. It is diagnostic evidence only:
// it is built alongside the sweep and handed to `Client.SweepTrace` after the
// result is final, and nothing in the harness decides on it.
//
// Bounded: each read keeps at most traceOrderCap records, and only requested or
// ours ones, while `Listed` still counts every record the read returned.
type SweepTrace struct {
	Ticker       string         `json:"ticker"`
	Requested    []string       `json:"requested"`
	Started      time.Time      `json:"started"`
	Ended        time.Time      `json:"ended"`
	Rounds       []TraceRound   `json:"rounds"`
	Confirms     []TraceConfirm `json:"confirms,omitempty"`
	Verdict      string         `json:"verdict"`
	StillResting []string       `json:"still_resting,omitempty"`
	OtherOurs    []string       `json:"other_ours,omitempty"`
	// Page is the lip-9tt decision on a not-clean sweep: "pending" (SEV3
	// SWEEP_PENDING) or "sev1" (SWEEP_INCOMPLETE). Empty otherwise.
	Page string `json:"page,omitempty"`
}

// TraceRound is one cancel-then-verify pass.
type TraceRound struct {
	Deletes []TraceDelete `json:"deletes"`
	Read    TraceRead     `json:"read"`
}

// TraceDelete is one DELETE and its classified answer.
type TraceDelete struct {
	OrderID   string    `json:"order_id"`
	Started   time.Time `json:"started"`
	Ended     time.Time `json:"ended"`
	Sent      bool      `json:"sent"`
	Status    int       `json:"status"`
	Outcome   string    `json:"outcome"`
	ReducedBy string    `json:"reduced_by,omitempty"`
	Err       string    `json:"err,omitempty"`
}

// TraceRead is one verifying `status=resting` read.
type TraceRead struct {
	Started time.Time    `json:"started"`
	Ended   time.Time    `json:"ended"`
	Outcome string       `json:"outcome"`
	Pages   int          `json:"pages"`
	Listed  int          `json:"listed"`
	Orders  []TraceOrder `json:"orders,omitempty"`
	Err     string       `json:"err,omitempty"`
}

// TraceOrder is one listed or named record, as the exchange stated it.
type TraceOrder struct {
	OrderID    string `json:"order_id"`
	Side       string `json:"side"`
	Status     string `json:"status"`
	Remaining  string `json:"remaining"`
	LastUpdate string `json:"last_update,omitempty"`
	Ours       bool   `json:"ours"`
	Requested  bool   `json:"requested"`
}

// TraceConfirm is one named-order read made before paging.
type TraceConfirm struct {
	OrderID string      `json:"order_id"`
	Started time.Time   `json:"started"`
	Ended   time.Time   `json:"ended"`
	Status  int         `json:"status"`
	Record  *TraceOrder `json:"record,omitempty"`
	Retired bool        `json:"retired"`
	Err     string      `json:"err,omitempty"`
}

const (
	traceOrderCap = 32
	traceErrCap   = 200
)

func traceNow() time.Time { return time.Now().UTC() }

func traceErr(err error) string {
	if err == nil {
		return ""
	}
	s := err.Error()
	if len(s) > traceErrCap {
		s = s[:traceErrCap] + "..."
	}
	return s
}

func traceOrder(o Order, requested bool) TraceOrder {
	return TraceOrder{OrderID: o.OrderID, Side: o.Side.String(),
		Status: o.Status, Remaining: o.Remaining.Wire(),
		LastUpdate: o.LastUpdate, Ours: o.Ours, Requested: requested}
}

func newSweepTrace(ticker string, orders []Order) *SweepTrace {
	return &SweepTrace{Ticker: ticker, Requested: orderIDs(orders),
		Started: traceNow()}
}

// round starts a new pass and returns it for the caller to fill in.
func (t *SweepTrace) round() *TraceRound {
	t.Rounds = append(t.Rounds, TraceRound{})
	return &t.Rounds[len(t.Rounds)-1]
}

func traceDelete(c CancelResult, started time.Time) TraceDelete {
	d := TraceDelete{OrderID: c.OrderID, Started: started, Ended: traceNow(),
		Sent: c.Sent, Status: c.Status, Outcome: c.Outcome.String(),
		Err: traceErr(c.Err)}
	if c.Outcome == CancelAccepted {
		d.ReducedBy = c.ReducedBy.Wire()
	}
	return d
}

func traceRead(r OrdersResult, requested map[string]bool, started time.Time) TraceRead {
	tr := TraceRead{Started: started, Ended: traceNow(),
		Outcome: r.Outcome.String(), Pages: r.Pages, Listed: len(r.Orders),
		Err: traceErr(r.Err)}
	for _, o := range r.Orders {
		if len(tr.Orders) == traceOrderCap {
			break
		}
		if requested[o.OrderID] || o.Ours {
			tr.Orders = append(tr.Orders, traceOrder(o, requested[o.OrderID]))
		}
	}
	return tr
}

func traceConfirm(c Confirmation, started time.Time) TraceConfirm {
	tc := TraceConfirm{OrderID: c.OrderID, Started: started, Ended: traceNow(),
		Status: c.Status, Retired: c.Retired, Err: traceErr(c.Err)}
	if c.Order.OrderID != "" {
		rec := traceOrder(c.Order, c.Order.OrderID == c.OrderID)
		tc.Record = &rec
	}
	return tc
}

// emitTrace finishes the trace from the final result and hands it over.
func (c *Client) emitTrace(t *SweepTrace, res SweepResult) {
	if c.SweepTrace == nil {
		return
	}
	t.Ended = traceNow()
	switch {
	case res.Throttle != nil:
		t.Verdict = "throttled"
	case !res.Replaces():
		t.Verdict = "unverified"
	case res.Clean && len(res.Confirmations) > 0:
		t.Verdict = "confirmed"
	case res.Clean:
		t.Verdict = "clean"
	default:
		t.Verdict = "incomplete"
	}
	for _, a := range res.Anomalies {
		switch a.Class {
		case "SWEEP_PENDING":
			t.Page = "pending"
		case "SWEEP_INCOMPLETE":
			t.Page = "sev1"
		}
	}
	t.StillResting = orderIDs(res.StillResting)
	t.OtherOurs = orderIDs(res.OtherOurs)
	c.SweepTrace(*t)
}

func orderIDs(orders []Order) []string {
	out := make([]string, 0, len(orders))
	for _, o := range orders {
		out = append(out, o.OrderID)
	}
	return out
}

// confidence: high
