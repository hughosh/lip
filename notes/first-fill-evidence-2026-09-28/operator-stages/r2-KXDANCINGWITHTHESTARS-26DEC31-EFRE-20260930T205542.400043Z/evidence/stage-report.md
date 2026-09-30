# lip-8hn.2 attended R2 stage — 2026-09-30, candidate-6, KXDANCINGWITHTHESTARS-26DEC31-EFRE, S=12, pilot

Hugh attended throughout and approved each live step one at a time: the launch, the SIGKILL, the restart, the SIGTERM and the disarm. The assistant executed each step only after that approval; see `control.log`. The procedure was [operator-handoff-candidate-6.md](../../../operator-handoff-candidate-6.md). The stage ended with a crash-restart, as Hugh decided on 2026-09-30.

## Identity and inputs

**Candidate**
- **Binary:** `dbc250d6…6825`.
- **Source manifest:** `ee8f4ec1…0231`.
- **Receipt:** `candidate-6/build-identity.json`.
- **Git head:** `81047d3`.
- **Config:** sha256 `a5882391…b9b6`.
  - Rung and capital: `pilot`, `heartbeat_s` 3600, capital `selected_shard_balance`.
  - Inventory limits: `s` 12, `s_max` 48, `inv_soft` 3, `inv_hard` 7, `inv_kill` 18, `pnl_kill` −15.

**First-stage binding**
- **Binding:** `predecessor`, to the candidate-5 MCI first stage (identity `bcdaa58f…`).
- **Receipt:** Hugh attested the 7-field `r2-receipt.json` in chat and it was committed in `81047d3`. The helper checks its file hashes, not the events it describes.

**Market**
- **Choice:** Hugh chose it from a GET-only scope run 20:51–20:53Z; see [scope-candidate-6/choice.json](../../../scope-candidate-6/choice.json), `shortlist.json` and `side-flow.json`.
- **Book at scope:** YES 42 / NO 56.
- **Taker flow:** takers hit the YES bid 4.4 times an hour and the NO bid 10.2 times an hour.
- **Fees:** `quadratic`, multiplier 1.
- **Programme:** runs to 2026-10-12T04:16Z. Its `period_reward` of 1,000,000 centi-cents is $100 for the whole 14-day programme.
- **Close:** 2027-01-07.

**Preparation**
- **Duration:** 9.7 s end to end (`prep-timing.json`):
  - tools built 20:55:42.6–44.7;
  - book 44.7–46.0;
  - programme reads 46.0–47.8;
  - candidate reads 47.8–49.2;
  - account 49.2–51.9.
- **Book at prep:** YES 44 × 14.95 / NO 55 × 901.70. It passed H-SEL-6/7.
- **Launch:** 20:55:52Z, 52.7 s before `launch_deadline_utc`.

**Preflight account**
- Complete and flat at 20:55:49.7Z.
- Cash $98.5479, deployable $73.91, worst entry $11.88.

**Runs**
- **Run 1:** `20260930T20555550EEA`, PID 48693, 20:55:52–22:55:26Z. It ended by SIGKILL.
- **Run 2:** `20260930T225639R03F0`, PID 56068, 22:56:37–22:58:47Z. This was the crash restart without `-resume`, and it ended by SIGTERM.

## Observed

1. **Startup**
   - STARTING→RUNNING (`reconciled`) at 20:55:56.519, then IDLE→QUOTING (`selected`) at 20:55:56.833.
   - FUNDING_LIMITS (a5) showed `recovery_commitments=$0.00` and `adding_ceiling=$73.91`, which is a normal start.
   - STARTUP (a7): adopted 0 orders, saw 0 foreign orders and 2 foreign fills.
2. **Adds**, both post-only at the touch:
   - YES 12 @ 44c `01a0f41a-4348-7d0c-bb5b-3a7afb4946bf`, bound 20:55:57.787;
   - NO 12 @ 55c `01a0f41a-4348-7d66-81ca-0907c4dd65c4`, bound 20:55:57.894.
3. **Round trip 1: the owned fill.**
   - NO 12 @ 55c, maker, fee $0.000000, trade `07232488-6389-80cc-b0ec-9ba4d2e922dc` on order `…65c4`.
   - Exchange `created_time` 21:07:10.391Z; harness `first_seen` 21:07:12.406Z, 2.0 s later.
   - The exchange record reads `side=no action=sell book_side=ask`, i.e. the YES ask at 45c.
   - Market QUOTING→REDUCING (`inv_hard`) at 21:07:12.406.
   - The YES 12 @ 44c add that was already resting became the |q|-capped reducer and was not cancelled, as at MCI.
   - The book at prep had shown about 900 contracts at NO 55 ahead of our order. The fill came in an 85.6-contract burst at 21:07:10, so most of that queue must have been cancelled or re-placed behind us. This is inferred, not verified.
4. **Reducer churn, then no reducer for 20 min 48 s.** See Finding 1.
   - At 21:25:37Z a taker cleared the NO 55 level.
   - At 21:25:38.785 a §6.5 requote cancelled `…46bf`. DELETE returned 200, but the list lag left it listed, so SEV3 `SWEEP_PENDING` (a49) and SEV2 `CANCEL_UNVERIFIED` (a50) were raised.
   - The first replacement, YES 12 @ 44c `…d587` (role `reducing`, reserved 21:25:42.540), was requoted at 21:25:43.529 (a51/a52). It swept clean at 21:25:47.7.
   - The second replacement, `…e8ab` (reserved 21:25:48.220), was requoted at 21:25:49.200 (a53/a54) and was never swept again.
   - From 21:25:49 to 21:46:37 no reducer rested while q = −12. A GET-only account read at 21:26:53Z showed zero open orders (`account-reducer-churn-2126.json`).
   - SEV2 `INVENTORY_STUCK` (a77) fired at 21:37:12.
   - At 21:46:36.548 an outside YES 45c bid triggered a §6.5 requote whose sweep named `…e8ab`. DELETE returned 404 GONE with 0 listed, so the sweep was clean.
   - At 21:46:37.166 the harness placed a new reducer, YES 12 @ 45c `01a0f448-a648-7ab8-87ab-d5708c171300`.
5. **Reduction.** All fills were maker with fee $0.000000, on order `…1300` with `book_side=bid`:
   - 3.65 @ 45c: trade `07232509-3ce9-80d2-f9b1-f348baf2b5e4`, exchange 21:55:05.374Z, seen 1.9 s later;
   - 1.76 @ 45c: trade `07232599-bf49-b256-ff02-68e86623dd98`, exchange 22:34:16.778Z, seen 1.0 s later;
   - 6.59 @ 45c: trade `072325b4-6ac9-b132-b3ca-6f2f50364dc0`, exchange 22:46:12.918Z, seen 5.0 s later.
   - REDUCING stayed on after |q| fell below `inv_hard`, as designed. It ends only at exactly zero (`harness/quote/machine.go:206-218`).
6. **Back to flat and re-adding.**
   - REDUCING→IDLE (`flat`) at 22:46:17.954, then IDLE→QUOTING (`selected`) at 22:46:17.969.
   - New adds: YES 12 @ 45c `01a0f47f-4a90-7425-83fc-3b99caa2473d` (bound 22:46:18.070) and NO 12 @ 54c `01a0f47f-4a90-7168-a34a-60f0d9199ea6` (bound 22:46:18.170).
   - Round trip 1 took 99 min, from 21:07:10 to 22:46:12.
7. **Crash drill: SIGKILL.**
   - The gate at 22:55:26 found the market QUOTING, q = 0, both adds bound, and 548 s with no order or state activity.
   - Argv and start time were checked, then SIGKILL was sent at 22:55:26.128Z.
   - `exit-status.txt` = **137**. No latch file was written.
8. **Post-crash account read**, 22:55:26.4–29.0Z, 0.26 s after the kill (`account-after-crash.json`):
   - complete, not flat;
   - both adds resting: `…473d` YES 12, and `…9ea6`, which the exchange lists on the YES leg;
   - no position; funding $98.5479.
9. **Restart without `-resume`**, PID 56068 at 22:56:37Z:
   - FUNDING_LIMITS (a1) showed `recovery_only=true`, `recovery_commitments=$24.00` (the two adopted orders at $1.00 each) and `capital_max=$122.55`, at 22:56:40.105.
   - The latch `{"trigger":"funding_recovery"}` has ts 22:56:40.105.
   - STARTING→RUNNING at 40.105, then RUNNING→WINDING_DOWN (`global_stop`) at 40.144.
   - STARTUP (a3): "adopted 2 order(s)… saw 0 foreign order(s)".
   - Cancel sweeps: `…9ea6` swept clean. `…473d` raised SEV3 `SWEEP_PENDING` (a4) and SEV2 `CANCEL_UNVERIFIED` (a5) from the list lag, then swept clean at 22:56:41.357.
   - WINDING_DOWN→DRAINED at 22:56:41.966. No order was placed.
10. **Restart exit.** SIGTERM at 22:58:47.465Z, with argv and start time checked first. It raised SEV2 `SIGNAL_DRAIN` (a24) with inventory=false and live orders=false. Exit status **0** (`restart-exit-status.txt`).
11. **Post-restart account-wide read**, 22:58:57.9–22:59:00.0Z (`account-after-restart.json`):
    - complete and flat, funding $98.5479;
    - all four fills present with order_id, side, action and book_side;
    - `live_ok` removed at 22:59:23.140Z;
    - the latch, store, WAL and journal are retained.

**Net P&L: $0.0000.**
- Bought NO 12 @ 55c, then YES 12 @ 45c in three maker fills (3.65 + 1.76 + 6.59), with no fees.
- Funding went from $98.5479 to $98.5479. The balance polls show the netting: $98.54 → $91.94 after the NO fill, then back to $98.54 by the last YES fill.

## F5 cross-checks (candidate-6's lip-2mz repair)
- **Every cross-check agreed.** Run 1 raised 69 `BOOK_QUIET` (SEV2), each followed by `BOOK_CROSSCHECK_AGREED` (SEV3); run 2 raised 1 of each.
- **No mismatches.** There were zero `BOOK_CROSSCHECK_MISMATCH` and zero `QUOTING_STOPPED_UNTIL_RESTART`.
- **Our own size was covered.** The checks ran with our own orders resting ("matched … through Target Size on both sides, including our own resting size (H-FAIL-6)").
- **Compared with candidate-5**, which raised a29/a36 on this path at MCI.

## Alarms and push receipt (lip-6b1)
- **SEV1 in either run: none.** `jq 'select(.sev==2)' runtime/anomaly.jsonl` is empty (0 of 240 rows).
- **ntfy deliveries:** 34 pushes, deduplicated by message id, are in `ntfy-delivery.json`; `ntfy-delivery.jsonl` is the raw 199-line stream.
  - 2 STARTING. The restart's STARTING push arrived as an attachment, so its text is not in the stream.
  - 1 hourly RUNNING heartbeat.
  - 31 SEV2: 9 `FOREIGN_FILL_INHERITED`, 9 `BOOK_QUIET`, 8 `PNL_MARK_UNAVAILABLE`, 3 `CANCEL_UNVERIFIED`, 1 `INVENTORY_STUCK`, 1 `SIGNAL_DRAIN`.
  - All were priority 3.
- **Pushes are budgeted.** The store holds far more SEV2 rows than were pushed: 70 `BOOK_QUIET` and 48 `PNL_MARK_UNAVAILABLE`.
- **Phone receipt times:** none recorded, and with no SEV1 there was none to record.

## Findings
1. **The reducer can disappear because of a "phantom" order (candidate-6 defect; bead lip-14o, P1, blocks lip-8hn.2).**
   - **What happens:** a reducer placed and then requoted before the first 5 s orders walk lists it is cancelled inside Kalshi's ~1.5 s list lag. Its sweep is unverified, so its `o.pending` entry stays: acked, never listed and never retired.
   - **Why no reducer is placed:**
     - `restingOn` counts that entry (`cmd/harness/run.go:3289-3296`), so the reducer's size is already at target.
     - `Decide` sees an order of ours at the touch and does nothing (`harness/quote/requote.go:263`).
   - **Why no alarm fires:** `escalateUnresolved` skips acked entries on the assumption they are younger than one `position_poll_s` (`cmd/harness/run.go:3788-3791`).
   - **Why it never clears:** reducing sides are not swept again after an unverified cancel. `harness/rest/cancel.go:227-228` assumes the caller does that every tick, but the owner does it only for sides being turned off (`cmd/harness/run.go:2583-2588`).
   - **Ways out:**
     - an outside touch move that triggers a §6.5 requote. That is what cleared it at 21:46:36.548: the sweep named `…e8ab` and came back clean.
     - a restart.
   - **Consequences:**
     - The position had no exit for 20 min 48 s, and nothing raised an alarm.
     - A planned SIGTERM cannot finish while a phantom exists. `anyLiveOrder` counts `len(o.pending)` (`cmd/harness/run.go:4073-4076`), and the drain needs it false (`harness/lifecycle/drain.go:349`). It would reach `DRAIN_TIMEOUT` (SEV1) after 12 h.
     - A crash-restart starts with an empty `pending` and does place the reducer.
   - A subagent traced this from the source, read-only. The assistant checked the cited lines, and the 21:46:36.548 sweep trace confirms the escape path.
2. **accountcheck cannot report a held position.** `parsePositions` returns `malformed` for any nonzero `event_exposure_dollars` (`cmd/accountcheck/main.go:242-252`). While inventory is held, the read therefore gives `account_scope_complete=false` and an empty `nonzero_positions` (`account-reducer-churn-2126.json`). The production decoder read the same positions fine. This is conservative by design, but it means a read taken mid-stage cannot show the position.
3. **Reading a store that still has its WAL.**
   - `sqlite3 "file:<db>?immutable=1"` ignores the WAL. Here the main database file was last written at 22:29Z, and all of run 2 lived only in the 4 MB WAL.
   - Use `?mode=ro` while `-wal` and `-shm` exist; it registers reader marks in `-shm`. The candidate-5 advice to use `immutable=1` applies only once the WAL is gone.
4. **Quiet-book SEV2 volume.** In 2 h the quiet book produced 70 `BOOK_QUIET` and 48 `PNL_MARK_UNAVAILABLE`. They are informational, but they dominate the push budget.
5. **Exit policy (a design question Hugh raised; not a defect).**
   - The maker-only reducer (spec §6.4) held inventory for 99 min, and quoting stayed one-sided until flat because `inv_hard` 7 is below `S` 12.
   - **On EFRE, crossing would have cost far more than it recovered.**
     - A taker exit costs about 2.7c per contract: 1c of spread plus the fee.
     - The pool is $100 over 14 days. At S=12 our two-sided share of it is about 1c/h.
   - **Crossing can pay only where incentive per hour is large relative to (spread + fee) × |q|.** Candidates:
     - a time- or adverse-move-bounded taker exit;
     - `inv_hard ≥ S` with skewed two-sided quoting;
     - ranking markets by pool per hour.
   - Filed as design bead lip-89x (P3).
6. **Fill detection lag** was 1.0–5.0 s over the four fills.

## Not demonstrated
- **At least 3 round trips.** Only 1 was observed, at 99 min; the 2 h cap was reached. Re-adding after flat was observed once.
- **A crash while holding inventory.** The drill crashed flat, with two adds resting. The restart's recovery reducer for inherited inventory was not exercised.
- **A planned drain with a phantom present.** This finding comes from the code, not from a live run.
- **SEV1 receipt timing.** No SEV1 occurred.
- **Beyond this stage's scope:** unattended operation (lip-8hn.4), CR-2 turnover, and LIP payout or profitability.
