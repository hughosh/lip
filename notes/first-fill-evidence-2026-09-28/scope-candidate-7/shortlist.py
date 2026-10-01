"""GET-only public shortlist for the candidate-6 R2 stage (the candidate-5 method).

  shortlist.py <dir> select  scope.json -> top40.json: both sides quoted, linear_cent,
                             H-SEL-6/7 with a 15-85c mid margin, top 40 by volume_24h.
  shortlist.py <dir> rank    fill-odds.json -> series-fees.json + shortlist.json: the
                             orderbook re-checked against H-SEL-6/7, series fee_type
                             quadratic, ranked by the worse side's join-the-touch wait.

No account, no credentials, no writes. Diagnostic, not launch authority.
"""
import datetime as dt
import json
import sys
import time
import urllib.request

BASE = "https://external-api.kalshi.com/trade-api/v2"
SP, MODE = sys.argv[1], sys.argv[2]
MID_MIN, MID_MAX, BID_SUM_MAX = 15, 85, 99


def passes(yes_bid, no_bid):
    if yes_bid is None or no_bid is None or yes_bid < 1 or no_bid < 1:
        return False
    return yes_bid + no_bid <= BID_SUM_MAX and MID_MIN <= (yes_bid + 100 - no_bid) / 2 <= MID_MAX


if MODE == "select":
    rows = json.load(open(f"{SP}/scope.json"))["rows"]
    keep = [r for r in rows if r.get("price_level_structure") == "linear_cent"
            and r["yes_bid"] is not None and r["yes_ask"] is not None
            and passes(r["yes_bid"], 100 - r["yes_ask"])]
    keep.sort(key=lambda r: -r["volume_24h"])
    top = [r["ticker"] for r in keep[:40]]
    json.dump(top, open(f"{SP}/top40.json", "w"))
    print(f"{len(rows)} tradeable; {len(keep)} pass H-SEL-6/7 with the {MID_MIN}-{MID_MAX}c margin; "
          f"top {len(top)} by volume_24h", file=sys.stderr)
elif MODE == "rank":
    odds = json.load(open(f"{SP}/fill-odds.json"))
    scope = {r["ticker"]: r for r in json.load(open(f"{SP}/scope.json"))["rows"]}
    fees = {}
    for series in sorted({r["ticker"].split("-")[0] for r in odds["rows"]}):
        with urllib.request.urlopen(f"{BASE}/series/{series}", timeout=30) as resp:
            fees[series] = json.load(resp)["series"].get("fee_type")
        time.sleep(0.15)
    json.dump(fees, open(f"{SP}/series-fees.json", "w"), indent=1, sort_keys=True)
    ranked = []
    for r in odds["rows"]:
        s = scope[r["ticker"]]
        fee = fees.get(r["ticker"].split("-")[0])
        waits = (r["yes_wait_min"], r["no_wait_min"])
        if fee != "quadratic" or not passes(r["yes_bid"], r["no_bid"]) or None in waits:
            continue
        yb, nb = r["yes_bid"], r["no_bid"]
        ranked.append({
            "ticker": r["ticker"], "title": s["title"],
            "book": f"YES bid {yb} / NO bid {nb} (mid {(yb + 100 - nb) / 2:g}c, bid sum {yb + nb})",
            "worse_wait_min": max(waits), "yes_wait_min": waits[0], "no_wait_min": waits[1],
            "yes_bid_queue": r["yes_bid_queue"], "no_bid_queue": r["no_bid_queue"],
            "yes_hits_per_h": r["yes_bid_hits_per_h"], "no_hits_per_h": r["no_bid_hits_per_h"],
            "trades": r["trades"], "window_h": r["window_h"], "fee_type": fee,
            "market_close": s["close_time"], "program_end": s["program_end"],
            "period_reward": s["period_reward"], "target_size": s["target_size"],
            "discount_bps": s["discount_bps"]})
    ranked.sort(key=lambda row: row["worse_wait_min"])
    json.dump({"observed_utc": dt.datetime.now(dt.timezone.utc).isoformat(),
               "odds_observed_utc": odds["observed_at_utc"],
               "method": ("GET-only: incentives walk, public /markets join (close >= 24h, liquidity "
                          "program active with >= 4h left), both sides quoted, linear_cent, H-SEL-6/7 "
                          "with a 15-85c mid margin, top 40 by volume_24h -> per-ticker orderbook + last "
                          "1000 trades (fill-odds-40.jsonl) + /series fee_type (series-fees.json). Ranked "
                          "by the worse side's join-the-touch wait: repeated cycles need flow on both "
                          "sides, repeatedly."),
               "limits": ("Diagnostic, not launch authority. Waits are historical averages (queue ahead / "
                          "taker hit rate over the last 1000 trades), not fill promises. Prep re-reads "
                          "program/book/fees/account and re-applies H-SEL-6/7."),
               "shortlist": ranked}, open(f"{SP}/shortlist.json", "w"), indent=1)
    for row in ranked[:12]:
        print(f"{row['worse_wait_min']:>6} {row['yes_wait_min']:>6}/{row['no_wait_min']:<6} "
              f"{row['ticker']:<38} {row['book']:<44} close {row['market_close'][:10]} "
              f"prog_end {row['program_end'][:16]} trades {row['trades']}/{row['window_h']}h")
    print(f"{len(ranked)} ranked of {len(odds['rows'])}", file=sys.stderr)
else:
    raise SystemExit("mode must be select or rank")
