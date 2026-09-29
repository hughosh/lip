#!/usr/bin/python3
"""Bounded macOS launchd/caffeinate probe; manages only its own unique jobs."""
import datetime
import json
import os
import plistlib
import re
import signal
import subprocess
import time
import uuid
from pathlib import Path

ROOT = Path(__file__).resolve().parent
CHILD = ROOT / "child.py"
DOMAIN = f"gui/{os.getuid()}"
NONCE = uuid.uuid4().hex[:10]
RESULT = {"started_utc": datetime.datetime.now(datetime.timezone.utc).isoformat(),
          "domain": DOMAIN, "nonce": NONCE, "cases": [], "direct": [], "cleanup": []}


def run(argv, timeout=8):
    p = subprocess.run(argv, text=True, stdout=subprocess.PIPE,
                       stderr=subprocess.PIPE, timeout=timeout)
    return {"argv": argv, "status": p.returncode, "stdout": p.stdout,
            "stderr": p.stderr}


def state(label):
    return run(["/bin/launchctl", "print", f"{DOMAIN}/{label}"])


def events(path):
    if not path.exists():
        return []
    return [json.loads(line) for line in path.read_text().splitlines() if line]


def observe(label, log, predicate, max_seconds):
    deadline = time.monotonic() + max_seconds
    samples = []
    while time.monotonic() < deadline:
        ev = events(log)
        sample = state(label)
        samples.append({"at": time.time(), "events": ev, "state": sample})
        if predicate(ev, sample):
            break
        time.sleep(0.25)
    return samples


def plist_for(label, mode, log):
    return {"Label": label, "ProgramArguments": ["/usr/bin/caffeinate", "-is",
            "/usr/bin/python3", str(CHILD), mode, str(log)],
            "RunAtLoad": True, "KeepAlive": {"SuccessfulExit": False},
            "ThrottleInterval": 3, "StandardOutPath": str(ROOT / f"{mode}.stdout"),
            "StandardErrorPath": str(ROOT / f"{mode}.stderr")}


def case(mode):
    label = f"com.lip.supervision.probe.{mode}.{NONCE}"
    log = ROOT / f"{mode}.events.jsonl"
    plist = ROOT / f"{mode}.plist"
    data = plist_for(label, mode, log)
    with plist.open("wb") as f:
        plistlib.dump(data, f)
    item = {"mode": mode, "label": label, "plist": str(plist),
            "plist_content": data, "before": state(label),
            "plutil": run(["/usr/bin/plutil", "-lint", str(plist)])}
    RESULT["cases"].append(item)
    if item["before"]["status"] == 0:
        item["error"] = "unique label unexpectedly loaded; refused to bootstrap"
        return
    if item["plutil"]["status"] != 0:
        item["error"] = "plist invalid"
        return
    try:
        item["bootstrap"] = run(["/bin/launchctl", "bootstrap", DOMAIN, str(plist)])
        if item["bootstrap"]["status"] != 0:
            return
        if mode == "zero":
            item["samples"] = observe(label, log,
                lambda ev, s: any(e["event"] == "exit" for e in ev)
                and "pid =" not in s["stdout"], 5)
            time.sleep(4)
            item["after_wait"] = {"events": events(log), "state": state(label)}
        elif mode == "nonzero":
            item["samples"] = observe(label, log,
                lambda ev, s: sum(e["event"] == "start" for e in ev) >= 2, 17)
            item["after_wait"] = {"events": events(log), "state": state(label)}
        elif mode == "term":
            item["samples"] = observe(label, log,
                lambda ev, s: any(e["event"] == "start" for e in ev), 5)
            item["kill_command"] = run(["/bin/launchctl", "kill", "SIGTERM", f"{DOMAIN}/{label}"])
            item["after_signal"] = observe(label, log,
                lambda ev, s: any(e["event"] == "signal" for e in ev), 3)
            time.sleep(1)
            item["one_second_after_signal"] = {"events": events(log), "state": state(label)}
            item["after_drain"] = observe(label, log,
                lambda ev, s: any(e["event"] == "exit" for e in ev), 5)
            time.sleep(4)
            item["after_wait"] = {"events": events(log), "state": state(label)}
        elif mode == "kill":
            item["samples"] = observe(label, log,
                lambda ev, s: any(e["event"] == "start" for e in ev), 5)
            item["kill_command"] = run(["/bin/launchctl", "kill", "SIGKILL", f"{DOMAIN}/{label}"])
            item["after_kill"] = observe(label, log,
                lambda ev, s: sum(e["event"] == "start" for e in ev) >= 2, 17)
            item["after_wait"] = {"events": events(log), "state": state(label)}
    finally:
        item["bootout"] = run(["/bin/launchctl", "bootout", f"{DOMAIN}/{label}"])
        item["after_bootout"] = state(label)
        RESULT["cleanup"].append({"label": label, "bootout": item["bootout"],
                                  "after_bootout": item["after_bootout"]})


try:
    for mode in ("zero", "nonzero"):
        RESULT["direct"].append(run(["/usr/bin/caffeinate", "-is", "/usr/bin/python3",
            str(CHILD), mode, str(ROOT / f"direct-{mode}.events.jsonl")]))
    for mode in ("zero", "nonzero", "term", "kill"):
        case(mode)
finally:
    RESULT["finished_utc"] = datetime.datetime.now(datetime.timezone.utc).isoformat()
    (ROOT / "raw-result.json").write_text(json.dumps(RESULT, indent=2))
    print(json.dumps({"raw_result": str(ROOT / "raw-result.json"),
                      "direct_statuses": [d["status"] for d in RESULT["direct"]],
                      "cases": [{"mode": c["mode"], "bootstrap": c.get("bootstrap", {}).get("status"),
                                 "bootout": c.get("bootout", {}).get("status")}
                                for c in RESULT["cases"]]}, indent=2))
