package wsx

// ObservationGap retires every pre-gap book/read without pretending the socket
// reconnected. Host sleep can leave it open, with old REST requests in flight.
// Only newly started reads carrying the returned token may restore authority.
//
// Portfolio truth is retired, not merely aged: TruthAge runs on the monotonic
// clock, which did not advance while the host slept, so pre-gap reads would
// still look seconds old to the drain and the turnover proofs. A reconnect
// keeps truth because its REST ages are real (lip-opm).
func (g *Gate) ObservationGap(now Stamp) ReconcileToken {
	g.gen++
	g.truthOK = [truthCount]bool{}
	g.NoteSeqGap(now)
	for _, m := range g.markets {
		m.clearQuietEpisode()
		m.f5 = false
		m.restOK = false
		m.lastFrame = now.Mono
		m.quietPinged = false
	}
	return ReconcileToken{gen: g.gen, valid: true}
}

// confidence: high
