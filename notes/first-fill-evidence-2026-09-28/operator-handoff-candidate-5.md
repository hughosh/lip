# Operator handoff: candidate-5 first-fill rerun (lip-dwf)

This amends [the candidate-4 handoff](operator-handoff.md) for the next attended first-fill stage. Sections 2-4 of that handoff and its "As executed on 2026-09-29" pattern still apply, except where this page differs. Never restart either earlier stage runtime: the 2026-09-28 BROSFT runtime keeps a signal latch, and the 2026-09-29 KXAAAGASW runtime has had its retained-latch restart.

- **Candidate:** `notes/first-fill-evidence-2026-09-28/candidate-5/harness`, receipt [candidate-5/build-identity.json](candidate-5/build-identity.json).
- **Decision:** [candidate-5/candidate-review.md](candidate-5/candidate-review.md).
- **Why a rerun (Hugh, 2026-09-29):**
  - lip-dwf stays open.
  - The 09-29 stage placed a correct funded reducer that never filled; Hugh closed the position manually.
  - Candidate-5 needs its own attended stage before the pilot anyway.
- **Market:** choose afresh from a GET-only scope.
  - KXAAAGASW-26OCT05-4.4200 is at YES 2c / NO 97c, and preparation now refuses it under H-SEL-6 (lip-t9p).

## What changed for the operator

1. **Preparation captures the book itself (lip-e2t).**
   - Run `scripts/operator_stage.py --stage first` **without `--book`**. It builds `incentives` and `accountcheck` into `<stage>/tools/` first, then captures the book, then runs the reads.
   - Passing `--book` restores the old slow path, where compile time counts against the 60 s window.
   - `evidence/prep-timing.json` records each step and `launch_deadline_utc`, which is the freshness deadline stated directly.
2. **H-SEL-6/7 at preparation (lip-t9p).**
   - A book whose best bids give a mid outside 10-90c, or a bid sum above 99c, is refused.
   - This is the entry filter the pinned-ticker stage lacked.
   - Remaining risk: the harness does not re-check it while running. The first-fill stop limits that to one fill.
3. **Paste-safe command block (lip-8q0).**
   - Guidance is prose `PHASE`/`CHECK`/`NOTE` lines with no shell metacharacters. Commands are indented and contain no `#`.
   - Paste one phase at a time. A phase never spans a human checkpoint.
4. **Hourly heartbeat (lip-1gb).**
   - The prepared config sets `heartbeat_s: 3600`, matching spec H-PING-1 and the dead-man monitor's 3600 s period with 1200 s grace.
   - The 30 s `lip <STATE>` pushes are gone, so any push during the stage deserves attention.
5. **Deferred sweep page (lip-9tt).**
   - An order still unconfirmed after a cancel sweep journals a non-paging SEV3 `SWEEP_PENDING`. It pages SEV1 `SWEEP_INCOMPLETE` only if it stays unconfirmed for 5 s across re-sweeps.
   - The order stays live and not clean throughout, so a SEV1 `SWEEP_INCOMPLETE` on candidate-5 means about 5 s of real non-confirmation: read the sweep trace (`"page"` field) and do not dismiss it as lag.
6. **Fill evidence names the order (lip-3ot).**
   - `accountcheck -fills` rows now carry `order_id`, `side`, `action`, `book_side` and `outcome_side`, raw.
   - `book_side` is authoritative for direction.
7. **Record SEV1 receipt (lip-6b1).**
   - For every SEV1, record the anomaly id, the ntfy delivery time and the time it was seen on the phone.
8. **Manual exit.**
   - If the reducer stalls, follow the runbook section "Stuck reducer and operator manual close" (lip-4qx).
   - The harness never crosses, by design (spec §6.4). A manual close produces SEV1 `FOREIGN_FILL` and `POSITION_DRIFT`, then a drain; that is the correct reaction.

Nothing here qualifies repeated cycles (R2), live turnover (CR-2) or profitability.
