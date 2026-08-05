#!/usr/bin/env python3
"""V5 of notes/harness-spec.md §17: the negative control -- the gate on the gate.

A gate that has never been shown to fail is not evidence. This deliberately
breaks the harness, one invariant at a time, and checks that a NAMED test
notices. Any mutation that survives every gate is a defect in the verification,
and the harness does not ship until the gate is strengthened -- unless the
mutation is positively established to be behaviourally inert, argued explicitly
and never assumed.

Each mutation is applied to a pristine copy of lip/go, so runs cannot
contaminate each other or the working tree. A replacement that fails to apply is
a hard error: a no-op mutation would otherwise be silently recorded as
"survived", which is the exact false-negative this script exists to prevent.

Mutations must be SEMANTIC. A mutation caught only because it left an import
unused tests the Go compiler, not the gate -- so where a mutation needs a new
import, it patches the import block too, and the resulting tree must COMPILE.
A mutation that fails to build is reported as such and does not count as caught.

Usage:
    python harness_negative_control.py [--out ../notes/harness-negative-control.md]
                                       [--only m01,m14]
"""
from __future__ import annotations

import argparse
import re
import shutil
import subprocess
import sys
import tempfile
from pathlib import Path

HERE = Path(__file__).resolve().parent
LIP = HERE.parent
GO_SRC = LIP / "go"
GO_BIN = "/usr/local/bin/go"

PKGS = ["./harness/...", "./cmd/harness/..."]

# (id, mutation name, [(relpath, old, new), ...], expected catching test)
#
# Every `old` must appear EXACTLY ONCE in its file.
MUTATIONS = [
    ("M1",
     "break the monitor loop when global state leaves RUNNING "
     "-- probebot.py's exact defect",
     [
         ("cmd/harness/monitor.go",
          '\t"lip/harness/risk"\n',
          '\t"lip/harness/quote"\n\t"lip/harness/risk"\n'),
         ("cmd/harness/monitor.go",
          "\t\t\tnow := m.now()\n\t\t\tsnap := m.src.Load()\n",
          "\t\t\tnow := m.now()\n\t\t\tsnap := m.src.Load()\n"
          "\t\t\tif snap != nil && snap.Global != quote.Running {\n"
          "\t\t\t\treturn\n\t\t\t}\n"),
     ],
     "TestM1_MonitorKeepsSamplingAfterGlobalStateLeavesRunning"),

    ("M14",
     "revert A5 to row freshness instead of source advancement "
     "-- fresh rows about a frozen world",
     [
         ("harness/risk/invariant.go",
          "\tif res.Stale {\n\t\treturn\n\t}\n",
          "\t// M14: a row was written, so call it fresh.\n"),
     ],
     "TestM14_FrozenOwnerPublicationIsReportedStale"),

    ("M14b",
     "monitor never marks a frozen source stalled "
     "-- the SEV1 OWNER_STALLED path is removed",
     [
         ("harness/risk/monitor.go",
          "\t} else if now-m.lastAdvance >= stallAfter {\n\t\tm.stalled = true\n",
          "\t} else if false {\n\t\tm.stalled = true\n"),
     ],
     "TestM14_FrozenOwnerPublicationIsReportedStale"),

    ("M7",
     "drop the size_R cap so a reducing fill can overshoot past flat",
     [
         ("harness/num/qty.go",
          "\tif derivedFrom != 0 && count > derivedFrom.Abs() {",
          "\tif false {"),
     ],
     "TestValidateCountRejectsOvershoot"),

    # Expected to SURVIVE, and that is NOT a verification defect. Argued, not
    # assumed, per §17 V5: `float64(q)/100 == 0.0` is equivalent to `q == 0`
    # for every reachable q. Qty is an int64; float64 represents every int64 of
    # magnitude < 2^53 exactly, and the smallest nonzero |Qty| of 1 maps to
    # 0.01 -- eleven orders of magnitude above float64's resolution near zero.
    # Verified exhaustively over q in +/-5,000,000 plus the +/-2^52 and
    # +/-(2^53 - 1) extremes: zero divergences.
    #
    # HR-026's residue arises from ACCUMULATING fractional contracts in
    # float64, not from comparing an already-quantized value. Qty forecloses
    # that by construction: there is no float64 accumulator to leave a residue
    # in, and a mutation that reintroduced one would be a type change that does
    # not compile. That the invariant is enforced by the type system rather
    # than by a test is the stronger outcome, and this row records that it was
    # checked rather than assumed.
    ("M26",
     "compare a quantized quantity as float64 instead of the exact quantum",
     [
         ("harness/num/qty.go",
          "func (q Qty) IsFlat() bool { return q == 0 }",
          "func (q Qty) IsFlat() bool { return q.Float() == 0.0 }"),
     ],
     "inert"),

    ("M26a",
     "quantize by truncation instead of half-away-from-zero "
     "-- a sub-quantum size silently becomes flat",
     [
         ("harness/num/qty.go",
          "\t\treturn Qty(math.Floor(scaled + 0.5))",
          "\t\treturn Qty(math.Floor(scaled))"),
     ],
     "TestQtyRoundTripAndFormat"),
]


def run(cmd: list[str], cwd: Path) -> tuple[int, str]:
    p = subprocess.run(cmd, cwd=cwd, capture_output=True, text=True)
    return p.returncode, p.stdout + p.stderr


def failed_tests(output: str) -> list[str]:
    return sorted(set(re.findall(r"^--- FAIL: (\S+)", output, re.M)))


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__.split("\n")[0])
    ap.add_argument("--out", default=str(LIP / "notes" /
                                         "harness-negative-control.md"))
    ap.add_argument("--only", default="")
    a = ap.parse_args()
    only = {x.strip().upper() for x in a.only.split(",") if x.strip()}

    # An --only value that matches nothing selects ZERO mutations, and a run of
    # zero mutations trivially "passes". That is the same false-green this
    # script exists to detect, reachable by a typo. Reject it rather than
    # reporting success for having checked nothing (H-PAGE-1a's lesson:
    # "absence is not evidence" reappearing as a spelling mistake).
    known = {m[0].upper() for m in MUTATIONS}
    if only:
        unknown = only - known
        if unknown:
            print(f"unknown mutation id(s): {sorted(unknown)}\n"
                  f"known: {sorted(known)}", file=sys.stderr)
            return 2

    # Baseline: the pristine tree must be green, or nothing below means
    # anything. A mutation "caught" by an already-red suite is not evidence.
    code, out = run([GO_BIN, "test", "-count=1", *PKGS], GO_SRC)
    if code != 0:
        print("BASELINE IS RED -- refusing to run mutations.\n" + out[-3000:],
              file=sys.stderr)
        return 1
    print("baseline: green")

    rows = []
    for mid, name, patches, expected in MUTATIONS:
        if only and mid.upper() not in only:
            continue
        with tempfile.TemporaryDirectory() as td:
            dst = Path(td) / "go"
            shutil.copytree(GO_SRC, dst)
            for rel, old, new in patches:
                f = dst / rel
                src = f.read_text()
                n = src.count(old)
                if n != 1:
                    print(f"{mid}: anchor appears {n}x in {rel}, need exactly 1",
                          file=sys.stderr)
                    return 1
                f.write_text(src.replace(old, new))

            build, bout = run([GO_BIN, "build", "./..."], dst)
            if build != 0:
                rows.append((mid, name, expected, "DID NOT BUILD",
                             bout.strip().splitlines()[:2], False))
                print(f"!! {mid}: DID NOT BUILD (does not count as caught)")
                continue

            code, out = run([GO_BIN, "test", "-count=1", *PKGS], dst)
            fails = failed_tests(out)
            caught = code != 0
            if expected == "inert":
                # An inert mutation is CORRECT to survive. It is kept in the
                # suite because "we checked, and it genuinely cannot matter" is
                # a finding worth re-verifying whenever the code around it
                # changes -- and because an inert declaration is exactly how a
                # real gate hole would hide.
                ok = not caught
                status = "inert, as expected" if ok else "NOT INERT"
            else:
                ok = caught and expected in fails
                status = "caught" if caught else "SURVIVED"
                if caught and not ok:
                    status = "caught, but NOT by the named test"
            rows.append((mid, name, expected, status, fails, ok))
            print(f"{'ok' if ok else '!!'} {mid}: {status} -> {fails}")

    lines = [
        "# V5 — negative control for the harness",
        "",
        "Generated by `scripts/harness_negative_control.py`. Each row is a",
        "deliberate violation of one invariant from `harness-spec.md` §17 V3,",
        "applied to a pristine copy of `lip/go`, then run through the harness",
        "test suite.",
        "",
        "A mutation that SURVIVES is a defect in the verification, not a",
        "curiosity. A mutation that DID NOT BUILD does not count as caught —",
        "the Go compiler is not a gate.",
        "",
        "| id | mutation | expected catching test | result | tests that failed |",
        "|---|---|---|---|---|",
    ]
    for mid, name, expected, status, fails, ok in rows:
        f = ", ".join(fails) if isinstance(fails, list) else str(fails)
        lines.append(f"| {mid} | {name} | `{expected}` | **{status}** | "
                     f"{f or '—'} |")
    good = sum(1 for r in rows if r[5])
    lines += ["",
              f"**{good} of {len(rows)} mutations produced their expected "
              f"outcome.**",
              "",
              "A mutation that SURVIVES unexpectedly is a defect in the",
              "verification and the harness does not ship until the gate is",
              "strengthened. A mutation declared `inert` must carry a positive",
              "argument in `harness_negative_control.py` — never an assumption,",
              "because an inert declaration is exactly how a real gate hole",
              "would hide.",
              ""]
    # A partial run must not overwrite the canonical report. Otherwise a
    # `--only m01` run silently replaces the record of all 23 mutations with a
    # record of one, and the file that documents the verification becomes the
    # place the verification disappears.
    if only:
        out_path = Path(a.out).with_suffix(".partial.md")
        print(f"\npartial run ({sorted(only)}) -- writing {out_path.name}, "
              f"not the canonical report")
    else:
        out_path = Path(a.out)
    out_path.write_text("\n".join(lines) + "\n")
    print(f"\nwrote {out_path}")

    # THE EXIT CODE. This function previously returned 0 unconditionally: it
    # counted `good`, printed "!! SURVIVED", wrote the report -- and exited
    # success. Every automated caller therefore read a surviving mutation, which
    # is by definition a hole in the verification, as a passing gate.
    #
    # That is this repository's own thesis turned on itself. §17 V5 exists
    # because "a gate that has never been shown to fail is not evidence", and
    # the gate on the gate could not fail. Found by an independent model reading
    # the source, not by any test here.
    bad = [r for r in rows if not r[5]]
    if bad:
        print(f"\n{len(bad)} of {len(rows)} mutations did NOT produce their "
              f"expected outcome:", file=sys.stderr)
        for mid, name, expected, status, fails, _ in bad:
            print(f"  {mid}: {status} (expected to be caught by {expected})",
                  file=sys.stderr)
        print("\nA mutation that SURVIVES is a defect in the VERIFICATION. The "
              "harness does not ship until the gate is strengthened.",
              file=sys.stderr)
        return 1

    # A full run must have exercised every mutation in the catalogue. A
    # mutation that quietly vanishes from MUTATIONS takes its coverage with it
    # and leaves the count looking healthy.
    if not only and len(rows) != len(MUTATIONS):
        print(f"\nexpected {len(MUTATIONS)} mutations, ran {len(rows)}",
              file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
