#!/usr/bin/env python3
"""Exercise the Q3/Q4/Q5 quote state machine against a stub exchange.

The kill switches protect against disaster; this protects against the quiet
failure that actually ended the $20 probe -- losing two-sided presence and
earning nothing while looking healthy. Every case drives the real
Probe.reconcile.

    python scripts/quotetest.py
"""
from __future__ import annotations

import asyncio
import sqlite3
import sys
import time

sys.path.insert(0, "/Users/hugh/kek/lip")

import probebot
from probebot import Config, OurOrder, Probe, now_ms
from scripts.killtest import PROGRAM, StubClient

RESULTS: list[tuple[str, bool, str]] = []


def record(name, ok, note):
    RESULTS.append((name, ok, note))
    print(f"  {'PASS' if ok else 'FAIL'}  {name:26s} {note}")


def make_probe(yes_levels, no_levels, **cfg_kw):
    conn = sqlite3.connect(":memory:", isolation_level=None)
    conn.executescript(probebot.SCHEMA)
    client = StubClient()
    cfg = Config(ticker="TEST-TICKER", size=50, dry_run=False, **cfg_kw)
    p = Probe(cfg, dict(PROGRAM), conn, client)
    p.entry_ms = now_ms()
    p.connected = True
    p.stale = False
    p.last_ref_ms = now_ms()
    p.last_disconnect_ms = 0
    p.book.apply_snapshot(yes_levels, no_levels)
    return p, client


def rest(p, side, price, count=50.0):
    p.orders[f"o-{side}-{price}"] = OurOrder(
        f"o-{side}-{price}", side, price, count, "c", now_ms())


async def main() -> int:
    print("Q3/Q4/Q5 quote state machine\n")
    YES = [(57, 2370.0), (56, 800.0)]
    NO = [(42, 2384.0), (41, 700.0)]
    # yes touch at 58 requires the no side to have stepped down to 41:
    # 58 + 42 = 100 would be a locked book and cannot rest.
    NO_STEPPED = [(41, 2384.0), (40, 700.0)]

    # ---- Q3.1 join the touch on both sides, N = 0 ----------------------
    p, c = make_probe(YES, NO)
    await p.reconcile()
    ok = sorted(c.created) == sorted([("yes", 57, 50), ("no", 42, 50)])
    record("Q3.1 join touch", ok, f"placed {c.created}")

    # ---- Q3.2 never improve the touch ----------------------------------
    prices_ok = all(
        (side == "yes" and px == 57) or (side == "no" and px == 42)
        for side, px, _ in c.created)
    record("Q3.2 never improve", prices_ok,
           "quoted AT the touch, never inside it")

    # ---- Q4.1 adverse move requotes (after debounce) -------------------
    p, c = make_probe([(58, 900.0), (57, 2370.0)], NO_STEPPED)
    rest(p, "yes", 57)
    rest(p, "no", 41)
    p.last_requote = {"yes": 0.0, "no": 0.0}
    await p.reconcile()                       # first pass: starts the debounce
    before = list(c.created)
    await asyncio.sleep(0.30)                 # let the 250ms window elapse
    await p.reconcile()
    moved = [x for x in c.created if x not in before]
    ok = c.cancelled == ["o-yes-57"] and moved == [("yes", 58, 50)]
    record("Q4.1 adverse requote", ok,
           f"cancelled {c.cancelled} then placed {moved}")

    # ---- Q4.2 debounce: no requote inside 250ms ------------------------
    p, c = make_probe([(58, 900.0), (57, 2370.0)], NO_STEPPED)
    rest(p, "yes", 57)
    rest(p, "no", 41)
    p.last_requote = {"yes": 0.0, "no": 0.0}
    await p.reconcile()
    await p.reconcile()                        # immediately again
    record("Q4.2 debounce holds", not c.cancelled and not c.created,
           "no requote within the 250ms window")

    # ---- Q4.4 favourable move does NOT requote -------------------------
    # Our bid is the best; the rest of the book has fallen away below us.
    p, c = make_probe([(57, 50.0), (55, 800.0)], NO)
    rest(p, "yes", 57)
    rest(p, "no", 42)
    p.last_requote = {"yes": 0.0, "no": 0.0}
    await asyncio.sleep(0.30)
    await p.reconcile()
    record("Q4.4 no chase downward", not c.cancelled,
           "our order is the reference at N=0; left alone")

    # ---- Q4.3 rate limit ------------------------------------------------
    p, c = make_probe([(58, 900.0), (57, 2370.0)], NO_STEPPED)
    rest(p, "yes", 57)
    rest(p, "no", 41)
    p.last_requote = {"yes": time.monotonic(), "no": 0.0}   # yes just requoted
    await asyncio.sleep(0.30)
    await p.reconcile()
    record("Q4.3 rate limit", not any(s == "yes" for s, _, _ in c.created),
           "second yes requote suppressed inside 5s")

    # ---- Q5.1 partial fill tops back up at the same price --------------
    p, c = make_probe(YES, NO)
    rest(p, "yes", 57, count=20.0)             # 30 of our 50 were filled
    rest(p, "no", 42)
    p.last_requote = {"yes": 0.0, "no": 0.0}
    await p.reconcile()
    ok = c.created == [("yes", 57, 30)]
    record("Q5.1 replenish", ok, f"topped up by {c.created}")

    # ---- Q5.3 inventory-capped side yields to the other ----------------
    # Long 30 yes (over the 25 cap) and missing BOTH sides: the yes side must
    # be refused and the no side restored, because two-sided presence is what
    # scores.
    p, c = make_probe(YES, NO)
    p.position_fp = 30.0
    p.last_requote = {"yes": 0.0, "no": 0.0}
    await p.reconcile()
    ok = c.created == [("no", 42, 50)]
    record("Q5.3 opposite side", ok,
           f"refused yes (long 30 > cap 25), restored {c.created}")

    # ---- Q2.4 a requote that would self-cross is refused ---------------
    # Touch moves to yes 58 while our stale no still rests at 42: 58+42 = 100.
    p, c = make_probe([(58, 900.0), (57, 2370.0)], [(42, 2384.0)])
    rest(p, "yes", 57)
    rest(p, "no", 42)
    p.last_requote = {"yes": 0.0, "no": 0.0}
    await p.reconcile()
    await asyncio.sleep(0.30)
    await p.reconcile()
    ok = not any(s == "yes" and px == 58 for s, px, _ in c.created)
    record("Q2.4 requote self-cross", ok,
           f"refused yes@58 against our no@42; created {c.created}")

    # ---- a stale or disconnected book must not be quoted into ----------
    p, c = make_probe(YES, NO)
    p.stale = True
    await p.reconcile()
    stale_ok = not c.created
    p, c = make_probe(YES, NO)
    p.connected = False
    await p.reconcile()
    record("no quoting while blind", stale_ok and not c.created,
           "reconcile is a no-op when stale or disconnected")

    print()
    failed = [r for r in RESULTS if not r[1]]
    print(f"{len(RESULTS) - len(failed)}/{len(RESULTS)} passed")
    if failed:
        print("FAILED: " + ", ".join(r[0] for r in failed))
        return 1
    print("Quote state machine behaves per Q3/Q4/Q5.")
    return 0


if __name__ == "__main__":
    raise SystemExit(asyncio.run(main()))
