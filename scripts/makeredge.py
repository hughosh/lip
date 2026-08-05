"""Maker-side economics: what does a resting order earn, per contract filled?

Flips the sign of the taker decomposition. Maker fills pay NO fee on Kalshi,
so the maker's per-contract P&L to horizon h is

    maker gross = SPREAD - DRIFT
                = dir*(exec_yes_px - pre_mid) - dir*(mid_h - pre_mid)

The load-bearing split is AT-TOUCH vs DEEP. A fill at the touch is what a
market maker who joins the best bid actually experiences. A fill 5 ticks
inside the book is a sweep victim -- high per-contract capture, but you only
get it when someone runs the book over, and you cannot choose to only be there.

    at_touch: resting_side='yes' and price == pre_best_yes
              resting_side='no'  and price == pre_best_no
    depth   : ticks from the touch on the resting order's own leg.
"""
import sqlite3
import math
from collections import defaultdict

DB = 'file:/Users/hugh/kek/lip/rig.db?mode=ro'


def load(horizon='1m'):
    mid = f'mid_{horizon}'
    con = sqlite3.connect(DB, uri=True)
    q = f"""SELECT ticker, ts_ms, resting_side, price, size, trade_through,
                   pre_best_yes, pre_best_no, pre_mid, pre_spread,
                   depth_at_price, {mid}
            FROM fill
            WHERE source='observed' AND {mid} IS NOT NULL AND pre_mid IS NOT NULL
              AND pre_best_yes IS NOT NULL AND pre_best_no IS NOT NULL"""
    out = []
    for (tk, ts, rs, price, size, tt, pby, pbn, pmid, pspr, dap, m) in con.execute(q):
        d = -1 if rs == 'yes' else 1
        ypx = price if rs == 'yes' else 100 - price
        touch = pby if rs == 'yes' else pbn
        back = touch - price          # ticks BEHIND the touch on the resting leg
        spread = d * (ypx - pmid)
        drift = d * (m - pmid)
        out.append(dict(ticker=tk, ts=ts, rs=rs, price=price, ypx=ypx,
                        size=size, tt=tt, pspr=pspr, dap=dap, back=back,
                        spread=spread, drift=drift,
                        maker=spread - drift))
    con.close()
    return out


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


def table(title, rows, keyfn, order=None):
    b = defaultdict(list)
    for r in rows:
        k = keyfn(r)
        if k is not None:
            b[k].append(r)
    print(f'\n=== {title} ===')
    print(f'{"bucket":>18s} {"n":>6s} {"vol":>10s} {"volsh":>6s} {"tk":>4s} '
          f'{"spread":>7s} {"drift":>7s} {"MAKER":>7s} {"se":>6s} {"t":>6s}')
    tot = sum(r['size'] for r in rows)
    for k in (order if order is not None else sorted(b)):
        rr = b.get(k)
        if not rr:
            continue
        s, d, mk = agg(rr, 'spread'), agg(rr, 'drift'), agg(rr, 'maker')
        print(f'{str(k):>18s} {mk["n"]:>6d} {mk["vol"]:>10.0f} '
              f'{mk["vol"]/tot:>6.1%} {mk["G"]:>4d} '
              f'{s["mean"]:>7.3f} {d["mean"]:>7.3f} '
              f'{mk["mean"]:>7.3f} {mk["se"]:>6.3f} {mk["t"]:>6.2f}')
    return b


def bkt(v, edges, labels):
    for e, l in zip(edges, labels):
        if v <= e:
            return l
    return labels[-1]


def main():
    for h in ('1m', '5m', '30m'):
        rows = load(h)
        a = agg(rows, 'maker')
        print('\n' + '#' * 76)
        print(f'# MAKER, horizon {h}:  n={a["n"]} vol={a["vol"]:.0f} G={a["G"]}  '
              f'gross {a["mean"]:+.3f}c (se {a["se"]:.3f}, t={a["t"]:.2f})')
        print('#' * 76)

        table('at touch?', rows,
              lambda r: 'AT TOUCH' if r['back'] == 0
              else ('behind' if r['back'] > 0 else 'AHEAD?!'))

        at = [r for r in rows if r['back'] == 0]
        table('AT TOUCH x book spread', at,
              lambda r: bkt(r['pspr'], [1, 2, 3, 4, 6, 9, 10 ** 9],
                            ['1', '2', '3', '4', '5-6', '7-9', '10+']),
              ['1', '2', '3', '4', '5-6', '7-9', '10+'])

        table('ticks BEHIND the touch', rows,
              lambda r: bkt(r['back'], [0, 1, 2, 4, 9, 10 ** 9],
                            ['0 (touch)', '1', '2', '3-4', '5-9', '10+']),
              ['0 (touch)', '1', '2', '3-4', '5-9', '10+'])

        if h != '1m':
            continue
        table('AT TOUCH x spread, ex-TRUMPMENTION',
              [r for r in at if not r['ticker'].startswith('KXTRUMPMENTION')],
              lambda r: bkt(r['pspr'], [1, 2, 4, 10 ** 9],
                            ['1', '2', '3-4', '5+']),
              ['1', '2', '3-4', '5+'])
        table('AT TOUCH x family', at,
              lambda r: r['ticker'].split('-')[0][:16])


if __name__ == '__main__':
    main()
