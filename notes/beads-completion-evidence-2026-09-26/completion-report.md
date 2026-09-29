# Beads completion — 2026-09-26

Closed **39 Beads** during this session. Final tracker checks show no open, in-progress, deferred or ready issues; **11 blocked issues** retain concrete external prerequisites. This is a point-in-time completion record, not trading approval.

## Completed issues

| Bead | Completed scope |
|---|---|
| lip-048 | Implementer can end its turn waiting on a background task, and the loop scores it as success |
| lip-12d | FromWire is dead code while its doc claims it guards 7.5 adoption |
| lip-1m5 | go/cmd/conform: the read-only conformance sweep |
| lip-2da | F16: sustained inventory-stuck detection and alert |
| lip-2gn | F9/F11 unproven: no detector or consumer found for reject-rate REDUCING or post_only-cross cross-check |
| lip-2v0 | H-CLOSE-1: stop adding when the ticker leaves the active LIP programme set |
| lip-34l | Eight retained pilot release scenarios end to end |
| lip-357 | F5: H-FAIL-6's REST cross-check does not exist, and quarantine strands the reducer |
| lip-428 | SPEC PATCH: rename disconnect_halt_s -> disconnect_reduce_s (H-HALT-1 forbids the identifier) |
| lip-44s | Programs() refuses volume-type incentive programmes; one active volume programme stops the harness booting |
| lip-4nb | Three files cite harness-spec §3.8, a section the spec does not contain |
| lip-52l | [loop] parked units are re-picked after every restart -- skip set is in-memory only |
| lip-63b | SPEC PATCH: reconcile stale post-red-team clauses |
| lip-732 | H-Q-4a: sustained gate-failure cancel and sweep |
| lip-7k2 | quote: cancel-group class when one (market,side) group mixes adding and reducing cancels |
| lip-7vq | EPIC LOOP — the unattended two-agent orchestration harness itself |
| lip-7we | Ping test fixture durability wait is sensitive to CPU contention |
| lip-8qv | F15/H-ORD-9: live foreign orders do not reach the durable global stop |
| lip-9h1 | The scale rung permits an S that H-CAP-8 now refuses at its own described capital |
| lip-9r3 | check.py: mechanically enforce H-HALT-1 (no identifier named 'halt') |
| lip-b1r | Scenario exchange replacing V2 (pilot-plan §3) |
| lip-b7r | F7: SleepDetector has no runtime caller, so host-sleep detection cannot fire in production |
| lip-bca | Negative control attributes any suite flake to the mutation in flight |
| lip-cb0 | RED-TEAM FIRST (do not implement): depth-depletion requote hypothesis |
| lip-dgg | Production invariants A1-A14: compose remaining enforcement |
| lip-ghg | Route event-market cancellation by ticker across exchange shards |
| lip-ivz | Revival: offline baseline, current program evidence, and bounded client repairs |
| lip-jw1 | F21 partial: the general deadline recompute after a clock step is unproven |
| lip-ld9 | Side is duplicated between harness/quote and harness/risk |
| lip-o7a | §15: restore 60s balance-poll telemetry |
| lip-one | Shutdown composition and mutation ratchet |
| lip-q6r | F8/H-QUE-3: 429 recovery with reserved P1 dispatch |
| lip-qhi | requote_interval_s is a dead parameter and a spec inconsistency - record, do not implement |
| lip-r90 | F13 relay unproven: no composed test shows position drift reaching market REDUCING or a hard global stop |
| lip-t9u | F12 partial: the §7.2 UNKNOWN protocol has no composed final-consumer test |
| lip-xp3 | F10 unproven: no composed test shows insufficient_balance reaching a durable global WINDING_DOWN |
| lip-y3q | Add a negative-control mutation for the gateStopped-restricted reducer aggregate check |
| lip-ytr | F1-F3: disconnect and half-open composed recovery |
| lip-zpo | §12 inv_hard relay unproven: no composed test shows the market-scoped adding-off/reducer-live pair |

Implementation and acceptance details are retained in the Bead closure reasons and [closure-rationales.json](closure-rationales.json). The [eight-scenario map](scenario-map.md) and [A1–A14 map](../invariant-enforcement-2026-09-26.md) identify production seams and named tests.

## Remaining prerequisites

| Bead | Concrete blocker |
|---|---|
| lip-8hn | 2026-09-26: CR-1 continuous-operation acceptance is externally blocked on operator-reviewed q01, selected-shard/dedicated-account evidence, the attended S=1/$2 clean-exit canary, S=12 burn-in, supervision/assignment evidence and external alarm acknowledgement drill. Local actionable software work is being completed and verified in this session; no trading authority is inferred from code or Bead status. |
| lip-wif | Session blocker (2026-09-26): current authoritative exchange_index and selected-shard balance semantics must be established with read-only conformance on a nominated market/account before implementing a trusted fundability rule. No current market/account nomination or authorized private evidence is available; aggregate balance is explicitly insufficient. Wrong-shard funding must remain an R1 blocker, never waived by local tests. Need operator-provided scoped balance/market routing responses with timestamps, then composed refusal regression and implementation; no account access or writes performed. |
| lip-yca | Session blocker (2026-09-26): acceptance requires attributable realized LIP payouts and true private fills/fees/exit prices for a bounded eligible account plus chronological held-out comparison. Available public reward/program snapshots and historical frozen corpus cannot supply private payout attribution or observed net EV; prior revival already records updated terms. Need operator-provided timestamped private fills, reward credits, inventory/cash-transfer boundaries and executable exit observations. No speculative strategy, fabricated profitability, collector edits or account access. |
| lip-8hn.2 | 2026-09-26: Local repairs and scenario evidence use the updated faster workflow. External blockers are the R1/current-funding/read-only prerequisites, completed attended S=1 first-fill canary, real S=12 candidate/account/rung evidence, and lip-9vc external escalation drill. No S=12 or canary execution is authorized by local issue closure. |
| lip-8hn.1 | 2026-09-26: The current faster workflow governs local code evidence; the full catalogue is not required. External blockers remain lip-wif (current selected-shard spendable funds), lip-q01 (attended real read-only operational evidence), dedicated-account/current-market preflight, and explicit operator authorization for the one-market S=1, one-contract, $2 attended canary with verified flat/no-open-orders exit. No live operation was performed. |
| lip-9vc | 2026-09-26: Implemented tested startup and periodic 1 GiB operational headroom refusal/durable-stop checks, preserving reducer and observation paths; added notes/cr1-operations-runbook.md with normal start, drain, latch, corrupt store, growth, heartbeat and SEV1 escalation procedures. External blocker remains: no configured primary/backup acknowledgement drill receipts demonstrating missed dead-man check-in and unacknowledged SEV1 escalation. This session has no authorization to operate those live/provider paths. Local gate evidence is recorded in notes/beads-completion-evidence-2026-09-26; a passing local fake cannot satisfy that operator drill. |
| lip-9j3 | Session blocker (2026-09-26): attended S=12 burn-in depends on the agreed S=1 one-market/one-contract/$2 canary and R2 evidence. Operator authorization, account truth and completed clean exit are unavailable in this local session; do not skip the canary or infer authorization from local checks. |
| lip-dwf | Session blocker (2026-09-26): attended live execution is outside routine issue closure. Requires completed q01 and repaired R1 code/funding dependencies, then explicit operator authorization and attendance for one market, S=1, one contract, $2 cap, first-fill stop and verified zero open orders/positions at end. No writes or arming performed. |
| lip-q01 | Session blocker (2026-09-26): acceptance requires an operator-run 4–6h real-account process, forced restart/disconnect and externally observed dead-man alarm. No agent executes this issue by its own contract; this session authorizes repository issue repair, not account access or launch. Old August ticker/candidate are stale. Operator must nominate current ticker/config/binary, perform the window and preserve zero-write/clean-exit evidence. |
| lip-1in | Session blocker (2026-09-26): explicitly future CR-2, requires a new selection/rotation operating-envelope decision; issue forbids pulling multi-market management into current one-market CR-1 without that decision. Current user forbids speculative strategy expansion. Need approved managed-market/rotation semantics and account/market envelope; preserve dedicated-account refusal rather than silently expand trading scope. |
| lip-0ns | Session blocker (2026-09-26): issue requires measured min_ts/window completeness or a proved durable high-water contract before changing exhaustive fills walks. Existing account has a small bounded historical sample; no authorized authenticated conformance experiment or complete timestamp-boundary/page fixture establishes current behavior. Preserve complete client-side backfill and require operator-provided/current read-only contract evidence before optimization. |

## Changes and preservation

Against the starting manifest: 340 scoped paths, 293 unchanged, 47 changed, zero missing; 52 new source/document paths. All 20 starting files in `go/core`, `go/feed`, `go/store`, and `go/cmd/rig` retain their original hashes. [Full file inventory and hashes](change-inventory.md).

Runtime changes cover owner dispatch/capital/UNKNOWN accounting, reserved reducer transport and throttled reads, F5 recovery, sleep/clock handling, foreign orders/drift/reject/stuck/program relays, durable shutdown and concurrency, and balance/disk telemetry. Added composed fake-exchange regressions. Process/docs changes cover mutation attribution, the retired loop, the exact identifier checker, canonical spec wording, and the operations runbook. Existing work remains uncommitted; no reset, cleanup, commit, push, frozen checksum regeneration, collector access, arming or live orders occurred.

## Verification

- Default integration checks, full module tests and harness race tests passed in the [candidate receipt](../../loop/gates-out/20260926T215920445758Z-9c202850/receipt.json), plus all 16 safety sentinels. Total 415.1s; module tests 94.6s, race 151.4s, safety smoke 161.3s.
- The [44-control run](affected-controls.partial.md) verified 43 and retained one inconclusive M-F2 test hang (603.3s). Its unbounded receive and overly broad mutation were repaired; a fresh baseline and the [single repaired control](reconcile-control.partial.md) passed in 12.3s. All 44 affected controls now have expected outcomes. Together with safety smoke: 60 selected controls; no full catalogue.
- After the candidate, the spec, Python checker/tests, and one bounded Go test/mutation definition changed. Four checker regressions and the fresh repaired-control baseline passed; [final static gate](../../loop/gates-out/20260926T221948805112Z-f93096d1/receipt.json) passed on the final source. [Exact post-candidate diff](post-candidate-changes.patch). No production Go code changed; the successful integration/race/safety results were reused.
- Affected package and owner tests ran during edits. The 24-test mutation-runner suite passed on frozen inputs. `git diff --check` passed. Earlier gate and runner failure receipts remain in this directory. Causes and repairs, including the separately observed shutdown race, are summarized in [verification.json](verification.json); inconclusive runs were not treated as mutation kills.

## Parallel work and limits

Work continued in successive bounded subagent waves while the root integrated shared owner changes and dependencies. Later waves handled actual integration failures, shutdown concurrency, and the deferred checker/spec cleanup. The measured session interval from the starting manifest is 112.7 minutes; successful integration/smoke took 6.9 minutes and affected controls and the bounded repair check 10.3 minutes. No serial baseline or per-agent token accounting was available, so elapsed savings and token savings are not quantified.

No live trial was performed. The agreed next writer trial remains attended, one market, S=1, one contract, $2 cap, first-fill stop, and verified no open orders or positions at exit, after its funding/read-only/operator prerequisites.
