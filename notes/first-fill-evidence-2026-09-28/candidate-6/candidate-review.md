# Candidate-6 decision (2026-09-30)

**Decision:** candidate-6 is accepted, as code evidence only, as the binary for the next operator-run attended stage. It replaces candidate-5 because it carries the lip-2mz repair.
- That stage is the R2 or pilot preparation (lip-8hn.2, lip-9j3). Its scope and procedure are Hugh's to set; this review sets neither.
- It grants no unattended operation, and no assistant authority to provision, arm, launch, signal, cancel or trade.

| | candidate-6 | predecessor (candidate-5) |
|---|---|---|
| binary sha256 | `dbc250d62521234b4d705a29ac6382503106b0107bf8e31f4a6341cf63f06825` | `516778b685e7a9d55981d94b62388c1e558430810909adc22306d115327f17c0` |
| Go manifest | `ee8f4ec16d077d83e98cee595dcbee651afe79959d6c000cb3a6f122292d0231` | `90114bfef8baa72ccd43d9ed8691de5a5594be5e0830ec553f415b5139ed39c5` |
| identity | [build-identity.json](build-identity.json) | [../candidate-5/build-identity.json](../candidate-5/build-identity.json) |

**Build:**
- `go build -trimpath -o candidate-6/harness ./cmd/harness` from `go/`, go 1.27.1 darwin/amd64, HEAD `b303614`, dirty tree, via `loop/run/build_candidate.py`.
- The manifest was identical before and after the build.
- An independent rebuild produced the same binary hash.

## What changed from candidate-5

Both manifests list 291 files and differ in exactly two, both from this triage: `go/harness/rest/orderbook.go` and `go/harness/rest/orderbook_precision_test.go`. No parallel-session Go change is included.

- **lip-2mz: F5 compares on the exchange's 0.01-contract grid** (`go/harness/rest/orderbook.go`).
  - **The defect.** The websocket book adds every delta into a float64 level (`core.Book.ApplyDelta`, frozen with `go/core`), so a level's residue grows with the number of deltas it absorbs. lip-2w3 forgave one adjacent step; candidate-5 then raised a29 (4 steps) and a36 (2 steps). No step count bounds that residue. Wire sizes are two-decimal (H-CO-2), so a real difference is at least one quantum, and residue stays far below half of one.
  - **`sameBookSize`** puts both sizes on the grid with `num.QtyFromFloat` (H-CO-4a's quantum) and compares the integers. lip-2w3's single-`Nextafter` tolerance is gone.
  - **`targetWalk`** totals in `num.Qty` and reaches the Target when `total.Float() >= target`. The int64 sum is overflow-guarded. A float total can fall just short of the Target where the grid total lands on it exactly. Residue causes that, and so does the naive sum's own rounding (0.3 + 0.6 < 0.9).
  - **The own-resting check** is `bookQty(have) < own.Size`, on the grid.
  - **`bookQty`** accepts finite, positive sizes below `maxBookSize` = 1e13 contracts. Below 2^44 a double's spacing is at most 2^-9 of a contract, so every two-decimal size lands within about 0.16 of its own quantum. From 2^45 up, adjacent quanta collide. A size below half a quantum is zero quanta but still a level, so a residue-only "ghost" level keeps its price-named reason.
- **Behaviour that changed beyond the bug:**
  - Residue on the level that reaches the Target now agrees. Before, the float walks ended at different depths, or the websocket walk never reached the Target.
  - Sizes of 1e13 contracts or more are invalid, so they are a disagreement. Before, they compared as floats: equal 1e300 sizes agreed.
  - lip-2w3's case "two rounding steps remain a mismatch" is inverted, because it pinned the defect.
- **Unchanged:** `core.Book.Qualifies` still totals the live book in float64. On a book whose float total falls just short of the Target, it crosses one level deeper, or reports "not qualifying", which blocks adding (fail-closed). `go/core` is frozen; see `targetWalk`'s comment.
- **Tests** (`go/harness/rest/orderbook_precision_test.go` only):
  - `TestCompareBooksObservedResidueAgrees`: a29 and a36 at their observed side, price, depth and Target 1000, plus residue far beyond any observed step count. Controls: one quantum short, one quantum long, and nearer-the-next-quantum.
  - `TestCompareBooksTargetBoundaryResidue`: residue at the Target boundary agrees. Controls: one quantum short with a deeper level, and one quantum short with none.
  - `TestCompareBooksOwnedSizeResidue`: residue under our own size agrees. Controls: one quantum short on each book.
  - `TestCompareBooksSizePrecisionAtDepth`: one case inverted, and five added. Four are bound cases: oversized equal, oversized different, a quantum apart at 2^45, and a quantum apart just under the cap. The fifth is a control: a residue-only websocket level is still named by its price.
- **Failed before, passes after:** [../lip-2mz-precision-repair/before-fix.txt](../lip-2mz-precision-repair/before-fix.txt) and [after-fix.txt](../lip-2mz-precision-repair/after-fix.txt).
  - The before-fix run is candidate-5's `orderbook.go` (sha256 `173b13d7…eea9`, equal to its manifest entry) with the final tests. 11 subtests fail and every control passes.
  - 9 of the 11 are wrong verdicts: the seven residue regressions, the inverted two-step case, and equal 1e300 sizes agreeing. a29 and a36 reproduce production's anomaly text word for word. The other 2 (unequal oversized sizes, and the 2^45 pair) were already rejected by candidate-5, only with a different reason.
  - After the fix, all 55 CompareBooks tests pass.
- **Adversarial review:** an independent review of the diff (item-by-item, with fuzzing against exact big-integer arithmetic) found one real hole.
  - The first build used `maxBookSize` = 1e15, above the 2^45 grid-exactness limit, so `35184372088832.02` and `.03` would agree. Verified, then fixed with the bound, a test and `M-2MZ-BOUND`.
  - It also found that refusing zero-quantum sizes had turned a ghost level's price-named reason into "does not reach Target Size". Fixed: a zero-quantum size is a level again.
  - It confirmed the overflow guard, the monotone target comparison, and that the walk-depth check cannot fire while both walk and comparison are on the grid.
  - That pre-review build (binary `63d0fe08…b6f7`, manifest `e5494b41…12c9`) had passed its own candidate gate and affected mutations. It is kept at [../candidate-6-prereview/](../candidate-6-prereview/) and superseded here; its `build-identity.json` still names the `candidate-6/` path it was built at.

## Code evidence on this exact source

- **Candidate gate:** `loop/gates-out/20260930T041920849857Z-4deb0b54/receipt.json` (sha256 `2d8de5ce…91ea`).
  - PASS in `--candidate` mode, 474 s.
  - It ran check.py, gate/isolation tests, catalogue anchors, build, vet, gofmt, the module suite (123 s), race (157 s) and the safety sentinels (182 s).
  - Sentinels: 18/18 caught, including the new `M-2W3-WIDE-TOLERANCE` and `M-2MZ-STEP-TOLERANCE`.
  - The repository fingerprint was `7174438c…0772` before and after. The Go manifest recomputed after the runs is still `ee8f4ec1…0231`.
- **Affected mutations:** `loop/gates-out/negative-control-20260930T042731509401Z/receipt.json` (sha256 `e154d7ae…abd0`).
  - PASS, **7/7 caught** by named catchers in 39 s, with a green baseline.
  - The sandbox-input hash `caf81dd7…ccbb` was stable across the run, and no log contains `panic:`.
  - The set is every catalogued mutation whose edits touch `rest/orderbook.go`: the two re-anchored `M-2W3-*` and the five new `M-2MZ-*`.
- **Static re-pin:** `loop/gates-out/20260930T042822903840Z-aaf3efb3/receipt.json` (sha256 `db96f2c7…be14`).
  - PASS on the final fingerprint `7174438c…0772`, the one the candidate gate ran on.
  - This review and the receipts sit in `notes/*-evidence-*`, which the fingerprint excludes.
- **Owner package:** `go test -count=1 ./cmd/harness/` (the F5 owner, `crosscheck.go`) passed on this comparison before the review fix. The module suite in the gate above covers it on the final source.
- **Pre-review build:** its receipts are `loop/gates-out/20260930T040112632318Z-2b26b555` (candidate gate PASS) and `loop/gates-out/negative-control-20260930T040915992149Z` (6/6 caught). They describe the superseded 1e15-bound source, not this candidate.

## Non-Go changes in the same triage (fingerprint only)

- `scripts/harness_negative_control.py`:
  - `M-2W3-WIDE-TOLERANCE` and `M-2W3-OWNED-UNDERSHOOT` are re-anchored onto the grid lines, with the same intent and catchers.
  - New: `M-2MZ-STEP-TOLERANCE` (lip-2w3's one-step rule), `M-2MZ-TRUNCATE` (truncating onto the grid), `M-2MZ-BOUND` (a 1e15 bound), `M-2MZ-FLOAT-WALK` (a float Target walk) and `M-2MZ-OWNED-FLOAT` (a float own-resting compare). The catalogue grows from 394 to 399.
- `scripts/safety_mutations.json`: adds the sentinels `M-2W3-WIDE-TOLERANCE` and `M-2MZ-STEP-TOLERANCE`, one per failure direction of this comparison (16 to 18).

## Residual risk (not fixed here)

- **Off-grid sizes are rounded, not refused.** Two sizes at half-quanta 0.01 apart (0.275 and 0.285) become the same Qty. A two-decimal wire cannot produce them. But neither the REST read nor `core` asserts two-decimal sizes, and `num.ParseQty` rounds more decimals too. It is a harness-wide H-CO-4a assumption, and asserting it would be the size analogue of H-CO-3a.
- **Ghost levels.** A residue-only websocket level (below 0.005, surviving ApplyDelta's 1e-9 deletion) inside the walk is still a disagreement, and so still latches QUOTING_STOPPED_UNTIL_RESTART. It is a real phantom level in the live book, where it can be the touch. Producing one needs, by estimate, levels near 1e7 contracts or a residue random walk of about 1e8 deltas.

Not covered: live behaviour. No stage has run candidate-6. Nothing here qualifies R2, CR-2, repeated cycles or profitability.

**Later static re-pin:** `loop/gates-out/20260930T050529097382Z-d315a8bf`, PASS on fingerprint `6b81d84d…0909`. It was taken after AGENTS.md gained the note that `notes/*-evidence-*` directories are outside the fingerprint. The Go manifest is still `ee8f4ec1…0231`.
