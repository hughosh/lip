#!/usr/bin/env python3
"""Authenticated Kalshi REST client for the Phase 2 probe.

This is the first write-capable code in the repo. Everything else here talks to
Kalshi read-only. Two safety properties are structural, not conventions:

  1. `Client(dry_run=True)` is the DEFAULT. A dry-run client physically cannot
     transmit a write: `_request` refuses any non-GET before the socket is
     touched. Going live requires passing `dry_run=False` explicitly.
  2. Writes are additionally gated on `live_ok`, a second flag that must be set
     by the caller after its own preflight. Both must be off-default to trade.

Credentials are read by path from ~/.kalshi/ via auth.Signer and are never
logged; `_redact` strips the signature headers from any diagnostic output.
"""
from __future__ import annotations

import json
import threading
import time
import urllib.error
import urllib.parse
import urllib.request
import uuid
from typing import Any

from auth import REST_HOST, Signer

API_PREFIX = "/trade-api/v2"

# Conservative against Kalshi's tiered limits. The probe's steady state is a
# handful of requests a minute; these exist to bound a runaway loop, not to
# extract throughput.
MIN_INTERVAL_READ = 0.10
MIN_INTERVAL_WRITE = 0.20
MAX_ATTEMPTS = 4
TIMEOUT = 30.0

SENSITIVE = {"kalshi-access-key", "kalshi-access-signature", "kalshi-access-timestamp"}


class KalshiError(RuntimeError):
    """A non-retryable API error. Carries the HTTP status and decoded body."""

    def __init__(self, status: int, body: str, method: str, path: str) -> None:
        super().__init__(f"{method} {path} -> HTTP {status}: {body[:400]}")
        self.status = status
        self.body = body


class DryRunViolation(RuntimeError):
    """Raised when a write is attempted on a client that is not cleared for it.

    This is a bug in the caller, never a condition to be caught and retried.
    """


def _redact(headers: dict[str, str]) -> dict[str, str]:
    return {k: ("<redacted>" if k.lower() in SENSITIVE else v) for k, v in headers.items()}


class Client:
    """Thread-safe-enough for one quoting loop plus a background poller."""

    def __init__(self, dry_run: bool = True, live_ok: bool = False,
                 logger=None) -> None:
        self.dry_run = dry_run
        self.live_ok = live_ok
        self.signer = Signer()
        self.log = logger or (lambda *a, **k: None)
        self._lock = threading.Lock()
        self._last_read = 0.0
        self._last_write = 0.0
        self.counts = {"get": 0, "post": 0, "delete": 0, "retry": 0,
                       "reject": 0, "dry": 0}

    # ------------------------------------------------------------ transport

    def _throttle(self, write: bool) -> None:
        gap = MIN_INTERVAL_WRITE if write else MIN_INTERVAL_READ
        with self._lock:
            last = self._last_write if write else self._last_read
            wait = gap - (time.monotonic() - last)
            if wait > 0:
                time.sleep(wait)
            if write:
                self._last_write = time.monotonic()
            else:
                self._last_read = time.monotonic()

    def _request(self, method: str, path: str, body: dict | None = None,
                 params: dict | None = None) -> Any:
        """Signed request against `API_PREFIX + path`.

        The signature covers the path WITHOUT the query string (auth.Signer's
        documented contract); the query is appended to the URL only.
        """
        method = method.upper()
        write = method != "GET"

        if write and (self.dry_run or not self.live_ok):
            raise DryRunViolation(
                f"blocked {method} {path} "
                f"(dry_run={self.dry_run}, live_ok={self.live_ok})"
            )

        full_path = API_PREFIX + path
        url = REST_HOST + full_path
        if params:
            url += "?" + urllib.parse.urlencode(
                {k: v for k, v in params.items() if v is not None}
            )

        payload = json.dumps(body).encode() if body is not None else None
        last_err: Exception | None = None

        for attempt in range(MAX_ATTEMPTS):
            self._throttle(write)
            headers = self.signer.headers(method, full_path)
            headers["Accept"] = "application/json"
            if payload is not None:
                headers["Content-Type"] = "application/json"

            req = urllib.request.Request(url, data=payload, headers=headers,
                                         method=method)
            try:
                with urllib.request.urlopen(req, timeout=TIMEOUT) as r:
                    self.counts[method.lower()] = self.counts.get(method.lower(), 0) + 1
                    raw = r.read().decode()
                    return json.loads(raw) if raw.strip() else {}
            except urllib.error.HTTPError as e:
                text = e.read().decode(errors="replace")
                # 429 and 5xx are transient. Everything else (400 bad order,
                # 401 bad signature, 403, 404) is a real answer -- retrying it
                # would just repeat a rejected order.
                if e.code == 429 or 500 <= e.code < 600:
                    last_err = KalshiError(e.code, text, method, full_path)
                    self.counts["retry"] += 1
                    self.log(f"[rest] {method} {path} HTTP {e.code}, "
                             f"retry {attempt + 1}/{MAX_ATTEMPTS}")
                    time.sleep(2 ** attempt)
                    continue
                self.counts["reject"] += 1
                raise KalshiError(e.code, text, method, full_path) from None
            except (urllib.error.URLError, TimeoutError, OSError) as e:
                last_err = e
                self.counts["retry"] += 1
                self.log(f"[rest] {method} {path} {e!r}, "
                         f"retry {attempt + 1}/{MAX_ATTEMPTS}")
                time.sleep(2 ** attempt)

        raise KalshiError(0, f"exhausted {MAX_ATTEMPTS} attempts: {last_err!r}",
                          method, full_path)

    # ------------------------------------------------------------ read side

    def balance(self) -> dict:
        return self._request("GET", "/portfolio/balance")

    def positions(self, ticker: str | None = None) -> dict:
        return self._request("GET", "/portfolio/positions",
                             params={"ticker": ticker, "limit": 200})

    def orders(self, ticker: str | None = None, status: str | None = None,
               limit: int = 200) -> dict:
        return self._request("GET", "/portfolio/orders",
                             params={"ticker": ticker, "status": status,
                                     "limit": limit})

    def order(self, order_id: str) -> dict:
        return self._request("GET", f"/portfolio/orders/{order_id}")

    def fills(self, ticker: str | None = None, limit: int = 200) -> dict:
        return self._request("GET", "/portfolio/fills",
                             params={"ticker": ticker, "limit": limit})

    def market(self, ticker: str) -> dict:
        return self._request("GET", f"/markets/{ticker}")

    def orderbook(self, ticker: str) -> dict:
        return self._request("GET", f"/markets/{ticker}/orderbook")

    # ----------------------------------------------------------- write side

    def create_order(self, ticker: str, side: str, action: str, count: int,
                     price_cents: int, post_only: bool = True,
                     client_order_id: str | None = None) -> dict:
        """Rest a bid on the yes or no side, priced in that side's own cents.

        Callers speak in the book's terms (a no bid at 42c passes side="no",
        price_cents=42) because that is what the LIP scoring rule is defined
        over. The V2 wire format does NOT work that way, and the translation
        happens here so exactly one place has to be right:

          side="yes", 42c  ->  {"side": "bid", "price": "0.4200"}
          side="no",  42c  ->  {"side": "ask", "price": "0.5800"}

        BookSide is the YES leg only -- "`bid` means buy YES, `ask` means sell
        YES. (Selling YES is economically equivalent to buying NO at `1 -
        price`...)" -- so a no bid at 42c is a yes ask at 58c. `count` and
        `price` are fixed-point STRINGS, not numbers; sending JSON numbers is
        a schema violation.

        Verified against docs.kalshi.com/openapi.yaml (CreateOrderV2Request,
        BookSide) on 2026-07-25. The legacy POST /portfolio/orders now returns
        HTTP 410 deprecated_v1_order_endpoint.
        """
        if side not in ("yes", "no"):
            raise ValueError(f"side must be yes|no, got {side!r}")
        if action != "buy":
            raise ValueError(f"probe only rests bids; got action={action!r}")
        if not (1 <= price_cents <= 99):
            raise ValueError(f"price_cents out of range: {price_cents}")
        if count <= 0:
            raise ValueError(f"count must be positive, got {count}")

        book_side = "bid" if side == "yes" else "ask"
        yes_cents = price_cents if side == "yes" else 100 - price_cents

        coid = client_order_id or str(uuid.uuid4())
        body = {
            "ticker": ticker,
            "side": book_side,
            "count": f"{count:.2f}",
            "price": f"{yes_cents / 100:.4f}",
            "time_in_force": "good_till_canceled",
            # taker_at_cross cancels the INCOMING order on a self-match and
            # leaves our resting maker alive. `maker` would do the opposite and
            # silently destroy the scoring presence Q2.4 exists to protect.
            "self_trade_prevention_type": "taker_at_cross",
            "post_only": post_only,
            "client_order_id": coid,
        }

        if self.dry_run or not self.live_ok:
            self.counts["dry"] += 1
            self.log(f"[DRY] create_order {json.dumps(body)}")
            return {"order_id": f"dry-{coid[:8]}", "client_order_id": coid,
                    "status": "resting", "_dry_run": True,
                    "_intent": {"side": side, "price_cents": price_cents}}
        return self._request("POST", "/portfolio/events/orders", body=body)

    def cancel_order(self, order_id: str) -> dict:
        """DELETE /portfolio/events/orders/{id}.

        Returns {order_id, client_order_id, reduced_by, ts_ms} -- `reduced_by`
        is the count actually cancelled, NOT a full order object.
        """
        if self.dry_run or not self.live_ok:
            self.counts["dry"] += 1
            self.log(f"[DRY] cancel_order {order_id}")
            return {"order_id": order_id, "reduced_by": "0.00", "_dry_run": True}
        return self._request("DELETE", f"/portfolio/events/orders/{order_id}")

    def cancel_all(self, ticker: str) -> list[dict]:
        """Cancel every resting order in `ticker`. Used by the Q6 kill path.

        Best-effort by design: one failed cancel must not prevent the rest, so
        failures are logged and the caller re-reads /portfolio/orders to confirm.
        """
        out = []
        try:
            resting = self.orders(ticker=ticker, status="resting").get("orders") or []
        except KalshiError as e:
            self.log(f"[cancel_all] could not list orders: {e}")
            return out
        for o in resting:
            oid = o.get("order_id")
            if not oid:
                continue
            try:
                out.append(self.cancel_order(oid))
            except KalshiError as e:
                self.log(f"[cancel_all] {oid} failed: {e}")
        return out


if __name__ == "__main__":
    import argparse

    ap = argparse.ArgumentParser(description="Read-only probe of the REST surface.")
    ap.add_argument("--ticker", default=None)
    a = ap.parse_args()

    c = Client(dry_run=True, logger=lambda m: print(m))
    print("balance   :", json.dumps(c.balance()))
    pos = c.positions(a.ticker)
    print("positions :", json.dumps(pos)[:600])
    od = c.orders(a.ticker, limit=10)
    print("orders    :", json.dumps(od)[:1200])
    fl = c.fills(a.ticker, limit=5)
    print("fills     :", json.dumps(fl)[:1200])
    print("counts    :", c.counts)
