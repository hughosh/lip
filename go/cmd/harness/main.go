// Command harness is the 24/7 LIP quoting harness of notes/harness-spec.md.
//
// It is NOT yet wired. The verification order in §17 is also the build order,
// and the terminal gates (V5 negative control, V6 dry run, V7 live minimum
// size) have not been run. Until they have, this binary refuses to start.
//
// That refusal is structural rather than a TODO comment on purpose: a
// half-wired trading binary that starts and does almost nothing is exactly the
// artifact that gets run "just to see", and this one would do so against a real
// account with real credentials on disk.
package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr,
		"harness: not wired yet -- refusing to start.\n"+
			"Build order and gates: notes/harness-spec.md §17.\n"+
			"Progress so far: harness/num (Qty), harness/cfg (§16 params),\n"+
			"harness/quote (states, skew, external_best, requote, queue,\n"+
			"machine), harness/risk (Snapshot, MonitorState, A5),\n"+
			"cmd/harness monitor loop + M1/M14.")
	os.Exit(2)
}

// confidence: high
