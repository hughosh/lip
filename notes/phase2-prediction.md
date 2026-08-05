# Phase 2 pre-registration — the $100 liquidity probe

**Written 2026-07-25 17:40Z, BEFORE entry and before any capital was
committed.** No order had been placed in this market at the time of writing and
the account had no open position (balance $100.25, position flat, no resting
orders). Spec §5 requires only that this precede the payout; it precedes the
trade, which is strictly stronger.

---

## 1. Selection (Q1)

**Chosen: `KXGENERICBALLOTVOTEHUB-26JUL31-T5.7`** — "Above 5.7%", generic
ballot vote hub. Program `c9f1111d`.

    pool                $1000   (5-10x any other candidate)
    Target Size           300
    Discount Factor       0.5
    period               166h, ends 2026-07-31 13:59Z
    market settles       2026-07-31   (5.8 days; can_close_early)
    book at selection    yes 57c / no 42c, sum 99, spread 1c
    touch depth          1784-2370 observed (varies)
    depth slack          23.5x Target Size
    vol24h                218    OI 143

Screened by `q1select.py --size 50` at 2026-07-25 17:36Z, re-run at selection
time rather than reusing the earlier scan, because Q1.1 depends on elapsed
fraction. 31 of 200 active programs passed every Q1 filter.

### Why this one

- **Q1.3 / fill risk is the binding consideration, and this market minimises
  it.** The $20 probe earned nothing because both sides were bought inside 35
  minutes, not because of drift. Here the queue ahead of us is 1784-2370
  contracts against 218 contracts of daily volume. For our order to fill, the
  entire queue in front of it must trade first. Across 200 seconds of live
  observation the book did not move at all — three consecutive 60s windows with
  zero deltas. Q7's ≥90% presence target is therefore achievable, which is what
  makes the ratio interpretable at all.
- **The prediction rests on one observable.** Touch depth alone (1784+) exceeds
  Target Size (300), so the qualifying walk terminates at the first level. Our
  share is `50 / (touch + 50)` and does not depend on the DF-weighted shape of
  the deep book. A thin-book candidate needs several levels to reach 300 and its
  prediction is correspondingly fragile.
- **2.6% is not in red-flag territory.** Spec §4 warns that a predicted share
  above ~50% is evidence of an unmodelled cost rather than an opportunity. The
  candidates at 24-44% share (`KXTRUMPTIME-26AUG01-*`) were passed over on that
  basis and on Q1.3 (slack 3.1-5.7x, versus 23.5x here).
- **Touch depth 1784-2370 against open interest of 143** says another LIP
  participant is already farming this program. That is the competitor whose
  response we are trying to measure, and it is already in the denominator.

### Passed over, and why

| Candidate | Share | Rejected on |
|---|---|---|
| `KXTRUMPTIME-26AUG01-H4` | 43.8% | Q1.3 slack 3.1x — the book can fall under Target Size and void snapshots for everyone, confounding the ratio with a mechanism that is not competition. Binary jump event, `can_close_early`. |
| `KXAPRPOTUS-26JUL31-*` | 6.8–16.9% | Q1.7 — 2,500–4,500 contracts/day against a 93-deep queue. We would fill repeatedly, which is the exact failure mode that killed the $20 probe. |
| `KXHOODA-28JANFUNDED-*` | 20–29% | Q1.1 — 32% elapsed, past the 25% bar. (Q1.6, the settlement rule flagged as most contentious, turned out not to bind: these were already excluded.) |
| `KXEOWEEK-26AUG01-*` | 18–42% | 20.9-day period. Capital lockup out of proportion to a $100 probe. |

### Two spec ambiguities resolved

- **Q1.1 states both** "remaining ≥ 90% of total length" **and** "never enter
  one past 25% elapsed". Enforced the 25% hard bar; treated 90% as the stated
  preference. At 15.5% elapsed this market satisfies the bar but not the
  preference, and the cost of that is priced in below.
- **Q6.3 was reinterpreted.** Taken literally ("no reference update for 60s
  while connected") it fired on the first quiet stretch of a healthy book —
  which is the normal state of the deep, low-volume market Q1.7 tells us to
  prefer. Enforcing it as written would halt the probe repeatedly and guarantee
  Q7 fails. Implemented instead as: silence past 60s triggers a REST orderbook
  cross-check, and the switch arms only if the touch actually disagrees with
  what the websocket believes. Active websocket pings were added so a half-open
  socket surfaces as a disconnect rather than as silence.

---

## 2. The prediction

Entry at ~15.5% elapsed. The denominator of our share is every gated snapshot
in the whole 166h period, including the 25.8h before entry, which is already in
competitors' numerators. So the achievable fraction is capped at **0.845** of
the instantaneous share.

    instantaneous share    50 / (touch + 50), both sides
      at touch 2370          2.07%
      at touch 1784          2.73%
      measured live          2.09%  (200s dry run, our size injected)

    central estimate       2.3% instantaneous
    x 0.845 late entry  ->  1.94% of the period
    x $1000 pool        ->  PREDICTED PAYOUT $19.40

**Point prediction: $19.40. Interval $17.50 – $23.00**, the band implied by the
observed touch-depth range. The $1.00 floor is not in play — the prediction
clears it roughly 19x.

Predicted fraction of gated seconds with a two-sided quote resting (Q7):
**≥ 98%**. The 200-second dry run achieved 99.0%, with the only absences being
the first snapshot before the book arrived. Anything below 90% and this run is
reported inconclusive rather than as evidence about competition.

Gate rate observed: 100% (199/200 snapshots gated; the one exception was the
startup snapshot before the first book arrived).

`Q8.2` will produce the authoritative integrated figure continuously during the
run and it is stored per-second in `probe.db`. The number above is the ex-ante
commitment; the integrated figure is the measurement, and where they disagree
the discrepancy is itself a finding.

---

## 3. Decision rule (spec §5, restated in dollars)

    ratio = realised / predicted,  predicted = $19.40

    ratio >= 0.7    realised >= $13.58   model holds; competition did not
                                         materially erode share. Proceed to a
                                         multi-market scale-up SPEC (not a
                                         scale-up).
    0.3 - 0.7       $5.82 - $13.58       partial erosion. Re-measure in a
                                         different market family before
                                         scaling. Do not scale on one
                                         observation.
    < 0.3           < $5.82              model is broken. Stop and diagnose:
                                         (a) participants respond to our
                                         presence, (b) the visible book is not
                                         the scoring book, (c) our presence
                                         fraction was overstated.
    exactly $0.00                        check the $1.00 floor first. Given a
                                         $19.40 prediction, $0.00 would be a
                                         refutation, not a floor artefact.

The payout is observable **only** as a `/portfolio/balance` delta — there is no
ledger, transactions, or per-program payout endpoint (all 404, verified
2026-07-25). The probe therefore runs in exactly one program with no other
account activity, and `balance_poll` in `probe.db` records the series.

---

## 4. What this cannot show

Nothing about adverse selection. codex's power analysis is binding: a 5c
per-contract effect needs ~384 independent market outcomes. One market for one
period supplies essentially no power, and any markout figure this run produces
is an anecdote. Do not update on it in either direction.

It also cannot distinguish "no competitor responded" from "competitors did not
notice 50 contracts". Our share is 2.6% of a book already 1784 deep; a rival
optimising against us would not need to respond at this size. A high ratio is
therefore evidence that the *scoring model* is right, not that the strategy
survives at scale.
