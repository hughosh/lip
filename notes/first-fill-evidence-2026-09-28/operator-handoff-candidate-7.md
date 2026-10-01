# Operator handoff: candidate-7 attended stage (lip-14o, lip-8hn.2)

## Status

- **Built:** candidate-7 is built from the lip-14o fix. Its receipts live in `notes/first-fill-evidence-2026-09-28/candidate-7/`: [build-identity.json](candidate-7/build-identity.json) and [candidate-review.md](candidate-7/candidate-review.md).
- **Not run:** the stage has NOT been run. This is a plan, not an authorization: every live step needs Hugh's explicit yes, one step at a time.
- **Binding (resolved 2026-10-01 UTC):** `operator_stage.py --stage r2` now binds through the recorded predecessor chain (candidate-7 → candidate-6 → candidate-5), so the MCI first stage, which candidate-5 ran, is the prior stage; section 1 step 2 has the paths. The review's adversarial-review findings are adopted in this build; the candidate gate and the affected-mutation receipts are recorded in the review when they land.

It follows [the candidate-6 handoff](operator-handoff-candidate-6.md) and its "As executed on 2026-09-30" pattern, with the same section numbers, except where this page differs. Never restart the BROSFT, KXAAAGASW, MCI or EFRE runtimes: their latches are retained.

- **Candidate:** `notes/first-fill-evidence-2026-09-28/candidate-7/harness`; binary sha256 `f701130c29630e12a7b4809304a68363cb36489da4932efe5f51179924781eb7`; Go manifest `af0ad4806d270a89ecd1ff53edac97e4f1a3ac37711eb96c28a36a4440a54436` (built 2026-10-01T01:18:33Z; `operator_stage.py` checks both against the receipt). Predecessor: candidate-6.
- **Rung `pilot`, fresh store, S <= 12,** no first-fill stop. S = 12 if the preflight funding arithmetic admits it (cash was $98.5479 at the EFRE exit). Attended operation only (lip-9vc; unattended items: lip-8hn.4).

## What this stage must show for lip-14o

Finding 1 of the [09-30 stage report](operator-stages/r2-KXDANCINGWITHTHESTARS-26DEC31-EFRE-20260930T205542.400043Z/evidence/stage-report.md): a reducer's cancel came back unverified inside Kalshi's ~1.5 s list lag, and no reducer rested for 20 min 48 s with q = -12 and no alarm. Candidate-7 re-sweeps a wanted side holding an unverified cancel on every tick (per-side `cancelUnverified` latch, as the cancel leg of a cancel-confirm-place) until a complete read confirms it absent; nothing is retired by absence or time. `CANCEL_UNVERIFIED` (SEV2) fires once per episode, `SWEEP_PENDING` (SEV3) still journals each sweep, and `SWEEP_INCOMPLETE` (SEV1) still pages after 5 s unconfirmed.

Each check ends PASS, FAIL or NOT EXERCISED: (a) is NOT EXERCISED if no cancel ever comes back unverified, and (b) if no reducer is ever cancelled while q != 0. Save the outputs in `evidence/lip-14o-checks.txt`.

Sweep traces are the log lines `harness: sweep-trace {json}` (`go/harness/rest/sweep_trace.go`). Fields used: `requested`, `ended`, `rounds[].deletes[]` (`order_id`, `status`), `verdict`, `still_resting`, `page`. `"verdict":"clean"` or `"verdict":"confirmed"` means every requested order is established absent. `"verdict":"incomplete"` means a complete read still listed one: it is in `still_resting`, with `"page":"pending"` or `"page":"sev1"`.

```sh
STAGE=<stage directory printed by launch.sh>; EVID=$STAGE/evidence; U="file:$STAGE/runtime/harness.db?mode=ro"
TS='def ts: (.[:19]+"Z"|fromdateiso8601)+(("0"+.[19:-1])|tonumber);'
traces() { sed -n 's/^harness: sweep-trace //p' "$EVID"/harness.log "$EVID"/restart.log 2>/dev/null; }
an() { sqlite3 "$U" "SELECT strftime('%H:%M:%f',first_ms/1000.0,'unixepoch'),class,sev,substr(text,1,110) FROM anomaly WHERE $1 ORDER BY first_ms"; }
st() { sqlite3 "$U" "SELECT strftime('%H:%M:%f',ts_ms/1000.0,'unixepoch'),scope,from_state||'>'||to_state,\"trigger\" FROM state_event ORDER BY rowid"; }
```

Use `?mode=ro` while the store has its `-wal` and `-shm` (Finding 3); `?immutable=1` after a clean exit removed them.

**(a) Every `CANCEL_UNVERIFIED` is followed within seconds by a sweep trace naming the same order that ends clean.**

```sh
an "class IN ('CANCEL_UNVERIFIED','SWEEP_PENDING','SWEEP_INCOMPLETE')"
traces | jq -nr "$TS"'[inputs|select(.verdict|IN("clean","confirmed","incomplete"))]
  | reduce .[] as $t ({o:{},r:[]}; reduce $t.requested[] as $i (.;
      if ($t.still_resting//[]|index($i)) then .o[$i] //= ($t.ended|ts)
      elif .o[$i] then .r += ["\($i) cleared after \((($t.ended|ts)-.o[$i])*1000|round) ms"] | del(.o[$i])
      else . end))
  | .r[], (.o|keys[]|"\(.) NOT CLEARED")'
```

- An episode runs from a trace listing an order in `still_resting` to the first later trace naming it in `requested` without listing it. Either side counts.
- **PASS:** every line reads `cleared after N ms`, N < 5000 (the page bound; the review expects one or two sweeps over a 1.1-1.8 s lag); no `NOT CLEARED`; each `CANCEL_UNVERIFIED` row falls within about a second after an episode opened (its text names market and side, not the order, so pair by time). A row with no episode before it (a `"verdict":"unverified"` trace, or `other_ours` on that side) is not a pass: read its trace and report it. On candidate-6's logs the program prints `cleared after 1247039 ms` for the phantom `01a0f435-9760-7e56-b6b5-dde5b060e8ab`.

**(b) While q != 0 in REDUCING, the time with no resting reducer never exceeds one `position_poll_s` (5 s) plus one sweep.** Run it with the reducing side (`yes` when q < 0, `no` when q > 0).

```sh
traces | jq -nr --arg side yes --slurpfile o <(sqlite3 -json "$U" "SELECT order_id,side,bound_ms FROM owned_order WHERE bound_ms IS NOT NULL") "$TS"'
  [inputs|.rounds[].deletes[]|select(.status==200)|{id:.order_id,t:(.ended|ts)}][] as $d
  | ($o[0][]|select(.order_id==$d.id and .side==$side)) as $x
  | ([$o[0][]|select(.side==$side and .bound_ms/1000>$d.t)|.bound_ms/1000]|min) as $n
  | "\($d.id) cancelled \($d.t|todate); next \($side) order bound " + (if $n then "\((($n-$d.t)*1000)|round) ms later" else "never" end)'
st
```

- The clock runs from the DELETE's 200 (the order stopped resting) to the replacement's `bound_ms`. Keep lines inside a REDUCING window from `st`; a gap that runs past `REDUCING>IDLE`, or `never`, is the market going flat, not a missing reducer.
- **PASS:** every kept gap <= 5 s plus one sweep (about 0.5 s in candidate-6's traces; use the confirming trace's `ended` minus `started` if close). Candidate-6's failing gap was 20 min 48 s.
- Spot-check once per reduction with `drill.py read NAME`: one open order on the reducing side (it reads `account_scope_complete=false` while inventory is held, Finding 2; use its open orders only). Zero open orders while q != 0 is the defect's signature.

**(c) No `SWEEP_INCOMPLETE` SEV1 from a list lag.**

```sh
an "sev=2"
grep -c '"page":"sev1"' "$EVID"/harness.log "$EVID"/restart.log
```

- **PASS:** no rows, and `0` per file (SEV1 is `sev=2`). A SEV1 stops the cycles for Hugh; `"page":"sev1"` means about 5 s of real non-confirmation, not lag to dismiss.

**(d) A planned SIGTERM drain while holding inventory places the exit and completes once flat.** It needs q != 0 at the signal (section 3, B or C), with `phantoms.py` printing `"phantom_candidates": {}` first.

```sh
an "class IN ('SIGNAL_DRAIN','DRAIN_TIMEOUT')"; st
cat "$EVID"/exit-status.txt "$EVID"/restart-exit-status.txt
jq '{account_scope_complete,flat}' "$EVID"/account-after-restart.json
```

- **PASS:** `SIGNAL_DRAIN` with `inventory=true`; one open order on the reducing side after the signal (spot-check read); no `DRAIN_TIMEOUT`; market `REDUCING>IDLE` and the global scope in `WINDING_DOWN` (a `>DRAINED` row may or may not precede the exit); the process exits by itself with status `0`, no second signal; the final read complete and flat.

## Not demonstrated by candidate-6 that this stage should try to cover

From the 09-30 report. None can be forced: each step is what to do if the condition arises.

1. **At least 3 round trips** (candidate-6: 1, in 99 min). Step: pick the market by two-sided flow (section 1, step 3) and cycle to 3 or the cap, reporting the count either way; only Hugh extends the cap. **Market-dependent:** the operator cannot cause a fill.
2. **A crash while holding inventory** (09-30 crashed flat). Step: variant B (section 3). **Market-dependent:** it needs a fill and a REDUCING market near the cap. `gate-reducing` and `crash-reducing` were never used live: read the first's output before the second.
3. **A planned drain with a phantom present** (candidate-6: code only). A phantom now lasts one lag episode, 1-2 s, shorter than any per-step approval, so it cannot be arranged by hand. Step: the (d) drain in B or C, with `phantoms.py` before and the (a) program after, to see whether an episode overlapped. **Market- and chance-dependent;** expect it to stay covered only by `TestPlannedDrainCompletesThroughAPhantomReducer` (`go/cmd/harness/phantom_reducer_test.go`).
4. **SEV1 receipt timing** (lip-6b1; no SEV1 occurred). Step: none provokes one; if one fires, record its anomaly id, ntfy delivery time and when Hugh saw it on the phone. **Event-dependent;** not a gate, and a SEV1 here also fails (c).

## 1. Before the stage (assistant, no account writes)

1. **Receipts.** Both candidate-7 receipts exist, the review's pending items are filled in, and the real hashes replace the placeholders (`operator_stage.py` checks them against the receipt).
2. **Binding.** `operator_stage.py --stage r2` runs `load_prior_stage`: the prior stage must be `stage: "first"`, `rung: "sizing"`, run by candidate-7 or by a receipt in the predecessor chain its build receipt records (`receipt_predecessors`: candidate-7 → candidate-6 → candidate-5, each hop past the first attested by that predecessor's own `build-identity.json`). Candidate-6 ran no first stage; the MCI first stage ran candidate-5 (`90114bfe…`/`516778b6…`), so it binds at `predecessor_depth` 2. That was a one-hop rule until 2026-10-01 and would have refused this; the change is in the candidate-7 review under non-Go changes, and Hugh can tighten it. The arguments: `--prior-config /Users/hugh/kek/lip/notes/first-fill-evidence-2026-09-28/operator-stages/first-KXEPLRELEGATION-27-MCI-20260929T205526.227001Z/config.json` (the path that stage's `identity.json` records as `config_path`; the script compares them, so pass it exactly) and `--r2-receipt notes/first-fill-evidence-2026-09-28/operator-stages/first-KXEPLRELEGATION-27-MCI-20260929T205526.227001Z/evidence/r2-receipt.json`, the same two candidate-6 used. `load_r2_receipt` still checks the receipt against the MCI stage's identity and its acceptance interval.
3. **Market scope** (GET-only), by the candidate-6 method and filters (programme, close time, H-SEL-6/7 margin, `fee_type`), with the scripts in [scope-candidate-6/](scope-candidate-6/) (`scope_markets.py`, `shortlist.py select`, `fill_odds.py`, `shortlist.py rank`, `side_flow.py`), into a new `scope-candidate-7/`. Rank by taker trades per hour on each side. New: prefer a market whose touch moves while we hold inventory, since the defect needs a reducer requote; a market that never requotes leaves (a) and (b) NOT EXERCISED. Hugh chooses.
4. **Helpers.** [stage-helpers-candidate-7/](stage-helpers-candidate-7/) is candidate-6's set with the constants changed (`EXE`, the `--binary` and `--candidate-receipt` paths, `SCR` = `loop/run/r2-candidate-7-scratch`; `MCI` unchanged). Two things candidate-6's did by hand are now in `launch.sh`: it writes `$SCR/harness.lstart` for `drill.py crash`/`term harness`, and copies `start_watchers.py` into the scratch directory, where `drill.py restart` runs it. Before any `drill.py` step, set `STAGE` (once `launch.sh` prints it) and `TICKER` in `drill.py`; both are `SET_ME` and `main()` refuses to run until they are replaced. The wrapper runs `caffeinate`, so the iMac stays awake.

## 2. Preparation and launch (one chained command, Hugh's yes)

`launch.sh <ticker>` chains this preparation with the 5 s deadline guard, `-provision`, `install -m 600 /dev/null <live_ok>` and the detached wrapper launch, inside the 60 s freshness window:

```sh
cd /Users/hugh/kek/lip
/Users/hugh/kek/.venv/bin/python scripts/operator_stage.py --stage r2 \
  --ticker "$TICKER" --size 12 \
  --binary notes/first-fill-evidence-2026-09-28/candidate-7/harness \
  --candidate-receipt notes/first-fill-evidence-2026-09-28/candidate-7/build-identity.json \
  --evidence-root notes/first-fill-evidence-2026-09-28/operator-stages \
  --prior-config "$(jq -r .config_path notes/first-fill-evidence-2026-09-28/operator-stages/first-KXEPLRELEGATION-27-MCI-20260929T205526.227001Z/evidence/identity.json)" \
  --r2-receipt "$PWD/notes/first-fill-evidence-2026-09-28/operator-stages/first-KXEPLRELEGATION-27-MCI-20260929T205526.227001Z/evidence/r2-receipt.json"
```

The script prints the 10 authoritative phases; `launch.sh` then prints `STAGE=`, `PID=` and the `ps` line, and writes `$SCR/harness.lstart` itself. Afterwards run `start_watchers.py "$STAGE" <pid> harness.log observer-notices.log <launch unix time>`: a detached observer on one PID, plus the ntfy stream (topic from `~/.kalshi/env`, on stdin).

**While it runs:**
- **Approvals.** A chat option naming the step ("Yes, SIGKILL now") is Hugh's yes. Live steps: launch, SIGKILL, restart, each SIGTERM, `rmlive`. `drill.py` checks argv and start time and logs to `evidence/control.log`.
- **Watching.** A Monitor expires after 30 min, so run the observer detached and `tail -F` its notices file under a Monitor you re-arm at the current line count, filtering out `PNL_MARK_UNAVAILABLE`. The observer prints only non-clean sweeps, so a cleared episode is the notices stopping (the (a) program is the proof). A second Monitor on `owned_order` by rowid every 3 s is the live view of (b). The observer lasts 150 min (`drill.py restart` starts the restart's own): start another if a drain outlasts it.
- **Reads.** `accountcheck` cannot report a held position (`parsePositions`, `go/cmd/accountcheck/main.go`; lip-006): only a flat read is complete.

## 3. Cycles, then the terminal drill

**Target:** at least 3 round trips, 2 h cap from launch. The pilot rung never stops by itself.

**Which drill.** Only one fits this store: after any stop latch the process never adds again. The state at the cap, or when Hugh ends the cycles, decides:
- **A. QUOTING, flat, adds resting** (the 09-30 case): the crash drill below.
- **B. REDUCING, q != 0:** crash while holding inventory, then a planned drain of the restart.
- **C. REDUCING, q != 0:** a planned drain of the first process, no crash.

Hugh picks B or C. B covers item 2 above and check (d) in one episode but mixes two stops; C isolates (d). Either gives item 3 its chance.

**A, one yes per step:**
1. `drill.py gate` until QUOTING, flat, both adds bound and quiet for 8 s. Never during a reduction.
2. `drill.py crash`: SIGKILL, then the prebuilt `accountcheck` into `account-after-crash.json`. Expect `exit-status.txt` = 137 and no latch. Cancel nothing by hand unless the restart fails.
3. `drill.py restart`: the wrapper without `-resume`, and a new observer on `restart.pid`.
4. Expect adoption of the resting orders; FUNDING_LIMITS `recovery_only=true`; a `funding_recovery` latch, then WINDING_DOWN; the adds cancelled and any inventory reduced to flat; DRAINED, no new adds (`notes/cr1-operations-runbook.md`, "Restart after a crash").
5. `phantoms.py "$STAGE" harness.log restart.log`, then `drill.py term restart`; expect `restart-exit-status.txt` = 0.
6. `drill.py read account-after-restart`: require `account_scope_complete` and `flat` true.
7. `drill.py rmlive` removes only this stage's `live_ok`. Retain the store, WAL, journal, latch and every failure.

**B:** `drill.py gate-reducing`, read its output, then `crash-reducing` in place of step 2, then steps 3-4. The crash read cannot show the position; take it from the store's `our_fill`. Run step 5 while the restart still holds inventory, not after DRAINED. A second signal upgrades an unplanned drain to a planned one (`start` in `go/harness/lifecycle/drain.go`); that has not been seen live.

**C:** `phantoms.py`, then `drill.py term harness` while REDUCING; no crash, no restart. Name the final read `account-after-restart`, which `rmlive` checks.

In B and C, `term` waits only 300 s, so `exit status = None` is not a failure: watch for the process to go, then read its status file (expect 0, with the exit coming from the process itself once flat), then steps 6-7. Exposure is bounded by S = 12; a manual close stays Hugh's call.

## 4. Stop conditions and fallbacks

Section 4 of the candidate-6 handoff applies, including its planned-stop path for a global stop and the manual close (runbook, "Stuck reducer and operator manual close"). Added:
- **The lip-14o signature** (a gap over (b)'s line, an (a) episode that does not clear, or zero open orders while q != 0): GET-only read at once, `phantoms.py`, tell Hugh, record FAIL. The known ways out (Finding 1) are an outside touch move or a restart (SIGKILL, then a start without `-resume`: it begins with an empty `pending` and places the reducer). Each is a live step needing his yes.
- **Cap reached with fewer than 3 round trips:** take the drill the state allows and report the count.
- **Known, filed, not fixed** (candidate review, lip-8tl): an order fully executed before its first orders walk can leave its side under-quoted until the touch moves. A missing add after a fast fill is that, not lip-14o.

## 5. After the stage

- **Report:** `evidence/stage-report.md` in the 09-30 format, plus the four lip-14o results with their output.
- **Beads:** a comment on lip-14o and on lip-8hn.2 linking the report, with PASS, FAIL or NOT EXERCISED per check. Both close only on Hugh's review.
- **Latch:** clearing it for any later run is a separate §10.4 decision.

Nothing here grants unattended operation, CR-2, or any assistant authority to trade.
