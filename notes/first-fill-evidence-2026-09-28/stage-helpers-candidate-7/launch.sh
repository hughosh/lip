#!/bin/bash
set -u
TICKER="$1"
REPO=/Users/hugh/kek/lip
PY=/Users/hugh/kek/.venv/bin/python
EXE=/Users/hugh/kek/lip/notes/first-fill-evidence-2026-09-28/candidate-7/harness
MCI=/Users/hugh/kek/lip/notes/first-fill-evidence-2026-09-28/operator-stages/first-KXEPLRELEGATION-27-MCI-20260929T205526.227001Z
SCR=/Users/hugh/kek/lip/loop/run/r2-candidate-7-scratch
cd "$REPO" || exit 90
mkdir -p "$SCR"
# drill.py restart runs start_watchers.py from the scratch directory
cp "$(cd "$(dirname "$0")" && pwd)/start_watchers.py" "$SCR/" || exit 91
date -u +prep_start=%Y-%m-%dT%H:%M:%SZ
"$PY" scripts/operator_stage.py --stage r2 --ticker "$TICKER" --size 12 \
  --binary notes/first-fill-evidence-2026-09-28/candidate-7/harness \
  --candidate-receipt notes/first-fill-evidence-2026-09-28/candidate-7/build-identity.json \
  --evidence-root notes/first-fill-evidence-2026-09-28/operator-stages \
  --prior-config "$MCI/config.json" \
  --r2-receipt "$MCI/evidence/r2-receipt.json" > "$SCR/prep.out" 2>&1
rc=$?
if [ "$rc" -ne 0 ]; then echo "PREP FAILED rc=$rc"; cat "$SCR/prep.out"; exit 91; fi
EVID=$(sed -n 's/^Prepared r2 evidence: //p' "$SCR/prep.out")
if [ ! -d "$EVID" ]; then echo "NO EVIDENCE DIR"; cat "$SCR/prep.out"; exit 92; fi
STAGE=$(dirname "$EVID")
CFG="$STAGE/config.json"
cp "$SCR/prep.out" "$EVID/prep.out"
"$PY" - "$EVID/prep-timing.json" <<'PYG' || { echo "DEADLINE GUARD REFUSED: nothing provisioned or armed"; exit 93; }
import datetime as dt, json, sys
t = json.load(open(sys.argv[1]))
deadline = dt.datetime.fromisoformat(t["launch_deadline_utc"])
left = (deadline - dt.datetime.now(dt.timezone.utc)).total_seconds()
print(f"launch_deadline_utc {deadline.isoformat()} slack {left:.1f}s")
sys.exit(0 if left >= 5 else 1)
PYG
"$EXE" -config "$CFG" -provision > "$EVID/provision.log" 2>&1 || { echo "PROVISION FAILED"; cat "$EVID/provision.log"; exit 94; }
install -m 600 /dev/null "$STAGE/runtime/live_ok"
date -u +%Y-%m-%dT%H:%M:%SZ > "$EVID/launch-time.txt"
"$PY" - "$EVID" "$EXE" "$CFG" harness "$EVID/exit-status.txt" <<'PYW'
import subprocess, sys
evid, exe, cfg, prefix, status = sys.argv[1:6]
script = ('cd "$1" || exit 97; "$2" -config "$3" -rung pilot -live > "$4.log" 2>&1 & pid=$!; '
          'echo "$pid" > "$4.pid"; /usr/bin/caffeinate -i -w "$pid" & wait "$pid"; echo "$?" > "$5"')
with open(f"{evid}/{prefix}-wrapper.log", "ab") as log:
    subprocess.Popen(["/bin/bash", "-c", script, "wrapper", evid, exe, cfg, prefix, status],
                     start_new_session=True, stdin=subprocess.DEVNULL, stdout=log, stderr=subprocess.STDOUT)
PYW
sleep 3
echo "STAGE=$STAGE"
echo "LAUNCHED_AT=$(cat "$EVID/launch-time.txt")"
echo "PID=$(cat "$EVID/harness.pid")"
ps -ww -o pid=,lstart=,command= -p "$(cat "$EVID/harness.pid")"
# start time that drill.py crash / term harness read for their argv+start check
ps -ww -o lstart= -p "$(cat "$EVID/harness.pid")" | sed 's/^ *//;s/ *$//' > "$SCR/harness.lstart"
tail -5 "$EVID/harness.log"
