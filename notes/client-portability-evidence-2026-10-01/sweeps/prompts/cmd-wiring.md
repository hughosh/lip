# Delegate: how the transport is constructed and consumed by the binaries

Scope (read-only): `/Users/hugh/kek/lip/go/cmd/harness/*.go` (non-test; 13k lines — use grep and
targeted reads, not whole-file reads), `/Users/hugh/kek/lip/go/cmd/accountcheck`,
`/Users/hugh/kek/lip/go/cmd/incentives`, `/Users/hugh/kek/lip/go/cmd/conform`,
`/Users/hugh/kek/lip/go/cmd/alarmcheck`. Open files under `/Users/hugh/kek/lip/go/harness/`
only to resolve a signature you need to name. Do not modify anything except the output file.

Task:
1. Construction. For each binary, the call sites that build the transport: signer/auth,
   REST client, websocket session/supervisor, write guard, throttle, clock injection, store
   handles. One row each: | binary | what | file:line | dependencies passed in |.
   Quote the constructor call verbatim (one line).
2. Surface used. For `cmd/harness` and separately for the small binaries, the complete list of
   exported identifiers used from `rest.`, `wsx.`, `feed.`, `netx.` (run e.g.
   `grep -ohE '\b(rest|wsx|feed|netx)\.[A-Z][A-Za-z0-9_]*' <files> | sort | uniq -c | sort -rn`).
   Present as a table with counts. This tells us the minimal API a standalone consumer needs.
3. Data flow. How book updates, trades, fills and order acks reach strategy code in
   `cmd/harness`: name the goroutines/loops that own the websocket and the REST poller, the
   channels/callbacks/interfaces that carry events, and where the strategy decision is made
   (function names, file:line). A 10–20 line ASCII flow is fine.
4. Minimal standalone usage. Using `cmd/accountcheck` and `cmd/incentives` as the examples,
   write the shortest sequence of calls (actual function names, with file:line of each
   definition) a new binary would need to: sign and GET /portfolio/balance; walk
   /portfolio/orders; place an order (via rest/write.go); cancel it; open a websocket and
   receive orderbook deltas for one ticker. Mark any step that is impossible without
   constructing LIP-specific objects (cfg.Params, risk.*, quote.*, hstore) and say which.

Output: `/Users/hugh/kek/lip/loop/run/client-portability/out/cmd-wiring.md` with citations.
Return in your final message only: the number of exported rest/wsx identifiers cmd/harness
uses vs the small binaries, and the list from item 4 of steps that are impossible standalone
(under 150 words).
