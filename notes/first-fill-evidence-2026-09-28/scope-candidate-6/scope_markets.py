"""GET-only public scope: join active LIP programs with public market stats.

No account, no credentials, no writes. Reads /markets?tickers=... in batches
from the same public base cmd/incentives uses.
"""
import datetime as dt
import json
import sys
import time
import urllib.parse
import urllib.request

BASE = "https://external-api.kalshi.com/trade-api/v2"
SP = sys.argv[1]
MIN_PROGRAM_LEFT = dt.timedelta(hours=float(sys.argv[2]) if len(sys.argv) > 2 else 4)
MIN_CLOSE_LEFT = dt.timedelta(hours=24)


def parse(t):
    return dt.datetime.fromisoformat(t.replace("Z", "+00:00"))


now = dt.datetime.now(dt.timezone.utc)
programs = json.load(open(f"{SP}/incentives-all.json"))["programs"]
live = {}
for p in programs:
    if p.get("incentive_type") != "liquidity":
        continue
    start, end = parse(p["start_date"]), parse(p["end_date"])
    if start <= now and end - now >= MIN_PROGRAM_LEFT:
        live.setdefault(p["market_ticker"], []).append(p)
tickers = sorted(live)
print(f"{len(programs)} programs; {len(tickers)} tickers with a liquidity program "
      f"active now and >= {MIN_PROGRAM_LEFT} left", file=sys.stderr)

markets = {}
for i in range(0, len(tickers), 100):
    batch = tickers[i:i + 100]
    q = urllib.parse.urlencode({"tickers": ",".join(batch), "limit": 1000})
    with urllib.request.urlopen(f"{BASE}/markets?{q}", timeout=30) as r:
        body = json.load(r)
    for m in body.get("markets", []):
        markets[m["ticker"]] = m
    time.sleep(0.15)

rows = []
for t, m in markets.items():
    close = parse(m["close_time"]) if m.get("close_time") else None
    if m.get("status") not in ("active", "open") or close is None or close - now < MIN_CLOSE_LEFT:
        continue

    def c(key):
        v = m.get(key)
        return None if v in (None, "") else round(float(v) * 100)
    yb, ya = c("yes_bid_dollars"), c("yes_ask_dollars")
    progs = live[t]
    rows.append({
        "ticker": t, "title": (m.get("title") or "")[:70],
        "yes_bid": yb, "yes_ask": ya,
        "spread": (ya - yb) if yb is not None and ya is not None else None,
        "volume_24h": float(m.get("volume_24h_fp") or m.get("volume_24h") or 0),
        "volume": float(m.get("volume_fp") or m.get("volume") or 0),
        "open_interest": float(m.get("open_interest_fp") or m.get("open_interest") or 0),
        "liquidity_dollars": float(m.get("liquidity_dollars") or 0),
        "close_time": m.get("close_time"), "can_close_early": m.get("can_close_early"),
        "program_end": max(p["end_date"] for p in progs),
        "period_reward": max(int(p["period_reward"]) for p in progs),
        "target_size": max(float(p["target_size_fp"]) for p in progs),
        "discount_bps": max(int(p["discount_factor_bps"]) for p in progs),
        "price_level_structure": m.get("price_level_structure"),
    })
json.dump({"observed_at_utc": now.isoformat(), "rows": rows}, open(f"{SP}/scope.json", "w"), indent=1)
print(f"{len(rows)} tradeable candidates written", file=sys.stderr)
