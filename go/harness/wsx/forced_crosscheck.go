package wsx

// ForceCrossCheck asks F5 to compare one current websocket book with REST now.
// A post_only_would_cross rejection is evidence that the exchange disagrees
// with our placement decision; it does not itself establish which book level
// is wrong. The normal F5 result path decides that from the independent read.
//
// No quiet clock is advanced here. An outstanding check already protects the
// market, and neither a disconnected nor a quarantined book can be blessed by
// agreement with REST.
func (g *Gate) ForceCrossCheck(ticker string, now Stamp) (CrossCheckRequest, bool) {
	m := g.markets[ticker]
	if m == nil || !g.BookCurrent(ticker) || m.f5 ||
		m.phase != quietNone || m.checkTries >= quietCheckMaxTries {
		return CrossCheckRequest{}, false
	}
	m.phase = quietChecking
	m.checkSeq++
	m.checkTries++
	m.quietHold = true
	return CrossCheckRequest{Ticker: ticker, Token: CrossCheckToken{
		ticker: ticker, gen: g.gen, seq: m.checkSeq, valid: true,
		requestedAt: now.Mono,
	}}, true
}

// NoteRejectRate makes F9's market reduction sticky. It returns whether this
// call newly changed the market; callers report the reject-rate anomaly at the
// threshold independently of any earlier cause of reduction.
func (g *Gate) NoteRejectRate(ticker string) bool {
	m := g.markets[ticker]
	if m == nil || m.reducing {
		return false
	}
	m.reducing = true
	return true
}

// confidence: high
