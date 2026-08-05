"""Conditional taker-edge slices against resting LIP liquidity.

Question: does any EX-ANTE observable condition flip the aggregate taker net
edge (-1.08c at 1m) positive?

Definitions (yes-denominated cents, per contract):
    resting markout   yes: mid_h - price        no: (100 - price) - mid_h
    taker gross       = -(resting markout)
    taker fee         = 7 * p * (1-p),  p = price/100      [maker fills are free]
    taker net         = taker gross - taker fee

Everything is SIZE-WEIGHTED. Standard errors are CLUSTERED BY TICKER, because
fills inside one market are not independent and neither are tickers inside an
event family (KXTRUMPMENTION-*). Cluster SE is the honest one here.

Read-only against rig.db. Never writes.
"""
import sqlite3
import math
import sys
from collections import defaultdict

DB = 'file:/Users/hugh/kek/lip/rig.db?mode=ro'


def fee_c(price):
    p = price / 100.0
    return 7.0 * p * (1.0 - p)


def load(horizon):
    """Rows with a valid markout at `horizon`. Returns list of dicts."""
    mid = f'mid_{horizon}'
    con = sqlite3.connect(DB, uri=True)
    q = f"""
        SELECT ticker, ts_ms, resting_side, price, size, taker_side,
               trade_through, pre_best_yes, pre_best_no, pre_yes_size,
               pre_no_size, pre_mid, pre_spread, depth_at_price,
               book_lag_ms, {mid}
        FROM fill
        WHERE source='observed' AND {mid} IS NOT NULL
    """
    out = []
    for (tk, ts, rs, price, size, tside, tt, pby, pbn, pys, pns,
         pmid, pspr, dap, lag, m) in con.execute(q):
        if rs == 'yes':
            rest = m - price
        elif rs == 'no':
            rest = (100 - price) - m
        else:
            continue
        gross = -rest
        f = fee_c(price)
        out.append(dict(
            ticker=tk, ts=ts, rs=rs, price=price, size=size, tside=tside,
            tt=tt, pby=pby, pbn=pbn, pys=pys, pns=pns, pmid=pmid,
            pspr=pspr, dap=dap, lag=lag, mid=m,
            gross=gross, fee=f, net=gross - f,
        ))
    con.close()
    return out


def agg(rows, field='net'):
    """Size-weighted mean + ticker-clustered SE.

    Cluster SE for a weighted mean: treat the estimator as a ratio
    mean = sum_i w_i x_i / sum_i w_i.  Per-cluster residual contribution
    u_g = sum_{i in g} w_i (x_i - mean).  Var(mean) = sum_g u_g^2 / (sum w)^2,
    with a G/(G-1) finite-cluster correction.
    """
    if not rows:
        return dict(n=0, vol=0, mean=float('nan'), se=float('nan'),
                    t=float('nan'), G=0)
    W = sum(r['size'] for r in rows)
    if W == 0:
        return dict(n=len(rows), vol=0, mean=float('nan'), se=float('nan'),
                    t=float('nan'), G=0)
    mean = sum(r['size'] * r[field] for r in rows) / W
    by = defaultdict(float)
    for r in rows:
        by[r['ticker']] += r['size'] * (r[field] - mean)
    G = len(by)
    if G < 2:
        se = float('nan')
    else:
        ss = sum(u * u for u in by.values())
        se = math.sqrt(ss * (G / (G - 1.0))) / W
    t = mean / se if se and se == se and se > 0 else float('nan')
    return dict(n=len(rows), vol=W, mean=mean, se=se, t=t, G=G)


def table(title, rows, keyfn, order=None, extra_fields=('gross', 'fee')):
    buckets = defaultdict(list)
    for r in rows:
        k = keyfn(r)
        if k is None:
            continue
        buckets[k].append(r)
    keys = order if order is not None else sorted(buckets)
    print(f'\n=== {title}  (n={len(rows)} fills) ===')
    hdr = f'{"bucket":>22s} {"n":>7s} {"vol":>9s} {"tk":>4s}'
    for f in extra_fields:
        hdr += f' {f:>8s}'
    hdr += f' {"NET":>8s} {"se":>7s} {"t":>7s}'
    print(hdr)
    for k in keys:
        b = buckets.get(k)
        if not b:
            continue
        a = agg(b, 'net')
        line = f'{str(k):>22s} {a["n"]:>7d} {a["vol"]:>9.0f} {a["G"]:>4d}'
        for f in extra_fields:
            line += f' {agg(b, f)["mean"]:>8.3f}'
        line += f' {a["mean"]:>8.3f} {a["se"]:>7.3f} {a["t"]:>7.2f}'
        print(line)
    return buckets


def bkt(v, edges, labels):
    if v is None:
        return None
    for e, l in zip(edges, labels):
        if v <= e:
            return l
    return labels[-1]


def main():
    for horizon in ('1m', '5m', '30m'):
        rows = load(horizon)
        print('\n' + '#' * 74)
        print(f'# HORIZON {horizon}   rows={len(rows)}  '
              f'volume={sum(r["size"] for r in rows)}  '
              f'tickers={len({r["ticker"] for r in rows})}')
        print('#' * 74)

        a = agg(rows, 'net')
        g = agg(rows, 'gross')
        f = agg(rows, 'fee')
        print(f'ALL: gross {g["mean"]:+.3f}c  fee {f["mean"]:.3f}c  '
              f'NET {a["mean"]:+.3f}c  (cluster se {a["se"]:.3f}, '
              f't={a["t"]:.2f}, G={a["G"]})')

        # --- 1. pre_spread ---
        sp_lbl = ['1', '2', '3', '4', '5-6', '7-9', '10-14', '15-24', '25+']
        table('by pre_spread', rows,
              lambda r: bkt(r['pspr'], [1, 2, 3, 4, 6, 9, 14, 24, 10 ** 9],
                            sp_lbl) if r['pspr'] is not None else None,
              order=sp_lbl)

        # --- 2. depth_at_price ---
        d_lbl = ['1-10', '11-50', '51-100', '101-300', '301-1000', '1001+']
        table('by depth_at_price', rows,
              lambda r: bkt(r['dap'], [10, 50, 100, 300, 1000, 10 ** 9], d_lbl),
              order=d_lbl)

        # --- 3. price - pre_mid (yes-denominated distance of resting px from mid) ---
        def dist(r):
            if r['pmid'] is None:
                return None
            ypx = r['price'] if r['rs'] == 'yes' else 100 - r['price']
            return ypx - r['pmid']
        dd_lbl = ['<=-10', '-9..-5', '-4..-2', '-1..1', '2..4', '5..9', '10+']
        table('by (resting yes-px - pre_mid)', rows,
              lambda r: bkt(dist(r), [-10, -5, -2, 1, 4, 9, 10 ** 9], dd_lbl),
              order=dd_lbl)

        # --- 4. trade_through ---
        table('by trade_through', rows, lambda r: f'tt={r["tt"]}')

        # --- 5. resting side ---
        table('by resting_side', rows, lambda r: r['rs'])

        # --- 6. price level ---
        p_lbl = ['1-9', '10-24', '25-49', '50-74', '75-89', '90-99']
        table('by yes-equivalent price', rows,
              lambda r: bkt(r['price'] if r['rs'] == 'yes' else 100 - r['price'],
                            [9, 24, 49, 74, 89, 100], p_lbl),
              order=p_lbl)

        # --- 7. book_lag_ms ---
        l_lbl = ['<=0', '1-50', '51-200', '201-1000', '1001+']
        table('by book_lag_ms', rows,
              lambda r: bkt(r['lag'], [0, 50, 200, 1000, 10 ** 9], l_lbl),
              order=l_lbl)

        # --- 8. fill size ---
        s_lbl = ['1-5', '6-20', '21-50', '51-200', '201+']
        table('by fill size', rows,
              lambda r: bkt(r['size'], [5, 20, 50, 200, 10 ** 9], s_lbl),
              order=s_lbl)

        # --- 9. hour of day (UTC) ---
        table('by UTC hour', rows,
              lambda r: f'{(r["ts"] // 3600000) % 24:02d}')

        if horizon != '1m':
            continue

        # --- 10. two-way: the most promising single cut crossed with spread ---
        table('tt=1 x pre_spread', [r for r in rows if r['tt'] == 1],
              lambda r: bkt(r['pspr'], [1, 2, 4, 9, 10 ** 9],
                            ['1', '2', '3-4', '5-9', '10+'])
              if r['pspr'] is not None else None,
              order=['1', '2', '3-4', '5-9', '10+'])

        table('depth<=10 x pre_spread', [r for r in rows if r['dap'] <= 10],
              lambda r: bkt(r['pspr'], [1, 2, 4, 9, 10 ** 9],
                            ['1', '2', '3-4', '5-9', '10+'])
              if r['pspr'] is not None else None,
              order=['1', '2', '3-4', '5-9', '10+'])


if __name__ == '__main__':
    main()
