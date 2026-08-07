// Package lifecycle is the IMPURE half of the harness's safety story: the
// durable halt latch (H-HALT-4), ordered startup adoption (§7.5), the
// foreign-activity policy (H-ORD-9), signal and drain handling (H-HALT-3), the
// single-instance lock (H-DEP-5), the host-sleep detector (F7) and the launchd
// plan (H-DEP-2, H-DEP-3).
//
// # Why this package exists at all
//
// `harness/quote` and `harness/risk` are pure by an explicit package rule: no
// clock, no I/O, no goroutines, so §14's simulator can replay a tape and get a
// deterministic trace. Every mechanism in this file's package needs a disk, a
// signal, or a file lock. Putting any of them in `quote` or `risk` would trade
// the one property that makes the state machine testable for the convenience of
// not having a package boundary.
//
// It is equally not `cmd/harness`. `cmd/harness` is wiring, and wiring is the
// thing this harness has the least evidence about — it is unbuilt (`lip-3af`)
// and its gates have not run. A protocol that only exists inside a `main` is a
// protocol that cannot be mutation-tested, and every rule below is one the
// negative control has to be able to break on purpose.
//
// # What this package does NOT do
//
// Nothing here registers a signal handler, publishes a snapshot sequence, opens
// a socket, writes a record, delivers an alert, or exits a process. Those are
// named deferrals, not omissions:
//
//   - `lip-6w5` owns the durable ownership ledger behind `risk.OwnershipLookup`,
//     the five records including `state_event`, queued anomaly delivery, the
//     `STARTUP` ping, heartbeat delivery, and durable uptime accounting. This
//     package returns anomalies and effects; it never delivers them.
//   - `lip-3af` owns the concrete adoption policy, `os/signal.Notify`
//     registration, the owner goroutine and its `seq` publication, the live REST
//     writer and H-VER-1's two-part write guard, the real process exit after a
//     verified drain, launchd installation, and every path and flag.
//   - `lip-0qj` owns both websocket owner-loop relays.
//
// No in-memory ledger, permissive adoption policy, or no-op production sink
// stands in for any of these. Every injected interface here is REQUIRED, and a
// nil one fails construction rather than defaulting to something harmless
// looking. A default is how a deferral becomes a silent production behaviour:
// the harness would run, the gate would pass, and the thing that was supposed to
// classify a fill would be a function that returns true.
//
// # The one invariant the whole package is arranged around
//
// I1: every stop path stops ADDING risk; no stop path stops reducing it, and no
// stop path stops watching it. So the failure response to every uncertainty
// below — an unreadable latch, a failed latch write, a partial adoption, a drain
// that has run past its timeout — is the same shape: block adding, keep
// cancelling, keep reducing, keep monitoring, keep retrying, and tell the
// operator. Not one of them exits, and not one of them cancels a reducer.
//
// # The advancing-sequence monitor is NOT here
//
// pilot-plan §7 item 5 lists it with this unit. It already exists and is already
// ratcheted: `risk.MonitorState.Step` marks a sample stale when `Seq` has not
// advanced, `risk.A5Tracker` asserts A5 over what the monitor actually produced,
// and M1, M14 and M14b break all three on purpose. What is missing is a
// `seq`-PUBLISHING owner, which is `lip-3af`'s wiring. Rewriting the working
// detector here would replace a mutation-tested mechanism with an untested one.
package lifecycle

// confidence: high
