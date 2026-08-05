#!/usr/bin/env python3
"""Phase 2 liquidity probe -- rest a two-sided quote in ONE LIP market.

Implements notes/phase2-spec.md. Rule ids in comments (Q3.1, Q6.4, ...) refer
to that document; it is the contract and this file is the implementation.

Safety posture, in order of strength:

  1. DRY RUN IS THE DEFAULT. `--live` is required to transmit anything, and it
     additionally requires `--i-understand-this-is-real-money`. kalshi.Client
     refuses non-GET requests unless both its flags are off-default, so a
     missing check here still cannot place an order.
  2. Every Q6 kill switch cancels all orders and halts PERMANENTLY. There is no
     auto-restart path; a halted probe stays halted until a human runs it again.
  3. The capital cap (Q2.1) is checked immediately before every single order,
     against resting notional plus position cost, not against an internal
     counter that could drift from reality.

The probe opens its OWN websocket. The rig exposes no IPC surface -- its book
state lives only in its process -- so this reconstructs the book independently
using the same `Book` class, and never touches the rig or its database (§7).
"""
from __future__ import annotations

import argparse
import asyncio
import json
import os
import signal
import sqlite3
import sys
import time
import uuid
from dataclasses import dataclass, field
from pathlib import Path

import websockets

sys.path.insert(0, str(Path(__file__).parent))

import probescore
from auth import WS_URL, Signer
from kalshi import Client, DryRunViolation, KalshiError
from rig import Book

HERE = Path(__file__).parent
DB_PATH = HERE / "probe.db"
SENTINEL = HERE / "probe.stop"

API = "https://api.elections.kalshi.com/trade-api/v2"


# ------------------------------------------------------------------ config


@dataclass
class Config:
    """Every number here traces to a rule in notes/phase2-spec.md."""
    ticker: str
    size: int = 50                    # Q2.2 contracts per side
    max_capital: float = 100.0        # Q2.1 hard cap, dollars
    max_inventory: float = 25.0       # Q2.3 net directional contracts
    kill_inventory: float = 50.0      # Q6.2 = 2x Q2.3
    kill_pnl: float = -15.0           # Q6.1 dollars
    flatten_inventory: float = 10.0   # Q2.3 taker exit, net contracts
    flatten_interval_s: float = 15.0  # min seconds between flatten attempts
    max_flatten_loss_c: float = 6.0   # cents/contract a taker exit may cost
    stale_bid_ticks: int = 8          # Q4.4 brake: ticks stranded above the book
    debounce_s: float = 0.250         # Q4.2
    requote_interval_s: float = 5.0   # Q4.3 per side
    max_orders_day: int = 500         # Q4.3
    reject_window: int = 50           # Q6.4
    reject_rate: float = 0.10         # Q6.4
    stale_ref_s: float = 60.0         # Q6.3 silence before a REST cross-check
    poll_interval_s: float = 5.0      # our own orders/positions
    balance_interval_s: float = 60.0  # Q8.3
    dry_run: bool = True


@dataclass
class OurOrder:
    order_id: str
    side: str
    price: int
    count: float
    client_order_id: str
    placed_ms: int


@dataclass
class Halt:
    switch: str
    detail: str
    ts_ms: int


# ------------------------------------------------------------------ schema


SCHEMA = """
CREATE TABLE IF NOT EXISTS run (
  run_id TEXT PRIMARY KEY, started_ms INTEGER, ticker TEXT, program_id TEXT,
  target REAL, df REAL, pool REAL, period_start_ms INTEGER, period_end_ms INTEGER,
  size INTEGER, dry_run INTEGER, note TEXT
);
CREATE TABLE IF NOT EXISTS order_event (
  id INTEGER PRIMARY KEY AUTOINCREMENT, run_id TEXT, ts_ms INTEGER, kind TEXT,
  side TEXT, action TEXT, price INTEGER, count REAL, order_id TEXT,
  client_order_id TEXT, status TEXT, exch_ts_ms INTEGER, reason TEXT, detail TEXT
);
CREATE TABLE IF NOT EXISTS our_fill (
  fill_id TEXT PRIMARY KEY, run_id TEXT, ts_ms INTEGER, exch_ts_ms INTEGER,
  ticker TEXT, side TEXT, action TEXT, price INTEGER, count REAL,
  is_taker INTEGER, order_id TEXT
);
CREATE TABLE IF NOT EXISTS snap (
  ts_ms INTEGER, run_id TEXT, gated INTEGER, connected INTEGER, two_sided INTEGER,
  ref_yes INTEGER, ref_no INTEGER,
  our_yes_price INTEGER, our_yes_size REAL, our_no_price INTEGER, our_no_size REAL,
  yes_share REAL, no_share REAL, share REAL, yes_total REAL, no_total REAL,
  PRIMARY KEY (ts_ms, run_id)
);
CREATE TABLE IF NOT EXISTS balance_poll (
  ts_ms INTEGER, run_id TEXT, balance_cents INTEGER, position_fp REAL,
  realized_pnl REAL, exposure REAL, PRIMARY KEY (ts_ms, run_id)
);
CREATE TABLE IF NOT EXISTS halt (
  ts_ms INTEGER, run_id TEXT, switch TEXT, detail TEXT, PRIMARY KEY (ts_ms, run_id)
);
"""


def connect(path: Path = DB_PATH) -> sqlite3.Connection:
    conn = sqlite3.connect(path, isolation_level=None)
    conn.executescript(SCHEMA)
    return conn


def now_ms() -> int:
    return int(time.time() * 1000)


# ------------------------------------------------------------------- probe


class Probe:
    def __init__(self, cfg: Config, program: dict, conn: sqlite3.Connection,
                 client: Client) -> None:
        self.cfg = cfg
        self.program = program
        self.conn = conn
        self.client = client
        self.run_id = uuid.uuid4().hex[:12]

        self.target = float(program["target_size_fp"])
        self.df = program["discount_factor_bps"] / 10000.0
        self.pool = program["period_reward"] / 10000.0
        self.period_end_ms = _iso_ms(program["end_date"])

        self.book = Book(self.target)
        self.signer = Signer()
        self.seq: dict[int, int] = {}
        self.stale = True
        self._needs_resnapshot = False

        self.connected = False
        self.last_ref_ms = 0
        # Seeded to start-of-run so that NEVER connecting (bad credentials, a
        # wrong ticker) trips Q6.3 like any other outage, instead of idling
        # silently forever.
        self.last_disconnect_ms = now_ms()
        self.stop_requested = False
        self.wedged_feed: str | None = None
        self.last_verify_ms = 0
        self.verifies = 0

        # our state
        self.orders: dict[str, OurOrder] = {}       # order_id -> OurOrder
        self.position_fp = 0.0                      # net yes-equivalent
        self.realized_pnl = 0.0
        self.position_cost = 0.0
        self.seen_fills: set[str] = set()

        # rate limiting / Q6.4
        self.last_requote: dict[str, float] = {"yes": 0.0, "no": 0.0}
        self.adverse_since: dict[str, float | None] = {"yes": None, "no": None}
        self.stranded_since: dict[str, float | None] = {"yes": None, "no": None}
        self.last_flatten = 0.0
        self.flattens = 0
        self.orders_today = 0
        self.order_day = time.gmtime().tm_yday
        self.recent_results: list[int] = []          # 1 = reject, 0 = ok

        self.halted: Halt | None = None
        self.snaps = 0                               # every 1 Hz tick
        self.observed_gated = 0                      # saw a gated book
        self.blind_snaps = 0                         # stale/disconnected: unknowable
        self.present_snaps = 0                       # gated AND two-sided
        self.integrated_share = 0.0                  # Q8.2 numerator

    # ------------------------------------------------------------- logging

    def log(self, msg: str) -> None:
        print(f"[{time.strftime('%H:%M:%S')}] {msg}", flush=True)

    def _ev(self, kind: str, **kw) -> None:
        self.conn.execute(
            "INSERT INTO order_event (run_id, ts_ms, kind, side, action, price, "
            "count, order_id, client_order_id, status, exch_ts_ms, reason, detail) "
            "VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)",
            (self.run_id, now_ms(), kind, kw.get("side"), kw.get("action"),
             kw.get("price"), kw.get("count"), kw.get("order_id"),
             kw.get("client_order_id"), kw.get("status"), kw.get("exch_ts_ms"),
             kw.get("reason"), json.dumps(kw.get("detail")) if kw.get("detail") else None),
        )

    # ------------------------------------------------------- our book state

    def our_resting(self, side: str) -> tuple[int | None, float]:
        """(price, total size) of our resting orders on `side`.

        If we somehow rest at more than one price on a side -- which the quote
        engine avoids but a partially-completed requote can produce -- report
        the BEST price and only the size there. That is what earns the score,
        and overstating our size would inflate the Q8.2 prediction.
        """
        mine = [o for o in self.orders.values() if o.side == side and o.count > 0]
        if not mine:
            return None, 0.0
        best = max(o.price for o in mine)
        return best, sum(o.count for o in mine if o.price == best)

    def deployed_capital(self) -> float:
        """Dollars committed: resting order notional + cost of open position."""
        resting = sum(o.price * o.count for o in self.orders.values()) / 100.0
        return resting + abs(self.position_cost)

    def levels(self, side: str) -> list[tuple[int, float]]:
        d = self.book.yes if side == "yes" else self.book.no
        return sorted(d.items(), key=lambda x: -x[0])

    def external_levels(self, side: str) -> list[tuple[int, float]]:
        """`levels(side)` with our own resting size taken out.

        Live, the websocket book already carries our own orders, so `levels`
        cannot answer the one question that separates "we are at the touch"
        from "we are the last bid standing at a price everyone else has left":
        both look like `best == our price`. Subtracting our size makes the
        difference observable.

        In a dry run nothing was transmitted, so the book never contained our
        size and there is nothing to remove -- subtracting there would delete
        somebody else's real liquidity.
        """
        lv = self.levels(side)
        if self.cfg.dry_run:
            return lv
        price, size = self.our_resting(side)
        if price is None or size <= 0:
            return lv
        return [(p, (s - size) if p == price else s) for p, s in lv]

    def external_best(self, side: str) -> int | None:
        """Best price on `side` that somebody other than us is bidding."""
        for p, s in self.external_levels(side):
            if s > 1e-9:
                return p
        return None

    # ----------------------------------------------------------- Q8.2 snap

    def sample(self) -> None:
        """One 1 Hz snapshot: mirror of the exchange's own scoring cadence."""
        yp, ys = self.our_resting("yes")
        np_, ns = self.our_resting("no")
        yes, no = self.levels("yes"), self.levels("no")

        # Live, the feed already carries our resting orders, so the book is
        # scored exactly as the exchange sees it. In a dry run nothing was
        # actually sent, so our size is missing from the denominator and the
        # share comes out ~2% high. Inject it, so a dry run predicts the same
        # number a live run would and the two are directly comparable.
        if self.cfg.dry_run:
            yes = _inject(yes, yp, ys)
            no = _inject(no, np_, ns)

        share, detail = probescore.combined_share(
            yes, no, self.target, self.df, ys, yp, ns, np_)

        # A stale or disconnected book is UNKNOWABLE, not ungated: the exchange
        # keeps sampling whether or not we can see it. Counting these as
        # non-gated would shrink the presence denominator and flatter us, so
        # they are tracked separately -- excluded from the share estimate
        # (we have no observation) but counted against presence (we cannot
        # prove we were there). The blind fraction is disclosed in Q8 output.
        blind = self.stale or not self.connected
        gated = bool(detail["gated"]) and not blind
        two_sided = bool(ys > 0 and ns > 0)
        self.snaps += 1
        if blind:
            self.blind_snaps += 1
        elif gated:
            self.observed_gated += 1
            self.integrated_share += share
            if two_sided:
                self.present_snaps += 1

        self.conn.execute(
            "INSERT OR REPLACE INTO snap (ts_ms, run_id, gated, connected, "
            "two_sided, ref_yes, ref_no, our_yes_price, our_yes_size, "
            "our_no_price, our_no_size, yes_share, no_share, share, "
            "yes_total, no_total) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)",
            (now_ms(), self.run_id, int(gated), int(self.connected),
             int(two_sided), detail["yes"]["ref"], detail["no"]["ref"],
             yp, ys, np_, ns, detail["yes_share"], detail["no_share"], share,
             detail["yes"]["total_score"], detail["no"]["total_score"]),
        )

    def total_gated_estimate(self) -> float:
        """Gated snapshots in the WHOLE period, including before we joined.

        Scoring is 1 Hz, so the period holds one snapshot per second; the gated
        subset is estimated by the gate rate we observe. This is the
        denominator of our share, and it is emphatically not the number of
        snapshots WE saw -- everyone else's score from the elapsed portion is
        already in it, which is exactly why Q1.1 bars a late entry.
        """
        observed = self.observed_gated + self.blind_snaps
        if not observed:
            return 0.0
        gate_rate = self.observed_gated / observed
        start = _iso_ms(self.program["start_date"])
        period_s = max(1.0, (self.period_end_ms - start) / 1000.0)
        return period_s * gate_rate

    def predicted_payout(self) -> float:
        """Q8.2 -- our integrated share of the pool.

        Numerator: our per-snapshot shares summed over the snapshots we were
        actually present for. Denominator: every gated snapshot in the period.
        Both are in the same 1 Hz units, so the ratio is the fraction of the
        pool the rule awards us. Payouts below $1.00 are not paid at all.
        """
        total = self.total_gated_estimate()
        if total <= 0:
            return 0.0
        return (self.integrated_share / total) * self.pool

    def presence_fraction(self) -> float:
        """Q7 -- share of gated seconds with a two-sided quote resting.

        Blind snapshots count against us: we cannot demonstrate presence for a
        book we could not see, and Q7 is a claim about evidence.
        """
        denom = self.observed_gated + self.blind_snaps
        return (self.present_snaps / denom) if denom else 0.0

    # -------------------------------------------------------- Q6 kill path

    def check_kills(self) -> Halt | None:
        c = self.cfg
        t = now_ms()

        if SENTINEL.exists():                                        # Q6.6
            return Halt("Q6.6-sentinel", str(SENTINEL), t)

        if t >= self.period_end_ms:                                  # Q6.5
            return Halt("Q6.5-period-end",
                        f"end_date {self.program['end_date']} reached", t)

        pnl = self.realized_pnl + self.unrealized_pnl()
        if pnl <= c.kill_pnl:                                        # Q6.1
            return Halt("Q6.1-pnl", f"pnl ${pnl:.2f} <= ${c.kill_pnl:.2f}", t)

        if abs(self.position_fp) > c.kill_inventory:                 # Q6.2
            return Halt("Q6.2-inventory",
                        f"net {self.position_fp:+.0f} > {c.kill_inventory:.0f}", t)

        # Q6.3 -- "quoting into a book we cannot see". Note that a silent book
        # is NOT by itself evidence of that: Q1.7 selected for a deep, quiet
        # market, where minutes with no reference change are the normal state.
        # Taking the rule literally halts a healthy probe on its first quiet
        # stretch and guarantees Q7 fails. `wedged_feed` is set only after a
        # REST cross-check has shown our book actually disagrees with the
        # exchange -- that is the condition the rule is really about.
        if self.wedged_feed:
            return Halt("Q6.3-wedged-feed", self.wedged_feed, t)
        if not self.connected and self.last_disconnect_ms and \
                (t - self.last_disconnect_ms) > c.stale_ref_s * 1000:
            return Halt("Q6.3-disconnect",
                        f"disconnected for "
                        f"{(t - self.last_disconnect_ms) / 1000:.0f}s", t)

        w = self.recent_results[-c.reject_window:]                   # Q6.4
        if len(w) >= c.reject_window and (sum(w) / len(w)) > c.reject_rate:
            return Halt("Q6.4-rejects",
                        f"{sum(w)}/{len(w)} rejected over the last "
                        f"{c.reject_window} orders", t)
        return None

    def unrealized_pnl(self) -> float:
        """Mark the open position at the mid. Zero when flat, which is the
        steady state -- matched yes/no fills self-liquidate (spec §3)."""
        if not self.position_fp:
            return 0.0
        mid = self.book.mid()
        if mid is None:
            return 0.0
        return (self.position_fp * mid / 100.0) - self.position_cost

    async def halt(self, h: Halt) -> None:
        if self.halted:
            return
        self.halted = h
        self.log(f"*** HALT {h.switch}: {h.detail}")
        self.conn.execute(
            "INSERT OR REPLACE INTO halt (ts_ms, run_id, switch, detail) "
            "VALUES (?,?,?,?)", (h.ts_ms, self.run_id, h.switch, h.detail))
        await self.cancel_all("halt:" + h.switch)

    # ------------------------------------------------------------- ordering

    def _rate_ok(self, side: str) -> bool:
        c = self.cfg
        day = time.gmtime().tm_yday
        if day != self.order_day:
            self.order_day, self.orders_today = day, 0
        if self.orders_today >= c.max_orders_day:
            return False
        return (time.monotonic() - self.last_requote[side]) >= c.requote_interval_s

    async def place(self, side: str, price: int, count: float,
                    reason: str) -> bool:
        """Place one resting bid. Every Q2 guard is applied here, immediately
        before transmission, so no caller can bypass them."""
        if self.halted:
            return False
        c = self.cfg

        if not (1 <= price <= 99):
            self._ev("skip", side=side, price=price, reason="price out of range")
            return False

        # Q2.4 -- never self-cross our own resting order on the other side.
        other = "no" if side == "yes" else "yes"
        op, os_ = self.our_resting(other)
        if op is not None and os_ > 0 and price + op >= 100:
            self._ev("skip", side=side, price=price,
                     reason=f"Q2.4 self-cross: {price} + our {other} {op} >= 100")
            self.log(f"  Q2.4 blocked {side}@{price} (our {other} rests at {op})")
            return False

        # Q2.1 -- hard capital cap, measured against reality not a counter.
        cost = price * count / 100.0
        if self.deployed_capital() + cost > c.max_capital + 1e-9:
            self._ev("skip", side=side, price=price, count=count,
                     reason=f"Q2.1 cap: deployed ${self.deployed_capital():.2f} "
                            f"+ ${cost:.2f} > ${c.max_capital:.2f}")
            return False

        # Q2.3 -- do not add to the side that is already long.
        if side == "yes" and self.position_fp >= c.max_inventory:
            self._ev("skip", side=side, reason="Q2.3 inventory cap (long yes)")
            return False
        if side == "no" and -self.position_fp >= c.max_inventory:
            self._ev("skip", side=side, reason="Q2.3 inventory cap (long no)")
            return False

        if not self._rate_ok(side):
            return False

        coid = str(uuid.uuid4())
        self._ev("place", side=side, action="buy", price=price, count=count,
                 client_order_id=coid, reason=reason)
        try:
            resp = await asyncio.to_thread(
                self.client.create_order, self.cfg.ticker, side, "buy",
                int(count), price, True, coid)
        except (KalshiError, DryRunViolation, ValueError) as e:
            self.recent_results.append(1)
            self._ev("reject", side=side, price=price, count=count,
                     client_order_id=coid, reason=str(e)[:300])
            self.log(f"  REJECT {side}@{price}x{count}: {e}")
            return False

        # CreateOrderV2Response is FLAT: {order_id, fill_count, remaining_count,
        # ts_ms}. Only `remaining_count` rests and therefore only it scores --
        # recording the requested count instead would overstate Q8.2.
        o = resp or {}
        oid = o.get("order_id") or f"unknown-{coid[:8]}"
        remaining = float(o.get("remaining_count") or count)
        filled = float(o.get("fill_count") or 0)
        self.recent_results.append(0)
        self.orders_today += 1
        self.last_requote[side] = time.monotonic()
        if remaining > 0:
            self.orders[oid] = OurOrder(oid, side, price, remaining, coid, now_ms())
        self._ev("ack", side=side, price=price, count=remaining, order_id=oid,
                 client_order_id=coid, status="resting" if remaining else "filled",
                 exch_ts_ms=o.get("ts_ms"),
                 detail={"fill_count": filled} if filled else None)
        self.log(f"  placed {side} {remaining:.0f} @ {price}c ({reason})"
                 + (f" [{filled:.0f} filled on entry]" if filled else ""))
        return True

    async def cancel(self, order_id: str, reason: str) -> bool:
        o = self.orders.get(order_id)
        self._ev("cancel", side=o.side if o else None,
                 price=o.price if o else None, order_id=order_id, reason=reason)
        try:
            await asyncio.to_thread(self.client.cancel_order, order_id)
        except (KalshiError, DryRunViolation) as e:
            self._ev("reject", order_id=order_id, reason=f"cancel failed: {e}"[:300])
            self.log(f"  cancel {order_id} failed: {e}")
            return False
        self.orders.pop(order_id, None)
        return True

    async def cancel_all(self, reason: str) -> None:
        for oid in list(self.orders):
            await self.cancel(oid, reason)
        if not self.cfg.dry_run:
            # Re-read rather than trust local state: the kill path must leave
            # nothing resting, and a cancel we think succeeded might not have.
            try:
                left = await asyncio.to_thread(
                    self.client.orders, self.cfg.ticker, "resting")
                n = len(left.get("orders") or [])
                self.log(f"  post-cancel sweep: {n} still resting")
                self._ev("sweep", reason=f"{n} resting after cancel_all")
            except KalshiError as e:
                self.log(f"  post-cancel sweep failed: {e}")

    # --------------------------------------------------------- flatten path

    async def maybe_flatten(self) -> None:
        """Q2.3 -- exit an accumulating one-sided position with a taker order.

        Q2.3 says "stop replenishing the long side AND flatten to within the
        cap"; §7 forbids "taker orders of any kind". The spec contradicts
        itself, and the code implemented only the first half, which is exactly
        how leg 2 reached +61 contracts with no way out: the same one-sided
        flow that built the position is the flow that would have to lift a
        resting exit, so a passive exit is not an exit at all.

        This is the missing half, and it is deliberately a TAKER. It makes the
        strategy no longer the pre-registered one; that is a known, accepted
        cost, recorded in the settlement write-up.
        """
        c = self.cfg
        if self.halted or abs(self.position_fp) < c.flatten_inventory:
            return
        if (time.monotonic() - self.last_flatten) < c.flatten_interval_s:
            return
        if self.stale or not self.connected:
            # Crossing into a book we cannot see would price the exit off a
            # stale reference -- the Q6.3 hazard, applied to a taker.
            return
        await self.flatten(self.position_fp)

    async def flatten(self, net: float) -> bool:
        """Cross the spread to bring net inventory back toward zero.

        `kalshi.create_order` refuses sells outright, so a long yes position is
        closed by BUYING NO: selling yes at B is buying no at 100 - B. That is
        the route proven by the manual exit of leg 2.

        Two ordering constraints, both load-bearing:

          1. Our own bid rests on the side we are about to hit. Self-trade
             prevention is `taker_at_cross`, which cancels the INCOMING order,
             so crossing without cancelling first would silently cancel the
             flatten and leave the position open.
          2. The cross price is read BEFORE that cancel. Afterwards our size is
             gone from `self.orders` but the websocket book has not necessarily
             caught up, so `external_best` would fall back to reading our own
             stale level as if it were somebody else's bid.
        """
        c = self.cfg
        n = int(abs(net))                      # floor: never overshoot into a flip
        if self.halted or n <= 0:
            return False

        # Long yes -> sell yes -> hit the YES bids, paying for it with a NO buy.
        cross = "yes" if net > 0 else "no"
        exit_side = "no" if net > 0 else "yes"

        best = self.external_best(cross)       # constraint 2: read before cancel
        if best is None:
            self._ev("skip", side=exit_side,
                     reason=f"flatten: no external {cross} bid to cross")
            self.log(f"  flatten deferred: nobody bidding {cross}")
            return False
        price = 100 - best
        if not (1 <= price <= 99):
            self._ev("skip", side=exit_side, price=price,
                     reason=f"flatten: cross price {price} out of range")
            return False

        # --- price floor. Without this the flatten is unbounded: it crosses to
        # whatever bid happens to be best, and a book with a hole in it turns a
        # protective exit into the worst trade available. This is not
        # hypothetical -- on 2026-07-29 the yes touch on this market printed 9c
        # for roughly a minute after a 50-lot sweep, while the true price was
        # 44c. An unguarded flatten would have sold there for about -$20
        # against a -$2.50 exit thirty seconds later.
        #
        # Bound the loss instead of the price. We hold `avg` cents of cost per
        # contract; the hedge costs `price`; the pair settles at 100, so the
        # loss per contract is (avg + price) - 100. Refuse anything worse than
        # `max_flatten_loss_c`, and let the resting quote on the exit side work
        # as a maker hedge in the meantime -- which is a real exit, at a better
        # price, whenever the flow that built the position reverses at all.
        avg = (abs(self.position_cost) / abs(net) * 100.0) if net else 0.0
        loss_c = (avg + price) - 100.0
        if avg > 0 and loss_c > c.max_flatten_loss_c:
            self.last_flatten = time.monotonic()      # back off, do not spin
            self._ev("skip", side=exit_side, price=price, count=n,
                     reason=f"flatten refused: crossing at {price}c against "
                            f"{avg:.1f}c cost loses {loss_c:.1f}c/contract "
                            f"> {c.max_flatten_loss_c:.1f}c cap")
            self.log(f"  flatten HELD: {cross} bid {best}c would cost "
                     f"{loss_c:.1f}c/contract (cap {c.max_flatten_loss_c:.0f}c); "
                     f"resting {exit_side} quote remains the hedge")
            return False

        for oid in [k for k, v in self.orders.items() if v.side == cross]:
            await self.cancel(oid, f"flatten: clear our {cross} bid before crossing")

        coid = str(uuid.uuid4())
        self._ev("flatten", side=exit_side, action="buy", price=price, count=n,
                 client_order_id=coid,
                 reason=f"net {net:+.1f} >= {c.flatten_inventory:.0f}; "
                        f"crossing the {cross} bid at {best}c")
        try:
            resp = await asyncio.to_thread(
                self.client.create_order, c.ticker, exit_side, "buy", n, price,
                False, coid)
        except (KalshiError, DryRunViolation, ValueError) as e:
            self.recent_results.append(1)
            self._ev("reject", side=exit_side, price=price, count=n,
                     client_order_id=coid, reason=f"flatten failed: {e}"[:300])
            self.log(f"  FLATTEN REJECTED {exit_side} {n}@{price}c: {e}")
            return False

        o = resp or {}
        oid = o.get("order_id")
        filled = float(o.get("fill_count") or 0)
        remaining = float(o.get("remaining_count") or 0)
        self.recent_results.append(0)
        self.orders_today += 1
        self.flattens += 1
        self.last_flatten = time.monotonic()
        self._ev("ack", side=exit_side, action="buy", price=price, count=n,
                 order_id=oid, client_order_id=coid, status="flatten",
                 exch_ts_ms=o.get("ts_ms"),
                 detail={"fill_count": filled, "remaining_count": remaining,
                         "net_before": net, "crossed": cross, "taker": True})
        self.log(f"  FLATTEN bought {exit_side} {n} @ {price}c "
                 f"(net was {net:+.1f}; {filled:.0f} filled, {remaining:.0f} left)")

        # An unfilled remainder does NOT rest. post_only=False only permits the
        # cross; whatever does not trade would sit as a bid one tick inside the
        # touch on the exit side, which improves the touch (Q3.2) and is not a
        # quote the scoring engine should ever see us make.
        if remaining > 0 and oid:
            await self.cancel(oid, "flatten: withdraw unfilled remainder")
        return True

    # ------------------------------------------------------- quote engine

    async def reconcile(self) -> None:
        """Drive actual quotes toward the desired two-sided state (Q3/Q4/Q5).

        Desired: `size` contracts resting at the touch on each side. The engine
        is written as a reconciliation rather than an event handler so that a
        fill, a requote and a restart all converge through the same path.
        """
        if self.halted or self.stale or not self.connected:
            return
        c = self.cfg
        by, _ = self.book.best_yes()
        bn, _ = self.book.best_no()
        if by is None or bn is None:
            return

        for side, best in (("yes", by), ("no", bn)):
            price, size = self.our_resting(side)

            # --- Q4.1/Q4.4: only an ADVERSE move (touch above us) requires a
            # requote. A touch that falls to or below our price means we ARE
            # the reference and already score N = 0.
            adverse = price is not None and best > price
            if adverse:
                if self.adverse_since[side] is None:
                    self.adverse_since[side] = time.monotonic()
            else:
                self.adverse_since[side] = None

            # --- Q4.4 disaster brake. Q4.4 itself is right, and for a reason
            # the spec states wrongly: score is DF^(ticks from reference) x
            # size, so a bid left ABOVE the crowd does not merely stay at
            # N = 0, it pushes everyone ELSE down the discount curve and our
            # share rises sharply -- measured on this book at 74% of the yes
            # side six ticks up, against 4.4% at the touch. Chasing a
            # favourable move down would throw that away, which is why we do
            # not do it for small gaps.
            #
            # A large gap is a different object. Once the market has genuinely
            # left, our bid is a standing gift: every seller hits us first, at
            # a price nobody else will pay. So we hold a small gap and abandon
            # a wide one. The inventory flatten bounds each such episode, and
            # this bounds how far wrong the price can be.
            ext = self.external_best(side)
            stranded = (price is not None and ext is not None
                        and (price - ext) >= c.stale_bid_ticks)
            if stranded:
                if self.stranded_since[side] is None:
                    self.stranded_since[side] = time.monotonic()
            else:
                self.stranded_since[side] = None

            if price is None or size <= 0:
                # Q3.1/Q5.1 -- absent side: join the touch to restore presence.
                await self.place(side, best, c.size - size, "join/replenish")
                continue

            if adverse:
                # Q4.2 -- the new reference must HOLD for the debounce window.
                held = time.monotonic() - (self.adverse_since[side] or 0)
                if held < c.debounce_s:
                    continue
                if not self._rate_ok(side):
                    continue
                # Cancel then place: brief absence is economically irrelevant
                # (economics.md §4) and it can never breach the Q2.1 cap, which
                # placing-then-cancelling could.
                for oid in [k for k, v in self.orders.items() if v.side == side]:
                    await self.cancel(oid, f"Q4.1 requote {price}->{best}")
                await self.place(side, best, c.size, f"Q4.1 requote to {best}")
                self.adverse_since[side] = None
                continue

            if stranded:
                # Same debounce and rate limit as Q4.1: a momentary hole in the
                # book is not the market leaving.
                held = time.monotonic() - (self.stranded_since[side] or 0)
                if held < c.debounce_s:
                    continue
                if not self._rate_ok(side):
                    continue
                for oid in [k for k, v in self.orders.items() if v.side == side]:
                    await self.cancel(oid, f"Q4.4-brake {price}->{ext}")
                await self.place(side, ext, c.size,
                                 f"Q4.4-brake requote {price}->{ext} "
                                 f"({price - ext} ticks stranded)")
                self.stranded_since[side] = None
                continue

            if size < c.size:
                # Q5.1 -- partially filled at a still-good price: top back up.
                # Q5.3 -- if inventory blocks this side, `place` skips it and
                # the opposite side is restored on its own pass.
                await self.place(side, price, c.size - size, "Q5.1 top-up")

    # ---------------------------------------------------------- REST polls

    async def poll_self(self) -> None:
        """Refresh our orders / position / fills from the exchange.

        The exchange is authoritative. Local order state is an optimisation,
        and anywhere the two disagree the exchange wins.
        """
        if self.cfg.dry_run:
            return
        try:
            resting = await asyncio.to_thread(
                self.client.orders, self.cfg.ticker, "resting")
            live: dict[str, OurOrder] = {}
            for o in resting.get("orders") or []:
                oid = o.get("order_id")
                if not oid:
                    continue
                # Reads still come back in the yes/no shape even though writes
                # are bid/ask, so normalise here. `book_side` is authoritative
                # when present: ask == a no bid, whatever `side` claims.
                bs = o.get("book_side")
                side = ("yes" if bs == "bid" else "no") if bs else \
                    (o.get("side") or o.get("outcome_side"))
                px = _cents(o.get(f"{side}_price_dollars"))
                rem = float(o.get("remaining_count_fp")
                            or o.get("remaining_count") or 0)
                if px is None or rem <= 0:
                    continue
                prev = self.orders.get(oid)
                live[oid] = OurOrder(oid, side, px, rem,
                                     prev.client_order_id if prev else "",
                                     prev.placed_ms if prev else now_ms())
            self.orders = live

            pos = await asyncio.to_thread(self.client.positions, self.cfg.ticker)
            for mp in pos.get("market_positions") or []:
                if mp.get("ticker") != self.cfg.ticker:
                    continue
                self.position_fp = float(mp.get("position_fp") or 0)
                self.realized_pnl = float(mp.get("realized_pnl_dollars") or 0)
                self.position_cost = float(mp.get("market_exposure_dollars") or 0)

            fills = await asyncio.to_thread(self.client.fills, self.cfg.ticker, 100)
            for f in fills.get("fills") or []:
                fid = f.get("fill_id")
                if not fid or fid in self.seen_fills:
                    continue
                self.seen_fills.add(fid)
                bs = f.get("book_side")
                side = ("yes" if bs == "bid" else "no") if bs else \
                    (f.get("side") or f.get("outcome_side"))
                self.conn.execute(
                    "INSERT OR REPLACE INTO our_fill (fill_id, run_id, ts_ms, "
                    "exch_ts_ms, ticker, side, action, price, count, is_taker, "
                    "order_id) VALUES (?,?,?,?,?,?,?,?,?,?,?)",
                    (fid, self.run_id, now_ms(), int(f.get("ts") or 0) * 1000,
                     self.cfg.ticker, side, f.get("action"),
                     _cents(f.get(f"{side}_price_dollars")),
                     float(f.get("count_fp") or 0),
                     int(bool(f.get("is_taker"))), f.get("order_id")))
                self.log(f"  FILL {side} {f.get('count_fp')} @ "
                         f"{_cents(f.get(f'{side}_price_dollars'))}c "
                         f"{'TAKER' if f.get('is_taker') else 'maker'}")
        except KalshiError as e:
            self.log(f"[poll_self] {e}")

    async def verify_book(self) -> None:
        """Cross-check the websocket book against a REST snapshot.

        Called only after `stale_ref_s` of silence. A deep quiet market goes
        minutes without a delta, so silence alone proves nothing; this asks the
        exchange directly. Agreement means the feed is healthy and merely idle
        -- the staleness clock is reset. Disagreement means we have been
        quoting against a book that no longer exists, which is precisely the
        Q6.3 hazard, and it arms the kill switch.
        """
        self.last_verify_ms = now_ms()
        self.verifies += 1
        try:
            resp = await asyncio.to_thread(self.client.orderbook, self.cfg.ticker)
        except KalshiError as e:
            # Could not verify. Not proof of a wedge -- do not halt on it, but
            # leave the clock unreset so we retry on the next pass.
            self.log(f"[verify] REST orderbook unavailable: {e}")
            self.last_ref_ms = 0
            return

        ob = (resp or {}).get("orderbook_fp") or {}

        def rest_side(key):
            return sorted(((round(float(p) * 100), float(s))
                           for p, s in (ob.get(key) or [])), key=lambda x: -x[0])

        for key, mine in (("yes_dollars", self.levels("yes")),
                          ("no_dollars", self.levels("no"))):
            theirs = rest_side(key)
            t_top = theirs[0] if theirs else (None, 0.0)
            m_top = mine[0] if mine else (None, 0.0)
            # Compare only the touch. Deep levels can legitimately differ by a
            # delta in flight; a touch that disagrees means we are scoring
            # against the wrong reference price, which is what matters.
            if t_top[0] != m_top[0]:
                self.wedged_feed = (
                    f"{key} touch disagrees after "
                    f"{(now_ms() - self.last_ref_ms) / 1000:.0f}s silence: "
                    f"ws={m_top[0]} rest={t_top[0]}")
                self.log(f"[verify] WEDGED: {self.wedged_feed}")
                return

        self.log(f"[verify] book quiet but healthy "
                 f"(yes {self.book.best_yes()[0]}c / no {self.book.best_no()[0]}c)")
        self.last_ref_ms = now_ms()

    async def poll_balance(self) -> None:
        """Q8.3 -- the payout is observable ONLY as a balance delta."""
        try:
            b = await asyncio.to_thread(self.client.balance)
        except KalshiError as e:
            self.log(f"[balance] {e}")
            return
        self.conn.execute(
            "INSERT OR REPLACE INTO balance_poll (ts_ms, run_id, balance_cents, "
            "position_fp, realized_pnl, exposure) VALUES (?,?,?,?,?,?)",
            (now_ms(), self.run_id, int(b.get("balance") or 0), self.position_fp,
             self.realized_pnl, self.position_cost))

    # ------------------------------------------------------------- feed

    def _handle(self, data: dict) -> None:
        kind = data.get("type")
        msg = data.get("msg") or {}

        sid, seq = data.get("sid"), data.get("seq")
        if sid is not None and seq is not None:
            prev = self.seq.get(sid)
            self.seq[sid] = seq
            if prev is not None and seq != prev + 1:
                # A gap means we may have missed a delta. Quoting into a book
                # we cannot trust is exactly what Q6.3 exists to prevent, so
                # quarantine until a fresh snapshot arrives.
                self.stale = True
                self._needs_resnapshot = True
                self.log(f"[seq gap {prev} -> {seq}] book quarantined")
                return

        if kind == "orderbook_snapshot":
            if msg.get("market_ticker") != self.cfg.ticker:
                return

            def lv(key):
                return [(round(float(p) * 100), float(s))
                        for p, s in (msg.get(key) or [])]
            self.book.apply_snapshot(lv("yes_dollars_fp"), lv("no_dollars_fp"))
            self.stale = False
            self.last_ref_ms = now_ms()

        elif kind == "orderbook_delta":
            if msg.get("market_ticker") != self.cfg.ticker or self.stale:
                return
            self.book.apply_delta(msg["side"],
                                  round(float(msg["price_dollars"]) * 100),
                                  float(msg["delta_fp"]))
            self.last_ref_ms = now_ms()

        elif kind == "error":
            self.log(f"[ws error] {data}")

    async def feed(self) -> None:
        backoff = 1.0
        while not self.halted:
            try:
                # The rig lets the server drive the heartbeat, which is fine for
                # a passive recorder. The probe has money resting on this
                # connection, so it pings actively: a half-open socket then
                # raises ConnectionClosed instead of silently going quiet, and
                # the Q6.3 disconnect branch picks it up.
                async with websockets.connect(
                    WS_URL, additional_headers=self.signer.ws_headers(),
                    ping_interval=20, ping_timeout=20, max_queue=2048,
                ) as ws:
                    await ws.send(json.dumps({
                        "id": 1, "cmd": "subscribe",
                        "params": {"channels": ["orderbook_delta"],
                                   "market_tickers": [self.cfg.ticker]},
                    }))
                    self.connected = True
                    self.last_ref_ms = now_ms()
                    backoff = 1.0
                    self.log(f"connected: {self.cfg.ticker}")
                    async for raw in ws:
                        if self.halted:
                            break
                        try:
                            self._handle(json.loads(raw))
                        except Exception as e:
                            self.log(f"[handle] {e!r}")
                        if self._needs_resnapshot:
                            self._needs_resnapshot = False
                            await ws.send(json.dumps({
                                "id": 3, "cmd": "update_subscription",
                                "params": {"sids": list(self.seq),
                                           "action": "get_snapshot",
                                           "market_tickers": [self.cfg.ticker]},
                            }))
            except (websockets.ConnectionClosed, OSError) as e:
                self.log(f"[reconnect in {backoff:.0f}s] {e!r}")
            finally:
                if self.connected:
                    self.last_disconnect_ms = now_ms()
                self.connected = False
                # Discard the book: any level carried across a gap may be wrong.
                self.book.yes.clear()
                self.book.no.clear()
                self.stale = True
                self.seq.clear()
            if self.halted:
                break
            await asyncio.sleep(backoff)
            backoff = min(backoff * 2, 60.0)

    # -------------------------------------------------------- control loop

    async def control(self) -> None:
        last_poll = last_balance = 0.0
        last_report = time.monotonic()
        while not self.halted:
            tick = time.monotonic()

            if self.stop_requested:
                await self.halt(Halt("signal", "SIGINT/SIGTERM", now_ms()))
                break

            # Silence past the threshold is ambiguous; resolve it against the
            # exchange before letting Q6.3 act on it.
            if (self.connected and not self.stale and self.last_ref_ms
                    and (now_ms() - self.last_ref_ms) > self.cfg.stale_ref_s * 1000):
                await self.verify_book()

            h = self.check_kills()
            if h:
                await self.halt(h)
                break

            if tick - last_poll >= self.cfg.poll_interval_s:
                await self.poll_self()
                last_poll = tick
            if tick - last_balance >= self.cfg.balance_interval_s:
                await self.poll_balance()
                last_balance = tick

            self.sample()                       # Q8.2, 1 Hz
            await self.maybe_flatten()          # Q2.3 taker exit
            await self.reconcile()

            if tick - last_report >= 60.0:
                self.report()
                last_report = tick

            await asyncio.sleep(max(0.0, 1.0 - (time.monotonic() - tick)))

    def report(self) -> None:
        mean = (self.integrated_share / self.observed_gated) \
            if self.observed_gated else 0.0
        yp, ys = self.our_resting("yes")
        np_, ns = self.our_resting("no")
        self.log(
            f"snaps={self.snaps} gated={self.observed_gated} "
            f"blind={self.blind_snaps} present={self.presence_fraction():.1%} "
            f"share={mean:.2%} pred=${self.predicted_payout():.2f} "
            f"| quote yes {ys:.0f}@{yp} no {ns:.0f}@{np_} "
            f"| pos {self.position_fp:+.0f} pnl "
            f"${self.realized_pnl + self.unrealized_pnl():+.2f} "
            f"| cap ${self.deployed_capital():.2f} flat={self.flattens}"
        )

    async def run(self) -> None:
        self.entry_ms = now_ms()
        self.conn.execute(
            "INSERT OR REPLACE INTO run (run_id, started_ms, ticker, program_id, "
            "target, df, pool, period_start_ms, period_end_ms, size, dry_run, note) "
            "VALUES (?,?,?,?,?,?,?,?,?,?,?,?)",
            (self.run_id, self.entry_ms, self.cfg.ticker, self.program["id"],
             self.target, self.df, self.pool, _iso_ms(self.program["start_date"]),
             self.period_end_ms, self.cfg.size, int(self.cfg.dry_run),
             "phase2 probe"))
        self.log(f"run {self.run_id} ticker={self.cfg.ticker} "
                 f"target={self.target:.0f} df={self.df} pool=${self.pool:.0f} "
                 f"size={self.cfg.size} dry_run={self.cfg.dry_run}")
        feed = asyncio.create_task(self.feed())
        try:
            await self.control()
        finally:
            if not self.halted:
                await self.halt(Halt("shutdown", "control loop exited", now_ms()))
            # Unconditional final sweep. `halt` already cancels, but it can
            # itself fail partway, and leaving live orders resting behind a
            # dead process is the one outcome with no recovery path.
            try:
                await self.cancel_all("final sweep")
            except Exception as e:
                self.log(f"!! final cancel sweep failed: {e!r} -- "
                         f"CHECK FOR RESTING ORDERS MANUALLY")
            feed.cancel()
            self.report()


# ------------------------------------------------------------------ helpers


def _inject(levels: list[tuple[int, float]], price: int | None,
            size: float) -> list[tuple[int, float]]:
    """Add `size` at `price`, creating the level if the book lacks it."""
    if price is None or size <= 0:
        return levels
    out = [(p, s + size if p == price else s) for p, s in levels]
    if not any(p == price for p, _ in out):
        out.append((price, size))
        out.sort(key=lambda x: -x[0])
    return out


def _iso_ms(s: str) -> int:
    import datetime as dt
    return int(dt.datetime.fromisoformat(s.replace("Z", "+00:00")).timestamp() * 1000)


def _cents(dollars) -> int | None:
    try:
        return round(float(dollars) * 100)
    except (TypeError, ValueError):
        return None


def fetch_program(ticker: str) -> dict:
    """Find the active liquidity program for `ticker`.

    Pages the listing instead of reading one fixed window. The count of active
    programs grew past 1000 mid-probe, which pushed this market outside a
    one-shot limit=200 fetch: a restart then died with "no active liquidity
    program" while the program was in fact still running to its end_date. A
    lookup that silently depends on position in an unsorted list is a
    correctness bug, not a tuning parameter.
    """
    import urllib.request
    cursor, seen = "", 0
    while True:
        url = f"{API}/incentive_programs?status=active&limit=1000"
        if cursor:
            url += f"&cursor={cursor}"
        with urllib.request.urlopen(url, timeout=20) as r:
            page = json.load(r)
        programs = page.get("incentive_programs") or []
        seen += len(programs)
        for p in programs:
            if p["market_ticker"] == ticker:
                return p
        cursor = page.get("next_cursor") or ""
        if not cursor or not programs:
            break
    raise SystemExit(
        f"no active liquidity program for {ticker} (searched {seen})")


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__.split("\n")[0])
    ap.add_argument("--ticker", required=True)
    ap.add_argument("--size", type=int, default=50)
    ap.add_argument("--live", action="store_true",
                    help="transmit real orders (default is dry run)")
    ap.add_argument("--i-understand-this-is-real-money", dest="confirm",
                    action="store_true")
    ap.add_argument("--db", default=str(DB_PATH))
    a = ap.parse_args()

    if a.live and not a.confirm:
        print("--live also requires --i-understand-this-is-real-money",
              file=sys.stderr)
        return 2
    if SENTINEL.exists():
        print(f"sentinel {SENTINEL} exists; remove it to run", file=sys.stderr)
        return 2

    cfg = Config(ticker=a.ticker, size=a.size, dry_run=not a.live)
    program = fetch_program(a.ticker)
    conn = connect(Path(a.db))
    client = Client(dry_run=not a.live, live_ok=a.live,
                    logger=lambda m: print(m, flush=True))
    probe = Probe(cfg, program, conn, client)

    loop = asyncio.new_event_loop()
    asyncio.set_event_loop(loop)

    # Request a stop rather than setting `halted` directly: the control loop
    # must reach `halt()` so orders are actually cancelled. Setting `halted`
    # here would break every loop and exit with orders still resting.
    def _sig(*_):
        probe.stop_requested = True
    for s in (signal.SIGINT, signal.SIGTERM):
        signal.signal(s, _sig)

    try:
        loop.run_until_complete(probe.run())
    finally:
        conn.commit()
        conn.close()
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
