"""Crash-drill helpers for the candidate-7 R2 stage (handoff section 3). One subcommand per step.

  gate                 read-only: is the market quiet, flat and QUOTING with both adds bound?
  gate-reducing        read-only: the same, allowing REDUCING with q != 0 (handoff variant B)
  status               read-only: store summary (state, q, latest adds, fills, anomalies)
  crash                LIVE: wait for a quiet window, check argv+start, SIGKILL, then at once the
                       GET-only account read into account-after-crash.json
  crash-reducing       LIVE: crash while REDUCING with q != 0 (variant B); read gate-reducing first
  restart              LIVE: restart without -resume through the wrapper, then a new observer
  term harness|restart LIVE: check argv+start, SIGTERM, wait for the wrapper's exit status
  read NAME            GET-only account read (go run, printed phase 9) into evidence/NAME.json
  summary FILE         summarise an account read
  rmlive               LIVE: remove this stage's live_ok after every process exited and a
                       complete flat account-after-restart read

Every LIVE subcommand runs only after Hugh's explicit chat yes for that step.
"""
import datetime as dt
import json
import os
import signal
import sqlite3
import subprocess
import sys
import time
from pathlib import Path

REPO = Path("/Users/hugh/kek/lip")
# set once launch.sh prints the stage directory (its STAGE= line); placeholder until then
STAGE = REPO / "notes/first-fill-evidence-2026-09-28/operator-stages/r2-KXEPLRELEGATION-27-MCI-20261001T031001.344628Z"
EVID, RUNTIME = STAGE / "evidence", STAGE / "runtime"
DB, LATCH, LIVE_OK = RUNTIME / "harness.db", RUNTIME / "harness.halt", RUNTIME / "live_ok"
EXE = REPO / "notes/first-fill-evidence-2026-09-28/candidate-7/harness"
CFG = STAGE / "config.json"
TICKER = "KXEPLRELEGATION-27-MCI"  # Hugh's choice (AskUserQuestion, 2026-10-01 ~03:06Z); the same ticker passed to launch.sh
PY = "/Users/hugh/kek/.venv/bin/python"
SCR = Path("/Users/hugh/kek/lip/loop/run/r2-candidate-7-scratch")
EXPECT_CMD = f"{EXE} -config {CFG} -rung pilot -live"
QUIET_MS = 8000


def utc():
    return dt.datetime.now(dt.timezone.utc).strftime("%Y-%m-%dT%H:%M:%S.%fZ")[:-4] + "Z"


def control(line):
    with open(EVID / "control.log", "a") as f:
        f.write(f"{utc()} {line}\n")


def store():
    with sqlite3.connect(f"file:{DB}?mode=ro", uri=True, timeout=2) as c:
        c.execute("PRAGMA query_only=ON")
        c.row_factory = sqlite3.Row
        orders = [dict(r) for r in c.execute("SELECT * FROM owned_order ORDER BY rowid")]
        fills = [dict(r) for r in c.execute("SELECT * FROM our_fill ORDER BY rowid")]
        states = [dict(r) for r in c.execute("SELECT * FROM state_event ORDER BY rowid")]
        anomalies = [dict(r) for r in c.execute(
            "SELECT anomaly_id, class, sev, first_ms, text FROM anomaly ORDER BY rowid")]
    return orders, fills, states, anomalies


def assess(allow_reducing=False):
    orders, fills, states, anomalies = store()
    q = sum(f["count_q"] if f["side"] == "yes" else -f["count_q"] for f in fills)
    market = [s for s in states if s["scope"] == "market"]
    glob = [s for s in states if s["scope"] == "global"]
    latest = {}
    for o in orders:
        latest[o["side"]] = o
    stamps = [v for o in orders for v in (o["reserved_ms"], o["bound_ms"], o["abandoned_ms"]) if v]
    stamps += [s["ts_ms"] for s in states] + [f["first_seen_ms"] for f in fills]
    idle_ms = int(time.time() * 1000) - max(stamps) if stamps else None
    reasons = []
    if not glob or glob[-1]["to_state"] != "RUNNING":
        reasons.append(f"global {glob[-1]['to_state'] if glob else None}")
    mstate = market[-1]["to_state"] if market else None
    if allow_reducing and mstate == "REDUCING" and q != 0:
        # Mid-reduction variant (Hugh's call at the cap): the reducing side's latest order is bound.
        side = "yes" if q < 0 else "no"
        o = latest.get(side)
        if not o or not o["order_id"] or o["abandoned_ms"]:
            reasons.append(f"no bound {side} reducer")
    else:
        if mstate != "QUOTING":
            reasons.append(f"market {mstate}")
        if q != 0:
            reasons.append(f"q {q / 100:g}")
        for side in ("yes", "no"):
            o = latest.get(side)
            if not o or not o["order_id"] or o["abandoned_ms"]:
                reasons.append(f"no bound {side} add")
    if idle_ms is None or idle_ms < QUIET_MS:
        reasons.append(f"last activity {idle_ms} ms ago")
    return {"quiet": not reasons, "reasons": reasons, "q": q / 100, "idle_ms": idle_ms,
            "global": glob[-1]["to_state"] if glob else None,
            "market": market[-1]["to_state"] if market else None,
            "latest": {s: {k: o[k] for k in ("price_cents", "count_q", "order_id", "role")}
                       for s, o in latest.items()},
            "orders": len(orders), "fills": len(fills), "states": len(states),
            "anomalies": len(anomalies)}


def check_argv(pid, lstart):
    out = subprocess.run(["ps", "-ww", "-o", "lstart=,command=", "-p", str(pid)],
                         capture_output=True, text=True).stdout
    got, want = " ".join(out.split()), " ".join(f"{lstart} {EXPECT_CMD}".split())
    if got != want:
        raise SystemExit(f"ARGV CHECK FAILED for pid {pid}: got {got!r}")
    return got


def alive(pid):
    return bool(subprocess.run(["ps", "-p", str(pid), "-o", "pid="], capture_output=True,
                               text=True).stdout.strip())


def wait_file(path, seconds):
    end = time.time() + seconds
    while time.time() < end:
        if path.exists() and path.read_text().strip():
            return path.read_text().strip()
        time.sleep(0.2)
    return None


def summary(path):
    r = json.loads(Path(path).read_text())
    print(json.dumps({
        "file": Path(path).name, "started_at": r.get("started_at"),
        "account_scope_complete": r.get("account_scope_complete"), "flat": r.get("flat"),
        "funding_micro": r.get("market_funding_available_micro"),
        "open_orders": [{k: o.get(k) for k in ("order_id", "ticker", "side", "remaining_count",
                                               "created_time")} for o in r.get("open_orders") or []],
        "nonzero_positions": r.get("nonzero_positions") or [],
        "incomplete": [k for k, v in r.items() if k.endswith("_status") and isinstance(v, dict)
                       and v.get("outcome") != "complete"]}, indent=1))


def account_read(name, prebuilt):
    tool = [str(STAGE / "tools/accountcheck")] if prebuilt else ["go", "run", "./cmd/accountcheck"]
    out = EVID / f"{name}.json"
    rc = subprocess.run(tool + ["-ticker", TICKER, "-fills", "-out", str(out)], cwd=REPO / "go",
                        capture_output=True, text=True)
    print(f"{utc()} accountcheck rc={rc.returncode} {rc.stderr.strip()[-200:]}")
    if out.exists():
        summary(out)


def main():
    cmd = sys.argv[1]
    if "SET_ME" in str(STAGE) or TICKER == "SET_ME":
        # Fail closed: a real STAGE with a placeholder TICKER would let `read` take a
        # wrong-ticker account read that `rmlive` then trusts.
        sys.exit("drill.py: set STAGE and TICKER (both are still SET_ME)")
    if cmd in ("gate", "status"):
        print(json.dumps(assess(), indent=1))
    elif cmd == "gate-reducing":
        print(json.dumps(assess(allow_reducing=True), indent=1))
    elif cmd in ("crash", "crash-reducing"):
        pid = int((EVID / "harness.pid").read_text())
        lstart = (SCR / "harness.lstart").read_text().strip()
        end = time.time() + 300
        while True:
            a = assess(allow_reducing=(cmd == "crash-reducing"))
            if a["quiet"]:
                break
            if time.time() > end:
                raise SystemExit(f"NO QUIET WINDOW in 300 s, nothing signalled: {a['reasons']}")
            time.sleep(0.5)
        check_argv(pid, lstart)
        os.kill(pid, signal.SIGKILL)
        control(f"SIGKILL pid {pid} started {lstart} (crash drill, Hugh's yes); gate: market "
                f"{a['market']}, q {a['q']:g}, idle {a['idle_ms']} ms, adds {json.dumps(a['latest'])}")
        print(f"{utc()} SIGKILL sent to {pid}; gate {json.dumps(a)}")
        print(f"exit-status.txt = {wait_file(EVID / 'exit-status.txt', 15)}")
        account_read("account-after-crash", prebuilt=True)
        print(f"latch exists: {LATCH.exists()}")
    elif cmd == "restart":
        old = int((EVID / "harness.pid").read_text())
        if alive(old):
            raise SystemExit(f"harness pid {old} is still alive")
        if LATCH.exists():
            raise SystemExit(f"latch present at {LATCH}; a no-resume restart would refuse")
        if not (EVID / "account-after-crash.json").exists():
            raise SystemExit("account-after-crash.json missing")
        script = ('cd "$1" || exit 97; "$2" -config "$3" -rung pilot -live > "$4.log" 2>&1 & pid=$!; '
                  'echo "$pid" > "$4.pid"; /usr/bin/caffeinate -i -w "$pid" & wait "$pid"; echo "$?" > "$5"')
        with open(EVID / "restart-wrapper.log", "ab") as log:
            subprocess.Popen(["/bin/bash", "-c", script, "wrapper", str(EVID), str(EXE), str(CFG),
                              "restart", str(EVID / "restart-exit-status.txt")],
                             start_new_session=True, stdin=subprocess.DEVNULL, stdout=log,
                             stderr=subprocess.STDOUT)
        (EVID / "restart-time.txt").write_text(utc() + "\n")
        pid = wait_file(EVID / "restart.pid", 10)
        time.sleep(2)
        ps = subprocess.run(["ps", "-ww", "-o", "lstart=", "-p", pid], capture_output=True, text=True)
        lstart = " ".join(ps.stdout.split())
        (SCR / "restart.lstart").write_text(lstart + "\n")
        control(f"restart pid {pid} started {lstart} without -resume (crash drill, Hugh's yes)")
        print(f"restart pid {pid} lstart {lstart!r}; argv {check_argv(int(pid), lstart)}")
        w = subprocess.run([PY, str(SCR / "start_watchers.py"), str(STAGE), pid, "restart.log",
                            "restart-observer-notices.log"], capture_output=True, text=True)
        print(w.stdout.strip(), w.stderr.strip())
    elif cmd == "term":
        which = sys.argv[2]
        pid = int((EVID / f"{which}.pid").read_text())
        lstart = (SCR / f"{which}.lstart").read_text().strip()
        check_argv(pid, lstart)
        os.kill(pid, signal.SIGTERM)
        control(f"SIGTERM {which} pid {pid} started {lstart} (Hugh's yes)")
        status = EVID / ("exit-status.txt" if which == "harness" else "restart-exit-status.txt")
        print(f"{utc()} SIGTERM sent to {pid}; exit status = {wait_file(status, 300)}")
    elif cmd == "read":
        account_read(sys.argv[2], prebuilt=False)
    elif cmd == "summary":
        summary(sys.argv[2])
    elif cmd == "rmlive":
        for name in ("harness.pid", "restart.pid"):
            p = EVID / name
            if p.exists() and alive(int(p.read_text())):
                raise SystemExit(f"{name} process still alive")
        r = json.loads((EVID / "account-after-restart.json").read_text())
        if r.get("account_scope_complete") is not True or r.get("flat") is not True:
            raise SystemExit("account-after-restart is not complete and flat")
        LIVE_OK.unlink(missing_ok=True)
        control(f"removed {LIVE_OK} after complete flat account-after-restart (Hugh's yes)")
        print(f"{utc()} removed live_ok; exists now: {LIVE_OK.exists()}")
    else:
        raise SystemExit(__doc__)


if __name__ == "__main__":
    main()
