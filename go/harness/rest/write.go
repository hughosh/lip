package rest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"lip/harness/cfg"
	"lip/harness/num"
	"lip/harness/risk"
)

// ---------------------------------------------------------------------------
// §7.2 — the write protocol
// ---------------------------------------------------------------------------
//
//	INTENT ──dequeue──► SENDING ──2xx──► ACKED ──► RESTING
//	                       │
//	                       ├──4xx (definite)──► REJECTED
//	                       │
//	                       └──timeout / 5xx / conn error──► UNKNOWN
//
// This is the single most dangerous path in the whole system.

// CreateOutcome is the definite classification of one create.
type CreateOutcome uint8

const (
	// CreateUnknown is the ambiguous outcome and the zero value ON PURPOSE.
	// A result that was never populated must not read as a rejection: a
	// rejection licenses re-placing, and re-placing an order that in fact
	// landed is the most expensive single mistake available to this system.
	CreateUnknown CreateOutcome = iota
	// CreateAcked is a 2xx. This response describes the order.
	CreateAcked
	// CreateAlreadyExists is a 409 `order_already_exists`.
	//
	// **This is a POSITIVE IDENTIFICATION, not an error and not an absence.**
	// It says the original create DID land. That is what removes the need to
	// prove a negative — and proving a negative is exactly what H-ORD-2a
	// deleted, because a single-page read cannot establish absence and an
	// exhaustive walk cannot either without a snapshot-consistency contract
	// that is not known to exist.
	CreateAlreadyExists
	// CreateRejected is a definite ordinary 4xx (excluding 429). No order exists.
	CreateRejected
)

func (o CreateOutcome) String() string {
	switch o {
	case CreateAcked:
		return "ACKED"
	case CreateAlreadyExists:
		return "ALREADY_EXISTS"
	case CreateRejected:
		return "REJECTED"
	}
	return "UNKNOWN"
}

// Definite reports whether the outcome resolves the order's existence.
func (o CreateOutcome) Definite() bool { return o != CreateUnknown }

// Exists reports whether an order is known to exist on the exchange.
func (o CreateOutcome) Exists() bool {
	return o == CreateAcked || o == CreateAlreadyExists
}

// CreateResult is one fully-resolved (or fully-unresolved) create.
type CreateResult struct {
	Outcome CreateOutcome
	Coid    string
	// Attempts is how many times the SAME coid was sent. Never more than
	// cfg.Params.RetrySameCoidMax.
	Attempts int
	// Status is the last HTTP status seen, 0 if no response ever arrived.
	Status int

	// RejectReason is the exchange's own `error.code` from a definite 4xx. It
	// is populated on CreateRejected and on nothing else.
	//
	// F9, F10 and F11 consume this string. Without it, every definite 4xx
	// collapses into a single indistinguishable "no": an
	// `insufficient_balance` (F10, which H-CAP-5 calls a
	// correctness failure rather than a market condition, worth a SEV1 and a
	// global WINDING_DOWN) and a `post_only` that would cross (F11, our book
	// view disagreeing with the exchange's) all read the same. No downstream
	// rule can act on a reason it cannot name.
	//
	// It is `error.code` and NOTHING ELSE — not `error.message`, not the
	// formatted `Err`, not a snippet of the raw body. A code is a value the
	// exchange controls and keeps stable; a message is prose that can be
	// reworded without notice, and a detector keyed on prose is a detector that
	// silently stops detecting.
	//
	// EMPTY IS THE SAFE VALUE and it is used whenever the code is missing,
	// malformed or unparseable: a consumer that cannot name the reason must be
	// handed nothing rather than a guess. It is therefore also empty for
	// everything that is not the exchange rejecting an order and saying why — a
	// local validation failure, a guarded refusal, a request that never left the
	// process, any ambiguous outcome, either kind of 409, and every 2xx.
	RejectReason string

	// OrderID and the counts are populated on CreateAcked only. A 409 tells us
	// the order exists but says nothing about its id or its fill state — that
	// is what the confirming read is for.
	OrderID   string
	Remaining num.Qty
	Filled    num.Qty

	// MaxLive is the maximum quantity that may be live on the exchange as a
	// result of this create.
	//
	// H-ORD-2 clause 6: an UNKNOWN order's maximum possibly-live quantity STAYS
	// in the risk model and in every aggregate cap (H-Q-5b, H-CAP-7) until it is
	// positively resolved. So this is the requested count for anything that is
	// not a definite rejection, and zero only for CreateRejected.
	MaxLive num.Qty

	Anomalies []risk.Anomaly
	Err       error
}

// ReconcileNow reports whether §7.2 clause 2's immediate reconciliation is
// required. It is required for an unresolved create AND for a 409 — the 409 is
// belt, the confirming read is braces, and the spec asks for both because the
// dedupe behaviour is observed on one account on one day rather than documented.
func (r CreateResult) ReconcileNow() bool {
	return r.Outcome == CreateUnknown || r.Outcome == CreateAlreadyExists
}

// errAlreadyExists is the code that makes a 409 a positive identification. The
// STATUS alone is not enough: a 409 carrying any other code is a conflict we
// have not characterised, and guessing which one it is would be guessing about
// whether an order exists.
const errAlreadyExists = "order_already_exists"

// Create sends one order and resolves its outcome, retrying the SAME coid on an
// ambiguous response.
//
// **The coid is never regenerated.** There is no argument, field or code path in
// this function that produces a new one — the body is sent byte-identically on
// every attempt. `probebot.py` generated a fresh `uuid.uuid4()` per order, which
// makes this mechanism unavailable in principle: a retry under a new coid IS a
// genuinely new order, and the exchange cannot report it as a duplicate because
// it is not one. A10 forbids the new-coid retry and this signature makes it
// unexpressible.
//
// V1.7, answered 2026-08-04 by live minimum-size experiment: two creates with an
// identical coid produced `200` then `409 order_already_exists`, with exactly
// one resting order bearing that coid.
func (c *Client) Create(ctx context.Context, body CreateOrder, p cfg.Params) CreateResult {
	res := CreateResult{Coid: body.ClientOrderID()}

	// --- Freeze the payload, validating twice on the way ------------------
	//
	// Nothing below this block can change what is sent: the bytes are built
	// once and every same-coid retry reuses them exactly.
	//
	// Rejected here means the request never reaches Doer at all, so Attempts
	// stays 0 and MaxLive stays 0 — nothing was sent, so nothing can be live.
	rejected := func(err error) CreateResult {
		return CreateResult{Coid: body.ClientOrderID(),
			Outcome: CreateRejected, Attempts: 0, MaxLive: 0, Err: err}
	}
	// 1. The intent, revalidated. The value may have been copied or zeroed
	//    since NewCreateOrder returned it.
	if err := body.validate(); err != nil {
		return rejected(fmt.Errorf("order is not dispatchable: %w", err))
	}
	// 2. The wire body, derived fresh rather than carried.
	w, err := body.wire()
	if err != nil {
		return rejected(err)
	}
	// 3. The completed wire body, checked independently against the intent.
	if err := validateWire(w, body); err != nil {
		return rejected(fmt.Errorf("wire body rejected before dispatch: %w", err))
	}
	requested := body.Count()
	res.MaxLive = requested

	payload, err := json.Marshal(w)
	if err != nil {
		return rejected(fmt.Errorf("unmarshalable order body: %w", err))
	}

	max := p.RetrySameCoidMax
	if max < 1 {
		max = 1
	}
	for res.Attempts < max {
		res.Attempts++
		resp, err := c.Doer.Do(ctx, Request{
			Method: "POST",
			Path:   "/portfolio/events/orders",
			Body:   payload,
		})
		if err != nil {
			var refused *WriteRefused
			if errors.As(err, &refused) {
				// H-VER-1. The guard stopped this BEFORE the transport, so the
				// attempt did not happen at all -- undo the increment, or a
				// read-only rehearsal reports network attempts it never made.
				res.Attempts--

				// `res.Attempts` now counts the attempts that REALLY reached
				// the transport, and it is the discriminator rather than
				// `Outcome`: `CreateUnknown` is the deliberate zero value, so
				// testing the outcome alone would treat the very first refusal
				// as "an earlier attempt was ambiguous" and preserve an UNKNOWN
				// for an order that was never sent even once.
				if res.Attempts > 0 && res.Outcome == CreateUnknown {
					// AN EARLIER ATTEMPT WAS AMBIGUOUS AND THIS RETRY WAS
					// REFUSED. The order may be resting on the exchange right
					// now: the sentinel was removed between the two attempts,
					// and disarming cannot retroactively un-send what was
					// already sent. Preserve `UNKNOWN`, the requested
					// `MaxLive`, and `ReconcileNow()` -- the reconciliation is
					// the only thing that will find it.
					//
					// Overwriting this with `Rejected` is the dangerous
					// simplification: it would drop a possibly-live order out
					// of every aggregate cap at the exact moment an operator
					// was reaching for the off switch.
					res.Err = err
					return res
				}
				// Nothing was ever transmitted under this coid.
				res.Outcome, res.Status, res.Attempts, res.MaxLive, res.Err =
					CreateRejected, 0, 0, 0, err
				return res
			}
			if !WasSent(err) {
				// The request provably never left the process, so no order can
				// exist. This is a definite "no", and withholding it would not
				// be conservative: an UNKNOWN keeps its full size in every
				// aggregate cap, so a broken signer would consume the pilot's
				// capital with orders that never existed.
				res.Outcome, res.Status, res.MaxLive, res.Err =
					CreateRejected, 0, 0, err
				return res
			}
			// No answer, but it may have been executed. This is the whole
			// reason the coid is deterministic.
			res.Outcome, res.Status, res.Err = CreateUnknown, 0, err
			continue
		}
		res.Status = resp.Status

		switch {
		case resp.Status == 200 || resp.Status == 201:
			// F1 — only an OBSERVED ack shape may narrow what we believe is
			// live. `200 {}` used to become ACKED with MaxLive = 0 and no
			// reconciliation, so a 12-lot that actually landed looked like
			// nothing at all and a replacement doubled the exposure.
			ack, err := parseAck(resp.Body, res.Coid, requested)
			if err != nil {
				res.Outcome = CreateUnknown
				res.MaxLive = requested
				res.Err = err
				continue // same-coid recoverable
			}
			res.Outcome, res.Err = CreateAcked, nil
			res.OrderID, res.Remaining, res.Filled = ack.orderID, ack.remaining, ack.filled
			res.MaxLive = ack.remaining
			return res

		case resp.Status >= 200 && resp.Status < 300:
			// A 2xx we have never observed for this endpoint — 202, 204, 206.
			// It may mean the order was accepted for later processing, which
			// is precisely the state we cannot distinguish from "landed".
			res.Outcome = CreateUnknown
			res.MaxLive = requested
			res.Err = fmt.Errorf("HTTP %d is not an observed create "+
				"acknowledgement (200 or 201); the order's existence is "+
				"undetermined", resp.Status)
			continue

		case resp.Status == 409 && errorCode(resp.Body) == errAlreadyExists:
			// The original landed. One round trip, definite, and H-PAGE-1's
			// pagination trap cannot produce a duplicate because we never had
			// to search for anything.
			res.Outcome, res.Err = CreateAlreadyExists, nil
			res.MaxLive = requested
			c.confirm409(ctx, &res, body.Ticker(), requested)
			return res

		case resp.Status == 409:
			// A conflict we have not characterised. It is NOT a rejection: we
			// do not know that no order exists, and treating it as one would
			// license a re-place. Stay ambiguous and reconcile.
			res.Outcome = CreateUnknown
			res.Err = fmt.Errorf("HTTP 409 with code %q, not %q: an "+
				"uncharacterised conflict is not evidence that no order exists",
				errorCode(resp.Body), errAlreadyExists)
			continue

		case resp.Status == http.StatusTooManyRequests:
			// F8 is not evidence that the create was rejected. Preserve the
			// full possible exposure and let the dispatcher schedule the next
			// same-coid action after backoff, outside its transport slot.
			res.Outcome = CreateUnknown
			res.MaxLive = requested
			res.Err = rateLimitError(resp)
			return res

		case resp.Status >= 400 && resp.Status < 500:
			// A definite answer, and the answer is no. Retrying would not be
			// dangerous, but it would also not be a retry of an ambiguous
			// write — a rejected order is the requote ladder's problem.
			//
			// The reason leaves here STRUCTURALLY, as the exchange's own
			// `error.code`. `Err` below is a bounded snippet of whatever the
			// body happened to contain, built for a human reading a log line;
			// recovering `insufficient_balance` from it would mean each of
			// F8/F9/F10/F11 re-parsing truncated prose, and the one that got it
			// wrong would fail silently.
			res.Outcome = CreateRejected
			res.MaxLive = 0
			res.RejectReason = errorCode(resp.Body)
			res.Err = fmt.Errorf("HTTP %d: %s", resp.Status, snippet(resp.Body))
			return res

		default:
			// 5xx and anything else unclassifiable: the exchange may have
			// executed the request before failing to tell us about it.
			res.Outcome = CreateUnknown
			res.Err = fmt.Errorf("HTTP %d: %s", resp.Status, snippet(resp.Body))
			continue
		}
	}

	// Exhausted. The order stays UNKNOWN and escalates. A retry loop against a
	// persistently ambiguous endpoint is its own failure mode, which is why
	// retry_same_coid_max exists at all.
	//
	// It stays in the risk model at its full requested size (H-ORD-2 clause 6):
	// we do not know it is absent, and "we could not find it" has never been
	// evidence of anything.
	res.MaxLive = requested
	// The owner emits ORDER_UNKNOWN once after unknown_ping_s. Transport
	// exhaustion is the beginning of that interval, not its expiry.

	return res
}

// ConfirmCoid searches the account for one coid as a COMPLETE cursor walk over
// orders of every status.
//
// Unfiltered, not `status=resting`: an order that landed and then filled or was
// cancelled is still an order that landed, and a status-filtered walk that
// missed it would read as absence. `found == false` from a COMPLETE walk means
// "no order bearing this coid exists on the account"; from an incomplete walk it
// means nothing at all, which is why the Walk is returned rather than swallowed.
func (c *Client) ConfirmCoid(ctx context.Context, ticker, coid string) (Order, bool, Walk) {
	r := c.Orders(ctx, ticker, "")
	if !r.Replaces() {
		return Order{}, false, r.Walk
	}
	for _, o := range r.Orders {
		if o.ClientOrderID == coid {
			return o, true, r.Walk
		}
	}
	return Order{}, false, r.Walk
}

// confirm409 is H-ORD-2b's "belt and braces".
//
// The 409 is already a positive identification and nothing here can revoke it.
// The confirming read exists because the dedupe behaviour is **observed, not
// documented** — one account, one market, one day — and the cost of being wrong
// about it is double inventory. So this narrows MaxLive when a complete walk
// tells us what actually rests, and escalates when it cannot.
//
// A complete walk that does NOT find the coid does not downgrade the outcome.
// The order landed; it is simply no longer open. Absence is not evidence in
// either direction (H-ORD-2a), and it is certainly not evidence against a
// positive identification we already hold.
func (c *Client) confirm409(ctx context.Context, res *CreateResult,
	ticker string, requested num.Qty) {

	o, found, w := c.ConfirmCoid(ctx, ticker, res.Coid)
	switch {
	case found:
		res.OrderID = o.OrderID
		res.Remaining = o.Remaining
		if o.Remaining <= requested {
			res.MaxLive = o.Remaining
		}
	case !w.Replaces():
		res.Anomalies = append(res.Anomalies, risk.Anomaly{
			Class: "CREATE_UNCONFIRMED", Sev: risk.SEV2, Ticker: ticker,
			Text: fmt.Sprintf("409 order_already_exists for %s, but the "+
				"confirming read did not complete (%s); the order exists and "+
				"its full requested size stays in every aggregate cap",
				res.Coid, w.Outcome),
		})
	default:
		res.Anomalies = append(res.Anomalies, risk.Anomaly{
			Class: "CREATE_TERMINAL", Sev: risk.SEV3, Ticker: ticker,
			Text: fmt.Sprintf("409 order_already_exists for %s and a complete "+
				"orders walk found no open order bearing it: the create landed "+
				"and has since filled or been cancelled", res.Coid),
		})
	}
	res.Anomalies = append(res.Anomalies, w.Anomalies...)
}

// ack is a create acknowledgement that matched an observed shape.
type ack struct {
	orderID   string
	remaining num.Qty
	filled    num.Qty
}

// parseAck reads the flat CreateOrderV2Response, strictly.
//
// `probebot.py:486-491`: "CreateOrderV2Response is FLAT: {order_id, fill_count,
// remaining_count, ts_ms}. Only `remaining_count` rests and therefore only it
// scores." An ack is not proof that anything rests -- a post-only order can come
// back fully filled against a resting counterparty, or entirely cancelled.
//
// **Only a response matching that shape may narrow MaxLive.** The earlier
// version accepted any 2xx and read missing fields as zero, so `200 {}` and
// `200 null` produced ACKED with Remaining = 0, MaxLive = 0, no error and no
// reconciliation. A 12-contract order at 99c that actually landed then looked
// like nothing at all, and the requote ladder would place a replacement on top
// of it: 24 contracts and $23.76 against a $100 account, repeatable.
//
// Every rejection here returns the create to UNKNOWN at full size, which keeps
// the exposure in every aggregate cap and leaves it same-coid recoverable.
func parseAck(body []byte, coid string, requested num.Qty) (ack, error) {
	var rec map[string]json.RawMessage
	if err := json.Unmarshal(body, &rec); err != nil {
		return ack{}, fmt.Errorf("2xx with an undecodable body: %w", err)
	}
	if rec == nil {
		return ack{}, fmt.Errorf("2xx with a null body: the order may exist")
	}

	var a ack
	a.orderID = scalar(rec["order_id"])
	if a.orderID == "" {
		return ack{}, fmt.Errorf("2xx with no order_id: the acknowledgement " +
			"does not identify an order, so it cannot be evidence about one")
	}
	// The echoed coid is REQUIRED, not merely checked when present.
	//
	// The measured 200/201 shape carries `client_order_id`, so a response
	// without one does not match the shape and is not evidence about our order.
	// Accepting it only "when nonempty" is the same hole one level down: a
	// response with zero counts and no coid would narrow a live 12-contract
	// order to MaxLive = 0, and the requote ladder would then place a
	// replacement on top of it — 24 contracts, $23.76, against a $100 account.
	echoedRaw, ok := rec["client_order_id"]
	if !ok {
		return ack{}, fmt.Errorf("2xx with no client_order_id: the measured "+
			"acknowledgement carries one, and without it this response cannot "+
			"be tied to the order we sent (%q)", coid)
	}
	if isJSONNull(echoedRaw) {
		return ack{}, fmt.Errorf("2xx with a null client_order_id (we sent %q)",
			coid)
	}
	echoed := scalar(echoedRaw)
	if echoed == "" {
		return ack{}, fmt.Errorf("2xx with an empty client_order_id (we sent %q)",
			coid)
	}
	if echoed != coid {
		return ack{}, fmt.Errorf("2xx echoes client_order_id %q but we sent "+
			"%q; this response is about a different order", echoed, coid)
	}

	rem := scalar(rec["remaining_count"])
	if rem == "" {
		return ack{}, fmt.Errorf("2xx with no remaining_count: only " +
			"remaining_count rests, so without it nothing about our resting " +
			"exposure is known")
	}
	remaining, err := num.ParseQty(rem)
	if err != nil {
		return ack{}, fmt.Errorf("remaining_count: %w", err)
	}
	fill := scalar(rec["fill_count"])
	if fill == "" {
		return ack{}, fmt.Errorf("2xx with no fill_count")
	}
	filled, err := num.ParseQty(fill)
	if err != nil {
		return ack{}, fmt.Errorf("fill_count: %w", err)
	}

	if remaining < 0 || filled < 0 {
		return ack{}, fmt.Errorf("negative counts (remaining %v, filled %v)",
			remaining, filled)
	}
	// Bounded by what we asked for. An acknowledgement claiming more than we
	// sent is not a shape we have observed, and believing it would understate
	// our exposure elsewhere.
	if remaining > requested || filled > requested || remaining+filled > requested {
		return ack{}, fmt.Errorf("counts exceed the %v requested "+
			"(remaining %v, filled %v)", requested, remaining, filled)
	}
	a.remaining, a.filled = remaining, filled
	return a, nil
}

// errorCode pulls `error.code` out of an error body. It returns "" when the
// body has no such field, which is treated as "unrecognised" everywhere.
func errorCode(body []byte) string {
	var env struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return ""
	}
	return env.Error.Code
}

// confidence: high
