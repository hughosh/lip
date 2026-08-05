#!/usr/bin/env python3
"""Exercise every Q6 kill switch. §6 makes this a precondition for a live order.

Drives the REAL Probe.check_kills / Probe.halt / Probe.place code paths against
a stub exchange -- nothing here reimplements the logic it is testing. For each
switch it asserts three things:

  1. the switch fires,
  2. it fires for the RIGHT reason (switch id), and
  3. halting actually cancelled every resting order.

Case 0 is a negative control: a healthy probe must NOT halt. Without it a
check_kills that returned a halt unconditionally would pass every other case.

    python scripts/killtest.py
"""
from __future__ import annotations

import asyncio
import sqlite3
import sys
import time
from pathlib import Path

sys.path.insert(0, "/Users/hugh/kek/lip")

import probebot
from kalshi import KalshiError
from probebot import Config, OurOrder, Probe, now_ms

PROGRAM = {
    "id": "test-program-0001",
    "market_ticker": "TEST-TICKER",
    "target_size_fp": "300.00",
    "discount_factor_bps": 5000,
    "period_reward": 1000000,                 # $100
    "start_date": "2026-07-25T00:00:00Z",
    "end_date": "2099-01-01T00:00:00Z",       # far future unless a case moves it
}


class StubClient:
    """Records intent; never touches a socket."""

    def __init__(self) -> None:
        self.created: list[tuple] = []
        self.cancelled: list[str] = []
        self.reject = False
        self.book = {"orderbook_fp": {"yes_dollars": [["0.57", "2370"]],
                                      "no_dollars": [["0.42", "2384"]]}}
        self.counts = {"dry": 0}

    def create_order(self, ticker, side, action, count, price_cents,
                     post_only=True, client_order_id=None):
        if self.reject:
            raise KalshiError(400, "stub rejection", "POST",
                              "/portfolio/events/orders")
        self.created.append((side, price_cents, count))
        return {"order_id": f"stub-{len(self.created)}",
                "remaining_count": f"{count:.2f}", "fill_count": "0.00",
                "ts_ms": now_ms()}

    def cancel_order(self, order_id):
        self.cancelled.append(order_id)
        return {"order_id": order_id, "reduced_by": "50.00"}

    def orders(self, ticker=None, status=None, limit=200):
        return {"orders": []}

    def orderbook(self, ticker):
        return self.book

    def balance(self):
        return {"balance": 10025}

    def positions(self, ticker=None):
        return {"market_positions": []}

    def fills(self, ticker=None, limit=200):
        return {"fills": []}


def make_probe(**cfg_kw) -> tuple[Probe, StubClient]:
    conn = sqlite3.connect(":memory:", isolation_level=None)
    conn.executescript(probebot.SCHEMA)
    client = StubClient()
    cfg = Config(ticker="TEST-TICKER", size=50, dry_run=False, **cfg_kw)
    p = Probe(cfg, dict(PROGRAM), conn, client)
    p.entry_ms = now_ms()
    # A healthy, connected, two-sided steady state.
    p.connected = True
    p.stale = False
    p.last_ref_ms = now_ms()
    p.last_disconnect_ms = 0
    p.book.apply_snapshot([(57, 2370.0), (56, 800.0)], [(42, 2384.0), (41, 700.0)])
    p.orders = {
        "o-yes": OurOrder("o-yes", "yes", 57, 50.0, "c1", now_ms()),
        "o-no": OurOrder("o-no", "no", 42, 50.0, "c2", now_ms()),
    }
    return p, client


RESULTS: list[tuple[str, bool, str]] = []


def record(name: str, ok: bool, note: str) -> None:
    RESULTS.append((name, ok, note))
    print(f"  {'PASS' if ok else 'FAIL'}  {name:24s} {note}")


async def expect_halt(name: str, p: Probe, client: StubClient,
                      switch: str) -> None:
    """Assert the switch fires, names itself correctly, and cancels."""
    h = p.check_kills()
    if h is None:
        record(name, False, "no halt fired")
        return
    if not h.switch.startswith(switch):
        record(name, False, f"wrong switch: {h.switch} ({h.detail})")
        return
    await p.halt(h)
    if p.orders:
        record(name, False, f"halted but {len(p.orders)} orders still tracked")
        return
    if len(client.cancelled) < 2:
        record(name, False, f"only {len(client.cancelled)} cancels issued")
        return
    record(name, True, f"{h.switch} -- {h.detail} (cancelled {len(client.cancelled)})")


async def main() -> int:
    print("Q6 kill switch dry run -- order placement stubbed\n")

    # ---- Case 0: negative control -------------------------------------
    p, c = make_probe()
    h = p.check_kills()
    record("0 negative control", h is None,
           "healthy probe does not halt" if h is None
           else f"spurious halt: {h.switch} -- {h.detail}")

    # ---- Q6.1 P&L ------------------------------------------------------
    p, c = make_probe()
    p.realized_pnl = -16.0
    await expect_halt("Q6.1 pnl", p, c, "Q6.1")

    # boundary: -14.99 must NOT trip, -15.00 must.
    p, _ = make_probe()
    p.realized_pnl = -14.99
    just_under = p.check_kills() is None
    p, _ = make_probe()
    p.realized_pnl = -15.00
    exactly_at = (p.check_kills() or type("x", (), {"switch": ""})).switch.startswith("Q6.1")
    record("Q6.1 boundary", just_under and exactly_at,
           "-14.99 passes, -15.00 trips")

    # ---- Q6.2 inventory ------------------------------------------------
    p, c = make_probe()
    p.position_fp = 60.0
    await expect_halt("Q6.2 inventory", p, c, "Q6.2")

    # ---- Q6.3a wedged feed (REST disagrees with the websocket book) ----
    p, c = make_probe()
    c.book = {"orderbook_fp": {"yes_dollars": [["0.61", "1500"]],
                               "no_dollars": [["0.38", "1200"]]}}
    p.last_ref_ms = now_ms() - 120_000
    await p.verify_book()
    await expect_halt("Q6.3a wedged feed", p, c, "Q6.3")

    # ---- Q6.3a' quiet-but-healthy must NOT halt (the spurious-halt bug) -
    p, c = make_probe()
    p.last_ref_ms = now_ms() - 120_000          # 2 minutes of silence
    await p.verify_book()                        # stub book AGREES
    h = p.check_kills()
    record("Q6.3 quiet != wedged", h is None,
           "2min silent but agreeing book does not halt" if h is None
           else f"spurious halt: {h.switch}")

    # ---- Q6.3b disconnect ----------------------------------------------
    p, c = make_probe()
    p.connected = False
    p.last_disconnect_ms = now_ms() - 61_000
    await expect_halt("Q6.3b disconnect", p, c, "Q6.3")

    # ---- Q6.4 rejection rate -------------------------------------------
    p, c = make_probe()
    p.recent_results = [1] * 6 + [0] * 44        # 6/50 = 12% > 10%
    await expect_halt("Q6.4 rejects", p, c, "Q6.4")

    # and that real rejections actually populate that window
    p, c = make_probe()
    c.reject = True
    p.last_requote["yes"] = 0.0
    await p.place("yes", 57, 50, "test")
    record("Q6.4 feeds window", p.recent_results == [1],
           f"a rejected order records a failure: {p.recent_results}")

    # ---- Q6.5 period end ------------------------------------------------
    p, c = make_probe()
    p.period_end_ms = now_ms() - 1000
    await expect_halt("Q6.5 period end", p, c, "Q6.5")

    # ---- Q6.6 sentinel --------------------------------------------------
    p, c = make_probe()
    probebot.SENTINEL.write_text("killtest\n")
    try:
        await expect_halt("Q6.6 sentinel", p, c, "Q6.6")
    finally:
        probebot.SENTINEL.unlink(missing_ok=True)

    # ---- Q2 guards (safety-critical, same gate) -------------------------
    p, c = make_probe()
    p.orders = {"o-no": OurOrder("o-no", "no", 42, 50.0, "c2", now_ms())}
    p.last_requote["yes"] = 0.0
    ok = not await p.place("yes", 58, 50, "self-cross test")   # 58 + 42 = 100
    record("Q2.4 self-cross", ok and not c.created,
           "blocked a yes@58 against our own no@42")

    p, c = make_probe()
    p.orders = {}
    p.position_cost = 95.0
    p.last_requote["yes"] = 0.0
    ok = not await p.place("yes", 57, 50, "cap test")
    record("Q2.1 capital cap", ok and not c.created,
           "blocked $28.50 on top of $95.00 deployed vs $100 cap")

    p, c = make_probe()
    p.orders = {}
    p.position_fp = 30.0                         # already long 30 yes
    p.last_requote["yes"] = 0.0
    ok = not await p.place("yes", 57, 50, "inventory test")
    record("Q2.3 inventory cap", ok and not c.created,
           "blocked adding to a +30 yes position (cap 25)")

    # ---- halt is permanent ----------------------------------------------
    p, c = make_probe()
    p.position_fp = 60.0
    await p.halt(p.check_kills())
    n_before = len(c.created)
    p.last_requote["yes"] = 0.0
    await p.place("yes", 57, 50, "post-halt attempt")
    await p.reconcile()
    record("halt is permanent", len(c.created) == n_before,
           "no orders placed after halt")

    print()
    failed = [r for r in RESULTS if not r[1]]
    print(f"{len(RESULTS) - len(failed)}/{len(RESULTS)} passed")
    if failed:
        print("FAILED: " + ", ".join(r[0] for r in failed))
        return 1
    print("All Q6 kill switches exercised and confirmed. §6 gate satisfied.")
    return 0


if __name__ == "__main__":
    raise SystemExit(asyncio.run(main()))
