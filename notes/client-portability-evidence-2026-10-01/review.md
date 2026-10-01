# Can the lip Go client serve as a general-purpose low-latency Kalshi trading client?

Review date: 2026-10-01. Tree: `0c65eb6` on `harness/lip-6w5-v2-checkpoint`, clean.
Author: Claude (Fable 5.1) under Hugh's standing non-live authorization; no order was
placed, amended or cancelled while producing this document. Every number below is either
cited to a file and line in this tree, to a sweep output archived in this directory, or to
the read-only probe in `probe/` whose source and raw output are archived alongside.

Scope: the client layers inside `go/` — `feed` (signer), `harness/rest`, `harness/wsx`,
`harness/netx`, `harness/num`, and the layers they lean on (`cfg`, `risk`, `quote`, `core`)
— judged for reuse by a strategy other than LIP market-making. The first candidate strategy
is the tennis serve-schedule maker scalp (memory `tennis-scalping-intent`; §6).

Sections 5, 6, 7, 8 and 9 were produced by delegated sweeps and then verified; the
verification record is §13.

---

## 0. Verdict in one paragraph

Yes for the transport, no for the harness, and the gap is features, not architecture. The
signer, the HTTP-level `Doer` seam, the measured pagination walker, the two-key write guard,
the create/cancel protocol and the supervised websocket are reusable as they stand: a
400-line probe in a separate Go module drove them against the live exchange and measured
~100 ms authenticated REST reads, ~98 ms cancels (recorded over 32), a feed lag of ~50 ms
once the local clock's 39 ms offset is included, and a socket with one connection and no
drops in 120 s (§4, §8). Their coupling to LIP is shallow: a leaf `Side` enum, a four-field
anomaly struct, eight `cfg.Params` fields and the frozen `core` decoder (§2). What is not
reusable is the ~2,000 lines of freshness and ownership machinery that share the same two
packages, and the harness's stated premise that latency is not binding (§5, S7), which puts
fills on a 5 s REST poll (recorded median 1.63 s, max 5.04 s) and hard-wires
`post_only`/GTC/`taker_at_cross` with validators that refuse anything else (§3.3, §7.2).
The tennis maker scalp needs resting quotes with fast cancels, sub-second fill awareness,
explicit shard-3 routing with pre-allocated collateral, discovery, dozens of concurrent
books and a demo environment (§6.2, §7.6); every missing piece is additive — `fill` and
`user_orders` channels, a filtered `trade` subscription, `add_markets`/`delete_markets`,
`exchange_index` on writes, `/markets` and `/series` readers, a host switch (§7.7, §11).
Recommendation: Option A — consume `rest`/`wsx` unchanged from a separate module and land
those additions inside them, strategy-free, so that extracting a `kalshi` module (Option B)
is a later move rather than a rewrite; do B only when a second consumer exists, and never C.
The one decision only Hugh can make is A/B/C plus whether `post_only`/TIF/STP may become a
per-client parameter. Bead: lip-p3v (§12).

---

## 1. Method

1. Package import graph from `go list` (no stdlib) and a symbol-level count of every
   cross-package identifier used by `rest`, `wsx` and `qual` (§2).
2. First-hand reading of the transport core: `rest/client.go`, `throttle.go`, `guard.go`,
   `wire.go` (order body), `write.go` (classification), `page.go` (outcomes),
   `wsx/transport.go`, `session.go`, `supervisor.go`, `frame.go`, `wire.go`,
   `feed/auth.go`, `cfg/params.go` (§3).
3. A read-only probe (`probe/main.go`) that reuses the PRODUCTION transport objects —
   `rest.NewHTTPDoer` under a zero-armed `rest.WriteGuard`, and `wsx.NewSupervisor` with
   `wsx.NewLiveDialer` and `cfg.Default()` — against the live exchange from the iMac, so the
   numbers are the harness's own latency path and not a synthetic client (§4).
4. Five delegated sweeps with disjoint scopes (spec clauses, tennis-strategy needs, latency
   instrumentation, binary wiring, API inventory), each verified by re-running its counts and
   quoting its cited lines (§5–§9, §13).

What was NOT done: no POST/DELETE timing (order writes are live steps), no demo-environment
test (the client has no demo switch; §7), no change to any file under `go/`.

---

## 2. Package map and coupling

### 2.1 Import graph (in-module and third-party only; `go list`, 2026-10-01)

```
cmd/accountcheck -> feed harness/rest
cmd/alarmcheck   -> harness/ping
cmd/conform      -> feed harness/rest harness/wsx tape
cmd/harness      -> core feed cfg hstore lifecycle netx num ping qual quote rest risk turnover wsx modernc.org/sqlite
cmd/incentives   -> harness/rest
cmd/replay       -> core store tape          (frozen consumer)
cmd/rig          -> core feed store tape     (frozen consumer)
core             -> (nothing)                (frozen)
feed             -> github.com/coder/websocket core   (frozen)
harness/cfg      -> num
harness/hstore   -> cfg num quote rest risk modernc.org/sqlite
harness/lifecycle-> cfg num quote rest risk
harness/netx     -> (nothing)
harness/num      -> (nothing)
harness/ping     -> hstore num quote risk
harness/qual     -> rest
harness/quote    -> core cfg num
harness/rest     -> core feed cfg num quote risk
harness/risk     -> cfg num quote
harness/turnover -> cfg num quote risk
harness/wsx      -> github.com/coder/websocket feed cfg rest risk
```

Three facts fall straight out of the graph:

- **The two transport packages are not leaves.** `rest` depends on `quote`, `risk`, `cfg`,
  `core`, `feed`; `wsx` depends on `rest`, `risk`, `cfg`, `feed`. Only `netx`, `num`, `cfg`
  (→ `num`) and the frozen `core`/`feed` are leaves.
- **Nothing above the transport is needed by it.** `hstore`, `lifecycle`, `quote`'s state
  machine, `risk`'s monitor, `turnover`, `ping` are consumers of `rest`/`wsx`, never
  dependencies. The dependency arrows all point the right way for extraction; the question is
  only how much of `quote`/`risk`/`cfg`/`core` the transport actually touches.
- **`feed` is frozen and both transports import it** for the signer and the two hosts
  (`feed/auth.go:31-34`, `rest/client.go:320`, `wsx/transport.go:246-247`). An extracted
  module cannot edit `feed`; it would copy the 185-line signer.

### 2.2 Symbol-level coupling (grep of exported identifiers in non-test files)

| package | symbols it uses from other lip packages (count) |
|---|---|
| `rest` | `quote`: `Side`(12) `SideYes`(6) `SideNo`(6) `MarketInput`(6) `MinPrice`(4) `MaxPrice`(4) `ValidPrice`(2) · `risk`: `Anomaly`(28) `SEV2`(7) `SEV1`(5) `SEV3`(2) `Portfolio`(1) · `cfg`: `Params`(3) `Validate`(1) · `core`: `Book`(7) `ParseSize`(5) `ParsePriceCents`(2) `Rig`(1) `NewBook`(1) `Levels`(1) · `feed`: `Universe`(6) `Signer`(6) `RestHost`(1) · `num`: `Qty`(21) `Money`(9) `ParseQty`(7) `MoneyScale`(4) `ValidateCount`(1) `QtyFromFloat`(1) |
| `wsx` | `risk`: `Anomaly`(36) `SEV2`(18) `FillEvent`(6) `SEV1`(5) `Portfolio`(5) `LiveOrder`(4) `OwnershipLookup`(3) `ReconcileMode`(2) `SEV3`(1) `PollRecord`(1) · `cfg`: `Params`(7) · `core`: `Rig`(10) `ParsePriceCents`(1) · `feed`: `WSURL` `WSPath` `Signer` · `rest`: `RateLimitError`(3) `PositionsResult`(2) `ParsePrice4`(2) `ParseFee6`(2) `OrdersResult`(2) `FillsResult`(2) `Doer`(2) `Client`(2) `Walk` `StatusResting` `Fill` `CentsExact` |
| `qual` | `rest`: `Doer`(6) `Response`(4) `Request`(4) `NewWriteGuard`(2) `Client`(1) |

Per-file, the coupling is concentrated:

```
rest/client.go: feed            rest/guard.go: -        rest/throttle.go: -
rest/page.go: risk              rest/sweep_trace.go: -  rest/account_positions.go: -
rest/coid.go: quote             rest/funding.go: num    rest/schedule.go: risk
rest/wire.go: num quote         rest/cancel.go: num risk
rest/orderbook.go: core num risk   rest/read.go: core num quote risk   rest/write.go: cfg num risk
wsx/transport.go: feed          wsx/session.go: cfg     wsx/supervisor.go: cfg
wsx/wire.go: -                  wsx/frame.go: rest risk wsx/read_throttle.go: rest
wsx/gate.go: cfg risk           wsx/portfolio.go: cfg rest risk
```

What each dependency really is:

- `risk.Anomaly` is a four-field value `{Class string; Sev Severity; Ticker string; Text string}`
  (`harness/risk/monitor.go:25-30`). It is the transport's only channel for "something odd
  happened" and accounts for 64 of the cross-package uses. It is a leaf type in disguise;
  it could move to its own tiny package without touching the monitor.
- `cfg.Params` is read for exactly eight fields across both packages: `WSPingInterval`,
  `PongTimeout`, `ReadDeadline`, `DisconnectReduce` (the F1 ladder, `cfg/params.go:73-84`),
  `Quiet`, `TruthMaxAge`, `PnLMarkMaxAge` (gate freshness) and `RetrySameCoidMax`
  (`rest/write.go:190`). The other 40-odd fields are LIP policy the transport never reads.
- `quote.Side` is not quote's at all: `type Side = num.Side` (`quote/queue.go:38-42`), a
  two-value enum in the leaf `num` package; `MinPrice`/`MaxPrice`/`ValidPrice` are 1..99 cent
  bounds. `quote.MarketInput` is the one genuinely strategy-shaped type `rest` touches
  (6 uses in `read.go`): it is how the orders/positions walk is projected into the quoting
  state machine's input. That projection belongs to the harness, not the transport.
- `core.Book`/`core.Rig` is the frozen, differentially tested book decoder. Frozen is fine
  for a consumer: it cannot change, which is exactly what a shared transport wants.
- `wsx/gate.go` (1,348 lines) and `wsx/portfolio.go` (616 lines) are NOT transport. They are
  the harness's freshness/ownership state machine (reconcile tokens, cross-checks, F5
  quarantine, PnL mark ages) and the REST portfolio poller. Together they are 1,964 of
  `wsx`'s 3,360 non-test lines. The actual socket client — `transport.go`, `session.go`,
  `supervisor.go`, `frame.go`, `wire.go`, `doc.go` — is 1,319 lines.

So the honest coupling map is: **the transport is ~2,900 lines of near-leaf code
(`rest` minus `read.go`'s projection, plus the five `wsx` socket files, plus `feed/auth.go`),
wrapped in ~2,000 lines of LIP-specific freshness and ownership machinery that lives in the
same two packages.**

---

## 3. What the transport does today (first-hand reading)

### 3.1 Signing and hosts (`feed/auth.go`)

- Loads `~/.kalshi/kalshi.pem` (PKCS#1 or PKCS#8 RSA) and `KALSHI_API_KEY_ID` from
  `~/.kalshi/env` (`feed/auth.go:43-74`); never logs key material.
- RSA-PSS SHA-256, salt length equal to the digest (`:164-179`) — the one PSS option Kalshi
  accepts, and the kind of detail a rewrite gets wrong first.
- Signs `timestamp_ms + METHOD + path-without-query`; websocket handshake signs `GET
  /trade-api/ws/v2` (`:181-183`). Hosts are constants: REST `api.elections.kalshi.com`,
  websocket `external-api-ws.kalshi.com` (`:31-33`). No demo host, no override.

### 3.2 REST transport (`harness/rest/client.go`)

- `Request{Method, Path, Query, Body}` / `Response{Status, Body, Header}` / `Doer` is an
  HTTP-level seam (`client.go:27-69`). A non-2xx is a `Response`, never an `error`; `error`
  means the outcome is unknown. `NotSent` and `WriteRefused` are the two provably-not-sent
  errors (`:71-108`).
- `HTTPDoer` (`:263-386`): signs every method the same way, builds `host + /trade-api/v2 +
  path (+ ?query)`, refuses redirects (`:303-317`), one `http.Client.Timeout`, **no retry,
  no throttle, no rate-limit awareness** — by design, retry policy is the write protocol's
  (`:258-262`). Production wires a `netx.CachedDialer` transport (one-hour last-known-good
  DNS, `:280-295`), distinct from the websocket's transport.
- `Endpoint` table (`:124-197`) pins per-endpoint cursor field, item keys, identity field,
  limit bounds and a page circuit breaker for `/portfolio/positions`, `/portfolio/orders`,
  `/portfolio/fills`, `/incentive_programs`. `Walk` (`page.go`) completes only on an empty
  cursor, asserts forward progress by first-record identity, and reports
  `WalkComplete | WalkRewound | WalkFailed` (`page.go:44-54`, `:165-176`).
- `Client` (`:396-416`) is a thin wrapper plus one piece of LIP state: the `unconfirmed`
  map that decides whether a cancel-and-sweep pages (`:407-412`).
- `RateLimitError` parses `Retry-After` (seconds or HTTP-date) and otherwise leaves backoff
  to the caller (`throttle.go:16-62`). The exchange sends no `Retry-After` on 429
  (readiness api-review, §8), so in practice every 429 is "caller decides".
- `WriteGuard` (`guard.go:38-167`): zero value is read-only; arming needs `-live` AND an
  absolute regular-file sentinel, stat'ed freshly per write; GETs bypass. Strategy-agnostic.

### 3.3 Order write path (`harness/rest/wire.go`, `write.go`, `coid.go`)

- The body is exactly `{ticker, side(bid|ask on YES), count "x.xx", price "0.xxxx",
  time_in_force, self_trade_prevention_type, post_only, client_order_id}`
  (`wire.go:143-161`). **`post_only` is always true, `time_in_force` is hard-coded
  `good_till_canceled`, STP is hard-coded `taker_at_cross`** (`wire.go:154-166`). No
  `expiration_ts`, no market/IOC/FOK order, no `buy_max_cost`, no reduce-only.
- `Create(ctx, body, p cfg.Params)` (`write.go:154`) retries an UNKNOWN outcome with the
  SAME coid up to `RetrySameCoidMax` (`:190`), classifies 2xx with the echoed coid as
  `CreateAcked`, `409 order_already_exists` as positive identification plus a confirming
  walk (`:282-288`, `:371-400`), other 409/429/5xx as `CreateUnknown` (`:291-330`), 4xx as
  `CreateRejected`.
- `Coid(runID, marketIdx, side, seq)` (`coid.go:52`) is a deterministic LIP scheme and
  `IsOurs` (`:145`) is ownership-by-prefix. Reusable as a scheme, but any other strategy
  needs its own run id namespace and must not collide.
- `Cancel` DELETEs by order id; `CancelAndSweep` (`cancel.go:106`, `:355`) is the LIP
  cancel-then-prove-absent protocol with the `unconfirmed` state.

### 3.4 Websocket (`harness/wsx/transport.go`, `session.go`, `supervisor.go`, `wire.go`, `frame.go`)

- Seams: `Signer` (handshake headers), `Socket` (frame-level Read/Write/Ping/Close),
  `Dialer`, `Clock` with a two-clock `Stamp{WallMs, Mono}` (`transport.go:24-81`).
- Live dialer: `coder/websocket`, permessage-deflate context takeover, 1 MiB read limit,
  redirects refused (`:111-169`). Per-read contexts carry no deadline on purpose (`:173-179`).
- Session (`session.go:113-295`): F1 three-clock ladder — ping every `WSPingInterval`
  (10 s), pong due in `PongTimeout` (5 s), and a `ReadDeadline` (60 s) **reset only by a
  delivered data frame** (`:219-222`). The one outbound command is `update_subscription`
  `get_snapshot` (`:11-35`, `:271-292`). Defaults: `cfg/params.go:149-153`.
- Supervisor (`supervisor.go`): dial with a 10 s handshake bound (`:307`), send
  `orderbook_delta` filtered to the universe and `trade` UNFILTERED (`wire.go:27-45`,
  deliberate: "one trade subscription yields the whole exchange tape"), backoff ladder
  1,2,4,8,16,32,60 s on abnormal close, none on clean close (`:13-33`, `:223-238`), emit
  `EventDisconnectReduce` once per outage after `DisconnectReduce` (`:241-284`).
  **Any change to the subscribed set is a reconnect** (`AddMarkets`, `:90-119`), chosen so
  the exchange re-sends full snapshots.
- `InspectFrame` (`frame.go:98-148`) classifies `orderbook_snapshot | orderbook_delta |
  trade | error | subscribed | other` and asserts integer-cent resting prices (H-CO-3a). The
  `fill`, `ticker`, `market_lifecycle_v2` and positions channels are neither subscribed nor
  decoded. Frames decode `ts_ms` (`core/rig.go:231-250`).

### 3.5 How fills reach the harness

Not over the socket. `wsx.Poller.Run` (`portfolio.go:140-173`) issues, sequentially and
without overlap, a COMPLETE `Fills` walk (all account history, lip-0ns), a `resting`
`Orders` walk and a `Positions` walk, then sleeps the remainder of `PositionPoll`
(5 s, `cfg/params.go:136`). This is the H-TOP-5 design — the socket is evidence, REST is
truth — and it bounds fill observation at `U(0, 5 s)` plus the cycle (§4).

---

## 4. The measured latency path (probe, 2026-10-01 21:45:42Z–21:48:10Z)

Probe: `probe/main.go`, built as its own module with `replace lip => go/`, run from the iMac
on the account's own key. It wraps `rest.NewHTTPDoer` in `rest.NewWriteGuard(doer,
rest.WriteArm{})` and proves the guard refuses a POST before any network I/O (output line 2),
then issues only signed GETs and opens the production `wsx.Supervisor` on ten open
ATP/WTA match markets for 120 s. Raw output: `probe/measure-20261001T214541Z.{json,txt}`.
Exchange state at the time: `trading_active=true` on all four exchange indices (Thursday halt
had ended at 09:00Z).

### 4.1 Fixed per-request costs

| quantity | value |
|---|---|
| RSA-PSS SHA-256 signature, 2048-bit key (n=200) | p50 1.05 ms, p90 1.08 ms, max 1.38 ms |
| cold connection (TCP+TLS+first GET `/exchange/status`), 1 sample | 151 ms |
| curl from the same host, cold, `/exchange/status` | connect 74 ms, TLS 118 ms, total 226 ms |

The two later "cold" samples (35 ms) reused `http.DefaultTransport`'s pool, because
`NewHTTPDoer` with a nil RoundTripper shares the default transport (`client.go:300-302`);
read them as warm.

### 4.2 Warm signed GET round trips (sequential, 250 ms apart, n=15 each, all HTTP 200)

| endpoint | first | min | p50 | p90 | max | body |
|---|---|---|---|---|---|---|
| `/exchange/status` | 32 | 27 | 39 | 110 | 202 | 667 B |
| `/portfolio/balance` | 101 | 96 | 107 | 151 | 229 | 271 B |
| `/portfolio/orders?status=resting&limit=1000` | 102 | 89 | 97 | 110 | 147 | 25 B |
| `/portfolio/positions?limit=200` | 105 | 92 | 101 | 108 | 110 | 1.9 KB |
| `/portfolio/fills?limit=1000` | 100 | 95 | 104 | 111 | 120 | 12.1 KB |
| `/markets/{T}/orderbook` | 91 | 89 | 97 | 101 | 124 | 1.4 KB |
| `/markets/{T}` | 102 | 92 | 101 | 108 | 111 | 2.4 KB |

Network floor from this host is ~27–39 ms (`/exchange/status`); every authenticated or
market endpoint sits at ~90–110 ms p50, i.e. ~60 ms of exchange-side work per call. The
typed `rest.Client` path (parse + validate + walk bookkeeping) adds under 10 ms:
`Balance` 100 ms, `Orderbook` 91 ms (`read`), `Orders(resting)` 90 ms (`complete`),
`Positions` 95 ms (`complete`), `Fills` complete 24 h walk 105 ms (`complete`, one page).

Six concurrent orderbook GETs completed in 110 ms wall (each p50 107 ms) on both a cold pool
and a warm one — the default transport parallelises across connections, so polling N books
costs one round trip, not N.

### 4.3 Websocket, production supervisor, 120 s

| quantity | value |
|---|---|
| dial + signed handshake + both `subscribed` acks | 305 ms |
| first `orderbook_snapshot` after connect | +68 ms |
| first `orderbook_delta` after connect | +918 ms |
| connects / disconnects in 120 s | 1 / 0 |
| frames | 18,892 `trade`, 103 `orderbook_delta`, 10 `orderbook_snapshot`, 2 `subscribed` |
| exchange-wide trade tape | 158 frames/s across 3,315 distinct tickers |
| **exchange `ts_ms` → local receipt, `trade`** (n=18,892) | **p50 10 ms, p90 16 ms, max 83 ms** |
| **exchange `ts_ms` → local receipt, `orderbook_delta`** (n=103) | **p50 8 ms, p90 12 ms** |
| exchange publish delay (`sending_ts_ms` − `ts_ms`, two sampled frames) | 3–4 ms |
| inter-frame gap, all frames | p50 0 ms, p90 21 ms, max 160 ms |
| inter-delta gap on the ten subscribed (pre-match) tennis books | p50 29 ms, p90 4.2 s, max 13.3 s |

The two "exchange `ts_ms` → local" rows are local wall clock minus exchange stamp, and the
local clock runs ~39 ms behind (§4.6): add that to read them as true exchange-to-process lag.

Two observations with consequences:

- **The feed is ~50 ms from exchange to process, and the transport is not the bottleneck
  anywhere.** The 10 ms figure is local wall clock minus exchange `ts_ms`, and the local
  clock runs ~39 ms behind (§4.6), so the corrected lag is roughly 40–55 ms at p50. A
  decision made on a delta can be acted on with a ~100 ms REST write (recorded ack proxy
  median 106 ms, recorded cancel median 98 ms, §8.2) ⇒ ~150 ms decision-to-book is what this
  stack can do today, before any dispatcher queueing.
- **The unfiltered `trade` subscription costs 158 JSON frames/s** at a quiet hour (crypto
  15-minute markets dominate: `KXBTC15M-26OCT011800-00` printed 6,377 trades in two minutes).
  The harness decodes every one in `core.Rig` and discards most. A strategy that only wants
  its own markets should pass `market_tickers` on the `trade` subscribe too; the harness
  deliberately does not (`wsx/wire.go:12-16`).

Live tennis trades at speed: an ATP Challenger match in play
(`KXATPCHALLENGERMATCH-26OCT01JUSSAK`) printed 713 trades in the window, ~6 per second across
its two legs. The ten main-tour tickers subscribed were pre-match and nearly idle.

### 4.4 The latency budget, as the architecture stands

| leg | today | source |
|---|---|---|
| book change at exchange → local book | ~50 ms (10 ms measured + ~39 ms local clock offset) | §4.3, §4.6 |
| local decision → signed POST on the wire | ~1 ms sign + dispatcher queue (global write bucket 5/s, burst 10, with reserved reducer slots; debounce 250 ms); in the harness also a SQLite permit commit before the first POST | `cfg/params.go:53-54,129`, `cmd/harness/dispatch.go:144,615-627,697-698` |
| POST round trip (ack proxy `bound_ms − reserved_ms`) | median 106 ms for later orders (n=11); 228–538 ms for the first order of a process | §8.2 (recorded) |
| cancel DELETE round trip | median 97.9 ms, 90.7–142.2 (n=32) | §8.2 (recorded, recomputed) |
| **own fill at exchange → harness knows** | **U(0, 5 s) + the ~0.3 s walk cycle; recorded over 10 fills: min 0.54, median 1.63, max 5.04 s** | §3.5, §8.2 |
| cancel → exchange list shows it gone | ~1.4–1.5 s typical (list lag) | §8.2 |
| abnormal socket drop → book restored | 1 s backoff + ~0.3 s dial/subscribe + 68 ms snapshot; recorded 1.28–1.30 s end to end | §3.4, §4.3, §8.2 |
| subscription change (new market) | full reconnect, ~0.3 s plus snapshots | `supervisor.go:90-119` |

The gap that matters is the fill leg: well over an order of magnitude between what the socket
delivers (~50 ms) and what the poll delivers (seconds). For a maker who must re-quote or hedge
the instant a leg fills, that leg is the whole game.

### 4.5 Shared host versus the dedicated host (REST-only, back-to-back, 22:00:48Z and 22:01:28Z)

The docs recommend `external-api.kalshi.com` for API traders; lip's REST uses the shared
`api.elections.kalshi.com` (§7.5). Same probe, same machine, `-rest-only`, n=15 per endpoint
(`probe/measure-host-*.{json,txt}`):

| endpoint (p50 ms) | shared | dedicated |
|---|---|---|
| `/exchange/status` | 36 | 88 |
| `/portfolio/balance` | 105 | 92 |
| `/portfolio/orders` (resting) | 98 | 85 |
| `/portfolio/positions` | 102 | 95 |
| `/portfolio/fills` | 106 | 92 |
| `/markets/{T}/orderbook` | 97 | 90 |
| `/markets/{T}` | 100 | 94 |
| cold connection, first sample | 158 | 337 |
| six concurrent orderbook GETs, wall | 117–120 | 163–166 |

The dedicated host is ~10 ms faster per sequential authenticated call, slower to connect and
slower for a parallel burst, and loses the edge-cached `/exchange/status`. Host choice is a
documentation-compliance item, not a latency lever; nothing in this review turns on it.

### 4.6 The local clock runs behind the exchange

Three independent signs, found while verifying §4.3 against the recorded evidence:

- `sntp time.apple.com` on this iMac at 22:0xZ reported the host clock **38.7 ms behind**
  (±20 ms) the NTP server.
- The probe's minimum trade lag was 1 ms (`probe/measure-20261001T214541Z.json`,
  `trade_exchange_ts_to_local_ms.min_ms`), below the ~15 ms one-way network time implied by
  the 27–39 ms `/exchange/status` round trip; a frame cannot arrive before it was sent, so
  the local clock must be behind the exchange by at least ~14 ms.
- Three recorded orders show the exchange's `created_time` **later** than the local
  `bound_ms` read after the ack by +24, +29 and +52 ms (§8.2), which is impossible with
  synchronised clocks.

Consequences: the §4.3 feed-lag figures are ~39 ms low (true p50 ≈ 40–55 ms); the harness's
`since` filter on fills, its signing timestamps and any future ack-latency metric all read
this clock; and a client that wants real exchange-relative latency needs an offset estimate
(the `sending_ts_ms` on every frame is a free one: local receipt minus `sending_ts_ms` must
exceed the one-way network time, which bounds the offset from below on every frame).

---

## 5. Spec clauses that bind the transport

Sweep: `sweeps/spec-transport-clauses.md` — 119 clauses of `notes/harness-spec.md` that
constrain the transport, each with a line range and a class: **60 G** (generic to any Kalshi
client), **19 L** (LIP-only, would be dropped), **40 M** (a generic mechanism parameterised
by LIP policy). Verification: every clause row's cited range contains its clause heading
(0 mismatches once a sub-range is allowed to sit under a heading up to 60 lines above);
three ranges re-read by eye match the sweep's summaries verbatim (§13).

**The spec's own premise is anti-speed.** S7, line 63: *"Latency is not binding. Scoring is
0.9999 at 10 ms requote latency and 0.9962 at 1 s. Nothing in this spec should be built for
speed."* That one sentence explains the architecture measured in §4: REST is truth and the
socket is evidence (H-TOP-5), fills are polled every 5 s, and a 60 s data-frame deadline is
acceptable. Nothing in the transport is slow by accident; it is slow where the spec said speed
did not matter.

**G — what an extraction must preserve** (the sweep's full list is in the archive; the
load-bearing ones): the HTTP-level `Doer` seam with the REJECTED/UNKNOWN split and the
unresolved-create accounting (H-ORD-2, lines 729-750); same-coid retry and 409 as positive
identification (H-ORD-2b, 783-826); the measured pagination contract, forward-progress guard
and status-string pinning (H-PAGE-1/1a, 1096-1112, 1161-1195); the two-key write guard
(H-VER-1, 2178-2180); the F1 ping/pong/read-deadline ladder and F6 DNS cache; redirects
refused; the coid scheme as a namespace (705-717).

**M — LIP policy hard-wired inside the transport.** These are the clauses an extraction must
turn into parameters, and the sweep's own top three:
- H-Q-3 (490-492): `post_only: true` *"with no exception, no flag, and no code path that
  sets it false … `harness/rest` has no parameter for it"*; H-CO-5 (343-348):
  `taker_at_cross` fixed, *"Do not change it, and do not omit it."* Both are ordinary
  Kalshi order fields that a scalper's exit leg needs to set differently.
- H-ORD-2 (729-750): the order tracker's "max possibly-live quantity stays reserved in every
  cap" ties transport order state to the strategy's risk model.
- H-FAIL-4 / H-FAIL-5 (1658-1667, 1583-1601): truth age and the disconnect quarantine gate
  every placement, so a shared module has to expose freshness and quarantine state rather
  than decide what to do about it. The same split runs through the detection-versus-response
  rows (F4, F5, F9, F10, F13-F15, H-POS-2, H-ORD-8, H-CAP-5): the detector is generic, the
  response (REDUCING, WINDING_DOWN) is harness state.

**L — what would be dropped**: two-connection topology and the frozen `feed` (H-TOP-1/2),
requote and cancel ordering (H-Q-9, H-QUE-2), startup adoption policy (H-ORD-5 steps 6-7,
5a, 5c), offline `rig.db` marking (H-ORD-7), `/incentive_programs` selection (H-SEL-2), the
halt and drain state machine (§10.4, H-FAIL-1/1a, §12 table, H-HALT-3), and §18 (2308-2309):
*"Taker orders of any kind, including emergency ones, are out of scope: there is no such code
path."*

**Judgement calls the sweep flagged that the reader should own**: (1) `post_only` and STP
are M, not L — making them parameters keeps the maker-safety property available instead of
discarding it; (2) the coid scheme is G if the `lipH-` prefix becomes a per-strategy
namespace; (3) **the spec never fixes the I/O boundary of `harness/rest`** — V1 (1978-1981)
calls it clock-free and I/O-free while H-TOP-3, H-VER-1 and H-TOP-4 put the signed HTTP
client and the write guard inside it — so where an extracted module's I/O and clock live is a
decision the extraction has to make, not one it can read off the spec.

---

## 6. What the tennis strategy needs from a client

Sweep: `sweeps/tennis-needs.md` — 94 cited claims (C01–C94) over `kalshi-bot` (KB, March
2026) and `kalshi_tennis` (KT, October 2025), each claim paired with a verbatim quote.
Verification: all 333 quotes were found at their cited lines by script (§13).

### 6.1 What the two repos actually built (stated)

| | KB (`kalshi-bot`) | KT (`kalshi_tennis`) |
|---|---|---|
| trigger | every live point event from the API-Tennis websocket, handled inline and serially (C62, C68) | a 30 s timer; each match evaluated once, then skipped (C70, C71) |
| Kalshi data | `GET /markets` by series (~16 tennis series), cached 60 s; no websocket, no depth (C02, C67) | two `GET /markets` per pass plus two per leg; no websocket (C72) |
| order | marketable limit buy at the touch: YES at the ask or NO at 100−bid; fresh UUID coid; `action: buy` only (C38–C41) | taker `market` buy of YES with a +2c buffer; the "exit sell" is sent as a second buy (C45, C46, C48) |
| order fields | ticker, action, side, type, count, `yes_price` — the 410'd v1 body; no TIF, post_only, expiry, STP, batch (C39) | v1 body; `time_in_force: good_till_cancel` on limits (C44, C50) |
| cancel / exit | none: no cancel call, no exit order, stops only block new entries (C42, C69, C86) | cancel-by-id after 30 s, once per pass; exits never sent (C47, C87) |
| fills / positions | assumed; the create response is unused (C41, C57) | balance and positions read before each order; order status polled per pass (C53, C58) |
| latency numbers written | model compute only: predict p95 <100 ms at 200 MC samples (C65); no Kalshi-side target (C66) | docs claim "<100 ms decision" and feed latency 2–5 s scraping / 1–2 s paid API; nothing about order-to-book (C75) |
| environment | demo by default, prod with `--prod`, hybrid "prod reads, demo orders" (C09) | prod only; executor needs extra env for the key (C91) |
| client-side limits | 20 read/s and 10 write/s token buckets, 429/5xx retries, 10 s timeout (C08) | ≥1 s spacing on the tennis API; none on Kalshi (C77) |

So the repos never needed a low-latency client: both are taker-at-the-touch, poll-driven,
and tolerate 3–60 s of Kalshi data age (requirements 4, 5). Neither can express a sell, an
ask, a resting quote, or an exit (requirements 6, 10). Both target the v1 order body that now
answers HTTP 410 (memory `kalshi-order-api-v2`), so their Kalshi clients are dead code either
way (requirement 15).

### 6.2 What the strategy Hugh actually wants needs (inferred from memory + §6.1)

The maker serve-schedule scalp (memory `tennis-scalping-intent`: post bids/offers around
service games, 1–3c target, 2–5 min horizon; taker round trips cost 6–8c and are
arithmetically dead) is NOT what either repo implements. Its client requirements, in the
order they bind:

1. **Resting post-only quotes on either leg with fast cancel.** The maker's risk is being
   picked off after a point by a faster feed, so the binding latency is point-event →
   cancel-on-the-wire, not hit speed. lip already has post-only bid/ask on either leg
   (§3.3) and a ~100 ms REST cancel (§4.2). No batch cancel exists (§7), so N resting quotes
   cost N round trips; six concurrent GETs took one round trip (§4.2), so parallel cancels
   should too, if the dispatcher allows it.
2. **Fill awareness in well under a second**, to re-quote the other leg or stop quoting a
   side that just filled. lip polls fills every 5 s (§3.5); the exchange's `fill` channel
   delivers them at websocket latency (~50 ms class, §4.3, §4.6) and lip does not subscribe to it
   (§7). This is the single largest gap for the strategy.
3. **A taker exit for emergencies** (a hold that goes wrong mid-set), which the spec
   forbids structurally (§5, H-Q-3, §18) and the wire refuses (`wire.go:268-272`). Fees make
   it expensive, not impossible; the strategy needs the option.
4. **Many markets at once**: on 2026-10-01 there were 24 open ATP and 54 open WTA match
   markets (§4 prelude), so dozens of books per process, with matches starting and ending
   through the day. lip's supervisor handles N tickers on one socket but reconnects for
   every added market (§3.4); `add_markets`/`delete_markets` exist in the protocol (§7).
5. **Discovery**: `GET /series?category=Sports&tags=Tennis` and `GET /markets?series_ticker=`
   (memory `kalshi-tennis-api-facts`), plus `market_lifecycle_v2` for new matches. lip
   implements none of these; it reads `/incentive_programs` and `/markets/{ticker}` only (§7).
6. **A demo environment and a hybrid mode** (KB's prod-reads/demo-orders) for rehearsal.
   lip has no demo switch (§7); its rehearsal mechanism is the read-only write guard, which
   is better for "does the live path work" and useless for "does my quoting logic fill".
7. **Shard-explicit writes and per-shard collateral** (§7.6): orders routed to exchange
   index 3 explicitly, and a funding step that moves collateral there before quoting.
8. **Not a client concern but the dominant latency**: the tennis feed. KT's own numbers put
   the point feed at 1–5 s behind reality (C75); the Kalshi book reprices on TV and faster
   feeds in that window. A 100 ms client under a 2 s feed is not the bottleneck; a 5 s fill
   poll is.

---

## 7. Feature inventory against the Kalshi API: implemented, hard-coded, absent

Sweep: `sweeps/api-inventory.md`, compared against OpenAPI 3.32.0 (100 paths, 117
operations) and AsyncAPI 2.0.0 fetched from `docs.kalshi.com` on 2026-10-01 (copies and
the sweep's counting scripts in `sweeps/api-spec/`; sha256 prefixes `fc70d406efd7a27d` and
`304956d30c986b02`). Verification: the operation, field and channel counts were recomputed
from the archived YAML, 155 code citations resolved with 0 hard misses, and three order-wire
citations were re-read verbatim (§13).

### 7.1 Counts

| metric | value |
|---|---|
| REST operations implemented | **11 of 117** (2 writes, 9 reads); 106 absent, of which the sweep classes 4 "needed", 26 "useful", 76 "niche" |
| cursor-paginated lists | 4, all through the one guarded walker |
| websocket data channels | **2 of 13** (`orderbook_delta` per-ticker, `trade` unfiltered) |
| websocket message types decoded | 3 data types + `error` + the `subscribed` ack, of 24 |
| websocket commands | `subscribe` at connect, `update_subscription get_snapshot`; no `unsubscribe`, `add_markets`, `delete_markets`, `list_subscriptions` |
| `CreateOrderV2Request` | 14 fields (6 required); 8 sent: 5 caller-controlled, 3 hard-coded |
| `orders` tag operations | 4 of 11 used; absent: cancel-all, batch create, batch cancel, amend, decrease, two queue-position reads |

### 7.2 Order features (sweep §4, re-read)

- Supported: ticker; `bid`/`ask` on YES via `ToWire` (a NO bid at p is `ask` at 100−p);
  two-decimal count with an upper bound; whole-cent prices 1..99 formatted `0.xx00`; a
  `lipH-` coid that must rebuild byte-identically (`wire.go:44-53, 146-147, 196-198`).
- Hard-coded and REFUSED if different, by a validator that runs at construction and again
  before dispatch: `time_in_force = good_till_canceled`, `post_only = true`,
  `self_trade_prevention_type = taker_at_cross` (`wire.go:164-166, 254-256, 268-281`). The
  client cannot send IOC, FOK, a taker, or `maker` STP.
- Absent: `expiration_time`, `reduce_only`, `cancel_order_on_pause`, `subaccount`,
  `order_group_id`, `exchange_index`; amend, decrease, batch create, batch cancel, cancel-all,
  queue position; a public get-by-id (an unexported `confirmRetired` GETs one order to
  confirm a lagging cancel, `cancel.go:302-341`). Sub-cent price grids (`deci_cent`,
  `tapered_deci_cent`, …, source of truth `price_ranges` on the market) are not expressible
  and a resting sub-cent level is rejected as a granularity anomaly (`wsx/frame.go:223-244`).
- `buy_max_cost` / `sell_position_floor` are not in the V2 request at all; they survive only
  in the legacy schema nothing references.

### 7.3 Websocket channels (sweep §5.2)

| channel | implemented | matters for a trading client |
|---|---|---|
| `orderbook_delta` (private, filter required, has `seq`) | yes, per ticker | yes |
| `trade` (public, filter optional, has `seq`) | yes, unfiltered: whole-exchange tape at 158 frames/s (§4.3) | yes, and should be filtered |
| `fill` (private, filter optional, **no `seq`**) | no — fills come from the 5 s REST poll | **yes: the fill-latency gap** |
| `user_orders` (private, **no `seq`**) | no — orders come from the REST poll | yes |
| `market_positions` (private, **no `seq`**) | no | useful |
| `ticker` (public, **no `seq`**) | no | useful (lighter than a book) |
| `market_lifecycle_v2` (public, no ticker filter) | no — `/markets/{ticker}` is polled every 30 s instead | yes for discovery and close handling |
| `multivariate_market_lifecycle`, `communications`, `order_group_updates`, `cfbenchmarks_value`, `cfbenchmarks_value_5hz`, `pyth_value` | no | niche |

The four private state channels carry no `seq`, so the protocol offers no gap detection for
them; a client that trusts `fill`/`user_orders` as truth needs its own reconciliation, which is
exactly the REST-truth discipline lip already has (H-TOP-5) — the two compose rather than
compete.

### 7.4 Hazards a generic client inherits or must add (sweep §7, selected)

1. Error bodies: the client reads nested `error.code` (observed live) while the spec's
   `ErrorResponse` is flat; both shapes should be accepted.
2. `use_yes_price` default on no-side book pricing "will be flipped to true" per the
   AsyncAPI; lip never sends the flag, so a flip changes the book semantics under it.
3. Market `status` has 8 values; `Schedule` accepts `active` and `finalized`+result and
   raises SEV1 on the rest (`rest/schedule.go`).
4. `GET /portfolio/fills` supports `min_ts`; the client never sends it (lip-0ns).
5. Subaccount/shard defaults differ: `Orders`/`Fills` cover all, `Positions` covers
   subaccount 0 unless `AccountPositions` is used.
6. Thursday pause: placement and amend are refused, cancels allowed, resting orders survive
   unless `cancel_order_on_pause` was set; lip cannot set it and does not read
   `/exchange/status` (lip-6dn).
7. Create success is documented 201, observed 200; the client accepts exactly those two.
8. Deprecated `side`/`action`/`taker_side` fields have passed their removal dates; the
   readers still parse them with fallbacks.

### 7.5 Environment (sweep §6)

- REST host is the SHARED `api.elections.kalshi.com`; the docs recommend the dedicated
  `external-api.kalshi.com` for API traders, which `cmd/incentives` already uses
  (`cmd/incentives/main.go:25`) and which the websocket already uses. Measured below (§4.5).
- No demo switch; demo hosts exist (`external-api.demo.kalshi.co`, demo websocket) with
  separate credentials.
- RSA keys only; timeouts are constructor arguments (production 10 s); REST and websocket
  use distinct transports over one cached dialer; rate limiting is a static dispatcher
  bucket (5/s, burst 10) with no tier awareness (`/account/limits` unread). Basic tier per
  the docs: 100 write tokens/s, 10 per order, 2 per cancel.

### 7.6 Two facts from the docs that bind a tennis client specifically

- **Exchange sharding.** Since 2026-08-24 new tennis and baseball events are created on
  exchange shard 3 (`GET /exchange/status` on 2026-10-01 lists index 3 as "Tennis, Baseball,
  Basketball"), and *"programmatic traders must preallocate collateral on a given exchange
  shard before order placement"*; transfers go through the intra-account transfer API and
  `GET /portfolio/balance` breaks the balance down by exchange index
  (`sweeps/api-spec/md_getting_started_exchange_sharding.md:22-33`). Omitting
  `exchange_index` on an order auto-routes, which the docs say *"will incur an additional
  latency cost"* and bills every shard's write bucket (sweep §4, `exchange_index` row). lip
  omits it on create and cancel. lip's `MarketFunding` IS shard-aware already: it reads the
  market's `exchange_index` and the shard-scoped balance with a `balance_breakdown`
  cross-check (`rest/funding.go:22-120`), so the read side of shard handling is reusable;
  the write side and the transfer endpoint are not there.
- **Ed25519 keys are now the recommended signer** (*"lower signing cost than RSA-PSS;
  64-byte signatures"*; RSA listed for "clients limited to RSA-PSS, including SDK versions
  before 3.31.0", `md_getting_started_api_keys.md:21-22`). lip's signer is RSA-only
  (`feed/auth.go:135-152`) and `feed` is frozen. RSA-PSS costs 1.05 ms per request (§4.1);
  Ed25519 would cost tens of microseconds. Small, but it sits on the cancel path.

### 7.7 The five absences that matter most for a generic trading client (sweep §8, agreed)

1. Order flexibility: IOC/FOK/taker, expiration, reduce-only, sub-cent prices, a free coid.
2. Order management: amend, decrease, batch create/cancel, cancel-all, queue position.
3. Private and lifecycle channels: `fill`, `user_orders`, `market_positions`, `ticker`,
   `market_lifecycle_v2`; `add_markets`/`delete_markets` instead of reconnecting.
4. Discovery and exchange state: `/markets`, `/series`, `/events`, trades, candlesticks,
   `/exchange/status`, `/exchange/schedule`; a full `/markets/{ticker}` reader.
5. Environment: demo switch, dedicated host, tier-aware limits.

---

## 8. Latency instrumentation that exists, and the recorded numbers

Sweep: `sweeps/latency-inventory.md` — 43 instrumentation rows, 66 recorded-measurement
rows (B.1–B.9) with a negative-results list (B.10), and 10 gaps (recounted here; the file's
own §5 tally says 42/77, a bookkeeping slip). Verification: 331 citations resolved (the 4
misses are ellipsised path abbreviations); the two headline distributions were recomputed
from their raw sources and match (§13).

### 8.1 What the harness measures today

- Websocket: a two-clock receive stamp per frame (`wsx/session.go:231`), the exchange
  `ts_ms` watermark in `core.Rig` (tracked, never compared with the local clock), per-market
  last-frame time for the F5 quiet test, the F1 timers (pass/fail; **no ping RTT is
  recorded**), the backoff ladder, the F4 outage clock, and the F7 host-sleep detector
  (wall minus monotonic, fires above 5 s, `lifecycle/sleep.go:87-149`).
- REST: **the production `Doer` reads no clock around `http.Client.Do`** and uses no
  `httptrace` (`rest/client.go:329-386`). The only request timing in the harness is the
  cancel sweep trace: `Started`/`Ended` per DELETE and per verifying `status=resting` read
  (`rest/sweep_trace.go`). 429s are captured in memory as `RateLimitError`; nothing persists
  them.
- Store (`hstore`): `owned_order.reserved_ms` (stamped before dispatch) and `bound_ms`
  (after the ack is parsed, but also by adoption walks) — the only order-ack proxy;
  `fill.first_seen_ms` (stamped after the whole three-walk cycle is applied);
  `state_event.ts_ms`; anomaly pipeline stamps; `balance_poll.ts_ms`.
- DNS/dialer: cache `resolvedAt` and the one-hour floor; **lookup and connect are not
  timed** (`netx/dns.go:144, 177-192`).
- Exchange timestamps: REST fills parse `ts` (whole seconds, ×1000); the microsecond
  `created_time` is not read (`rest/read.go:493, 644`).

### 8.2 Recorded numbers (sweep B.1–B.8; "derived" = recomputed by the sweep from raw rows)

| quantity | value | basis |
|---|---|---|
| order reserve→bind (ack proxy), later orders | **median 106 ms** (n=11, 95–112) | derived, 5 stages 2026-09-28..10-01, `owned_order` |
| same, first order of each process | 228, 430, 476, 489, 538 ms (median 476) | derived; cause not established (cold pool or SQLite permit) |
| exchange `created_time` minus local `bound_ms`, same order | **+24, +29, +52 ms** | derived, n=3: the local clock reads behind the exchange (§4.6) |
| decision latency `IDLE→QUOTING` to first `reserved_ms` | 0.41–0.43 s | derived, 3 stages |
| **cancel DELETE round trip** | **n=32: min 90.7, median 97.9, max 142.2 ms** (8×200, 24×404) | derived from five sweep-trace logs; recomputed here exactly |
| verifying resting read / whole cancel sweep | median 103.0 ms (n=31) / median 477 ms (n=19) | derived; recomputed here exactly |
| **fill detection lag, 10 owned fills** (exchange `created_time` → `first_seen_ms`) | **min 0.54, median 1.63, mean 2.20, max 5.04 s** | derived; the maximum is one poll interval |
| cancel→list visibility lag after our own cancel | ~1.4–1.5 s typical; 0.27–1.2 s in six SEV1 sweeps; one episode cleared in 219 ms | stated (lip-9tt, lip-kaf) |
| forced reconnect, disconnect→connected | 1.28 s and 1.30 s incl. the 1 s backoff ⇒ dial+upgrade+subscribe ≈ 0.28 s; clean reconnect 0.26 s; halt-period cycle ⇒ ≈ 0.37 s | derived, q01 runs and the Thursday halt |
| process start → first `websocket:connected` | median 3.2 s (n=9) incl. startup REST reads | derived |
| authenticated GET round trip (`cmd/accountcheck`, status 200) | n=776: min 88.3, **p50 103.5, p90 122.8**, p99 572, max 1,901 ms; first request of a process p50 234 ms | derived; recomputed here over 716 requests in 36 files: p50 103.6, p90 122.0, p99 572, max 1,901, first p50 237 |
| worst recorded REST burst | 0.95–1.9 s per request, six requests, 2026-09-29 preflight | derived |
| harness's own authenticated request rate | 1.5–3.4 req/s | derived from q01 evidence |
| whole-exchange feed, July 2026 (Python rig) | ~390 frames/s; deltas ~400/s, trades ~2/s | stated |
| per-frame `rig.Handle` cost (in-process replay) | p99 14.7 µs, p99.9 41.7 µs at 88,659 frames/s | stated |
| `balance_poll` interval regularity | p50 5.000 s, p99 5.13–5.17 s, max 5.6–8.9 s | derived, 5 stages |
| `WS_DISCONNECT` through the 2026-10-01 halt | 117 rows, period median 61.37 s | derived (lip-6dn) |
| modelled LIP score vs requote latency (S7) | 10 ms 0.9999 · 200 ms 0.9988 · 1 s 0.9962 · 5 s 0.9882 · 60 s 0.9471 | `notes/economics.md:113-122` |

Not found anywhere in `notes/`: a handshake or dial duration, a ping/pong RTT, a DNS lookup
time, an authenticated 429 count or any `Retry-After` value, a stated create-ack latency.

### 8.3 Gaps a low-latency client would want (sweep C, condensed)

Per-request REST timing with phase breakdown (`httptrace`); submit→ack as a first-class
metric (the create and cancel responses carry `ts_ms`, unused); fill observation quality;
websocket delivery lag (every frame carries `ts_ms`, never compared); ping RTT; connection
setup phases; **clock offset to the exchange**; rate-limit telemetry; the disconnect cause
(`Event.Cause` is dropped before the store); internal pipeline latency (frame→decision,
write-queue wait, permit wait).

---

## 9. How the binaries wire the transport; the minimal standalone surface

Sweep: `sweeps/cmd-wiring.md` — construction rows for all five binaries, the exported
surface each uses (from a Go AST pass cross-checked by `go/types` over 269 packages),
the `cmd/harness` data flow, and five standalone call sequences compiled with `go vet` in a
scratch module. Verification: 376 citations resolved (one miss is my checker resolving a
bare `main.go` to the wrong binary); the probe in §4 is the live counterpart of sequences
(a), (b) and (e2) (§13).

### 9.1 Surface

| consumer | distinct exported identifiers from `rest`/`wsx` | code uses |
|---|---|---|
| `cmd/harness` | **88** (47 `rest` + 41 `wsx`; 97 with `netx` and `feed`) | 267 |
| the four small binaries together | **14** (13 `rest` + 1 `wsx`; 15 with `feed.NewSigner`) | 63 |

Seven identifiers are common to both: `rest.Client`, `Doer`, `NewClient`, `Request`,
`Response`, `StatusResting`, `wsx.InspectFrame`. The small binaries use the raw layer the
harness never touches directly (`Endpoint`, `EpOrders`/`EpFills`/`EpPositions`/`EpPrograms`,
`NewHTTPDoer`, `ParsePrice4`), which is exactly the layer a new consumer wants.

### 9.2 Data flow in `cmd/harness` (sweep §3, condensed)

Input goroutines — the websocket supervisor (events, buffer 64), the portfolio poller
(reads, buffer 2), six REST loops (schedule, programs, selection, terminal, balance,
cross-check), and a dispatch pool of at most two workers calling `rest.Client.Create` and
`CancelAndSweep` (results, buffer 2) — all feed ONE owner goroutine's select loop
(`run.go:837-938`). The owner is the sole writer of the book, the position model and the
queue; after every event and every 250 ms it runs a chain of pure functions
(`evaluate → quote.NextMarket → quote.Decide → enqueue → pump → build`) and hands a
`rest.CreateOrder` to the dispatcher. In a read-only process `pump` stops before the send and
records a would-write.

Properties a consumer inherits from the transport versus from the harness (sweep §3.4):

- From the transport: polled fills/orders/positions (§3.5); a reconnected-never-abandoned
  socket with the F1 ladder; a 429 surfaced as `RateLimitError` with no retry.
- From the harness, NOT the transport (a standalone consumer does not get these): the
  first placement waits for a SQLite permit commit before the POST (`dispatch.go:144,
  288-422`); a verified cancel closes the placement gate until a fresh full poll cycle has
  landed, so cancel-then-replace costs a DELETE, a resting walk, then three more walks before
  the new POST (`run.go:3694-3705, 2600, 2043-2048`); all pacing (250 ms tick, 250 ms
  debounce, the 5/s write bucket, per-endpoint read backoff).

### 9.3 Minimal standalone usage (sweep §4; the probe ran a, b and e2 live)

| step | standalone? | needs from the LIP side |
|---|---|---|
| (a) sign and `GET /portfolio/balance` | yes | nothing constructed (the binary links `core`, `feed`, `cfg`, `num`, `quote`, `risk` through `rest`'s imports) |
| (b) walk `/portfolio/orders` | yes | nothing; the typed result flags `Ours` by the `lipH-` prefix, so every non-LIP order reads as foreign |
| (c) place an order | **no** | a `cfg.Params` (one field read: `RetrySameCoidMax`), a `num.Side` (re-exported as `quote.Side`), and a coid that parses as `lipH-<run>-<idx>-<side>-<seq>`; post-only GTC only |
| (d) cancel | yes | `Cancel(ctx, id)` is a bare DELETE (no shard routing, no production caller); `CancelAndSweep` is the verified path |
| (e1) raw websocket via frozen `feed.Dial` | yes | nothing; no keepalive, no deadline |
| (e2) supervised websocket | **no** | a `cfg.Params` that passes `Validate()` (use `cfg.Default()`); four fields are read |
| book from frames | **no** | `core.NewRig` plus a `core.Sink` (frozen, fine) |

No step needs `risk.*`, `hstore`, `lifecycle` or `quote`'s state machine.

---

## 10. Open client beads and whether they travel with the transport

| bead | what | portable? |
|---|---|---|
| lip-0ns P4 | `Fills()` walks all account history every call; `backfill_h` is client-side | travels with `rest.Fills`; measured cost today 105 ms / 1 page, so harmless until history grows |
| lip-6dn P3 | F1 read deadline reconnects a healthy, frame-silent socket every 61 s through the Thursday halt | travels with `wsx.session`; any consumer with quiet markets hits it, tennis included (pre-match books went 13 s without a delta in §4.3; the exchange-wide trade tape is what keeps the deadline fed) |
| lip-8tl P3 | acked order that fills before its first orders walk stays in `o.pending` | `cmd/harness/run.go` only; not transport |
| lip-1ug P3 | book sizes never asserted two-decimal | `wsx/frame.go`/`core`; travels, low impact |
| lip-006 P4 | accountcheck marks positions malformed on nonzero event exposure | `cmd/accountcheck`; not transport |

---

## 11. Options

What every option has in common, because the strategy needs it and the client lacks it
regardless of where the code lives (§6.2, §7): decode the `fill` and `user_orders` channels;
filter the `trade` subscription; `add_markets`/`delete_markets` instead of reconnecting;
`exchange_index` on writes; `GET /markets` and `GET /series` readers; a demo host switch; and
eventually a policy seam for the hard-coded `post_only`/TIF/STP constants. Call that the
**common additions**. They are additive, strategy-free, and cost the same under A, B or C.
The options differ only in where the transport lives and who pays the requalification.

### Option A — consume as-is from a separate module; add the common additions inside `rest`/`wsx` (recommended)

- The tennis bot is its own Go module (as `probe/` is: `replace lip => …/lip/go`) importing
  `lip/feed`, `lip/harness/rest`, `lip/harness/wsx` and `lip/harness/cfg` unchanged. The
  probe is the existence proof: signer + `HTTPDoer` + `WriteGuard` + `Client` +
  `Supervisor` + `InspectFrame` + `cfg.Default()` is a working, guarded, measured client in
  ~400 lines, built in minutes, with the harness's `Gate`/`Poller` simply not used.
- The common additions land in `rest`/`wsx` under the ordinary verification workflow, each
  written strategy-free (parameters, not constants; the leaf `Anomaly` type; no new
  `cfg.Params` reads) so that a later extraction is a move, not a rewrite.
- The maker scalp needs exactly the write policy the transport already pins (post-only,
  GTC, `taker_at_cross`); the taker exit is deferred and handled as the one spec-level
  decision below.
- Cost: no extraction, no re-pinning of harness receipts (nothing new is added under
  `go/` except the additions, which require requalification in every option). Risk: the
  tennis bot inherits lip's gate cadence for any transport change it needs, and
  `NewSupervisor` demands a fully valid `cfg.Params` for four fields (use `cfg.Default()`;
  ugly, harmless).

### Option B — extract a `kalshi` transport module now; the harness becomes one consumer

- New module holding: a signer (copied from frozen `feed`, plus Ed25519), `Doer`/`HTTPDoer`/
  `WriteGuard`/`RateLimitError`/`Endpoint`/`Walk`/parsers, a generic create order with all
  14 V2 fields and a per-client policy pin, cancel/amend/batch, the websocket
  `Dialer`/`Socket`/`Clock`/session/supervisor with every data channel decoded, a leaf
  `Anomaly`, a 4-field transport params struct. lip's `rest`/`wsx` keep `CancelAndSweep`, the
  coid scheme, `Gate`, `Poller` and the `MarketInput` projection, and import the module.
- The coupling map says the code move is modest (§2.2: ~2,900 near-leaf lines; eight
  `cfg.Params` fields; one four-field anomaly struct; `quote.Side`). The cost is elsewhere:
  every lip importer changes (`cmd/harness` 13,142 lines, `hstore`, `lifecycle`, `qual`,
  `ping`), the mutation catalogue entries that target `rest`/`wsx` behaviours must be
  re-aimed at the new seam, the spec's unspecified I/O boundary (§5) has to be decided and
  written into `harness-spec.md`, and A1/A2 (post-only on every order the harness sends)
  must be re-proven at a seam that now has a parameter. That is a full requalification
  of the harness for a change that makes the harness no better.
- Right when: a second non-LIP consumer exists, or lip's gate cadence is demonstrably
  blocking the tennis bot. Not before the strategy has shown it earns.

### Option C — fork `rest`/`wsx`/`feed` into the tennis repo and evolve them separately

- Fastest to first trade and free of lip's gates; also the end of "one client". Two copies of
  the measured-contract code diverge, fixes stop flowing, the 11,500 test lines and the
  mutation catalogue stay behind, and the hard-won traps (cursor rewind, 409 semantics,
  status spelling, PSS salt, redirect hazard, integer-cent assertion) have to be re-learned
  on the copy. Reasonable only if lip is being retired.

### Recommendation

**A, with the additions written so that B is a move later.** Evidence: the transport is
already fast where it is (§4: ~50 ms feed, ~100 ms authenticated REST, ~98 ms cancels, one
connection and no drops in 120 s) and already usable standalone (the probe); what the strategy lacks is
features, not a boundary (§6.2, §7.6); and the spec's anti-speed premise (§5, S7) lives in
the harness's state machine, not in the transport. Extraction's cost is requalifying 13k
lines of a harness that, per the economics review, is not where the money is.

**The one decision only Hugh can make**: whether `post_only`/TIF/STP become a per-client
policy pin (so a tennis client can send a taker exit) or stay structurally impossible in
`harness/rest` as H-Q-3 requires. Changing it is a spec edit and a new mutation proving the
harness cannot construct a taker-capable client. It is not needed for the first maker
version, so it can wait until the exit logic exists.

**Order of work under A** (each a bead; none live):
1. `wsx`: decode `fill` and `user_orders` frames; `market_tickers` on the `trade` subscribe;
   `add_markets`/`delete_markets` commands. Measures: fill observation drops from U(0,5 s)
   to the ~50 ms class; subscription changes no longer reconnect.
2. `rest`: `exchange_index` on create/cancel; `GET /markets` and `GET /series` readers;
   `min_ts` on fills (closes lip-0ns); host/demo switch on `HTTPDoer` and the dialer.
3. Read `/exchange/status` in the session so a frame-silent socket during a pause is not a
   disconnect (closes lip-6dn).
4. The tennis bot module, consuming 1–3, with its own coid namespace and quoting logic.
5. Only then: the policy pin for taker exits (spec decision), and Ed25519 in a non-frozen
   signer.

---

## 12. Bead

**lip-p3v** (P2, open): *Client portability: consume lip rest/wsx as-is from a separate
module for the tennis maker bot; add fill/user_orders channels, trade filter, add/delete
markets, exchange_index, /markets+/series, demo host; defer extraction.* Its body carries
the verdict, the ordered common additions, the options not chosen and the decision left to
Hugh. It is a decision-plus-plan bead: if Hugh picks B or C it should be re-titled rather
than closed, and the additions become child beads once the path is chosen. Related open
beads it would close on the way: lip-0ns (`min_ts` on fills), lip-6dn (exchange status in
the session).

---

## 13. Verification record for the delegated sweeps

Each sweep ran as a sonnet subagent with a written prompt (archived in `sweeps/prompts/`),
a read-only scope disjoint from the others, and one output file. Its claims were treated as
claims until checked here.

| sweep | claimed | recomputed | citation check |
|---|---|---|---|
| spec-transport-clauses | 119 rows; G 60 / L 19 / M 40 | 119 clause rows (plus 20 parameter rows); G 60 / L 19 / M 40 | script: each row's clause id found within its cited range or under a heading ≤60 lines above: 0 mismatches. Eye: line 63 (S7), 490-492 (H-Q-3), 343-348 (H-CO-5) match verbatim |
| tennis-needs | 94 claims, 333 quotes | 333 `[repo path:line] \`quote\`` pairs | script: every quote found at its cited line (333/333). Eye: C07, C08, C09, C38, C67 read against the repos |
| api-inventory | 117 ops / 11 implemented; 13 ws channels / 2; 14 create fields / 8 sent; spec fetched | PyYAML over the archived copies: 100 paths, 117 operations, `CreateOrderV2Request` 14 fields / 6 required, 15 AsyncAPI channels of which 13 are data channels (`root`, `control_frames` excluded); sha256 prefixes match | script: 155 code citations resolve, 0 hard misses, 35 soft (identifier elsewhere on the line). Eye: `wire.go:164,254-256,268-281` and `cancel.go:302-306` verbatim |
| latency-inventory | A 42 rows / B 77 rows / C 10 (file §5); agent's closing note 43/66/10; anchors verified | recount by script: A 43, B 66, C 10 (the file's §5 line is wrong, the content is not). Authenticated GET distribution recomputed over `requests[].duration_ms` in 36 `account-*.json` files (716 requests): p50 103.6 / p90 122.0 / p99 572 / max 1,901 ms, first-request p50 237 — sweep's 776-request figures agree to rounding. DELETE/read/sweep durations recomputed from the five sweep-trace logs: n=32 median 97.9 (90.7–142.2), 8×200 + 24×404; reads n=31 median 103.0; sweeps n=19 median 477.2 — exact match | script: 331 citations resolve; the 4 misses are ellipsised path abbreviations; 80 soft. Eye: stage-report lines 65 and 79, api-review.json:22-23 |
| cmd-wiring | 88 identifiers in `cmd/harness` (47+41), 14 in the small binaries; (c) and (e2) need `cfg.Params`, (c) a `lipH-` coid; sequences compiled with `go vet` | `quote.Side` alias confirmed at `quote/queue.go:38-42`; the probe (§4) is a live run of sequences (a), (b) typed and (e2) | script: 376 citations resolve, 1 miss is the checker resolving a bare `main.go` to `cmd/harness` (the cited `cmd/accountcheck/main.go:525` exists; the file has 545 lines), 40 soft |

---

## Appendix A — how to reproduce the probe

```
cd /Users/hugh/kek/lip/notes/client-portability-evidence-2026-10-01/probe
GOTOOLCHAIN=local GOPROXY=off GOSUMDB=off GOFLAGS=-mod=mod go build -o /tmp/measure .
/tmp/measure -tickers <comma-separated open tickers> -n 15 -gap 250ms -ws 120s -out /tmp/measure.json
```

The module's `replace lip => /Users/hugh/kek/lip/go` pins it to this tree. It needs
`~/.kalshi/kalshi.pem` and `~/.kalshi/env`. It cannot write: the guard is constructed with a
zero `WriteArm` and the program aborts if a POST is not refused before transmission.
