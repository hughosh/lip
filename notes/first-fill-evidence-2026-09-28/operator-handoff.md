# Operator handoff: candidate-4 first-fill stage (lip-dwf)

This supersedes [the September 28 handoff](../production-turnover-2026-09-28/operator-handoff.md) for the next attended stage. Its history, including the stopped and flat e3f8a697 attempt, remains valid. That attempt's runtime keeps a signal latch: never restart it.

- **Candidate:** `notes/first-fill-evidence-2026-09-28/candidate-4/harness`, sha256 `4d9b8a1ac4a37b6760f6c9d9ad65ef3fe9424e2c89054e98da97cd8ef5797771`.
- **Source manifest:** `cdb8440a…37ce`.
- **Decision:** [candidate-review.md](candidate-review.md).
- **Market:** Hugh chose `KXAAAGASW-26OCT05-4.4200` from a GET-only scope ([scope/choice.json](scope/choice.json)).
- **Size:** `S=12` on the `sizing` rung, capital from `selected_shard_balance`.

Hugh provisions, arms, launches, signals and cancels. The assistant runs only the GET-only preparation, account reads and a read-only observer, and records evidence. No restricted action is routed through another model or tool.

## 1. Preparation (assistant, within 60 s of arming)

When Hugh says he is ready and attending, the assistant takes a fresh public book capture and immediately runs `scripts/operator_stage.py --stage first` with this candidate and receipt. It then reviews the output: rules, fees, depth, account-wide flatness and funding arithmetic. It hands over a comment-free command block with the exact new stage paths. If more than 60 s pass before arming, preparation is repeated.

## 2. Launch, attend, drain (Hugh, one terminal)

Everything from `-provision` through `wait` and the exit-status file runs in **one** shell, with the stage variables set in that same shell. The last run lost its exit code because `wait` ran elsewhere. Paste only the command lines handed over; there are no comment lines.

While it runs, the assistant watches the stage store, journal and sweep traces read-only and relays changes in the conversation. You are the attendant; the relay does not replace ntfy, and ntfy does not replace your watching. The observer reports:

- every SEV1/SEV2 except the expected BOOK_QUIET and FOREIGN_FILL_INHERITED;
- each fill, state transition and appearance of the latch;
- any sweep whose verdict is not `clean`/`confirmed`;
- the process exit.

Hugh can watch too:

```sh
tail -f "$STAGE/runtime/anomaly.jsonl"
grep --line-buffered 'sweep-trace' "$STAGE/evidence/harness.log"
```

The first owned fill should latch the durable first-fill stop, move to WINDING_DOWN, cancel and verify the adding orders, and place a funded, bounded reduction. Stay until the reduction resolves. A no-fill window is inconclusive; request drain at 45 minutes.

If `SEV1 SWEEP_INCOMPLETE` fires, read the matching `sweep-trace` line: requested id, DELETE statuses, listed record, named read. The order stays live in the risk model, and the next tick cancels again. Verify with a named-order GET before concluding anything, and do not lower the alarm.

## 3. After exit (assistant reads, Hugh confirms)

The assistant runs `go run ./cmd/accountcheck -ticker … -fills` into the stage evidence. It must show `account_scope_complete=true` and `flat=true`, zero open orders and zero nonzero positions across every enumerated subaccount. It also collects named-order GETs for every stage order, with fill fee/taker fields.

## 4. Retained-latch restart (Hugh)

Only after the original PID is absent and the first-fill latch is inspected, and after a fresh re-read of account, program, book, fees and funding. Run `-resume 'first-owned-fill stop validation'`, observe adoption and WINDING_DOWN with no new adds, then signal and wait in the same shell. Then take a **new** account-wide flat read; the post-drain read cannot substitute for it. Remove only this stage's `live_ok`, after every stage process has exited and flatness is proven. Retain the store, WAL, SHM, journal, latch and failures.

Nothing in this handoff qualifies repeated cycles (R2), live turnover (CR-2) or profitability.

## As executed on 2026-09-29, and the pattern to reuse

The stage ran, and the outcome is in `operator-stages/first-KXAAAGASW-26OCT05-4.4200-20260929T172207.089474Z/evidence/stage-report.md`.

- **Who ran what.** Hugh switched Claude Code to **Manual** permission mode (from the mobile dropdown) and approved each live command. The assistant executed them. Auto mode's classifier refuses assistant-run live steps outright, whatever is said in chat.
- **Launch.** One command ran the book capture, `operator_stage.py`, provisioning, arming and the launch together, so the 60 s freshness window held: the book was 57 s old at launch, and preparation alone takes about 52 s.
- **Detaching.** Each harness ran detached (`start_new_session`) under a `/bin/bash -c` wrapper that writes `harness.pid`/`restart.pid`, runs `caffeinate -w <pid>`, `wait`s, and writes `exit-status.txt`/`restart-exit-status.txt`. This is how the exit codes were captured despite the separate shells.
- **Before each SIGTERM,** `ps -ww -o command=` had to match the exact expected argv. Every control action is logged in `evidence/control.log`.
- **During the run,** the assistant watched a read-only observer (`observe_stage.py`) through a Monitor stream.
- **The ntfy "lip WINDING_DOWN" messages every 30 s** are the heartbeat (`heartbeat_s: 30`, inherited from the q01-v3 template config), not alarms.
