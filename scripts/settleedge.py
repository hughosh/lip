#!/usr/bin/env python
"""Settlement-anchored maker edge, and a test of the pre_mid artifact.

Two things at once.

(A) THE ARTIFACT TEST.  Prior claim 3: "maker edge rises with distance behind
    the touch, -0.49c at touch -> +13.7c at >=12 ticks", measured as
    (pre_mid - price).  But pre_mid is the mid *before* the trade.  A resting
    order 12 ticks behind the touch can only fill if a sweep reaches it, at
    which point pre_mid necessarily sits ~12 ticks above the fill price.  If
    measured edge ~= distance, the claim is a tautology, not an edge.

(B) THE NON-CIRCULAR MEASURE.  For a fill with resting_side=S at price p, the
    maker BOUGHT side S at p cents.  Settlement pays 100 if S is the winning
    outcome, else 0.  So maker P&L per contract = 100*[result==S] - p, with a
    clean efficiency null of E[P&L] = 0.  No quote-derived mark anywhere.

Standard errors are clustered by ticker throughout: one family
(KXTRUMPMENTION-*) is ~61% of volume, so unclustered SEs are meaningless.
"""
import math
import sqlite3
import sys
from collections import defaultdict

RIG = "file:/Users/hugh/kek/lip/rig.db?mode=ro"
SETTLE = "/Users/hugh/kek/lip/settle.db"


def cluster_stats(rows, wkey=None):
    """rows: list of (ticker, value, weight). Returns (wmean, se_clustered, n, ntick).

    Cluster-robust SE for a weighted mean: treat the mean as a ratio estimator
    and use the sandwich form over per-ticker aggregates.
    """
    if not rows:
        return (float("nan"),) * 2 + (0, 0)
    W = sum(w for _, _, w in rows)
    if W <= 0:
        return (float("nan"),) * 2 + (0, 0)
    mu = sum(v * w for _, v, w in rows) / W
    g = defaultdict(float)
    for t, v, w in rows:
        g[t] += w * (v - mu)
    G = len(g)
    if G < 2:
        return mu, float("nan"), len(rows), G
    var = sum(s * s for s in g.values()) / (W * W)
    var *= G / (G - 1.0)
    return mu, math.sqrt(var), len(rows), G


def fmt(mu, se, n, G, width=0):
    if se != se:
        return f"{mu:+8.3f}  (se n/a, n={n}, tickers={G})"
    t = mu / se if se > 0 else float("nan")
    star = "***" if abs(t) > 2.58 else ("**" if abs(t) > 1.96 else ("*" if abs(t) > 1.64 else ""))
    return f"{mu:+8.3f} +- {se:6.3f}  t={t:+6.2f}{star:<3} n={n:>6} tick={G:>3}"


def main():
    con = sqlite3.connect(RIG, uri=True)
    con.execute(f"ATTACH DATABASE 'file:{SETTLE}?mode=ro' AS s")

    # ---------------------------------------------------------------- coverage
    print("=" * 100)
    print("COVERAGE")
    print("=" * 100)
    for label, where in [("all fills", "1=1"),
                         ("settled ticker", "st.result IN ('yes','no')")]:
        r = con.execute(f"""
            SELECT count(*), sum(f.size), count(DISTINCT f.ticker)
              FROM fill f LEFT JOIN s.settlement st ON st.ticker=f.ticker
             WHERE {where}""").fetchone()
        print(f"  {label:<18} fills={r[0]:>7}  contracts={r[1]:>12,.0f}  tickers={r[2]:>4}")

    # family concentration on the settled subset
    print("\n  settled-subset concentration by family:")
    fam = con.execute("""
        SELECT substr(f.ticker,1,instr(f.ticker||'-','-')-1) fam,
               count(*), sum(f.size), count(DISTINCT f.ticker)
          FROM fill f JOIN s.settlement st ON st.ticker=f.ticker
         WHERE st.result IN ('yes','no')
         GROUP BY 1 ORDER BY 3 DESC LIMIT 6""").fetchall()
    tot = sum(x[2] for x in fam)
    for name, n, sz, nt in fam:
        print(f"     {name:<28}{n:>7} fills {sz:>12,.0f} ctr  {sz/tot*100:5.1f}%  {nt:>3} tickers")

    # ------------------------------------------------ (A) the pre_mid artifact
    print()
    print("=" * 100)
    print("(A) ARTIFACT TEST -- is 'maker edge' just the distance itself?")
    print("=" * 100)
    print("   distance = ticks the fill price sat behind the touch on the resting side")
    print("   edge_pre = pre_mid - price          (the prior measure; suspect)")
    print("   edge_1m  = mid_1m  - price          (post-trade, 1 minute)")
    print("   edge_30m = mid_30m - price          (post-trade, 30 minutes)")
    print()
    rows = con.execute("""
        SELECT f.ticker,
               CASE WHEN f.resting_side='yes' THEN f.pre_best_yes - f.price
                    ELSE f.pre_best_no - f.price END          AS dist,
               CASE WHEN f.resting_side='yes' THEN f.pre_mid - f.price
                    ELSE (100 - f.pre_mid) - f.price END      AS edge_pre,
               CASE WHEN f.mid_1m IS NULL THEN NULL
                    WHEN f.resting_side='yes' THEN f.mid_1m - f.price
                    ELSE (100 - f.mid_1m) - f.price END       AS edge_1m,
               CASE WHEN f.mid_30m IS NULL THEN NULL
                    WHEN f.resting_side='yes' THEN f.mid_30m - f.price
                    ELSE (100 - f.mid_30m) - f.price END      AS edge_30m,
               f.size
          FROM fill f
         WHERE f.pre_mid IS NOT NULL AND f.pre_best_yes IS NOT NULL
           AND f.pre_best_no IS NOT NULL AND f.size > 0""").fetchall()

    buckets = [(0, 0), (1, 1), (2, 3), (4, 7), (8, 11), (12, 999)]
    print(f"   {'distance':<12}{'edge_pre':>34}{'':4}{'edge_1m':>34}")
    for lo, hi in buckets:
        sel = [r for r in rows if lo <= r[1] <= hi]
        a = cluster_stats([(r[0], r[2], r[5]) for r in sel])
        b = cluster_stats([(r[0], r[3], r[5]) for r in sel if r[3] is not None])
        lab = f"{lo}" if lo == hi else (f">={lo}" if hi == 999 else f"{lo}-{hi}")
        print(f"   {lab:<12}{fmt(*a):>34}    {fmt(*b):>34}")

    print()
    print("   TAUTOLOGY CHECK -- mean(edge_pre) vs mean(distance) in each bucket:")
    print(f"   {'distance':<12}{'mean dist':>12}{'mean edge_pre':>15}{'difference':>13}")
    for lo, hi in buckets:
        sel = [r for r in rows if lo <= r[1] <= hi]
        if not sel:
            continue
        W = sum(r[5] for r in sel)
        md = sum(r[1] * r[5] for r in sel) / W
        me = sum(r[2] * r[5] for r in sel) / W
        lab = f"{lo}" if lo == hi else (f">={lo}" if hi == 999 else f"{lo}-{hi}")
        print(f"   {lab:<12}{md:>12.2f}{me:>15.2f}{me-md:>13.2f}")

    # ------------------------------------------- (B) settlement-anchored edge
    print()
    print("=" * 100)
    print("(B) SETTLEMENT-ANCHORED MAKER EDGE  (cents/contract; efficiency null = 0)")
    print("=" * 100)
    srows = con.execute("""
        SELECT f.ticker,
               CASE WHEN f.resting_side='yes' THEN f.pre_best_yes - f.price
                    ELSE f.pre_best_no - f.price END              AS dist,
               (CASE WHEN st.result = f.resting_side THEN 100.0 ELSE 0.0 END)
                    - f.price                                     AS pnl,
               f.size, f.price, f.resting_side, f.taker_side
          FROM fill f JOIN s.settlement st ON st.ticker = f.ticker
         WHERE st.result IN ('yes','no') AND f.size > 0
           AND f.pre_best_yes IS NOT NULL AND f.pre_best_no IS NOT NULL""").fetchall()

    allst = cluster_stats([(r[0], r[2], r[3]) for r in srows])
    print(f"   {'ALL settled maker fills':<26}{fmt(*allst)}")
    print()
    print("   by distance behind touch:")
    for lo, hi in buckets:
        sel = [r for r in srows if lo <= r[1] <= hi]
        lab = f"{lo}" if lo == hi else (f">={lo}" if hi == 999 else f"{lo}-{hi}")
        st = cluster_stats([(r[0], r[2], r[3]) for r in sel])
        print(f"     {lab:<10}{fmt(*st)}")

    print()
    print("   by price bucket (calibration: is the traded price a fair probability?):")
    pbk = [(1, 9), (10, 24), (25, 49), (50, 74), (75, 90), (91, 99)]
    for lo, hi in pbk:
        sel = [r for r in srows if lo <= r[4] <= hi]
        st = cluster_stats([(r[0], r[2], r[3]) for r in sel])
        W = sum(r[3] for r in sel) or 1
        mp = sum(r[4] * r[3] for r in sel) / W
        wr = sum((1.0 if r[2] > 0 else 0.0) * r[3] for r in sel) / W
        print(f"     {lo:>2}-{hi:<2}c  avg_px={mp:5.1f}  win_rate={wr*100:5.1f}%  {fmt(*st)}")

    # one-sided: what a LIP quoter actually does -- rest at/near the touch
    print()
    print("   the LIP-relevant slice (distance 0-1, i.e. at or one tick behind touch):")
    sel = [r for r in srows if r[1] <= 1]
    st = cluster_stats([(r[0], r[2], r[3]) for r in sel])
    print(f"     {'settlement P&L':<22}{fmt(*st)}")
    for side in ("yes", "no"):
        ss = [r for r in sel if r[5] == side]
        print(f"     {'  resting ' + side:<22}{fmt(*cluster_stats([(r[0], r[2], r[3]) for r in ss]))}")

    print()
    print("   NOTE on power: each observation is a binary payoff, sd ~40c. With SEs")
    print("   clustered on ~117 settled tickers the detectable effect is coarse.")
    print("   A null result here is 'not measurable', not 'proven zero'.")


if __name__ == "__main__":
    main()
