#!/usr/bin/env python3
"""Pick which LIP markets the quoting bot should sit in.

This is probe.py's screen turned into something importable, plus the two
filters that came out of the live probe:

  - Reject markets that do not already qualify on BOTH sides. A side whose
    book never reaches Target Size has its qualifying set CLEARED, so the
    snapshot scores nothing for anyone.
  - Require depth slack over Target Size. A book that only just clears the
    target can dip under it and void the snapshot for every participant,
    including us.

Deliberately absent: any filter on days-to-close. The risk that matters is
time-to-NEWS, not time-to-expiry — a market resolving in six months whose
news drops tomorrow is far more dangerous than a quiet one expiring next
week. Screening by expiry would have selected exactly the wrong markets.
"""
from __future__ import annotations

import sys
from dataclasses import dataclass
from pathlib import Path

sys.path.insert(0, str(Path(__file__).parent))

from score import book, programs, qualify


@dataclass
class Candidate:
    ticker: str
    program_id: str
    pool: float           # period reward, dollars
    target: float         # Target Size
    df: float             # discount factor, e.g. 0.5
    yes_bid: int          # best yes bid, cents
    no_bid: int           # best no bid, cents
    spread: int           # 100 - yes_bid - no_bid, in ticks
    mid: float            # implied mid, cents
    touch: float          # size at the thinner side's touch
    slack: float          # min(total depth) / target — cushion over the gate
    share: float          # our share of the pool if we join both touches
    cost: float           # capital to post `size` on both sides, dollars
    earn: float           # share * pool, dollars per period
    end_date: str

    @property
    def ret(self) -> float:
        """Return on deployed capital, percent per period."""
        return (self.earn / self.cost * 100.0) if self.cost else 0.0


def _share_if_we_join(levels: list, target: float, df: float, size: int) -> float:
    """Our normalized score share on one side after adding `size` at the touch.

    We sit AT the reference price, so our own discount exponent is 0 and our
    raw score is exactly `size`. The denominator is the whole side's score
    after our size is added.
    """
    joined = [(levels[0][0], levels[0][1] + size)] + list(levels[1:])
    ref, qual = qualify(joined, target)
    if ref is None:
        return 0.0
    denom = sum(df ** (ref - price) * sz for price, sz in qual)
    return (size / denom) if denom else 0.0


def evaluate(p: dict, size: int) -> Candidate | None:
    """Score a single program. Returns None if it is not quotable."""
    ticker = p["market_ticker"]
    target = float(p["target_size_fp"])
    df = p["discount_factor_bps"] / 10000.0

    try:
        yes, no = book(ticker)
    except Exception:
        return None
    if not yes or not no:
        return None

    # Both sides must already reach Target Size, else nobody scores.
    if qualify(yes, target)[0] is None or qualify(no, target)[0] is None:
        return None

    yes_bid, no_bid = yes[0][0], no[0][0]
    spread = 100 - yes_bid - no_bid
    if spread < 0:
        return None                      # crossed/locked book, not quotable

    share = (
        _share_if_we_join(yes, target, df, size)
        + _share_if_we_join(no, target, df, size)
    ) / 2.0
    pool = p["period_reward"] / 10000.0   # centi-cents -> dollars

    return Candidate(
        ticker=ticker,
        program_id=p["id"],
        pool=pool,
        target=target,
        df=df,
        yes_bid=yes_bid,
        no_bid=no_bid,
        spread=spread,
        mid=yes_bid + spread / 2.0,
        touch=min(yes[0][1], no[0][1]),
        slack=min(sum(s for _, s in yes), sum(s for _, s in no)) / target,
        share=share,
        cost=size * (yes_bid + no_bid) / 100.0,
        earn=pool * share,
        end_date=p.get("end_date") or "",
    )


def candidates(
    size: int = 20,
    budget: float = 20.0,
    min_slack: float = 1.3,
    mid_lo: float = 10.0,
    mid_hi: float = 90.0,
    min_spread: int = 1,
) -> list[Candidate]:
    """Quotable markets, best expected earnings first.

    Args:
        size: contracts we intend to rest on each side.
        budget: max capital for one market's two-sided quote, dollars.
        min_slack: required depth cushion as a multiple of Target Size.
        mid_lo/mid_hi: reject near-certain markets. They are capital-heavy
            (a 95c contract ties up 95c to earn the same tick) and one
            adverse resolution wipes out many periods of reward.
        min_spread: need at least this many ticks between the bids, or
            there is no room to rest both sides without self-crossing.
    """
    out = []
    for p in programs("active"):
        c = evaluate(p, size)
        if c is None:
            continue
        if c.cost > budget:
            continue
        if not (mid_lo <= c.mid <= mid_hi):
            continue
        if c.slack < min_slack:
            continue
        if c.spread < min_spread:
            continue
        out.append(c)
    out.sort(key=lambda c: -c.earn)
    return out


def main() -> int:
    import argparse

    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("--size", type=int, default=20, help="contracts per side")
    ap.add_argument("--budget", type=float, default=20.0, help="max $ per market")
    ap.add_argument("--slack", type=float, default=1.3, help="min depth / target")
    ap.add_argument("--top", type=int, default=15)
    a = ap.parse_args()

    rows = candidates(size=a.size, budget=a.budget, min_slack=a.slack)
    print(
        f"{len(rows)} quotable: qualify both sides, fit ${a.budget:.0f} at "
        f"{a.size}/side, mid {10}-{90}c, >={a.slack}x target depth\n"
    )
    hdr = (f"{'market':<36}{'pool$':>7}{'share':>7}{'earn$':>7}{'cost$':>7}"
           f"{'mid':>5}{'sprd':>5}{'touch':>7}{'slack':>6}{'ret%':>7}")
    print(hdr)
    print("-" * len(hdr))
    for c in rows[:a.top]:
        print(f"{c.ticker[:36]:<36}{c.pool:>7.0f}{c.share:>7.1%}{c.earn:>7.2f}"
              f"{c.cost:>7.2f}{c.mid:>5.0f}{c.spread:>5.0f}{c.touch:>7.0f}"
              f"{c.slack:>6.1f}{c.ret:>7.1f}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
