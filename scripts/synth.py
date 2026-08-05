"""Where should a quote sit? LIP score decays 0.5^N with distance from the
reference; the maker edge GROWS with distance. This finds the product.

For each N = ticks behind the touch, measured from rig.db:
    vol_N   contracts/market-hour that actually trade at that distance
    edge_N  maker gross cents/contract at 30m (fee-free -- maker fills are free)
    dep_N   median resting depth at that price (what you must share the fill with)

Then, for a quote of size S at distance N:
    trading $/mkt-hr = vol_N * S/(dep_N+S) * edge_N / 100
    LIP     $/mkt-hr = POOL * S*DF^N / (FIELD + S*DF^N)

FIELD is the rest of the market's summed score and is NOT observable from public
data -- the exchange never publishes other participants' scores. It is swept
over a plausible range instead, and the conclusion is reported as a function
of it rather than pretending to a point estimate.
"""
import sqlite3
import math
from collections import defaultdict

DB = 'file:/Users/hugh/kek/lip/rig.db?mode=ro'
TICKER_HOURS = 11466.6      # sum of per-ticker reference spans, ts_ms>0 rows
POOL = 1.554                # $/market-hour, median of 200 live LIP programs
DF = 0.5                    # discount_factor_bps 5000, all 200 programs


def load(h='30m'):
    con = sqlite3.connect(DB, uri=True)
    q = f"""SELECT ticker, resting_side, price, size, pre_best_yes, pre_best_no,
                   pre_mid, depth_at_price, mid_{h}
            FROM fill
            WHERE source='observed' AND mid_{h} IS NOT NULL AND pre_mid IS NOT NULL
              AND pre_best_yes IS NOT NULL AND pre_best_no IS NOT NULL"""
    out = []
    for (tk, rs, price, size, pby, pbn, pmid, dap, m) in con.execute(q):
        d = -1 if rs == 'yes' else 1
        ypx = price if rs == 'yes' else 100 - price
        touch = pby if rs == 'yes' else pbn
        out.append(dict(ticker=tk, size=size, dap=dap, back=touch - price,
                        maker=d * (ypx - pmid) - d * (m - pmid)))
    con.close()
    return out


def wmean_se(rows, f='maker'):
    W = sum(r['size'] for r in rows)
    if W == 0:
        return float('nan'), float('nan')
    mean = sum(r['size'] * r[f] for r in rows) / W
    by = defaultdict(float)
    for r in rows:
        by[r['ticker']] += r['size'] * (r[f] - mean)
    G = len(by)
    se = (math.sqrt(sum(u * u for u in by.values()) * (G / (G - 1.0))) / W
          if G >= 2 else float('nan'))
    return mean, se


def main():
    rows = [r for r in load('30m') if r['back'] >= 0]
    byN = defaultdict(list)
    for r in rows:
        byN[min(r['back'], 12)].append(r)

    print('Measured book profile (30m markout, maker side, no fee)')
    print(f'{"N":>3s} {"n":>6s} {"vol/mkt-hr":>11s} {"edge c":>8s} {"se":>6s} '
          f'{"med dep":>8s} {"DF^N":>8s} {"trade $/h*":>11s}')
    prof = {}
    for N in sorted(byN):
        rr = byN[N]
        vol = sum(x['size'] for x in rr)
        vph = vol / TICKER_HOURS
        e, se = wmean_se(rr)
        deps = sorted(x['dap'] for x in rr)
        dep = deps[len(deps) // 2]
        prof[N] = (vph, e, dep)
        # trade $/h at S=100 contracts, for scale
        S = 100.0
        tr = vph * (S / (dep + S)) * e / 100.0
        print(f'{N:>3d} {len(rr):>6d} {vph:>11.3f} {e:>8.2f} {se:>6.2f} '
              f'{dep:>8.1f} {DF ** N:>8.4f} {tr:>11.4f}')
    print('  * trading $/market-hour for a 100-contract quote at that distance')

    print(f'\nExpected $/market-hour, quote size S=100, pool ${POOL:.3f}/mkt-hr')
    print('FIELD = rest of market\'s score in contract-equivalents at the reference')
    print(f'{"N":>3s} {"trade$":>8s} | ' +
          ' '.join(f'{"F=" + str(f):>9s}' for f in (500, 2000, 10000)))
    S = 100.0
    best = {}
    for N in sorted(prof):
        vph, e, dep = prof[N]
        tr = vph * (S / (dep + S)) * e / 100.0
        cells = []
        for F in (500, 2000, 10000):
            my = S * DF ** N
            lip = POOL * my / (F + my)
            tot = tr + lip
            cells.append(f'{tot:>9.4f}')
            best.setdefault(F, (-9e9, None))
            if tot > best[F][0]:
                best[F] = (tot, N)
        print(f'{N:>3d} {tr:>8.4f} | ' + ' '.join(cells))
    print()
    for F in (500, 2000, 10000):
        tot, N = best[F]
        print(f'  FIELD={F:>6d}: optimum N={N}  -> ${tot:.4f}/market-hour '
              f'= ${tot * 24:.2f}/market-day')

    print('\nWhat the whole book is worth per market-hour (ALL participants):')
    tot_tr = sum(prof[N][0] * prof[N][1] / 100.0 for N in prof if N > 0)
    at_touch = prof[0][0] * prof[0][1] / 100.0
    print(f'  behind-touch maker surplus : ${tot_tr:.4f}/mkt-hr')
    print(f'  at-touch maker surplus     : ${at_touch:+.4f}/mkt-hr')
    print(f'  LIP reward pool            : ${POOL:.4f}/mkt-hr')
    print(f'  => LIP is {POOL / max(tot_tr, 1e-9):.1f}x the entire behind-touch surplus')


if __name__ == '__main__':
    main()
