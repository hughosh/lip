# First-fill attempt: stopped and independently flat; no-fill inconclusive

**Final containment evidence, September 28, 20:28:43 UTC:** Hugh's first SIGTERM worked. PID70087 was absent at20:24:30. A separate complete account-wide read at20:27:52–20:27:54 reports zero open orders and zero nonzero positions (`account_scope_complete=true`, `flat=true`). Three named-order GETs at20:28:43 show all three actual stage orders canceled, zero filled and zero remaining, with zero maker/taker fee counters. See this stage's `evidence/post-exit-summary.json`, `process-after-operator-drain.json`, `account-after-drain.json` and `order-details-after-drain.json`.

No further halt/cancel command is required for this stopped stage. The later `kill` found no process. The operator's `wait` ran in a different shell, and `STAGE` was unset there, so the OS exit code remains **unknown**; neither the shell's wait failure nor its failed redirection is a harness exit code. Interactive zsh also tried to execute pasted `#` comment lines. Future command blocks must be self-contained and omit those comments. No restart is authorized by this report.

At20:23:19.139 the store recorded RUNNING→WINDING_DOWN→DRAINED. The retained halt latch is `trigger=sigterm`, timestamp1790626999110, not a first-fill latch. The SIGNAL_DRAIN receipt reports50m14.216573221s uptime and no inventory/live orders at signal time. This exceeds the45-minute target by about5m14s. The earlier prelaunch freshness deviation also remains a failed condition. The store's read-only integrity check was `ok`; DB/WAL/SHM/journal/latch remain preserved. `live_ok` remains present in this stopped runtime; do not mistake that file for an active process or qualification.

## New late evidence and restart blockers

The bounded observer's final20:18:07 sample was accurate at that time. Later activity is additional evidence:

- The initial YES order was canceled with exchange update time20:19:22.315385Z. A replacement YES order `01a0e9ac-14e0-7144-9953-d1f111fe220d`, client ID `lipH-20260928T1933054SN2H-000-yes-00000003`, was created20:19:24.157785Z for12 at26c. It was canceled with update time20:21:24.717386Z. The original NO order's canceled update time is20:21:25.125244Z.
- Six real SEV1 `SWEEP_INCOMPLETE` records (a89,a91,a93,a97,a99,a101) and associated `CANCEL_UNVERIFIED` records occurred during reprice and market stop. The first SEV1 has a delivery timestamp; later same-class records do not establish separate human receipt. No human acknowledgment/time is inferred. `lip-kaf` tracks adjudication.
- At20:21:24.468, `BOOK_CROSSCHECK_MISMATCH` compared YES23c size456.00000000000006 on websocket with456 on REST, producing a market stop. This is concrete floating-point comparison evidence, tracked separately as `lip-2w3`.

Independent bounded source review found no all-orders-versus-side cancellation scope bug: only requested IDs still visible in a complete verifying read trigger the sweep alarm; the opposite-side order is tracked separately. Later terminal timestamps make transient listing lag plausible, but the run did not capture each DELETE response and verifying list body. The cause is therefore unresolved; eventual flatness does not erase those failures or justify lowering the assertion. Preserve cancellation/reduction when intermediate account truth is unresolved. Capture exact per-sweep evidence before claiming that this recurrence is understood.

The first-fill stage remains **inconclusive**: zero owned fills, no actual first-fill stop/reduction or retained-first-fill-latch restart, no R2 and no live CR-2 turnover. No positive-EV claim is supported. Current binary/config and original q01 remain separately bound; any repair is a new source candidate requiring only the checks justified by its actual difference before an explicit future writer decision. `lip-3dw` and `lip-1in` stay closed.

The narrow `lip-2w3` source repair now accepts the observed single adjacent floating-point size difference, capped below0.005 contract, while rejecting a real0.01-contract difference and invalid sizes. Exact naive Target Size accumulation is retained. The new regression failed before repair and passed afterward; the focused CompareBooks suite, F5 owner integration and three existing cancel-sweep cases passed. [Repair receipts and limits](precision-repair/README.md) include the failed regression. Only `go/harness/rest/orderbook.go` and its new precision regression test changed in the candidate source. This is code evidence, not a replacement binary, gate, q01 or financial qualification. The historical e3f8a697 binary is unchanged and does not contain the repair. Multi-step drift remains conservative disagreement. The cancellation cause remains open.

## Earlier observations retained

At **20:18:59 UTC**, PID **70087** was still running the live first-stage writer, past its **20:18:03 UTC** 45-minute drain target. No owned fill, latch, operator drain, process exit or post-exit flatness was observed. Do not start another stage while these obligations remain.

The independent account-wide read ended **20:18:58.510266Z**: complete scope, two open orders with 12 contracts remaining each, zero nonzero positions, 15 historical fills and **flat=false**. This is a during-run receipt, not post-exit proof.

## Exact candidate and actual orders

Stage: `/Users/hugh/kek/lip/notes/production-turnover-2026-09-28/continuation-review/operator-stages/first-KXBROSFT-26OCT08-T110-20260928T193111.232608Z`.

- Binary: `e3f8a697a20c3153f5c2a8650094b949ff57b985f94b154a7defdad22e9ef361`.
- Source manifest: `6047b85a4306c70bd7048c8ed15e41e2ab87b8a21075a8a550747a8c13e1c709`.
- Live config: `d2e621fd810dbe9c6a885593a62a480e800fd35d55228bf003138458bb5d562b`: static one-market sizing, S12, selected-shard balance, existing risk limits, no CR-2 turnover flag.
- Run: `20260928T1933054SN2H`. Hugh reported provisioning, arming and launching; exact process/config/start identity was independently verified. Start was 19:33:03 UTC, parent operator terminal PID70075. Attendance was confirmed at the beginning; no later witness attestation is fabricated.
- YES order `01a0e981-b150-714a-b910-47ca43522e5b`, client ID `lipH-20260928T1933054SN2H-000-yes-00000001`: 12 at23 cents, created19:33:06.843172Z.
- NO order `01a0e981-b150-7a9c-81d5-c6fcc7baa657`, client ID `lipH-20260928T1933054SN2H-000-no-00000002`: 12 at39 cents, created19:33:06.942929Z. Exchange canonical representation is a YES ask at61 cents, outcome_side=no, consistent with the existing wire conversion.

`evidence/order-details-during.json` contains two actual [Get Order](https://docs.kalshi.com/api-reference/orders/get-order) receipts at19:54:50UTC: both resting, zero filled quantity and zero maker/taker fee counters. These responses omit post_only. Structural post-only evidence remains the exact candidate's wire construction and passed safety sentinels; actual maker-fill/fee evidence is absent. The GET helper is outside the candidate Go source tree and contains no trading method.

`evidence/read-only-observer.jsonl` records bounded SELECT/process observations every10 seconds. The final20:18:07UTC sample has two ownership records, zero owned fills, no latch, no SEV1,541 balance polls and RUNNING/QUOTING. Quiet-book events were followed by matching REST cross-checks. The support observer exited normally at the target and never signaled the trader. These observations prove neither first-fill cancellation/reduction nor retained-first-fill restart/repeated cycles.

## Reviewed decision and preserved deviations

[First-writer review](first-writer-review.md) explicitly adjudicates the older R1 difference and current external q01 evidence. Actual stage identity/decision are in `evidence/identity.json`, `candidate-decision.json` and `actual-candidate-launch.json`. The q01 receipt remains bound to config `47b3b73244755998afab6d6d27376a15b96c6eb7d99fd680eafcef776ae67135`; neither it nor old R1 is relabeled as this financial run.

Preparation completed at19:31:17UTC, with a19:32:09UTC advertised freshness deadline. Actual launch at19:33:03UTC exceeded it. Preserve `launch-freshness-deviation.json`: later reads do not retroactively repair the prelaunch timing. Runtime funding was independently refreshed at19:33:04.974252Z: $98.5802 cash,25% reserve, $73.93515 deployable cap. Public rules, active status, fees and two-sided depth were refreshed at19:36:30UTC with no relevant change observed.

The writer remained active beyond its20:18:03UTC drain target; preserve that separate overrun. Hugh subsequently sent the signal described above. The assistant did not send it because it triggers financial cancellation. No restricted action was delegated or routed through another tool.

## Alarm and process state

At19:29:21UTC, the signed-in Healthchecks dashboard showed the matching monitor DOWN, period1hour, grace20minutes, email ON, last ping an hour ago. The private configured endpoint matched. This preceded the live start and was consistent with the stopped GET-only observer. A later during-run browser refresh failed because the debugger was unavailable; no later provider state is asserted. No manual success ping was sent.

Earlier real missed-timeout evidence was reused for this attended decision. No fresh timeout or human response timestamp was invented. Receipt of the three earlier ntfy messages remains confirmed without timing. For this actual20:19–20:21 cancellation incident, Hugh explicitly answered **“Did not notice an alert.”** See `actual-sev1-human-response.json`. The stored transport delivery timestamp proves neither human receipt nor acknowledgment; the answer alone also does not prove a provider delivery failure. Primary-only ntfy is intentional; no backup/fallback or permanent independently scheduled watchdog was installed. The expired support observer is not unattended monitoring. The real incident therefore does not qualify the human response path for unattended operation.

The live sentinel remains present in the stopped runtime; the signal latch is now retained. The final20:30:47 scoped process snapshot finds no executable matching lip/harness/collect and no com.lip/ops_watchdog launchd labels. This limited executable-name scan does not inventory every Python collector; no collector was modified or signaled. No trader signal, trade or cancellation was executed by the assistant. Frozen artifacts, earlier failures and original receipts remain preserved. No commit or push was made.

## Required next action and limits

The operator drain and post-exit flatness check are complete. Do not signal PID70087 again or restart this runtime to retrieve a missing exit status. If status remains available in the original launch shell it can be saved using an absolute evidence path; otherwise retain `unknown`. Retain store/WAL/SHM/journal/latch and receipts. Review the two new incident findings before another refreshed attended stage; the successful q01 is not replayed merely because this attempt had no fill.

No-fill is **inconclusive**. A signal-created latch cannot qualify first-fill restart, and no R2 receipt may be manufactured. First-fill, repeated cycles/R2 and real CR-2 turnover remain blocked. Real-exposure unusable-store recovery remains unproved for lip-9vc: preserve ownership evidence, get independent complete truth, require operator-owned-only cleanup/reconciliation and later flatness before restarting. No destructive live fault was created. Profitability remains unproved for lip-yca.

Checks were receipt/hash review, fresh public/account GETs, bounded stage SELECT/process observations, two initial and three final named-order GETs, refresh-helper syntax inspection, the narrow precision regression and affected owner/cancel tests, and prose whitespace verification. No full gate, q01 or broad audit was repeated. The only new client change is the documented precision repair. Initial sandbox account reads failed and remain saved; the network-enabled success is separate. A local SQLite export initially failed to serialize a JSON BLOB; its subsequent JSON-aware SELECT export and diagnostic were retained. It was not a runtime/store failure.
