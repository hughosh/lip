#!/usr/bin/env python3
"""Q8.2 predicted-share computation.

`score.py` answers "what share would I get if I JOINED this book?" — it takes a
book we are absent from and adds our size to the touch. The live probe needs the
other question: "what share am I getting RIGHT NOW?", against a book that
already contains our resting orders. Adding our size again would double-count.

`our_side_share` is the generalisation. It scores a stated quantity resting at a
stated price inside a book as observed. It reduces to `score.py`'s calculation
exactly when the resting quantity sits at the reference price, which is the
N = 0 case Q3.1 mandates — `scripts/checkscore.py` verifies that equivalence
against the frozen implementation rather than asserting it.

Everything here is pure: no clock, no I/O, no network. Prices are integer cents,
sizes are floats (Kalshi's `_fp` fields are fractional).
"""
from __future__ import annotations

Levels = list[tuple[int, float]]


def descending(levels: Levels) -> Levels:
    """Bids sorted best-first. The qualifying walk depends on this order."""
    return sorted(levels, key=lambda x: -x[0])


def qualify(levels: Levels, target: float) -> tuple[int | None, Levels]:
    """Walk down from the reference accumulating size until `target` is met.

    Mirrors score.qualify: a book that never reaches Target Size has its
    qualifying set CLEARED, so that side scores nothing at all. Returns
    (reference_price, qualifying_levels) or (None, []).
    """
    if not levels or levels[0][0] >= 100:
        return None, []
    ref = levels[0][0]
    total, qual = 0.0, []
    for price, size in levels:
        total += size
        qual.append((price, size))
        if total >= target:
            return ref, qual
    return None, []


def side_score(levels: Levels, target: float, df: float) -> tuple[int | None, float]:
    """(reference_price, total qualifying score) for one side."""
    ref, qual = qualify(levels, target)
    if ref is None:
        return None, 0.0
    return ref, sum(df ** (ref - p) * s for p, s in qual)


def our_side_share(levels: Levels, target: float, df: float,
                   our_size: float, our_price: int | None) -> tuple[float, dict]:
    """Our share of one side's qualifying score, book taken AS OBSERVED.

    `levels` must already include `our_size` at `our_price` — that is what the
    exchange sees and therefore what it scores. Returns (share, detail).

    Share is 0 when we rest below the qualifying set: the LIP rule credits only
    bids inside the walk, so depth further down earns nothing even though it is
    real liquidity.
    """
    levels = descending(levels)
    ref, qual = qualify(levels, target)
    detail = {"ref": ref, "qual_levels": len(qual), "our_price": our_price,
              "our_size": our_size, "in_qualifying": False,
              "our_score": 0.0, "total_score": 0.0, "ticks_back": None}
    if ref is None:
        return 0.0, detail

    total = sum(df ** (ref - p) * s for p, s in qual)
    detail["total_score"] = total
    if our_price is None or our_size <= 0 or total <= 0:
        return 0.0, detail

    if not any(p == our_price for p, _ in qual):
        return 0.0, detail          # resting outside the qualifying walk

    ticks = ref - our_price
    ours = df ** ticks * our_size
    detail.update(in_qualifying=True, our_score=ours, ticks_back=ticks)
    return ours / total, detail


def combined_share(yes: Levels, no: Levels, target: float, df: float,
                   our_yes_size: float, our_yes_price: int | None,
                   our_no_size: float, our_no_price: int | None
                   ) -> tuple[float, dict]:
    """Our share of a single snapshot, averaged across the two sides.

    Each side contributes 1.0 of a total 2.0, so the snapshot share is the mean
    of the two side shares — the same normalisation score.py applies.

    The snapshot is GATED: if either side fails to reach Target Size the whole
    snapshot is excluded for every participant ("Snapshots will be excluded if
    there is not two-sided liquidity"), so the share is 0 and, critically, the
    snapshot must not be counted in the denominator of our presence fraction
    either. `detail["gated"]` reports that distinction to the caller.
    """
    ys, yd = our_side_share(yes, target, df, our_yes_size, our_yes_price)
    ns, nd = our_side_share(no, target, df, our_no_size, our_no_price)
    gated = yd["ref"] is not None and nd["ref"] is not None
    share = ((ys + ns) / 2.0) if gated else 0.0
    return share, {"gated": gated, "yes": yd, "no": nd,
                   "yes_share": ys if gated else 0.0,
                   "no_share": ns if gated else 0.0}


def join(levels: Levels, size: float) -> Levels:
    """Add `size` at the current best price — score.py's `join`, for the
    hypothetical 'if I joined' calculation used at selection time."""
    levels = descending(levels)
    if not levels:
        return levels
    out = list(levels)
    out[0] = (out[0][0], out[0][1] + size)
    return out
