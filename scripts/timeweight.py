#!/usr/bin/env python3
"""Time-weighted LIP score decay. Supersedes the move-anchored decay in
notes/economics.md §3.

WHY THE WEIGHTING CHANGED
-------------------------
Appendix A of rules02112639183.pdf (CFTC filing 2026-02-11, effective
2026-02-28) settles the cadence question:

    "During a Time Period, if the market is open for trading, a snapshot of the
     book ("Snapshot") will be taken once for each second, with the exact time
     of the snapshot drawn from a random uniform distribution on a periodic
     basis."

One snapshot per second at a uniformly random sub-second offset samples TIME
uniformly. The correct weight for a book state is therefore HOW LONG IT
PERSISTED, not that it occurred.

The original decay table was anchored at a reference move ("quote is at the
reference at t=0, no requote, what survives after X?"). That conditions on a
move having just happened, so it is MOVE-weighted. Reference moves are violently
bursty -- 52.6% of them land within 100ms of the previous one but those account
for 0.1% of market time, while 73.1% of market time sits in stretches longer
than 10 minutes with no reference change at all. Move-weighting therefore
overstates decay by roughly an order of magnitude.

MODEL
-----
A bot requotes to the reference with total tick-to-quote latency L. At instant t
its bid rests at ref(t-L). Ticks below the live reference:

    N(t) = max(0, ref(t) - ref(t-L))

max(0, .) because a reference that has fallen BELOW the bot's resting bid leaves
that bid as the highest bid -- it IS the reference, N=0. That case is not free;
it is exactly the adverse-fill case, and it is priced as markout, not here. So
this is the scoring-side number only. `--abs` reports DF^|N| instead as a
pessimistic bracket in which being above the reference is also penalised.

Only gated instants count: a Snapshot with less than two-sided Target Size
liquidity is excluded from scoring entirely.

Usage:
    python scripts/timeweight.py [rig.db]
"""
from __future__ import annotations

import argparse
import bisect
import random
import sqlite3
from collections import Counter, defaultdict
from pathlib import Path

DF = 0.5
LATENCIES_MS = [0, 10, 50, 200, 1_000, 5_000, 60_000, 300_000]
SEED = 20260725


def load(db: Path):
    con = sqlite3.connect(db)
    rows = con.execute(
        "SELECT ticker, ts_ms, ref_yes, ref_no, gate FROM reference "
        "WHERE ts_ms > 0 ORDER BY ticker, ts_ms"
    ).fetchall()
    by = defaultdict(list)
    for tkr, ts, ry, rn, g in rows:
        by[tkr].append((ts, ry, rn, g))
    return by


def multiplier(ev, side_idx, lag_ms, use_abs=False):
    """Exact time integral of DF^N over gated time, by merging the reference
    step function with a copy of itself shifted forward by `lag_ms`."""
    if len(ev) < 2:
        return 0.0, 0
    t_end = ev[-1][0]
    pts = sorted({e[0] for e in ev} | {e[0] + lag_ms for e in ev})
    i = j = 0
    cur = lag = None
    cur_gate = 0
    acc, gated = 0.0, 0
    for k in range(len(pts) - 1):
        t0 = pts[k]
        if t0 >= t_end:
            break
        t1 = min(pts[k + 1], t_end)
        while i < len(ev) and ev[i][0] <= t0:
            cur, cur_gate = ev[i][side_idx], ev[i][3]
            i += 1
        while j < len(ev) and ev[j][0] + lag_ms <= t0:
            lag = ev[j][side_idx]
            j += 1
        if cur is None or lag is None or not cur_gate:
            continue
        d = cur - lag
        n = abs(d) if use_abs else max(0, d)
        acc += DF ** n * (t1 - t0)
        gated += t1 - t0
    return acc, gated


def table_decay(by, use_abs=False):
    print(f"{'requote latency':>16} | {'YES mult':>9} {'NO mult':>9} | {'mean':>7}")
    print("-" * 50)
    for L in LATENCIES_MS:
        ty = tn = 0.0
        gy = gn = 0
        for ev in by.values():
            a, g = multiplier(ev, 1, L, use_abs); ty += a; gy += g
            a, g = multiplier(ev, 2, L, use_abs); tn += a; gn += g
        my = ty / gy if gy else 0.0
        mn = tn / gn if gn else 0.0
        lab = f"{L}ms" if L < 1000 else f"{L // 1000}s"
        print(f"{lab:>16} | {my:>9.4f} {mn:>9.4f} | {(my + mn) / 2:>7.4f}")


def table_signed(by):
    """Monte-Carlo the signed N distribution -- an independent code path from
    the integration above, and the cross-check that it is right."""
    random.seed(SEED)
    idx = {t: ([e[0] for e in ev], ev) for t, ev in by.items() if len(ev) >= 2}
    tkrs = list(idx)

    def ref_at(times, ev, t):
        k = bisect.bisect_right(times, t) - 1
        return (ev[k][1], ev[k][3]) if k >= 0 else (None, 0)

    print(f"\n{'L':>6} {'N<0':>7} {'N=0':>7} {'N=+1':>7} {'N=+2':>7} {'N>=+3':>7}"
          f" | {'clamp':>7} {'abs':>7}")
    print("-" * 62)
    for L in (1_000, 5_000, 60_000, 300_000):
        c = Counter()
        clamp = av = 0.0
        n = 0
        for _ in range(300_000):
            times, ev = idx[random.choice(tkrs)]
            lo, hi = times[0] + L, times[-1]
            if hi <= lo:
                continue
            t = random.randint(lo, hi)
            cur, g = ref_at(times, ev, t)
            lag, _ = ref_at(times, ev, t - L)
            if cur is None or lag is None or not g:
                continue
            d = cur - lag
            c["neg" if d < 0 else "zero" if d == 0 else
              "p1" if d == 1 else "p2" if d == 2 else "p3+"] += 1
            clamp += DF ** max(0, d)
            av += DF ** abs(d)
            n += 1
        f = lambda k: c[k] / n
        print(f"{L // 1000:>5}s {f('neg'):>6.1%} {f('zero'):>6.1%} {f('p1'):>6.1%} "
              f"{f('p2'):>6.1%} {f('p3+'):>6.1%} | {clamp / n:>7.4f} {av / n:>7.4f}")


def table_burst(by):
    """Where market TIME lives versus where reference MOVES live. This is the
    whole reason the two weightings disagree."""
    buckets = [(0, 100), (100, 1_000), (1_000, 10_000), (10_000, 60_000),
               (60_000, 600_000), (600_000, 1 << 60)]
    bt, bc = Counter(), Counter()
    tot_t = 0
    for ev in by.values():
        for a, b in zip(ev, ev[1:]):
            gap = b[0] - a[0]
            if gap <= 0:
                continue
            tot_t += gap
            for k in buckets:
                if k[0] <= gap < k[1]:
                    bt[k] += gap
                    bc[k] += 1
                    break
    tot_m = sum(bc.values())
    print(f"\n{'gap bucket':>18} {'% of moves':>12} {'% of TIME':>11}")
    for lo, hi in buckets:
        lab = f"{lo/1000:g}s-{hi/1000:g}s" if hi < (1 << 60) else f">{lo/1000:g}s"
        print(f"{lab:>18} {bc[(lo,hi)]/tot_m:>11.1%} {bt[(lo,hi)]/tot_t:>10.1%}")

    span = gated = 0
    for ev in by.values():
        if len(ev) < 2:
            continue
        span += ev[-1][0] - ev[0][0]
        for a, b in zip(ev, ev[1:]):
            if a[3]:
                gated += b[0] - a[0]
    if span:
        print(f"\ngated (two-sided Target Size met) share of market-time: "
              f"{gated/span:.1%}")


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("db", nargs="?", type=Path,
                    default=Path(__file__).resolve().parent.parent / "rig.db")
    ap.add_argument("--abs", action="store_true",
                    help="penalise N<0 too (pessimistic bracket)")
    a = ap.parse_args()
    by = load(a.db)
    print(f"db={a.db}  DF={DF}  markets={len(by)}"
          f"{'  [pessimistic |N|]' if a.abs else ''}\n")
    table_decay(by, a.abs)
    table_signed(by)
    table_burst(by)


if __name__ == "__main__":
    main()
