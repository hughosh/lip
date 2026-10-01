"""GET-only touch-movement proxy for shortlisted tickers (candidate-7 addition, handoff section 1 step 3).
lip-14o needs a reducer requote under section 6.5, which needs the touch to move while we hold
inventory. A market that never requotes leaves checks (a) and (b) NOT EXERCISED. Public trades do
not show the bid directly, so this counts how often the traded YES price changed between
consecutive trades: per hour over the last 1000 trades and over the last 3 h, the distinct prices
and the price range in the last 3 h, and the age of the last change. Writes touch-moves.json next
to this script. No account, no credentials, no other writes. Diagnostic, not launch authority.
"""
import datetime as dt
import json
import sys
import urllib.request

BASE = "https://external-api.kalshi.com/trade-api/v2"
now = dt.datetime.now(dt.timezone.utc)
out = []


def price_cents(tr):
    for key in ("yes_price_dollars", "yes_price_fp"):
        if tr.get(key) not in (None, ""):
            return int(round(float(tr[key]) * 100))
    if tr.get("yes_price") not in (None, ""):
        return int(tr["yes_price"])
    return None


for t in sys.argv[1:]:
    with urllib.request.urlopen(f"{BASE}/markets/trades?ticker={t}&limit=1000", timeout=30) as r:
        trades = json.load(r).get("trades", [])
    rows = sorted((dt.datetime.fromisoformat(tr["created_time"].replace("Z", "+00:00")), price_cents(tr))
                  for tr in trades if price_cents(tr) is not None)
    window_h = max((now - rows[0][0]).total_seconds() / 3600, 1e-6) if rows else 0.0
    recent = [r for r in rows if (now - r[0]).total_seconds() <= 3 * 3600]
    moves = [(b[0], a[1], b[1]) for a, b in zip(rows, rows[1:]) if a[1] != b[1]]
    moves_3h = [m for m in moves if (now - m[0]).total_seconds() <= 3 * 3600]
    last_move = moves[-1][0] if moves else None
    row = {
        "ticker": t,
        "trades": len(rows),
        "window_h": round(window_h, 2),
        "moves": len(moves),
        "moves_per_h": round(len(moves) / window_h, 2) if window_h else None,
        "trades_3h": len(recent),
        "moves_3h": len(moves_3h),
        "distinct_px_3h": len({p for _, p in recent}),
        "px_range_3h_c": (max(p for _, p in recent) - min(p for _, p in recent)) if recent else None,
        "last_px_c": rows[-1][1] if rows else None,
        "last_move_age_min": round((now - last_move).total_seconds() / 60, 1) if last_move else None,
        "last_trade_age_min": round((now - rows[-1][0]).total_seconds() / 60, 1) if rows else None,
    }
    out.append(row)
    print(json.dumps(row))
json.dump({"observed_at_utc": now.isoformat(), "rows": out},
          open(sys.argv[0].rsplit("/", 1)[0] + "/touch-moves.json", "w"), indent=1)
