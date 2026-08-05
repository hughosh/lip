The filesystem is enforced read-only. Creating `notes/harness-review.md` was attempted and rejected; no repository file changed. The intended file contents follow.

# Hostile design review — 24/7 LIP quoting harness

## HR-001 — SETTLING lets the exact abandonment bug pass every gate

Severity: CRITICAL

Rules attacked: I1, H-CLOSE-2, A4, V1.10, V5, V6, V7

Concrete failure: add mutation M13: on entry to SETTLING, cancel every order and never place the capped reducer. At 12:00, q=+80 and close_time=16:00. The mutation removes the reducer for four hours.

Every gate can pass:

- A4 expressly accepts “or is in SETTLING.”
- A5 continues receiving rows.
- V1.10 tests deadline calculation, not resulting orders.
- V4 has no close-action fault.
- V5 lacks this mutation.
- V6 only forces the state in a zero-write run.
- V7 need not cross close_lead.

Required rule: before final_lead, SETTLING must require a resting or in-flight reducer whose aggregate quantity is at most abs(q). The A4 exemption begins only after final_lead, trading close, or q=0. Add M13 and a complete close lifecycle test.

## HR-002 — The mandated parser cannot quote the actual touch

Severity: CRITICAL

Rules attacked: H-CO-1, H-CO-3, H-Q-1, H-Q-2, H-Q-6, H-SEL-7, V1.1, V1.2, V1.4, V2

Concrete failure: the raw book has YES bid $0.2570 and NO bid $0.7300. The actual sum is 98.7 cents. core.ParsePriceCents turns 25.7 into 26, so the harness sends $0.2600 and improves the YES touch by 0.3 cents while believing it joined. A $0.2530 touch instead becomes $0.2500, leaving the quote behind.

This is a known repository defect, not speculation: [port-spec.md](/Users/hugh/kek/lip/notes/port-spec.md:364) says integer-cent rounding is faithful reproduction, not endorsement; [codex-rt-rig.md](/Users/hugh/kek/lip/notes/codex-rt-rig.md:43) says it merges fractional levels; [num.go](/Users/hugh/kek/lip/go/core/num.go:27) says fractional cents occur in 8.8% of observed prices.

V1 tests integer cents, and V2 passes the tape through the same lossy oracle, so both bless the defect.

Required rule: live prices use exact fixed point preserving all four wire decimals through book storage, scoring distance, self-cross checks, and order encoding. Frozen core rounding may remain for rig compatibility, but it cannot drive trading. Add tenth- and half-cent no-improve/join tests.

## HR-003 — The reducer creates a permanent sign-flip cycle

Severity: CRITICAL

Rules attacked: §5.2, §6.2, H-Q-5, H-HALT-3, A4, V1.3, M7

Concrete failure: with S=100, q=+61 enters REDUCING and posts 161 NO. A full fill gives q=-100, not zero. The next reducer is 200 YES; a full fill gives q=+100. Full reducers can then alternate q=-100,+100 indefinitely. REDUCING exits only at exact zero, so the specified success case prevents drain.

For any 0<abs(q)<50, filling abs(q)+S also increases absolute inventory to 100. The state table separately contradicts the formula: QUOTING says S/S, while §6.2 demands S+abs(q) on one side for every nonzero q.

Required rule: aggregate potentially fillable reducer quantity in REDUCING is min(abs(q), S_max, funded_quantity). Every positive partial fill must strictly decrease abs(q); a full fill must produce zero. Define any two-sided SKEWED imbalance separately.

## HR-004 — Overlapping replacements defeat every reducer cap

Severity: CRITICAL

Rules attacked: H-Q-9, H-ORD-2, H-CLOSE-2, A4, V1.3

Concrete failure: q=+100 has 200 NO resting at 30. The touch rises to 31. H-Q-9 allows a second 200-NO order before cancelling the first: aggregate size is exactly S_max=400 and notional is only $122. The cancel is delayed or ambiguous; both orders fill, producing q=-300.

SETTLING has the same bug. Two individually capped 100-contract reducers can both fill and flip q=+100 to q=-100 despite the claim that the cap “cannot flip us.”

Required rule: caps apply to aggregate RESTING + SENDING + UNKNOWN + unconfirmed-cancel quantity. REDUCING and SETTLING must use cancel-confirm-place whenever overlap would exceed abs(q). Add an ACK/cancel race test in which both old and replacement orders fill.

## HR-005 — The configured reserve can forbid the only exit

Severity: CRITICAL

Rules attacked: H-CAP-1 through H-CAP-5, §6.2, F10, A4, A7

Concrete failure: defaults make the per-market cap $500/6×2=$166.67 and reserve $125. A 100-contract NO fill leaves q=-100 while YES trades at 89. The specified 200-YES reducer requires $178. It violates the concentration cap even after every other order is cancelled and exceeds the entire reserve.

Six simultaneous one-sided fills are worse: the single $125 reserve is not enough to fund six opposing reducers. WINDING_DOWN does not create collateral.

There is also an in-flight race. With $370 committed, two workers can each approve a $70 order against the same state and land at $510 unless SENDING and UNKNOWN notional is reserved atomically.

Required rule: collateral includes positions and every RESTING, SENDING, and UNKNOWN order. Reserve before dispatch. Configuration must remain reducible after the worst permitted simultaneous fills, or risk-reducing orders must be exempt from concentration after adding orders are cancelled. An unfundable intent does not satisfy A4.

## HR-006 — Two negative reads cannot prove an UNKNOWN create never landed

Severity: CRITICAL

Rules attacked: H-ORD-2, H-POS-3, H-POS-4, F12, V1.7, V4.11, M4

Concrete failure: create C is accepted, fills immediately, and its response times out. During the next ten seconds, 201 newer fills or terminal orders arrive. Both reconciliation attempts miss C on page one and declare it absent. The harness requeues as C2, which also fills, doubling inventory.

Even exhaustive pagination cannot prove absence unless cursors are snapshot-consistent and the endpoints have documented retention and visibility bounds. Eventual consistency beyond ten seconds produces the same failure.

The assertion that the protocol is correct whether Kalshi deduplicates or not is false.

Required rule:

- If client_order_id idempotency is documented and confirmed, retry the same coid.
- Otherwise, reissue only after a linearizable coid lookup proves nonexistence under a documented visibility contract.
- If neither exists, ambiguous creates are permanently non-retriable and require reconciliation/operator resolution.

Tests must place the target beyond page one and delay visibility beyond two polls.

## HR-007 — The monitor can emit fresh lies indefinitely

Severity: CRITICAL

Rules attacked: I1, I2, H-TOP-4, §8.3, A5, F18, H-PING-1

Concrete failure: the owner publishes q=0, then deadlocks. One second later a resting YES order fills. The monitor has no independent exchange observation; it rereads the same pointer, stamps new snap rows, and sends hourly heartbeats reporting q=0.

A5 passes because it checks row time rather than source-snapshot time. F18 does not fire because the process and heartbeat remain alive. Separating goroutines prevents the Python break but does not make observation independent.

The process-death case is also incomplete: ntfy does not generate an alert because an expected heartbeat failed to arrive. Without an external timer, an operator asleep at 02:00 does not “see” the absence.

Required rule: snapshots carry an advancing sequence and monotonic publication time. A5 requires source advancement, not fresh restamping. The monitor needs an independent read-only portfolio/orders observation or must label exchange truth stale. Add an external service that alerts after missed heartbeats. Freeze owner publication while leaving monitor/ping alive; the test must emit SEV1.

## HR-008 — STARTING can enter WINDING_DOWN without knowing what exists

Severity: CRITICAL

Rules attacked: I1, H-TOP-1, H-ORD-5, H-SEL-11, STARTING/WINDING_DOWN, A4

Concrete failure: launchd restarts with q=+70 in a market whose LIP program ended but whose market remains open. Three position requests return 503. The harness enters WINDING_DOWN with q unknown, so it cannot choose a reducing side or size. No continuing startup reconciliation is specified.

If positions succeeds, the inactive ticker need not exist in the fixed core.Rig universe, leaving REDUCING without a book. A prefixed adding order in an unselected q=0 market is also adopted but never explicitly cancelled.

Required rule: managed markets are the union of selected markets, every nonzero position, and every prefixed RESTING/SENDING/UNKNOWN order, irrespective of LIP status. Failed truth reads remain UNKNOWN_RISK and retry indefinitely. Before RUNNING, cancel and sweep every adopted adding order invalid under current selection, close, size, price, and capital rules.

## HR-009 — WINDING_DOWN is neither durable nor non-terminal

Severity: CRITICAL

Rules attacked: I1, H-FAIL-1, H-HALT-3, H-DEP-2, §10.4, V4.17

Concrete failure:

1. A taker fill causes global WINDING_DOWN.
2. An unrelated panic kills the process.
3. launchd restarts it.
4. STARTING never reads the prior halt, so flat markets resume adding quotes.

The global halt self-clears without operator action.

SIGTERM is also contradictory. When flat, the process exits and KeepAlive immediately restarts it. When not flat, drain_timeout_h permits exit after 12 hours with inventory open, contradicting I1 and H-FAIL-1; during shutdown, launchd may not restart it.

Required rule: persist a global halt latch before changing state, with a non-SQLite fallback. STARTING checks it before any placement and never self-clears it. A planned exit restarts only into latched DRAINED or disables KeepAlive. Drain timeout escalates alerts but never exits with q nonzero. Test global halt followed by SIGKILL/restart.

## HR-010 — Shared network or portfolio failure leaves adding orders live

Severity: CRITICAL

Rules attacked: I1, H-FAIL-2, F4, F6, H-POS-1 through H-POS-4, A4, A8

Concrete failure: q=+61 has 100 YES adding and 161 NO reducing. Websocket and REST both fail because of shared DNS, routing, TLS, authentication, or exchange failure. The harness requests cancellation of YES but cannot confirm it. That order remains and fills, taking q to +161. Positions, fills, and resting-order truth are unavailable, while the table claims adding off and monitoring full.

A narrower failure suffices: public market data remains healthy while portfolio endpoints return 503 or 401. No §11 row defines a maximum portfolio-truth age. Resting fills cannot be attributed from public trades alone, so real q can diverge indefinitely.

Required rule: “off” means exchange-confirmed absent, not cancel requested. Track independent freshness for positions, orders, and fills. On bounded expiry, stop all new dispatch and report LIVE_UNCANCELLED orders as unmanaged SEV1. Use short exchange-side expirations or cancel-on-disconnect if available. Authentication failure is global immediately.

## HR-011 — A close-window reducer grants an option to informed takers

Severity: CRITICAL

Rules attacked: S1, §6.2, H-Q-5, H-CLOSE-2, H-CLOSE-4

Concrete failure: q=+100 YES; YES becomes publicly certain while the still-open book remains YES 49 / NO 50. The reducer buys NO at 50, equivalent to selling YES at 50. An informed taker buys the winning YES contracts, causing the harness to surrender $100 of settlement value for $50. If NO were certain, the favorable opposite fill would not arrive.

Maker fee zero does not remove selection on whether the order fills.

S1 studies settlement versus a five-minute midpoint and a taker stop. It does not establish settlement value conditional on a later maker reducer being selected, especially near resolution. Repository analysis already reports adverse drift cancelling touch spread.

Required rule: default close_lead_keep_reducing=false for markets whose outcome can become knowable before exchange close. Retaining reducers requires separate fill-conditioned settlement evidence by time-to-close and early-close status, plus a final information deadline.

## HR-012 — H-Q-4 keeps risking money when reward is zero

Severity: CRITICAL

Rules attacked: H-Q-1, H-Q-4, I1, §8.3

Concrete failure: Target Size is 1,000. The market initially has 1,300 per side and the harness rests 100/side. External participants cancel 900 per side, leaving only 400 including the harness. Every snapshot is excluded and reward is zero. H-Q-4 nevertheless leaves both adding orders live. YES fills for 100, creating directional exposure and adverse-selection loss during an interval that cannot pay.

Required rule: after a gate-failure debounce, cancel and verify all adding orders. If q≠0, retain only an aggregate-capped reducer. Re-enter only from a fresh complete qualifying book.

## HR-013 — List reads are not complete, consistent snapshots

Severity: MAJOR

Rules attacked: H-ORD-4, H-ORD-5, H-ORD-8, H-POS-3, H-POS-4, H-SEL-2, V4.13, V7.1–V7.4

Concrete failure: an orders response returns 200 objects and a cursor; ours is item 201. H-POS-4 replaces the map from page one, declares ours gone, and places another order. A 100-contract order split into more than 200 fractional fills can similarly push a taker fill off page one before polling, defeating H-ORD-8.

The repository has already experienced this class of bug: [probebot.py](/Users/hugh/kek/lip/probebot.py:1100) documents active programs exceeding 1,000 and a limit-200 lookup falsely reporting that a live program was absent. [kalshi.py](/Users/hugh/kek/lip/kalshi.py:160) still exposes one-page portfolio helpers.

Required rule: define cursor exhaustion, ordering, retention, deduplication, and concurrent-insert behavior per endpoint. Replace state only after a complete walk; page-k failure preserves prior state and marks it stale. Test targets on page two and inserts between pages. Paginate the complete active-program universe before ranking.

## HR-014 — The simulator’s fill oracle contradicts the code it cites

Severity: MAJOR

Rules attacked: V2, V4, V5, V8

Concrete failure: 1,000 contracts are ahead of ours at 50. A public print of 100 occurs at 50. The simulator fills our 100; live fills zero because the queue ahead absorbs it. Conversely, a trade through 50 can occur after our order was cancelled microseconds earlier or through self-trade prevention.

[core/rig.go](/Users/hugh/kek/lip/go/core/rig.go:13) explicitly says an at-price print does not prove fill and trade-through is not proof. The proposed at-or-through min(order, print) model is not core’s strict trade-through calculation.

Required rule: simulate the range of legal allocations—zero, partial, full, delayed reporting, cancel race, and multiple fills—not one invented truth. At-touch execution varies with queue position. Strict trade-through supplies only a conditional lower bound under explicit assumptions. Calibrate with private V7 fills.

## HR-015 — F5 blesses a wedged size book as healthy

Severity: MAJOR

Rules attacked: F5, F11, H-Q-4, H-Q-10, V4.5

Concrete failure: websocket shows YES 50×1,300 and NO 49×1,300, Target=1,000. It misses a YES size delta of -1,290. REST shows YES 50×10. Touch prices agree, so F5 resets the clock and continues, although the market no longer qualifies and own-size subtraction is wrong.

V4.5 passes if its injected mismatch changes price, leaving this literal defect untested.

Required rule: compare prices and sizes through the entire Target Size walk on both sides, including expected own-size subtraction. Any mismatch replaces/quarantines the book. Add same-price depth, deep-walk, and own-subtraction fault cases.

## HR-016 — A clean close is treated as proof no event was missed

Severity: MAJOR

Rules attacked: F2, H-Q-2, H-Q-10, V4.2

Concrete failure: before a clean code-1000 close, YES is 50/51. During a 500 ms gap it becomes 40/60. Before a new snapshot, retained state sends a 50 bid. post_only accepts it below the real ask, but the harness has improved the real touch by ten cents and exposed money at a price unsupported by external bids.

P25a is a fidelity rule for cmd/rig, not a safety property for a trader.

Required rule: every disconnect makes books non-actionable until new snapshots and portfolio reconciliation complete. Old state may remain diagnostic only. V4.2 must require quarantine, not retained tradability.

## HR-017 — A five-minute poll cannot enforce a one-minute final lead

Severity: MAJOR

Rules attacked: H-CLOSE-1 through H-CLOSE-3, F21, V1.10, V4.18

Concrete failure: cached close_time is 17:00 at 12:00. At 12:00:01 it moves to 12:03. The next poll occurs at 12:05, after close. Neither close lead nor final cancellation ran. This is a schedule update, not can_close_early.

Required rule: poll materially faster than final_lead or consume market-status events. A newly observed close already inside a lead executes catch-up synchronously, with final cancel taking precedence. Test a close moved inside one polling interval immediately after a poll.

## HR-018 — V7.4 passes a position model that ignores fills

Severity: MAJOR

Rules attacked: H-POS-1, H-POS-2, A6, V7.4, V7.5

Concrete failure: q_local stays zero between polls. Each poll reads q_exch=100, performs q_local:=q_exch, then checks equality. One thousand perfect equalities are recorded although incremental fill handling does not exist. Checking before overwrite still proves nothing if no fills occur during those polls. V7.5 can pass after the next five-second correction.

Required rule: record q_before and compare it with q_exch before overwrite. V7.4 must induce YES, NO, partial, duplicate, and out-of-order fill reports during the sequence and bound reaction latency. Add a mutation that ignores fill updates until position polling; it must fail.

## HR-019 — The priority queue reverses H-Q-9 and can starve reducers

Severity: MAJOR

Rules attacked: H-Q-9, §6.6, F8, A4

Concrete failure: an upward requote creates a P3 placement and P0 cancel. If both enter the queue, P0 dispatches first, converting place-then-cancel into cancel-then-place. K workers also do not preserve exchange arrival or ACK order.

P0 bypasses only the local bucket. A cancel storm can receive 429s, occupy every worker in backoff, and starve P1. Cancelling a reducer does not “only reduce exposure”; it removes the exit.

Required rule: dependent actions are not independent queue entries. The cancel becomes eligible only after replacement ACK. Reserve exchange capacity for reducers, rate-limit/coalesce cancels, and test an unbounded P0/429 storm with a waiting reducer.

## HR-020 — Cancel handling contradicts no-retry and can invent fills

Severity: MAJOR

Rules attacked: H-ORD-2, H-ORD-4, §7.4, F12, M12

Concrete failure: DELETE successfully cancels 100 but its response times out. An eventually consistent sweep still shows the order, so H-ORD-4 retries, violating H-ORD-2. The retry returns 404 or reduced_by=0; comparing that with stale requested size can be misread as a fill despite q never moving.

Required rule: distinguish creates from cancels. Duplicate cancellation may be explicitly retried because it is risk-decreasing, but reduced_by proves only what that particular DELETE removed. Never derive position movement from it. Until reconciliation, include the maximum possibly live quantity in risk.

## HR-021 — The P&L kill has no defined input

Severity: MAJOR

Rules attacked: §12 P&L row, pnl_kill, H-HALT-2, V1, V4, V5

Concrete failure: q=+150 was acquired at 80; mid falls to 20, an unrealized loss of $90. The specification defines no cost basis, realized-P&L calculation, mark source/age, baseline, or handling for payouts and external cash flows. A balance-based implementation may see no loss; a $100 reward can mask it entirely. Both satisfy the stated schema.

Required rule: define P&L algebraically from authoritative fills/cost basis, realized flows, and a specified maximum-age mark. Define scope and treatment of rewards, deposits, and foreign activity. Add exact threshold tests and unavailable-mark behavior. Otherwise remove the trigger.

## HR-022 — The live dry run requires writes it structurally forbids

Severity: MAJOR

Rules attacked: H-VER-1, V4.8–V4.17, V6, V8

Concrete failure: V6 requires zero non-GET attempts while requiring every V4 fault live. V4.11 needs an accepted placement followed by lost response; V4.10 needs a real balance reject; V4.13 needs our taker fill; V4.17 needs real open inventory. None can occur when the client refuses all writes.

Required rule: classify tests as simulator/proxy-only, live-read-only, or live-minimum-size. Accepted-write, cancel, private-fill, and real-inventory tests belong in V7. V8 must name the artifact proving each injection fired.

## HR-023 — Persistence has multiple writers and impossible failure invariants

Severity: MAJOR

Rules attacked: H-TOP-4, F19, H-HALT-2, A5, A9, H-STORE-1

Concrete failure: the owner “owns hstore,” but monitor writes snap/uptime and ping updates anomalies. Separate SQLite writers can block monitor persistence for more than three seconds. If SQLite fails, F19 says keep trading and monitoring, while A5 cannot write a snap and A9 cannot write the transition caused by the failure. An on-disk journal is not a fallback for disk-full or I/O-stall on the same disk.

Required rule: one dedicated DB writer accepts immutable records. Separate sample-production freshness from persistence freshness. Persistence failure stops adding risk while reduction and in-memory observation continue. Heartbeats explicitly report persistence loss. Test lock stalls and disk-full, not merely returned errors.

## HR-024 — Foreign activity makes ownership and attribution ambiguous

Severity: MAJOR

Rules attacked: H-ORD-5, H-ORD-6, H-ORD-8, F14, F15, H-SEL-11

Concrete failure: after startup, a manual YES order fills 100. q_exch now includes +100 indistinguishable account exposure. Excluding the market does not remove it, while H-SEL-11 says nonzero markets cannot be deselected. A manual taker fill can either enter our_fill and falsely trigger F14/import as ours, or be filtered by unknown order ID and expose gaps in terminal-order ownership.

Required rule: either mandate a dedicated account with continuously verified zero foreign activity, or keep a durable ownership ledger covering terminal orders and classify fills by order_id. New foreign activity latches global WINDING_DOWN; it is not merely a per-market selection exclusion.

## HR-025 — Two reward conclusions are mathematically reversed

Severity: MAJOR

Rules attacked: S3, H-Q-2, §6.2, H-SEL-1

Concrete failure 1: Target=200; external depth is 100@50 and 100@49. Joining 100@50 gives our side share 100/200=50%. Improving to 51 makes our score 100 and the 50-cent external score 50, yielding 66.7%. Improving does create scoring gain.

Concrete failure 2: field score is 100 per side. Normal 100/100 quoting earns (50%+50%)/2=50%. A one-sided 200 reducer earns (0+66.7%)/2=33.3%, one-third less. S3 preserves the quoted side’s weight; it does not make the absent side free.

Required rule: retain no-improve only as an explicit adverse-selection tradeoff. Compute combined share for every skew state using the actual scorer. Delete “no scoring gain” and “costs nothing in reward terms.”

## HR-026 — Exact float zero is not a lifecycle boundary

Severity: MINOR

Rules attacked: H-CO-4, §5.2, H-CLOSE-2, H-SEL-11

Concrete failure: fills of 0.10, 0.20, and -0.30 can leave a binary residue near 5.6e-17. The market is economically flat but q!=0, so it remains REDUCING and SIGTERM does not see drain. SETTLING formats abs(q) as “0.00,” producing rejects until a successful position poll overwrites it.

Required rule: store contract quantities in the exchange’s exact fixed-point quantum, or quantize before sign/zero transitions. Validate that formatted count is positive and no greater than the quantized position.

## Attacks that did not produce findings

- The integer-cent YES/NO wire transform is algebraically correct for exactly representable integer prices. The failure is fractional-price destruction before transformation.
- Equal-size YES and NO maker fills with fixed prices summing below 100 do lock a positive amount under stipulated immediate netting and zero maker fees.
- One single, non-overlapping SETTLING reducer capped at abs(q) cannot flip inventory. The failure requires permitted overlap or the abs(q)+S formula.
- The separate monitor goroutine prevents the exact probebot.py control-flow break. It does not prevent stale-source false monitoring.
- Assuming the exchange honors post_only, I could not produce a taker fill without an API/semantic failure. Detection still depends on complete fill pagination.
- A complete, current, authoritative cancel sweep does establish cancellation. The failures arise from incomplete pagination, ambiguous retry, transport loss, and exposure before the sweep.