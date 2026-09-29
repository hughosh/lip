package main

import (
	"fmt"

	"lip/harness/risk"
)

// storeHealthGuard belongs to the owner goroutine. The store's adding permit
// already fails closed when its writer fails or stalls; the owner must also
// record a durable stop so a restart cannot resume adding after that failure.
type storeHealthGuard struct {
	stopped bool
	// probe is an account-free test seam. Production samples Store.Health.
	probe func() storeHealthSnapshot
}

type storeHealthSnapshot struct {
	healthy, adding, stalled bool
	pending                  int
	failures                 uint64
	stalls                   uint64
	lastError                string
}

func (o *owner) checkRuntimeStore() {
	if o.sd != nil {
		o.sd.finalClose.RLock()
		defer o.sd.finalClose.RUnlock()
		// Only the successful, authorized stop can exempt a closed writer.
		// Until then an unexpected exit still stops adding and raises SEV1.
		if o.sd.orderlyClosed {
			return
		}
	}
	g := &o.storeHealth
	if g.stopped {
		return
	}
	var h storeHealthSnapshot
	if g.probe != nil {
		h = g.probe()
	} else {
		actual := o.r.store.Health()
		h = storeHealthSnapshot{
			healthy: actual.Healthy(), adding: actual.AllowsAdding(),
			stalled: actual.Stalled(), pending: actual.Pending(),
			failures: actual.Failures(), stalls: actual.Stalls(),
			lastError: actual.LastError(),
		}
	}
	// A failed attempt or observed stall can recover between owner ticks. The
	// monotonic counters retain either condition after the queue drains.
	if h.healthy && h.adding && h.failures == 0 && h.stalls == 0 {
		return
	}
	g.stopped = true
	o.r.anom.raise(risk.Anomaly{Class: "STORE_UNHEALTHY", Sev: risk.SEV1,
		Text: fmt.Sprintf("operational store failed or stalled: failures=%d observed_stalls=%d stalled=%t pending=%d error=%q; stop adding and retain observation, cancellation and permitted reduction while the halt latch is retried", h.failures, h.stalls, h.stalled, h.pending, h.lastError)})
	o.requestStop("store_unhealthy", "")
}

// confidence: high
