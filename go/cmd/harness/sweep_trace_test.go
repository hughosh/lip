package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"lip/harness/rest"
)

// One sweep, one line, filterable by prefix and decodable as the trace.
func TestSweepTraceWriterWritesOneJSONLinePerSweep(t *testing.T) {
	var buf bytes.Buffer
	write := sweepTraceWriter(&buf)
	write(rest.SweepTrace{Ticker: "T1", Requested: []string{"o1"}, Verdict: "confirmed"})
	write(rest.SweepTrace{Ticker: "T1", Requested: []string{"o2"}, Verdict: "incomplete"})

	lines := strings.Split(strings.TrimSuffix(buf.String(), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("%d lines for two sweeps: %q", len(lines), buf.String())
	}
	for i, want := range []string{"confirmed", "incomplete"} {
		line, ok := strings.CutPrefix(lines[i], sweepTracePrefix)
		if !ok {
			t.Fatalf("line %d lacks the %q prefix: %q", i, sweepTracePrefix, lines[i])
		}
		var got rest.SweepTrace
		if err := json.Unmarshal([]byte(line), &got); err != nil {
			t.Fatalf("line %d is not a trace: %v", i, err)
		}
		if got.Verdict != want {
			t.Fatalf("line %d verdict %q, want %q", i, got.Verdict, want)
		}
	}
}

// The production composition traces the one shared client, which is what both
// the dispatcher's cancels and startup's adoption sweep go through.
func TestTheRigTracesItsCancelSweeps(t *testing.T) {
	h := newSeamHarness(t, seamOptions{})
	if h.rig.api.SweepTrace == nil {
		t.Fatal("the rig's exchange client has no sweep trace; a live stage's " +
			"harness.log would again hold none of the per-sweep evidence " +
			"lip-kaf needed")
	}
}
