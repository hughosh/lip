#!/usr/bin/env python3
"""Deterministic replay of a captured raw websocket tape through the rig.

Two jobs, one implementation:

  1. DIFFERENTIAL TEST. The port is only trustworthy if it produces identical
     `fill` rows from identical input. This replays a tape into a fresh DB with
     no network and no clock, so the output is a pure function of the tape.

  2. THROUGHPUT BENCHMARK. Replay runs the same `_handle` path as the live rig
     with the network removed, so frames/sec here is CPython's ceiling. That is
     the number the "is Python the bottleneck" question actually turns on.

WHAT IS AND IS NOT DETERMINISTIC

`fill` rows are a pure function of the tape: `_record_trade` never reads the
clock or the Target Size. They are the comparison surface.

`reference` rows are NOT, quite: `gate` depends on each market's Target Size,
which comes from the REST API and is not in the tape. Replay defaults every
target to 0.0, which forces gate=0. Pass --universe to restore the real gate.

Forward mids (mid_1m/5m/30m) are resolved by a wall-clock asyncio task in the
live rig, so replay does not populate them at all. They are excluded from the
differential surface by construction.

TAPE FRAMING

Kalshi frames carry a trailing newline, so the capture writer's
`f'...,"m":{raw}}}\\n'` puts the envelope's closing brace on its own line. The
tape is therefore NOT line-delimited JSON — every record spans two lines and
neither parses. A streaming decoder recovers it exactly; this reader uses one,
so it handles both the old broken framing and the fixed framing unchanged.

Usage:
    python replay.py raw-20260724T225527Z.jsonl.gz --out /tmp/replay.db
    python replay.py raw-*.jsonl.gz --bench          # throughput only, no DB
"""
from __future__ import annotations

import argparse
import gzip
import json
import sqlite3
import sys
import time
from pathlib import Path

sys.path.insert(0, str(Path(__file__).parent))

import rig as rigmod


class _NoSigner:
    """Replay never connects, so it must never touch the private key."""

    def ws_headers(self):  # pragma: no cover - defensive
        raise RuntimeError("replay does not open a websocket")


def iter_frames(path: Path):
    """Yield envelope dicts from a tape, tolerant of framing and a live tail.

    Uses raw_decode rather than splitlines so that the trailing-newline framing
    bug (and a partially-written final record on a tape still being appended
    to) are both handled without loss.
    """
    dec = json.JSONDecoder()
    buf = ""
    pos = 0
    with gzip.open(path, "rt", encoding="utf-8") as fh:
        while True:
            try:
                chunk = fh.read(1 << 20)
            except (EOFError, OSError, gzip.BadGzipFile):
                chunk = ""          # truncated tail of a tape being written
            if chunk:
                buf = buf[pos:] + chunk
                pos = 0
            while True:
                while pos < len(buf) and buf[pos] in " \t\r\n":
                    pos += 1
                if pos >= len(buf):
                    break
                try:
                    obj, pos = dec.raw_decode(buf, pos)
                except ValueError:
                    break           # incomplete record, need more bytes
                yield obj
            if not chunk:
                return


def scan_universe(paths: list[Path]) -> tuple[dict[str, float], bool]:
    """Recover the subscribed universe, with real Target Sizes when available.

    Tapes written after the header fix carry {"universe": {ticker: target}} as
    their first record, which reproduces the `reference` gate exactly. Older
    tapes do not, so fall back to the snapshot frames: the live rig receives
    exactly one snapshot per subscribed market, which recovers the ticker set
    but not the targets (they default to 0.0, forcing gate=0).
    """
    tickers: dict[str, float] = {}
    exact = False
    for p in paths:
        for env in iter_frames(p):
            u = env.get("universe")
            if isinstance(u, dict):
                tickers.update({k: float(v) for k, v in u.items()})
                exact = True
                continue
            m = env.get("m") or {}
            if m.get("type") == "orderbook_snapshot":
                t = (m.get("msg") or {}).get("market_ticker")
                if t:
                    tickers.setdefault(t, 0.0)
    return tickers, exact


def replay(paths: list[Path], tickers: dict[str, float], out: Path | None):
    rigmod.Signer = _NoSigner
    conn = rigmod.connect(out) if out else sqlite3.connect(":memory:")
    if out is None:
        conn.executescript(rigmod.SCHEMA)
    rig = rigmod.Rig(conn, tickers, raw_path=None)

    n = errors = 0
    t0 = time.perf_counter()
    for p in paths:
        for env in iter_frames(p):
            m = env.get("m")
            if not isinstance(m, dict):
                continue
            n += 1
            try:
                rig._handle(m)
            except Exception as e:
                errors += 1
                if errors <= 5:
                    print(f"[handle] {e!r}", file=sys.stderr)
    elapsed = time.perf_counter() - t0
    conn.commit()
    return rig, conn, n, errors, elapsed


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__,
                                 formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("tape", nargs="+", type=Path)
    ap.add_argument("--out", type=Path, default=None,
                    help="DB to write (default: in-memory, benchmark only)")
    ap.add_argument("--universe", type=Path, default=None,
                    help="JSON {ticker: target_size} to restore the real gate")
    ap.add_argument("--bench", action="store_true",
                    help="report throughput only")
    a = ap.parse_args()

    paths = sorted(a.tape)
    if a.universe:
        tickers = {k: float(v) for k, v in json.loads(a.universe.read_text()).items()}
        print(f"universe: {len(tickers)} tickers from {a.universe}", file=sys.stderr)
    else:
        t0 = time.perf_counter()
        tickers, exact = scan_universe(paths)
        src = "tape header (targets exact)" if exact else "snapshots (targets=0, gate forced 0)"
        print(f"universe: {len(tickers)} tickers from {src} "
              f"({time.perf_counter() - t0:.1f}s scan)", file=sys.stderr)

    if a.out and a.out.exists():
        a.out.unlink()
    rig, conn, n, errors, elapsed = replay(paths, tickers, a.out)

    fills, tt = conn.execute(
        "SELECT COUNT(*), COALESCE(SUM(trade_through), 0) FROM fill").fetchone()
    refs = conn.execute("SELECT COUNT(*) FROM reference").fetchone()[0]
    s = rig.stats

    print(f"frames        {n:,}  in {elapsed:.2f}s  "
          f"-> {n / elapsed:,.0f}/s", file=sys.stderr)
    print(f"trades seen   {s['trades']:,}  (universe-filtered)", file=sys.stderr)
    print(f"fills         {fills:,}   trade_through {tt:,}", file=sys.stderr)
    print(f"reference     {refs:,}", file=sys.stderr)
    print(f"gaps {s['gaps']}  quarantined {s['quarantined']}  "
          f"stale {len(rig.stale)}  handler errors {errors}", file=sys.stderr)

    if not a.bench and a.out:
        print(f"wrote {a.out}", file=sys.stderr)
    conn.close()
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
