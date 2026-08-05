"""Is the LIP book's half-spread priced at fair adverse-selection value?

Three tests:

  1. REGRESSION  drift_i = a + b*spread_i.  Glosten-Milgrom equilibrium implies
     b ~= 1, a ~= 0: the half-spread a resting quote charges equals the
     information it loses to the taker.  If reward-farming made quotes
     systematically TOO TIGHT for the risk, we would see b < 1 or a > 0
     (drift exceeding the spread charged), i.e. a free lunch for the taker.
     Weighted by size, ticker-clustered SE.

  2. FAMILY SIGN TEST.  48% of fills are KXTRUMPMENTION.  Collapse to one
     observation per event family and ask how many families are negative.

  3. SPLIT-SAMPLE.  Rank candidate buckets by net edge on the FIRST half of
     the sample; evaluate the winners on the SECOND half.  An edge that only
     exists in-sample is a bucket-search artifact, not a strategy.
"""
import sqlite3
import math
from collections import defaultdict
from takerdecomp import load, agg, bkt

DB = 'file:/Users/hugh/kek/lip/rig.db?mode=ro'


def wls_clustered(rows, xf, yf):
    """Weighted OLS y = a + b x, with ticker-clustered covariance."""
    W = sum(r['size'] for r in rows)
    mx = sum(r['size'] * xf(r) for r in rows) / W
    my = sum(r['size'] * yf(r) for r in rows) / W
    sxx = sum(r['size'] * (xf(r) - mx) ** 2 for r in rows)
    sxy = sum(r['size'] * (xf(r) - mx) * (yf(r) - my) for r in rows)
    b = sxy / sxx
    a = my - b * mx
    # cluster-robust: meat = sum_g (sum_i w_i xtil_i e_i)^2 ; bread = 1/sxx
    by = defaultdict(float)
    for r in rows:
        e = yf(r) - a - b * xf(r)
        by[r['ticker']] += r['size'] * (xf(r) - mx) * e
    G = len(by)
    meat = sum(u * u for u in by.values()) * (G / (G - 1.0))
    se_b = math.sqrt(meat) / sxx
    return a, b, se_b, G


def main():
    rows = load('1m')

    print('=' * 74)
    print('1. REGRESSION  drift = a + b * spread     (size-weighted, cluster SE)')
    print('=' * 74)
    for name, sub in [('ALL', rows),
                      ('spread<=2', [r for r in rows if r['pspr'] <= 2]),
                      ('tt=0', [r for r in rows if r['tt'] == 0]),
                      ('tt=1', [r for r in rows if r['tt'] == 1]),
                      ('depth<=50', [r for r in rows if r['dap'] <= 50]),
                      ('depth>300', [r for r in rows if r['dap'] > 300]),
                      ('KXTRUMPMENTION',
                       [r for r in rows if r['ticker'].startswith('KXTRUMPMENTION')]),
                      ('ex-TRUMPMENTION',
                       [r for r in rows if not r['ticker'].startswith('KXTRUMPMENTION')])]:
        a, b, se, G = wls_clustered(sub, lambda r: r['spread'], lambda r: r['drift'])
        t1 = (b - 1.0) / se
        print(f'  {name:>18s}  n={len(sub):>6d} G={G:>3d}   '
              f'b={b:6.3f} (se {se:.3f})  b-1 t={t1:+6.2f}   a={a:+6.3f}c')
    print('\n  b<1 or a>0 would mean quotes are too tight for the risk they carry.')

    print('\n' + '=' * 74)
    print('2. FAMILY SIGN TEST   (one row per event family)')
    print('=' * 74)
    fam = defaultdict(list)
    for r in rows:
        fam[r['ticker'].split('-')[0][:18]].append(r)
    neg = pos = 0
    print(f'  {"family":>20s} {"n":>6s} {"tk":>4s} {"drift":>7s} {"spread":>7s} '
          f'{"fee":>6s} {"NET":>7s} {"se":>6s}')
    for k in sorted(fam, key=lambda k: -len(fam[k])):
        rr = fam[k]
        d, s, f, nt = (agg(rr, 'drift'), agg(rr, 'spread'),
                       agg(rr, 'fee'), agg(rr, 'net'))
        flag = ''
        if nt['mean'] < 0:
            neg += 1
        else:
            pos += 1
            flag = '  <-- positive'
        print(f'  {k:>20s} {len(rr):>6d} {d["G"]:>4d} {d["mean"]:>7.3f} '
              f'{s["mean"]:>7.3f} {f["mean"]:>6.3f} {nt["mean"]:>7.3f} '
              f'{nt["se"]:>6.3f}{flag}')
    print(f'\n  {neg} of {neg + pos} families negative.')

    print('\n' + '=' * 74)
    print('3. SPLIT-SAMPLE   rank buckets in-sample (1st half), test out-of-sample')
    print('=' * 74)
    ts = sorted(r['ts'] for r in rows)
    cut = ts[len(ts) // 2]
    A = [r for r in rows if r['ts'] < cut]
    B = [r for r in rows if r['ts'] >= cut]
    print(f'  first half  n={len(A)}  tickers={len({r["ticker"] for r in A})}')
    print(f'  second half n={len(B)}  tickers={len({r["ticker"] for r in B})}')

    # Candidate grid: every combination of coarse spread x depth x price.
    def key(r):
        return (bkt(r['pspr'], [1, 2, 4, 10 ** 9], ['s1', 's2', 's3-4', 's5+']),
                bkt(r['dap'], [10, 100, 1000, 10 ** 9],
                    ['d<=10', 'd<=100', 'd<=1k', 'd1k+']),
                bkt(r['ypx'], [9, 24, 74, 100], ['p1-9', 'p10-24', 'p25-74', 'p75+']))

    ga, gb = defaultdict(list), defaultdict(list)
    for r in A:
        ga[key(r)].append(r)
    for r in B:
        gb[key(r)].append(r)

    cands = []
    for k, rr in ga.items():
        if sum(x['size'] for x in rr) < 5000:
            continue
        cands.append((agg(rr, 'net')['mean'], k, rr))
    cands.sort(reverse=True)

    print(f'\n  {"bucket":>34s} {"IN n":>6s} {"IN net":>8s} | '
          f'{"OUT n":>6s} {"OUT net":>8s} {"se":>6s} {"t":>6s}')
    for m, k, rr in cands[:12]:
        ob = gb.get(k, [])
        o = agg(ob, 'net') if ob else dict(n=0, mean=float('nan'),
                                           se=float('nan'), t=float('nan'))
        print(f'  {"/".join(k):>34s} {len(rr):>6d} {m:>8.3f} | '
              f'{o["n"]:>6d} {o["mean"]:>8.3f} {o["se"]:>6.3f} {o["t"]:>6.2f}')

    npos_in = sum(1 for m, _, _ in cands if m > 0)
    print(f'\n  {npos_in} of {len(cands)} buckets positive in-sample.')
    surv = 0
    for m, k, rr in cands:
        if m <= 0:
            continue
        ob = gb.get(k, [])
        if ob and agg(ob, 'net')['mean'] > 0:
            surv += 1
    print(f'  {surv} of those still positive out-of-sample.')


if __name__ == '__main__':
    main()
