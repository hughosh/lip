package risk

import "time"

// Severity is the anomaly ping contract of §13.2.
type Severity uint8

const (
	SEV3 Severity = iota // informational; batched into the next heartbeat
	SEV2                 // degraded but the risk is managed; act today
	SEV1                 // risk state is wrong or unmanaged; act now
)

func (s Severity) String() string {
	switch s {
	case SEV1:
		return "SEV1"
	case SEV2:
		return "SEV2"
	}
	return "SEV3"
}

// Anomaly is journalled synchronously before any delivery is attempted (§13.1).
type Anomaly struct {
	Class  string
	Sev    Severity
	Ticker string
	Text   string
}

// Sample is one monitor observation of one market: the `snap` row of §15.
type Sample struct {
	Ticker string
	Snap   MarketSnap
	// Stale marks a sample produced from a source snapshot whose Seq has not
	// advanced. A stale sample is recorded -- silence is worse -- but is NOT
	// presented as current and is NOT integrated into uptime.
	Stale bool
	// SourceSeq is the Seq the sample was derived from, so a reader of the
	// table can reconstruct source advancement without trusting Stale.
	SourceSeq uint64
}

// MonitorState is the monitor's own memory across ticks. Pure: it is stepped by
// the caller's clock and never reads one.
type MonitorState struct {
	lastSeq      uint64
	lastAdvance  time.Duration
	seenAny      bool
	stalled      bool
	stallPinged  bool
	sampleCount  uint64
	staleSamples uint64
}

// StepResult is what one monitor tick produced.
type StepResult struct {
	Samples   []Sample
	Anomalies []Anomaly
	Account   AccountSnapshot
	// Stale is true when the source snapshot has not advanced for longer than
	// stallAfter. Rows and heartbeats derived from this tick are marked stale=1
	// and the tick is not integrated into uptime.
	Stale bool
	// IntegrateUptime is the inverse of Stale, stated positively because it is
	// the thing §8.3 step 4 actually does. A gated market is excluded from the
	// denominator separately, per market, by Sample.Snap.Gated.
	IntegrateUptime bool
}

// Step runs one monitor tick against a published snapshot. §8.3 steps 0-4.
//
// `now` is a monotonic reading supplied by the caller. `snap` is the pointer
// the caller read from the owner's atomic.Pointer; it may be nil before the
// first publication.
//
// This function NEVER returns an error, never signals "stop", and has no path
// that declines to produce a sample. A stalled owner is exactly when
// observation matters most, so a stall marks the output stale and keeps
// sampling -- it does not suppress it.
func (m *MonitorState) Step(now time.Duration, snap *Snapshot,
	stallAfter time.Duration) StepResult {

	var res StepResult

	if snap == nil {
		// Before the first publication there is nothing to be stale about.
		// This is not a stall: the owner has not claimed anything yet.
		return res
	}

	// Step 0 -- source advancement, before anything is derived from the
	// snapshot. A5 asserts THIS, not row freshness.
	advanced := !m.seenAny || snap.Seq != m.lastSeq
	if advanced {
		m.lastSeq = snap.Seq
		m.lastAdvance = now
		m.seenAny = true
		if m.stalled {
			m.stalled = false
			m.stallPinged = false
			res.Anomalies = append(res.Anomalies, Anomaly{
				Class: "OWNER_RESUMED", Sev: SEV2,
				Text: "owner snapshot sequence advanced again",
			})
		}
	} else if now-m.lastAdvance >= stallAfter {
		m.stalled = true
		if !m.stallPinged {
			m.stallPinged = true
			res.Anomalies = append(res.Anomalies, Anomaly{
				Class: "OWNER_STALLED", Sev: SEV1,
				Text: "owner snapshot sequence has not advanced; " +
					"monitor output is stale and is not being integrated",
			})
		}
	}

	res.Stale = m.stalled
	res.IntegrateUptime = !m.stalled
	res.Account = snap.Account
	if m.stalled {
		res.Account.TruthFresh = false
		res.Account.TradingPnL.Evaluable = false
	}

	// Steps 1-3 -- produce one sample per market, in EVERY global state. The
	// clause "in every global state" is what would have caught 2 snapshots in
	// 6.14 hours, at the first tick after the halt.
	res.Samples = make([]Sample, 0, len(snap.Markets))
	for i := range snap.Markets {
		res.Samples = append(res.Samples, Sample{
			Ticker:    snap.Markets[i].Ticker,
			Snap:      snap.Markets[i],
			Stale:     m.stalled,
			SourceSeq: snap.Seq,
		})
	}
	m.sampleCount += uint64(len(res.Samples))
	if m.stalled {
		m.staleSamples += uint64(len(res.Samples))
	}
	return res
}

// Stalled reports whether the monitor currently considers its input frozen.
func (m *MonitorState) Stalled() bool { return m.stalled }

// LastSeq is the last source sequence the monitor observed.
func (m *MonitorState) LastSeq() uint64 { return m.lastSeq }

// Counts returns (samples produced, of which stale).
func (m *MonitorState) Counts() (uint64, uint64) {
	return m.sampleCount, m.staleSamples
}

// confidence: high
