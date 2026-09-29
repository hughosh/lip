# Does resting at the touch make money?

> Historical analysis, reviewed 2026-09-26: the July 30, 2026 LIP terms changed
> reference-price construction and scale payouts by the non-excluded snapshot
> fraction. Sections 2–5 below use the earlier rules; in particular, the claim
> that excluded snapshots leave the reward pool intact is no longer current.
> These public-markout and static-book estimates do not establish private fills
> or a profitable strategy. See [the revival assessment](revival-2026-09-26.md)
> for current sources, fee checks, and the evidence still required.

Measured 2026-07-25 against `rig.db`: 40,392 fills / 91,309 reference rows /
153 markets / 6.5h. This is one session on one day, dominated by
KXTRUMPMENTION markets. Treat it as a first read, not a conclusion.

## 1. The naive report is wrong, and by a lot

`rig.py --report` says the trade-through population has POSITIVE markout:
+1.23c at 1m, +2.15c at 5m, +2.25c at 30m.

It is measuring at the **print** price. The trade-through flag is evidence about
the **touch**. Those are 4.70 ticks apart on average (n=11,427).

Re-measured at the touch — the price a resting quote would actually have had:

    horizon   at print (naive)   AT TOUCH (correct)
    1m            +1.228c            -1.771c
    5m            +2.149c            -0.733c
    30m           +2.253c            -0.320c

**Resting at the touch loses ~1.8c/contract at 1m.** That is adverse selection:
you get filled precisely when the market is moving against you.

This is not a new discovery, it is P19 / §6.2 — `rig.py`'s own docstring says
"analysis of 'would my quote at the touch have filled' must use the touch price
(pre_best_yes / pre_best_no), not the print price". The default report ignores
its own warning. Any future analysis must use the touch.

## 2. So the strategy only works if LIP rewards exceed adverse selection

They appear to, by a wide margin. At 100 contracts/side (`score.py --size 100`):

    active programs        200
    total pool             $14,775
    meet target both sides 171 of 200 (86%)
    median share captured  23.8%
    median return/period   12.9%
    best markets           50-81% return per period

Against ~1.8c/contract of adverse selection, the reward side dominates — IF the
quote stays at the reference.

## 3. The cadence is settled: 1 Hz, randomized sub-second offset

**RESOLVED 2026-07-25** from the primary source — Appendix A of
`rules02112639183.pdf`, KalshiEX self-certification filed with the CFTC
2026-02-11, effective 2026-02-28. Read directly from the PDF, not from a
summarizer:

> "During a Time Period, if the market is open for trading, a snapshot of the
>  book ("Snapshot") will be taken **once for each second**, with the exact time
>  of the snapshot drawn from a random uniform distribution *on a periodic
>  basis*."

The cover letter explains the italicised amendment: Kalshi "does not need to
generate a new snapshot time based on a new random uniform distribution for each
snapshot… while continuing to meet the purpose of that mechanism (to ensure that
the exact snapshot times will be nonpublic, fairly and randomly chosen, and
changed on a periodic basis)." So the offset is nonpublic and random but may be
reused across a stretch of seconds rather than redrawn every second.

Aggregation is sum-then-normalize, not an average:

    TimePeriodScore(user) = Σ_snap Score(user) / Σ_snap Σ_users Score(u)
    Payout(user) ≈ TimePeriodScore(user) × TimePeriodReward

paid only if ≥ $1.00, rounded down to the cent. Snapshots without two-sided
Target Size liquidity are excluded from numerator and denominator alike, so
they do not shrink the pool — they only concentrate it onto the qualifying
seconds.

Source: https://www.cftc.gov/sites/default/files/filings/orgrules/26/02/rules02112639183.pdf

⚠️ WebFetch's summarizer fabricated two different wrong answers for this
document on two separate calls ("every 10 seconds, averaged" and a garbled
variant) before the raw PDF was read. Do not re-derive this from a summary.

## 4. Which inverts the drift finding. Drift is NOT the binding constraint.

`discount_factor_bps = 5000`, so DF = 0.5 and each tick off the reference still
halves the score. But **one snapshot per second at a uniformly random offset
samples TIME uniformly**, so the weight on a book state is how long it persisted
— not that it happened. The earlier decay table was anchored at a reference move
(t=0 = a move just occurred), which is MOVE-weighted, and reference moves are
violently bursty:

    gap to next move    % of moves    % of TIME
    0 - 0.1s               52.6%          0.1%
    0.1 - 1s               24.4%          0.4%
    1 - 10s                15.3%          2.5%
    10 - 60s                5.3%          6.3%
    60 - 600s               2.1%         17.6%
    > 600s                  0.4%         73.1%

Half of all moves land within 100ms of the previous one and together they are
one part in a thousand of market time; 73% of market time is a stretch longer
than ten minutes with no reference change at all. Move-weighting overstates
decay by roughly an order of magnitude.

Re-measured time-weighted over gated instants — `scripts/timeweight.py`,
128 markets, exact step-function integration cross-checked against an
independent Monte Carlo (agrees to <0.5%):

    requote latency   score multiplier      (old move-weighted figure)
        10 ms              0.9999
        50 ms              0.9996
       200 ms              0.9988
         1 s               0.9962                    0.588
         5 s               0.9882                    0.530
        60 s               0.9471                    0.366
         5 m               0.8865                    0.248

At a 1-second requote loop the reference is unchanged at **99.0%** of gated
instants. Even a 60-second loop holds 86.2% and retains 95% of the ideal score.
A pessimistic bracket that also penalises being ABOVE the reference (`--abs`)
still gives 0.9937 at 1s and 0.9055 at 60s.

**The 43ms median gap was never the enemy.** It measures how fast the reference
flickers during bursts, and scoring never sees those bursts.

Latency still matters, but as a race against other participants for share, not
against decay: LIP normalises within each side, so you lose share only by being
slower than the others, which is why §10 ranks tick-to-quote above throughput.

## 5. What this says about Phase 2

Two of the three objections are gone. Restating the P&L honestly:

- **Reward** — median 23.8% share, 12.9% return per period at 100 contracts/side.
  Multiply by ~0.996 for a 1s loop, not the 0.59 assumed before. Essentially
  undiminished.
- **Adverse selection** — unchanged at ~-1.8c/contract (§1). Small in absolute
  terms against a $100 pool unless fill volume is high.
- **Gating** — 52.0% of market-time meets two-sided Target Size. This does *not*
  halve the payout (excluded snapshots leave the pool intact); it only matters
  through correlation between gating and your own presence.

What now binds, in order:

1. **Competition for share.** The 23.8% median assumes today's book. It is the
   one input that moves against us if other LIP bots join, and nothing in the
   public data reveals other participants' scores.
2. **Adverse selection on fills**, and inventory left after a fill breaks the
   two-sided quote.
3. **The $1.00 minimum payout** — with a $100 pool, share must clear 1%.

$100 of capital funds roughly one market at ~100 contracts/side. The cap is the
right size to test competition for share, which is now the live unknown.
