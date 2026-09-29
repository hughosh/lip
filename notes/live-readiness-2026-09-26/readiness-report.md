# Live readiness evidence — 2026-09-26

Continuous trading was **not started**. No live order, cancellation, reduction,
fund transfer, or other financial write was issued. All harness runs were
structurally read-only: no `-live` flag, no `live_ok` sentinel, and preserved
transport counters. The user has authorized live trading; missing authorization
is not a blocker. The assistant cannot execute financial trades or start an
autonomous trader. An operator must perform the remaining financial stages.
Existing work and collector data were preserved; no commit, push, reset, or
collector operation was performed.

## What changed

The earlier implementation used configured `capital_max`; aggregate balance
polling was telemetry. The new explicit `capital_source=selected_shard_balance`
mode authenticates the chosen ticker's `exchange_index`, queries primary
subaccount 0 on that shard, and derives the run cap from exact available cash.
An omitted `capital_max` adds no implicit $100 ceiling; an explicitly supplied
value remains an entry ceiling. Legacy configured mode remains available.

Startup requires complete account-wide resting orders and positions from every
enumerated subaccount. Unsupported non-selected exposure refuses startup.
Inherited selected-market exposure selects recovery-only behavior: no new adds,
conservative commitments retained, cancellation and funded reduction preserved.
Periodic scoped cash is an additional placement guard; it cannot raise the
frozen capital cap or enter trading P&L. Missing/stale cash stops additions.
Possibly-live orders are conservatively reserved against cash, even if the
exchange already deducted them. This can undersize. It does not solve mixed
shard allocation or CR-2 management.

The new `sizing` rung allows S<=12 while retaining the durable first-owned-fill
stop. The historical one-contract canary remains usable, but is no longer the
required first writer. The revised q01 event test has a 20-minute minimum and
45-minute stop target, with unchanged event, completeness, cadence, zero-write,
and stop assertions. This is a test-design choice, not proof of equivalence to
the old multi-hour soak. See `../attended-stages-2026-09-26.md`.

Observed read-only startup exposed a genuine snapshot protocol defect: an empty
subscription-ID list was sent, and later recovery could include the trade
subscription. The repair waits for the automatic initial snapshot, tracks the
current orderbook subscription, emits exactly one valid ID, and resets identity
on reconnect. Quarantine and fresh reconciliation requirements remain.
Official contracts: [connection protocol](https://docs.kalshi.com/websockets/websocket-connection)
and [orderbook snapshots](https://docs.kalshi.com/websockets/orderbook-updates).

## Authenticated limits and account scope

The diagnostic nominee is `KXTOKENUSE-26SEP28-T146`, primary subaccount 0,
exchange index 0. It is not an approved continuous trading candidate.
The initial complete account check ended 22:45:50Z; observer startup re-read
funding at 23:09:37.860237Z. Both observed:

| Control | Effective value |
|---|---:|
| Selected-shard available cash and frozen run cap | $98.580200 |
| Explicit entry ceiling | none |
| Reserve | 25% = $24.645050 |
| Adding capital ceiling before other bounds | $73.935150 |
| S / S_max | 12 / 48 contracts |
| Inventory soft / hard / kill | 3 / 7 / 18 contracts |
| Trading P&L loss-stop floor | -$15 |
| Managed market count | 1 |
| Scoped cash / complete positions cadence | 5 seconds |
| Maximum cash/truth age | 60 seconds |
| Diagnostic heartbeat cadence | 30 seconds |

These are observed/composed limits, not values inferred from the user's approximate
$100 balance. They are stop/placement rules, not a guarantee that realized losses
cannot exceed a threshold. The run ledger contains `CapitalMax=98580200` in
microdollars and the full effective parameter JSON. `FUNDING_EXACT` preserves
subcent cash/reserve precision; `FUNDING_LIMITS` records scope and risk settings.

The complete preflight found one enumerated subaccount and no resting orders or
nonzero positions. Historical zero-valued market/event position records were
present; they are not open inventory. Failed initial DNS/enumeration probes were
retained and do not support flatness claims. Later successful complete probes do.
These are point-in-time account snapshots; a future writer needs fresh reads.

## Observed stages (UTC)

- 22:36–22:45: authenticated REST conformance and full selected-account scope.
  Production decoders accepted the relevant orders, positions, and 15 retained
  private fills. Active liquidity-program paging completed. Unfiltered historical
  volume-program refusals are retained separately; they are not active-program
  decoder failures.
- 22:47: visible authenticated Portfolio Rewards popover inspected.
- 22:55:55: primary ntfy test `LIP20260926A` accepted; operator acknowledged
  receipt on primary.
- 23:05:37–23:09:41: configured Healthchecks check-in identified the named
  monitor; temporarily shortened period/grace from 1h/20m to 1m/1m, withheld
  check-ins, observed DOWN at 23:07:51, and obtained human email receipt and
  confirmation of an independent backup route. Original schedule restored at
  23:08:48; check-in/recovery observed UP. No monitor secrets were retained.
- 23:09:35.930–23:13:06.443: first real read-only harness stage, 3m28.115 active.
  64,724 would-write observations; zero above/below-guard non-GET requests.
  208/209 fresh monitor slots and 42/42 complete fresh portfolio slots. The
  selected would-write orders were 12 YES at 14c and 12 NO at 55c. They were
  **not sent**. Two quiet-book REST comparisons agreed. Normal SIGTERM committed
  the stop and reached DRAINED. Fresh complete account flatness followed at
  23:13:10. The snapshot error led to repair before further qualification.
- 23:21:48–23:23:08: repaired binary reopened the original halted ledger with the
  latch retained. It stayed WINDING_DOWN/DRAINED with no would-write decisions.
  A subsequent SIGTERM exited normally; complete account flatness was confirmed
  at 23:23:16.
- 23:22:34–23:22:35: production WebSocket supervisor/session accepted two explicit
  single-ID snapshot requests, one on each side of a deliberate socket close and
  reconnect. No error frame occurred. This probes the data protocol, not the
  entire owner/portfolio process, so it does not fulfill q01 by itself.

The first assessment failed three q01 requirements: less than 20 minutes of
active time, no linked unclean/restarted segment, and no complete-process
disconnect/recovery cycle. It is diagnostic evidence, not q01 completion.

## Fees, exits, rewards, and economic uncertainty

Private retained history has 15 fills, eight taker fills, and $1.929800 total
fees. Those are historical account fills, not trades from this work.

At 23:12:02Z the selected book's best YES bid was 14c for one contract, and best
NO bid was 55c for one. Walking visible bids for a hypothetical 12-contract exit
would yield $1.28 gross for YES or $6.06 gross for NO before fees. This records
available book depth at that instant; no execution is guaranteed and no exit
was traded. The production reducer's actual executable price, fill, fees and
reconciliation remain to be observed in an operator-run stage.

The market badge popover showed a **$200 shared liquidity pool**, target 1,000
shares, discount factor 0.5 and Sep 21–27 period. It did not expose a personal
liquidity earnings estimate or estimate timestamp in the inspected UI. The
Portfolio Rewards popover showed September $0 and lifetime $7.65, with a July31
liquidity incentive entry for `KXGENERICBALLOTVOTEHUB-26JUL31`. This is a displayed
historical credit, not an independently corroborated payment-ledger transaction.
No undocumented reward route was assumed. The temporary UI display-mode change
was restored. No positive expected value is established by these observations.

`min_ts` read probes matched inclusive full-walk results at nine boundaries on
this small retained history. Production keeps exhaustive fill walks; this does
not prove long-history pagination/optimization completeness.

## Remaining promotion conditions

The first financial stage still needs real sizing/placement, owned fills,
actual fees, cancellation, quantity-capped reduction, authoritative
reconciliation, restart and complete account-wide clean exit. A no-fill stage
cannot establish fill handling. If flatness is unavailable or nonzero, the
operator must remain attended with cancellation/reduction available.

The Healthchecks drill proves missed-check-in detection, delivery and recovery,
with primary and independent backup receipts acknowledged by the operator.
It does not prove an acknowledgment-aware SEV1 escalation timer, full launchd
supervision/recovery, store-loss recovery under real exposure, or long-tail
network/host failures. No background ping source is installed by this work.

The user selected CR-2 continuous participation across turnover. The current
owner is statically assigned one ticker. It reduces and idles when its active
program disappears; it can resume that same ticker if membership returns, but
does not select another market. There is no CR-2 configuration switch. Safe
rotation requires a durable managed-market set retaining old owned exposures,
books and schedules until flat, shard funding allocation and aggregate risk,
restart reconstruction, foreign-order separation, and observed turnover cleanup.
A 24/7 single-market process does not supply that behavior. `lip-1in` is now
implementation work with the scope decision resolved, not a question awaiting
the user's authorization.

No short run establishes multi-hour reliability, incentive competitiveness,
future rewards, paid credit attribution, or positive expected value. Those risks
remain separate from successful code checks and behavior probes.


## Final repaired observation, identity, and checks

The repaired fresh observer ran from **23:23:40.844Z to 23:28:50.430Z**,
including a deliberate SIGKILL at **23:25:55.630Z** and restart at
**23:26:00.338Z** on the same ledger, config, binary, and linked evidence bundle.
The first segment intentionally has no clean end. Final SIGTERM committed its
durable stop and exited normally. The stored halt latches were retained, and
neither observer directory has a `live_ok` sentinel.

The assessor recorded **4m58.008s of linked active time**, one historical
unclean segment followed by the resumed run, **88,123 would-write observations**,
**zero above/below-guard non-GET requests**, **297/300 fresh monitor slots (99%)**,
and **60/61 fresh complete portfolio slots (98.36%)**. No actual portfolio walk
was stale/incomplete, but one expected slot was not covered. Four quiet-book
REST comparisons agreed, and the repaired runs emitted no `WS_ERROR_FRAME`.
The result is still **not q01**: duration, portfolio-slot coverage and the
whole-process disconnect cycle failed. The separate real WebSocket protocol
probe does not erase the composed-process requirement.

The final complete account check spans **23:28:56.281–23:28:58.501Z**:
**no resting orders, no nonzero positions, selected-shard available cash
$98.580200, and the same 15 retained fill IDs as before testing**. There were
no open orders requiring cancellation. Financial writes and new trading fees
from this work are zero. All four observer processes exited; unrelated existing
collector/wake-lock processes were left alone. Continuous operation was not
started. The Healthchecks schedule remains restored to 1h plus 20m grace; no
continuing test heartbeat is scheduled after observer exit.

The exact **diagnostic** nominee is [candidate-sizing-v2.json](candidate-sizing-v2.json)
and [harness-candidate-v2](harness-candidate-v2):

- Binary SHA256: `77b61ff23c763aa0e29f8bb0d5a77e5e5aeca4bfef7bf2dc24d71673dc5747bd`.
- Runtime config hash: `3626f61885fcf831fdc69066a1b99068b0258308d7f548ed1f706c4961706200`.
- VCS base: `f7e9571547bb948418b7a27adc9c30ae38314b00`, with the preserved dirty tree.
  Base commit alone does not identify this candidate.
- Final gate's before/after source fingerprint:
  `a1758a0209e8288836c7930a45f1ac0461974b76e9823f4acc394d4ba84f330e`.
  Subsequent changes only preserved evidence, the report and issue status;
  [candidate-identity.json](candidate-identity.json) retains exact Go-file hashes.
- [Effective run limits](effective-run-limits.json) exports the committed
  parameter rows and exact funding records for all four observer runs.

The final [candidate receipt](../../loop/gates-out/20260926T232311274781Z-bd478d50/receipt.json)
passed static integrity, catalogue anchors, build, vet, formatting, module tests,
race tests and all **16 safety mutations** in **386.420 seconds**. The
[affected mutation evidence](affected-mutations.md) separately caught all five
selected mutants, including wrong-shard startup funding, removal of the S=12
first-fill stop, fundability validation, and q01 duration/external-evidence
assertions. The full mutation catalogue was not run.

Earlier failed receipts were preserved: confidence-trailer and stale catalogue
anchor failures were corrected; a later source-drift receipt resulted from
writing a diagnostic JSON included in the fingerprint, while module and race
tests themselves passed. Subsequent stable candidate receipts passed. An
exploratory worker package run during concurrent edits also reported a shutdown
`os.Exit` panic without a preserved complete trace; it is inconclusive, is not
counted as successful verification, and its cause was not established. The
stable serialized checks do not retroactively explain that failure.

Evidence entry points:

- [Initial complete scoped account check and min_ts probes](account-selected.json)
- [Final complete account check and retained fills](account-final.json)
- [First observer bundle](observer-1-evidence.json) and [failed assessment](observer-1-assessment.json)
- [Repaired observer bundle](observer-v2-evidence.json) and [failed assessment](observer-v2-assessment.json)
- [Retained-latch restart](latched-restart-evidence.json)
- [Live snapshot/reconnect protocol probe](ws-protocol-evidence.json)
- [Alarm drill and human acknowledgments](alarm-drill.json)
- [Reward UI observations](browser-rewards.json) and [visible exit depth](selected-book.json)

## Beads disposition at this checkpoint

`lip-wif` is completed for authenticated scoped funding, balance-derived cap,
complete startup truth and the wrong-shard regression/mutation. Real collateral
behavior with outstanding live orders remains part of the attended financial
stage, not claimed here. Newly discovered `lip-bup` is completed for the snapshot
protocol repair, focused owner/session tests and observed live data-protocol
confirmation. These closures do not close q01 or promote a writer.

`lip-q01`, `lip-dwf`, `lip-8hn.1`, `lip-8hn.2`, `lip-9j3`, `lip-9vc`,
`lip-8hn`, and `lip-yca` remain blocked by their actual missing operational,
financial-stage or economic evidence. The historical S1/$2 prerequisites and
mandatory full-catalogue language in current promotion records were replaced.
`lip-1in` is open with CR-2 scope resolved and implementation outstanding.
`lip-0ns` is open at low priority: small-history conformance is recorded and
exhaustive fills remain safe for the current 15-fill account; optimization is
not a prerequisite for this small-account stage. Current mutable details live
in Beads; this is only the checkpoint disposition.
