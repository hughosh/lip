# Production turnover continuation — September 27, 2026

**Current continuation (18:59 UTC):** [read-only qualification and operations report](continuation-readonly-report.md) is the latest result. Both new exact-candidate attempts failed; a partial shutdown repair is preserved but the close boundary remains unresolved (`lip-3dw`). No replacement candidate, trader or permanent watchdog is running. The latest account-wide check is complete and flat at 18:58:30 UTC. ntfy accepted the external drill messages, with no human acknowledgment observed.

**Later qualification finding (18:48 UTC):** the unchanged `ecf16ea…` candidate completed a new 20m30.626s GET-only event run, including real selection, forced disconnect/recovery and automatic supervised restart. It passed duration and cadence checks but **failed finalized q01** on shutdown-time `STORE_UNHEALTHY` and `ANOMALY_SUBMISSION_FAILED` SEV1 events. [Assessment](q01-cr2-attempt2-2026-09-27/assessment.json), [summary](q01-cr2-attempt2-2026-09-27/summary.json), and the earlier [overlapping-read failed attempt](q01-cr2-2026-09-27/failure-review.json) are preserved separately. New P1 `lip-3dw` tracks the contained shutdown repair and required new-candidate qualification; `lip-1in` is not reopened. Fresh account-wide truth at 18:48:40 UTC is complete and flat, with zero open orders or nonzero positions. The observer and both temporary launchd jobs are stopped; Healthchecks accepted the stopped-source failure signal. The historical code and smoke receipts below keep their original identities.

Production turnover is integrated and code-qualified. Continuous active trading is **not running**. `lip-1in` is complete as a code task, supported by composed production-path scenarios and the selected-only mutation. Live financial turnover, the first-owned-fill stage, attended repeated cycles/R2 and external operations evidence remain unobserved. This session performed no financial writes or writer activation. Prior account authorization remains in force; the session restriction, rather than missing authorization, prevents those actions.

## Exact candidate and scope

[Build identity](runtime-evidence-2026-09-27/candidate/build-identity.json) binds the executable, source archive and passing receipts. HEAD is `f7e9571547bb948418b7a27adc9c30ae38314b00`, with the starting dirty tree preserved. No commit or push was made.

| Artifact | SHA256 |
| --- | --- |
| Go/module source manifest | `74f8ee7634c406f0e4279429b4e8bbdca9cab31bc2f3a1564163b1d42c4263c2` |
| Go source archive | `e8b4361e6efcbf6580c50a1bc433e7cc64b572c5c2688f7a837f771a25215016` |
| Candidate binary | `ecf16ea19c4f0e05dad8cce0c261d6e6f2204eeb8f9615913a79b54816b311c6` |
| Two-market GET-only config | `0c01ba575df66988f4d694d39947c2effe497499159ca97e7a3b764cc90c0791` |
| Prepared one-market first-stage config | `71edf38a626554a98361b310360c38d48152ecfa4c4ef242bc5284b8b59e46fa` |

The [source delta](runtime-evidence-2026-09-27/candidate/source-delta.json) from the preceding candidate is 14 changed and 13 added Go files, no removals and no frozen core/feed/store/rig changes. Existing collectors, credentials, stores, archived q01/R1 artifacts and receipts were preserved. Only new isolated observer and fixture stores were created. The source manifest captured during the successful gate and the later build has the same hash. Repository gate and mutation-runner fingerprints cover different input sets and are not interchangeable. Two additional named mutation definitions were added after the full gate; the final named run validates those definitions against unchanged Go source. Documentation and receipt additions subsequently change the broader repository fingerprint.

## Production path completed

Explicit `turnover: true` plus 1–32 approved candidates enables CR2; the seed must be included and funding must use selected-shard balances. Legacy one-market behavior remains available. Selection reads complete active programs, eligible books and market schedules, ranks candidates and applies rotation hysteresis. A vacancy can select a replacement immediately. Selected authority expires with stale accounting or selection evidence.

The owner retains a context per managed market: state, schedule/close, program membership, book recovery, order role and reduction/cancel state. Account positions, owned listed orders, pending/UNKNOWN reservations and inherited durable obligations extend the managed set beyond selection. Approved and previously managed contexts remain observed conservatively. The dynamic gate/supervisor universe has generation-tagged events and rejects retired snapshots, deltas and disconnects; adding a market refreshes book authority before decisions. Schedule reads and crosscheck queues cover every managed market.

Funding maps each market to its authoritative exchange shard, deduplicates cash by shard and freezes admitted caps. Both owner planning and final dispatch call the turnover accounting path. Pending and in-flight costs are scoped to their actual shard, while the aggregate ceiling is checked once. Reducers use fresh authoritative signed quantity and their own shard's fundable amount. A newly encountered unadmitted shard fails closed; it does not expand capital. Funding age starts when the complete scan starts, not when a slow scan finishes.

All managed positions are marked for aggregate risk/P&L; immutable account summaries reach the monitor. A stalled owner invalidates freshness and P&L evaluability. Durable hstore reservation/binding is the production authority, rather than a separate isolated coordinator ledger. Complete omission does not release a possibly-live obligation. Exact terminal evidence waits for in-flight create results and committed bindings, preventing late acknowledgment from resurrecting released exposure. Foreign or merely prefix-shaped orders are never adopted for cancellation.

Startup reconstructs inherited orders from the durable ownership ledger plus complete all-status exchange truth, including old tickers absent from the new candidate list. Exact positive terminal evidence can resolve an obligation; omission retains its reserved maximum. An unknown unbound omitted reservation blocks safe startup pending positive resolution or attended recovery. Latches remain in force across restart.

## Composed evidence and checks

The following scenarios run actual production owner/REST/websocket/dispatch/store/monitor/drain code against a disposable fake exchange. Their fills and fees are simulated, not Kalshi account transactions.

- `TestComposedTurnoverKeepsHeldOldMarketWhenSelectionMoves`: A→B selection retains A's subscription, schedule, mark and reducer; foreign orders remain untouched.
- `TestComposedTurnoverDispatchesNewMarketWhileReducingOwnedOldFill`: production dispatch creates both A adding sides; an attributable fake A fill changes inventory; B then dispatches both adding sides while A reduces, with old A adds canceled and aggregate monitor/risk evidence. A later foreign B order stops adding and remains uncanceled.
- `TestComposedTurnoverRestartReconstructsOldMarketReducesAndExits`: retained latch, refused unresumed start, B-only restart reconstructing A and a down-time fill, authoritative bounded reduction, positive terminal evidence, fresh flat truth and signal-authorized exit. Fresh account-wide fixture reads follow exit.
- `TestComposedTurnoverRestartRetainsOmittedBoundOldOrder`: a durable bound A obligation omitted from both complete order reads is reconstructed after B-only restart and goes through owned-only cancellation.

The [candidate gate](../../loop/gates-out/20260927T170933719968Z-8521aa8d/receipt.json) passed in 411.248 seconds: integrity/tool checks, build, vet, format, full Go module suite, race checks and all 16 standard safety sentinels. The [additional named run](../../loop/gates-out/negative-control-20260927T171702926992Z/receipt.json) passed its pristine baseline and caught all 13 selected controls in 136.783 seconds:

`M-3AF-WEDGE`, `M-603-STARTONLY`, `M-CLOSE-PROGRAM-MEMBERSHIP`, `M-CR2-RUNTIME-SELECTED-ONLY`, `M-CR2-RUNTIME-SHARD-SIZE`, `M-CR2-RUNTIME-PENDING-SCOPE`, `M-CR2-RUNTIME-OMISSION`, `M-CR2-RUNTIME-LATE-ACK`, `M-CR2-RUNTIME-FUNDING-AGE`, `M-CR2-RUNTIME-RESTART-OMISSION`, `M-CR2-MONITOR-STALE-PNL`, `M-CR2-BOOK-OLD-UNIVERSE`, `M-CR2-SWEEP-FOREIGN-PREFIX`.

The selected-only and restart-omission mutations are caught by composed owner scenarios. This is a selected subset of the 353-control catalogue; no full catalogue replay or archived q01 rerun was needed. Gate `live_eligible` and `advance_eligible` remain false. [Ops tests](ops-final-tests.log) passed all 26 current watchdog/source/recovery tests. Earlier affected and race logs remain alongside the final receipts.

Failures are retained and explained in [failure review](preserved-failures.md), including the real vacancy-selection bug, fixture defects, moved mutation anchors, a candidate run invalidated by concurrent evidence-file fingerprint changes, and the GET-only assessment's unmet q01 criteria. The later pass supplements those receipts rather than replacing them.

## Actual GET-only observation

The [process receipt](runtime-evidence-2026-09-27/read-only-smoke/process-receipt.json) records 91.668798 seconds wall time, 89.453062 seconds linked active time and planned SIGTERM exit 0. No `-live` or arming sentinel was present. The [observation review](runtime-evidence-2026-09-27/read-only-smoke/summary.json) records 300 exchange GETs, zero non-GETs, zero owned-order rows and zero owned-fill rows. The seed was T81; the owner selected `KXOPENSOURCESHARE-26SEP29-T70` and produced 16,743 guarded would-write observations. These are repeated observations of two quote fingerprints, not orders or execution throughput.

All 19 portfolio walks were fresh and complete. The monitor recorded 89 checks, 88 fresh and one early nonfresh slot. There was no OWNER_STALLED or SEV1 anomaly. An initial missing sample is consistent with the callback's freshness rule, but per-slot timestamps do not establish that cause. Two quiet-book events triggered successful actual REST crosschecks. At SIGTERM a stale place intent had a zero remainder and emitted WRITE_NOT_BUILDABLE; it created no order. Fifteen inherited foreign fill events refer to historical account rows.

The saved [assessment](runtime-evidence-2026-09-27/read-only-smoke/assessment.json) exits 1: below 20 minutes, 88/90 expected fresh monitor slots below 99%, no forced disconnect cycle, and no linked restart. This short observation is not q01, production financial turnover or R2. Archived v3 q01 and R1 remain valid only for their exact original candidate.

## Operations and remaining financial evidence

[Operations handoff](ops-operator-handoff.md) has the external primary/backup acknowledgment, deliberate nonacknowledgment and unusable-store recovery procedure. Loopback HTTP tests exercised primary 202 with synthetic acknowledgment, deadline escalation after durable reload, and primary 503 followed by backup delivery. The corrupt-store fixture validates retained exact ownership against independent-snapshot assertions and excludes foreign orders. Those assertions do not authenticate an exchange view; the tool cannot restore a ledger, execute cleanup or attest a person.

`lip-9vc` remains blocked on real route configuration locations, named primary/backup operators, the actual external acknowledgment/nonacknowledgment drill, installed independent scheduling, and real exposure/store-loss recovery. The existing escalation interval is 300 seconds. No secret routes or people were invented. Healthchecks provider acceptance is not human acknowledgment or an installed watchdog.

The [current operator handoff](operator-handoff.md) provides new candidate-bound commands and an unprovisioned, unarmed first-stage config. Fresh preparation used new account, program, fee and depth reads rather than old cash or a historical ticker. Funding observed $98.5802, with $73.935150 deployable after reserve; S=12 passed the current admission arithmetic. The official [fee schedule](https://kalshi.com/docs/kalshi-fee-schedule.pdf) and fresh quadratic multiplier 1 metadata support the hypothetical exit calculation. The optional raw PDF download returned HTTP429 and was not retried; the successful web review and failed attempt are retained in the fee receipt. No actual exit fee, reward or profitability is inferred.

`lip-dwf`, `lip-9j3` and `lip-8hn.2` remain blocked on real first-owned-fill, cancellation, fee, bounded reduction, retained-latch restart, complete clean exits, attended repeated cycles and R2. Each attended stage must end with new complete account-wide no-orders/no-positions evidence. One-market first/R2 evidence will not by itself prove live A→B turnover.

At **17:29:41.082742 UTC on September 27**, [fresh final account truth](runtime-evidence-2026-09-27/account-final.json) was complete across enumerated subaccount 0: **zero open orders and zero nonzero positions**. Two returned position rows were flat historical rows. The [fill pressure measurement](runtime-evidence-2026-09-27/fill-growth-final.json) remains 15 identical fills, one page, 7,996 bytes, 103.778434 ms (2.0756% of a 5-second poll). `lip-0ns` stays open at P4 with no measured pressure and no completeness optimization. `lip-yca` remains blocked: no new attributable fills, fees or rewards; shared pools and historical credit establish no positive EV. Momentum expansion remains deferred.

[Process state](runtime-evidence-2026-09-27/process-final.json) at 17:27:48 UTC found no harness, caffeinate, acknowledgment watchdog, `com.lip.*` current-user launch job or scoped live sentinel. The GET-only observer is absent and its latch/store remain preserved. Healthchecks accepted [the stopped-source failure signal](runtime-evidence-2026-09-27/heartbeat-stopped-final.json) at 17:27:48 UTC with HTTP200 and exact OK. No dashboard state or human acknowledgment was verified. There is no permanent trader or watchdog installed by this work.

`lip-1in` is closed for its composed implementation acceptance. Financial and operations Beads remain blocked as described above; their current machine-readable state is retained with the final evidence. See [verification index](runtime-evidence-2026-09-27/verification-index.json) and [Beads state](runtime-evidence-2026-09-27/beads-final.json). Code readiness does not establish continuous active participation.
