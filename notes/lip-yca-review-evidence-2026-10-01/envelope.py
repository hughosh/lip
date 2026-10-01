#!/usr/bin/env python3
"""Operating-envelope arithmetic for the lip-yca review (pure arithmetic, no I/O).

Payout rule (July 30, 2026 terms): payout = period_share * reward * eligible_fraction,
paid only if >= $1.00, floored to the cent. A quoter that is up for a fraction u of the
period, with snapshot share s while up, has period_share = s * u (the summed-score
normalisation integrates over all snapshots). Fees: taker 0.07*C*P*(1-P) rounded up to
the centi-cent, maker $0 on `quadratic` series (verified on the account's own fills).
"""
import math


def payout(share, pool, uptime=1.0, elig=1.0):
    x = share * pool * uptime * elig
    return math.floor(x * 100) / 100 if x >= 1.0 else 0.0


def hours_to_clear_floor(share, pool, period_hours, elig=1.0):
    """Hours of continuous presence needed for share*pool*(h/period)*elig >= $1."""
    if share * pool * elig <= 0:
        return float("inf")
    return period_hours / (share * pool * elig)


def taker_fee(contracts, price_cents):
    p = price_cents / 100.0
    return math.ceil(0.07 * contracts * p * (1 - p) * 10000) / 10000


def main():
    print("# A. Attended 2 h stage at S=12: LIP payout for the period (all $0 by the floor)")
    print(f"{'pool':>6} {'period':>7} {'share':>6} {'raw $':>8} {'paid $':>7}")
    for pool, days in ((100, 14), (100, 7), (100, 1), (200, 2), (1000, 7), (1000, 1)):
        for share in (0.013, 0.03, 0.10):
            u = 2 / (24 * days)
            raw = share * pool * u
            print(f"{pool:6d} {days:5d}d {share:6.3f} {raw:8.3f} {payout(share, pool, u):7.2f}")
    print()
    print("# B. Continuous presence needed to clear the $1 floor (hours), eligible fraction 1")
    print(f"{'pool':>6} {'period':>7} {'share':>6} {'hours':>8} {'share of period':>16}")
    for pool, days in ((100, 14), (100, 7), (100, 1), (200, 2), (1000, 7), (1000, 1)):
        for share in (0.013, 0.03, 0.10):
            h = hours_to_clear_floor(share, pool, 24 * days)
            print(f"{pool:6d} {days:5d}d {share:6.3f} {h:8.1f} {h/(24*days):16.1%}")
    print()
    print("# C. LIP $/day per market at full uptime, by share and pool/day")
    print(f"{'pool/day':>9} " + " ".join(f"{s:>7.1%}" for s in (0.013, 0.03, 0.10, 0.165)))
    for pd in (7.14, 14.3, 20, 50, 100, 143):
        print(f"{pd:9.1f} " + " ".join(f"{s*pd:7.2f}" for s in (0.013, 0.03, 0.10, 0.165)))
    print()
    print("# D. Round-trip cost per contract (cents) by exit style at price P")
    print(f"{'P':>4} {'taker fee':>10} {'taker exit (1c spread+fee)':>28} {'maker exit':>11}")
    for p in (3, 30, 45, 50, 65, 96):
        f = taker_fee(1, p) * 100
        print(f"{p:4d} {f:10.3f} {1 + f:28.3f} {'0 + markout':>11}")
    print()
    print("# E. Stage 1 taker exit check: 12 NO @ 96c -> fee", taker_fee(12, 96), "(evidence: $0.0323)")
    print("# F. Probe taker check: 23 YES-equiv @ 44c -> fee", taker_fee(23, 44), "(evidence: $0.3968)")
    print()
    print("# G. One market, S=12, continuous: LIP $/day vs trading $/day")
    print("     trading $/day = round trips/day * 12 contracts * net cents per contract")
    print(f"{'rt/day':>7} " + " ".join(f"{c:>+7.1f}c" for c in (-1.8, -0.5, 0.0, 1.0)))
    for rt in (1, 3, 6, 12):
        print(f"{rt:7d} " + " ".join(f"{rt*12*c/100:+8.2f}" for c in (-1.8, -0.5, 0.0, 1.0)))
    print("     LIP $/day at $100/14d: 1.3%% -> %.2f, 3%% -> %.2f, 10%% -> %.2f" % (0.013 * 100 / 14, 0.03 * 100 / 14, 0.10 * 100 / 14))


if __name__ == "__main__":
    main()
