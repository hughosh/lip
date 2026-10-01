"""Start the detached observer (one PID) and, optionally, the read-only ntfy stream.

usage: start_watchers.py <stage> <pid> <log name> <notices name> [ntfy-since-unix]
The ntfy topic is read from ~/.kalshi/env and passed to curl on stdin, never argv or output.
"""
import subprocess, sys
from pathlib import Path

stage, pid, log_name, notices_name = sys.argv[1:5]
since = sys.argv[5] if len(sys.argv) > 5 else None
evid = Path(stage) / "evidence"
scr = Path("/Users/hugh/kek/lip/loop/run/r2-candidate-7-scratch")
obs = subprocess.Popen(
    ["/Users/hugh/kek/.venv/bin/python", "/Users/hugh/kek/lip/notes/first-fill-evidence-2026-09-28/observe_stage.py",
     "--stage", stage, "--pid", pid, "--log", log_name, "--max-minutes", "150"],
    stdout=open(evid / notices_name, "ab"), stderr=subprocess.STDOUT, stdin=subprocess.DEVNULL,
    start_new_session=True)
(scr / f"observer-{pid}.pid").write_text(f"{obs.pid}\n")
print(f"observer pid {obs.pid} -> {evid / notices_name}")
if since:
    topic = None
    for line in Path.home().joinpath(".kalshi/env").read_text().splitlines():
        line = line.strip().removeprefix("export ").strip()
        if line.startswith("NTFY_TOPIC="):
            topic = line.split("=", 1)[1].strip().strip("'\"")
    if not topic:
        raise SystemExit("NTFY_TOPIC missing")
    curl = subprocess.Popen(["/usr/bin/curl", "-sN", "--config", "-"], stdin=subprocess.PIPE,
                            stdout=open(evid / "ntfy-delivery.jsonl", "ab"),
                            stderr=open(scr / "ntfy-curl.err", "ab"), start_new_session=True)
    curl.stdin.write(f'url = "https://ntfy.sh/{topic}/json?since={since}"\n'.encode())
    curl.stdin.close()
    (scr / "ntfy-curl.pid").write_text(f"{curl.pid}\n")
    print(f"ntfy stream pid {curl.pid} since {since} -> {evid / 'ntfy-delivery.jsonl'}")
