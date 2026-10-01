# Harness-spec transport clauses

Source: `/Users/hugh/kek/lip/notes/harness-spec.md` (2,386 lines, read in full). Rows are in document order; check any `lines` cell with `sed -n 'A,Bp'`.

Class test: G = would survive unchanged in a Kalshi transport module serving any strategy. L = only meaningful for the LIP quoting strategy (repo-topology rows are flagged). M = generic mechanism with a LIP-specific trigger, response or threshold; the class cell names which part is which.

Reviewed and excluded as not transport: S1-S6, §2 (I1/I2), H-TOP-5, §5.2, §6.1-§6.5 quoting policy (except H-Q-3, H-Q-9, H-Q-9a), §8.3 monitor, §9 H-CLOSE-1..4, §10.1-§10.3 selection and capital (except H-SEL-2, H-CAP-5, H-CAP-7), F16-F20, H-HALT-1/2/5, §13 ntfy alerting, H-DEP-2/3/4, §15 persistence, V2-FILL, V8.

## Transport clauses

| clause id or § | lines | one-line summary (<= 25 words) | class |
|---|---|---|---|
| S7 | 63 | Latency is not binding: scoring is 0.9999 at 10ms and 0.9962 at 1s, so nothing should be built for speed. | L |
| H-TOP-1 | 107-125 | Harness is a separate process with its own core.Rig and its own websocket connection; two Kalshi connections and two reconstructed books accepted. | L (repo topology: rig/harness split) |
| H-TOP-2 | 127-135 | go/feed is read-only and its P-rules require no read deadline and no ping; harness needs both, so cannot use that package. | L (repo freeze, not strategy) |
| H-TOP-3 | 140-144 | wsx = websocket client with read deadline, ping/pong, resolution fallback; rest = signed REST client reusing feed.Signer.Headers verbatim for any path. | G |
| H-TOP-4 (pump, rest-worker rows) | 171-172 | pump runs the socket read loop, pushing []byte onto frames; rest-worker ×K does outbound HTTP via orderReq/orderResp channels; neither touches state. | G |
| §4 intro | 219-227 | Three non-interchangeable representations: book terms (yes/no bids in cents), wire terms (bid/ask on YES leg, dollar string), position terms (signed, YES-positive). | G |
| H-CO-1 | 229-241 | yes@p cents becomes wire bid p/100; no@p cents becomes wire ask (100−p)/100; NO bid 42c equals YES ask 58c. | G |
| H-CO-2 (encoding) | 243-251 | count and price are fixed-point strings, never JSON numbers: count "%.2f", price "%.4f" of yesCents/100. | G |
| H-CO-2 (endpoints) | 253-263 | Legacy POST /portfolio/orders is HTTP 410; live endpoints POST/DELETE /trade-api/v2/portfolio/events/orders[/{id}]; DELETE returns reduced_by, not an order object. | G |
| H-CO-2 (create response) | 265-277 | Create returns {order_id, client_order_id, fill_count, remaining_count, ts_ms} with no status; 409 order_already_exists; resting-list objects use different keys, so parse each endpoint explicitly. | G |
| H-CO-3 | 279-285 | Orderbook GET returns orderbook_fp with yes_dollars/no_dollars, best level last, so reverse; only core.ParsePriceCents and core.ParseSize may parse prices and sizes. | G |
| H-CO-3a | 287-315 | Resting book prices are integer-cent (0 of 14.8M fractional); wsx and rest must assert it, else SEV2 BOOK_PRICE_GRANULARITY and send the market to REDUCING. | M — G: assert every parsed book price is an exact integer cent in wsx/rest; L: market to REDUCING, LIP-tape evidence |
| H-CO-4 | 317-323 | Sizes are float64, fractional in ~45% of observed rows (20.5% of resting levels); no int count anywhere in the position model. | G |
| H-CO-4a | 325-337 | Quantities are held in, or quantized to, the exchange's fixed-point quantum (contracts×100 integer) before any sign or zero comparison that decides a state transition. | G |
| H-CO-4b | 339-341 | A formatted count must be strictly positive and no greater than the quantized position it derives from; "0.00" is never sent. | M — G: validate count > 0 before dispatch, never send "0.00"; L: upper bound is the position a reducer derives from |
| H-CO-5 | 343-348 | Every order carries self_trade_prevention_type taker_at_cross: cancels the incoming order on self-match, keeps resting maker; never maker, never omitted. | M — G: STP field on every create; L: value taker_at_cross chosen to protect the scoring maker presence |
| H-CO-6 | 350-354 | Never place an order that makes our_yes_price + our_no_price ≥ 100 against our own resting order; check own resting state, not the book. | G |
| §5.1 (STARTING, UNKNOWN_RISK) | 378-395 | STARTING and UNKNOWN_RISK place nothing; UNKNOWN_RISK retries truth reads forever and leaves only after a complete reconciliation; STARTING reads the halt latch first. | M — G: no writes until truth is read, retry forever; L: state names and latch-implied transitions |
| H-Q-3 | 490-492 | post_only: true on every order, no exception, no flag; enforced structurally because harness/rest has no parameter for it. | M — G: post_only is a create-order field; L: hard-wired true with no setter (never-take policy) |
| H-Q-5b | 552-566 | Every cap applies to aggregate RESTING+SENDING+UNKNOWN+unconfirmed-cancel quantity per side, never a single order; reducing sides use cancel-confirm-place. | M — G: order-state aggregate over in-flight, unknown and unconfirmed-cancel orders; L: the caps themselves (abs(q), S_max, capital) and reducer sequencing |
| H-Q-9 | 633-648 | Place-then-cancel on an upward requote only on the adding side; reducing and settling sides cancel-confirm-place, replacement waits for cancel confirmation by response or sweep. | L (requote policy; consumes the generic cancel-confirm primitive) |
| H-Q-9a | 650-653 | On place-then-cancel the cancel becomes eligible only after the replacement ACKs; otherwise the P0 cancel dispatches first and inverts the rule. | M — G: ack-gated dependent write in the queue; L: place-then-cancel requote policy |
| §6.6 (order priority queue) | 659-678 | Single intent queue, classes P0–P4; P0 (cancels, WINDING_DOWN writes) bypasses the token bucket; intents re-evaluated at dequeue; >30s old promoted one class. | M — G: global write budget, dequeue-time re-evaluation, anti-starvation; L: P0–P4 classes keyed to reducing and adding roles |
| H-QUE-1 | 680-684 | A place-then-cancel requote is one ordered intent; its cancel leg becomes eligible only after the placement ACKs, else dispatch order reverses. | M — G: ordered multi-leg intent with ack-gated second leg; L: the requote use case |
| H-QUE-2 | 686-690 | A reducing-side cancel removes the exit, so it is P1 and issued only as the first leg of cancel-confirm-place, never alone. | L |
| H-QUE-3 | 692-697 | P0 bypasses only the local token bucket, not the exchange's; reserve ≥1 worker slot and a write-budget share for P1; coalesce cancels per (market, side). | M — G: local vs exchange limiter, reserved worker slot, cancel coalescing; L: the reserve is for P1 reducers |
| H-ORD-1 | 705-717 | Client order ids are deterministic (lipH-{runID}-{marketIdx:03d}-{side}-{seq:08d}), never random; the lipH- prefix is how startup adoption recognises our orders. | G (literal prefix and fields are config) |
| §7.2 (write protocol) | 719-727 | Write states INTENT→SENDING→ACKED→RESTING; definite 4xx gives REJECTED; timeout, 5xx or connection error gives UNKNOWN. | G |
| H-ORD-2 | 729-750 | Ambiguous create becomes UNKNOWN, never a new-coid order; RECONCILE_NOW resolves via complete walks; absence leaves it UNKNOWN; max live quantity stays reserved. | M — G: UNKNOWN state, same-coid recovery, complete-walk resolution; L: no new orders on that side, SEV2 at unknown_ping_s, risk-model reservation |
| H-ORD-2a | 752-781 | Absence is not evidence: the 'never landed after two negative reads' branch is deleted; single-page reads and eventual consistency cannot prove absence. | G |
| H-ORD-2b (dedupe evidence) | 783-796 | V1.7 live test: Kalshi dedupes client_order_id; a second create with the identical coid returns 409 order_already_exists and exactly one order rests. | G |
| H-ORD-2b (retry rules) | 801-816 | Retry ambiguous creates only with the same coid and byte-identical payload; a 2xx acknowledges the coid; a 409 is positive evidence it exists, so reconcile. | G |
| H-ORD-2b (limits) | 818-826 | Dedupe is observed, not documented: A10, RECONCILE_NOW and a confirming read after a 409 stay; retry_same_coid_max = 3 total transport attempts, then UNKNOWN escalates. | G |
| H-ORD-3 | 833-837 | There is no amend; the primitive is cancel-then-place or place-then-cancel; an amend endpoint would be a follow-on with its own gate. | G |
| §7.4 (cancel) | 841-844 | DELETE can cancel partially; reduced_by below requested means the remainder may have filled; not an error: reconcile immediately and expect the position may have moved. | G |
| H-ORD-4a | 846-868 | Cancels are idempotent and retriable with sweep verification; creates only get same-coid recovery; reduced_by proves only what that DELETE removed, never a position change. | G |
| H-ORD-4 | 870-881 | Cancel-all is verified by re-reading status=resting; unconfirmed orders retry once, then read by id; SEV1 SWEEP_INCOMPLETE only after sweepPageBound (5 s), SEV3 before. | G |
| H-ORD-4b | 883-890 | After a clean sweep, place nothing in any market until a portfolio cycle started after the sweep has applied; cancels continue; the hold is account-wide. | M — G: post-sweep fresh-truth barrier on writes; L: account-wide scope justified by §10.2 capital sums (lip-mgh) |
| H-ORD-4c | 892-905 | An order is retired only by the exchange's own terminal record (listed, or GET by id); 404, error, absence or elapsed time retire nothing. | G |
| H-ORD-5 (steps 1-4) | 909-918 | STARTING places nothing until positions (incl. unselected markets), resting orders, fills back backfill_h (24h) and balance are all read; balance seeds the capital model. | G |
| H-ORD-5 (step 5) | 919-927 | Resting lipH-* orders are adopted at stated price and size; other coids are foreign: not cancelled, SEV2 FOREIGN_ORDER, market excluded; later foreign activity latches WINDING_DOWN. | M — G: classify resting orders by coid prefix, adopt ours, never cancel others; L: market exclusion and WINDING_DOWN latch |
| H-ORD-5 (steps 6-7) | 928-930 | Any market with q ≠ 0 enters at REDUCING, not QUOTING, whatever its size; emit a STARTUP ping summarising adopted positions and orders. | L |
| H-ORD-5 (failure path) | 932-935 | If startup reads fail after startup_retries (3): enter UNKNOWN_RISK, SEV1, retry indefinitely with backoff; never quote against an unknown position, never exit. | M — G: bounded then indefinite backoff retry of truth reads, no placement; L: UNKNOWN_RISK state and SEV1 ping |
| H-ORD-5a | 937-944 | Ignorance is not a halt: failed position reads enter UNKNOWN_RISK, not WINDING_DOWN, which only keeps trying to find out and places nothing. | L |
| H-ORD-5b | 946-960 | Managed set = selected ∪ nonzero-position ∪ own resting/sending/unknown orders, regardless of LIP status or core.Rig universe; harness subscribes to whatever it holds. | M — G: subscribe to every market holding a position or own order; L: selected set, LIP program status, core.Rig universe |
| H-ORD-5c | 962-967 | Before leaving STARTING, adopted lipH- orders invalid under current selection, close, size, price or capital rules are cancelled and swept. | L |
| §7.6 (fills authoritative) | 969-995 | GET /portfolio/fills is the authoritative record of our fills; V1.8: fill carries trade_id (non-null, unique on 15/15), is_taker, fee_cost, order_id, count_fp, fill_id. | G |
| §7.6 (trade stream join) | 996-999 | Public trade websocket stream carries the same trade_id; a rig.db fill row is ours iff its trade_id is in harness.db.our_fill. | M — G: public trade channel carries trade_id; L: rig.db attribution join |
| H-ORD-6 | 1001-1006 | our_fill has PRIMARY KEY (trade_id) written INSERT OR IGNORE; run_id means first-observed, with a separate backfilled flag, and is never a correctness filter. | G |
| H-ORD-7 | 1008-1013 | Harness never writes rig.db; source='ours' marking is done offline, idempotently, by scripts/importfills.py. | L (repo evidence base) |
| H-ORD-9 (ownership ledger) | 1015-1040 | Durable ledger of every coid ever sent classifies fills by order_id; unknown order_id is foreign: not in our_fill, no F14, SEV1 FOREIGN_FILL and global WINDING_DOWN. | M — G: durable coid/order_id ownership ledger, classify fills against it; L: foreign fill triggers global WINDING_DOWN, F15 vs H-SEL-11 |
| H-ORD-9 (dedicated account) | 1042-1046 | Recommended: run on a dedicated account with no manual activity and assert at startup and every poll that no foreign order or fill exists. | G |
| H-ORD-8 | 1048-1053 | Every fill of ours must have is_taker false; one taker fill is SEV1 and global WINDING_DOWN; fee_cost > 0 corroborates. | M — G: read is_taker and fee_cost on every fill; L: any taker fill breaches never-take, so SEV1 and global halt |
| §8.1 | 1059-1064 | Position per market is one signed float64 q, YES-positive; Kalshi nets offsetting holdings, so no separate YES and NO holdings are tracked. | G |
| §8.2 | 1066-1071 | Two derivations: q_local from our own acks and observed fills on every event; q_exch from GET /portfolio/positions every position_poll_s = 5s as truth. | G |
| H-POS-1 | 1073-1076 | Exchange is authoritative: every poll sets q_local := q_exch; q_local exists to react between polls, not to argue with the exchange. | G |
| H-POS-2 | 1078-1087 | Record every poll's (q_local, q_exch, delta, agreed); drift >pos_drift_tol on two polls gives SEV2 and market REDUCING; >pos_drift_hard gives SEV1 and global WINDING_DOWN. | M — G: compare q_local to q_exch every poll and record agreements; L: thresholds and REDUCING / WINDING_DOWN responses |
| H-POS-3 | 1089-1094 | /portfolio/positions returns every market, so do not poll per market; it is O(1) in requests, not pages; same for orders and fills. | G |
| H-PAGE-1 | 1096-1112 | Every list read is a complete cursor walk; state is replaced only after a full walk; a failed walk keeps prior state, marked stale. | G |
| H-PAGE-1 / V1.8a (measured contract) | 1114-1152 | Cursor field and item keys are per endpoint: cursor for positions, orders, fills; next_cursor for /incentive_programs; keyset cursors; orders/fills limit 1…1000; 4,170 active programs. | M — G: portfolio endpoints' cursor, item-key and limit contract, keyset consistency; L: /incentive_programs row and its 4,170-program size |
| H-PAGE-1 / V1.8a (unestablished) | 1154-1159 | Unestablished, must not be assumed: positions paging and limit behaviour, whether its two arrays paginate together, retention horizon, and mid-walk deletions or in-place updates. | G |
| H-PAGE-1a (cursor guards) | 1161-1187 | Portfolio endpoints rewind to page 1 on a bad cursor: use cursors as received, assert forward progress by first-record identity, abandon with SEV2 CURSOR_REWIND. | G |
| H-PAGE-1a (status strings) | 1188-1192 | Unknown filter value is a bug, not a result: status=cancelled returns 0 rows, status=canceled returns 2; every status string sent is pinned in a test. | G |
| H-PAGE-1a (page cap) | 1194-1195 | A page cap is no substitute: it converts a silent infinite loop into a silent truncation, which clause 3 forbids. | G |
| H-POS-4 | 1206-1212 | Resting orders are replaced wholesale each 5s tick from GET /portfolio/orders?status=resting, but only after a complete walk; an incomplete response must not retire an order. | G |
| H-CLOSE-0 | 1256-1268 | schedule_poll_s is 30s, not 300s, to beat the shortest lead; prefer a market-status event stream where one exists; catch-up runs synchronously. | L (lifecycle scheduling; REST poll cadence) |
| H-SEL-2 | 1405 | /incentive_programs must be fetched with status=active; omitting it returns a different, wrong set. | L |
| H-CAP-5 | 1442 | An insufficient_balance reject is a correctness failure, not a market condition: SEV1 and global WINDING_DOWN. | M — G: insufficient_balance is a distinct reject reason; L: treated as a capital-accounting error that halts globally |
| H-CAP-7 | 1444 | Collateral counts positions plus every RESTING, SENDING and UNKNOWN order, reserved atomically before dispatch, so K REST workers cannot approve against the same committed total. | M — G: atomic pre-dispatch reservation across concurrent REST workers; L: LIP collateral model |
| §10.4 | 1514-1519 | Restart from WINDING_DOWN only by operator action (remove the harness.resume.ok sentinel, restart); the harness never self-clears a global halt. | L |
| F1 | 1530 | Half-open socket: ping every 10s, pong within 5s, 60s read deadline as backstop; close, reconnect with backoff 1s doubling to 60s, full resnapshot, RECONCILE_NOW. | G |
| F2 | 1531 | Clean close (1000/1001): reconnect immediately; book is non-actionable until resnapshot and portfolio reconcile complete (H-FAIL-5); no ping. | G |
| F3 | 1532 | Abnormal disconnect (any other close or error): backoff, then reset book state and resnapshot; SEV2 if longer than 60s. | G |
| F4 | 1533 | Disconnect beyond disconnect_reduce_s (60s): affected markets go REDUCING; cancels are still attempted over REST, a separate transport; SEV1. | M — G: REST cancels continue during a websocket outage; L: markets go REDUCING after disconnect_reduce_s |
| F5 | 1534 | Book silent >60s on a healthy socket: REST orderbook cross-check of prices and sizes to Target Size; agree resets clock; disagree quarantines, REDUCING, resubscribes. | M — G: per-market silence triggers REST orderbook cross-check and in-session resubscribe; L: Target Size depth walk, REDUCING, REST book as reducer pricing source |
| F6 | 1535 | macOS DNS wedge: any resolution failure to a previously resolved host falls back to last-known-good IP with SNI preserved; cache floor TTL 1h. | G (host-specific, strategy-agnostic) |
| F7 | 1536 | Host sleep or stall: wall-vs-monotonic clock divergence over 5s is treated as downtime; force resnapshot and RECONCILE_NOW; record the interval in uptime. | M — G: gap detection forces resnapshot and reconcile; L: uptime accounting (revenue metric) |
| F8 | 1537 | HTTP 429: halve the token-bucket rate, exponential retry with jitter, restore the rate after 60s clean; SEV2 if sustained over 60s. | G |
| F9 | 1538 | Definite 4xx reject with parseable reason is recorded; reject rate over 10% across the last 50 orders in one market sends that market to REDUCING. | M — G: classify and record definite 4xx rejects; L: per-market reject-rate threshold sends market to REDUCING |
| F10 | 1539 | insufficient_balance reject: global WINDING_DOWN, SEV1 (H-CAP-5). | M — G: distinct reject reason; L: global WINDING_DOWN |
| F11 | 1540 | post_only would-cross reject means our book view disagrees with the exchange's: force F5's REST cross-check on that market immediately; SEV2 if repeated. | G |
| F12 | 1541 | Ambiguous write (timeout/5xx, no parseable response): §7.2 UNKNOWN protocol, bounded byte-identical same-coid recovery only, never a new coid; SEV2 after 120s unresolved. | G |
| F13 | 1542 | Position drift: tolerance is recorded, sustained drift sends the market to REDUCING, hard drift triggers global WINDING_DOWN. | M — G: q_local vs q_exch comparison; L: REDUCING / WINDING_DOWN responses |
| F14 | 1543 | A taker fill of ours (is_taker or fee_cost > 0), seen on the fill poll, triggers global WINDING_DOWN immediately, SEV1. | M — G: detect is_taker / fee_cost on the fill poll; L: global WINDING_DOWN (never-take policy) |
| F15 | 1544 | Foreign order (coid not lipH-*): exclude that market from selection, do not cancel the order, SEV2. | M — G: detect foreign coid and leave it alone; L: exclude market from selection |
| F21 | 1550 | Clock step: recompute civil-time deadlines (close leads) from the corrected wall clock; heartbeat, retry, alert-suppression, drain, unresolved-fill and stuck intervals stay monotonic. | M — G: retry and elapsed-time intervals on the monotonic clock; L: civil-time close-lead recompute |
| H-FAIL-1 | 1552-1555 | No failure response is exit: the only exits are operator SIGINT/SIGTERM (after draining) and SIGKILL or power loss, recovered by §7.5. | L |
| H-FAIL-1a | 1557-1567 | F4, F5-disagree, F9, silent quarantined books and granularity frames latch a market's REDUCING until process restart; reconnect, resnapshot or agreeing cross-check never clears it. | L |
| H-FAIL-2 | 1569-1573 | REST and websocket are distinct, not independent: cancels, polls and pings survive a feed outage, but DNS, routing, TLS, credentials and exchange are shared. | G |
| H-FAIL-3 | 1575-1581 | 'Off' means exchange-confirmed absent (response or sweep), never cancel-requested; until then the order is live and fillable and fully counted in the risk model. | M — G: cancel confirmed only by response or sweep; L: unconfirmed quantity stays in risk model and every cap |
| H-FAIL-5 | 1583-1601 | After any disconnect, clean or not, the book is quarantined: no placement decision uses it until a fresh snapshot and portfolio reconciliation both complete. | G |
| H-FAIL-6 | 1603-1617 | F5's REST cross-check walks both sides to Target Size comparing price and size at every level, and checks our own resting size appears where believed. | M — G: depth-and-size comparison of websocket book against REST orderbook; L: Target Size depth and own-resting-size check |
| H-FAIL-7 | 1619-1656 | F5 disagree: request in-session resubscription immediately; reducer is priced from the retained REST book, adding stays off; quarantine lifts on fresh snapshot plus reconcile. | M — G: in-session resubscribe and quarantine lifecycle (SEV1 if unlifted past disconnect_reduce_s); L: reducer priced from retained REST book, adding side off |
| H-FAIL-4 | 1658-1667 | Freshness clocks for positions, orders, fills: any past truth_max_age_s (60s) stops all new dispatch and flags unconfirmed cancels SEV1 LIVE_UNCANCELLED; 401/403 is global, immediate. | G |
| H-FAIL-4 (server-side expiry) | 1669-1671 | Where the exchange offers server-side order expiration or cancel-on-disconnect, use it, so total loss of contact bounds exposure without us being alive. | G (conditional; spec does not say Kalshi offers it) |
| §12 (halt table) | 1691-1705 | Every halt trigger keeps the reducing quote live and monitoring full; transport rows: feed wedge (REST-priced reducer), disconnect over 60s (cancels over REST), harness.stop sentinel. | L |
| H-HALT-3 | 1750-1762 | SIGTERM sets WINDING_DOWN and keeps the process alive until every market is flat or closed; drain_timeout_h (12h) escalates alerts, never exits with q ≠ 0. | L |
| H-HALT-4 | 1764-1791 | Global halt is latched on disk (harness.halt) before in-memory state changes; STARTING reads it before any placement; never self-cleared by the harness. | M — G: durable latch consulted before any write; L: WINDING_DOWN/DRAINED semantics and launchd KeepAlive interplay |
| H-DEP-1 | 1851 | CGO_ENABLED=0 makes Go use its pure resolver (reads /etc/resolv.conf, bypassing getaddrinfo); hypothesis, tested live in V4.6, that this avoids the macOS DNS wedge. | G (host-specific, strategy-agnostic) |
| H-DEP-5 | 1855 | One instance only, enforced by a PID lockfile at startup: two harnesses on one account is an unrecoverable position-model conflict. | G |
| H-DEP-6 | 1856 | Credentials by path only (~/.kalshi/kalshi.pem, ~/.kalshi/env); never logged, printed, put in a table or in a ping. | G |
| V1 (header) | 1978-1981 | harness/quote, harness/risk and harness/rest are called clock-free and I/O-free, so V1 is table tests with literals only. | G (see Judgement calls: conflicts with H-TOP-3) |
| V1.1, V1.2 | 1985-1986 | Pin the H-CO-1 transform for all 99 prices and the exact wire payload byte-for-byte against kalshi.py's pinned examples. | G |
| V1.7a | 1992 | Same-coid retry vs a sim that ACKs then drops the response, twice: fails on a duplicate order or a 409 treated as an error. | G |
| V1.8b, V1.8c | 1995-1996 | Cursor-walk guards (rewind detected by first-record identity, walk abandoned, SEV2 CURSOR_REWIND) and endpoint constants: cursor field, item key, limit 1…1000, status strings. | G |
| V2 (simulated exchange) | 2003-2016 | Deterministic fake exchange with REST and websocket surface, injected clock, every §11 failure injectable; trade prints parsed at full wire precision, not via ParsePriceCents. | M — G: fake REST/ws exchange, injected clock, fault injection, full-precision trade-print parsing; L: tape replay through core and the LIP quoting fill model |
| V3 (header) | 2052-2055 | Invariants are checked every tick; in the sim a violation fails the test, in production it emits SEV1 and forces WINDING_DOWN. | L |
| A1 | 2059 | No order is ever sent with post_only != true. | M — as H-Q-3: G field check, L hard-wired never-take |
| A2 | 2060 | No fill of ours ever has is_taker == true or fee_cost > 0. | M — as H-ORD-8: G fill-field check, L never-take breach |
| A3 | 2061 | our_yes_price + our_no_price < 100 for every resting pair of ours. | G |
| A10 | 2068 | No unresolved create is retried under a new coid or changed payload; only bounded byte-identical same-coid recovery is allowed; cancels follow H-ORD-4a. | G |
| A13 | 2071 | No placement decision from a quarantined or stale book, or from portfolio truth older than truth_max_age_s; F5 quarantine prices only the reducer from REST. | M — G: no placement from quarantined/stale book or stale truth; L: reducer priced from retained REST book |
| V4 (transport faults V4.1-V4.3, V4.8, V4.11) | 2098-2100; 2105; 2108 | Transport faults injected in sim: half-open socket (detect within 15s), clean/abnormal close, 429 storm (no order lost or duplicated), accepted-then-timed-out write (same-coid recovery). | G |
| V5 (transport mutations) | 2144-2163 | Transport-bearing mutations M4, M5, M10–M12, M15–M17 and M20–M23 must each be caught by a named test. | M — G: M4, M10–M12, M15–M17, M21–M23; L: M5 (post_only on reducer), M20 (halt latch) |
| H-VER-1 | 2177-2181 | harness/rest refuses any non-GET request unless both --live on the command line and a live_ok file at a configured path hold; ports probebot.py's DryRunViolation. | G |
| V6-SCOPE | 2186-2200 | Structural write refusal makes V4.10, V4.11, V4.13 and V4.17 undrillable in V6; they are sim-only or re-drilled live at minimum size in V7. | L (test plan) |
| V6 (gate) | 2204 | V6 gate: zero non-GET requests attempted, asserted rather than observed. | G |
| V7.1-V7.4 | 2229-2232 | Live minimum-size: orders visible by coid, cancels swept, fills keyed by trade_id with is_taker false, q_before == q_exch compared before the overwrite over 1000+ polls. | G |
| §18 (taker orders) | 2308-2309 | Taker orders of any kind, including emergency ones, are out of scope: there is no such code path and adding one is a spec change. | L |

## Parameters (§16)

Line numbers are the §16 table rows (table header 1920). Defaults are as written, bold markers dropped. §16 claims "Every knob, one place" (1918); the prose-only knobs listed below it contradict that.

| name | default | unit | line | governs |
|---|---|---|---|---|
| `write_rate` | 5 | writes/s global | 1936 | local token-bucket rate (§6.6; F8 halves it) |
| `write_burst` | 10 | writes | 1937 | token-bucket burst |
| `max_queue_age` | 30 | s | 1935 | promote a queued intent one class (§6.6, text at 675) |
| `requote_interval_s` | 5.0 | s/side/market | 1933 | per-side requote spacing; §16 cites §6.6, which never names it |
| `position_poll_s` | 5 | s | 1938 | positions and resting-orders REST poll (H-POS-1, H-POS-4) |
| `balance_poll_s` | 60 | s | 1939 | `GET /portfolio/balance` poll (§15, 1874) |
| `schedule_poll_s` | 30 | s | 1940 | market and program schedule poll (H-CLOSE-0) |
| `truth_max_age_s` | 60 | s | 1943 | positions/orders/fills freshness bound (H-FAIL-4) |
| `pos_drift_tol` | 0.01 | contracts | 1947 | q_local vs q_exch tolerance (H-POS-2) |
| `pos_drift_hard` | 5 | contracts | 1948 | hard drift trigger; deliberately not rescaled and flagged for review (1502-1506) |
| `ping_interval_s` (ws) | 10 | s | 1949 | websocket ping (F1) |
| `pong_timeout_s` | 5 | s | 1950 | pong deadline (F1) |
| `read_deadline_s` | 60 | s | 1951 | websocket read-deadline backstop (F1) |
| `quiet_s` | 60 | s | 1952 | per-market book silence before REST cross-check (F5; F5 itself writes the literal 60s, 1534) |
| `disconnect_reduce_s` | 60 | s | 1953 | websocket outage before REDUCING (F4); also quarantine escalation (H-FAIL-7, 1648) |
| `unknown_resolve_s` | 10 | s | 1955 | orphaned: its only consumer is the branch H-ORD-2a deleted (756, 766) |
| `retry_same_coid_max` | 3 | attempts | 1956 | same-coid transport attempts (H-ORD-2b, 802, 824) |
| `unknown_ping_s` | 120 | s | 1957 | UNKNOWN unresolved alert (H-ORD-2 item 5, 747; F12) |
| `backfill_h` | 24 | h | 1964 | startup fills backfill (H-ORD-5 step 3, 917) |

Transport parameters stated in prose but absent from §16:
- `startup_retries` = 3 (932); `sweepPageBound` = 5 s (877); sweep retries once, then reads by id (873-874).
- REST worker count `K` has no value (172, 694, 1444).
- F1 reconnect backoff 1s doubling to 60s (1530); F6 cached-resolution floor TTL 1h (1535); F7 divergence threshold 5s (1536).
- F8 halve rate, restore after 60s clean, SEV2 after 60s sustained (1537); F9 reject rate >10% over last 50 orders per market (1538).
- orders/fills `limit` 1…1000 (1149); coid field widths `03d`/`08d` (708); wire formats `%.2f`/`%.4f` (249-250).
- Control files: `live_ok` plus `--live` (2179), `harness.halt` (1767), `harness.resume.ok` (1516), `harness.stop` (1704, only mention); credential paths (1856).

Not specified anywhere in the file (grep-verified): a REST per-request timeout value (the only REST-timeout mentions are the ambiguous-outcome class at 712, 726, 732, 1541); the exchange's own rate-limit figures (only the local bucket at 1936-1937, and `write_rate`/`write_burst` appear nowhere in the prose); websocket channel list, delta sequence-gap handling and auth handshake (only "resnapshot" at 1530-1532, "in-session resubscription" at 1642-1643, "fixed at subscribe time" at 958, which leave these to frozen `feed/` and `core`).

## Judgement calls

- **post_only, STP, A1, A2 are M, not G or L** (490-492, 343-348, 2059-2060). Both are ordinary Kalshi create-order fields, but H-Q-3 hard-wires `post_only` true with no setter in `harness/rest` and H-CO-5 fixes `taker_at_cross` to protect a scoring maker. An extracted module must expose both as parameters and let the strategy pin them; calling them wholly L drops a safety property, wholly G bakes LIP policy into the module.
- **coid scheme is G despite harness-specific literals** (705-717). The `lipH-` prefix, `marketIdx` and `side` are treated as a namespace choice. The prefix-keyed adoption is G, but the foreign-order response (exclude market, WINDING_DOWN latch) is split off as M (919-927, 1015-1040, 1544).
- **Tracker-to-risk coupling rows are M** (729-750, 1575-1581, 552-566, 1444, 883-890). The order state machine (UNKNOWN, unconfirmed-cancel, sweep barrier) is generic, but each clause also says that quantity stays in the risk model and caps, or that the barrier is account-wide because §10.2 budgets sum exposure. Arguably G with the tracker exposing "max possibly-live quantity" and L owning the arithmetic.
- **Detection-versus-response rows are M** (F4, F5, F9, F10, F13-F15, H-POS-2, H-ORD-8, H-CAP-5). The detector (silence, drift, is_taker, reject class) is generic; the response (REDUCING, global WINDING_DOWN) is LIP harness state. Alternative reading: G with the response as a callback.
- **Pagination split** (H-PAGE-1 and H-PAGE-1a G at 1096-1112 and 1161-1195; the measured-contract table M at 1114-1152). The walk contract and forward-progress guard are G, but the endpoint table puts `/incentive_programs` (`next_cursor`, 4,170 programs; also H-SEL-2 at 1405) beside positions, orders and fills, and that endpoint is LIP-universe data.
- **Repo-boundary rows are L though not strategy-specific** (107-135, 1008-1013, and the rig.db half of 996-999): frozen `feed/`, separate `rig.db` and the second websocket connection are rig/harness topology and would be dropped on extraction. Conversely host-specific rows (F6, H-DEP-1: macOS DNS wedge, pure-Go resolver) are G because nothing in them depends on the strategy.
- **Halt latch and sentinels** (H-HALT-4 M at 1764-1791; §10.4 L at 1514-1519). A persisted latch checked before any write is generic, but WINDING_DOWN/DRAINED and KeepAlive semantics are harness. Of the control files only `live_ok` plus `--live` is bound to `harness/rest` (2178-2180); `harness.halt`, `harness.resume.ok` and `harness.stop` feed the state machine, and `harness.stop` has no definition beyond one §12 table row (1704).
- **I/O boundary of `harness/rest` is unspecified** (1978-1981 vs 142-144, 2178, 171-172). V1 calls it clock-free and I/O-free, H-TOP-3 calls it the signed REST client, H-VER-1 puts the write guard in it, and H-TOP-4 puts the HTTP in rest-workers. All `rest` rows are classed G regardless, but where the extractable unit's I/O and clock live cannot be determined from the spec.
