#!/usr/bin/env python
"""Calibrate a predictive LIP revenue model against the one verified payout.

There is exactly one ground-truth observation: +$7.65 paid on
KXGENERICBALLOTVOTEHUB-26JUL31-T5.7, pool $1000, period 2026-07-24T16:01Z ->
2026-07-31T13:59Z, quoting 50 contracts/side with gaps.

Hypothesis:   reward = (time-weighted mean share over the WHOLE period) * pool
where share is what probe.db.snap already records each tick, and share is zero
during the gaps when nothing was resting.

If this reproduces $7.65 it is a usable forecasting model for sizing decisions.
"""
import sqlite3

PROBE = "file:/Users/hugh/kek/lip/probe.db?mode=ro"

POOL = 1000.0
PSTART = 1784908908945
PEND = 1785506340000
ACTUAL = 7.65

c = sqlite3.connect(PROBE, uri=True)

snaps = c.execute(
    """SELECT ts_ms, share, yes_share, no_share, our_yes_size, our_no_size,
              yes_total, no_total, gated
         FROM snap WHERE ts_ms BETWEEN ? AND ? ORDER BY ts_ms""",
    (PSTART, PEND)).fetchall()

period_h = (PEND - PSTART) / 3_600_000
print("=" * 92)
print("LIP REVENUE MODEL  vs  the one verified payout")
print("=" * 92)
print(f"  period      {period_h:.2f} h   pool ${POOL:.0f}   pool rate ${POOL/period_h:.3f}/h")
print(f"  snaps in period: {len(snaps)}")

# Integrate share over wall-clock time. Any wall-clock second with no snap is
# treated as share=0 (nothing resting) -- that is the whole point: downtime is
# a direct multiplicative loss of reward.
GAPCAP = 5_000  # ms; a gap longer than this counts as downtime, not as coverage
integral = 0.0
covered = 0
prev_ts = None
prev_share = 0.0
for ts, share, ys, ns, oy, on_, yt, nt, gated in snaps:
    if prev_ts is not None:
        dt = ts - prev_ts
        if dt <= GAPCAP:
            integral += prev_share * dt
            covered += dt
    prev_ts = ts
    prev_share = share or 0.0

twm_period = integral / (PEND - PSTART)
twm_uptime = integral / covered if covered else 0.0
uptime = covered / (PEND - PSTART)

print()
print(f"  covered wall-clock            {covered/3_600_000:8.2f} h  ({uptime*100:5.1f}% uptime)")
print(f"  time-weighted share WHILE UP  {twm_uptime:8.5f}  ({twm_uptime*100:.3f}%)")
print(f"  time-weighted share OVER PERIOD{twm_period:8.5f}")
print()
print(f"  PREDICTED reward = share_over_period * pool = ${twm_period*POOL:6.3f}")
print(f"  ACTUAL   reward                            = ${ACTUAL:6.3f}")
err = twm_period * POOL - ACTUAL
print(f"  error                                      = ${err:+6.3f}  ({err/ACTUAL*100:+.1f}%)")

print()
print("=" * 92)
print("FORECAST: what the same market pays at full uptime and larger size")
print("=" * 92)
# share scales as ours/(others+ours); recover the field's size from the snaps
fields = []
for ts, share, ys, ns, oy, on_, yt, nt, gated in snaps:
    if oy and yt and yt > oy:
        fields.append(yt - oy)
field = sorted(fields)[len(fields) // 2] if fields else float("nan")
print(f"  median external yes depth (field size): {field:,.0f} contracts")
print()
print(f"  {'our size/side':>14}{'implied share':>15}{'$/period':>11}{'$/day':>9}"
      f"{'collateral':>12}{'%/day on collat':>17}")
for sz in (25, 50, 100, 200, 400):
    sh = sz / (field + sz)
    per = sh * POOL
    # collateral: a resting yes bid at p and no bid at (1-p) both post full cost
    collat = sz * 1.00  # yes bid at p + no bid at ~1-p == ~$1.00 per contract-pair
    print(f"  {sz:>14}{sh:>15.5f}{per:>11.2f}{per/(period_h/24):>9.2f}"
          f"{collat:>12.2f}{per/(period_h/24)/collat*100:>16.2f}%")

print()
print("  NOTE: the two-sided collateral figure assumes quoting BOTH sides. Claim 7")
print("  (market-level gate) says a one-sided quoter still scores its own side, which")
print("  halves collateral per market at the cost of carrying directional inventory.")

print()
print("=" * 92)
print("BREAK-EVEN: how much trading damage the reward can absorb")
print("=" * 92)
print(f"  {'our size/side':>14}{'$/day reward':>14}{'crossing cost if we flatten':>30}")
print(f"  {'':>14}{'':>14}{'(2.4c/contract, per full turn)':>30}")
for sz in (25, 50, 100, 200):
    sh = sz / (field + sz)
    rev = sh * POOL / (period_h / 24)
    turns = rev / (2.4 * sz / 100.0)
    print(f"  {sz:>14}{rev:>14.2f}   {turns:>6.2f} full flattens/day before reward is gone")
print()
print("  The probe crossed 111 contracts in ~3 days of quoting at size 50:")
print("    crossing cost  ~$2.50 (measured: $0.57 spread + $1.93 fees)")
print(f"    reward earned  ${ACTUAL:.2f} over {period_h/24:.1f} days at {uptime*100:.0f}% uptime")
