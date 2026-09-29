// Package wsx_test is deliberately EXTERNAL. It sees exactly what a production
// caller sees, which is the only vantage point from which "there is no escape
// hatch" is a checkable claim: an in-package test can reach every private
// field and would pass whether the seal held or not.
package wsx_test

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"lip/harness/cfg"
	"lip/harness/wsx"
)

// TestPortfolioTruthHasNoPublicBypass is the structural half of A13.
//
// The gate's whole purpose is that placement requires a snapshot from the
// current connection AND three complete portfolio walks reconciled against it.
// Every one of those is expensive and slow, and every one of them is exactly
// the thing an integration under time pressure reaches around.
//
// The shapes that reach around it are:
//
//   - an exported `NoteTruth`. It is a `SetActionable(true)` with a longer
//     name: three calls and one snapshot and the gate is open with no complete
//     walk anywhere in the story.
//   - a settable field on `PortfolioRead`. A caller that can assemble one can
//     assemble a complete-looking reconciliation out of nothing, and hand it
//     to `ApplyPortfolio`, which will believe it.
//
// So both are asserted absent from the exported surface rather than merely
// unused today. `ApplyPortfolio` is the only production path that advances
// truth, and it advances it only from a walk that reported `WalkComplete`.
func TestPortfolioTruthHasNoPublicBypass(t *testing.T) {
	gate := reflect.TypeOf(&wsx.Gate{})
	for i := 0; i < gate.NumMethod(); i++ {
		name := gate.Method(i).Name
		if strings.Contains(strings.ToLower(name), "notetruth") {
			t.Fatalf("*Gate exports %s: any caller can declare a portfolio "+
				"endpoint reconciled without a complete walk having happened, "+
				"which is SetActionable(true) with a longer name", name)
		}
		if strings.Contains(strings.ToLower(name), "setactionable") {
			t.Fatalf("*Gate exports %s", name)
		}
	}

	read := reflect.TypeOf(wsx.PortfolioRead{})
	for i := 0; i < read.NumField(); i++ {
		f := read.Field(i)
		if f.IsExported() {
			t.Fatalf("PortfolioRead exports %s: a caller that can populate a "+
				"read can manufacture a complete-looking reconciliation and "+
				"hand it to ApplyPortfolio, which will believe it", f.Name)
		}
	}

	// The accessors that DO exist are read-only, and there must be at least
	// one -- a type with no accessors would pass the loop above trivially.
	if _, ok := read.MethodByName("Seq"); !ok {
		t.Fatal("PortfolioRead has no Seq accessor; the loop above would pass " +
			"for a type nobody can read either")
	}
	if _, ok := read.MethodByName("CompletedAt"); !ok {
		t.Fatal("PortfolioRead has no CompletedAt accessor")
	}
	startedAt, ok := read.MethodByName("StartedAt")
	if !ok {
		t.Fatal("PortfolioRead has no StartedAt accessor")
	}
	wantStartedAt := reflect.TypeOf(func(wsx.PortfolioRead, wsx.Truth) wsx.Stamp { return wsx.Stamp{} })
	if startedAt.Type != wantStartedAt {
		t.Fatalf("PortfolioRead.StartedAt has type %s, want exact read-only diagnostic signature %s", startedAt.Type, wantStartedAt)
	}

	// StartedAt returns a Stamp value, so callers cannot mutate the private
	// read by changing a returned diagnostic. Check the zero-value path too.
	zeroRead := wsx.PortfolioRead{}
	stamp := zeroRead.StartedAt(wsx.TruthFills)
	stamp.WallMs = 1
	stamp.Mono = 1
	if got := zeroRead.StartedAt(wsx.TruthFills); got != (wsx.Stamp{}) {
		t.Fatalf("mutating a returned StartedAt stamp changed PortfolioRead: %+v", got)
	}

	// A private field with an exported setter is the same hole with one more
	// step in it, so the exported method set is pinned to the read-only
	// diagnostic accessors. Both value and pointer receivers are checked: a
	// mutator would naturally be written on the pointer.
	allowed := map[string]bool{"Seq": true, "CompletedAt": true, "StartedAt": true, "RateLimits": true}
	for _, typ := range []reflect.Type{read, reflect.PointerTo(read)} {
		for i := 0; i < typ.NumMethod(); i++ {
			if name := typ.Method(i).Name; !allowed[name] {
				t.Fatalf("%s exports the method %s; the only exported surface "+
					"on a portfolio read may be read-only diagnostics, or a "+
					"caller can populate one and manufacture a reconciliation",
					typ, name)
			}
		}
	}

	// The gate's own state is private too, so the seal is not one exported
	// field away from being pointless.
	g := reflect.TypeOf(wsx.Gate{})
	for i := 0; i < g.NumField(); i++ {
		if g.Field(i).IsExported() {
			t.Fatalf("Gate exports the field %s", g.Field(i).Name)
		}
	}

	// A token nobody was issued licenses nothing, and the zero value is not a
	// token. This is the same argument that made rest.WalkUnset the zero value.
	var zero wsx.ReconcileToken
	if zero.Valid() {
		t.Fatal("the zero ReconcileToken reports itself valid")
	}
	tok := reflect.TypeOf(wsx.ReconcileToken{})
	for i := 0; i < tok.NumField(); i++ {
		if tok.Field(i).IsExported() {
			t.Fatalf("ReconcileToken exports %s, so a caller can forge one "+
				"for the current generation", tok.Field(i).Name)
		}
	}
}

// TestCrossCheckHasNoPublicBypass is the structural half of F5's agree branch.
//
// The branch is new and it is the only thing in this package that can return a
// market to QUOTING without a websocket snapshot: `NoteCrossCheck(tok,
// CrossCheckAgree, ...)` resets the staleness clock and clears the hold that
// stops placement. So the token is the whole of its safety, and everything that
// makes a token forgeable is asserted absent from the exported surface rather
// than merely unused today.
//
// The shapes that reach around it are the same two `TestPortfolioTruthHasNoPublicBypass`
// names, one layer along:
//
//   - exported fields on `CrossCheckToken`, so a caller can build one for the
//     current generation and declare a silent book verified;
//   - an exported `Gate` method that clears the hold, resets the clock or marks
//     a market checked WITHOUT a token -- a `SetActionable(true)` reachable from
//     a REST response.
//
// The behavioural half is that a token nobody was issued is refused, which is
// checked here from OUTSIDE the package, where a test cannot reach a private
// field to make it true.
func TestCrossCheckHasNoPublicBypass(t *testing.T) {
	tok := reflect.TypeOf(wsx.CrossCheckToken{})
	for i := 0; i < tok.NumField(); i++ {
		if tok.Field(i).IsExported() {
			t.Fatalf("CrossCheckToken exports %s, so a caller can forge one for "+
				"the current generation and reset a wedged market's staleness "+
				"clock without any read having happened", tok.Field(i).Name)
		}
	}
	var zero wsx.CrossCheckToken
	if zero.Valid() {
		t.Fatal("the zero CrossCheckToken reports itself valid")
	}

	// The gate's only cross-check entry point is the one that takes a token.
	// Anything shaped like "mark this market checked" or "clear the quiet hold"
	// is the bypass, whatever it is called.
	gate := reflect.TypeOf(&wsx.Gate{})
	for i := 0; i < gate.NumMethod(); i++ {
		name := strings.ToLower(gate.Method(i).Name)
		for _, banned := range []string{
			"setcrosscheck", "clearquiet", "markchecked", "setquiet",
			"resetquiet", "notequiet",
		} {
			if strings.Contains(name, banned) {
				t.Fatalf("*Gate exports %s: F5's agree branch must be reachable "+
					"only through a token the gate itself issued",
					gate.Method(i).Name)
			}
		}
	}

	// And the refusal is real, not merely undocumented. A forged token cannot
	// be made current from out here, so an accepted one would be a gate that
	// checks nothing.
	g, err := wsx.NewGate([]string{"KXSEAL-26AUG08-T1"}, cfg.Default())
	if err != nil {
		t.Fatal(err)
	}
	now := wsx.Stamp{WallMs: 1_700_000_000_000, Mono: time.Minute}
	g.OnConnect(now)
	for _, out := range []wsx.CrossCheckOutcome{
		wsx.CrossCheckAgree, wsx.CrossCheckDisagree,
		wsx.CrossCheckGranularity, wsx.CrossCheckUnavailable,
	} {
		if eff := g.NoteCrossCheck(zero, out, "forged", now); eff.Accepted {
			t.Fatalf("the zero CrossCheckToken was accepted for outcome %v", out)
		}
	}
}

// TestProductionClockIsBehindItsInterface keeps the injected-clock seam whole.
//
// Every deadline in the package is measured against `Clock`. A caller holding
// the concrete production clock could read a second one, and the scenario
// exchange could not substitute for it.
func TestProductionClockIsBehindItsInterface(t *testing.T) {
	c := wsx.NewSystemClock()
	if reflect.TypeOf(c).Kind() != reflect.Ptr {
		t.Fatalf("NewSystemClock returned %T", c)
	}
	if name := reflect.TypeOf(c).Elem().Name(); name != "" &&
		name[0] >= 'A' && name[0] <= 'Z' {
		t.Fatalf("NewSystemClock returns the exported concrete type %s; it "+
			"must return the Clock interface so nothing can reach past the "+
			"seam every deadline is measured against", name)
	}
}
