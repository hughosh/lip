# lip-dwf attended first-fill stage — 2026-09-29, candidate-5, KXEPLRELEGATION-27-MCI, S=12

Hugh attended throughout. He approved the launch, both SIGTERMs, the restart and the disarm one at a time in chat. The assistant executed each only after that approval; see `control.log`. Procedure: [operator-handoff-candidate-5.md](../../../operator-handoff-candidate-5.md).

## Identity and inputs
- **Binary:** `516778b6…17c0`. Source manifest `90114bfe…39c5`, receipt `candidate-5/build-identity.json`, config sha256 `251151dd…2d55` (`heartbeat_s` 3600, rung `sizing`, capital `selected_shard_balance`).
- **Market:** Hugh chose it from a GET-only scope run 20:38–20:42Z ([scope-candidate-5/choice.json](../../../scope-candidate-5/choice.json)): book YES 33 / NO 65, estimated join-the-touch waits YES about 6.6 min and NO about 2.2 min, fees `quadratic`.
- **Preparation:** run without `--book` (lip-e2t); 9.4 s end to end (`prep-timing.json`):
  - tools built 20:55:26.5–28.3;
  - book 20:55:28.3–29.6;
  - programs, candidate and account reads to 20:55:35.7.
  - The book passed H-SEL-6/7 (lip-t9p).
  - Launch at 20:55:36Z, 52.5 s before `launch_deadline_utc` (the book was about 8 s old; on 09-29 it was 57 s).
- **Preflight account:** complete and flat at 20:55:33Z. Cash $98.4279, deployable $73.82, worst entry $11.88.
- **Run 1:** `20260929T2055382WVM5`, PID 77826, 20:55:36–21:20:49Z.
- **Run 2 (retained-latch restart):** `20260929T2123062KJE2`, PID 78974, 21:23:04–21:23:40Z.

## Observed
1. **Startup:** STARTING→RUNNING (`reconciled`) 20:55:38.938, then IDLE→QUOTING (`selected`) 20:55:39.234.
2. **Orders placed, all post-only adds:**
   - YES 12 @ 33c `01a0eef3-a4e0-7c89-9951-8c62edc2e9ea` (20:55:40.071);
   - NO 12 @ 65c `01a0eef3-a4e0-7071-b6bc-a761a3865547` (20:55:40.183);
   - after a reprice, YES 12 @ 34c `01a0eef3-c808-7514-a313-f5e767b58996` (20:55:49.291).
3. **Reprice cancel under candidate-5's deferred page (lip-9tt), in production.**
   - The first sweep, DELETE `…e9ea` at 20:55:47.02, came back 200 `reduced_by 12`. The list and named read still showed it resting, so the verdict was `incomplete` with `page: pending`. That journalled SEV3 `SWEEP_PENDING` (a25) and a non-paging SEV2 `CANCEL_UNVERIFIED` (a26).
   - The next sweep, 20:55:48.57, returned DELETE 404 with the order absent: `clean`, 1.5 s after the first. No SEV1 paged. On candidate-4 this same lag paged false SEV1s (a19, a21, a27 on 09-29).
4. **First owned fill:** NO 12 @ 65c, maker (`is_taker=false`), fee $0.000000, trade `07235516-1181-98b3-3f83-d07dc114950f` on order `…5547`.
   - Exchange `created_time` 21:01:59.623Z; harness `first_seen` 21:02:04.206Z (4.6 s detection lag).
   - Exchange record: `side=no action=sell book_side=ask`. `book_side` is authoritative (lip-3ot); it is the YES ask at 35c.
5. **Durable first-fill stop:**
   - Latch `canary_owned_fill`, ts_ms 1790715724206 (`latch-before-restart.json`).
   - RUNNING→WINDING_DOWN (`global_stop`) and QUOTING→REDUCING at 21:02:04.242.
6. **Reduction by the harness, with no operator action:**
   - With q = NO 12, the reducing side is YES. The YES 12 @ 34c order already resting at the touch became the reducer (`cmd/harness/run.go:2551`). Its aggregate equalled |q|, so it was neither cancelled nor resized.
   - The NO adding side had filled completely, so there was no adding order left to cancel after the fill. The post-fill cancel path was therefore not exercised in this run; the reprice cancel in item 3 was.
   - `owned_order.role` still reads `adding` because the role is recorded when the order is reserved.
7. **Reducer fill:** YES 12 @ 34c, maker, fee $0.000000, trade `07235506-1a69-9eae-17dd-c4129d0f0ac6` on order `…8996`.
   - Exchange 21:06:23.663Z; harness `first_seen` 21:06:24.206Z.
   - About 4.4 min after the first fill.
8. **Drain:**
   - REDUCING→IDLE at 21:06:24.225 and WINDING_DOWN→DRAINED at 21:06:24.838.
   - Confirm sweep 21:06:24.23: DELETE 404, 0 listed, `clean`.
9. **First exit:** SIGTERM at 21:20:49Z (argv and start time checked first). SEV2 `SIGNAL_DRAIN` with inventory=false, live orders=false. Exit status **0** (`exit-status.txt`).
10. **Post-exit account-wide read**, 21:21:00Z: complete and flat, zero resting orders, no nonzero positions (`account-after-drain.json`). Both fills are present with order_id, side, action and book_side.
11. **Pre-restart reads**, 21:21–21:22Z:
    - latch;
    - program (live);
    - book, series and fees (`quadratic`, market active);
    - a fresh account read at 21:21:33Z, complete and flat, funding $98.5479 (`account-before-restart.json`).
12. **Retained-latch restart** with `-resume 'first-owned-fill stop validation'`:
    - STARTING→WINDING_DOWN (`halt_latch`) 21:23:06.770, then →DRAINED 21:23:07.467.
    - No orders adopted or placed, no SEV1.
    - SIGTERM at 21:23:40Z, exit status **0**.
13. **Post-restart account-wide read**, 21:23:45–47Z: complete and flat, funding $98.5479 (`account-after-restart.json`). `live_ok` removed at 21:25:03Z. The latch, store and journal are retained.

**Net P&L: +$0.1200.**
- Sold YES 12 @ 35c (NO bought @ 65c) and bought YES 12 @ 34c, both as maker with no fees.
- Funding went from $98.4279 to $98.5479.

## Alarms and push receipt (lip-6b1)
- **SEV1 in either run: none.** `jq 'select(.sev==2)'` over `runtime/anomaly.jsonl` is empty.
- **ntfy deliveries:** each push's server delivery time is in `ntfy-delivery.json` (20 pushes, deduplicated by message id; `ntfy-delivery.jsonl` is the raw stream, which replayed once on reconnect). All were priority 3: two STARTING heartbeats and 18 SEV2s. There were no 30 s heartbeats (lip-1gb verified).
- **Push backlog:** the restart's STARTING push reported `undelivered: 10`. SEV2 a36 (21:19:57Z) was delivered only at 21:23:06Z, when run 2 flushed the backlog. SEV2 pushes from run 1 were therefore budgeted or queued rather than sent in real time. Nothing was lost.
- **Phone receipt times:** none recorded. With no SEV1 there was no SEV1 receipt to record.

## Findings
- **F5 float residue persists on candidate-5.** This is a new bead; lip-2w3's fix is incomplete.
  - At 21:08:27Z, BOOK_QUIET led to a REST cross-check, which raised `BOOK_CROSSCHECK_MISMATCH` (a29): YES depth 6, 28c is `29.999999999999986` on the websocket and `30` on REST. That is 4 ULPs (measured with `math.nextafter`).
  - The mismatch then raised sticky `QUOTING_STOPPED_UNTIL_RESTART` (a30).
  - At 21:19:57Z a second mismatch (a36) showed NO 64c at `90.85000000000002` against `90.85`, which is 2 ULPs. `sameBookSize` (`go/harness/rest/orderbook.go:483`) accepts only a single step, so both mismatches come from the same path.
  - There was no exposure here, because the harness was already DRAINED and flat. On the pilot rung this would stop adding after the first quiet-book cross-check.
- **Fill detection lag of 4.6 s on the first fill** (0.5 s on the second). The adding YES order stayed live through that window; it was on the reducing side, so no harm.

## Not demonstrated
- **The post-first-fill cancellation of a remaining adding order.** The only other order became the reducer; the 09-29 candidate-4 stage exercised that path.
- **SEV1 receipt timing.** No SEV1 occurred.
- **R2, repeated cycles, turnover or profitability.** Nothing here covers them.
