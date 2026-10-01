# lip-8hn.2 attended R2 stage — 2026-10-01, candidate-7, KXEPLRELEGATION-27-MCI, S=12, pilot

Hugh attended and approved each live step one at a time in chat: the launch ("Yes, launch now", about 03:09Z) and the planned SIGTERM ("Yes, SIGTERM harness now", 05:58Z); the disarm (`rmlive`) is the last line of `control.log`. He pre-selected drill C at about 03:06Z for the case the market was REDUCING at the cap. The assistant executed each step only after that approval; see `control.log`. This stage is the attended evidence for the lip-14o fix (section "lip-14o checks" below) and follows [the candidate-7 handoff](../../../operator-handoff-candidate-7.md).

## Identity and inputs

**Candidate**
- **Binary:** `f701130c…1eb7` (`candidate-7/harness`; `independent_rebuild_sha256` equal).
- **Source manifest:** `af0ad480…4436`, recomputed from the tree at 02:5xZ and again by the prep.
- **Receipt:** `candidate-7/build-identity.json`, predecessor candidate-6 (`dbc250d6…6825`).
- **Git head:** `6facdb1`. The prep recorded the tree dirty: `stage-helpers-candidate-7/drill.py` carried `TICKER`, plus the untracked binaries and `scope-candidate-7/` (`identity.json` `git_status`).
- **Config:** sha256 `79aa15d0…`.
  - Rung and capital: `pilot`, `heartbeat_s` 3600, capital `selected_shard_balance`.
  - Inventory limits: `s` 12, `s_max` 48, `inv_soft` 3, `inv_hard` 7, `inv_kill` 18, `pnl_kill` −15.

**First-stage binding**
- **Binding:** `predecessor` at `predecessor_depth` 2, through the recorded chain candidate-7 → candidate-6 → candidate-5, to the candidate-5 MCI first stage (identity `bcdaa58f…`). Candidate-6 ran no first stage; the one-hop rule would have refused this (handoff, section 1 step 2).
- **Receipt:** the same `r2-receipt.json` candidate-6 used. `load_r2_receipt` checks its file hashes, not the events it describes.
- **Checked twice:** at 02:5xZ through the script's own `validate_build_receipt`, `receipt_predecessors`, `load_prior_stage` and `load_r2_receipt`, with no provisioning and no account read; then by the prep itself.

**Market**
- **Choice:** Hugh chose it from a GET-only scope run 03:00–03:04Z; see [scope-candidate-7/choice.json](../../../scope-candidate-7/choice.json), `ranked-table.txt`, `shortlist.json`, `side-flow.json` and `touch-moves.json` (new: a touch-movement proxy).
- **Rank:** first of 30 by the handoff's criterion, taker trades per hour on the worse side: 13.22/h on the YES bid, 17.32/h on the NO bid over the last 1000 trades (32.7 h). Join-the-touch waits YES 16.0 min (queue 101.55) and NO 35.6 min (queue 344.95).
- **Declined:** KXRAIN-26OCT01-NOLA (the assistant's recommendation for its 16c 3 h range and 4.7/14.4 min waits; a volatile weather market resolving 2026-10-02) and KXAAAGASW-26OCT05-4.3800.
- **Book at scope:** YES 30 / NO 68, spread 2c, a 4c trade-price range in 3 h.
- **Fees:** `quadratic`, multiplier 1.
- **Programme:** runs to 2026-10-03T03:59Z; `period_reward` 2,000,000 centi-cents.
- **Close:** 2027-06-21.
- **Same ticker as the first stage.** Its runtime keeps its latch and was not touched; this stage provisioned its own store.

**Preparation**
- **Duration:** about 10 s end to end (`prep-timing.json`):
  - tools built 03:10:01.6–03.5;
  - book 03.5–04.8;
  - programme reads 04.8–06.7;
  - candidate reads 06.7–08.3;
  - account from 08.4.
- **Book at prep:** YES 30 × 101.55 / NO 68 × 344.95. It passed H-SEL-6/7.
- **Exit arithmetic:** taker exits of 12 at captured depth net $3.4236 (YES) and $7.9772 (NO).
- **Launch:** 03:10:11Z, 52.3 s before `launch_deadline_utc` (03:11:03.6Z).

**Preflight account**
- Complete and flat at 03:10:08.8Z.
- Cash $98.5479, deployable $73.91, worst entry $11.88.

**Runs**
- **Run 1:** `20261001T031013XE7QF`, PID 83084, 03:10:11–10:49:34Z (7 h 39 min). Planned SIGTERM at 05:58:17Z (drill C); it ended by its own exit, status 0, 4 h 51 min later, once flat. There is no restart in drill C.

## Observed

1. **Startup**
   - STARTING→RUNNING (`reconciled`) at 03:10:14.379, then IDLE→QUOTING (`selected`) at 03:10:14.703.
   - FUNDING_LIMITS (a9) showed `recovery_commitments=$0.00` and `adding_ceiling=$73.91`, a normal start.
   - STARTUP (a11): adopted 0 orders, cancelled 0 stale, saw 0 foreign orders and 4 foreign fills.
   - 28 `FOREIGN_FILL_INHERITED` (SEV2): the account's fills from earlier stages, including candidate-6's EFRE fills (for example `072325b4…` on `01a0f448…1300`), "already on the account at startup", reported and not latched.
2. **Adds**, both post-only at the touch, bound at 03:10:15:
   - YES 12 @ 30c `01a0f570-f1d8-79ac-ac6b-552963bb65ed` (exchange `created_time` 03:10:15.677);
   - NO 12 @ 68c `01a0f570-f1d8-7a08-901a-41219019b3ed`.
3. **Round trip 1: the owned fill.**
   - NO 12 @ 68c, maker, fee $0.000000, trade `0723237f-9719-ad46-cf60-a22b10c5da55` on order `…b3ed`.
   - Exchange `created_time` 03:30:54.681Z (`taker_side=yes`, `taker_book_side=bid`: a YES buyer at 32c lifting our NO); harness `first_seen` 03:30:55.372, 0.7 s later.
   - Market QUOTING→REDUCING (`inv_hard`) at 03:30:55.372.
   - The YES 12 @ 30c add that was already resting became the |q|-capped reducer and was not cancelled, as at MCI's first stage and at EFRE.
4. **The filled order's cancel came back unverified, and cleared in 219 ms.** The side being turned off (NO) was swept at 03:30:55.372–55.893: DELETE answered 404 in both rounds, but the verifying read still listed `…b3ed` inside Kalshi's list lag, so SEV3 `SWEEP_PENDING` (a62) and SEV2 `CANCEL_UNVERIFIED` (a63) were raised at 55.893/55.894. The re-sweep at 55.894–56.112 found it absent on a complete read and ended `clean`. `phantoms.py` has printed `"phantom_candidates": {}` at every read since. This is the one list-lag episode of the stage; see lip-14o check (a).
5. **Reduction, partial.** 3.17 @ 30c, maker, fee $0.000000, trade `0723232d-9659-b806-0937-bbe98c136339` on `…65ed` (`taker_side=no`, `taker_book_side=ask`): exchange 03:52:12.798Z, seen 03:52:16.864, 4.1 s later. q went from −12 to −8.83; the remaining 8.83 kept resting at 30c.
   - SEV2 `INVENTORY_STUCK` (a64) at 04:00:55: 30 min beyond `inv_hard` while already REDUCING. Hugh was told (push) and chose to wait.
   - Spot-check reads during the reduction (`reduction-1.json` 03:31:41Z, `after-sigterm.json` 05:58:50Z, `during-halt-0728.json` 07:28:50Z): one open order each time, the YES reducer, with `remaining_count` 12.00 then 8.83. Funding $90.3879 after the NO fill, $92.6069 after the partial (3.17 pairs netted), each exact to the cent.
6. **Flow after the fill was one-sided.** Every print from 03:52Z to the halt was a YES buyer lifting the 32–33c ask (04:44:40 ×255.12, 05:16:32 ×269.59 and ×50.51, 06:13:20 ×40.52, 06:57:52 ×21.99). Nobody sold into the 30c bid and nobody bid above it, so no §6.5 requote fired and the queue ahead of our order (about 100 contracts at launch) was never consumed. The 30c level grew to about 1,080 contracts, mostly behind us in time priority.
7. **Cap at 05:10:11Z:** market REDUCING, q −8.83, the reducer bound, `phantoms.py` `{}`, no SEV1, no latch. One round trip started, none completed. Per the plan, the state chose drill C.
8. **Drill C: planned SIGTERM while holding inventory.**
   - Argv and start time were checked, then SIGTERM was sent at 05:58:17.034Z (`drill.py term harness`; Hugh's yes at 05:58Z, 47 min after the cap question).
   - SEV2 `SIGNAL_DRAIN` (a65): "sigterm received after 2h48m3s of uptime with inventory=true and live orders=true; the process does NOT exit. It stops adding, keeps the reducing quote alive, keeps monitoring, and exits only once every market …".
   - RUNNING→WINDING_DOWN (`global_stop`) at 05:58:17.056. The latch `{"trigger":"sigterm"}` has ts 05:58:17.034.
   - The post-signal read at 05:58:50Z showed the one open YES order, 8.83 remaining. `term` reported `exit status = None` after its 300 s wait, as the handoff says to expect.
9. **Kalshi's scheduled halt stopped the drain.** From 07:00:01Z no trade printed anywhere on the exchange. `GET /exchange/status` returned `exchange_active=false`, `trading_active=false` on every index; `GET /exchange/schedule` lists Thursday hours as close 03:00 / open 05:00 ET, i.e. 07:00–09:00Z. The reducer cannot fill before 09:00Z; it stayed on the exchange (`during-halt-0728.json`).
   - **Websocket churn through the halt.** From 07:01:06Z SEV2 `WS_DISCONNECT` fired every 61.4 s (29 by 07:29Z), each followed by `RECONCILE_TOKEN_STALE` (a portfolio read that straddled the reconnect, discarded) and `PNL_MARK_UNAVAILABLE`. The ping ladder stayed healthy and REST answered 200 from the harness and from the operator shell; name resolution was fine. The cause is the F1 read-deadline backstop in `wsx/session.go`: the 60 s deadline is reset only by a delivered data frame, and a halted exchange sends none. Before the halt the exchange-wide `trade` subscription kept it fed through every quiet minute of this book. Filed as **lip-6dn** (P3); see Finding 1. No SEV1.
   - **Observer gap.** The first observer reached its 150 min limit at 05:41:11Z; Hugh's SIGTERM yes came at 05:58Z and a fresh observer started at 05:59:31Z, so `observer.jsonl` has an 18 min gap in which the store records no event. The ntfy stream ran throughout.
10. **Flatten, exit and final read.**
    - The reducer filled its remaining 8.83 @ 30c, maker, fee $0.000000, trade `07232925-38f9-b3b2-255b-2bf1730c39a5` on `…65ed` (`taker_side=no`, `taker_book_side=ask`): exchange 10:49:33.514Z, seen 10:49:34.845, 1.3 s later. It was the first YES seller to reach the 30c bid since 03:52Z, 7 h 18 min after the position opened.
    - WINDING_DOWN→DRAINED (`drained`) at 10:49:34.846 and REDUCING→IDLE (`global_stop`) at 10:49:34.847. The process exited by itself: `exit-status.txt` = **0**, no second signal. The observer saw it absent at 10:49:39Z and stopped at 10:49:49Z.
    - Final account-wide read 10:50:00.5–02.7Z (`account-after-restart.json`): complete and flat, zero open orders, no nonzero position, funding $98.7879; all three fills present.
    - `live_ok` removed at 13:11:32.183Z (`drill.py rmlive`, Hugh's yes at 13:11Z), after it re-checked that no stage process was alive and that `account-after-restart.json` was complete and flat. The latch (`{"trigger":"sigterm"}`), store, WAL and journal are retained; no stage process remains.

**Net P&L: +$0.2400, no fees.** Bought NO 12 @ 68c, then YES 12 @ 30c in two maker fills (3.17 + 8.83): $11.76 for 12 pairs worth $12.00. Funding went $98.5479 → $90.3879 (after the NO fill) → $92.6069 (3.17 pairs netted) → $98.7879, each step exact to the cent.

## lip-14o checks (handoff, "What this stage must show")

Outputs are in `lip-14o-checks.txt` (appended by `loop/run/r2-candidate-7-scratch/checks.sh` after each episode).

- **(a) Every `CANCEL_UNVERIFIED` is followed within seconds by a sweep trace naming the same order that ends clean — PASS, one episode.** `01a0f570-f1d8-7a08-901a-41219019b3ed cleared after 219 ms`; no `NOT CLEARED`; the single `CANCEL_UNVERIFIED` row (a63, 03:30:55.894) falls inside the episode its trace opened at 55.893. Scope: the episode was on the side being turned off after its fill, with DELETE 404 rather than 200. No reducer on a wanted side was ever requoted, so the branch candidate-7 added, the per-side `cancelUnverified` re-sweep of a side the state still wants, was not reached live. It remains covered by the Go tests named in the candidate review.
- **(b) While q ≠ 0 in REDUCING, the time with no resting reducer never exceeds one `position_poll_s` plus one sweep — NOT EXERCISED.** No reducer was cancelled while q ≠ 0: the (b) program found no DELETE 200. The complementary evidence is positive: the YES reducer rested without a break from 03:10:15 through every spot-check read (one open order on the reducing side each time), and the defect's signature, zero open orders while q ≠ 0, never appeared.
- **(c) No `SWEEP_INCOMPLETE` SEV1 from a list lag — PASS.** `an "sev=2"` returns no rows over the whole run (0 of 417 anomaly rows); `"page":"sev1"` occurs 0 times in `harness.log`. The one list-lag episode ended at `"page":"pending"` and cleared in 219 ms.
- **(d) A planned SIGTERM drain while holding inventory places the exit and completes once flat — PASS.** `phantoms.py` printed `{}` before the signal. `SIGNAL_DRAIN` (a65) carried `inventory=true` and `live orders=true`; the post-signal read showed one open order on the reducing side; no `DRAIN_TIMEOUT` in 4 h 51 min of WINDING_DOWN; REDUCING→IDLE and WINDING_DOWN→DRAINED at 10:49:34; `exit-status.txt` = 0 with no second signal; the final read complete and flat. The exit was the add-turned-reducer that had rested since 03:10:15; no replacement order was ever needed, so the drain's "places the exit" branch was satisfied by the order already resting.

## F5 cross-checks
- **Every cross-check that ran agreed.** 18 `BOOK_QUIET` (SEV2) and 15 `BOOK_CROSSCHECK_AGREED` (SEV3); the three quiet rows without a paired agreement fell in the halt, when the book was non-actionable between reconnects. 0 `BOOK_CROSSCHECK_MISMATCH`, 0 `QUOTING_STOPPED_UNTIL_RESTART`.

## Alarms and push receipt (lip-6b1)
- **SEV1: none.** 0 of 417 anomaly rows have `sev=2`.
- **Anomaly rows by class:** 119 `PNL_MARK_UNAVAILABLE`, 117 `WS_DISCONNECT`, 113 `RECONCILE_TOKEN_STALE` (all three almost entirely from the 07:00–09:00Z halt), 28 `FOREIGN_FILL_INHERITED`, 18 `BOOK_QUIET`, 15 `BOOK_CROSSCHECK_AGREED`, and one each of `STARTUP`, `FUNDING_LIMITS`, `FUNDING_EXACT`, `SWEEP_PENDING`, `CANCEL_UNVERIFIED`, `INVENTORY_STUCK`, `SIGNAL_DRAIN`.
- **ntfy deliveries:** 49 pushes with distinct message ids reached the topic (`ntfy-delivery.jsonl`, 663 raw lines, streamed from launch to 10:50Z); the budget held the SEV2 flood far below the 349 SEV2 rows in the store.
- **Phone receipt times:** none recorded, and with no SEV1 there was none to record. The assistant sent Hugh three terminal/mobile pushes of its own (INVENTORY_STUCK, the stalled drain, the exchange halt); receipt was not timed.

## Findings
1. **The F1 read-deadline backstop reconnects a healthy socket every 61 s through Kalshi's scheduled halt (bead lip-6dn, P3).** Kalshi closes trading every Thursday 03:00–05:00 ET. With no trades and no book deltas anywhere, no data frame reaches the socket, pongs are still answered, and `wsx/session.go` returns "no data frame within read_deadline_s 60s", so the supervisor reconnects, the snapshot resets the timer, and the cycle repeats: about two SEV2 rows a minute into the store, `anomaly.jsonl`, ntfy and the observer, every book non-actionable most of each minute, and portfolio reads that straddle a reconnect thrown away. Not a safety defect: the exchange keeps the resting order and REST polling continues. Nothing in `go/harness` reads `/exchange/status` or the schedule. Fix direction and acceptance are in the bead. Operationally: do not launch a stage that will drain into 07:00–09:00Z on a Thursday, and name the window in the runbook.
2. **Market choice decides what lip-14o evidence a stage can give.** The defect needs a reducer requote, which needs the touch to move against our bid while we hold inventory. The handoff's primary criterion (two-sided taker flow) ranked MCI first, but its book moved 4c in 3 h at scope time and did not move above our bid in four hours live. The assistant's recommended alternative, KXRAIN-26OCT01-NOLA, moved 16c in 3 h; Hugh chose MCI for the bounded risk. The trade-off was explicit in `choice.json` and it played out as written: (a) passed on a turned-off-side episode and (b) was not exercised.
3. **One round trip in 7 h 39 min.** The join-the-touch queue at launch (about 100 contracts ahead) and overnight one-sided flow (buyers only from 03:52Z until the exit fill at 10:49Z) left the reducer waiting 7 h 18 min; the level behind it grew past 2,300 contracts. The 3-round-trip target was never reachable at this flow, and the exchange halt removed two hours. The round trip completed only because drill C kept the maker reducer resting through the drain; a crash drill at the cap would have ended the stage with inventory held.
4. **Fill detection lag** was 0.7 s, 4.1 s and 1.3 s over the three fills.
5. **accountcheck cannot report a held position (lip-006, known).** Every read with inventory held gave `account_scope_complete=false` and an empty `nonzero_positions`; only the open-orders list was used, as the handoff says.
6. **The observer's 150 min limit lapsed twice.** The first observer stopped at 05:41:11Z and was restarted at 05:59:31Z (18 min); the second stopped at 08:29:37Z and was not restarted until 10:06:34Z (96 min), because the assistant's notices watch was tailing a file nothing was writing. The store shows no fill, state change, sweep or SEV1 in either gap, and status reads at 09:01, 09:05, 09:31 and 10:05Z bracket the second. Remedies applied late and worth making standard: run `observe_stage.py` with `--max-minutes` sized to the expected drain (it was restarted with 720), and watch the store and process directly rather than the observer's notices, so a stopped observer is noticed rather than mistaken for quiet.

## Not demonstrated
- **At least 3 round trips.** 1 completed, in 7 h 18 min, and only inside the drain; 0 completed before the 2 h cap at 05:10Z. Re-adding after flat was not observed, because WINDING_DOWN stops adding.
- **A crash while holding inventory.** Drill C has no crash, by Hugh's choice.
- **A planned drain with a phantom present.** No phantom arose; `phantoms.py` was empty at every read.
- **The lip-14o wanted-side re-sweep.** No reducer requote occurred, so the fix's own branch was not reached live; it rests on `TestPlannedDrainCompletesThroughAPhantomReducer` and its companions.
- **SEV1 receipt timing.** No SEV1 occurred.
- **Beyond this stage's scope:** unattended operation (lip-8hn.4), CR-2 turnover, and LIP payout or profitability.
