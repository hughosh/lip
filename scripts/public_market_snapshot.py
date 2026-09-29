#!/usr/bin/env python3
"""GET-only market/book/fee metadata for an attended operator preflight."""
import argparse
import datetime as dt
import json
from pathlib import Path
import re
import time
import urllib.request

BASE = "https://external-api.kalshi.com/trade-api/v2"
TICKER = re.compile(r"[A-Za-z0-9][A-Za-z0-9._-]*\Z")


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, *args, **kwargs):
        return None


def read(path):
    # Only the documented public object and orderbook routes are reachable.
    if not re.fullmatch(r"/(markets|events|series)/[A-Za-z0-9][A-Za-z0-9._-]*(/orderbook)?", path):
        raise ValueError("non-public route refused")
    started = time.monotonic()
    at = dt.datetime.now(dt.timezone.utc).isoformat()
    req = urllib.request.Request(BASE + path, method="GET")
    with urllib.request.build_opener(NoRedirect).open(req, timeout=15) as response:
        body = response.read(8_000_001)
        if response.status != 200 or len(body) > 8_000_000:
            raise ValueError("public response failed or exceeded size bound")
        return {"path": path, "at_utc": at, "status": response.status,
                "latency_ms": round((time.monotonic() - started) * 1000, 3),
                "body": json.loads(body)}


def snapshot(ticker):
    if not TICKER.fullmatch(ticker):
        raise ValueError("invalid ticker")
    calls = {"market": read("/markets/" + ticker),
             "orderbook": read("/markets/" + ticker + "/orderbook")}
    market = calls["market"]["body"]["market"]
    if market["ticker"] != ticker:
        raise ValueError("market identity mismatch")
    event = market["event_ticker"]
    if not TICKER.fullmatch(event):
        raise ValueError("invalid event ticker")
    calls["event"] = read("/events/" + event)
    series = calls["event"]["body"]["event"]["series_ticker"]
    if not TICKER.fullmatch(series):
        raise ValueError("invalid series ticker")
    calls["series"] = read("/series/" + series)
    if calls["series"]["body"]["series"]["ticker"] != series:
        raise ValueError("series identity mismatch")
    return {"ticker": ticker, "read_only": True, "calls": calls,
            "completed_at_utc": dt.datetime.now(dt.timezone.utc).isoformat(),
            "note": "Sequential snapshots, not executable quotes or actual fees. Re-read immediately before arming."}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--ticker", required=True)
    parser.add_argument("--out", type=Path, required=True)
    args = parser.parse_args()
    result = snapshot(args.ticker)
    with args.out.open("x") as out:
        json.dump(result, out, indent=2)
        out.write("\n")
    print("Public GET-only market/book/series receipt written.")


if __name__ == "__main__":
    main()
