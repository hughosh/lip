#!/usr/bin/env python3
"""Bounded read-only support for a human-attended stage; not a watchdog.

It never signals, writes to the exchange, or mutates the store. Each sample is
appended to <stage>/evidence/observer.jsonl; a one-line NOTICE is printed only
when something an attending operator should look at changes: a new SEV1/SEV2
anomaly, a new owned fill, a state transition, the latch appearing, a sweep
trace whose verdict is not clean/confirmed, or the process exiting.
"""
import argparse
import datetime as dt
import json
import sqlite3
import subprocess
import sys
import time
from pathlib import Path

TRACE_PREFIX = "harness: sweep-trace "
# Expected, high-volume SEV2 classes: journalled in observer.jsonl, not announced.
QUIET_SEV2 = {"BOOK_QUIET", "FOREIGN_FILL_INHERITED"}


def now():
    return dt.datetime.now(dt.timezone.utc)


def process(pid):
    ps = subprocess.run(["ps", "-p", str(pid), "-o", "pid=,lstart=,command="],
                        capture_output=True, text=True)
    return ps.stdout.strip()


def store(db):
    # Only read a live WAL store: opening a closed store could create sidecars.
    if not (Path(str(db) + "-wal").exists() and Path(str(db) + "-shm").exists()):
        return None
    with sqlite3.connect(db.as_uri() + "?mode=ro", uri=True, timeout=1) as c:
        c.execute("PRAGMA query_only=ON")
        c.row_factory = sqlite3.Row
        c.execute("BEGIN")
        out = {
            "owned_orders": c.execute("SELECT COUNT(*) FROM owned_order").fetchone()[0],
            "fills": [dict(x) for x in c.execute("SELECT * FROM our_fill ORDER BY rowid")],
            "states": [dict(x) for x in c.execute("SELECT * FROM state_event ORDER BY rowid")],
            "anomalies": [dict(x) for x in c.execute(
                "SELECT anomaly_id,class,sev,first_ms,delivered_ms,text FROM anomaly "
                "WHERE sev >= 1 ORDER BY rowid")],
        }
        c.rollback()
    return out


def traces(log, offset):
    if not log.exists():
        return [], offset
    with log.open("rb") as f:
        f.seek(offset)
        data = f.read()
    lines, new_offset = [], offset
    for raw in data.splitlines(keepends=True):
        if not raw.endswith(b"\n"):
            break
        new_offset += len(raw)
        line = raw.decode("utf-8", "replace").rstrip("\n")
        if line.startswith(TRACE_PREFIX):
            try:
                lines.append(json.loads(line[len(TRACE_PREFIX):]))
            except json.JSONDecodeError:
                lines.append({"undecodable": line[:200]})
    return lines, new_offset


def notice(msg):
    print(f"NOTICE {now().strftime('%H:%M:%SZ')} {msg}", flush=True)


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--stage", required=True, type=Path)
    ap.add_argument("--pid", required=True, type=int)
    ap.add_argument("--log", default="harness.log", help="file under <stage>/evidence")
    ap.add_argument("--interval", type=float, default=10)
    ap.add_argument("--max-minutes", type=float, default=120)
    a = ap.parse_args()
    a.stage = a.stage.resolve()  # store() builds a file: URI, which needs an absolute path
    db = a.stage / "runtime/harness.db"
    latch = a.stage / "runtime/harness.halt"
    log = a.stage / "evidence" / a.log
    out = a.stage / "evidence" / "observer.jsonl"
    seen_anoms, seen_fills, seen_states = set(), 0, 0
    latch_seen, gone, offset = latch.exists(), 0, 0
    stop_at = now() + dt.timedelta(minutes=a.max_minutes)
    notice(f"observing pid {a.pid} stage {a.stage.name} until {stop_at:%H:%M:%SZ}")
    while now() < stop_at:
        row = {"at_utc": now().isoformat(), "process": process(a.pid),
               "latch_exists": latch.exists()}
        try:
            s = store(db)
        except sqlite3.Error as exc:
            s, row["store_error"] = None, str(exc)
        if s is not None:
            row.update({"owned_orders": s["owned_orders"], "fill_count": len(s["fills"]),
                        "state_count": len(s["states"]), "anomaly_count": len(s["anomalies"])})
            for an in s["anomalies"]:
                if an["anomaly_id"] not in seen_anoms:
                    seen_anoms.add(an["anomaly_id"])
                    if an["sev"] == 1 and an["class"] in QUIET_SEV2:
                        continue
                    sev = {2: "SEV1", 1: "SEV2"}.get(an["sev"], an["sev"])
                    notice(f"{sev} {an['class']} {an['anomaly_id']}: {an['text'][:220]}")
            for f in s["fills"][seen_fills:]:
                notice(f"FILL {json.dumps(f, default=str)[:300]}")
            seen_fills = len(s["fills"])
            for st in s["states"][seen_states:]:
                notice(f"STATE {json.dumps(st, default=str)[:240]}")
            seen_states = len(s["states"])
        if row["latch_exists"] and not latch_seen:
            notice(f"LATCH present at {latch}")
        latch_seen = row["latch_exists"]
        new_traces, offset = traces(log, offset)
        for t in new_traces:
            verdict = t.get("verdict")
            if verdict not in ("clean", "confirmed"):
                notice(f"SWEEP {verdict} requested={t.get('requested')} still={t.get('still_resting')}")
        row["sweeps"] = [{"verdict": t.get("verdict"), "requested": t.get("requested"),
                          "confirms": len(t.get("confirms") or [])} for t in new_traces]
        with out.open("a") as f:
            f.write(json.dumps(row, default=str) + "\n")
        if not row["process"]:
            gone += 1
            if gone == 1:
                notice(f"PROCESS {a.pid} absent")
            if gone >= 2:
                break
        else:
            gone = 0
        time.sleep(a.interval)
    notice("observer stopped")


if __name__ == "__main__":
    sys.exit(main())
