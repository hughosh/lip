package wsx

import "testing"

func TestForcedCrossCheckUsesF5TokenAndOnlyHoldsItsMarket(t *testing.T) {
	g, err := NewGate([]string{"A", "B"}, testParams())
	if err != nil {
		t.Fatal(err)
	}
	ce := g.OnConnect(at(0))
	for _, ticker := range []string{"A", "B"} {
		g.ApplyFrame(snapFrame(ticker), okHandle, at(0))
	}
	for _, truth := range []Truth{TruthPositions, TruthOrders, TruthFills} {
		g.noteTruth(truth, ce.Token, at(0))
	}
	if !g.Actionable("A", at(1)) || !g.Actionable("B", at(1)) {
		t.Fatal("setup: both markets must be actionable")
	}
	lastFrame := g.markets["A"].lastFrame
	req, ok := g.ForceCrossCheck("A", at(1))
	if !ok || !req.Token.Valid() || req.Ticker != "A" || req.Token.Ticker() != "A" || req.Refresh {
		t.Fatalf("forced request=%+v, ok=%v", req, ok)
	}
	if g.markets["A"].lastFrame != lastFrame {
		t.Fatal("forcing a read forged a fresh websocket frame")
	}
	if g.Actionable("A", at(1)) || !g.Actionable("B", at(1)) {
		t.Fatal("forced check did not hold exactly A")
	}
	if _, ok := g.ForceCrossCheck("A", at(1)); ok {
		t.Fatal("a second request was issued with a check outstanding")
	}
	eff := g.NoteCrossCheck(req.Token, CrossCheckAgree, "", at(2))
	if !eff.Accepted || !g.Actionable("A", at(2)) || g.markets["A"].lastFrame != at(2).Mono {
		t.Fatalf("ordinary F5 agreement did not restore the market: %+v", eff)
	}
	if _, ok := g.ForceCrossCheck("A", at(3)); !ok {
		t.Fatal("a later cross rejection could not request a new check")
	}
}

func TestForcedCrossCheckCannotLicenseAbsentOrRetiredBook(t *testing.T) {
	g, err := NewGate([]string{"A"}, testParams())
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := g.ForceCrossCheck("A", at(0)); ok {
		t.Fatal("disconnected book got a cross-check token")
	}
	g.OnConnect(at(0))
	if _, ok := g.ForceCrossCheck("A", at(0)); ok {
		t.Fatal("unsnapshotted book got a cross-check token")
	}
	g.ApplyFrame(snapFrame("A"), okHandle, at(0))
	req, ok := g.ForceCrossCheck("A", at(1))
	if !ok {
		t.Fatal("current book got no token")
	}
	g.ApplyDisconnect(at(2), true)
	if eff := g.NoteCrossCheck(req.Token, CrossCheckAgree, "", at(3)); eff.Accepted {
		t.Fatal("an old connection's result was credited")
	}
	if _, ok := g.ForceCrossCheck("A", at(3)); ok {
		t.Fatal("disconnected book got a new check")
	}
	g.OnConnect(at(4))
	g.ApplyFrame(snapFrame("A"), okHandle, at(4))
	g.markets["A"].quarantined = true
	if _, ok := g.ForceCrossCheck("A", at(5)); ok {
		t.Fatal("quarantined book got a cross-check token")
	}
}

func TestRejectRateReductionIsMarketScopedAndSticky(t *testing.T) {
	g, err := NewGate([]string{"A", "B"}, testParams())
	if err != nil {
		t.Fatal(err)
	}
	if !g.NoteRejectRate("A") || !g.Reducing("A") || g.Reducing("B") {
		t.Fatal("reject-rate reduction was not confined to A")
	}
	if g.NoteRejectRate("A") || g.NoteRejectRate("unknown") {
		t.Fatal("reduction was repeated or admitted an unknown market")
	}
	g.OnConnect(at(0))
	if !g.Reducing("A") || g.Reducing("B") {
		t.Fatal("reconnect cleared or spread reject-rate reduction")
	}
}
