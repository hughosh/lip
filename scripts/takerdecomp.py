"""Decompose the taker's P&L against resting LIP liquidity.

    taker net = DRIFT - SPREAD - FEE

  DRIFT  = dir * (mid_h - pre_mid)     did the market move toward the taker?
                                       >0 means the resting quote was picked off.
  SPREAD = dir * (exec_yes_px - pre_mid)   half-spread the taker paid on entry.
  FEE    = 7 * p * (1-p)                   Kalshi taker fee, cents/contract.
  dir    = +1 taker bought yes (resting_side='no'), -1 taker sold yes.

DRIFT is the only term that can be an *edge*. SPREAD and FEE are pure costs.
The research question reduces to: is there an ex-ante condition under which
DRIFT > SPREAD + FEE?

Also cuts on fractional fill size. `count_fp` is fractional for 45% of fills,
which is dollar-denominated (retail) order entry -- an ex-ante-invisible but
diagnostically useful label for WHO the taker is.
"""
import sqlite3
import math
from collections import defaultdict

DB = 'file:/Users/hugh/kek/lip/rig.db?mode=ro'


def load(horizon='1m', gate_join=False):
    mid = f'mid_{horizon}'
    con = sqlite3.connect(DB, uri=True)
    q = f"""SELECT ticker, ts_ms, resting_side, price, size, trade_through,
                   pre_mid, pre_spread, depth_at_price, book_lag_ms, {mid}
            FROM fill
            WHERE source='observed' AND {mid} IS NOT NULL AND pre_mid IS NOT NULL"""
    out = []
    for (tk, ts, rs, price, size, tt, pmid, pspr, dap, lag, m) in con.execute(q):
        d = -1 if rs == 'yes' else 1          # taker sold yes / bought yes
        ypx = price if rs == 'yes' else 100 - price
        drift = d * (m - pmid)
        spread = d * (ypx - pmid)
        p = price / 100.0
        fee = 7.0 * p * (1.0 - p)
        out.append(dict(ticker=tk, ts=ts, rs=rs, dir=d, price=price, ypx=ypx,
                        size=size, tt=tt, pmid=pmid, pspr=pspr, dap=dap,
                        lag=lag, mid=m, drift=drift, spread=spread, fee=fee,
                        net=drift - spread - fee,
                        frac=0 if float(size).is_integer() else 1))
    con.close()
    return out


def agg(rows, field):
    W = sum(r['size'] for r in rows)
    if not rows or W == 0:
        return dict(n=0, vol=0.0, mean=float('nan'), se=float('nan'),
                    t=float('nan'), G=0)
    mean = sum(r['size'] * r[field] for r in rows) / W
    by = defaultdict(float)
    for r in rows:
        by[r['ticker']] += r['size'] * (r[field] - mean)
    G = len(by)
    se = (math.sqrt(sum(u * u for u in by.values()) * (G / (G - 1.0))) / W
          if G >= 2 else float('nan'))
    t = mean / se if se == se and se > 0 else float('nan')
    return dict(n=len(rows), vol=W, mean=mean, se=se, t=t, G=G)


def table(title, rows, keyfn, order=None):
    b = defaultdict(list)
    for r in rows:
        k = keyfn(r)
        if k is not None:
            b[k].append(r)
    keys = order if order is not None else sorted(b)
    print(f'\n=== {title} ===')
    print(f'{"bucket":>20s} {"n":>6s} {"vol":>10s} {"tk":>4s} '
          f'{"DRIFT":>7s} {"se":>6s} {"t":>6s} {"spread":>7s} {"fee":>6s} '
          f'{"NET":>7s} {"se":>6s}')
    for k in keys:
        rr = b.get(k)
        if not rr:
            continue
        d, s, f, nt = (agg(rr, 'drift'), agg(rr, 'spread'),
                       agg(rr, 'fee'), agg(rr, 'net'))
        print(f'{str(k):>20s} {d["n"]:>6d} {d["vol"]:>10.0f} {d["G"]:>4d} '
              f'{d["mean"]:>7.3f} {d["se"]:>6.3f} {d["t"]:>6.2f} '
              f'{s["mean"]:>7.3f} {f["mean"]:>6.3f} '
              f'{nt["mean"]:>7.3f} {nt["se"]:>6.3f}')
    return b


def bkt(v, edges, labels):
    if v is None:
        return None
    for e, l in zip(edges, labels):
        if v <= e:
            return l
    return labels[-1]


def main():
    for h in ('1m', '5m', '30m'):
        rows = load(h)
        print('\n' + '#' * 78)
        d, s, f, nt = (agg(rows, 'drift'), agg(rows, 'spread'),
                       agg(rows, 'fee'), agg(rows, 'net'))
        print(f'# HORIZON {h}  n={len(rows)}  vol={d["vol"]:.0f}  G={d["G"]}')
        print(f'#   DRIFT {d["mean"]:+.3f}c (se {d["se"]:.3f}, t={d["t"]:.2f})'
              f'   SPREAD {s["mean"]:.3f}c   FEE {f["mean"]:.3f}c'
              f'   NET {nt["mean"]:+.3f}c')
        print('#' * 78)

        table('by pre_spread', rows,
              lambda r: bkt(r['pspr'], [1, 2, 3, 4, 6, 9, 10 ** 9],
                            ['1', '2', '3', '4', '5-6', '7-9', '10+']),
              ['1', '2', '3', '4', '5-6', '7-9', '10+'])

        table('by yes-equiv price', rows,
              lambda r: bkt(r['ypx'], [9, 24, 49, 74, 89, 100],
                            ['1-9', '10-24', '25-49', '50-74', '75-89', '90-99']),
              ['1-9', '10-24', '25-49', '50-74', '75-89', '90-99'])

        table('by depth_at_price', rows,
              lambda r: bkt(r['dap'], [10, 50, 100, 300, 1000, 10 ** 9],
                            ['1-10', '11-50', '51-100', '101-300',
                             '301-1k', '1k+']),
              ['1-10', '11-50', '51-100', '101-300', '301-1k', '1k+'])

        table('by trade_through', rows, lambda r: f'tt={r["tt"]}')
        table('by taker direction', rows,
              lambda r: 'taker BUYS yes' if r['dir'] > 0 else 'taker SELLS yes')
        table('by fractional size (retail $-denominated)', rows,
              lambda r: 'fractional' if r['frac'] else 'whole-lot')
        table('by fill size', rows,
              lambda r: bkt(r['size'], [1, 5, 20, 50, 200, 10 ** 9],
                            ['<=1', '1-5', '6-20', '21-50', '51-200', '201+']),
              ['<=1', '1-5', '6-20', '21-50', '51-200', '201+'])

        if h != '1m':
            continue

        # The one candidate from the first pass: cheap contracts. Split it.
        cheap = [r for r in rows if r['ypx'] <= 9]
        table('PRICE 1-9c  x  taker direction', cheap,
              lambda r: 'taker BUYS yes(cheap)' if r['dir'] > 0
              else 'taker SELLS yes(cheap)')
        table('PRICE 1-9c  x  spread', cheap,
              lambda r: bkt(r['pspr'], [1, 2, 10 ** 9], ['1', '2', '3+']),
              ['1', '2', '3+'])
        table('PRICE 1-9c  x  ticker family', cheap,
              lambda r: r['ticker'].split('-')[0][:18])

        # Same for 90-99 (the mirror image; fee is equally small there).
        rich = [r for r in rows if r['ypx'] >= 90]
        table('PRICE 90-99c x taker direction', rich,
              lambda r: 'taker BUYS yes(rich)' if r['dir'] > 0
              else 'taker SELLS yes(rich)')

        # Ticker-family concentration of the whole sample.
        table('by ticker family (all)', rows,
              lambda r: r['ticker'].split('-')[0][:18])


if __name__ == '__main__':
    main()
