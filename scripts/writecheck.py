#!/usr/bin/env python3
"""Verify the V2 write path with ONE contract, then cancel it.

The order encoding changed under us: the legacy endpoint is HTTP 410 and V2
prices on the YES leg with fixed-point strings. That translation is verified
against the OpenAPI spec but NOT against the matching engine, and the failure
mode that matters is not a rejection (harmless) but an order that rests at a
price we did not intend. One contract answers it for about 57 cents.

Structural guards, not conventions:
  * MAX_COUNT = 1 is asserted immediately before transmission.
  * post_only, so the order cannot cross and cannot take.
  * the cancel runs in a `finally`, so it happens even if an assertion fails.
  * a final sweep re-reads /portfolio/orders and fails loudly if anything is
    still resting.

    python scripts/writecheck.py --ticker <T> --i-understand-this-is-real-money
"""
from __future__ import annotations

import argparse
import sys
import time

sys.path.insert(0, "/Users/hugh/kek/lip")

from kalshi import Client, KalshiError

MAX_COUNT = 1

CHECKS: list[tuple[str, bool, str]] = []


def check(name: str, ok: bool, note: str) -> None:
    CHECKS.append((name, ok, note))
    print(f"  {'PASS' if ok else 'FAIL'}  {name:28s} {note}")


def touch(client: Client, ticker: str) -> tuple[int, float, int, float]:
    ob = (client.orderbook(ticker) or {}).get("orderbook_fp") or {}

    def side(key):
        lv = sorted(((round(float(p) * 100), float(s))
                     for p, s in (ob.get(key) or [])), key=lambda x: -x[0])
        return lv[0] if lv else (None, 0.0)
    (yp, ys), (np_, ns) = side("yes_dollars"), side("no_dollars")
    return yp, ys, np_, ns


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--ticker", required=True)
    ap.add_argument("--i-understand-this-is-real-money", dest="confirm",
                    action="store_true", required=True)
    a = ap.parse_args()

    client = Client(dry_run=False, live_ok=True, logger=lambda m: print(m))

    print(f"write-path verification on {a.ticker}\n")
    yp, ys, np_, ns = touch(client, a.ticker)
    print(f"  book before : yes {ys:.0f} @ {yp}c | no {ns:.0f} @ {np_}c")
    if yp is None:
        print("no yes-side book; aborting")
        return 2

    bal_before = int((client.balance() or {}).get("balance") or 0)
    print(f"  balance     : {bal_before / 100:.2f}\n")

    assert MAX_COUNT == 1, "this script places exactly one contract"

    order_id = None
    try:
        resp = client.create_order(a.ticker, "yes", "buy", MAX_COUNT, yp,
                                   post_only=True)
        order_id = resp.get("order_id")
        check("order accepted", bool(order_id),
              f"order_id={order_id} remaining={resp.get('remaining_count')} "
              f"fill={resp.get('fill_count')}")
        check("post_only did not cross", resp.get("fill_count") in (None, "0.00", 0),
              f"fill_count={resp.get('fill_count')} (must be zero)")

        time.sleep(1.5)

        # --- read it back: is it where we meant to put it? ---
        found = None
        for o in (client.orders(ticker=a.ticker, status="resting")
                  .get("orders") or []):
            if o.get("order_id") == order_id:
                found = o
                break
        check("order is resting", found is not None,
              "visible in GET /portfolio/orders" if found else "NOT FOUND")

        if found:
            bs = found.get("book_side")
            ypx = found.get("yes_price_dollars")
            rem = found.get("remaining_count_fp") or found.get("remaining_count")
            check("book_side is bid", bs == "bid", f"book_side={bs!r}")
            check("price as intended", _cents(ypx) == yp,
                  f"yes_price_dollars={ypx!r} -> {_cents(ypx)}c, meant {yp}c")
            check("count is 1", abs(float(rem or 0) - 1.0) < 1e-9,
                  f"remaining={rem!r}")

        # --- and does the public book agree? ---
        yp2, ys2, _, _ = touch(client, a.ticker)
        check("book shows our size", yp2 == yp and ys2 >= ys + 1 - 1e-9,
              f"touch {yp2}c depth {ys:.0f} -> {ys2:.0f} (expected +1)")

    except KalshiError as e:
        check("order accepted", False, str(e))
    finally:
        if order_id:
            try:
                c = client.cancel_order(order_id)
                check("cancelled", c.get("reduced_by") in ("1.00", "1.0", "1"),
                      f"reduced_by={c.get('reduced_by')!r}")
            except KalshiError as e:
                check("cancelled", False, f"CANCEL FAILED: {e}")

        time.sleep(1.0)
        try:
            left = (client.orders(ticker=a.ticker, status="resting")
                    .get("orders") or [])
            check("nothing left resting", not left,
                  f"{len(left)} resting order(s) remain"
                  if left else "account is clean")
        except KalshiError as e:
            check("nothing left resting", False, f"could not verify: {e}")

    bal_after = int((client.balance() or {}).get("balance") or 0)
    print(f"\n  balance     : {bal_after / 100:.2f} "
          f"(delta {(bal_after - bal_before) / 100:+.2f})")

    failed = [c for c in CHECKS if not c[1]]
    print(f"\n{len(CHECKS) - len(failed)}/{len(CHECKS)} passed")
    if failed:
        print("FAILED: " + ", ".join(c[0] for c in failed))
        print("DO NOT START THE PROBE until this is understood.")
        return 1
    print("Write path verified: orders encode, rest, and cancel as intended.")
    return 0


def _cents(dollars):
    try:
        return round(float(dollars) * 100)
    except (TypeError, ValueError):
        return None


if __name__ == "__main__":
    raise SystemExit(main())
