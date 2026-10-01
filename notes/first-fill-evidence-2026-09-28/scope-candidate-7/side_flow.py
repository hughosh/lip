"""GET-only per-side taker flow for shortlisted tickers (complements fill_odds.py).

The join-the-touch wait (queue / contracts per hour) looks short on a thin market
whose touch queue is tiny, even when a taker arrives only once an hour. This counts
the taker trades that hit each bid over the last 1000 trades and the last 3 h:
trades per hour, contracts per hour, trades of at least 12 contracts, and the age
of the last hit. Writes side-flow.json next to this script.
"""
import datetime as dt
import json
import statistics
import sys
import urllib.request

BASE = "https://external-api.kalshi.com/trade-api/v2"
now = dt.datetime.now(dt.timezone.utc)
out = []
for t in sys.argv[1:]:
    with urllib.request.urlopen(f"{BASE}/markets/trades?ticker={t}&limit=1000", timeout=30) as r:
        trades = json.load(r).get("trades", [])
    rows = [(dt.datetime.fromisoformat(tr["created_time"].replace("Z", "+00:00")),
             float(tr.get("count_fp") or tr.get("count") or 0), tr.get("taker_side")) for tr in trades]
    window_h = max((now - min((ts for ts, _, _ in rows), default=now)).total_seconds() / 3600, 1e-6)
    row = {"ticker": t, "trades": len(rows), "window_h": round(window_h, 2)}
    # taker_side no = taker bought NO = sold YES into the YES bid; taker_side yes hits the NO bid.
    for side, taker in (("yes_bid", "no"), ("no_bid", "yes")):
        hits = [(ts, n) for ts, n, ts_side in rows if ts_side == taker]
        recent = [n for ts, n in hits if now - ts <= dt.timedelta(hours=3)]
        row[side] = {
            "trades_per_h": round(len(hits) / window_h, 2),
            "contracts_per_h": round(sum(n for _, n in hits) / window_h, 1),
            "ge12_per_h": round(sum(1 for _, n in hits if n >= 12) / window_h, 2),
            "median_size": statistics.median([n for _, n in hits]) if hits else None,
            "last3h_trades": len(recent), "last3h_contracts": sum(recent),
            "last_hit_min_ago": round((now - max(ts for ts, _ in hits)).total_seconds() / 60, 1) if hits else None,
        }
    out.append(row)
    print(json.dumps(row))
json.dump({"observed_at_utc": now.isoformat(), "rows": out},
          open(sys.argv[0].rsplit("/", 1)[0] + "/side-flow.json", "w"), indent=1)
