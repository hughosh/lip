#!/usr/bin/env python3
"""Kalshi Liquidity Incentive Program per-snapshot scoring, July 30, 2026 terms, next to the
February 2026 rule for comparison.

Source: "Liquidity Incentive Program - July 30, 2026 Modification to July 15, 2026 Update"
(saved as notes/revival-evidence-2026-09-26/terms-july30.pdf, read raw on 2026-10-01).

July 30 rule, one side (the no side is the same with no bids):
  walk the bids from the highest price down, accumulating size and collecting levels;
  the first level at which cumulative size >= target/5 becomes the Reference Price;
  stop at the first level where cumulative size >= target (that level is included);
  if the bids run out first, the side has NO qualifying bids (snapshot excluded).
  score(bid) = DF ** max(reference - price, 0) * size, normalised within the side.
  A user's snapshot score = normalised yes score + normalised no score (2.0 per snapshot).
  Period score = sum over snapshots / sum over snapshots and users; payout = period score
  * reward * (non-excluded snapshots / total snapshots), paid only if >= $1.00, floored to cents.

February rule (frozen core/score.py): identical walk but the Reference Price is the highest
bid, so every level is at or below it and N = reference - price.

Distances are in CENTS. Sub-cent price ladders exist on some markets below $0.05; there a
cent is more than one tick, so this understates N for those levels, overstates the field
score and understates our share (conservative for us).

Pure functions; no I/O, no network.
"""
from typing import Iterable, List, Optional, Tuple

Level = Tuple[float, float]  # (price_cents, size)


def levels_from_dollars(rows: Iterable) -> List[Level]:
    """[["0.2000","60.00"], ...] -> [(20.0, 60.0), ...] sorted best (highest) first."""
    out = {}
    for p, s in rows:
        c = round(float(p) * 100, 4)
        out[c] = out.get(c, 0.0) + float(s)
    return sorted(out.items(), key=lambda kv: -kv[0])


def join(levels: List[Level], price: float, size: float) -> List[Level]:
    d = dict(levels)
    d[price] = d.get(price, 0.0) + size
    return sorted(d.items(), key=lambda kv: -kv[0])


def walk(levels: List[Level], target: float, rule: str = "jul30"):
    """Return (reference, qualifying_levels, cumulative). reference None = side cleared."""
    cum = 0.0
    ref: Optional[float] = None
    qual: List[Level] = []
    for p, s in levels:
        cum += s
        qual.append((p, s))
        if ref is None:
            if rule == "feb" or cum >= target / 5.0:
                ref = p
        if cum >= target:
            return ref, qual, cum
    return None, [], cum


def side_total(qual: List[Level], ref: float, df: float) -> float:
    return sum(s * df ** max(ref - p, 0.0) for p, s in qual)


def side_share(levels: List[Level], target: float, df: float, rule: str,
               price: float, size: float):
    """Our normalised share on one side after resting `size` at `price`.
    Returns (share, reference, field_total_without_us, qualifies)."""
    ref, qual, _ = walk(join(levels, price, size), target, rule)
    if ref is None:
        return 0.0, None, 0.0, False
    tot = side_total(qual, ref, df)
    inq = any(abs(p - price) < 1e-9 for p, _ in qual)
    ours = size * df ** max(ref - price, 0.0) if inq else 0.0
    return (ours / tot if tot > 0 else 0.0), ref, tot - ours, True


def snapshot(yes: List[Level], no: List[Level], target: float, df: float, rule: str,
             size: float, where: str = "touch"):
    """Both sides joined at `where`: 'touch' = the best bid, 'ref' = the July 30 reference level
    (the Feb reference is the touch). Returns a dict with per-side detail and the combined
    snapshot share (ours / 2.0), plus the eligibility flags with and without us."""
    out = {"rule": rule, "size": size, "where": where}
    elig_wo = walk(yes, target, rule)[0] is not None and walk(no, target, rule)[0] is not None
    out["eligible_without_us"] = elig_wo
    tot_share = 0.0
    both = True
    for name, lv in (("yes", yes), ("no", no)):
        if not lv:
            out[name] = {"price": None, "share": 0.0, "ref": None, "qualifies": False}
            both = False
            continue
        touch = lv[0][0]
        if where == "ref":
            r0 = walk(lv, target, rule)[0]
            price = r0 if r0 is not None else touch
        else:
            price = touch
        sh, ref, field, q = side_share(lv, target, df, rule, price, size)
        out[name] = {"price": price, "share": sh, "ref": ref, "field": field, "qualifies": q,
                     "touch": touch}
        both = both and q
        tot_share += sh
    out["eligible_with_us"] = both
    out["snapshot_share"] = (tot_share / 2.0) if both else 0.0
    out["collateral"] = size * ((out["yes"]["price"] or 0) + (out["no"]["price"] or 0)) / 100.0
    return out


if __name__ == "__main__":
    # Self-check against notes/revival-evidence-2026-09-26/quote-counterfactual.json
    import json, sys
    book = json.load(open(sys.argv[1]))["orderbook_fp"]
    yes = levels_from_dollars(book["yes_dollars"])
    no = levels_from_dollars(book["no_dollars"])
    for where in ("touch",):
        r = snapshot(yes, no, 300, 0.5, "jul30", 12, where)
        print(json.dumps(r, indent=1))
    # one tick below reference
    sh_y = side_share(yes, 300, 0.5, "jul30", yes[0][0] - 1, 12)
    sh_n = side_share(no, 300, 0.5, "jul30", no[0][0] - 1, 12)
    print("one tick below:", sh_y, sh_n, "period share", (sh_y[0] + sh_n[0]) / 2)
