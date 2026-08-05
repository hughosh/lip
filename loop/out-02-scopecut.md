## PROPORTIONALITY

No. The unchanged §17 ladder is not proportionate to a $100 pilot. It is acceptance testing for a reusable multi-market trading platform, not a bounded experiment whose economics remain unverified.

| Bound | Amount | What it really means |
|---|---:|---|
| Adding capital after reserve | $75 | `100 × (1−0.25)` |
| Initial six-market one-sided fill set | **$71.28** | `6 × 12 × $0.99` |
| Inventory kill threshold | 108 contracts | `6 × 18`; a trigger, not a cap |
| P&L kill | −$15 | Stops adding; does not flatten or limit residual loss |
| Hard account loss | **$100** | Only if the dedicated account actually contains no more than $100 |

Thus $71.28 is the worst loss from the opening simultaneous one-sided fill set if all six positions settle worthless. It is not the lifetime maximum: orders can cycle, thresholds can be crossed between observations, and both `inv_kill` and `pnl_kill` merely enter `WINDING_DOWN`. Under the spec’s funded-collateral model, the real external bound is the account balance: $100. If the account contains other funds, even that bound disappears when the capital-accounting code is the thing that is wrong.

The current plan implies roughly 56–120 substantive units, or about 19–60 loop-hours at the stated cadence, followed by 72 hours of zero-write observation. That is plainly too much pre-revenue investment to insure $100.

The proportionate ladder is:

- Keep targeted V1 tests for money, quantity, wire encoding, pagination, capital, reduction, reconciliation, and close handling.
- Keep production invariants that can stop abandonment, duplication, taking, stale-truth placement, or excess exposure.
- Replace V2 with explicit event scenarios.
- Reduce V4 to six integrated failure scenarios plus cheap response-unit tests.
- Reduce V5 to the mutations capable of losing most of the account.
- Replace V6 with 4–6 hours read-only.
- Move V7’s core $1 canary earlier and let small live exposure provide evidence that public replay fundamentally cannot.

## CUT

1. **Replace full V2 — largest development saving, roughly 5–12 hours.** Do not build tape-driven exchange emulation or a combinatorial fill allocator. Public prints cannot reveal private queue position, so V2-FILL’s sophistication is mostly precise machinery around unknowable inputs. Cost: no multi-hour deterministic market replay and less selection realism. The cheaper replacement is described under SIMULATOR.

2. **Defer six-market automation — roughly 4–10 hours.** First live should be one manually chosen, fixed ticker at `S=1`, then `S=12`; not six markets and not one giant order. Defer H-SEL-1–3, H-SEL-5, H-SEL-9/10, automated reselection, H-CAP-2 concentration machinery, and multi-market fairness. Retain live qualification, price-sum, tenor, freshness, and “never deselect inventory” checks: H-Q-4, H-SEL-4/7/8/11. Cost: lower modeled revenue, no diversification, and manual replacement when the market deteriorates. Two fixed markets can follow; six dynamic markets should wait for a payout.

3. **Replace §15’s eleven-table store with five durable records — roughly 3–7 hours.** Keep `run`, a durable order/ownership ledger, `our_fill`, `state_event`, and `anomaly`. Merge intent data into order events. Defer `market`, dense `position_poll`, `snap`, `balance_poll`, and `uptime`. Persist drifts and failures, not an agreeing row every five seconds. Cost: no exact uptime/share reconstruction, weaker post-mortem detail, and no payout-model calibration. Those are research losses, not capital-safety losses.

   Keep `harness/ping`, but shrink it: immediate SEV1, startup/drain notifications, hourly heartbeat, and an external missed-heartbeat check. Defer durable delivery ordering, per-class token buckets, suppression counts, and SEV3 batching. Cost: an ntfy outage may lose individual messages, although the dead-man still reveals sustained silence.

4. **Reduce V4 from eighteen harness cases to six integrated scenarios — roughly 3–6 hours.** Keep:

   - Half-open/disconnect/wedged-feed → quarantine, reconnect, resnapshot, reconcile, and REST cancel.
   - Accepted create with lost response → same-coid recovery and no duplicate.
   - Ignored/partial cancel → sweep still finds it and retry remains safe.
   - Stale or divergent portfolio truth → stop dispatch and reduce/wind down.
   - SIGKILL/restart with inventory and a durable halt latch.
   - Clock/close-time jump → catch-up actions and final cancel are not skipped.

   Make V4.8, V4.10, V4.13, V4.14, V4.15, and V4.16 simple response-path tests. Treat clean and abnormal disconnects identically. Defer the live DNS-wedge drill V4.6, host-sleep drill V4.7, and the 50-order reject-rate apparatus V4.9. Cost: less evidence for rare availability failures and fewer nuanced responses; these can cost uptime, but cannot exceed the account’s external cash bound if stale-truth and cancel rules work.

5. **Reduce V5 from 23 mutations to 12 safety mutations — roughly 2–5 hours, plus less repeated gate time.** Keep M1, M2, M3, M4, M7, M11, M12, M13, M14, M19, M20, and M21. Fold M17 into M4 and M18 into M7.

   Keep ordinary behavioral tests, but stop paying a separate mutation turn, for M5, M9, M10, M15, M16, M22, and M23. Their tests are direct and discriminating: exact payloads, self-chase fixtures, all-price transforms, scripted fill events, and page-two/cursor-rewind fixtures. Defer M6 with dynamic selection and M8 with dense persistence.

   Also stop running the entire growing mutation catalogue on every unit. Run the current unit’s mutation when relevant and the full suite at milestones/final acceptance. The current script’s M14b/M26/M26a variants are useful engineering history, but not additional launch obligations; M26 is explicitly expected to survive.

6. **Replace the 72-hour V6 with 4–6 hours read-only, saving 66–68 hours of wall clock.** Force reconnect, stale feed, schedule jump, and one process restart during that window. The full three days primarily test uptime, DNS, sleep, and leaks; they exercise none of the dangerous write paths. Continue observation during the subsequent `S=1` live canary instead. Cost: lower confidence in rare host failures and long-memory leaks.

   The missed-heartbeat alarm should be required before an unattended live writer, not before a read-only run. It is cheap and protects the operator, but it should not block six harmless hours of GETs.

7. **Defer these additional launch optimizations:**

   - H-Q-9/H-QUE-1 place-then-cancel: use cancel-confirm-place everywhere. Cost: brief scoring gaps. The queue is already built, so do not delete it; simply avoid making launch depend on this optimization.
   - H-QUE-3 multi-worker reservation: run one REST writer for the one-market pilot. Cost: lower write throughput.
   - H-FAIL-6 full Target-Size REST comparison: on quiet/stale data, quarantine and resnapshot unconditionally. Cost: more downtime, much less comparison machinery.
   - H-DEP-1 last-known-good-IP fallback and V4.6: ordinary reconnect plus alerts first. Cost: downtime during a real resolver wedge.
   - H-HALT-5’s exact marked P&L for the `S=1` one-fill canary. Replace it initially with “first directional fill latches winding-down.” Build the proper P&L kill before continuous `S=12` operation. Cost: without either safeguard, repeated small losses could consume the full account.
   - H-CLOSE-4: exclude `can_close_early` markets during the pilot instead of supporting them.
   - V7.6, V7.8, and V7.11: fill import, payout prediction, and reducer markout are post-revenue analytics.
   - V7.9’s deliberately induced live balance/ambiguous/taker faults and V7.10’s forced live close with inventory. Test them with event scenarios; do not manufacture dangerous live conditions merely to satisfy a matrix.
   - Replace V7.4’s 1,000 polls with 100 live polls plus scripted duplicate/out-of-order/partial events.

## KEEP

- H-Q-3/A1/A2: `post_only` structurally fixed; no taker-order code path.
- H-ORD-1/2b/4/5 and H-PAGE-1/1a: deterministic coids, same-coid recovery, verified cancel, full pagination, reconcile before placing.
- H-Q-5a/5b and H-CAP-1/4/7/8: exact quantities, aggregate exposure, funded reducers, and no sign-flipping exit.
- I1/I2/I3 plus A4/A5: never abandon inventory or monitoring; detect a frozen owner rather than reporting stale truth as fresh.
- H-FAIL-3/4/5: cancel-requested is still live; stale portfolio data and disconnected books cannot authorize placements.
- H-HALT-3/4 and startup adoption: halt survives restart, and a restarted process recovers every position before acting.
- H-CLOSE-3: verified final cancel before close.
- Dedicated account, one process, supervision, SEV1 alert, and dead-man heartbeat.

## SIMULATOR

**Verdict: replace V2.**

Build a small deterministic scenario exchange, not a simulated market. It needs an injected clock, an order ledger, coid idempotency, scripted REST results, scripted websocket events, and restartable exchange state. A scenario should be able to say:

```text
book → place accepted → response dropped → retry same coid → 409
→ partial fill → delayed fill report → cancel ignored → sweep finds order
→ position poll disagrees → restart → adopt position → reduce to flat
```

Fills should be explicit test inputs: zero, partial, full, delayed, duplicate, out-of-order, multiple, and cancel-race. Run each relevant transition over those variants. Do not infer fills from public prints. That is more honest than V2-FILL because [the repository’s own `core.Rig` contract](/Users/hugh/kek/lip/go/core/rig.go:13) says neither a print at the price nor a trade-through proves our private order filled.

Retain only a few captured-wire fixtures to prove real snapshots, deltas, orders, positions, fills, and cursor fields parse correctly. The scenario exchange then buys nearly all confidence needed for V3, the six retained V4 cases, and the retained V5 mutations.

What is lost is realistic long-duration selection, queue competition, and replay reproducibility over a whole market tape. None of those can establish private fill truth, and none is worth delaying a $1 canary.

## MODEL TIERING

Official guidance positions GPT-5.6 Sol as the flagship, Terra as strong lower-cost performance, and Luna as the efficient high-volume tier; it also recommends testing the same reasoning level and one level lower on representative work. The routing below applies that distinction to this loop. [OpenAI model guidance](https://developers.openai.com/api/docs/guides/latest-model)

| Turn | Recommendation | Expected failure if downgraded further |
|---|---|---|
| Driver | `gpt-5.6-sol`, `xhigh` | Keep this frontier. Its job is semantic decomposition across a 2,283-line contract. The observed wrong-test `ALREADY_SATISFIED` closure is exactly the failure: a real symbol is cited for a neighboring obligation. |
| Implementer | `gpt-5.6-terra`, `high` | At Luna/low, expect code that compiles and covers the happy path but misses state conjunctions, ambiguous outcomes, or a falsifiable test. A bounded directive does not require Sol/max. |
| Audit | `gpt-5.6-terra`, `high`; use `sol/high` for `ALREADY_SATISFIED` claims | At medium/low, expect either rubber-stamping or style-based false DRIFT. The no-diff `ALREADY_SATISFIED` path specifically needs semantic evidence matching, not `grep`. |
| Challenge | `gpt-5.6-sol`, `max`, only for safety-bearing units | This genuinely needs frontier reasoning. A downgrade typically yields a non-compiling, unreachable, duplicate, or cosmetic mutation and consumes the one challenge without testing the oracle. |
| Adjudicate | No model on the green path; if mandatory, `gpt-5.6-luna`, `low`. On divergence use `gpt-5.6-sol`, `xhigh`; reserve `max` for a surviving mutation plus conflicting evidence. | A cheap model on divergence may confuse CAUGHT with a defect, overlook a SURVIVED oracle gap, or average contradictory evidence into an unjustified ADVANCE. |

The conductor can deterministically advance when gates are green, audit is `FAITHFUL/CLEAN`, and no mutation survived. Spending five minutes of frontier reasoning to reproduce that Boolean is waste.

One protocol change matters more than any model upgrade: `ALREADY_SATISFIED` must receive an independent semantic audit before closure. Merely proving the named test exists has already failed once.

I would not route a safety role to `gpt-5.6-sol-wm` solely from its slug; the public model guide does not characterize that variant. Nor is `ultra` justified for any bounded turn here.

## FASTEST PATH TO FIRST REVENUE

1. **Fix the pilot profile:** dedicated account containing exactly $100; one operator-selected ticker; no early-close markets; one REST worker; cancel-confirm-place only. Start with `S=1`, then `S=12`. One `S=12` one-sided fill costs at most $11.88, below the nominal −$15 kill.

2. **Finish the risk-bearing pure logic:** skew/reducer sizing, external touch, state machine, exact aggregate caps, fundability, stale-truth checks, close catch-up, and final cancel.

3. **Build the minimal REST layer:** exact payload, structural `post_only`, deterministic coid, same-coid ambiguous-create recovery, complete guarded pagination, positions/orders/fills/balance reads, and cancel sweep.

4. **Build conservative websocket and ownership wiring:** ping/pong, reconnect, quarantine after every disconnect, resnapshot, portfolio reconcile, local fill reaction, and five-second authoritative position overwrite. Skip DNS-IP fallback.

5. **Wire lifecycle safety:** independent advancing-sequence monitor, durable halt latch, startup adoption, dedicated-account foreign-activity assertion, single-instance lock, and launchd/caffeinate supervision.

6. **Add the five-record operational store and minimal alerts:** order ownership must be durable before dispatch; storage failure stops adding but must not stop reduction or monitoring. Configure the external missed-heartbeat alarm before leaving live orders unattended.

7. **Run the reduced gates:** targeted V1, the deterministic event-scenario exchange, six integrated faults, and the twelve safety mutation controls. Run the full mutation catalogue once here, not after every preceding unit.

8. **Run 4–6 hours read-only**, forcing disconnect, resnapshot, restart, and a close-time jump.

9. **Go live on one market at `S=1`.** Demonstrate create visibility, verified cancel, fill attribution, zero taker fees, one-sided reduction, and restart adoption. The first directional fill should latch `WINDING_DOWN`; this bounds the canary to roughly $1.

10. **Raise the same market to `S=12` after the canary is clean.** Initially stop after the first directional fill and inspect it. Add the exact P&L kill before allowing repeated unattended fill cycles. This begins meaningful reward accrual without waiting for six-market machinery.

11. **Add a second fixed market only after a clean reduction/restart cycle; add dynamic selection and markets three through six only after the first actual payout.**

Consciously skipped until after first returns: full tape simulator, automated reselection, multi-worker scheduling, eleven-table telemetry, Go share scoring, long DNS/sleep drills, 72-hour zero-write V6, full V5 ceremony, live fault manufacture, payout-prediction gating, fill import, and reducer-markout research.