# Harness spec — the 24/7 LIP quoting harness

**Status:** design contract, **v2 — post-adversarial-review.** Written
2026-08-04, before any Go is written; revised the same day against a hostile
cross-model review.
**Audience:** whoever (human or agent) implements the harness.
**Rule:** this document outranks your judgement about what a market maker
should do. Where it contradicts `notes/phase2-spec.md`, this file wins; where it
contradicts `notes/port-spec.md`, port-spec wins and you escalate.

> **v2 changes.** One adversarial pass (codex `gpt-5.6-sol`, max reasoning,
> read-only) returned 26 findings; 24 were material. Full disposition,
> including the one finding refuted by measurement and the reasoning for every
> classification, is in **`notes/harness-redteam.md`**. The pre-review text is
> preserved at `notes/harness-spec.md.bak-preredteam`.
>
> The changes you most need to know about before implementing:
>
> | What changed | Rule | Why |
> |---|---|---|
> | Reducer capped at `\|q\|`, was `\|q\| + S` | H-Q-5a | overshot into a `−100/+100` sign-flip cycle |
> | All caps are aggregate, not per-order | H-Q-5b, A11 | two overlapping reducers could both fill and flip us |
> | Concentration cap never binds the exit | H-CAP-6 | the opening config could not fund its own reducer |
> | A5 asserts source *advancement* | H-TOP-5, I3 | a deadlocked owner produced fresh rows about a frozen world |
> | A4 no longer exempts `SETTLING` early | H-CLOSE-2a | abandoning inventory for 4h passed every gate (**M13**) |
> | Every list read is a full cursor walk | H-PAGE-1 | `probebot.py:1100` records this exact bug in production |
> | "Never landed" branch deleted; same-coid retry instead | H-ORD-2a/2b | absence was unprovable; **V1.7 since confirmed Kalshi dedupes (409), making retry safe** |
> | Global halt persists across restart | H-HALT-4 | `launchd KeepAlive` silently self-cleared it |
> | "Off" means exchange-confirmed absent | H-FAIL-3 | it meant "we sent a cancel" |
> | Improving the touch *does* score better | H-Q-2 | rule kept, false justification deleted |
> | One-sided quoting costs ~1/3 of reward | §6.2 | it was claimed to be free |
>
> **`close_lead_keep_reducing`'s default is formally unresolved** and is the one
> open decision (§19.1, `harness-redteam.md` §5).

---

## 0. The one-sentence contract

Quote at the touch in N Liquidity Incentive Program markets, continuously, to
farm the incentive pool — **never crossing the spread, never abandoning
inventory, and never going quiet about either.**

Everything below exists to make those three negations mechanically true rather
than aspirationally true.

---

## 1. What is already settled, and is not reopened here

These are inputs. Do not re-derive them, do not re-measure them, do not build a
probe to re-check them.

| # | Settled fact | Source |
|---|---|---|
| S1 | **Never cross.** `E[settlement − mid_5m]` is statistically zero in every drawdown bucket, so stopping out has no expected benefit while costing ~2.4–2.8c/contract with certainty (half-spread ~1.07c + taker fee 1.31–1.75c). | `reeval-verdict.md` §2, `scripts/holdvsflat.py` |
| S2 | **Maker fees are $0.00.** Taker fees are `0.07·C·P·(1−P)` dollars, verified to 4dp against the account's own `fee_cost`. A never-crossing harness pays zero trading fees. | `reeval-verdict.md` C2 |
| S3 | **The two-sided gate is market-level, not per-user.** "Resting orders sufficient to meet the Target Size on each side of the market." A user's score is the sum of their own normalised yes and no scores, so a one-sided quoter still scores at full weight on the side it quotes. | CFTC filing, read raw; `reeval-verdict.md` C7 |
| S4 | **Uptime is revenue.** Reward = time-weighted share integrated over the *whole* period × pool. The model reproduced the one verified payout to −1.0% ($7.574 predicted vs $7.65 actual) at 37.8% uptime. Downtime is a direct multiplicative loss. | `reeval-verdict.md` §4, `scripts/revmodel.py` |
| S5 | **A matched two-sided fill self-liquidates at a profit** whenever `yes_bid + no_bid < 100`. Kalshi nets offsetting YES/NO holdings on the spot; `market_exposure_dollars` returns to 0 without waiting for settlement. Only a **one-sided** fill creates a problem. | `phase2-spec.md` §3 lesson 3, verified on the account |
| S6 | **Market selection dominates size.** DF=0.5 crushes everything more than a few ticks behind the touch, so a market's total qualifying score can be tiny even when its depth is large. Rank by pool ÷ field qualifying score. | `reeval-verdict.md` §5 |
| S7 | **Latency is not binding.** Scoring is 0.9999 at 10ms requote latency and 0.9962 at 1s. Nothing in this spec should be built for speed. | `notes/economics.md` §4 |

**The economics thread is closed.** No proposal in this document is a
measurement run, and no section of it may be implemented as one.

---

## 2. The defect this harness exists to not have

The probe lost $9.32 of trading damage against $7.65 of reward. The
decomposition reads "strategy 73% / harness 27%", **and that reading is wrong.**
The timeline says otherwise:

- `probebot.py`'s `halt()` cancels resting orders and **never flattens**.
- The snapshot loop `break`s in the same control-loop iteration.
- Measured consequence: **2 snapshots recorded over the following 6.14 hours**,
  while 61 contracts sat naked and directional.
- The market fell 57 → 49 unobserved. It then **settled YES at $1.00**; holding
  all 111 contracts would have returned **+$51.73** instead of −$7.39.

The fault is not the entry, the drift, or the fee. It is that **the safety
mechanism was a stop button on the wrong system.** It stopped the observer and
left the risk running.

Two structural rules follow, and they are the load-bearing rules in this
document. Everything else is detail.

> **I1 — The harness never exits, halts, or degrades in a way that leaves
> inventory unmanaged and unobserved.** Every stop path stops *adding* risk. No
> stop path stops *reducing* it, and no stop path stops watching it.

> **I2 — The monitoring path is structurally unreachable from the quoting
> path's control flow.** Monitoring runs on its own goroutine, with its own
> lifetime, and cannot be terminated by any decision the quote engine makes.
> There is no `break` in the quote engine that a monitor can observe.

I2 is stated structurally, not behaviourally, on purpose. "Remember to keep
sampling after a halt" is the kind of rule that gets unlearned. "The sampler is
not in that function and has no way to be" is not.

---

## 3. Topology

### H-TOP-1 — A new binary, `cmd/harness`

The harness is a **separate process in the same Go module**, not an extension of
`cmd/rig`. It uses `core` as a library and gets its own `core.Rig` instance and
its own websocket connection.

Rationale, recorded so it is not re-litigated:

- `cmd/rig`'s entire correctness argument is port-spec §8: gates 1–7 plus a
  seven-day shadow run, **on the artifact that ships**. Adding a trading path to
  that binary invalidates the most expensive piece of evidence in the plan.
- `rig.db` collection is the evidence base for everything. A harness crash must
  not be able to stop it.
- `core` was built to be exactly this: pure, deterministic, clock-free, I/O-free.
  Instantiating a second `core.Rig` costs one map and some level structs.

Cost accepted: two websocket connections to Kalshi, and two independently
reconstructed books. The books are not compared; `rig.db` remains the
measurement record and `harness.db` is the operational record.

### H-TOP-2 — What may not be touched

| Path | Status |
|---|---|
| `go/core/**` | **Read-only.** Every P-rule in port-spec §4 binds. The harness consumes it and adds nothing to it. |
| `go/feed/**` | **Read-only.** P25, P25a, P26, P27 bind, and P-rules require the *absence* of a read deadline and a ping. The harness needs both, so it cannot use this package. |
| `go/store/**` | **Read-only.** Its `fill` INSERT hard-codes `source='observed'`. |
| `go/cmd/rig/**` | **Read-only.** |
| `lip/rig.py replay.py score.py auth.py scripts/diffdb.py testdata/*.tsv` | **Frozen.** `scripts/check.py` must stay green. Re-freezing to pass a gate is itself a failure. |

### H-TOP-3 — New packages

```
harness/wsx     websocket client WITH read deadline, ping/pong, and
                resolution fallback. A sibling of feed/, never a patch to it.
harness/rest    signed REST client: portfolio reads, order writes, market and
                orderbook reads. Reuses feed.Signer.Headers verbatim (it is
                REST-general — it signs timestamp+METHOD+path for any path).
harness/quote   the per-market state machine, skew function, priority queue.
                Pure: no clock, no I/O, no goroutines. Differentially testable
                like core.
harness/risk    position model, capital allocator, invariant assertions.
                Pure.
harness/hstore  harness.db writer.
harness/ping    anomaly journal + queued ntfy delivery.
harness/sim     the deterministic simulated exchange (§14).
cmd/harness     wiring, goroutines, signals, supervision.
```

**`harness/quote` and `harness/risk` are pure by the same rule that makes `core`
pure** (port-spec §3: no clock, no I/O, no goroutines). This is not stylistic —
it is what makes §14's simulator able to replay a tape and produce a
deterministic, comparable trace. Anything needing a clock lives in `cmd/harness`
and is passed in.

### H-TOP-4 — Concurrency

Same discipline as the rig: a single owner goroutine mutates all state.

| Goroutine | Owns | May touch |
|---|---|---|
| **owner** | `core.Rig`, all books, all `quote` state, all `risk` state, `hstore` | everything |
| **pump** | the socket read loop | pushes `[]byte` onto `frames`; touches no book, no row, no DB |
| **rest-worker** ×K | outbound HTTP | consumes `orderReq` from a channel, pushes `orderResp` back. Never touches state. |
| **monitor** | nothing | reads a lock-free snapshot published by the owner; writes only to its own tables. **This is the I2 goroutine.** |
| **ping** | the delivery queue | reads the journal, POSTs, marks delivered |
| **signal** | the shutdown latch | sets one flag |

`harness/quote` and `harness/risk` carry no mutex, exactly as `core` carries
none. Safety is single-writer, not locking.

**The monitor does not read owner state directly.** The owner publishes an
immutable snapshot struct via `atomic.Pointer[Snapshot]` after each tick; the
monitor reads the pointer. This is what makes I2 structural: the monitor has no
reference to anything the quote engine can stop.

### H-TOP-5 — The snapshot is sequenced, because liveness is not truthfulness

`Snapshot` carries a **monotonically advancing `seq`** and a **monotonic
publication timestamp**, both set by the owner at publication.

> **I3 — The monitor must be able to detect that its own input has stopped
> advancing, and must say so rather than continue republishing it.**

I2 makes the monitor unstoppable by the quote engine. It does **not** make it
truthful, and red-team HR-007 is the proof:

> The owner publishes `q = 0`, then deadlocks. One second later a resting order
> fills. The monitor re-reads the same pointer forever, stamps a fresh `snap` row
> every second, and pushes hourly heartbeats reporting `q = 0` — while real
> inventory grows unobserved. **A5 passes at every tick. F18 never fires, because
> the process and its heartbeat are both alive.**

This is `probebot.py`'s observable — confident silence about live risk — reached
by a different route than `probebot.py`'s `break`. §2's I2 was written against the
`break` and does prevent it (the reviewer confirmed this). It does not prevent
this. A monitor that cannot fail is not the same thing as a monitor that cannot
lie, and the design had conflated them.

Therefore:

- **A5 asserts source advancement, not row freshness** (§17 V3).
- A monitor observing a stalled `seq` emits `SEV1 OWNER_STALLED`, marks its
  output **stale**, and stops presenting it as current — in `snap` rows, in
  heartbeats, and in the integrated `uptime` accounting.
- Mutation **M14** (freeze owner publication, leave monitor and ping alive) must
  **fail** against the old A5 and **pass** against this one.

---

## 4. Coordinate systems — get this wrong and everything else is noise

There are **three** representations in play and they are not interchangeable.

| Representation | Used by | Shape |
|---|---|---|
| **Book terms** | LIP scoring, `core`, `probescore.py`, this spec | yes bids at cents, no bids at cents. Both sides are *bids*. |
| **Wire terms** | the V2 order API | `side` ∈ {`bid`,`ask`} on the **YES leg only**; `price` is a YES-denominated dollar string |
| **Position terms** | `/portfolio/positions` | one signed number per market, YES-positive |

### H-CO-1 — The transform, pinned

```
book (side="yes", p cents)  ->  wire {"side":"bid", "price": p/100}
book (side="no",  p cents)  ->  wire {"side":"ask", "price": (100-p)/100}
```

A NO bid at 42c **is** a YES ask at 58c. From
`kalshi.py:create_order`, verified against `docs.kalshi.com/openapi.yaml`
(`CreateOrderV2Request`, `BookSide`) on 2026-07-25:

> "`bid` means buy YES, `ask` means sell YES. (Selling YES is economically
> equivalent to buying NO at `1 - price`…)"

### H-CO-2 — Wire encoding

`count` and `price` are **fixed-point STRINGS**, not JSON numbers. Sending
numbers is a schema violation.

```go
Count: fmt.Sprintf("%.2f", count)          // "50.00"
Price: fmt.Sprintf("%.4f", yesCents/100.0) // "0.5800"
```

The legacy `POST /portfolio/orders` returns **HTTP 410
`deprecated_v1_order_endpoint`**. The live endpoints are:

```
POST   /trade-api/v2/portfolio/events/orders
DELETE /trade-api/v2/portfolio/events/orders/{order_id}
```

`DELETE` returns `{order_id, client_order_id, reduced_by, ts_ms}` — `reduced_by`
is the count actually cancelled, **not** a full order object. A partial cancel
is possible and must be handled.

**The create response is also not a full order object.** Observed 2026-08-04:

```
201/200 → {order_id, client_order_id, fill_count, remaining_count, ts_ms}
409     → {"error":{"code":"order_already_exists","message":"order already exists"}}
```

There is **no `status` field**; `fill_count` and `remaining_count` are
fixed-point strings on the way out just as `count` and `price` are on the way in.
A create that returns `remaining_count > 0` is resting; resting-order objects read
back from `/portfolio/orders` use different keys again (`yes_price_dollars`,
and `count_fp` which was observed **null**), so do not assume one order shape
across endpoints — parse each explicitly.

### H-CO-3 — The orderbook read

`GET /markets/{ticker}/orderbook` returns levels under **`orderbook_fp`** with
keys `yes_dollars` / `no_dollars`, **best level last** (ascending by price).
Reverse before use. `core.ParsePriceCents` (banker's rounding on the exact
binary product, P1) is the only permitted price parser; `core.ParseSize` is the
only permitted size parser. Do not write a second one.

**H-CO-3a — The resting book is integer-cent. Assert it; do not assume it.**

`core.ParsePriceCents` rounds to integer cents, and `num.go:27` records that
8.8% of *all* price occurrences are fractional. Red-team HR-002 argued from this
that the harness cannot represent the touch it is required to join (H-Q-1) or
avoid improving (H-Q-2). **Measured against `raw-20260724T231211Z.jsonl.gz`, the
argument does not hold:**

| Source | Price strings | Fractional-cent |
|---|---|---|
| `orderbook_delta.price_dollars` | 14,792,425 | **0 (0.00%)** |
| `orderbook_snapshot.{yes,no}_dollars_fp` | 7,356 | **0 (0.000%)** |
| `trade.{yes,no}_price_dollars` | 182,029,958 | 31,097,490 (17.08%) |

**Fractional cents occur only in trade prints, never in resting book levels** —
14,799,781 book price strings with zero exceptions. `num.go`'s 8.8% is across a
population that outnumbers book levels 12:1 and is dominated by trades. So
`ParsePriceCents` is lossless for every price the quoting path reads, and H-CO-1,
H-Q-1 and H-Q-2 are implementable exactly as written.

**But this is one tape from one universe, and tick size is the exchange's to
change.** Therefore `harness/wsx` and `harness/rest` **assert** that every parsed
book price is an exact integer cent, and on violation emit
`SEV2 BOOK_PRICE_GRANULARITY` and send that market to `REDUCING`. Ten lines that
convert a measured regularity into a detected anomaly.

**The simulator does not get this exemption.** It reads trade prints, where 17%
*are* fractional, and rounding them through `ParsePriceCents` corrupts its price
comparison. See §17 V2.

### H-CO-4 — Sizes are floats

`size` is contracts and is **fractional for ~45% of observed rows** (20.5% of
resting book levels in the sampled tape). Our own position, our own fills, and
every size the harness computes are `float64`. There is no `int` count anywhere
in the position model. A partial fill of an integer-count order can leave a
fractional resting remainder.

**H-CO-4a — Quantities are quantized before every sign or zero comparison.**

`q` is stored in the exchange's exact fixed-point quantum (contracts × 100 as an
integer, matching the two-decimal wire format), or is quantized to it before any
comparison that decides a state transition.

> Red-team HR-026, and reachable rather than theoretical given the measured 20.5%
> fractional sizes. Fills of 0.10, 0.20 and −0.30 leave a binary residue near
> `5.6e-17`. The market is economically flat, but `q != 0` is *true*, so:
> `REDUCING` never reaches `IDLE`; SIGTERM never sees a drained market and the
> process never exits (H-HALT-3); and `SETTLING` formats `|q|` as `"0.00"` and
> rejects in a loop. **A float equality test was gating three lifecycle
> transitions.**

**H-CO-4b — A formatted `count` is validated before dispatch:** it must be
strictly positive and no greater than the quantized position it derives from.
`"0.00"` is never sent.

### H-CO-5 — Self-trade prevention

Every order carries `"self_trade_prevention_type": "taker_at_cross"`. This
cancels the **incoming** order on a self-match and leaves our resting maker
alive. `maker` would do the opposite and silently destroy the scoring presence
we are paid for. Do not change it, and do not omit it.

### H-CO-6 — Never self-cross

Never place an order that would make `our_yes_price + our_no_price ≥ 100`
against our own resting order on the other side. Check before every placement,
against our own current resting state, not against the book.

---

## 5. State machines

### 5.1 Global state

```
                          halt latch set (H-HALT-4)
        ┌─────────┐  ───────────────────────────────┐
        │STARTING │  reconcile ok   ┌──────────┐    │
        │         ├────────────────►│ RUNNING  │    │
        └────┬────┘                 └────┬─────┘    │
             │                           │ global halt trigger
             │ truth reads fail          ▼          │
             ▼                      ┌──────────┐    │
      ┌──────────────┐              │ WINDING  │◄───┘
      │ UNKNOWN_RISK │─ reads ok ──►│  _DOWN   │ ── all flat ──► DRAINED
      │  (retries    │   (to the    └──────────┘
      │   forever)   │    state the latch implies)
      └──────────────┘
```

| State | Adding quotes | Reducing quotes | Monitoring | Process |
|---|---|---|---|---|
| `STARTING` | **no** | **no** | yes | alive |
| `UNKNOWN_RISK` | **no** | **no — `q` is unknown, so no reducer can be sized** | yes | alive, retrying truth reads forever |
| `RUNNING` | yes | yes | yes | alive |
| `WINDING_DOWN` | **no, permanently** | **yes** | yes | **alive** |
| `DRAINED` | no | no (nothing to reduce) | yes | alive, idle |

**`UNKNOWN_RISK` is not a halt** (H-ORD-5a). It is the state for "we may hold
inventory and cannot see it", and its only job is to keep trying to find out. It
never places, never exits, and never decays into `RUNNING` without a complete
successful reconciliation. Sending this case to `WINDING_DOWN` — as an earlier
version did — puts the harness in a state whose entire purpose is keeping a
reducing quote alive, without the one number needed to size one.

**`STARTING` reads the halt latch before anything else** (H-HALT-4). A latch set
by a previous incarnation sends the process straight to `WINDING_DOWN`, and only
an operator clears it.

**`WINDING_DOWN` is not "stopped".** It is the state the probe should have
entered and did not. There is no transition from `WINDING_DOWN` back to
`RUNNING` without an operator action (§10.4).

**`DRAINED` does not exit the process.** It idles, keeps monitoring, keeps
heartbeating, and waits for an operator. Exiting on drain would mean the one
moment the harness is safe is also the moment it stops being able to tell you
anything.

### 5.2 Per-market state

Let `q` = signed net position in contracts (YES-positive), `S` = base quote size.

```
  IDLE ──select──► QUOTING ◄──────────────┐
                     │                     │  |q| falls back under inv_soft
                     │ |q| > inv_soft      │
                     ▼                     │
                  SKEWED ──────────────────┘
                     │ |q| > inv_hard, or market halt trigger,
                     │ or program end, or close lead
                     ▼
                 REDUCING ──── q == 0 ────► IDLE
                     │
                     │ close_time − final_lead
                     ▼
                 SETTLING ──── close ────► CLOSED
```

| State | Adding side | Reducing side | Notes |
|---|---|---|---|
| `IDLE` | — | — | flat, not selected, or waiting for capital |
| `QUOTING` | size `S` at touch | size `S` at touch | the normal state; symmetric |
| `SKEWED` | tapered `size_A` (§6.2) | `min(\|q\|, S_max, funded)` at touch | continuous, no cliff |
| `REDUCING` | **cancelled, exchange-confirmed** | `min(\|q\|, S_max, funded)` at touch | one-sided; scores on the quoted side only (§6.2) |
| `SETTLING` | cancelled, exchange-confirmed | `min(\|q\|, S_max, funded)` — **aggregate**, cannot flip us | see §8, §9 |

All three reducing sizes are **aggregate across `RESTING` + `SENDING` +
`UNKNOWN` + unconfirmed-cancel** (H-Q-5b), never per-order, and never exceed
`|q|` (H-Q-5a). "Cancelled" means **exchange-confirmed absent**, not
cancel-requested (H-FAIL-3).
| `CLOSED` | — | — | position settled; terminal |

**Note what is absent: there is no `HALTED` per-market state that cancels
everything.** A market-level halt trigger sends the market to `REDUCING`, which
keeps the exit alive. That is the inversion.

---

## 6. Quoting

### 6.1 Placement

- **H-Q-1 — Join the touch, both sides, `N = 0`.** With DF = 0.5, one tick back
  halves the score. No fill-avoidance benefit repays that.
- **H-Q-2 — Never improve the touch.** Improving is a pure adverse-selection
  trade: it makes us the best bid, so we are filled more often and by better-
  informed counterparties, and `reeval-verdict.md` §3a measured at-touch fills as
  **completely adversely selected already** (−0.786c signed 1-minute drift,
  t = −10.75). Being *inside* the touch buys more of exactly that.

  > **This rule survives, but its earlier justification was false and is
  > deleted.** The previous text said improving bought "no scoring gain — we are
  > at `N = 0` either way." Red-team HR-025 disproved it against
  > `probescore.py`. Target 200, external depth 100@50 and 100@49:
  >
  > | Action | Qualifying set | Total score | Our score | Our side share |
  > |---|---|---|---|---|
  > | Join 100@50 | `[(50,200)]` | 200 | 100 | **50.0%** |
  > | Improve to 51 | `[(51,100),(50,100)]` | 150 | 100 | **66.7%** |
  >
  > Improving demotes the whole field one tick — halving it at DF = 0.5 — while
  > we stay at `N = 0`. **There is a real scoring gain, and we decline it** because
  > the adverse selection costs more. Stating it the old way invited an
  > implementer to recompute, find the spec wrong, and override the rule.

- **H-Q-3 — Never take.** `post_only: true` on every order the harness ever
  sends, with no exception, no flag, and no code path that sets it false.
  This is enforced structurally: `harness/rest` has **no parameter** for it.
- **H-Q-4 — Quote only into a qualifying book, and do not hold risk in one.** If
  the market's own qualifying walk fails on either side
  (`core.Book.Qualifies() == 0`), the snapshot is excluded for everyone (S3's
  gate) and our presence earns nothing. Do not place. Record the interval as
  gated-out in the uptime accounting so it is excluded from the denominator, not
  counted as our downtime.

  **H-Q-4a — After `gate_fail_debounce_s` (30s) of continuous gate failure,
  cancel and exchange-confirm every *adding* order.** If `q ≠ 0`, keep only the
  aggregate-capped reducer (H-Q-5a). Re-enter only from a fresh, complete,
  qualifying book.

  > Red-team HR-012: the earlier rule said "do not cancel what is already
  > resting." Reward in a gated-out interval is **zero for every participant**,
  > but our orders stayed live and fillable, taking directional inventory during
  > an interval that could not pay — and the gated-out accounting excluded the
  > interval from our uptime denominator, so the loss was invisible in the
  > metric. The debounce is what the original rule was really protecting: churn
  > on transient blips. Thirty seconds keeps that protection and closes the hole.

### 6.2 The skew function — the only inventory-reduction mechanism

Reducing side `R` = `"no"` if `q > 0`, `"yes"` if `q < 0`, none if `q == 0`.
Adding side `A` = the opposite.

```
              ┌ S                                            |q| ≤ inv_soft
size_A(q) =   │ S · (1 − (|q| − inv_soft)/(inv_hard − inv_soft))   inv_soft < |q| ≤ inv_hard
              └ 0                                            |q| > inv_hard

size_R(q) = min( |q| , S_max , funded(market) )
```

Both quoted at their own side's touch.

- The taper is **linear and continuous** so a single contract never flips the
  whole quote on and off. A cliff produces order churn against the rate limit
  for no scoring benefit.

> **H-Q-5a — The reducer never overshoots flat.** `size_R` is capped at `|q|`.
> Every positive partial fill must **strictly decrease** `|q|`, and a full fill
> must produce **exactly zero**. There is no `q` and no fill sequence for which a
> reducing order can change the sign of `q`.
>
> **This corrects a defect in an earlier version of this spec** (red-team HR-003),
> which sized the reducer at `|q| + S` and claimed that a full fill "lands us at
> `−S` — a normal-sized position … the system is self-correcting." That claim was
> false against §10.3's own parameters. With `S = 100` and `inv_hard = 60`,
> `q = +61` posted 161 contracts; a full fill gave `q = −100`, which is **above
> `inv_hard`** and therefore immediately back in `REDUCING` — now posting 200.
> Full fills alternate `−100, +100` indefinitely, and `REDUCING` exits only at
> exactly zero. The designed success case re-entered the failure state, because
> `S > inv_hard`. A reducer sized `|q| + S` is not a reducing quote; it is a
> reducing quote of `|q|` plus an **adding** quote of `S` on the opposite side,
> which is precisely what `REDUCING` exists to switch off.
>
> Any deliberate two-sided imbalance is a `SKEWED` behaviour, specified by
> `size_A`, and never rides on the reducer.

> **H-Q-5b — Caps are aggregate, not per-order.** Every size limit in this
> document (`|q|`, `S_max`, and every capital cap) applies to the **sum of
> `RESTING` + `SENDING` + `UNKNOWN` + unconfirmed-cancel quantity** on that side
> of that market — never to a single order in isolation.
>
> Red-team HR-004: with `q = +100`, H-Q-9's place-then-cancel permitted a second
> 200-contract reducer because the momentary doubled size was exactly
> `S_max = 400` and the notional only ~$122. If the cancel was slow or ambiguous
> and both filled, `q` went to `−300`. The same hole sat inside `SETTLING`, where
> two individually `|q|`-capped reducers could both fill and flip `+100` to
> `−100`, falsifying H-CLOSE-2's "it cannot flip us."
>
> Consequently **`REDUCING` and `SETTLING` use cancel-confirm-place** whenever an
> overlap could exceed `|q|`. H-Q-9's place-then-cancel is confined to the
> **adding** side, where overshoot is bounded by `size_A` by construction.

- **The reducing quote is a scoring quote, but it is not a free one.** It sits at
  the touch, inside the qualifying walk, and by S3 it earns full weight *on the
  side it quotes*. It earns **nothing on the side it abandons**, and
  `combined_share` averages the two sides. Measured against `probescore.py`:
  two-sided 100/100 into a field of 100/100 earns `(50% + 50%)/2 = 50%`; a
  one-sided 200-contract reducer earns `(0 + 66.7%)/2 = 33.3%`.

> **Skewing to flat costs roughly a third of the market's reward rate for as long
> as it lasts.** An earlier version of this spec claimed it "costs nothing in
> reward terms" and cited S3 for it (red-team HR-025). S3 says a one-sided quoter
> scores at full weight *on the side it quotes* — it says nothing about the absent
> side, and the absent side is half the average. If the remaining field alone
> misses Target Size on the abandoned side, the snapshot gates out entirely and
> the market pays **nobody**, which is worse still.
>
> This does not change S1 — that is derived from population settlement data and
> stands. It does mean `inv_soft` and `inv_hard` are a **real economic tradeoff**
> (time skewed is time at ~2/3 revenue), not the free action the earlier text
> described, and it is why H-Q-5a caps the reducer at `|q|` rather than
> overshooting: overshoot buys nothing and extends the one-sided interval.

### 6.3 Reduction is unconditional

**H-Q-5 — There is no loss guard on the reducing quote.** `probebot.py` refused
to flatten above `max_flatten_loss_c = 6.0` cents. That guard was correct for a
*taker* exit, which pays a certain cost. It is wrong for a maker reduction: by
S1 the mid is a martingale, so reducing at the touch is EV-neutral against
holding, and it frees capital that has a positive expected use. Reduce at
whatever the market gives. Do not port the guard.

### 6.4 What happens when the reducing quote does not fill

This is the case that killed the probe, so it is specified rather than implied.

**Nothing escalates.** In order:

1. The reducing quote keeps resting, and is requoted to follow the touch under
   §6.5 like any other quote.
2. The adding side stays off (`REDUCING`).
3. Monitoring continues at full cadence. **This is not conditional on anything.**
4. If `|q| > inv_hard` has held for longer than `stuck_s`, emit an anomaly ping
   of class `INVENTORY_STUCK` (§11). This is information, not an alarm — the
   operator may choose to act; the harness will not.
5. If `|q| > inv_kill`, the *global* state goes to `WINDING_DOWN` — every
   market stops adding — and a `SEV1` ping fires. The reducing quote in the
   affected market stays live.
6. At the close lead, §8 takes over and the position settles.

**At no point does the harness cross the spread**, and at no point does it stop
observing. If the reducing quote never fills, the position settles at $0.00 or
$1.00 and that outcome is accepted (S1: it is EV-neutral against the alternative
we are forbidden from taking anyway).

### 6.5 Requote policy

- **H-Q-6** Requote only when the reference moves **against** us, leaving our
  order ≥1 tick behind the touch. Do not requote when it moves in our favour —
  our order is then the best bid and already scores at `N = 0`.
- **H-Q-7 — Debounce 250ms.** Requote only once the new touch has held for
  250ms. Bursts under 100ms are 52.6% of all moves and 0.1% of scored time;
  chasing them buys ~0.0003 of multiplier while multiplying order rate.
- **H-Q-8 — Stranded brake.** If our resting price is `stale_bid_ticks` (8) or
  more behind the external touch, snap back to the external touch regardless of
  which direction it moved. This catches the case where our own order is the
  only thing holding a price level nobody else wants.
- **H-Q-9 — Place-then-cancel on an upward requote, adding side only.** A requote
  moves our bid *up*. Placing the new order before cancelling the old one avoids
  a presence gap; both are bids on the same side and cannot cross each other.
  Presence gaps are revenue (S4), so the ordering is not cosmetic.

  Permitted **only** when all of:
  1. the new price still satisfies H-CO-6 against our other side;
  2. the momentary aggregate size is within `S_max` and the capital cap; **and**
  3. **it is the adding side.** On a reducing side, the momentary aggregate would
     exceed `|q|`, which H-Q-5a forbids — two individually-capped reducers that
     both fill flip the position (red-team HR-004). Reducing and settling sides
     use **cancel-confirm-place**: the replacement is not dispatched until the
     cancel is confirmed by response *or* sweep.

  Otherwise cancel-then-place. The presence gap on a reducing side is accepted:
  a reducer that overshoots is worse than a reducer that is briefly absent.

- **H-Q-9a — The cancel is not an independent queue entry.** On a place-then-cancel
  requote the cancel becomes **eligible only after the replacement ACKs**. §6.6
  otherwise dispatches the P0 cancel before the P3 placement and silently inverts
  this rule into cancel-then-place (red-team HR-019).
- **H-Q-10 — External book.** Every touch used for a placement decision comes
  from `external_best(side)`: the book with **our own resting size subtracted
  from its price level**. Reading the raw book means reading our own order and
  chasing ourselves. `probebot.py:external_levels` is the reference behaviour.

### 6.6 The order priority queue

With N markets live, the write budget is the binding constraint, not latency.
Every intended write enters one queue with a class:

| Class | Contents | Starves? |
|---|---|---|
| **P0** | any cancel; any write in `WINDING_DOWN` | never — bypasses the bucket |
| **P1** | reducing-side placement where `\|q\| > inv_soft` | never |
| **P2** | presence restoration (a qualifying side with nothing resting) | no |
| **P3** | requote (we are ≥1 tick behind) | yes, bounded |
| **P4** | top-up after a partial fill | yes, bounded |

- P0 bypasses rate limiting entirely. A cancel of an **adding** order is the one
  write that can only reduce exposure, and it must never be queued behind a
  requote.
- **Anti-starvation:** any queued intent older than `max_queue_age` (30s) is
  promoted one class. An intent whose triggering condition is no longer true is
  dropped, not sent — the queue holds *intents*, re-evaluated at dequeue, not
  pre-built requests.

**H-QUE-1 — Dependent writes are one intent, not two.** A place-then-cancel
requote enters the queue as a **single ordered intent** whose cancel leg becomes
eligible only after the placement ACKs (H-Q-9a). Enqueuing them separately puts
the cancel at P0 and the placement at P3, which dispatches them in the exact
reverse of the mandated order (red-team HR-019).

**H-QUE-2 — Cancelling a reducer is not "risk-reducing".** P0's justification is
that a cancel can only reduce exposure. That is true of an adding-side cancel and
**false of a reducing-side cancel, which removes the exit.** A reducing-side
cancel is P1 and is only ever issued as the first leg of a
cancel-confirm-place — never on its own.

**H-QUE-3 — Reducers get reserved capacity.** P0 bypasses only the *local* token
bucket; it does not bypass the exchange's. A cancel storm can absorb 429s, occupy
every REST worker in backoff, and starve a P1 reducer indefinitely. At least one
worker slot and a reserved share of the write budget are held for P1 at all
times, and cancels are coalesced per (market, side) rather than issued per order.
V4 gains an unbounded-cancel-storm-with-waiting-reducer case.

---

## 7. Order lifecycle

### 7.1 Client order IDs

**H-ORD-1 — Deterministic, not random.**

```
coid = "lipH-{runID}-{marketIdx:03d}-{side}-{seq:08d}"
```

`runID` is a ULID-like string fixed at process start. `probebot.py` used a fresh
`uuid.uuid4()` per order, which makes a timed-out write unresolvable without a
full order scan. A deterministic, greppable coid makes reconciliation a lookup
and makes every order in `harness.db` traceable to the state that produced it.

The `lipH-` prefix is load-bearing: it is how startup adoption (§7.5)
distinguishes our orders from anything else on the account.

### 7.2 The write protocol

```
INTENT  ──dequeue──►  SENDING  ──2xx──►  ACKED  ──►  RESTING
                          │
                          ├──4xx (definite)──►  REJECTED
                          │
                          └──timeout / 5xx / conn error──►  UNKNOWN
```

**H-ORD-2 — The harness never retries a write it cannot prove did not happen.**

This is the single most dangerous path in the whole system and it is specified
tightly. On any ambiguous outcome — request timeout, connection reset, 5xx, or
any response the client cannot parse — the order goes to `UNKNOWN`:

1. It is **not** retried. Not with the same coid, not with a new one.
2. A `RECONCILE_NOW` is scheduled immediately.
3. Reconciliation resolves it by searching for the coid in
   `GET /portfolio/orders` (all statuses) and `GET /portfolio/fills`, each read
   as a **complete cursor walk** (H-PAGE-1).
   - found resting → adopt as `RESTING`
   - found filled/cancelled → record the terminal state
   - **not found → it stays `UNKNOWN`.** There is no negative-evidence branch.
4. While any order in a market is `UNKNOWN`, that market places **no new
   orders** on that side. It may still cancel (P0).
5. An `UNKNOWN` unresolved for `unknown_ping_s` (120s) emits `SEV2`.
6. An `UNKNOWN` order's **maximum possibly-live quantity stays in the risk model
   and in every aggregate cap** (H-Q-5b, H-CAP-7) until it is positively
   resolved.

**H-ORD-2a — Absence is not evidence. The "declare it never landed" branch is
deleted.**

The earlier protocol resolved an `UNKNOWN` create by failing to find it twice,
`unknown_resolve_s` apart, and then permitting a requeue. Red-team HR-006 broke
this, and the break is not theoretical:

- A single-page read cannot establish absence. `kalshi.py:160-175` — the only
  verified-correct client in this repository — exposes `positions()`, `orders()`
  and `fills()` as one-shot `limit=200` calls **with no cursor loop at all**. An
  order that filled and sits at item 201 reads as absent, twice, and gets
  reissued. Inventory doubles.
- Even an exhaustive walk cannot establish absence without a documented
  snapshot-consistency and visibility contract, which is not known to exist. An
  eventually-consistent endpoint that takes longer than `unknown_resolve_s` to
  show a fill produces the identical failure.

**This repository has already been bitten by exactly this.**
`probebot.py:1100-1108` carries a written incident report: the count of active
programs grew past 1,000 mid-probe, a one-shot `limit=200` fetch pushed the live
market outside the window, and a restart died claiming "no active liquidity
program" while the program was still running. Its own conclusion:

> *"A lookup that silently depends on position in an unsorted list is a
> correctness bug, not a tuning parameter."*

**The claim that this protocol "is correct under either answer" to V1.7 was
therefore false**, and is withdrawn. If Kalshi does not dedupe on
`client_order_id` *and* absence is unprovable, requeue duplicates inventory —
which is the most expensive single mistake available to this system.

**H-ORD-2b — Ambiguous creates are retried with the SAME coid, never a new one.**

> **V1.7 answered 2026-08-04 by live minimum-size experiment: Kalshi DOES dedupe
> on `client_order_id`.** Two creates with an identical coid, 1 contract, YES @
> 2c, `post_only`, in `KXALBUMRELEASEDATE-NEWDON-27OCT01`:
>
> ```
> create #1 → 200  {order_id, client_order_id, fill_count "0.00",
>                   remaining_count "1.00", ts_ms}
> create #2 → 409  {"error":{"code":"order_already_exists",
>                            "message":"order already exists"}}
> resting orders bearing that coid: exactly 1
> cancel → reduced_by "1.00";  sweep → clean
> ```

This is the good outcome, and it **resolves HR-006 rather than merely
constraining around it.** The `UNKNOWN` protocol becomes self-resolving:

1. On an ambiguous create, **retry with the same coid.**
2. `2xx` → the original never landed; this one is now the order.
3. **`409 order_already_exists` → the original DID land.** The 409 is a
   *positive identification*, not an absence — so we never have to prove a
   negative, and H-PAGE-1's pagination trap cannot produce a duplicate.
4. Either way the outcome is definite in one round trip, and **at most one order
   can exist per coid** — exactly what H-ORD-1's deterministic coid was designed
   to buy.

**This is why H-ORD-1 forbids `uuid4()` per attempt.** `probebot.py` generated a
fresh UUID per order, which makes this mechanism unavailable: a retry under a new
coid *is* a genuinely new order, and the exchange cannot report it as a duplicate
because it is not one. The deterministic coid was specified for greppability; it
turns out to be the thing that makes ambiguous writes recoverable at all.

**Two constraints on relying on it:**

- **Observed, not documented.** Confirmed empirically, one account, one market,
  one day. So A10 still forbids retrying under a *new* coid, `RECONCILE_NOW` is
  still scheduled, and a 409 is still followed by a confirming read — belt and
  braces, because the cost of being wrong is double inventory.
- **`retry_same_coid_max` = 3**, then the order stays `UNKNOWN` and escalates. A
  retry loop against a persistently ambiguous endpoint is its own failure mode.

The "declare it never landed after two negative reads" branch stays deleted
regardless: it was unsound (HR-006), and it is now also unnecessary.

### 7.3 Amend

**H-ORD-3 — There is no amend.** The primitive is cancel-then-place, or
place-then-cancel under H-Q-9. `probebot.py` had no amend path and neither does
this. If an amend endpoint is later confirmed to exist and to preserve queue
position, it is a follow-on optimisation with its own gate — not an assumption
in v1.

### 7.4 Cancel

`DELETE` returns `reduced_by`. **A cancel can be partial.** Treat
`reduced_by < requested` as "the remainder may have filled between our decision
and the exchange's" — do not treat it as an error, do reconcile immediately, and
do expect the position may have moved.

**H-ORD-4a — Cancels are retriable; creates are not. `reduced_by` is not a fill
report.**

H-ORD-2 forbids retrying any write whose outcome is unknown. H-ORD-4 requires a
sweep that still finds our order to "retry once". **A DELETE is a write, so as
written the two rules contradicted each other** (red-team HR-020). The
distinction that resolves it:

- A duplicate **create** can double inventory. Non-retriable (H-ORD-2a).
- A duplicate **cancel** is idempotent in effect and strictly risk-decreasing.
  **Explicitly retriable**, and this is stated as the exception rather than left
  to be inferred.

And the trap that made the contradiction dangerous rather than merely untidy:

> **`reduced_by` proves only what that particular DELETE removed. It never
> implies the position moved.** Comparing a *retry's* `reduced_by = 0` (or a 404)
> against the *original* requested size reads as "the remainder filled" — when
> the true explanation is that the first DELETE already cancelled it and `q`
> never moved at all. Position changes come from `/portfolio/positions` and
> `/portfolio/fills`, never from a cancel response.

Until reconciliation confirms otherwise, the **maximum possibly-live quantity**
stays in the risk model (H-Q-5b, H-CAP-7).

**H-ORD-4 — Cancel-all is verified, not assumed.** After issuing cancels,
re-read `GET /portfolio/orders?status=resting` and confirm nothing of ours
remains. `probebot.py` did this ("sweep") and it is the only thing that turns a
cancel into a fact. A sweep that still finds our orders retries once, then pings
`SEV1`.

### 7.5 Startup adoption — the exchange is authoritative

**H-ORD-5 — Reconcile before quoting. Always. No exceptions.**

`STARTING` runs, in this order, and does not place a single order until all of
it succeeds:

1. `GET /portfolio/positions` → seed `q` for every market with a non-zero
   position, **including markets not in the selection set.**
2. `GET /portfolio/orders?status=resting` → every resting order on the account.
3. `GET /portfolio/fills` → paginate back `backfill_h` (24h) into `our_fill`.
4. `GET /portfolio/balance` → seed the capital model.
5. Classify every resting order:
   - coid matches `lipH-*` → **adopt** into the state machine at its stated
     price and size.
   - coid does not match → **not ours.** Do not cancel it. Ping `SEV2`
     (`FOREIGN_ORDER`) and exclude that market from selection — an account with
     unexplained orders on it is an account whose position model we cannot trust.
     **Foreign activity appearing *after* startup latches global `WINDING_DOWN`**
     (H-ORD-9), because by then it is not a pre-existing condition we adopted but
     a live third party trading the account we are modelling.
6. Any market with `q ≠ 0` enters at `REDUCING`, **not** `QUOTING`, regardless
   of size. Adopted inventory is inventory whose provenance we do not know.
7. Emit a `STARTUP` ping summarising adopted position and orders.

If any of steps 1–4 fails after `startup_retries` (3), the harness enters
**`UNKNOWN_RISK`**, pings `SEV1`, and **retries indefinitely with backoff**. It
does **not** proceed to quote against an unknown position, and it does **not**
exit.

> **H-ORD-5a — Ignorance is not a halt state.** The earlier rule sent this case
> to `WINDING_DOWN`. Red-team HR-008: `WINDING_DOWN`'s entire purpose is keeping
> the *reducing* quote alive, and a reducing quote requires knowing `q` — which
> side to quote and how much. Entered from a failed position read, it has no `q`,
> so it can do nothing at all, and no continuing reconciliation was specified. It
> would simply sit there with inventory it could not see. `UNKNOWN_RISK` is a
> distinct state whose only job is to **keep trying to find out**, forever, while
> placing nothing.

**H-ORD-5b — The managed set is not the selected set.** Markets under management
are the **union** of:

- every selected market,
- **every market with a non-zero position**, and
- **every market with a resting, sending or unknown `lipH-` order**,

**irrespective of LIP program status, selection rank, or `core.Rig` universe
membership.** The harness subscribes to whatever it holds.

> Red-team HR-008, second half: a market whose LIP program has ended but whose
> market is still open can hold inventory, and need not appear in the `core.Rig`
> universe — which is fixed at subscribe time. `REDUCING` with no book to quote
> against is not an exit. H-SEL-11 said a held market is never *deselected*; it
> did not say a held market is always *subscribed*.

**H-ORD-5c — Adopted adding orders are validated, not just inherited.** Before
leaving `STARTING`, every adopted `lipH-` order that is invalid under current
selection, close, size, price or capital rules is **cancelled and swept**
(H-ORD-4). Adoption preserves orders we would place today; it is not a licence
for a prior incarnation's orders to persist unexamined — including an adding
order in a market that is now flat and unselected.

### 7.6 Fill attribution — the `source='ours'` path

Attribution is a **join on `trade_id`**, not a heuristic.

- `GET /portfolio/fills` is the authoritative record of our fills.

> **V1.8 answered 2026-08-04, read-only against the account's own 15 historical
> fills.** The fill object's keys are, verbatim:
>
> ```
> action  book_side  count_fp  created_time  fee_cost  fill_id  is_taker
> market_ticker  no_price_dollars  order_id  outcome_side  side
> subaccount_number  ticker  trade_id  ts  yes_price_dollars
> ```
>
> **`trade_id` is present, non-null and unique on all 15 fills** (sample
> `1582884a-1f64-7c20-fab4-b40468ad5df6`). **H-ORD-6's join key exists and §7.6
> stands unchanged.** Confirmed in the same response:
>
> | Key | Confirms |
> |---|---|
> | `trade_id` | H-ORD-6 — the `rig.db` join, and `our_fill`'s primary key |
> | `is_taker`, `fee_cost` | H-ORD-8 / F14 — the taker detector and its independent corroborator (S2) |
> | `order_id` | H-ORD-9 — ownership classification against the ledger |
> | `count_fp` | H-CO-4 — counts are fixed-point, not integers |
> | `fill_id` | a second identity, distinct from `trade_id`; `trade_id` remains the key because it is the one the public `trade` stream also carries |
> | `cursor` (top level) | H-PAGE-1 — cursor pagination exists and is the mechanism to walk |
- The public `trade` websocket stream carries the same `trade_id`. `core`
  already writes it as `fill.trade_id` in `rig.db`.
- Therefore: a `rig.db` fill row is ours iff its `trade_id` appears in
  `harness.db.our_fill`.

**H-ORD-6** — `harness.db.our_fill` has `PRIMARY KEY (trade_id)` and is written
`INSERT OR IGNORE`. `probe.db.our_fill` was incomplete (8 rows against 12 real
fills) and mis-attributed `run_id` because run 3 backfilled historical fills
under its own id. The fix is structural: fills are keyed by their own identity,
`run_id` is recorded as "the run that first observed this fill" with a separate
`backfilled` boolean, and it is never used as a filter for correctness.

**H-ORD-7 — The harness does not write to `rig.db`.** Two collectors already
write it, SQLite has one writer, and a third live writer is a contention and
corruption risk for the evidence base. The `source='ours'` marking is done
offline by `scripts/importfills.py`, a single-writer, opt-in, idempotent step
that `UPDATE fill SET source='ours' WHERE trade_id IN (...)`. `store.go`'s
hard-coded `'observed'` INSERT stays as it is.

**H-ORD-9 — Ownership is a ledger, not an inference, and foreign activity is
global.**

Red-team HR-024 found two collisions the spec could not resolve as written:

1. **F15 vs H-SEL-11.** F15 excludes a market with a foreign order from
   selection; H-SEL-11 says a market with `q ≠ 0` is *never* deselected. A manual
   fill makes both apply to the same market at once. Exclusion also does not
   remove the exposure — it only stops us managing it.
2. **F14 misfires.** A manual *taker* fill either lands in `our_fill` and
   **falsely trips F14's global halt**, or is filtered out and leaves a hole in
   the ownership record. There was no way to tell the two apart.

Therefore:

- `harness.db` keeps a **durable ownership ledger** covering every coid the
  harness has ever sent, **including terminal orders**. Fills are classified by
  `order_id` against that ledger — never by heuristic, never by `run_id`.
- A fill whose `order_id` is not in the ledger is **foreign**. It does not enter
  `our_fill`, does not trigger F14, and does trigger **`SEV1 FOREIGN_FILL` and
  global `WINDING_DOWN`** — someone else is trading the account our position
  model describes, and that model is now unreliable everywhere, not in one
  market.
- H-SEL-11 wins over F15 on the exclusion question: a market with `q ≠ 0` stays
  under management in `REDUCING`. Exclusion applies to *new selection*, not to
  abandoning inventory.

> **The cheaper fix is operational, and is recommended:** run the harness on a
> **dedicated account with no manual activity**, and assert at startup and on
> every poll that no foreign order or fill exists. That converts this entire
> class from a modelling problem into a precondition. The ledger is specified
> because the assertion can fail, not because the assertion is optional.

**H-ORD-8 — `is_taker` must be false on every one of our fills, forever.** A
single fill with `is_taker = true` means H-Q-3 has been violated by something —
a `post_only` that did not take effect, a marketable price, an API change. It is
an immediate `SEV1` ping and global `WINDING_DOWN`. This is the cheapest
possible detector for the most expensive possible bug, and `fee_cost > 0` on any
fill is the independent corroborator (S2).

---

## 8. Position monitoring

### 8.1 What "position" means

Per market, exactly one number: `q`, signed float64 net contracts,
YES-positive. `q > 0` is long YES; `q < 0` is long NO. Kalshi nets offsetting
holdings on the spot (S5), so there is no separate "YES holding" and "NO
holding" to track — a matched pair self-liquidates and leaves `q` unchanged.

### 8.2 Two derivations, always both

| | Source | Cadence | Role |
|---|---|---|---|
| `q_local` | incrementally updated from our own acks and observed fills | every event | reaction speed |
| `q_exch` | `GET /portfolio/positions` | `position_poll_s` = **5s** | truth |

**H-POS-1 — The exchange is authoritative and overwrites.** On every poll,
`q_local := q_exch`. `probebot.py` did this ("the exchange is authoritative")
and it is correct. `q_local` exists so we can react between polls, not so we can
argue with the exchange.

**H-POS-2 — Every disagreement is recorded, and a sustained one is an
anomaly.** Write `(ts, ticker, q_local, q_exch, delta, agreed)` to
`position_poll` on every poll — including agreements, because a table with only
disagreements in it cannot distinguish "no drift" from "not polling".

- `|delta| > 0` for one poll: normal (a fill landed between events). Record.
- `|delta| > pos_drift_tol` (0.01 contracts) for **two consecutive polls**:
  `SEV2` ping (`POSITION_DRIFT`), market → `REDUCING`.
- `|delta| > pos_drift_hard` (5 contracts) at any single poll: `SEV1`, global
  `WINDING_DOWN`. Our model of our own risk is wrong; stop adding to it.

**H-POS-3 — `/portfolio/positions` is O(1) in *requests*, not in *pages*.** One
endpoint returns every market, so do not poll per-market — but one endpoint is
not one response. The same holds for `/portfolio/orders` and `/portfolio/fills`.
Position monitoring cost does not scale with market count, which is what makes
N-market operation viable at a 5s cadence; it *does* scale with account history,
which is what H-PAGE-1 exists for.

**H-PAGE-1 — Every list read is a complete cursor walk, or it is stale, never
empty.**

This binds `/portfolio/positions`, `/portfolio/orders`, `/portfolio/fills`,
`/incentive_programs`, and every other paginated endpoint the harness reads.

1. Walk the cursor to exhaustion. A single page is never treated as a complete
   answer, whatever its length, and `limit` is never used as a substitute for
   pagination.
2. **State is replaced only after a full walk succeeds.** A failure at page *k*
   **preserves the prior state and marks it stale**, which starts that endpoint's
   freshness clock running against `truth_max_age_s` (H-FAIL-4). It never
   produces an empty or partial replacement.
3. "Not present in the response" means "not present in a **complete** response",
   and nothing else.
4. Ordering, retention, deduplication and concurrent-insert behaviour are
   established per endpoint by V1.8a and recorded, not assumed.

> **V1.8a answered 2026-08-04/05, read-only (169 GETs, 0 writes). Full record:
> `notes/pagecontract.md`; raw report `notes/pagecontract.json`.** The measured
> contract, in brief:
>
> | Endpoint | Cursor field | Item key(s) |
> |---|---|---|
> | `/portfolio/positions` | `cursor` | `market_positions` **and** `event_positions` |
> | `/portfolio/orders` | `cursor` | `orders` |
> | `/portfolio/fills` | `cursor` | `fills` |
> | `/incentive_programs` | **`next_cursor`** | `incentive_programs` |
>
> **There is no single contract, and this repository already held both halves of
> the answer while believing each was universal:** `probebot.py:1123` reads
> `next_cursor`, `scripts/opportunity.py:54` reads `cursor`. Each is correct for
> its endpoint and silently wrong for the other — a reader using the wrong key
> sees an empty cursor, concludes the walk finished, and returns page one as the
> whole answer.
>
> - **Cursors are composite keyset cursors `(sort_key, tiebreak_id)`, not
>   offsets** — decoded from protobuf; the portfolio cursor embeds the last
>   record's own id (the fills cursor carried `trade_id`
>   `1582884a-…`, the exact record V1.8 sampled).
> - **Concurrent inserts are safe, measured not inferred.** Ten pages of the
>   exchange-wide `/markets/trades` feed at `limit=5` across ~20s of live churn:
>   50 records, 50 unique, zero duplicates — including two different trades
>   sharing a boundary microsecond across a page break. All lists are
>   newest-first, so a record inserted mid-walk sorts *above* the anchor and is
>   simply not seen. **A complete walk is a consistent suffix as of its start**,
>   never a torn read. The residual is staleness, not corruption, and
>   `truth_max_age_s` already bounds it.
> - **`/incentive_programs?status=active` returned 4,170 programs.**
>   `probebot.py:1104` recorded the population passing 1,000. A `limit=200`
>   single-page read now sees **4.8%** of the universe, and H-SEL-1 ranks by
>   `pool ÷ field qualifying score`, so the best market is not preferentially
>   near the front.
> - `orders`/`fills` accept `limit` **1…1000**; 0 and >1000 are HTTP 400.
>   `incentive_programs` has no observed upper bound (`limit=5000` returned all
>   4,170 in one page). `limit` may be changed mid-walk — keyset cursors do not
>   encode page size.
>
> Four things remain **unestablished and must not be assumed** (`pagecontract.md`
> §6): `positions` never paginated (one row on the account) and handles `limit`
> unlike its siblings; the two arrays it returns under one cursor may not
> paginate together; retention has no reachable horizon; and mid-walk *deletions
> and in-place updates* are untested — H-POS-4's replace-only-on-complete-walk
> already handles the last of these correctly.

**H-PAGE-1a — An invalid cursor is silently ignored, so a walk asserts its own
forward progress.**

The portfolio family **does not reject a cursor it cannot parse. It rewinds the
walk to page one and returns HTTP 200.** Measured against `/portfolio/fills`:
garbage, empty, a truncated-but-valid cursor, and a cursor minted by
`/incentive_programs` **all returned the first record**, while the valid control
advanced correctly. `/incentive_programs` behaves oppositely and more safely —
garbage is HTTP 400, a foreign cursor is HTTP 500.

Injecting one corrupted cursor at page 3 of a `limit=1` walk reproduces the
failure directly: **8 pages, 8 records, 5 unique, 3 duplicates, and no
termination.** Against `fills` that is duplicated fill attribution; against
`orders` it is a walk that never completes, so under clause 2 it never replaces
state and every truth clock runs to `truth_max_age_s`.

Therefore every cursor walk:

1. **Uses a cursor exactly as received, or abandons the walk.** A cursor is
   never repaired, truncated, re-encoded, or carried across endpoints.
2. **Asserts forward progress.** The identity of each page's first record is
   tracked; a repeat of any identity already seen in this walk means the walk
   has rewound. Abandon it, **preserve prior state and mark it stale** (clause
   2), discard the accumulated records, and emit `SEV2 CURSOR_REWIND`.
3. **Does not terminate on one array being empty** while the cursor is still
   non-empty — `positions` returns `market_positions` *and* `event_positions`
   under a single cursor.
4. **Treats an unknown filter value as a bug, not a result.** `status=cancelled`
   returns 0 rows; `status=canceled` returns 2. The endpoint does not reject an
   unrecognised status, it answers "nothing" — "absence is not evidence"
   (H-ORD-2a) reappearing as a spelling mistake. Every status string the harness
   sends is pinned in a test.

A page cap is **not** a substitute for any of this: it converts a silent
infinite loop into a silent truncation, which clause 3 forbids.

> Red-team HR-013, and this is a repeat, not a new risk: `probebot.py:1100`
> documents this exact failure in production on this account (see H-ORD-2a). The
> spec had reintroduced the pattern as the foundation of H-POS-3, H-POS-4,
> H-ORD-4, H-ORD-5 and H-ORD-8 — including **H-ORD-8's taker detection**, which
> is the cheapest detector for the most expensive bug and which silently stops
> working the moment our fills exceed one page. A 100-contract order split into
> more than 200 fractional fills does that on its own, and sizes are fractional
> in 20.5% of observed rows (H-CO-4).

**H-POS-4 — Resting orders are reconciled the same way.**
`GET /portfolio/orders?status=resting` on the same 5s tick **replaces** our
order map wholesale — but only on a complete walk (H-PAGE-1). An order we believe
is resting and a *complete* exchange response does not report is either filled or
cancelled; either way our belief is wrong and the exchange's is not. An
**incomplete** response tells us nothing and must not be allowed to retire an
order from our model.

### 8.3 The monitor loop (the I2 goroutine)

Runs at 1 Hz, independent of everything:

0. Read the published `Snapshot` pointer and compare its `seq` against the
   previous read. **If `seq` has not advanced for `owner_stall_s` (3s), the
   snapshot is stale**: emit `SEV1 OWNER_STALLED`, mark every row and heartbeat
   produced from it as `stale=1`, stop integrating it into `uptime`, and keep
   doing so until `seq` advances (H-TOP-5 / I3). Do **not** exit, and do not stop
   sampling — a stalled owner is exactly when observation matters most.
1. Read the published `Snapshot` pointer.
2. Compute, per market, our predicted LIP share from the live book and our own
   resting orders, using a Go port of `probescore.py`
   (`combined_share`/`our_side_share`). **This is not a re-derivation** — the
   Python is the reference and `scripts/checkscore.py` already verifies its
   equivalence to the frozen `score.py`; the Go port is gated against the Python
   the same way (V1.5).
3. Write one `snap` row per market: our size and price per side, field
   qualifying score per side, `gated`, and our share.
4. Integrate presence and share into `uptime`.
5. Every `heartbeat_s`, publish liveness (§11.4).

**This loop has no reference to the quote engine, the order queue, or the REST
client.** It cannot be stopped by them. It stops when the process stops, and
the process does not stop while inventory is open (I1).

**And it can tell you when what it is watching has stopped moving** (step 0).
Unstoppable and truthful are two properties, and this loop now has both.

---

## 9. Close and program-end handling

Two different deadlines, two different meanings. Both are per-market.

| Boundary | Source | Meaning |
|---|---|---|
| `program.end_date` | `/incentive_programs?status=active` | reward accrual stops |
| `market.close_time` | `/markets/{ticker}` | trading stops; position settles |

`close_time` may fall before or after `end_date`. Neither is assumed static.

**H-CLOSE-0 — Schedule polling must be faster than the shortest lead it
enforces.** `schedule_poll_s` is **30s**, not 300s, and a market-status event
stream is consumed in preference to polling where one exists. A newly observed
`close_time` that is **already inside a lead** runs its catch-up actions
**synchronously and immediately**, in lead order, with the final cancel taking
precedence over everything else.

> Red-team HR-017: at `schedule_poll_s = 300` with `final_lead = 60s`, a
> `close_time` that moves from 17:00 to 12:03 at 12:00:01 is next observed at
> 12:05 — after the close. **Neither the close lead nor the final cancel ever
> ran**, and orders rested into the close. This is not `can_close_early`; it is
> an ordinary schedule update. A poll interval five times longer than the lead it
> is supposed to trigger cannot enforce that lead.

### H-CLOSE-1 — Program end

At `end_date`, the market goes to `REDUCING`. There is no reward left to earn,
so the adding quote has no purpose. The reducing quote is now genuinely free —
there is no reward left for it to forfeit (§6.2's one-sided cost applies only
while the program is paying) — and it is still our only exit. It stays.

### H-CLOSE-2 — The close lead

At `close_time − close_lead` (default **4h**):

- The adding side is cancelled if it is not already, and **exchange-confirmed
  absent**.
- The market goes to `SETTLING`.
- **The reducing quote remains, aggregate-capped at `|q|`** (H-Q-5a, H-Q-5b) — it
  cannot flip us, so it takes no new inventory, which is what decision 2 forbids.
- Remaining inventory settles. The forfeited reward during this window is
  accepted.

**H-CLOSE-2a — `SETTLING` is not an exemption from having an exit.** Until
`final_lead`, a market in `SETTLING` with `q ≠ 0` **must** have a reducing quote
resting or an in-flight intent to place one, of aggregate quantity ≤ `|q|`. A4's
`SETTLING` exemption begins at `final_lead`, trading close, or `q = 0` —
**never at `SETTLING` entry.**

> Red-team HR-001, and the single highest-value finding of the round. The
> reviewer was asked to name a mutation that survives every gate in §17 and did:
>
> > **M13 — on entry to `SETTLING`, cancel every order and never place the capped
> > reducer.** With `q = +80` and a 16:00 close, this abandons inventory for four
> > hours — `probebot.py`'s exact defect, restricted to the close window.
>
> Every gate passed it. **A4 expressly exempted `SETTLING`** ("…*or is in
> `SETTLING`*"). A5 kept receiving rows. V1.10 tested deadline arithmetic, not
> resulting orders. V4 had no close-action fault. V5's list did not contain it.
> V6 is a zero-write run. V7 is one market at minimum size and need not cross
> `close_lead` at all.
>
> The exemption was written to acknowledge that inventory settles at close. It
> opened four hours early and covered the entire window in which the exit still
> existed. **M13 is now mandatory in V5** and a full close-lifecycle test is
> mandatory in V1 and V7.

> **Interpretation flagged for review — now with a named evidence gap.**
> Decision 2 as stated is "stop quoting at a fixed lead time before market close
> and let remaining inventory settle." Read strictly, that cancels everything
> including the reducing quote. This spec keeps a reducing quote capped at `|q|`,
> on the reasoning that (a) it can only move us toward flat and so takes no new
> inventory, and (b) cancelling it is the exact shape of the probe's defect —
> removing the exit and keeping the risk.
>
> **The former reason (b), "it is free by S3", is withdrawn** (HR-025), and a
> third consideration now cuts the other way (HR-011):
>
> **S1 does not cover this case.** `holdvsflat.py` measured
> `E[settlement − mid_5m]` **unconditionally**. It did not measure settlement
> value *conditional on our maker order having been selected* — which is the only
> case a resting reducer ever experiences. With `q = +100` and the outcome
> publicly knowable while the book still shows YES 49 / NO 50, our NO bid at 50
> is hit **precisely when YES is winning**: we surrender $100 of settlement value
> for $50. Zero maker fees remove the cost of trading, not the selection on
> *whether* we trade. §3a already measured at-touch fills as completely adversely
> selected under normal conditions; near resolution the asymmetry is strictly
> worse.
>
> **Resolution: `close_lead_keep_reducing` stays `true`, and `close_lead` is cut
> from 4h to 60m.** Both settings have a real failure mode:
>
> | Setting | Failure mode |
> |---|---|
> | `true` | the reducer is an option written to informed takers near a knowable outcome |
> | `false` (strict decision 2) | structurally the probe's defect — the exit is removed and the risk kept |
>
> Three reasons for keeping the exit and shortening the window instead of picking
> a side:
>
> 1. **HR-011's argument does not stop at the close window.** A resting reducer is
>    adversely selected *at all times* — `reeval-verdict.md` §3a measured at-touch
>    fills as completely adversely selected in normal conditions. Taken to its
>    conclusion, the argument indicts the reducing quote in general, which is the
>    mechanism S1 requires and the entire design rests on. We already accept that
>    trade because the alternative (crossing) costs 2.4–2.8c with certainty. What
>    changes near resolution is the *magnitude*, not the sign — so the
>    proportionate response is to shorten the exposure, not to delete the exit.
> 2. **The reducer follows the touch.** H-Q-6 and H-Q-8 requote as the reference
>    moves. If YES becomes certain, the NO touch collapses and our NO bid follows
>    it down — we buy NO at 2c, not 50c. HR-011's $100-for-$50 scenario needs the
>    book to be *stale* while the outcome is knowable, i.e. an informed taker
>    beating our requote. Real, but a narrower window than "the last four hours".
> 3. **`false` removes the exit, and that failure mode has already cost real
>    money on this account.** Between a bounded, measurable adverse-selection cost
>    and the exact structural defect the harness exists to eliminate, the first is
>    the better risk to hold.
>
> **`close_lead` 4h → 60m** cuts exposure to the stale-book window by ~75% while
> leaving the exit alive throughout. Nothing depends on 4h; it was a round number.
>
> **This is a judgement call made on argument, not evidence, and it is
> instrumented rather than asserted.** V7 records the fill-conditioned markout of
> every reducer fill by time-to-close, so the next revision decides it with data.
> That is operational telemetry on our own fills — not an economics probe, and so
> not what §18 excludes. Full reasoning in `notes/harness-redteam.md` §5.

### H-CLOSE-3 — Final cancel

At `close_time − final_lead` (default **60s**), cancel everything in that
market and verify with a sweep (H-ORD-4). Nothing of ours rests into the close.

### H-CLOSE-4 — Early close

Markets with `can_close_early: true` can settle before `close_time`, so the lead
is not a guarantee. This is accepted settlement risk under decision 2. Selection
(§10) **prefers** markets without it, but does not exclude them — the thin books
where the opportunity lives (S6) frequently have it.

---

## 10. Market selection and capital

### 10.1 Selection

**H-SEL-1 — Rank by `pool ÷ field qualifying score`, not by pool.** DF = 0.5
crushes depth more than a few ticks behind the touch, so a market's total
qualifying score can be tiny even when its book is large. The audited example
`KXUST10AD-26AUG05-T4.65`: pool $80, target 1000, field qualifying score 34.41
(yes) / 17.30 (no) — 400 contracts at the touch buys share 0.92/0.96. The
probe's market had a field score of ~2,450, **70× thicker**, for less pool. It
picked the wrong market.

Filters, all mandatory:

| # | Filter | Value |
|---|---|---|
| H-SEL-2 | `/incentive_programs` fetched **with `status=active`** | omitting it returns a different, wrong set — this is what made the prior "economics are thin" claim wrong |
| H-SEL-3 | elapsed fraction of the program period | ≤ 25% |
| H-SEL-4 | both sides qualify now, and still qualify after adding our size | `core.Book.Qualifies()` |
| H-SEL-5 | depth slack on the thinner side | ≥ 1.3 × Target Size |
| H-SEL-6 | mid | 10c – 90c |
| H-SEL-7 | `yes_bid + no_bid` | ≤ 99 (required for both sides to rest, and what makes a matched fill profitable — S5) |
| H-SEL-8 | market settles within | `max_tenor_d` (30 days) — a one-sided fill in a Jan-2028 market is an 18-month directional hold |
| H-SEL-9 | predicted share | ≤ 50%. **Above ~50% is a red flag, not an opportunity** — it means the qualifying depth is far behind the touch and something unmodelled is holding others out. |

**H-SEL-10 — Re-selection is slow and hysteretic.** Re-rank every
`reselect_s` (3600s). A market already held is only dropped if it falls
`hysteresis` (30%) below the marginal candidate — churning markets means
repeatedly paying entry and re-accruing presence from zero, and presence is the
whole revenue model (S4).

**H-SEL-11 — A market with `q ≠ 0` is never deselected.** It goes to `REDUCING`
and stays under management until flat or closed. Deselecting a market we hold is
literally the probe's bug.

### 10.2 Capital

Collateral for a two-sided quote of `S` contracts per side at book prices
`p_yes`, `p_no`:

```
capital(market) = S · (p_yes + p_no) / 100        dollars
```

Upper bound, and conservative: Kalshi may net more favourably, and we do not
depend on it doing so.

| # | Rule | |
|---|---|---|
| H-CAP-1 | global deployed capital ≤ `capital_max` | hard, checked before every placement |
| H-CAP-2 | per-market ≤ `capital_max / n_markets · concentration` | `concentration` = 2.0. **Adding quotes only** — see H-CAP-6 |
| H-CAP-3 | reserve `capital_reserve` (25%) unallocated | so a fill never strands the reducing quote |
| H-CAP-4 | **reducing quotes are funded before adding quotes, globally** | if capital is short, the adding side is what gets cut. Always. |
| H-CAP-5 | an `insufficient_balance` reject is a **correctness failure**, not a market condition | our accounting is wrong → `SEV1`, global `WINDING_DOWN` |
| H-CAP-6 | **the concentration cap never applies to a risk-reducing order** once that market's adding orders are cancelled | H-CAP-2 is a diversification rule for *taking* risk. Applying it to the exit is the probe's defect reached by arithmetic. |
| H-CAP-7 | collateral counts **positions plus every `RESTING`, `SENDING` and `UNKNOWN` order**, reserved atomically **before dispatch** | K REST workers must not each approve against the same committed total |
| H-CAP-8 | **a configuration that cannot fund the reducer for the worst permitted simultaneous fill set is rejected at startup** | checked as arithmetic, not discovered at fill time |

> **Why H-CAP-6 and H-CAP-8 exist.** Red-team HR-005. Under §10.3's own defaults
> the per-market cap is `500/6 × 2 = $166.67`. A `q = −100` position needing a
> 100-contract YES reducer with YES at 89c requires `$89` — fine. But the *earlier*
> `|q| + S` sizing needed 200 contracts and `$178`, **exceeding the per-market cap
> and the entire $125 reserve**, so the only exit was unfundable under the
> harness's own opening configuration, even after cancelling everything else.
> H-Q-5a's cap at `|q|` removes most of this; H-CAP-6 removes the rest.
>
> Six simultaneous one-sided fills remain the binding case: one $125 reserve
> cannot fund six opposing reducers, and `WINDING_DOWN` does not create
> collateral. H-CAP-8 makes that a startup arithmetic check — if
> `n_markets · S · max_price` cannot be funded from `capital_max`, the harness
> refuses to start rather than discovering it while holding inventory.
>
> **A capital rule that forbids the exit is the same defect as the probe's,
> arrived at from the opposite direction.** The probe removed the exit
> deliberately; this configuration removed it by arithmetic.

### 10.3 First live configuration

Several markets live from day one, per the operator's decision. Recommended
opening configuration, to be raised only after §14's gates are green:

```
n_markets      = 6
capital_max    = $500
S              = 100 contracts/side
inv_soft       = 25    inv_hard = 60    inv_kill = 150
```

The capacity model puts $500 at ~$596/day gross across 38 markets, so 6 markets
is deliberately well inside the modelled curve. Those figures are **modelled,
not verified** (`reeval-verdict.md` §5 lists five specific reasons they may not
hold, competitive response first). Nothing in this harness depends on them being
right; they set an opening size, not an expectation.

### 10.4 Restart from `WINDING_DOWN`

Only by operator action: remove the sentinel file `harness.resume.ok` guard and
restart the process. The harness never self-clears a global halt. It also never
requires a restart to keep managing inventory — that is the whole point of
`WINDING_DOWN` being a live state.

---

## 11. Failure modes

Every row: how it is detected, what the harness does, and whether it pings. "Adds
no risk" always means the reducing quote survives.

| # | Failure | Detection | Response | Ping |
|---|---|---|---|---|
| F1 | **Half-open socket** (hung 1,268s in probe run 1) | client **ping every 10s, pong expected within 5s**; plus a 60s read deadline as backstop | close, reconnect with backoff (1s doubling to 60s), full resnapshot, `RECONCILE_NOW` | `SEV2` if > 60s |
| F2 | **Clean disconnect** (close 1000/1001) | `IsCleanClose` | reconnect immediately; **book is quarantined as non-actionable until resnapshot + portfolio reconcile complete** (H-FAIL-5) | none |
| F3 | **Abnormal disconnect** | any other close/error | backoff, then reset book state and resnapshot | `SEV2` if > 60s |
| F4 | **Disconnect > `disconnect_halt_s` (60s)** | wall clock since last connected | affected markets → `REDUCING`; **cancels are still attempted over REST**, which is a separate transport | `SEV1` |
| F5 | **Wedged feed, single market** | book silent > 60s while socket healthy → `GET /markets/{t}/orderbook`, compare **prices *and sizes* through the full Target Size walk on both sides**, including our own expected resting size (H-FAIL-6) | agree → reset the staleness clock, keep quoting. Disagree → that market's book is replaced and quarantined; market → `REDUCING` | `SEV2` on disagree |
| F6 | **macOS DNS wedge** (system-wide, ~every 2.5h; `nslookup` still works and masks it) | any resolution failure to a host that resolved before | fall back to **last-known-good IP with SNI preserved**; cached resolutions have a floor TTL of 1h | `SEV2`, queued |
| F7 | **Host sleep / process stall** | per tick, compare wall-clock delta against monotonic delta; divergence > 5s | treat the gap as downtime, force resnapshot + `RECONCILE_NOW`, record the interval in `uptime` | `SEV2` |
| F8 | **429 rate limit** | HTTP 429 | halve the token bucket rate, exponential retry with jitter, restore rate after 60s clean | `SEV2` if sustained > 60s |
| F9 | **Order reject, definite (4xx)** | HTTP 4xx with a parseable reason | record; if reject rate > 10% over the last 50 orders **in one market**, that market → `REDUCING` | `SEV2` |
| F10 | **`insufficient_balance` reject** | reject reason | global `WINDING_DOWN` (H-CAP-5) | `SEV1` |
| F11 | **`post_only` would cross** | reject reason | our book view disagrees with the exchange's → force F5's REST cross-check on that market immediately | `SEV2` if repeated |
| F12 | **Ambiguous write (timeout/5xx)** | no parseable response | §7.2 `UNKNOWN` protocol. **No retry.** | `SEV2` after 120s unresolved |
| F13 | **Position drift** | `q_local` vs `q_exch` (§8.2) | tolerance → record; sustained → market `REDUCING`; hard → global `WINDING_DOWN` | `SEV2` / `SEV1` |
| F14 | **A taker fill of ours** (`is_taker` or `fee_cost > 0`) | fill poll | global `WINDING_DOWN` immediately | `SEV1` |
| F15 | **Foreign order on the account** | coid does not match `lipH-*` | exclude that market from selection; do **not** cancel it | `SEV2` |
| F16 | **Inventory stuck** | `\|q\| > inv_hard` for > `stuck_s` (1800s) | nothing mechanical — already `REDUCING` | `SEV2` |
| F17 | **Inventory beyond `inv_kill`** | position poll | global `WINDING_DOWN` | `SEV1` |
| F18 | **Harness process death** | absence of the heartbeat (§11.4) | external — supervision restarts it (§12); startup adoption (§7.5) recovers | operator sees missing heartbeat |
| F19 | **SQLite write failure** | error from `hstore` | **keep trading, keep monitoring**, buffer to an on-disk journal. Losing the record is not a reason to stop managing the risk. | `SEV1` |
| F20 | **Ping delivery failure** | POST error | queue and retry with backoff; never block the owner goroutine | none (it is the ping channel) |
| F21 | **Clock step** | wall vs monotonic (as F7) | recompute all deadlines from the new wall clock; never let a step skip a close lead | `SEV2` |

**H-FAIL-1 — No failure response is "exit".** Not one row above terminates the
process. The only process exits are: operator SIGINT/SIGTERM (which enters
`WINDING_DOWN` and keeps running until drained, then exits), and SIGKILL or
power loss (which are recovered by §7.5 on restart).

**H-FAIL-2 — REST and the websocket are *distinct* transports, not independent
ones.** A dead websocket does not imply a dead REST path, and cancels, position
polls and pings continue during a feed outage — F4's response depends on that.
But they share DNS, routing, TLS, credentials and the exchange itself, so a
shared failure is entirely possible and must not be modelled away.

**H-FAIL-3 — "Off" means exchange-confirmed absent, never cancel-requested.**

Everywhere this document says an adding quote is "off", "cancelled", or
"stopped" — including every row of §12 — the claim is only true once the
exchange has confirmed the order is gone, by response or by sweep (H-ORD-4).
Until then the order is **live and fillable** and its full quantity remains in
the risk model and every aggregate cap.

**H-FAIL-5 — A disconnect makes the book non-actionable, however clean it was.**

After **any** disconnect, clean or not, the book is quarantined: it may be read
for diagnostics but **no placement decision may be taken from it** until a fresh
snapshot and a portfolio reconciliation have both completed.

> Red-team HR-016. F2 previously retained state across a clean close "to match
> P25a". **P25a is a fidelity rule binding `cmd/rig`, whose job is to reproduce
> Python's measurement behaviour — it is not a safety property for a trader.**
> Concretely: YES is 50/51 before a clean code-1000 close; during a 500 ms gap it
> becomes 40/60; retained state then sends a 50 bid. `post_only` **accepts** it,
> because 50 is below the real ask — and we have improved the real touch by ten
> cents and exposed money at a price no external bid supports. A clean close tells
> us the *socket* closed politely. It says nothing about what the book did next.

**H-FAIL-6 — A book cross-check compares depth, not just the touch.**

F5's REST cross-check walks **both sides to Target Size**, comparing price *and*
size at every level, and additionally checks that our own resting size appears
where we believe it is.

> Red-team HR-015: comparing the touch price alone blesses a book that is
> price-correct and size-stale. The websocket shows YES 50×1,300 and NO 49×1,300
> with Target 1,000; it has missed a YES size delta of −1,290 and the real book is
> YES 50×10. **The touch prices agree, so F5 resets the staleness clock and keeps
> quoting** — into a market that no longer qualifies (so the reward is zero) with
> an `external_best` subtraction that is now wrong (so H-Q-10 self-chases).
> Everything F5 exists to protect depends on size, and F5 was not looking at size.
> V4.5 passed only because its injected fault happened to move a price, leaving
> the literal defect untested.

**H-FAIL-4 — Portfolio truth has a maximum age.**

Independent freshness clocks are kept for `positions`, `orders` and `fills`. When
any exceeds `truth_max_age_s` (60s):

- **all new dispatch stops** (adding and reducing alike — we cannot size a
  reducer against an unknown `q`);
- every order that cannot be confirmed cancelled is reported as
  **`SEV1 LIVE_UNCANCELLED`**, named as *unmanaged* rather than assumed dead;
- an authentication failure (401/403) is **global and immediate**, not per-market.

Where the exchange offers server-side order expiration or cancel-on-disconnect,
the harness uses it, so that a total loss of contact bounds our exposure without
requiring us to be alive to act.

> Red-team HR-010: with `q = +61`, 100 YES adding and 161 NO reducing, a shared
> DNS/routing/TLS/auth/exchange failure means we *request* the YES cancel and
> cannot confirm it. It rests, and it fills, taking `q` to +161 — while §12's
> table asserts "adding: off" and "monitoring: full". The narrower and far more
> common case is worse because it looks healthy: **public market data keeps
> flowing while portfolio endpoints return 503 or 401.** The books look live, the
> monitor keeps writing rows, and no rule bounded how old our position truth was
> allowed to get. Resting fills cannot be attributed from public trades alone
> (`core/rig.go:13-39`), so real `q` diverges from believed `q` indefinitely.

---

## 12. Halt semantics — the inversion, enumerated

Every trigger, and what the system looks like afterwards. Compare the row
"monitoring" against `probebot.py`, where every one of these produced *2
snapshots in 6.14 hours*.

| Trigger | Scope | Adding quotes | Reducing quote | Monitoring | Process |
|---|---|---|---|---|---|
| `inv_hard` breach | market | off | **live** | **full** | alive |
| `inv_kill` breach | **global** | off everywhere | **live** | **full** | alive |
| P&L ≤ `pnl_kill` (H-HALT-5) | **global** | off everywhere | **live** | **full** | alive |
| Reject rate > 10%/50 | market | off | **live** | **full** | alive |
| Feed wedge (F5) | market | off | **live** | **full** | alive |
| Disconnect > 60s | affected markets | off | **live** (cancels over REST) | **full** | alive |
| Taker fill detected | **global** | off everywhere | **live** | **full** | alive |
| `insufficient_balance` | **global** | off everywhere | **live** | **full** | alive |
| Position drift, hard | **global** | off everywhere | **live** | **full** | alive |
| Program `end_date` | market | off | **live** | **full** | alive |
| Close lead | market | off | live, capped at `\|q\|` | **full** | alive |
| Sentinel file `harness.stop` | **global** | off everywhere | **live** | **full** | alive |
| SIGINT / SIGTERM | **global** | off everywhere | **live** | **full** | alive until drained |

**H-HALT-5 — `pnl_kill` is defined algebraically or it does not exist.**

§16 sets `pnl_kill = −$75` and §12 makes it a **global** halt trigger, but the
earlier spec never defined P&L (red-team HR-021). Two conforming implementations
disagreed completely: with `q = +150` acquired at 80 and the mid at 20, a
fills-and-cost-basis reading sees −$90 and halts, while a balance-delta reading
sees nothing — and **an incoming $100 LIP reward masks the drawdown entirely**,
which is the specific way this fails silently on a system whose whole purpose is
collecting rewards.

The definition, pinned:

```
pnl = realised + unrealised
realised    = Σ over our_fill of signed cash flow, from the ownership ledger
              (H-ORD-9), fees included
unrealised  = Σ over markets of q · (mark − avg_cost) / 100
mark        = external_best mid, age ≤ pnl_mark_max_age_s (30s)
```

- **LIP rewards, deposits, withdrawals and any foreign flow are excluded.** This
  is a *trading* P&L; the reward is the thing it is measuring us against, not a
  component of it.
- Cost basis is per-market average cost from our own fills, seeded at startup
  from the backfill (§7.5 step 3).
- **If no mark of acceptable age exists, the trigger cannot be evaluated**: emit
  `SEV2` and treat it as not-fired, never as fired-or-safe by default.
- V1 gains exact-threshold tests either side of `pnl_kill`, a reward-arrival
  test proving the reward does not move `pnl`, and a stale-mark test.

If this cannot be implemented exactly as specified, **remove the trigger**. An
undefined global kill switch is worse than no global kill switch: it fires on
conditions nobody predicted and stays silent on the ones it was written for.

**H-HALT-1** — The word "halt" does not appear in the codebase. The states are
`WINDING_DOWN` and `REDUCING`. This is deliberate: `halt()` is a name that
invites the semantics that lost the money.

**H-HALT-2** — There is exactly one place in the codebase that transitions the
global state, and exactly one that transitions a market's state. Both write a
`state_event` row with the trigger. A grep for those two functions enumerates
every stop path in the system.

**H-HALT-3** — SIGTERM does not exit. It sets `WINDING_DOWN`, keeps the process
alive, and exits only when every market is flat or closed. An operator
who genuinely wants the process gone with inventory open must SIGKILL, which is
an explicit and recoverable act (§7.5), not an accident.

**`drain_timeout_h` (12h) escalates; it does not exit.** At the timeout the
harness pings `SEV1` and keeps pinging on an escalating cadence — it does not
terminate with `q ≠ 0`.

> Red-team HR-009: the earlier rule permitted exit after 12 hours **with
> inventory open**. That is I1 and H-FAIL-1 contradicted in the parameter table —
> a timer that eventually does the exact thing the whole document forbids. A
> drain timeout is evidence the operator is needed, not authority to abandon.

**H-HALT-4 — A global halt is durable and survives restart.**

The global halt state is written to a **latch that outlives the process** —
`harness.halt` on disk, written **before** the in-memory state changes, with the
trigger, timestamp and market — and to `state_event`. The file is authoritative
if the two disagree, because SQLite may be the thing that failed.

- `STARTING` reads the latch **before any placement** and, if set, enters
  `WINDING_DOWN` directly.
- **The harness never self-clears it.** Clearing is the operator action of §10.4.
- A planned exit either restarts into a latched `DRAINED` or disables
  `KeepAlive`; it never exits into a supervisor that will immediately resume
  quoting.

> Red-team HR-009, and this one is severe because the spec's own deployment rule
> causes it. Sequence: a taker fill latches global `WINDING_DOWN` (F14) — the most
> serious stop condition in the system. An unrelated panic kills the process.
> `launchd KeepAlive: true` (H-DEP-2) restarts it. `STARTING` had no notion of a
> prior halt, so flat markets resumed adding quotes. **The halt self-cleared
> without any operator action, because the supervision policy the spec mandates
> erased the safety state the spec mandates.** §10.4's `harness.resume.ok`
> sentinel governed *resuming* from a halt; nothing persisted the halt at the
> moment it was set.

> This deliberately diverges from `cmd/rig`, where SIGTERM rolls the transaction
> back to reproduce Python's unhandled-signal behaviour (port-spec §6a.3). That
> faithfulness rule binds the rig, not the harness. The harness has inventory;
> the rig does not.

---

## 13. Anomaly ping contract

### 13.1 Two legs, and only one of them can fail

- **The journal** — every anomaly is appended to `harness.db.anomaly` and to a
  plain-text file, *synchronously, before* any delivery is attempted. This leg
  cannot be lost to a network outage and is the record of record.
- **The push** — `POST https://ntfy.sh/{topic}`, from the `ping` goroutine,
  reading the journal. Best effort, retried, never blocking.

The topic is read from `~/.kalshi/env` as `NTFY_TOPIC` and is **never logged,
printed, or written to any table.** It is a bearer secret: anyone with the topic
can read the pings.

### 13.2 Severity

| Level | Meaning | Push |
|---|---|---|
| `SEV1` | risk state is wrong or unmanaged; act now | immediate, `priority: urgent`, bypasses rate limiting |
| `SEV2` | something is degraded but the risk is managed; act today | rate limited per class |
| `SEV3` | informational (startup, selection change, drain complete) | batched into the next heartbeat |

### 13.3 Rate limiting

Per `(class, market)` token bucket: **1 push per 15 minutes**, burst 1.
Suppressed pings are still journalled, and the next push in that class carries a
`(suppressed: n)` count so nothing is silently dropped.

`SEV1` bypasses the bucket entirely but is deduplicated: the same
`(class, market)` at `SEV1` re-pushes at most every 5 minutes.

### 13.4 The heartbeat — the dead-man's switch

**H-PING-1** — A `SEV3` heartbeat pushes every `heartbeat_s` (**3600s**)
carrying: global state, per-market state, `q` per market, total capital
deployed, integrated share per market, uptime fraction since start, and the
count of journalled-but-undelivered anomalies.

This exists because **an anomaly-only channel cannot report its own death.**
F18 is detected by the *absence* of a heartbeat, which is the operator's job
unless a dead-man's-switch service is added later. The heartbeat is what makes
silence informative instead of ambiguous.

### 13.5 Queue durability

Undelivered pings persist across restarts (they are rows, not memory). On
recovery from an outage the queue is drained oldest-first with each message
carrying its original timestamp, so an F6 DNS wedge produces a delayed but
complete record rather than a gap.

---

## 14. Deployment

| # | Rule | |
|---|---|---|
| H-DEP-1 | **`CGO_ENABLED=0`** | module-wide already; also means Go uses its **pure-Go resolver**, which reads `/etc/resolv.conf` directly and bypasses `getaddrinfo`/`mDNSResponder`. **Hypothesis: the harness is immune to the macOS DNS wedge (F6).** This is a hypothesis, not a fact — V4.6 tests it *during* an actual outage, because `nslookup` works throughout and masks the failure. |
| H-DEP-2 | **`launchd`, `KeepAlive: true`** | not `nohup`, not a terminal. Restart on any exit, including exit 0. |
| H-DEP-3 | **`caffeinate`** | the launchd job execs `/usr/bin/caffeinate -is <harness>`, so the assertion lives exactly as long as the process. The Mac idle-sleeps and a sleep silently voids everything (F7). |
| H-DEP-4 | stdout/stderr to a rotated file | the log is not the record; `harness.db` is. |
| H-DEP-5 | one instance only | a PID lockfile, checked at startup. Two harnesses on one account is an unrecoverable position-model conflict. |
| H-DEP-6 | credentials by path only | `~/.kalshi/kalshi.pem`, `~/.kalshi/env`. Never logged, never printed, never in a table, never in a ping. |

---

## 15. Persistence — `harness.db`

Separate from `rig.db` (H-ORD-7). Opened with `journal_mode=WAL`,
`synchronous=NORMAL`, single writer.

| Table | Key | Written when | Why it exists |
|---|---|---|---|
| `run` | `run_id` | once at start | config snapshot: every parameter in §16, verbatim |
| `market` | `(run_id, ticker, selected_ms)` | on select/deselect | pool, target, df, period, close_time, rank, reason |
| `order_intent` | `intent_id` | on enqueue | class, market state, `q`, the touch, why |
| `order_event` | `(coid, seq)` | place/ack/reject/cancel/sweep/unknown/resolved | with HTTP status and latency |
| `our_fill` | **`trade_id`** | fill poll | `INSERT OR IGNORE`; `backfilled` flag separate from `run_id` (H-ORD-6) |
| `position_poll` | `(ts_ms, ticker)` | every 5s | `q_local`, `q_exch`, `delta`, `agreed` — **including agreements** |
| `snap` | `(ts_ms, ticker)` | every 1s | our size/price per side, field score per side, `gated`, our share |
| `balance_poll` | `ts_ms` | every 60s | the payout is observable only as a balance delta |
| `state_event` | `event_id` | every transition | global and per-market, with trigger. H-HALT-2's audit trail. |
| `anomaly` | `anomaly_id` | before any delivery attempt | class, sev, text, first_ms, delivered_ms, attempts, suppressed_count |
| `uptime` | `(ticker, start_ms)` | on interval close | presence intervals, with a `gated_out` flag so market-gate exclusions are not counted as our downtime |

**H-STORE-1 — `position_poll` records agreements too.** A table containing only
disagreements cannot distinguish "no drift" from "not polling", and "not
polling" is the failure that actually happened.

**H-STORE-2 — One writer goroutine, and it is nobody's second job.**

§15 mandated a single writer while H-TOP-4 gave `hstore` to the owner, §8.3 had
the **monitor** writing `snap`/`uptime`, and §13.1 had **ping** updating
`anomaly` — three writers (red-team HR-023). Instead: a dedicated **hstore-writer
goroutine** consumes immutable records from a buffered channel. Owner, monitor
and ping all *send*; none of them writes, and none of them blocks on the write. A
full channel drops the oldest low-value rows (`snap`) and never the audit rows
(`state_event`, `anomaly`, `our_fill`, `order_event`).

**H-STORE-3 — Sample production and sample persistence are separate
properties.**

A5 asserts that the monitor **produced** a fresh, source-advanced sample. It does
**not** assert that the sample reached disk.

> This resolves a direct contradiction: F19 says keep trading and monitoring
> through a SQLite failure, while A5 (as written) required a `snap` row to have
> been *written* within 3 seconds — so a disk failure would trip A5, whose
> production response is `SEV1` + `WINDING_DOWN`, doing exactly what F19 forbids.
> A9 had the same problem: it could not record the state transition that the
> persistence failure itself caused.

On persistence failure the harness **stops adding risk**, continues reducing and
observing in memory, and **says so explicitly in the heartbeat** — a heartbeat
that silently omits "I cannot write anything down" is the ambiguity §13.4 exists
to remove. Note also that an on-disk journal is **not** a fallback for a full or
stalled disk, so the in-memory path must be the one that keeps working. V4.15 is
extended from "the store returns errors" to **lock stalls and disk-full**, which
are the failures that actually occur.

---

## 16. Parameters

Every knob, one place. All are in `run` at startup.

| Name | Default | Unit | Section |
|---|---|---|---|
| `n_markets` | 6 | count | §10.3 |
| `capital_max` | 500 | USD | H-CAP-1 |
| `capital_reserve` | 0.25 | fraction | H-CAP-3 |
| `concentration` | 2.0 | × | H-CAP-2 |
| `S` (base quote size) | 100 | contracts/side | §6.2 |
| `S_max` | 400 | contracts/side | §6.2 |
| `inv_soft` | 25 | contracts | §6.2 |
| `inv_hard` | 60 | contracts | §6.2 |
| `inv_kill` | 150 | contracts | §6.4 |
| `pnl_kill` | −75 | USD | §12 |
| `debounce_s` | 0.250 | s | H-Q-7 |
| `requote_interval_s` | 5.0 | s/side/market | §6.6 |
| `stale_bid_ticks` | 8 | ticks | H-Q-8 |
| `max_queue_age` | 30 | s | §6.6 |
| `write_rate` | 5 | writes/s global | §6.6 |
| `write_burst` | 10 | writes | §6.6 |
| `position_poll_s` | 5 | s | H-POS-1 |
| `balance_poll_s` | 60 | s | §15 |
| `schedule_poll_s` | **30** | s | H-CLOSE-0 — must be ≪ `final_lead` |
| `gate_fail_debounce_s` | 30 | s | H-Q-4a |
| `owner_stall_s` | 3 | s | H-TOP-5 / I3 |
| `truth_max_age_s` | 60 | s | H-FAIL-4 |
| `pnl_mark_max_age_s` | 30 | s | H-HALT-5 |
| `reselect_s` | 3600 | s | H-SEL-10 |
| `hysteresis` | 0.30 | fraction | H-SEL-10 |
| `pos_drift_tol` | 0.01 | contracts | H-POS-2 |
| `pos_drift_hard` | 5 | contracts | H-POS-2 |
| `ping_interval_s` (ws) | 10 | s | F1 |
| `pong_timeout_s` | 5 | s | F1 |
| `read_deadline_s` | 60 | s | F1 |
| `quiet_s` | 60 | s | F5 |
| `disconnect_halt_s` | 60 | s | F4 |
| `stuck_s` | 1800 | s | F16 |
| `unknown_resolve_s` | 10 | s | §7.2 |
| `retry_same_coid_max` | 3 | attempts | H-ORD-2b |
| `unknown_ping_s` | 120 | s | §7.2 |
| `close_lead` | **1** (was 4) | h | H-CLOSE-2 — shortened to bound HR-011 exposure |
| `final_lead` | 60 | s | H-CLOSE-3 |
| `close_lead_keep_reducing` | true | bool | H-CLOSE-2 — decided on argument, instrumented in V7 (`harness-redteam.md` §5) |
| `max_tenor_d` | 30 | days | H-SEL-8 |
| `drain_timeout_h` | 12 | h | H-HALT-3 — **escalates alerts; never exits with `q ≠ 0`** |
| `heartbeat_s` | 3600 | s | H-PING-1 |
| `backfill_h` | 24 | h | §7.5 |

---

## 17. Verification plan

**This is the part that matters.** The economics are closed; harness correctness
is the open question, and this section is the answer to "how do you know?"
rather than "why do you believe?".

The methodology is the one port-spec §2 already established and proved on this
codebase: **layered gates, cheapest first, and a negative control on the gates
themselves.** A gate that has never been shown to fail is not evidence.

### V1 — Pure unit tests (`harness/quote`, `harness/risk`, `harness/rest`)

These packages are clock-free and I/O-free, so every one of these is a table
test with no fixtures beyond literals.

| # | Pins | Fails if |
|---|---|---|
| V1.1 | The H-CO-1 transform, both directions, all 99 prices | a NO bid at 42c does not encode as `{"side":"ask","price":"0.5800"}` |
| V1.2 | The exact wire payload, byte-for-byte, against `kalshi.py`'s pinned examples | `count`/`price` become JSON numbers, or `.2f`/`.4f` formatting drifts |
| V1.3 | `size_A`/`size_R` across the whole `q` domain, including `q=0`, `q=±inv_soft`, `q=±inv_hard`, fractional `q` | the taper has a discontinuity, or `size_R` can overshoot past `−S` |
| V1.4 | H-CO-6: no `(yes,no)` price pair the skew function can emit sums to ≥100 | a self-cross is reachable |
| V1.5 | The Go port of `probescore.py` against the Python, over randomised books | `combined_share`/`our_side_share` diverge by more than 0 |
| V1.6 | The capital formula against hand-computed cases | reserve or concentration is mis-applied |
| V1.7 | ~~**Does Kalshi dedupe on `client_order_id`?**~~ **ANSWERED YES, 2026-08-04, live minimum-size.** | a duplicate create returns **`409 order_already_exists`**; exactly one order rests. Same-coid retry is safe and is now the specified `UNKNOWN` resolution (H-ORD-2b). The claim that §7.2 was "correct under either answer" remains **withdrawn** — it was false, and the answer happened to be the favourable one. |
| V1.7a | Same-coid retry against a sim that ACKs then drops the response, twice | a duplicate order exists, or a 409 is treated as an error rather than as positive identification |
| V1.8 | ~~**Does `/portfolio/fills` return `trade_id`?**~~ **ANSWERED YES, 2026-08-04, read-only.** | H-ORD-6's join key exists; §7.6 stands unchanged. See below. |
| V1.8a | ~~**Pagination contract per endpoint.**~~ **ANSWERED 2026-08-04/05, read-only.** Recorded in `notes/pagecontract.md`. | Cursor field name is **per-endpoint** (`cursor` vs `next_cursor`); cursors are keyset, so a complete walk is a consistent suffix and concurrent inserts cannot duplicate (measured on 20s of live churn); **an invalid cursor is silently ignored and rewinds the portfolio family to page 1**, producing a non-terminating duplicating walk — hence new **H-PAGE-1a**. 4,170 active programs, so a single page is 4.8% of the universe. |
| V1.8b | Cursor-walk guards from H-PAGE-1a: a rewound cursor is detected by first-record identity, the walk is abandoned, prior state preserved and marked stale, and `SEV2 CURSOR_REWIND` emitted | a corrupted cursor produces duplicates, a silent truncation, or a non-terminating walk |
| V1.8c | Endpoint constants: cursor field name and item key per endpoint; `orders`/`fills` `limit` bounds 1…1000; the `positions` dual-array walk; every `status` string the harness sends | `next_cursor` is read from a `cursor` endpoint (or vice versa) and pagination silently stops at page 1; a misspelled `status` reads as an empty result |
| V1.11 | Aggregate cap accounting: no `(RESTING, SENDING, UNKNOWN, unconfirmed-cancel)` combination on a reducing side can exceed `\|q\|` | A11/A12 — the HR-004 overlap is reachable |
| V1.12 | H-CAP-8's startup fundability check across the whole parameter space of §10.3 | a configuration in which the worst permitted simultaneous fill set cannot be reduced is accepted |
| V1.13 | `pnl` per H-HALT-5, including a reward arrival that must **not** move it, and a stale mark | the kill switch is evaluable but undefined |
| V1.9 | Priority queue ordering and anti-starvation | a P3 requote can precede a P0 cancel |
| V1.10 | Close-lead computation across timezone strings, DST, and a `close_time` that moves | a lead is computed against a stale schedule |

### V2 — The simulated exchange (`harness/sim`) — the centrepiece

A deterministic, in-process fake implementing the REST and websocket surface,
driven by a **real captured tape** from `lip/raw-*.jsonl.gz` or `lip/archive/`.

- Book state comes from replaying the tape through `core` — the same code the
  live harness uses.
- **Fill model — a range of legal allocations, not one invented truth.**
- Clock is injected and steppable. `time.Now()` appears nowhere in the tested
  path.
- The sim can inject every failure in §11 on command.
- **Trade prints are parsed at full wire precision**, not through
  `core.ParsePriceCents` — 17.08% of prints are fractional cents (H-CO-3a) and
  rounding them corrupts the price comparison the fill model depends on.

**V2-FILL — What the simulator may and may not claim.**

An earlier version of this spec specified: *"a resting order of ours at price `P`
fills for `min(our_size, print_size)` when a public print occurs at or through
`P`… the fill model is not invented — it is the measured one."*

**`core` says the opposite, in a comment it marks as load-bearing prose that must
not be deleted** (`core/rig.go:13-39`, carried across from `rig.py:19-47`):

> *"Seeing a print at your price does NOT mean your order filled — you may have
> been behind the queue at that level, and public data cannot see queue
> position."* … and a trade-through *"is NOT proof"*, because maker-side
> self-trade prevention cancels the resting maker and continues matching at worse
> prices, and a cancel landing microseconds before the sweep produces an
> identical observation.

Concretely (red-team HR-014): with 1,000 contracts ahead of ours at 50, a 100-lot
print at 50 fills us **fully in the sim and not at all live**. The simulator would
have systematically over-filled, and every downstream gate — V3's invariants, V4's
faults, V5's mutations — would have been validated against a fill distribution the
exchange does not produce.

Therefore the simulator **enumerates the legal outcomes** for each print against
each of our resting orders — zero fill, partial fill, full fill, delayed
reporting, cancel-race, and multiple fills — and scenarios assert over the range.
Where a single number is needed, strict trade-through supplies a **conditional
lower bound under stated assumptions**, labelled as such. **Queue position is
explicitly unobservable and is never assumed.** The model is calibrated against
private V7 fills, which are the only ground truth that exists.

**This makes the entire harness replayable and deterministic**, which is what
turns every scenario below from a manual exercise into a test. It is the
analogue of `cmd/replay`, and it earns its cost the same way.

### V3 — Invariant assertions, enabled in the simulator *and in production*

Each is checked every tick. In the sim a violation fails the test; in production
it emits `SEV1` and forces `WINDING_DOWN`.

| # | Invariant |
|---|---|
| A1 | No order ever sent with `post_only != true` |
| A2 | No fill of ours ever has `is_taker == true` or `fee_cost > 0` |
| A3 | `our_yes_price + our_no_price < 100` for every resting pair of ours |
| A4 | While `q ≠ 0` in any market, that market has a reducing quote resting **or** an in-flight intent to place one — **including in `SETTLING` until `final_lead`** (H-CLOSE-2a). The exemption applies only after `final_lead`, after trading close, or at `q = 0`. An **unfundable** intent does not satisfy A4 (H-CAP-6). |
| A5 | The monitor has produced a sample from a **source snapshot whose `seq` advanced** within the last 3 seconds, for every selected market, in every global state (H-TOP-5). Row freshness alone does **not** satisfy A5, and persistence success is **not** required (H-STORE-3). |
| A6 | `\|q_local − q_exch\| ≤ pos_drift_tol` at every poll, or a `position_poll` row records the disagreement |
| A7 | Deployed capital ≤ `capital_max` at every placement decision |
| A8 | No market in `REDUCING` ever has an adding-side order resting or in flight |
| A9 | Every state transition has a `state_event` row |
| A10 | No **create** is retried while its outcome is `UNKNOWN` (cancels are exempt — H-ORD-4a) |
| A11 | Every size cap is evaluated against **aggregate** `RESTING + SENDING + UNKNOWN + unconfirmed-cancel` quantity, never a single order (H-Q-5b) |
| A12 | No reducing order's aggregate quantity exceeds `\|q\|`; no fill sequence can change the sign of `q` via a reducer (H-Q-5a) |
| A13 | No placement decision is taken from a book that is quarantined or stale, or from portfolio truth older than `truth_max_age_s` (H-FAIL-4, H-FAIL-5) |
| A14 | A global halt latch on disk implies global state is `WINDING_DOWN` or `DRAINED` (H-HALT-4) |

**A4 and A5 are the two that encode the probe's defect directly.** A5 in
particular: *"in every global state"* is the clause that would have caught 2
snapshots in 6.14 hours, at the first tick after the halt.

**Both were nonetheless evadable as originally written**, which is why each now
carries an extra clause:

- **A4** exempted `SETTLING`, and `SETTLING` begins four hours before close —
  so abandoning inventory for the entire close window satisfied it (HR-001/M13).
- **A5** checked that a row was *written*, not that the thing it described had
  *moved* — so a deadlocked owner produced fresh rows about a frozen world
  forever (HR-007/M14).

Both evasions produce `probebot.py`'s observable while passing the assertion
written to prevent it. That is the failure mode this section exists to catch, and
it took an adversarial pass to find it, which is the argument for V5.

### V4 — Fault injection matrix

Each row: injected in the simulator as a test, **and** drilled once live in
dry-run (V6) before any capital.

| # | Fault | Injection | Expected observable |
|---|---|---|---|
| V4.1 | Half-open socket | sim stops delivering frames, never closes | ping/pong fails within 15s; reconnect; resnapshot; `SEV2` |
| V4.2 | Clean close | sim closes 1000 | immediate reconnect, state retained, **no** ping |
| V4.3 | Abnormal close | sim closes 1006 | backoff, book reset, resnapshot |
| V4.4 | Disconnect > 60s | sim refuses reconnect | markets → `REDUCING`; **cancels still issued over REST**; `SEV1` |
| V4.5 | Wedged feed, one market | sim serves a stale book for one ticker; REST disagrees | that market → `REDUCING`; others unaffected; `SEV2` |
| V4.6 | **macOS DNS wedge** | **live only** — wait for a real outage (~2.5h cycle) and exercise the harness *during* it | H-DEP-1's hypothesis confirmed or refuted. **`nslookup` works throughout and masks the failure — testing after recovery proves nothing.** |
| V4.7 | Host sleep | `pmset sleepnow` with inventory open, live dry-run | gap detected, resnapshot, reconcile, `uptime` records the interval, `SEV2` |
| V4.8 | 429 storm | sim returns 429 for 90s | bucket halves, no order lost, no order duplicated, `SEV2` |
| V4.9 | Reject storm | sim rejects 20% of placements | market → `REDUCING` after the 10%/50 threshold, others unaffected |
| V4.10 | `insufficient_balance` | sim returns it once | global `WINDING_DOWN`, `SEV1` |
| V4.11 | Ambiguous write | sim accepts, then times out the response | order → `UNKNOWN`; **zero retries**; resolved at the next poll; no duplicate order exists |
| V4.12 | Position drift | sim reports a `q_exch` we did not derive | recorded; sustained → `REDUCING`; hard → global |
| V4.13 | Taker fill | sim reports a fill with `is_taker: true` | global `WINDING_DOWN` within one poll, `SEV1` |
| V4.14 | Foreign order | sim reports a resting order with a non-`lipH-` coid | market excluded; the order **not** cancelled; `SEV2` |
| V4.15 | SQLite failure | sim store returns errors | trading and monitoring continue; `SEV1`; journal buffers |
| V4.16 | ntfy unreachable | sim ping endpoint refuses | owner goroutine never blocks; queue drains in order on recovery |
| V4.17 | **SIGKILL with inventory** | `kill -9` mid-run, then restart | §7.5 recovers `q` from the exchange, market enters `REDUCING`, resting orders adopted by coid, `STARTUP` ping fires |
| V4.18 | Clock step | sim jumps wall clock ±1h | deadlines recomputed; **no close lead is skipped** |

### V5 — Negative control: the gate on the gate

port-spec §2 gate 7, applied here. For each invariant in V3, apply a mutation
that violates it and confirm a named test catches it. **Any mutation that
survives every gate is a defect in the verification, and the harness does not
ship until the gate is strengthened** — unless the mutation is positively
established to be behaviourally inert, argued explicitly and never assumed.

Mutations must be semantic. A mutation caught only because it left an import
unused tests the Go compiler, not the gate.

The mandatory ones, each named for what it reproduces:

| # | Mutation | Must be caught by |
|---|---|---|
| M1 | **`break` the monitor loop when global state leaves `RUNNING`** — *this is `probebot.py`'s exact defect* | A5 |
| M2 | Cancel the reducing quote on a market halt instead of keeping it | A4 |
| M3 | `os.Exit` on SIGTERM instead of draining | a lifecycle test asserting the process outlives SIGTERM with `q ≠ 0` |
| M4 | Retry an `UNKNOWN` write once | A10 + V4.11's duplicate check |
| M5 | Set `post_only: false` on the reducing quote | A1, A2 |
| M6 | Deselect a market that has `q ≠ 0` | H-SEL-11 test + A4 |
| M7 | Drop the `size_R` cap so a fill can overshoot past flat | V1.3 |
| M8 | Poll positions but skip writing agreeing rows | a `position_poll` density test |
| M9 | Use the raw book instead of `external_best` | a self-chase test: our own order must never move the quote |
| M10 | Reverse the H-CO-1 transform for the NO side | V1.1, V1.2 |
| M11 | Place before reconciling at startup | an ordering test on the `STARTING` sequence |
| M12 | Skip the cancel-sweep verification | H-ORD-4 test with a sim that ignores the first cancel |
| **M13** | **On entry to `SETTLING`, cancel everything and never place the capped reducer** — *the probe's defect confined to the close window* | **A4 (amended)** + the close-lifecycle test |
| **M14** | **Freeze owner snapshot publication; leave monitor and ping alive** — *fresh rows about a frozen world* | **A5 (amended)** + `SEV1 OWNER_STALLED` |
| M15 | Ignore fill events until the next position poll | V7.4 (amended) — must fail |
| M16 | Read only page one of every list endpoint | H-PAGE-1 test with the target on page two |
| **M22** | **Read `next_cursor` from a `cursor` endpoint** (or vice versa) — pagination silently stops at page 1 | V1.8c + H-PAGE-1 test with the target on page two |
| **M23** | **Carry a corrupted cursor instead of abandoning the walk** — the endpoint rewinds to page 1 and the walk duplicates without terminating | V1.8b + `SEV2 CURSOR_REWIND` |
| M17 | Re-place an `UNKNOWN` create after two negative reads | A10 + the duplicate-order check |
| M18 | Size the reducer at `\|q\| + S` | A12 + V1.3 |
| M19 | Apply the concentration cap to a reducing order | A4 (unfundable intent) + H-CAP-6 test |
| M20 | Do not persist the global halt latch before the state change | A14 + halt-then-SIGKILL-then-restart test |
| M21 | Treat a cancel-requested adding order as "off" | A13 + H-FAIL-3 test |

**M1 is the regression test for the $9.32 loss.** It is written first, before
the harness works, and it must fail against the mutation and pass against the
real code. If M1 ever stops failing against its mutation, the verification is
broken regardless of what else is green.

**M13 and M14 exist because the first adversarial pass produced them.** §17's own
question — *"what mutation would survive every gate?"* — had two answers, and
neither was in M1–M12. A mutation list that has never been extended by an attack
is a list that has only ever been checked against its author's imagination.

### V6 — Dry run against the live feed

**H-VER-1 — The dry-run guard is structural, not a flag.**
`harness/rest` refuses any non-GET request unless **two independent conditions**
hold: `--live` on the command line *and* a `live_ok` file present at a
configured path. This reproduces `probebot.py`'s `DryRunViolation` pattern,
which is the one piece of that bot's safety design worth porting verbatim.

Run **72 hours**, real feed, real market selection, all state machines live,
zero writes.

**V6-SCOPE — Each V4 fault is classified, because V6 cannot drill all of them.**

As written, V6 required *"zero non-GET requests attempted (asserted, not
observed)"* **and** *"every fault in V4 drilled at least once"*. These are
contradictory (red-team HR-022): V4.10 needs a real balance reject, V4.11 an
accepted placement whose response is then lost, V4.13 our own taker fill, V4.17
real open inventory through a SIGKILL. **None can occur under a client that
structurally refuses writes.** Two gates that cannot both be met are not a
standard; they are an invitation to quietly drop one.

| Class | Faults | Where |
|---|---|---|
| **sim-only** | V4.1, V4.2, V4.3, V4.4, V4.5, V4.8, V4.9, V4.10, V4.11, V4.12, V4.13, V4.14, V4.15, V4.16, V4.18 | V2 simulator |
| **live-read-only** | V4.6 (DNS wedge), V4.7 (host sleep) | **V6** — these need real host conditions and no writes |
| **live-minimum-size** | V4.10, V4.11, V4.13, V4.17 re-drilled for real | **V7** |

Gates:

- zero non-GET requests attempted (asserted, not observed)
- every per-market state reached at least once, including `REDUCING` and
  `SETTLING` (forced by a synthetic position injected into the model), and at
  least one full close lifecycle through `final_lead` (M13's gate)
- uptime ≥ 99% of non-gated seconds
- every **sim-only** and **live-read-only** fault drilled; each with the artifact
  that proves it fired, named
- V3's invariants held throughout
- at least one real DNS wedge survived (V4.6)
- at least one real macOS sleep survived, if one occurs; otherwise forced (V4.7)

**V6 entry gate — a missed-heartbeat alarm must already exist and have been shown
to fire.** §13.4 concedes that an anomaly channel cannot report its own death and
files a dead-man's switch as a later nicety. Against a revenue model where
downtime is a direct multiplicative loss (S4), that is the difference between a
20-minute outage and an 8-hour one, and an operator asleep at 02:00 does not
observe an absence. It is a third-party cron plus a dead-man's-switch endpoint,
not a build, and the 72-hour run does not start without it.

### V7 — Live minimum-size run

One market, `S = 1` contract per side, real orders, real capital (~$1).

| # | Must be demonstrated, not asserted |
|---|---|
| V7.1 | Every placed order appears in `/portfolio/orders` with our coid |
| V7.2 | Every cancel is confirmed by a sweep |
| V7.3 | Every fill appears in `our_fill` keyed by `trade_id`, and `is_taker` is false with `fee_cost == 0.00` |
| V7.4 | **`q_before == q_exch`, compared *before* the overwrite**, on ≥1000 consecutive polls — during which YES, NO, partial, duplicate and out-of-order fill reports are deliberately induced, with reaction latency bounded. **Mutation M15 (ignore fills until the poll) must fail this.** |
| V7.5 | **A deliberately induced one-sided fill is reduced to flat by a resting quote on the opposite side, with zero taker fills** — the core claim of this design |
| V7.6 | `scripts/importfills.py` marks exactly our `trade_id`s in a copy of `rig.db` as `source='ours'`, and no others |
| V7.7 | SIGKILL and restart mid-position recovers correctly (V4.17, live) |
| V7.8 | The realised payout is within the band the integrated `snap` share predicts |
| V7.9 | The **live-minimum-size** faults V6 could not drill: V4.10, V4.11, V4.13, V4.17 (V6-SCOPE) |
| V7.10 | A full close lifecycle through `close_lead` → `SETTLING` → `final_lead` with `q ≠ 0`, showing the capped reducer alive throughout the window and gone after `final_lead` (H-CLOSE-2a, M13) |
| V7.11 | **Fill-conditioned markout of every reducer fill, bucketed by time-to-close** — the evidence that decides `close_lead_keep_reducing` and `close_lead` in the next revision, since HR-011 could only be settled by argument here. Telemetry on our own fills, not an economics probe (§18). |

> **V7.4 was the gate that could not fail.** H-POS-1 sets `q_local := q_exch` on
> every poll; the earlier V7.4 then asserted `q_local == q_exch`.
> **Overwrite-then-compare passes unconditionally** — including against an
> implementation with no incremental fill handling whatsoever (red-team HR-018).
> The single gate whose job was to prove the position model works could not
> distinguish a working model from an absent one.

### V8 — Definition of done

- V1 green; **V1.7 ✅, V1.8 ✅ (2026-08-04) and V1.8a ✅ (2026-08-04/05) all
  answered. No blocking fact-finding item remains before the order layer.**
- V2 simulator replays a real multi-hour tape deterministically: same tape, same
  trace, twice; fill model per V2-FILL (a range, not a point).
- **V3's fourteen invariants** asserted in both sim and production builds.
- V4's eighteen faults injected, each classified per V6-SCOPE, each with the
  expected observable **and the artifact proving it fired** recorded.
- **V5 green: every mutation M1–M23 caught, with the catching test named**,
  recorded in `notes/harness-negative-control.md`. M1 first; **M13 and M14 are
  not optional** — they are the two the first adversarial pass produced; **M22
  and M23 are the two V1.8a's measurement produced.**
- V6's 72-hour dry run complete, with its gates met, and its entry gate (the
  missed-heartbeat alarm) demonstrated firing beforehand.
- V7 complete, all ten rows demonstrated.
- `scripts/check.py` green; no frozen artifact modified.

**The cross-model adversarial round has already been run, and it was run on this
document rather than on the harness.**

> **Amended.** This bullet previously required *"one cross-model adversarial
> review round (codex `gpt-5.6-sol`, `xhigh`, read-only sandbox), primed to assume
> the harness is wrong, that stops finding findings that are reachable in the
> deployed configuration."*
>
> That round was instead spent on **the spec, before implementation** — one pass,
> `gpt-5.6-sol` at `model_reasoning_effort=max`, read-only sandbox, recorded in
> `notes/harness-redteam.md`. It returned 26 findings, 24 of them material,
> including two mutations (**M13**, **M14**) that would have survived every gate
> in this section and both of which reproduce `probebot.py`'s observable.
>
> **This is the better trade and it is deliberate.** Those 24 findings would
> otherwise have been found — at best — after the harness was written, the
> simulator built, and 72 hours of dry run spent validating a design whose
> reducer could overshoot, whose caps were per-order, whose halt self-cleared on
> restart, and whose monitor could report a frozen world indefinitely. Two of
> them (HR-003, HR-005) meant the exit itself did not work under the opening
> configuration.
>
> **No further cross-model round is part of the definition of done.** The harness's
> terminal gates are **V5 (negative control) + V6 (dry run) + V7 (live minimum
> size)**, and the standard those gates enforce is unchanged: a finding counts only
> if it is **reachable in the deployed configuration**, and *reachability is argued
> in writing, never assumed, in either direction* — the standard by which HR-002
> was rejected on 14.8 million measured price strings and HR-001 was accepted.

**The order is not interchangeable.** V5 before V6, because a dry run gated by
untested gates is 72 hours of evidence about nothing. V6 before V7, because the
first live order should not be the first time a state machine is exercised. And
**V1.7/V1.8/V1.8a before any of it**, because each can invalidate a design rule
rather than merely fail a test — as V1.8a in fact did, adding H-PAGE-1a and two
mutations that no amount of implementation care would have produced.

---

## 18. Explicitly out of scope

- **Any economics measurement.** No liquidity probes, no pool-size re-derivation,
  no capture-curve verification, no maker-edge study. That thread is closed.
- **Taker orders of any kind**, including "emergency" ones. There is no such
  code path and adding one is a spec change, not an implementation decision.
- **Improving the touch** (H-Q-2).
- **Any change to `core`, `feed`, `store`, `cmd/rig`, or any frozen Python.**
- **Amend orders** (H-ORD-3) — a follow-on with its own gate.
- **Writing to `rig.db` from the harness** (H-ORD-7).
- **Cross-market or cross-venue hedging.** Inventory is reduced in the market
  that created it, by quote skew, or it settles.

---

## 19. Decisions this spec made, that the reader should check

Mechanical decisions are recorded inline. These are the judgement calls where a
different answer is defensible, collected here so review is cheap. **Full
disposition of the adversarial round is in `notes/harness-redteam.md`.**

1. **§9 H-CLOSE-2 keeps a reducing quote inside the close-lead window**, capped
   at `|q|` so it cannot take new inventory. Decision 2 read strictly would
   cancel everything. Knob is `close_lead_keep_reducing`. **This is the one place
   the spec extends a locked decision rather than implementing it literally.**
   HR-011 showed S1 does not cover it — S1 measured unconditional
   settlement-versus-mid, not settlement conditional on our maker order being
   *selected*, which is the only case a resting reducer experiences.
   **Resolved on argument, not evidence: knob stays `true`, `close_lead` cut 4h →
   60m, and V7.11 instruments the fill-conditioned markout so the next revision
   decides it with data.** See `harness-redteam.md` §5.
2. ~~**§6.2 sizes the reducing quote at `|q| + S`, not `S`.**~~ **Withdrawn —
   this was wrong** (HR-003). It overshot flat into a `−100/+100` sign-flip
   cycle, because `S = 100 > inv_hard = 60` made the designed success case a
   re-entry into the failure state. `size_R` is now capped at `|q|` (H-Q-5a) and
   caps are aggregate (H-Q-5b).
3. **§7.5 does not cancel foreign orders at startup**, it excludes the market and
   pings. Cancelling someone else's — or a previous incarnation's — orders is a
   destructive act taken on incomplete information. **But foreign activity
   arising *after* startup now latches global `WINDING_DOWN`** (H-ORD-9), and a
   dedicated account is the recommended way to make this moot.
4. **§12 H-HALT-3 makes SIGTERM non-terminal.** This diverges deliberately from
   `cmd/rig`, where SIGTERM rolls back to reproduce Python. Stated at the rule.
   **`drain_timeout_h` no longer exits** (HR-009) — it escalates.
5. **§6.1 H-Q-2 still forbids improving the touch, but for a different reason.**
   Improving *does* produce a scoring gain (HR-025, verified against
   `probescore.py`: 50.0% → 66.7%). We decline it because at-touch fills are
   already completely adversely selected (`reeval-verdict.md` §3a) and being
   inside the touch buys more of that. The rule is unchanged; its justification
   was false and is replaced.
6. **§6.2 one-sided quoting is no longer treated as free.** It costs ~1/3 of the
   market's reward rate (HR-025), which makes `inv_soft`/`inv_hard` a real
   economic tradeoff rather than a pure safety knob. No parameter changed on this
   basis yet; flagged for tuning after V7 supplies real fill data.

---

## 20. Definition of done for *this document*

Done. It pins order lifecycle (§7), position monitoring (§8), exit behaviour
(§6.2–6.4), close handling (§9), failure modes (§11), halt semantics (§12), the
ping contract (§13), and the verification plan (§17) precisely enough to
implement without re-litigating design.

**It has also now been attacked once, hard, by a model that was not shown the
reasoning behind it** — and it did not survive that intact. 24 material findings
against a document whose author considered it done is the evidence for doing this
before the code rather than after it. Two of those findings (HR-003, HR-005) meant
the exit mechanism did not work under the opening configuration, and two more
(HR-001/M13, HR-007/M14) were mutations that reproduced `probebot.py`'s exact
observable while passing every gate in §17.

**The next session implements. It does not redesign, and it does not run another
adversarial round.** If implementation finds a rule that is wrong, patch this
document and say why — a recurring "fix" applied twice in code is a spec bug, not
a code bug (port-spec §7.4).

**Blocking before any order-layer code: nothing remains.** V1.7 ✅ and V1.8 ✅
were answered 2026-08-04; **V1.8a ✅ was answered 2026-08-04/05**
(`notes/pagecontract.md`) and did move rules — it added **H-PAGE-1a**, gates
V1.8b/V1.8c, and mutations **M22/M23**, because the measurement found that the
cursor field name differs per endpoint and that an invalid cursor is *silently
ignored* rather than rejected. Implementation is unblocked.
