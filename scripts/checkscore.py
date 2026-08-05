#!/usr/bin/env python3
"""Differential test for probescore.py, against two independent oracles.

Case A -- the frozen `score.py`. Covers our size resting AT the reference
price (N = 0), which is the only geometry score.py can express.

Case B -- an independently formulated oracle in this file. Case A alone is not
sufficient and was observed not to be: with our order always at the touch,
df**(ref - our_price) is always df**0 == 1, so a sign flip in the exponent is
invisible, and membership of the qualifying walk is true by construction, so
dropping the membership test is invisible too. Both mutations passed a Case-A-
only suite. The probe rests below the touch on every requote lag (Q4.1), so
that path carries real weight and needs a real oracle.

`oracle_side` is deliberately written in a DIFFERENT shape from probescore's:
membership in the qualifying set is a declarative predicate over cumulative
depth ("is the size strictly above this price less than Target Size?") rather
than an iterative accumulate-and-break walk. A bug shared by both formulations
is correspondingly less likely.

    python scripts/checkscore.py
    python scripts/checkscore.py --mutate df-exponent

Every mutation must be CAUGHT. A mutation that slips through is a failing test
suite, not a passing one, and is reported as such.
"""
from __future__ import annotations

import argparse
import random
import sys

sys.path.insert(0, "/Users/hugh/kek/lip")

import probescore
import score

MUTATIONS = {
    "df-exponent": "score our bid as df**(our_price - ref) -- sign flipped",
    "no-clear": "treat an under-target book as qualifying instead of clearing it",
    "sum-not-mean": "sum the two side shares instead of averaging them",
    "ignore-depth": "credit our size even when it rests outside the qualifying walk",
    "off-by-one": "walk one level too far down the book",
}


# --------------------------------------------------------------- oracle (B)


def oracle_side(levels, target, df, our_size, our_price):
    """Independent implementation of one side's share, declarative in form."""
    if not levels:
        return 0.0
    ref = max(p for p, _ in levels)
    if ref >= 100:
        return 0.0
    if sum(s for _, s in levels) < target:
        return 0.0                      # never reaches Target Size -> cleared

    def in_qual(price):
        """A level qualifies iff the depth strictly above it is under target:
        the walk had not yet met Target Size when it arrived at this price."""
        return sum(s for q, s in levels if q > price) < target

    total = sum(df ** (ref - p) * s for p, s in levels if in_qual(p))
    if not total or our_price is None or our_size <= 0:
        return 0.0
    if not any(p == our_price for p, _ in levels) or not in_qual(our_price):
        return 0.0
    return (df ** (ref - our_price) * our_size) / total


def oracle_combined(yes, no, target, df, ys, yp, ns, np_):
    """Gated snapshot share: both sides must reach Target Size or nothing counts."""
    def reaches(levels):
        return bool(levels) and max(p for p, _ in levels) < 100 \
            and sum(s for _, s in levels) >= target
    if not (reaches(yes) and reaches(no)):
        return 0.0
    return (oracle_side(yes, target, df, ys, yp)
            + oracle_side(no, target, df, ns, np_)) / 2.0


# ------------------------------------------------------------- mutations


def apply_mutation(name: str) -> None:
    if name == "df-exponent":
        def our_side_share(levels, target, df, our_size, our_price):
            levels = probescore.descending(levels)
            ref, qual = probescore.qualify(levels, target)
            if ref is None or our_price is None or our_size <= 0:
                return 0.0, {"ref": ref}
            total = sum(df ** (ref - p) * s for p, s in qual)
            if not total or not any(p == our_price for p, _ in qual):
                return 0.0, {"ref": ref}
            return (df ** (our_price - ref) * our_size) / total, {"ref": ref}
        probescore.our_side_share = our_side_share

    elif name == "no-clear":
        def qualify(levels, target):
            if not levels or levels[0][0] >= 100:
                return None, []
            ref = levels[0][0]
            total, qual = 0.0, []
            for price, size in levels:
                total += size
                qual.append((price, size))
                if total >= target:
                    break
            return ref, qual
        probescore.qualify = qualify

    elif name == "sum-not-mean":
        _orig = probescore.combined_share

        def combined_share(*a, **k):
            share, detail = _orig(*a, **k)
            return share * 2.0, detail
        probescore.combined_share = combined_share

    elif name == "ignore-depth":
        def our_side_share(levels, target, df, our_size, our_price):
            levels = probescore.descending(levels)
            ref, qual = probescore.qualify(levels, target)
            if ref is None or our_price is None or our_size <= 0:
                return 0.0, {"ref": ref}
            total = sum(df ** (ref - p) * s for p, s in qual)
            if not total:
                return 0.0, {"ref": ref}
            return (df ** (ref - our_price) * our_size) / total, {"ref": ref}
        probescore.our_side_share = our_side_share

    elif name == "off-by-one":
        def qualify(levels, target):
            if not levels or levels[0][0] >= 100:
                return None, []
            ref = levels[0][0]
            total, qual = 0.0, []
            for price, size in levels:
                qual.append((price, size))
                if total >= target:          # tested BEFORE adding -- one too far
                    return ref, qual
                total += size
            return None, []
        probescore.qualify = qualify

    else:
        raise SystemExit(f"unknown mutation {name!r}")


# ------------------------------------------------------------- generators


def random_book(rng):
    top = rng.randint(5, 95)
    out, price = [], top
    for _ in range(rng.randint(1, 8)):
        if price < 1:
            break
        out.append((price, round(rng.uniform(0.5, 900.0), 2)))
        price -= rng.randint(1, 4)
    return out


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--mutate", choices=sorted(MUTATIONS), default=None)
    ap.add_argument("--n", type=int, default=4000)
    ap.add_argument("--seed", type=int, default=20260725)
    a = ap.parse_args()

    if a.mutate:
        print(f"MUTATION ACTIVE: {a.mutate} -- {MUTATIONS[a.mutate]}")
        apply_mutation(a.mutate)

    rng = random.Random(a.seed)
    a_checked = a_fail = degenerate = 0
    b_checked = b_fail = b_below = b_outside = 0
    worst_a = worst_b = 0.0
    shown = 0

    for _ in range(a.n):
        yes = probescore.descending(random_book(rng))
        no = probescore.descending(random_book(rng))
        target = rng.choice([300.0, 1000.0, 50.0])
        df = rng.choice([0.5, 0.9, 0.25])
        size = rng.choice([20.0, 50.0, 100.0])

        # ---- Case A: joined at the touch, checked against frozen score.py
        jy = score.join(list(yes), size)
        jn = score.join(list(no), size)
        ry, sy = score.side_score(jy, target, df)
        rn, sn = score.side_score(jn, target, df)
        if ry is None or rn is None:
            ref_share = 0.0
            degenerate += 1
        else:
            ref_share = ((size / sy if sy else 0.0) + (size / sn if sn else 0.0)) / 2.0
        got, _ = probescore.combined_share(
            jy, jn, target, df, size, jy[0][0] if jy else None,
            size, jn[0][0] if jn else None)
        a_checked += 1
        worst_a = max(worst_a, abs(got - ref_share))
        if abs(got - ref_share) > 1e-12:
            a_fail += 1
            if shown < 3:
                shown += 1
                print(f"  [A] MISMATCH score.py={ref_share!r} probe={got!r} "
                      f"target={target} df={df} size={size}")

        # ---- Case B: our size resting at an ARBITRARY level, incl. below the
        # touch and outside the qualifying walk. This is the path Case A cannot
        # reach, and the one the bot occupies whenever a requote is pending.
        if yes and no:
            yi = rng.randrange(len(yes))
            ni = rng.randrange(len(no))
            byes = list(yes)
            bno = list(no)
            byes[yi] = (byes[yi][0], byes[yi][1] + size)
            bno[ni] = (bno[ni][0], bno[ni][1] + size)
            yp, np_ = byes[yi][0], bno[ni][0]
            if yi > 0 or ni > 0:
                b_below += 1

            exp = oracle_combined(byes, bno, target, df, size, yp, size, np_)
            act, det = probescore.combined_share(
                byes, bno, target, df, size, yp, size, np_)
            if det.get("gated") and not det["yes"].get("in_qualifying", True):
                b_outside += 1
            b_checked += 1
            worst_b = max(worst_b, abs(act - exp))
            if abs(act - exp) > 1e-12:
                b_fail += 1
                if shown < 6:
                    shown += 1
                    print(f"  [B] MISMATCH oracle={exp!r} probe={act!r} "
                          f"target={target} df={df} size={size} "
                          f"yes_at={yp} (ref {byes[0][0]}) no_at={np_}")

    print(f"[A] vs frozen score.py : {a_checked} books "
          f"({degenerate} non-qualifying), max |delta| {worst_a:.3e}, {a_fail} fail")
    print(f"[B] vs independent oracle: {b_checked} books "
          f"({b_below} with our order BELOW the touch, "
          f"{b_outside} resting outside the qualifying walk), "
          f"max |delta| {worst_b:.3e}, {b_fail} fail")

    fails = a_fail + b_fail
    if a.mutate:
        if fails:
            where = []
            if a_fail:
                where.append(f"A:{a_fail}")
            if b_fail:
                where.append(f"B:{b_fail}")
            print(f"PASS (negative control): {a.mutate!r} CAUGHT ({', '.join(where)}).")
            return 0
        print(f"FAIL (negative control): {a.mutate!r} slipped through both oracles. "
              f"The suite does not discriminate and proves nothing.")
        return 1

    if fails:
        print(f"FAIL: {fails} disagreements.")
        return 1
    print("PASS: probescore agrees with both oracles on every case.")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
