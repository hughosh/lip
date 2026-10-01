# Delegate: REST + websocket feature inventory vs the Kalshi public API

Scope (read-only): `/Users/hugh/kek/lip/go/harness/rest/*.go` (non-test),
`/Users/hugh/kek/lip/go/harness/wsx/*.go` (non-test), `/Users/hugh/kek/lip/go/feed/*.go`.
Plus the Kalshi API reference online: try `https://docs.kalshi.com/openapi.yaml` (WebFetch or
`curl -sL`), and the docs pages under `https://docs.kalshi.com/`. The marketing site
`kalshi.com` answers 429 to tools; if docs.kalshi.com also fails, say so explicitly and fall
back to the facts below. Do not modify anything except the output file.

Facts already verified in this project (use, do not re-derive): write path is
`POST /trade-api/v2/portfolio/events/orders` and `DELETE /trade-api/v2/portfolio/events/orders/{id}`;
`CreateOrderV2Request` requires `{ticker, side(bid|ask on YES), count(string), price(string),
time_in_force, self_trade_prevention_type}`, optional `post_only`, `client_order_id`; dedupe on
client_order_id returns 409 `order_already_exists`; GET /portfolio/{orders,fills,positions}
paginate with `cursor`, /incentive_programs with `next_cursor`; the portfolio family silently
rewinds to page 1 on a bad cursor; ws URL `wss://external-api-ws.kalshi.com/trade-api/ws/v2`,
channel `market_lifecycle_v2` has no ticker filter; rate limits are a token bucket (basic 100
write tokens/s, order 10 tokens, cancel 2); 429 carries no Retry-After.

Task:
1. REST endpoints IMPLEMENTED. One row per endpoint: | method | path | Go function | file:line |
   params supported | pagination (walker? limits?) | response fields parsed | retry/timeout |.
2. Websocket IMPLEMENTED: URL, how the connection is authenticated, channels subscribed and
   whether per-ticker or all, message types decoded (snapshot, delta, trade, fill, error, ...),
   subscribe/unsubscribe/update_subscription support, ping/pong, reconnect and resubscribe
   logic, sequence-number gap detection. Cite file:line for each.
3. Order features: a checklist of CreateOrderV2 fields and related endpoints and whether the
   client supports each (side/bid-ask, count, price, time_in_force values, post_only, STP
   values, expiration_ts, reduce_only / buy_max_cost / sell_position_floor if they exist,
   amend, decrease, batch create, batch cancel, get order by id, queue position). For each:
   supported / hard-coded to X / absent, with file:line.
4. NOT implemented but present in the Kalshi API and relevant to trading (from the OpenAPI
   spec or docs): list endpoints (markets list/get, orderbook, trades, candlesticks, series,
   events, exchange status/schedule, balance, settlements, order groups, RFQ, subaccounts,
   milestones, etc.) and ws channels (ticker, ticker_v2, orderbook_delta, trade, fill,
   market_lifecycle_v2, multivariate, market_positions, ...). Mark which are needed for a
   generic trading client vs niche. If you could not fetch the spec, mark the list as
   "from project facts + memory, unverified today".
5. Environment: base URL(s), demo-environment switch, config of host/timeouts.

Output: `/Users/hugh/kek/lip/loop/run/client-portability/out/api-inventory.md` with citations.
Return in your final message only: counts (endpoints implemented / ws channels implemented /
order fields supported vs absent), whether the OpenAPI spec was fetched, and the 5 most
important absent features for a generic trading client (under 150 words).
