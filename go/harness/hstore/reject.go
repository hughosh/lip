package hstore

import (
	"fmt"

	"lip/harness/risk"
)

// RecordRejectedClass is §13.2's class for a record this store accepted and
// then lost.
//
// It is SEV1 for the same reason `FOREIGN_ORDER` is: the harness is no longer
// able to say what happened. A missing audit row is not a degraded log, it is a
// hole in the evidence the next decision is taken from -- a binding that is
// gone makes every fill on that order unclassifiable, a lost anomaly is an
// alert the operator will never be offered, and a lost state event is a
// transition that cannot be reconstructed afterwards.
const RecordRejectedClass = "STORE_RECORD_REJECTED"

// Rejection is the SEV1 this outcome raises, if it raises one.
//
// It is derived from `Err` and from NOTHING ELSE, and that is the point.
//
// The obvious alternative is to read the condition off `Health()`: the store
// latches a sticky `fault` on a permanent rejection and renders it into
// `LastError()`, so an unhealthy store does say that a record was lost. That
// reading is wrong in both directions.
//
//   - It UNDERCOUNTS. `fault` is one string for the whole store, overwritten by
//     each rejection, so two lost records read as one condition and the one it
//     names is whichever happened last. The other record's evidence is gone and
//     nobody is told which one it was.
//   - It has no EDGE. A sticky fault is reported for the life of the process,
//     so an alert derived from it either fires once and never repeats or
//     repeats on every poll forever. `Err != nil` is terminal BY CONSTRUCTION
//     -- the writer publishes an error only after it has stopped retrying -- so
//     it occurs exactly once per record that was lost, which is the event.
//   - It also misses the whole writer-exit path, where records are terminally
//     failed with no permanent `fault` latched at all.
//
// `Health().LastError()` remains what it is: operator text for §13.3's
// heartbeat, and not a protocol.
func (r Result) Rejection() (risk.Anomaly, bool) {
	if r.Err == nil {
		return risk.Anomaly{}, false
	}
	return risk.Anomaly{
		Class: RecordRejectedClass,
		Sev:   risk.SEV1,
		// No ticker. The loss is a property of the store, not of a market: §13
		// renders an empty ticker as `account`, which is where an operator has
		// to go looking when a record no longer exists.
		Ticker: "",
		Text: fmt.Sprintf("the %s record submitted as #%d was accepted as "+
			"durable-in-progress and is now GONE -- the store has stopped "+
			"retrying it and nothing will ever write it: %v. Adding authority "+
			"is revoked (H-STORE-3); reducing, cancelling and monitoring "+
			"continue (I1). The audit trail has a hole in it at this record "+
			"and no later success closes it",
			r.Kind, r.Receipt.Seq(), r.Err),
	}, true
}

// Rejections maps one `TakeResults` batch to the SEV1s it raises.
//
// This is the shape a result loop wants: every terminal `Result` is a record
// that is gone, so the whole of the alerting rule is "raise one per failed
// result". A caller that instead inspects `Health()` after draining its
// receipts has already lost the per-record identity, because the store keeps
// one latched fault and not a list.
func Rejections(results []Result) []risk.Anomaly {
	var out []risk.Anomaly
	for _, r := range results {
		if a, raised := r.Rejection(); raised {
			out = append(out, a)
		}
	}
	return out
}

// confidence: high
