package main

import (
	"errors"
	"lip/harness/quote"
	"lip/harness/rest"
	"lip/harness/risk"
	"math"
	"math/rand"
	"time"
)

func sameWrite(a, b writeRequest) bool {
	if a.Op != b.Op || a.Market != b.Market || a.Side != b.Side {
		return false
	}
	if a.Op == quote.OpPlace {
		return a.Order.ClientOrderID() == b.Order.ClientOrderID()
	}
	return len(a.IDs) > 0 && len(b.IDs) > 0 && a.IDs[0] == b.IDs[0]
}

// retryDelay is bounded exponential jitter, with a server-supplied lower bound.
func retryDelay(n int, throttle *rest.RateLimitError, jitter float64) time.Duration {
	if n > 6 {
		n = 6
	}
	if n < 0 {
		n = 0
	}
	base := time.Second * time.Duration(1<<n)
	delay := base/2 + time.Duration(float64(base/2)*jitter)
	if throttle.HasDelay && throttle.Delay > delay {
		delay = throttle.Delay
	}
	return delay
}

func retryDeadline(now, delay time.Duration) time.Duration {
	if delay > time.Duration(math.MaxInt64)-now {
		return time.Duration(math.MaxInt64)
	}
	return now + delay
}
func (o *owner) updateThrottle(now time.Duration) {
	if !o.throttled {
		return
	}
	if now-o.throttleAt >= 60*time.Second {
		o.throttled = false
		o.throttlePinged = false
		return
	}
	if now-o.throttleFirst >= 60*time.Second && !o.throttlePinged {
		o.throttlePinged = true
		o.r.anom.raise(risk.Anomaly{Class: "RATE_LIMIT_SUSTAINED", Sev: risk.SEV2, Text: "exchange throttling has persisted for 60 seconds; write rate remains halved"})
	}
}

// noteRESTThrottle runs only on the owner goroutine. Every read and write 429
// shares the same clean-period clock, effective write rate and sustained SEV2.
func (o *owner) noteRESTThrottle(throttle *rest.RateLimitError) {
	if throttle == nil {
		return
	}
	now := o.r.ex.Mono()
	if !o.throttled {
		o.throttleFirst = now
	}
	o.throttled, o.throttleAt = true, now
	o.updateThrottle(now)
}

func (o *owner) noteRESTError(err error) {
	var throttle *rest.RateLimitError
	if errors.As(err, &throttle) {
		o.noteRESTThrottle(throttle)
	}
}

func (o *owner) retryThrottled(res writeResult) bool {
	throttle := res.Sweep.Throttle
	if throttle == nil {
		errors.As(res.Create.Err, &throttle)
	}
	if throttle == nil {
		errors.As(res.Sweep.Walk.Err, &throttle)
	}
	if throttle == nil {
		return false
	}
	o.noteRESTThrottle(throttle)
	now := o.r.ex.Mono()
	req := res.Req
	if req.Op == quote.OpPlace {
		if o.pending == nil {
			o.pending = make(map[string]pendingOrder)
		}
		if _, exists := o.pending[req.Order.ClientOrderID()]; !exists {
			o.pending[req.Order.ClientOrderID()] = pendingOrder{ticker: req.Market, side: req.Side, cents: req.Order.PriceCents(), qty: req.Order.Count(), at: now}
		}
		o.cancelConfirmed[req.Side] = false
		o.offerToken(o.reconcileOut, o.reconcileToken)
		if req.attempts >= o.p.RetrySameCoidMax {
			for _, id := range req.IDs {
				o.r.queue.Drop(id)
			}
			return true
		}
	}
	req.retryAt = retryDeadline(now, retryDelay(req.retryN, throttle, rand.Float64()))
	req.retryN++
	o.retries = append(o.retries, req)
	return true
}
func (o *owner) pumpRetry(now time.Duration, writes chan<- writeRequest) bool {
	for i, req := range o.retries {
		if req.retryAt > now {
			continue
		}
		if req.Op == quote.OpPlace && !o.retryPlacementAllowed(now, req) {
			// Retiring retry authority never retires possibly-live exposure.
			for _, id := range req.IDs {
				o.r.queue.Drop(id)
			}
			o.retries = append(o.retries[:i], o.retries[i+1:]...)
			return true
		}
		class := req.class
		if req.Op == quote.OpCancel && req.Role == quote.RoleReducing {
			class = quote.P1
		} else if req.Op == quote.OpCancel {
			class = quote.P0
		} else if req.Role == quote.RoleReducing && o.r.pf.Q(req.Market).Abs() > o.p.InvSoft {
			class = quote.P1
		} else {
			class = quote.P3
		}
		grant, ok := o.capacity.Admit(class)
		if !ok {
			continue
		}
		req.Grant = grant
		o.capacity = o.capacity.Take(grant)
		select {
		case writes <- req:
			o.inflight = append(o.inflight, &req)
			o.retries = append(o.retries[:i], o.retries[i+1:]...)
			return true
		default:
			o.capacity = releaseWrite(o.capacity, o.p, grant, false)
			return false
		}
	}
	return false
}

// A retry preserves its body but rechecks current authority before sending it.
// Incompatible inventory or a halted adding side cannot be repaired by resizing
// the old coid: stop retrying and retain its unresolved exposure instead.
func (o *owner) retryPlacementAllowed(now time.Duration, req writeRequest) bool {
	restore := o.marketContext(req.Market)
	defer restore()
	if o.hasListedCreate(req.Order.ClientOrderID(), req.Market, req.Side) {
		return false
	}
	if req.Market != o.ticker() {
		return false
	}
	if o.global == quote.Starting || o.global == quote.UnknownRisk || o.global == quote.Drained {
		return false
	}
	if req.Role == quote.RoleAdding && (!o.global.AddsRisk() || (o.market != quote.Quoting && o.market != quote.Skewed)) {
		return false
	}
	q := o.r.pf.Q(req.Market)
	reducing, held := quote.ReducingSide(q)
	if req.Role == quote.RoleReducing {
		aggregate := o.atRisk(req.Side)
		// The same coid's UNKNOWN maximum is already in pending. A retry
		// cannot create an additional order, so count that reservation once.
		if _, counted := o.pending[req.Order.ClientOrderID()]; !counted {
			aggregate += req.Order.Count()
		}
		if !held || reducing != req.Side || aggregate > q.Abs() {
			return false
		}
	} else if held && reducing == req.Side {
		return false
	}
	if !o.conditions(now).Valid(quote.Intent{Market: req.Market, Side: req.Side, Role: req.Role, Kind: quote.KindPlace}) {
		return false
	}
	// During F5 this resolves only to a fresh retained REST book and only for
	// reducers. It refuses an old coid when that source has aged out, even if
	// the websocket book still has a plausible price.
	if _, allowed := o.pricingBook(req.Role); !allowed {
		return false
	}
	other := quote.SideYes
	if req.Side == quote.SideYes {
		other = quote.SideNo
	}
	price, has := o.bestOn(other)
	return quote.CheckPlacement(req.Side, req.Order.PriceCents(), price, has) == nil && o.checkPlacementCapital(req) == nil
}

// confidence: high
