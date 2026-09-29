package rest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"lip/harness/num"
	"lip/harness/risk"
)

// ---------------------------------------------------------------------------
// §7.4 — cancel, and the sweep that turns it into a fact
// ---------------------------------------------------------------------------

// CancelOutcome classifies one DELETE.
type CancelOutcome uint8

const (
	// CancelUnknown is the zero value, and again on purpose: a cancel we have
	// no answer for has NOT taken effect as far as the risk model is concerned.
	// H-FAIL-3 — "off" means exchange-confirmed absent, never cancel-requested.
	CancelUnknown CancelOutcome = iota
	// CancelAccepted is a 2xx carrying `reduced_by`.
	CancelAccepted
	// CancelGone is a 404: the exchange has no such open order.
	CancelGone
	// CancelRejected is an ordinary definite 4xx, excluding 429.
	CancelRejected
)

func (o CancelOutcome) String() string {
	switch o {
	case CancelAccepted:
		return "ACCEPTED"
	case CancelGone:
		return "GONE"
	case CancelRejected:
		return "REJECTED"
	}
	return "UNKNOWN"
}

// CancelResult is one DELETE's answer.
//
// **It deliberately carries no field describing the position, the fill, or the
// "remainder".** `reduced_by` proves only what that particular DELETE removed,
// and it never implies the position moved. The trap H-ORD-4a exists to close is
// arithmetic that looks obviously right:
//
//	remainder := requested - res.ReducedBy   // <- WRONG, and expensively so
//
// Comparing a *retry's* `reduced_by = 0` (or a 404) against the *original*
// requested size reads as "the remainder filled", when the true explanation is
// that the first DELETE already cancelled it and `q` never moved at all.
// Position changes come from `/portfolio/positions` and `/portfolio/fills`,
// never from a cancel response — so this struct gives a caller nothing to
// subtract from.
type CancelResult struct {
	Outcome CancelOutcome
	OrderID string
	Coid    string
	Status  int
	// RejectReason is the exchange's own `error.code` from an ordinary 4xx that
	// is not a 404. It is populated on CancelRejected and on nothing else, and
	// it carries the same meaning, and the same emptiness rules, as
	// CreateResult.RejectReason: `error.code`, never `error.message`, never the
	// formatted `Err`, and empty rather than guessed.
	//
	// A 429 is F8 rate limiting, not a definite rejection. It leaves the
	// cancel outcome unknown, and Retry-After is carried in Err.
	//
	// It is NOT populated for CancelGone. A 404 is the exchange answering about
	// the ORDER — there is no such open order — rather than rejecting our
	// request, and it is not a fill report either (H-ORD-4a). Nor is it
	// populated for the local empty-order-id refusal, where no exchange was
	// involved at all.
	RejectReason string
	// ReducedBy is what THIS delete removed. It is not a fill report.
	ReducedBy num.Qty
	Err       error
	// Sent reports that the DELETE actually reached the transport (H-VER-1).
	//
	// It exists because a guarded refusal and a lost answer are opposite facts
	// that both arrive as `Outcome != CancelAccepted` with a non-nil `Err`. A
	// refusal consumed no write capacity and cannot have cancelled anything; an
	// ambiguous transport error may have cancelled the order. The dispatcher
	// refunds its capacity token on the first and not the second, and it is
	// this field that tells them apart.
	Sent bool
}

// Cancel issues one DELETE.
//
// It makes exactly one attempt. Cancels are retriable — H-ORD-4a states that as
// the explicit exception to H-ORD-2, because a duplicate cancel is idempotent in
// effect and strictly risk-decreasing, where a duplicate create can double
// inventory. But the retry belongs to the sweep, which re-issues against
// whatever a complete read still shows resting, rather than to a loop here that
// would be retrying against our own belief.
func (c *Client) Cancel(ctx context.Context, orderID string) CancelResult {
	// Keep the ID-only helper for callers that already know they are addressing
	// shard 0. The production event-market path is `cancelForMarket`, which adds
	// the ticker Kalshi requires to auto-route a cancellation. In particular,
	// `CancelAndSweep` must not call this helper: an order ID alone defaults to
	// exchange index 0 and can leave a nonzero-shard order resting.
	return c.cancel(ctx, "", orderID)
}

// cancelForMarket issues a DELETE routed by the market ticker.
//
// Kalshi documents that an order ID alone defaults to exchange index 0. An
// event-market cancel must carry `market_ticker` (and omit `exchange_index`) to
// auto-route to the matching engine that owns the order. The verifying read in
// `CancelAndSweep` remains authoritative; a 2xx from a misrouted or ineffective
// cancel is never treated as absence.
func (c *Client) cancelForMarket(ctx context.Context, ticker, orderID string) CancelResult {
	if err := validateTicker(ticker); err != nil {
		return CancelResult{OrderID: orderID, Outcome: CancelRejected,
			Err: fmt.Errorf("market-routed cancel: %w", err)}
	}
	return c.cancel(ctx, ticker, orderID)
}

func (c *Client) cancel(ctx context.Context, ticker, orderID string) CancelResult {
	res := CancelResult{OrderID: orderID}
	if orderID == "" {
		res.Outcome, res.Err = CancelRejected, fmt.Errorf("empty order id")
		return res
	}
	req := Request{
		Method: "DELETE",
		Path:   "/portfolio/events/orders/" + orderID,
	}
	if ticker != "" {
		req.Query = url.Values{"market_ticker": []string{ticker}}
	}
	resp, err := c.Doer.Do(ctx, req)
	if err != nil {
		var refused *WriteRefused
		if errors.As(err, &refused) {
			// H-VER-1. Nothing was transmitted, so this cancel provably did
			// not happen -- but the ORDER is untouched and still resting. It
			// stays live in the risk model, which is the same conclusion as an
			// ambiguous cancel and reached for the opposite reason: there, we
			// do not know; here, we know nothing was sent.
			res.Outcome, res.Sent, res.Err = CancelUnknown, false, err
			return res
		}
		// No answer. The order may still be resting, so it stays live in the
		// risk model until the sweep says otherwise.
		res.Outcome, res.Sent, res.Err = CancelUnknown, true, err
		return res
	}
	res.Sent = true
	res.Status = resp.Status

	switch {
	case resp.Status >= 200 && resp.Status < 300:
		res.Outcome = CancelAccepted
		var rec map[string]json.RawMessage
		if err := json.Unmarshal(resp.Body, &rec); err != nil {
			// A 2xx we cannot read is still an accepted cancel, but we do not
			// know how much it removed. The sweep decides.
			res.Err = fmt.Errorf("2xx with an undecodable body: %w", err)
			return res
		}
		res.Coid = scalar(rec["client_order_id"])
		// The response is {order_id, client_order_id, reduced_by, ts_ms} — a
		// flat acknowledgement, NOT a full order object. There is no status
		// field to read and no remaining count to trust.
		if v := scalar(rec["reduced_by"]); v != "" {
			q, err := num.ParseQty(v)
			if err != nil {
				res.Err = fmt.Errorf("reduced_by: %w", err)
				return res
			}
			res.ReducedBy = q
		}
		return res

	case resp.Status == 404:
		// The exchange has no such open order. This is a definite answer about
		// THIS order, and it is not a fill report either.
		res.Outcome = CancelGone
		return res

	case resp.Status == http.StatusTooManyRequests:
		res.Outcome = CancelUnknown
		res.Err = rateLimitError(resp)
		return res

	case resp.Status >= 400 && resp.Status < 500:
		// Definite, and the reason leaves here structurally rather than as
		// prose inside `Err`, for the reason `Create`'s 4xx branch gives.
		res.Outcome = CancelRejected
		res.RejectReason = errorCode(resp.Body)
		res.Err = fmt.Errorf("HTTP %d: %s", resp.Status, snippet(resp.Body))
		return res

	default:
		res.Outcome = CancelUnknown
		res.Err = fmt.Errorf("HTTP %d: %s", resp.Status, snippet(resp.Body))
		return res
	}
}

// ---------------------------------------------------------------------------
// H-ORD-4 — cancel-all is verified, not assumed
// ---------------------------------------------------------------------------

// sweepRetries is H-ORD-4's "retries once, then pings SEV1", stated as a
// constant rather than as a parameter because it is not a tuning knob: the
// point is that the sweep is bounded and escalates, not that it is patient.
const sweepRetries = 1

// sweepPageBound is how long a requested order may stay unconfirmed, across
// consecutive sweeps of it, before SWEEP_INCOMPLETE pages SEV1.
//
// lip-9tt: after our own DELETE answered 200 with reduced_by equal to the whole
// order, production showed BOTH the complete resting list and the named read
// still "resting" for about 1.4 s (lip-kaf's list lags were 0.27-1.2 s). The
// caller re-sweeps such an order every tick, so each lag paged a SEV1 that
// cleared itself about 1.5 s later. The bound defers only the PAGE: until a
// complete read or the exchange's own record says otherwise the order stays in
// StillResting and the sweep stays not Clean. Nothing is ever retired by
// elapsed time (H-FAIL-3, H-ORD-4c).
const sweepPageBound = 5 * time.Second

// SweepResult is the verified outcome of cancelling a set of orders.
type SweepResult struct {
	// Throttle asks the caller to requeue the sweep after backoff. A 429
	// stops this attempt immediately, without a second DELETE in the slot.
	Throttle *RateLimitError
	// Walk is the LAST verifying read. Clean is meaningful only when it
	// completed: an incomplete walk cannot establish that anything is absent.
	Walk
	// Cancels is every DELETE issued, in order, across every round.
	Cancels []CancelResult
	// Rounds is how many cancel-then-verify passes ran (1 + retries).
	Rounds int
	// StillResting is the subset of the REQUESTED orders that a complete
	// verifying read still shows resting.
	StillResting []Order
	// OtherOurs is every other `lipH-` order resting in this ticker that was
	// NOT in the requested set.
	//
	// It is reported and deliberately NOT cancelled. H-ORD-4's sweep verifies
	// "nothing of ours remains", and it would be easy to implement that by
	// cancelling whatever the verifying read turns up — which is M2 waiting to
	// happen: on a market halt that set includes the reducing quote, and A4
	// requires the reducer be kept alive, not cancelled. A caller performing a
	// genuine cancel-all (H-CLOSE-3's final cancel, `WINDING_DOWN`) asserts this
	// is empty; a caller cancelling one side does not.
	OtherOurs []Order
	// Clean is true only when a complete read found none of the REQUESTED
	// orders resting, counting as not resting only an order the exchange's
	// own record reports retired (`exchangeRetired`). This is the ONLY thing
	// in this package that licenses treating an order as off.
	Clean bool
	// Confirmations is every named-order read made before paging SEV1: one
	// per requested order a complete read still listed after the retry.
	Confirmations []Confirmation
	Anomalies     []risk.Anomaly
}

// Confirmation is one named-order read made before a sweep pages.
type Confirmation struct {
	OrderID string
	// Status is the HTTP status; zero when no answer arrived.
	Status int
	// Order is the exchange's record, when the answer decoded as one.
	Order Order
	// Retired is true only when that record names this order, in this
	// market, as terminal with nothing remaining.
	Retired bool
	Err     error
}

// exchangeRetired reports whether an order record, as the exchange itself
// states it, says nothing of the order rests: one of the two pinned terminal
// statuses and nothing remaining. Anything else -- resting, empty, unknown, a
// misspelling, a terminal status still carrying quantity -- leaves it live.
func exchangeRetired(o Order) bool {
	return (o.Status == StatusCanceled || o.Status == StatusExecuted) &&
		o.Remaining == 0
}

// confirmRetired reads one order by id: GET /portfolio/orders/{order_id},
// answered as {"order": {...}} in the record shape the list uses.
//
// This is H-FAIL-3's "confirmed gone by response", and it is the only thing
// that may overrule a complete list still showing a requested order: a list
// can lag a cancel, but a terminal order does not rest again. A 404 is not a
// confirmation -- absence is not evidence (H-ORD-2a) -- and neither is an
// error, an undecodable body or a record about another order or market.
func (c *Client) confirmRetired(ctx context.Context, ticker, orderID string) Confirmation {
	conf := Confirmation{OrderID: orderID}
	resp, err := c.Doer.Do(ctx, Request{Method: "GET",
		Path: EpOrders.Path + "/" + orderID})
	if err != nil {
		conf.Err = err
		return conf
	}
	conf.Status = resp.Status
	if resp.Status != http.StatusOK {
		conf.Err = fmt.Errorf("HTTP %d: %s", resp.Status, snippet(resp.Body))
		return conf
	}
	var body struct {
		Order json.RawMessage `json:"order"`
	}
	if err := json.Unmarshal(resp.Body, &body); err != nil {
		conf.Err = fmt.Errorf("undecodable order answer: %w", err)
		return conf
	}
	if len(body.Order) == 0 || string(body.Order) == "null" {
		conf.Err = fmt.Errorf("no order object: %s", snippet(resp.Body))
		return conf
	}
	o, _, err := decodeOrder(body.Order)
	if err != nil {
		conf.Err = err
		return conf
	}
	conf.Order = o
	switch {
	case o.OrderID != orderID:
		conf.Err = fmt.Errorf("the answer is about order %s", o.OrderID)
	case o.Ticker != ticker:
		conf.Err = fmt.Errorf("the answer is about market %s", o.Ticker)
	default:
		conf.Retired = exchangeRetired(o)
	}
	return conf
}

// CancelAndSweep cancels `orders` and then verifies, by reading the exchange,
// that none of ours remains.
//
// `probebot.py` did this ("sweep") and it is the only thing that turns a cancel
// into a fact. Everything short of it — a 2xx, a `reduced_by` equal to what we
// asked for, our own belief that we sent a DELETE — is a cancel *request*, and
// H-FAIL-3 is explicit that a cancel-requested order is still live and fillable
// until the exchange confirms it absent.
//
// The verifying read is `status=resting` over a COMPLETE cursor walk. A single
// page would be the same defect in a new place: an order at item 201 reads as
// absent and we would declare the sweep clean while it rests.
func (c *Client) CancelAndSweep(ctx context.Context, ticker string,
	orders []Order) (res SweepResult) {

	// Partition what the exchange still shows into "the orders we were asked
	// to cancel" and "everything else of ours". Only the first set is
	// retried; cancelling the second would be M2 -- on a halt it contains the
	// reducing quote, and A4 requires that be kept alive.
	requested := make(map[string]bool, len(orders))
	for _, o := range orders {
		requested[o.OrderID] = true
	}
	tr := newSweepTrace(ticker, orders)
	defer func() { c.emitTrace(tr, res) }()
	targets := orders

	for round := 0; round <= sweepRetries; round++ {
		res.Rounds = round + 1
		rt := tr.round()
		for _, o := range targets {
			at := traceNow()
			cancel := c.cancelForMarket(ctx, ticker, o.OrderID)
			rt.Deletes = append(rt.Deletes, traceDelete(cancel, at))
			res.Cancels = append(res.Cancels, cancel)
			if throttle, ok := cancel.Err.(*RateLimitError); ok {
				res.Throttle = throttle
				return res
			}
		}

		// Verify. Not "assume", not "trust the 2xx".
		at := traceNow()
		read := c.Orders(ctx, ticker, StatusResting)
		rt.Read = traceRead(read, requested, at)
		res.Walk = read.Walk
		res.Anomalies = append(res.Anomalies, read.Anomalies...)

		if !read.Replaces() {
			// An incomplete read tells us nothing. It cannot retire an order
			// from our model and it certainly cannot declare a sweep clean.
			res.Clean = false
			res.StillResting = nil
			res.Anomalies = append(res.Anomalies, risk.Anomaly{
				Class: "SWEEP_UNVERIFIED", Sev: risk.SEV1, Ticker: ticker,
				Text: fmt.Sprintf("cancel sweep could not be verified: the "+
					"resting-orders read ended %s (%v). Every cancelled order "+
					"stays live in the risk model until a complete read says "+
					"otherwise (H-FAIL-3)", read.Outcome, read.Err),
			})
			return res
		}

		// Requested-ID matching comes FIRST, before ownership classification.
		// Filtering by `Ours()` first loses a requested order whose
		// `client_order_id` came back missing or malformed: it would be
		// classified foreign, dropped from StillResting, and the sweep would
		// report Clean while the order was still resting and fillable. The
		// caller would then place a replacement on top of it.
		//
		// We asked to cancel a specific order id. Whether the exchange echoed a
		// coid we recognise does not change whether that id is still there.
		var still, other []Order
		for _, o := range read.Orders {
			switch {
			case requested[o.OrderID] && exchangeRetired(o):
				// The filter lagged but the record did not: the exchange's
				// own record for this order says nothing of it rests.
			case requested[o.OrderID]:
				still = append(still, o)
			case o.Ours:
				other = append(other, o)
			}
		}
		res.StillResting, res.OtherOurs = still, other

		if len(still) == 0 {
			res.Clean = true
			c.settleUnconfirmed(requested, nil)
			return res
		}

		// Still there. Retry once against what the EXCHANGE says is resting,
		// not against what we thought we had cancelled.
		targets = still
	}

	// Still listed after the retry. Before paging, ask the exchange about each
	// such order by id. lip-kaf: every one of the 2026-09-28 first stage's six
	// SEV1s named an order the exchange had cancelled 0.27-1.2 s earlier; the
	// complete resting list lagged the cancel for longer than both rounds
	// take. Only the exchange's own record retires an order here, never
	// absence or waiting, and anything short of it pages exactly as before.
	var live []Order
	for _, o := range res.StillResting {
		at := traceNow()
		conf := c.confirmRetired(ctx, ticker, o.OrderID)
		tr.Confirms = append(tr.Confirms, traceConfirm(conf, at))
		res.Confirmations = append(res.Confirmations, conf)
		if !conf.Retired {
			live = append(live, o)
		}
	}
	res.StillResting = live
	oldest := c.settleUnconfirmed(requested, live)
	if len(live) == 0 {
		res.Clean = true
		return res
	}

	// lip-9tt: the same verdict either way -- not Clean, StillResting = live --
	// and only the page waits for sweepPageBound.
	res.Clean = false
	if oldest >= sweepPageBound {
		res.Anomalies = append(res.Anomalies, risk.Anomaly{
			Class: "SWEEP_INCOMPLETE", Sev: risk.SEV1, Ticker: ticker,
			Text: fmt.Sprintf("cancel sweep still finds %d of our orders resting "+
				"after %d rounds; they are live and fillable. Named read: %s. "+
				"Oldest unconfirmed for %s (page bound %s)",
				len(res.StillResting), res.Rounds,
				unretiredSummary(res.Confirmations), oldest, sweepPageBound),
		})
		return res
	}
	res.Anomalies = append(res.Anomalies, risk.Anomaly{
		Class: "SWEEP_PENDING", Sev: risk.SEV3, Ticker: ticker,
		Text: fmt.Sprintf("cancel sweep still finds %d of our orders resting "+
			"after %d rounds; they are live and fillable and the sweep is NOT "+
			"clean. Named read: %s. Oldest unconfirmed for %s; they page as "+
			"SWEEP_INCOMPLETE if still unconfirmed at %s (lip-9tt)",
			len(res.StillResting), res.Rounds,
			unretiredSummary(res.Confirmations), oldest, sweepPageBound),
	})
	return res
}

// settleUnconfirmed records what one sweep established about its requested
// orders and returns the oldest unconfirmed age among `live`.
//
// It is called only after a COMPLETE verifying read. Every requested order not
// in `live` was then absent from that read, listed terminal (exchangeRetired)
// or confirmed retired by name (confirmRetired), so its episode ends. Every
// order in `live` keeps the first time a sweep left it unconfirmed, or starts
// one now. An incomplete read and a throttled sweep never reach here, so they
// neither start nor end an episode.
func (c *Client) settleUnconfirmed(requested map[string]bool, live []Order) time.Duration {
	now := c.now()
	c.unconfirmedMu.Lock()
	defer c.unconfirmedMu.Unlock()
	isLive := make(map[string]bool, len(live))
	for _, o := range live {
		isLive[o.OrderID] = true
	}
	for id := range requested {
		if !isLive[id] {
			delete(c.unconfirmed, id)
		}
	}
	var oldest time.Duration
	for _, o := range live {
		first, ok := c.unconfirmed[o.OrderID]
		if !ok {
			if c.unconfirmed == nil {
				c.unconfirmed = make(map[string]time.Time)
			}
			c.unconfirmed[o.OrderID] = now
			first = now
		}
		// Both readings come straight from the clock, so with time.Now this
		// subtraction is monotonic and a wall-clock step cannot move it.
		if age := now.Sub(first); age > oldest {
			oldest = age
		}
	}
	return oldest
}

func (c *Client) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

// unretiredSummary says, briefly, why each named read did not retire its order.
func unretiredSummary(confs []Confirmation) string {
	const shown = 4
	var parts []string
	for _, conf := range confs {
		if conf.Retired {
			continue
		}
		if len(parts) == shown {
			parts = append(parts, "...")
			break
		}
		why := fmt.Sprintf("status %q remaining %s", conf.Order.Status,
			conf.Order.Remaining.Wire())
		if conf.Err != nil {
			why = conf.Err.Error()
			if len(why) > 120 {
				why = why[:120]
			}
		}
		parts = append(parts, conf.OrderID+" "+why)
	}
	return strings.Join(parts, "; ")
}

// confidence: high
