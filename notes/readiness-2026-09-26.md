> Process audit addendum: [verification-workflow.md](verification-workflow.md)
> supersedes this handoff’s per-bead advancement/cadence rules. The receipts
> below remain historical evidence. No release or live qualification is added,
> and this handoff does not authorize resuming its next trading repair.

# Existing-client readiness assessment — 2026-09-26

The client is **not qualified for a live canary or unattended trading**. This
session repaired cancellation/reconciliation hazards and strengthened their
regression evidence. It did not place orders, read account credentials, run an
authenticated exchange probe, provision a trading configuration, or promote a
release. No new strategy or profitability claim is part of this assessment.

This supplements the [revival assessment](revival-2026-09-26.md), which remains
unchanged as dated evidence. Starting HEAD was
`f7e9571547bb948418b7a27adc9c30ae38314b00`, on
`harness/lip-6w5-v2-checkpoint`, with substantial existing uncommitted work.
The [starting manifest](readiness-evidence-2026-09-26/start-manifest.json)
fingerprints that work. Beads holds task status and dependencies; this document
records findings and evidence, not a replacement task tracker. No commit, push,
remote sync, frozen-code edit, or collector-database edit was performed.

## What was repaired

**Cancel-confirm-replace and possible fills (`lip-732`).** The initial tree
released a replacement while its old reducer remained in the local portfolio,
so sizing rejected the replacement with `WRITE_NOT_BUILDABLE`. An initial
candidate that immediately discounted the canceled order fixed that symptom
but failed independent review: a reducer can fill just before DELETE, leaving
stale local inventory. Replacing it immediately can trade through flat.

The accepted implementation records the precise IDs proved absent by a complete
sweep, retires only ACKed pending orders named by that cancellation, preserves
unresolved creates' maximum possible exposure, and discards the dependent
placement. It requests reconciliation using the current connection token and
holds placement and drain completion until **all three** complete, applied
fills/orders/positions walks started after the sweep. An older read completing
later cannot release the hold. Fresh truth rederives the order's role and size;
a fresh flat position produces no replacement. Cancellation remains available
during the hold. Newer positive orders evidence supersedes prior absence.

This intentionally rejects the older bead note's unconditional immediate
replacement criterion. Order absence proves neither unchanged inventory nor
absence of fills. A failed fresh walk must keep placement blocked; the existing
five-second poll remains a fallback when an immediate request coalesces.
Missing first-poll truth also cannot authorize a flat-looking drain.

The new controls cover both inventory signs, full and partial fills before
DELETE using the actual cancel/sweep path against a fake exchange, a read
overlapping cancellation, an adding fill hidden by stale flat inventory,
unknown creates, ACKs not yet listed, positive order reappearance, and continued
cancel dispatch. Existing recap/lifecycle fixtures are updated to supply the
fresh truth they require. The reward/P&L fixture holds an actual 0.01 contract
so its unchanged `WINDING_DOWN` assertion tests a non-drained account; its exact
realized loss and reward-independence assertions remain intact.

**Shard-aware cancellation (`lip-ghg`).** Every production cancel/sweep leg,
including retries, now supplies `market_ticker` and omits `exchange_index` to
auto-route to the market's matching engine. The complete verifying walk remains
the only authority for absence. A shard-aware fake leaves the order live after
the first accepted DELETE and verifies correct routing again on the retry.

The [current Cancel Order V2 contract](https://docs.kalshi.com/api-reference/orders/cancel-order-v2)
defaults ID-only cancellation to shard 0. The
[sharding documentation](https://docs.kalshi.com/getting_started/exchange_sharding)
describes migrations after the August corpus. This is documentation plus fake
transport evidence, **not** an authenticated observation of a misrouted order.
The ID-only public helper remains for shard-0 callers; the production sweep no
longer uses it.

The [orders read contract](https://docs.kalshi.com/api-reference/orders/get-orders)
includes all shards when `exchange_index` is omitted, supporting the existing
verifying query shape. The
[positions contract](https://docs.kalshi.com/api-reference/portfolio/get-positions)
also includes all shards, but defaults to **primary subaccount 0**. Thus a
primary-account flat result alone cannot prove every subaccount is flat. The
dedicated-account preflight must prove no non-primary exposure or supply complete
scoped reads before describing its result as account-wide.

**Regression evidence (`lip-y3q` and related repairs).** Permanent semantic
mutations reintroduce the gateStopped-only reducer cap, doubled gate-failure
debounce, target-zero reducer cancellation, missing shard routing, and the new
reconciliation/accounting defects. Each declares a named catcher and deployed
reachability. The portfolio-read seal continues forbidding public construction
or mutation; its new endpoint-start accessor returns a diagnostic value.

## Priority and dependency assessment

The revival's build/test successes remain useful history, but neither they nor
today's public incentive observations qualify this changed candidate. Historical
completion percentages and old catalogue counts are not current evidence.

| Order | Remaining requirement and consequence | Evidence / Beads |
|---|---|---|
| 1 | Safe transport under throttling and a hung request. A single worker gives reducers no reserved transport; ordinary 4xx handling includes 429 and can erase ambiguous create exposure. | `runtime.go:dispatchWorkers`, `rest/write.go` 4xx branch; `lip-q6r`, required by pilot-plan §6.1. |
| 1 | Feed recovery must restore usable book evidence and reducer service. REST depth comparison and immediate resubscription remain absent; `RESNAPSHOT_DEFERRED` is emitted instead. | `run.go:resnapshot`, `wsx/gate.go` F5; `lip-357`. |
| 1 | Current exchange compatibility and selected-shard collateral. Aggregate cash cannot establish available funds for the market's matching engine. | `rest/read.go:Balance`; new `lip-wif`, refreshed `lip-1m5`; `lip-ghg` repair still awaits full promotion evidence. |
| 2 | Finish production wiring and composed fault evidence: live foreign orders, host-sleep recovery, all F1–F21/§12 consumers, scenario exchange/eight scenarios/A1–A14. Component tests alone do not prove these relays. | `lip-8qv`, `lip-b7r`, `lip-b1r`, `lip-34l`, `lip-dgg`; pilot-plan §6.1. |
| 2 | Qualify the exact candidate and configuration through the full release gate. No survivor, wrong catcher, noncompiling replacement, unexplained flap, or unexplained inert result is acceptable. | `lip-8hn.1`, `lip-bca`, `loop/gates.sh`; new fixes do not close this gate. |
| 3 | Real read-only qualification and external operational proof, only after code prerequisites. | `lip-q01`, pilot-plan §6.2; no such run was attempted here. |
| 4 | Later attended one-market first-fill canary, only after all entry evidence. | `lip-dwf`, pilot-plan §6.3; no live authorization advanced here. |
| Later CR-1 | Repeated-cycle program membership, stuck-inventory escalation, remaining continuous-runtime relays, and attended S=12 burn-in. | `lip-2v0`, `lip-2da`, pilot-plan §6.4. `ProgramEnded` is still literal false. |

The selected-shard issue is newly explicit: the
[Get Balance contract](https://docs.kalshi.com/api-reference/portfolio/get-balance)
returns an aggregate when unscoped, whereas collateral checks are local to an
exchange index. `lip-wif` requires current read-only conformance, authoritative
market `exchange_index`, a composed wrong-shard-funds counterexample, and strict
scoped funding or a rigorously proven supported-market restriction. Simply
selecting a shard-0 market does not prove that aggregate cash resides on shard 0.
The $2 bound must remain unchanged. Both shard issues are R1 dependencies.

Already present and preserved: the two-key transport guard, alert/dead-man
composition, rung-preserving launchd configuration, startup fundability call,
inventory/P&L stop consumers, canary first-fill latch, and revival liquidity
filter/public inspector. Their presence is not a claim that the remaining
production wiring or external evidence gates have passed.

## Verification and limits

Final receipts are recorded in
[verification.json](readiness-evidence-2026-09-26/verification.json), including
source fingerprints, exact commands, results, intermediate failures, and partial
mutation reports. All exchange behavior in tests is simulated; loopback is
permitted for fake servers. The primary module/vet/gate runs and mutation batch
A clear the inherited environment and disable dependency-network lookup.
Mutation batches B and C inherited unrelated environment variables. B explicitly
disabled dependency-network lookup; C used the default Go environment/cache.
Their receipts record those limitations rather than claiming credential
isolation. No account request or credential access was part of those local
fake-exchange suites.
The canonical historical negative-control report is
preserved; subset runs write separate reports.

| Completed check | Result |
|---|---|
| `go build ./...` with CGO disabled; `go vet ./...` | Passed. |
| `go test -count=1 ./...` | Passed across the full Go module. |
| `CGO_ENABLED=0 go test -race -count=1 ./harness/... ./cmd/harness/...` | Passed in the unchanged standalone rerun. |
| Formatting and `scripts/check.py` | Passed; frozen artifacts and read-only Go trees remain intact. |
| Catalogue unit tests | 9 passed. |
| Full mutation compilation | 300/300 replacements compiled. |
| Affected mutation outcomes | 22/22 caught by their declared tests; three green baselines; no reported flaps. |
| Final standalone `loop/gates.sh --quick` | **GREEN**, `ADVANCE_ELIGIBLE: NO`; every step passed, full outcome catalogue explicitly skipped. |

The first quick gate was **RED**: while three mutation jobs ran concurrently,
`TestAFullAnomalyBufferProducesADropReportRatherThanSilence` reached its existing
20-second store-drain deadline with eight records pending. There was no
data-race or write-failure diagnostic. After those jobs ended, the unchanged
race step passed. No assertion or deadline was relaxed. This is consistent
with verification-load sensitivity, not proof of universal determinism; the
first failure and source analysis remain in the evidence and `lip-bca` notes.
The entire standalone quick gate subsequently completed GREEN on the same
frozen source, including a second 300/300 compilation preflight. No verification
job remains running at the pause checkpoint.

The release distinction is strict: a quick gate includes every mutation's
compilation but **skips full catalogue outcomes** and cannot advance a bead.
Affected-mutation outcomes do not substitute for the cumulative full gate on
the exact nominated release candidate. No final release/pilot promotion is
claimed. Operational evidence cannot be synthesized from these code checks.
The R1 bead explicitly permits baseline, anchor, catcher, and affected-mutation
checks while implementing a coherent repair; its final promotion review follows
q01. The complete catalogue is also required before starting that real
qualification run. It is deferred here until the outstanding candidate changes
are complete, rather than credited from the earlier tree or a subset report.

## Evidence required before the later attended test

1. Complete the applicable code blockers above, refresh read-only API contracts
   for the selected market and account, nominate exact code/config/rung hashes,
   and run `loop/gates.sh` without `--quick` on that unchanged candidate. Review
   the entire outcome catalogue, including flaps. Subsequent changes invalidate
   candidate-specific release evidence.
2. Pass the real 4–6-hour read-only run with neither live invocation nor sentinel
   armed. Retain below-guard transport counts proving zero network non-GETs,
   above-guard attempts, at least 99% monitor availability, complete fresh
   portfolio walks, durable restart linkage, bounded reservations/evidence, and
   forced disconnect/resnapshot, restart, owner-stall, and clock/schedule-jump
   evidence. Retain actual alert delivery and a fired external missed-heartbeat
   alarm. The local q01 assessor explicitly reports `local_evidence_only`; it
   cannot supply account/feed/launchd/caffeinate or external-provider provenance.
3. Confirm account eligibility, current program membership and fees, market
   trading/closure state, account-wide starting orders/positions, and spendable
   selected-shard collateral. Use the existing dedicated one-market canary:
   `rung=canary`, `s=1`, `s_max=1`, `n_markets=1`, `capital_max=2`,
   `inv_soft=0.1`, `inv_hard=0.25`, `inv_kill=0.5`, `pnl_kill=-1`.
   Provision and hash its actual absolute paths, stop/latch files, and alarm
   destinations; the example's placeholder ticker is not a runnable approval.
4. Attend the entire test. Capture create visibility and an owned maker fill
   (`is_taker=false`, `fee_cost=0` where required), durable first-entry
   `WINDING_DOWN`, verified adding cancellation, an aggregate-capped maker
   reduction to flat, restart adoption, and no re-adding after flat or restart.
   A run with no fill does not prove this sequence.
5. Plan the end before arming: a bounded attended window, a stop request that
   durably disables adding, continued cancel/reducer service, and an explicit
   operator escalation/cleanup procedure if maker reduction cannot finish.
   Do not remove the write sentinel while cancels/reduction are still needed:
   the guard blocks cancels too. Finish with fresh **account-wide**, complete
   orders and positions walks showing no resting orders and no positions,
   reconciled fills, and a durable drained state. If flatness cannot be proved,
   remain attended and retain the incident/cleanup record; a timeout is not a
   successful drain or permission to leave residual exposure unattended.

## Pause handoff

Paused at the user's request after the current verification run. Repairs remain
uncommitted, with `lip-732`, `lip-y3q`, and `lip-ghg` in progress because none has
the full cumulative release outcome gate. `lip-wif` and the refreshed conformance
notes describe the additional funding/read-scope qualification work. Start a
resumed session with this assessment, `verification.json`, the current Beads
dependencies, and the actual working tree; do not assume either dated assessment
is a current release approval.

Resume with the transport/429/reserved-P1 work (`lip-q6r`), then the book recovery
path (`lip-357`), and selected-shard funding/current API conformance. Keep those
as separately reviewed changes, with compiling mutations and composed evidence.
The full candidate still needs the remaining §6.1 wiring/scenario requirements,
the complete outcome catalogue, q01, and final R1 review before an attended test.

Practical gotchas:

- This was already a dirty tree. Session-only diffs and exact before-image
  hashes distinguish the repair from the revival and earlier gate-failure work.
  The conductor historically reverted accumulated work when its allowed paths
  were incomplete; review its scope before any future invocation.
- An absent order may have filled. Keep the endpoint **start-time** condition,
  pending-UNKNOWN exposure, and no-false-drain controls. Do not restore immediate
  replacement merely to recover the old queue-stage expectation.
- `loop/gates.sh` overwrites `loop/gates-out`; copy receipts before rerunning.
  The mutation runner changes subset report names to `.partial.md`. A build-only
  run is compilation evidence, never a caught-mutation result.
- Avoid overlapping heavy full-suite mutation runs with the race gate. Preserve
  any timeout/flap and investigate it; do not extend timeouts just for a green
  result. B/C's inherited-environment limitation is recorded separately from the
  primary cleared-environment checks.
- Fake-server tests need loopback permission in this sandbox. Use `env -i` plus
  an explicit PATH, GOMODCACHE, GOCACHE, `GOPROXY=off`, `GOSUMDB=off`, and
  `GOTOOLCHAIN=local` for controlled offline checks. A bind denial is not a
  product regression, and a test retry is not external operational evidence.
- The example configuration still has a placeholder ticker. No account, ticker,
  funding allocation, live sentinel, or canary launch has been approved here.
