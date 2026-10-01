# cmd-wiring: how the transport is built and consumed by the binaries

Read-only sweep of `go/cmd/{harness,accountcheck,incentives,conform,alarmcheck}` for
`loop/run/client-portability/prompts/cmd-wiring.md`. Repo HEAD `0c65eb6`, `go/` tree clean when read.
Paths are relative to `/Users/hugh/kek/lip/go/` (`cmd/harness/runtime.go:290` is line 290 of
`/Users/hugh/kek/lip/go/cmd/harness/runtime.go`). Conventions: a bare `file.go:N` is in `cmd/harness/`, or in
the file named by the table row it sits in (small-binary rows); `rest/`, `wsx/`, `quote/`, `cfg/`, `num/`,
`netx/`, `lifecycle/` stand for `harness/<name>/`; `feed/` and `core/` are top-level under `go/`; a bare
`:N` continues the file cited just before it; bare `read_throttle.go` is always `cmd/harness/read_throttle.go`
(`wsx/read_throttle.go` is a different file and is not cited). Every cite can be checked with `sed -n 'Np' <path>`.

## 0. Answer in numbers, and how they were obtained

- **Surface (item 2).** `cmd/harness` uses **88** distinct exported identifiers from `rest`/`wsx`
  (47 `rest` + 41 `wsx`; plus 7 `netx` and 2 `feed`, 97 in all, 267 code uses). The four small binaries
  together use **14** (13 `rest` + 1 `wsx`; plus `feed.NewSigner`, 15 in all, 63 code uses):
  accountcheck 11 `rest`, incentives 5 `rest`, conform 12 `rest` + 1 `wsx`, alarmcheck 0 (it imports only
  `lip/harness/ping`). Seven identifiers are common to both sets: `rest.Client`, `rest.Doer`,
  `rest.NewClient`, `rest.Request`, `rest.Response`, `rest.StatusResting`, `wsx.InspectFrame`.
- **Item 4, steps that need LIP objects.** (c) placing an order needs a `cfg.Params` value, a `quote.Side`
  (an alias of `num.Side`) and a LIP-format client order id, and sends only post-only GTC limits;
  (e) a supervised websocket through `wsx.NewSupervisor` needs a `cfg.Params` that passes `Validate()`;
  turning frames into a book needs `core.NewRig`. Steps (a), (b), (d) and a raw websocket via `feed.Dial`
  need none. Nothing in item 4 needs `risk.*` or `hstore` (§4).
- **How the numbers were obtained.** Counts are code-only, from a Go AST pass that resolves each selector
  against the file's imports (comments, string literals and a struct field named `rest` are excluded). A
  second pass type-checked each binary with `go/types` over export data from `go list -export -deps`
  (269 packages, 0 type errors) and reproduced both the identifier sets and the use counts (97 / 267 for
  `cmd/harness`, 15 / 63 for the small binaries). The prompt's literal grep
  over-counts for `cmd/harness` (103 distinct) because it also matches five identifiers that occur only in
  comments (`feed.Universe`, `rest.CancelAndSweep`, `rest.Create`, `rest.Fills`, `wsx.FrameEffects`) and the
  field access `nt.rest.DialContext`; the "grep count" column in §2 shows the difference per identifier.
- **Item-4 sequences were compiled, not run.** A scratch module outside the repo (`replace lip =>
  /Users/hugh/kek/lip/go`) holds all five steps and `go vet` passes; a deliberately wrong method name made
  it fail, so the check is live. Nothing was executed against the exchange or any account.
- **Scope note.** Besides signatures I read bodies in `harness/rest` (client, write, wire, cancel, guard,
  coid, throttle, page, read), `harness/wsx` (supervisor, session, transport, wire, portfolio, frame),
  `harness/cfg/params.go`, `harness/netx/dns.go`, `go/feed` and `go/core/rig.go`, because the item-3 and
  item-4 claims about what a call needs are claims about those bodies. `quote`, `lifecycle`, `hstore`,
  `risk` and `qual` were opened only to name signatures and interfaces.

## 1. Construction

Wiring happens in one chain: `main` (`cmd/harness/main.go:338`) calls `productionExchange`
(`runtime.go:288`), which builds the signer, the F6 network layer (`newF6Net`, `runtime.go:196`) and, through
`exchangeOver` (`runtime.go:351`), the REST doer and the websocket collaborators, all returned in one
`exchange` struct (`runtime.go:100-127`). `newRigWithLock` (`runtime.go:610`) then builds the guarded REST
client, the gate, the supervisor, the poller and the LIP state. The `exchange` struct has "no
production default" for any field (comment `runtime.go:86-99`, check `runtime.go:129-168`), which is the
injection seam.

### 1.1 Rows (all five binaries)

Quotes are verbatim, leading whitespace removed. "L" after a line number means the call continues on the
next line, quoted in the last column.

| binary | what | file:line | constructor call (verbatim) | dependencies passed in |
|---|---|---|---|---|
| harness | signer / auth | cmd/harness/runtime.go:290 | `signer, err := feed.NewSignerFrom(c.Paths.Key, c.Paths.Env)` | PEM path and env-file path from the JSON config (`paths.key`, `paths.env`; `config.go:201-202`). Result `*feed.Signer`; the websocket side sees it only as the `wsx.Signer` interface (`WSHeaders`), `runtime.go:397`. |
| harness | resolver cache and dial (F6) | cmd/harness/runtime.go:299 L, inner :205 | `nt, err := newF6Net(anom, netx.SystemResolver(),` then L300 `(&net.Dialer{Timeout: restTimeout}).DialContext, time.Now)`; inside: `cd, err := netx.NewCachedDialer(r, d, now, dnsFallbackReporter(anom))` | the process `*anomalySink`, a resolver, a dial func (10 s connect bound, `restTimeout` = `runtime.go:66`) and the clock `time.Now`. All four are mandatory: `NewCachedDialer` rejects nil (`netx/dns.go:92-99`). |
| harness | two transports over the one cache | cmd/harness/runtime.go:209 | `nt := &f6Net{dialer: cd, rest: f6Transport(cd), ws: f6Transport(cd)}` | the `*netx.CachedDialer`. `f6Transport` (`runtime.go:223-233`) restates `net/http` defaults (proxy from env, HTTP/2, 100 idle conns, 90 s idle timeout, 10 s TLS handshake) and replaces only `DialContext`. `nt.dials` (`dial_barrier.go:13`) fences detached dials at final close. |
| harness | REST doer (signed HTTP) | cmd/harness/runtime.go:354 | `var doer rest.Doer = rest.NewHTTPDoerWithTransport(signer, restTimeout, nt.rest)` | signer, `restTimeout` = 10 s (`runtime.go:66`), the REST `*http.Transport`. Inside `rest`: Host `feed.RestHost` (`rest/client.go:320`), clock `time.Now` (`:321`), redirects refused (`:315`). |
| harness | request counters (qualification only) | cmd/harness/runtime.go:357 and :335 | `doer, err = qrec.WrapDoer(doer)` and `clientDoer, err = qrec.WrapAttemptDoer(clientDoer)` | a `*qual.Recorder` (`qual.Open`, `qualification.go:88`); nil outside a qualification run (guards `runtime.go:355`, `:334`). The attempt counter sits above the guard and the transport counter below it (`runtime.go:321-326`). |
| harness | write guard | cmd/harness/runtime.go:329 (arm: :328) | `guarded, err := rest.NewWriteGuard(next, arm)` with `arm := rest.WriteArm{Live: c.Live, LiveOKPath: c.Paths.LiveOK}` | the next doer; the `-live` flag (`c.Live = live`, `main.go:285`) and the `live_ok` sentinel path (`config.go:206`), statted freshly per write (`rest/guard.go:137,147`). Built only inside `clientDoerOver` (`runtime.go:327`), called from `runtime.go:372`, `runtime.go:794`, `main.go:345`. |
| harness | the shared REST client | cmd/harness/runtime.go:798 | `r.api = rest.NewClient(clientDoer)` | the guarded (and, in qualification, counted) doer from `clientDoerOver(ex.Doer, c, qrec)` (`:794`); then `r.api.SweepTrace = sweepTraceWriter(os.Stderr)` (`:799`). One client, shared by the dispatcher workers, startup and every read loop. |
| harness | startup read client | cmd/harness/runtime.go:376 | `programs := rest.NewClient(startup).Programs(ctx)` | `startup`, a second guarded client over the raw doer (`runtime.go:372`); reads the active incentive universe before the rig exists. |
| harness | read-only preflight clients | cmd/harness/funding_cap.go:53 and turnover_funding.go:24 | `client := rest.NewClient(getOnlyDoer{next: doer})` | the guarded production doer, wrapped again by `getOnlyDoer` (`funding_cap.go:31-38`, refuses non-GET); account-funding reads before the rig is built. |
| harness | websocket dialer | cmd/harness/runtime.go:396 | `Dialer:  wsx.NewLiveDialerWithTransport(nt.ws),` | the websocket `*http.Transport`, a separate pool from REST (H-FAIL-2, comment `runtime.go:174-181`). |
| harness | clock injection | cmd/harness/runtime.go:398, :400-401 (and :300) | `Clock:   wsx.NewSystemClock(),` | no args. Also `NowMs:   func() int64 { return time.Now().UnixMilli() },` and `Mono:    func() time.Duration { return time.Since(start) },` (`start` = `time.Now()`, `runtime.go:393`); `time.Now` is also passed to `newF6Net` (`:300`). `rest` keeps its own defaults: `HTTPDoer.Now = time.Now` (`rest/client.go:321`), `Client.Now == nil` means `time.Now` (`rest/client.go:405`, `rest/cancel.go:530`). |
| harness | websocket gate (freshness and quarantine state) | cmd/harness/runtime.go:838 | `r.gate, err = wsx.NewGate(tickers, c.Params)` | `[]string{c.Ticker}` (`:821`), `cfg.Params`. |
| harness | websocket supervisor | cmd/harness/runtime.go:842 L | `r.sup, err = wsx.NewSupervisor(ex.Signer, ex.Dialer, ex.Clock, c.Params,` then L843 `tickers)` | the `exchange` struct's `Signer`, `Dialer`, `Clock` (validated non-nil, `runtime.go:129-168`), `cfg.Params`, the ticker list. |
| harness | portfolio REST poller | cmd/harness/runtime.go:847 | `r.poll, err = wsx.NewPoller(&portfolioThrottleSource{src: r.portfolio, mono: ex.Mono}, ex.Clock, c.Params.PositionPoll)` | `r.portfolio` (= `r.api`, `runtime.go:806`, or a funded wrapper, `:814`/`:816`), the injected clock, interval `position_poll_s`. |
| harness | write throttle (token bucket) | cmd/harness/runtime.go:864 (+863) | `r.cap = quote.NewCapacity(dispatchWorkers, c.Params.WriteBurst)` | `dispatchWorkers` = 2 (`runtime.go:69`) and `write_burst`; the queue is `r.queue = quote.NewQueue(c.Params.MaxQueueAge)` (`:863`). Refilled by `refillWrites` (`dispatch.go:612`) from `owner.pump` (`run.go:2946`); `WriteRate` is halved while a 429 is live (`run.go:2943-2945`). |
| harness | read throttle (per-endpoint backoff) | cmd/harness/read_throttle.go:17 (type); instances run.go:1573, crosscheck.go:27, program_membership.go:37, turnover_runtime.go:267; wrapper type read_throttle.go:94, built at runtime.go:847 | `var backoff readBackoff` (`run.go:1573`) | none. Exponential backoff from `rest.RateLimitError` (`retryDelay`, `dispatch_throttle.go:24`). `rest` itself never sleeps on a 429 (`rest/throttle.go:15`). |
| harness | store handles | cmd/harness/runtime.go:682 L | `r.store, err = hstore.Open(hstore.StoreConfig{` | `c.Paths.DB`, `c.Paths.AnomalyLog` (`:683-684`); writer goroutine `r.store.Run(storeCtx)` (`:697-700`); run row `r.store.BeginRun(r.runID, ex.NowMs(), c.Params)` (`:731`); ownership ledger `r.store.Ownership()` (`:823`); halt latch `lifecycle.NewFileLatch(c.Paths.Latch)` (`:749`); instance lock `acquireHarnessLock(c)` (`:662`). |
| harness | write dispatcher goroutines | cmd/harness/run.go:777 | `inputs.start(ctx, func(ctx context.Context) { r.dispatchLoop(ctx, writes, sd.Permits(), results) })` | the `writes`/`results` channels (`run.go:744-745`) and the store's reservation-result stream `sd.Permits()`. |
| accountcheck | signer | cmd/accountcheck/main.go:525 | `signer, e := feed.NewSigner()` | none; reads `~/.kalshi/kalshi.pem` and `~/.kalshi/env` (`feed/auth.go:43-52`). |
| accountcheck | REST doer | cmd/accountcheck/main.go:532 | `r := run(ctx, rest.NewHTTPDoer(signer, 15*time.Second), *ticker, *fills)` | signer, 15 s client timeout, Go's default transport (`rest/client.go:276-278`, rt nil), Host `feed.RestHost`. |
| accountcheck | GET-only wrapper (local; not `rest.WriteGuard`) | cmd/accountcheck/main.go:402 (type :31-59) | `g := &guarded{next: d}` | the doer. `guarded.Do` refuses non-GET or body-bearing requests (`main.go:37`) and logs path, status, duration and bytes per request (`:53-54`). |
| accountcheck | REST client | cmd/accountcheck/main.go:403 | `c := rest.NewClient(g)` | the wrapped doer. |
| incentives | REST doer (own, unsigned) | cmd/incentives/main.go:252 | `d := publicDoer{client: &http.Client{Timeout: 30 * time.Second}}` | none. `publicDoer` (`main.go:29`) implements `rest.Doer` over plain `net/http` against a hard-coded `apiBase` (`:25`), GET-only with a route allowlist (`publicPath`, `:65`), 8 MiB body cap (`:55-61`), redirects refused (`:47`). |
| incentives | REST client | cmd/incentives/main.go:183 | `w := rest.NewClient(d).Walk(ctx, rest.EpPrograms, filters)` | the public doer. |
| incentives | clock injection | cmd/incentives/main.go:253 | `out, err := inspect(ctx, d, *ticker, time.Now)` | `now func() time.Time` parameter (`main.go:180`). The only small binary that injects a clock. |
| conform | signer | cmd/conform/main.go:456 | `signer, err := feed.NewSigner()` | none (default credential paths). |
| conform | REST doer + GET-only wrapper | cmd/conform/main.go:460 | `doer := getOnly{next: rest.NewHTTPDoer(signer, restTimeout)}` | signer, `restTimeout` = 20 s (`main.go:65`); `getOnly` (`main.go:81-92`) refuses non-GET before the transport. |
| conform | REST client (live) | cmd/conform/main.go:461 | `live := rest.NewClient(doer)` | the wrapped doer. |
| conform | REST clients (offline decode) | cmd/conform/main.go:509 and :586 | `if derr := p.decode(ctx, rest.NewClient(onePage{body: body})); derr != nil {` | canned-body doer `onePage` (`main.go:98-100`): one synthetic page per raw record, replayed through the production typed decoder. |
| conform | websocket frames (decode only; no socket) | cmd/conform/main.go:369 | `info := wsx.InspectFrame(env.M)` | recorded tape frames from `tape.Frames` (`main.go:355`). No connection is opened. |
| alarmcheck | none (no Kalshi transport) | cmd/alarmcheck/main.go:29-31 | `loadTopic:   ping.LoadNTFYTopic,` | imports only `lip/harness/ping` (ntfy topic and sender, healthchecks.io deadman); no `rest`, `wsx`, `feed` or `netx`. |

Absent from all four small binaries: a websocket session or supervisor, `rest.NewWriteGuard`, any throttle or
backoff, and any store handle (accountcheck and conform write a JSON report with `os.WriteFile`).

Other LIP objects built in the same `newRigWithLock` body, not transport but the things the transport feeds:
`core.NewRig(noopSink{}, map[string]float64{c.Ticker: ex.Target})` (`runtime.go:861`),
`risk.NewPortfolio()` (`:862`), `lifecycle.NewStartup(...)` (`:832`), `lifecycle.NewForeignGuard(...)` (`:823`),
`lifecycle.NewGlobalController(...)` (`:753`), `lifecycle.NewSignalController` (`:852`),
`lifecycle.NewDrainTracker` (`:856`).

### 1.2 What the wiring shows

- The process-level Kalshi-facing construction is four functions: `newF6Net` (`runtime.go:196-213`),
  `clientDoerOver` (`:327-341`), `exchangeOver` (`:351-405`), `productionExchange` (`:288-305`), about 100
  lines. The ~275-line `newRigWithLock` (`:610-885`) then builds the shared REST client, gate, supervisor and
  poller in its second half (`:794-850`) and composes the LIP state (store, lifecycle, quote, risk, core)
  around them.
- Substitution is already designed in: `rest.Doer` (`rest/client.go:67`), `wsx.Dialer`, `wsx.Socket`,
  `wsx.Signer`, `wsx.Clock` (`wsx/transport.go:24-81`) and `wsx.PortfolioSource` (`wsx/portfolio.go:21`) are
  interfaces. The small binaries use the `rest.Doer` seam three different ways: a signed default
  (`NewHTTPDoer`), an unsigned public doer (incentives), and a canned-body replay doer (conform).
- The write guard is a convention of construction order, not a type: `HTTPDoer.Do` sends any method
  (`rest/client.go:329-386`), and `rest.NewWriteGuard` is applied only because `clientDoerOver` is the one
  place that composes it and every harness client is built from its result (`runtime.go:307-326`).
  accountcheck and conform substitute their own GET-only wrappers instead of the guard.
- Only `cmd/harness` constructs a websocket or the F6 cached transports; the small binaries use Go's
  default transport.

## 2. Surface used

### 2.1 Totals

| package | cmd/harness: distinct identifiers / code uses | small binaries: distinct / code uses |
|---|---|---|
| `rest` | 47 / 166 | 13 / 60 |
| `wsx` | 41 / 90 | 1 / 1 |
| `netx` | 7 / 9 | 0 / 0 |
| `feed` | 2 / 2 | 1 / 2 |
| **all four** | **97 / 267** | **15 / 63** |

Per small binary (distinct `rest` + `wsx`): accountcheck 11 + 0, incentives 5 + 0, conform 12 + 1,
alarmcheck 0 + 0. Common to `cmd/harness` and the small binaries: `rest.Client`, `rest.Doer`,
`rest.NewClient`, `rest.Request`, `rest.Response`, `rest.StatusResting`, `wsx.InspectFrame`. Used by the small
binaries but not by `cmd/harness`: `rest.Endpoint`, `rest.EpFills`, `rest.EpOrders`, `rest.EpPositions`,
`rest.EpPrograms`, `rest.NewHTTPDoer`, `rest.ParsePrice4`, `feed.NewSigner`.

Column notes. "code uses" counts occurrences outside comments and strings. "grep count" is the prompt's
command (`grep -ohE '\b(rest|wsx|feed|netx)\.[A-Z][A-Za-z0-9_]*'`), which also matches comments and strings.
"kind" is the declaration kind in the defining package; "defined at" is relative to `go/`.

### 2.2 `cmd/harness` (complete)

#### cmd/harness, `rest.*`: 47 distinct identifiers, 166 code uses

| # | identifier | kind | code uses | grep count | defined at | also in small binaries |
|---|---|---|---|---|---|---|
| 1 | `rest.Order` | type | 24 | 24 | harness/rest/read.go:177 |  |
| 2 | `rest.RateLimitError` | type | 11 | 11 | harness/rest/throttle.go:16 |  |
| 3 | `rest.Client` | type | 9 | 13 | harness/rest/client.go:396 | yes |
| 4 | `rest.Balance` | type | 8 | 8 | harness/rest/read.go:761 |  |
| 5 | `rest.Doer` | type | 8 | 8 | harness/rest/client.go:67 | yes |
| 6 | `rest.ScheduleResult` | type | 8 | 8 | harness/rest/schedule.go:122 |  |
| 7 | `rest.CreateRejected` | const | 7 | 7 | harness/rest/write.go:48 |  |
| 8 | `rest.ProgramsResult` | type | 7 | 7 | harness/rest/read.go:675 |  |
| 9 | `rest.OrderbookResult` | type | 6 | 6 | harness/rest/orderbook.go:86 |  |
| 10 | `rest.StatusResting` | const | 6 | 6 | harness/rest/client.go:220 | yes |
| 11 | `rest.BookLevel` | type | 4 | 4 | harness/rest/orderbook.go:39 |  |
| 12 | `rest.CentsExact` | func | 4 | 4 | harness/rest/read.go:157 |  |
| 13 | `rest.CreateResult` | type | 4 | 4 | harness/rest/write.go:72 |  |
| 14 | `rest.NewClient` | func | 4 | 4 | harness/rest/client.go:416 | yes |
| 15 | `rest.PositionsResult` | type | 4 | 4 | harness/rest/read.go:425 |  |
| 16 | `rest.Walk` | type | 4 | 4 | harness/rest/page.go:70 |  |
| 17 | `rest.WalkFailed` | const | 4 | 4 | harness/rest/page.go:54 |  |
| 18 | `rest.OrdersResult` | type | 3 | 3 | harness/rest/read.go:204 |  |
| 19 | `rest.OwnResting` | type | 3 | 3 | harness/rest/orderbook.go:332 |  |
| 20 | `rest.Fill` | type | 2 | 2 | harness/rest/read.go:483 |  |
| 21 | `rest.FillsResult` | type | 2 | 2 | harness/rest/read.go:497 |  |
| 22 | `rest.ReducerBook` | type | 2 | 2 | harness/rest/orderbook.go:573 |  |
| 23 | `rest.Response` | type | 2 | 2 | harness/rest/client.go:35 | yes |
| 24 | `rest.SideBooks` | type | 2 | 2 | harness/rest/orderbook.go:338 |  |
| 25 | `rest.StatusCanceled` | const | 2 | 2 | harness/rest/client.go:221 |  |
| 26 | `rest.StatusExecuted` | const | 2 | 2 | harness/rest/client.go:222 |  |
| 27 | `rest.SweepResult` | type | 2 | 3 | harness/rest/cancel.go:236 |  |
| 28 | `rest.SweepTrace` | type | 2 | 2 | harness/rest/sweep_trace.go:16 |  |
| 29 | `rest.WriteRefused` | type | 2 | 2 | harness/rest/guard.go:57 |  |
| 30 | `rest.BookCheck` | type | 1 | 1 | harness/rest/orderbook.go:345 |  |
| 31 | `rest.Coid` | func | 1 | 1 | harness/rest/coid.go:52 |  |
| 32 | `rest.CompareBooks` | func | 1 | 1 | harness/rest/orderbook.go:379 |  |
| 33 | `rest.CreateOrder` | type | 1 | 1 | harness/rest/wire.go:109 |  |
| 34 | `rest.CreateUnknown` | const | 1 | 1 | harness/rest/write.go:35 |  |
| 35 | `rest.Funding` | type | 1 | 1 | harness/rest/funding.go:20 |  |
| 36 | `rest.MarketStatusActive` | const | 1 | 1 | harness/rest/schedule.go:43 |  |
| 37 | `rest.NewCreateOrder` | func | 1 | 2 | harness/rest/wire.go:175 |  |
| 38 | `rest.NewHTTPDoerWithTransport` | func | 1 | 1 | harness/rest/client.go:291 |  |
| 39 | `rest.NewWriteGuard` | func | 1 | 1 | harness/rest/guard.go:83 |  |
| 40 | `rest.OrderbookFailed` | const | 1 | 1 | harness/rest/orderbook.go:65 |  |
| 41 | `rest.OrderbookGranularity` | const | 1 | 1 | harness/rest/orderbook.go:62 |  |
| 42 | `rest.OrderbookRead` | const | 1 | 1 | harness/rest/orderbook.go:56 |  |
| 43 | `rest.Request` | type | 1 | 1 | harness/rest/client.go:27 | yes |
| 44 | `rest.ScheduleFailed` | const | 1 | 1 | harness/rest/schedule.go:83 |  |
| 45 | `rest.ScheduleRead` | type | 1 | 1 | harness/rest/schedule.go:104 |  |
| 46 | `rest.ValidRunID` | func | 1 | 3 | harness/rest/coid.go:75 |  |
| 47 | `rest.WriteArm` | type | 1 | 1 | harness/rest/guard.go:38 |  |

#### cmd/harness, `wsx.*`: 41 distinct identifiers, 90 code uses

| # | identifier | kind | code uses | grep count | defined at | also in small binaries |
|---|---|---|---|---|---|---|
| 1 | `wsx.TruthOrders` | const | 9 | 9 | harness/wsx/gate.go:21 |  |
| 2 | `wsx.TruthPositions` | const | 9 | 9 | harness/wsx/gate.go:20 |  |
| 3 | `wsx.CrossCheckRequest` | type | 8 | 8 | harness/wsx/gate.go:998 |  |
| 4 | `wsx.ReconcileToken` | type | 8 | 8 | harness/wsx/gate.go:46 |  |
| 5 | `wsx.Command` | type | 4 | 4 | harness/wsx/session.go:29 |  |
| 6 | `wsx.CrossCheckUnavailable` | const | 4 | 4 | harness/wsx/gate.go:1131 |  |
| 7 | `wsx.Event` | type | 4 | 4 | harness/wsx/session.go:79 |  |
| 8 | `wsx.TruthFills` | const | 4 | 4 | harness/wsx/gate.go:22 |  |
| 9 | `wsx.PortfolioRead` | type | 3 | 3 | harness/wsx/portfolio.go:34 |  |
| 10 | `wsx.CrossCheckAgree` | const | 2 | 2 | harness/wsx/gate.go:1135 |  |
| 11 | `wsx.EventDisconnected` | const | 2 | 2 | harness/wsx/session.go:57 |  |
| 12 | `wsx.EventFrame` | const | 2 | 2 | harness/wsx/session.go:56 |  |
| 13 | `wsx.FrameSnapshot` | const | 2 | 2 | harness/wsx/frame.go:19 |  |
| 14 | `wsx.Truth` | type | 2 | 2 | harness/wsx/gate.go:17 |  |
| 15 | `wsx.ApplyPortfolio` | func | 1 | 1 | harness/wsx/portfolio.go:332 |  |
| 16 | `wsx.Binding` | type | 1 | 1 | harness/wsx/portfolio.go:261 |  |
| 17 | `wsx.Clock` | type | 1 | 1 | harness/wsx/transport.go:78 |  |
| 18 | `wsx.CmdResnapshot` | const | 1 | 1 | harness/wsx/session.go:20 |  |
| 19 | `wsx.CrossCheckDisagree` | const | 1 | 1 | harness/wsx/gate.go:1138 |  |
| 20 | `wsx.CrossCheckGranularity` | const | 1 | 1 | harness/wsx/gate.go:1143 |  |
| 21 | `wsx.CrossCheckOutcome` | type | 1 | 1 | harness/wsx/gate.go:1124 |  |
| 22 | `wsx.Dialer` | type | 1 | 1 | harness/wsx/transport.go:48 |  |
| 23 | `wsx.EventConnected` | const | 1 | 1 | harness/wsx/session.go:55 |  |
| 24 | `wsx.EventDisconnectReduce` | const | 1 | 1 | harness/wsx/session.go:61 |  |
| 25 | `wsx.FrameDelta` | const | 1 | 1 | harness/wsx/frame.go:20 |  |
| 26 | `wsx.FrameInfo` | type | 1 | 1 | harness/wsx/frame.go:53 |  |
| 27 | `wsx.Gate` | type | 1 | 4 | harness/wsx/gate.go:194 |  |
| 28 | `wsx.InspectFrame` | func | 1 | 1 | harness/wsx/frame.go:98 | yes |
| 29 | `wsx.NewGate` | func | 1 | 1 | harness/wsx/gate.go:225 |  |
| 30 | `wsx.NewLiveDialerWithTransport` | func | 1 | 1 | harness/wsx/transport.go:143 |  |
| 31 | `wsx.NewPoller` | func | 1 | 1 | harness/wsx/portfolio.go:100 |  |
| 32 | `wsx.NewSupervisor` | func | 1 | 1 | harness/wsx/supervisor.go:60 |  |
| 33 | `wsx.NewSystemClock` | func | 1 | 1 | harness/wsx/transport.go:94 |  |
| 34 | `wsx.PnLMarkAbsent` | const | 1 | 1 | harness/wsx/gate.go:923 |  |
| 35 | `wsx.PnLMarkStale` | const | 1 | 1 | harness/wsx/gate.go:925 |  |
| 36 | `wsx.Poller` | type | 1 | 3 | harness/wsx/portfolio.go:93 |  |
| 37 | `wsx.PortfolioEffects` | type | 1 | 2 | harness/wsx/portfolio.go:214 |  |
| 38 | `wsx.PortfolioSource` | type | 1 | 1 | harness/wsx/portfolio.go:21 |  |
| 39 | `wsx.Signer` | type | 1 | 2 | harness/wsx/transport.go:24 |  |
| 40 | `wsx.Stamp` | type | 1 | 2 | harness/wsx/transport.go:63 |  |
| 41 | `wsx.Supervisor` | type | 1 | 1 | harness/wsx/supervisor.go:43 |  |

#### cmd/harness, `netx.*`: 7 distinct identifiers, 9 code uses

| # | identifier | kind | code uses | grep count | defined at | also in small binaries |
|---|---|---|---|---|---|---|
| 1 | `netx.CachedDialer` | type | 2 | 3 | harness/netx/dns.go:78 |  |
| 2 | `netx.Fallback` | type | 2 | 2 | harness/netx/dns.go:53 |  |
| 3 | `netx.DialFunc` | type | 1 | 1 | harness/netx/dns.go:46 |  |
| 4 | `netx.NewCachedDialer` | func | 1 | 2 | harness/netx/dns.go:92 |  |
| 5 | `netx.Reporter` | type | 1 | 2 | harness/netx/dns.go:69 |  |
| 6 | `netx.Resolver` | type | 1 | 1 | harness/netx/dns.go:41 |  |
| 7 | `netx.SystemResolver` | func | 1 | 1 | harness/netx/dns.go:223 |  |

#### cmd/harness, `feed.*`: 2 distinct identifiers, 2 code uses

| # | identifier | kind | code uses | grep count | defined at | also in small binaries |
|---|---|---|---|---|---|---|
| 1 | `feed.NewSignerFrom` | func | 1 | 2 | feed/auth.go:54 |  |
| 2 | `feed.Signer` | type | 1 | 2 | feed/auth.go:37 |  |

Grep-only matches in `cmd/harness`, i.e. not code uses: `feed.Universe` (comment `runtime.go:364`),
`rest.CancelAndSweep` (4 comments, `run.go:334,364,3500,3798`), `rest.Create` (comment `run.go:47`),
`rest.Fills` (comment `run.go:4013`), `wsx.FrameEffects` (comment `run.go:1352`) and `nt.rest.DialContext`
(`runtime.go:210`, a field named `rest` on `f6Net`).

### 2.3 Small binaries (complete, separately)

#### Small binaries (accountcheck, incentives, conform, alarmcheck), all four packages

15 distinct identifiers (rest 13, wsx 1, feed 1, netx 0), 63 code uses.

| # | identifier | kind | accountcheck | incentives | conform | alarmcheck | total | grep count | defined at | also in cmd/harness |
|---|---|---|---|---|---|---|---|---|---|---|
| 1 | `rest.Response` | type | 3 | 8 | 4 |  | 15 | 15 | harness/rest/client.go:35 | yes |
| 2 | `rest.Client` | type | 2 |  | 6 |  | 8 | 8 | harness/rest/client.go:396 | yes |
| 3 | `rest.Doer` | type | 3 | 2 | 1 |  | 6 | 7 | harness/rest/client.go:67 | yes |
| 4 | `rest.Request` | type | 2 | 2 | 2 |  | 6 | 6 | harness/rest/client.go:27 | yes |
| 5 | `rest.NewClient` | func | 1 | 1 | 3 |  | 5 | 5 | harness/rest/client.go:416 | yes |
| 6 | `rest.EpFills` | var | 2 |  | 1 |  | 3 | 3 | harness/rest/client.go:177 |  |
| 7 | `rest.EpPositions` | var | 1 |  | 2 |  | 3 | 3 | harness/rest/client.go:155 |  |
| 8 | `rest.EpPrograms` | var |  | 1 | 2 |  | 3 | 3 | harness/rest/client.go:185 |  |
| 9 | `rest.ParsePrice4` | func |  |  | 3 |  | 3 | 4 | harness/rest/read.go:36 |  |
| 10 | `feed.NewSigner` | func | 1 |  | 1 |  | 2 | 2 | feed/auth.go:43 |  |
| 11 | `rest.Endpoint` | type | 1 |  | 1 |  | 2 | 2 | harness/rest/client.go:124 |  |
| 12 | `rest.EpOrders` | var | 1 |  | 1 |  | 2 | 2 | harness/rest/client.go:170 |  |
| 13 | `rest.NewHTTPDoer` | func | 1 |  | 1 |  | 2 | 2 | harness/rest/client.go:276 |  |
| 14 | `rest.StatusResting` | const | 2 |  |  |  | 2 | 2 | harness/rest/client.go:220 | yes |
| 15 | `wsx.InspectFrame` | func |  |  | 1 |  | 1 | 4 | harness/wsx/frame.go:98 | yes |

`alarmcheck` has no entries: it uses none of the four packages.

### 2.4 Member-level surface (type-checked)

The package-qualified list above under-counts what a consumer needs, because most of the API is reached as
methods and fields on values. A `go/types` pass lists every method, field and composite-literal key of a
`rest`/`wsx`/`feed`/`netx` type that each binary touches (call-site counts in parentheses).

**Small binaries.**

| binary | type | methods called (call sites) | fields (sites) |
|---|---|---|---|
| accountcheck | `rest.Client` | MarketFunding(1), Orders(1), Positions(1), Walk(2) |  |
| accountcheck | `rest.Doer` | Do(2) |  |
| accountcheck | `rest.Funding` |  | Available(1), ObservedAt(1), UpdatedTS(1) |
| accountcheck | `rest.OrdersResult` |  | Orders(1) |
| accountcheck | `rest.PositionsResult` |  | ByTicker(1) |
| accountcheck | `rest.Request` |  | Body(1), Method(2), Path(2), Query(2) |
| accountcheck | `rest.Response` |  | Body(2), Status(3) |
| accountcheck | `rest.Walk` | Records(5), Replaces(2) | Outcome(4), Pages(4) |
| accountcheck | `rest.WalkOutcome` | String(4) |  |
| incentives | `rest.Client` | Walk(1) |  |
| incentives | `rest.Doer` | Do(1) |  |
| incentives | `rest.Request` |  | Body(1), Method(3), Path(4), Query(1) |
| incentives | `rest.Response` |  | Body(2), Status(3) |
| incentives | `rest.Walk` | Records(1), Replaces(1) | Err(2), Outcome(1), Pages(2) |
| conform | `rest.Balance` |  | Cents(1) |
| conform | `rest.Client` | Balance(1), Fills(1), Orders(1), Positions(1), Programs(1), Schedule(1), Walk(1) |  |
| conform | `rest.Doer` | Do(1) |  |
| conform | `rest.Endpoint` |  | CursorField(2), Path(2) |
| conform | `rest.Request` |  | Method(2), Path(1) |
| conform | `rest.Response` |  | Body(1), Status(1) |
| conform | `rest.ScheduleRead` | Observed(1) | Anomalies(1), Err(4) |
| conform | `rest.ScheduleResult` |  | HasClose(1), Result(2), Status(2) |
| conform | `rest.Walk` | Records(1), Replaces(5) | Anomalies(1), Err(6), Outcome(1), Pages(1) |
| conform | `wsx.FrameInfo` |  | Anomalies(1), Deliver(1), Granularity(1), Kind(1) |
| conform | `wsx.FrameKind` | String(1) |  |

**`cmd/harness`.**

| type | methods called (call sites) | fields read/written or set in literals (sites) |
|---|---|---|
| `netx.CachedDialer` | DialContext(2) |  |
| `netx.Fallback` |  | Address(1), Connected(1), Host(1), LookupErr(1) |
| `rest.Balance` |  | Cents(3) |
| `rest.BookCheck` |  | No(1), Target(1), Yes(1) |
| `rest.BookLevel` |  | Cents(6), Size(4) |
| `rest.BookVerdict` |  | Agree(1), Why(1) |
| `rest.CancelResult` |  | Err(2), Sent(2) |
| `rest.Client` | AccountPositions(5), CancelAndSweep(1), Create(1), MarketFunding(4), Orderbook(2), Orders(5), Programs(3), Schedule(2) | SweepTrace(1) |
| `rest.CreateOrder` | ClientOrderID(18), Count(9), PriceCents(9), Side(1), Ticker(3) |  |
| `rest.CreateOutcome` | Exists(3) |  |
| `rest.CreateResult` | ReconcileNow(1) | Anomalies(1), Attempts(2), Coid(11), Err(1), Filled(2), MaxLive(4), OrderID(7), Outcome(11), RejectReason(3), Status(5) |
| `rest.Doer` | Do(1) |  |
| `rest.FillsResult` |  | Walk(1) |
| `rest.Funding` |  | Available(9), ExchangeIndex(12), ObservedAt(6), Subaccount(4), Ticker(4) |
| `rest.Order` |  | ClientOrderID(15), Fractional(5), OrderID(43), Price4(9), PriceCents(12), Remaining(18), Side(16), Status(6), Ticker(31) |
| `rest.OrderbookResult` | Read(1), Snapshot(2) | Err(6), No(6), Outcome(2), Ticker(6), Yes(6) |
| `rest.OrdersResult` |  | Orders(7), Walk(1) |
| `rest.OwnResting` |  | Cents(3), Size(1) |
| `rest.PositionsResult` |  | ByTicker(4), Walk(1) |
| `rest.ProgramsResult` |  | ByTarget(3), Walk(1) |
| `rest.RateLimitError` |  | Delay(2), HasDelay(1) |
| `rest.ReducerBook` | Invalidate(5), Retain(1), Snapshot(1) |  |
| `rest.Request` |  | Method(2) |
| `rest.ScheduleRead` | Observed(2) | Anomalies(1), Err(5), Outcome(1) |
| `rest.ScheduleResult` |  | CanCloseEarly(1), CloseTime(4), HasClose(2), ScheduleRead(1), Status(2), Ticker(4), TradingClosed(3) |
| `rest.SideBooks` |  | Ours(2), REST(2), WS(2) |
| `rest.SweepResult` |  | Anomalies(1), Cancels(1), Clean(1), OtherOurs(2), StillResting(1), Throttle(1), Walk(1) |
| `rest.Walk` | Records(1), Replaces(13) | Anomalies(1), Err(21), Outcome(8), Pages(1) |
| `rest.WriteArm` |  | Live(1), LiveOKPath(1) |
| `wsx.Binding` |  | Coid(1), OrderID(1) |
| `wsx.Clock` | Now(16) |  |
| `wsx.Command` |  | Kind(1), Sids(1), Tickers(1) |
| `wsx.ConnectEffects` |  | Anomalies(1), Token(1) |
| `wsx.CrossCheckEffects` |  | Accepted(1), Anomalies(1), Reduce(1), ReplaceBook(1), Resnapshot(1), RetainRESTBook(1) |
| `wsx.CrossCheckRequest` |  | Refresh(2), Token(4) |
| `wsx.CrossCheckToken` | Ticker(3) |  |
| `wsx.DisconnectEffects` |  | Anomalies(1), ResetBooks(1), Token(1) |
| `wsx.Event` |  | At(7), Clean(2), Down(1), Frame(2), Kind(4), UniverseRevision(4) |
| `wsx.EventKind` | String(1) |  |
| `wsx.FrameEffects` |  | Anomalies(2), Delivered(2), Reduce(2), Resnapshot(1) |
| `wsx.FrameInfo` |  | BookSubscribeSID(2), Kind(3), SID(3), Ticker(11) |
| `wsx.Gate` | AcceptsUniverse(1), Actionable(4), AddMarkets(1), ApplyDisconnect(1), ApplyFrameForUniverse(1), BookCurrent(2), Connected(2), F5Quarantined(6), ForceCrossCheck(1), Generation(4), NoteCrossCheck(1), NoteDisconnectSustained(1), NoteRejectRate(1), NoteSeqGap(1), ObservationGap(1), OnConnectForUniverse(1), PnLMark(1), RESTReducerActionable(1), Reducing(2), RefreshUniverse(1), Tick(1), TruthAge(3), UniverseRevision(1) |  |
| `wsx.Poller` | Run(1) |  |
| `wsx.PortfolioEffects` |  | Anomalies(2), Applied(11), BackfilledFill(3), Bound(1), Foreign(1), InvKill(2), OwnedFill(3), Records(4), Reduce(1), Stale(1), Stop(1) |
| `wsx.PortfolioRead` | RateLimits(1), StartedAt(4) |  |
| `wsx.PortfolioSource` | Fills(1), Orders(1), Positions(1) |  |
| `wsx.ReconcileToken` | Valid(1) |  |
| `wsx.Stamp` |  | Mono(7) |
| `wsx.Supervisor` | AddMarkets(1), RefreshUniverse(1), Run(1), UniverseRevision(1) |  |
| `wsx.TickEffects` |  | Anomalies(2), CrossCheck(1), Reduce(2), Resnapshot(2), Stop(1) |

Methods reached through harness-local or collaborator interfaces do not appear in that table, because the
selection resolves to the interface, not to `rest.Client`. The effective `rest.Client` reach in `cmd/harness`
(definitions carry a `rest/` prefix; call-site names are in `cmd/harness/`):

| `rest.Client` method (def) | reached? | call sites |
|---|---|---|
| `Create` (`rest/write.go:154`) | yes | `dispatch.go:374` only |
| `CancelAndSweep` (`rest/cancel.go:355`) | yes | `dispatch.go:535`; startup sweep through `lifecycle.CancelSweeper` (`lifecycle/startup.go:102`, called `:896`) |
| `Orders` (`rest/read.go:237`) | yes | `funding_cap.go:62`, `turnover_accounting.go:175`, `turnover_funding.go:29,188`, `turnover_restart.go:55`; polled through `wsx.PortfolioSource` (`read_throttle.go:130`) |
| `Positions` (`rest/read.go:436`) | via interface | `wsx.PortfolioSource` (`read_throttle.go:140`); `lifecycle/startup.go:637` |
| `Fills` (`rest/read.go:544`) | via interface | `wsx.PortfolioSource` (`read_throttle.go:120`); `lifecycle/startup.go:657` |
| `Balance` (`rest/read.go:766`) | via interface | `balanceSource(r.api)` (`balance_telemetry.go:72`); `lifecycle/startup.go:676` |
| `AccountPositions` (`rest/account_positions.go:85`) | yes | `funding_cap.go:58`, `funding_runtime.go:38`, `turnover_funding.go:25,175,184` |
| `MarketFunding` (`rest/funding.go:31`) | yes | `funding_cap.go:54`, `funding_runtime.go:42`, `turnover_funding.go:54,218` |
| `Orderbook` (`rest/orderbook.go:146`) | yes | `read_throttle.go:75`, `turnover_selection.go:107` |
| `Programs` (`rest/read.go:707`) | yes | `read_throttle.go:85`, `runtime.go:376`, `turnover_selection.go:69` |
| `Schedule` (`rest/schedule.go:187`) | yes | `read_throttle.go:55`, `turnover_selection.go:112` |
| `Walk` (`rest/page.go:104`) | no | called only inside `rest` (typed readers) and by the small binaries |
| `Cancel` (`rest/cancel.go:106`) | no | no production caller anywhere under `go/` |
| `ConfirmCoid`, `SubaccountNumbers` | no | internal to `rest` (`rest/write.go:386`, `rest/account_positions.go:86`) |

So the harness reaches 11 of the 15 exported `rest.Client` methods; its entire write path is two call sites
(`dispatch.go:374`, `dispatch.go:535`). The small binaries reach 8 (`Balance`, `Fills`, `MarketFunding`,
`Orders`, `Positions`, `Programs`, `Schedule`, `Walk`), all reads.

### 2.5 Reproduce

```sh
cd /Users/hugh/kek/lip/go/cmd/harness
FILES=$(ls *.go | grep -v _test.go)
# the prompt's command (comments and strings included): 103 distinct
grep -ohE '\b(rest|wsx|feed|netx)\.[A-Z][A-Za-z0-9_]*' $FILES | sort | uniq -c | sort -rn | wc -l
# code-only approximation (strips // comments, ignores a leading dot): 97 distinct, 267 uses
cat $FILES | sed -E 's://.*$::' | grep -ohE '(^|[^.A-Za-z0-9_])(rest|wsx|feed|netx)\.[A-Z][A-Za-z0-9_]*' \
  | sed -E 's/^[^a-z]//' | sort | uniq -c | sort -rn
cd ..; grep -ohE '\b(rest|wsx|feed|netx)\.[A-Z][A-Za-z0-9_]*' \
  accountcheck/main.go incentives/main.go conform/main.go alarmcheck/main.go | sort | uniq -c | sort -rn   # 15 distinct
```

The code-only command reproduces the AST result exactly for `cmd/harness`. For the small binaries it gives 65
uses against the AST's 63, because conform's flag help and a progress message name `wsx.InspectFrame` inside
string literals (`conform/main.go:427`, `:437`). The AST, type-check and compile tools are scratch programs
outside the repo, in
`/private/tmp/claude-501/-Users-hugh-kek/12802585-9fa1-41d1-b8e5-0e40163305f9/scratchpad/`
(`astsel/`, `typesel/`, `defs/`, `portcheck/`).

## 3. Data flow in `cmd/harness`

### 3.1 Goroutines, channels, owner

```
 INPUT GOROUTINES (runtimeGroup `inputs`, run.go:747-799; runtime_group.go:37)
 +- WS supervisor   r.sup.Run(ctx, commands, events)       run.go:750 -> wsx/supervisor.go:161
 |    dial + 2 subscribes (wsx/supervisor.go:291) -> session.run (wsx/session.go:138), reader goroutine (:144)
 |    sock.Read -> frames -> events <- Event{EventFrame, raw bytes}   wsx/session.go:231  --> events  (cap 64)
 +- Portfolio poll  r.poll.Run(ctx, tokens, reads)         run.go:760 -> wsx/portfolio.go:140
 |    every position_poll_s, or at once on a ReconcileToken: Fills, Orders, Positions walks
 |    through rest.Client (wsx/portfolio.go:165-170)                                        --> reads   (cap 2)
 +- REST loops      scheduleLoop :786, programLoop :788, turnoverSelectionLoop :790,
 |                  turnoverTerminalLoop :792, balanceLoop :794, crossCheckLoop :799    --> one chan each (cap 1)
 +- Dispatch pool   dispatchLoop :777 (dispatch.go:209) -> <=2 workers (dispatch.go:233)
        placeWrite (dispatch.go:266)  -> rest.Client.Create          (dispatch.go:374)
        cancelWrite (dispatch.go:532) -> rest.Client.CancelAndSweep  (dispatch.go:535)  --> results (cap 2)

 OWNER goroutine = rig.serveWithShutdown select loop (run.go:837-938); sole writer of book, portfolio, queue
   case <-events:  applyEvent (1250)  -> gate.ApplyFrameForUniverse(.., book.Handle(frame)) (1306)  [books, trades]
   case <-reads:   applyRead (1846)   -> wsx.ApplyPortfolio(gate, pf, ownership, store, read) (1851) [orders, fills, positions]
   case <-results: applyWriteResult (3613) -> queue.AckPlace (3869), pf.ApplyAck (3875)             [order acks]
   then, after EVERY event and every 250 ms tick (run.go:64, 932-937):
     evaluate (2405) -> evaluateMarket (2494) -> quote.NextMarket (2527) -> decideSide (2808)
       -> quote.Decide (2849) -> enqueue (2913) -> pump (2940): Queue.Dequeue (2953) -> build (3069)
       -> writes <- req (3004) -> dispatchLoop
```

Other goroutines in the process, not on the market-data path: store writer (`runtime.go:697`), monitor
(`run.go:691`), store result loop (`run.go:699`), alert service (`alerts.go:81`), drain loop (`run.go:814`),
permit router (`dispatch.go:177`), qualification checkpoints (`run.go:679`).

### 3.2 What carries what

| reaches strategy | origin | carrier (buffer) | produced at | consumed at | state it changes |
|---|---|---|---|---|---|
| book snapshots and deltas | websocket `orderbook_delta`, filtered to the ticker list (`wsx/wire.go:27-38`) | `chan wsx.Event` (64; `eventBuffer` `run.go:76`, made `run.go:739`) | `session.run` `wsx/session.go:231` | `applyEvent`, EventFrame arm `run.go:1303-1343`: `Gate.ApplyFrameForUniverse(info, func() error { return o.r.book.Handle(frame) }, ...)` (`:1306-1308`) | `core.Rig` books (`r.book`, `runtime.go:861`) and the gate's quarantine and truth state |
| trade prints | websocket `trade`, unfiltered (`wsx/wire.go:40-45`) | same `events` channel | same | same path into `core.Rig.Handle`; the sink is `noopSink` (`runtime.go:449-466`) so nothing is recorded, and the gate updates only on snapshot and delta frames (`wsx/gate.go:622-658`), so a trade has no effect the decision can see | none that the decision reads |
| connection state | `EventConnected`, `EventDisconnected`, `EventDisconnectReduce` | same `events` channel | `wsx/supervisor.go:194-257` | `applyEvent` `run.go:1260-1302` -> `Gate.OnConnectForUniverse`, `ApplyDisconnect`, `NoteDisconnectSustained`; a `ReconcileToken` goes back to the poller on `tokens` (cap 1, `run.go:741`; `offerToken` `run.go:1480`) | gate generation; forces an immediate portfolio poll |
| fills | REST `GET /portfolio/fills`, full walk with zero `since` (`wsx/portfolio.go:166`) | `chan wsx.PortfolioRead` (2; `readBuffer` `run.go:77`, made `:740`) | `Poller.Run`, `wsx/portfolio.go:180` | `applyRead` `run.go:1846` -> `wsx.ApplyPortfolio` (`:1851`) -> `eff.OwnedFill` -> `store.RecordFill` (`:1868`) and `applyPnLFill` (`:1905`) | `risk.Portfolio` (`r.pf`), PnL ledger, store |
| positions, resting orders | REST `GET /portfolio/positions` and `/portfolio/orders?status=resting` | same `reads` channel | same | same `applyRead`; positions overwrite last (`run.go:1919-1927`) | `r.pf` (wholesale order replace), `o.pnlQExch` |
| order acks (create) | return value of `rest.Client.Create` (`CreateResult`) in a dispatch worker | `chan writeResult` (2; `run.go:745`) | `dispatch.go:240-243` | `applyWriteResult` `run.go:3613`: `queue.AckPlace` (`:3869`); `pf.ApplyAck` for an ack-embedded `Filled` (`:3871-3878`); `o.pending[coid]` (`:3844`) | queue, `risk.Portfolio`, pending map |
| cancel results | return value of `rest.Client.CancelAndSweep` (`SweepResult`) | same `results` channel | `dispatch.go:535-562` | `applyWriteResult` cancel branch `run.go:3686-3705`: `noteSwept` (`:3693`); when `res.Absent` (set at `dispatch.go:562`) it sets `awaitCancelTruth` and forces a poll (`:3700-3705`) | swept-orders map, `absentOrders`, the placement gate |
| schedule, programs, balance, cross-check | REST loops | `schedules`, `programs`, `balanceThrottles`, `crossResults` (cap 1 each, `run.go:785-797`) | loops at `run.go:786-799` | `applySchedule` (`run.go:1614`), `applyProgramMembership`, `noteRESTThrottle` (`dispatch_throttle.go:62`), `applyCrossCheck` (`crosscheck.go:66`) | owner fields |
| owner -> socket | resnapshot request | `chan wsx.Command` (1; `run.go:742-743`) | `crosscheck.go:241` | `session.run` `wsx/session.go:271` | the socket (only writer) |

### 3.3 Where the decision is made

On the owner goroutine only, as a chain of pure functions over state the owner holds (no I/O):
`owner.evaluate` (`run.go:2405`) calls `owner.evaluateMarket` (`run.go:2494`), which runs the market state
machine `quote.NextMarket` (`run.go:2527`; defined `quote/machine.go:134`) and sizing `quote.SizesFor`
(`run.go:2582`; `quote/skew.go:223`), then `owner.decideSide` (`run.go:2808`) per side, which reads the book
through `pricingBook` (`crosscheck.go:179`) and decides with `quote.Decide(in, o.p)` (`run.go:2849`;
`quote/requote.go:206`). The intent goes to `owner.enqueue` (`run.go:2913`, `quote.Queue.Enqueue`
`quote/queue.go:791`); `owner.pump` (`run.go:2940`) admits it under the write budget (`Queue.Dequeue`,
`quote/queue.go:950`, called `run.go:2953`), `owner.build` (`run.go:3069`) turns it into a `rest.CreateOrder`
(`rest.Coid` `run.go:3157`, `rest.NewCreateOrder` `run.go:3161`) and hands it to the dispatcher
(`run.go:3004`). In a read-only process (`!cfg.Live`) `pump` stops before the send and records a would-write
(`run.go:2982-2997`).

### 3.4 Properties a standalone consumer would inherit or have to replace

- **Fills and order truth are polled, not pushed.** The websocket subscribes only `orderbook_delta` and
  `trade` (`wsx/supervisor.go:315-336`; `wsx/wire.go:27-45`); there is no `fill`, `user_orders` or
  `market_positions` subscription (no such string in non-test `wsx`). Fills, resting orders and positions
  arrive through three sequential walks per cycle, fills first with no `since` (`wsx/portfolio.go:165-170`),
  every `position_poll_s` (default 5 s, `cfg/params.go:136`) or immediately after a connect or disconnect
  token. Fills enter `risk.Portfolio` at three places only: the poll (`wsx/portfolio.go:427`), startup
  adoption (`lifecycle/startup.go:799`) and a create ack carrying `Filled > 0` (`run.go:3871-3875`).
- **A first placement waits for a SQLite commit before the POST**: reserve, await the commit (bounded by
  `permitWait` = `restTimeout` = 10 s, `dispatch.go:144`), `permit.Order()`, then `Create`, then bind
  (`dispatch.go:288-422`); a retry reuses the permit (`dispatch.go:292-294`). Every cancel includes a
  verifying resting-orders walk after a DELETE per listed order, and a named GET per order if the list lags
  (`rest/cancel.go:372-458`).
- **A verified cancel closes the placement gate until a fresh full poll cycle lands.** When the sweep reports
  the side absent, the owner sets `awaitCancelTruth`, drops the dependent intents and forces an immediate
  poll (`run.go:3694-3705`); placement stays blocked (`run.go:2600`) until one cycle whose fills, orders and
  positions walks all started after the cancel has been applied (`run.go:2043-2048`). A cancel-then-replace
  therefore costs the DELETE, a resting-orders walk, then three more sequential walks before the new POST.
- **Pacing lives in the harness, not in `rest`.** Owner tick 250 ms (`run.go:64`), requote debounce 250 ms
  (`cfg/params.go:129`), write bucket 5/s with burst 10 (`:133-134`, reserve for reducers `quote/queue.go:586`),
  per-endpoint read backoff (`read_throttle.go`). `rest` surfaces a 429 as `RateLimitError`
  (`rest/throttle.go:16`) and `Create` returns `CreateUnknown` on one without retrying (`rest/write.go:301-308`).
- **One owner, tiny buffers.** The book, position model, queue and capacity have no mutex and are touched only
  by the owner (`run.go:23-55`); hand-off buffers are deliberately small so a slow owner drops stale reads
  rather than queueing them (`run.go:66-84`).
- **The websocket is reconnected, never abandoned.** Backoff ladder 1, 2, 4, 8, 16, 32, 60 s (`wsx/supervisor.go:20`),
  F1 ping, pong and read-deadline timers on the injected clock (`wsx/session.go:162-266`).

## 4. Minimal standalone usage

`accountcheck` and `incentives` contain only reads: neither places or cancels an order or opens a websocket.
They are the precedent for steps (a) and (b); steps (c) to (e) are taken from the package definitions and the
harness's own use of them. All five sequences below compile against the real packages (`go vet`, scratch module
outside the repo). File:line is the definition, relative to `go/`; `rest/`, `wsx/`, `num/`, `cfg/`, `quote/` stand for `harness/<name>/`.

### 4.1 Call sequences

**(a) Sign and GET /portfolio/balance.** Precedent: `accountcheck/main.go:525,532,403`; raw GET
`accountcheck/main.go:186-187,404`.

```go
signer, err := feed.NewSigner()                   // feed/auth.go:43; ~/.kalshi/kalshi.pem + ~/.kalshi/env (NewSignerFrom :54)
doer := rest.NewHTTPDoer(signer, 15*time.Second)  // rest/client.go:276; HTTPDoer.Do rest/client.go:329 signs method+path
client := rest.NewClient(doer)                    // rest/client.go:416
bal, err := client.Balance(ctx)                   // rest/read.go:766 -> rest.Balance{Cents int64} (rest/read.go:761)
// raw form: doer.Do(ctx, rest.Request{Method: "GET", Path: "/portfolio/balance"})   // Request rest/client.go:27, Response :35
```

**(b) Walk /portfolio/orders.** Precedent: `accountcheck/main.go:200-209,470,503`; the same walker on
`/incentive_programs` at `incentives/main.go:183`.

```go
w := client.Walk(ctx, rest.EpOrders, url.Values{"status": {rest.StatusResting}}) // rest/page.go:104; EpOrders rest/client.go:170; StatusResting rest/client.go:220
if !w.Replaces() { /* w.Outcome, w.Pages, w.Err */ }                           // rest/page.go:90 (only a complete walk may replace state)
recs := w.Records("orders")                                                      // rest/page.go:93 -> []json.RawMessage
// typed: res := client.Orders(ctx, "", rest.StatusResting)                      // rest/read.go:237 -> OrdersResult{Walk; Orders []rest.Order}
```

**(c) Place an order.** No small binary does this; the only production caller is `cmd/harness/dispatch.go:374`.

```go
g, err := rest.NewWriteGuard(rest.NewHTTPDoer(signer, 10*time.Second),
          rest.WriteArm{Live: true, LiveOKPath: "/path/live_ok"})              // rest/guard.go:83, :38; optional, Create does not require it
c := rest.NewClient(g)
coid, err := rest.Coid("RUN1", 0, num.SideYes, 1)                              // rest/coid.go:52 -> "lipH-RUN1-000-yes-00000001"
order, err := rest.NewCreateOrder("TICKER", num.SideYes, 42, num.QtyFromFloat(1), 0, coid) // rest/wire.go:175; num.Side num/side.go:5; num.QtyFromFloat num/qty.go:46
res := c.Create(ctx, order, cfg.Default())                                     // rest/write.go:154; cfg.Default cfg/params.go:114
```

**(d) Cancel it.**

```go
resting := c.Orders(ctx, "TICKER", rest.StatusResting)                         // rest/read.go:237
sweep := c.CancelAndSweep(ctx, "TICKER", resting.Orders)                       // rest/cancel.go:355 -> SweepResult (rest/cancel.go:236): DELETE per order, then a complete resting walk
// an ack's id is enough: c.CancelAndSweep(ctx, "TICKER", []rest.Order{{OrderID: res.OrderID, Ticker: "TICKER"}})
// single DELETE without verification: c.Cancel(ctx, res.OrderID)             // rest/cancel.go:106; shard 0 only, no production caller
```

**(e) Open a websocket and receive orderbook deltas for one ticker.** Two routes.

```go
// (e1) raw frames; no LIP objects
conn, err := feed.Dial(ctx, signer, time.Now().UnixMilli(), []string{"TICKER"}) // feed/feed.go:115
b, err := conn.Read(ctx)                                                        // feed/feed.go:207 (or conn.Pump(ctx, ch) feed/feed.go:232)
info := wsx.InspectFrame(b)                                                     // wsx/frame.go:98 -> FrameInfo{Kind, Ticker, SID, ...}
// (e2) supervised: ping/pong, read deadline, reconnect with backoff
sup, err := wsx.NewSupervisor(signer, wsx.NewLiveDialer(), wsx.NewSystemClock(),
            cfg.Default(), []string{"TICKER"})                                  // wsx/supervisor.go:60; wsx/transport.go:130, :94
events := make(chan wsx.Event, 64)
go sup.Run(ctx, nil, events)                                                    // wsx/supervisor.go:161; Event wsx/session.go:79; EventFrame carries raw bytes
```

### 4.2 Which steps need LIP-specific objects

| step | standalone? | what it needs from the LIP side | what it does not need |
|---|---|---|---|
| (a) sign + GET balance | yes | nothing is constructed. At link time `rest` imports `core`, `feed`, `cfg`, `num`, `quote`, `risk` (`rest/*.go`), so the binary links them. | `cfg.Params`, `risk.*`, `quote.*`, `hstore` |
| (b) walk orders | yes | nothing constructed. The typed `Orders` result carries `quote.Side` (alias of `num.Side`, `quote/queue.go:38-42`) and `num.Qty`, and flags `Order.Ours` by the `lipH-` coid prefix, so every non-LIP order reads as foreign (`rest/read.go:177-200`). The raw `Walk` has no such coupling. | same |
| (c) place an order | **no**, not without LIP types | a `cfg.Params` value: `Create` reads only `RetrySameCoidMax` and a zero value means one attempt (`rest/write.go:190-193`). A `quote.Side` (`num.SideYes` works). A LIP coid from `rest.Coid`: `NewCreateOrder` rejects any coid that is not `lipH-{run}-{idx:03d}-{side}-{seq:08d}`, round-trips it and matches its side (`rest/wire.go:216-236`). Price is an integer cent in 1..99 (`quote.ValidPrice`, `quote/cross.go:13-34`; `rest/wire.go:203`). The wire body is fixed: `post_only` true, `good_till_canceled`, `taker_at_cross` (`rest/wire.go:164-165,256`, enforced `:267-305`), so no taker, IOC or expiring order can be sent through this path. | `risk.*`, `hstore`. The harness's reserve, commit and permit sequence (`cmd/harness/dispatch.go:288-369`) is its own wrapper; `Create` takes a `rest.CreateOrder`, not a permit. |
| (d) cancel | yes | nothing constructed. `CancelAndSweep` needs `[]rest.Order`, from a walk or a literal with `OrderID` set. `Cancel` routes by order id alone, which defaults to exchange index 0 (`rest/cancel.go:107-111`), so `CancelAndSweep` (market-routed through `cancelForMarket`, `rest/cancel.go:115-129`) is the usable path. | everything LIP |
| (e1) raw websocket via `feed.Dial` | yes | nothing. Frozen, hash-pinned package: no keepalive, no read deadline, no reconnect (`feed/feed.go:103-114`); always also subscribes the unfiltered `trade` channel (`:163-180`). | everything LIP |
| (e2) supervised websocket via `wsx.NewSupervisor` | **no**, not without `cfg.Params` | a `cfg.Params` that passes `Validate()` (`wsx/supervisor.go:72`); the session reads `WSPingInterval`, `PongTimeout` and `ReadDeadline` (`wsx/session.go:162-246`) and the supervisor `DisconnectReduce` (`wsx/supervisor.go:255`). `cfg.Default()` supplies one. The URL is fixed to `wsx.WSURL` (`wsx/supervisor.go:86`); only a custom `wsx.Dialer` can redirect it. | `wsx.NewGate`, `core`, `hstore` |
| (e3) a book from those frames | **no**, not without `core.Rig` | `core.NewRig(sink, map[string]float64{ticker: target})` (`core/rig.go:126`) plus a three-method `core.Sink` (`core/rig.go:85-89`); `target` is the LIP Target Size and only feeds `Book.Qualifies()` (`core/book.go:37,208`). `InspectFrame` classifies and validates frames but does not return levels. | `risk.*`, `hstore` |

The impossible-without-LIP-objects list is therefore: **(c)** (`cfg.Params`, `quote.Side`/`num.Side`, a
LIP-format coid, post-only GTC only), **(e2)** (`cfg.Params`), **(e3)** (`core.Rig` and `core.Sink`).
`risk.*` and `hstore` are not needed by any of the five steps.

## 5. Limits

- Nothing was run against the exchange or an account, so "impossible" means by construction or type, not by
  observed network behaviour.
- Item 2's package-qualified list is exact; the member-level tables are exact for direct selections. Methods
  reached through interface values are traced by hand in §2.4 for `rest.Client` only.
- `lifecycle`, `qual`, `hstore` and `risk` internals were not read beyond the interfaces and constructors named.
- `cmd/rig` is outside this scope. It is the other in-tree consumer of `feed.Dial` and `Conn.Pump`
  (`cmd/rig/main.go:466,475`), cited only as a pointer for (e1).
- The prompt's wording "walk /portfolio/orders" is read as the cursor walk (`rest.Client.Walk`); the typed
  `Orders` decoder is first-error-wins, so one undecodable order invalidates the whole walk (see the header
  comment of `cmd/conform/main.go`).
