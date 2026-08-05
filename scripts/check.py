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
RO_MANIFEST = LIP / "testdata" / "READONLY.sha256"

# harness-spec.md H-TOP-2. Read-only to the harness: it consumes these and adds
# nothing to them. Whole trees, not a file list, so an ADDED file is caught too.
READONLY_TREES = ["go/core", "go/feed", "go/store", "go/cmd/rig"]

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


def readonly_files() -> list[Path]:
    out = []
    for tree in READONLY_TREES:
        out.extend((LIP / tree).rglob("*.go"))
    return sorted(out)


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


def check_readonly() -> list[str]:
    """harness-spec.md H-TOP-2 — the read-only Go trees.

    §9.5 freezes Python. Nothing froze Go, and H-TOP-2 marks four Go trees
    read-only on the strength of an argument, not a check:

        go/core   go/feed   go/store   go/cmd/rig

    Their correctness argument is port-spec §8 — gates 1-7 plus a seven-day
    shadow run **on the artifact that ships**. An edit to any of them silently
    voids the most expensive evidence in the plan, and until now nothing in this
    repository would have noticed. Convention is not a control when the editor
    is an unattended agent.

    Unlike FROZEN this hashes a TREE, so it also catches an added or deleted
    file, which is the shape an agent's mistake actually takes.
    """
    if not RO_MANIFEST.exists():
        return [f"no manifest at {RO_MANIFEST.relative_to(LIP)}; run "
                f"--freeze-readonly once, deliberately, while the trees are "
                f"known good"]
    want = {}
    for line in RO_MANIFEST.read_text().splitlines():
        line = line.strip()
        if line and not line.startswith("#"):
            digest, name = line.split(None, 1)
            want[name] = digest

    got = {str(p.relative_to(LIP)): sha256(p) for p in readonly_files()}
    bad = []
    for name in sorted(set(want) | set(got)):
        if name not in got:
            bad.append(f"{name}: READ-ONLY FILE DELETED (H-TOP-2)")
        elif name not in want:
            bad.append(f"{name}: ADDED to a read-only tree (H-TOP-2)")
        elif got[name] != want[name]:
            bad.append(
                f"{name}: MODIFIED\n"
                f"      was {want[name][:16]}…  now {got[name][:16]}…\n"
                f"      H-TOP-2 marks this tree read-only; editing it voids "
                f"port-spec §8's correctness argument. Revert it.")
    return bad


def freeze_readonly() -> int:
    files = readonly_files()
    if not files:
        print("FATAL: no files found in the read-only trees; refusing to write "
              "an empty manifest")
        return 1
    lines = [
        "# harness-spec.md H-TOP-2 — read-only Go tree checksums.",
        "#",
        "# go/core, go/feed, go/store and go/cmd/rig are consumed by the",
        "# harness and added to by nothing. Their correctness argument is",
        "# port-spec §8, which is evidence about the artifact that ships; an",
        "# edit here voids it. Regenerating this manifest to make a change pass",
        "# is the same gate failure as regenerating FROZEN.sha256.",
        "",
    ]
    for p in files:
        lines.append(f"{sha256(p)}  {p.relative_to(LIP)}")
    RO_MANIFEST.write_text("\n".join(lines) + "\n")
    print(f"froze {len(files)} read-only files -> "
          f"{RO_MANIFEST.relative_to(LIP)}")
    return 0


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
    ap.add_argument("--freeze-readonly", action="store_true",
                    help="(re)generate the H-TOP-2 read-only tree manifest; "
                         "same caveat — this is a tightening mechanism, not an "
                         "escape hatch for a change you already made")
    a = ap.parse_args()
    if a.freeze:
        return freeze()
    if a.freeze_readonly:
        return freeze_readonly()

    failures = 0
    for title, section, fn in (
        ("slop scan", "§9.4", check_slop),
        ("confidence trailers", "§9.1", check_trailers),
        ("frozen artifacts", "§9.5", check_frozen),
        ("read-only Go trees", "H-TOP-2", check_readonly),
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
