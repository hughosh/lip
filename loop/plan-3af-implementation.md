# lip-3af implementation plan — wire cmd/harness, the pilot profile

Written 2026-08-07 after `lip-eyq` landed green (130/130, commit `7bd5803`) and a
three-way read-only audit of `lip-bw0` plus a two-way audit of the runtime
surface. This is step 3 of 3 on the critical path to a runnable binary:
`lip-eyq -> lip-bw0 -> lip-3af`.

Operator directive standing from 2026-08-07: get the harness up and running
ASAP; cut audit/red-team ceremony with diminishing returns, NOT verification.

## The finding that shapes everything

**Every component is built, unit-tested and mutation-guarded. Nothing is
composed.** `cmd/harness` is `main.go` (29 lines, `os.Exit(2)`) plus
`monitor.go`. There is no `RunTick`, no orchestration, no composition root
anywhere in `go/` -- tests included. `quote.Decide` has ZERO callers outside its
own tests. `wsx.Poller` is complete and nothing consumes its output channel.

So `lip-3af` is not "finish the harness". It is "write the only file that has
ever known these packages exist at the same time". That is a different risk
profile: the parts are individually proven, and every bug will be at a seam.

### What EXISTS (do not rebuild)

| Piece | Where | Note |
|---|---|---|
| Request signing | `go/feed/auth.go` | RSA-PSS/SHA-256, `PSSSaltLengthEqualsHash`. HASH-PINNED, complete |
| REST | `rest.NewClient(NewHTTPDoer(signer, timeout))` | Create/Cancel/CancelAndSweep/ConfirmCoid/Positions/Orders/Fills/Balance |
| WS session | `wsx.NewSupervisor(signer, dialer, clock, params, tickers)` | `feed.NewLiveDialer()`, `wsx.NewSystemClock()` |
| Poll cycle | `wsx.NewPoller(src, clk, interval)` + `Poller.Run(ctx, tokens, out)` | coalesces overruns, polls through outages, reacts to reconnect |
| Portfolio apply | `wsx.ApplyPortfolio(g, pf, own, bind, read, mode, now, p)` | order: orders -> fills -> positions. Do NOT reorder |
| Quote decision | `quote.Decide(in RequoteInput, p) Requote` | `AllowPlaceThenCancel` zero value false = pilot requirement, free |
| Priority queue | `harness/quote/queue.go` | pure, no I/O |
| State machine | `quote.NextGlobal` | the ONLY global transition (H-HALT-2) |
| Risk | `risk.NewPortfolio`, `Snapshot`, `A5Tracker`, `MonitorState` | |
| Store | `hstore.Open(StoreConfig{DBPath, AnomalyLogPath})` | 5 records, ownership ledger, `DispatchPermit`, `TakeResults`, `Wake`, `Drain`, `Shutdown`, `Rejections` |
| Lifecycle | `GlobalController`, `Startup`, `ForeignGuard`, `DrainTracker`, `SignalController`, `AcquireInstanceLock`, `LaunchdPlan`, sleep detector | all green, all mutation-guarded, ZERO production callers |
| Monitor loop | `cmd/harness/monitor.go` | needs a snapshot publisher that does not exist |

### What DOES NOT EXIST (this bead)

1. **Any config mechanism.** No file loading, no env convention, no flags in
   production Go. `cfg.Default()` + `Validate()` is the whole surface.
2. **DB provisioning.** `hstore.Open` requires the DB to already exist --
   "opened, never created fresh".
3. **The composition root.** Nothing constructs `Gate`, `Portfolio`, `Store`,
   `Supervisor`, `Poller`, `Startup` together.
4. **The poll loop.** Nothing consumes `Poller.out` -> `ApplyPortfolio`.
5. **The quote loop.** Nothing calls `quote.Decide` or feeds the queue.
6. **The dispatch path.** Nothing turns a dequeued leg into
   `ReserveOrder` -> drain `TakeResults` -> `DispatchPermit.Order()` ->
   `rest.Create` -> IMMEDIATE `BindOrder`.
7. **A snapshot publisher** for the monitor's `atomic.Pointer[risk.Snapshot]`.
8. **Signal handling.** No `signal.Notify` in `cmd/harness` at all.
9. **The lifecycle wiring** the `lip-bw0` audit itemised: instance lock before
   first REST, `RecordGlobalState` (H-HALT-4's `state_event` mirror),
   `hstore.Shutdown` on the stop path, `hstore.Rejections` in the result loop,
   plist install + `caffeinate -is` exec.

### Explicitly OUT of scope

**Market selection.** It does not exist in Go and is not needed. Selection is
Python-only (`q1select.py`); `risk.Snapshot.SelectedTickers()` only READS a
pre-populated flag. The pilot profile is "one operator-chosen ticker", so the
ticker is a CONFIG FIELD, not an algorithm. Do not build ranking or
`can_close_early` exclusion logic -- `quote.MarketState.TradingClosed` is
already authoritative and consumed.

## Design decisions

### D1. Config: one JSON file, one `-config` flag, overlaying `cfg.Default()`

40 `§16` params plus ticker, `S`, credential paths, DB paths and run id. Flags
do not scale to that and env vars leave no audit trail -- and §16's parameter
table is the thing an operator most needs to see verbatim when something goes
wrong. A file that OVERLAYS `cfg.Default()` means the file names only
deviations, so a diff against the spec's defaults is the config review.

`Validate()` runs after the overlay, always. An unknown key is a hard error, not
a warning: a typo'd knob that silently keeps its default is a config that lies.

### D2. The refusal to start is REPLACED, not deleted

`main.go`'s current refusal is structural on purpose -- "a half-wired trading
binary that starts and does almost nothing is exactly the artifact that gets run
'just to see'". It gets replaced by a NARROWER refusal, not removed:

- refuse without an explicit `-config`; there is no default config path;
- refuse if the instance lock is held;
- refuse if the halt latch is set and `-resume` was not passed (§10.4 is an
  OPERATOR action, and this is where it becomes one);
- refuse if `S > 1` without an explicit `-rung` naming the ladder step, so the
  canary is what you get by default and raising size is a deliberate act
  (pilot-plan §1: "Build once; raise the knob").

### D3. Dispatch is a single-threaded owner goroutine

H-ORD-6's two-stage commit, `DispatchPermit`, and "ONE REST writer" all say the
same thing. One goroutine owns: dequeue -> reserve -> await permit -> create ->
bind. No second path may call `rest.Create`. This is also what makes
`AllowPlaceThenCancel=false` enforceable rather than aspirational.

### D4. `BindOrder` is called on the `CreateResult`, inline

Not batched, not deferred to the next poll. `lip-6w5` v3's note is explicit:
every cycle between ack and binding is a cycle where our own fills are DEFERRED
and `q_local` is behind the account, and past 120 s it raises SEV2
`FILL_UNCLASSIFIABLE`. The `wsx` order-walk rebind is the safety net for a lost
binding, one poll late by construction -- the normal path must not rely on it.

## Order of work

### Phase 1 (SERIAL, me) — the skeleton other work depends on
- `config.go`: the config struct, JSON load, overlay onto `cfg.Default()`,
  unknown-key rejection, `Validate()`.
- `runtime.go`: the composition root — a `rig` struct holding every
  collaborator, plus its constructor and teardown. Defines the types phases 2
  and 3 build against. NO loops yet.

### Phase 2 (PARALLEL, three agents, disjoint files)
- `provision.go` + `deploy.go` — DB creation (refuse if exists), plist render +
  install, `caffeinate -is` exec check.
- `dispatch.go` — the owner goroutine: dequeue, reserve, permit, create, bind.
- `shutdown.go` — `signal.Notify` -> `SignalController` -> `DrainTracker` ->
  `hstore.Shutdown`; plus `RecordGlobalState` and the `Rejections` result loop.

### Phase 3 (SERIAL, me) — the loops and the seam tests
- `run.go`: poll loop (`Poller.out` -> `ApplyPortfolio`), quote loop
  (`quote.Decide` -> queue), snapshot publisher for the monitor.
- `main.go`: flag parsing, the narrowed refusal, wiring, exit codes.
- Seam tests. Every bug in this bead is at a seam, so the tests are
  integration-shaped: a fake exchange driving a full tick.

### Phase 4 — gates + catalogue
Run `--only` on any mutation whose anchor moved, THEN the full 130. Add
`M-3AF-*` entries for the seams that carry risk: dispatch without a permit,
bind deferred past the poll, a second REST writer, the lock not held,
`AllowPlaceThenCancel` flipped true.

## Standing constraints (unchanged)

- `notes/harness-spec.md` SHA-pinned. `go/core`, `go/feed`, `go/store`,
  `go/cmd/rig` hash-pinned. `harness/quote`, `harness/lifecycle`,
  `harness/hstore`, `cmd/harness` are editable.
- Every non-test Go file ends `// confidence: high`. `scripts/check.py` rejects
  `t.Skip`, `testing.Short`, stub panics, `_ = err`.
- New SQLite test DBs under `t.TempDir()` ONLY. Never touch `lip/rig.db`,
  `lip/lip.db`, or any `*.db`.
- `perl` is `/usr/bin/perl`. Python `/Users/hugh/kek/.venv/bin/python`. Go
  `/usr/local/bin/go`, `CGO_ENABLED=0`. `timeout(1)` not installed.
- Re-run the anchor audit after ANY signature change; it validates `old` only,
  so also `--only` the affected ids to prove the REPLACEMENT still compiles.
- Do NOT edit under `go/` while gates or the negative control runs.

## The line I do not cross

This bead ends with a binary that CAN trade. Building it, testing it and running
the gates is in scope. **Running it against the live account is not** -- not the
S=1 canary, not a "quick check". `pilot-plan.md` §0 states the precondition the
code cannot verify:

> the Kalshi account contains exactly the pilot capital and nothing else. This
> is the single most effective risk control available and it costs nothing.

...because the balance stops being a bound "if the account holds other funds
while the capital-accounting code is the thing that is wrong". Whether that
account exists and is cleanly funded is an operator confirmation, and it comes
before any run rather than after.
