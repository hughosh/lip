// Package wsx_test is deliberately EXTERNAL. It sees exactly what a production
// caller sees, which is the only vantage point from which "there is no escape
// hatch" is a checkable claim: an in-package test can reach every private
// field and would pass whether the seal held or not.
package wsx_test

import (
	"reflect"
	"strings"
	"testing"

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

	// A private field with an exported setter is the same hole with one more
	// step in it, so the exported method set is pinned to the two read-only
	// accessors. Both value and pointer receivers are checked: a mutator would
	// naturally be written on the pointer.
	allowed := map[string]bool{"Seq": true, "CompletedAt": true}
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
