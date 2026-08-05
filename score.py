#!/usr/bin/env python3
"""Exact implementation of Kalshi's LIP scoring rule (Appendix A, Feb 2026 amendment).

Answers the only question that matters: if I join the touch with S contracts on both
sides, what share of the snapshot score do I actually get, and what does it cost?

Rule text (rules02112639183.pdf, effective 2026-02-28):
  - Reference Yes Price = highest yes bid (if < highest possible price).
  - Walk DOWN from reference accumulating size until cumulative >= Target Size.
    Those bids are the Qualifying Yes Bids. If the book never reaches Target Size,
    Qualifying Yes Bids is CLEARED -> that side scores nothing.
  - Score(bid) = DiscountFactor ** (ticks from Reference to bid price) * Size(bid)
  - Normalized within side; each side contributes 1.0 total across all users.
  - "Snapshots will be excluded if there is not two-sided liquidity."
"""
import json, urllib.request, argparse, statistics, sys

API = "https://api.elections.kalshi.com/trade-api/v2"


def fetch(url):
    with urllib.request.urlopen(url, timeout=20) as r:
        return json.load(r)


def programs(status="active"):
    return fetch(f"{API}/incentive_programs?status={status}&limit=200")["incentive_programs"]


def book(ticker):
    ob = fetch(f"{API}/markets/{ticker}/orderbook").get("orderbook_fp") or {}
    def side(k):
        # [[price_str, size_str], ...] ascending by price; best bid = highest price
        return sorted(((round(float(p) * 100), float(s)) for p, s in (ob.get(k) or [])),
                      key=lambda x: -x[0])
    return side("yes_dollars"), side("no_dollars")


def qualify(levels, target):
    """Walk down from the reference price accumulating size until target is met.

    Returns (reference_price_cents, [(price, size), ...]) or (None, []) if the
    book never reaches target -> the side has NO qualifying bids at all.
    """
    if not levels or levels[0][0] >= 100:
        return None, []
    ref = levels[0][0]
    tot, qual = 0.0, []
    for price, size in levels:
        tot += size
        qual.append((price, size))
        if tot >= target:
            return ref, qual
    return None, []          # never reached target -> cleared


def side_score(levels, target, df):
    """Total qualifying score on one side, and the reference price."""
    ref, qual = qualify(levels, target)
    if ref is None:
        return None, 0.0
    return ref, sum(df ** (ref - p) * s for p, s in qual)


def join(levels, size):
    """Add `size` contracts at the current best price (N=0, maximum credit)."""
    if not levels:
        return levels
    out = list(levels)
    out[0] = (out[0][0], out[0][1] + size)
    return out


def analyse(p, my_size):
    tgt = float(p["target_size_fp"])
    df = p["discount_factor_bps"] / 10000.0
    reward = p["period_reward"] / 10000.0
    try:
        yes, no = book(p["market_ticker"])
    except Exception as e:
        return None

    res = {"ticker": p["market_ticker"], "reward": reward, "target": tgt, "df": df,
           "yes_touch": yes[0][1] if yes else 0, "no_touch": no[0][1] if no else 0}

    # --- status quo: does the market currently qualify at all? ---
    ry, sy = side_score(yes, tgt, df)
    rn, sn = side_score(no, tgt, df)
    res["qualifies_now"] = ry is not None and rn is not None

    # --- after I join the touch with my_size on both sides ---
    ry2, sy2 = side_score(join(yes, my_size), tgt, df)
    rn2, sn2 = side_score(join(no, my_size), tgt, df)
    res["qualifies_after"] = ry2 is not None and rn2 is not None
    if not res["qualifies_after"]:
        res["share"] = 0.0
        res["cost"] = 0.0
        return res

    # my score at the touch is DF**0 * my_size = my_size on each side
    my_yes = my_size / sy2 if sy2 else 0.0
    my_no = my_size / sn2 if sn2 else 0.0
    res["share"] = (my_yes + my_no) / 2.0        # each side contributes 1.0 of 2.0

    # capital: S contracts at the yes touch + S at the no touch
    py = ry2 if ry2 is not None else 0
    pn = rn2 if rn2 is not None else 0
    res["cost"] = my_size * (py + pn) / 100.0
    res["earn"] = res["share"] * reward
    res["ret"] = (res["earn"] / res["cost"] * 100) if res["cost"] else 0.0
    return res


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--size", type=int, default=100, help="contracts to post per side")
    ap.add_argument("--top", type=int, default=20)
    a = ap.parse_args()

    ps = programs("active")
    rows = []
    for p in ps:
        r = analyse(p, a.size)
        if r:
            rows.append(r)
        print(f"\r  scanned {len(rows)}/{len(ps)}", end="", file=sys.stderr)
    print(file=sys.stderr)

    n = len(rows)
    qnow = sum(r["qualifies_now"] for r in rows)
    qaft = sum(r["qualifies_after"] for r in rows)
    pool = sum(r["reward"] for r in rows)
    print(f"active programs scanned : {n}")
    print(f"total pool              : ${pool:,.0f}")
    print(f"MEET target both sides now (snapshot would count) : {qnow} ({qnow/n:.0%})")
    print(f"still fail after I add {a.size}/side                : {n-qaft} ({(n-qaft)/n:.0%})")

    live = [r for r in rows if r["qualifies_after"] and r["cost"] > 0]
    live.sort(key=lambda r: -r["ret"])
    print(f"\nPosting {a.size} contracts per side, at the touch (best possible score):")
    shares = [r["share"] for r in live]
    if shares:
        print(f"  median share of pool captured : {statistics.median(shares):.2%}")
        print(f"  median return per period      : {statistics.median([r['ret'] for r in live]):.2f}%")
    print(f"\n{'market':<40}{'pool$':>7}{'cost$':>8}{'share':>8}{'earn$':>8}{'ret%':>8}")
    for r in live[:a.top]:
        print(f"{r['ticker']:<40}{r['reward']:>7.0f}{r['cost']:>8.0f}"
              f"{r['share']:>7.1%}{r['earn']:>8.2f}{r['ret']:>8.2f}")


if __name__ == "__main__":
    main()
