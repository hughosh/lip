#!/usr/bin/env python
"""Fetch settlement outcomes for every ticker in rig.db.

The prior analysis' stated weakness on claim 1 was that all fair-value marks are
quote-derived mids from the same bot population under test -- circular -- and
that "no DB here has settlement outcomes". They are fetchable. This writes them
to settle.db (a NEW file; rig.db stays read-only, two collectors are writing it).
"""
import json
import sqlite3
import sys
import time
import urllib.error
import urllib.request

sys.path.insert(0, "/Users/hugh/kek/lip")
from auth import Signer  # noqa: E402

BASE = "https://api.elections.kalshi.com"
OUT = "/Users/hugh/kek/lip/settle.db"


def main():
    rig = sqlite3.connect("file:/Users/hugh/kek/lip/rig.db?mode=ro", uri=True)
    tickers = [r[0] for r in rig.execute("SELECT DISTINCT ticker FROM fill ORDER BY ticker")]
    print(f"{len(tickers)} distinct tickers in rig.db fill")

    db = sqlite3.connect(OUT)
    db.execute("""CREATE TABLE IF NOT EXISTS settlement (
        ticker TEXT PRIMARY KEY,
        status TEXT, result TEXT,
        close_time TEXT, open_time TEXT, expiration_time TEXT,
        last_price REAL, previous_price REAL,
        yes_bid REAL, yes_ask REAL,
        fetched_ms INTEGER, raw TEXT)""")
    db.commit()

    have = {r[0] for r in db.execute("SELECT ticker FROM settlement")}
    todo = [t for t in tickers if t not in have]
    print(f"{len(have)} already fetched, {len(todo)} to go")

    s = Signer()
    ok = err = 0
    for i, t in enumerate(todo, 1):
        path = f"/trade-api/v2/markets/{t}"
        try:
            req = urllib.request.Request(BASE + path, headers=s.headers("GET", path))
            m = json.load(urllib.request.urlopen(req, timeout=20))["market"]
        except urllib.error.HTTPError as e:
            print(f"  [{i}/{len(todo)}] {t}: HTTP {e.code}")
            err += 1
            time.sleep(0.5)
            continue
        except Exception as e:
            print(f"  [{i}/{len(todo)}] {t}: {type(e).__name__} {e}")
            err += 1
            time.sleep(1.0)
            continue

        def f(k):
            v = m.get(k)
            return float(v) if v is not None else None

        db.execute(
            "INSERT OR REPLACE INTO settlement VALUES (?,?,?,?,?,?,?,?,?,?,?,?)",
            (t, m.get("status"), m.get("result"), m.get("close_time"), m.get("open_time"),
             m.get("expiration_time"), f("last_price_dollars"), f("previous_price_dollars"),
             f("yes_bid_dollars"), f("yes_ask_dollars"), int(time.time() * 1000),
             json.dumps(m)))
        ok += 1
        if i % 25 == 0:
            db.commit()
            print(f"  [{i}/{len(todo)}] ok={ok} err={err}  last={t[:44]} "
                  f"status={m.get('status')} result={m.get('result')}")
        time.sleep(0.12)
    db.commit()

    print(f"\nfetched ok={ok} err={err}")
    print("\nstatus x result:")
    for row in db.execute(
        "SELECT status, result, count(*) FROM settlement GROUP BY 1,2 ORDER BY 3 DESC"
    ):
        print(f"   {str(row[0]):<14}{str(row[1]):<10}{row[2]:>5}")


if __name__ == "__main__":
    main()
