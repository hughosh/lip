package rest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

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
	// CancelRejected is any other definite 4xx.
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
	// A cancel is a write, so it is rate-limited and rejected through the same
	// mechanism a create is — a 429 on a DELETE is F8's detection condition
	// arriving on the one write that only ever reduces exposure, and a harness
	// that could not tell it from an ordinary refusal would be unable to tell
	// "slow down" from "this order cannot be cancelled".
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
	res := CancelResult{OrderID: orderID}
	if orderID == "" {
		res.Outcome, res.Err = CancelRejected, fmt.Errorf("empty order id")
		return res
	}
	resp, err := c.Doer.Do(ctx, Request{
		Method: "DELETE",
		Path:   "/portfolio/events/orders/" + orderID,
	})
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

// SweepResult is the verified outcome of cancelling a set of orders.
type SweepResult struct {
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
	// orders resting. This is the ONLY thing in this package that licenses
	// treating an order as off.
	Clean     bool
	Anomalies []risk.Anomaly
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
	orders []Order) SweepResult {

	var res SweepResult
	targets := orders

	for round := 0; round <= sweepRetries; round++ {
		res.Rounds = round + 1
		for _, o := range targets {
			res.Cancels = append(res.Cancels, c.Cancel(ctx, o.OrderID))
		}

		// Verify. Not "assume", not "trust the 2xx".
		read := c.Orders(ctx, ticker, StatusResting)
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

		// Partition what the exchange still shows into "the orders we were
		// asked to cancel" and "everything else of ours". Only the first set is
		// retried; cancelling the second would be M2 -- on a halt it contains
		// the reducing quote, and A4 requires that be kept alive.
		requested := make(map[string]bool, len(orders))
		for _, o := range orders {
			requested[o.OrderID] = true
		}
		//
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
			case requested[o.OrderID]:
				still = append(still, o)
			case o.Ours:
				other = append(other, o)
			}
		}
		res.StillResting, res.OtherOurs = still, other

		if len(still) == 0 {
			res.Clean = true
			return res
		}

		// Still there. Retry once against what the EXCHANGE says is resting,
		// not against what we thought we had cancelled.
		targets = still
	}

	res.Clean = false
	res.Anomalies = append(res.Anomalies, risk.Anomaly{
		Class: "SWEEP_INCOMPLETE", Sev: risk.SEV1, Ticker: ticker,
		Text: fmt.Sprintf("cancel sweep still finds %d of our orders resting "+
			"after %d rounds; they are live and fillable",
			len(res.StillResting), res.Rounds),
	})
	return res
}

// confidence: high
