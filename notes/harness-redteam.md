# Harness spec — adversarial review disposition

**Round:** one cross-model hostile pass, codex `gpt-5.6-sol`, `model_reasoning_effort=max`,
read-only sandbox, session `019fcff5-b019-73b2-a85a-f26854081446`, 2026-08-04.
**Target:** `notes/harness-spec.md` @ 1209 lines (backed up as
`notes/harness-spec.md.bak-preredteam`).
**Raw findings, verbatim and unedited:** `notes/harness-redteam-raw.md`.

The reviewer was given the spec, its intended use, and the constraints. It was **not**
given the endorsement, the rationale for the design, the prior break ledger, or a
desired verdict. It was primed to assume the spec is wrong and directed hardest at
§6.2, §7.2, §12, and §17 V5.

**Result: 26 findings — 12 CRITICAL, 13 MAJOR, 1 MINOR.**
**Disposition: 24 material, 1 refuted by measurement, 1 already-accepted-but-elevated.**

That hit rate is itself the headline. This was not a spec that needed touching up.

---

## 0. How each finding was adjudicated

Every finding was checked against the repository or against arithmetic before being
classified. Three were checked against *data* rather than reasoning, and one of those
reversed the reviewer's verdict. Reachability is argued in writing in both directions,
per §17 V8's own standard.

| Verdict | Meaning |
|---|---|
| **MATERIAL** | reachable in the deployed configuration and changes the spec |
| **NON-MATERIAL** | the failure scenario is not reachable, argued with evidence |
| **ACCEPTED** | the spec already states this tradeoff knowingly |

---

## 1. The one finding that is wrong, and why it matters that it is

### HR-002 — "the mandated parser cannot quote the actual touch" — **NON-MATERIAL**

**Claim.** `core.ParsePriceCents` rounds to integer cents. Given a raw book with a YES
bid of `$0.2570`, the harness would send `$0.2600`, improving the touch by 0.3c while
believing it joined — breaking H-Q-1, H-Q-2 and H-CO-6. Cited `num.go:27` ("8.8% of all
price occurrences are fractional cents") as proof this is a live defect, not speculation.

**Measured, `raw-20260724T231211Z.jsonl.gz`** (full tape for deltas and trades; the
sampled first 200k frames for snapshot levels):

| Source | Price strings | Fractional-cent |
|---|---|---|
| `orderbook_delta.price_dollars` | **14,792,425** | **0 (0.00%)** |
| `orderbook_snapshot.{yes,no}_dollars_fp` | 7,356 | **0 (0.000%)** |
| `trade.{yes,no}_price_dollars` | 182,029,958 | 31,097,490 (17.08%) |

**The resting book is integer-cent — 14,799,781 price strings, zero exceptions.
Fractional cents occur only in trade prints.** `0.2570`, the price the reviewer built
its counterexample from, is in the top-6 most common *trade* prices in that very tape.
It is not a book level and cannot be one. `num.go`'s 8.8% figure is across all price
occurrences, a population outnumbering book levels 12:1 and dominated by trades.

So the harness cannot be handed a fractional touch to join, and H-CO-3's mandate to use
`core.ParsePriceCents` is lossless for every price the quoting path ever reads.

**Why this is recorded rather than deleted.** The finding was specific, plausible, and
cited real source. It was wrong only because a measurement contradicted it. That is the
distinction §17 V8 demands — "reachability is argued in writing, never assumed, in
either direction" — and it is the reason this one paragraph exists instead of a silent
drop.

**Residual, adopted anyway (cheap):**
- `harness/wsx` and `harness/rest` **assert** that every parsed book price is an exact
  integer cent, and raise `SEV2 BOOK_PRICE_GRANULARITY` + market → `REDUCING` if one is
  not. One tape from one universe is not a guarantee across all markets or across a
  future tick-size change. This converts an assumption into a detected anomaly for
  roughly ten lines of code.
- The **simulator** does read trade prints, where 16.5% *are* fractional. Rounding them
  through `ParsePriceCents` corrupts the fill oracle's price comparison. Folded into
  HR-014's rework of the fill model.

---

## 2. Already accepted, but elevated

### HR-007b — an anomaly channel cannot alert on its own absence — **ACCEPTED**

§13.4 already says this outright: F18 is detected by the *absence* of a heartbeat,
"which is the operator's job unless a dead-man's-switch service is added later." The
reviewer restates it as a finding. It is not new.

**Elevated anyway:** the spec files this as a later nicety. An operator asleep at 02:00
does not observe an absence, so in the deployed configuration this is the difference
between a 20-minute outage and an 8-hour one, against a revenue model where downtime is
a direct multiplicative loss (S4). Promoted from "later" to a **V6 entry gate** — the
72-hour dry run does not start until a missed-heartbeat alarm exists and has been shown
to fire. It is a third-party cron and a dead-man's-switch endpoint, not a build.

(HR-007's primary claim — the stale-snapshot one — is material and separate. See §3.)

---

## 3. Material findings

Grouped by what they break. Every one changes the spec.

### 3.1 The reducer is the single point of failure, and three findings hit it

These three interlock and were adjudicated together. Together they say: **the only
inventory-reduction mechanism in the design can overshoot, can be duplicated, and can
be unfundable.**

**HR-003 — `size_R = |q| + S` overshoots past flat into a sign-flip cycle. CRITICAL.**
Verified by arithmetic against §10.3's own parameters. `q=+61` posts 161 NO; a full fill
gives `q=-100`, not zero. `|q|` went 61 → 100. Since `inv_hard = 60`, the result is
immediately back in `REDUCING`, now posting 200. Full fills alternate `-100, +100`
indefinitely, and `REDUCING` exits only at exactly zero.

The spec anticipated the landing point and blessed it — §6.2: "we land at `−S` … a
normal-sized position … the system is self-correcting." **That claim is false against
§10.3, where `S = 100` and `inv_hard = 60`.** `S > inv_hard` makes the designed success
case a re-entry into the failure state. The spec contradicts itself across two sections
that were written to agree.

**HR-004 — H-Q-9's overlap defeats every cap. CRITICAL.** Place-then-cancel is permitted
whenever the momentary doubled size is within `S_max`. With `q=+100`, two 200-contract
NO reducers are exactly `S_max = 400` and only ~$122 of notional, so the guard passes.
If the cancel is slow or ambiguous and both fill, `q` goes to `-300`. The same defect
sits inside `SETTLING`, where two individually `|q|`-capped reducers can both fill and
flip `+100` to `-100` — directly falsifying H-CLOSE-2's "it cannot flip us."

**HR-005 — the capital rules can forbid the only exit. CRITICAL.** H-CAP-2 gives
`500/6 × 2 = $166.67` per market. A `q=-100` position needing a 200-contract YES reducer
with YES at 89c requires `$178`. **The reducer is unfundable under the harness's own
default configuration**, even after cancelling everything else. Six simultaneous
one-sided fills are worse: one $125 reserve cannot fund six opposing reducers, and
`WINDING_DOWN` does not create collateral. There is also a genuine in-flight race — K
REST workers can each approve against the same committed total.

A capital rule that forbids the exit is the same defect as the probe's, arrived at from
the opposite direction. The probe removed the exit deliberately; this configuration
removes it by arithmetic.

**Resolution (all three):**
- Reducer sizing becomes `size_R = min(|q|, S_max, funded)`. Every positive partial fill
  must strictly decrease `|q|`; a full fill must produce exactly zero. Overshoot past
  flat is forbidden, not blessed. Any deliberate two-sided imbalance is specified
  separately as a `SKEWED` behaviour and never rides on the reducer.
- All caps apply to **aggregate `RESTING + SENDING + UNKNOWN + unconfirmed-cancel`
  quantity**, not per-order. `REDUCING` and `SETTLING` use cancel-confirm-place whenever
  an overlap could exceed `|q|`; H-Q-9's place-then-cancel is confined to the adding
  side, where overshoot is bounded by design.
- Collateral accounting covers positions and every `RESTING`/`SENDING`/`UNKNOWN` order,
  reserved atomically before dispatch. Risk-reducing orders are exempt from the
  concentration cap once adding orders are cancelled. **A configuration that cannot fund
  the reducer for the worst permitted simultaneous fill set is rejected at startup**, not
  discovered at fill time. An unfundable intent does not satisfy A4.

### 3.2 The monitor can be structurally alive and still lying

**HR-007 — A5 checks row freshness, not source freshness. CRITICAL.**

I2 makes the monitor unstoppable by the quote engine. It does not make it *truthful*.
If the owner goroutine deadlocks after publishing `q=0`, the monitor re-reads the same
`atomic.Pointer[Snapshot]` forever, stamps new `snap` rows every second, and pushes
hourly heartbeats reporting `q=0` — while a resting order fills and real inventory
grows. **A5 passes at every tick. F18 never fires, because the process and its heartbeat
are both alive.**

This is the sharpest finding in the round, because it defeats the spec's centrepiece
invariant on the spec's own terms. §2's I2 was written against `probebot.py`'s
control-flow `break`, and it does prevent exactly that. It does not prevent the same
observable outcome — confident silence about live risk — reached by a different route.

**Resolution:** `Snapshot` carries a monotonically advancing sequence number and a
publication timestamp. **A5 becomes: the source sequence has advanced within the last 3
seconds**, not "a row was written." A monitor that observes a stalled sequence emits
`SEV1 OWNER_STALLED` and marks its own output stale rather than continuing to publish
it as current. New mutation **M14** (freeze owner publication, leave monitor and ping
alive) must fail against the current A5 and pass against the new one.

### 3.3 "Absent" cannot be proven, and this codebase has already been bitten by it

**HR-006 (CRITICAL) + HR-013 (MAJOR)**, adjudicated together.

§7.2 resolves an `UNKNOWN` create by searching `/portfolio/orders` and `/portfolio/fills`
and, on two negative reads `unknown_resolve_s` apart, **declaring it never landed and
permitting a requeue.** Two problems:

1. A single-page read does not establish absence. `kalshi.py:160-175` — the only
   verified-correct client in the repository — exposes `positions()`, `orders()` and
   `fills()` as one-shot `limit=200` calls **with no cursor loop at all**. An order that
   filled and sits at item 201 reads as absent, twice, and gets reissued. Inventory
   doubles.
2. Even exhaustive pagination does not establish absence without a documented
   snapshot-consistency and visibility contract, which is not known to exist.

**This is not hypothetical for this repository.** `probebot.py:1100-1108` carries a
written incident report: active programs grew past 1,000 mid-probe, a one-shot
`limit=200` fetch pushed the live market out of the window, and a restart died claiming
"no active liquidity program" while the program was still running. Its own conclusion:
*"A lookup that silently depends on position in an unsorted list is a correctness bug,
not a tuning parameter."* The spec then re-introduces that exact pattern as the
foundation of H-POS-3, H-POS-4, H-ORD-4, H-ORD-5 and H-ORD-8.

The spec's claim that §7.2 "is correct under either answer" to V1.7 is therefore
**false**: if Kalshi does not dedupe on `client_order_id` *and* absence is unprovable,
requeue duplicates inventory.

**Resolution:**
- Every list read is a **complete cursor walk**. State is replaced only after a full
  walk succeeds; a failure at page *k* preserves prior state and marks it **stale**,
  never "empty". This binds positions, orders, fills, and the active-program universe
  used for selection.
- The "declare it never landed" branch is **deleted**. An ambiguous create resolves only
  by a positive identification, or it stays `UNKNOWN` and its quantity stays in the risk
  model at maximum possibly-live size. If V1.7 confirms documented `client_order_id`
  idempotency, retrying the same coid becomes permitted and the branch returns under that
  proof — not before.
- **V1.7 and V1.8 are promoted from informational to blocking.** V1.7's answer now
  determines whether a code path exists at all.
- Tests must place the target beyond page one and delay its visibility past two polls.

### 3.4 Halt is neither durable, complete, nor honestly named

**HR-009 — `WINDING_DOWN` self-clears on restart. CRITICAL.** Global halt lives in
memory. A taker fill latches `WINDING_DOWN` (F14); an unrelated panic kills the process;
`launchd KeepAlive: true` restarts it; `STARTING` never reads a prior halt and flat
markets resume quoting. **The most serious stop condition in the system is erased by the
supervision policy the spec mandates.** §10.4's `harness.resume.ok` sentinel governs
operator-initiated resume; nothing persists the halt at the moment it is set.

H-HALT-3 is separately contradictory: `drain_timeout_h = 12` permits exit **with
inventory open**, which is I1 and H-FAIL-1 violated in the parameter table. And when the
harness is flat, SIGTERM exits into `KeepAlive: true`, which restarts it immediately.

**HR-008 — `STARTING` can halt without knowing what it holds. CRITICAL.** If
`/portfolio/positions` fails 3 times, §7.5 sends the harness to `WINDING_DOWN` — a state
whose entire purpose is keeping the reducing quote alive, entered with **no knowledge of
`q`**, so no reducing side and no size can be chosen. No continuing reconciliation is
specified, so it sits there. Separately, a market holding inventory need not exist in the
`core.Rig` universe (fixed at subscribe time), leaving `REDUCING` with no book to quote
against.

**HR-010 — "adding: off" means "we sent a cancel". CRITICAL.** H-FAIL-2 asserts REST and
websocket are independent transports. They share DNS, routing, TLS, credentials, and the
exchange. A shared failure — or the narrower and more common case where public data stays
healthy while portfolio endpoints return 401/503 — leaves an adding order resting and
fillable while §12's table claims adding is off and monitoring is full. No §11 row defines
a maximum acceptable age for portfolio truth.

**HR-024 — foreign activity collides with H-SEL-11. MAJOR.** F15 excludes a market with a
foreign order from selection; H-SEL-11 says a market with `q ≠ 0` is never deselected. A
manual fill makes both apply. Worse, a manual *taker* fill either lands in `our_fill` and
falsely trips F14's global halt, or is filtered out and leaves a gap in ownership.

**Resolution:**
- A **durable halt latch** written before the state change, with a non-SQLite fallback
  (a file). `STARTING` reads it before any placement and never self-clears it.
- Drain timeout **escalates alerts; it never exits with `q ≠ 0`.** A planned exit either
  restarts into a latched `DRAINED` or disables `KeepAlive`.
- Failed truth reads at startup enter **`UNKNOWN_RISK`** and retry indefinitely with
  backoff, pinging `SEV1`. `WINDING_DOWN` is not entered on ignorance, because it cannot
  be acted on from ignorance.
- Managed markets = selected ∪ every non-zero position ∪ every `lipH-` order,
  irrespective of LIP status or universe membership. The harness subscribes to whatever
  it holds.
- **"Off" is redefined throughout as exchange-confirmed absent**, not cancel-requested.
  Independent freshness clocks for positions, orders and fills; on expiry, all new
  dispatch stops and any order that cannot be confirmed cancelled is reported as
  `SEV1 LIVE_UNCANCELLED` — named as unmanaged rather than assumed dead. Auth failure is
  global and immediate.
- Foreign activity **latches global `WINDING_DOWN`** rather than merely excluding a
  market; fills are classified by `order_id` against a durable ownership ledger that
  includes terminal orders. The dedicated-account alternative is recorded as the cheaper
  fix and left to the operator.

### 3.5 Two load-bearing reward claims are arithmetically backwards

**HR-025 — MAJOR.** Verified directly against `probescore.py`, which is the reference
`scripts/checkscore.py` already pins to the frozen `score.py`.

*Claim 1 — improving the touch does produce scoring gain.* Target 200, external depth
100@50 and 100@49:

| Action | Qualifying set | Total score | Our score | **Our side share** |
|---|---|---|---|---|
| Join 100@50 | `[(50,200)]` | 200 | 100 | **50.0%** |
| Improve to 51 | `[(51,100),(50,100)]` | 150 | 100 | **66.7%** |

Improving demotes the entire field one tick — halving it at DF=0.5 — while we stay at
`N = 0`. H-Q-2's stated reason, *"for no scoring gain — we are at N = 0 either way,"* is
**false**.

*Claim 2 — the absent side is not free.* `combined_share` averages the two sides, each
contributing 1.0 of 2.0. Two-sided 100/100 against a field of 100/100 earns
`(50% + 50%)/2 = 50%`. Going one-sided with a 200 reducer earns `(0 + 66.7%)/2 = 33.3%`.
S3 preserves the *quoted* side's weight; it says nothing about the absent one. §6.2's
*"Skewing to flat costs nothing in reward terms"* is **false** — it costs roughly a third
of the market's reward rate for as long as it lasts. (Worse if the field alone misses
Target Size on the abandoned side: the snapshot then gates out entirely and the market
pays nobody.)

**Resolution.** H-Q-2 **survives**, on the adverse-selection ground alone — §3a of
`reeval-verdict.md` measured complete adverse selection at the touch (−0.786c signed
1-minute drift, t = −10.75), and improving the touch buys more of exactly that. But the
false justification is deleted and replaced with the true one, and §6.2's "costs
nothing" becomes a quantified cost. This matters beyond bookkeeping: a spec that asserts
something an implementer can disprove in ten lines of Python is a spec that gets
overridden at 3am by someone who recomputed it. It also means `inv_soft`/`inv_hard` are
now a real economic tradeoff — time spent skewed is time at ~2/3 revenue — rather than
the free action §6.2 claims.

**HR-011 — S1 does not cover a fill-conditioned exit near resolution. CRITICAL.**
`holdvsflat.py` measured `E[settlement − mid_5m]` unconditionally. It did **not** measure
settlement value *conditional on our maker order having been selected*, which is the only
case that matters for a resting reducer. With `q=+100` and the outcome publicly knowable
while the book still shows YES 49 / NO 50, our NO bid at 50 is hit precisely when YES is
winning: we surrender $100 of settlement value for $50. Zero maker fees do not remove
selection on *whether* the order fills. §3a already measured that at-touch fills are
completely adversely selected in normal conditions; near resolution the asymmetry is
strictly worse.

**Resolution.** §9's `close_lead_keep_reducing` knob stays, and the reasoning behind
keeping the exit alive stays — cancelling the exit is the probe's defect shape. But the
spec must stop citing S1 as authority for a case S1 did not measure, and must record
that the default is **unresolved pending evidence**. Flagged to the operator rather than
flipped unilaterally: both settings have a defensible failure mode, and the choice is
the one genuine judgement call this round surfaced. See §5.

**HR-012 — H-Q-4 keeps risking money when reward is zero. CRITICAL.** When a market
stops qualifying, H-Q-4 says *"Do not place; do not cancel what is already resting."*
Reward for that interval is zero for everyone (S3's gate), but our orders stay live and
fillable, taking directional inventory during an interval that cannot pay. The
gated-out accounting correctly excludes it from our uptime denominator, which makes the
loss invisible in the metric.

**Resolution.** After a debounce (to avoid churning on transient blips — the reason the
original rule was written), cancel and verify all **adding** orders. If `q ≠ 0`, keep
only the aggregate-capped reducer. Re-enter only from a fresh, complete, qualifying book.

### 3.6 Verification gates that pass against broken implementations

This was the category explicitly requested, and it produced the round's highest-value
findings.

**HR-001 — a mutation that survives every gate in §17. CRITICAL.** The reviewer was
asked to name one. It did:

> **M13 — on entry to `SETTLING`, cancel every order and never place the capped
> reducer.** With `q=+80` and a 16:00 close, this abandons inventory for four hours —
> `probebot.py`'s exact defect, restricted to the close window.

Why every gate passes: **A4 expressly exempts `SETTLING`** ("…**or** is in `SETTLING`").
A5 keeps receiving rows. V1.10 tests deadline arithmetic, not resulting orders. V4 has no
close-action fault. V5's list does not contain it. V6 is a zero-write run. V7 is one
market at minimum size and need not cross `close_lead` at all.

The A4 exemption was written to acknowledge that inventory settles at close. It is far
too wide: it opens at `close_time − 4h` and covers the entire window in which the exit
still exists.

**Resolution.** A4's `SETTLING` exemption begins only at `final_lead`, trading close, or
`q = 0` — not at `SETTLING` entry. Before `final_lead`, `SETTLING` must have a resting or
in-flight reducer of aggregate quantity ≤ `|q|`. **M13 added to V5.** A full close-
lifecycle test is added to V1 and drilled in V7.

**HR-018 — V7.4 is tautological. MAJOR.** H-POS-1 sets `q_local := q_exch` on every poll.
V7.4 then requires `q_local == q_exch` on ≥1000 consecutive polls. **Overwrite-then-compare
always passes**, including against an implementation with no incremental fill handling at
all. The gate that exists to prove the position model works cannot fail.
*Resolution:* record `q_before` and compare **before** overwrite; V7.4 must induce YES,
NO, partial, duplicate and out-of-order fill reports during the sequence and bound
reaction latency; new mutation **M15** (ignore fill events until the position poll) must
fail it.

**HR-014 — the simulator's fill oracle contradicts the code it cites. MAJOR.** §17 V2
says the fill model "is not invented — it is the measured one," citing `core`'s
`trade_through`. `core/rig.go:13-39` says the opposite, in a comment marked *"This
retraction is load-bearing … Do not delete it as prose"*: a print at your price does not
prove you filled (you may be behind the queue), and even a trade-through is not proof
(maker-side STP cancels the resting maker and continues at worse prices; a cancel landing
microseconds earlier looks identical). With 1,000 contracts ahead of ours at 50, a
100-lot print at 50 fills us in the sim and fills us zero live.
*Resolution:* the sim simulates the **range of legal allocations** — zero, partial, full,
delayed reporting, cancel race, multiple fills — rather than one invented truth, and
strict trade-through supplies only a conditional lower bound under stated assumptions.
Queue position is explicitly unobservable. Calibrated against private V7 fills. It also
stops rounding fractional trade prints through `ParsePriceCents` (HR-002's residual).

**HR-022 — V6 forbids the writes it requires. MAJOR.** V6 demands "zero non-GET requests
attempted (asserted, not observed)" *and* "every fault in V4 drilled at least once". V4.10
needs a real balance reject, V4.11 an accepted placement whose response is then lost,
V4.13 our own taker fill, V4.17 real open inventory through a SIGKILL. **None can occur
under a client that structurally refuses writes.** The two gates cannot both be met.
*Resolution:* each V4 row is classified **sim-only / live-read-only / live-minimum-size**.
Write-dependent faults move to V7. V8 names the artifact proving each injection fired.

### 3.7 Remaining material findings

| # | Sev | Finding | Resolution |
|---|---|---|---|
| HR-015 | MAJOR | F5 compares **touch price only**, so a book that is price-correct but size-stale (YES 50×1300 vs a real 50×10) is blessed as healthy — while qualification and `external_best` subtraction are both size-dependent. V4.5 passes because its injected fault moves a price, leaving the literal defect untested. | Compare prices **and sizes** through the full Target Size walk on both sides, including expected own-size subtraction. Add same-price-depth, deep-walk and own-subtraction fault cases. |
| HR-016 | MAJOR | F2 retains book state across a clean close because P25a says so — but P25a is a *fidelity* rule for `cmd/rig`, not a safety property for a trader. A 500ms gap in which 50/51 becomes 40/60 leaves us quoting 50 into a real 40 bid: `post_only` accepts it, and we have improved the real touch by ten cents. | Every disconnect makes books **non-actionable** until resnapshot and portfolio reconciliation complete. Old state is diagnostic only. V4.2 requires quarantine, not retained tradability. |
| HR-017 | MAJOR | `schedule_poll_s = 300` cannot enforce `final_lead = 60s`. A `close_time` that moves from 17:00 to 12:03 at 12:00:01 is next observed at 12:05 — after close. Neither the close lead nor the final cancel ran. Not `can_close_early`; an ordinary schedule update. | Poll materially faster than `final_lead`, or consume market-status events. A newly observed close already inside a lead runs catch-up **synchronously**, final cancel first. Test a close moved inside one polling interval. |
| HR-019 | MAJOR | §6.6 puts cancels at **P0 (bypasses the bucket)** and requotes at P3 — so on an upward requote the cancel dispatches *first*, silently inverting H-Q-9's mandated place-then-cancel into cancel-then-place. K workers preserve neither arrival nor ACK order. A cancel storm can also take 429s, occupy every worker in backoff and starve P1 reducers; and cancelling a reducer does not "only reduce exposure" — it removes the exit. | Dependent actions stop being independent queue entries: the cancel becomes eligible only after the replacement ACKs. Reserve exchange capacity for reducers; coalesce and rate-limit cancels. Test an unbounded P0/429 storm with a reducer waiting. |
| HR-020 | MAJOR | H-ORD-2 forbids retrying any ambiguous write; H-ORD-4 requires a sweep that still finds our order to "retry once". A DELETE is a write — the two rules contradict. Worse, comparing a retry's `reduced_by = 0` against a stale requested size can be **misread as a fill** when `q` never moved. | Distinguish creates from cancels explicitly. Duplicate cancels **may** be retried, because they are risk-decreasing. `reduced_by` proves only what that DELETE removed and never implies position movement. Until reconciled, carry maximum possibly-live quantity in risk. |
| HR-021 | MAJOR | §12 makes `P&L ≤ pnl_kill` a **global** halt trigger and §16 sets it to −$75, but P&L is never defined: no cost basis, no realized/unrealized split, no mark source or maximum mark age, no treatment of LIP rewards, deposits, or foreign flows. A balance-based reading sees no loss at all, and an incoming $100 reward masks a $90 drawdown entirely. Both readings satisfy the spec. | Define P&L algebraically from authoritative fills and cost basis, realized flows, and a mark with a stated maximum age; state the treatment of rewards and external cash flows; add exact-threshold and unavailable-mark tests. **Otherwise remove the trigger** — an undefined global kill switch is worse than none. |
| HR-023 | MAJOR | §15 mandates a single writer, but H-TOP-4 gives `hstore` to the owner, §8.3 has the **monitor** writing `snap`/`uptime`, and §13.1 has **ping** updating `anomaly` — three writers. Separately, F19 says keep trading and monitoring through SQLite failure, while A5 cannot write a `snap` row and A9 cannot write the transition the failure itself causes; A5's production response is `SEV1` + `WINDING_DOWN`, so F19 and A5 directly contradict. An on-disk journal is not a fallback for a full or stalled disk. | One dedicated DB-writer goroutine accepting immutable records. **Separate sample-production freshness from persistence freshness** — A5 asserts the former. Persistence failure stops adding risk while reduction and in-memory observation continue; heartbeats report persistence loss explicitly. Test lock stalls and disk-full, not just returned errors. |
| HR-026 | MINOR | `q` is `float64` and fills are fractional — **measured 20.5% of resting sizes in the sampled tape are fractional**, so this is reachable, not theoretical. Fills of 0.10, 0.20 and −0.30 leave a residue near 5.6e-17. The market is economically flat but `q ≠ 0`, so it stays `REDUCING` and SIGTERM never sees drain; `SETTLING` then formats `|q|` as `"0.00"` and rejects in a loop. | Store quantities in the exchange's exact fixed-point quantum, or quantize before every sign/zero transition. Validate that a formatted `count` is positive and ≤ the quantized position. |

---

## 4. What the reviewer tried and could not break

Recorded because absence of a finding here is informative:

- The integer-cent YES/NO wire transform (H-CO-1) is algebraically correct for
  exactly-representable integer prices. **And the book is always integer-cent (§1), so
  the qualifier the reviewer attached to this is itself unreachable.** H-CO-1 stands
  unconditionally.
- Equal-size YES and NO maker fills at prices summing below 100 do lock in a positive
  amount under immediate netting and zero maker fees. **S5 and H-SEL-7 stand.**
- A single non-overlapping `SETTLING` reducer capped at `|q|` genuinely cannot flip
  inventory. The flip requires the permitted overlap (HR-004) or the `|q|+S` formula
  (HR-003) — i.e. the rule is sound and its two implementations were not.
- The separate monitor goroutine **does** prevent `probebot.py`'s exact control-flow
  break. I2 achieves what it was written to achieve; HR-007 is a different route to the
  same observable.
- Assuming the exchange honours `post_only`, no taker fill could be produced without an
  API or semantic failure. **H-Q-3's structural enforcement stands**; its *detection*
  (H-ORD-8) depends on complete fill pagination, which is HR-013.
- A complete, current, authoritative cancel sweep does establish cancellation. H-ORD-4's
  idea is sound; its failures are pagination, ambiguous retry, transport loss, and
  exposure in the window before the sweep.

---

## 5. The one question this round could not settle for me

Everything above is adjudicated and patched. **HR-011 was the exception** — a
genuine judgement call with a real cost on both sides. Put to the operator, who
was unsure; **resolved on argument, instrumented for evidence.** The options were:

- **Keep the reducing quote inside the close window** (current default,
  `close_lead_keep_reducing = true`): the exit stays alive. Cost: near a knowable
  outcome, the reducer is an option written to informed takers — it fills exactly when
  we are winning, converting $100 of settlement value into $50.
- **Cancel it** (`false`, decision 2 read strictly): no adverse fill near resolution.
  Cost: this is structurally the probe's defect — removing the exit and keeping the risk
  — and inventory settles unmanaged.

S1 does not adjudicate this, because S1 measured unconditional settlement-versus-mid,
not settlement conditional on our maker order being selected. **The evidence to decide it
does not exist yet**, and generating it *as a study* would be an economics measurement —
which §18 puts explicitly out of scope.

**Decision: keep the reducer (`close_lead_keep_reducing = true`), and cut `close_lead`
from 4h to 60m.** Reasoning:

1. **HR-011's argument does not stop at the close window.** A resting reducer is
   adversely selected at all times — §3a measured at-touch fills as *completely*
   adversely selected under normal conditions. Followed to its conclusion the argument
   indicts the reducing quote in general, which is the mechanism S1 requires and the
   whole design rests on. We already accept that trade because crossing costs 2.4–2.8c
   with certainty. Near resolution the *magnitude* changes, not the sign — so the
   proportionate response is to shorten exposure, not delete the exit.
2. **The reducer follows the touch** (H-Q-6, H-Q-8). If YES becomes certain the NO touch
   collapses and our NO bid follows it down — we buy NO at 2c, not 50c. HR-011's
   $100-for-$50 case needs the book to be *stale* while the outcome is knowable, i.e. an
   informed taker beating our requote. Real, but far narrower than "the last four hours".
3. **`false` removes the exit, and that failure mode has already cost real money on this
   account.** Between a bounded adverse-selection cost and the exact structural defect
   the harness exists to eliminate, the first is the better risk to hold.

**Instrumented rather than asserted:** V7.11 records fill-conditioned markout of every
reducer fill bucketed by time-to-close, so the next revision decides this with data. That
is telemetry on our own fills — not an economics probe, and so not what §18 excludes.

This is a judgement call made on argument. It is flagged as such in the spec at
H-CLOSE-2 and in §19.1, and it is cheap to reverse: one bool and one duration.

---

## 6. Follow-on facts established while patching

- **V1.8 answered YES** (read-only, 2026-08-04): `/portfolio/fills` returns
  `trade_id`, non-null and unique across all 15 fills on the account. H-ORD-6's
  join key exists; §7.6 needed no re-specification. The same response confirmed
  `is_taker`, `fee_cost`, `order_id`, `count_fp` and a top-level `cursor` —
  i.e. the taker detector, its corroborator, the ownership-ledger key, fractional
  counts, and the pagination mechanism H-PAGE-1 requires all exist.
- **V1.7 answered YES** (live minimum-size, operator-authorised, 2026-08-04):
  **Kalshi dedupes on `client_order_id`.** Two creates with an identical coid,
  1 contract YES @ 2c `post_only` in `KXALBUMRELEASEDATE-NEWDON-27OCT01` →
  create #1 `200`, create #2 **`409 order_already_exists`**, exactly one resting
  order, cancel `reduced_by 1.00`, sweep clean. Total exposure $0.02; nothing
  left on the account.

  **This resolves HR-006 rather than merely constraining around it.** The
  `UNKNOWN` protocol becomes self-resolving: retry the *same* coid, and a 409 is
  a **positive identification** that the original landed. We never have to prove
  absence, so H-PAGE-1's pagination trap cannot produce a duplicate order
  (H-ORD-2b). Note this also retroactively justifies H-ORD-1's deterministic
  coid on a ground the spec had not claimed: `probebot.py`'s per-attempt
  `uuid4()` makes the mechanism unavailable entirely.

  Caveat recorded in the spec: **observed, not documented** — one account, one
  market, one day — so A10 still forbids new-coid retries, a 409 is still
  followed by a confirming read, and `retry_same_coid_max = 3`.

- **HR-006's severity is unchanged by this.** The finding was correct: the spec's
  claim that §7.2 was "correct under either answer" was false. The answer merely
  turned out to be the favourable one, which is luck, not design.

## 7. Standing constraint

This was the single adversarial pass. **Codex is not used again in any capacity** — no
resume thread, no audit gate, no confirmation round, no re-review of these patches. All
adjudication above is mine, checked against the repository and against arithmetic, with
the evidence shown inline so it can be audited without re-running the round.
