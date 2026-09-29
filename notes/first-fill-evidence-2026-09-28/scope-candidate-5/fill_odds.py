"""GET-only public estimate of time-to-first-fill when joining each touch.

For each ticker: the queue resting at the best YES bid and best NO bid, and the
taker flow that hit each side over the recent trade window. Joining the touch
(H-Q-2) with S contracts, the first contract of ours fills only after the queue
ahead is consumed, so expected wait ~= queue_ahead / hit_rate per side.
"""
import datetime as dt
import json
import sys
import urllib.request

BASE = "https://external-api.kalshi.com/trade-api/v2"


def get(path):
    with urllib.request.urlopen(BASE + path, timeout=30) as r:
        return json.load(r)


def levels(ob, side):
    fp = ob.get("orderbook_fp") or {}
    rows = fp.get(f"{side}_dollars")
    if rows is not None:
        return [(round(float(p) * 100), float(q)) for p, q in rows]
    rows = (ob.get("orderbook") or {}).get(side) or []
    return [(int(p), float(q)) for p, q in rows]


now = dt.datetime.now(dt.timezone.utc)
out = []
for t in sys.argv[1:]:
    ob = get(f"/markets/{t}/orderbook")
    yes, no = levels(ob, "yes"), levels(ob, "no")
    by, bn = max(yes, default=(None, 0)), max(no, default=(None, 0))
    trades = get(f"/markets/trades?ticker={t}&limit=1000").get("trades", [])
    hit_yes = hit_no = 0.0  # contracts whose taker SOLD into the YES bid / NO bid
    oldest = now
    for tr in trades:
        ts = dt.datetime.fromisoformat(tr["created_time"].replace("Z", "+00:00"))
        oldest = min(oldest, ts)
        n = float(tr.get("count_fp") or tr.get("count") or 0)
        # taker_side=no: taker bought NO = sold YES into the YES bid.
        if tr.get("taker_side") == "no":
            hit_yes += n
        elif tr.get("taker_side") == "yes":
            hit_no += n
    hours = max((now - oldest).total_seconds() / 3600, 1e-6)
    row = {"ticker": t, "yes_bid": by[0], "yes_bid_queue": by[1], "no_bid": bn[0],
           "no_bid_queue": bn[1], "window_h": round(hours, 2), "trades": len(trades),
           "yes_bid_hits_per_h": round(hit_yes / hours, 1),
           "no_bid_hits_per_h": round(hit_no / hours, 1)}
    for side in ("yes", "no"):
        rate = row[f"{side}_bid_hits_per_h"]
        q = row[f"{side}_bid_queue"]
        row[f"{side}_wait_min"] = round(60 * q / rate, 1) if rate > 0 else None
    out.append(row)
    print(json.dumps(row))
json.dump({"observed_at_utc": now.isoformat(), "rows": out},
          open(sys.argv[0].rsplit("/", 1)[0] + "/fill-odds.json", "w"), indent=1)
