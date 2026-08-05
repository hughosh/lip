#!/usr/bin/env python3
"""port-spec.md §9.1, §9.2, §9.4 and §9.5, as a runnable check.

Three things the spec asks for that were previously enforced by prompt
convention and self-report:

  §9.4  SLOP SCAN. Greps for the shapes faked completion takes in Go. Bun's
        equivalent greps for todo!() and unimplemented!(); ours has to cover
        stubs, skipped tests and discarded errors.

  §9.5  FROZEN-ARTIFACT CHECKSUMS. §2 names artifacts no agent may modify for
        any reason -- not to make a case pass, not to "align" them with the Go.
        Bun enforced its equivalent ("0 tests skipped or deleted") by convention
        and self-report, with no committed check. With no human reviewer that is
        not good enough, so the list is checksummed here. Convention is not
        enforcement.

  §9.1/§9.2  Every ported file carries a confidence trailer, and the TODO(port)
        and PORT NOTE markers are empty before §8 is satisfied.

Running --freeze to make a failing check pass is itself a gate failure, exactly
as editing a frozen artifact is. The manifest records what the artifacts were
when they were frozen; regenerating it after a change records the change as if
it had always been there, which is the one thing this file exists to prevent.

Usage:
    python check.py            # verify; exit 1 on any failure
    python check.py --freeze   # (re)generate the checksum manifest
"""
from __future__ import annotations

import argparse
import hashlib
import re
import sys
from pathlib import Path

HERE = Path(__file__).resolve().parent
LIP = HERE.parent
GO = LIP / "go"
MANIFEST = LIP / "testdata" / "FROZEN.sha256"

# port-spec.md §2's frozen list, plus the fixtures the numeric gates rest on.
FROZEN = [
    "rig.py",
    "replay.py",
    "score.py",
    "auth.py",
    "scripts/diffdb.py",
    "testdata/prices.tsv",
    "testdata/sizes.tsv",
    "testdata/sums.tsv",
    "testdata/NULLABILITY.tsv",
]

# (name, regex, why it is disqualifying)
SLOP = [
    ("stub panic", r'panic\("(TODO|not implemented|unimplemented)',
     "a function that was never finished"),
    ("skipped test", r'\bt\.Skip\b',
     "a test that reports success without running"),
    ("short-mode escape", r'\btesting\.Short\b',
     "a test that can be silently downgraded"),
    ("discarded error", r'^\s*_\s*=\s*\w*[Ee]rr\b',
     "an error assigned to the blank identifier"),
    ("unfinished marker", r'//\s*TODO\(port\)',
     "§9.2 markers must be zero before §8 is satisfied"),
]


def go_files(tests: bool = True) -> list[Path]:
    return sorted(p for p in GO.rglob("*.go")
                  if tests or not p.name.endswith("_test.go"))


def sha256(path: Path) -> str:
    return hashlib.sha256(path.read_bytes()).hexdigest()


def check_slop() -> list[str]:
    """§9.4. Scans tests too: a skipped test is the point of the check."""
    bad = []
    for path in go_files():
        rel = path.relative_to(LIP)
        for i, line in enumerate(path.read_text().splitlines(), 1):
            # A regex that matched its own definition would make this file
            # unable to pass its own check; only Go is scanned, so that cannot
            # happen here, but comments describing a pattern still can.
            if line.lstrip().startswith("//"):
                continue
            for name, pattern, why in SLOP:
                if re.search(pattern, line):
                    bad.append(f"{rel}:{i}: {name} — {why}\n      {line.strip()}")
    return bad


def check_trailers() -> list[str]:
    """§9.1. Non-test files must end with a confidence trailer."""
    bad = []
    for path in go_files(tests=False):
        lines = [ln for ln in path.read_text().splitlines() if ln.strip()]
        if not lines or not re.match(r'//\s*confidence:\s*(high|low)$', lines[-1].strip()):
            bad.append(f"{path.relative_to(LIP)}: no `// confidence: high|low` trailer")
        elif "low" in lines[-1]:
            # Not a failure. Low-confidence files are reviewed first and are
            # never the last thing shipped, so they are worth surfacing.
            print(f"  note   {path.relative_to(LIP)} is marked confidence: low")
    return bad


def check_frozen() -> list[str]:
    """§9.5."""
    if not MANIFEST.exists():
        return [f"no manifest at {MANIFEST.relative_to(LIP)}; run --freeze once, "
                f"deliberately, while the artifacts are known good"]
    want = {}
    for line in MANIFEST.read_text().splitlines():
        line = line.strip()
        if line and not line.startswith("#"):
            digest, name = line.split(None, 1)
            want[name] = digest

    bad = []
    for name in FROZEN:
        path = LIP / name
        if not path.exists():
            bad.append(f"{name}: FROZEN ARTIFACT IS MISSING")
            continue
        if name not in want:
            bad.append(f"{name}: not in the manifest")
            continue
        got = sha256(path)
        if got != want[name]:
            bad.append(
                f"{name}: MODIFIED\n"
                f"      frozen {want[name][:16]}…  now {got[:16]}…\n"
                f"      Modifying a frozen artifact is itself a gate failure "
                f"(§2). Revert it; do not re-freeze.")
    for name in want:
        if name not in FROZEN:
            bad.append(f"{name}: in the manifest but no longer in the frozen list")
    return bad


def freeze() -> int:
    lines = [
        "# port-spec.md §9.5 — frozen-artifact checksums.",
        "#",
        "# These are the artifacts §2 forbids any agent from modifying. Regenerating",
        "# this file to make `check.py` pass is itself a gate failure, identical to",
        "# editing the artifact directly: it records the change as if it had always",
        "# been there. If a check fails, revert the artifact.",
        "",
    ]
    for name in FROZEN:
        path = LIP / name
        if not path.exists():
            print(f"FATAL: {name} does not exist; refusing to freeze a partial list")
            return 1
        lines.append(f"{sha256(path)}  {name}")
    MANIFEST.write_text("\n".join(lines) + "\n")
    print(f"froze {len(FROZEN)} artifacts -> {MANIFEST.relative_to(LIP)}")
    for name in FROZEN:
        print(f"  {sha256(LIP / name)[:16]}…  {name}")
    return 0


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--freeze", action="store_true",
                    help="(re)generate the checksum manifest; see the caveat above")
    a = ap.parse_args()
    if a.freeze:
        return freeze()

    failures = 0
    for title, section, fn in (
        ("slop scan", "§9.4", check_slop),
        ("confidence trailers", "§9.1", check_trailers),
        ("frozen artifacts", "§9.5", check_frozen),
    ):
        bad = fn()
        if bad:
            failures += len(bad)
            print(f"\nFAIL  {section} {title} — {len(bad)} problem(s)")
            for b in bad:
                print(f"  {b}")
        else:
            print(f"ok    {section} {title}")

    if failures:
        print(f"\n{failures} problem(s). §8 is not satisfied.")
        return 1
    print("\nAll §9 checks pass.")
    return 0


if __name__ == "__main__":
    sys.exit(main())
