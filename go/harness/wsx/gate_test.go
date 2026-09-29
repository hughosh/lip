package wsx

import (
	"testing"
	"time"

	"lip/harness/cfg"
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
// # What lip-357 changed about this test, and what it did not
//
// F5 does not reduce on silence. It reads `GET /markets/{t}/orderbook` and
// compares (H-FAIL-6), and only DISAGREEMENT reduces -- agreement resets the
// staleness clock and the market keeps quoting. Before the cross-check existed
// there was no agree branch at all, so a market that went quiet for a minute
// was quarantined and reducing until the socket happened to cycle for an
// unrelated reason, with no way back (`lip-357`).
//
// So the sequence asserted here is the whole of F5's reducing path: silence
// past `quiet_s` asks ONE question about ONE market, and the MISMATCH answer is
// what reduces it. Every earlier assertion is unchanged -- exactly `quiet_s` is
// not past it, only the silent market is affected, the ping does not repeat,
// the reduction is sticky across a resnapshot, and a disconnected socket
// produces no per-market wedge at all.
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
	if eff := g.Tick(at(quiet)); len(eff.Reduce) != 0 ||
		len(eff.CrossCheck) != 0 {
		t.Fatalf("a market silent for EXACTLY quiet_s produced reduce=%v "+
			"crosscheck=%v; the comparison is strictly greater",
			eff.Reduce, eff.CrossCheck)
	}

	// Keep portfolio truth fresh across the quiet window, so the assertions
	// below are about the silence and not about truth_max_age_s aging out.
	for _, k := range []Truth{TruthPositions, TruthOrders, TruthFills} {
		g.noteTruth(k, ce.Token, at(quiet+1))
	}

	eff := g.Tick(at(quiet + 1))
	if len(eff.CrossCheck) != 1 || eff.CrossCheck[0].Ticker != "A" {
		t.Fatalf("crosscheck = %+v, want exactly one, for the silent market. "+
			"F5's response to a wedged single market is a REST orderbook read "+
			"on that market and nothing else", eff.CrossCheck)
	}
	tok := eff.CrossCheck[0].Token
	if !tok.Valid() || tok.Ticker() != "A" {
		t.Fatalf("the cross-check token is valid=%v ticker=%q",
			tok.Valid(), tok.Ticker())
	}
	if len(eff.Reduce) != 0 {
		t.Fatalf("silence ALONE reduced %v. Silence asks F5's question; the "+
			"REST answer is what decides it, and reducing here deletes the "+
			"agree branch entirely", eff.Reduce)
	}
	sev, ok := sevOf(eff.Anomalies, "BOOK_QUIET")
	if !ok || sev != risk.SEV2 {
		t.Fatalf("anomalies = %v, want SEV2 BOOK_QUIET", eff.Anomalies)
	}
	if g.Actionable("A", at(quiet+1)) {
		t.Fatal("the quiet market stayed actionable while its cross-check was " +
			"outstanding; the book has not been shown wrong, and it has not " +
			"been shown right either, and no new order may be priced from it " +
			"in between")
	}
	if !g.Actionable("B", at(quiet+1)) {
		t.Fatal("the publishing market was closed along with the quiet one")
	}

	// The ping does not repeat every tick: an alert that fires once a second
	// for a market that closed for the night is an alert channel nobody reads.
	// Nor does the REQUEST: one check is outstanding at a time.
	again := g.Tick(at(quiet + 2))
	if hasClass(again.Anomalies, "BOOK_QUIET") {
		t.Fatal("BOOK_QUIET repeated on the next tick")
	}
	if len(again.Reduce) != 0 {
		t.Fatalf("the quiet market was re-reduced: %v", again.Reduce)
	}
	if len(again.CrossCheck) != 0 {
		t.Fatalf("a second cross-check was requested while the first was still "+
			"outstanding: %+v", again.CrossCheck)
	}

	// THE MISMATCH. This is what reduces the market, and it is the only thing
	// that does: F5 disagreeing means the book we hold is not the book the
	// exchange has.
	res := g.NoteCrossCheck(tok, CrossCheckDisagree,
		"the yes side is 50x1300 here and 50x10 on REST", at(quiet+3))
	if !res.Accepted {
		t.Fatal("the gate rejected the result of the check it had just asked for")
	}
	if len(res.Reduce) != 1 || res.Reduce[0] != "A" {
		t.Fatalf("reduce = %v, want only the silent market", res.Reduce)
	}
	if !res.Resnapshot {
		t.Fatal("a disagreeing cross-check did not request a resnapshot")
	}
	if !res.ReplaceBook {
		t.Fatal("a disagreeing cross-check did not ask for the REST book to " +
			"replace ours; F5 says the market's book is REPLACED and " +
			"quarantined, and a quarantine over a book we already know is " +
			"wrong leaves the wrong book in place for every diagnostic that " +
			"reads it")
	}
	sev, ok = sevOf(res.Anomalies, "BOOK_CROSSCHECK_MISMATCH")
	if !ok || sev != risk.SEV2 {
		t.Fatalf("anomalies = %v, want SEV2 BOOK_CROSSCHECK_MISMATCH",
			res.Anomalies)
	}
	if !g.Reducing("A") {
		t.Fatal("the gate does not report the mismatched market REDUCING")
	}
	if g.Reducing("B") {
		t.Fatal("the market that never went quiet was reduced too")
	}
	if g.Actionable("A", at(quiet+3)) {
		t.Fatal("a market whose book F5 has just replaced stayed actionable; " +
			"A13 forbids a placement decision from a quarantined book and the " +
			"replacement is not a websocket snapshot")
	}

	// The reduction is sticky. A resnapshot arriving lifts the quarantine --
	// the book is current again -- but does not put the market back to
	// quoting, because that is the state machine's decision and not the
	// socket's.
	g.ApplyFrame(snapFrame("A"), okHandle, at(quiet+4))
	if !g.Reducing("A") {
		t.Fatal("a resnapshot cleared the sticky REDUCING flag; recovering " +
			"the feed is evidence about the feed, not about the risk taken " +
			"while it was wedged")
	}

	// While the socket is DOWN, silence is explained by the disconnect and
	// must not be reported as N separate wedged feeds.
	g.ApplyDisconnect(at(quiet+5), false)
	down := g.Tick(at(quiet + 5 + 2*quiet))
	if hasClass(down.Anomalies, "BOOK_QUIET") {
		t.Fatal("a disconnected socket produced per-market BOOK_QUIET " +
			"anomalies, burying the one disconnect that explains all of them")
	}
	if len(down.CrossCheck) != 0 {
		t.Fatalf("a disconnected socket asked for %d REST cross-check(s); "+
			"every market is silent by construction while the socket is down, "+
			"and F5's question is only meaningful about a HEALTHY one",
			len(down.CrossCheck))
	}
}

// quietGate brings a two-market gate to the moment one market's cross-check is
// outstanding, and returns its token.
//
// "A" is the silent market and "B" is the control. Both start fully licensed to
// place, so anything that closes either afterwards is attributable.
func quietGate(t *testing.T) (*Gate, cfg.Params, ReconcileToken, CrossCheckToken) {
	t.Helper()
	p := testParams()
	g, err := NewGate([]string{"A", "B"}, p)
	if err != nil {
		t.Fatal(err)
	}
	ce := g.OnConnect(at(0))
	for _, tk := range []string{"A", "B"} {
		g.ApplyFrame(snapFrame(tk), okHandle, at(0))
	}
	quiet := int(p.Quiet / time.Second)
	// B keeps publishing throughout, which is what makes A's silence a WEDGED
	// FEED rather than a socket nobody is talking on.
	g.ApplyFrame(deltaFrame("B"), okHandle, at(quiet+1))
	for _, k := range []Truth{TruthPositions, TruthOrders, TruthFills} {
		g.noteTruth(k, ce.Token, at(quiet+1))
	}
	if !g.Actionable("A", at(quiet+1)) {
		t.Fatal("setup: A must start actionable, or nothing below is about F5")
	}
	eff := g.Tick(at(quiet + 1))
	if len(eff.CrossCheck) != 1 {
		t.Fatalf("setup: crosscheck = %+v, want one", eff.CrossCheck)
	}
	return g, p, ce.Token, eff.CrossCheck[0].Token
}

// TestCrossCheckAgreementResetsTheClockAndOnlyTheQuietHold is F5's agree
// branch, which did not exist before lip-357.
//
// Three properties, and the second and third are what stop the branch becoming
// an escape hatch:
//
//  1. agreement resets the staleness clock, so the same silence does not ask
//     the same question again a tick later;
//  2. it clears F5's TEMPORARY closure and nothing else. A market carrying an
//     unrelated quarantine stays quarantined, and a market carrying the sticky
//     REDUCING latch stays reducing -- neither of those is a claim about
//     whether this book is current, and a cross-check that lifted them would be
//     a `SetActionable(true)` reachable from a REST response;
//  3. it says nothing about any other market.
func TestCrossCheckAgreementResetsTheClockAndOnlyTheQuietHold(t *testing.T) {
	t.Run("agreement returns the market to quoting", func(t *testing.T) {
		g, p, _, tok := quietGate(t)
		quiet := int(p.Quiet / time.Second)

		res := g.NoteCrossCheck(tok, CrossCheckAgree, "", at(quiet+2))
		if !res.Accepted {
			t.Fatal("the gate rejected its own outstanding check")
		}
		if len(res.Reduce) != 0 || res.ReplaceBook || res.Resnapshot {
			t.Fatalf("agreement produced %+v; F5 says agree keeps quoting", res)
		}
		if hasClass(res.Anomalies, "BOOK_CROSSCHECK_MISMATCH") {
			t.Fatalf("agreement raised the disagreement anomaly: %v",
				classesOf(res.Anomalies))
		}
		if !g.Actionable("A", at(quiet+2)) {
			t.Fatal("a market whose book the exchange has just confirmed is " +
				"still non-actionable. F5's agree branch is what returns a " +
				"quiet market to quoting, and without it a wedged feed is a " +
				"one-way trip to REDUCING")
		}
		if g.Reducing("A") {
			t.Fatal("agreement reduced the market")
		}

		// The clock was reset AT RESULT TIME, so the market is not instantly
		// silent again: a second question is only due `quiet_s` after the
		// answer.
		if eff := g.Tick(at(quiet + 2 + quiet)); len(checksFor(eff, "A")) != 0 {
			t.Fatalf("a second cross-check was requested %v after the first "+
				"agreed: %+v. The staleness clock is reset by agreement, so "+
				"the next one is due quiet_s after the ANSWER", p.Quiet,
				eff.CrossCheck)
		}
		if eff := g.Tick(at(quiet + 3 + 2*quiet)); len(checksFor(eff, "A")) != 1 {
			t.Fatalf("a market that went silent AGAIN did not ask again: %+v; "+
				"agreement resets the clock, it does not retire the detector",
				eff.CrossCheck)
		}
	})

	t.Run("an unrelated quarantine survives agreement", func(t *testing.T) {
		g, p, _, tok := quietGate(t)
		quiet := int(p.Quiet / time.Second)

		// A subscription-wide sequence gap lands while the check is in flight.
		// It quarantines every book and says nothing about which market lost
		// the delta.
		g.NoteSeqGap(at(quiet + 2))

		res := g.NoteCrossCheck(tok, CrossCheckAgree, "", at(quiet+3))
		if !res.Accepted {
			t.Fatal("the gate rejected its own outstanding check")
		}
		if g.Actionable("A", at(quiet+3)) {
			t.Fatal("a REST cross-check lifted a SEQUENCE-GAP quarantine. F5 " +
				"asked one question -- is this silent book still right -- and " +
				"a yes to it is not evidence about a delta the subscription " +
				"lost; only an accepted snapshot reopens that")
		}
	})

	t.Run("the sticky REDUCING latch survives agreement", func(t *testing.T) {
		p := testParams()
		g, err := NewGate([]string{"A", "B"}, p)
		if err != nil {
			t.Fatal(err)
		}
		// An outage takes both markets to REDUCING and latches it; the socket
		// then comes back and both books are resnapshotted and reconciled.
		g.OnConnect(at(0))
		g.ApplyDisconnect(at(1), false)
		g.NoteDisconnectSustained(p.DisconnectReduce, at(61))
		ce := g.OnConnect(at(62))
		for _, tk := range []string{"A", "B"} {
			g.ApplyFrame(snapFrame(tk), okHandle, at(62))
		}
		quiet := int(p.Quiet / time.Second)
		for _, k := range []Truth{TruthPositions, TruthOrders, TruthFills} {
			g.noteTruth(k, ce.Token, at(63+quiet))
		}
		if !g.Reducing("A") {
			t.Fatal("setup: the outage should have latched REDUCING")
		}

		eff := g.Tick(at(63 + quiet))
		if len(eff.CrossCheck) != 2 {
			t.Fatalf("setup: crosscheck = %+v, want both markets", eff.CrossCheck)
		}
		res := g.NoteCrossCheck(eff.CrossCheck[0].Token, CrossCheckAgree, "",
			at(64+quiet))
		if !res.Accepted {
			t.Fatal("the gate rejected its own outstanding check")
		}
		if !g.Reducing("A") {
			t.Fatal("a REST cross-check cleared the sticky REDUCING latch a " +
				"sustained outage set. Nothing in this package clears that " +
				"latch: a book being current is evidence about the book, not " +
				"about the risk taken while nobody was watching it")
		}
	})
}

// TestStaleCrossCheckResultChangesNothing is the race the token exists for.
//
// A REST orderbook read is issued because a market went silent, and it lands
// after the world moved. Every one of the four cases below describes a book the
// answer was not about, and F5's two outcomes -- reset the clock, or replace the
// book and reduce the market -- are both catastrophic applied to the wrong one.
func TestStaleCrossCheckResultChangesNothing(t *testing.T) {
	for _, tc := range []struct {
		name string
		// invalidate moves the world on while the check is in flight.
		invalidate func(g *Gate, p cfg.Params)
	}{
		{"an accepted snapshot landed first", func(g *Gate, p cfg.Params) {
			g.ApplyFrame(snapFrame("A"), okHandle, at(int(p.Quiet/time.Second)+2))
		}},
		{"an accepted delta landed first", func(g *Gate, p cfg.Params) {
			g.ApplyFrame(deltaFrame("A"), okHandle, at(int(p.Quiet/time.Second)+2))
		}},
		{"the socket cycled", func(g *Gate, p cfg.Params) {
			q := int(p.Quiet / time.Second)
			g.ApplyDisconnect(at(q+2), false)
			g.OnConnect(at(q + 3))
		}},
		{"a later check for the same market superseded it",
			func(g *Gate, p cfg.Params) {
				q := int(p.Quiet / time.Second)
				g.NoteCrossCheck(currentToken(g, p), CrossCheckUnavailable,
					"transport", at(q+2))
				if eff := g.Tick(at(q + 3)); len(eff.CrossCheck) != 1 {
					panic("the retry did not issue a second check")
				}
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g, p, _, tok := quietGate(t)
			quiet := int(p.Quiet / time.Second)
			tc.invalidate(g, p)

			for _, out := range []CrossCheckOutcome{
				CrossCheckAgree, CrossCheckDisagree, CrossCheckGranularity,
			} {
				res := g.NoteCrossCheck(tok, out, "stale", at(quiet+9))
				if res.Accepted {
					t.Fatalf("a stale %v result was accepted", out)
				}
				if res.ReplaceBook || len(res.Reduce) != 0 ||
					len(res.Anomalies) != 0 || res.Resnapshot {
					t.Fatalf("a stale result produced effects: %+v", res)
				}
			}
			if g.Reducing("A") {
				t.Fatal("a stale cross-check result reduced the market")
			}
		})
	}

	t.Run("a token nobody issued licenses nothing", func(t *testing.T) {
		g, p, _, _ := quietGate(t)
		var zero CrossCheckToken
		if zero.Valid() {
			t.Fatal("the zero CrossCheckToken reports itself valid")
		}
		quiet := int(p.Quiet / time.Second)
		if res := g.NoteCrossCheck(zero, CrossCheckAgree, "", at(quiet+2)); res.Accepted {
			t.Fatal("the zero CrossCheckToken was accepted, so any caller can " +
				"declare a silent book verified without a check having happened")
		}
		if g.Actionable("A", at(quiet+2)) {
			t.Fatal("a forged agreement returned the market to quoting, which " +
				"is `SetActionable(true)` reachable by anyone who can build a " +
				"struct literal")
		}
	})
}

// currentToken re-derives the token for the check outstanding right now, for
// the one test that has to answer a check it did not capture.
func currentToken(g *Gate, p cfg.Params) CrossCheckToken {
	m := g.markets["A"]
	return CrossCheckToken{ticker: "A", gen: g.gen, seq: m.checkSeq, valid: true}
}

// TestCrossCheckFailureRetriesBoundedThenReduces is the third outcome, and the
// one an implementation is most likely to get wrong in the dangerous direction.
//
// "We could not ask" is not "the book is right". A transport failure must never
// advance the staleness clock and must never return the market to quoting -- and
// it must not produce one REST request per owner tick either, because a wedged
// feed and a dead REST path are the SAME failure often enough to matter
// (H-FAIL-2: the two transports share DNS, routing, TLS, credentials and the
// exchange itself).
//
// So the retry is bounded, and what happens when it runs out is what a market
// whose book cannot be verified deserves: quarantine and REDUCING.
func TestCrossCheckFailureRetriesBoundedThenReduces(t *testing.T) {
	g, p, _, tok := quietGate(t)
	quiet := int(p.Quiet / time.Second)

	// Attempts 1 and 2 fail. Neither reduces, neither reopens the market, and
	// neither moves the clock -- the next tick asks again precisely because the
	// market is still silent.
	for i := 1; i < quietCheckMaxTries; i++ {
		res := g.NoteCrossCheck(tok, CrossCheckUnavailable,
			"dial tcp: connection refused", at(quiet+i))
		if !res.Accepted {
			t.Fatalf("attempt %d: the gate rejected its own outstanding check", i)
		}
		if len(res.Reduce) != 0 || res.ReplaceBook {
			t.Fatalf("attempt %d produced %+v; a failed READ is not a failed "+
				"BOOK, and replacing or reducing on one would make an "+
				"unreachable endpoint into a market halt on the first try",
				i, res)
		}
		if g.Actionable("A", at(quiet+i)) {
			t.Fatalf("attempt %d returned the market to quoting; a read we "+
				"could not make is not evidence that the book is right", i)
		}
		eff := g.Tick(at(quiet + i + 1))
		if len(eff.CrossCheck) != 1 {
			t.Fatalf("attempt %d: the retry did not ask again (%+v)", i, eff.CrossCheck)
		}
		tok = eff.CrossCheck[0].Token
	}

	// The last one. Now the market is closed for good and reduced.
	res := g.NoteCrossCheck(tok, CrossCheckUnavailable, "dial tcp: refused",
		at(quiet+quietCheckMaxTries))
	if len(res.Reduce) != 1 || res.Reduce[0] != "A" {
		t.Fatalf("reduce = %v after %d failed attempts, want the silent market: "+
			"nothing can establish that this book is right and it is still "+
			"silent", res.Reduce, quietCheckMaxTries)
	}
	if !res.Resnapshot {
		t.Fatal("no resnapshot was requested for a book nothing can verify")
	}
	if res.ReplaceBook {
		t.Fatal("a failed read replaced the book; there is no book in a failed " +
			"read to replace it with")
	}
	sev, ok := sevOf(res.Anomalies, "BOOK_CROSSCHECK_UNAVAILABLE")
	if !ok || sev != risk.SEV2 {
		t.Fatalf("anomalies = %v, want SEV2 BOOK_CROSSCHECK_UNAVAILABLE",
			res.Anomalies)
	}
	if !g.Reducing("A") || g.Actionable("A", at(quiet+quietCheckMaxTries)) {
		t.Fatal("the market is not closed and reducing after the retry bound")
	}

	// And it does not hot-loop. The market is still silent and still connected,
	// and no further request is made on any tick.
	for i := 1; i <= 5; i++ {
		if eff := g.Tick(at(quiet + quietCheckMaxTries + i)); len(eff.CrossCheck) != 0 {
			t.Fatalf("tick %d after the bound asked again: %+v. One request per "+
				"owner tick against an endpoint that is not answering is a "+
				"denial of service we perform on ourselves", i, eff.CrossCheck)
		}
	}

	// An accepted snapshot ends the episode, and a market that goes silent
	// AGAIN afterwards gets a fresh set of attempts.
	g.ApplyFrame(snapFrame("A"), okHandle, at(quiet+quietCheckMaxTries+6))
	eff := g.Tick(at(2*quiet + quietCheckMaxTries + 7))
	if len(checksFor(eff, "A")) != 1 {
		t.Fatalf("a new silent episode did not ask: %+v", eff.CrossCheck)
	}
}

// checksFor is the cross-check requests naming one market.
//
// It exists because the control market in these fixtures eventually goes silent
// too -- it publishes once and then stops -- and an assertion counting the whole
// slice would be about the fixture's other market rather than the subject's.
func checksFor(eff TickEffects, ticker string) []CrossCheckRequest {
	var out []CrossCheckRequest
	for _, r := range eff.CrossCheck {
		if r.Ticker == ticker {
			out = append(out, r)
		}
	}
	return out
}

// TestCrossCheckGranularityReducesAndReplacesNothing is H-CO-3a arriving over
// the other transport.
//
// A REST book whose prices are not integer cents is not a book this harness can
// represent -- every price comparison, cap and requote decision in it is
// integer-cent arithmetic -- so it may not replace anything. It is emphatically
// not agreement either: the exchange answered, and what it said is that the tick
// size has changed.
func TestCrossCheckGranularityReducesAndReplacesNothing(t *testing.T) {
	g, p, _, tok := quietGate(t)
	quiet := int(p.Quiet / time.Second)

	res := g.NoteCrossCheck(tok, CrossCheckGranularity,
		`REST book price "0.4150" is not an integer cent`, at(quiet+2))
	if !res.Accepted {
		t.Fatal("the gate rejected its own outstanding check")
	}
	if res.ReplaceBook {
		t.Fatal("a fractional-cent REST book replaced the live book. Once a " +
			"fractional level is in a Book the touch it implies is already " +
			"wrong, which is why H-CO-3a keeps it out rather than quarantining " +
			"afterwards")
	}
	if len(res.Reduce) != 1 || res.Reduce[0] != "A" {
		t.Fatalf("reduce = %v, want the market H-CO-3a fired on", res.Reduce)
	}
	sev, ok := sevOf(res.Anomalies, "BOOK_PRICE_GRANULARITY")
	if !ok || sev != risk.SEV2 {
		t.Fatalf("anomalies = %v, want SEV2 BOOK_PRICE_GRANULARITY", res.Anomalies)
	}
	if g.Actionable("A", at(quiet+2)) {
		t.Fatal("the market stayed actionable after H-CO-3a fired on it")
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

// TestBookCurrentTracksTheBookAndNotThePortfolio is `BookCurrent`'s generation
// and quarantine boundary, and its deliberate independence from A13's other
// half.
//
// H-Q-4a times a continuous gate failure from BOOK EVIDENCE. The predicate that
// decides which frames count therefore has to answer "is this the depth THIS
// connection published" and nothing else -- and the two ways to get that wrong
// are opposite and both expensive:
//
//   - Fold portfolio freshness in, and a stale positions walk CLEARS the
//     episode. H-FAIL-4 already stops dispatch when truth ages out; if it also
//     stopped the gate-failure clock, the adding order H-Q-4a exists to retire
//     would rest through both conditions at once -- the one interval in which
//     nothing is watching either the book or the position.
//   - Leave the generation or quarantine tests out, and a book carried across a
//     gap, or one core refused, or one a sequence gap invalidated, is judged as
//     though it were current. A delta onto a book we have decided not to trust
//     is an unknown base plus a known edit; it can neither start an episode nor
//     end one.
//
// The last sub-case is the one an implementation is most likely to get right
// for snapshots and wrong for deltas: an accepted delta against a book this
// generation DID snapshot is current, because the snapshot is what established
// the base and the delta is a legal increment onto it.
func TestBookCurrentTracksTheBookAndNotThePortfolio(t *testing.T) {
	g, err := NewGate([]string{fxTicker}, testParams())
	if err != nil {
		t.Fatal(err)
	}

	// 1. Before anything. Not connected, never snapshotted.
	if g.BookCurrent(fxTicker) {
		t.Fatal("a gate with no connection reports a current book; nothing has " +
			"published one")
	}

	ce := g.OnConnect(at(0))

	// 2. Connected and NOT yet snapshotted. A new connection has no books --
	// `ConnectEffects.Resnapshot` is unconditionally true for exactly this
	// reason -- so being connected is not being current.
	if g.BookCurrent(fxTicker) {
		t.Fatal("a connection with no snapshot reports a current book; " +
			"ConnectEffects.Resnapshot is always true because a new connection " +
			"has no books at all")
	}

	// 3. An accepted snapshot on this generation. Current, and current BEFORE
	// any portfolio walk has landed -- which is the separation the predicate
	// exists for.
	g.ApplyFrame(snapFrame(fxTicker), okHandle, at(1))
	if !g.BookCurrent(fxTicker) {
		t.Fatal("an accepted snapshot on the current generation is not reported " +
			"as a current book")
	}
	if g.Actionable(fxTicker, at(1)) {
		t.Fatal("Actionable is true with no portfolio reconciliation at all; " +
			"this test's whole subject is the difference between the two, and " +
			"there is none if A13 is already satisfied")
	}

	// 4. A delta onto that snapshot keeps it current.
	g.ApplyFrame(deltaFrame(fxTicker), okHandle, at(2))
	if !g.BookCurrent(fxTicker) {
		t.Fatal("an accepted delta onto a snapshot from this generation " +
			"retired the book; a legal increment onto a base we received in " +
			"full is exactly what a delta is")
	}

	// 5. Full A13, then let PORTFOLIO TRUTH age out past truth_max_age_s. The
	// book has not changed by one level, and the answer here must not change
	// either.
	for k := Truth(0); k < truthCount; k++ {
		if !g.noteTruth(k, ce.Token, at(2)) {
			t.Fatalf("a %v read for the current connection was rejected", k)
		}
	}
	if !g.Actionable(fxTicker, at(2)) {
		t.Fatal("a snapshotted, fully reconciled market is not actionable")
	}
	stale := at(2 + int(testParams().TruthMaxAge/time.Second) + 1)
	if g.Actionable(fxTicker, stale) {
		t.Fatalf("truth older than truth_max_age_s (%v) still authorises "+
			"placement", testParams().TruthMaxAge)
	}
	if !g.BookCurrent(fxTicker) {
		t.Fatalf("portfolio truth ageing past truth_max_age_s (%v) retired the "+
			"BOOK.\n\nThe two are separate measurements. H-FAIL-4 stops "+
			"dispatch when the portfolio goes stale; if it also stopped the "+
			"book being current, an H-Q-4a episode timed from book evidence "+
			"would be CLEARED by a REST outage -- and the adding order that "+
			"rule exists to cancel would keep resting through a gated-out "+
			"interval while nothing was watching the position either",
			testParams().TruthMaxAge)
	}

	// 6. A rejected book frame revokes it. `quarantineRejectedBook` sets the
	// quarantine and clears `snapGen`, so only an accepted SNAPSHOT reopens the
	// market -- a delta cannot.
	g.ApplyFrame(deltaFrame(fxTicker), rejectHandle("sizes do not parse"), at(3))
	if g.BookCurrent(fxTicker) {
		t.Fatal("a book frame core REFUSED left the market reported as current; " +
			"the frame left the book untouched, so the book is now known to be " +
			"behind by exactly that delta")
	}
	g.ApplyFrame(deltaFrame(fxTicker), okHandle, at(4))
	if g.BookCurrent(fxTicker) {
		t.Fatal("an accepted DELTA reopened a quarantined market. A delta is an " +
			"increment against a book we have decided not to trust; replaying " +
			"increments onto it cannot make it right, and only a full " +
			"replacement can")
	}
	g.ApplyFrame(snapFrame(fxTicker), okHandle, at(5))
	if !g.BookCurrent(fxTicker) {
		t.Fatal("an accepted snapshot did not lift the quarantine")
	}

	// 7. A subscription-wide sequence gap. Same revocation, different cause.
	g.NoteSeqGap(at(6))
	if g.BookCurrent(fxTicker) {
		t.Fatal("a subscription-wide sequence gap left the book current; the " +
			"gap means SOME market lost a delta and nothing says which")
	}
	g.ApplyFrame(snapFrame(fxTicker), okHandle, at(7))
	if !g.BookCurrent(fxTicker) {
		t.Fatal("a resnapshot after a sequence gap did not restore the book")
	}

	// 8. A disconnect. The generation moves, so every snapshot taken on the
	// dead connection is retired without anything being erased -- and the
	// RECONNECT alone does not bring it back.
	g.ApplyDisconnect(at(8), true)
	if g.BookCurrent(fxTicker) {
		t.Fatal("a CLEAN disconnect left the book current. H-FAIL-5: a " +
			"disconnect makes the book non-actionable however clean it was, " +
			"because whatever happened to the book while we were not connected " +
			"is unknown")
	}
	g.OnConnect(at(9))
	if g.BookCurrent(fxTicker) {
		t.Fatal("reconnecting alone made the book current again; the new " +
			"connection has delivered nothing yet")
	}
	g.ApplyFrame(deltaFrame(fxTicker), okHandle, at(10))
	if g.BookCurrent(fxTicker) {
		t.Fatal("a delta on the new connection made the book current before " +
			"any snapshot; the base it edits is from the far side of the gap")
	}
	g.ApplyFrame(snapFrame(fxTicker), okHandle, at(11))
	if !g.BookCurrent(fxTicker) {
		t.Fatal("a snapshot on the new connection did not make the book current")
	}

	// 9. A market this gate does not manage is never current.
	if g.BookCurrent("KXNOTOURS-26AUG08-T1") {
		t.Fatal("the gate reports a current book for a market it does not manage")
	}
}

func TestF5IndependentReducerRefreshAndRecovery(t *testing.T) {
	g, p, rec, tok := quietGate(t)
	quiet := int(p.Quiet / time.Second)
	now := at(quiet + 2)
	eff := g.NoteCrossCheck(tok, CrossCheckDisagree, "size mismatch", now)
	if !eff.RetainRESTBook || !eff.Resnapshot || !g.F5Quarantined("A") || g.Actionable("A", now) {
		t.Fatalf("mismatch: %+v", eff)
	}
	if !g.RESTReducerActionable("A", now) || g.RESTReducerActionable("B", now) {
		t.Fatal("REST source not restricted to F5 market")
	}
	// Deltas cannot relicense the refuted websocket book or end REST refresh.
	g.ApplyFrame(deltaFrame("A"), func() error { return nil }, at(quiet+3))
	if !g.F5Quarantined("A") || g.Actionable("A", at(quiet+3)) {
		t.Fatal("delta lifted F5")
	}
	later := at(2*quiet + 3)
	if g.RESTReducerActionable("A", later) {
		t.Fatal("stale REST/truth licensed")
	}
	checks := checksFor(g.Tick(later), "A")
	if len(checks) != 1 || !checks[0].Refresh {
		t.Fatalf("no REST refresh: %+v", checks)
	}
	refreshed := g.NoteCrossCheck(checks[0].Token, CrossCheckAgree, "", later)
	if !refreshed.RetainRESTBook || refreshed.ReplaceBook || g.Actionable("A", later) {
		t.Fatalf("refresh licensed websocket: %+v", refreshed)
	}
	// Old truth cannot satisfy recovery even after an accepted replacement snapshot.
	g.ApplyFrame(snapFrame("A"), func() error { return nil }, later)
	if g.Actionable("A", later) || !g.F5Quarantined("A") {
		t.Fatal("snapshot bypassed post-quarantine reconciliation")
	}
	for k := Truth(0); k < truthCount; k++ {
		g.noteTruth(k, rec, later)
	}
	if !g.Actionable("A", later) || g.F5Quarantined("A") || g.RESTReducerActionable("A", later) {
		t.Fatal("snapshot+reconciliation did not retire REST source")
	}
}

func TestF5QuarantineEscalatesOnce(t *testing.T) {
	g, p, _, tok := quietGate(t)
	now := at(int(p.Quiet/time.Second) + 2)
	g.NoteCrossCheck(tok, CrossCheckDisagree, "mismatch", now)
	later := now
	later.Mono += p.DisconnectReduce + time.Nanosecond
	if !hasClass(g.Tick(later).Anomalies, "BOOK_QUARANTINE_SUSTAINED") {
		t.Fatal("no SEV1 escalation")
	}
	if hasClass(g.Tick(later).Anomalies, "BOOK_QUARANTINE_SUSTAINED") {
		t.Fatal("repeated escalation")
	}
}

func TestF5SlowReadDoesNotBecomeFreshAtCompletion(t *testing.T) {
	g, p, rec, tok := quietGate(t)
	later := at(2*int(p.Quiet/time.Second) + 3)
	for k := Truth(0); k < truthCount; k++ {
		g.noteTruth(k, rec, later)
	}
	eff := g.NoteCrossCheck(tok, CrossCheckDisagree, "slow read", later)
	if !eff.RetainRESTBook {
		t.Fatal("valid read not retained")
	}
	if g.RESTReducerActionable("A", later) {
		t.Fatal("read aged from completion instead of request")
	}
}
