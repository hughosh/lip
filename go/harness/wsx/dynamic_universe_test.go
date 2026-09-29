package wsx

import (
	"context"
	"strings"
	"testing"
)

func TestDynamicUniverseRetiresOldEvidenceAndKeepsHeldMarket(t *testing.T) {
	g, err := NewGate([]string{"HELD"}, testParams())
	if err != nil {
		t.Fatal(err)
	}
	old := g.OnConnect(at(0))
	g.ApplyFrame(snapFrame("HELD"), okHandle, at(1))
	for k := Truth(0); k < truthCount; k++ {
		g.noteTruth(k, old.Token, at(1))
	}
	if !g.Actionable("HELD", at(1)) {
		t.Fatal("fixture's held market is not actionable")
	}
	if !g.AcceptsUniverse(0) {
		t.Fatal("initial legacy event lost single-market compatibility")
	}
	grown, err := g.AddMarkets(at(2), []string{"NEW"})
	if err != nil {
		t.Fatal(err)
	}
	if got := g.Tickers(); len(got) != 2 || got[0] != "HELD" || got[1] != "NEW" {
		t.Fatalf("managed markets = %v", got)
	}
	if g.Actionable("HELD", at(2)) || g.BookCurrent("HELD") || g.Actionable("NEW", at(2)) {
		t.Fatal("expansion did not retire old book authority")
	}
	if g.AcceptsUniverse(0) {
		t.Fatal("legacy unversioned event survived expansion")
	}
	revisionAfterAdd := g.UniverseRevision()
	if again, err := g.AddMarkets(at(2), []string{"NEW"}); err != nil || again.Valid() ||
		g.UniverseRevision() != revisionAfterAdd {
		t.Fatalf("idempotent gate addition changed revision or minted a token: err=%v revision=%d token=%v",
			err, g.UniverseRevision(), again.Valid())
	}
	if g.noteTruth(TruthPositions, old.Token, at(2)) {
		t.Fatal("pre-expansion read refreshed truth")
	}
	// The socket is down until the re-dial succeeds, and portfolio truth keeps
	// flowing under the expansion's token without licensing a placement.
	if !grown.Valid() || !g.noteTruth(TruthPositions, grown, at(2)) {
		t.Fatal("expansion token cannot carry portfolio truth through the re-dial")
	}
	if g.Actionable("HELD", at(2)) || g.Actionable("NEW", at(2)) {
		t.Fatal("expansion token made a market actionable before a new connection")
	}
	if _, ok := g.OnConnectForUniverse(at(3), 1); ok {
		t.Fatal("buffered old connection was accepted")
	}
	if g.OnConnect(at(3)).Token.Valid() {
		t.Fatal("legacy connect bypassed subscription revision")
	}
	called := false
	oldFrame := g.ApplyFrameForUniverse(snapFrame("HELD"), func() error {
		called = true
		return nil
	}, at(3), 1)
	if called || oldFrame.Delivered {
		t.Fatal("old subscribed frame reached core")
	}
	if g.ApplyFrame(snapFrame("HELD"), func() error {
		called = true
		return nil
	}, at(3)).Delivered || called {
		t.Fatal("legacy frame route bypassed subscription revision")
	}
	revision := g.UniverseRevision()
	current, ok := g.OnConnectForUniverse(at(4), revision)
	if !ok || !current.Token.Valid() {
		t.Fatal("new subscription was not accepted")
	}
	if g.ApplyFrameForUniverse(snapFrame("HELD"), okHandle, at(5), revision).Delivered == false ||
		g.ApplyFrameForUniverse(snapFrame("NEW"), okHandle, at(5), revision).Delivered == false {
		t.Fatal("current snapshots were not delivered")
	}
	for k := Truth(0); k < truthCount; k++ {
		g.noteTruth(k, current.Token, at(6))
	}
	if !g.Actionable("HELD", at(6)) || !g.Actionable("NEW", at(6)) {
		t.Fatal("held and new markets did not recover after current snapshots and truth")
	}
	refreshed := g.RefreshUniverse(at(7))
	if g.UniverseRevision() != revision+1 || g.AcceptsUniverse(revision) ||
		g.Actionable("HELD", at(7)) || g.Actionable("NEW", at(7)) {
		t.Fatal("same-set target refresh retained prior book authority")
	}
	if g.noteTruth(TruthOrders, current.Token, at(7)) ||
		!refreshed.Valid() || !g.noteTruth(TruthOrders, refreshed, at(7)) {
		t.Fatal("target refresh did not hand portfolio truth to a new-generation token")
	}
	if g.Actionable("HELD", at(7)) || g.Actionable("NEW", at(7)) {
		t.Fatal("refresh token made a market actionable before a new connection")
	}
}

func TestDynamicUniverseSupervisorReconnectsWithFullSubscription(t *testing.T) {
	clk := newFakeClock()
	d := newScriptedDialer()
	first, second, third := newScriptedSocket(), newScriptedSocket(), newScriptedSocket()
	d.push(first, nil)
	d.push(second, nil)
	d.push(third, nil)
	s, err := NewSupervisor(&fakeSigner{}, d, clk, testParams(), []string{"HELD"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	events := make(chan Event, 16)
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx, nil, events) }()
	connected := waitEvent(t, events, EventConnected)
	if connected.UniverseRevision != 1 {
		t.Fatalf("initial revision = %d", connected.UniverseRevision)
	}
	first.frames <- []byte(`{"type":"subscribed","id":1}`)
	if frame := waitEvent(t, events, EventFrame); frame.UniverseRevision != 1 {
		t.Fatalf("old frame revision = %d", frame.UniverseRevision)
	}
	if err := s.AddMarkets([]string{"HELD", "NEW"}); err != nil {
		t.Fatal(err)
	}
	if s.UniverseRevision() != 2 {
		t.Fatal("expansion did not advance subscription revision")
	}
	down := waitEvent(t, events, EventDisconnected)
	if down.UniverseRevision != 1 || !down.Clean {
		t.Fatalf("retired socket event = %+v", down)
	}
	connected = waitEvent(t, events, EventConnected)
	if connected.UniverseRevision != 2 {
		t.Fatalf("replacement connection revision = %d", connected.UniverseRevision)
	}
	if second.writtenCount() != 2 {
		t.Fatalf("replacement subscription writes = %d", second.writtenCount())
	}
	if !strings.Contains(string(second.written(0)), `"market_tickers":["HELD","NEW"]`) {
		t.Fatalf("replacement subscription = %s", second.written(0))
	}
	second.frames <- []byte(`{"type":"subscribed","id":1}`)
	if frame := waitEvent(t, events, EventFrame); frame.UniverseRevision != 2 {
		t.Fatalf("new frame revision = %d", frame.UniverseRevision)
	}
	if err := s.AddMarkets([]string{"NEW"}); err != nil {
		t.Fatal(err)
	}
	if s.UniverseRevision() != 2 {
		t.Fatal("idempotent addition advanced revision")
	}
	s.RefreshUniverse()
	if down := waitEvent(t, events, EventDisconnected); down.UniverseRevision != 2 {
		t.Fatalf("same-set refresh disconnect revision = %d", down.UniverseRevision)
	}
	if connected := waitEvent(t, events, EventConnected); connected.UniverseRevision != 3 {
		t.Fatalf("same-set refresh connection revision = %d", connected.UniverseRevision)
	}
	if third.writtenCount() != 2 {
		t.Fatalf("same-set refresh writes = %d", third.writtenCount())
	}
	if !strings.Contains(string(third.written(0)), `"market_tickers":["HELD","NEW"]`) {
		t.Fatal("same-set refresh lost managed market subscription")
	}
	cancel()
	<-done
}
