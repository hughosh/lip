"""Read-only: order ids whose LAST sweep verdict was not clean and that no later sweep cleared.

Each is a possible o.pending phantom (acked, never listed, cancel unverified). A planned stop
cannot drain while one exists. usage: phantoms.py <stage dir> [log name ...]
"""
import json, sys
from pathlib import Path

stage = Path(sys.argv[1])
logs = sys.argv[2:] or ["harness.log"]
last = {}
for name in logs:
    path = stage / "evidence" / name
    if not path.exists():
        continue
    for line in path.read_text().splitlines():
        if line.startswith("harness: sweep-trace "):
            t = json.loads(line[len("harness: sweep-trace "):])
            still = set(t.get("still_resting") or [])
            for oid in t.get("requested") or []:
                last[oid] = (t["started"], name, "unverified" if oid in still else "cleared")
bad = {k: v for k, v in last.items() if v[2] == "unverified"}
print(json.dumps({"swept_ids": len(last), "phantom_candidates": bad}, indent=1))
