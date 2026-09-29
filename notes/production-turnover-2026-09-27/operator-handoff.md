# Current attended-stage operator handoff

**September 28 superseding handoff:** The repaired exact candidate passed its new real GET-only local q01 and has a fresh unexecuted operator-stage preparation. Use the [current operator handoff](../production-turnover-2026-09-28/operator-handoff.md) and [continuation report](../production-turnover-2026-09-28/continuation-report.md). The dated details and old failed-candidate commands below are retained history. Continuous trading remains stopped; no financial Bead is closed by q01.

Continuous active trading is **not running**. The production turnover code is integrated and code-qualified against composed fake exchange scenarios. This session cannot place/cancel financial orders or activate a trading writer. Hugh's prior authorization remains recorded; repeat authorization is not the missing prerequisite. Actual financial stages must be performed by an attended operator. No operator attestations were created here.

**Current qualification blocker:** the exact binary below failed a finalized 20m30.626s read-only event test on shutdown-time store-health/submission SEV1 events. The duration, feed recovery, supervision and cadence evidence does not override that failure. See [the retained assessment](q01-cr2-attempt2-2026-09-27/assessment.json) and new `lip-3dw`; a repaired runtime requires its own identity and qualification. The preparations below remain unexecuted historical examples until a matching current candidate is established. Fresh account-wide closeout truth at 18:58:30 UTC is complete and flat. A narrow working-tree shutdown fix has focused race evidence but remains incomplete and unbuilt; do not treat it as a replacement qualified candidate. See [the continuation report](continuation-readonly-report.md).

The last observed candidate is bound by [build identity](runtime-evidence-2026-09-27/candidate/build-identity.json):

- HEAD: `f7e9571547bb948418b7a27adc9c30ae38314b00`, dirty tree preserved.
- Go/module source manifest SHA256: `74f8ee7634c406f0e4279429b4e8bbdca9cab31bc2f3a1564163b1d42c4263c2`.
- Binary SHA256: `ecf16ea19c4f0e05dad8cce0c261d6e6f2204eeb8f9615913a79b54816b311c6`.
- Prepared first-stage config SHA256: `71edf38a626554a98361b310360c38d48152ecfa4c4ef242bc5284b8b59e46fa`.

[Concrete first stage](runtime-evidence-2026-09-27/operator-stages/first-KXOPENSOURCESHARE-26SEP29-T70-20260927T172810.219932Z/config.json) is `KXOPENSOURCESHARE-26SEP29-T70`, `S=12`, `rung=sizing`, one market. Its runtime is unprovisioned and unarmed. The helper printed exact provision, arming, launch, drain, retained-latch restart and post-exit read commands into the [preparation log](runtime-evidence-2026-09-27/operator-preflight/20260927T172808838681Z-1.log); none was executed. These are attended steps with observation boundaries, not one unattended script.

The [stage evidence](runtime-evidence-2026-09-27/operator-stages/first-KXOPENSOURCESHARE-26SEP29-T70-20260927T172810.219932Z/evidence/identity.json) binds fresh program, market, event, series fee, executable book, account and funding observations. Account preflight ended 17:28:18.214840 UTC on September 27, complete and flat. Selected-shard spendable funds were $98.5802, reserve $24.645050, deployable $73.935150; $12 admission bound for S=12 passed. These are timestamped observations, not reusable cash facts. The 17:28:09 book had 745 YES and 645 NO at $0.02, and deeper $0.01 bids. The captured program was active through September 29 14:00 UTC, subject to market early close. Series fees were quadratic, multiplier 1. Hypothetical 12-contract exits at the unchanged $0.02 bids yield $0.24 gross, $0.0165 estimated taker fee and $0.2235 net proceeds per side. This excludes entry costs and rewards and establishes no expected profit or actual execution.

Refresh immediately before the attended window. The helper's maximum observation age is 60 seconds. Use newly printed paths after each successful preparation; preserve this preparation. The ticker below is a refreshable diagnostic assignment, not a recommendation or permanent selection.

The 60-second check applies when preparation finishes; the printed commands do not enforce a second freshness check at launch. The attending operator must refresh account, program, fees, executable depth and balance-derived limits immediately before arming/launch and before each subsequent stage. Re-run preparation after a delay or changed conditions. Do not reuse the numerical balance or depth above as current facts.

```sh
cd /Users/hugh/kek/lip
PY=/Users/hugh/kek/.venv/bin/python
TICKER=KXOPENSOURCESHARE-26SEP29-T70
SIZE=12
EVIDENCE=/Users/hugh/kek/lip/notes/production-turnover-2026-09-27/runtime-evidence-2026-09-27
BIN="$EVIDENCE/candidate/harness"
CANDIDATE_RECEIPT="$EVIDENCE/candidate/build-identity.json"
PREFLIGHT="$EVIDENCE/operator-preflight"
mkdir -p "$PREFLIGHT"
BOOK="$PREFLIGHT/${TICKER}-$(date -u +%Y%m%dT%H%M%SZ)-market-book-series.json"
"$PY" scripts/public_market_snapshot.py --ticker "$TICKER" --out "$BOOK"
"$PY" scripts/operator_stage.py --stage first --ticker "$TICKER" --size "$SIZE" \
  --binary "$BIN" --candidate-receipt "$CANDIDATE_RECEIPT" --book "$BOOK" \
  --evidence-root "$EVIDENCE/operator-stages"
```

Review actual market rules, current fees/depth, account identity, balance-derived limits, candidate receipts and [operations readiness](ops-operator-handoff.md) before any writer. The user selected ntfy-only alerts. Actual human acknowledgment and real exposure/store-loss recovery remain incomplete; independent backup was not exercised. The candidate gate alone grants no live eligibility. If source or binary differs, qualify the changed paths and create a matching build identity before preparing a stage.

For `lip-dwf`, provision only the new stage's store. Attend actual post-only placement, first owned directional fill, durable stop, cancellation of adding orders, exact order/fill identifiers and fees, funded bounded reduction and complete account-wide flat exit. Bound the initial observation at 45 minutes; no owned fill by that point is inconclusive, then request drain and remain through cleanup. SIGTERM is a drain request, not a flatness assertion. Preserve logs, exit statuses, DB/WAL/SHM, journal, latch, provider delivery and human response receipts.

Measure that observation window from process launch. A no-fill run still needs drain and fresh account-wide flat proof, but cannot qualify the first-owned-fill path or advance R2. A signal-created latch is not first-fill-latch evidence. If foreign orders or positions appear, keep the account-wide exit requirement, do not cancel foreign orders, and leave the stage blocked and attended until the account's responsible operator resolves them. Unavailable depth or unresolved exposure is not permission to force an exit at an invented price.

Before retained-latch restart, verify the original process is absent, preserve and inspect the latch, and take a fresh complete account-wide read. The string-valued `-resume 'first-owned-fill stop validation'` records the reason and starts WINDING_DOWN; it does not clear the latch or permit adds. Observe reconstruction/adoption and reduction with no new adds. If it idles DRAINED, signal only its verified PID, then wait for exit. Collect a **new** complete account-wide no-orders/no-positions read after that restart exits. The first post-drain report cannot substitute for the post-restart report.

The after-drain/before-restart read can be one observation at that boundary if still fresh; take another if restart is delayed. `accountcheck -ticker` uses the ticker for market/funding diagnostics, while orders and positions remain account-wide across enumerated subaccounts. Inspect `account_scope_complete=true`, `flat=true`, complete component statuses, zero open orders and zero nonzero positions. Verify a signal target against its executable, exact stage config/store argv and process start time, never by process name alone.

```sh
# FIRST_DIR must be the actual newly prepared stage directory.
cd /Users/hugh/kek/lip/go
go run ./cmd/accountcheck -ticker "$TICKER" -fills -out "$FIRST_DIR/evidence/account-before-restart.json"
# Perform the reviewed attended retained-latch restart and wait for its exit.
go run ./cmd/accountcheck -ticker "$TICKER" -fills -out "$FIRST_DIR/evidence/account-after-restart.json"
```

Unknown ownership, incomplete/nonflat truth, failed cancellation/reduction, unavailable store, missing alarm response or any new add after the first-fill latch blocks promotion. Never cancel foreign orders. Remove only the stage's `live_ok` after every writer/reducer has exited and final complete flat truth is established. Preserve the latch. Do not bootstrap a trading launchd job from this handoff; that activates a writer.

For `lip-9j3` and `lip-8hn.2`, retain real candidate-bound evidence for orders, fills, fees, reduction, clean exit, restart and alarm. Each of the seven fields requires named human observation, valid timestamp and actual hashed files bound to the first-stage identity. The [existing receipt schema](../active-continuation-2026-09-26/operator-handoff.md) remains the format reference only; its old binary, paths and ticker are superseded here. Do not populate fields with planned events or fixture results. The helper checks structural provenance and hashes, not truth of attestations.

The attending operator/coordinator assembles `first-stage-receipt.json` after the referenced observations and files exist, retaining the named witness for each field. The `orders` evidence includes owned adding-order cancel requests/results and positive exchange terminal status. The `fees` evidence uses actual attributable fill `fee_cost` and `is_taker` values; hypothetical preflight exit arithmetic is not an actual fee receipt. A settlement-only disappearance of exposure cannot substitute for the required bounded-reduction execution.

After completed first-stage evidence, create another fresh book/account/program/fee preflight. Use the same reviewed binary and actual first-stage config/evidence receipt:

```sh
cd /Users/hugh/kek/lip
BOOK="$PREFLIGHT/${TICKER}-$(date -u +%Y%m%dT%H%M%SZ)-r2-market-book-series.json"
"$PY" scripts/public_market_snapshot.py --ticker "$TICKER" --out "$BOOK"
"$PY" scripts/operator_stage.py --stage r2 --ticker "$TICKER" --size "$SIZE" \
  --binary "$BIN" --candidate-receipt "$CANDIDATE_RECEIPT" --book "$BOOK" \
  --prior-config "$FIRST_DIR/config.json" \
  --r2-receipt "$FIRST_DIR/evidence/first-stage-receipt.json" \
  --evidence-root "$EVIDENCE/operator-stages"
```

R2 preserves the first stage's runtime/ownership paths and selects pilot rung. A human must separately adjudicate any existing latch against authoritative complete truth before new adds; the helper provides no latch-clearing command. Attend repeated entry/fill/reduction/flat cycles, restart and alarms, then prove fresh account-wide zero open orders and zero positions after the last process exits.

These one-market financial stages do not prove live CR2 turnover. The new `turnover: true` mode requires an explicit approved `candidates` list (1–32 tickers including the seed) and selected-shard funding. It observes approved markets and retains held/possibly-live obligations even after selection changes. The [GET-only config](runtime-evidence-2026-09-27/read-only-smoke/config.json) is an observed two-candidate example, not a live config. Continuous promotion still needs attended real A→B cycles with old-market exposure, per-shard funding, restart and complete exit evidence on the exact live configuration. A newly encountered unadmitted shard or unresolved unbound reservation refuses rather than silently expanding capital or forgetting exposure.

Archived v3 q01 and R1 remain valid for their exact old identity. The new candidate's 91.7-second GET-only observation is neither a new q01 nor financial evidence. See [current report](report.md) for its preserved limitations and the implementation qualification.
