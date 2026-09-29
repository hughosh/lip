# Candidate-5 decision (2026-09-29)

**Decision:** candidate-5 is accepted, as code evidence only, for the next operator-run attended static one-market `sizing` first-owned-fill stage: the lip-dwf rerun, S ≤ 12, with fresh inputs and operator review.
- It supersedes candidate-4 for that stage.
- It grants no unattended operation, and no assistant authority to provision, arm, launch, signal, cancel or trade.
- The operator procedure is [../operator-handoff-candidate-5.md](../operator-handoff-candidate-5.md).

| | candidate-5 | predecessor (candidate-4) |
|---|---|---|
| binary sha256 | `516778b685e7a9d55981d94b62388c1e558430810909adc22306d115327f17c0` | `4d9b8a1ac4a37b6760f6c9d9ad65ef3fe9424e2c89054e98da97cd8ef5797771` |
| Go manifest | `90114bfef8baa72ccd43d9ed8691de5a5594be5e0830ec553f415b5139ed39c5` | `cdb8440a337f445a6d3ae82361272148c24cf823091a1160605afc2714b637ce` |
| identity | [build-identity.json](build-identity.json) | [../candidate-4/build-identity.json](../candidate-4/build-identity.json) |

**Build:**
- `go build -trimpath -o candidate-5/harness ./cmd/harness` from `go/`, go 1.27.1, HEAD `f7e9571`, dirty tree, via `loop/run/build_candidate.py`.
- The manifest was identical before and after the build.
- An independent rebuild produced the same binary hash.

## What changed from candidate-4

The manifests differ in exactly nine files, all from this triage (the lip-dwf 2026-09-29 stage findings). No parallel-session Go change is included.

- **lip-9tt: defer the sweep page, never the retirement.**
  - Files: `go/harness/rest/cancel.go`, `client.go` and `sweep_trace.go`.
  - `Client` now keeps mutex-guarded per-order first-unconfirmed times, using an injected clock with monotonic ages.
  - An order still unconfirmed after the retry and the named read journals a SEV3 `SWEEP_PENDING`. It pages SEV1 `SWEEP_INCOMPLETE` once it has been unconfirmed for `sweepPageBound` (5 s) across re-sweeps.
  - Either way the sweep stays not clean and the order stays in `StillResting`. The caller keeps it in `sweptOrders` and re-sweeps it every tick (`go/cmd/harness/run.go:3009`).
  - An episode starts or ends only on a complete verifying read. The incomplete-read and throttle paths neither start nor end one.
  - SWEEP_INCOMPLETE has no non-paging consumer. The only SEV1-driven stop in production matches `POSITION_DRIFT` (`portfolio_stop.go:33`).
  - Spec H-ORD-4 and H-ORD-4c are amended to match. H-FAIL-3 is unchanged: nothing retires on elapsed time.
  - Hugh chose this route over treating our own `200 reduced_by=full` followed by a DELETE 404 as terminal evidence. That alternative contradicted H-ORD-4c.
  - Remaining risk: a genuinely stuck order pages about 5 s after its first unconfirmed sweep ends, roughly 6-7 s after the DELETE.
- **lip-3ot: fill evidence rows in accountcheck.**
  - File: `go/cmd/accountcheck/main.go`.
  - Rows now carry the raw `order_id`, `side`, `action`, `book_side` and `outcome_side`.
  - `book_side` is included because `rest.readSide` treats it as authoritative over `side`.
- **Tests.**
  - Changed: `cancel_test.go`, `cancel_lag_test.go` and `go/cmd/harness/cancel_lag_dispatch_test.go`. The existing escalation tests now prove SEV3 on the first sweep and SEV1 at the bound; none of their SEV1 assertions were deleted.
  - Added, five episode tests: first sweep pending; page at the bound; named-read retirement ends the episode; an unverified read neither starts nor ends one; a clean sweep ends all.
  - Added: accountcheck attribution tests.
  - `go/cmd/harness/runtime.go` changed only in a comment.

## Code evidence on this exact source

- **Candidate gate:** `loop/gates-out/20260929T201249040088Z-2b8ecb7d/receipt.json` (sha256 `177f4a1f…593c`).
  - PASS in `--candidate` mode, 466 s.
  - It ran check.py, gate/isolation tests, catalogue anchors, build, vet, gofmt, the module suite (120 s), race (155 s) and the safety sentinels (180 s).
  - The repository fingerprint was `12abee70…a1bc` before and after, and the gated Go source is candidate-5's manifest.
- **Affected mutations:** `loop/gates-out/negative-control-20260929T202054203501Z/receipt.json` (sha256 `af5f4d24…da6c`).
  - PASS, **21/21 caught** by named catchers in 140 s, with a green baseline.
  - The sandbox-input hash `e6c84096…2d8d` was stable across the run, and no log contains `panic:`.
  - The set is every catalogued mutation whose edits touch `rest/cancel.go`, `rest/client.go`, `rest/sweep_trace.go` or `cmd/accountcheck/main.go` (20), plus `M-KAF-TRACE-UNWIRED` for the runtime client wiring.
  - It includes five new lip-9tt mutations: `M-9TT-PAGE-NOW`, `M-9TT-NEVER-PAGE`, `M-9TT-PENDING-CLEAN` (caught at the dispatcher, where `Absent` must stay false), `M-9TT-NO-CLEAR` and `M-9TT-UNVERIFIED-SETTLES`.
- **Static re-pin:** `loop/gates-out/20260929T202344606511Z-dfa3c7ff/receipt.json` (sha256 `1b97aeb4…ad45`).
  - PASS on the final fingerprint `d0c38c0b…d51f`, taken after the runbook's lip-9tt sentence was updated. Its Go manifest is still `90114bfe…39c5`.

## Non-Go changes in the same triage (fingerprint only)

- `scripts/operator_stage.py`:
  - paste-safe phases (lip-8q0);
  - `heartbeat_s: 3600` (lip-1gb);
  - H-SEL-6/7 at preparation (lip-t9p);
  - tools built before an in-prepare book capture, plus `prep-timing.json` (lip-e2t).
  - `scripts/test_operator_stage.py`: 22 tests, OK.
- `notes/cr1-operations-runbook.md`: "Stuck reducer and operator manual close" (lip-4qx; the SEV1 receipt line is for lip-6b1).
- `notes/harness-spec.md`: the H-ORD-4 and H-ORD-4c amendment.
- `scripts/harness_negative_control.py`: the five `M-9TT-*` entries.

Not covered: live behaviour of any of this. The lip-dwf rerun is the first live observation. Nothing here qualifies R2, CR-2, repeated cycles or profitability.

**Later static re-pin:** `loop/gates-out/20260929T203603574208Z-e685d6c8`, PASS on fingerprint `92a69517…2566`. It was taken after `notes/verification-workflow.md` gained the unittest and receipt-hash-scope notes. The Go manifest is still `90114bfe…39c5`.
