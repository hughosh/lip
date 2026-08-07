package wsx

import (
	"testing"
	"time"

	"lip/harness/risk"
)

func snapFrame(ticker string) FrameInfo {
	return FrameInfo{Kind: FrameSnapshot, Ticker: ticker, Deliver: true}
}

func deltaFrame(ticker string) FrameInfo {
	return FrameInfo{Kind: FrameDelta, Ticker: ticker, Deliver: true}
}

// TestEveryDisconnectQuarantinesUntilFreshSnapshotAndAllPortfolioTruth is
// H-FAIL-5 and the reconciliation half of H-ORD-5.
//
// Two properties, and the second is the one an earlier design would have
// missed:
//
//  1. EVERY disconnect quarantines, however clean. The shadow rig treats a
//     clean close as a non-event and does not reset its books (port-spec P25a),
//     which is right for a measurement rig and fatal for a trading one -- a
//     polite goodbye is still a gap, and "the exchange said goodbye" is not
//     evidence about depth.
//
//  2. A fresh snapshot ALONE does not re-authorise placement. The book being
//     current says nothing about our own position, our own resting orders, or
//     our own fills, and a market that resumed quoting on the snapshot would be
//     placing against inventory it had not re-read since before the gap.
//
// It also asserts the reverse: that a clean close keeps the old book for
// diagnostics while an abnormal one demands core.Rig.ResetOnReconnect. That
// asymmetry authorises nothing -- both are non-actionable -- and exists so an
// operator can see what the market was doing when the socket closed.
func TestEveryDisconnectQuarantinesUntilFreshSnapshotAndAllPortfolioTruth(t *testing.T) {
	for _, tc := range []struct {
		name      string
		clean     bool
		wantReset bool
	}{
		{"clean close 1000/1001", true, false},
		{"abnormal close", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := testParams()
			g, err := NewGate([]string{fxTicker}, p)
			if err != nil {
				t.Fatal(err)
			}

			// Fully licensed to place.
			connectAndReconcile(g, fxTicker, at(0))
			if !g.Actionable(fxTicker, at(0)) {
				t.Fatal("a connected, snapshotted, fully reconciled market is " +
					"not actionable")
			}

			de := g.ApplyDisconnect(at(10), tc.clean)
			if de.ResetBooks != tc.wantReset {
				t.Fatalf("ResetBooks = %v, want %v", de.ResetBooks, tc.wantReset)
			}
			if !hasClass(de.Anomalies, "WS_DISCONNECT") {
				t.Fatalf("the disconnect was silent: %v",
					classesOf(de.Anomalies))
			}
			if g.Actionable(fxTicker, at(10)) {
				t.Fatalf("a %s disconnect left the market actionable; "+
					"H-FAIL-5 makes the book non-actionable HOWEVER clean the "+
					"close was", tc.name)
			}

			// Reconnect. Still nothing: no snapshot on this generation, and no
			// portfolio truth reconciled against it.
			ce := g.OnConnect(at(20))
			if !ce.Resnapshot {
				t.Fatal("a new connection did not request a resnapshot")
			}
			if g.Actionable(fxTicker, at(20)) {
				t.Fatal("reconnecting alone re-authorised placement")
			}

			// A fresh snapshot. STILL nothing -- this is property 2.
			g.ApplyFrame(snapFrame(fxTicker), okHandle, at(21))
			if g.Actionable(fxTicker, at(21)) {
				t.Fatal("a fresh snapshot alone re-authorised placement; the " +
					"book being current says nothing about our position, our " +
					"resting orders or our fills")
			}

			// Each endpoint in turn. Only the third completes the set, and the
			// loop asserts that no PROPER SUBSET does -- a reconciliation that
			// counted two of three would place against fills it had not
			// re-read.
			order := []Truth{TruthPositions, TruthOrders, TruthFills}
			for i, k := range order {
				g.noteTruth(k, ce.Token, at(22))
				last := i == len(order)-1
				if got := g.Actionable(fxTicker, at(22)); got != last {
					t.Fatalf("after reconciling %v (%d of %d), actionable = "+
						"%v, want %v", k, i+1, len(order), got, last)
				}
			}
		})
	}
}

// TestLatePreDisconnectReconcileCannotUnlockNewConnection is the race the
// generation counter exists for.
//
// A portfolio read is issued, the socket drops, the process reconnects, and
// only then does the read land. It describes the account on the far side of a
// gap. Crediting it would unlock placement on a connection whose books have not
// been resnapshotted and whose position has not been re-read -- and the state
// would LOOK reconciled, which is why it is discarded wholesale rather than
// partially credited.
func TestLatePreDisconnectReconcileCannotUnlockNewConnection(t *testing.T) {
	g, err := NewGate([]string{fxTicker}, testParams())
	if err != nil {
		t.Fatal(err)
	}

	old := g.OnConnect(at(0)).Token
	g.ApplyFrame(snapFrame(fxTicker), okHandle, at(1))

	g.ApplyDisconnect(at(5), false)
	fresh := g.OnConnect(at(6))
	g.ApplyFrame(snapFrame(fxTicker), okHandle, at(7))

	// The reads issued for the OLD connection land now.
	for _, k := range []Truth{TruthPositions, TruthOrders, TruthFills} {
		if g.noteTruth(k, old, at(8)) {
			t.Fatalf("a %v read carrying the pre-disconnect token was "+
				"accepted", k)
		}
		if age := g.TruthAge(k, at(8)); age != -1 {
			t.Fatalf("a rejected %v read still moved the freshness clock "+
				"(age %v); half-accepting a stale reconciliation produces a "+
				"state that looks reconciled", k, age)
		}
	}
	if g.Actionable(fxTicker, at(8)) {
		t.Fatal("a pre-disconnect reconciliation unlocked the new connection")
	}

	// The same three reads, carrying the CURRENT token, do unlock it.
	for _, k := range []Truth{TruthPositions, TruthOrders, TruthFills} {
		if !g.noteTruth(k, fresh.Token, at(9)) {
			t.Fatalf("a %v read for the current connection was rejected", k)
		}
	}
	if !g.Actionable(fxTicker, at(9)) {
		t.Fatal("a complete reconciliation on the current connection did not " +
			"unlock placement")
	}

	// A token nobody issued is not a token.
	var zero ReconcileToken
	if zero.Valid() || g.noteTruth(TruthPositions, zero, at(10)) {
		t.Fatal("the zero ReconcileToken was accepted")
	}
}

// TestSequenceGapQuarantinesAndRequestsResnapshot is core's subscription-wide
// gap, folded into the gate.
//
// The gap is subscription-wide and not per market: it means SOME market lost a
// delta and nothing in the data says which, so every book on that subscription
// is suspect. It does NOT reduce -- a resnapshot repairs it within one round
// trip, and reducing on every gap would convert an ordinary exchange hiccup
// into a full unwind.
func TestSequenceGapQuarantinesAndRequestsResnapshot(t *testing.T) {
	g, err := NewGate([]string{"A", "B"}, testParams())
	if err != nil {
		t.Fatal(err)
	}
	ce := g.OnConnect(at(0))
	for _, tk := range []string{"A", "B"} {
		g.ApplyFrame(snapFrame(tk), okHandle, at(1))
	}
	for _, k := range []Truth{TruthPositions, TruthOrders, TruthFills} {
		g.noteTruth(k, ce.Token, at(1))
	}
	if !g.Actionable("A", at(1)) || !g.Actionable("B", at(1)) {
		t.Fatal("setup: both markets should be actionable")
	}

	eff := g.NoteSeqGap(at(2))
	if !eff.Resnapshot {
		t.Fatal("a sequence gap did not request a resnapshot")
	}
	if len(eff.Reduce) != 0 {
		t.Fatalf("a sequence gap reduced %v; it is a data-quality event a "+
			"resnapshot repairs, not a risk event", eff.Reduce)
	}
	sev, ok := sevOf(eff.Anomalies, "BOOK_SEQ_GAP")
	if !ok || sev != risk.SEV2 {
		t.Fatalf("anomalies = %v, want SEV2 BOOK_SEQ_GAP", eff.Anomalies)
	}
	for _, tk := range []string{"A", "B"} {
		if g.Actionable(tk, at(2)) {
			t.Fatalf("%s stayed actionable after a subscription-wide gap; "+
				"nothing says which market lost the delta", tk)
		}
	}

	// A snapshot for one market recovers only that one.
	g.ApplyFrame(snapFrame("A"), okHandle, at(3))
	if !g.Actionable("A", at(3)) {
		t.Fatal("A did not recover after its resnapshot")
	}
	if g.Actionable("B", at(3)) {
		t.Fatal("B recovered on A's snapshot")
	}
}

// TestQuietMarketAloneReducesAndResnapshots is F5.
//
// "Alone" is the load-bearing word twice over. The market is silent while the
// SOCKET IS HEALTHY -- pings answer, other markets are publishing -- so no
// disconnect explains it, and the wedge is invisible to every other detector in
// the package. And ONE market goes to REDUCING, not the whole book: the
// evidence is about that market.
//
// H-FAIL-6's full-depth comparison is deferred by pilot-plan.md §2.7, so this
// reduces rather than trying to prove the book right. That is the conservative
// direction: the alternative is quoting against a book we have decided to trust
// on the strength of a check we have not built.
func TestQuietMarketAloneReducesAndResnapshots(t *testing.T) {
	p := testParams()
	g, err := NewGate([]string{"A", "B"}, p)
	if err != nil {
		t.Fatal(err)
	}
	ce := g.OnConnect(at(0))
	for _, tk := range []string{"A", "B"} {
		g.ApplyFrame(snapFrame(tk), okHandle, at(0))
	}
	for _, k := range []Truth{TruthPositions, TruthOrders, TruthFills} {
		g.noteTruth(k, ce.Token, at(0))
	}

	// B keeps publishing; A goes silent. Exactly quiet_s is NOT past it.
	quiet := int(p.Quiet / time.Second)
	g.ApplyFrame(deltaFrame("B"), okHandle, at(quiet))
	if eff := g.Tick(at(quiet)); len(eff.Reduce) != 0 {
		t.Fatalf("a market silent for EXACTLY quiet_s reduced: %v; the "+
			"comparison is strictly greater", eff.Reduce)
	}

	// Keep portfolio truth fresh across the quiet window, so the assertions
	// below are about the silence and not about truth_max_age_s aging out.
	for _, k := range []Truth{TruthPositions, TruthOrders, TruthFills} {
		g.noteTruth(k, ce.Token, at(quiet+1))
	}

	eff := g.Tick(at(quiet + 1))
	if len(eff.Reduce) != 1 || eff.Reduce[0] != "A" {
		t.Fatalf("reduce = %v, want only the silent market", eff.Reduce)
	}
	if !eff.Resnapshot {
		t.Fatal("a quiet market did not request a resnapshot")
	}
	sev, ok := sevOf(eff.Anomalies, "BOOK_QUIET")
	if !ok || sev != risk.SEV2 {
		t.Fatalf("anomalies = %v, want SEV2 BOOK_QUIET", eff.Anomalies)
	}
	if g.Actionable("A", at(quiet+1)) {
		t.Fatal("the quiet market stayed actionable")
	}
	if !g.Actionable("B", at(quiet+1)) {
		t.Fatal("the publishing market was reduced along with the quiet one")
	}

	// The ping does not repeat every tick: an alert that fires once a second
	// for a market that closed for the night is an alert channel nobody reads.
	again := g.Tick(at(quiet + 2))
	if hasClass(again.Anomalies, "BOOK_QUIET") {
		t.Fatal("BOOK_QUIET repeated on the next tick")
	}
	if len(again.Reduce) != 0 {
		t.Fatalf("the quiet market was re-reduced: %v", again.Reduce)
	}

	// The reduction is sticky. A resnapshot arriving lifts the quarantine --
	// the book is current again -- but does not put the market back to
	// quoting, because that is the state machine's decision and not the
	// socket's.
	g.ApplyFrame(snapFrame("A"), okHandle, at(quiet+3))
	if !g.Reducing("A") {
		t.Fatal("a resnapshot cleared the sticky REDUCING flag; recovering " +
			"the feed is evidence about the feed, not about the risk taken " +
			"while it was wedged")
	}

	// While the socket is DOWN, silence is explained by the disconnect and
	// must not be reported as N separate wedged feeds.
	g.ApplyDisconnect(at(quiet+4), false)
	down := g.Tick(at(quiet + 4 + 2*quiet))
	if hasClass(down.Anomalies, "BOOK_QUIET") {
		t.Fatal("a disconnected socket produced per-market BOOK_QUIET " +
			"anomalies, burying the one disconnect that explains all of them")
	}
}

// TestDisconnectSustainedReducesEveryMarketOnceAndStaysSticky is the recording
// half of F4. The DETECTION lives in the supervisor, which owns the reconnect
// clock; this asserts what the gate does when told.
func TestDisconnectSustainedReducesEveryMarketOnceAndStaysSticky(t *testing.T) {
	p := testParams()
	g, err := NewGate([]string{"A", "B"}, p)
	if err != nil {
		t.Fatal(err)
	}
	connectAndReconcile(g, "A", at(0))
	g.ApplyDisconnect(at(1), false)

	eff := g.NoteDisconnectSustained(p.DisconnectReduce, at(61))
	if len(eff.Reduce) != 2 {
		t.Fatalf("reduce = %v, want every market", eff.Reduce)
	}
	sev, ok := sevOf(eff.Anomalies, "WS_DISCONNECT_SUSTAINED")
	if !ok || sev != risk.SEV1 {
		t.Fatalf("anomalies = %v, want SEV1 WS_DISCONNECT_SUSTAINED",
			eff.Anomalies)
	}

	// Once per episode.
	if again := g.NoteDisconnectSustained(p.DisconnectReduce, at(62)); len(again.Reduce) != 0 ||
		len(again.Anomalies) != 0 {
		t.Fatalf("F4 fired twice in one outage: %+v", again)
	}

	// Reconnecting and fully reconciling restores ACTIONABILITY -- the
	// evidence is good again -- but not quoting: the sticky flag survives, and
	// clearing it is an operator's decision, not this package's.
	connectAndReconcile(g, "A", at(70))
	if !g.Actionable("A", at(70)) {
		t.Fatal("a fully reconciled market is not actionable after recovery")
	}
	if !g.Reducing("A") || !g.Reducing("B") {
		t.Fatal("reconnecting silently restored quoting; a socket coming back " +
			"is evidence about the socket, not about the inventory nobody " +
			"was watching")
	}
}

// TestGateStartsClosed protects the initial state.
//
// A gate that defaulted to open would authorise placement in the window
// between process start and first connection -- precisely the window H-ORD-5
// reserves for reconciliation.
func TestGateStartsClosed(t *testing.T) {
	g, err := NewGate([]string{fxTicker}, testParams())
	if err != nil {
		t.Fatal(err)
	}
	if g.Connected() {
		t.Fatal("a new gate reports itself connected")
	}
	if g.Actionable(fxTicker, at(0)) {
		t.Fatal("a new gate authorises placement before the first connection")
	}
	if g.Actionable("a market we do not manage", at(0)) {
		t.Fatal("the gate authorises a market it does not know")
	}
	if _, err := NewGate(nil, testParams()); err == nil {
		t.Fatal("a gate over an empty market set was accepted")
	}
}
