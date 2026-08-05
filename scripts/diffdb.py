#!/usr/bin/env python3
"""Differential comparator: Python replay DB versus Go replay DB.

Gate 4 and gate 5 of notes/port-spec.md. This is the terminal acceptance test
for the port, and it is a FROZEN ARTIFACT -- it must not be weakened to make a
run pass. Widening a tolerance, dropping a column, or filtering rows is a gate
failure, not a fix.

`fill` is compared on every column except mid_1m/mid_5m/mid_30m, which are
resolved by a wall-clock task in the live rig and are not a function of the
tape. Comparison is exact: floats are compared by their raw float64 bits, not
with a tolerance, because the port's claim is bit-identity and anything less
would hide exactly the class of bug this exists to catch.

`reference` is compared the same way, on (ts_ms, ticker). Earlier planning
treated it as a soft gate on the theory that Python's insertion-ordered dict
summation could not be reproduced in Go; core.Levels reproduces it, so this is
a hard gate too.

Usage:
    python diffdb.py PYTHON.db GO.db
"""
from __future__ import annotations

import struct
import sqlite3
import sys

FILL_KEY = "trade_id"
FILL_COLS = [
    "ts_ms", "ticker", "resting_side", "price", "size", "taker_side",
    "trade_through", "pre_best_yes", "pre_best_no", "pre_yes_size",
    "pre_no_size", "pre_mid", "pre_spread", "depth_at_price", "book_lag_ms",
    "source",
]
REF_KEY = ["ts_ms", "ticker"]
REF_COLS = ["ref_yes", "ref_no", "gate", "yes_depth", "no_depth", "target"]


def bits(v):
    """Canonicalise a value so float comparison is by exact bit pattern."""
    if isinstance(v, float):
        return ("f", struct.pack(">d", v))
    return v


def load(path: str, table: str, key: list[str], cols: list[str]):
    conn = sqlite3.connect(path)
    conn.row_factory = sqlite3.Row
    rows = {}
    for r in conn.execute(f"SELECT {','.join(key + cols)} FROM {table}"):
        rows[tuple(r[k] for k in key)] = {c: bits(r[c]) for c in cols}
    conn.close()
    return rows


def compare(label: str, a: dict, b: dict, cols: list[str]) -> int:
    only_a = a.keys() - b.keys()
    only_b = b.keys() - a.keys()
    shared = a.keys() & b.keys()

    per_col = {c: 0 for c in cols}
    examples: list[str] = []
    for k in shared:
        for c in cols:
            if a[k][c] != b[k][c]:
                per_col[c] += 1
                if len(examples) < 10:
                    examples.append(f"    {k} {c}: python={a[k][c]!r} go={b[k][c]!r}")

    bad = sum(per_col.values()) + len(only_a) + len(only_b)
    print(f"\n{label}")
    print(f"  python rows {len(a):,}   go rows {len(b):,}   shared {len(shared):,}")
    if only_a:
        print(f"  ONLY IN PYTHON: {len(only_a):,}  e.g. {sorted(only_a)[:3]}")
    if only_b:
        print(f"  ONLY IN GO:     {len(only_b):,}  e.g. {sorted(only_b)[:3]}")
    mism = {c: n for c, n in per_col.items() if n}
    if mism:
        print(f"  COLUMN MISMATCHES: {mism}")
        print("\n".join(examples))
    if not bad:
        print(f"  0 mismatches across {len(cols)} columns")
    return bad


def main() -> int:
    if len(sys.argv) != 3:
        print(__doc__)
        return 2
    py, go = sys.argv[1], sys.argv[2]

    bad = compare("fill", load(py, "fill", [FILL_KEY], FILL_COLS),
                  load(go, "fill", [FILL_KEY], FILL_COLS), FILL_COLS)
    bad += compare("reference", load(py, "reference", REF_KEY, REF_COLS),
                   load(go, "reference", REF_KEY, REF_COLS), REF_COLS)

    print()
    if bad:
        print(f"GATE FAILED: {bad:,} discrepancies")
        return 1
    print("GATE PASSED: byte-identical on every compared column")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
