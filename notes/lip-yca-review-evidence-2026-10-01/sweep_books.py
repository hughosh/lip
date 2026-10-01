#!/usr/bin/env python3
"""Public, unauthenticated, GET-only sweep of orderbooks and market status for every
market in a saved /incentive_programs listing.

Usage:
  sweep_books.py --programs programs-active.json --out-dir DIR [--rate 8] [--depth 100]

Writes DIR/books.jsonl (one line per market: ticker, ts, http, yes, no) and
DIR/markets.jsonl (batched /markets?tickers= reads: status, price ladder, close times).
No credentials are loaded. Nothing is placed, amended or cancelled.
"""
import argparse, json, os, sys, time, urllib.request, urllib.error, urllib.parse
from datetime import datetime, timezone

HOST = "https://api.elections.kalshi.com/trade-api/v2"
UA = "lip-yca-review/1.0 (read-only public sweep)"


def now():
    return datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%S.%fZ")


def get(url, retries=5):
    for attempt in range(retries):
        req = urllib.request.Request(url, headers={"User-Agent": UA, "Accept": "application/json"})
        try:
            with urllib.request.urlopen(req, timeout=30) as r:
                return r.status, json.loads(r.read().decode())
        except urllib.error.HTTPError as e:
            if e.code == 429 or e.code >= 500:
                time.sleep(3 * (attempt + 1))
                continue
            return e.code, None
        except Exception:
            time.sleep(2 * (attempt + 1))
    return 0, None


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--programs", required=True)
    ap.add_argument("--out-dir", required=True)
    ap.add_argument("--rate", type=float, default=8.0, help="requests per second target")
    ap.add_argument("--depth", type=int, default=100)
    ap.add_argument("--limit", type=int, default=0, help="debug: stop after N markets")
    ap.add_argument("--shard", default="0/1", help="i/n: take every n-th ticker starting at i")
    ap.add_argument("--skip-markets", action="store_true", help="books only (status pass run once)")
    a = ap.parse_args()
    os.makedirs(a.out_dir, exist_ok=True)
    progs = json.load(open(a.programs))["incentive_programs"]
    tickers = []
    seen = set()
    for p in progs:
        t = p["market_ticker"]
        if t not in seen:
            seen.add(t)
            tickers.append(t)
    if a.limit:
        tickers = tickers[: a.limit]
    si, sn = (int(x) for x in a.shard.split("/"))
    all_tickers = tickers
    tickers = [t for i, t in enumerate(tickers) if i % sn == si]
    gap = 1.0 / a.rate
    books_path = os.path.join(a.out_dir, "books.jsonl" if sn == 1 else f"books-{si}.jsonl")
    done = set()
    if os.path.exists(books_path):
        for line in open(books_path):
            try:
                done.add(json.loads(line)["ticker"])
            except Exception:
                pass
    print(f"{now()} start books: {len(tickers)} tickers, {len(done)} already done", flush=True)
    codes = {}
    with open(books_path, "a") as f:
        for i, t in enumerate(tickers):
            if t in done:
                continue
            t0 = time.time()
            code, body = get(f"{HOST}/markets/{urllib.parse.quote(t)}/orderbook?depth={a.depth}")
            codes[code] = codes.get(code, 0) + 1
            ob = (body or {}).get("orderbook_fp") or {}
            rec = {
                "ticker": t,
                "ts": now(),
                "http": code,
                "yes": ob.get("yes_dollars") or [],
                "no": ob.get("no_dollars") or [],
            }
            f.write(json.dumps(rec, separators=(",", ":")) + "\n")
            if (i + 1) % 500 == 0:
                f.flush()
                print(f"{now()} books {i+1}/{len(tickers)} codes={codes}", flush=True)
            dt = time.time() - t0
            if dt < gap:
                time.sleep(gap - dt)
    print(f"{now()} books done codes={codes}", flush=True)
    if a.skip_markets:
        return
    tickers = all_tickers

    # Market status in batches of 100 via /markets?tickers=
    markets_path = os.path.join(a.out_dir, "markets.jsonl")
    print(f"{now()} start markets status", flush=True)
    mcodes = {}
    keep = (
        "ticker", "status", "price_level_structure", "price_ranges", "close_time",
        "expected_expiration_time", "expiration_time", "can_close_early", "yes_bid_dollars",
        "yes_ask_dollars", "no_bid_dollars", "no_ask_dollars", "volume_24h_fp",
        "open_interest_fp", "liquidity_dollars", "market_type", "event_ticker", "result",
    )
    with open(markets_path, "w") as f:
        for i in range(0, len(tickers), 100):
            batch = tickers[i : i + 100]
            t0 = time.time()
            code, body = get(f"{HOST}/markets?limit=100&tickers=" + ",".join(batch))
            mcodes[code] = mcodes.get(code, 0) + 1
            for m in (body or {}).get("markets") or []:
                f.write(json.dumps({k: m.get(k) for k in keep if k in m} | {"ts": now()}, separators=(",", ":")) + "\n")
            dt = time.time() - t0
            if dt < gap:
                time.sleep(gap - dt)
    print(f"{now()} markets done codes={mcodes}", flush=True)


if __name__ == "__main__":
    main()
