// Package qual records the compact, durable evidence bundle for a read-only
// qualification run.
//
// The recorder is intentionally independent of cmd/harness.  The command owns
// when an observation is made; this package owns making observations
// concurrency-safe, resumable, deterministic, and durable.  In particular,
// callers compose HTTP evidence below the write guard as:
//
//	counted, err := recorder.WrapDoer(rawDoer)
//	guard, err := rest.NewWriteGuard(counted, arm)
//
// With that ordering, a read-only run's checkpoint can prove that no non-GET
// request crossed the guard.  Would-write decisions are recorded separately,
// before the guard, through RecordWouldWrite.
package qual

// confidence: high
