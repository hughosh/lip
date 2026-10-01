# Kalshi API inventory: what the lip Go client implements vs the public API

Delegate: `api-inventory` (prompt: `/Users/hugh/kek/lip/loop/run/client-portability/prompts/api-inventory.md`).
Date: 2026-10-01. Read-only sweep; this file is the only thing written.

Conventions

- Go paths are relative to `/Users/hugh/kek/lip/go/` (`harness/rest/client.go:155` is `/Users/hugh/kek/lip/go/harness/rest/client.go:155`). Line numbers are from the files as they stand on 2026-10-01.
- `(outside scope)` marks a file outside `harness/rest`, `harness/wsx`, `feed` that I read only to resolve a fact the scoped code delegates to it.
- `openapi.yaml:N` and `asyncapi.yaml:N` are lines in the copies fetched today (section 0).
- "Implemented" means a non-test call site exists in `harness/rest`, `harness/wsx` or `feed`.
- Class codes in section 5: **N** = a generic trading client is incomplete or unsafe without it; **U** = useful for many strategies (makers, low-latency, risk control) but a minimal client can run without it; **S** = niche (strategy-, account-type- or institution-specific). The classes are my judgment; the counts and field lists are computed from the spec.

## 0. Provenance: what was fetched, what was read

The OpenAPI spec WAS fetched. `docs.kalshi.com` answered HTTP 200 to `curl -sL` on every request (no 429). `kalshi.com` was not queried.

| artifact | URL | HTTP | bytes | sha256 prefix | what it is |
| - | - | - | - | - | - |
| OpenAPI | https://docs.kalshi.com/openapi.yaml | 200 | 366,380 | fc70d406efd7a27d | OpenAPI 3.0.0, `info.version` 3.32.0, 100 paths, 117 operations, 207 schemas. Servers listed at `openapi.yaml:6-14` |
| AsyncAPI | https://docs.kalshi.com/asyncapi.yaml | 200 | 166,145 | 304956d30c986b02 | AsyncAPI 3.0.0, "Kalshi Market Data WebSocket API" 2.0.0: 13 data channels, 24 data message types, 15 command/response types |
| index | https://docs.kalshi.com/llms.txt | 200 | 56,739 | n/a | 259 lines; 117 `api-reference` pages (equals the 117 operations), 15 `websockets`, 12 `fix`, 51 `margin-rest`, 8 `margin-ws`. It also lists four more specs that were NOT fetched or parsed: `perps_openapi.yaml`, `perps_scm_openapi.yaml`, `perps_asyncapi.yaml` (Perps/margin exchange) |
| doc pages | https://docs.kalshi.com/<page>.md (33 pages) | 200 | n/a | n/a | getting_started: api_environments, demo_env, rate_limits, pagination, quick_start_websockets, orderbook_responses, order_direction, fixed_point_migration, market_lifecycle, maintenance_and_pauses, exchange_sharding, api_keys, order_groups. websockets: index, connection, connection-keep-alive, orderbook-updates, market-ticker, public-trades, user-fills, market-positions, user-orders, market-and-event-lifecycle, multivariate-market-and-event-lifecycle, communications, order-group-updates, cfbenchmarks-value, cfbenchmarks-value-5hz, pyth-value. Also fix/connectivity, cfbenchmarks/rest-passthrough, sdks/overview, changelog/index |
| changelog | https://docs.kalshi.com/changelog/index.md | 200 | 221,031 | n/a | 192 dated entries, 2025-06-12 to 2026-10-08. The two newest are dated 2026-10-08, after today's date (2026-10-01), so they announce scheduled releases. Used in sections 5 to 7 for changes that postdate the lip code |

Method for spec facts: the YAML was parsed with PyYAML and every count in this file was computed, not eyeballed. Scripts and the downloaded copies live in the session scratchpad `.../scratchpad/api-inventory/` (not deliverables). To reproduce: `curl -sL https://docs.kalshi.com/openapi.yaml -o openapi.yaml`, then count `paths.*.{get,post,put,delete,patch}` (117).

Source read in full (non-test): `harness/rest` 14 files / 5,184 lines, `harness/wsx` 11 files / 3,360 lines, `feed` 3 files / 571 lines. Outside-scope files consulted: `core/rig.go` (sequence-gap detector and frame decoder), `cmd/harness/run.go`, `cmd/harness/runtime.go`, `cmd/harness/dispatch.go`, `cmd/harness/config.go`, `harness/cfg/params.go`, `harness/num/qty.go`, `harness/quote/cross.go`, `notes/harness-spec.md` (observed error-body shape).

## 1. Summary counts

| metric | value |
| - | - |
| REST operations in spec 3.32.0 | 117 |
| REST operations implemented | **11** distinct method+path pairs (9.4%): 2 writes, 9 reads. 12 call sites, because `feed.Universe` repeats `GET /incentive_programs` unsigned. One of the 11 (`GET /portfolio/orders/{order_id}`) is reached only through an unexported helper |
| REST operations absent | **106**: 5 class N, 28 class U, 73 class S (section 5.1) |
| Query/path/body parameters declared on the 11 | 54; the client ever sends 29; all 29 exist in spec 3.32.0 (no stale names) |
| Cursor-paginated lists | 4 (`orders`, `fills`, `positions`, `incentive_programs`), all through one guarded walker |
| WebSocket data channels | **2 of 13** implemented: `orderbook_delta` (per-ticker), `trade` (all markets). 11 absent |
| WebSocket message types decoded | 3 data types (`orderbook_snapshot`, `orderbook_delta`, `trade`) plus `error` and the `subscribed` ack, of 24 data message types |
| WebSocket commands | `subscribe` (at connect), `update_subscription` with `get_snapshot` only. Absent: `unsubscribe`, `list_subscriptions`, `add_markets`, `delete_markets` |
| `CreateOrderV2Request` fields | 14 in spec (6 required, 8 optional). **8 sent** (5 caller-controlled: ticker, side, count, price, client_order_id; 3 hard-coded constants: time_in_force, self_trade_prevention_type, post_only). **6 absent**: expiration_time, cancel_order_on_pause, reduce_only, subaccount, order_group_id, exchange_index |
| Spec `orders` tag (11 operations) | 4 used (create, cancel, list, get-by-id internal). 7 absent: cancel-all, batch create, batch cancel, amend, decrease, queue positions (2) |
| `buy_max_cost`, `sell_position_floor` | do not exist in the V2 request (section 4) |

## 2. REST endpoints implemented

Columns follow the prompt. "Walker" means `Client.Walk` (section 2.2). "Timeout" is `http.Client.Timeout`, which production sets to 10 s (`cmd/harness/runtime.go:66,354`, outside scope).

| method | path | Go function | file:line | params supported | pagination | response fields parsed | retry / timeout |
| - | - | - | - | - | - | - | - |
| POST | `/portfolio/events/orders` | `(*Client).Create` | `harness/rest/write.go:154` (request at `:196-200`) | body only: ticker, side, count, price, client_order_id, plus 3 constants (time_in_force, self_trade_prevention_type, post_only). 8 of 14 spec fields (`harness/rest/wire.go:143-161`) | n/a | 200/201 only (`write.go:254`): order_id, client_order_id (must echo ours), remaining_count, fill_count, all required (`write.go:436-511`). Not read: average_fill_price, average_fee_paid, ts_ms. Errors: nested `error.code` only (`write.go:515-525`) | same-coid retry up to `RetrySameCoidMax`=3 (`write.go:190-194`, `harness/cfg/params.go:157`, outside scope), no delay between attempts. Retried on transport error, unobserved 2xx, uncharacterised 409, 5xx. A definite 4xx or a 429 returns at once (`write.go:301-325`). 10 s per attempt; dispatcher bounds the whole write at 10 s + 3 x 10 s (`cmd/harness/dispatch.go:144,236`, outside scope) |
| DELETE | `/portfolio/events/orders/{order_id}` | `(*Client).Cancel` -> `cancel`; production uses `cancelForMarket` | `harness/rest/cancel.go:106`, `:122`, `:130` (request `:136-143`) | path order_id; query `market_ticker` for shard auto-routing (`cancel.go:140-142`). Not sent: subaccount, exchange_index | n/a | any 2xx: client_order_id, reduced_by (`cancel.go:164-185`). 404 = gone (`:187-191`); 429 = RateLimitError (`:193-196`); other 4xx: `error.code` (`:198-204`). Not read: order_id, ts_ms | one attempt per call (`cancel.go:100-105`). `CancelAndSweep` re-DELETEs once after a verifying walk (`cancel.go:220,370-438`). 10 s |
| GET | `/portfolio/orders` | `(*Client).Orders` | `harness/rest/read.go:237`; endpoint table `harness/rest/client.go:170-176` | ticker, status (validated to resting/canceled/executed, `client.go:219-248`), limit=1000, cursor. Not sent: event_ticker, min_ts, max_ts, subaccount, exchange_index | walker; cursor field `cursor`; item key `orders`; identity `order_id`; limit 1..1000; MaxPages 1000 | `decodeOrder` (`read.go:276-337`): order_id, client_order_id, ticker (fallback market_ticker), status, last_update_time, book_side (fallback side/outcome_side), yes/no_price_dollars, remaining_count_fp (fallback remaining_count). 11 of 27 spec Order fields read; one undecodable record fails the whole walk (`read.go:256-269`) | none inside the walk; a failure returns `WalkFailed` and the caller keeps prior state (`harness/rest/page.go:102-103`). 10 s per page |
| GET | `/portfolio/orders/{order_id}` | `(*Client).confirmRetired` (unexported) | `harness/rest/cancel.go:302` (request `:304-305`) | path order_id only | n/a | `{"order": {...}}` through `decodeOrder` (`cancel.go:315-339`); a 404 is explicitly NOT a confirmation | none; 10 s. Used only by `CancelAndSweep` to overrule a lagging resting list |
| GET | `/portfolio/fills` | `(*Client).Fills` | `harness/rest/read.go:544`; `client.go:177-183` | ticker (single), limit=1000, cursor. Not sent: order_id, min_ts, max_ts, subaccount, exchange_index. The `since` argument is applied CLIENT-SIDE after a full walk (`read.go:537-543,567-570`) | walker; cursor `cursor`; key `fills`; identity `fill_id`; limit 1..1000; MaxPages 1000 | `decodeFill` (`read.go:576-647`): fill_id, trade_id (required), order_id, ticker (fallback market_ticker), fee_cost (kept as string, parsed by `ParseFee6` at `read.go:106-148`), is_taker (required, null refused), book_side (fallback side/outcome_side), yes/no_price_dollars, count_fp (fallback count), ts (seconds or ms, `read.go:652-668`). 14 of 18 spec Fill fields read | as `Orders`. The poller walks ALL fills with no ticker and zero `since` every cycle (`harness/wsx/portfolio.go:166`) |
| GET | `/portfolio/positions` | `(*Client).Positions`; `(*Client).AccountPositions` | `harness/rest/read.go:436`; `harness/rest/account_positions.go:85-153`; `client.go:155-169` | limit=200, cursor; `AccountPositions` adds `subaccount=N` per enumerated subaccount (`account_positions.go:175`). Deliberately no ticker filter (`read.go:430-435`). Not sent: count_filter, settlement_status (spec default `unsettled`), ticker, event_ticker, exchange_index | walker; cursor `cursor`; TWO item keys under one cursor (`market_positions`, `event_positions`); identity `market_positions.ticker`; limit 1..1000; MaxPages 1000 | `market_positions[]`: ticker, position_fp (fallback position) (`read.go:441-473`). `event_positions[]`: event_ticker, event_exposure_dollars, read only by `AccountPositions` (`account_positions.go:118-139`). Not read: exchange_index, total_traded_dollars, market_exposure_dollars, realized_pnl_dollars, fees_paid_dollars, last_updated_ts | as `Orders` |
| GET | `/portfolio/balance` | `(*Client).Balance`; `(*Client).MarketFunding` | `harness/rest/read.go:766` (request `:767`); `harness/rest/funding.go:31` (request `:58-61`) | `Balance`: none. `MarketFunding`: `subaccount=0` and `exchange_index=N` | n/a | `Balance`: `balance` as int64 cents, null refused (`read.go:782-799`). `MarketFunding`: balance, balance_dollars, updated_ts, balance_breakdown[].exchange_index/balance, cross-checked (`funding.go:74-119`). Not read: portfolio_value | none; 10 s |
| GET | `/portfolio/subaccounts/balances` | `(*Client).SubaccountNumbers` | `harness/rest/account_positions.go:18` (request `:19`; path const `:13`) | none | n/a; a `cursor` in the answer is refused as "completeness unknown" (`:34-36`) | subaccount_balances[].subaccount_number (0..63) and exchange_index; requires primary 0 present (`:37-73`). Balances not read | none; 10 s |
| GET | `/markets/{ticker}` | `(*Client).Schedule`; also `MarketFunding` | `harness/rest/schedule.go:187` (request `:196`); `funding.go:35` | path ticker, validated to `[A-Za-z0-9._-]` (`schedule.go:232-260`) | n/a | `Schedule`: market.ticker (identity check), status, result, can_close_early (required), close_time (`schedule.go:277-368`). `MarketFunding`: market.ticker, market.exchange_index (`funding.go:42-57`). 6 of 54 spec Market fields. Status accepted: `active`, and `finalized` with a result (`schedule.go:405-457`); any other status fails the read unless a `result` is present | none; 10 s. Polled every 30 s per market (`harness/cfg/params.go:138`) |
| GET | `/markets/{ticker}/orderbook` | `(*Client).Orderbook` | `harness/rest/orderbook.go:146` (request `:153-155`) | path ticker only. The spec `depth` (0..100) is never sent, so every call fetches the full book | n/a | `orderbook_fp.yes_dollars` / `no_dollars` as [price, size] string pairs (`orderbook.go:172-228`). Asserts integer-cent prices and strict ordering, reverses to best-first (`orderbook.go:244-312`); a sub-cent level yields outcome `OrderbookGranularity`, not a usable book (`orderbook.go:216-226`) | none; 10 s |
| GET | `/incentive_programs` | `(*Client).Programs` (signed, walked); `feed.Universe` (unsigned, one page) | `harness/rest/read.go:707`; `client.go:185-196`; `feed/universe.go:55-134` | `Programs`: status=active, type=liquidity (`read.go:709-710`), limit=1000, cursor. `Universe`: status=active, limit=200, no cursor (`universe.go:56`) | walker; cursor field **`next_cursor`**; key `incentive_programs`; limit 1..5000 (spec max 10000); MaxPages 1000. `Universe` does not paginate and truncates at 200 (`universe.go:44-47`) | market_ticker, target_size_fp (`read.go:716-751`; `universe.go:93-98`). 2 of 12 spec fields | none. `Programs` 10 s; `Universe` 20 s per Read/Write (`universe.go:16-35,68-78`) |

### 2.1 Transport facts that apply to every row

- Seam: `Doer` is an HTTP-level interface (`harness/rest/client.go:67-69`). A non-2xx status is a `Response`, never an `error`; `error` means "outcome unknown" (`client.go:35-69`). `NotSent` and `WriteRefused` mark requests that provably never left the process (`client.go:86-108`).
- Signing: every request is signed, including the three the spec marks public (`/markets/{ticker}`, `/markets/{ticker}/orderbook`, `/incentive_programs`). Message is `timestamp_ms + METHOD + "/trade-api/v2" + path` with no query string; RSA-PSS, SHA-256, salt length = hash length (`client.go:329-386`, `feed/auth.go:164-179`). Headers `KALSHI-ACCESS-KEY`, `KALSHI-ACCESS-TIMESTAMP`, `KALSHI-ACCESS-SIGNATURE`.
- No retry loop in the transport (`client.go:257-262`); redirects are refused so a 307/308 cannot double a write (`client.go:315-317`).
- 429 handling: `RateLimitError` carries `Retry-After` if present and a `HasDelay` flag (`harness/rest/throttle.go:16-62`). Kalshi sends no `Retry-After` (docs `rate_limits.md`), so `HasDelay` is false in practice and the caller owns backoff; no REST method sleeps (`throttle.go:13-15`). There is no token-bucket accounting in `rest`.
- Write guard: `WriteGuard` wraps any `Doer` and refuses every non-GET unless `-live` and a sentinel file are both present, re-checked per write (`harness/rest/guard.go:73-167`).
- Client order ids must look like `lipH-{run}-{idx:03d}-{side}-{seq:08d}` (`harness/rest/coid.go:22-65,110-141`); `CreateOrder.validate` refuses anything else (`harness/rest/wire.go:207-236`).

### 2.2 The pagination walker (`harness/rest/page.go:104-213`)

- It is the only list reader: there is no single-page read (`page.go:17-18`). `Endpoint` is data: cursor field name, item keys, identity field per key, MinLimit/MaxLimit/PageLimit, MaxPages (`client.go:124-151`). A new list endpoint needs a new `Endpoint` value with measured contract values, not new walker code.
- Guards: `ValidateLimit` refuses a limit outside the measured range (`client.go:200-206`); a cursor is sent exactly as received (`client.go:430-451`); a repeated first-record identity or a repeated cursor ends the walk as `WalkRewound` and discards the records (`page.go:173-210`, `:217-228`); an absent or null cursor key or item array is a malformed response, not an empty page (`page.go:244-337`); more than `MaxPages` abandons the walk (`page.go:117-136`).
- Result type: `Walk.Replaces()` is true only for `WalkComplete`; the zero value is `WalkUnset`, which licenses nothing (`page.go:29-93`).
- Not in the walker: any sleep, retry, per-page deadline beyond the HTTP timeout, or server-side time filter (`min_ts`/`max_ts`).

## 3. WebSocket implemented

There are two websocket clients. `harness/wsx` is the harness's (supervised, keepalive, gate). `feed.Conn` is the shadow rig's (hash-pinned, deliberately minimal). Both use `github.com/coder/websocket v1.8.15` (`go.mod`). In section 3.1 a bare `wire.go`, `session.go`, `supervisor.go`, `transport.go`, `frame.go` or `gate.go` means the file under `harness/wsx/`.

### 3.1 `harness/wsx`

| facet | implementation | file:line |
| - | - | - |
| URL | `wss://external-api-ws.kalshi.com/trade-api/ws/v2`: const `feed.WSURL`, re-exported as `wsx.WSURL`, copied into the private `Supervisor.url`; there is no setter. A custom `Dialer` receives the URL as an argument and can ignore it | `feed/auth.go:32`; `harness/wsx/transport.go:245-248`; `harness/wsx/supervisor.go:86,310` |
| Authentication | Signed upgrade request. `Supervisor.dial` calls `Signer.WSHeaders(wallMs)`, which signs `ts_ms + "GET" + "/trade-api/ws/v2"` with RSA-PSS/SHA-256 and returns the three `KALSHI-ACCESS-*` headers; they are copied into the `http.Header` handed to `Dial`. Re-signed on every dial. 10 s bound on the handshake. Redirects refused. No post-connect auth step exists in the API | `supervisor.go:291-313`; `feed/auth.go:164-183`; `transport.go:147-169` |
| Channels subscribed | `orderbook_delta` with `market_tickers` = the managed universe (**per-ticker**); `trade` with no filter (**whole-exchange tape**). Command ids are fixed 1 and 2. Both are written or the dial fails. Ticker list must be non-empty and duplicate-free. No `use_yes_price`, `send_initial_snapshot` or `skip_ticker_ack` is ever sent | `harness/wsx/wire.go:17-45,78-97`; `supervisor.go:315-336` |
| Message types decoded by wsx | `InspectFrame` classifies `orderbook_snapshot`, `orderbook_delta`, `trade`, `error`, and the `subscribed` ack for command id 1 only (to capture the book `sid`); everything else, including `ok` and `unsubscribed`, is `FrameOther` and passed on. wsx decodes only what it needs to validate: `market_ticker` and the price strings for the integer-cent assertion. `fill`, `user_order`, `ticker`, `market_position`, lifecycle messages are never subscribed and never decoded | `harness/wsx/frame.go:98-148`; `wire.go:109-129` |
| Full decode | Done by `core.Rig.Handle` (outside scope): envelope `type/sid/seq/msg`; snapshot (`market_ticker`, `ts_ms`, `yes_dollars_fp`, `no_dollars_fp`); delta (`market_ticker`, `ts_ms`, `side`, `price_dollars`, `delta_fp`); trade (`trade_id`, `market_ticker`, `ts_ms`, `count_fp`, `yes_price_dollars`, `no_price_dollars`, `taker_side`); `error` is only logged | `core/rig.go:226-256,259-302` |
| Error frames | Classified as `FrameError`, raised as SEV2 `WS_ERROR_FRAME` with a 200-byte snippet. The numeric error codes (1-28 in the AsyncAPI) are not interpreted: no reaction differs by code (for example code 25 buffer overflow, 26 market limit, 27 command rate limit) | `frame.go:126-132` |
| subscribe | Only at connect, inside `dial` | `supervisor.go:329-336` |
| unsubscribe | **Absent.** No code in `go/` builds it | grep of non-test `go/` |
| list_subscriptions | **Absent** | grep of non-test `go/` |
| update_subscription | `action: get_snapshot` only, for exactly one positive book `sid`; sent from the session loop only when the command's sid equals the sid learned on THIS socket. `add_markets` and `delete_markets` are **absent**: adding a market advances a revision, which ends the session and re-dials with the enlarged ticker list | `wire.go:47-64`; `session.go:271-292`; `supervisor.go:94-129`; `session.go:268-269` |
| Ping/pong | Server pings (every 10 s, body `heartbeat`, docs `connection-keep-alive.md`) are answered by the library inside its read loop (per code comments). Client side: own ping every `ping_interval_s` (default 10 s), `pong_timeout_s` (5 s) without a pong ends the session, and a separate `read_deadline_s` (60 s) backstop is reset only by delivered data frames, never by pongs. Parameter relations validated | `session.go:121-136,162-164,236-266`; `harness/cfg/params.go:149-151,285-292` (outside scope); `transport.go:36-37` |
| Reconnect | `Supervisor.Run` never returns because of a socket. Abnormal disconnect: backoff ladder 1, 2, 4, 8, 16, 32, 60 s then 60 s forever. Clean close (1000/1001): reconnect immediately. A rejected handshake (for example 401) is classified as `HandshakeError` but retried like any other failure. After 60 s down, `EventDisconnectReduce` is emitted once per outage | `supervisor.go:20-33,35-42,161-239,247-284`; `transport.go:205-240` |
| Resubscribe | Every dial re-sends both subscriptions with the CURRENT ticker set, and the exchange answers with fresh snapshots. Each connect advances a generation that quarantines every book until its snapshot arrives | `supervisor.go:291-338`; `harness/wsx/gate.go:343-386,421-480` |
| Sequence-gap detection | NOT in wsx. wsx decodes `seq` into the envelope but never reads it (`envelope.Seq`, no uses). The detector is `core.Rig.Handle` (outside scope): per `sid`, `seq != prev+1` marks every book stale, drops the frame and sets `NeedsResnapshot`; the first frame on a `sid` is never a gap; `ResetOnReconnect` clears the map because sids may be renumbered. The harness relays the flag to `Gate.NoteSeqGap`, which quarantines every market and requests a `get_snapshot`; the gap is subscription-wide because nothing says which market lost a delta | `harness/wsx/wire.go:112`; `core/rig.go:265-289,175-186`; `cmd/harness/run.go:1324-1330` (outside scope); `harness/wsx/gate.go:711-739` |
| Other | Read limit 1 MiB, inherited from the Python rig (the server's own limit is 64 MiB per changelog 2026-09-24). permessage-deflate with context takeover is always offered, again for parity with the Python rig; the server now treats compression as optional (changelog 2026-09-24), so an uncompressed session is possible. Distinct `http.Transport` from REST by design | `transport.go:116-130,159,167` |

### 3.2 `feed.Conn` (shadow rig; hash-pinned)

- Same URL, same signer, same two subscriptions (`feed/feed.go:23-38,115-182`).
- Deliberately NO client-side keepalive and NO read deadline (`feed.go:103-114`). A handshake rejection is fatal here (`feed.go:54-71,142-156`), unlike `wsx`.
- `Resnapshot(sids, tickers)` writes `update_subscription get_snapshot` for every live subscription (`feed.go:40-52,188-196`).
- No reconnect or sequence logic in the package: `Pump` reads frames to a channel and returns the error (`feed.go:218-248`); reconnect lives in the caller (`cmd/rig/main.go:466,475`, outside scope).
- `feed/universe.go` is the unsigned `GET /incentive_programs` row in section 2.

## 4. Order features checklist

In section 4 a bare `wire.go`, `write.go`, `cancel.go`, `coid.go` or `read.go` means the file under `harness/rest/`. Spec: `CreateOrderV2Request` at `openapi.yaml:8473`; the client's wire struct is `createOrderWire` at `harness/rest/wire.go:143-161`. The client's public intent type `CreateOrder` has no setter for wire policy (`wire.go:109-116`); the constants are set in `wire()` (`wire.go:244-259`) and re-checked by `validateWire` (`wire.go:267-305`) before the payload is frozen (`write.go:170-186`).

| feature | spec (V2) | client status | file:line |
| - | - | - | - |
| ticker | required string | supported; only "non-empty" is checked on create | `wire.go:196-198` |
| side bid/ask on YES | required enum `bid`, `ask` | supported. Callers express a bid on YES or on NO; `ToWire` maps YES bid to `bid` at p and NO bid to `ask` at 100-p. No other vocabulary | `wire.go:44-53,251` |
| count | required fixed-point string, 0-2 decimals, 0.01 granularity | supported as `"%.2f"`; must be > 0 and, when a bound is given, <= it | `wire.go:146-147,252,239`; `harness/num/qty.go:133,162-171` (outside scope) |
| price | required fixed-point dollars string, must lie on the market's price grid | supported ONLY as whole cents 1..99 formatted `"0.xx00"`; sub-cent grid prices are not expressible | `wire.go:44-53,315-317`; `harness/quote/cross.go:13-14,34` (outside scope) |
| time_in_force | required enum `fill_or_kill`, `good_till_canceled`, `immediate_or_cancel` | **hard-coded `good_till_canceled`**; `validateWire` refuses any other value. FOK and IOC absent | `wire.go:164,254,279-281` |
| post_only | optional bool | **hard-coded `true`**; `validateWire` refuses false. The client cannot send a taker order | `wire.go:256,268-272` |
| self_trade_prevention_type | required enum `taker_at_cross`, `maker` | **hard-coded `taker_at_cross`**; `maker` is refused | `wire.go:165,255,273-278` |
| client_order_id | optional string | supported but constrained: must parse as a `lipH-...` coid that rebuilds byte-identically and whose embedded side matches the order; market index <= 999, sequence <= 99,999,999. Arbitrary or foreign ids are refused | `wire.go:207-236`; `coid.go:52-65,110-141` |
| expiration_time (legacy name `expiration_ts`) | optional int, seconds; GTC + expiry; cannot combine with IOC | **absent** | none |
| reduce_only | optional bool; accepted only with IOC | **absent** | none |
| buy_max_cost, sell_position_floor | NOT in `CreateOrderV2Request`. They exist only in the legacy `CreateOrderRequest` schema (`openapi.yaml:8174`, fields at `:8245-8254`), which no operation references | not applicable; never sent | spec only |
| cancel_order_on_pause | optional bool; cancels the order when trading pauses | **absent** | none |
| subaccount | optional int | **absent** (omitted, so primary 0) | none |
| order_group_id | optional string | **absent** | none |
| exchange_index | optional int; `>= 0` routes directly, `-1` or omitted with a ticker auto-routes. Docs: auto-routing "will incur an additional latency cost" and bills every shard's Write bucket (`exchange_sharding.md`, `rate_limits.md`) | **absent**: every create is auto-routed, and there is no way to pin the shard. New tennis events live on shard 3 (section 7, item 1) | none |
| cancel one order | `DELETE /portfolio/events/orders/{order_id}` with optional subaccount, exchange_index, market_ticker | supported with `market_ticker` routing; no subaccount or exchange_index | `cancel.go:106-211` |
| cancel all | `DELETE /portfolio/events/orders` | **absent** (the harness cancels per id and verifies) | none |
| amend | `POST .../{order_id}/amend` | **absent** | none |
| decrease | `POST .../{order_id}/decrease` (reduce_by or reduce_to) | **absent** | none |
| batch create | `POST /portfolio/events/orders/batched` | **absent** | none |
| batch cancel | `DELETE /portfolio/events/orders/batched` | **absent** | none |
| get order by id | `GET /portfolio/orders/{order_id}` | partial: unexported helper used only to confirm a lagging cancel; no public getter | `cancel.go:302-341` |
| list orders | `GET /portfolio/orders` | supported (ticker, status only) | `read.go:237-274` |
| queue position | `GET /portfolio/orders/queue_positions`, `GET /portfolio/orders/{order_id}/queue_position` | **absent** | none |

Create-outcome semantics worth knowing: 200 or 201 with an exactly-matching ack is `ACKED`; `409 order_already_exists` is a positive identification followed by a confirming complete orders walk; a 429 returns `UNKNOWN` with `MaxLive` kept at the requested size; a definite 4xx is `REJECTED` with `error.code` in `RejectReason`; everything else is `UNKNOWN` and retried with the same coid (`write.go:27-124,254-333`).

## 5. In the Kalshi API but not implemented

All of section 5 was computed from the spec fetched today (section 0). Nothing here is "unverified today".

### 5.1 REST: all 106 absent operations

Class N and U operations, listed individually (33). `operationId` is the spec's.

| class | method | path | operationId | tag | why it matters to a generic client |
| - | - | - | - | - | - |
| N | GET | `/exchange/status` | GetExchangeStatus | exchange | `trading_active`/`exchange_active` plus per-shard `exchange_index_statuses`. The client has no read of exchange state; a pause is inferred from failures |
| N | GET | `/markets` | GetMarkets | market | Market discovery and universe selection (status, series, event, time filters; cursor `cursor`). The client can only fetch a market whose ticker it already knows |
| N | DELETE | `/portfolio/events/orders` | CancelAllOrders | orders | One-call kill switch across all shards; now costs the same as one cancel (changelog 2026-09-03) |
| N | POST | `/portfolio/intra_exchange_instance_transfer` | IntraExchangeInstanceTransfer | portfolio | Moves collateral between exchange shards. Docs: "Programmatic traders must preallocate collateral on a given exchange shard before order placement". Tennis is on shard 3 (section 7, item 1); the client can read per-shard cash (`harness/rest/funding.go:31-122`) but cannot move it |
| N | GET | `/portfolio/settlements` | GetSettlements | portfolio | Settlement and realized-P&L accounting; `/portfolio/positions` defaults to unsettled only |
| U | GET | `/account/limits` | GetAccountApiLimits | account | Tier and read/write token budgets. The client's write pacing is a static knob (`harness/cfg/params.go:133-134`, outside scope) |
| U | GET | `/account/endpoint_costs` | GetAccountEndpointCosts | account | Authoritative non-default token costs |
| U | GET | `/events` | GetEvents | events | Event-to-markets structure |
| U | GET | `/events/{event_ticker}` | GetEvent | events | Same |
| U | GET | `/events/fee_changes` | GetEventFeeChanges | events | Fee-schedule changes; fees decide maker versus taker viability |
| U | GET | `/exchange/schedule` | GetExchangeSchedule | exchange | Maintenance windows (Thursday 03:00-05:00 ET trading pause, docs `maintenance_and_pauses.md`) |
| U | GET | `/exchange/user_data_timestamp` | GetUserDataTimestamp | exchange | Freshness of the REST portfolio endpoints |
| U | GET | `/series/fee_changes` | GetSeriesFeeChanges | exchange | Fee-schedule changes |
| U | GET | `/markets/orderbooks` | GetMarketOrderbooks | market | Multi-market REST book in one call |
| U | GET | `/markets/trades` | GetTrades | market | Public trade history and tape backfill |
| U | GET | `/series` | GetSeriesList | market | Series discovery |
| U | GET | `/series/{series_ticker}` | GetSeries | market | Series terms and fee model |
| U | GET | `/portfolio/order_groups` | GetOrderGroups | order-groups | Exchange-side rolling contract limit with auto-cancel (risk control) |
| U | POST | `/portfolio/order_groups/create` | CreateOrderGroup | order-groups | Same family (7 operations) |
| U | DELETE | `/portfolio/order_groups/{order_group_id}` | DeleteOrderGroup | order-groups | Same |
| U | GET | `/portfolio/order_groups/{order_group_id}` | GetOrderGroup | order-groups | Same |
| U | PUT | `/portfolio/order_groups/{order_group_id}/limit` | UpdateOrderGroupLimit | order-groups | Same |
| U | PUT | `/portfolio/order_groups/{order_group_id}/reset` | ResetOrderGroup | order-groups | Same |
| U | PUT | `/portfolio/order_groups/{order_group_id}/trigger` | TriggerOrderGroup | order-groups | Same |
| U | DELETE | `/portfolio/events/orders/batched` | BatchCancelOrdersV2 | orders | Cancel many in one round trip (billed per item, so it saves latency, not tokens: docs `rate_limits.md`) |
| U | POST | `/portfolio/events/orders/batched` | BatchCreateOrdersV2 | orders | Place many in one round trip |
| U | POST | `/portfolio/events/orders/{order_id}/amend` | AmendOrderV2 | orders | Requote in place; an expiry-only amend preserves queue position (`openapi.yaml:8668`) |
| U | POST | `/portfolio/events/orders/{order_id}/decrease` | DecreaseOrderV2 | orders | Shrink without losing queue priority |
| U | GET | `/portfolio/orders/queue_positions` | GetOrderQueuePositions | orders | Queue position for all resting orders; core to maker/scalper strategies |
| U | GET | `/portfolio/orders/{order_id}/queue_position` | GetOrderQueuePosition | orders | Same, one order |
| U | GET | `/portfolio/summary/total_resting_order_value` | GetPortfolioRestingOrderTotalValue | portfolio | Resting exposure |
| U | GET | `/portfolio/target_balance_allocation` | GetTargetBalanceAllocation | portfolio | Opt-in auto-rebalancing of cash across shards (alternative to manual transfers) |
| U | POST | `/portfolio/target_balance_allocation` | SetTargetBalanceAllocation | portfolio | Same |

Class S (73), grouped by spec tag; every operationId is listed so the union with the table above is exactly the 106 absent operations:

- account (2): UpgradeAccountApiUsageLevel, GetAccountApiUsageLevelVolumeProgress
- api-keys (4): GetApiKeys, CreateApiKey, GenerateApiKey, DeleteApiKey
- communications (18; RFQ, quotes, block trades): GetBlockTradeProposals, ProposeBlockTrade, AcceptBlockTradeProposal, GetCommunicationsID, GetQuotes, CreateQuote, DeleteQuote (deprecated), GetQuote (deprecated), AcceptQuote (deprecated), ConfirmQuote (deprecated), GetRFQs, CreateRFQ, DeleteRFQ, GetRFQ, DeleteRFQQuote, GetRFQQuote, AcceptRFQQuote, ConfirmRFQQuote
- events (4): GetMultivariateEvents, GetEventMetadata, GetMarketCandlesticksByEvent, GetEventForecastPercentilesHistory
- fcm (10; FCM members only): GetFCMFills, GetFCMOrders, GetFCMPositions, ListFCMSubtraders, CreateFCMSubtrader, GetFCMSubtraderBlockedCategories, UpdateFCMSubtraderBlockedCategories, DeleteFCMEventContractDailyCap, GetFCMEventContractDailyCap, UpdateFCMEventContractDailyCap
- historical (8; archive and backtest data): GetHistoricalCutoff, GetFillsHistorical, GetHistoricalMarkets, GetHistoricalMarket, GetMarketCandlesticksHistorical, GetHistoricalOrders, GetHistoricalPositions, GetTradesHistorical
- live-data (7; strategy-conditional, for example a sports-score feed for a tennis strategy): GetLiveDatas, GetEventLiveData, GetLiveDataByMilestone, GetGameStats, GetWeatherIndex, GetWeatherIndexCalibrations, GetLiveData
- market (2; candlesticks): BatchGetMarketCandlesticks, GetMarketCandlesticks
- milestone (2): GetMilestones, GetMilestone
- multivariate (3): GetMultivariateEventCollections, GetMultivariateEventCollection, CreateMarketInMultivariateEventCollection
- portfolio (9; cash history, subaccounts, transfer history): GetDeposits, GetWithdrawals, CreateSubaccount, GetSubaccountNetting, UpdateSubaccountNetting, ApplySubaccountTransfer, GetSubaccountTransfers, GetIntraExchangeInstanceTransfers, GetIntraExchangeInstanceTransfer
- search (2): GetFiltersForSports, GetTagsForSeriesCategories
- structured-targets (2): GetStructuredTargets, GetStructuredTarget

Counts check: 33 + 73 = 106; 106 + 11 implemented = 117 operations in the spec.

Other API surfaces that are NOT in `openapi.yaml` and were not inventoried in detail (separate products or transports; all absent from lip): the **FIX 5.0 SP2 API** (FIXT.1.1; order entry, market data, drop copy, RFQ; hosts such as `mm.fix.elections.kalshi.com:8228`; docs `fix/connectivity.md`; one connection per API key; REST and FIX drain the same token buckets); the **CF Benchmarks REST passthrough** (`GET /trade-api/v2/cfbenchmarks/...`, 50 read tokens per call, entitlement required); the **Perps (margin) exchange** (51 REST pages, 8 WebSocket pages, its own FIX, three separate spec files). For the event-contract client under evaluation these are class S, except that FIX is the documented lower-latency alternative to REST for order entry. Kalshi publishes Python and TypeScript SDKs only (no Go SDK) and tells active traders to treat `openapi.yaml` and `asyncapi.yaml` as the source of truth and to generate their own client (`sdks/overview.md`).

### 5.2 WebSocket channels and commands (AsyncAPI 2.0.0)

`ticker_v2` is not a channel: it does not appear in `asyncapi.yaml`, `llms.txt` or any of the 28 fetched pages. The channel list below is the spec's complete set.

| channel | auth / filter per docs | seq in message | implemented | class | note |
| - | - | - | - | - | - |
| `orderbook_delta` | private; market filter required | yes | YES (per-ticker) | N | snapshot first, then deltas; `get_snapshot`, `add_markets`, `delete_markets` supported by the exchange |
| `trade` | public; market filter optional | yes | YES (unfiltered: whole-exchange tape) | U | the exchange supports `market_tickers` here; lip subscribes to all by design (`harness/wsx/wire.go:8-16`) |
| `ticker` | public; filter optional | **no** | no | U | price, bid/ask, volume, open interest; lighter than a book |
| `fill` | private; filter optional | **no** | no | N | real-time fills with `post_position_fp`; lip polls `GET /portfolio/fills` instead (`harness/wsx/portfolio.go:166`; default cycle 5 s, `harness/cfg/params.go:136`) |
| `user_orders` | private; filter optional | **no** | no | N | created/updated/canceled orders in real time; lip polls `GET /portfolio/orders` |
| `market_positions` | private; filter optional | **no** | no | U | real-time position changes |
| `market_lifecycle_v2` | public; **no ticker filter supported** | yes | no | N | open/close/determination/settlement, `is_deactivated`, `price_level_structure` and `price_ranges` changes; lip instead polls `GET /markets/{ticker}` every 30 s |
| `multivariate_market_lifecycle` | public | yes | no | S | combo markets |
| `communications` | private | yes | no | S | RFQ and quote events |
| `order_group_updates` | private | yes | no | U | only if order groups are adopted |
| `cfbenchmarks_value` | private (channel requires auth); `index_ids` | yes | no | S | index values |
| `cfbenchmarks_value_5hz` | private; `index_ids` required | yes | no | S | index values at up to 5 Hz |
| `pyth_value` | private; `underlying_tickers`; paid feed (docs: $1,000/month) | yes | no | S | Pyth prices |

Facts that follow from the table: of the 24 data message types, exactly four carry no top-level `seq` (`ticker`, `fill`, `market_position`, `user_order`), so the private state channels have no gap detection in the protocol and need REST reconciliation. Commands in the spec not used by lip: `unsubscribe`, `list_subscriptions`, `update_subscription` with `add_markets` or `delete_markets` (and the CF Benchmarks and Pyth variants). Subscribe parameters never sent: `market_ticker` (single), `market_id(s)`, `send_initial_snapshot`, `skip_ticker_ack`, `use_yes_price`, `user_filter`, `shard_factor`, `shard_key`, `index_ids`, `underlying_tickers`. Server error codes 25 (subscription buffer overflow), 26 (per-subscription market limit; the AsyncAPI gives no number, the changelog of 2026-06-18 says 500k market subscriptions per session and 10k commands per second) and 27 (command rate limit) have no distinct handling.

## 6. Environment

| item | state | file:line |
| - | - | - |
| REST base URL | `https://api.elections.kalshi.com` plus `/trade-api/v2`. This is the SHARED host the docs call "also supported"; the docs recommend the dedicated `https://external-api.kalshi.com/trade-api/v2` for API traders. `cmd/incentives` (outside scope) already uses the dedicated host | `feed/auth.go:31`; `harness/rest/client.go:255,320`; `feed/universe.go:56`; `cmd/incentives/main.go:25` |
| WebSocket URL | `wss://external-api-ws.kalshi.com/trade-api/ws/v2` (the dedicated host, as recommended) | `feed/auth.go:32-33` |
| Demo environment | **No switch anywhere.** The spec lists demo REST hosts `external-api.demo.kalshi.co` and `demo-api.kalshi.co` (`openapi.yaml:12-14`) and demo WS `wss://external-api-ws.demo.kalshi.co/trade-api/ws/v2`; demo credentials are separate from production (docs `api_environments.md`, `demo_env.md`). In code: REST host is the exported field `HTTPDoer.Host`, assignable after construction but never assigned in `go/`; the WS URL is a const copied into a private field with no setter | `client.go:263-270,320`; `supervisor.go:86` |
| Credentials | `~/.kalshi/kalshi.pem` and `~/.kalshi/env` (`KALSHI_API_KEY_ID`) by default, or explicit paths via `NewSignerFrom`; `cmd/harness` takes `paths.key` and `paths.env` from its config with no default. **RSA only**: the loader accepts PKCS#1 or PKCS#8 RSA keys and signing is RSA-PSS; an Ed25519 key is refused (section 7, item 3) | `feed/auth.go:12-13,43-74,135-152,164-179`; `cmd/harness/config.go:140,467-469`, `cmd/harness/runtime.go:290` (outside scope) |
| REST timeout | a constructor argument. Production: 10 s (rationale: three sequential poll walks must fit a 60 s freshness budget). Tooling: `cmd/accountcheck` 15 s. `feed.Universe`: 20 s per Read/Write | `client.go:276-323`; `cmd/harness/runtime.go:53-66,354`; `cmd/accountcheck/main.go:532`; `feed/universe.go:16-35` |
| Production HTTP transport | separate transports for REST and WS (a pool fault must not take both); both resolve through a cached dialer (`netx.CachedDialer`); HTTP/2 attempted, 100 idle conns, 90 s idle timeout, 10 s TLS handshake | `client.go:280-295`; `transport.go:132-145`; `cmd/harness/runtime.go:223-232,396` (outside scope) |
| WebSocket timers | handshake 10 s; ping 10 s; pong timeout 5 s; read deadline 60 s; quiet 60 s; disconnect-reduce 60 s; reconnect ladder 1..60 s | `supervisor.go:307,20-33`; `harness/cfg/params.go:149-153` (outside scope) |
| Rate-limit config | none tier-aware. Static dispatcher knobs `write_rate` 5/s and `write_burst` 10; Basic tier is 100 write tokens/s and 200 read tokens/s with 10 tokens per order and 2 per cancel (docs `rate_limits.md`) | `harness/cfg/params.go:53-54,133-134` (outside scope) |

## 7. Hazards and spec-versus-code discrepancies a generic client would hit

1. **Error-body shape.** The client reads a nested `error.code` (`harness/rest/write.go:515-525`; observed live body `{"error":{"code":"order_already_exists",...}}` in `notes/harness-spec.md:269,792` and `harness/rest/write_test.go:39`). The OpenAPI `ErrorResponse` is flat `{code, message, details}` (`openapi.yaml:4948`), and the documented 429 body is `{"error": "too many requests"}` (a string). If Kalshi moves to the documented flat shape the 409 dedupe stops being recognised (`write.go:282`), falls to the uncharacterised-409 branch (`write.go:291-299`), and `RejectReason` is empty. That fails safe (UNKNOWN) but loses positive identification.
2. **`use_yes_price` default will flip.** The AsyncAPI says the default for no-side `orderbook_delta`/`orderbook_snapshot` pricing "will be flipped to `true` in a future release" and the flag then removed (`asyncapi.yaml:2419-2430`). lip never sends the flag (`harness/wsx/wire.go:31-37`), and its book decoder assumes no-leg pricing for the no side (`core/rig.go:361`, outside scope). Unpinned wire semantics.
3. **Sub-cent price grids.** The docs (fixed-point page, "Last Updated: August 20, 2026") list many price-level structures (`deci_cent`, `tapered_deci_cent`, `center_deci_edge_centi_cent` and more) and say `price_ranges` on the Market is the source of truth. lip places only whole cents 1..99 (valid on every grid) and treats any fractional book price as a tick-size change: frames are refused and the market is quarantined and sent to REDUCING (`harness/wsx/frame.go:164-168,189-193`, `gate.go:566-577`; REST book at `orderbook.go:216-226`). On a sub-cent market the client can place at whole cents but cannot read the book.
4. **Market `status` vocabulary.** The spec enum has 8 values (initialized, inactive, active, closed, determined, disputed, amended, finalized). `Schedule` accepts `active` and `finalized`+result; any other status without a `result` fails the read with SEV1 `MARKET_STATUS_UNKNOWN` (`schedule.go:405-457`). Only 6 of the 54 Market fields are read; `price_level_structure`, `price_ranges`, `open_time`, bid/ask and volume are not.
5. **Fills are walked in full, every cycle.** `GET /portfolio/fills` supports `min_ts`; the client never sends it and filters client-side after a complete walk (`read.go:537-574`), and the poller walks all fills every cycle (`harness/wsx/portfolio.go:166`). Cost grows with account history.
6. **Subaccount/shard defaults differ by endpoint.** `Orders` and `Fills` send no `subaccount` or `exchange_index`, so per the spec they cover all subaccounts and shards; `Positions` without `subaccount` covers subaccount 0 only, which is why `AccountPositions` exists (`account_positions.go:82-153`). Creates and cancels cannot target a subaccount.
7. **Trading pauses.** Thursday 03:00-05:00 ET is a trading pause (cancels allowed, placement and amend not) and resting orders stay unless `cancel_order_on_pause` was set (docs `maintenance_and_pauses.md`). lip cannot set that flag and does not read `/exchange/status`.
8. **Create success code.** The spec documents 201; observed live was 200 (`notes/harness-redteam.md:493`). The client accepts exactly 200 and 201 and treats any other 2xx as UNKNOWN (`write.go:254,271-281`).
9. **Legacy fields.** `side`/`action` on Order and Fill and `taker_side` on Trade are deprecated; the spec says "will not be removed before May 14, 2026" (`openapi.yaml:6454,6461,6809`), the docs page says May 28, 2026; both dates have passed. The REST readers prefer `book_side` (`read.go:339-366`); `core.Rig` still reads `taker_side` (`core/rig.go:255`, outside scope).
10. **Read-side fallbacks the spec no longer lists.** The client falls back to `market_ticker` on Order, `remaining_count`, `count` and `position`, which are not in the 3.32.0 schemas (`read.go:286,323-325,456-458,630-633`). Harmless fallbacks, listed so they are not mistaken for current fields.

## 8. The five most important absent features for a generic trading client

1. **Order flexibility.** Only post-only, good-till-canceled, taker_at_cross bids at whole cents with a `lipH-` coid. No IOC/FOK/taker, expiration, reduce_only, cancel_order_on_pause, subaccount, order group, or sub-cent price.
2. **Order management.** No amend, decrease, batch create, batch cancel, cancel-all, or queue-position endpoints.
3. **Private and lifecycle websocket channels.** No `fill`, `user_orders`, `market_positions`, `ticker`, `market_lifecycle_v2`; no `unsubscribe`, `add_markets` or `delete_markets` (adding a market reconnects the socket). Fills and orders are discovered by REST polling.
4. **Discovery and exchange state.** No `GET /markets`, events, series, trades, candlesticks, settlements, `/exchange/status` or `/exchange/schedule`; `GET /markets/{ticker}` reads 6 of 54 fields and refuses most statuses; the REST book has no `depth`.
5. **Environment and limits.** No demo switch, shared (not dedicated) REST host, no tier-aware rate limiting (`/account/limits`), no unpinned-semantics protection (`use_yes_price`).
