package main

import (
	"encoding/json"
	"fmt"
	"io"
	"sync"

	"lip/harness/rest"
)

// sweepTracePrefix starts every trace line, so a stage's harness.log can be
// filtered to them.
const sweepTracePrefix = "harness: sweep-trace "

// sweepTraceWriter writes each cancel sweep's trace as one JSON line (lip-kaf).
//
// The destination is the process's own output, which the operator's launch
// command captures as the stage's harness.log. It is deliberately NOT the
// anomaly journal: SEV3 rows ride the ntfy heartbeat body, and a trace per
// sweep would crowd the one alert route there is. A write that fails loses a
// diagnostic line and nothing else.
func sweepTraceWriter(w io.Writer) func(rest.SweepTrace) {
	var mu sync.Mutex
	return func(t rest.SweepTrace) {
		b, err := json.Marshal(t)
		if err != nil {
			return
		}
		mu.Lock()
		defer mu.Unlock()
		fmt.Fprintf(w, "%s%s\n", sweepTracePrefix, b)
	}
}

// confidence: high
