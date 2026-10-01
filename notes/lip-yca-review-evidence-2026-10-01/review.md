# LIP profitability under the July 30, 2026 terms — review for lip-yca (2026-10-01)

Scope: an evidence-backed verdict on whether Liquidity Incentive Program (LIP) market-making
with this harness is profitable now, under Kalshi's July 30, 2026 terms and current fees.
Everything here is either read from preserved evidence, re-run read-only on copies, or fetched
from public unauthenticated endpoints today. No order was placed, no key was armed, and no new
authenticated call was made: the last authenticated account read is the candidate-7 stage's
`accountcheck` at 2026-10-01T10:50Z and nothing has traded since.

Working files are in this directory (`TASKS.md`, `lip30score.py`, `sweep_books.py`,
`analyse_sweep.py`, `envelope.py`, `sweep/`). Nothing under `go/`, `scripts/` or the frozen
trees was changed.

## 0. Verdict

**Not profitable as operated, and not shown profitable as designed.**

- **Realized money.** The account is down **−$1.26 lifetime** on about $100: trading
  −$8.91 (of which fees −$1.96, all taker), LIP credits +$7.65. Every number in that chain is
  tied to an authenticated balance read (§2). The loss is one event, the July probe's
  halt-with-inventory; the four harness stages since 2026-09-29 netted **+$0.21** maker-only
  and earned **no LIP credit**.
- **Why the stages earned nothing.** Under the July 30 terms a market pays only if
  `share × reward × uptime × eligible-fraction ≥ $1.00` for the period. An attended 2-hour
  stage at S=12 in a $100 pool yields $0.01–0.06 before the floor, so **$0** (§5). This is not
  a measurement; it is the rule's arithmetic. Under the current operating envelope the LIP
  revenue line is identically zero.
- **What the terms changed.** The reference price moved from the top of the book to the
  depth level holding one fifth of the target, with full credit for everything at or above it;
  and the payout is now scaled by the non-excluded snapshot fraction (§1). The first dilutes a
  touch quote's share wherever the touch is thin; the second removes the "excluded snapshots
  leave the pool intact" property that the August thin-book thesis relied on.
- **The opportunity is larger than August said in dollars, and smaller in capture.** 8,135
  active liquidity programs with $937,745 of reward in flight (median $100 over 7 days), versus
  the 174-program, $15,955 figure in `reeval-verdict.md` §5, which was a single 200-row page.
  But a full public sweep scored under the July 30 rule gives 6–13× less modelled capture than
  the February rule on the same books: at S=12 the median eligible market pays $0.84 per period
  at full uptime, below the floor, and the three stage markets are at 1.1–1.2% share, not the
  3% assumed in `lip-89x`. Share and the $1 floor, not pool size, are what bind (§4).
- **Net EV at the current size is undetermined, and small either way.** At S=12 in a
  $100/14-day pool the LIP line is $0.09–0.21 per market-day at full uptime; the trading line
  from the measured fill cadence is ±$0.2–0.7 per market-day depending on markout, which no
  private data pins down (four round trips, hand-picked quiet books). The reward term is
  smaller than the trading noise. Nothing in hand establishes a positive expectation.
- **The only verified positive** remains the July $7.65: a $1,000 pool, 2.0% share at S=50,
  37.8% uptime, reproduced today by the revenue model to −1.0%.

What would change this is listed in §7; the decisive item is one continuous multi-day run
that clears the floor, which is exactly the North Star the harness work is aimed at.

## 1. What the July 30 terms say, and what each change does

Read raw from `notes/revival-evidence-2026-09-26/terms-july30.pdf` (tracked and clean
copies; 8 pages). The `revival-2026-09-26.md` paraphrase is accurate. Changes against the
February 2026 rule that `core/score.py` implements:

| # | July 30 text (paraphrased from the tracked copy) | Economic effect |
|---|---|---|
| 1 | Reference Yes Price = the first bid level, walking down, at which cumulative size ≥ **one fifth of Target Size**. The walk continues until cumulative ≥ Target Size; that level is included. If bids run out, the side has no qualifying bids. | Where the touch holds < target/5, the reference sits deeper. Everything at or above it scores at full weight, so a touch quote no longer out-scores a quote resting at the reference. Field score rises; our share at the touch falls (§4 quantifies). |
| 2 | `Score = DF^max(reference − price, 0) × size`. | A bid better than the reference is never penalised; improving the touch earns nothing extra. |
| 3 | Snapshot excluded if the market is not open for trading **or** either side lacks resting depth ≥ Target Size. | Same gate as before, now explicit about closed markets. |
| 4 | **Payout ≈ period score × reward × (non-excluded snapshots ÷ total snapshots)**, paid only if ≥ $1.00, floored to the cent. | Excluded time now shrinks the pool. Under the February rule excluded snapshots concentrated the pool on the remaining ones (`economics.md` header now says so). Thin books that flicker in and out of eligibility pay proportionally less. |
| 5 | Time Period Reward: minimum **$1** (was $10), maximum $1,000 per calendar day, per market. | Explains the $20–$60 pools in today's universe. |
| 6 | Eligible participants: all members except IB/FCM and their customers (affiliate and market-maker-agreement exclusions deleted). | Potentially more competition for share. |
| 7 | Program runs to the earlier of 2027-01-01 or amendment/termination. | Three months of runway, revocable. |

Unchanged: one snapshot per second at a nonpublic random offset; per-side normalisation; a
user's snapshot score is the sum of both sides (2.0 per eligible snapshot across all users);
period score = summed snapshot scores ÷ everyone's summed snapshot scores.

Because the period normalisation sums over non-excluded snapshots and the payout then
multiplies by the non-excluded fraction, the payout equals the average over **all** seconds
of the period of (our share that second, zero when we are absent or the market is excluded)
× reward. That is exactly the "whole period" form `scripts/revmodel.py` uses, which is why it
calibrates to the July payout even though the ballot period straddled the change-over.

## 2. Realized money to date

All amounts in dollars. "Verified" means an authenticated GET at the time, preserved in the
cited file. The fills-based arithmetic was previously flagged as not a cash ledger (bead
notes, `economics/review.md`); the balance identity below closes that gap.

| When (UTC) | Item | Amount | Source | Status |
|---|---|---|---|---|
| 2026-07-24 | KXHOODA 20 YES / 20 NO maker pairs | +0.20 | `positions-pnl.json` `realized_pnl_dollars` | verified |
| 2026-07-25T04:19 | Balance before any ballot fill | 100.25 | `probe.db` `balance_poll` first row (immutable read) | verified balance; the deposit itself is not in any ledger here (implied $100.05) |
| 2026-07-28/29 | KXGENERICBALLOTVOTEHUB-26JUL31-T5.7, 111 maker buys at 57/49c, 111 taker sells at 44–49c (NO side) | −7.39 price, −1.9298 fees = **−9.3198** | `account-read.json` 12 fills + 1 settlement | arithmetic, corroborated below |
| 2026-07-31 | LIP credit, event KXGENERICBALLOTVOTEHUB-26JUL31 | +7.65 | browser Rewards popover (`browser-rewards-alarm.md`) | UI only, corroborated below |
| 2026-09-27T00:20 | Balance | 98.5802 | `historical-cashflow.md` | verified |
| 2026-09-29 | Stage 1 KXAAAGASW: NO 12 @ 97c maker, manual taker exit NO 12 @ 96c, fee 0.0323 | −0.1523 | `account-preflight.json` 98.5802 → `account-after-drain.json` 98.4279 | verified |
| 2026-09-29 | Stage 2 KXEPLRELEGATION-27-MCI: NO 12 @ 65c, YES 12 @ 34c, both maker | +0.12 | 98.4279 → 98.5479 | verified |
| 2026-09-30 | Stage 3 KXDANCINGWITHTHESTARS-26DEC31-EFRE: NO 12 @ 55c, YES 12 @ 45c in three maker fills | 0.00 | 98.5479 → 98.5479 | verified |
| 2026-10-01 | Stage 4 MCI: NO 12 @ 68c, YES 12 @ 30c in two maker fills (7 h 18 min to complete) | +0.24 | 98.5479 → 98.7879 (`account-after-restart.json`, 10:50Z) | verified |

**Balance identity.** 100.25 − 9.3198 + 7.65 = **98.5802**, the Sep 27 balance to the cent.
Two numbers that were each uncorroborated (the paired-fill arithmetic and the UI credit) are
jointly pinned by an authenticated balance delta over a window with no other activity. Then the
four stage deltas reproduce the fills exactly. Lifetime from the implied deposit:

    trading   +0.20 − 9.3198 + 0.2077 = −8.9121   (fees −1.9621, all taker)
    LIP                               = +7.65
    net                               = −1.2621   (98.7879 − 100.05)

No LIP credit for any September or October market can exist yet: the three stage programs end
2026-10-03 (MCI), 10-05 (KXAAAGASW) and 10-12 (EFRE), and §5 predicts $0 for each.

Kalshi exposes no reward or payout endpoint (the repo's 2026-07-25 probes found ledger,
transactions and per-program payout routes 404; delegate sweep today found nothing newer), so
the only reward evidence available is balance deltas and the browser popover.

## 3. The edge, decomposed

**Reward.** `reward = (time-weighted share while up) × uptime × eligible fraction × pool`.
Re-run today on a scratch copy of `probe.db` (`scripts/revmodel.py` unchanged):

    period 165.95 h, pool $1,000, 225,725 snapshots
    covered 62.80 h (37.8% uptime); share while up 2.001% at S=50 against a ~2,100-contract field
    predicted $7.574 vs actual $7.65 (−1.0%)

One observation, but an exact one; it is the calibration anchor for everything below.

**Adverse selection.** No private measurement exists with statistical weight:

- Public July 25 data at the touch (`economics.md` §1, `rig.db`): trade-through markout
  −1.77c at 1 min, −0.73c at 5 min, −0.32c at 30 min. The `--report` figures are at the print
  price and wrong (memory `lip-economics-verdict`).
- Settlement-anchored at-touch P&L (`reeval-verdict.md` §3a): −0.505 ± 1.236c, i.e. not
  distinguishable from zero; 1-min at-touch markout +0.016c = +0.80c half-spread captured
  against −0.79c signed drift.
- Private: four harness round trips (stages 2–4 maker/maker: +1c, 0c, +2c per contract;
  stage 1 maker/manual-taker: −1.27c). Hand-picked quiet books, S=12, n=4. Directionally the
  maker-exit reducer captured the spread when it filled, but it took 4.4 min, 99 min and
  7 h 18 min to fill, and in stage 1 it never filled in 15 min.

**Fees.** Taker `0.07 × C × P × (1−P)`, rounded up to the centi-cent; maker $0 on `quadratic`
series. Verified on the account's own fills: $0.0323 on 12 @ 96c (stage 1), $0.3968 on
23 @ 44c (probe); every maker fill $0.000000. All series the account has traded, and the three
stage series, are `quadratic`, multiplier 1 (public `/series` endpoint, 2026-10-01). The fee
page and PDF at kalshi.com answer 429 to non-browser clients; the help article defers to the
PDF. The maker coefficient on `quadratic_with_maker_fees` series (e.g. KXATPMATCH) is not
verified here and is out of scope. Fees were 100% of the probe's fee cost because it crossed to
exit; the harness never crosses by design (spec §6.4), and the one stage taker exit was manual.

**Downtime and the floor.** Reward is proportional to uptime (calibrated), and the $1.00
floor applies per market per period. `envelope.py` tables A and B:

    attended 2 h stage, S=12:  $100/14d at 3% share → $0.018 → paid $0
                               $100/1d  at 10%      → $0.833 → paid $0
                               $1,000/7d at 10%     → $1.19  → paid $1.19
    continuous hours to clear $1: $100/14d at 3% → 112 h (33% of the period)
                                  $100/7d  at 3% → 56 h;  $200/2d at 3% → 8 h
                                  $1,000/7d at 3% → 5.6 h

**Competition.** The probe held 2.0% at S=50 in a thick book. The stage markets' shares at
S=12 under the July 30 rule are in §4 (fresh books today). No data on how the field responds to
our presence; the August capacity curve assumed none.

## 4. The opportunity today, scored under the July 30 rule (MODELLED)

A full public sweep: every market in the 2026-10-01T20:02Z program listing, one orderbook
each (`depth=100`), fetched 20:54–21:06Z in three throttled shards, all 8,135 HTTP 200, then
one `/markets?tickers=` status pass. Scored with `lip30score.py` by joining the touch (what
the harness does) at S=12 and S=100 under both rules. Files: `sweep/books.jsonl.gz`,
`sweep/markets.jsonl.gz`, `sweep-summary.json`, `sweep-summary-rows.json.gz`,
`sweep-summary-cuts.json`. Everything in this section is a one-shot snapshot of the field: no
competitive response, no fills, no fees, no downtime, no eligibility time series. It prices a
resting quote's share of the reward, not profit.

**Universe and eligibility at the instant.**

    programs 8,135   closed/finalized/determined 68   one side empty 778   scored 7,289
    eligible now (both sides ≥ target) 7,163 = 88% of programs, 98% of two-sided books
    price ladders: 7,876 linear cent; 259 sub-cent (scorer distances in cents: conservative)
    reward in eligible markets $847,685 per period, $251,980 per day

The 778 one-sided books and the 68 closed markets are the excluded snapshots of rule #4; the
August collector's 85% mean eligibility (`lip.db`, 5-min cadence) is the same picture over
time.

**What the reference rule does.** The reference sits below the touch on 25% of sides
(p90 gap 3–4c). For the median market nothing changes (field score ratio July 30 ÷ February
= 1.00 at p50) but at p90 the field score is 2.66× larger, i.e. exactly the thin-touch books
the August thesis targeted now give the touch quote a 2–3× smaller share. Field score without
us: p10 1,065, p50 3,972, p90 20,164 contract-equivalents across both sides; against a
target of 1,000 per side this is why S=12 is structurally small.

**Share and reward at the touch, all eligible markets, full uptime, July 30 rule.**

    S=12 : share p10/p50/p90  0.18% / 0.82% / 2.80%
           $/day p50 $0.15;  $/period p50 $0.84  → the MEDIAN market never pays at S=12
           clears $1 at 100% uptime: 3,106 of 7,163 (43%); at 50% uptime: 1,318 (18%)
    S=100: share p10/p50/p90  1.48% / 7.03% / 24.3%
           $/day p50 $1.34;  $/period p50 $7.36
           clears $1 at 100% uptime: 6,859 (96%); at 50%: 6,167
    February rule, same books, S=12: share p50 1.08%, p90 10.2%; S=100: p50 8.3%, p90 41%

Resting at the July 30 reference instead of the touch (`jul30_ref_*`) changes none of this
(share p50 0.82% vs 0.82%): under the new rule the two positions score the same, and the
reference is the touch in 75% of markets anyway.

**The stage markets today, S=12 at the touch, July 30 rule.**

| market | pool / period | share | $/day | $/period at 100% uptime |
|---|---|---|---|---|
| KXAAAGASW-26OCT05-4.4200 | $100 / 6.6 d | 1.09% (Feb rule 1.55%) | $0.17 | $1.09 |
| KXDANCINGWITHTHESTARS-26DEC31-EFRE | $100 / 14 d | 1.06% | $0.08 | $1.06 |
| KXEPLRELEGATION-27-MCI | $200 / 7.3 d | 1.20% (Feb 1.52%) | $0.33 | $2.40 |

`lip-89x`'s "about 3% at S=12" was 2.5–3× optimistic; each of these clears the floor only
at near-100% uptime for the whole period, and pays $1–2.40 if it does.

**Capacity curves (greedy by $/day per $ of collateral, fixed S per market, floor respected).**
The all-periods version is dominated by 90-minute table-tennis and 3-hour earnings-mention
programs whose "$/day" assumes continuous re-entry the harness cannot do, and the
per-collateral ranking exalts degenerate books bid at 1c on both sides. The honest cut is
programs with periods ≥ 1 day, ≥ 0.5 day remaining, both touches within 10–90c (3,059
programs, 3,023 eligible):

    capital       Feb rule S=12   July 30 S=12   July 30 S=100      (modelled $/day)
      $100            613              46            137   (1 market)
      $500          1,545             115            252
    $1,000          2,032             173            373
    $5,000          3,492             456          1,068
   $10,000          4,043             670          1,644
  $100,000          4,517             870          6,549
  markets in the $100k allocation:   2,182         1,843          1,144

Across that cut at S=12: share p50 1.29%, $/day p50 $0.23, $/period p50 $1.30; 1,843 of 3,023
markets clear the floor at full uptime, 923 at half. Quoting every one of them at S=12 would
need ~3,000 simultaneous markets and ~$36k of collateral for ~$1,000/day.

Read against `reeval-verdict.md` §5 ($282/day at $100, $4,030/day at $100k, Feb rule, 174
programs): the same method on today's full universe gives 6–13× **less** under July 30 than
under the February rule at every capital level, because the reference change removes most of
the thin-touch capture that curve was made of. What remains is real reward flow but with the
same five caveats as before, plus one new one: the 25% of sides where the reference is below
the touch are precisely the books whose depth is thinnest and whose eligibility is most
intermittent, and rule #4 now scales their pool down by that intermittency.

**Where the modelled money sits.** The top picks in the ≥ 1-day cut are $500–$1,000
one-to-two-day pools (e.g. KXTRUMPAPPROVE-26OCT02-U38.5: $600 over 1 day, 3.4% share at S=12,
$20.66/day on $11.88 of collateral) and $100 pools on quiet books. The probe's $7.65 was the
same mechanics at 2% of a $1,000/7-day pool; such pools are scarce (37 at $1,000, 139 at $500
today) and are where competition for share, not pool size, will decide the outcome.

## 5. The harness's operating envelope against the rule

The harness as qualified runs one market, S=12, `inv_hard` 7, maker-only, in attended stages
of about two hours, in markets chosen for quiet books (`lip-89x`). Against the terms:

1. **LIP revenue is $0 by construction** (§3 floor table). No attended stage in a $100–$200
   pool can clear the floor at any realistic share. The stages were correctly treated as
   harness qualification, not earning; the review simply makes the zero explicit.
2. **Continuous operation is the precondition for any reward.** At S=12 and 3% share a
   $100/14-day market needs 112 h of presence to pay $1, and pays $3 for the full period
   ($0.21/day). The $1,000-pool markets (37 today) pay $1 after 5.6 h at 3%.
3. **Per-market EV at S=12 is dominated by fills, not reward** (`envelope.py` G). With the
   stage cadence of roughly one owned fill per 2–7 h, 1–6 round trips a day at 12 contracts
   gives −$1.30 to +$0.72 per day across the plausible markout range (−1.8c to +1c per
   contract), against +$0.09 to +$0.21 of LIP. The sign of the sum is not determined by any
   evidence in hand.
4. **Inventory breaks scoring.** With `inv_hard` 7 < S 12, one fill stops adding until flat;
   the market scores one side only until the reducer fills (4 min to 7 h in the stages). Under
   the July 30 rule the one-sided quote still scores its side (the gate is market-level), so
   skewed two-sided quoting with `inv_hard ≥ S` (`lip-89x` option b) would roughly double the
   reward line during reduction at the cost of carrying the inventory.
5. **Crossing is still wrong, now more so.** A taker exit costs 1.3–2.8c per contract
   (`envelope.py` D). At S=12 that is $0.15–0.33 per exit against $0.09–0.21 of daily reward,
   i.e. one to three days of incentive, in line with `lip-89x`'s 40 h estimate. `reeval` §2's
   population finding that stopping out has zero expected benefit stands.

## 6. Verified versus modelled

| Claim | Status | Basis |
|---|---|---|
| July 30 rule text (reference at target/5, cap, exclusion scaling, $1 floor) | **verified** | PDF read raw |
| Lifetime realized −$1.26; stages +$0.21; LIP +$7.65 | **verified** (chain of authenticated balances; deposit implied) | §2 |
| Taker fee formula and $0 maker on `quadratic` series | **verified** on own fills | §3 |
| All traded/stage series are `quadratic` ×1 | **verified** today | public `/series` |
| Reward = share × uptime × pool | **calibrated**, n=1, −1.0% | `revmodel.py` re-run |
| Attended 2 h stages earn $0 | **arithmetic from the rule** | `envelope.py` A |
| Adverse selection at the touch −0.5 to −1.8c | **public, July, thick books**; private n=4 | §3 |
| 8,135 programs / $937,745 pool | **verified snapshot** 2026-10-01T20:02Z | `programs-active.json` |
| Eligibility fraction (mean 0.85, 78% of markets ≥ 0.95) | **historical** (Aug 13–18, 5-min cadence, Feb-rule gate, same depth test) | `lip.db` immutable read |
| Share at S=12/S=100 and the capacity curve under July 30 | **modelled**, one-shot public books, no competitive response, no fills, no fees, no downtime | §4 |
| Net EV of the current policy | **not established** | §5 |

## 7. What would change the verdict, ranked by information per unit cost

1. **One continuous run that clears the floor.** Seven or more days in one $100–$200 pool
   market at S ≥ 12, unattended-qualified, then the balance delta after the period ends.
   Expected $1–3 if the share model holds under the new rule. Verifies at once: the July 30
   share model, the eligibility fraction, competitive response, and that payouts still arrive.
   This is the North Star stage; nothing cheaper is decisive.
2. **Reconcile the three stage programs when they end** (2026-10-03, 10-05, 10-12): a
   GET-only balance read after each. Predicted $0 for all three. A credit would falsify the
   uptime model or reveal a floor rule the PDF does not state. Cost: three `accountcheck`
   runs.
3. **Private markout over ≥ 30 round trips** from the harness's own fills, which only a
   continuous run produces. This decides the sign of the trading line in §5.3.
4. **Eligibility at 1 Hz under the July 30 definition** for candidate markets, from a
   dedicated orderbook poll (the 5-min August collector cannot certify it; memory
   `lip-reward-inverse-liquidity`).
5. **Field response**: re-sweep the same books daily while a quote rests, to measure share
   decay. Converts §4 from one-shot to a trajectory.
6. **The fee PDF read in a browser** (Hugh), to close the `quadratic_with_maker_fees`
   coefficient. Low impact while scope stays on `quadratic` series.
7. **Chronological held-out policy comparison and momentum** (lip-yca acceptance items 3–4)
   need timestamped private fills in quantity; not feasible until item 3 exists.

## 8. Method and reproduction

- Terms: `notes/revival-evidence-2026-09-26/terms-july30.pdf`, pages 1–8.
- Money: `notes/live-continuation-evidence-2026-09-26/economics/{account-read.json,
  historical-cashflow.md, review.md, positions-pnl.json}`; `probe.db` `balance_poll` and
  `run` via `sqlite3 "file:probe.db?immutable=1"`; the four
  `notes/first-fill-evidence-2026-09-28/operator-stages/*/evidence/account-*.json` and
  `stage-report.md`.
- Calibration: `cp probe.db <scratch>/ && python3 scripts/revmodel.py` with the copy.
- Universe: `GET /trade-api/v2/incentive_programs?status=active&type=liquidity&limit=10000`
  at 2026-10-01T20:02Z, saved as `programs-active.json.gz`; summary in `programs-summary.json`.
- Scorer: `python3 lip30score.py ../revival-evidence-2026-09-26/book.json` reproduces
  `quote-counterfactual.json` (0.164872 / 0.164874 per side; 0.089843 one tick below).
- Sweep: `python3 sweep_books.py --programs programs-active.json --out-dir sweep --shard i/3
  --skip-markets` ×3 (rate 4/s each), then the status pass; `analyse_sweep.py` produces
  `sweep-summary.json` and `sweep-summary-rows.json` (gzipped here; `capacity_cuts.py`
  reads the unzipped rows and writes `sweep-summary-cuts.json`).
- Envelope: `python3 envelope.py`.
- Eligibility: `sqlite3 "file:lip.db?immutable=1"` per-ticker `avg(qualifies)` over tickers
  with ≥ 50 snapshots (Aug 13–18, 4,099 tickers, 1,025 timestamps).

## 9. Against lip-yca's acceptance criteria, and lip-89x

- *Reproduce updated scoring on fixtures*: done for the one July 30 fixture that exists
  (`quote-counterfactual.json`); no exchange-produced July 30 payout exists to test against.
- *Reconcile estimated vs observed payout*: done for the only observed payout (−1.0%), which
  predates or straddles the change-over; the first post-change payout is item 7.1.
- *Net EV and uncertainty for touch/offset quotes and immediate-vs-maker exits*: §3–5 give the
  arithmetic and the measured inputs; the private inputs have n=4, so the EV is bounded, not
  estimated. Immediate (taker) exits lose 1.3–2.8c per contract with certainty against a
  reward line of 1–2c per contract-day at S=12; maker exits are free but slow (4 min–7 h).
- *Momentum only on out-of-sample timestamped data*: no such data exists; deferred.
- lip-89x: the sweep supports option (c), ranking by pool per hour divided by field score,
  and the terms support option (b), since a one-sided quote keeps scoring. Option (a), a
  bounded taker exit, pays only where the reward line is tens of cents per hour, i.e. $500+
  pools; at $100 pools it never does.
