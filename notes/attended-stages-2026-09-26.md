# Attended event stages — 2026-09-26 revision

This is the current operational stage design for the one-market path. The former
four-to-six-hour q01 window and `S=1`/`$2` first writer are historical plans,
not current thresholds. Existing attempt bundles and audit reports retain their
original meaning. This revision changes no safety assertion and grants no live
authority. The primary agent can inspect code and evidence but cannot execute
trades; the operator performs and attends every live stage.

## 1. Read-only operational qualification (q01)

The operator chooses a current market, exact binary and config, dedicated
account, and a real-feed window. Keep the write guard unarmed. Run until the
required events have been observed, for at least **20 minutes of linked active
time**, with a **45-minute stop target**. Going above the target is a recorded
deviation, not an automatic local failure. The shorter window is an event-test
design choice, not evidence that 20 minutes is empirically equivalent to the
former four-hour observation. It does not prove a long soak or economic value.

Preserve the finalized bundle showing zero network non-GET requests, every
transport request counted above the guard, real read-only would-write decisions,
a forced process restart with resumed linked segment, a genuine websocket
disconnect and recovery/resnapshot, heartbeat and dead-man check-ins, at least
99% distinct fresh one-second monitor slots and five-second complete portfolio
slots, no stale/incomplete portfolio walk, no lost anomalies, and no unexpected
SEV1. Deterministic owner-stall and schedule/clock-jump scenarios remain code
evidence; do not change the host clock for this test. Separately retain a
provider-side receipt that a deliberately missed dead-man check-in fired.
`AssessQ01Local` checks only the local bundle and cannot certify account,
provider, or operator provenance. Review the result and external receipts
together before the next stage.

## 2. First attended writer: balance-derived sizing

After code and read-only gates, the operator verifies the selected-shard
spendable balance, complete positions and open orders, current market and
credentials, dedicated account, fixed `capital_max`, reserve, and H-CAP-8
fundability. Derive the permitted one-market size from that current balance
and configured risk bounds, with **`S <= 12`**. Do not assume `S=1` or a `$2`
cap; record the arithmetic and the selected value before arming both write
keys. The operator stays present. The first post-adoption directional entry
from an acknowledgment, newly owned fill, or complete position observation
triggers the durable first-fill stop: cancel and
verify adding orders, attribute the fill, reduce safely to flat, verify no
open orders or positions, and prove restart does not resume adding. Preserve
order, fill, fee/taker, reduction, account, and clean-exit evidence. A run that
does not get a fill is inconclusive for the fill path.

## 3. Attended repeated-cycle candidate

Review first-writer findings and close any reachable safety defect. At the
same account-derived, fundable one-market size (`S <= 12`), exercise an
attended add → owned fill → reduce to flat → restart cycle with clean account
truth. To support continuous participation, also observe live fill handling,
reduction, restart adoption, and cleanup around market/program turnover. A
single fill or short q01 event test does not establish repeated-cycle health.

## 4. Continuous participation across turnover (CR-2)

The target is continuous participation through market and program turnover,
not merely an unattended process bound to one ticker. Before unattended
operation, require reviewed attended evidence, current account and funding
checks, durable stops, selection/rotation and cleanup behavior, supervision,
and working external alarm routes. Demonstrate a missed dead-man alarm at the
real provider and an SEV1 notification received and acknowledged by the
primary operator; demonstrate backup receipt/acknowledgment and escalation
when the primary does not acknowledge. Record timestamps and provider receipts.
An unacknowledged or unproven route blocks unattended promotion. Local tests,
assessor output, and issue closure are never release authority.
