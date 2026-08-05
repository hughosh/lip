# The pilot plan — what actually ships first

**Status:** supersedes `harness-spec.md` §17's ladder *for the pilot only*.
Written 2026-08-05 after a cross-model scope review (`loop/out-02-scopecut.md`).

`harness-spec.md` remains the design contract. Every rule in it still binds
where it is implemented. This document says **what gets built first and what
waits**, because §17 was sized for a $500 six-market deployment and the pilot is
$100 on one market.

---

## 0. The real risk bound, corrected

The opening six-market one-sided fill set costs at most `6 × 12 × $0.99 =
$71.28`. **That is not the lifetime maximum.** Orders cycle, thresholds are
crossed between observations, and both `inv_kill` and `pnl_kill` only enter
`WINDING_DOWN` — they stop *adding*, they do not flatten.

> **The only hard external bound is the account balance.** And it stops being a
> bound at all if the account holds other funds while the capital-accounting
> code is the thing that is wrong.

Therefore, as a **precondition, not a preference**: the Kalshi account contains
exactly the pilot capital and nothing else. This is the single most effective
risk control available and it costs nothing.

## 1. Capital is a ladder, not a decision

Safety properties do not change with capital — never crossing, the reducer
staying alive, the monitor never going quiet, position truth from the exchange
are identical at $1 and at $500. Capital is a `cfg` knob. What changes with size
is only statistical power on the reward model.

| Rung | Size | Question it answers |
|---|---|---|
| canary | 1 market, `S=1`, ~$1 | does the order path work? fills, attribution, `is_taker=false`, verified cancel, restart adoption |
| pilot | 1 market, `S=12`, $100 | do we get paid, and does the scoring model predict the payout? |
| second market | 2 markets, `S=12` | does anything break with concurrency? |
| scale | 6 markets, larger `S`, $500 | revenue — **only after a real payout is observed** |

Nothing above requires a decision now. Build once; raise the knob.

## 2. What is CUT or DEFERRED, and what it costs

| # | Cut | Saves | Cost |
|---|---|---|---|
| 1 | **V2's tape-driven exchange emulator and combinatorial fill allocator** → replaced (§3) | 5–12h | no long deterministic market replay; less selection realism |
| 2 | **Six-market automation.** H-SEL-1/2/3/5/9/10, automated reselection, H-CAP-2 concentration. One hand-picked ticker. | 4–10h | no diversification; manual market replacement |
| 3 | **§15's eleven tables → five.** Keep `run`, the durable ownership ledger, `our_fill`, `state_event`, `anomaly`. Defer `market`, dense `position_poll`, `snap`, `balance_poll`, `uptime`. | 3–7h | no exact uptime/share reconstruction, no payout calibration — research losses, not safety losses |
| 4 | **V4: 18 harness cases → 6 integrated scenarios** (§4) | 3–6h | less evidence on rare availability failures |
| 5 | **V5: 23 mutations → 12**, and **stop running the whole catalogue every unit** — run the unit's mutation, full suite at milestones | 2–5h + much repeated gate time | less redundancy |
| 6 | **V6: 72h → 4–6h read-only**, forcing disconnect, resnapshot, restart, close-time jump | **66–68h wall clock** | lower confidence on long-memory leaks |
| 7 | H-Q-9/H-QUE-1 place-then-cancel → cancel-confirm-place everywhere; H-QUE-3 multi-worker → one REST writer; H-FAIL-6 full-depth compare → quarantine+resnapshot; H-DEP-1 IP fallback; H-CLOSE-4 (exclude `can_close_early` instead) | several hours | brief scoring gaps, lower write throughput, more downtime |
| 8 | V7.6, V7.8, V7.11 (fill import, payout prediction, reducer markout); V7.9/V7.10 live fault manufacture; V7.4's 1000 polls → 100 + scripted events | — | post-revenue analytics |

**Do not delete already-built work** (the priority queue is done and correct);
simply do not make launch depend on the deferred optimisations.

## 3. What replaces V2 — a scenario exchange, not a simulator

Not a market. A small deterministic fake with an injected clock, an order
ledger, coid idempotency, scripted REST results, scripted websocket events, and
restartable state. A scenario reads:

```
book → place accepted → response dropped → retry same coid → 409
→ partial fill → delayed fill report → cancel ignored → sweep finds order
→ position poll disagrees → restart → adopt position → reduce to flat
```

**Fills are explicit test inputs** — zero, partial, full, delayed, duplicate,
out-of-order, multiple, cancel-race — never inferred from public prints. This is
*more* honest than V2-FILL, because `core/rig.go:13-39` already establishes that
neither a print at our price nor a trade-through proves our private order
filled. V2-FILL was precise machinery around an unknowable input.

Keep a handful of captured-wire fixtures proving real snapshots, deltas, orders,
positions, fills and cursor fields parse correctly.

## 4. The six retained fault scenarios

1. half-open / disconnect / wedged feed → quarantine, reconnect, resnapshot, reconcile, REST cancel
2. accepted create with lost response → same-coid recovery, no duplicate
3. ignored / partial cancel → sweep still finds it, retry stays safe
4. stale or divergent portfolio truth → dispatch stops, market reduces
5. SIGKILL / restart with inventory + durable halt latch
6. clock / close-time jump → catch-up actions and final cancel not skipped

V4.8/4.10/4.13/4.14/4.15/4.16 become simple response-path unit tests. V4.6 (live
DNS wedge) and V4.7 (host sleep) are deferred.

## 5. Retained mutations

M1, M2, M3, M4, M7, M11, M12, M13, M14, M19, M20, M21. M17 folds into M4, M18
into M7. M5, M9, M10, M15, M16, M22, M23 keep ordinary behavioural tests but
stop paying a separate mutation turn — their tests are already direct and
discriminating.

## 6. What must NOT be cut at any capital

- H-Q-3 / A1 / A2 — `post_only` structural, **no taker code path exists**
- H-ORD-1 / 2b / 4 / 5, H-PAGE-1 / 1a — deterministic coids, same-coid recovery, verified cancel, complete pagination, reconcile before placing
- H-Q-5a / 5b, H-CAP-1 / 4 / 7 / 8 — exact quantities, aggregate exposure, funded reducers, no sign-flipping exit
- I1 / I2 / I3, A4 / A5 — never abandon inventory or monitoring; detect a frozen owner rather than reporting stale truth as fresh
- H-FAIL-3 / 4 / 5 — cancel-requested is still live; stale truth and disconnected books cannot authorise a placement
- H-HALT-3 / 4 and startup adoption — the halt survives restart; a restarted process recovers every position before acting
- H-CLOSE-3 — verified final cancel before close
- Dedicated account, one process, supervision, SEV1 alert, dead-man heartbeat

These are not about the size of the loss. They are the difference between a
bounded loss and an **unobserved** one, which is the failure that already cost
real money.

## 7. Build order

1. **Pilot profile**: dedicated account with exactly the pilot capital; one
   operator-chosen ticker; no `can_close_early` markets; one REST writer;
   cancel-confirm-place only.
2. **Finish the risk-bearing pure logic**: skew/reducer sizing, `external_best`,
   the state machine, aggregate caps, fundability, stale-truth checks, close
   catch-up, final cancel.
3. **Minimal REST**: exact payload, structural `post_only`, deterministic coid,
   same-coid ambiguous-create recovery, guarded pagination, positions/orders/
   fills/balance, cancel sweep.
4. **Conservative websocket + ownership wiring**: ping/pong, reconnect,
   quarantine after *every* disconnect, resnapshot, reconcile, local fill
   reaction, 5s authoritative position overwrite. No DNS-IP fallback.
5. **Lifecycle safety**: advancing-sequence monitor, durable halt latch, startup
   adoption, foreign-activity assertion, single-instance lock, launchd +
   caffeinate.
6. **Five-record store + minimal alerts**: ownership durable *before* dispatch;
   a storage failure stops adding but never stops reducing or monitoring.
   External missed-heartbeat alarm configured before any unattended live writer.
7. **Reduced gates**: targeted V1, the scenario exchange, six faults, twelve
   mutations. Full catalogue run **once**, here.
8. **4–6 hours read-only**, forcing disconnect, resnapshot, restart, schedule jump.
9. **Canary: one market, `S=1`.** The **first directional fill latches
   `WINDING_DOWN`** — that bounds the canary to ~$1 without the full P&L kill.
   Demonstrate create visibility, verified cancel, fill attribution, zero taker
   fees, one-sided reduction, restart adoption.
10. **Raise to `S=12`** after a clean canary. Stop after the first directional
    fill and inspect. Build the exact H-HALT-5 P&L kill **before** allowing
    repeated unattended fill cycles.
11. **Second market only after a clean reduction/restart cycle. Markets three
    through six and dynamic selection only after an observed payout.**

## 8. Deliberately skipped until after first returns

Full tape simulator · automated reselection · multi-worker scheduling ·
eleven-table telemetry · Go share scoring · long DNS/sleep drills · the 72-hour
V6 · full V5 ceremony · manufactured live faults · payout-prediction gating ·
fill import · reducer markout research.
