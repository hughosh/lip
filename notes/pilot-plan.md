# The pilot plan — qualification ladder to continuous runtime

**Status:** revised 2026-09-26 for balance-derived sizing and staged evidence. This
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

The current binary has the `-live` plus `live_ok` two-key write guard.
Read-only qualification excludes `-live` and counts transported methods.
The selected-shard mode resolves authenticated funds before recording a run;
an unscoped aggregate balance is not an entry budget.

## 1. Capital and finish lines are separate ladders

Safety properties do not weaken with capital. What changes with size is the
external loss bound and the amount of live evidence being collected.

| Rung | Size | Purpose | Is it continuous runtime? |
|---|---:|---|---|
| read-only qualification | no writes | prove live-feed, restart, schedule, monitoring, and alert behaviour | no |
| attended sizing | 1 market, `S <= 12`, authenticated selected-shard capital | exercise actual sizing and order path; first owned fill durably stops adding | **no — it deliberately self-stops** |
| attended pilot burn-in | 1 market, fundable `S <= 12`, same balance-derived envelope | prove add → fill → reduce → restart cycles | no |
| continuous pilot (CR-1) | 1 market, reviewed balance-derived envelope | permit repeated unattended fill cycles during one declared market assignment | **yes, within that assignment** |
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

The user's 2026-09-26 target is **CR-2 continuous participation across
turnover**. That resolves the scope decision in `lip-1in`; it does not establish
implementation or release evidence. Selection, shard funding, managed exposure,
and rotation/restart tests remain required. The historical S=1/$2 `canary` rung
remains usable as an optional diagnostic, but is not the required first stage.

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

Dense `position_poll`, `snap`, and `uptime` tables remain deferred. Every complete
position discrepancy above tolerance is still recorded as an attributable
`POSITION_DISAGREEMENT` journal row (local, exchange and delta quantities). The
`balance_poll` observation table is restored in schema v3 (atomic migration from
the five-table v2 store); its values do not feed trading PNL or capital. The qualification runner must still produce a compact evidence bundle
for its required polls, samples, and intervals; deleting a table does not delete
the fact that must be proved.

### 2.4 Work that stays deferred

Do not make the pilot wait for a tape-driven exchange simulator, inferred fill
allocation, automatic market selection, multi-worker scheduling, eleven-table
telemetry, Go payout/share scoring, DNS-IP fallback, fill import, payout
prediction, reducer markout research, or the 72-hour V6 soak.

Do not delete work that is already built. In particular, the priority queue and
the existing mutation catalogue remain available for targeted checks and optional
deep audits; the process policy is §5.

## 3. Optional scenario fixture — deterministic private truth

When a composed test needs it, use a small in-process fake with an injected
clock and order ledger,
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
eight composed scenarios must have named behavioral evidence. Add a mutation
when the catcher needs a negative control; permanent mutations for every scenario
are not a separate completion requirement.

## 5. Fast verification

Use [verification-workflow.md](verification-workflow.md). Ordinary edits use
relevant tests; a coherent checkpoint uses `loop/gates.sh`. Before an attended
candidate, use ordinary tests, affected race checks and useful safety mutations
(`--candidate` is the convenient bundled check). All 300 mutations and repeated
full suites are optional deep audits, not per-bead or universal promotion gates.
A completed implementation bead is not permission to trade. Known safety failures
must still be addressed; process ceremony is not a substitute for working code.

## 6. Entry and exit gates

### 6.1 Code evidence for the intended rung

A real read-only experiment requires explicit operator authorization, the composed
two-key guard in item 1, correct current read contracts/complete portfolio walks,
and relevant local tests. It may expose missing observation/recovery behavior;
it does not require finishing unrelated writer features first. An incomplete
walk or lost monitoring is a failed experiment, not an excuse to run for hours.

Before the first live writer, the following safety properties remain required.
Use direct composed tests of the actual client; no separate simulator project or
all-catalogue completion ceremony is required:

1. H-VER-1's two-key write guard is composed at the transport boundary:
   non-GET requests require both an explicit `--live` invocation and a present
   absolute-path `live_ok` sentinel, checked at dispatch time. Without either,
   zero non-GET requests can reach the network.
2. No unexplained flaky result is relied on as safety evidence.
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
8. The eight fault scenarios in §4 have named behavioral evidence exercising
   the production seams relevant to the one-market canary. A separate scenario
   exchange and universal A1–A14 assertion framework are not prerequisites.
9. Required detectors and §12 stops for the intended rung reach their production
   consumers. Component existence alone is insufficient; known reachable safety
   defects block the trial. Later-rung features do not block an earlier safe rung.
10. Relevant code checks pass on the candidate: the normal suite, affected race
    tests, safety sentinels and any affected regression controls. There is no
    all-300-mutation prerequisite. Retain the exact code/config identity.

### 6.2 Read-only qualified

The [2026-09-26 attended event stages](attended-stages-2026-09-26.md) revise
this operational gate. Run against the real feed with the write guard unarmed
until required events are observed, for at least 20 minutes of linked active
time, targeting a stop by 45 minutes. This shorter event test gives no long
soak or economic-value proof. Assert zero
network non-GET attempts rather than inferring it from an empty order list.
During the run force a genuine disconnect/resnapshot and process restart.
Exercise owner-only stall and schedule/clock jump with deterministic injected
code scenarios before promotion; the production binary has no safe external
owner-stall hook, and changing the real host clock is not a qualification drill.
Retain actual external missed-heartbeat alarm evidence separately. Require ≥99% monitor availability outside
explicitly gated intervals, fresh complete portfolio walks, no silent anomaly
loss, and a fired external missed-heartbeat alarm. Preserve the evidence bundle
and provider receipt. The local assessor alone cannot award promotion.

### 6.3 Attended canary passed

With the operator present, verify selected-shard spendable funds and derive a
fundable one-market size `S <= 12` within the fixed account and risk bounds;
record the balance, cap, reserve and sizing arithmetic. Arm both write keys.
The first post-adoption directional entry from an
ack, newly owned fill, or complete position walk must durably latch
`WINDING_DOWN`. Demonstrate create visibility, verified adding-side cancel,
owned-fill attribution, `is_taker=false`, `fee_cost=0`, a capped maker reduction
to flat, restart adoption, and no resumption of adding after flat or restart.
Verify no open orders or positions at exit. A no-fill run leaves the fill path
unqualified. Follow the current attended-stage plan for operator evidence.

### 6.4 Continuous pilot authorised

Before unattended repeated cycles, close the remaining repeated-cycle gaps: live
`inv_kill` (`lip-lqw`), the stuck-inventory escalation (`lip-2da`), algebraic
trading P&L and `pnl_kill` (`lip-gp8`), active-program membership
(`lip-2v0`), and any safety finding produced by the first writer. Then run an
attended, balance-derived `S <= 12` candidate through a directional fill,
reduction to flat, restart, and market/program turnover cleanup.

Continuous runtime begins only after that evidence is reviewed, the halt is
cleared by an operator, the applicable code and regression checks pass, no priority-0 or priority-1
safety bead for this operating envelope remains open, and launchd + caffeinate + external dead-man
supervision are active. The real provider must prove a missed-dead-man alarm,
primary SEV1 receipt and acknowledgment, and backup acknowledgment/escalation
when the primary does not acknowledge. Continuous participation across turnover
also requires proven selection/rotation and cleanup; see the current
[attended stages](attended-stages-2026-09-26.md). A local green result grants
no unattended authority.

## 7. Historical critical path (consult current Beads and receipts)

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

## 8. Historical estimate (not current readiness)

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
