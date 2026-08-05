# LIP re-evaluation — 2026-08-04

Adversarial re-derivation. Nothing here is inherited from `exploit-verdict.md`;
where the two disagree, this file states why.

## 0. What changed vs the prior verdict

| prior claim | status |
|---|---|
| C1 taker edge zero/negative | not re-tested (harness will never cross, so moot) |
| C2 maker free / taker `0.07·C·P·(1−P)` | **CONFIRMED** exactly, on the account's own `fee_cost` |
| C3 maker edge rises to +13.7c at depth | **~85% ARTIFACT** — see §3 |
| C4 queue position scarce | not re-tested |
| C5 latency not binding | accepted, not re-tested |
| C6 economics thin ($1.55/market-hour) | **WRONG SCALE** — see §5 |
| C7 two-sided gate is market-level | accepted; load-bearing for one-sided quoting |

## 1. The probe's loss, decomposed (task: "decide harness vs strategy")

`probe.db.our_fill` is **incomplete** — 8 rows against 12 real fills, and it
mis-attributes `run_id` because run 3 backfilled historical fills under its own
id. Rebuilt from `/portfolio/fills`:

    111 contracts bought MAKER, 111 sold TAKER, in TWO round trips (not one).

    maker edge at entry        +0.56
    drift while held           -7.38
    spread paid crossing out   -0.57
    taker fees                 -1.93      (100% of fees; maker fees were $0.00)
    ---------------------------------
    trading damage             -9.32
    LIP reward                 +7.65
    NET                        -1.67

Decomposition alone says "strategy 73% / harness 27%". **That reading is wrong.**
The timeline (`scripts/probetimeline.py`) shows why:

- The market sat at 57/58, unmoved, for **3.5 days with zero fills**.
- Within **2 seconds** of the third maker fill the book swept 57 → 56 → 55.
- `Q6.2-inventory` fired correctly 2.8s later at net +61 > 50.
- `halt()` cancels quotes but **never flattens**, and the snapshot loop `break`s
  in the same iteration — **2 snaps in the following 6.14 hours**.
- The position sat unmanaged and directional for 6.1h while the market fell to 49.
- Run 3 then re-entered at 49 and crossed out at 44 three minutes later.
- **The market settled YES at $1.00.** Holding all 111 contracts would have
  returned **+$51.73** instead of −$7.39.

So the drift was not adverse selection; it was an unmanaged directional hold
through a temporary dislocation. The fault is a **third thing** the prior
framing had no slot for: *halt-with-inventory*.

## 2. Never cross — derived from population data, not the n=1 anecdote

`scripts/holdvsflat.py`, settled tickers, SEs clustered by ticker.
E[settlement − mid_5m], i.e. hold minus stop-out, by drawdown bucket:

    favourable  -2.43±2.95   1-2c +0.67±2.34   3-5c -1.89±3.26
    6-10c       -2.53±3.49   11-20c +0.61±4.27  >=21c +4.89±9.02

**Every bucket is statistically zero.** The mid is a martingale; the 5m→30m
follow-on move is ~zero in every bucket too. Stopping out has *no expected
benefit*, while costing half-spread (mean quoted spread 2.13 ticks → ~1.07c)
plus taker fee (1.31–1.75c) = **~2.4–2.8c/contract with certainty**.

That predicts the probe's crossing cost at 111 × 2.4c ≈ $2.66; measured $2.50.

> **Design rule: the harness must never cross. Unwind by resting an offer
> (quote skew), not by taking.** With C7 (market-level gate) a one-sided quote
> still scores, so skewing to flat is free in reward terms.

## 3. Claim 3 is ~85% an artifact

`pre_mid` is the mid *before* the trade, so a fill N ticks behind the touch
mechanically implies `pre_mid − price ≈ N`. Size-weighted OLS slope of measured
edge on distance = **0.9940** (tautology ⇒ 1.0). At distance ≥12, mean distance
22.02 vs mean "edge" 25.91 — the 3.89c residual is exactly the half-spread.

Real forward markout (`mid_1m − price`):

    dist 0: +0.016±0.070    dist 4-7: +1.565±0.374***   dist >=12: +10.509±2.529***

Genuine depth edge exists but is ~40% of the claimed size, and is **zero at the
touch** — which is where LIP scoring requires you to be.

### 3a. Correction from adversarial review
At-touch "+0.016 ≈ no signal" is a **knife-edge cancellation**, not an absence:
+0.802c half-spread captured against −0.786c signed 1-min drift (−0.786±0.073,
t=−10.75). That is *complete adverse selection*. Also corrected: a filter on
`pre_best_*` dropped 35.8% of settled volume for no reason; corrected at-touch
settlement P&L is **−0.505±1.236** (was −1.748). Calibration is **not** as clean
as first reported — the 50–74c bucket is 62.4c priced vs 52.4% realised, and
per-leg errors of ±12–15c offset inside buckets. Volume-weighted mean
|miscalibration| = 2.96c. None of this changes the design rule in §2.

## 4. Revenue model — calibrated, ±1%

`reward = (time-weighted share integrated over the WHOLE period) × pool`

Against the one verified payout: predicted **$7.574** vs actual **$7.65**
(−1.0%), at 37.8% uptime and 2.001% share while up. **Downtime is a direct
multiplicative loss of reward.**

## 5. The opportunity is far larger than C6 claimed, and lives in THIN books

`/incentive_programs` needs **`status=active`** (`score.py:27`). Without it you
get a different, wrong set — this is what made C6 read "thin".

    174 live programs with books
    total pool  $15,955 per period   |   $5,426.55/day exchange-wide
    pool/program  $20 .. $135, median $80
    145/174 already meet target on both sides

DF=0.5 crushes everything more than a few ticks behind the touch, so a market's
**total qualifying score can be tiny even when its depth is large**. Audited
example, `KXUST10AD-26AUG05-T4.65` (pool $80, target 1000):

    yes: field qualifying score  34.41   (depth walks past 561 contracts)
    no : field qualifying score  17.30

400 contracts at the touch ⇒ share 0.92/0.96. The probe's market had a field
score of ~2,450 — **70× thicker**. It picked the wrong market.

### Modelled capture curve (`scripts/capacity.py`)
Two-sided at the touch, optimal greedy allocation, single book snapshot:

    capital    $/day    $/30d   markets
       $100   282       8,452     13
       $500   596      17,886     38
     $1,000   807      24,200     54
     $5,000 1,573      47,181     93
    $10,000 2,039      61,169    107
   $100,000 4,030     120,903    156

Saturating toward the $5,427/day ceiling.

**These are MODELLED, not verified.** Specific reasons they may not hold:
1. **Competitive response.** Field score is 34 because nobody is there. Our
   entry changes it; other farmers react. The model is a one-shot snapshot.
2. **Adverse selection at the touch in thin books is unmeasured.** §3a measured
   −0.79c/1min drift on the *rig* population; these books are far thinner.
3. Single snapshot — no time-integration of field size over the period.
4. Short-period programs (1 day) dominate the $/day ranking.
5. Nothing above deducts trading losses, fees, or downtime.

## 6. What this implies for the harness

- Never cross (§2). Inventory is managed by quote skew, one side at a time.
- Halt must **not** abandon inventory: stop *adding* risk, keep the *reducing*
  quote live, keep sampling, ping the operator. The probe did the opposite.
- Uptime is revenue (§4), so wedged-feed detection is a revenue feature, not
  just a safety one. **Neither** `probebot.py` **nor the Go rig has a read
  deadline or heartbeat** — a half-open socket hangs both indefinitely.
- Market *selection* dominates size (§5). Rank by pool ÷ field qualifying score.
