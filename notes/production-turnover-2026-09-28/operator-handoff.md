# Current attended-stage operator handoff

**20:28:43 UTC — stopped and independently flat:** Hugh’s SIGTERM succeeded; PID70087 is absent. A separate complete account-wide GET after exit (20:27:54 UTC) proves zero open orders and zero nonzero positions. Three named-order GETs show all stage orders canceled, zero fills and zero fees. OS exit code is unknown because `wait` ran in another shell and `STAGE` was unset; no further halt command is needed. No-fill remains inconclusive. Six late cancellation-verification SEV1s (`lip-kaf`) and a concrete floating-point book mismatch (`lip-2w3`) require review before another writer. Signal latch and all store/failed evidence are retained; do not restart. See the [updated stage report](continuation-review/live-stage-report.md). No continuous trading or independently scheduled watchdog is running. Older timestamped entries below are historical. The narrow source-only precision repair has focused regression and affected owner/cancel PASS receipts; e3f8a697 remains unchanged and does not include it. Hugh did not notice the actual incident alert; human receipt/acknowledgment is not established. Cancellation cause remains open.

**20:18:59 UTC — drain pending:** PID70087 remains live on the current first-stage config, with two open owned12-contract orders and no fills. Its45-minute target has passed. Do not provision or launch another stage. Use the [live-stage report](continuation-review/live-stage-report.md) to verify identity, perform operator SIGTERM/wait, then collect a NEW complete account-wide flat report after exit. The account-at-target receipt is not post-exit evidence. Preserve cancellation/reduction availability and live_ok until cleanup is verified. A no-fill observation cannot qualify R2 or first-fill-latch restart.

**19:31 UTC continuation:** the [explicit first-writer review](continuation-review/first-writer-review.md) now adjudicates the older R1 candidate difference and external q01 evidence for the exact current binary. Hugh confirmed attendance. Signed-in current Healthchecks state and route continuity were verified; the earlier missed-timeout demonstration is reused without a new q01 or timeout drill. Technical evidence is accepted for the attended static first-fill stage only. The latest exact prepared stage is recorded in [current-stage.txt](continuation-review/current-stage.txt), with its own `evidence/identity.json` and `candidate-decision.json`. These paths do not waive the 60-second input freshness requirement. Operator launch instructions have been provided; no financial outcome is established by preparation or this review. Use the live conversation and subsequent actual receipts to establish whether it launched.

Continuous active trading is **not running**. The shutdown-repair candidate is identified in [build identity](candidate-2/build-identity.json). Its code gate passed, including module/race tests and the sixteen safety sentinels; fourteen shutdown-specific mutations were also caught. The fresh GET-only q01 outcome is in [the continuation report](continuation-report.md) and [its exact assessment](q01-shutdown-repair-attempt2/assessment.json). A code or read-only pass does not close financial stages or activate a writer.

Candidate binary: `/Users/hugh/kek/lip/notes/production-turnover-2026-09-28/candidate-2/harness`, SHA256 `e3f8a697a20c3153f5c2a8650094b949ff57b985f94b154a7defdad22e9ef361`. Source manifest `6047b85a4306c70bd7048c8ed15e41e2ab87b8a21075a8a550747a8c13e1c709`, HEAD `f7e9571547bb948418b7a27adc9c30ae38314b00`, dirty tree preserved. Retain both September 27 failed runs and the September 28 ineligible-program attempt; none is promoted by the replacement run.

The former T70/T81 diagnostic scope has aged out of the selector's first-quarter eligibility window. Do not reuse its selection, old book, balances or unexecuted stage paths as current authority. The replacement GET-only scope observed `KXTOKENUSE-26OCT05-T168` and `KXBROSFT-26OCT08-T110`, selecting BROSFT. That assignment is diagnostic and refreshable, not a trade recommendation or permanent selection.

Hugh's financial authorization remains recorded. This assistant session cannot execute financial trades, cancel financial orders, or activate a trading writer; it must not route that work through another model/tool. The remaining financial observations require an attended operator. No financial receipts or human attestations were fabricated.

## Refresh before each operator stage

A concrete new [first-stage config](operator-stages/first-KXBROSFT-26OCT08-T110-20260928T180445.198999Z/config.json) and [identity](operator-stages/first-KXBROSFT-26OCT08-T110-20260928T180445.198999Z/evidence/identity.json) were prepared at `2026-09-28T18:04:45.320752+00:00` for BROSFT, `S=12`, using fresh complete account/program/market/fee/depth inputs. Its runtime is empty, unprovisioned and unarmed. [Printed operator commands](operator-preflight/20260928T180443Z.log) were not executed. The [funding calculation](operator-stages/first-KXBROSFT-26OCT08-T110-20260928T180445.198999Z/evidence/funding-arithmetic.json) used $98.5802 cash, $24.645050 reserve and $73.935150 deployable cash at that observation; refresh again before arming.

Run the existing read-only preparation with the reviewed binary and fresh public book. It refreshes active programs, market/event/series fees, executable depth, account-wide orders/positions/fills and selected-shard funds, then computes the one-market balance-derived envelope for `S <= 12`. The 60-second freshness check is at preparation completion. It does not automatically recheck freshness at later arming; repeat preparation after delay or changed conditions and before every subsequent stage.

```sh
cd /Users/hugh/kek/lip
PY=/Users/hugh/kek/.venv/bin/python
EVIDENCE=/Users/hugh/kek/lip/notes/production-turnover-2026-09-28
BIN="$EVIDENCE/candidate-2/harness"
CANDIDATE_RECEIPT="$EVIDENCE/candidate-2/build-identity.json"
TICKER=KXBROSFT-26OCT08-T110
SIZE=12
PREFLIGHT="$EVIDENCE/operator-preflight"
mkdir -p "$PREFLIGHT"
BOOK="$PREFLIGHT/${TICKER}-$(date -u +%Y%m%dT%H%M%SZ)-market-book-series.json"
"$PY" scripts/public_market_snapshot.py --ticker "$TICKER" --out "$BOOK"
"$PY" scripts/operator_stage.py --stage first --ticker "$TICKER" --size "$SIZE" \
  --binary "$BIN" --candidate-receipt "$CANDIDATE_RECEIPT" --book "$BOOK" \
  --evidence-root "$EVIDENCE/operator-stages"
```

The helper only prepares evidence/configuration and prints operator-only commands. Read its actual new output; do not copy historical runtime paths. Inspect the market's current rules, close/early-close condition, fees, spread/depth and eligibility, account identity, spendable funds/reserve and exact candidate identity. [Operations scope](operations-update.md) is primary-only ntfy by the user's instruction; no backup is configured. Human receipt of the three prior message IDs is confirmed without inferred timing. Real-exposure unusable-store recovery is still unproved.

## First fill, reduction and retained-latch restart

For `lip-dwf`, provision only the new stage's empty store and have the operator attend actual post-only placement, first owned directional fill, durable first-fill stop, adding-order cancellations, actual order/fill identifiers and fees, funded bounded reduction, and full account-wide flat exit. The first window is bounded at 45 minutes from launch; if no owned fill occurs, the outcome is inconclusive. Request drain and remain through cleanup. Neither a simulated fill, no-fill observation, signal-created latch nor settlement-only disappearance qualifies this path.

Verify a signal target using exact executable/config/store arguments and process start time. SIGTERM requests drain; it does not prove flatness. Unknown ownership, partial truth, unresolved exposure, failed cancellation/reduction, missing depth or new adds after the first-fill latch block progression. Never cancel foreign orders or invent an executable exit price. The responsible account operator must resolve foreign obligations while the account-wide exit requirement remains in force.

Preserve logs, exit status, DB/WAL/SHM, anomaly journal, halt latch, provider delivery and actual human response evidence. After the process exits, collect a new complete account-wide accountcheck report. Require `account_scope_complete=true`, `flat=true`, complete component statuses, zero open orders and zero nonzero positions across all enumerated subaccounts.

Before retained-latch restart, verify the original process is absent, preserve/inspect the actual first-fill latch, and refresh full account-wide truth, program/market/fees/depth/funding. `-resume 'first-owned-fill stop validation'` records the reason and starts WINDING_DOWN; it does not clear the latch or authorize adds. Observe adoption/reconstruction and reduction with no new adding. If it idles DRAINED, signal only the reverified PID and await exit. Obtain a **new** complete account-wide flat report after this restart exits. The first post-drain read cannot substitute for that post-restart read.

The after-drain/before-restart read may serve both sides of that one boundary only while fresh. `accountcheck -ticker` chooses market/funding diagnostics; order and position scope remains account-wide. Remove only this stage's `live_ok`, only after all writers/reducers have exited and final flatness is proven. Retain the latch. Do not bootstrap an unattended trading job from this handoff.

## Repeated cycles, R2 and live turnover

`lip-9j3` and `lip-8hn.2` require actual evidence bound to the same first-stage identity for all seven fields: orders, fills, fees, reduction, clean exit, restart and alarm. Each needs a named human witness, valid observation time and actual hashed files. Use the [existing receipt-schema reference](../active-continuation-2026-09-26/operator-handoff.md) for format only. The helper checks structural provenance and hashes, not the truth of attestations. Record real attributable fill `fee_cost`/`is_taker` values; hypothetical exit arithmetic is not a fee receipt.

After real first-stage completion, refresh all inputs again. Only then call `operator_stage.py --stage r2` with the actual `--prior-config` and completed `--r2-receipt`; it preserves the first runtime/ownership paths and selects the pilot rung. A human must separately adjudicate any latch against complete truth before new adds. Attend repeated entry/fill/reduction/flat cycles, restart and alarms, ending with complete account-wide flatness after the last process exits. No R2 receipt is prepared from q01 or fixture results.

The one-market stages do not prove live A→B turnover. Later continuous promotion needs the exact `turnover: true` live config with an explicitly reviewed candidate list and selected-shard funding, real A→B cycles with old-market exposure, retained obligations and per-shard limits through selection/program changes, restart, and complete account-wide exit evidence. No newly encountered shard or unresolved reservation silently expands capital or disappears. The read-only two-market selection event is not that financial turnover proof. Profitability and attributable reward performance remain unproved.
