#!/usr/bin/env python
"""The live LIP opportunity set, priced with the frozen scorer.

Pool is FIXED per program, so our share is ours/(field+ours): return on capital
declines as we add depth and is hard-capped at "capture the whole pool". Scaling
can only come from breadth. What therefore matters per market is the ratio

        pool  /  (qualifying field score  x  period length)

i.e. dollars per day per contract of collateral. This ranks every live program
by that, using probescore.combined_share (the frozen LIP scoring rule) rather
than a crude total-depth proxy, because score decays 0.5^N behind the touch.
"""
import json
import sys
import time
import urllib.parse
import urllib.request
from collections import Counter

sys.path.insert(0, "/Users/hugh/kek/lip")
from auth import Signer  # noqa: E402
import probescore as ps  # noqa: E402

BASE = "https://api.elections.kalshi.com"
OURS = 100.0  # contracts per side, for ranking


def get(s, path, **q):
    qs = urllib.parse.urlencode(q)
    url = BASE + path + ("?" + qs if qs else "")
    return json.load(urllib.request.urlopen(
        urllib.request.Request(url, headers=s.headers("GET", path)), timeout=25))


def levels(raw):
    """orderbook_fp side -> Levels [(cents, size)], best level is LAST in the API."""
    out = []
    for lv in raw or []:
        try:
            out.append((int(round(float(lv[0]) * 100)), float(lv[1])))
        except Exception:
            continue
    return out


def main():
    s = Signer()
    progs, cur = [], ""
    while True:
        r = get(s, "/trade-api/v2/incentive_programs", limit=200,
                **({"cursor": cur} if cur else {}))
        progs.extend(r.get("incentive_programs", []))
        cur = r.get("cursor") or ""
        if not cur:
            break

    now = time.time()

    def ts(x):
        return time.mktime(time.strptime(x[:19], "%Y-%m-%dT%H:%M:%S")) if x else None

    print(f"{len(progs)} incentive programs returned")
    print("  period_reward  :", Counter(str(p['period_reward']) for p in progs).most_common())
    print("  target_size_fp :", Counter(str(p['target_size_fp']) for p in progs).most_common())
    print("  discount_factor:", Counter(str(p['discount_factor_bps']) for p in progs).most_common())
    print("  pool convention: score.py:77  period_reward / 10000 -> dollars")
    print(f"  => pools are ${min(p['period_reward'] for p in progs)/1e4:.0f} .. "
          f"${max(p['period_reward'] for p in progs)/1e4:.0f}")
    print(f"  the ONE program we farmed and were paid on had pool $1000 "
          f"(period_reward 10,000,000)")
    tot_pool = sum(p["period_reward"] / 1e4 for p in progs)
    print(f"  TOTAL pool across all live programs: ${tot_pool:,.0f} per period")

    print()
    print("=" * 112)
    print(f"PER-PROGRAM ECONOMICS  (resting {OURS:.0f} contracts/side at the touch)")
    print("=" * 112)
    print(f"{'ticker':<38}{'pool$':>7}{'days':>6}{'tgt':>6}"
          f"{'yes_d':>8}{'no_d':>8}{'share':>8}{'$/day':>8}{'c/day/ctr':>11}{'%/day':>8}")

    rows = []
    for p in progs:
        tk = p["market_ticker"]
        pool = p["period_reward"] / 1e4
        tgt = float(p["target_size_fp"])
        df = p["discount_factor_bps"] / 10000.0
        end, start = ts(p["end_date"]), ts(p["start_date"])
        days = (end - max(start, now)) / 86400.0
        if days <= 0.05:
            continue
        try:
            ob = get(s, f"/trade-api/v2/markets/{tk}/orderbook", depth=100)
        except Exception:
            continue
        fp = ob.get("orderbook_fp") or {}
        yes, no = levels(fp.get("yes_dollars")), levels(fp.get("no_dollars"))
        if not yes or not no:
            continue
        # join our size at each touch, then score the book as observed
        yj, nj = ps.join(yes, OURS), ps.join(no, OURS)
        ybest = max(x[0] for x in yes)
        nbest = max(x[0] for x in no)
        try:
            share, _ = ps.combined_share(yj, nj, tgt, df, OURS, ybest, OURS, nbest)
        except Exception:
            continue
        perday = share * pool / days
        collat = OURS * 1.00  # two-sided: yes bid at p + no bid at ~1-p ~= $1/pair
        rows.append((perday / collat * 100, tk, pool, days, tgt,
                     sum(x[1] for x in yes), sum(x[1] for x in no), share, perday))
        time.sleep(0.08)

    rows.sort(reverse=True)
    for pct, tk, pool, days, tgt, dy, dn, share, perday in rows[:25]:
        print(f"{tk[:37]:<38}{pool:>7.0f}{days:>6.1f}{tgt:>6.0f}"
              f"{dy:>8.0f}{dn:>8.0f}{share:>8.4f}{perday:>8.3f}"
              f"{perday/OURS*100:>11.3f}{pct:>7.3f}%")

    if not rows:
        print("  (no priced rows)")
        return
    pcts = sorted(r[0] for r in rows)
    n = len(pcts)
    print()
    print(f"{n} priced live programs. %/day on collateral at {OURS:.0f}/side:")
    print(f"   min {pcts[0]:.3f}%   p25 {pcts[n//4]:.3f}%   median {pcts[n//2]:.3f}%"
          f"   p75 {pcts[3*n//4]:.3f}%   max {pcts[-1]:.3f}%")
    print()
    print("WHAT $98.58 OF CAPITAL CAN ACTUALLY EARN")
    print("-" * 112)
    CAP = 98.58
    for k in (1, 3, 5, 10, 20):
        best = rows[:k]
        if len(best) < k:
            break
        per = CAP / k                      # collateral per market
        sz = per / 1.00                    # contracts/side, two-sided
        tot = 0.0
        for pct, tk, pool, days, tgt, dy, dn, share, perday in best:
            # rescale share for the smaller size: share ~ ours/(field+ours)
            field = OURS / share - OURS if share > 0 else float("inf")
            sh2 = sz / (field + sz)
            tot += sh2 * pool / days
        print(f"   top {k:>2} markets, {sz:>5.1f} contracts/side each: "
              f"${tot:>6.3f}/day  = {tot/CAP*100:>5.2f}%/day  = ${tot*30:>6.2f}/month")


if __name__ == "__main__":
    main()
