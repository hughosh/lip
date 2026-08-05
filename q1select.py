#!/usr/bin/env python3
"""Q1 market screener for the Phase 2 liquidity probe.

Applies every Q1 filter from notes/phase2-spec.md against fresh data and prints
an auditable table: one column per filter, so a rejection can always be traced
to the rule that caused it.

    Q1.1  program remaining >= 90% of total length (never enter past 25% elapsed)
    Q1.2  qualifies_now AND qualifies_after at --size per side
    Q1.3  depth slack >= 1.3x Target Size on the thinner side
    Q1.4  mid between 10c and 90c
    Q1.5  yes_bid + no_bid <= 99
    Q1.6  prefer near settlement (days to market close)
    Q1.7  prefer lower 24h volume among survivors

Nothing here places an order. Read-only, unauthenticated public endpoints.
"""
from __future__ import annotations

import argparse
import concurrent.futures as cf
import datetime as dt
import json
import sys
import urllib.request

sys.path.insert(0, "/Users/hugh/kek/lip")
from score import API, book, programs, qualify

UTC = dt.timezone.utc


def now() -> dt.datetime:
    return dt.datetime.now(UTC)


def ts(s: str) -> dt.datetime | None:
    """Parse a Kalshi RFC3339 timestamp; they vary in fractional-second digits."""
    if not s:
        return None
    try:
        return dt.datetime.fromisoformat(s.replace("Z", "+00:00"))
    except ValueError:
        return None


def num(v) -> float:
    try:
        return float(v)
    except (TypeError, ValueError):
        return 0.0


def market(ticker: str) -> dict:
    try:
        with urllib.request.urlopen(f"{API}/markets/{ticker}", timeout=20) as r:
            return json.load(r)["market"]
    except Exception:
        return {}


def side_den(levels, target, df, size):
    """Denominator of our share on one side after joining the touch with `size`."""
    joined = [(levels[0][0], levels[0][1] + size)] + list(levels[1:])
    ref, qual = qualify(joined, target)
    if ref is None:
        return None, 0.0
    return ref, sum(df ** (ref - p) * s for p, s in qual)


def evaluate(p: dict, size: int, t_now: dt.datetime) -> dict | None:
    """Score one program against every Q1 filter. Returns a row, or None if the
    book could not be fetched at all."""
    tkr = p["market_ticker"]
    tgt = float(p["target_size_fp"])
    df = p["discount_factor_bps"] / 10000.0
    pool = p["period_reward"] / 10000.0

    try:
        yes, no = book(tkr)
    except Exception:
        return None
    if not yes or not no:
        return None

    m = market(tkr)
    r: dict = {
        "ticker": tkr, "program": p["id"][:8], "pool": pool, "target": tgt, "df": df,
        "title": (m.get("title") or "")[:70],
        "vol24": num(m.get("volume_24h_fp")), "oi": num(m.get("open_interest_fp")),
        "fails": [],
    }

    # --- Q1.1 elapsed fraction of the program's Time Period ---
    start, end = ts(p.get("start_date")), ts(p.get("end_date"))
    if not start or not end or end <= start:
        r["fails"].append("Q1.1:no-dates")
        r["elapsed"] = 1.0
        r["period_h"] = 0.0
        r["remain_h"] = 0.0
    else:
        total = (end - start).total_seconds()
        r["period_h"] = total / 3600.0
        r["remain_h"] = max(0.0, (end - t_now).total_seconds()) / 3600.0
        r["elapsed"] = min(1.0, max(0.0, (t_now - start).total_seconds() / total))
        if r["elapsed"] > 0.25:
            r["fails"].append(f"Q1.1:elapsed{r['elapsed']:.0%}")
    r["end"] = (end.strftime("%m-%d %H:%MZ") if end else "?")

    # --- Q1.2 qualifies now and after we add our size ---
    ry, qy = qualify(yes, tgt)
    rn, qn = qualify(no, tgt)
    r["q_now"] = ry is not None and rn is not None
    dy = side_den(yes, tgt, df, size)
    dn = side_den(no, tgt, df, size)
    r["q_after"] = dy[0] is not None and dn[0] is not None
    if not r["q_now"]:
        r["fails"].append("Q1.2:not-now")
    if not r["q_after"]:
        r["fails"].append("Q1.2:not-after")

    r["share"] = 0.0
    if r["q_after"] and dy[1] and dn[1]:
        r["share"] = (size / dy[1] + size / dn[1]) / 2.0
    r["earn"] = pool * r["share"]

    # --- Q1.3 depth slack on the thinner side ---
    r["slack"] = min(sum(s for _, s in yes), sum(s for _, s in no)) / tgt
    if r["slack"] < 1.3:
        r["fails"].append(f"Q1.3:slack{r['slack']:.2f}")

    # --- Q1.4 / Q1.5 price sanity ---
    ybest, nbest = yes[0][0], no[0][0]
    r["ybest"], r["nbest"] = ybest, nbest
    r["spread"] = 100 - ybest - nbest
    r["mid"] = ybest + r["spread"] / 2.0
    r["cost"] = size * (ybest + nbest) / 100.0
    r["touch"] = min(yes[0][1], no[0][1])
    if not (10 <= r["mid"] <= 90):
        r["fails"].append(f"Q1.4:mid{r['mid']:.0f}")
    if ybest + nbest > 99:
        r["fails"].append(f"Q1.5:sum{ybest + nbest}")

    # --- Q1.6 time to settlement (market close, NOT program end) ---
    close = ts(m.get("close_time")) or ts(m.get("expiration_time"))
    r["settle_d"] = ((close - t_now).total_seconds() / 86400.0) if close else 9999.0
    r["close"] = close.strftime("%Y-%m-%d") if close else "?"
    r["early"] = bool(m.get("can_close_early"))

    r["ret"] = (r["earn"] / r["cost"] * 100.0) if r["cost"] else 0.0
    r["ok"] = not r["fails"]
    return r


def main() -> None:
    ap = argparse.ArgumentParser()
    ap.add_argument("--size", type=int, default=50, help="contracts per side (Q2.2)")
    ap.add_argument("--budget", type=float, default=100.0, help="hard cap (Q2.1)")
    ap.add_argument("--top", type=int, default=20)
    ap.add_argument("--show-rejects", action="store_true",
                    help="also print near-misses and the rule that killed them")
    a = ap.parse_args()

    t_now = now()
    ps = programs("active")
    rows = []
    with cf.ThreadPoolExecutor(max_workers=8) as ex:
        futs = [ex.submit(evaluate, p, a.size, t_now) for p in ps]
        for i, f in enumerate(cf.as_completed(futs), 1):
            r = f.result()
            if r:
                rows.append(r)
            print(f"\r  scanned {i}/{len(ps)}", end="", file=sys.stderr)
    print(file=sys.stderr)

    print(f"scan at {t_now.strftime('%Y-%m-%d %H:%M:%SZ')}  "
          f"programs={len(ps)} books={len(rows)}  size={a.size}/side budget=${a.budget:.0f}")

    ok = [r for r in rows if r["ok"] and r["cost"] <= a.budget]
    overbudget = [r for r in rows if r["ok"] and r["cost"] > a.budget]
    if overbudget:
        print(f"  ({len(overbudget)} passed Q1 but exceed the ${a.budget:.0f} budget "
              f"at {a.size}/side — would need smaller size)")

    # Q1.6 then Q1.7: near settlement first, then quieter book. Share is a
    # tiebreaker, deliberately NOT the sort key (spec §4 Q1: >50% is a red flag).
    ok.sort(key=lambda r: (r["settle_d"], r["vol24"], -r["earn"]))

    print(f"\n{len(ok)} candidates pass ALL Q1 filters, sorted by Q1.6 (settlement) "
          f"then Q1.7 (volume):\n")
    hdr = (f"{'market':<36}{'pool$':>6}{'shr':>6}{'earn$':>7}{'cost$':>7}"
           f"{'tgt':>6}{'slack':>6}{'mid':>4}{'sp':>3}{'elap':>6}{'remH':>6}"
           f"{'setl_d':>7}{'vol24':>7}")
    print(hdr)
    print("-" * len(hdr))
    for r in ok[:a.top]:
        print(f"{r['ticker'][:36]:<36}{r['pool']:>6.0f}{r['share']:>6.1%}{r['earn']:>7.2f}"
              f"{r['cost']:>7.2f}{r['target']:>6.0f}{r['slack']:>6.1f}{r['mid']:>4.0f}"
              f"{r['spread']:>3.0f}{r['elapsed']:>6.0%}{r['remain_h']:>6.0f}"
              f"{r['settle_d']:>7.1f}{r['vol24']:>7.0f}")

    print("\nDetail on the leading candidates:")
    for r in ok[:6]:
        print(f"\n  {r['ticker']}   program {r['program']}")
        print(f"    {r['title']}")
        print(f"    bid yes {r['ybest']}c / no {r['nbest']}c  sum={r['ybest'] + r['nbest']} "
              f"spread={r['spread']}c  touch={r['touch']:.0f}  DF={r['df']}")
        print(f"    ${r['cost']:.2f} for {a.size}+{a.size}  ->  share {r['share']:.1%} "
              f"of ${r['pool']:.0f} = ${r['earn']:.2f}  ({r['ret']:.0f}% per period)")
        print(f"    period {r['period_h']:.0f}h, {r['elapsed']:.0%} elapsed, "
              f"{r['remain_h']:.0f}h remain, ends {r['end']}")
        print(f"    market settles {r['close']} ({r['settle_d']:.1f}d)"
              f"{'  CAN CLOSE EARLY' if r['early'] else ''}"
              f"  vol24h {r['vol24']:.0f}  OI {r['oi']:.0f}")

    if a.show_rejects:
        near = [r for r in rows if r["fails"] and r["share"] > 0.02]
        near.sort(key=lambda r: -r["earn"])
        print(f"\n\nRejected but otherwise interesting ({len(near)}), "
              f"with the rule that killed each:\n")
        for r in near[:25]:
            print(f"  {r['ticker'][:40]:<40} shr{r['share']:>6.1%} "
                  f"earn${r['earn']:>6.2f} setl{r['settle_d']:>7.1f}d   "
                  f"{', '.join(r['fails'])}")


if __name__ == "__main__":
    main()
