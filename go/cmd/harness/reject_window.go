package main

import (
	"fmt"

	"lip/harness/rest"
	"lip/harness/risk"
	"lip/harness/wsx"
)

const rejectWindowSize = 50

// rejectWindow counts outcomes of distinct sent creates, by market. A result
// may summarize several attempts of the same coid, and a replayed result must
// not turn those attempts into several orders. Unknown and 429 outcomes occupy
// a slot because a write was sent, but neither is a definite rejection.
type rejectWindow struct {
	markets map[string]*marketRejectWindow
}

type marketRejectWindow struct {
	// Keep coids for the session, even after they leave the ring: a delayed
	// result must never re-enter as a new order. The one-market pilot's write
	// rate makes this smaller than the live order journal for the same run.
	seen     map[string]bool // whether this coid already supplied a definite coded reject
	coids    [rejectWindowSize]string
	rejected [rejectWindowSize]bool
	n        int
	next     int
	rejects  int
	alerted  bool
}

// record returns whether this result is new information and whether it first
// crossed F9's strictly-greater-than-10-percent threshold over a complete
// 50-order window. A 429 or UNKNOWN reserves the coid's one slot; a later
// definite rejection from the same-coid retry upgrades that slot in place.
func (w *rejectWindow) record(market string, result rest.CreateResult, sent bool) (bool, bool) {
	if !sent || market == "" || result.Coid == "" {
		return false, false
	}
	if w.markets == nil {
		w.markets = make(map[string]*marketRejectWindow)
	}
	m := w.markets[market]
	if m == nil {
		m = &marketRejectWindow{seen: make(map[string]bool)}
		w.markets[market] = m
	}
	reject := result.Outcome == rest.CreateRejected &&
		result.Status >= 400 && result.Status < 500 &&
		result.Status != 429 && result.RejectReason != ""
	if alreadyRejected, already := m.seen[result.Coid]; already {
		if alreadyRejected || !reject {
			return false, false
		}
		// The retry resolved a previously uncertain outcome. The coid may
		// have left the last-50 ring during its backoff; in that case the
		// reason is still new information for F11, but F9's current window
		// must not count an order no longer in it.
		m.seen[result.Coid] = true
		for i, coid := range m.coids {
			if coid == result.Coid {
				m.rejected[i] = true
				m.rejects++
				break
			}
		}
	} else {
		m.seen[result.Coid] = reject
		if m.n == rejectWindowSize && m.rejected[m.next] {
			m.rejects--
		}
		m.coids[m.next] = result.Coid
		m.rejected[m.next] = reject
		if reject {
			m.rejects++
		}
		if m.n < rejectWindowSize {
			m.n++
		}
		m.next = (m.next + 1) % rejectWindowSize
	}
	if m.n == rejectWindowSize && m.rejects > rejectWindowSize/10 && !m.alerted {
		m.alerted = true
		return true, true
	}
	return true, false
}

// observeRejectResponse consumes the dispatcher's structured create outcome.
// It runs on the owner goroutine after the write is known to have been sent.
func (o *owner) observeRejectResponse(market string, result rest.CreateResult, sent bool) {
	accepted, threshold := o.rejects.record(market, result, sent)
	if !accepted {
		return
	}
	if threshold {
		if o.r.gate.NoteRejectRate(market) {
			o.noteReduce([]string{market})
		}
		o.r.anom.raise(risk.Anomaly{
			Class: "ORDER_REJECT_RATE", Sev: risk.SEV2, Ticker: market,
			Text: fmt.Sprintf("%d of the last %d sent creates in this market were definite 4xx rejects with an exchange reason; the market is REDUCING", o.rejects.markets[market].rejects, rejectWindowSize),
		})
	}
	if result.Outcome != rest.CreateRejected || result.Status < 400 ||
		result.Status >= 500 || result.RejectReason != "post_only_would_cross" {
		return
	}
	if o.postOnlyCrossSeen == nil {
		o.postOnlyCrossSeen = make(map[string]bool)
	}
	if o.postOnlyCrossSeen[market] {
		o.r.anom.raise(risk.Anomaly{
			Class: "POST_ONLY_WOULD_CROSS_REPEATED", Sev: risk.SEV2,
			Ticker: market,
			Text:   "the exchange repeatedly rejected post_only creates as crossing; F5's REST orderbook cross-check is required for this market",
		})
	}
	o.postOnlyCrossSeen[market] = true
	if req, ok := o.r.gate.ForceCrossCheck(market, o.r.ex.Clock.Now()); ok {
		o.requestCrossChecks([]wsx.CrossCheckRequest{req})
	}
}

// confidence: high
