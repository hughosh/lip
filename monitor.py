#!/usr/bin/env python3
"""Kalshi Liquidity Incentive Program monitor — read-only, no auth required.

Polls /incentive_programs for active liquidity programs, pulls each market's
orderbook, and appends a snapshot row to SQLite. The point is to answer one
question: are the near-empty books at the touch a persistent structural gap,
or an artifact of when we happened to look?

Nothing here places, amends, or cancels an order. There is no auth and no
write path to Kalshi at all.

Usage:
    python monitor.py                 # poll every 5 min forever
    python monitor.py --once          # single sweep, then exit
    python monitor.py --interval 120  # custom seconds between sweeps
    python monitor.py --report        # summarise what's been collected
"""
from __future__ import annotations

import argparse
import json
import sqlite3
import sys
import time
import urllib.error
import urllib.parse
import urllib.request
from datetime import datetime, timezone
from pathlib import Path

BASE = "https://api.elections.kalshi.com/trade-api/v2"
DB_PATH = Path(__file__).parent / "lip.db"

# Basic tier is 200 read tokens/sec at 10 tokens/request = ~20 req/s.
# We stay far under that; there is no deadline on a 5-minute sweep.
REQ_DELAY = 0.08


# ---------------------------------------------------------------- http

def _get(path: str, **params) -> dict:
    url = BASE + path + ("?" + urllib.parse.urlencode(params) if params else "")
    req = urllib.request.Request(
        url, headers={"accept": "application/json", "User-Agent": "lip-monitor"}
    )
    for attempt in range(4):
        try:
            with urllib.request.urlopen(req, timeout=30) as resp:
                return json.load(resp)
        except urllib.error.HTTPError as e:
            if e.code == 429:
                time.sleep(2 ** attempt)
                continue
            raise
        except (urllib.error.URLError, TimeoutError):
            if attempt == 3:
                raise
            time.sleep(2 ** attempt)
    return {}


def active_programs() -> list[dict]:
    """All active liquidity incentive programs, paginated."""
    out: list[dict] = []
    cursor = None
    while True:
        params = {"status": "active", "limit": 200}
        if cursor:
            params["cursor"] = cursor
        data = _get("/incentive_programs", **params)
        batch = data.get("incentive_programs", [])
        out.extend(batch)
        cursor = data.get("cursor")
        if not cursor or not batch or len(out) > 5000:
            break
        time.sleep(REQ_DELAY)
    return [p for p in out if p.get("incentive_type") == "liquidity"]


def orderbook(ticker: str) -> tuple[float, float, float, float]:
    """Return (yes_bid, yes_bid_size, no_bid, no_bid_size) at the touch.

    Kalshi returns the book under `orderbook_fp` (not `orderbook`), with prices
    as dollar strings. A missing side comes back as an empty list or null.
    """
    yes, no = full_book(ticker)
    yb, ybs = (yes[0][0] / 100.0, yes[0][1]) if yes else (0.0, 0.0)
    nb, nbs = (no[0][0] / 100.0, no[0][1]) if no else (0.0, 0.0)
    return (yb, ybs, nb, nbs)


def full_book(ticker: str) -> tuple[list, list]:
    """Whole book per side as [(price_cents, size)], best bid first.

    NOT depth-limited: the qualifying walk in Kalshi's rule accumulates size
    down the book until Target Size is met, so a truncated book silently
    mis-answers the gate question.
    """
    try:
        book = _get(f"/markets/{ticker}/orderbook").get("orderbook_fp") or {}
    except Exception:
        return ([], [])

    def side(key):
        return sorted(((round(float(p) * 100), float(q))
                       for p, q in (book.get(key) or [])), key=lambda x: -x[0])

    return side("yes_dollars"), side("no_dollars")


def qualify(levels: list, target: float):
    """Walk down from the best bid accumulating size until `target` is met.

    Returns (reference_price_cents, qualifying_levels). If the book never
    reaches Target Size the rule CLEARS the qualifying set, so the side scores
    nothing and the snapshot is excluded entirely -> (None, []).
    """
    if not levels or levels[0][0] >= 100:
        return None, []
    ref, total, qual = levels[0][0], 0.0, []
    for price, size in levels:
        total += size
        qual.append((price, size))
        if total >= target:
            return ref, qual
    return None, []


def marginal_share(yes: list, no: list, target: float, discount_bps: int,
                   size: int = 100) -> tuple[int, float]:
    """(qualifies_now, share_of_pool) if we joined the touch with `size` a side.

    This is the quantity the reward actually keys on. Size at the touch alone
    is not it: a market can look empty at the touch and still meet Target Size
    deeper in the book, and a market with a fat touch can fail the gate.
    """
    df = discount_bps / 10000.0
    if not yes or not no:
        return (0, 0.0)
    now = qualify(yes, target)[0] is not None and qualify(no, target)[0] is not None

    total = 0.0
    for levels in (yes, no):
        joined = [(levels[0][0], levels[0][1] + size)] + list(levels[1:])
        ref, qual = qualify(joined, target)
        if ref is None:
            return (int(now), 0.0)
        denom = sum(df ** (ref - p) * s for p, s in qual)
        if not denom:
            return (int(now), 0.0)
        total += size / denom          # our bid sits at the reference, so N=0
    return (int(now), total / 2.0)


# ---------------------------------------------------------------- storage

SCHEMA = """
CREATE TABLE IF NOT EXISTS snapshot (
    ts            TEXT    NOT NULL,
    ticker        TEXT    NOT NULL,
    program_id    TEXT    NOT NULL,
    pool_usd      REAL    NOT NULL,
    target_size   REAL    NOT NULL,
    discount_bps  INTEGER NOT NULL,
    end_date      TEXT,
    yes_bid       REAL,
    yes_bid_size  REAL,
    no_bid        REAL,
    no_bid_size   REAL,
    qualifies     INTEGER,
    share100      REAL,
    PRIMARY KEY (ts, ticker)
);
CREATE INDEX IF NOT EXISTS idx_snapshot_ticker ON snapshot(ticker);
"""


def connect() -> sqlite3.Connection:
    conn = sqlite3.connect(DB_PATH)
    conn.executescript(SCHEMA)
    # rows written before the scoring rule was implemented lack these
    have = {r[1] for r in conn.execute("PRAGMA table_info(snapshot)")}
    for col, typ in (("qualifies", "INTEGER"), ("share100", "REAL")):
        if col not in have:
            conn.execute(f"ALTER TABLE snapshot ADD COLUMN {col} {typ}")
    conn.commit()
    return conn


def sweep(conn: sqlite3.Connection) -> int:
    ts = datetime.now(timezone.utc).isoformat(timespec="seconds")
    progs = active_programs()
    rows = []
    for p in progs:
        ticker = p["market_ticker"]
        target = float(p["target_size_fp"])
        yes, no = full_book(ticker)
        yb, ybs = (yes[0][0] / 100.0, yes[0][1]) if yes else (0.0, 0.0)
        nb, nbs = (no[0][0] / 100.0, no[0][1]) if no else (0.0, 0.0)
        qual, share = marginal_share(yes, no, target, p["discount_factor_bps"])
        rows.append((
            ts,
            ticker,
            p["id"],
            p["period_reward"] / 10000.0,   # centi-cents -> dollars
            target,
            p["discount_factor_bps"],
            p.get("end_date"),
            yb, ybs, nb, nbs,
            qual, share,
        ))
        time.sleep(REQ_DELAY)
    conn.executemany(
        "INSERT OR REPLACE INTO snapshot VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)", rows
    )
    conn.commit()

    pool = sum(r[3] for r in rows)
    # A snapshot only scores if BOTH sides reach Target Size cumulatively down
    # the book. Size at the touch is not the gate -- see marginal_share().
    qual = sum(1 for r in rows if r[11])
    dead_pool = sum(r[3] for r in rows if not r[11])
    take = sum(r[3] * r[12] for r in rows)
    print(
        f"[{ts}] {len(rows):>4} programs  ${pool:>8,.0f} pool  "
        f"| qualifying: {qual:>3}  | unquotable pool: ${dead_pool:>6,.0f}  "
        f"| 100ct/side take: ${take:>7,.0f}",
        flush=True,
    )
    return len(rows)


# ---------------------------------------------------------------- report

def report(conn: sqlite3.Connection) -> None:
    cur = conn.cursor()
    n, first, last = cur.execute(
        "SELECT COUNT(DISTINCT ts), MIN(ts), MAX(ts) FROM snapshot"
    ).fetchone()
    if not n:
        print("No snapshots yet — run without --report first.")
        return
    print(f"{n} sweeps from {first} to {last}\n")

    q, share = cur.execute(
        "SELECT AVG(qualifies), AVG(share100) FROM snapshot WHERE qualifies IS NOT NULL"
    ).fetchone()
    if q is not None:
        print(f"share of snapshots where the market qualifies at all : {q:.0%}")
        print(f"mean share of pool from joining the touch with 100/side: {share:.1%}\n")

    # Ranked by return on capital actually deployed, not by pool size. The
    # payout is share-of-score, so capturing the whole pool is an upper bound
    # that no participant realises.
    print("Best risk-adjusted markets (100 contracts a side, share-of-score):")
    print(f"{'market':<40}{'pool$':>7}{'seen':>6}{'qual%':>7}{'share':>8}{'cap$':>7}{'earn$':>7}{'ret%':>8}")
    rows = cur.execute(
        """
        SELECT ticker,
               AVG(pool_usd), COUNT(*), AVG(qualifies), AVG(share100),
               AVG(yes_bid + no_bid)
        FROM snapshot
        WHERE qualifies IS NOT NULL AND yes_bid > 0 AND no_bid > 0
        GROUP BY ticker
        """
    ).fetchall()
    ranked = []
    for tkr, pool, seen, qual, share, pair in rows:
        cap = 100 * (pair or 0)
        if cap > 0 and share:
            earn = pool * share
            ranked.append((tkr, pool, seen, qual, share, cap, earn, earn / cap * 100))
    ranked.sort(key=lambda r: -r[7])
    for tkr, pool, seen, qual, share, cap, earn, ret in ranked[:25]:
        print(f"{tkr[:40]:<40}{pool:>7.0f}{seen:>6}{qual:>7.0%}"
              f"{share:>8.1%}{cap:>7.0f}{earn:>7.2f}{ret:>8.1f}")
    dead = [r for r in rows if not r[3]]
    if dead:
        print(f"\n{len(dead)} markets never qualified (unquotable): "
              f"${sum(r[1] for r in dead):,.0f} of pool is unreachable")

    print("\nCompetition trend (is anyone showing up?):")
    for ts, mkts, avg_min in cur.execute(
        """
        SELECT ts, COUNT(*), AVG(MIN(yes_bid_size, no_bid_size))
        FROM snapshot GROUP BY ts ORDER BY ts
        """
    ).fetchall():
        print(f"  {ts}  markets={mkts:<5} avg size at touch (thinner side)={avg_min:,.0f}")


# ---------------------------------------------------------------- main

def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("--once", action="store_true", help="single sweep then exit")
    ap.add_argument("--interval", type=int, default=300, help="seconds between sweeps")
    ap.add_argument("--report", action="store_true", help="summarise collected data")
    args = ap.parse_args()

    conn = connect()
    if args.report:
        report(conn)
        return 0

    print(f"Writing to {DB_PATH}. Read-only against Kalshi. Ctrl-C to stop.", flush=True)
    try:
        while True:
            try:
                sweep(conn)
            except Exception as e:  # keep the loop alive across transient failures
                print(f"[sweep failed] {e!r}", file=sys.stderr, flush=True)
            if args.once:
                break
            time.sleep(args.interval)
    except KeyboardInterrupt:
        print("\nstopped.")
    finally:
        conn.close()
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
