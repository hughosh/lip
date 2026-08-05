"""Stress-test the one large positive: resting DEEP and catching sweeps.

Headline to attack: fills >=10 ticks behind the touch earn the resting order
+11.9c/contract at 1m and +14.9c at 30m (t=3.7-5.4).

Four ways it could be fake, tested here:
  1. STP artifact. Every deep fill has trade_through=1 by construction. Kalshi's
     MAKER-side self-trade prevention cancels the resting maker and CONTINUES
     matching at worse prices, so a deep print is not proof an order rested
     there. If the edge is an artifact it should concentrate where
     depth_at_price is tiny (a cancelled order leaves no depth behind).
  2. Concentration. A handful of violent events (Finding D book holes) could be
     the whole effect. Jackknife by ticker and by event family.
  3. Out-of-sample. Split the sample in half by time.
  4. Capital efficiency. Per-contract capture is not a return. Measure the
     contracts/ticker-hour actually swept at each depth and the resting depth
     you would have to compete with to capture it.
"""
import sqlite3
import math
from collections import defaultdict

DB = 'file:/Users/hugh/kek/lip/rig.db?mode=ro'
HOURS = 612.5          # ticker-hours of reference coverage (measured earlier)


def load(h='30m'):
    con = sqlite3.connect(DB, uri=True)
    q = f"""SELECT ticker, ts_ms, resting_side, price, size, trade_through,
                   pre_best_yes, pre_best_no, pre_mid, pre_spread,
                   depth_at_price, mid_{h}
            FROM fill
            WHERE source='observed' AND mid_{h} IS NOT NULL AND pre_mid IS NOT NULL
              AND pre_best_yes IS NOT NULL AND pre_best_no IS NOT NULL"""
    out = []
    for (tk, ts, rs, price, size, tt, pby, pbn, pmid, pspr, dap, m) in con.execute(q):
        d = -1 if rs == 'yes' else 1
        ypx = price if rs == 'yes' else 100 - price
        touch = pby if rs == 'yes' else pbn
        out.append(dict(ticker=tk, ts=ts, size=size, tt=tt, pspr=pspr,
                        dap=dap, back=touch - price,
                        maker=d * (ypx - pmid) - d * (m - pmid)))
    con.close()
    return out


def agg(rows, f='maker'):
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


def line(lbl, rows):
    a = agg(rows)
    print(f'  {lbl:>34s} n={a["n"]:>5d} vol={a["vol"]:>8.0f} G={a["G"]:>3d} '
          f'{a["mean"]:>8.3f}c se {a["se"]:>6.3f} t={a["t"]:>6.2f}')


def main():
    for h in ('1m', '30m'):
        rows = load(h)
        deep = [r for r in rows if r['back'] >= 10]
        print('=' * 78)
        print(f'HORIZON {h}   deep = fills >=10 ticks behind the touch')
        print('=' * 78)
        line('ALL deep', deep)

        print('\n-- 1. STP artifact test: depth left at the price --')
        for lo, hi, lbl in [(0, 0, 'depth_at_price == 0'),
                            (0.01, 10, 'depth 0-10'),
                            (10.01, 100, 'depth 10-100'),
                            (100.01, 1e9, 'depth >100')]:
            line(lbl, [r for r in deep if lo <= r['dap'] <= hi])
        print('  (an STP cancel leaves NO depth: if the edge lives only in the')
        print('   depth==0 / tiny-depth rows it is an artifact, not a fill)')

        print('\n-- 2a. jackknife: drop the single largest-contributing ticker --')
        contrib = defaultdict(float)
        a_all = agg(deep)
        for r in deep:
            contrib[r['ticker']] += r['size'] * r['maker']
        top = sorted(contrib.items(), key=lambda kv: -kv[1])[:5]
        for tk, c in top:
            print(f'     {tk:<44s} contributes {c:>12.0f} c*contracts '
                  f'({c / (a_all["vol"] * a_all["mean"]):>6.1%})')
        for k in (1, 2, 3, 5):
            drop = {tk for tk, _ in top[:k]}
            line(f'ex top-{k} ticker(s)', [r for r in deep if r['ticker'] not in drop])

        print('\n-- 2b. by event family --')
        fam = defaultdict(list)
        for r in deep:
            fam[r['ticker'].split('-')[0][:16]].append(r)
        for k in sorted(fam, key=lambda k: -sum(x['size'] for x in fam[k])):
            line(k, fam[k])

        print('\n-- 3. split sample by time --')
        ts = sorted(r['ts'] for r in rows)
        # median over ALL fills so the halves are comparable calendar spans
        cut = ts[len(ts) // 2]
        line('first half', [r for r in deep if r['ts'] < cut])
        line('second half', [r for r in deep if r['ts'] >= cut])

        if h != '30m':
            continue

        print('\n-- 4. capital efficiency: what is actually available --')
        print(f'  {"depth band":>14s} {"vol swept":>10s} {"ctr/tkr-hr":>11s} '
              f'{"maker c":>8s} {"$/tkr-hr":>9s} {"med depth":>10s}')
        bands = [(1, 1), (2, 2), (3, 4), (5, 9), (10, 19), (20, 10 ** 9)]
        for lo, hi in bands:
            rr = [r for r in rows if lo <= r['back'] <= hi]
            if not rr:
                continue
            a = agg(rr)
            vol = a['vol']
            per_hr = vol / HOURS
            dollars = per_hr * a['mean'] / 100.0
            deps = sorted(r['dap'] for r in rr)
            med = deps[len(deps) // 2]
            print(f'  {f"{lo}-{hi} back":>14s} {vol:>10.0f} {per_hr:>11.2f} '
                  f'{a["mean"]:>8.2f} {dollars:>9.3f} {med:>10.1f}')
        print(f'\n  ({HOURS:.0f} ticker-hours of coverage, 192 tickers.')
        print('   $/tkr-hr is the TOTAL economic value at that band shared by')
        print('   every resting order there -- your cut is your_size/total_depth.)')


if __name__ == '__main__':
    main()
