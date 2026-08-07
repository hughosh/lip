package wsx

import (
	"fmt"
	"sort"
	"time"

	"lip/harness/cfg"
	"lip/harness/risk"
)

// Truth names one of the three portfolio endpoints whose freshness A13
// requires. They are tracked SEPARATELY because they fail separately: the
// positions endpoint answering while the fills endpoint has been erroring for
// two minutes is a state in which we know our size and not what produced it,
// and a single "portfolio is fresh" flag reports that as healthy.
type Truth uint8

const (
	TruthPositions Truth = iota
	TruthOrders
	TruthFills
	truthCount
)

func (t Truth) String() string {
	switch t {
	case TruthPositions:
		return "positions"
	case TruthOrders:
		return "orders"
	case TruthFills:
		return "fills"
	}
	return "INVALID"
}

// ReconcileToken identifies the connection a reconciliation was started for.
//
// It is opaque on purpose. The failure it exists to prevent is a portfolio read
// that was issued before a disconnect and lands after the reconnect: it
// describes the account as it was on the far side of a gap, and if it were
// allowed to satisfy the post-reconnect reconciliation it would unlock
// placement against a book that has not been resnapshotted. A caller that could
// construct a token could construct that.
type ReconcileToken struct {
	gen   uint64
	valid bool
}

// Valid reports whether the token was issued by a Gate. The zero value is not.
func (t ReconcileToken) Valid() bool { return t.valid }

// marketGate is one market's half of the gate.
type marketGate struct {
	// snapGen is the connection generation that delivered this market's most
	// recent snapshot. Zero means never.
	snapGen uint64
	// quarantined blocks placement regardless of everything else.
	quarantined bool
	// lastFrame is when this market last delivered any book frame, monotonic.
	lastFrame time.Duration
	// quietPinged stops BOOK_QUIET repeating every tick for a market that is
	// simply closed for the night.
	quietPinged bool
	// reducing is sticky. Nothing in this package clears it: recovering a
	// socket is not evidence that the risk taken during the outage is gone,
	// and a market that silently resumed quoting on reconnect would be the
	// harness deciding that for itself.
	reducing bool
}

// Gate is A13, evaluated: "no placement decision is taken from a book that is
// quarantined or stale, or from portfolio truth older than truth_max_age_s".
//
// Every field is private and there is deliberately no setter. A gate that could
// be forced open is not a gate -- and the specific shape that invites it, a
// SetActionable(bool) for "just this one case at startup", is how the
// pre-reconciliation placement H-ORD-5 forbids gets written.
type Gate struct {
	p       cfg.Params
	tickers []string
	markets map[string]*marketGate

	// gen advances on every connection. Everything the previous connection
	// established is identified by an older generation and is therefore
	// invalid without anything having to be erased.
	gen       uint64
	connected bool

	// downSince is when the current disconnected episode began, monotonic.
	downSince time.Duration
	// downReduced records that this episode has already produced its one
	// reduce effect, so the SEV1 does not repeat every tick for an hour.
	downReduced bool

	truthGen [truthCount]uint64
	truthAt  [truthCount]time.Duration
	truthOK  [truthCount]bool
}

// NewGate builds the gate for a fixed market set.
//
// It starts DISCONNECTED and non-actionable. That is not a formality: a gate
// that defaulted to open would authorise placement in the window between
// process start and first connection, which is precisely the window H-ORD-5
// reserves for reconciliation.
func NewGate(tickers []string, p cfg.Params) (*Gate, error) {
	if err := ValidateTickers(tickers); err != nil {
		return nil, err
	}
	g := &Gate{
		p:       p,
		tickers: append([]string(nil), tickers...),
		markets: make(map[string]*marketGate, len(tickers)),
	}
	for _, t := range tickers {
		g.markets[t] = &marketGate{}
	}
	return g, nil
}

// Tickers returns the market set, in subscription order.
func (g *Gate) Tickers() []string { return append([]string(nil), g.tickers...) }

// Generation is the current connection generation. Exposed for diagnostics and
// for tests that need to assert a token was invalidated rather than merely
// unused.
func (g *Gate) Generation() uint64 { return g.gen }

// Connected reports whether a session is currently up.
func (g *Gate) Connected() bool { return g.connected }

// Reducing reports the sticky per-market REDUCING flag this gate has raised.
func (g *Gate) Reducing(ticker string) bool {
	m := g.markets[ticker]
	return m != nil && m.reducing
}

// ---------------------------------------------------------------------------
// Connection lifecycle
// ---------------------------------------------------------------------------

// ConnectEffects is what a new connection licenses and requires.
type ConnectEffects struct {
	// Token identifies this connection. Every portfolio read taken for this
	// connection must carry it.
	Token ReconcileToken
	// Resnapshot is always true: a new connection has no books.
	Resnapshot bool
	Anomalies  []risk.Anomaly
}

// OnConnect advances the generation and issues the reconciliation token.
//
// Advancing the generation is the whole invalidation mechanism. Nothing is
// erased -- the old snapshot marks and the old truth marks are still in the
// struct -- but every one of them now carries a generation that is not the
// current one, so no combination of them can satisfy Actionable. Erasure would
// be equivalent and would have a failure mode this does not: a field that
// someone later forgets to clear.
func (g *Gate) OnConnect(now Stamp) ConnectEffects {
	g.gen++
	g.connected = true
	g.downReduced = false
	for _, m := range g.markets {
		// The quiet clock restarts with the connection. A market that was
		// silent before the gap has not been silent on THIS connection, and
		// charging it for the outage would reduce every market on every
		// reconnect.
		m.lastFrame = now.Mono
		m.quietPinged = false
	}
	return ConnectEffects{
		Token:      ReconcileToken{gen: g.gen, valid: true},
		Resnapshot: true,
	}
}

// DisconnectEffects is what a disconnect requires of the caller.
type DisconnectEffects struct {
	// Token is a VALID token for the disconnected generation, and it is the
	// thing that keeps position monitoring alive through an outage.
	//
	// Without it the generation bump below would silently blind the poller.
	// The poller carries the token it was last given; that token belongs to the
	// connection that just died, so every read it completes during the outage
	// would be discarded as stale -- REST requests still going out, answers
	// still coming back, and `q` frozen at whatever it was when the socket
	// dropped. Resting orders keep filling against that frozen number. That is
	// probebot.py's observable -- confident silence about live risk -- reached
	// through the freshness machinery instead of through a `break`.
	//
	// A read taken under THIS token updates positions, orders, fills and their
	// freshness clocks. It cannot make anything actionable, because Actionable
	// requires `connected` and a snapshot from the current generation, and an
	// outage has neither.
	//
	// The caller relays it to the poller, which polls immediately.
	Token ReconcileToken

	// ResetBooks is true when `core.Rig.ResetOnReconnect` must be called.
	//
	// It is false for a CLEAN close, and that is a diagnostics decision and
	// not a safety one: the old levels are kept so an operator can look at
	// what the market was doing when the exchange closed the socket. It
	// authorises nothing either way -- every market lost its snapshot
	// generation the moment this ran.
	ResetBooks bool
	Anomalies  []risk.Anomaly
}

// ApplyDisconnect is H-FAIL-5: a disconnect makes the book non-actionable,
// however clean it was.
//
// The shadow rig treats a clean close as a non-event and does not reset its
// books (port-spec P25a), which is correct for a measurement rig and fatal for
// a trading one. A clean close is still a gap: whatever happened to the book
// while we were not connected is unknown, and "the exchange said goodbye
// politely" is not evidence about depth.
//
// # What `clean` decides, and what it does not
//
// BOTH close classes advance the generation, and after either one EVERY book is
// non-actionable. That is unconditional and `clean` cannot reach it.
//
// What `clean` decides is two narrower things:
//
//   - the RECONNECT DELAY, in the supervisor: a clean close reconnects
//     immediately and an abnormal one climbs the backoff ladder;
//   - whether `core.Rig.ResetOnReconnect` is called. A clean close keeps the
//     old levels so an operator can see what the market was doing when the
//     socket closed; an abnormal one discards them. Neither authorises
//     anything, because both markets are already unqualified.
//
// # What the generation bump does and does not invalidate
//
// It retires every SNAPSHOT and every reconciliation taken on the dead
// connection: those described a world we were watching and are not. It must NOT
// retire the ability to keep reading the portfolio, which is why a token for
// the new generation is handed back in the same breath. Book truth comes from
// the socket and dies with it; POSITION truth comes from REST and does not.
// Conflating the two is what made the first implementation of this method
// blind for the length of an outage.
func (g *Gate) ApplyDisconnect(now Stamp, clean bool) DisconnectEffects {
	if !g.connected {
		return DisconnectEffects{}
	}
	g.connected = false
	g.downSince = now.Mono
	g.downReduced = false

	// Retire the snapshots and the reconciliations of the dead connection.
	g.gen++

	kind := "abnormal"
	if clean {
		kind = "clean"
	}
	return DisconnectEffects{
		Token:      ReconcileToken{gen: g.gen, valid: true},
		ResetBooks: !clean,
		Anomalies: []risk.Anomaly{{
			Class: "WS_DISCONNECT", Sev: risk.SEV2,
			Text: fmt.Sprintf("the websocket disconnected (%s); every book is "+
				"non-actionable until it has been resnapshotted on the new "+
				"connection AND positions, orders and fills have each "+
				"reconciled after it (H-FAIL-5). Portfolio polling continues "+
				"under a token for the disconnected generation", kind),
		}},
	}
}

// ---------------------------------------------------------------------------
// Frame-driven state
// ---------------------------------------------------------------------------

// FrameEffects is what one inbound frame did to the gate.
type FrameEffects struct {
	// Delivered reports whether the frame actually reached core -- that is,
	// whether ApplyFrame invoked the handler AND the handler returned nil.
	//
	// It is a REPORT of what happened, not a permission for the caller to do
	// it afterwards. ApplyFrame owns the delivery, because the previous shape
	// -- return a verdict, let the caller call core -- made the gate's book
	// bookkeeping and core's acceptance of the frame two separate events that
	// could disagree, and the disagreement certified a book core had refused.
	Delivered  bool
	Reduce     []string
	Resnapshot bool
	Anomalies  []risk.Anomaly
}

// ApplyFrame delivers one inspected frame to core and folds the OUTCOME into
// the gate.
//
// A book frame that never reaches the book makes a previously QUALIFIED market
// unqualified. See quarantineRejectedBook: not certifying a refused frame is
// only half the rule, and it is the half that does nothing for the market that
// was licensed to place a millisecond ago.
//
// `handle` invokes `core.Rig.Handle` exactly once. It is a function and not a
// `*core.Rig` so that this package keeps no book state and the scenario
// exchange can substitute.
//
// # Why delivery and bookkeeping are one transaction
//
// H-CO-3a's assertion runs BEFORE the handler: a fractional level must never
// enter a Book, because once it has, the touch it implies is already wrong and
// quarantining afterwards only stops us compounding it.
//
// But passing that assertion is not the same as core ACCEPTING the frame. Core
// rejects a snapshot whose sizes do not parse, a delta missing its required
// fields, a message it cannot decode -- and it rejects the WHOLE frame, leaving
// the book exactly as it was. If the gate marked the market snapshotted anyway,
// the sequence is:
//
//	clean disconnect (the old book is retained for diagnostics)
//	 -> reconnect -> a snapshot arrives with a malformed size
//	 -> core refuses it, the book is still the PRE-DISCONNECT one
//	 -> the gate records snapGen = current, quarantine lifted
//	 -> we quote against a book from the far side of the gap
//
// So `snapGen`, the quarantine and the quiet clock all move only after the
// handler returns nil. A rejected book frame leaves the market exactly as
// unqualified as it was, requests a resnapshot, and says so.
//
// It deliberately does NOT reduce on a rejection. One malformed frame is a
// data-quality event a resnapshot repairs within a round trip; if the market
// does not recover, its quiet clock is not being refreshed either, and F5
// reduces it on the existing path rather than on a second, parallel one.
func (g *Gate) ApplyFrame(info FrameInfo, handle func() error,
	now Stamp) FrameEffects {

	var eff FrameEffects
	eff.Anomalies = append(eff.Anomalies, info.Anomalies...)

	m := g.markets[info.Ticker]

	if info.Granularity {
		// Never reaches core. This is the one class where the frame is
		// structurally unsafe rather than merely unparsed.
		if m != nil {
			m.quarantined = true
			if !m.reducing {
				m.reducing = true
				eff.Reduce = append(eff.Reduce, info.Ticker)
			}
		}
		return eff
	}
	if !info.Deliver {
		// Undecodable, or otherwise uncertifiable. Not delivered, and nothing
		// about any book changes.
		return eff
	}
	if handle == nil {
		// Fail closed. A deliverable frame with nowhere to deliver it is a
		// wiring defect, and the safe reading of "core never saw it" is that
		// no book moved -- not that the market may be certified as current.
		eff.Anomalies = append(eff.Anomalies, risk.Anomaly{
			Class: "FRAME_HANDLER_MISSING", Sev: risk.SEV1, Ticker: info.Ticker,
			Text: "a deliverable frame arrived with no core handler wired; " +
				"nothing was delivered and no book may be treated as current",
		})
		if isBookFrame(info.Kind) {
			quarantineRejectedBook(m, info.Kind)
			eff.Resnapshot = true
		}
		return eff
	}

	if err := handle(); err != nil {
		eff.Anomalies = append(eff.Anomalies, risk.Anomaly{
			Class: "CORE_BOOK_FRAME_REJECTED", Sev: risk.SEV2,
			Ticker: info.Ticker,
			Text: fmt.Sprintf("core refused a %s frame (%v); it left the book "+
				"untouched, so this market is NOT current and its silence "+
				"clock does not restart", info.Kind, err),
		})
		if isBookFrame(info.Kind) {
			quarantineRejectedBook(m, info.Kind)
			eff.Resnapshot = true
		}
		return eff
	}
	eff.Delivered = true

	if m == nil {
		// A frame for a market we do not manage. The unfiltered `trade`
		// subscription delivers the whole exchange tape, so this is the common
		// case and not an anomaly.
		return eff
	}

	switch info.Kind {
	case FrameSnapshot:
		m.lastFrame = now.Mono
		m.quietPinged = false
		// The snapshot is what lifts the quarantine, and only for the
		// connection that delivered it -- and only now that core has it.
		m.snapGen = g.gen
		m.quarantined = false
	case FrameDelta:
		m.lastFrame = now.Mono
		m.quietPinged = false
	}
	return eff
}

// quarantineRejectedBook REVOKES a market's placement licence because a book
// frame addressed to it never reached the book.
//
// This is the half that is easy to leave out, because the obvious reading of a
// rejected frame is "do not certify it" -- which only closes the gate for a
// market that was already closed. The dangerous case is the market that is
// currently LICENSED: it was actionable a millisecond ago, a delta arrived that
// core refused, and the book is now known to be behind by exactly that delta.
// Leaving the licence in place means quoting against a book we have already
// been told is wrong, until a resnapshot lands or `quiet_s` elapses -- up to a
// minute. At S = 12 that is one more order at up to 99c.
//
// Clearing `snapGen` as well as setting `quarantined` is what makes only an
// accepted SNAPSHOT able to reopen the market. A delta is an increment against
// a book we have decided not to trust; replaying increments onto it cannot
// make it right, and only a full replacement can.
//
// It deliberately does not touch `lastFrame`. A market whose every frame core
// refuses is not publishing anything we can use, and its silence clock must
// keep running so F5 reduces it on the existing path rather than on a second,
// parallel one.
func quarantineRejectedBook(m *marketGate, kind FrameKind) {
	if m == nil || !isBookFrame(kind) {
		return
	}
	m.quarantined = true
	m.snapGen = 0
}

// NoteSeqGap records `core.Rig`'s subscription-wide sequence gap.
//
// The gap is subscription-wide and not per market: a gap means SOME market lost
// a delta and nothing in the data says which. Every book on that subscription
// is therefore suspect. That is why this quarantines all of them rather than
// the market whose frame happened to carry the gap.
//
// It does NOT reduce. A gap is a data-quality event that a resnapshot repairs
// within one round trip, and reducing every market on every gap would convert
// an ordinary exchange hiccup into a full unwind.
func (g *Gate) NoteSeqGap(now Stamp) FrameEffects {
	for _, m := range g.markets {
		m.quarantined = true
		m.snapGen = 0
	}
	return FrameEffects{
		Resnapshot: true,
		Anomalies: []risk.Anomaly{{
			Class: "BOOK_SEQ_GAP", Sev: risk.SEV2,
			Text: "a subscription-wide sequence gap means some market lost a " +
				"delta and nothing says which, so every book is quarantined " +
				"until it has been resnapshotted",
		}},
	}
}

// ---------------------------------------------------------------------------
// Portfolio truth
// ---------------------------------------------------------------------------

// noteTruth records a successful read of one portfolio endpoint.
//
// It returns false when the token is not the current one, and in that case
// records NOTHING. That is the "discarded wholesale" rule: a read issued
// before a disconnect describes the account across a gap, and accepting even
// its timestamp would move the freshness clock forward on evidence that
// predates the reconnect. Half-accepting a stale reconciliation is worse than
// rejecting it, because the resulting state looks reconciled.
//
// It is PRIVATE, and that is the whole of the "no escape hatch" rule. An
// exported version is a `SetActionable(true)` with a longer name: three calls
// to it and one snapshot and the gate is open, with no complete walk anywhere
// in the story. `ApplyPortfolio` is the only production path that advances
// truth, and it advances it only from a walk that reported WalkComplete.
func (g *Gate) noteTruth(kind Truth, tok ReconcileToken, now Stamp) bool {
	if kind >= truthCount {
		return false
	}
	if !tok.valid || tok.gen != g.gen {
		return false
	}
	g.truthGen[kind] = g.gen
	g.truthAt[kind] = now.Mono
	g.truthOK[kind] = true
	return true
}

// TruthAge is how long ago one endpoint last succeeded, or -1 if never.
func (g *Gate) TruthAge(kind Truth, now Stamp) time.Duration {
	if kind >= truthCount || !g.truthOK[kind] {
		return -1
	}
	return now.Mono - g.truthAt[kind]
}

// ---------------------------------------------------------------------------
// A13
// ---------------------------------------------------------------------------

// Actionable is the predicate A13 names. It answers exactly one question: may a
// PLACEMENT decision be taken for this market right now?
//
// Every clause below has to hold, and each closes a different hole:
//
//   - connected, because a decision taken while the socket is down is taken
//     from a book nothing is updating;
//   - not quarantined, which covers the fractional-price and sequence-gap
//     cases;
//   - a snapshot from THIS connection, so a book carried across a gap cannot
//     authorise anything (H-FAIL-5);
//   - positions, orders AND fills each reconciled after this connection began,
//     which is H-ORD-5's complete reconciliation and not a subset of it;
//   - none of the three older than truth_max_age_s (H-FAIL-4).
//
// It deliberately does NOT distinguish adding from reducing. A13 is about the
// quality of the evidence, and evidence too poor to price an entry is too poor
// to price an exit at a particular price. What survives a false answer here is
// the resting reducer that is already on the book -- H-FAIL-4 stops DISPATCH,
// it does not cancel anything, which is exactly why RUNNING does not fall into
// UNKNOWN_RISK when a poll fails.
//
// CANCELLING IS NOT GATED BY THIS. See CancelPermitted.
func (g *Gate) Actionable(ticker string, now Stamp) bool {
	m := g.markets[ticker]
	if m == nil || !g.connected || m.quarantined {
		return false
	}
	if m.snapGen != g.gen {
		return false
	}
	for k := Truth(0); k < truthCount; k++ {
		if !g.truthOK[k] || g.truthGen[k] != g.gen {
			return false
		}
		if now.Mono-g.truthAt[k] > g.p.TruthMaxAge {
			return false
		}
	}
	return true
}

// CancelPermitted is always true, and the constancy is the property.
//
// Every stop path in this system stops ADDING risk; none of them stops reducing
// it and none of them stops watching it (I1). A cancel is how a reducing market
// gets its adding side off the book, how H-CLOSE-3's final cancel runs, and how
// a stranded order is retired. A gate that withheld cancels when truth went
// stale would leave orders resting precisely because we had stopped being able
// to see them -- which is probebot.py's failure reached by a new route.
//
// The arguments are accepted and ignored so that a caller cannot conclude the
// answer depends on something it forgot to pass, and so that a mutation that
// makes this consult the gate is a compiling, testable mutation rather than a
// signature change.
//
// **It is a policy sentinel and NOT proof that any caller behaves this way.**
// Nothing in this package dispatches a cancel. The property that a real cancel
// and a real sweep proceed while `Actionable` is false has to be demonstrated
// where cancels are actually issued, which is `lip-3af`.
func (g *Gate) CancelPermitted(ticker string, now Stamp) bool { return true }

// ---------------------------------------------------------------------------
// The clock-driven checks
// ---------------------------------------------------------------------------

// TickEffects is what the periodic gate evaluation produced.
type TickEffects struct {
	Reduce     []string
	Resnapshot bool
	Anomalies  []risk.Anomaly
	// Stop requests global WINDING_DOWN. Nothing in this file sets it; it
	// exists so the caller's effect handling is uniform across the package.
	Stop bool
}

// Tick runs the ONE clock-driven check this type owns: per-market silence (F5).
//
// F4's disconnect duration is deliberately NOT here. The supervisor owns the
// reconnect clock and is the only thing that knows when an outage began, so it
// detects the threshold and emits the event; this type records the consequence
// through NoteDisconnectSustained. Two independent detectors of one threshold,
// each with its own clock, is a way for a market to reduce twice or for each
// detector to assume the other did it.
//
// It is called by the caller's loop rather than by a timer of its own, because
// this type reads no clock -- which is what lets a test drive an hour of
// silence in one call and a scenario replay it exactly.
func (g *Gate) Tick(now Stamp) TickEffects {
	var eff TickEffects

	// F5 -- a market silent for strictly more than quiet_s while the socket is
	// healthy. Only while connected: while the socket is down every market is
	// silent by construction, and reporting that as N separate wedged-feed
	// anomalies would bury the one disconnect that explains all of them.
	if g.connected {
		for _, t := range g.tickers {
			m := g.markets[t]
			if now.Mono-m.lastFrame <= g.p.Quiet {
				continue
			}
			eff.Resnapshot = true
			if !m.reducing {
				m.reducing = true
				eff.Reduce = append(eff.Reduce, t)
			}
			m.quarantined = true
			if !m.quietPinged {
				m.quietPinged = true
				eff.Anomalies = append(eff.Anomalies, risk.Anomaly{
					Class: "BOOK_QUIET", Sev: risk.SEV2, Ticker: t,
					Text: fmt.Sprintf("no book frame for %v while the socket "+
						"is healthy, past quiet_s %v; the book is quarantined "+
						"and a resnapshot is requested. H-FAIL-6's full-depth "+
						"comparison is deferred, so this reduces rather than "+
						"trying to prove the book right",
						now.Mono-m.lastFrame, g.p.Quiet),
				})
			}
		}
	}

	sort.Strings(eff.Reduce)
	return eff
}

// NoteDisconnectSustained records F4: the socket has been down for
// disconnect_reduce_s.
//
// The DETECTION lives in the supervisor, which owns the reconnect clock and is
// the only thing that knows when the episode began. This method is the
// recording half. Two independent detectors of the same threshold, each with
// its own clock, is a way to have the market reduce twice, or -- worse -- for
// each to believe the other did it.
//
// The reduction is STICKY. Nothing in this package clears it, and reconnecting
// does not: a socket coming back is evidence about the socket, not about
// whatever happened to our inventory while nobody was watching it.
func (g *Gate) NoteDisconnectSustained(down time.Duration, now Stamp) TickEffects {
	var eff TickEffects
	if g.downReduced {
		return eff
	}
	g.downReduced = true
	for _, t := range g.tickers {
		m := g.markets[t]
		if !m.reducing {
			m.reducing = true
			eff.Reduce = append(eff.Reduce, t)
		}
	}
	eff.Anomalies = append(eff.Anomalies, risk.Anomaly{
		Class: "WS_DISCONNECT_SUSTAINED", Sev: risk.SEV1,
		Text: fmt.Sprintf("the websocket has been down for %v, past "+
			"disconnect_reduce_s %v; every market is sent to REDUCING and "+
			"stays there after recovery, because a reconnect is evidence "+
			"about the socket and not about the risk taken while it was down",
			down, g.p.DisconnectReduce),
	})
	sort.Strings(eff.Reduce)
	return eff
}

// confidence: high
