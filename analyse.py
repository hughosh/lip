#!/usr/bin/env python3
"""What does resting at the touch actually earn, before LIP rewards?

SIGN CONVENTION: every number is cents per contract accruing to the RESTING
side. Positive = the passive order made money. Adverse selection shows up as
a negative number.

    resting yes at p   ->  markout_h = mid_h - p
    resting no  at n   ->  markout_h = (100 - mid_h) - n

THE DECOMPOSITION THAT MATTERS

Raw markout is not edge. At the instant of the fill the resting order is
already "up" by roughly half the spread, purely because the mid sits between
the bids. On a 1-tick book that is +0.5c by construction and says nothing
about whether the trade was good.

    markout_0  = pre_mid - price      the half-spread you are handed
    drift_h    = markout_h - markout_0    what the market then does to you

`drift_h` IS the adverse selection. `markout_h` is what you keep. Reporting
only the second is how a passive strategy looks profitable right up until it
isn't.

TWO POPULATIONS, NEVER POOL THEM

  trade_through = 1   the aggressor cleared the level, so an order resting
                      there necessarily filled. No queue assumption.
  trade_through = 0   a print AT the touch. Whether YOUR order filled depends
                      on queue position, which public data cannot see.

The confirmed population is the honest one. The full population is an upper
bound on fill count and its selection bias has no known sign.

EFFECTIVE SAMPLE SIZE

Fills from one sweep are one event, not many. Standard errors here are
computed over sweeps (ticker + taker side + 1s bucket), not over rows, because
treating 800 contracts from a single aggressor as 800 observations overstates
confidence by roughly the square root of the cluster size.
"""
from __future__ import annotations

import argparse
import math
import sqlite3
from pathlib import Path

DB_PATH = Path(__file__).parent / "rig.db"

HORIZONS = ("1m", "5m", "30m")

# markout to the resting side, in cents
MARKOUT = ("CASE WHEN resting_side='yes' THEN {m} - {p}"
           " ELSE (100 - {m}) - {p} END")

# The price the trade actually printed at.
def _mk(h: str) -> str:
    return MARKOUT.format(m=f"mid_{h}", p="price")


PRE = MARKOUT.format(m="pre_mid", p="price")

# The pre-trade touch on the resting side. When a sweep prints BELOW the touch,
# the trade-through fact is evidence about an order resting at the TOUCH, not
# at the deeper price the print landed on. Any "would my quote at the touch
# have filled, and what would it have earned" question must use these.
TOUCH = "CASE WHEN resting_side='yes' THEN pre_best_yes ELSE pre_best_no END"


def _mk_touch(h: str) -> str:
    return MARKOUT.format(m=f"mid_{h}", p=f"({TOUCH})")


PRE_TOUCH = MARKOUT.format(m="pre_mid", p=f"({TOUCH})")


def _stats(rows: list[tuple[float, float]]) -> tuple[int, float, float]:
    """(n, size-weighted mean, cluster-robust standard error).

    The SE uses SQUARED weights. Var of a weighted mean is
    sum(w_i^2 (v_i - mean)^2) / (sum w_i)^2, not sum(w_i * resid^2)/sum(w)/n —
    the latter understates uncertainty whenever cluster sizes vary, which here
    they do by three orders of magnitude (a 1-lot print next to an 800-lot
    sweep).
    """
    if not rows:
        return 0, float("nan"), float("nan")
    n = len(rows)
    tot_w = sum(w for _, w in rows)
    if tot_w <= 0:
        return n, float("nan"), float("nan")
    mean = sum(v * w for v, w in rows) / tot_w
    if n < 2:
        return n, mean, float("nan")
    var = (n / (n - 1)) * sum((w * (v - mean)) ** 2 for v, w in rows) / tot_w ** 2
    return n, mean, math.sqrt(var)


def _clustered(conn: sqlite3.Connection, expr: str, where: str) -> tuple[int, float, float]:
    """Aggregate `expr` to one observation per sweep, then summarise.

    A sweep is one aggressor walking the book: same market, same taker side,
    same second. Its fills share a single information event.
    """
    sql = f"""
        SELECT SUM(({expr}) * size) / SUM(size) AS v, SUM(size) AS w
        FROM fill
        WHERE {where}
        GROUP BY ticker, taker_side, ts_ms / 1000
    """
    return _stats([(v, w) for v, w in conn.execute(sql) if v is not None])


def _line(label: str, n: int, mean: float, se: float) -> str:
    if n == 0 or mean != mean:
        return f"  {label:<26}{'-':>8}{'-':>12}{'-':>12}"
    t = f"{mean/se:+.1f}" if se == se and se > 0 else "-"
    return f"  {label:<26}{n:>8,}{mean:>+12.3f}{se:>12.3f}   t={t}"


def report(conn: sqlite3.Connection) -> None:
    cur = conn.cursor()
    n, first, last, mkts, tt = cur.execute(
        "SELECT COUNT(*), MIN(ts_ms), MAX(ts_ms), COUNT(DISTINCT ticker),"
        " SUM(trade_through) FROM fill"
    ).fetchone()
    if not n:
        print("No fills recorded yet. Run rig.py first.")
        return
    span_h = (last - first) / 3_600_000.0
    sweeps = cur.execute(
        "SELECT COUNT(*) FROM (SELECT 1 FROM fill"
        " GROUP BY ticker, taker_side, ts_ms/1000)"
    ).fetchone()[0]

    print(f"{n:,} passive executions | {mkts} markets | {span_h:.2f}h")
    print(f"{sweeps:,} independent sweeps (the real sample size)")
    print(f"trade-through confirmed: {tt:,} ({tt/n:.1%})")

    print("\n" + "=" * 74)
    print("SPREAD CAPTURE AT THE FILL  (what you are handed, not what you keep)")
    print("=" * 74)
    print(f"  {'population':<26}{'sweeps':>8}{'cents':>12}{'stderr':>12}")
    for label, w in (("all prints", "pre_mid IS NOT NULL"),
                     ("trade-through only", "pre_mid IS NOT NULL AND trade_through=1")):
        print(_line(label, *_clustered(conn, PRE, w)))

    for h in HORIZONS:
        print("\n" + "=" * 74)
        print(f"HORIZON +{h}")
        print("=" * 74)
        base = f"mid_{h} IS NOT NULL AND pre_mid IS NOT NULL"
        conf = base + " AND trade_through=1"
        touched = conf + f" AND ({TOUCH}) IS NOT NULL"
        print(f"  {'':<26}{'sweeps':>8}{'cents':>12}{'stderr':>12}")
        print(_line("markout, all prints", *_clustered(conn, _mk(h), base)))
        # Priced at the TOUCH, which is the level the trade-through evidence is
        # actually about — this is the number a quoting policy would realise.
        print(_line("markout @touch, confirmed",
                    *_clustered(conn, _mk_touch(h), touched)))
        print(_line("ADVERSE DRIFT, all",
                    *_clustered(conn, f"(({_mk(h)}) - ({PRE}))", base)))
        print(_line("ADVERSE DRIFT @touch, conf",
                    *_clustered(conn, f"(({_mk_touch(h)}) - ({PRE_TOUCH}))", touched)))

    # Queue depth is the lever a real quoting policy controls: join a thin
    # touch and you are near the front, join a fat one and you are behind size
    # that has to be eaten before you trade at all.
    print("\n" + "=" * 74)
    print("ADVERSE DRIFT +5m BY DEPTH RESTING AT THE FILL PRICE")
    print("=" * 74)
    base = "mid_5m IS NOT NULL AND pre_mid IS NOT NULL"
    drift = f"(({_mk('5m')}) - ({PRE}))"
    print(f"  {'depth at price':<26}{'sweeps':>8}{'cents':>12}{'stderr':>12}")
    for label, cond in (("< 10", "depth_at_price < 10"),
                        ("10 - 100", "depth_at_price >= 10 AND depth_at_price < 100"),
                        ("100 - 1000", "depth_at_price >= 100 AND depth_at_price < 1000"),
                        (">= 1000", "depth_at_price >= 1000")):
        print(_line(label, *_clustered(conn, drift, f"{base} AND {cond}")))

    print("\n" + "=" * 74)
    print("ADVERSE DRIFT +5m BY SWEEP SIZE  (bigger aggressor = better informed?)")
    print("=" * 74)
    print(f"  {'sweep contracts':<26}{'sweeps':>8}{'cents':>12}{'stderr':>12}")
    for label, cond in (("< 10", "size < 10"),
                        ("10 - 100", "size >= 10 AND size < 100"),
                        ("100 - 1000", "size >= 100 AND size < 1000"),
                        (">= 1000", "size >= 1000")):
        print(_line(label, *_clustered(conn, drift, f"{base} AND {cond}")))

    print("\n" + "=" * 74)
    print("REFERENCE DRIFT  (what strands a quote and zeroes its LIP score)")
    print("=" * 74)
    rows = cur.execute(
        "SELECT ticker, COUNT(*) moves, MAX(ref_yes)-MIN(ref_yes) rng"
        " FROM reference WHERE ref_yes IS NOT NULL"
        " GROUP BY ticker ORDER BY moves DESC LIMIT 10"
    ).fetchall()
    print(f"  {'market':<44}{'moves':>8}{'tick range':>12}")
    for tkr, moves, rng in rows:
        print(f"  {tkr[:44]:<44}{moves:>8,}{rng:>12}")

    gate = cur.execute("SELECT AVG(gate) FROM reference WHERE gate IS NOT NULL").fetchone()[0]
    if gate is not None:
        print(f"\n  book states meeting the Target Size gate: {gate:.1%}")
        print("  (snapshots below the gate score nothing for anyone)")

    print("\n" + "=" * 74)
    print("HOW TO READ THIS")
    print("=" * 74)
    print("""  Net edge per contract = spread capture + adverse drift + LIP reward share.
  The first is handed to you, the second is taken back, the third is capped by
  the pot and shrinks as you add size. If |adverse drift| exceeds spread
  capture, quoting loses money before rewards and the reward has to cover the
  gap. Judge significance on the sweep count and t-stat, not the fill count.""")


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("--db", type=Path, default=DB_PATH)
    a = ap.parse_args()
    conn = sqlite3.connect(a.db)
    report(conn)
    conn.close()
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
