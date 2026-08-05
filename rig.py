#!/usr/bin/env python3
"""Read-only measurement rig for Kalshi LIP markets.

Answers, from the public tape, what it costs to rest at the touch — using
everyone's passive fills rather than only our own. Our own $20-$100 of orders
could never supply enough independent episodes; the whole exchange does.

WHAT IT LOGS

  fill       one row per observed passive execution: the pre-trade book, the
             size resting at the fill price (queue-ahead proxy), and the
             `trade_through` flag.
  reference  every change in Reference Yes Price (the highest yes bid, which
             is what LIP scores against) and every flip of the both-sides-
             meet-Target-Size gate. This is what actually killed the manual
             probe: the market drifted 6 ticks in 90 minutes and the quote
             scored 0.5**7 of a share thereafter.

THE TRADE-THROUGH SIGNAL, AND WHAT IT IS NOT

Kalshi matches price-time. Seeing a print at your price does NOT mean your
order filled — you may have been behind the queue at that level, and public
data cannot see queue position. When an aggressor trades THROUGH a level to a
worse price, that is strong evidence the level was exhausted.

It is NOT proof, and an earlier version of this file wrongly claimed it was:

  - Kalshi's maker-side self-trade prevention CANCELS the resting maker and
    continues matching at worse prices. A print below the touch can therefore
    occur without the touch order ever filling.
  - A cancel or amend landing microseconds before the sweep produces the same
    observation, with its delta arriving afterwards.

So `trade_through` selects a population that is heavily enriched for genuine
fills but is not causally clean. Public aggregate data cannot separate "filled"
from "cancelled just in time".

Note also what the flag is evidence ABOUT. If the touch is 48 and the print is
at 47, the row describes a real fill at 47, but the trade-through fact concerns
an order resting at 48. Analysis of "would my quote at the touch have filled"
must use the touch price (`pre_best_yes` / `pre_best_no`), not the print price.

Rows without the flag still carry `depth_at_price`, so a queue model can be
applied later. Do not pool the two populations.

Nothing here places, amends, or cancels an order. The only credential use is
the websocket handshake, which Kalshi requires even for public channels.

Usage:
    python rig.py                 # stream until interrupted
    python rig.py --duration 3600 # stop after an hour
    python rig.py --report        # summarise what has been collected
"""
from __future__ import annotations

import argparse
import asyncio
import gzip
import json
import sqlite3
import sys
import time
from collections import deque
from datetime import datetime, timezone
from pathlib import Path

import websockets

sys.path.insert(0, str(Path(__file__).parent))

from auth import WS_URL, Signer
from score import programs

DB_PATH = Path(__file__).parent / "rig.db"

# Forward horizons for the markout estimator, in seconds.
HORIZONS = {"1m": 60, "5m": 300, "30m": 1800}

# How much book history to retain per market. Trades and their deltas arrive
# within milliseconds of each other; 5s is a wide margin. Never fully emptied,
# so a quiet market still has a book to attribute a trade against.
HISTORY_SECONDS = 5.0
HISTORY_MAX = 64


# ---------------------------------------------------------------- schema

SCHEMA = """
CREATE TABLE IF NOT EXISTS fill (
    trade_id       TEXT PRIMARY KEY,
    ts_ms          INTEGER NOT NULL,
    ticker         TEXT    NOT NULL,
    resting_side   TEXT    NOT NULL,   -- side that was passively filled
    price          INTEGER NOT NULL,   -- cents paid by the resting order
    size           REAL    NOT NULL,
    taker_side     TEXT,
    trade_through  INTEGER,            -- 1 = proof a resting order at `price` filled
    pre_best_yes   INTEGER,
    pre_best_no    INTEGER,
    pre_yes_size   REAL,
    pre_no_size    REAL,
    pre_mid        REAL,               -- yes-denominated mid, cents
    pre_spread     INTEGER,
    depth_at_price REAL,               -- resting size at `price` before the trade
    book_lag_ms    INTEGER,            -- age of the book state used; audit field
    mid_1m         REAL,
    mid_5m         REAL,
    mid_30m        REAL,
    source         TEXT DEFAULT 'observed'  -- 'observed' | 'ours'
);
CREATE INDEX IF NOT EXISTS idx_fill_ticker ON fill(ticker);
CREATE INDEX IF NOT EXISTS idx_fill_tt     ON fill(trade_through);

CREATE TABLE IF NOT EXISTS pending_mid (
    trade_id TEXT    NOT NULL,
    ticker   TEXT    NOT NULL,
    horizon  TEXT    NOT NULL,
    due_ms   INTEGER NOT NULL,
    PRIMARY KEY (trade_id, horizon)
);
CREATE INDEX IF NOT EXISTS idx_pending_due ON pending_mid(due_ms);

CREATE TABLE IF NOT EXISTS reference (
    ts_ms     INTEGER NOT NULL,
    ticker    TEXT    NOT NULL,
    ref_yes   INTEGER,           -- highest yes bid = LIP Reference Yes Price
    ref_no    INTEGER,
    gate      INTEGER,           -- 1 = both sides reach Target Size
    yes_depth REAL,
    no_depth  REAL,
    target    REAL,
    PRIMARY KEY (ts_ms, ticker)
);
CREATE INDEX IF NOT EXISTS idx_ref_ticker ON reference(ticker);
"""


def connect(path: Path = DB_PATH) -> sqlite3.Connection:
    conn = sqlite3.connect(path)
    conn.executescript(SCHEMA)
    conn.commit()
    return conn


# ---------------------------------------------------------------- book state


class Book:
    """Live orderbook for one market, plus a short history for attribution."""

    __slots__ = ("yes", "no", "history", "target", "ref_yes", "ref_no", "gate")

    def __init__(self, target: float = 0.0) -> None:
        self.yes: dict[int, float] = {}
        self.no: dict[int, float] = {}
        self.history: deque[tuple[float, dict[int, float], dict[int, float]]] = deque()
        self.target = target
        self.ref_yes: int | None = None
        self.ref_no: int | None = None
        self.gate: int | None = None

    # -- mutation

    def apply_snapshot(self, yes_levels, no_levels) -> None:
        self.yes = {p: s for p, s in yes_levels if s > 0}
        self.no = {p: s for p, s in no_levels if s > 0}

    def apply_delta(self, side: str, price: int, delta: float) -> None:
        d = self.yes if side == "yes" else self.no
        new = d.get(price, 0.0) + delta
        if new > 1e-9:
            d[price] = new
        else:
            d.pop(price, None)

    def checkpoint(self, ts: float) -> None:
        """Record the current state so a later trade can be attributed to it."""
        self.history.append((ts, dict(self.yes), dict(self.no)))
        cutoff = ts - HISTORY_SECONDS
        while len(self.history) > 1 and (
            self.history[0][0] < cutoff or len(self.history) > HISTORY_MAX
        ):
            self.history.popleft()

    def state_before(self, ts: float):
        """(yes, no, lag_seconds) as of just before `ts`.

        Falls back to the oldest retained state if every checkpoint is newer
        than the trade — which happens when the deltas for a trade are
        processed before the trade message itself.
        """
        chosen = None
        for h_ts, yes, no in self.history:
            # No early break: checkpoints are appended in arrival order and a
            # single out-of-order entry would otherwise hide every later one.
            if h_ts < ts and (chosen is None or h_ts >= chosen[0]):
                chosen = (h_ts, yes, no)
        if chosen is None:
            if not self.history:
                return dict(self.yes), dict(self.no), None
            h_ts, yes, no = self.history[0]
            return yes, no, (ts - h_ts)
        return chosen[1], chosen[2], (ts - chosen[0])

    # -- derived

    @staticmethod
    def _best(d: dict[int, float]) -> tuple[int | None, float]:
        if not d:
            return None, 0.0
        p = max(d)
        return p, d[p]

    def best_yes(self):
        return self._best(self.yes)

    def best_no(self):
        return self._best(self.no)

    def mid(self) -> float | None:
        """Yes-denominated mid in cents.

        A resting no bid at n is a yes offer at 100-n, so the yes ask is
        100 - best_no.
        """
        by, _ = self.best_yes()
        bn, _ = self.best_no()
        if by is None or bn is None:
            return None
        return (by + (100 - bn)) / 2.0

    def qualifies(self) -> int:
        """1 if BOTH sides reach Target Size cumulatively down the book.

        Mirrors score.qualify(): the walk is what the LIP rule gates on, and a
        side that never reaches Target Size has its qualifying set cleared.
        """
        if not self.target:
            return 0
        for d in (self.yes, self.no):
            if not d or max(d) >= 100:
                return 0
            total = 0.0
            for p in sorted(d, reverse=True):
                total += d[p]
                if total >= self.target:
                    break
            else:
                return 0
        return 1


def _mid_of(yes: dict[int, float], no: dict[int, float]) -> float | None:
    if not yes or not no:
        return None
    return (max(yes) + (100 - max(no))) / 2.0


# ---------------------------------------------------------------- the rig


class Rig:
    def __init__(self, conn: sqlite3.Connection, tickers: dict[str, float],
                 raw_path: Path | None = None) -> None:
        self.conn = conn
        self.books: dict[str, Book] = {t: Book(tg) for t, tg in tickers.items()}
        self.signer = Signer()
        self.seq: dict[int, int] = {}
        self.stats = {"trades": 0, "fills": 0, "through": 0, "gaps": 0,
                      "refs": 0, "quarantined": 0}

        # Every raw frame is kept. Without it none of the attribution decisions
        # below can be revisited, and the port cannot be validated by replaying
        # an identical tape through both implementations.
        #
        # compresslevel=6, not gzip's default 9: at the observed feed rate level
        # 9 costs 0.61% of a core and level 6 costs 0.22% for 8% more disk.
        # That made compression the single largest CPU line item in this
        # process's own code -- larger than JSON decoding and the whole
        # measurement path combined.
        self.raw = (gzip.open(raw_path, "at", encoding="utf-8", compresslevel=6)
                    if raw_path else None)
        if self.raw is not None:
            # Target Size is a REST field and is not carried on any websocket
            # frame, so a tape alone cannot reproduce the `reference` gate.
            # Recording the universe here makes replay self-contained.
            self.raw.write(json.dumps({"universe": tickers}) + "\n")

        # Single clock domain. Mixing exchange ts_ms with local time.time()
        # shifts every checkpoint by the clock offset plus delivery latency,
        # which silently mis-attributes trades to post-trade book states.
        self.ex_ts: int = 0

        # Markets whose book is not currently trustworthy: awaiting a first
        # snapshot, or downstream of a dropped delta. Trades are not recorded
        # for these until a fresh snapshot arrives.
        self.stale: set[str] = set(self.books)
        self._needs_resnapshot = False

    def _watermark(self, ts_ms: int | None) -> int:
        """Monotone exchange-time watermark; never goes backwards."""
        if ts_ms:
            self.ex_ts = max(self.ex_ts, int(ts_ms))
        return self.ex_ts

    # -- persistence

    def _record_reference(self, ticker: str, book: Book, ts_ms: int) -> None:
        """Write a row only when the reference price or the gate actually moves."""
        ry, _ = book.best_yes()
        rn, _ = book.best_no()
        gate = book.qualifies()
        if (ry, rn, gate) == (book.ref_yes, book.ref_no, book.gate):
            return
        book.ref_yes, book.ref_no, book.gate = ry, rn, gate
        self.conn.execute(
            "INSERT OR REPLACE INTO reference VALUES (?,?,?,?,?,?,?,?)",
            (ts_ms, ticker, ry, rn, gate,
             sum(book.yes.values()), sum(book.no.values()), book.target),
        )
        self.stats["refs"] += 1

    def _record_trade(self, m: dict) -> None:
        ticker = m["market_ticker"]
        book = self.books.get(ticker)
        if book is None:
            return

        ts_ms = int(m["ts_ms"])
        size = float(m["count_fp"])
        yes_price = round(float(m["yes_price_dollars"]) * 100)
        no_price = round(float(m["no_price_dollars"]) * 100)
        taker = m.get("taker_side") or ""

        # Taker bought yes  -> lifted a resting NO bid  -> resting side is 'no'.
        # Taker bought no   -> hit a resting YES bid    -> resting side is 'yes'.
        resting_side = "no" if taker == "yes" else "yes"
        price = no_price if resting_side == "no" else yes_price

        pre_yes, pre_no, lag = book.state_before(ts_ms / 1000.0)
        best_yes = max(pre_yes) if pre_yes else None
        best_no = max(pre_no) if pre_no else None
        resting_pre = pre_no if resting_side == "no" else pre_yes
        best_resting = best_no if resting_side == "no" else best_yes

        # The only queue-assumption-free fill proof: the aggressor reached a
        # worse price than the touch, so the touch level was exhausted.
        trade_through = int(best_resting is not None and price < best_resting)

        self.conn.execute(
            "INSERT OR IGNORE INTO fill (trade_id, ts_ms, ticker, resting_side,"
            " price, size, taker_side, trade_through, pre_best_yes, pre_best_no,"
            " pre_yes_size, pre_no_size, pre_mid, pre_spread, depth_at_price,"
            " book_lag_ms, source)"
            " VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,'observed')",
            (
                m["trade_id"], ts_ms, ticker, resting_side, price, size, taker,
                trade_through, best_yes, best_no,
                pre_yes.get(best_yes, 0.0) if best_yes is not None else 0.0,
                pre_no.get(best_no, 0.0) if best_no is not None else 0.0,
                _mid_of(pre_yes, pre_no),
                (100 - best_yes - best_no)
                if (best_yes is not None and best_no is not None) else None,
                resting_pre.get(price, 0.0),
                int(lag * 1000) if lag is not None else None,
            ),
        )
        self.conn.executemany(
            "INSERT OR IGNORE INTO pending_mid VALUES (?,?,?,?)",
            [(m["trade_id"], ticker, name, ts_ms + secs * 1000)
             for name, secs in HORIZONS.items()],
        )
        self.stats["trades"] += 1
        self.stats["fills"] += 1
        self.stats["through"] += trade_through

    # -- deferred forward mids

    async def resolve_mids(self) -> None:
        """Fill in mid_1m / mid_5m / mid_30m as each horizon comes due.

        Reads the live in-memory book, so no extra API load. Pending rows live
        in the DB, so a restart resumes rather than losing the horizon.
        """
        while True:
            await asyncio.sleep(5)
            now_ms = int(time.time() * 1000)
            due = self.conn.execute(
                "SELECT trade_id, ticker, horizon FROM pending_mid"
                " WHERE due_ms <= ? LIMIT 2000",
                (now_ms,),
            ).fetchall()
            if not due:
                continue
            for trade_id, ticker, horizon in due:
                book = self.books.get(ticker)
                mid = book.mid() if book else None
                if mid is not None:
                    self.conn.execute(
                        f"UPDATE fill SET mid_{horizon} = ? WHERE trade_id = ?",
                        (mid, trade_id),
                    )
                self.conn.execute(
                    "DELETE FROM pending_mid WHERE trade_id = ? AND horizon = ?",
                    (trade_id, horizon),
                )
            self.conn.commit()

    # -- websocket

    async def _subscribe(self, ws) -> None:
        tickers = list(self.books)
        await ws.send(json.dumps({
            "id": 1, "cmd": "subscribe",
            "params": {"channels": ["orderbook_delta"], "market_tickers": tickers},
        }))
        # `trade` takes no market filter: one subscription yields the whole
        # exchange tape, which we filter down to our universe.
        await ws.send(json.dumps({
            "id": 2, "cmd": "subscribe", "params": {"channels": ["trade"]},
        }))

    def _handle(self, data: dict) -> None:
        kind = data.get("type")
        msg = data.get("msg") or {}

        # Sequence is subscription-wide, not per market: a gap means SOME
        # market lost a delta and we cannot tell which. Every book on that
        # subscription is therefore suspect and must be resnapshotted.
        # Checked before mutating, so a known-bad delta is never applied.
        sid, seq = data.get("sid"), data.get("seq")
        if sid is not None and seq is not None:
            prev = self.seq.get(sid)
            self.seq[sid] = seq
            if prev is not None and seq != prev + 1:
                self.stats["gaps"] += 1
                self.stale |= set(self.books)
                self._needs_resnapshot = True
                print(f"[seq gap on sid {sid}: {prev} -> {seq}] quarantining "
                      f"{len(self.books)} books", file=sys.stderr, flush=True)
                return

        if kind == "orderbook_snapshot":
            ticker = msg.get("market_ticker")
            book = self.books.get(ticker)
            if book is None:
                return
            def levels(key):
                return [(round(float(p) * 100), float(s))
                        for p, s in (msg.get(key) or [])]
            book.apply_snapshot(levels("yes_dollars_fp"), levels("no_dollars_fp"))
            # Snapshots carry no exchange timestamp. Stamping them with local
            # time would inject a second clock, so they adopt the current
            # exchange watermark instead.
            ts_ms = self._watermark(msg.get("ts_ms"))
            book.history.clear()
            book.checkpoint(ts_ms / 1000.0)
            self.stale.discard(ticker)
            self._record_reference(ticker, book, ts_ms)

        elif kind == "orderbook_delta":
            ticker = msg.get("market_ticker")
            book = self.books.get(ticker)
            if book is None or ticker in self.stale:
                return
            ts_ms = self._watermark(msg.get("ts_ms"))
            book.apply_delta(
                msg["side"],
                round(float(msg["price_dollars"]) * 100),
                float(msg["delta_fp"]),
            )
            book.checkpoint(ts_ms / 1000.0)
            self._record_reference(ticker, book, ts_ms)

        elif kind == "trade":
            self._watermark(msg.get("ts_ms"))
            if msg.get("market_ticker") in self.stale:
                self.stats["quarantined"] += 1
                return
            self._record_trade(msg)

        elif kind == "error":
            print(f"[ws error] {data}", file=sys.stderr, flush=True)

    async def run(self, duration: float | None = None) -> None:
        deadline = (time.time() + duration) if duration else None
        resolver = asyncio.create_task(self.resolve_mids())
        last_commit = last_report = time.time()
        backoff = 1.0
        try:
            while True:
                if deadline and time.time() >= deadline:
                    break
                try:
                    async with websockets.connect(
                        WS_URL,
                        additional_headers=self.signer.ws_headers(),
                        ping_interval=None,      # server drives the heartbeat
                        max_queue=4096,
                    ) as ws:
                        await self._subscribe(ws)
                        print(f"connected: {len(self.books)} markets, "
                              f"whole-exchange trade tape", flush=True)
                        backoff = 1.0
                        async for raw in ws:
                            if self.raw is not None:
                                # Kalshi frames carry a TRAILING NEWLINE. Without
                                # the strip the envelope's closing brace lands on
                                # its own line, so every record spans two lines
                                # and neither is valid JSON. Interpolate the frame
                                # verbatim rather than re-encoding it: the tape is
                                # only a faithful replay input if it preserves the
                                # exact bytes the exchange sent.
                                if isinstance(raw, bytes):
                                    raw = raw.decode("utf-8")
                                self.raw.write(
                                    f'{{"recv_ms":{int(time.time()*1000)},'
                                    f'"m":{raw.strip()}}}\n'
                                )
                            try:
                                self._handle(json.loads(raw))
                            except Exception as e:
                                print(f"[handle] {e!r}", file=sys.stderr, flush=True)

                            if self._needs_resnapshot:
                                self._needs_resnapshot = False
                                await ws.send(json.dumps({
                                    "id": 3, "cmd": "update_subscription",
                                    "params": {"sids": list(self.seq),
                                               "action": "get_snapshot",
                                               "market_tickers": list(self.books)},
                                }))

                            now = time.time()
                            if now - last_commit > 2.0:
                                self.conn.commit()
                                last_commit = now
                            if now - last_report > 60.0:
                                s = self.stats
                                print(f"[{int(now)}] trades={s['trades']} "
                                      f"through={s['through']} refs={s['refs']} "
                                      f"gaps={s['gaps']} "
                                      f"quarantined={s['quarantined']} "
                                      f"stale={len(self.stale)}", flush=True)
                                last_report = now
                            if deadline and now >= deadline:
                                break
                except (websockets.ConnectionClosed, OSError) as e:
                    self.conn.commit()
                    print(f"[reconnect in {backoff:.0f}s] {e!r}",
                          file=sys.stderr, flush=True)
                    await asyncio.sleep(backoff)
                    backoff = min(backoff * 2, 60.0)
                    # Discard the books outright, not just their history: any
                    # level carried across the gap may already be wrong, and
                    # trades arriving before the replacement snapshot would be
                    # attributed to it. Quarantine until each market resnaps.
                    for b in self.books.values():
                        b.yes.clear()
                        b.no.clear()
                        b.history.clear()
                        b.ref_yes = b.ref_no = b.gate = None
                    self.stale = set(self.books)
                    self.seq.clear()
        finally:
            resolver.cancel()
            self.conn.commit()
            if self.raw is not None:
                self.raw.close()


# ---------------------------------------------------------------- report


def report(conn: sqlite3.Connection) -> None:
    cur = conn.cursor()
    n, first, last = cur.execute(
        "SELECT COUNT(*), MIN(ts_ms), MAX(ts_ms) FROM fill"
    ).fetchone()
    if not n:
        print("No fills recorded yet.")
        return
    span = (last - first) / 3600000.0
    tt, mkts = cur.execute(
        "SELECT SUM(trade_through), COUNT(DISTINCT ticker) FROM fill"
    ).fetchone()
    print(f"{n:,} passive executions across {mkts} markets over {span:.1f}h")
    print(f"  trade-through confirmed (queue-assumption-free): {tt:,} "
          f"({tt/n:.1%})")

    print("\nPassive markout, cents per contract (negative = the resting side lost):")
    print(f"  {'horizon':<10}{'n':>8}{'all':>10}{'confirmed':>12}")
    for h in ("1m", "5m", "30m"):
        # Resting yes at p is long yes: markout = mid_h - p.
        # Resting no at n is short yes at (100-n): markout = (100 - mid_h) - n.
        expr = (f"CASE WHEN resting_side='yes' THEN mid_{h} - price "
                f"ELSE (100 - mid_{h}) - price END")
        row = cur.execute(
            f"SELECT COUNT(*), SUM({expr}*size)/SUM(size) FROM fill"
            f" WHERE mid_{h} IS NOT NULL"
        ).fetchone()
        conf = cur.execute(
            f"SELECT SUM({expr}*size)/SUM(size) FROM fill"
            f" WHERE mid_{h} IS NOT NULL AND trade_through=1"
        ).fetchone()[0]
        c, a = row
        print(f"  {h:<10}{c:>8,}{(a if a is not None else 0):>10.3f}"
              f"{(conf if conf is not None else 0):>12.3f}")

    print("\nReference-price drift (what strands a resting quote):")
    rows = cur.execute(
        "SELECT ticker, COUNT(*), MIN(ref_yes), MAX(ref_yes)"
        " FROM reference WHERE ref_yes IS NOT NULL GROUP BY ticker"
        " ORDER BY COUNT(*) DESC LIMIT 10"
    ).fetchall()
    print(f"  {'market':<40}{'moves':>7}{'min':>6}{'max':>6}{'range':>7}")
    for tkr, c, lo, hi in rows:
        print(f"  {tkr[:40]:<40}{c:>7}{lo:>6}{hi:>6}{hi-lo:>7}")

    g = cur.execute(
        "SELECT AVG(gate) FROM reference WHERE gate IS NOT NULL"
    ).fetchone()[0]
    if g is not None:
        print(f"\n  share of observed book states meeting the Target Size gate: {g:.1%}")


# ---------------------------------------------------------------- main


def universe() -> dict[str, float]:
    """Active LIP markets -> Target Size."""
    return {p["market_ticker"]: float(p["target_size_fp"])
            for p in programs("active")}


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("--duration", type=float, default=None,
                    help="seconds to run (default: until interrupted)")
    ap.add_argument("--report", action="store_true")
    ap.add_argument("--db", type=Path, default=DB_PATH)
    ap.add_argument("--no-raw", action="store_true",
                    help="skip raw frame capture (loses replay/reanalysis)")
    a = ap.parse_args()

    conn = connect(a.db)
    if a.report:
        report(conn)
        return 0

    raw_path = None
    if not a.no_raw:
        stamp = datetime.now(timezone.utc).strftime("%Y%m%dT%H%M%SZ")
        raw_path = a.db.parent / f"raw-{stamp}.jsonl.gz"

    tickers = universe()
    print(f"universe: {len(tickers)} active LIP markets -> {a.db}", flush=True)
    if raw_path:
        print(f"raw capture -> {raw_path}", flush=True)
    rig = Rig(conn, tickers, raw_path)
    try:
        asyncio.run(rig.run(a.duration))
    except KeyboardInterrupt:
        print("\nstopped.")
    finally:
        conn.commit()
        conn.close()
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
