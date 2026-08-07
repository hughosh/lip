// Package lifecycle_test is deliberately EXTERNAL, for the reason
// `wsx/seal_test.go` gives: it sees exactly what a production caller sees, which
// is the only vantage point from which "there is no escape hatch" is a checkable
// claim. An in-package test can reach every private field and would pass whether
// the seal held or not.
package lifecycle_test

import (
	"reflect"
	"strings"
	"testing"

	"lip/harness/lifecycle"
)

// TestLifecycleSurfaceCannotClearLatchOrForgeCompleteAdoption is the structural
// half of this whole unit.
//
// Two objects in this package are LICENCES, and both are expensive to obtain:
//
//   - the latch's absence licenses adding. H-HALT-4: "the harness never
//     self-clears it. Clearing is the operator action of §10.4." A method that
//     clears, removes or resets it is that sentence deleted, and it would be
//     reachable from any code path that believed it had a good reason.
//   - an `Adoption` licenses leaving STARTING. Obtaining one honestly costs four
//     complete walks, an ownership classification, a policy pass over every
//     adopted order and a verified clean sweep. A settable field on it is all of
//     H-ORD-5 replaced by a composite literal -- and the literal a hurried
//     integration would write is `&Adoption{}`, which asserts the account is flat,
//     nothing rests, and nothing is foreign.
//
// So both are asserted absent from the EXPORTED surface rather than merely
// unused today. Value and pointer method sets are both checked: a mutator would
// naturally be written on the pointer.
func TestLifecycleSurfaceCannotClearLatchOrForgeCompleteAdoption(t *testing.T) {
	// --- the latch has no clear ---------------------------------------------
	forbidden := []string{"clear", "remove", "reset", "delete", "unlatch",
		"overwrite", "resume", "unset", "truncate", "forget"}
	latchTypes := []reflect.Type{
		reflect.TypeOf(lifecycle.FileLatch{}),
		reflect.TypeOf(&lifecycle.FileLatch{}),
		reflect.TypeOf((*lifecycle.LatchStore)(nil)).Elem(),
	}
	for _, typ := range latchTypes {
		for i := 0; i < typ.NumMethod(); i++ {
			name := typ.Method(i).Name
			lower := strings.ToLower(name)
			for _, bad := range forbidden {
				if strings.Contains(lower, bad) {
					t.Fatalf("%s exports %s: H-HALT-4 makes clearing the latch "+
						"the operator action of §10.4, and a method that does "+
						"it puts the halt one call away from self-clearing",
						typ, name)
				}
			}
		}
	}

	// FileLatch's own state is private, so the seal is not one exported field
	// away from being pointless -- a settable `path` would let a caller point a
	// live latch at a scratch file.
	fl := reflect.TypeOf(lifecycle.FileLatch{})
	for i := 0; i < fl.NumField(); i++ {
		if fl.Field(i).IsExported() {
			t.Fatalf("FileLatch exports the field %s", fl.Field(i).Name)
		}
	}

	// --- an Adoption cannot be assembled ------------------------------------
	//
	// It is an INTERFACE with an unexported method, and both halves are load
	// bearing. A struct with none of its fields exported was the previous
	// version, and it was still forgeable: Go permits `&lifecycle.Adoption{}`
	// from any package as long as no unexported field is named. The zero value
	// then claims a flat account, nothing resting and nothing foreign -- the
	// single most dangerous assertion this type can make, available to any
	// wiring error.
	ad := reflect.TypeOf((*lifecycle.Adoption)(nil)).Elem()
	if ad.Kind() != reflect.Interface {
		t.Fatalf("Adoption is a %v, not an interface: a concrete exported type "+
			"can be composite-literalled into existence from any package, and "+
			"its zero value is a licence to leave STARTING", ad.Kind())
	}

	// The unexported marker is what makes it unimplementable outside this
	// package. Without it, an external type with eleven matching methods
	// satisfies the interface and H-ORD-5's four walks are optional again.
	marker := false
	for i := 0; i < ad.NumMethod(); i++ {
		if !ad.Method(i).IsExported() {
			marker = true
		}
	}
	if !marker {
		t.Fatal("the Adoption interface has no unexported method, so any " +
			"package can implement it; the licence to leave STARTING would be " +
			"eleven method stubs away from anyone who wanted one")
	}

	// Its exported surface is read-only accessors, pinned by name. Anything not
	// on this list is either a mutator or a second way to construct the licence.
	allowed := map[string]bool{
		"Portfolio": true, "Balance": true, "OwnedFills": true, "Kept": true,
		"Foreign": true, "Managed": true, "Excluded": true, "States": true,
		"Summary": true, "Causes": true, "Anomalies": true,
	}
	for i := 0; i < ad.NumMethod(); i++ {
		m := ad.Method(i)
		if !m.IsExported() {
			continue
		}
		if !allowed[m.Name] {
			t.Fatalf("Adoption exports the method %s; the only exported surface "+
				"on an adoption may be read-only, or H-ORD-5's \"reconcile "+
				"before quoting, always, no exceptions\" is one call away from "+
				"being optional", m.Name)
		}
	}
	// And there must BE accessors, or the loop above passes for a type nobody
	// can read either.
	for _, name := range []string{"Portfolio", "Managed", "Causes"} {
		if _, ok := ad.MethodByName(name); !ok {
			t.Fatalf("Adoption has no %s accessor", name)
		}
	}

	// The zero value of the licence is nil, which is the only thing "no
	// adoption" can look like.
	var zero lifecycle.Adoption
	if zero != nil {
		t.Fatal("the zero Adoption is not nil")
	}
	var at lifecycle.Attempt
	if at.Adoption != nil {
		t.Fatal("a zero Attempt carries a non-nil Adoption")
	}

	// --- the zero values license nothing ------------------------------------
	//
	// Same argument as `rest.WalkUnset` and `wsx.ReconcileToken`: the value a
	// caller gets by forgetting to set anything must be the conservative one.
	var phase lifecycle.Phase
	if phase != lifecycle.PhaseUnset {
		t.Fatal("the zero Phase is not PhaseUnset")
	}
	if phase == lifecycle.PhaseStartup {
		t.Fatal("the zero Phase is PhaseStartup: every caller that forgot to " +
			"set it would get the permissive reading, under which a live third " +
			"party trading the account is merely excluded from selection")
	}
	var decision lifecycle.AdoptionDecision
	if decision != lifecycle.AdoptionUnset {
		t.Fatal("the zero AdoptionDecision is not AdoptionUnset")
	}
	if decision == lifecycle.AdoptionKeep {
		t.Fatal("the zero AdoptionDecision is AdoptionKeep: H-ORD-5c forbids a " +
			"prior incarnation's orders persisting unexamined, and a policy " +
			"that falls through would give exactly that licence")
	}

	// --- the drain cannot be told it may exit -------------------------------
	//
	// `ExitAuthorised` is computed from an observation, never set. A setter on
	// the tracker is HR-009 with a method name: the timeout eventually doing the
	// exact thing the whole document forbids.
	dt := reflect.TypeOf(lifecycle.DrainTracker{})
	for i := 0; i < dt.NumField(); i++ {
		if dt.Field(i).IsExported() {
			t.Fatalf("DrainTracker exports the field %s", dt.Field(i).Name)
		}
	}
	for _, typ := range []reflect.Type{dt, reflect.PointerTo(dt)} {
		for i := 0; i < typ.NumMethod(); i++ {
			lower := strings.ToLower(typ.Method(i).Name)
			if strings.Contains(lower, "authoris") || strings.Contains(lower, "authoriz") ||
				strings.Contains(lower, "forceexit") || strings.Contains(lower, "abandon") {
				t.Fatalf("%s exports %s: a drain timeout is evidence the "+
					"operator is needed, not authority to abandon inventory "+
					"(H-HALT-3, HR-009)", typ, typ.Method(i).Name)
			}
		}
	}
}

// TestNoProductionDefaultStandsInForADeferredCollaborator is the other half of
// this package's boundary.
//
// Every deferral in `doc.go` is enforced by a constructor that refuses nil
// rather than by a comment. A default is how a deferral becomes a silent
// production behaviour: the harness runs, the gates pass, and the thing that was
// supposed to classify a fill is a function that returns true.
func TestNoProductionDefaultStandsInForADeferredCollaborator(t *testing.T) {
	if _, err := lifecycle.NewForeignGuard(nil); err == nil {
		t.Fatal("NewForeignGuard accepted a nil ownership lookup")
	}
	if _, _, err := lifecycle.NewGlobalController(nil); err == nil {
		t.Fatal("NewGlobalController accepted a nil latch store: a latch that " +
			"does not outlive the process is not a latch")
	}
	if _, err := lifecycle.NewFileLatch(""); err == nil {
		t.Fatal("NewFileLatch accepted an empty path")
	}
	if _, err := lifecycle.NewFileLatch("harness.halt"); err == nil {
		t.Fatal("NewFileLatch accepted a relative path: a halt written to one " +
			"directory and read from another self-clears")
	}

	// There is no exported adoption policy in this package. `lip-3af` supplies
	// the concrete one; a permissive stand-in here would be H-ORD-5c deleted,
	// and it would pass every gate in this package.
	pkg := reflect.TypeOf(lifecycle.AdoptionFacts{}).PkgPath()
	if pkg != "lip/harness/lifecycle" {
		t.Fatalf("unexpected package path %q", pkg)
	}
}
