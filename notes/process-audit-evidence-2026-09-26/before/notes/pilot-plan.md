# The pilot plan — qualification ladder to continuous runtime

**Status:** revised 2026-08-09 after implementation and gate review. This
supersedes `harness-spec.md` §17's verification ladder **for the one-market
pilot only**. The spec remains the design contract: every safety rule still
binds where its behaviour is in scope. This plan decides what blocks each rung
and what evidence authorises the next one.

Section numbers used by code comments are intentionally stable. In particular,
`§2.3`, `§3`, and `§7.4` through `§7.9` retain their prior subjects.

A pilot profile may remove optional breadth or choose a more conservative
setting. It may not relax a non-negotiable safety rule outside §17. Any true
design conflict is quarantined as a `spec-patch` bead; this plan does not edit
the SHA-pinned contract until the code passes.

---

## 0. The external loss boundary

The opening six-market one-sided fill set costs at most `6 × 12 × $0.99 =
$71.28`. That is **not** a lifetime bound. Orders cycle, thresholds can be
crossed between observations, and `inv_kill` and `pnl_kill` stop adding; they do
not flatten.

> **The account balance is the only hard external bound, and it is a sound bound
> only when this harness has a dedicated account.**

Before any live writer, the selected account contains exactly the capital for
the current rung and nothing else: no unrelated cash, positions, resting
orders, or manual activity. The operator verifies the account, the credentials,
the balance, the ticker, and the halt-latch path immediately before arming
writes. Software checks for foreign activity are a backstop, not a substitute.

“Exactly” is a funding-time precondition, not a claim that settlement and
rewards cannot change the balance. During a run there are no deposits,
withdrawals, or manual trades; `capital_max` remains fixed; and any reward or
settlement proceeds above it are not deployable without an explicit rung change
and a new config review.

No command called a “dry run” may be pointed at live credentials until §6.1's
structural write guard exists. The current binary has no `--live` plus
`live_ok` two-key guard, so running it is a live-writer action.

## 1. Capital and finish lines are separate ladders

Safety properties do not weaken with capital. What changes with size is the
external loss bound and the amount of live evidence being collected.

| Rung | Size | Purpose | Is it continuous runtime? |
|---|---:|---|---|
| read-only qualification | no writes | prove live-feed, restart, schedule, monitoring, and alert behaviour | no |
| attended canary | 1 market, `S=1`, `capital_max=$2` | prove the real order path; the first directional entry durably stops adding | **no — it deliberately self-stops** |
| attended pilot burn-in | 1 market, `S=12`, at most $100 in the dedicated account | prove one full-size add → fill → reduce → restart cycle | no |
| continuous pilot (CR-1) | 1 market, `S=12`, at most $100 | permit repeated unattended fill cycles during one declared market assignment | **yes, within that assignment** |
| second market | 2 markets after a clean continuous-pilot interval | expose concurrency assumptions | yes, expanded |
| scale | up to 6 markets only after an observed payout | revenue validation | yes, expanded |

For this plan, **continuous runtime (CR-1)** means that the one-market `S=12`
writer is authorised to remain under `launchd` supervision and repeat adding
and reducing cycles without an operator babysitting each fill **during a
declared assignment to one active ticker**. It has durable stop semantics,
external missed-heartbeat detection, complete position/order/fill truth, and a
tested recovery path. It does not mean “the process stayed up once” or “the
canary can be launched.” True 24/7 quoting across programme and market turnover
is CR-2 and requires selection/rotation automation; CR-1 does not rename manual
replacement as unattended operation.

## 2. What the pilot ships

### 2.1 One-market conservative profile

- One operator-chosen ticker that is in the current active LIP universe.
- Prefer `can_close_early=false`. When the available ticker can close early,
  record that fact and use the implemented four-hour adding backoff; do not
  pretend the flag was excluded when most of the live universe carries it.
- One dispatch authority and a bounded worker pool with at least one transport
  slot reserved for P1 reducers, as H-QUE-3 requires. The current one-worker
  composition is a pre-live defect: a hung cancel or 429 backoff can own the
  only slot. Backoff never sleeps while monopolising a slot. Cancel-confirm-
  place remains mandatory on every replacement.
- No automated selection, reselection, concentration optimisation, or dynamic
  market replacement.

### 2.2 Safety and truth path

The pilot includes exact wire payloads, structural `post_only`, deterministic
coids, same-coid ambiguous-create recovery, complete guarded pagination,
verified cancel sweeps, exchange-authoritative positions, owned-fill
attribution, aggregate exposure accounting, conservative websocket quarantine,
schedule catch-up, a durable global stop, startup adoption, single-instance
locking, and independent monitoring.

Known missing readers or composition seams are not “configured features.” A
parameter that no production path reads provides no protection. The pre-canary
and pre-continuous gates in §6 name those gaps explicitly.

### 2.3 Five-record store and external alerts

Keep exactly the pilot records: `run`, durable `owned_order`, `our_fill`,
`state_event`, and `anomaly`. Ownership is durable before dispatch. A store
failure stops adding while reducing and monitoring continue.

The `hstore` and `ping` components already exist, but component existence is not
delivery. Before qualification, `cmd/harness` must run the alert service, drain
durable anomalies oldest-first, send immediate SEV1 notifications and hourly
heartbeats, and check in with an external dead-man service. The operator must
demonstrate that a missed check-in alarms; a configured URL is not evidence.

Dense `position_poll`, `snap`, `balance_poll`, and `uptime` tables remain
deferred. The qualification runner must still produce a compact evidence bundle
for its required polls, samples, and intervals; deleting a table does not delete
the fact that must be proved.

### 2.4 Work that stays deferred

Do not make the pilot wait for a tape-driven exchange simulator, inferred fill
allocation, automatic market selection, multi-worker scheduling, eleven-table
telemetry, Go payout/share scoring, DNS-IP fallback, fill import, payout
prediction, reducer markout research, or the 72-hour V6 soak.

Do not delete work that is already built. In particular, the priority queue and
the full mutation catalogue are retained and continue to ratchet.

## 3. Scenario exchange — deterministic private truth, not a market simulator

Build a small in-process exchange with an injected clock, order ledger,
deterministic coid idempotency, scripted REST outcomes, scripted websocket
events, and restartable state. Fills are explicit private inputs: zero, partial,
full, delayed, duplicate, out-of-order, multiple, and cancel-race. They are never
inferred from public prints; neither a print at our price nor a trade-through
proves that our private order filled.

A representative script is:

```text
book → place accepted → response dropped → retry same coid → 409
→ partial fill → delayed fill report → cancel ignored → sweep finds order
→ position poll disagrees → restart → adopt position → reduce to flat
```

Keep a small captured-wire fixture set for real snapshots, deltas, orders,
positions, fills, schedules, and each cursor field. The scenario exchange is a
release oracle for the composed process, not a second strategy implementation.

## 4. Eight release scenarios

The pilot release pack is intentionally smaller than `harness-spec.md` V4, but
it covers every failure that can strand exposure or make silence look healthy:

1. half-open, clean disconnect, and wedged feed → quarantine, reconnect,
   resnapshot, portfolio reconcile, and REST cancellation;
2. accepted create with lost response → same-coid recovery and no duplicate;
3. ignored or partial cancel → complete sweep, safe retry, and no false absence;
4. stale or divergent positions/orders/fills → dispatch stops, prior complete
   truth remains, and reduction resumes only from current truth;
5. SIGKILL/restart with inventory, owned orders, and a durable stop → adoption
   before action and no adding-order resurrection;
6. wall-clock or close-time jump → synchronous catch-up and verified final
   cancel are never skipped;
7. 429 storm on creates and cancels → adaptive backoff, no dropped ownership,
   and reducer/cancel capacity is preserved; and
8. SQLite or alert transport failure → adding stops when required, while
   reducing, monitoring, durable retry, and honest heartbeat health continue.

Rare availability cases may remain direct response-path tests, but each of the
eight composed scenarios must have a named artifact and a permanent mutation
that proves its catcher can fail.

## 5. Gates are a ratchet

- Inner development may use targeted tests and the quick gate.
- A bead advances only on the full build, vet, format, race, repository check,
  and mutation-negative-control gate.
- The current catalogue has 173 declared outcomes. It is retained in full; the
  earlier proposal to cut it to twelve is withdrawn because the additional
  mutations are already built evidence, not future scope.
- Every new behaviour adds a compiling semantic mutation, a reachability
  argument for the deployed rung, and a named deterministic catcher.
- A probabilistic catcher is a red gate even when one run happens to pass.
- At a milestone, run the whole catalogue once on the exact tree being
  advanced. Between milestones, run the affected mutations individually.

## 6. Entry and exit gates

### 6.1 Code-qualified for read-only operation

All of the following are required:

1. H-VER-1's two-key write guard is composed at the transport boundary:
   non-GET requests require both an explicit `--live` invocation and a present
   absolute-path `live_ok` sentinel, checked at dispatch time. Without either,
   zero non-GET requests can reach the network.
2. The flaky `M-HS-ONERUN` catcher is deterministic (`lip-ke1`).
3. Startup history cannot be replayed onto exchange-seeded position and is
   recorded with honest provenance (`lip-da6`).
4. Startup calls H-CAP-8 fundability (`lip-lpf`), and 429 handling cannot lose a
   create, cancel, or reducer (`lip-q6r`). H-QUE-3's reserved reducer transport
   slot is exercised in the same composed fault surface.
5. The alert/dead-man service in §2.3 is wired into the production process and
   a missed check-in has been demonstrated externally.
6. A pilot launchd job preserves the config's required rung in its argv; an
   `S=12` job cannot enter a refusal/KeepAlive restart loop because `-rung
   pilot` was dropped.
7. H-Q-4a's `gate_fail_debounce_s` has a production consumer: after 30 seconds
   of continuous gate failure, adding orders are cancelled and swept while a
   capped reducer remains.
8. The scenario exchange, eight scenarios, and production assertions A1–A14
   are composed and mutation-gated.
9. A row-by-row production wiring audit covers F1–F21 and every §12 trigger, so
   a configured detector is not credited merely because its component exists.
10. The full gate is green on the resulting tree.

### 6.2 Read-only qualified

Run 4–6 hours against the real feed with the write guard unarmed. Assert zero
network non-GET attempts rather than inferring it from an empty order list.
During the run force a disconnect/resnapshot, process restart, owner-stall
alarm, and schedule/clock jump. Require ≥99% monitor availability outside
explicitly gated intervals, fresh complete portfolio walks, no silent anomaly
loss, and a fired external missed-heartbeat alarm. Preserve the evidence bundle.

### 6.3 Attended canary passed

With the operator present, fund only the canary account, arm both write keys,
and run one market at `S=1`. The first post-adoption directional entry from an
ack, newly owned fill, or complete position walk must durably latch
`WINDING_DOWN`. Demonstrate create visibility, verified adding-side cancel,
owned-fill attribution, `is_taker=false`, `fee_cost=0`, a capped maker reduction
to flat, restart adoption, and no resumption of adding after flat or restart.

### 6.4 Continuous pilot authorised

Before raising to `S=12`, close the remaining repeated-cycle gaps: live
`inv_kill` (`lip-lqw`), the stuck-inventory escalation (`lip-2da`), algebraic
trading P&L and `pnl_kill` (`lip-gp8`), active-program membership
(`lip-2v0`), and any safety finding produced by the canary. Then run an attended
`S=12` burn-in through a directional fill, reduction to flat, and restart.

Continuous runtime begins only after that evidence is reviewed, the halt is
cleared by an operator, the full gate is green, no priority-0 or priority-1
safety bead remains open, and launchd + caffeinate + external dead-man
supervision are active. At that point repeated unattended cycles are permitted.

## 7. Ordered critical path

The numbering in this section is stable because existing code comments cite it.

1. **§7.1 Pilot profile — mostly delivered.** Dedicated-account config, one
   ticker, one dispatch authority, cancel-confirm-place, rung assertion,
   provision, and deploy surfaces exist. Restore H-QUE-3's reserved P1 transport
   slot before live operation. Tracker cleanup may close `lip-3af` after
   recording its delivered composition commits; the worker repair lives with
   `lip-q6r`.
2. **§7.2 Risk-bearing logic — partial.** Skew, reducers, aggregate caps,
   position truth, and close handling exist. Fix `lip-da6` and call H-CAP-8
   before the canary; implement `inv_kill`, stuck escalation, and exact P&L
   before continuous runtime.
3. **§7.3 REST and write arming — partial.** Exact payloads, coids, pagination,
   same-coid recovery, and cancel sweeps exist. Add H-VER-1 and F8/429 handling
   before any live qualification; restore and prove the reserved reducer slot.
4. **§7.4 Conservative websocket and ownership wiring — delivered.** Keep
   quarantine after every disconnect, resnapshot plus portfolio reconciliation,
   local fill reaction, and authoritative position overwrite.
5. **§7.5 Lifecycle safety — delivered after the green `lip-2t6` tree is
   committed.** Durable stop, fail-safe latch retry, startup adoption,
   single-instance lock, signal drain, and canary history boundary are present.
6. **§7.6 Five-record store and minimal alerts — partial.** The store and alert
   packages exist and the store is composed. Compose alert delivery and the
   external dead-man into `cmd/harness`; prove a missed heartbeat fires.
7. **§7.7 Release verification — partial.** Unit/race checks and 173 mutation
   outcomes are green, but `lip-ke1` makes one catcher probabilistic. Fix it,
   then add the scenario exchange, eight composed faults, and A1–A14 production
   assertions. Audit every F1–F21 and §12 consumer rather than finding dead
   configuration fields incidentally.
8. **§7.8 Read-only qualification — not started.** Complete §6.2 only after
   §6.1 is green.
9. **§7.9 Attended first-fill canary — code green, live evidence not started.**
   Commit `lip-2t6`, then execute §6.3. This rung is bounded by policy, not by an
   `inv_kill` setting, and it never resumes adding after its first directional
   entry.
10. **§7.10 Repeated-cycle safety — not complete.** Land `inv_kill`, stuck
    escalation, P&L kill, and active-program membership; run the attended
    `S=12` burn-in.
11. **§7.11 Continuous pilot (CR-1) — not started.** Authorise unattended
    operation only through §6.4 and only for the declared market assignment.
    Review after the first payout as well as after any safety latch.
12. **§7.12 Expansion / CR-2 — deferred.** A second market follows a clean
    continuous pilot. Markets three through six follow an observed payout.
    Continuous operation across programme turnover requires selection and
    rotation automation; it is not delivered by a CR-1 process idling in
    `DRAINED`.

## 8. Current estimate

As of the green 2026-08-09 `lip-2t6` tree:

- **about 70% to an attended canary**, and
- **about 55% to CR-1 continuous one-market runtime**, with roughly ±10 percentage
  points of uncertainty.

This is a weighted engineering estimate, not `closed / total beads`. Most of the
runtime and lifecycle implementation exists, but the remaining work contains
disproportionately important release evidence: the structural write guard,
production alerts, launchd rung propagation, gate-failure cancellation,
deterministic gate repair, scenario exchange, composed invariants, read-only
qualification, and all live demonstrations. The prior 60–65%
continuous-runtime estimate counted the eleven old build steps too evenly and
did not price those missing gates heavily enough.

## 9. Deliberately after continuous runtime

Tape-driven simulator · inferred fill allocator · automated selection and
reselection · multi-market scheduling · eleven-table telemetry · full payout
calibration · fill import into `rig.db` copies · reducer markout research · live
DNS outage manufacture · 72-hour soak · depth-depletion requote research ·
markets three through six.
