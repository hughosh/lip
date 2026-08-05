#!/usr/bin/env python
"""Should a maker stop out when the market moves against the position?

The live probe's inventory cap forced a taker liquidation that, ex post, sold
the bottom of a dislocation (market went 57 -> 44 -> settled YES at 100). n=1,
so that proves nothing on its own. This tests the rule on the population.

Design: take maker fills whose position moved ADVERSELY by >= K cents within
5 minutes -- exactly the state that trips an inventory/loss stop -- and compare
    stop-out value  = mid_5m  (what you get crossing out then, before costs)
    hold value      = settlement
If E[settlement] >= E[mid_5m], stopping out is a loss-generating rule and the
harness must never cross to flatten.  If E[settlement] < E[mid_5m], adverse
moves carry information and stopping out is protective.

All P&L in cents/contract from the maker's perspective. SEs clustered by ticker.
"""
import sqlite3
import sys

sys.path.insert(0, "/Users/hugh/kek/lip/scripts")
from settleedge import cluster_stats, fmt  # noqa: E402

con = sqlite3.connect("file:/Users/hugh/kek/lip/rig.db?mode=ro", uri=True)
con.execute("ATTACH DATABASE 'file:/Users/hugh/kek/lip/settle.db?mode=ro' AS s")

rows = con.execute("""
    SELECT f.ticker, f.size, f.price, f.resting_side,
           CASE WHEN f.resting_side='yes' THEN f.pre_best_yes - f.price
                ELSE f.pre_best_no - f.price END                       AS dist,
           CASE WHEN f.resting_side='yes' THEN f.mid_5m ELSE 100 - f.mid_5m END
                                                                        AS mk5,
           CASE WHEN f.resting_side='yes' THEN f.mid_30m ELSE 100 - f.mid_30m END
                                                                        AS mk30,
           CASE WHEN st.result = f.resting_side THEN 100.0 ELSE 0.0 END AS term
      FROM fill f JOIN s.settlement st ON st.ticker = f.ticker
     WHERE st.result IN ('yes','no') AND f.size > 0
       AND f.mid_5m IS NOT NULL AND f.pre_best_yes IS NOT NULL
       AND f.pre_best_no IS NOT NULL""").fetchall()

print("=" * 104)
print("HOLD vs STOP-OUT  (maker fills that moved against the position within 5 min)")
print("=" * 104)
print(f"  usable fills: {len(rows)}  tickers: {len({r[0] for r in rows})}")
print()
print("  'drawdown' = price - mid_5m  (cents the position is underwater at 5 min)")
print()
hdr = (f"  {'drawdown':<12}{'n':>7}{'stop-out (mid_5m - px)':>32}"
       f"{'hold (settle - px)':>32}{'hold - stop':>14}")
print(hdr)
print("  " + "-" * (len(hdr) - 2))

for lo, hi in [(-99, 0), (1, 2), (3, 5), (6, 10), (11, 20), (21, 99)]:
    sel = [r for r in rows if lo <= (r[2] - r[5]) <= hi]
    if len(sel) < 30:
        continue
    stop = cluster_stats([(r[0], r[5] - r[2], r[1]) for r in sel])
    hold = cluster_stats([(r[0], r[7] - r[2], r[1]) for r in sel])
    diff = cluster_stats([(r[0], r[7] - r[5], r[1]) for r in sel])
    lab = "favourable" if hi == 0 else (f">={lo}c" if hi == 99 else f"{lo}-{hi}c")
    print(f"  {lab:<12}{len(sel):>7}{fmt(*stop):>32}{fmt(*hold):>32}"
          f"{diff[0]:>+10.2f}+-{diff[1]:.2f}")

print()
print("  Restricted to AT-TOUCH fills (distance <= 1) -- the LIP-relevant case:")
print(hdr)
print("  " + "-" * (len(hdr) - 2))
for lo, hi in [(-99, 0), (1, 2), (3, 5), (6, 10), (11, 99)]:
    sel = [r for r in rows if r[4] <= 1 and lo <= (r[2] - r[5]) <= hi]
    if len(sel) < 30:
        continue
    stop = cluster_stats([(r[0], r[5] - r[2], r[1]) for r in sel])
    hold = cluster_stats([(r[0], r[7] - r[2], r[1]) for r in sel])
    diff = cluster_stats([(r[0], r[7] - r[5], r[1]) for r in sel])
    lab = "favourable" if hi == 0 else (f">={lo}c" if hi == 99 else f"{lo}-{hi}c")
    print(f"  {lab:<12}{len(sel):>7}{fmt(*stop):>32}{fmt(*hold):>32}"
          f"{diff[0]:>+10.2f}+-{diff[1]:.2f}")

print()
print("=" * 104)
print("MEAN REVERSION vs MOMENTUM: does an adverse move predict the NEXT move?")
print("=" * 104)
print("  If adverse moves continue (momentum), stopping out is protective.")
print("  If they revert, stopping out realises the worst point -- what the probe did.")
print()
print(f"  {'drawdown at 5m':<18}{'n':>7}{'further move 5m -> 30m (cents)':>36}")
for lo, hi in [(-99, 0), (1, 2), (3, 5), (6, 10), (11, 99)]:
    sel = [r for r in rows if r[6] is not None and lo <= (r[2] - r[5]) <= hi]
    if len(sel) < 30:
        continue
    nxt = cluster_stats([(r[0], r[6] - r[5], r[1]) for r in sel])
    lab = "favourable" if hi == 0 else (f">={lo}c" if hi == 99 else f"{lo}-{hi}c")
    print(f"  {lab:<18}{len(sel):>7}{fmt(*nxt):>36}")

print()
print("=" * 104)
print("COST OF CROSSING (what the stop-out actually pays, on top of the above)")
print("=" * 104)
r = con.execute("""
    SELECT avg(pre_spread), count(*) FROM fill
     WHERE pre_spread IS NOT NULL AND pre_spread BETWEEN 0 AND 20""").fetchone()
print(f"  mean quoted spread: {r[0]:.2f} ticks  (n={r[1]})  -> half-spread ~{r[0]/2:.2f}c")
print("  taker fee: 0.07 * C * P * (1-P) dollars -> at P=0.5, 1.75c/contract;")
print("             at P=0.25/0.75, 1.31c/contract.")
print("  maker fee: 0.00")
