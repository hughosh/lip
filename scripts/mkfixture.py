#!/usr/bin/env python3
"""Emit a golden fixture pinning CPython's price/size numeric conversions.

The Go port must reproduce `round(float(p) * 100)` and `float(s)` bit-for-bit.
Python's round() is banker's rounding on the EXACT binary value, so the fixture
records both the integer result and the raw float64 bits of the product; a Go
implementation that matches both has the same primitive, not merely the same
answers on this sample.
"""
import gzip
import json
import struct
import sys
from pathlib import Path

LIP = Path("/Users/hugh/kek/lip")


def frames(path):
    dec = json.JSONDecoder()
    buf, pos = "", 0
    fh = gzip.open(path, "rt", encoding="utf-8")
    while True:
        try:
            chunk = fh.read(1 << 20)
        except Exception:
            chunk = ""          # truncated tail of a tape still being written
        if chunk:
            buf = buf[pos:] + chunk
            pos = 0
        while True:
            while pos < len(buf) and buf[pos] in " \t\r\n":
                pos += 1
            if pos >= len(buf):
                break
            try:
                obj, pos = dec.raw_decode(buf, pos)
            except ValueError:
                break
            yield obj
        if not chunk:
            return


prices: set[str] = set()
sizes: set[str] = set()

for tape in sorted(LIP.glob("raw-*.jsonl.gz")) + sorted(LIP.glob("archive/raw-*.jsonl.gz")):
    for env in frames(tape):
        m = env.get("m")
        if not isinstance(m, dict):
            continue
        t = m.get("type")
        msg = m.get("msg") or {}
        if t == "orderbook_delta":
            prices.add(msg["price_dollars"])
            sizes.add(msg["delta_fp"])
        elif t == "orderbook_snapshot":
            for k in ("yes_dollars_fp", "no_dollars_fp"):
                for p, s in (msg.get(k) or []):
                    prices.add(p)
                    sizes.add(s)
        elif t == "trade":
            prices.add(msg["yes_price_dollars"])
            prices.add(msg["no_price_dollars"])
            sizes.add(msg["count_fp"])

# Synthetic adversarial cases: every 4-decimal string whose cent value is an
# exact or near half, plus the classic banker's-rounding tripwires. The tape
# only proves what the exchange happened to send today.
for i in range(0, 10000, 5):
    prices.add(f"{i / 10000:.4f}")
for extra in ("0.4850", "0.4750", "0.0050", "0.9950", "0.5000", "1.0000", "0.0000"):
    prices.add(extra)

out = LIP / "testdata"
out.mkdir(exist_ok=True)

with (out / "prices.tsv").open("w") as fh:
    fh.write("# price_string\tround(float(p)*100)\tfloat64_bits_of_product\n")
    for p in sorted(prices):
        v = float(p) * 100
        fh.write(f"{p}\t{round(v)}\t{struct.pack('>d', v).hex()}\n")

with (out / "sizes.tsv").open("w") as fh:
    fh.write("# size_string\tfloat64_bits_of_float(s)\n")
    for s in sorted(sizes):
        fh.write(f"{s}\t{struct.pack('>d', float(s)).hex()}\n")

# --- summation ----------------------------------------------------------
#
# CPython's sum() uses NEUMAIER COMPENSATED summation on floats, not naive
# left-to-right addition, and the two disagree by one ULP on realistic
# two-decimal book sizes. Pin it: each row is a list of sizes and the exact
# float64 bits CPython's sum() produces for it, in that order.
import random

random.seed(20260724)
cases: list[list[float]] = [
    [],
    [1.0],
    [0.1, 0.2, 0.3],
    [1e16, 1.0, 1.0, -1e16],       # naive gives 0.0, compensated gives 2.0
    [1003.0, 513.0, 140.0, 1.0, 2.0, 1.0, 1.0, 1882.0],  # a real snapshot side
]
for _ in range(2000):
    n = random.randint(1, 80)
    cases.append([round(random.uniform(0.01, 20000), 2) for _ in range(n)])

with (out / "sums.tsv").open("w") as fh:
    fh.write("# comma_separated_values\tfloat64_bits_of_sum(values)\n")
    for vals in cases:
        fh.write(",".join(repr(v) for v in vals) + "\t"
                 + struct.pack(">d", sum(vals)).hex() + "\n")


def _naive(vals):
    f = 0.0
    for v in vals:
        f += v
    return f


discriminating = sum(1 for c in cases if _naive(c) != sum(c))
print(f"sums    {len(cases):,}  of which naive-summation would get wrong: {discriminating}")

# How many of these actually discriminate banker's rounding from half-away?
import math
disagree = [p for p in prices
            if round(float(p) * 100) != int(math.copysign(math.floor(abs(float(p) * 100) + 0.5),
                                                          float(p) * 100))]
print(f"prices  {len(prices):,}  sizes {len(sizes):,}")
print(f"prices where banker's != half-away-from-zero: {len(disagree)}")
print("  e.g.", sorted(disagree)[:8])
