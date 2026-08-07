// Package wsx is the harness's own websocket client and the ownership wiring
// that hangs off it: pilot-plan.md §7.4.
//
// It is deliberately NOT `feed`. `feed` is the shadow rig's socket and is
// hash-pinned, because its job is to be bug-for-bug identical to `rig.py` --
// including the absence of a client-side keepalive and of any read deadline,
// which is a faithful reproduction of a real weakness. A harness that trades
// cannot inherit that: a half-open socket there means a book that stops
// updating while the process believes it is connected, and quotes resting
// against a world that has moved. So the signer is shared through an interface
// and nothing else is.
//
// # The seams, and why they are where they are
//
// Four interfaces -- Signer, Socket, Dialer, Clock -- exist so that every
// behaviour below can be tested without a network or a wall clock. The
// scenario exchange of pilot-plan.md §3 (`lip-b1r`) plugs in at exactly these
// points. They are at the FRAME level rather than the message level for the
// same reason `rest.Doer` is at the HTTP level: a typed seam cannot express a
// truncated frame, a socket that accepts a write and never answers, or a close
// that arrives between the ping and the pong.
//
// # What this package refuses to do
//
// It does not decide market state, does not place or cancel anything, and does
// not own the position. It reports what it saw -- connected, this frame,
// disconnected, quarantined, stale -- and what that licenses. The decision
// belongs to the state machine in `quote` and the lifecycle in `cmd/harness`.
//
// H-FAIL-2 is the structural half of that: REST and the websocket are distinct
// transports, and a socket failure must never cancel the portfolio poller.
// This is enforced by construction -- the session's context is derived inside
// the supervisor and is never the context the poller runs on -- and not by
// remembering to do it.
package wsx

// confidence: high
