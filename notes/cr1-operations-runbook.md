# CR1 operator runbook — one declared market

This is an operator procedure, not authority to start a live writer. Use the
reviewed binary, config, rung and account approved for the current trial. Keep
the account dedicated: before every start, obtain a **complete account-wide**
read-only orders, positions and fills view from the exchange, save its timestamp
and raw response, and resolve any foreign or unknown activity. An unavailable or
incomplete view is not a clean account. Record the selected market and verify it
is the single market in the config. Record the approved commit and binary SHA-256;
the binary does not self-attest its commit under this module layout.

Set these shell variables to the *reviewed* paths; the examples are the canary
layout, not a default or an instruction to arm it:

```sh
CFG=/Users/hugh/.lip/canary/config.json
BIN=/Users/hugh/.lip/bin/harness
PLIST="$HOME/Library/LaunchAgents/com.lip.harness.plist"
DB=$(jq -er '.paths.db' "$CFG")
JOURNAL=$(jq -er '.paths.anomaly_log' "$CFG")
LATCH=$(jq -er '.paths.latch' "$CFG")
STOP=$(jq -er '.paths.stop' "$CFG")
jq -e '{ticker,rung,s,capital_max,n_markets,paths:{db:.paths.db,anomaly_log:.paths.anomaly_log,latch:.paths.latch,stop:.paths.stop}}' "$CFG"
shasum -a 256 "$BIN" "$CFG"
git -C /Users/hugh/kek/lip rev-parse HEAD
```

For a normal start, confirm the recorded hashes and rung against the approved
candidate, no other harness instance, no halt/stop sentinel, the account-wide
read-only view above, and both alert destinations and the external dead-man
monitor tested. Confirm the approved config has the expected account key path;
do not print secret contents. Measure both available space and growth from the
previous observation:

```sh
test -x "$BIN" && test -s "$DB" && test ! -e "$LATCH" && test ! -e "$STOP"
df -k "$(dirname "$DB")" "$(dirname "$JOURNAL")"
du -k "$DB" "$DB"-wal "$DB"-shm "$JOURNAL" 2>/dev/null
sqlite3 -readonly "$DB" 'PRAGMA quick_check;'
launchctl print "gui/$(id -u)/com.lip.harness"
```

The Go start check refuses below **1 GiB available** on any store, journal or
qualification-evidence volume; a running owner samples at least once per minute
and durably stops adding on low or unknown headroom. The floor reserves room for
recovery writes, not a prediction of the run's total size. During a run, record
`df -k` and `du -k` for DB, WAL, SHM, journal and evidence hourly. Growth is
bounded by the free-space reserve; an unexpected rising trend, inability to
sample, or any volume approaching the floor is a stop condition. Preserve active
artifacts. Stop and archive only **after** the store is closed and a verified
copy exists; never truncate, move or vacuum an active ledger. A full mutation
audit is a different workload: the measured Go-cache floor is **15 GiB free**
before starting it. Schedule `go clean -cache` only with no build/gate running,
and record free space before and after.

Only after every precondition passes, start the already installed, reviewed job
with `launchctl bootstrap "gui/$(id -u)" "$PLIST"`; a deployed live plist is a
write-capable operation. Observe its first complete startup reconciliation,
state, heartbeat/dead-man check-in, journal, and no unexpected resting order.
Keep the exact command, timestamp, binary/config hash, account-wide views and
first observations in the run evidence. A refused start must not be retried by
changing the DB path or clearing the latch.

For a planned stop, record the time and send
`kill -TERM "$HARNESS_CHILD_PID"` to the **identified harness child** of
`caffeinate`. Obtain that PID from the reviewed job/process tree; do not guess.
Keep the supervisor loaded throughout drain; booting it out can force-kill a
process that has not finished reducing. Only after verified flat, no open orders
and the child's clean exit may you run
`launchctl bootout "gui/$(id -u)" "$PLIST"` to unload the job. Do not use `pkill -f harness`: it can
kill the wrapper and orphan the actual writer. SIGTERM asks for a durable
`WINDING_DOWN`; it does not mean flat or authorize abandoning inventory. Watch
complete positions/orders/fills walks, reducing orders, alerts and journal until
the account is verified flat with no resting order of ours. Preserve the final
store, WAL, journal and latch and the exchange response. If the child does not
exit and `harness.err` shows `ORDERLY_STOP_REFUSED` saying the stop failed
permanently, it will not retry and stays up. Preserve `harness.err` and the
store files, confirm the account flat through the independent read-only route,
then unload the job with the `launchctl bootout` command above, which ends the
child without a KeepAlive restart. Follow the unreadable-store path below
before any restart. If the refusal says the instance lock is released, unload
this job before starting anything else.

If a latch is present at restart, inspect it with `cat "$LATCH"` and save a
copy. No automatic restart may clear it. `-resume 'documented operator reason'`
allows the process to enter `WINDING_DOWN` and manage residual risk; it does
**not** clear the latch or resume adding. Before any later fresh writer, a human
must establish complete exchange truth, settle every anomaly and separately
authorize clearing the latch under §10.4. An unreadable latch also means
latched, not clear.

If the store is missing, zero-length, corrupt or unreadable, or its journal
cannot reconcile, **do not provision a replacement or infer flatness**. Preserve
the files and errors (`ls -l "$DB" "$DB"-wal "$DB"-shm "$JOURNAL" "$LATCH"`;
`sqlite3 -readonly "$DB" 'PRAGMA quick_check;'` after stopping). Prevent any
new writer, obtain complete exchange orders/positions/fills views with an
independent read-only route, and continue attended risk reduction through the
approved recovery procedure. A store that cannot classify ownership cannot
license new adds. Restoration must recover the real ownership ledger, then
reconcile exchange truth before a restart. Escalate immediately if any order or
inventory may remain unmanaged. Never create an empty store over this state.

For low disk headroom, capture `df -k` and `du -k` as above, stop new starts,
and observe the running harness's `DISK_HEADROOM` SEV1 and durable latch. Its
owner loop should keep complete reads, cancellation and reducers active where
exchange truth permits. Free space by removing only **unrelated, verified
disposable** material; do not delete an active store, WAL, journal, evidence or
latch. Re-measure and investigate growth before considering a new start.

**Sticky REDUCING.** `QUOTING_STOPPED_UNTIL_RESTART` (SEV2) names a market
that has stopped adding for the rest of the process (spec H-FAIL-1a). It
usually arrives beside its cause: `WS_DISCONNECT_SUSTAINED` (SEV1, every
market), an F5 cross-check disagreement, an F9 reject rate, or a quarantined
book. Nothing in the process clears it. Reducers, cancels and monitoring
continue, so leaving it running is safe but earns nothing in that market. To
quote again, first confirm the cause has cleared, for example that the network
is back after F4. Then do the planned stop above and let it drain and exit.
Inspect and save the latch, dispose of every anomaly, and clear the latch
under spec §10.4 only when complete exchange truth shows a clean account.
Restart through the normal-start checks. `-resume` does not regain quoting:
it only lets a latched process enter `WINDING_DOWN`.

**Stuck reducer and operator manual close.** The harness never crosses the
spread, by design (spec §6.4): an unfilled reducer keeps resting and follows the
touch, and a position it never reduces settles at $0.00 or $1.00, which is
accepted. `INVENTORY_STUCK` after `stuck_s` (1800 s) is information; "the
operator may choose to act; the harness will not", and nothing requires waiting
for it. A near-resolved book can leave the reducer unfilled: in the worked
example YES 12 @ 3c rested 15 minutes against YES 2c / NO 97c. To exit, close
the whole position yourself on the same account, through the exchange UI or a
manual order; it crosses and pays the taker fee (12 NO @ 96c, fee $0.0323).
Save the order and fill record as `evidence/operator-manual-close.json`: who
acted, their statement, order id, fill id, time, price, count, `is_taker`, fee.

The harness reaction is correct, not a malfunction. The order id is not in its
ownership ledger, so it raises SEV1 `FOREIGN_FILL` with global `WINDING_DOWN`;
a position delta above `pos_drift_hard` (5 contracts) at one poll raises SEV1
`POSITION_DRIFT`, also global. It cancels its reducer and goes `REDUCING`→`IDLE`
and `WINDING_DOWN`→`DRAINED` (example: a23, a24). Exchange reads lag a cancel by
about 1.4 s. Candidate-4 paged that lag as a false SEV1 `SWEEP_INCOMPLETE`
(a27); from candidate-5 it journals a non-paging SEV3 `SWEEP_PENDING` and pages
SEV1 only after 5 s unconfirmed (lip-9tt), so a SEV1 there is real. SEV2 `PNL_BASIS_UNAVAILABLE` is expected after a manual trade.
`DRAINED` did not end the process in the example. Then, in order, with `CFG`
and `JOURNAL` from that stage's config: confirm the PID and its start time
identify the harness; record the time and send SIGTERM; `wait` for it in the
shell that launched it (under launchd, read the job's last exit status) and
require 0; take the complete account-wide read-only view and require zero open
orders and zero nonzero positions. Only if the stage calls for it, restart on
the retained latch with `-resume 'documented operator reason'`, expect
`WINDING_DOWN`→`DRAINED` with no order adopted or placed and no SEV1, stop it
the same way (exit 0) and take a second complete flat read. Then remove only
that stage's `live_ok`. Retain the latch, store, WAL and journal; clearing the
latch stays under §10.4.

```sh
ps -o pid=,lstart=,command= -p "$HARNESS_CHILD_PID"
kill -TERM "$HARNESS_CHILD_PID"
wait "$HARNESS_CHILD_PID"; echo "exit $?"
jq -r 'select(.sev == 2) | [.anomaly_id, .class, (.first_ms / 1000 | todate)] | @tsv' "$JOURNAL"
rm -- "$(jq -er '.paths.live_ok' "$CFG")"
```

For every SEV1, record in the stage evidence its anomaly id (the `jq` line lists
`sev` 2, which is SEV1), the ntfy delivery time and the time it was actually
seen on the phone; the worked example did not record receipt. The periodic
heartbeat is not an alarm: seeing 30-second `WINDING_DOWN` heartbeats is not
SEV1 receipt (bead lip-1gb). Worked example, 2026-09-29:
`notes/first-fill-evidence-2026-09-28/operator-stages/first-KXAAAGASW-26OCT05-4.4200-20260929T172207.089474Z/`.

An hourly heartbeat is expected. The external dead-man service must alarm on a
missed check-in; treat a missing heartbeat or dead-man alarm as a possible dead
process or broken alert route, not proof that exposure is zero. Immediately
inspect the job (`launchctl print "gui/$(id -u)/com.lip.harness"`), the last
journal line (`tail -n 20 "$JOURNAL"`), and complete exchange truth through the
independent read-only route. Issue an attended stop request through the
configured stop sentinel only when the operator has decided to stop adding:
`touch "$STOP"`; record its timestamp. It latches a stop and cannot end the
process. If the process is absent, prevent an unreviewed KeepAlive restart and
follow the store/latch recovery path above.

The launchd job starts at load and restarts nonzero failures at a throttled
60-second cadence. A planned flat drain or supervised structural refusal exits
zero and leaves the job stopped. Inspect `harness.err` and the job's last exit
status before any manual restart. A throttle limits retry frequency, not the
number of retries. Replacing a plist on disk does not change a loaded job;
bootout and a later operator-controlled bootstrap are required to adopt it.

Every SEV1 is immediate operator action; every SEV2 is reviewed and assigned a
disposition the same day. In the incident record, retain class, timestamp,
market, journal row, exchange truth, operator, action and closure evidence.
The external alert route must page a primary operator and, if a SEV1 remains
unacknowledged after **5 minutes**, page a named backup. A missed hourly
heartbeat/dead-man alarm must page immediately and also reach the backup if
unacknowledged after **5 minutes**. Acknowledgement means a human has assumed
the incident, not that the risk is resolved. Until both routes, timeouts,
contacts and acknowledgement receipts are configured and an end-to-end drill
is saved, CR1 external escalation is **unverified and blocks promotion**.
The drill must suppress a check-in and send a test SEV1, show both primary and
backup alarm delivery without an acknowledgement, then show a human ack and
save provider timestamps/receipts. Do not treat a local fake alert test as that
external drill.
