"""Account-free named watchdog mutations, copied into disposable directories."""
import datetime
import hashlib
import json
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile

ROOT = Path(__file__).resolve().parents[2]
OUT = Path(__file__).resolve().parent / ("watchdog-mutations-" + datetime.datetime.now(datetime.timezone.utc).strftime("%Y%m%dT%H%M%S%fZ"))
CASES = [
    ("M-OPS-TRANSPORT-IS-ACK", "ops_watchdog.py", 'if item["ack"] is not None:\n                continue', 'if item["ack"] is not None or item["primary"]["sent_ms"] is not None:\n                continue', "test_ops_watchdog.WatchdogTest.test_primary_nonack_pages_backup_at_deadline"),
    ("M-OPS-NO-BACKUP", "ops_watchdog.py", 'if role == "backup" and delivery["attempts"] == 0 and not clock_regressed and now_ms < item["first_seen_ms"] + timeout_ms:', 'if role == "backup":', "test_ops_watchdog.WatchdogTest.test_failed_primary_still_pages_backup_and_retries"),
    ("M-OPS-ATTEMPT-NOT-DURABLE", "ops_watchdog.py", 'delivery["last_attempt_ms"] = now_ms\n                save(path, state)', 'delivery["last_attempt_ms"] = now_ms\n                # mutant omits durable attempt', "test_ops_watchdog.WatchdogTest.test_failed_durable_write_prevents_external_send"),
    ("M-OPS-CLOCK-REWIND", "ops_watchdog.py", 'clock_regressed = now_ms < latest', 'clock_regressed = False', "test_ops_watchdog.WatchdogTest.test_clock_rewind_pages_backup_and_retries_failed_primary"),
    ("M-OPS-UNJOURNALED-SEV1", "ops_watchdog_source.py", 'FROM anomaly WHERE sev = ? ORDER BY', 'FROM anomaly WHERE sev = ? AND journaled_ms IS NOT NULL ORDER BY', "test_ops_watchdog_source.SourceAdapterTest.test_imports_committed_sev1_even_if_unjournaled_or_delivered"),
]


def run(temp, names, label):
    script = """import json, sys, unittest
suite = unittest.TestSuite(unittest.defaultTestLoader.loadTestsFromName(name) for name in sys.argv[1:])
result = unittest.TextTestRunner(verbosity=2).run(suite)
print(json.dumps({'tests': result.testsRun, 'failures': len(result.failures), 'errors': len(result.errors), 'skipped': len(result.skipped)}))
"""
    result = subprocess.run([sys.executable, "-c", script, *["scripts." + n for n in names]], cwd=temp,
                            env={"PATH": "/usr/bin:/bin", "PYTHONDONTWRITEBYTECODE": "1"}, capture_output=True, text=True, timeout=30)
    (OUT / (label + ".log")).write_text(result.stderr + result.stdout)
    if result.returncode:
        return {"verdict": "INCONCLUSIVE", "exit_code": result.returncode}
    counts = json.loads(result.stdout.strip().splitlines()[-1])
    return counts


def main():
    OUT.mkdir()
    sources = ["ops_watchdog.py", "ops_watchdog_source.py", "test_ops_watchdog.py", "test_ops_watchdog_source.py"]
    receipt = {"started_at_utc": datetime.datetime.now(datetime.timezone.utc).isoformat(),
               "scope": "Named exact Python assertions; no network/account calls, only injected senders and disposable databases", "mutations": [],
               "sources": {name: hashlib.sha256((ROOT / "scripts" / name).read_bytes()).hexdigest() for name in sources}}
    with tempfile.TemporaryDirectory(prefix="lip-watchdog-mutations-") as directory:
        temp = Path(directory)
        (temp / "scripts").mkdir()
        (temp / "go/harness/hstore").mkdir(parents=True)
        shutil.copyfile(ROOT / "go/harness/hstore/schema.go", temp / "go/harness/hstore/schema.go")
        for name in sources:
            shutil.copyfile(ROOT / "scripts" / name, temp / "scripts" / name)
        receipt["baseline"] = run(temp, [case[4] for case in CASES], "baseline")
        if receipt["baseline"] != {"tests": len(CASES), "failures": 0, "errors": 0, "skipped": 0}:
            receipt["verdict"] = "INCONCLUSIVE"
        else:
            for name, file, old, new, catcher in CASES:
                original = (ROOT / "scripts" / file).read_text()
                if original.count(old) != 1:
                    row = {"name": name, "verdict": "INCONCLUSIVE", "reason": "anchor count", "count": original.count(old)}
                else:
                    (temp / "scripts" / file).write_text(original.replace(old, new, 1))
                    result = run(temp, [catcher], name)
                    row = {"name": name, "catcher": catcher, "result": result,
                           "verdict": "CAUGHT" if result == {"tests": 1, "failures": 1, "errors": 0, "skipped": 0} else "INCONCLUSIVE_OR_SURVIVED"}
                    (temp / "scripts" / file).write_text(original)
                receipt["mutations"].append(row)
            receipt["verdict"] = "PASS" if all(row["verdict"] == "CAUGHT" for row in receipt["mutations"]) else "FAIL"
    receipt["finished_at_utc"] = datetime.datetime.now(datetime.timezone.utc).isoformat()
    (OUT / "receipt.json").write_text(json.dumps(receipt, indent=2) + "\n")
    print(json.dumps({"verdict": receipt["verdict"], "receipt": str(OUT / "receipt.json")}))
    return 0 if receipt["verdict"] == "PASS" else 1


if __name__ == "__main__":
    sys.exit(main())
