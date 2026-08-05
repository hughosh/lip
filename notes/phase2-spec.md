# Phase 2 spec — the $100 liquidity probe

## 0. The one-sentence contract

Rest a two-sided quote in one LIP market for one entire Time Period with $100 of
capital, and measure whether the realised payout matches the share the model
predicts — because competition for share is the only remaining unknown that
public data cannot answer.

---

## 1. What this is designed to measure, and what it is not

**Measures one thing:** `realised payout / pre-registered predicted payout`.

Everything else in this document exists to make that ratio interpretable. The
prediction is computed continuously from the live book and OUR OWN resting
orders, integrated over the period, and is **written down before the payout is
observed** (Q8, gate 3). A post-hoc prediction measures nothing.

**Explicitly does NOT measure adverse selection.** codex's power analysis
(`notes/codex-rt-execmode.md` §4) is correct and binding: detecting a 5c
per-contract effect needs ~384 independent market outcomes, 2c needs ~2,401.
One market for one period supplies essentially no power. Any markout number this
probe produces is an anecdote. Do not update on it in either direction.

**Does not test the port's latency.** §4 of `notes/economics.md` settled that
scoring is insensitive to requote latency across the entire plausible range
(0.9999 at 10ms, 0.9962 at 1s). This probe cannot and need not distinguish them.

---

## 2. The economics being tested

Measured 2026-07-25 across 200 active programs:

    Time Period length   median 180.8h (7.5 days); 153/200 are >= 7 days
    pool                 median $100/period; median $8.2 per day of lockup
    Target Size          1000 contracts (191/200); 300 (9/200)
    Discount Factor      0.5 (200/200)
    predicted share      median 21.5% at 100 contracts/side
    predicted return     median 12.1% per period

Scoring is 1 snapshot/second at a nonpublic random sub-second offset, summed
over the period and normalised across all users (`notes/economics.md` §3).
Payout is skipped entirely below $1.00.

**The consequence that dominates the design: share is accrued over the whole
period, not per snapshot.** A quote present for 1% of the period earns ~1% of
the share it would earn if present throughout. Presence duration, not
per-snapshot excellence, is the objective function.

---

## 3. Why the prior $20 probe earned nothing

Reconstructed from `/portfolio/orders` and `/portfolio/fills` on 2026-07-24, in
market `KXHOODA-28JANFUNDED-33000000` (program `51248bff`, 2026-07-23 14:23Z →
2026-07-30 03:59Z, $100 pool):

    20:44:07  buy YES 20 @ 54c   -> fully filled 21:18:38   (rested 34.5 min)
    20:44:31  buy NO  20 @ 45c   -> fully filled 20:50:39   (rested  6.1 min)
    21:09:50  buy NO  20 @ 44c   -> cancelled   22:17:49    (rested 68 min, unfilled)

Two-sided presence — the only state that scores — was roughly 20:44:31–20:50:39
plus 21:09:50–21:18:38, about **15 minutes of a 7.5-day period**, or 0.14% of
snapshots. Even at 100% per-snapshot share that is $0.14 against a $1.00 floor.
**Expected payout $0.00, and that is the correct prediction, not a failure of
the model.**

Three lessons, each of which becomes a rule below:

1. **Duration dominates.** Q1/Q7 — the probe runs the full period or it produces
   nothing.
2. **Fills, not drift, ended the presence.** Both sides were consumed inside 35
   minutes at a 1c spread. Replenishment (Q5) is the core operational problem.
   The earlier diagnosis of "lost scoring presence to reference drift" was wrong;
   the orders did not go stale, they were bought.
3. **Matched fills self-liquidate.** Buying YES 20 @54c and NO 20 @45c cost
   $19.80 and released back to cash at $20.00 immediately — realised +$0.20, and
   `market_exposure_dollars` returned to 0 without waiting for the Jan-2028
   settlement. Kalshi nets offsetting YES/NO holdings on the spot. **A both-sides
   fill is therefore profitable and self-unwinding whenever
   `yes_bid + no_bid < 100`.** Only a ONE-SIDED fill creates a problem.

---

## 4. Rules

### Q1 — Market selection

Choose exactly one market, by this order of filters:

- **Q1.1** Has an `incentive_type: liquidity` program whose remaining period is
  ≥ 90% of its total length. Joining late is directly dilutive: the denominator
  already contains every other participant's score from the elapsed portion, so
  maximum achievable share is capped at roughly the remaining fraction. Prefer a
  program that has just started; never enter one past 25% elapsed.
- **Q1.2** Both sides currently meet Target Size (`qualifies_now`), and still do
  after adding our size (`qualifies_after`). `score.py` computes both.
- **Q1.3** Depth slack ≥ 1.3× Target Size on the thinner side (`probe.py`'s
  criterion). A book that only just clears the gate can drop below it and void
  the snapshot for everyone, including us.
- **Q1.4** Mid between 10c and 90c. Near-certain markets have degenerate books.
- **Q1.5** `yes_bid + no_bid ≤ 99`. Required for both our orders to rest without
  crossing, and it is what makes a both-sides fill profitable (§3 lesson 3).
- **Q1.6** **Prefer near settlement.** This is the filter that overrides headline
  share. The highest-share candidates on 2026-07-25 are all
  `KXHOODA-28JANFUNDED-*` at 42–79% predicted share — and they settle in
  **January 2028**. A one-sided fill there is directional exposure with an
  18-month tail, exitable only by crossing a thin spread. That risk premium is
  most of why the share looks free. Prefer a market settling within weeks unless
  Q2's one-sided cap is tightened to compensate.
- **Q1.7** Prefer lower trade volume among survivors. A deep, quiet book scores
  identically and fills less.

**A predicted share above ~50% is a red flag, not an opportunity.** It arises
when the qualifying depth sits many ticks below the touch and is therefore
discounted to near-nothing by DF=0.5, so our size at the touch dominates the
denominator. That is a real mechanism, but a 79% share on a $100 weekly pool for
$99 of capital implies a return no competitive market leaves lying around.
Treat it as evidence of an unmodelled cost — most likely Q1.6 — and select for
plausibility, not for the top of the table.

### Q2 — Sizing

- **Q2.1** Total deployed capital ≤ **$100**. Hard cap, enforced in code, checked
  before every order.
- **Q2.2** Initial quote **50 contracts per side** (~$50), not 100. Half the
  budget is held as replenishment reserve. At 50/side the predicted share is
  still ~10–20%, which clears the $1.00 floor by an order of magnitude — the
  probe is sized to *measure*, not to maximise.
- **Q2.3** Maximum net directional inventory **25 contracts**. On breach, stop
  replenishing the long side and flatten to within the cap.
- **Q2.4** Never place an order that would make `yes_bid + no_bid ≥ 100` against
  our own resting order on the other side. Kalshi's maker-side self-trade
  prevention cancels the resting maker; a self-cross would silently destroy our
  own scoring presence.

### Q3 — Quote placement

- **Q3.1** Join the touch on both sides — `N = 0`, maximum credit. With DF = 0.5
  one tick back halves the score, which no fill-avoidance benefit repays.
- **Q3.2** Never improve the touch. Improving moves the Reference Price to us and
  makes the whole book score against a reference we set — self-referential, and
  it widens our own adverse-selection exposure for no scoring gain (we are at
  `N = 0` either way).

### Q4 — Requote policy

- **Q4.1** Requote only when the reference moves **against** us (up on the yes
  side / up on the no side), leaving our order ≥1 tick below the touch.
- **Q4.2** **Debounce 250ms.** Requote only once the new reference has held for
  250ms. This is the direct consequence of the time-weighting result: bursts
  under 100ms are 52.6% of all moves and 0.1% of scored time, so chasing them
  buys ~0.0003 of multiplier while multiplying order rate and self-inflicted
  queue loss.
- **Q4.3** Rate limit: at most **1 requote per side per 5 seconds**, and at most
  500 orders per day. A 5s worst-case staleness costs 1.2% of score (0.9882) and
  removes any possibility of a runaway loop against the exchange.
- **Q4.4** Do **not** requote when the reference moves in our favour (drops below
  our resting bid). Our order is then the best bid and already scores `N = 0`.

### Q5 — Fill handling

- **Q5.1** On any fill, replenish that side back to Q2.2 size at the current
  touch, subject to Q2.1 and Q2.3.
- **Q5.2** Replenish within the Q4.3 rate limit — a fill is not an emergency.
- **Q5.3** If replenishing would breach the inventory cap (Q2.3), replenish the
  *opposite* side instead to restore two-sided presence, which is what scoring
  requires.

### Q6 — Kill switches

Any one of these cancels all orders and halts, permanently, requiring manual
restart:

- **Q6.1** Realised + unrealised P&L ≤ **-$15**.
- **Q6.2** Net inventory > 2× the Q2.3 cap (50 contracts) — indicates the
  replenishment logic is misbehaving.
- **Q6.3** No reference update for the selected market for **60s** while the
  websocket reports connected, or any disconnect lasting > 60s. Quoting into a
  book we cannot see is the one failure mode that can lose real money fast.
- **Q6.4** Order rejection rate > 10% over any 50 orders.
- **Q6.5** Wall-clock: hard stop and cancel-all at program `end_date`.
- **Q6.6** Manual: a sentinel file on disk, checked every loop.

### Q7 — Duration

The probe runs from entry to `end_date` of the program, target **≥ 90% of gated
seconds with a two-sided quote resting**. Below 90% the payout comparison is
confounded by absence and the run should be reported as inconclusive rather than
as evidence about competition.

### Q8 — Measurement

- **Q8.1** Log every order, cancel, fill, and rejection with local and exchange
  timestamps into a SQLite DB alongside `rig.db`.
- **Q8.2** Every second, compute and store our **predicted snapshot score share**
  from the live book: our qualifying score ÷ total qualifying score, per side,
  exactly as `score.py` does. Integrate over the period to get predicted
  `TimePeriodLiquidityProviderScore`.
- **Q8.3** Poll `/portfolio/balance` every 60s. There is no ledger, transactions,
  or per-program payout endpoint (all 404 — verified 2026-07-25), so **the payout
  is observable only as a balance delta**. This is why the probe must run in
  exactly one program at a time with no other account activity; otherwise the
  payout is unattributable and the experiment is void.
- **Q8.4** Record the settled `fill` rows into the existing rig schema with
  `source='ours'` so the existing analysis path works unchanged.

---

## 5. Pre-registration and the decision rule

Before the period ends, and before any payout is observed, write to
`notes/phase2-prediction.md`:

- the market, program id, entry time, and size;
- the integrated predicted share and the resulting predicted payout in dollars;
- the fraction of gated seconds with a two-sided quote.

Then, on payout:

    ratio = realised / predicted

    ratio >= 0.7   model holds. Competition did not materially erode share.
                   Proceed to a multi-market scale-up spec.
    0.3 - 0.7      partial erosion. Re-measure with a second period in a
                   different market family before scaling. Do not scale on one
                   observation.
    < 0.3          model is broken. Stop. The most likely causes, in order:
                   other participants respond to our presence; the visible book
                   is not the scoring book; our presence fraction was
                   overstated. Diagnose before any further capital.
    exactly $0.00  check the $1.00 floor first (Q8.2 predicted < $1 makes this
                   uninformative, not a refutation).

The asymmetry is deliberate. A good result buys a scale-up spec, not a scale-up.

---

## 6. Definition of done

- One full Time Period completed under Q7, or an explicit inconclusive report.
- `notes/phase2-prediction.md` written and timestamped **before** payout.
- The realised/predicted ratio recorded with the decision-rule outcome.
- Every kill switch in Q6 exercised at least once in a dry run against the
  shadow feed before a single live order is placed.
- No frozen artifact modified (`scripts/check.py` green).

---

## 7. Explicitly out of scope

- Any conclusion about adverse selection (§1).
- Multi-market operation. One market, one program, or the balance-delta
  observable (Q8.3) breaks.
- Taker orders of any kind. The program rewards resting liquidity; crossing the
  spread pays fees and forfeits the premise.
- Improving the touch (Q3.2).
- Any change to the Go rig. The probe consumes its feed; it does not modify it.

---

## 8. Standing risks, stated plainly

- **Capital is locked for the period** (median 7.5 days), and for a one-sided
  fill in a long-dated market, potentially far longer (Q1.6).
- **The headline returns are implausibly good.** 12.1% median per period, 79% at
  the top. Either the market is genuinely uncontested at this size, or there is a
  cost not in the model. This probe exists precisely because we cannot tell those
  apart from public data, and $100 is the cheapest way to find out.
- **Kalshi may revoke participation** for conduct it deems abusive. Ordinary
  two-sided quoting is the behaviour the program is designed to buy, but Q3.2 and
  Q2.4 exist partly so nothing we do resembles self-trading or reference
  manipulation.
- **The program sunsets 2026-09-01** unless extended, which bounds the total
  opportunity regardless of the result.
