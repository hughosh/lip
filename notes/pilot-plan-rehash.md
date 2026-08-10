# Pilot-plan rehash — courier companion

**Date:** 2026-08-09  
**Branch reviewed:** `harness/lip-6w5-v2-checkpoint` at `fb929f7`, plus the
uncommitted `lip-2t6` tree  
**Authority:** companion to `notes/pilot-plan.md`; Beads remains the durable
task source of truth.

## Verdict

Advance `lip-2t6` and commit it as its own unit. The reported full gate is green:
build, vet, format, race tests, repository checks, and all 173 declared mutation
outcomes passed. The previously reported documentation defects were repaired.
`lip-da6` is a real pre-existing startup-history reconciliation defect, not a
reason to discard the completed canary latch; it becomes the next serial fix.

This does **not** authorise a live run. The current binary has no H-VER-1
two-key write guard, and “read-only” is therefore not a safe invocation mode.

## What changed at the top level

1. **Continuous runtime now has a definition.** CR-1 is one fixed, active
   `S=12` market repeating maker-entry and reduction cycles unattended during a
   declared assignment, under launchd/caffeinate and an external dead man. An
   S=1 canary that deliberately stops on its first directional entry is not
   continuous runtime. CR-2—24/7 quoting across programme turnover—still needs
   selection/rotation automation.
2. **The gates are separated.** Code qualification, 4–6-hour read-only
   qualification, attended S=1 canary, attended S=12 burn-in, and CR-1
   authorisation are distinct promotions with objective evidence.
3. **The mutation cut is withdrawn.** The existing 173-outcome catalogue stays.
   Targeted mutations are the inner loop; the complete catalogue gates every
   advancement. `lip-ke1` must make its probabilistic catcher deterministic.
4. **The scenario exchange remains small, but the release pack is eight composed
   faults.** 429 and storage/alert failure join the original six because they
   can strand exposure or create confident silence.
5. **Five records remain the pilot persistence cut, but alerts are not credited
   until composed.** `hstore` is production-wired; `ping` is not. The process
   must run ntfy delivery, hourly heartbeat, and external dead-man check-in, and
   the operator must prove that a missed check-in alarms.
6. **The one-worker cut is reversed.** Keep one dispatch authority, but restore
   H-QUE-3's reserved P1 transport slot. A cancel storm, 429 backoff, or hung
   request may not own the reducer's only route to the exchange.

## Current readiness

The plan records **about 70% to an attended canary** and **about 55% to CR-1**,
both with roughly ±10 points of uncertainty. That estimate is weighted by
remaining risk, not by bead counts.

Most core implementation exists: the runnable binary, exact REST layer,
websocket quarantine/reconciliation, exchange-authoritative portfolio model,
durable ownership store, lifecycle controller, latch retry, signal drain,
monitor, provisioner, and launchd renderer. The remaining work is smaller in
lines but heavier in release authority:

- H-VER-1 is absent; the production HTTP doer can send writes without a
  `--live` plus `live_ok` conjunction.
- `ping.Service` and dead-man transports have no `cmd/harness` production
  caller.
- launchd emits `-config` but drops `-rung`; an `S=12` KeepAlive job refuses and
  respawns forever.
- H-CAP-8, F17 `inv_kill`, H-HALT-5 `pnl_kill`, and F16 `stuck_s` have no final
  production consumer.
- 429 is treated as a definite create/cancel rejection; no adaptive rate exists.
- the runtime starts one HTTP dispatch worker even though H-QUE-3 requires a
  reserved reducer slot; the queue's internal capacity cannot create a worker
  the process never starts.
- `gate_fail_debounce_s` has no production reader, so H-Q-4a does not cancel
  unpaid adding orders after sustained gate failure.
- `lip-da6` replays old fills onto exchange-seeded position and records their
  provenance incorrectly.
- Only A5 is implemented as a named invariant tracker; the scenario pack,
  production A1–A14 composition, read-only soak, dead-man proof, and live
  demonstrations remain.

## `lip-2t6` adjudication

The implementation correctly makes first-fill stopping a compiled canary-rung
policy rather than a config knob. It covers acknowledgement fills, newly
classified owned fills, and a complete position transition from flat to
nonzero; history is separated by a pre-filter identity baseline; stronger
portfolio causes win; pilot and later rungs are excluded; the end-to-end seam
stays latched through flat and restart.

The bead's broad history wording exposed `lip-da6`: an old baseline trade is
suppressed as a canary cause but still reaches live reconciliation, can perturb
`q`, and can be recorded `backfilled=false`. The behaviour pre-dates `lip-2t6`.
The chosen fix for `lip-da6` is baseline-aware reconciliation: still classify
old IDs so taker/foreign checks run, apply owned baseline fills with seed
semantics so quantity never moves, and record owned history as backfilled. This
preserves H-ORD-8 and honest provenance.

## Tracker decisions

Close as delivered after recording commit evidence: `lip-eyq`, `lip-bw0`,
`lip-3af`, `lip-afr`, and `lip-8a8`. Close `lip-2t6` only after its commit.

Keep `lip-6w5` open but narrow it to the missing production alert/dead-man
composition. Keep `lip-jy4` open until `lip-ke1` is deterministic and every new
pre-live mutation is in the full green catalogue. Supersede the old full-tape
V2 epic `lip-7yo` with `lip-b1r`; supersede the empty V3 catch-all `lip-fdz`
with the actionable invariant work.

Create four missing launch-path beads:

- H-VER-1 structural read-only/live arming;
- launchd rung propagation for non-canary jobs;
- H-Q-4a sustained gate-failure cancellation;
- an F1–F21 plus §12 final-consumer wiring audit.

Create one CR-1 milestone and one deferred spec-patch bead. The spec-patch bead
records stale post-red-team contradictions—especially “never retry” versus
same-coid retry—and does not authorise editing the SHA-pinned spec in an
ordinary implementation unit.

## First courier sequence

1. Commit only the current `lip-2t6` implementation and generated negative-
   control report; exclude `.codex-handoff/`, the pilot-plan rewrite, and
   unrelated `.beads/interactions.jsonl` history from that commit.
2. Apply the ordered Beads restructuring in the driver response and close
   `lip-2t6` with the commit and green-gate evidence.
3. Fix `lip-ke1` first so future full gates are deterministic.
4. Fix `lip-da6` with baseline-aware seed semantics, then run its targeted
   mutations and the full gate.
5. In parallel, prepare isolated implementations for H-VER-1, launchd rung
   propagation, H-Q-4a, H-CAP-8, 429/reducer-capacity handling, alert wiring,
   scenario exchange, and invariants. Integrate owner/runtime seams serially.
6. Do not begin the 4–6-hour rehearsal until every §6.1 entry gate in the pilot
   plan is closed and the full exact-tree gate is green.

The revised plan deliberately makes “green code,” “safe read-only process,”
“attended live canary,” and “unattended continuous writer” four different
claims. They require four different bodies of evidence.
