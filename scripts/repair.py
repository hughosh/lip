"""Repair the non-random mid_h dropout using lip.db's independent quote series.

rig.db's mid_h is NULL whenever either side of the book is empty at t+h, which
happens preferentially near close and on 4.6x-larger fills -- 36.8% of volume.
lip.db.snapshot is a SEPARATE poller (2,342 tickers, ISO ts, prices in dollars)
and is available for many of those instants.

Step 1 validates lip.db against rig.db where BOTH exist. Step 2 uses it to
resolve the missing marks. Step 3 reports the taker/maker results as a point
estimate over what is resolvable plus explicit bounds on the irreducible
remainder -- never a midpoint of [0,100] passed off as an estimate.
"""
import sqlite3
import math
import bisect
from datetime import datetime
from collections import defaultdict

RIG = 'file:/Users/hugh/kek/lip/rig.db?mode=ro'
LIP = 'file:/Users/hugh/kek/lip/lip.db?mode=ro'
H_MS = 60_000
STALE = 900_000          # accept a lip.db quote within +/-15 min of the target


def lipindex():
    con = sqlite3.connect(LIP, uri=True)
    idx = defaultdict(lambda: ([], []))
    for tk, ts, yb, nb in con.execute(
            "SELECT ticker, ts, yes_bid, no_bid FROM snapshot "
            "WHERE yes_bid IS NOT NULL AND no_bid IS NOT NULL "
            "AND yes_bid > 0 AND no_bid > 0"):
        ms = int(datetime.fromisoformat(ts).timestamp() * 1000)
        t, v = idx[tk]
        t.append(ms)
        v.append((yb * 100.0 + (100.0 - nb * 100.0)) / 2.0)
    con.close()
    for tk in idx:
        t, v = idx[tk]
        z = sorted(zip(t, v))
        idx[tk] = ([a for a, _ in z], [b for _, b in z])
    return idx


def nearest(idx, tk, ts):
    t, v = idx.get(tk, ([], []))
    if not t:
        return None, None
    i = bisect.bisect_left(t, ts)
    best = None
    for j in (i - 1, i):
        if 0 <= j < len(t):
            d = abs(t[j] - ts)
            if best is None or d < best[0]:
                best = (d, v[j])
    if best is None or best[0] > STALE:
        return None, None
    return best[1], best[0]


def agg(rows, f):
    W = sum(r['size'] for r in rows)
    if not rows or W == 0:
        return dict(n=0, vol=0.0, mean=float('nan'), se=float('nan'),
                    t=float('nan'), G=0)
    mean = sum(r['size'] * r[f] for r in rows) / W
    by = defaultdict(float)
    for r in rows:
        by[r['ticker']] += r['size'] * (r[f] - mean)
    G = len(by)
    se = (math.sqrt(sum(u * u for u in by.values()) * (G / (G - 1.0))) / W
          if G >= 2 else float('nan'))
    return dict(n=len(rows), vol=W, mean=mean, se=se,
                t=mean / se if se == se and se > 0 else float('nan'), G=G)


def main():
    idx = lipindex()
    print(f'lip.db index: {len(idx)} tickers')

    con = sqlite3.connect(RIG, uri=True)
    rows = []
    for (tk, ts, rs, price, size, pby, pbn, dap, m) in con.execute(
            """SELECT ticker, ts_ms, resting_side, price, size,
                      pre_best_yes, pre_best_no, depth_at_price, mid_1m
               FROM fill WHERE source='observed'"""):
        d = -1 if rs == 'yes' else 1
        ypx = price if rs == 'yes' else 100 - price
        p = price / 100.0
        lm, lag = nearest(idx, tk, ts + H_MS)
        rows.append(dict(ticker=tk, ts=ts, rs=rs, size=size, dap=dap,
                         ypx=ypx, dir=d, fee=7.0 * p * (1 - p),
                         rig=m, lip=lm, lag=lag,
                         back=(pby - price) if rs == 'yes' and pby is not None
                         else ((pbn - price) if rs == 'no' and pbn is not None
                               else None)))
    con.close()

    # ---- 1. validate lip.db against rig.db where both exist ----
    both = [r for r in rows if r['rig'] is not None and r['lip'] is not None]
    diffs = sorted(r['lip'] - r['rig'] for r in both)
    n = len(diffs)
    print(f'\n1. VALIDATION: both sources present on {n} fills '
          f'({sum(r["size"] for r in both):,.0f} contracts)')
    print(f'   lip - rig (cents):  p10 {diffs[n//10]:+.2f}  '
          f'p25 {diffs[n//4]:+.2f}  median {diffs[n//2]:+.2f}  '
          f'p75 {diffs[3*n//4]:+.2f}  p90 {diffs[9*n//10]:+.2f}')
    mae = sum(abs(d) for d in diffs) / n
    print(f'   mean abs error {mae:.2f}c  '
          f'(median staleness {sorted(r["lag"] for r in both)[n//2]/1000:.0f}s)')

    # ---- 2. coverage ----
    tot = sum(r['size'] for r in rows)
    miss = [r for r in rows if r['rig'] is None]
    fixed = [r for r in miss if r['lip'] is not None]
    still = [r for r in miss if r['lip'] is None]
    print(f'\n2. COVERAGE of {tot:,.0f} contracts')
    print(f'   rig mid present          {sum(r["size"] for r in rows if r["rig"] is not None):>10,.0f} '
          f'({sum(r["size"] for r in rows if r["rig"] is not None)/tot:>5.1%})')
    print(f'   repaired from lip.db     {sum(r["size"] for r in fixed):>10,.0f} '
          f'({sum(r["size"] for r in fixed)/tot:>5.1%})')
    print(f'   STILL unresolved         {sum(r["size"] for r in still):>10,.0f} '
          f'({sum(r["size"] for r in still)/tot:>5.1%})')

    # ---- 3. results ----
    for r in rows:
        v = r['rig'] if r['rig'] is not None else r['lip']
        r['have'] = v is not None
        if r['have']:
            r['net'] = r['dir'] * (v - r['ypx']) - r['fee']
            r['gross'] = r['dir'] * (v - r['ypx'])
            r['maker'] = -r['gross']
        # bounds for unresolved: value in [0,100]
        r['net_lo'] = min(r['dir'] * (0 - r['ypx']), r['dir'] * (100 - r['ypx'])) - r['fee']
        r['net_hi'] = max(r['dir'] * (0 - r['ypx']), r['dir'] * (100 - r['ypx'])) - r['fee']
        if r['have']:
            r['net_lo'] = r['net_hi'] = r['net']

    have = [r for r in rows if r['have']]
    print(f'\n3. TAKER NET @1m')
    a, g = agg(have, 'net'), agg(have, 'gross')
    print(f'   resolved population ({sum(r["size"] for r in have)/tot:.1%} of volume):')
    print(f'     NET   {a["mean"]:+.3f}c  se {a["se"]:.3f}  t={a["t"]:+.2f}  '
          f'n={a["n"]}  G={a["G"]}')
    print(f'     GROSS {g["mean"]:+.3f}c  se {g["se"]:.3f}  t={g["t"]:+.2f}')
    lo, hi = agg(rows, 'net_lo'), agg(rows, 'net_hi')
    print(f'   full population, worst/best case on the unresolved tail:')
    print(f'     NET in [{lo["mean"]:+.3f}, {hi["mean"]:+.3f}]c')
    ex = [r for r in have if not r['ticker'].startswith('KXTRUMPMENTION')]
    a2 = agg(ex, 'net')
    print(f'   ex-KXTRUMPMENTION: NET {a2["mean"]:+.3f}c se {a2["se"]:.3f} '
          f't={a2["t"]:+.2f} (G={a2["G"]})')

    print(f'\n4. MAKER by ticks behind the touch (resolved population)')
    pv = [r for r in have if r['back'] is not None and r['back'] >= 0]
    byN = defaultdict(list)
    for r in pv:
        byN[min(r['back'], 12)].append(r)
    for N in sorted(byN):
        a = agg(byN[N], 'maker')
        print(f'   N={N:>2d}  n={a["n"]:>6d} vol={a["vol"]:>9.0f} '
              f'{a["mean"]:>+8.3f}c se {a["se"]:>6.3f} t={a["t"]:>+6.2f}')

    print(f'\n5. QUEUE PROXY at the touch (resolved population)')
    at = [r for r in pv if r['back'] == 0 and r['dap'] and r['dap'] > 0]
    for lo_, hi_, lbl in [(0, .1, '<10% (front)'), (.1, .33, '10-33%'),
                          (.33, .67, '33-67%'), (.67, 1., '67-100%'),
                          (1., 9e9, '>=100% cleared')]:
        sub = [r for r in at if lo_ <= r['size'] / r['dap'] < hi_]
        if not sub:
            continue
        a = agg(sub, 'maker')
        print(f'   {lbl:>16s} n={a["n"]:>6d} vol={a["vol"]:>8.0f} '
              f'{a["mean"]:>+7.3f}c se {a["se"]:.3f} t={a["t"]:+.2f}')

    print(f'\n6. DEEP BID >=10 back (resolved population)')
    deep = [r for r in pv if r['back'] >= 10]
    a = agg(deep, 'maker')
    print(f'   all      n={a["n"]:>4d} vol={a["vol"]:>8.0f} G={a["G"]:>3d} '
          f'{a["mean"]:+.3f}c se {a["se"]:.3f} t={a["t"]:+.2f}')
    contrib = defaultdict(float)
    for r in deep:
        contrib[r['ticker']] += r['size'] * r['maker']
    top = [tk for tk, _ in sorted(contrib.items(), key=lambda kv: -kv[1])[:5]]
    for k in (1, 3, 5):
        a = agg([r for r in deep if r['ticker'] not in set(top[:k])], 'maker')
        print(f'   ex top-{k} n={a["n"]:>4d} vol={a["vol"]:>8.0f} '
              f'{a["mean"]:+.3f}c se {a["se"]:.3f} t={a["t"]:+.2f}')


if __name__ == '__main__':
    main()
