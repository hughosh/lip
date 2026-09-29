#!/usr/bin/python3
"""Disposable, account-free launchd child for the supervision probe."""
import json
import os
import signal
import sys
import time

mode, log_path = sys.argv[1:3]
start = time.monotonic()
term_at = None


def record(event, **fields):
    with open(log_path, "a", encoding="utf-8") as f:
        f.write(json.dumps({"event": event, "mode": mode, "pid": os.getpid(),
                            "ppid": os.getppid(), "at": time.time(), **fields}) + "\n")
        f.flush()
        os.fsync(f.fileno())


def on_term(signum, _frame):
    global term_at
    term_at = time.monotonic()
    record("signal", signum=signum, signal_name=signal.Signals(signum).name)


signal.signal(signal.SIGTERM, on_term)
record("start", argv=sys.argv, elapsed=0)
if mode == "zero":
    record("exit", status=0)
    sys.exit(0)
if mode == "nonzero":
    record("exit", status=17)
    sys.exit(17)
if mode in ("term", "kill"):
    while True:
        if mode == "term" and term_at is not None and time.monotonic() - term_at >= 3:
            record("exit", status=0, seconds_after_signal=round(time.monotonic() - term_at, 3))
            sys.exit(0)
        if time.monotonic() - start > 25:
            record("timeout_exit", status=0)
            sys.exit(0)
        time.sleep(0.05)
raise SystemExit("unknown mode")
