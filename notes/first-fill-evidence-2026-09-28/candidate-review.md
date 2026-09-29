# Candidate-4 first-writer decision (2026-09-29)

**Decision:** candidate-4 is accepted, as code evidence, for the operator-run, attended, static one-market `sizing` first-owned-fill stage (`lip-dwf`, S=12). This is subject to fresh stage inputs within 60 s of arming and operator review. It grants neither unattended operation nor this assistant permission to provision, arm, launch, signal, cancel or trade. It replaces the e3f8a697 decision in [first-writer-review.md](../production-turnover-2026-09-28/continuation-review/first-writer-review.md), which must not be reused to launch.

| | candidate-4 | predecessor (candidate-2) |
|---|---|---|
| binary sha256 | `4d9b8a1ac4a37b6760f6c9d9ad65ef3fe9424e2c89054e98da97cd8ef5797771` | `e3f8a697a20c3153f5c2a8650094b949ff57b985f94b154a7defdad22e9ef361` |
| Go source manifest | `cdb8440a337f445a6d3ae82361272148c24cf823091a1160605afc2714b637ce` | `6047b85a4306c70bd7048c8ed15e41e2ab87b8a21075a8a550747a8c13e1c709` |
| identity | [candidate-4/build-identity.json](candidate-4/build-identity.json) | [candidate-2/build-identity.json](../production-turnover-2026-09-28/candidate-2/build-identity.json) |

The build is `go build -trimpath ./cmd/harness` from `go/`, with HEAD `f7e9571` on a dirty tree. The manifest was the same before and after the build, and an independent rebuild produced the same binary hash. Candidate-3 (`3ace5a62…`) is superseded without use: its first candidate gate failed `check.py` §9.1 because two new files lacked confidence trailers. See [candidate-3/superseded.json](candidate-3/superseded.json). The failed receipt `loop/gates-out/20260928T235511879742Z-52eb2086` is preserved.

## Code evidence on this exact source

- **Candidate gate** `loop/gates-out/20260928T235640338814Z-6681a0f7/receipt.json` (sha256 `e7179310…85495`): PASS in `--candidate` mode. It covers check.py, gate/isolation tests, catalogue anchors, build, vet, gofmt, the module suite (123 s), race tests (153 s) and the 16 safety sentinels (170 s). The repository fingerprint was `1dfd94fb…` before and after the run, and the gated Go source is exactly candidate-4's manifest.
- **Affected mutations** (C3): `loop/gates-out/negative-control-20260929T000511076826Z/receipt.json`, PASS, **67/67 caught** by their named catchers in 624 s. The baseline was green, the source was stable across the run, and no catcher log contains a panic. The set covers:
  - every mutation named by lip-vv2, lip-opm, lip-pcr, lip-6w8, lip-tdz, lip-r0g, lip-d3e, lip-9nq and lip-mgh (47);
  - the 11 lip-kaf mutations and the 7 existing sweep/adoption controls they touch;
  - two new lip-2w3 tolerance mutations, `M-2W3-WIDE-TOLERANCE` and `M-2W3-OWNED-UNDERSHOOT`, since that repair had only its regression.

  Adding those two entries changed only `scripts/harness_negative_control.py`. The re-pinned static gate `loop/gates-out/20260929T001718371782Z-1f8c1d66` passes on the final fingerprint `b1da9fd0…`, whose Go manifest is still candidate-4's.
- **lip-kaf** reproducers and receipts are in [lip-kaf/](lip-kaf/). Rest level and dispatch level, both failed before the fix and passed after it; see the receipts `*-before-fix.txt` / `*-after-fix.txt`. The affected packages passed after the fix: rest, lifecycle and cmd/harness, the last in 114 s. The first mutation receipt, `negative-control-20260928T234900636899Z`, caught 22 of 22.

## What changed since e3f8a697

Thirty-six Go files differ. Hunk attribution for the ten pre-existing production files was delegated to a read-only reviewer and then checked mechanically:

- all 47 hunk counts recomputed;
- the orderbook changes are byte-identical to the lip-2w3 repair diff;
- the lip-vv2 refactor is behaviour-preserving (both `f5TargetPrice` consumers use the price only when `ok`).

No hunk is unattributed. The table excludes lip-kaf's own files: `rest/cancel.go`, `read.go`, `client.go`, the new `sweep_trace.go` files and the comment-only `lifecycle/startup.go` are described above and covered by their own receipts.

| bead | hunks | production behaviour in a single-market run |
|---|---|---|
| lip-2w3 | 5 (orderbook.go) | F5 compares sizes with a one-step, <0.005 tolerance, and invalid sizes never agree. Reachable read-only. |
| lip-vv2 | 10 (crosscheck.go, run.go) | None. The F5 pricing/reducer arithmetic is deduplicated into `targetPriceOn`/`fundedReducerOn`, and stale comments are fixed. |
| lip-opm | 1 (observation_gap.go) | After a host-sleep gap, truth is unknown until a post-gap read lands. Reachable read-only. |
| lip-pcr | 2 (run.go) | One SEV2 `QUOTING_STOPPED_UNTIL_RESTART` at the first sticky reduce per market. Reachable read-only. |
| lip-6w8 | 7 (run.go, runtime.go, shutdown.go) | A permanent final-close failure is reported once, with an accurate lock statement. This is the error path only. |
| lip-tdz | 13 (dispatch.go, qualification.go, run.go, runtime.go, shutdown.go) | None in production. The router extraction and parameter removal are refactors, and the nil-collaborator guards are unreachable because production wires every collaborator. |
| lip-r0g, lip-d3e | 5 + 2 | Turnover-only; no effect with `turnover: false`. |
| lip-mgh | 1 (run.go comment) | None. |
| lip-kaf | 1 (runtime.go) | Installs the sweep-trace writer. |

## Behaviour the attending operator will see differently

1. **Cancel sweeps (lip-kaf).** Before paging, a requested order that a complete `status=resting` read still lists after the retry is read by id. Only the exchange's own record retires it: that order, that market, `canceled`/`executed`, nothing remaining. A 404, an error or a resting record still pages `SEV1 SWEEP_INCOMPLETE`. The alarm text now ends with `Named read: <id> <why>`. Every sweep writes one `harness: sweep-trace {json}` line to the stage's `harness.log`. The line records each DELETE (status, outcome, `reduced_by`, wall times), each verifying read (outcome, pages, listed records with status, remaining and `last_update_time`) and each named read. Last run's six SEV1s came from a resting list that lagged the cancel. The trace in this run shows whether the named read is current in production. If it lags too, SEV1 still fires, which is safe but noisy, and the trace is the evidence to adjudicate it.
2. **F5 precision (lip-2w3).** The single adjacent floating-point residue that stopped BROSFT (`456.00000000000006` vs `456`) is agreement. A real one-cent difference, two rounding steps or an invalid size is still a mismatch.
3. **Sticky REDUCING notice (lip-pcr).** After a gate-held reduce (F4, F5 disagreement, F9, quarantined book, granularity), a new `SEV2 QUOTING_STOPPED_UNTIL_RESTART` says adding waits for an operator restart. See the runbook section "Sticky REDUCING".
4. **Host sleep (lip-opm).** After an observation gap, portfolio truth is unknown until a read started under the gap's token lands. That takes one read cycle, and WINDING_DOWN cannot reach DRAINED from pre-sleep reads. Keep the host awake with `caffeinate` regardless.
5. **Permanent close failure (lip-6w8).** A close failure recorded after the writer stopped is reported once and not retried every second. The text states whether instance ownership is retained or the lock is already released.
6. **Composition hardening (lip-tdz).** Placement requires the permit router, the exchange must carry the dial fence, an orderly stop requires the owner, and qualification errors always exist. Production already used all four; nil-collaborator test paths are gone.

lip-r0g and lip-d3e are turnover-only (`turnover: false` in this stage). lip-9nq and lip-mgh changed tests and comments only.

## q01 adjudication for this attended rung

**No fresh GET-only q01 is required for this attended first-fill stage.** The existing external q01 evidence (config `47b3b732…`, binary e3f8a697) and the previously accepted provider missed-timeout demonstration stay reused. That is the same adjudication the September 28 decision made for the R1→e3f8a697 difference.

Four behaviour differences are reachable by a read-only process with `turnover: false`, and each is covered by code evidence on this source:

- **F5 tolerance (lip-2w3).** Its surface is strictly narrower than before. The regression failed before the fix and passes after it, plus the two new tolerance mutations.
- **Post-sleep truth (lip-opm).** It fires only after a host sleep, and the stage keeps the host awake with `caffeinate`. Covered by `TestObservationGapRetiresPreGapPortfolioTruth` and `M-OPM-PREGAP-TRUTH`.
- **One added SEV2 (lip-pcr).** q01 forbids only SEV1 and dropped anomalies.
- **A shutdown error path (lip-6w8).** It needs a recorded final-close failure.

The sweep confirmation and trace (lip-kaf) are live-only. The one exception, startup adoption, needs orders to cancel, and the account is flat. q01 cannot exercise any of it. The composition changes (lip-tdz) have no production effect, and seam tests plus the M-TDZ/M-9NQ mutations cover the production wiring.

None of these changes makes the prior read-only evidence misleading for this rung. The attended stage itself exercises startup and shutdown under supervision before any order exists. A fresh q01 on the exact binary remains required before any unattended or continuous promotion (pilot-plan §6.4). This is a recommendation the account owner may override.

## Financial evidence still required

Unchanged from the prior decision. Before arming, refresh complete account-wide exposure, the active program, current rules and early-close conditions, market/event/series fees, two-sided executable depth, the selected shard and spendable funds/reserve, all within 60 s of arming. The envelope is `rung=sizing`, one market, `S=12`, `capital_source=selected_shard_balance`, the existing inventory/loss limits and the durable first-owned-fill stop.

The operator provisions, arms and launches, and records the exit status in the launch shell. Preserve:

- real order/fill IDs, post-only and actual fee/taker fields;
- the durable first-fill latch;
- adding-order cancellation and verification, now including the sweep-trace lines;
- funded bounded reduction and alarms.

A no-fill window is inconclusive. After the first exit, obtain a fresh complete account-wide read showing zero open orders and zero nonzero positions. Before the retained-latch restart, refresh everything again. After that restart exits, obtain a separate new complete account-wide flat read. Remove only this stage's `live_ok`, after every stage process is absent and flatness is proven. Retain store, WAL, SHM, journal, latch and failures.
