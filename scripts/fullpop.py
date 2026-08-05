"""Rebuild the taker/maker results on the FULL fill population.

Two defects found by adversarial review of scripts/takerdecomp.py:

  1. `mid_h` is NULL whenever either side of the book is empty at t+h, and
     those fills are 4.6x LARGER than average. Conditioning on mid_h drops
     36.8% of volume non-randomly (books collapse to one side near close).
  2. The `pre_mid IS NOT NULL` filter is algebraically unnecessary:
         gross = drift - spread
               = dir*(mid_h - pre_mid) - dir*(ypx - pre_mid)
               = dir*(mid_h - ypx)                <- pre_mid cancels
     It discarded a further 129,121 contracts for nothing.

This script drops filter (2) entirely, and repairs (1) by reconstructing an
as-of mid from the `reference` table at t+h (ref_yes / ref_no are the two touch
prices) before falling back to explicit bounds on whatever is still unresolved.

Bounds convention when the book is one-sided at t+h:
  - no yes asks (ref_no missing)  -> nobody will sell yes -> value >= ref_yes
  - no yes bids (ref_yes missing) -> nobody will buy yes  -> value <= 100-ref_no
A one-sided book gives a one-sided bound, which still signs the taker's P&L in
the direction that matters. Anything with neither side is bounded [0,100].
"""
import sqlite3
import math
import bisect
from collections import defaultdict

DB = 'file:/Users/hugh/kek/lip/rig.db?mode=ro'


def refindex(con):
    """ticker -> (sorted ts list, [(ref_yes, ref_no)])"""
    idx = defaultdict(lambda: ([], []))
    for tk, ts, ry, rn in con.execute(
            "SELECT ticker, ts_ms, ref_yes, ref_no FROM reference "
            "WHERE ts_ms > 0 ORDER BY ticker, ts_ms"):
        t, v = idx[tk]
        t.append(ts)
        v.append((ry, rn))
    return idx


def asof(idx, tk, ts, max_stale_ms=600_000):
    t, v = idx.get(tk, ([], []))
    if not t:
        return None
    i = bisect.bisect_right(t, ts) - 1
    if i < 0 or ts - t[i] > max_stale_ms:
        return None
    return v[i]


def load(horizon_ms=60_000, midcol='mid_1m'):
    con = sqlite3.connect(DB, uri=True)
    idx = refindex(con)
    rows = []
    stats = defaultdict(int)
    volstats = defaultdict(float)
    for (tk, ts, rs, price, size, tt, pby, pbn, pmid, pspr, dap, m) in con.execute(
            f"""SELECT ticker, ts_ms, resting_side, price, size, trade_through,
                       pre_best_yes, pre_best_no, pre_mid, pre_spread,
                       depth_at_price, {midcol}
                FROM fill WHERE source='observed'"""):
        d = -1 if rs == 'yes' else 1
        ypx = price if rs == 'yes' else 100 - price
        p = price / 100.0
        fee = 7.0 * p * (1.0 - p)
        lo = hi = None
        if m is not None:
            lo = hi = m
            src = 'rig'
        else:
            r = asof(idx, tk, ts + horizon_ms)
            if r is None:
                lo, hi, src = 0.0, 100.0, 'none'
            else:
                ry, rn = r
                if ry is not None and rn is not None:
                    lo = hi = (ry + (100 - rn)) / 2.0
                    src = 'ref'
                elif ry is not None:          # bids but no asks -> value >= ry
                    lo, hi, src = float(ry), 100.0, 'bound_lo'
                elif rn is not None:          # asks but no bids -> value <= 100-rn
                    lo, hi, src = 0.0, float(100 - rn), 'bound_hi'
                else:
                    lo, hi, src = 0.0, 100.0, 'none'
        stats[src] += 1
        volstats[src] += size
        # taker net at the two ends of the value interval
        n_lo = d * (lo - ypx) - fee
        n_hi = d * (hi - ypx) - fee
        rows.append(dict(ticker=tk, ts=ts, rs=rs, size=size, tt=tt, pspr=pspr,
                         dap=dap, ypx=ypx, dir=d, fee=fee, src=src,
                         net_lo=min(n_lo, n_hi), net_hi=max(n_lo, n_hi),
                         net_pt=(min(n_lo, n_hi) + max(n_lo, n_hi)) / 2.0,
                         maker_lo=-max(n_lo, n_hi) - fee,
                         maker_hi=-min(n_lo, n_hi) - fee,
                         back=(pby - price) if rs == 'yes' and pby is not None
                         else ((pbn - price) if rs == 'no' and pbn is not None
                               else None)))
    con.close()
    return rows, stats, volstats


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


def show(lbl, rows, f='net_pt'):
    a = agg(rows, f)
    lo, hi = agg(rows, 'net_lo'), agg(rows, 'net_hi')
    print(f'  {lbl:>30s} n={a["n"]:>6d} vol={a["vol"]:>9.0f} G={a["G"]:>3d} '
          f'{a["mean"]:>+7.3f}c se {a["se"]:>5.3f} t={a["t"]:>+6.2f}  '
          f'[{lo["mean"]:+.3f}, {hi["mean"]:+.3f}]')


def main():
    rows, stats, volstats = load()
    tot = sum(volstats.values())
    print('Value source for the t+1m mark:')
    for k in sorted(stats, key=lambda k: -volstats[k]):
        print(f'  {k:>9s}: n={stats[k]:>6d} vol={volstats[k]:>10.0f} '
              f'({volstats[k]/tot:>5.1%})')

    resolved = [r for r in rows if r['net_lo'] == r['net_hi']]
    print(f'\npoint-valued (rig mid or reference mid): '
          f'{sum(r["size"] for r in resolved)/tot:.1%} of volume')

    print('\n=== TAKER NET, 1m ===  (point estimate; [lower, upper] bound)')
    show('FULL population', rows)
    show('point-valued only', resolved)
    show('rig mid only (old sample)', [r for r in rows if r['src'] == 'rig'])
    show('reference-mid repaired only', [r for r in rows if r['src'] == 'ref'])
    print()
    show('ex-KXTRUMPMENTION, full pop',
         [r for r in rows if not r['ticker'].startswith('KXTRUMPMENTION')])
    show('KXTRUMPMENTION only',
         [r for r in rows if r['ticker'].startswith('KXTRUMPMENTION')])

    print('\n  gross (fee added back), point-valued:')
    for r in resolved:
        r['gross'] = r['net_pt'] + r['fee']
    a = agg(resolved, 'gross')
    print(f'  {"GROSS":>30s} {a["mean"]:>+7.3f}c se {a["se"]:.3f} t={a["t"]:+.2f}')

    print('\n=== MAKER, by ticks behind the touch (point-valued only) ===')
    print(f'  {"N back":>10s} {"n":>6s} {"vol":>9s} {"tk":>4s} {"MAKER":>8s} '
          f'{"se":>6s} {"t":>6s}')
    pv = [r for r in resolved if r['back'] is not None and r['back'] >= 0]
    for r in pv:
        r['maker'] = -(r['net_pt'] + r['fee'])
    byN = defaultdict(list)
    for r in pv:
        byN[min(r['back'], 12)].append(r)
    for N in sorted(byN):
        a = agg(byN[N], 'maker')
        print(f'  {N:>10d} {a["n"]:>6d} {a["vol"]:>9.0f} {a["G"]:>4d} '
              f'{a["mean"]:>+8.3f} {a["se"]:>6.3f} {a["t"]:>+6.2f}')

    print('\n=== MAKER at touch, by fraction of level consumed (queue proxy) ===')
    at = [r for r in pv if r['back'] == 0 and r['dap'] and r['dap'] > 0]
    for lo, hi, lbl in [(0, .1, '<10% (front)'), (.1, .33, '10-33%'),
                        (.33, .67, '33-67%'), (.67, 1., '67-100%'),
                        (1., 9e9, '>=100% cleared')]:
        sub = [r for r in at if lo <= r['size'] / r['dap'] < hi]
        if not sub:
            continue
        a = agg(sub, 'maker')
        print(f'  {lbl:>16s} n={a["n"]:>6d} vol={a["vol"]:>8.0f} '
              f'{a["mean"]:>+7.3f}c se {a["se"]:.3f} t={a["t"]:+.2f}')

    print('\n=== DEEP BID (>=10 ticks behind) on the repaired population ===')
    deep = [r for r in pv if r['back'] >= 10]
    a = agg(deep, 'maker')
    print(f'  all deep: n={a["n"]} vol={a["vol"]:.0f} G={a["G"]} '
          f'{a["mean"]:+.3f}c se {a["se"]:.3f} t={a["t"]:+.2f}')
    contrib = defaultdict(float)
    for r in deep:
        contrib[r['ticker']] += r['size'] * r['maker']
    top = [tk for tk, _ in sorted(contrib.items(), key=lambda kv: -kv[1])[:5]]
    for k in (1, 3, 5):
        a = agg([r for r in deep if r['ticker'] not in set(top[:k])], 'maker')
        print(f'  ex top-{k}: n={a["n"]} vol={a["vol"]:.0f} '
              f'{a["mean"]:+.3f}c se {a["se"]:.3f} t={a["t"]:+.2f}')


if __name__ == '__main__':
    main()
