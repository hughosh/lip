// Package hstore_test is deliberately EXTERNAL, for the reason
// `lifecycle/seal_test.go` gives: it sees exactly what a production caller sees,
// which is the only vantage point from which "there is no escape hatch" is a
// checkable claim. An in-package test can reach every private field and would
// pass whether the seal held or not.
package hstore_test

import (
	"reflect"
	"strings"
	"testing"

	"lip/harness/hstore"
)

// TestStoreSurfaceCannotForgeLicencesOrReachSQL is the structural half of this
// unit.
//
// Three things are asserted, and each closes a hole that a composite literal or
// a convenience method would otherwise open:
//
//  1. `RunHandle` and `DispatchPermit` are LICENCES. Go permits
//     `hstore.DispatchPermit{}` from any package as long as no unexported field
//     is named, so the seal cannot be "you cannot build one" -- it has to be
//     "the one you can build asserts nothing". Both zero values are invalid and
//     `Order` refuses.
//  2. Neither licence, and no record payload, has a settable exported field.
//     A permit whose order could be swapped after issue is H-ORD-6 replaced by
//     a struct assignment.
//  3. There is no public SQL. A generic `Query`/`Exec` is how a second writer
//     arrives, how a read starts filtering `owned_order` by run, and how the
//     one place that decides what a fill means stops being one place.
func TestStoreSurfaceCannotForgeLicencesOrReachSQL(t *testing.T) {
	// --- the licences assert nothing when forged -----------------------------
	if (hstore.RunHandle{}).Valid() {
		t.Fatal("the zero RunHandle is valid; it would license records against " +
			"a run row that was never written")
	}
	if (hstore.DispatchPermit{}).Valid() {
		t.Fatal("the zero DispatchPermit is valid")
	}
	if _, err := (hstore.DispatchPermit{}).Order(); err == nil {
		t.Fatal("the zero DispatchPermit released an order: H-ORD-6's barrier " +
			"is reachable by writing `hstore.DispatchPermit{}`")
	}
	if (hstore.Health{}).Healthy() || (hstore.Health{}).AllowsAdding() {
		t.Fatal("the zero Health reports healthy; a caller that failed to " +
			"obtain a real snapshot must not read the failure as permission")
	}
	if (hstore.StateEvent{}).Valid() {
		t.Fatal("the zero StateEvent is valid")
	}
	if (hstore.Receipt{}).Valid() {
		t.Fatal("the zero Receipt is valid")
	}

	// --- no exported fields on the licences or the result --------------------
	sealed := []reflect.Type{
		reflect.TypeOf(hstore.RunHandle{}),
		reflect.TypeOf(hstore.DispatchPermit{}),
		reflect.TypeOf(hstore.Receipt{}),
		reflect.TypeOf(hstore.Health{}),
		reflect.TypeOf(hstore.StateEvent{}),
	}
	for _, ty := range sealed {
		for i := 0; i < ty.NumField(); i++ {
			f := ty.Field(i)
			if f.PkgPath == "" {
				t.Fatalf("%s.%s is exported and therefore settable from "+
					"outside; the invariant this type carries would be one "+
					"assignment away from being edited out", ty.Name(), f.Name)
			}
		}
	}
	// Result carries the licences, and must not expose them as fields a caller
	// could overwrite on a failed result.
	res := reflect.TypeOf(hstore.Result{})
	for i := 0; i < res.NumField(); i++ {
		f := res.Field(i)
		if f.PkgPath != "" {
			continue
		}
		if f.Type == reflect.TypeOf(hstore.RunHandle{}) ||
			f.Type == reflect.TypeOf(hstore.DispatchPermit{}) {
			t.Fatalf("Result.%s exposes a licence as a settable field", f.Name)
		}
	}

	// --- no public SQL -------------------------------------------------------
	forbidden := []string{"query", "exec", "sql", "db", "conn", "raw",
		"execute", "prepare", "statement", "delete", "drop", "truncate"}
	for _, ty := range []reflect.Type{
		reflect.TypeOf(&hstore.Store{}),
		reflect.TypeOf(&hstore.Reader{}),
		reflect.TypeOf(&hstore.Ownership{}),
	} {
		for i := 0; i < ty.NumMethod(); i++ {
			name := strings.ToLower(ty.Method(i).Name)
			for _, bad := range forbidden {
				if strings.Contains(name, bad) {
					t.Fatalf("%s has exported method %s; §15's tables are "+
						"licences rather than a log, and a generic SQL or "+
						"deletion surface is how the one place that decides "+
						"what a fill means stops being one place",
						ty, ty.Method(i).Name)
				}
			}
		}
	}

	// No exported method may accept a raw SQL string either. The read surface
	// is a fixed set of queries; a string parameter on any of them is the
	// escape hatch by another name.
	rd := reflect.TypeOf(&hstore.Reader{})
	allowedStringArgs := map[string]bool{
		"Run": true, "OwnedOrder": true, "Fill": true, "Anomaly": true,
	}
	for i := 0; i < rd.NumMethod(); i++ {
		m := rd.Method(i)
		for a := 1; a < m.Type.NumIn(); a++ {
			if m.Type.In(a).Kind() == reflect.String && !allowedStringArgs[m.Name] {
				t.Fatalf("Reader.%s takes a string argument and is not one of "+
					"the fixed key lookups", m.Name)
			}
		}
	}
}
