#!/usr/bin/env python3
"""V1.8a -- establish the per-endpoint pagination contract, read-only.

H-PAGE-1 requires every list read to be a complete cursor walk, and requires
"ordering, retention, deduplication and concurrent-insert behaviour ...
established per endpoint by V1.8a and recorded, not assumed."

This script establishes it. It issues GETs only, through kalshi.Client with
dry_run=True, which physically cannot transmit a write.

Why it is not one experiment: this repository already contains two different
beliefs about the cursor field name -- probebot.py:1123 reads `next_cursor`,
scripts/opportunity.py:54 reads `cursor` -- and at most one of them can be
right for a given endpoint. Guessing the contract is what probebot.py:1100
records going wrong in production.

Probes, per endpoint:

  C1  cursor field name(s) actually present
  C2  terminal signal (absent key / empty string / repeated cursor)
  C3  the item-array key
  C4  limit bounds and clamping
  C5  ordering: which field, and which direction
  C6  completeness and dedup: small-limit walk vs large-limit walk
  C7  cursor opacity: offset-based or keyset-based (decides C9's failure mode)
  C8  filter/param interaction across a cursor
  C9  concurrent-insert behaviour, measured on a naturally churning public list

Output: a JSON report to stdout, and a human summary to stderr.
"""
from __future__ import annotations

import argparse
import base64
import json
import sys
import time
from typing import Any

sys.path.insert(0, "/Users/hugh/kek/lip")

from kalshi import Client, KalshiError  # noqa: E402

# (path, params, small-page-size). The item key is discovered, not assumed.
#
# `small` is per-endpoint because a limit=1 walk of incentive_programs is
# 1000+ requests (probebot.py:1104 records the population passing 1000). The
# portfolio endpoints are small enough to walk one row at a time, which is the
# strongest possible test of cursor arithmetic.
ENDPOINTS = {
    "positions": ("/portfolio/positions", {}, 1),
    "orders": ("/portfolio/orders", {}, 1),
    "fills": ("/portfolio/fills", {}, 1),
    "incentive_programs": ("/incentive_programs", {"status": "active"}, 50),
}

CURSOR_KEYS = ("cursor", "next_cursor", "next", "page_cursor")

# Fields that plausibly order a list, checked in preference order.
ORDER_FIELDS = ("created_time", "ts", "created_ts", "last_updated_ts",
                "start_date", "end_date", "trade_id", "fill_id", "order_id",
                "market_ticker", "ticker")


def item_key(page: dict, path: str) -> str | None:
    """The key holding the list. Discovered from the response, not assumed."""
    best, best_n = None, -1
    for k, v in page.items():
        if isinstance(v, list) and len(v) > best_n:
            best, best_n = k, len(v)
    return best


def cursor_of(page: dict) -> tuple[str | None, str]:
    """Return (field_name_present, value). Empty value means terminal."""
    for k in CURSOR_KEYS:
        if k in page:
            return k, (page.get(k) or "")
    return None, ""


def identity(rec: dict) -> str:
    """A stable per-record identity, for dedup and set comparison."""
    for k in ("trade_id", "fill_id", "order_id", "id", "market_ticker",
              "ticker"):
        if k in rec and rec[k] is not None:
            base = str(rec[k])
            # positions/programs key on ticker, which is unique per list
            return base
    return json.dumps(rec, sort_keys=True)[:200]


def decode_cursor(cur: str) -> Any:
    """C7: is the cursor opaque, an offset, or a keyset?"""
    if not cur:
        return None
    out: dict[str, Any] = {"raw_len": len(cur), "raw_prefix": cur[:24]}
    for pad in ("", "=", "==", "==="):
        try:
            dec = base64.urlsafe_b64decode(cur + pad)
        except Exception:
            continue
        try:
            out["b64_utf8"] = dec.decode()
        except UnicodeDecodeError:
            out["b64_hex"] = dec.hex()[:120]
        break
    if cur.isdigit():
        out["looks_like_offset"] = True
    return out


def walk(c: Client, path: str, params: dict, limit: int,
         max_pages: int = 400) -> dict:
    """Complete cursor walk. Records every page's shape."""
    cur, pages, items, seen_cursors = "", [], [], []
    curkey_seen: set[str] = set()
    while True:
        p = dict(params)
        p["limit"] = limit
        if cur:
            p["cursor"] = cur
        t0 = time.monotonic()
        page = c._request("GET", path, params=p)
        dt = time.monotonic() - t0
        ik = item_key(page, path)
        batch = page.get(ik) or [] if ik else []
        ckey, cval = cursor_of(page)
        if ckey:
            curkey_seen.add(ckey)
        pages.append({
            "n": len(batch),
            "cursor_key": ckey,
            "cursor_empty": not cval,
            "top_level_keys": sorted(page.keys()),
            "ms": round(dt * 1000),
        })
        items.extend(batch)
        if cval:
            seen_cursors.append(cval)
        if not cval:
            break
        if cval in seen_cursors[:-1]:
            pages[-1]["ABORT"] = "cursor repeated -- non-terminating"
            break
        if len(pages) >= max_pages:
            pages[-1]["ABORT"] = f"hit max_pages={max_pages}"
            break
        cur = cval
    ids = [identity(r) for r in items]
    return {
        "item_key": item_key({"x": items}, path) if items else None,
        "pages": pages,
        "n_pages": len(pages),
        "n_items": len(items),
        "n_unique": len(set(ids)),
        "duplicates": sorted({i for i in ids if ids.count(i) > 1})[:10],
        "ids": ids,
        "items": items,
        "cursor_keys_seen": sorted(curkey_seen),
        "first_cursor_decoded": decode_cursor(seen_cursors[0])
        if seen_cursors else None,
        "second_cursor_decoded": decode_cursor(seen_cursors[1])
        if len(seen_cursors) > 1 else None,
    }


def ordering(items: list[dict]) -> dict:
    """C5: which field orders the list, and in which direction."""
    if len(items) < 2:
        return {"verdict": "too few items to determine"}
    out = {}
    for f in ORDER_FIELDS:
        vals = [r.get(f) for r in items]
        if any(v is None for v in vals):
            continue
        asc = all(str(vals[i]) <= str(vals[i + 1]) for i in range(len(vals) - 1))
        desc = all(str(vals[i]) >= str(vals[i + 1]) for i in range(len(vals) - 1))
        if asc or desc:
            out[f] = "ascending" if asc else "descending"
            if asc and desc:
                out[f] = "constant"
    return out or {"verdict": "no tested field is monotonic"}


def limit_bounds(c: Client, path: str, params: dict) -> dict:
    """C4: what limits are accepted, and is a large limit clamped?"""
    out = {}
    for lim in (0, 1, 100, 200, 201, 1000, 1001, 5000):
        p = dict(params)
        p["limit"] = lim
        try:
            page = c._request("GET", path, params=p)
            ik = item_key(page, path)
            n = len(page.get(ik) or []) if ik else 0
            ckey, cval = cursor_of(page)
            out[str(lim)] = {"ok": True, "returned": n,
                             "cursor_present": bool(cval)}
        except KalshiError as e:
            out[str(lim)] = {"ok": False, "status": e.status,
                             "body": e.body[:160]}
    return out


def probe_endpoint(c: Client, name: str, path: str, params: dict,
                   small: int, large: int) -> dict:
    sys.stderr.write(f"\n=== {name}  {path} {params or ''}\n")
    rep: dict[str, Any] = {"path": path, "params": params}

    rep["C4_limit_bounds"] = limit_bounds(c, path, params)
    sys.stderr.write(f"  C4 limits: "
                     f"{ {k: v.get('returned', v.get('status')) for k, v in rep['C4_limit_bounds'].items()} }\n")

    big = walk(c, path, params, large)
    rep["C6_large_walk"] = {k: v for k, v in big.items()
                            if k not in ("ids", "items")}
    sys.stderr.write(f"  large(limit={large}): {big['n_items']} items in "
                     f"{big['n_pages']} pages, {big['n_unique']} unique\n")

    if big["n_items"] == 0:
        rep["VERDICT"] = "empty -- contract not establishable from this account"
        rep["C5_ordering"] = {"verdict": "no items"}
        return rep

    rep["C1_cursor_keys"] = big["cursor_keys_seen"]
    rep["C2_terminal"] = ("cursor key absent from final page"
                          if big["pages"][-1]["cursor_key"] is None
                          else "cursor key present but empty on final page")
    rep["C3_item_key"] = item_key(
        {"k": big["items"]}, path) and next(
        (k for k in ("positions", "orders", "fills", "incentive_programs",
                     "market_positions", "event_positions")
         if k in (big["pages"][0]["top_level_keys"])), None)
    rep["C5_ordering"] = ordering(big["items"])
    rep["C7_cursor_shape"] = {
        "first": big["first_cursor_decoded"],
        "second": big["second_cursor_decoded"],
    }

    small_walk = walk(c, path, params, small)
    rep["C6_small_walk"] = {k: v for k, v in small_walk.items()
                            if k not in ("ids", "items")}
    sys.stderr.write(f"  small(limit={small}): {small_walk['n_items']} items "
                     f"in {small_walk['n_pages']} pages, "
                     f"{small_walk['n_unique']} unique\n")

    a, b = set(big["ids"]), set(small_walk["ids"])
    rep["C6_agreement"] = {
        "large_only": sorted(a - b)[:10],
        "small_only": sorted(b - a)[:10],
        "n_large_only": len(a - b),
        "n_small_only": len(b - a),
        "sets_equal": a == b,
        "large_has_dupes": big["duplicates"],
        "small_has_dupes": small_walk["duplicates"],
    }
    sys.stderr.write(f"  C6 sets_equal={a == b} "
                     f"large_only={len(a - b)} small_only={len(b - a)} "
                     f"dupes={bool(big['duplicates'] or small_walk['duplicates'])}\n")
    return rep


def probe_filter_interaction(c: Client) -> dict:
    """C8: does a cursor survive alongside a filter param?"""
    out = {}
    for label, params in (
        ("orders_status_resting", {"status": "resting"}),
        ("orders_no_filter", {}),
    ):
        try:
            w = walk(c, "/portfolio/orders", params, 1, max_pages=25)
            out[label] = {"n_items": w["n_items"], "n_pages": w["n_pages"],
                          "n_unique": w["n_unique"],
                          "cursor_keys": w["cursor_keys_seen"]}
        except KalshiError as e:
            out[label] = {"error": e.status, "body": e.body[:160]}
    return out


def probe_churn(c: Client, ticker: str, limit: int, pages: int) -> dict:
    """C9: concurrent-insert behaviour, on a naturally churning public list.

    /markets/trades is append-only and public, so it inserts rows during our
    walk without us writing anything. If the cursor is offset-based over a
    newest-first list, inserts push already-seen rows down and the walk yields
    DUPLICATES. If it is keyset-based, the walk is stable and yields none.
    This is the only read-only way to observe the behaviour that decides
    whether a /portfolio/fills walk can double-count a fill.
    """
    out: dict[str, Any] = {"ticker": ticker}
    cur, ids, per_page = "", [], []
    try:
        for _ in range(pages):
            p = {"ticker": ticker, "limit": limit}
            if cur:
                p["cursor"] = cur
            page = c._request("GET", "/markets/trades", params=p)
            ik = item_key(page, "/markets/trades")
            batch = page.get(ik) or [] if ik else []
            ckey, cval = cursor_of(page)
            got = [str(r.get("trade_id")) for r in batch]
            per_page.append({"n": len(batch), "cursor_key": ckey,
                             "first": got[0] if got else None,
                             "last": got[-1] if got else None})
            ids.extend(got)
            if not cval:
                break
            cur = cval
            time.sleep(1.5)  # let real trades land between pages
        out["per_page"] = per_page
        out["n"] = len(ids)
        out["n_unique"] = len(set(ids))
        out["duplicates"] = sorted({i for i in ids if ids.count(i) > 1})[:10]
        out["VERDICT"] = ("keyset-stable (no duplicates across a churning walk)"
                          if len(ids) == len(set(ids))
                          else "OFFSET-LIKE -- duplicates appeared mid-walk")
    except KalshiError as e:
        out["error"] = {"status": e.status, "body": e.body[:200]}
    return out


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__.split("\n")[0])
    ap.add_argument("--small", type=int, default=0,
                    help="override the per-endpoint fine-grained page size")
    ap.add_argument("--large", type=int, default=200)
    ap.add_argument("--churn-ticker", default=None,
                    help="busy ticker for the C9 concurrent-insert probe")
    ap.add_argument("--churn-pages", type=int, default=6)
    ap.add_argument("--out", default="/Users/hugh/kek/lip/notes/pagecontract.json")
    a = ap.parse_args()

    c = Client(dry_run=True, live_ok=False,
               logger=lambda *x, **k: sys.stderr.write(" ".join(map(str, x)) + "\n"))

    report: dict[str, Any] = {"probed_at": time.strftime("%Y-%m-%dT%H:%M:%SZ",
                                                         time.gmtime()),
                              "endpoints": {}}
    for name, (path, params, small) in ENDPOINTS.items():
        try:
            report["endpoints"][name] = probe_endpoint(
                c, name, path, params, a.small or small, a.large)
        except KalshiError as e:
            report["endpoints"][name] = {"error": {"status": e.status,
                                                   "body": e.body[:300]}}
            sys.stderr.write(f"  !! {name}: HTTP {e.status} {e.body[:200]}\n")

    sys.stderr.write("\n=== C8 filter interaction\n")
    report["C8_filter_interaction"] = probe_filter_interaction(c)
    sys.stderr.write(json.dumps(report["C8_filter_interaction"], indent=2) + "\n")

    if a.churn_ticker:
        sys.stderr.write(f"\n=== C9 concurrent insert ({a.churn_ticker})\n")
        report["C9_concurrent_insert"] = probe_churn(
            c, a.churn_ticker, 5, a.churn_pages)
        sys.stderr.write(json.dumps(report["C9_concurrent_insert"],
                                    indent=2)[:1500] + "\n")

    report["request_counts"] = c.counts
    with open(a.out, "w") as f:
        json.dump(report, f, indent=2, default=str)
    sys.stderr.write(f"\nwrote {a.out}  (requests: {c.counts})\n")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
