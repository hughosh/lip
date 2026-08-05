#!/usr/bin/env python3
"""Screen for a single small-probe market: ~$20, 20 contracts a side.

Filters to markets that are actually SAFE to sit in for a day, not just the
ones with the fattest headline share.
"""
import sys, json, urllib.request
sys.path.insert(0, "/Users/hugh/kek/lip")
from score import programs, book, qualify, API

SIZE = 20
BUDGET = 20.0

def mkt(t):
    try:
        return json.load(urllib.request.urlopen(f"{API}/markets/{t}", timeout=20))["market"]
    except Exception:
        return {}

def num(v):
    try: return float(v)          # _fp fields are STRINGS
    except (TypeError, ValueError): return 0.0

rows = []
for p in programs("active"):
    t = p["market_ticker"]; tgt = float(p["target_size_fp"]); df = p["discount_factor_bps"]/10000.0
    try: yes, no = book(t)
    except Exception: continue
    if not yes or not no: continue

    ry, qy = qualify(yes, tgt); rn, qn = qualify(no, tgt)
    if ry is None or rn is None: continue          # doesn't qualify -> earns nothing

    ybest, nbest = yes[0][0], no[0][0]
    cost = SIZE * (ybest + nbest) / 100.0
    if cost > BUDGET: continue
    mid = ybest + (100 - ybest - nbest) / 2.0
    if not (10 <= mid <= 90): continue             # avoid near-certain markets

    share = 0.0
    for lv in (yes, no):
        j = [(lv[0][0], lv[0][1] + SIZE)] + list(lv[1:])
        r, q = qualify(j, tgt)
        den = sum(df**(r - px)*s for px, s in q)
        share += SIZE/den if den else 0
    share /= 2.0

    # robustness: how much slack above Target Size on the THINNER side.
    # A book that only just clears target can drop below it and void the
    # snapshot for everyone.
    slack = min(sum(s for _, s in yes), sum(s for _, s in no)) / tgt
    m = mkt(t)
    rows.append(dict(t=t, pool=p["period_reward"]/10000.0, share=share, cost=cost,
                     earn=p["period_reward"]/10000.0*share, mid=mid, slack=slack,
                     touch=min(yes[0][1], no[0][1]), spread=100-ybest-nbest,
                     ybest=ybest, nbest=nbest, end=p["end_date"][:16],
                     vol=num(m.get("volume_24h_fp")), oi=num(m.get("open_interest_fp")),
                     title=(m.get("title") or "")[:58]))

rows = [r for r in rows if r["slack"] >= 1.3]      # need real depth cushion
rows.sort(key=lambda r: -r["earn"])
print(f"{len(rows)} markets qualify, fit ${BUDGET:.0f}, mid 10-90c, >=1.3x target depth\n")
hdr = f"{'market':<34}{'pool$':>6}{'share':>7}{'earn$':>7}{'cost$':>7}{'mid':>5}{'sprd':>5}{'touch':>6}{'slack':>6}{'vol24':>7}"
print(hdr); print("-"*len(hdr))
for r in rows[:18]:
    print(f"{r['t'][:34]:<34}{r['pool']:>6.0f}{r['share']:>7.1%}{r['earn']:>7.2f}"
          f"{r['cost']:>7.2f}{r['mid']:>5.0f}{r['spread']:>5.0f}{r['touch']:>6.0f}"
          f"{r['slack']:>6.1f}{r['vol']:>7.0f}")
print("\nDetail on top 6:")
for r in rows[:6]:
    print(f"\n  {r['t']}\n    {r['title']}")
    print(f"    bid yes {r['ybest']}c / bid no {r['nbest']}c  -> ${r['cost']:.2f} for {SIZE}+{SIZE}"
          f" | period ends {r['end']} | vol24h {r['vol']:.0f} OI {r['oi']:.0f}")
