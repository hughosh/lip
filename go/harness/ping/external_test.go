// Package ping_test is deliberately EXTERNAL. It sees exactly what a production
// caller sees, which is the only vantage point from which "the credential cannot
// be read out of this type" is a checkable claim.
package ping_test

import (
	"fmt"
	"reflect"
	"strings"
	"testing"

	"lip/harness/ping"
)

// TestPingSurfaceKeepsSecretsOpaque is the structural half of §13's credential
// boundary.
//
// Two values here are bearer secrets: the ntfy topic, which is the channel, and
// the dead-man endpoint, which can suppress the F18 alarm indefinitely. The seal
// is not "you cannot construct one" -- Go permits `ping.Topic{}` from any
// package, since every field is unexported -- it is that the constructible one
// holds nothing and that neither type will render what it holds.
//
// The `%v` check is the one that matters in practice. A struct with an
// unexported STRING field is printed in full by `fmt.Sprintf("%v", ...)`, which
// is exactly how a credential reaches a log: somebody adds the sender to a debug
// print. Holding the value behind a closure makes that print an address.
func TestPingSurfaceKeepsSecretsOpaque(t *testing.T) {
	// --- the zero values hold nothing ---------------------------------------
	if (ping.Topic{}).Valid() {
		t.Fatal("the zero Topic is valid")
	}
	if _, err := ping.NewNTFYSender(ping.Topic{}); err == nil {
		t.Fatal("a sender was built from a forged Topic")
	}
	if _, err := ping.NewHTTPSDeadman(""); err == nil {
		t.Fatal("a dead man was built from an empty endpoint")
	}

	// --- no renderer, on either receiver ------------------------------------
	forbidden := []string{"String", "GoString", "Format", "MarshalText",
		"MarshalJSON", "MarshalBinary", "Reveal", "Secret", "Value", "URL",
		"Endpoint", "Topic"}
	for _, ty := range []reflect.Type{
		reflect.TypeOf(ping.Topic{}),
		reflect.TypeOf(&ping.NTFYSender{}),
		reflect.TypeOf(&ping.HTTPSDeadman{}),
	} {
		for _, name := range forbidden {
			if _, ok := ty.MethodByName(name); ok {
				t.Fatalf("%s has a %s method; the only correct rendering of a "+
					"bearer credential is not to render it", ty, name)
			}
		}
	}

	// --- no exported field on any secret-bearing type ------------------------
	for _, ty := range []reflect.Type{
		reflect.TypeOf(ping.Topic{}),
		reflect.TypeOf(ping.NTFYSender{}),
		reflect.TypeOf(ping.HTTPSDeadman{}),
		reflect.TypeOf(ping.Service{}),
	} {
		for i := 0; i < ty.NumField(); i++ {
			if f := ty.Field(i); f.PkgPath == "" {
				t.Fatalf("%s.%s is exported; the credential would be one field "+
					"read away from any caller", ty.Name(), f.Name)
			}
		}
	}

	// --- %v does not spill it ------------------------------------------------
	//
	// A forged Topic is used deliberately: it is the only one this package can
	// build, and if the FIELD SHAPE were a plain string, `%v` on a loaded one
	// would print it. Asserting the shape here is what makes that impossible.
	ty := reflect.TypeOf(ping.Topic{})
	for i := 0; i < ty.NumField(); i++ {
		if ty.Field(i).Type.Kind() == reflect.String {
			t.Fatalf("Topic holds its value in a string field, so "+
				"fmt.Sprintf(\"%%v\", topic) prints the credential; %s must be "+
				"held behind a closure", ty.Field(i).Name)
		}
	}
	if out := fmt.Sprintf("%v", ping.Topic{}); strings.Contains(out, "=") {
		t.Fatalf("the zero Topic renders as %q", out)
	}
}
