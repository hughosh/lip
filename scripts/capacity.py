#!/usr/bin/env python
"""How many incentive dollars exist per period, and how many are capturable
per dollar of capital held in resting orders.

Answers two questions in dollars, not percentages:

  1. TOTAL POOL   -- what the exchange is paying out across all live programs.
  2. CAPTURE CURVE -- for a capital budget C held in resting orders, the dollars
     of incentive captured per period and per day, under an optimal allocation.

Method. Score is the frozen LIP rule: walk down from the reference (best bid)
accumulating size until target_size is met; each qualifying level N ticks behind
the reference contributes size * DF^N. Our share of a market is our qualifying
score over the total, and the market only pays at all if BOTH sides reach target
(the gate is market-level, not per-user -- CFTC filing).

Capital. Resting a yes bid at y cents costs y/100 dollars per contract; a no bid
at n cents costs n/100. Quoting both sides costs (y+n)/100 per contract-pair,
which is BELOW $1.00 whenever the market is wide -- wide markets are cheap to
quote.

Allocation is greedy on marginal dollars-per-capital, which is exact here
because each market's share(s) = s/(field+s) is concave in our size.

    python scripts/capacity.py [--refresh] [--df 0.5]
"""
import argparse
import json
import os
import sys
import time
import urllib.request

sys.path.insert(0, "/Users/hugh/kek/lip")

API = "https://api.elections.kalshi.com/trade-api/v2"
CACHE = "/Users/hugh/kek/lip/oppcache.json"


def fetch(url):
    with urllib.request.urlopen(url, timeout=25) as r:
        return json.load(r)


def side(ob, k):
    """Levels best-first (highest bid first). API returns ascending."""
    return sorted(((round(float(p) * 100), float(s)) for p, s in (ob.get(k) or [])),
                  key=lambda x: -x[0])


def build_cache():
    progs = fetch(f"{API}/incentive_programs?status=active&limit=200")["incentive_programs"]
    out = []
    for i, p in enumerate(progs, 1):
        tk = p["market_ticker"]
        try:
            ob = fetch(f"{API}/markets/{tk}/orderbook").get("orderbook_fp") or {}
        except Exception:
            continue
        out.append(dict(ticker=tk, pool=p["period_reward"] / 1e4,
                        target=float(p["target_size_fp"]),
                        df=p["discount_factor_bps"] / 10000.0,
                        start=p["start_date"], end=p["end_date"],
                        yes=side(ob, "yes_dollars"), no=side(ob, "no_dollars")))
        if i % 40 == 0:
            print(f"  fetched {i}/{len(progs)}", file=sys.stderr)
        time.sleep(0.06)
    json.dump(out, open(CACHE, "w"))
    return out


def qual_score(levels, target, df, extra_at_touch=0.0):
    """(reference, total qualifying score, our score from `extra`) per the LIP rule."""
    if not levels or levels[0][0] >= 100:
        return None, 0.0, 0.0
    lv = list(levels)
    if extra_at_touch:
        lv[0] = (lv[0][0], lv[0][1] + extra_at_touch)
    ref = lv[0][0]
    tot, score, ours = 0.0, 0.0, 0.0
    for price, size in lv:
        w = df ** (ref - price)
        take = size
        score += take * w
        if price == ref and extra_at_touch:
            ours = extra_at_touch * w
        tot += take
        if tot >= target:
            return ref, score, ours
    return None, 0.0, 0.0          # never reached target -> side does not qualify


def market_curve(m, sizes):
    """[(size, share, capital_per_period, qualifies)] for two-sided quoting at the touch."""
    out = []
    for s in sizes:
        ry, sy, oy = qual_score(m["yes"], m["target"], m["df"], s)
        rn, sn, on = qual_score(m["no"], m["target"], m["df"], s)
        if ry is None or rn is None:
            out.append((s, 0.0, 0.0, False))
            continue
        share = 0.5 * ((oy / sy if sy else 0.0) + (on / sn if sn else 0.0))
        cap = s * (ry + rn) / 100.0
        out.append((s, share, cap, True))
    return out


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--refresh", action="store_true")
    args = ap.parse_args()

    if args.refresh or not os.path.exists(CACHE):
        mk = build_cache()
    else:
        mk = json.load(open(CACHE))
    mk = [m for m in mk if m["yes"] and m["no"]]

    def days(m):
        """FULL program period, start->end. Steady-state: we quote the whole period.
        Using *remaining* time divides by ~0 for programs expiring today."""
        e = time.mktime(time.strptime(m["end"][:19], "%Y-%m-%dT%H:%M:%S"))
        b = time.mktime(time.strptime(m["start"][:19], "%Y-%m-%dT%H:%M:%S"))
        return max((e - b) / 86400.0, 0.5)

    print("=" * 96)
    print("1.  TOTAL INCENTIVE DOLLARS AVAILABLE")
    print("=" * 96)
    tot_pool = sum(m["pool"] for m in mk)
    tot_perday = sum(m["pool"] / days(m) for m in mk)
    print(f"  live programs with a book      : {len(mk)}")
    print(f"  total pool, summed over programs: ${tot_pool:,.0f}   (one period each)")
    print(f"  total pool per DAY             : ${tot_perday:,.2f}/day")
    dd = sorted(days(m) for m in mk)
    print(f"  period length remaining, days  : min {dd[0]:.1f}  median {dd[len(dd)//2]:.1f}"
          f"  max {dd[-1]:.1f}")
    pools = sorted(m["pool"] for m in mk)
    print(f"  pool per program               : min ${pools[0]:.0f}  median "
          f"${pools[len(pools)//2]:.0f}  max ${pools[-1]:.0f}")
    ngate = sum(1 for m in mk if market_curve(m, [0.0])[0][3])
    print(f"  markets already meeting target both sides (would pay today): {ngate}/{len(mk)}")

    # ------------------------------------------------------------------ curve
    SIZES = [10, 25, 50, 100, 200, 400, 800, 1600, 3200, 6400]
    # Per-market ordered increment lists. Increments MUST be taken in order --
    # you cannot buy the 6400-contract step without the steps beneath it.
    steps = {}
    per_mkt = {}
    for m in mk:
        cur = market_curve(m, [0.0] + SIZES)
        d = days(m)
        per_mkt[m["ticker"]] = (cur, d, m["pool"])
        seq, prev_pay, prev_cap = [], 0.0, 0.0
        for s, share, cap, ok in cur[1:]:
            if not ok:
                continue
            pay = share * m["pool"] / d          # $/day
            dcap, dpay = cap - prev_cap, pay - prev_pay
            if dcap > 0 and dpay > 0:
                seq.append((dcap, dpay, s))
            prev_pay, prev_cap = pay, cap
        if seq:
            steps[m["ticker"]] = seq

    print()
    print("=" * 96)
    print("2.  CAPTURE CURVE -- dollars captured vs capital held in resting orders")
    print("=" * 96)
    print("  Two-sided quoting at the touch, optimal greedy allocation across markets.")
    print("  'capital' = collateral locked in resting orders (yes bid + no bid).")
    print()
    print(f"  {'capital':>10}{'$/day':>10}{'$/week':>10}{'$/30d':>10}"
          f"{'markets':>9}{'largest pos':>12}{'marg $/day/$100':>17}")
    BANDS = [100, 250, 500, 1000, 2500, 5000, 10000, 25000, 50000, 100000]
    ptr = {tk: 0 for tk in steps}
    size_at = {tk: 0.0 for tk in steps}
    spent = pay = 0.0
    marg = float("nan")
    for budget in BANDS:
        while True:
            best, btk = None, None
            for tk, seq in steps.items():
                i = ptr[tk]
                if i >= len(seq):
                    continue
                dcap, dpay, s = seq[i]
                if spent + dcap > budget:
                    continue
                r = dpay / dcap
                if best is None or r > best[0]:
                    best, btk = (r, dcap, dpay, s), tk
            if best is None:
                break
            r, dcap, dpay, s = best
            spent += dcap
            pay += dpay
            size_at[btk] = s
            ptr[btk] += 1
            marg = r
        nmk = sum(1 for tk in size_at if size_at[tk] > 0)
        biggest = max(size_at.values()) if size_at else 0
        print(f"  {budget:>10,}{pay:>10.2f}{pay*7:>10.2f}{pay*30:>10.2f}"
              f"{nmk:>9}{biggest:>12,.0f}{marg*100:>17.2f}")

    print()
    print("  ceiling check: capturing 100% of every live program's pool")
    print(f"    ${tot_perday:,.2f}/day  = ${tot_perday*30:,.0f}/30d   "
          f"(unreachable; share -> 1 needs infinite size)")

    print()
    print("=" * 96)
    print("3.  WHERE THE CAPACITY IS  (top markets by $/day at 400 contracts/side)")
    print("=" * 96)
    print(f"  {'ticker':<38}{'pool':>7}{'days':>6}{'yes_b':>7}{'no_b':>7}"
          f"{'share':>8}{'capital':>9}{'$/day':>8}")
    rank = []
    for m in mk:
        cur, d, pool = per_mkt[m["ticker"]]
        row = [c for c in cur if c[0] == 400]
        if not row or not row[0][3]:
            continue
        s, share, cap, ok = row[0]
        rank.append((share * pool / d, m["ticker"], m, share, cap, d))
    rank.sort(key=lambda x: -x[0])
    for pd_, tk, m, share, cap, d in rank[:15]:
        print(f"  {tk[:37]:<38}{m['pool']:>7.0f}{d:>6.1f}"
              f"{m['yes'][0][0]:>7}{m['no'][0][0]:>7}{share:>8.3f}{cap:>9.0f}{pd_:>8.2f}")
    print()
    print(f"  markets that qualify at 400/side: {len(rank)}/{len(mk)}")

    # ------------------------------------------------------------- audit trail
    print()
    print("=" * 96)
    print("4.  AUDIT -- the single richest market, book shown so the share is checkable")
    print("=" * 96)
    if rank:
        _, tk, m, _, _, d = rank[0]
        print(f"  {tk}   pool ${m['pool']:.0f}   target {m['target']:.0f}   "
              f"df {m['df']}   period {d:.1f}d")
        for nm in ("yes", "no"):
            lv = m[nm]
            ref = lv[0][0]
            tot, sc, shown = 0.0, 0.0, 0
            print(f"    {nm} book (best first) -- qualifying walk to target "
                  f"{m['target']:.0f}:")
            for p, s in lv:
                w = m["df"] ** (ref - p)
                tot += s
                sc += s * w
                if shown < 8:
                    print(f"      {p:>3}c  size {s:>10,.1f}  N={ref-p:>3}  "
                          f"w={w:9.6f}  contrib {s*w:>10,.2f}  cum {tot:>10,.1f}")
                    shown += 1
                if tot >= m["target"]:
                    break
            print(f"      -> field qualifying score {sc:,.2f}")
            for probe in (10, 100, 400):
                print(f"         our {probe:>4} at touch -> share "
                      f"{probe / (sc + probe):6.3f}")
    print()
    print("  READ THIS BEFORE BELIEVING THE CURVE:")
    print("  * DF=0.5 means score concentrates at the touch. Where the touch is thin,")
    print("    a small size captures a large share -- that is the model, and it is why")
    print("    the low-capital end of the curve looks so rich.")
    print("  * That is also exactly where we would BE the entire touch, i.e. the sole")
    print("    liquidity provider. Measured at-touch economics on the rig population:")
    print("    +0.80c half-spread captured, -0.79c adverse 1-min drift, net ~0.")
    print("    In thinner books than the rig sample, that net is unmeasured, not zero.")
    print("  * Books here are ONE snapshot. Field size varies and other farmers react.")
    print("  * Nothing above deducts trading losses, fees, or downtime.")


if __name__ == "__main__":
    main()
