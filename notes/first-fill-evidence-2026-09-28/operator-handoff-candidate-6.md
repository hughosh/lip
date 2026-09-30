# Operator handoff: candidate-6 R2 repeated-cycle stage (lip-8hn.2)

This is the procedure for the next attended stage. It follows [the candidate-5 handoff](operator-handoff-candidate-5.md) and its "As executed" pattern except where this page differs. It is preparation only: nothing here has run, and every live step needs Hugh's explicit yes, one step at a time.

Never restart the 2026-09-28 BROSFT, 2026-09-29 KXAAAGASW or 2026-09-29 MCI runtimes. Their latches are retained, and this stage only reads the MCI stage's files.

- **Candidate:** `candidate-6/harness`.
  - Binary sha256 `dbc250d62521234b4d705a29ac6382503106b0107bf8e31f4a6341cf63f06825`; Go manifest `ee8f4ec1…0231`.
  - Receipt: [candidate-6/build-identity.json](candidate-6/build-identity.json).
  - Decision: [candidate-6/candidate-review.md](candidate-6/candidate-review.md). It carries the lip-2mz F5 repair.
- **Rung:** `pilot`. S <= 12, and there is no first-fill stop, so the harness quotes again after each return to flat.
- **Size:** balance-derived. Use S = 12 if the preflight funding arithmetic admits it. At the MCI exit, cash was $98.5479.
- **Store:** fresh, provisioned in this stage's own directory.
- **First-fill binding:** the candidate-5 MCI first stage.
  - Candidate-5 is candidate-6's recorded predecessor (`build-identity.json` `predecessor`). The two differ only in `go/harness/rest/orderbook.go` and its test.
  - `operator_stage.py --stage r2` checks that one-hop binding.
  - It also checks Hugh's attested 7-field receipt, `…-MCI-…/evidence/r2-receipt.json`. The helper verifies that receipt's file hashes, not the events it describes.
- **Decisions (Hugh, 2026-09-30):**
  - lip-9vc covers attended operation only; alarm receipt is recorded when possible. The unattended items are in lip-8hn.4.
  - One stage with a fresh store, not a candidate-6 first stage followed by R2.
  - The stage ends with a crash-restart.

## What only this stage can prove

1. **Round trips.** At least 3 round trips in one pilot-rung process. A round trip is flat → owned fill → the harness reduces to exactly flat as a maker. Each needs fill attribution by `order_id`/`book_side` and its actual fee.
2. **Re-adding.** Adding resumes after each return to flat: REDUCING → QUOTING.
3. **Crash-restart with live orders.** A SIGKILL writes no latch, so the restart runs without `-resume`. Expected sequence:
   - adoption of the resting orders;
   - FUNDING_LIMITS with `recovery_only=true`;
   - a `funding_recovery` latch, then WINDING_DOWN;
   - the adds cancelled, any inventory reduced to flat;
   - DRAINED, with no new adds.
   - See the runbook's "Restart after a crash".
4. **F5 on candidate-6 in production.** A BOOK_QUIET cross-check should agree where candidate-5 raised a29/a36. A disagreement names a real price and depth.
5. **Clean exits.** Each process exits cleanly and is followed by a complete account-wide flat read.

Not proven by this stage:
- unattended operation (lip-8hn.4);
- SEV1 receipt timing (lip-6b1, opportunistic);
- unreadable-store recovery with real exposure;
- CR-2 turnover;
- LIP payout or profitability.

## 1. Before the stage (assistant, no account writes)

1. **Receipt.** `r2-receipt.json` must exist in the MCI evidence directory. It is Hugh's attestation. The assistant drafts it from `evidence/stage-report.md` and writes it only after Hugh says yes.
2. **Market scope** (GET-only), using the candidate-5 method in [scope-candidate-5/](scope-candidate-5/): `scope_markets.py`, then `fill_odds.py` over the top 40 by 24 h volume. Write it to a new `scope-candidate-6/` directory. Requirements:
   - an active liquidity programme with at least 4 h left, and close at least 24 h away;
   - H-SEL-6/7 with the 15-85c margin;
   - series `fee_type` = `quadratic`.
3. **Ranking.** Rank by the worse side's join-the-touch wait. Repeated cycles need flow on both sides, repeatedly, so prefer markets where both sides' waits are short. Hugh chooses the market.
4. **Host.** The iMac must stay awake: the launch wrapper runs `caffeinate`. A DNS wedge (about every 2.5 h) is handled by the harness (F6); watch for its SEV2.

## 2. Preparation and launch (one chained command, Hugh's yes)

Preparation and launch must fit the 60 s freshness window, so they run as one chained command after Hugh's yes. The assistant executes it; Hugh approves it.

```sh
cd /Users/hugh/kek/lip
/Users/hugh/kek/.venv/bin/python scripts/operator_stage.py --stage r2 \
  --ticker "$TICKER" --size 12 \
  --binary notes/first-fill-evidence-2026-09-28/candidate-6/harness \
  --candidate-receipt notes/first-fill-evidence-2026-09-28/candidate-6/build-identity.json \
  --evidence-root notes/first-fill-evidence-2026-09-28/operator-stages \
  --prior-config /Users/hugh/kek/lip/notes/first-fill-evidence-2026-09-28/operator-stages/first-KXEPLRELEGATION-27-MCI-20260929T205526.227001Z/config.json \
  --r2-receipt /Users/hugh/kek/lip/notes/first-fill-evidence-2026-09-28/operator-stages/first-KXEPLRELEGATION-27-MCI-20260929T205526.227001Z/evidence/r2-receipt.json
```

- **`--prior-config`** must be exactly the absolute path recorded in the MCI `identity.json`: it is compared as a string.
- **The chain continues** only if at least 5 s remain before `launch_deadline_utc` (`evidence/prep-timing.json`). It then runs `-provision`, `install -m 600 /dev/null <live_ok>`, and the detached launch below.
- **Printed phases.** The script prints this stage's 10 paste-safe phases, from provision through the removal of `live_ok`. They are the authoritative order. The assistant executes each one through the wrapper below, only after Hugh's yes for that step.

**Detached launch wrapper.** It survives the tool shell, holds the Mac awake and records the exit code. `PREFIX` is `harness` for the first run and `restart` for the restart. `EXIT` is `exit-status.txt` for the first run and `restart-exit-status.txt` for the restart.

```sh
/Users/hugh/kek/.venv/bin/python - "$EVID" "$EXE" "$CFG" "$PREFIX" "$EVID/$EXIT" <<'PY'
import subprocess, sys
evid, exe, cfg, prefix, status = sys.argv[1:6]
script = ('cd "$1" || exit 97; "$2" -config "$3" -rung pilot -live > "$4.log" 2>&1 & pid=$!; '
          'echo "$pid" > "$4.pid"; /usr/bin/caffeinate -i -w "$pid" & wait "$pid"; echo "$?" > "$5"')
with open(f"{evid}/{prefix}-wrapper.log", "ab") as log:
    subprocess.Popen(["/bin/bash", "-c", script, "wrapper", evid, exe, cfg, prefix, status],
                     start_new_session=True, stdin=subprocess.DEVNULL, stdout=log, stderr=subprocess.STDOUT)
PY
```

**While it runs:**
- **Observer.** Run `observe_stage.py --stage <stage dir> --pid <pid> --max-minutes 150` under a Monitor.
  - It follows one PID and exits two samples after that PID is gone, so the restart needs a new observer on `restart.pid`.
- **ntfy delivery times.** Stream `https://ntfy.sh/$NTFY_TOPIC/json?since=<unix>` read-only into `evidence/ntfy-delivery.jsonl`, then deduplicate by `id` into `ntfy-delivery.json`.
  - Read the topic from `~/.kalshi/env` and never print it.
- **Phone receipt.** For each SEV1, Hugh gives the phone-seen time if he can. It is not a gate.
- **Signal checks.** Before every signal, `ps -ww -o command= -p <pid>` must match the exact expected argv and start time. Log each control action in `evidence/control.log`.

## 3. Cycles, then the crash drill

- **Target:** at least 3 round trips, with a 2 h cap from launch. The pilot rung never stops by itself.
- **Timing:** start the drill only when the market is QUOTING, flat, with its adding orders resting (observer state and store), and when no create, cancel or reprice has happened in the last few seconds. Never start it during a reduction. A kill between reservation and acknowledgement leaves an unbound reservation, which the next start retries rather than latches.

The drill, one yes per step:
1. `kill -KILL <harness pid>`. The wrapper records `exit-status.txt`; expect 137.
2. Take the account read at once with the prebuilt tool (printed phase 5) into `evidence/account-after-crash.json`. It should show the resting orders the restart must adopt. Cancel nothing by hand unless the restart fails.
3. Restart with the wrapper (`PREFIX=restart`), **without** `-resume`, since no latch exists. Start a new observer on `restart.pid`.
4. Expect the item-3 sequence from "What only this stage can prove". A latched process idles in DRAINED until signalled.
5. `kill -TERM <restart pid>`. Expect `restart-exit-status.txt` = 0.
6. Take the complete account-wide read `account-after-restart.json`. Require `account_scope_complete` and `flat` both true.
7. Remove only this stage's `live_ok`. Retain the store, WAL, journal, latch and every failure.

## 4. Stop conditions and fallbacks

- **Cap reached with fewer than 3 round trips:** do the crash drill anyway if orders are resting. The report must say how many round trips were observed.
- **A global stop latched during the cycles** (inv_kill, pnl_kill, disk, store, foreign order, sweep, and so on):
  - The process is WINDING_DOWN and reduces by itself, so skip the crash drill.
  - Once it is DRAINED: SIGTERM (exit 0), a flat read, a `-resume` retained-latch restart to DRAINED, SIGTERM, then another flat read. This is the MCI pattern.
- **`QUOTING_STOPPED_UNTIL_RESTART` (sticky REDUCING):** record its cause; an F5 disagreement is candidate-6 evidence in its own right. Then use the same planned-stop path, without the crash drill.
- **A reducer resting unfilled for 30 min, or INVENTORY_STUCK:** Hugh may close by hand (runbook, "Stuck reducer and operator manual close").
  - After a manual close, SEV1 `FOREIGN_FILL` and `POSITION_DRIFT` are the correct reaction. Save `evidence/operator-manual-close.json`.
- **The restart refuses, or does not adopt:** cancel every resting order and close any position by hand. Then follow the runbook's unreadable-store path.
  - Never provision over this store or clear its latch.
- **Any account read incomplete or not flat at the end:** the stage stays attended until a complete read shows flat.

## 5. After the stage

- **Stage report:** `evidence/stage-report.md`, in the MCI report's format. It covers identity and inputs; each round trip with its order ids, trade ids, fees and timings; the crash drill's adoption and funding rows; F5 cross-checks; and every SEV1 and SEV2 with its disposition.
- **Beads:** a comment on lip-8hn.2 linking the report and stating what was and was not observed. lip-8hn.2 closes only on Hugh's review.
- **Latch:** clearing this stage's latch for any later run is a separate §10.4 decision.

Nothing here grants unattended operation, CR-2, or any assistant authority to trade.
