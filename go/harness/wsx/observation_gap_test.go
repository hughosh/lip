package wsx

import "testing"

func TestObservationGapRequiresNewReadGenerationAndSnapshot(t *testing.T) {
	g, err := NewGate([]string{fxTicker}, testParams())
	if err != nil {
		t.Fatal(err)
	}
	old := g.OnConnect(at(0)).Token
	for k := Truth(0); k < truthCount; k++ {
		g.noteTruth(k, old, at(0))
	}
	g.ApplyFrame(snapFrame(fxTicker), func() error { return nil }, at(0))
	if !g.Actionable(fxTicker, at(0)) {
		t.Fatal("initial gate not actionable")
	}
	fresh := g.ObservationGap(at(10))
	if !g.Connected() || !fresh.Valid() {
		t.Fatal("gap lost live socket or read token")
	}
	g.ApplyFrame(snapFrame(fxTicker), func() error { return nil }, at(11))
	for k := Truth(0); k < truthCount; k++ {
		if g.noteTruth(k, old, at(12)) {
			t.Fatal("pre-gap walk accepted after fresh snapshot")
		}
	}
	if g.Actionable(fxTicker, at(12)) {
		t.Fatal("snapshot alone authorized placement")
	}
	for k := Truth(0); k < truthCount; k++ {
		g.noteTruth(k, fresh, at(13))
	}
	if !g.Actionable(fxTicker, at(13)) {
		t.Fatal("fresh reads plus snapshot failed to recover")
	}
}
