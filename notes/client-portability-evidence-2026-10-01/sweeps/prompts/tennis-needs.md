# Delegate: what the tennis strategies need from a Kalshi client

Scope (read-only): `/Users/hugh/kek/kalshi_tennis` (33 .py, Oct 2025) and
`/Users/hugh/kek/kalshi-bot` (40 .py, Mar 2026). Skip any `.venv`, `tennis_env`,
`node_modules`, data dumps, and plots. Do not modify anything except the output file.

Context you may rely on: Hugh's goal is a MAKER serve-schedule scalp on tennis match markets
(post bids/offers around service games, 1–3c targets, 2–5 minute horizon). The Kalshi v1
integer-cent order endpoints are gone (HTTP 410); v2 uses `POST /portfolio/events/orders`
with `side: bid|ask` on the YES leg and string prices. Fees: taker ~2c/contract near mid,
maker $0 on `quadratic` series.

Task: determine, with `file:line` citations and short verbatim quotes, what these two
strategies require from a Kalshi client:
1. REST endpoints called (exact paths/strings in code), and the Kalshi API vintage targeted
   (v1 cents vs v2 dollars; which client files are dead against the 410).
2. Websocket channels used (Kalshi side) and the tennis data feed used (API-Tennis websocket?),
   and how the two are joined (by ticker, by player name heuristics?).
3. Order features used or assumed: limit vs market, post_only, time_in_force, expiration,
   self-trade prevention, client_order_id, cancel/replace cadence, batch.
4. Position and fill tracking: how they learn about fills (poll, websocket, assume), and
   how often they poll positions/orders/balance.
5. The decision loop timing: what triggers a trade decision (a tennis point event? a timer?),
   the stated or implied reaction-time requirement from point event to order on the book,
   max hold time, and any latency numbers or targets written anywhere in code or docs.
6. Market discovery: how they find the market for a live match, and how often.
7. Anything the strategy needs that a LIP market-making client would not have: e.g.
   multi-market concurrency (how many matches at once), taker exits, stop-loss, expiry-timed
   orders, event-level (both-leg) handling, settlement handling.

Output: write `/Users/hugh/kek/lip/loop/run/client-portability/out/tennis-needs.md` with a
section per item above, each claim cited. Add a final "Requirements list" of <= 15 bullets,
each phrased as a client requirement (e.g. "needs orderbook_delta on N tickers concurrently"),
tagged [stated] if written in the repos or [inferred] if you derived it.
Return in your final message only the requirements list and the count of cited claims
(under 200 words).
