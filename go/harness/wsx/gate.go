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

// CrossCheckToken identifies one outstanding F5 REST cross-check.
//
// It is opaque for the same reason ReconcileToken is, and against a sharper
// race. The cross-check is a REST read issued because a book went silent; it
// completes tens or hundreds of milliseconds later, and in that window the
// websocket may have delivered a snapshot, the socket may have cycled, or an
// earlier check may already have replaced the book. A result carrying a token
// from before any of those describes a book that no longer exists -- and F5's
// two outcomes are "reset the staleness clock" and "replace the book and reduce
// the market", so crediting a stale one either blesses a book nobody checked or
// overwrites a fresh book with an older one. A caller that could construct a
// token could do both.
type CrossCheckToken struct {
	requestedAt time.Duration
	ticker      string
	gen         uint64
	seq         uint64
	valid       bool
}

// Valid reports whether the token was issued by a Gate. The zero value is not.
func (t CrossCheckToken) Valid() bool { return t.valid }

// Ticker is the market this check was issued for. It is exposed because the
// caller has to know which book to read; it authorises nothing on its own.
func (t CrossCheckToken) Ticker() string { return t.ticker }

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
	// pnlMarkAt is when this market last delivered a book frame that core
	// ACCEPTED, monotonic. It is H-HALT-5's mark clock and it is deliberately
	// NOT `lastFrame`.
	//
	// The two answer different questions and must not be conflated. F5 asks
	// "is this market still talking to us", so `lastFrame` is seeded at
	// connect (there is no silence yet) and keeps running through frames core
	// REFUSES, because a market whose every frame is rejected is exactly the
	// silence F5 exists to reduce. H-HALT-5 asks "is the price I am about to
	// value inventory at real", and neither of those answers it: a connection
	// that has said nothing has no price, and a rejected frame is a price core
	// declined to believe. Valuing a position against either is how a loss
	// floor fires late, or fails to fire at all.
	pnlMarkAt time.Duration
	// pnlMarkGen is the connection generation that established `pnlMarkAt`.
	// Zero means no mark. It follows `snapGen`'s convention rather than
	// `lastFrame`'s: invalidation is by generation, so a disconnect retires
	// the mark without anything having to be erased.
	pnlMarkGen uint64
	// reducing is sticky. Nothing in this package clears it: recovering a
	// socket is not evidence that the risk taken during the outage is gone,
	// and a market that silently resumed quoting on reconnect would be the
	// harness deciding that for itself.
	reducing bool

	// --- F5's cross-check episode -------------------------------------------

	// quietHold is F5's TEMPORARY closure, and it is deliberately not
	// `quarantined`.
	//
	// A market whose feed has gone silent past `quiet_s` is one we are about to
	// ask the exchange about over REST, and until the answer lands we do not
	// know whether its book is stale or merely quiet. Placement stops for the
	// length of that round trip -- `Actionable` reads this -- because an order
	// priced from a book we have just decided to check is an order priced from
	// evidence we have said we do not trust.
	//
	// It is a separate field because AGREEMENT clears it and must clear NOTHING
	// ELSE: a market carrying a sequence-gap quarantine or a sticky REDUCING
	// from an earlier outage stays exactly as closed as it was, and a single
	// `quarantined` flag would have made a successful cross-check silently lift
	// both.
	quietHold bool
	// phase is where this market's F5 episode has got to. See quietPhase.
	phase quietPhase
	// checkSeq identifies the outstanding cross-check WITHIN a generation. A
	// result carrying an older seq describes a book we have already replaced or
	// re-snapshotted, and crediting it would reset the staleness clock of a book
	// it never looked at.
	checkSeq uint64
	// checkTries counts the requests made in THIS episode, and it is what bounds
	// the retry. An unanswerable endpoint must not produce one request per owner
	// tick for the life of the process.
	checkTries  int
	f5          bool
	f5At        time.Duration
	f5Escalated bool
	restAt      time.Duration
	restOK      bool
	refreshAt   time.Duration
}

// quietPhase is where one market's F5 episode has got to.
//
// It exists so that "one outstanding check" is a state and not a convention. A
// bare boolean would have to mean both "a check is in flight" and "this episode
// is over", and the two behave oppositely at the next Tick: the first must not
// re-request, and the second must not re-request EVER, and a single flag that
// gets cleared on a result makes the settled case re-request on the following
// tick forever.
type quietPhase uint8

const (
	// quietNone is no episode, or an episode whose next request is due.
	quietNone quietPhase = iota
	// quietChecking is one cross-check outstanding.
	quietChecking
	// quietSettled is an episode that has been decided against the book: it has
	// already quarantined and reduced, so there is nothing left for another
	// round trip to establish.
	quietSettled
)

// quietCheckMaxTries bounds F5's retry within one silent episode.
//
// Three, and small on purpose. The retry is for a transport that dropped one
// answer, not for an endpoint that is down: a shared DNS, routing, TLS or
// credential failure takes REST out along with the websocket (H-FAIL-2), and in
// that world every further request is a request nobody will answer. What the
// harness does when the tries run out is what a market whose book cannot be
// verified deserves -- quarantine and REDUCING -- and it should reach that in
// seconds rather than after an unbounded argument with the network.
const quietCheckMaxTries = 3

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
	// universeRevision matches Supervisor's revision for a subscribed socket.
	universeRevision uint64
	dynamicUniverse  bool

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

// NewGate builds the gate for an initial market set.
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
		p:                p,
		tickers:          append([]string(nil), tickers...),
		markets:          make(map[string]*marketGate, len(tickers)),
		universeRevision: 1,
	}
	for _, t := range tickers {
		g.markets[t] = &marketGate{}
	}
	return g, nil
}

// Tickers returns the market set, in subscription order.
func (g *Gate) Tickers() []string { return append([]string(nil), g.tickers...) }

// AddMarkets extends the managed universe without retiring held markets.
// The owner must pass the same additions to Supervisor.AddMarkets. Any actual
// addition retires the current generation immediately: frames and REST reads
// from the old subscription cannot certify the expanded universe. The new
// markets remain closed until a new connection snapshots them and all three
// portfolio truths reconcile under that connection's token. An addition
// returns RefreshUniverse's token; no addition returns an invalid one.
func (g *Gate) AddMarkets(now Stamp, tickers []string) (ReconcileToken, error) {
	if err := ValidateTickers(tickers); err != nil {
		return ReconcileToken{}, err
	}
	added := false
	for _, ticker := range tickers {
		if _, ok := g.markets[ticker]; ok {
			continue
		}
		g.markets[ticker] = &marketGate{lastFrame: now.Mono}
		g.tickers = append(g.tickers, ticker)
		added = true
	}
	if added {
		return g.RefreshUniverse(now), nil
	}
	return ReconcileToken{}, nil
}

// RefreshUniverse retires all book and portfolio authority when the owner
// rebuilds core for changed target sizes while the ticker set is unchanged.
// The owner must call Supervisor.RefreshUniverse in the same owner turn.
//
// The returned token is for the new generation, and the owner relays it to the
// poller as it relays ApplyDisconnect's. The gate is left disconnected, so a
// failed re-dial reports a disconnect that mints nothing; without this token
// the poller keeps the retired one and every read is discarded as stale until
// a connection succeeds. Like the disconnect token, it cannot make anything
// actionable: that needs a new connection and its snapshot.
func (g *Gate) RefreshUniverse(now Stamp) ReconcileToken {
	g.gen++
	g.universeRevision++
	g.dynamicUniverse = true
	g.connected = false
	for _, m := range g.markets {
		m.clearQuietEpisode()
		m.lastFrame = now.Mono
	}
	return ReconcileToken{gen: g.gen, valid: true}
}

// UniverseRevision identifies the market set, independently of socket
// generations. Events from an earlier subscription must be dropped before
// their frame handler reaches core.
func (g *Gate) UniverseRevision() uint64 { return g.universeRevision }

// AcceptsUniverse reports whether an event belongs to the current market set.
func (g *Gate) AcceptsUniverse(revision uint64) bool {
	// Existing single-market callers construct Event values without a
	// revision. That legacy zero is accepted only before dynamic turnover.
	if revision == 0 && !g.dynamicUniverse {
		return true
	}
	return revision == g.universeRevision
}

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
	if g.dynamicUniverse {
		// After expansion the caller must use the revision-bearing event API.
		// A buffered connection event for the prior subscription can no
		// longer certify any book.
		return ConnectEffects{}
	}
	return g.onConnect(now)
}

// OnConnectForUniverse accepts only a connection carrying this gate's current
// subscription revision. The owner should check every EventFrame with
// AcceptsUniverse before passing its bytes to core or ApplyFrame.
func (g *Gate) OnConnectForUniverse(now Stamp, revision uint64) (ConnectEffects, bool) {
	if !g.AcceptsUniverse(revision) {
		return ConnectEffects{}, false
	}
	return g.onConnect(now), true
}

func (g *Gate) onConnect(now Stamp) ConnectEffects {
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
		// So does F5's episode. Any cross-check outstanding across the gap was
		// issued about the previous connection's book, and the generation bump
		// above has already retired its token; clearing the phase is what lets a
		// market that goes silent on THIS connection ask again.
		m.clearQuietEpisode()
		m.f5 = false
		m.restOK = false
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
	if g.dynamicUniverse {
		return FrameEffects{}
	}
	return g.applyFrame(info, handle, now)
}

// ApplyFrameForUniverse refuses old subscribed frames before their handler can
// mutate core's books. It is required after the first dynamic addition.
func (g *Gate) ApplyFrameForUniverse(info FrameInfo, handle func() error,
	now Stamp, revision uint64) FrameEffects {
	if !g.AcceptsUniverse(revision) || !g.connected {
		return FrameEffects{}
	}
	return g.applyFrame(info, handle, now)
}

func (g *Gate) applyFrame(info FrameInfo, handle func() error,
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
		// An ACCEPTED book frame ends the silence, so it ends F5's episode and
		// invalidates any cross-check outstanding for it. A REST answer that
		// lands after this describes the book as it was BEFORE this frame, and
		// F5 would either reset a staleness clock this frame has already reset
		// or replace a fresh book with an older one.
		m.clearQuietEpisode()
		// The snapshot is what lifts the quarantine, and only for the
		// connection that delivered it -- and only now that core has it.
		m.snapGen = g.gen
		m.quarantined = false
		g.finishF5(m)
		// A snapshot is a whole book, so it ESTABLISHES the mark clock
		// (H-HALT-5). This is reachable only past `handle()` returning nil,
		// which is the one moment a frame is known to have been accepted.
		m.pnlMarkAt = now.Mono
		m.pnlMarkGen = g.gen
	case FrameDelta:
		m.lastFrame = now.Mono
		m.quietPinged = false
		// The same invalidation a snapshot performs, and for the same reason:
		// the market is talking again, which is the whole of the condition F5
		// opened its episode on.
		if !m.f5 {
			m.clearQuietEpisode()
		}
		// A delta REFRESHES the mark, and only against a book this generation
		// has already snapshotted. An increment applied to a book we never
		// received in full does not make the resulting price current; it makes
		// it an unknown base plus a known edit, which is not a mark.
		if m.pnlMarkGen == g.gen {
			m.pnlMarkAt = now.Mono
		}
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
// clearQuietEpisode ends this market's F5 episode and invalidates whatever
// cross-check it had outstanding.
//
// It clears the TEMPORARY closure and nothing else. `quarantined`, `snapGen`,
// `pnlMarkGen` and the sticky `reducing` are all untouched: they record
// conclusions reached about the book and about the risk, and the fact that a
// market has started talking again is not evidence against any of them.
func (m *marketGate) clearQuietEpisode() {
	m.phase = quietNone
	m.quietHold = false
	m.checkTries = 0
	// The seq is NOT reset. It only ever advances, so a token issued before this
	// call can never be matched by a later episode's -- which is the property
	// that makes a late REST answer unusable rather than merely unlikely.
}

func quarantineRejectedBook(m *marketGate, kind FrameKind) {
	if m == nil || !isBookFrame(kind) {
		return
	}
	m.quarantined = true
	m.snapGen = 0
	// The mark goes with the snapshot. `lastFrame` deliberately survives here
	// so F5's silence clock keeps running; the mark must NOT, because the
	// rejected frame is precisely a price core declined to accept.
	m.pnlMarkGen = 0
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
		// Subscription-wide, so every mark goes too: the gap means SOME market
		// lost a delta and nothing says which, and a mark that might be
		// missing an increment is a price we cannot value inventory against.
		m.pnlMarkGen = 0
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
	for _, m := range g.markets {
		g.finishF5(m)
	}
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
//   - not under F5's quiet hold, which is the window between a book going
//     silent past `quiet_s` and the REST cross-check answering. The book has
//     not been shown wrong yet, and it has not been shown right either, and
//     the one thing that must not happen in between is a new order priced from
//     it;
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
	if m == nil || !g.connected || m.quarantined || m.quietHold || m.f5 {
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

// BookCurrent reports whether this market's BOOK is the one THIS connection
// published: connected, not quarantined, and snapshotted on this generation.
//
// It is deliberately NOT `Actionable`, and the difference is the whole reason it
// exists. `Actionable` answers "may a PLACEMENT decision be taken", which folds
// in H-ORD-5's reconciliation and H-FAIL-4's `truth_max_age_s`. This answers a
// narrower question -- "is the depth I am about to judge the depth this
// connection published" -- and H-Q-4a needs exactly that one, because it times a
// continuous gate failure from BOOK EVIDENCE.
//
// Conflating the two fails in both directions, and the second is the dangerous
// one:
//
//   - A portfolio endpoint that has been erroring for a minute makes `Actionable`
//     false while the socket keeps delivering perfectly good depth. Charging that
//     interval to the qualifying walk would start an H-Q-4a episode -- and cancel
//     an adding order -- for a reason that has nothing to do with the book.
//   - Worse, a truth outage would CLEAR one. If book evidence stopped arriving in
//     the gate's judgement the moment the positions walk went stale, the episode's
//     clock would stop with it, and the adding order H-Q-4a exists to retire would
//     rest through BOTH conditions at once -- the one interval in which nothing is
//     watching either the book or the position.
//
// A13 and H-FAIL-4 are not weakened by this: DISPATCH authority is still
// `Actionable`'s, and this predicate authorises nothing. It only says which book
// the caller is looking at.
//
// It reads no clock. Every input is a generation comparison or a flag, so a
// clock step (F21) cannot move the answer, and nothing here can be aged out.
func (g *Gate) BookCurrent(ticker string) bool {
	m := g.markets[ticker]
	if m == nil || !g.connected || m.quarantined {
		return false
	}
	// Zero is never the current generation -- `NewGate` starts there and
	// `quarantineRejectedBook` and `NoteSeqGap` both clear the field back to it
	// -- so "never snapshotted" and "the snapshot was revoked" answer the same
	// way without a second flag that has to be kept in step with this one.
	return m.snapGen == g.gen
}

// F5Quarantined includes the interval after a replacement snapshot but before
// all portfolio endpoints have reconciled. The owner keeps adding off throughout.
func (g *Gate) F5Quarantined(ticker string) bool {
	m := g.markets[ticker]
	return m != nil && g.connected && m.f5
}

// RESTReducerActionable authorizes only the independently retained REST source.
// The caller must still enforce reducing role, inventory and every ordinary cap.
func (g *Gate) RESTReducerActionable(ticker string, now Stamp) bool {
	m := g.markets[ticker]
	if m == nil || !g.connected || !m.f5 || !m.restOK || now.Mono < m.restAt || now.Mono-m.restAt > g.p.Quiet {
		return false
	}
	for k := Truth(0); k < truthCount; k++ {
		if !g.truthOK[k] || g.truthGen[k] != g.gen || now.Mono < g.truthAt[k] || now.Mono-g.truthAt[k] > g.p.TruthMaxAge {
			return false
		}
	}
	return true
}

func (g *Gate) finishF5(m *marketGate) {
	if !m.f5 || m.quarantined || m.snapGen != g.gen {
		return
	}
	for k := Truth(0); k < truthCount; k++ {
		if !g.truthOK[k] || g.truthGen[k] != g.gen || g.truthAt[k] <= m.f5At {
			return
		}
	}
	m.f5, m.restOK = false, false
	m.clearQuietEpisode()
}

// PnLMarkState is what the gate can say about one market's H-HALT-5 mark.
//
// Three answers rather than a boolean, because H-HALT-5 makes the operator's
// SEV2 distinguish a book that is ABSENT from one that is merely STALE, and a
// two-valued answer forces the caller to guess which it is looking at. Both
// non-fresh answers are equally not-fired -- the distinction is diagnostic, not
// behavioural.
type PnLMarkState int

const (
	// PnLMarkAbsent is no usable mark at all: this generation has not accepted
	// a snapshot for the market, or the mark was retired by a rejected frame,
	// a sequence gap or a disconnect.
	PnLMarkAbsent PnLMarkState = iota
	// PnLMarkStale is a mark that exists and is older than `pnl_mark_max_age_s`.
	PnLMarkStale
	// PnLMarkFresh is a mark inside the age bound and safe to value against.
	PnLMarkFresh
)

// PnLMark reports the age of a market's accepted-book mark and whether it may
// be used to value inventory (H-HALT-5).
//
// The bound is `pnl_mark_max_age_s`, and it is NOT either of the two 60s clocks
// this file already keeps. `quiet_s` asks whether the market is still talking;
// `truth_max_age_s` asks whether the PORTFOLIO reads are current. Neither is a
// statement about the freshness of the PRICE, and at 60s both would authorise
// valuing a position against a mark H-HALT-5 has already declared unusable.
//
// The comparison is `>`, so a mark of exactly `pnl_mark_max_age_s` is FRESH.
// §16 states the parameter as a maximum age and H-HALT-5 as "age <= 30s", so
// the boundary belongs to the usable side.
func (g *Gate) PnLMark(ticker string, now Stamp) (time.Duration, PnLMarkState) {
	m := g.markets[ticker]
	if m == nil || m.pnlMarkGen == 0 || m.pnlMarkGen != g.gen {
		return 0, PnLMarkAbsent
	}
	age := now.Mono - m.pnlMarkAt
	if age > g.p.PnLMarkMaxAge {
		return age, PnLMarkStale
	}
	return age, PnLMarkFresh
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
	// CrossCheck is F5's REST reads to issue, at most one per market and at
	// most one outstanding at a time.
	//
	// It is a REQUEST and not an action, for the reason every other effect in
	// this package is: this type reads no clock, performs no I/O and starts no
	// goroutine, and a gate that could issue an HTTP request would be a gate the
	// owner goroutine blocks inside.
	CrossCheck []CrossCheckRequest
}

// CrossCheckRequest is one market's F5 read, addressed and bound to the
// connection that asked for it.
type CrossCheckRequest struct {
	Ticker string
	Token  CrossCheckToken
	// Refresh retains independent reducer depth; it never compares the quarantined book.
	Refresh bool
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
			if m.f5 {
				if !m.f5Escalated && now.Mono-m.f5At > g.p.DisconnectReduce {
					m.f5Escalated = true
					eff.Anomalies = append(eff.Anomalies, risk.Anomaly{Class: "BOOK_QUARANTINE_SUSTAINED", Sev: risk.SEV1, Ticker: t, Text: "F5 quarantine exceeded disconnect_halt_s; reducer remains live only from fresh REST depth"})
				}
				if m.phase != quietChecking && now.Mono-m.refreshAt >= g.p.Quiet {
					m.phase = quietChecking
					m.checkSeq++
					m.refreshAt = now.Mono
					eff.CrossCheck = append(eff.CrossCheck, CrossCheckRequest{Ticker: t, Token: CrossCheckToken{ticker: t, gen: g.gen, seq: m.checkSeq, valid: true, requestedAt: now.Mono}, Refresh: true})
				}
				continue
			}
			if now.Mono-m.lastFrame <= g.p.Quiet {
				continue
			}
			if m.quarantined {
				// A market that is ALREADY closed for another reason -- a book
				// frame core refused, a subscription-wide sequence gap -- has no
				// placement licence for a cross-check to protect, and its
				// silence means the resnapshot that would repair it has not
				// arrived either. There is nothing for F5 to establish here:
				// proving the REST book agrees with a book we have already
				// decided not to trust would license nothing, and the silence
				// itself is now the wedged feed. This is the pre-cross-check
				// behaviour, kept exactly, for exactly this case.
				eff.Resnapshot = true
				if !m.reducing {
					m.reducing = true
					eff.Reduce = append(eff.Reduce, t)
				}
				g.pingQuiet(m, t, now, &eff, "the book was already quarantined, "+
					"so there is nothing a cross-check could re-license; the "+
					"market is sent to REDUCING")
				continue
			}
			// Silence ASKS A QUESTION; it does not answer one. F5's response to
			// a wedged single market is `GET /markets/{t}/orderbook`, and the
			// two outcomes are opposite: agree and the market keeps quoting,
			// disagree and its book is replaced and it goes to REDUCING.
			// Reducing here, before the read, would delete the agree branch --
			// which is exactly the state lip-357 found: with no cross-check
			// there is no way back to quoting, so a market that went quiet for a
			// minute stayed quarantined and reducing until the socket happened
			// to cycle for some unrelated reason.
			if m.phase != quietNone {
				// One outstanding check per market, and none at all once the
				// episode has been decided against the book.
				continue
			}
			m.phase = quietChecking
			m.checkSeq++
			m.checkTries++
			// Placement stops for the round trip. The book has not been shown
			// wrong, and it has not been shown right either.
			m.quietHold = true
			eff.CrossCheck = append(eff.CrossCheck, CrossCheckRequest{
				Ticker: t,
				Token: CrossCheckToken{
					ticker: t, gen: g.gen, seq: m.checkSeq, valid: true, requestedAt: now.Mono,
				},
			})
			g.pingQuiet(m, t, now, &eff, "F5's REST cross-check is requested and "+
				"no placement decision is taken from this market until it "+
				"answers. Agreement resets the staleness clock and the market "+
				"keeps quoting; disagreement replaces the book, quarantines it "+
				"and sends the market to REDUCING")
		}
	}

	sort.Strings(eff.Reduce)
	return eff
}

// pingQuiet raises BOOK_QUIET once per silent episode.
//
// Once, because a market that closed for the night is silent for hours: an alert
// that fires four times a second into a 256-slot buffer evicts every other
// anomaly in it, including the one the operator needs. `quietPinged` re-arms on
// the next accepted frame and on the next connection.
func (g *Gate) pingQuiet(m *marketGate, ticker string, now Stamp,
	eff *TickEffects, what string) {

	if m.quietPinged {
		return
	}
	m.quietPinged = true
	eff.Anomalies = append(eff.Anomalies, risk.Anomaly{
		Class: "BOOK_QUIET", Sev: risk.SEV2, Ticker: ticker,
		Text: fmt.Sprintf("no book frame for %v while the socket is healthy, "+
			"past quiet_s %v; %s", now.Mono-m.lastFrame, g.p.Quiet, what),
	})
}

// CrossCheckOutcome is what one completed F5 read established. It is the
// caller's CONCLUSION, not its data: this package holds no book and cannot
// compare one.
type CrossCheckOutcome uint8

const (
	// CrossCheckUnavailable is the zero value, and it is the safe one: a
	// transport error, a non-200, a body that did not decode, a request that
	// was never dispatched. It is never agreement, and it never advances the
	// staleness clock -- "we could not ask" is not "the book is right".
	CrossCheckUnavailable CrossCheckOutcome = iota
	// CrossCheckAgree is a COMPLETE H-FAIL-6 walk that matched: both sides
	// through Target Size, price and size at every level, and our own resting
	// size where we believe it is.
	CrossCheckAgree
	// CrossCheckDisagree is a valid REST book that differs from ours anywhere
	// H-FAIL-6 looks.
	CrossCheckDisagree
	// CrossCheckGranularity is H-CO-3a on the REST book: a resting price that is
	// not an integer cent. The read landed and what it says is that the tick
	// size has changed, so this book may not replace ours -- but it is
	// emphatically not agreement either.
	CrossCheckGranularity
)

// CrossCheckEffects is what folding one cross-check result into the gate
// requires of the caller.
type CrossCheckEffects struct {
	// Accepted is whether the token was current. A false here means NOTHING
	// happened: no clock moved, no book may be replaced, no market reduced.
	Accepted bool
	// ReplaceBook asks the owner to install the REST snapshot wholesale. It is
	// set only for a disagreement over a valid book, because that is the one
	// case where we hold a complete book we have proven is better than ours.
	ReplaceBook bool
	// RetainRESTBook installs a separate reducer-only pricing source.
	RetainRESTBook bool
	Reduce         []string
	Resnapshot     bool
	Anomalies      []risk.Anomaly
}

// NoteCrossCheck folds one F5 cross-check result into the gate.
//
// The gate decides what the outcome MEANS; the caller decided what the outcome
// IS. That split is deliberate: comparing two books needs `core.Rig` and
// `risk.Portfolio`, both of which belong to the owner goroutine and neither of
// which this package may hold, while deciding whether a market may still quote
// is A13 and belongs here.
//
// # What each outcome does
//
//	Agree         -> reset the staleness clock, clear the quiet hold ONLY.
//	Disagree      -> replace the book, quarantine, resnapshot, REDUCING, SEV2.
//	Granularity   -> quarantine and REDUCING, and replace NOTHING (H-CO-3a).
//	Unavailable   -> retry, up to quietCheckMaxTries; then quarantine + REDUCING.
//
// Agreement is the only outcome that touches the clock, and it touches nothing
// else. A market that was already quarantined by a sequence gap, or already
// latched REDUCING by an earlier outage, is left exactly as it was: F5 asked one
// question -- "is this silent book still right?" -- and a yes is not evidence
// about anything else that closed the market.
func (g *Gate) NoteCrossCheck(tok CrossCheckToken, out CrossCheckOutcome,
	why string, now Stamp) CrossCheckEffects {

	var eff CrossCheckEffects
	m := g.markets[tok.ticker]
	if m == nil || !tok.valid || tok.gen != g.gen ||
		m.phase != quietChecking || m.checkSeq != tok.seq {
		// A result about a book that no longer exists: the socket cycled, a
		// snapshot landed, or an earlier check already settled this episode.
		// Discarded WHOLESALE -- not half-credited, not used for its timestamp
		// -- for the reason `noteTruth` discards a stale reconciliation.
		return eff
	}
	eff.Accepted = true
	if m.f5 {
		m.phase = quietSettled
		if out == CrossCheckAgree || out == CrossCheckDisagree {
			m.restAt, m.restOK = tok.requestedAt, true
			eff.RetainRESTBook = true
		} else {
			m.restOK = false
			eff.Anomalies = append(eff.Anomalies, risk.Anomaly{Class: "BOOK_REDUCER_REFRESH_FAILED", Sev: risk.SEV2, Ticker: tok.ticker, Text: why})
		}
		return eff
	}

	switch out {
	case CrossCheckAgree:
		// F5: "agree -> reset the staleness clock, keep quoting." The clock is
		// reset at RESULT time and not at request time, because the interval we
		// have evidence about ends when the exchange answered.
		m.lastFrame = now.Mono
		m.quietPinged = false
		m.clearQuietEpisode()
		eff.Anomalies = append(eff.Anomalies, risk.Anomaly{
			Class: "BOOK_CROSSCHECK_AGREED", Sev: risk.SEV3, Ticker: tok.ticker,
			Text: "F5's REST cross-check matched the websocket book through " +
				"Target Size on both sides, including our own resting size " +
				"(H-FAIL-6); the staleness clock is reset and the market keeps " +
				"quoting. Nothing else about the market is changed by this",
		})
		return eff

	case CrossCheckDisagree:
		m.phase = quietSettled
		m.quietHold = false
		eff.ReplaceBook = true
		eff.RetainRESTBook = true
		m.f5, m.f5Escalated = true, false
		m.f5At, m.restAt, m.refreshAt = now.Mono, tok.requestedAt, now.Mono
		m.restOK = true
		eff.Resnapshot = true
		g.quarantineCrossChecked(m, tok.ticker, &eff)
		eff.Anomalies = append(eff.Anomalies, risk.Anomaly{
			Class: "BOOK_CROSSCHECK_MISMATCH", Sev: risk.SEV2, Ticker: tok.ticker,
			Text: "F5's REST cross-check disagrees with the websocket book: " +
				why + ". The complete REST book replaces ours, the market is " +
				"quarantined until a fresh websocket snapshot lands, and it is " +
				"sent to REDUCING",
		})
		return eff

	case CrossCheckGranularity:
		m.phase = quietSettled
		m.quietHold = false
		g.quarantineCrossChecked(m, tok.ticker, &eff)
		eff.Anomalies = append(eff.Anomalies, risk.Anomaly{
			Class: "BOOK_PRICE_GRANULARITY", Sev: risk.SEV2, Ticker: tok.ticker,
			Text: "F5's REST cross-check read a resting price that is not an " +
				"integer cent (" + why + "); H-CO-3a sends this market to " +
				"REDUCING. The REST book replaces nothing, because a book whose " +
				"prices this harness cannot represent is not a repair",
		})
		return eff
	}

	// Unavailable. The endpoint did not answer, or did not answer usably.
	if m.checkTries < quietCheckMaxTries {
		// Ask again on a later tick. The phase goes back to `quietNone` and the
		// HOLD STAYS SET: no placement while we still do not know.
		m.phase = quietNone
		eff.Anomalies = append(eff.Anomalies, risk.Anomaly{
			Class: "BOOK_CROSSCHECK_FAILED", Sev: risk.SEV2, Ticker: tok.ticker,
			Text: fmt.Sprintf("F5's REST cross-check did not land (attempt %d of "+
				"%d): %s. The market stays non-actionable and the staleness "+
				"clock is NOT advanced -- a read we could not make is not "+
				"evidence that the book is right",
				m.checkTries, quietCheckMaxTries, why),
		})
		return eff
	}
	m.phase = quietSettled
	m.quietHold = false
	g.quarantineCrossChecked(m, tok.ticker, &eff)
	eff.Resnapshot = true
	eff.Anomalies = append(eff.Anomalies, risk.Anomaly{
		Class: "BOOK_CROSSCHECK_UNAVAILABLE", Sev: risk.SEV2, Ticker: tok.ticker,
		Text: fmt.Sprintf("F5's REST cross-check did not land in %d attempts "+
			"(%s), so nothing can establish that this silent book is right. The "+
			"market is quarantined and sent to REDUCING, which is where a "+
			"wedged feed that cannot be checked belongs -- REST and the "+
			"websocket are distinct transports but they share DNS, routing, TLS "+
			"and credentials (H-FAIL-2), so both failing at once is a state and "+
			"not a contradiction", quietCheckMaxTries, why),
	})
	return eff
}

// quarantineCrossChecked closes a market that F5 has decided against.
//
// It clears `snapGen` as well as setting `quarantined`, so that only an accepted
// SNAPSHOT can reopen the market -- the same rule `quarantineRejectedBook`
// applies, and for the same reason: what we hold is a book we have been told is
// wrong, and replaying increments onto it cannot make it right. The mark goes
// with it (H-HALT-5): a price the cross-check has just refuted is not a price to
// value inventory against.
func (g *Gate) quarantineCrossChecked(m *marketGate, ticker string,
	eff *CrossCheckEffects) {

	m.quarantined = true
	m.snapGen = 0
	m.pnlMarkGen = 0
	if !m.reducing {
		m.reducing = true
		eff.Reduce = append(eff.Reduce, ticker)
	}
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
