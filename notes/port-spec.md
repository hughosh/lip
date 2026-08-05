# Port spec — `lip/rig.py` → Go

**Status:** Phase A contract. Written 2026-07-24, before any semantic Go was written.
**Audience:** whoever (human or agent) implements a unit of the port.
**Rule:** this document outranks your judgement about what good Go looks like.

---

## 0. The one-sentence contract

Phase A is a **faithful, bug-for-bug translation**. If the Python does something
that looks wrong, the Go does the same wrong thing, and you do not mention it in
code as a defect — it is listed in §6 as deferred work and will be fixed
separately, against a measured diff, in Phase B.

The port is finished when a captured raw websocket tape replayed through Python
and through Go produces **identical rows**, not similar ones.

---

## 1. Why this document exists in this form

Published agentic-rewrite practice converges on a handful of mechanisms. The
ones that apply at this scale, and what each one is doing here:

- **A written "do not idiomize" contract, with the reason attached to each
  rule.** Bun's Zig→Rust rewrite used a `PORTING.md` that banned tokio and
  `async fn` by name, because those are precisely what an agent reaches for
  unprompted. §3 and §4 are that document. Reasons are attached because a rule
  without a reason gets unlearned the moment it becomes inconvenient.
- **A table pinning every semantically ambiguous mapping before translation.**
  Bun used a `LIFETIMES.tsv` so parallel agents could not each reinterpret the
  ambiguous cases. §4 is that table. At 672 lines it is exhaustive rather than
  sampled.
- **A layered oracle, cheapest gate first.** Google's internal migration
  pipeline and Airbnb's per-file state machine both gate expensive checks behind
  cheap ones. §2.
- **The terminal gate is behavioural, never "it compiles."** Kaizen measured
  LLM translations that compiled 100% of the time and were semantically correct
  15% of the time.
- **Self-attestation counts for nothing.** On code containing deliberate traps,
  one controlled study found 39.7% semantic drift, of which 31.7% was
  self-endorsed as correct by the model that produced it. "I preserved the
  rounding" is not evidence. The fixture is evidence.
- **Retry by feeding the verbatim failure back.** Airbnb's explicit finding was
  that brute-force retry carrying the actual validation error beat cleverer
  prompting. §7.
- **Escalation is retry-budget → human, not → bigger model.** No surveyed source
  (Google, Airbnb, Amazon Q) escalates to a larger model on failure.
- **There is no human code review, and that is a hard constraint, not a
  shortfall to apologise for.** Every surveyed migration terminates escalation
  at a human reader — Airbnb after 50–100 retries, Google after three rounds,
  Amazon Q at hand-back. Removing that backstop means the oracle carries 100% of
  the correctness burden, so the oracle itself has to be verified. §2 gate 7 is
  the substitute: deliberately break each rule in §4 and confirm the gate
  catches it. A gate that has never been shown to fail is not evidence.
- **Watch for load-bearing detail that reads as cruft.** The ScanCode
  Python→Rust port had an agent silently strip copyright and licence notices
  because nobody said not to. The equivalent here is named in §4 as P20.

Sources: bun.com/blog/bun-in-rust; cursor.com/blog/agent-swarm-model-economics;
Airbnb "Accelerating Large-Scale Test Migration with LLMs"; arXiv 2501.06972 and
2504.09691 (Google); arXiv 2607.04058 (Kaizen); arXiv 2605.21537 ("Articulate
but Wrong"); aboutcode.org/blog/agentic-scancode-port-case-study.

Two honest gaps in the literature, which is why the rest of this file is
hand-built rather than borrowed: nobody publishes a byte-identical replay oracle
at this granularity (the closest analogues accept test-suite-pass or a ~1e-3
numeric tolerance), and nobody has a good mechanism for "preserve this specific
quirk deliberately."

---

## 2. The acceptance oracle

Frozen artifacts. **No agent may modify any of these, for any reason.** Not to
make a case pass, not to "align" them with the Go, not to fix what looks like a
bug in them. Widening a tolerance, skipping a column, filtering rows out of the
comparison, or regenerating a fixture from the Go's own output are all ways of
making the gate pass without making the code correct, and all of them are
prohibited. Modifying a frozen artifact is itself a gate failure.

Bun's rewrite hit exactly this: agents stubbed out functions to make a build go
green, and ran destructive `git` commands to escape a failing state. Assume the
same pressure applies here.

| Artifact | Role |
|---|---|
| `lip/rig.py` | ground truth for behaviour |
| `lip/replay.py` | reference replayer; already validated at 809 shared rows / 0 mismatches vs the live DB |
| `lip/raw-*.jsonl.gz`, `lip/archive/raw-*.jsonl.gz` | the tapes |
| `lip/testdata/prices.tsv`, `sizes.tsv` | pinned CPython numeric conversions |

Gates, in order. Each is cheaper than the next; failing one means you do not run
the next.

1. **Builds.** `CGO_ENABLED=0 go build ./...` and `go vet ./...`.
2. **Numeric fixture.** `go test ./core/ -run Parse`. 2,001 price strings and
   33,686 size strings must match CPython exactly, including the float64 bits of
   the intermediate product. 46 of the price strings discriminate banker's
   rounding from half-away-from-zero; `TestFixtureDiscriminatesBankersRounding`
   fails if that discriminating power is ever lost.
3. **Named quirk tests.** Standalone unit tests pinning each row of §4 that has
   an observable behaviour, independent of the tape. These exist so a quirk is
   caught by name rather than by luck.
4. **Differential replay — the terminal gate.** Replay the same tape through
   `replay.py` and through `cmd/replay`, then join the two `fill` tables on
   `trade_id` and require **zero** mismatches on every column except
   `mid_1m` / `mid_5m` / `mid_30m`. Row counts must match exactly in both
   directions (no row present in one and absent in the other).

   The excluded three are resolved by a wall-clock task in the live rig and are
   not a function of the tape; they are out of the surface by construction.
   Everything else — `ts_ms`, `ticker`, `resting_side`, `price`, `size`,
   `taker_side`, `trade_through`, `pre_best_yes`, `pre_best_no`, `pre_yes_size`,
   `pre_no_size`, `pre_mid`, `pre_spread`, `depth_at_price`, `book_lag_ms`,
   `source` — is in.

5. **`reference` table, also a hard gate.** Earlier planning treated this as a
   soft gate on the theory that `sum(dict.values())` follows Python insertion
   order while Go map iteration is randomized, making the float sums
   irreproducible. That is avoidable: see P2. With an insertion-ordered book the
   `reference` rows are bit-identical too, and are compared on `(ts_ms, ticker)`
   with zero tolerance.

6. **Stats.** `trades`, `fills`, `through`, `gaps`, `refs`, `quarantined` and
   the final `stale` set must match the Python replay exactly.

7. **Negative control — the gate on the gate.** Without a human reader, gates
   1-6 are the entire safety argument, and an untested gate is worth nothing. So
   break the code on purpose and confirm the gate notices.

   For each mutation below, apply it to an otherwise-passing Go build, run gates
   1-6 against a real multi-hour tape, and record which gate caught it and how
   many rows differed:

   The suite is `scripts/negative_control.py`; live results are in
   `notes/negative-control.md`. It covers P1, P2 (both halves), P3, P4, P7, P9,
   P10, P12, P15, P24, and the live-path rules P25, P26, P27 and P28.

   **Any mutation that survives every gate is a defect in the oracle**, and the
   port does not proceed until the gate is strengthened to catch it — unless the
   mutation is positively established to be *behaviourally inert*, which must be
   argued in §4 and never assumed. An unexamined "inert" label is exactly how a
   real oracle hole would hide.

   Two harness rules, both learned the hard way on the first run:

   - **Stage `testdata/` alongside `go/`.** The fixture tests resolve
     `../../testdata`; without it every fixture test fails in every copy, and
     all ten mutations report as "caught" while actually catching nothing.
   - **Run an unmutated control first.** It must pass every gate. This is the
     only thing standing between a broken harness and ten false positives, and
     it caught both of the harness bugs above immediately.

   Mutations must be semantic. Two of the originals were caught only because
   they left an import unused, testing the Go compiler rather than the oracle.

   **Result (2026-07-24, live tape with real Target Sizes): 18 of 19 caught, 1
   inert.** Every one was caught at gate 2/3 — the cheap unit tests — rather
   than the differential replay. That is strictly better than predicted, and it
   is the layering working as intended.

   The nine added with the feed layer are worth reading as a group, because
   seven of them (m13-m19) violate rules **no tape can exercise**: a replay has
   no reconnect, and the tape writer sits upstream of the replay rather than
   inside it. Their being caught is entirely down to named unit tests. The other
   two (m11, m12) break P2's ordered bookkeeping and exist to answer a question
   the earlier suite left open — m02 shows that Sum's *consumption* of the order
   is undetectable, and m11/m12 show that the *bookkeeping* is not. Those are
   different claims and only the first one is inert.

   Three mutations initially survived *everything*, and all three for the same
   reason: **the tape is too benign to exercise the path.** `book_lag_ms` is
   never negative in captured data (min 0 across 4,034 fills), there are zero
   duplicate `trade_id`s across 544k frames, and with no sequence gaps the
   quarantine branch never executes at all. Each is now pinned by a named unit
   test instead of hoping a future tape contains it. This is the single most
   valuable thing gate 7 produced: **the differential replay alone would have
   certified a port with three undetectable defects.**

---

## 3. Working rules for implementers

- **Do not idiomize.** Preserve control flow shape where it carries semantics.
  Specific bans, each with its reason, are in §4. *This rule is suspended, under
  stated conditions, during the optimisation phase — see §10. It is not repealed:
  it binds until §10's entry condition is met, and §10 keeps it binding on
  everything observable.*
- **Do not "fix" anything.** If you believe you have found a bug, check §6
  first — it is probably already known and deferred. If it is not in §6, stop
  and escalate; do not fix it in passing.
- **Do not restructure concurrency.** `core` is single-threaded, allocates no
  goroutines, reads no clock, and performs no I/O. Anything that needs a clock
  or a socket lives in `feed`, `store` or `cmd`. This is what makes `core`
  differentially testable.
- **No `time.Now()` anywhere in `core`.** The Python deliberately runs a single
  exchange-time clock domain; introducing a second one silently mis-attributes
  trades to post-trade book states.
- **Comments that state a retraction, a hazard, or a deliberate omission are
  load-bearing.** Carry them across. See P20.
- **If you need a paragraph to justify a workaround, the code is wrong.** Bun's
  rule, and it holds here. A long comment explaining why a divergence is
  acceptable is a signal to stop and escalate, not to proceed.
- **Never stub, skip, or short-circuit to reach a green result.** No `t.Skip`,
  no `if testing.Short()`, no returning a canned value from a function you have
  not finished, no filtering rows out of the comparison. A red gate that is
  honestly red is worth more than a green one that is not.
- **Never run destructive `git` or filesystem commands to escape a failing
  state** — no `git reset --hard`, no `git stash`, no deleting a DB or tape to
  force a clean run. Both were observed failure modes in Bun's fleet.

---

## 4. Preservation table

Every row is a place where a reasonable Go rewrite silently diverges. `rig.py`
line references are against the 672-line version current at 2026-07-24.

| # | Site | Python behaviour | Required Go behaviour | Why it matters |
|---|---|---|---|---|
| **P1** | `rig.py:331,332,449-450,469` | `round(float(p) * 100)` — banker's rounding on the exact binary product | `core.ParsePriceCents` → `int(math.RoundToEven(f * 100))`. **Never `math.Round`.** | 8.8% of observed prices are fractional cents; 46 distinct wire strings round differently under the two modes. Note `float("0.0150")*100` is `1.4999999999999998`, so it rounds *down* under both — parsing the decimal text instead of multiplying in float64 gets this wrong. |
| **P2** | `rig.py:319` | `sum(book.yes.values())` iterates in CPython dict insertion order | Book sides use an **insertion-ordered** int→float map replicating CPython dict semantics: append on new key, keep position on overwrite, re-append after delete-then-reinsert. | **Downgraded, honestly.** The original claim was that iteration order drove the depth sums because float addition is not associative. That was wrong — P24's compensated summation is order-independent across 700k randomised trials including adversarial magnitude mixes, and `Sum` is the only consumer of insertion order (`Max` and `SortedDesc` are order-free). The ordering is kept because it is faithful, free, and would become load-bearing again if Phase B ever sums without compensation — but it is **not observable in any output**, and the negative control records that explicitly rather than pretending otherwise. Be precise about *what* is undetectable, because the two halves differ: the ordered **bookkeeping** (append on new, keep position on overwrite, re-append after delete) is pinned at gate 3 by `core/levels_test.go` through the public `Keys()`, and a mutation of it is caught. What no gate can detect is a change to **which order `Sum` consumes**, which is what m02 does and why m02 is inert. `Keys()` exists so that deleting the ordering wholesale is a failing test rather than a failing compile — §2 rejects compiler catches as evidence. |
| **P3** | `rig.py:196` | `if h_ts < ts and (chosen is None or h_ts >= chosen[0])` — `>=`, so on a timestamp tie the **last** matching checkpoint wins | Iterate history oldest→newest, replace on `>=`, not `>` | Ties are common: deltas and their trade arrive in the same millisecond. `>` picks the wrong book. |
| **P4** | `rig.py:193-197` | No early `break` in the `state_before` scan | Scan the entire history every time | Deliberate. Checkpoints are appended in arrival order; one out-of-order entry would otherwise hide every later one. The comment says so — keep it. |
| **P5** | `rig.py:198-203` | If every checkpoint is newer than `ts`, fall back to `history[0]` (the **oldest**) and return a **negative** lag | Same, including the negative lag | Happens whenever a trade's deltas are processed before the trade message. |
| **P6** | `rig.py:199-200` | Empty history → return copies of the **live** book with lag `None` → `book_lag_ms` is SQL NULL | Same; distinguish "no lag" from "zero lag" | A zero would be indistinguishable from a perfectly fresh checkpoint. |
| **P7** | `rig.py:366` | `int(lag * 1000)` truncates **toward zero**, including for negative lags | `int64(lag * 1000)` — truncates toward zero. **Never `math.Floor`.** | `-1.7` must give `-1`, not `-2`. |
| **P8** | `rig.py:301-303` | `if ts_ms:` — a **falsy** guard, so `0`, `None` and absent all skip the update | Treat `0` as "no update", not merely absent | A zero timestamp must not reset or advance the watermark. |
| **P9** | `rig.py:232-250` | `qualifies()`: falsy `target` → 0; empty side or `max(price) >= 100` → 0; `for/else` → if the walk never reaches target, **0**. The walk runs `sorted(d, reverse=True)` — **highest price first** — and accumulates with a plain `total += d[p]` | Same, all four exits, same direction, same naive addition | Mirrors `score.qualify()`: a side that never reaches Target Size has its qualifying set *cleared*, not truncated. Retaining a partial walk is the exact failure codex flagged as most likely. **The DIRECTION is a separate rule and was undefended until 2026-07-25**, when gate 7 mutation m29 reversed it and survived every gate including a byte-identical replay of 12,726 `reference` rows. It survives for a structural reason, not a tape accident: sizes are strictly positive (P10, P22), so the running total is monotonic and `reached` is true iff the *whole side* sums to Target regardless of order. Only float rounding within an ulp of Target can distinguish the directions, and real books never come that close. `TestP9_QualifyingWalkIsHighestPriceFirst` constructs a case that does — 1e16 absorbs two 1.0s walked high-first but not low-first — and is the only thing pinning the direction. Note this also means the naive `total +=` here must NOT be "upgraded" to P24's compensated `Sum`. |
| **P10** | `rig.py:168-174` | `apply_delta`: keep iff `new > 1e-9` (strict), else delete the key | Same epsilon, same strictness | Negative and near-zero results both delete. |
| **P11** | `rig.py:179-183` | Eviction: `while len(history) > 1 and (history[0].ts < cutoff or len(history) > HISTORY_MAX)`, `cutoff = ts - 5.0` from the **current** ts | Same compound condition, same order | The `len > 1` guard means history is never fully emptied, so a quiet market still has a book to attribute against. |
| **P12** | `rig.py:317,351,369` | `INSERT OR REPLACE` for `reference` (PK `ts_ms,ticker`, last wins); `INSERT OR IGNORE` for `fill` (PK `trade_id`, **first** wins) and `pending_mid` | Same conflict clauses | A repeated `trade_id` must keep the first row's book state, not the last. |
| **P13** | `rig.py:431-441` | Seq-gap check runs **before** any mutation; `self.seq[sid] = seq` is assigned **before** the gap test; on a gap the frame is dropped entirely via `return` | Same order: record seq, then test, then drop the whole frame | The frame's book update is discarded along with it. Assigning seq before the test means the next frame is judged against the post-gap value. |
| **P14** | `rig.py:443-459` | Snapshot: `history.clear()` then exactly one checkpoint at the watermark. Snapshots carry **no** `ts_ms` on the wire, so the watermark is unchanged | Same | Confirmed against the tape: `orderbook_snapshot` msgs have only `market_id`, `market_ticker`, `yes_dollars_fp`, `no_dollars_fp`. |
| **P15** | `rig.py:461-473` vs `475-480` | **Asymmetric:** a delta for a stale market returns *before* `_watermark`; a trade for a stale market advances the watermark *first*, then returns | Preserve the asymmetry exactly | Looks like an inconsistency. It is observable in `ts_ms` on every subsequent row. |
| **P16** | `rig.py:325-327` | `_record_trade` returns before any stats increment when the ticker is not in the universe | Same | `stats["trades"]` counts universe trades only, not the whole-exchange tape. 98.6% of decoded trade frames are discarded here. |
| **P17** | `rig.py:333,337` | `taker = m.get("taker_side") or ""`; then `resting_side = "no" if taker == "yes" else "yes"` | Same: anything that is not exactly `"yes"` — including empty and absent — yields resting `"yes"` | **Side semantics are confirmed correct and must not be "fixed."** A yes-taker lifts a resting NO bid; a no-taker hits a resting YES bid. |
| **P18** | `rig.py:359-364` | `pre_yes_size`/`pre_no_size` are `0.0` (not NULL) when that side is empty; `pre_spread` is NULL when either best is None; `pre_mid` is NULL when either side is empty | Same NULL-vs-zero split | Three different absence encodings in adjacent columns. |
| **P19** | `rig.py:364` | `depth_at_price = resting_pre.get(price, 0.0)` uses the **print** price, not the touch | Same | This is deferred item §6.2. It is wrong on purpose in Phase A. |
| **P20** | `rig.py:19-47` | The module docstring explicitly **retracts** an earlier claim that `trade_through` is assumption-free proof of a fill | Carry the retraction across verbatim in the Go package doc | Kalshi's maker-side self-trade prevention cancels the maker and continues to worse prices, so a print below the touch can occur without the touch order filling. An agent will read this as prose cruft and drop it. It is the single most important sentence in the file. |
| **P21** | `rig.py:77` | `HORIZONS` is a dict literal, so iteration order is `1m, 5m, 30m` | Use an ordered slice, not a map | `pending_mid` insert order. |
| **P22** | `rig.py:164-166` | `apply_snapshot` filters `s > 0` (strict) and, on a duplicate price, the later size wins while the key keeps its **first** position | Same, per CPython dict-assignment semantics | Interacts with P2. |
| **P23** | Go-side hazard, no Python counterpart | — | **Never write a float computation as a Go untyped constant expression.** Any arithmetic that must match Python has to run on `float64` *variables* at runtime. | Go evaluates untyped constant expressions at **arbitrary precision** at compile time: `0.1 + 0.2 + 0.3` folds to exactly `0.6`, where Python (and Go at runtime) give `0.6000000000000001`. This was caught by a test that correctly declared itself vacuous — it would otherwise have silently proved nothing. The live risk is any future constant-folded price or size arithmetic, and any *test* that computes its own expected value as a constant. |
| **P24** | `rig.py:319` | `sum()` over floats is **Neumaier compensated summation**, not naive left-to-right addition | `Levels.Sum` carries the compensation term: `t = f+x; c += (|f|>=|x|) ? (f-t)+x : (x-t)+f; f = t`, returning `f+c` | CPython's `sum()` has a float fast path that tracks the rounding error lost at each step and folds it back at the end. On realistic two-decimal book sizes this disagrees with naive accumulation **often**, by one ULP. Found by the differential gate: `fill` was byte-identical on the first run while `reference` showed 2,503 `yes_depth` and 4,628 `no_depth` one-ULP diffs. **P2 and P24 are both required** — correct order with naive addition still fails, and compensated addition in the wrong order still fails. |

### 4a. The live-path rules (P25-P28)

P1-P24 cover the pure surface, which the differential replay can see. P25-P28
cover `resolve_mids` and `run` (`rig.py:379-569`) — the socket, the clock and the
tape writer. **The differential gate is structurally blind to all four**: a tape
is the *input* to replay, so nothing that produces one is on the replayed path,
and no reconnect ever occurs during a replay. They are therefore carried by named
unit tests alone, which is the same position P5/P7/P13 were in before gate 7
exposed it — a rule the tape cannot exercise is a rule the tape cannot defend.

| # | Site | Python behaviour | Required Go behaviour | Why it matters |
|---|---|---|---|---|
| **P13a** | `rig.py:521` vs `431-441` | `json.loads(raw)` parses the WHOLE frame, and only then does `_handle` run the sequence-gap check | Keep the TWO-STAGE decode: envelope first, gap check, message second. A single union decode of envelope+message is forbidden | Found while looking for an optimisation, not by a gate. Decoding both halves at once would remove the `json.RawMessage` copy — 8.43% of allocations — and looks free. It is not. The two arrangements agree on a malformed frame (both error before the gap check) and on a well-formed frame with a bad field type (both reach the gap check, then fail). A union decode breaks the second case: the frame would fail *before* `self.seq[sid] = seq` is recorded, so the NEXT frame is judged against a stale sequence number and a gap is fabricated or hidden — and P13 makes that quarantine every book on the subscription. The staging is load-bearing, not an artifact of how the port was written. |
| **P25** | `rig.py:548-564` | On an **abnormal** disconnect only — the `except (ConnectionClosed, OSError)` path — every book's `yes`, `no` and `history` are cleared, `ref_yes`/`ref_no`/`gate` are set back to `None`, `stale` is reset to the **whole** universe, and `self.seq` is cleared, after a backoff sleep and a commit | `core.Rig.ResetOnReconnect()` does all five, and `NeedsResnapshot` too — but **only on the abnormal path** | Five separate pieces of state and dropping any one is silent. Keeping the levels attributes a post-gap trade to a pre-gap book. Keeping `ref_*`/`gate` suppresses the first `reference` row after reconnect, because change detection compares against a value that is no longer true of any book. Keeping `seq` fabricates a gap against a sid the new connection renumbered — or worse, hides one. |
| **P25a** | `rig.py:505` vs `548` | A **clean** close is not an exception. `websockets`' `__aiter__` catches `ConnectionClosedOK` and *returns*, so on close code 1000/1001 the `async for` ends normally, the `async with` exits, and the `try` completes **without entering `except`** — no commit, no backoff, and **no reset**. The outer `while True` reconnects immediately carrying the old books, history, refs, seq and stale set | Distinguish a clean close from an error close. On clean: reconnect immediately, no commit, no backoff, no `ResetOnReconnect` | **This is the rule P25 originally got wrong**, and it was written wrong here before it was written wrong in Go — a spec bug, per §7.4, not a code bug. It is also the one live-path rule that is genuinely counter-intuitive: "reconnect" reads as one event and is two, with opposite state handling. Verified directly against websockets 16.1.1, whose `Connection.__aiter__` is `try: while True: yield await self.recv() / except ConnectionClosedOK: return`. Carrying stale books across a clean close looks like a Python bug; Phase A reproduces it. |
| **P26** | `rig.py:511-523` | The frame is written to the tape **before** `_handle` runs, as `{"recv_ms":<int>,"m":<raw.strip()>}\n` with the frame **interpolated verbatim**, not re-encoded | Same order, same bytes, same `.strip()` | Two failures hide here. Writing after handling loses exactly the frames worth having — the ones that crashed the handler. Re-encoding through a JSON marshaller reorders keys and renormalises numbers, so the tape stops being the bytes the exchange sent and the replay stops being a replay. The `.strip()` is what fixed the archive tapes' broken framing; without it the envelope's closing brace lands on its own line. |
| **P27** | `rig.py:411-421` | Two subscriptions: `id:1` `orderbook_delta` **with** `market_tickers`, `id:2` `trade` with **no** market filter. Resnapshot is `id:3` `update_subscription` with `action: get_snapshot`, `sids: list(self.seq)` and the full ticker list | Same three payloads, same ids, same fields | Filtering `trade` to the universe looks like an obvious tidy-up and would change nothing in the `fill` table — which is precisely why it is dangerous. `stats["trades"]` is defined against the whole-exchange tape (P16), the 98.6% discard rate is the documented sanity check on the subscription being right, and the captured tape would silently stop being a whole-exchange capture. |
| **P29** | `rig.py:178`, `rig.py:193-203` | `checkpoint()` appends a **new** dict pair every time (`dict(self.yes)`), and `state_before` returns references to those dicts. Nothing ever mutates a stored checkpoint in place, so a book handed back by `state_before` stays valid for the lifetime of the caller | A `*Levels` returned by `StateBefore` must remain **stable** across any number of subsequent `Checkpoint` calls | Found by reading a diff, not by a gate. An optimisation that recycles checkpoint storage — a ring buffer reusing its backing arrays — makes the returned book mutate under the caller once the ring wraps. It is invisible today only because `recordTrade` consumes the result inside the same `Handle` call with no interleaved checkpoint, so **no tape can expose it**: the differential gate passed byte-identically with the aliasing live. That is precisely the shape §10.5 warns about, and the rule was implicit in the Python until an optimisation broke it. |
| **P28** | `rig.py:379-407` | `resolve_mids` polls every 5s; `LIMIT 2000`; `if not due: continue` — **no commit** when nothing was due; the mid is written **only** when `book.mid()` is not None, but the pending row is deleted **unconditionally** | Same, including the unconditional delete | The unconditional delete means a market that happens to be one-sided at the horizon loses that markout permanently rather than retrying. That is a defect, it is deferred, and "fixing" it here would add rows the Python never had. The 5s poll against the DB — rather than an in-memory timer heap — is also load-bearing: the docstring's "a restart resumes rather than losing the horizon" is only true because the pending set is persistent. |

---

## 5. Package layout and interfaces

Module `lip`, at `/Users/hugh/kek/lip/go`. `CGO_ENABLED=0` throughout —
`modernc.org/sqlite` is pure Go. Deploy target is this machine (decided
2026-07-24; tower1/tower2 dropped), but staying CGO-free costs nothing and keeps
that reversible.

```
core/   pure, deterministic, no I/O, no clock, no goroutines — the gated surface
store/  SQLite writer (modernc.org/sqlite)
tape/   gzip streaming JSON reader; tolerates both framings and a truncated tail
feed/   websocket client + RSA-PSS signer
cmd/rig     live binary
cmd/replay  deterministic replayer, mirrors replay.py's CLI
```

Signatures are pinned here so parallel work cannot produce integration
mismatch. Implementations may add unexported helpers; they may not change these.

```go
package core

// Numeric primitives — already implemented and gated (core/num.go).
func ParsePriceCents(s string) (int, error)
func ParseSize(s string) (float64, error)

// Levels replicates CPython dict semantics for one side of a book: insertion
// ordered, with overwrite keeping position and delete-then-reinsert appending.
// P2 depends on this; do not substitute a plain map.
type Levels struct{ /* keys []int; idx map[int]int; val map[int]float64 */ }

func (l *Levels) Get(price int) (float64, bool)
func (l *Levels) Set(price int, size float64)
func (l *Levels) Delete(price int)
func (l *Levels) Len() int
func (l *Levels) Max() (price int, size float64, ok bool)  // nil-safe: ok=false when empty
func (l *Levels) Sum() float64                             // insertion order — P2
func (l *Levels) Clone() *Levels
func (l *Levels) SortedDesc() []int                        // for the qualifying walk

type Book struct {
    Target                float64
    RefYes, RefNo, Gate   *int   // nil == Python None; drives change detection
}

func NewBook(target float64) *Book
func (b *Book) ApplySnapshot(yes, no [][2]string) error  // raw wire strings; P1, P22
func (b *Book) ApplyDelta(side string, price int, delta float64)  // P10
func (b *Book) Checkpoint(ts float64)                    // P11
func (b *Book) StateBefore(ts float64) (yes, no *Levels, lag *float64)  // P3-P6
func (b *Book) BestYes() (int, float64, bool)
func (b *Book) BestNo() (int, float64, bool)
func (b *Book) Mid() *float64
func (b *Book) Qualifies() int                           // P9

// Sink receives rows. store implements it; tests use an in-memory recorder.
type Sink interface {
    Fill(FillRow)
    PendingMid(tradeID, ticker, horizon string, dueMS int64)
    Reference(ReferenceRow)
}

type Rig struct{ Stats Stats }

func NewRig(sink Sink, universe map[string]float64) *Rig
func (r *Rig) Handle(frame []byte) error   // one raw websocket frame; P8, P13-P16
```

`FillRow` mirrors the `fill` table one-for-one, using `*int` / `*float64` for
every column that can be SQL NULL (`pre_best_yes`, `pre_best_no`, `pre_mid`,
`pre_spread`, `book_lag_ms`). Do not collapse those to zero values — P18 turns
on the distinction.

`Handle` takes raw bytes rather than a decoded struct so the Go decoder's
behaviour is inside the gated surface, not upstream of it.

---

## 6. Explicitly out of scope for Phase A

These are known defects, red-teamed and accepted for now. Several of them
**change `fill` rows**, which directly contradicts the byte-identical gate — so
each is applied separately in Phase B, against a measured diff versus the Phase A
baseline. Do not implement any of them here. Full detail in `codex-rt-rig.md`.

1. Per-sweep episode model instead of per-print rows.
2. Trade-through evidence attaches to the print price, not the touch it is
   actually evidence about (P19).
3. Excluding causally ambiguous transitions (self-trade prevention, late
   cancels).
4. Event reordering by exchange time behind a completeness watermark.
5. Gap-based sweep sessionization rather than second-floor bucketing.
6. Binning by total sweep size rather than individual print size.
7. Time-weighting the Target Size gate instead of averaging transition events.
8. Preserving fractional-cent prices exactly rather than rounding to cents (P1
   is the *faithful* reproduction of the rounding, not an endorsement of it).
9. Setting `use_yes_price` explicitly against a documented future default flip.
10. Cluster-robust standard errors in `analyse.py`.

### 6a. Accepted live-path divergences

Distinct from the list above: these are places the Go **does not match the
Python** and where the divergence is accepted rather than deferred. Each was
raised by the codex round of 2026-07-24 and is recorded here because an
undocumented known divergence is exactly what gate 7 exists to prevent. None is
detectable by any gate, so this list is the only thing holding them.

1. **The resolver cannot run during a blocking network call.** asyncio keeps
   `resolve_mids` runnable while the main task is suspended at
   `await websockets.connect()`, `await ws.send()`, or the close handshake; the
   owner goroutine is inside those calls and cannot service its timer. The Go
   can also interleave the resolver between any two *buffered* frames where
   Python would consume both without yielding.

   **Corrected 2026-07-25.** This entry previously claimed the observable
   surface was `mid_1m`/`mid_5m`/`mid_30m` alone. **That was wrong**, and the
   codex round was right to reject it. `resolve_mids` ends with
   `self.conn.commit()` (rig.py:407), and that commits the **entire shared
   transaction** — every `fill` and `reference` row accumulated since the last
   commit, not merely its own `mid_*` updates. So a resolver pass that fires
   during a blocking network call in Python, and cannot in Go, moves a **commit
   boundary**, not just a mid.

   It is still accepted, but for the correct and narrower reason: a commit
   boundary is only observable if the process dies before the next one. On any
   normal exit both implementations commit the same rows eventually; the
   difference is confined to *which rows survive an abnormal termination*, and
   it interacts with the SIGTERM rollback above. The alternative — recasting
   Dial, Resnapshot and Close as asynchronous state-machine operations so the
   owner keeps servicing the resolver throughout — is a large change to the most
   delicate part of the rig, and it would be made to align a crash boundary.

   The lesson generalises past this row: **a shared transaction means no writer
   on it is independent of any other.** Any future claim that a divergence
   touches only one column has to establish that it also touches no commit.

2. **`time.Time` comparisons carry a monotonic reading.** *Fixed, not accepted.*
   The deadline, `lastCommit` and `lastReport` are now CPython-style wall-clock
   `float64` seconds via `pyTime()`, compared against fresh wall-clock reads.
   The original acceptance argued a clock step "cuts the safer way"; that is
   true but irrelevant, because a step also changes which rows are inside the
   open transaction when the process ends.

3. **Clean shutdown flushes the gzip trailer.** SIGTERM rolls the transaction
   back exactly as Python's unhandled signal does, so the rows match — but the
   tape is still closed properly rather than left truncated. The tape is a replay
   *input*, never a compared output, so a complete one cannot change a row, and
   it removes the repair step (`gzip -dc … | gzip -6 -c`) every Python tape needs
   before it can be gated.

4. **permessage-deflate parameters are not byte-identical.** Compression is
   negotiated, matching Python's default, but `websockets` additionally
   advertises `client_max_window_bits` and `coder/websocket` offers no way to
   express that. The handshake's `Sec-WebSocket-Extensions` value therefore
   differs. Accepted because closing it needs a patched websocket library, and
   no row depends on the negotiated window size.

### 6b. Residuals — correct findings, unreachable conditions

Raised by codex round 3 and **not fixed**. Each is a true statement about the
code. Each needs a condition this deployment cannot produce, and §8's
materiality bar is reachability, argued rather than assumed. If the deployment
changes, these are re-examined — not re-discovered.

1. **The REST socket timeout wraps TCP, below TLS.** `socketTimeoutConn` refreshes
   its deadline per underlying read, and one `tls.Conn.Read` may perform several,
   so TLS record fragments arriving every 19 seconds could keep a single
   application read alive indefinitely where CPython carries one deadline across
   the retries of an SSL read. Unreachable: the endpoint is Kalshi's own REST
   API returning a ~40 KB response in one flight, and this runs once at startup.
   Also unfixed here: urllib sends `Connection: close` and installs proxy
   handling from the environment; neither affects a single request to a fixed
   host with no proxy configured.

2. **Python's second commit attempt is not reproduced.** `rig.run`'s `finally`
   commits, and `main`'s `finally` commits again, so a transient first failure
   (SQLite busy during journal finalisation) can still persist in Python. Go
   attempts one. Unreachable in the relevant sense: the DB is opened with a
   single writer connection and nothing else holds it, so `SQLITE_BUSY` has no
   source. Reproducing it properly needs a pinned `sql.Conn` with explicit
   BEGIN/COMMIT, because a failed `sql.Tx` is already marked done and cannot be
   retried.

3. **`pyString` is not exact for lone surrogates or invalid UTF-8.**
   `json.dumps("\ud800")` emits `"\ud800"`; Go's decoder has already replaced an
   unpaired surrogate with U+FFFD before `pyString` sees it. Unreachable: the
   only strings this formats are Kalshi market tickers, which are ASCII
   alphanumerics and dashes. Recorded because the day a ticker stops being ASCII
   is the day this becomes real, and nothing would announce it.

4. **Binary websocket frames lose their type.** Python decodes a binary frame as
   UTF-8 before taping, so invalid UTF-8 is fatal there; Go tapes the bytes and
   lets the JSON decoder substitute U+FFFD. Unreachable: Kalshi sends text
   frames, and a binary frame carrying invalid UTF-8 would additionally have to
   be valid JSON to reach a row.

5. **Universe decoding accepts and rejects different shapes.**
   `{"incentive_programs": null}` yields an empty universe in Go and raises in
   Python; a numeric `target_size_fp` is accepted by Python's `float()` and
   rejected by Go's string field; Go's field matching is case-insensitive.
   Unreachable against the live endpoint, which returns 200 programs with
   string-valued `target_size_fp` — verified directly. An empty universe is now
   permitted rather than rejected, matching Python.

6. **SIGTERM exits 0 rather than by signal.** Python's unhandled SIGTERM
   terminates by signal, so the shell sees 143. Go returns normally after
   discarding. No row depends on it; a supervisor reading exit status would.

---

## 7. Retry and escalation protocol

On a gate failure:

1. **Feed the failure back verbatim.** The specific mismatching rows, both
   sides' values, and the frame index that produced them — not a paraphrase and
   not "fix the bug." Brute-force retry carrying the real error outperforms
   re-prompting.
2. **Minimise before re-prompting.** Bisect the tape to the shortest frame
   prefix that reproduces the mismatch, and feed that. A three-frame repro beats
   a 544,000-frame log.
3. **Budget: three attempts per unit.** The surveyed literature then hands to a
   human. That route is closed here, so the fallback is instead:

   a. **Cross-model adversarial review.** Hand the unit, the Python source, the
      relevant §4 rows and the failing diff to codex (`gpt-5.6-sol`, `xhigh`,
      read-only sandbox), primed to assume the Go is wrong. A second model with
      no stake in the code it is judging is the closest available substitute for
      an independent reader.
   b. **If that does not resolve it, the unit is parked, not shipped.** Mark it
      failing in the task list and move to another unit. Do not merge a unit
      that has never passed the gate on the grounds that it is "probably fine."

   Escalating to a larger model is not a step. No surveyed source does it, and
   it does not address the failure mode — a bigger model that cannot see the
   divergence is not more likely to see it on the fourth try.
4. **A recurring failure mode is a spec bug, not a code bug.** If an
   implementation "fixes" the rounding, or the summation order, or the
   qualifying walk more than once, patch §3/§4 rather than patching the code
   again. The rule was not clear enough. Bun's response to agents faking green
   builds was to rewrite the workflow prompt, not to hand-patch each instance.
5. **Adversarial review before acceptance: 2-of-3 vote, default to refuted.**
   Three independent reviewers per unit, each told to assume the Go is wrong and
   to **default to `refuted: true` when uncertain**; two refutals overturn the
   unit. That default is the whole mechanism — a reviewer that resolves doubt in
   favour of the code finds nothing. It is Bun's `lifetime-classify` rule
   verbatim: *"Adversarially verify a lifetime classification. Default
   refuted=true if uncertain."*

   Each reviewer must cite **the specific §4 row violated**, not a general
   impression. "Does this look reasonable" is not a review.

   Reviewers get the full context, not just a diff: the Go file, the
   corresponding `rig.py` range, and the §4 rows in scope. Bun's reviewers are
   often described as seeing only the diff; their actual prompts hand over the
   diff, the whole current file, and the sibling `.zig` as ground truth. Scope
   the *flagging* to the changed region, never the *reading*.

   **Model self-assessment is not admissible as evidence.** On code containing
   deliberate traps, one controlled study measured 39.7% semantic drift, and
   31.7% of the drifted cases were endorsed as correct by the same model that
   produced them — models will articulate the precise bug they introduced and
   still call the code correct. An implementer's claim that it preserved a §4
   rule carries zero weight. Only the fixture, the diff and the negative control
   carry weight.

---

## 8. Definition of done

- Gates 1-6 green on a real multi-hour tape, both tapes in `lip/` and
  `lip/archive/`.
- **Gate 7 green: every mutation in the negative-control table was caught**, with
  the catching gate and row counts recorded in `notes/negative-control.md`. This
  replaces human review and is not optional.
- Two independent adversarial review passes, plus a codex round
  (`gpt-5.6-sol`, `xhigh`, read-only) that stops finding real issues.

  **"Real" needs a definition, or this never terminates.** A reviewer instructed
  to default to refuted will always produce another finding, because the supply
  of increasingly improbable conditions is unbounded. That is the prompt working
  as designed, and it is why the *reviewer* cannot be the one who decides when
  to stop.

  A finding is REAL, and blocking, if it can change a `fill` or `reference` row
  under conditions **reachable in the deployed configuration**: this binary,
  against Kalshi, with these credentials, on this machine. Reachability is
  argued explicitly and written down — never assumed, in either direction.

  A finding is a RESIDUAL if it is correct about the code but needs a condition
  that configuration cannot produce. Residuals are recorded in §6a with the
  reachability argument, not silently dropped. If the deployment changes, the
  argument is re-examined rather than the finding re-discovered.

  Three rounds of this produced 14, 11 and 12 findings. The count is not the
  signal; the *materiality* is. By round 3 the verified-correct list covered
  every numeric and clock rule, and what remained needed a bare-CR credentials
  file or a lone surrogate in a market ticker. That is the point at which the
  round has stopped finding real issues, and continuing is not diligence.
- The §10 optimisation tournament run to termination, with `notes/scoreboard.md`
  showing a final round that won at no level.
- Seven-day shadow run alongside the Python rig, including forced disconnects,
  with the two `fill` tables agreeing.

**The order of those last two matters and is not interchangeable.** The shadow
run must be the LAST thing, on the code that will actually ship. Shadowing for
seven days and then optimising means the artifact that ran for seven days is not
the artifact being deployed, and the most expensive piece of evidence in the
whole plan would have been spent on the wrong binary. If the tournament produces
a change after a shadow has started, the shadow restarts.

There is no human code-review step. That is a deliberate choice, and it is why
gate 7 exists and why the shadow run is seven days rather than one.

---

## 9. Mechanisms adopted from Bun's rewrite

Bun's porting scaffolding was merged to `main` and deleted ten minutes later in
a commit titled "Delete stray files"; it is recoverable from history but was
never officially published. `docs/PORTING.md` (576 lines) is at commit
`46d3bc29f270fa881dd5730ef1549e88407701a5`; the orchestration lives in 52
`.claude/workflows/*.workflow.js` files at merge commit `23427dbc`.

Adopted, adapted to this port:

**9.1 Confidence trailer.** Every ported Go file ends with a comment trailer
stating `confidence: high` (should be correct with only mechanical fixes) or
`confidence: low` (logic may be wrong, re-read the Python). Low-confidence files
are reviewed first and are never the last thing shipped.

**9.2 Marker conventions.** `// PORT NOTE:` where Go's type system forced a
reshape that has no Python counterpart. `// TODO(port):` where something could
not be translated confidently — flagging beats guessing. Both are greppable and
must be empty before §8 is satisfied.

**9.3 No justification comments.** Bun's rule, verbatim: *"If you need a
paragraph to justify it, the code is wrong — fix the code instead."* Note this
sits in deliberate tension with 9.2, and the resolution is Bun's: a marker
saying "I could not do this" is welcome; an essay arguing "this divergence is
fine actually" is a defect. Explaining *why the Python behaves oddly* is
encouraged and is what §4 is for; explaining *why the Go may deviate* is not.

**9.4 Slop scan.** A grep gate for faked completion, run before a unit counts as
done. Bun greps for `todo!(`, `unimplemented!(`, stub macros. The Go equivalent:

    panic("TODO"  |  panic("not implemented"  |  t.Skip  |  testing.Short
    return nil, nil  (from a function that must produce a row)
    _ = err          (a discarded error)
    // TODO(port):   (must be zero at §8)

**9.5 Frozen-artifact checksums — where we deliberately exceed Bun.** Bun's "0
tests skipped or deleted" was enforced by prompt convention and self-report; no
committed CI check verifies it. With no human reviewer that is not good enough
here, so the §2 frozen list is checksummed and the gate fails if any of them
changed. Convention is not enforcement.

**9.6 `NULLABILITY.tsv` — the `LIFETIMES.tsv` analogue.** Bun pre-classified
every ambiguous pointer field once, adversarially verified it, and had every
downstream agent `grep` the table rather than re-derive the answer. The data was
never published; the generator was.

Our recurring cross-file ambiguity is not lifetimes, it is **None-versus-zero**:
which Python values become `*T` in Go and which become `T`. Getting it wrong is
invisible at compile time and corrupts a column. Build
`lip/testdata/NULLABILITY.tsv` with columns:

    py_site | py_expr | can_be_none | go_type | sql_null | evidence

covering every value that reaches a SQL column or a struct field, at minimum:
`pre_best_yes`, `pre_best_no`, `pre_mid`, `pre_spread`, `book_lag_ms` (all
nullable), `pre_yes_size`, `pre_no_size`, `depth_at_price` (zero, never NULL —
P18), `Book.RefYes/RefNo/Gate` (nil-initialised, so the first reference row
always writes), and the `lag` return of `StateBefore` (nil only on empty
history — P6).

Generate it once, verify each row 2-of-3 adversarially with default-refuted, and
have implementers use the `go_type` column verbatim rather than inferring it.

**9.7 Dependency-tiered chunking.** Bun computed a crate DAG and ported in
tiers. Ours is small enough to state outright:

    T0  core/num          (done, gated)
    T1  core/levels       (T0)
    T2  core/book         (T1)
    T3  core/rig          (T2)
    T4  store, tape       (T3)
    T5  cmd/replay        (T4)  -> gates 1-6 first become runnable here
    T6  feed, cmd/rig     (T5)

Nothing at T(n) starts before T(n-1) passes its unit tests. `cmd/replay` is the
first point at which the differential gate exists, so T0-T4 are carried by unit
tests and review alone — which is precisely why §4 is exhaustive.

**9.8 Not adopted.** Worktree isolation, `flock` apply-locks, explicit-path-only
commits and optimistic-concurrency retry all exist to stop 64 concurrent agents
stomping a shared tree. At seven files with a linear dependency chain, they are
overhead. Revisit only if implementation is genuinely parallelised.

Only then does cutover happen, and only then does Phase 2 (the live quoting bot,
$100 cap) start.

---

## 10. The optimisation phase

### 10.1 Why this section reverses an earlier rule

§3 bans restructuring the hot path, and the working notes repeat it: *"Go
replays slower than Python. Don't fix it by restructuring the hot path — that
risks semantics for nothing."*

That rule was correct **when it was written**, and it was correct for a specific
reason: there was no oracle, so any change to the hot path traded a known-good
translation for an unverifiable one. The reason has since expired. There is now a
byte-identical differential replay over 8,515 `fill` rows across 16 columns and
12,726 `reference` rows across 6, plus 58 named unit tests, plus a negative
control in which 18 of 19 deliberate violations were caught. The risk the ban was
protecting against is now measured rather than assumed.

Per §7.4, a rule that has become wrong is patched rather than quietly violated.
This section is that patch. It does not weaken the correctness standard by one
row; it changes only what counts as a permissible *representation* of behaviour
that is already pinned.

### 10.2 Entry condition

Do not optimise unverified code. The phase may begin only when:

- gates 1-7 are green on a real multi-hour tape, and
- a cross-model correctness round (§7.3a) has come back clean.

Optimising first and verifying afterwards inverts the whole argument: it produces
a fast artifact whose correctness rests on a gate that was never run against the
version being shipped.

### 10.3 The scoreboard is lexicographic

Five dimensions, in strict priority order. **A round is won at the highest level
where the challenger improves, and only if no higher level regresses beyond its
noise floor.**

| | Dimension | Measured by | How it is won |
|---|---|---|---|
| **L0** | Correctness | gates 1-7; `fill` and `reference` byte-identical; declared invariants intact | Not a score — a filter. Also winnable by landing a confirmed §4 violation in the opponent's code. |
| **L1** | Tail latency | p99 / p99.9 / max of per-frame `rig.Handle`, via `replay --latency` | Any of p99, p99.9 or max improves beyond its floor with none of them regressing. |
| **L2** | Allocations | allocs/frame and total allocated, via `replay --bench` | Improves, and L1 does not regress. |
| **L3** | Throughput | median frames/s over N replays | Improves beyond its floor, and L1/L2 do not regress. |
| **L4** | Live CPU + RSS | Go-vs-Python ratio over a fixed simultaneous window | Improves, and nothing above regresses. |

L0 is never traded. A change that alters one row loses no matter what it buys,
and is reverted rather than argued about.

**Why latency outranks throughput.** Throughput has no operational value here.
The live feed delivers roughly 390 frames/s — 35,053 frames in a measured 90
second window — and the replay handles 87,000/s. That is ~220x headroom, so no
throughput improvement will ever be observable in production, and Phase A's
measurement path has no deadline to miss. What Phase 2's quoting bot needs is
tick-to-decision latency, and specifically its tail: being 300 microseconds late
to leave the touch is the loss, not being 2% slower on average.

Median throughput actively hides that. The baseline reads 87,323 f/s with a p50
of 7.28 us — and a max of 347.85 us, a 48x spread. Throughput cannot see it.

**Why allocations sit directly under latency.** They are the *cause* of the tail,
not an independent goal: 52.32 allocations per frame produce 548 GC cycles over
one tape, whose worst pause is 193 us — squarely in the p99.99-to-max band. They
are also measured with a **zero noise floor** (identical to two decimal places
across all five baseline reps), where p99.9 is noisy. So allocations are the
low-variance corroborator for a high-variance metric that matters more.

This ordering also makes the right trade *winnable*: batching, and the
4096-frame channel buffer, both help throughput and hurt latency. Under the
previous ordering a change that halved p99.9 at the cost of 2% throughput would
have LOST. It should win.

**L4 is a regression guard, not a target, and is expected never to decide a
round.** The rig uses on the order of 1-2% of one core; there is no prize for
halving that. It is correlated with L2 and L3 anyway — all three largely measure
work per frame — so it will usually just move with them and win nothing on its
own.

Its job is to catch the optimisation that wins on replay and *costs* something
live, which is a real failure mode rather than a hypothetical one. Replay never
executes the gzip tape writer, TLS, or the socket read path at all, and the tape
writer was the single largest CPU line item in the Python rig's own code —
larger than JSON decoding and the whole measurement path combined. An allocation
win bought with buffer pools that balloon steady-state RSS across a 200-book
multi-hour run would look like a clean L2 victory and show up nowhere else.

So L4 is measured every round and consulted only to veto. Being the lowest tier
is the point, not a shortcoming.

Measuring the Go-vs-Python **ratio** in the same window, rather than absolute
CPU, is what makes L2 usable: exchange activity varies by orders of magnitude
between rounds and the ratio cancels most of it.

### 10.4 Noise floors are measured before the tournament, not during

"Beat the metric" is unfalsifiable without them, and a challenger that wins by
0.4% on a metric with 3% run-to-run variance has won nothing. Each dimension's
floor is measured on the unmodified baseline, repeated, and recorded in
`notes/scoreboard.md` before round 1. An improvement counts only if it exceeds
the floor.

Allocations are near-deterministic, so their floor is close to zero and small
allocation wins are real. Wall-clock throughput is not; treat it accordingly.
Tail percentiles are the noisiest of all — p99.99 and max are single-sample
statistics over one tape, so a change must move p99 and p99.9 too before it is
believed, and the allocation and GC-cycle counts should corroborate it.

**Run `--latency` as a SEPARATE pass from `--bench`.** Two clock reads per frame
and a 9 MiB sample buffer perturb both the throughput and the allocation
numbers. The same binary, two runs, three dimensions.

### 10.5 Declared invariants — where a fast wrong answer would hide

The gate is the referee, and the referee has known blind spots. Every §4 rule
that no gate can detect must be listed here and checked **by reading the diff**,
because nothing else will catch it:

- **P2's consumption by `Sum`.** Gate 7 m02 changes which order `Sum` iterates
  and survives every gate, because compensated summation is order-independent.
  A challenger may not remove the ordering on the strength of that. The ordered
  *bookkeeping* is separately defended (m11, m12, both caught), so this invariant
  covers only the `Sum` iteration itself.

Any change that touches a declared invariant is reviewed as a §4 change, not as
an optimisation, regardless of what the gates say. Adding to this list is
expected; removing from it requires a mutation showing the gate now catches it.

### 10.6 Rounds: strict alternation, bar for bar

Two contestants, not a reviewer and an author:

    A   Opus 5, xhigh          moves first
    B   gpt-5.6-sol, xhigh     moves second

They alternate strictly — A, B, A, B — and each move is scored by the same
referee on the same scoreboard. A round is one move by each.

**Both sides see the same state before moving:** the current `notes/scoreboard.md`,
the opponent's last diff, and the full source. Neither gets a metric the other
does not.

**Codex runs `--sandbox read-only`, so B emits a unified diff and A applies it
VERBATIM.** This is the one place the contest could be corrupted, so it is a
hard rule: A does not tidy, rename, fix, or improve B's patch on the way in. If
B's diff does not apply, does not compile, or fails a gate, that is B's move and
B loses the round — exactly as it would if A shipped something broken. A may
hand the verbatim failure back for B to correct within the same move, which is
the retry budget of §7.1, not a rewrite by A.

The reverse guard binds A: A may not look at B's pending diff and pre-empt it,
and A's own move must be written before B's is requested.

Each move: apply, re-run all 7 gates, re-measure every dimension, append a row
to `notes/scoreboard.md` naming the mover, what changed, and what it bought. A
change that moves a row or fails a gate is reverted, and **the attempt is
recorded rather than deleted** — a rejected optimisation is evidence about the
oracle, and a pattern of rejections in one area is a finding in its own right.

### 10.7 Termination

The tournament ends at the first full round in which the challenger improves
nothing beyond the noise floor at any level.

The lexicographic ordering is what makes that a real stopping condition rather
than a judgement call: every accepted change is a Pareto improvement in a fixed
priority order, so the tournament cannot cycle between two changes that each win
on a different metric while losing on another.

### 10.8 What is still forbidden

Phase A faithfulness continues to bind on **observable behaviour**. Representation
and control-flow shape are in play; semantics are not. Specifically unchanged:

- every rule in §4 and §4a, on the behaviour it describes;
- §6 stays out of scope — an optimisation may not smuggle in a deferred fix;
- §3's bans on stubbing, skipping, and destructive commands to escape a failing
  state;
- the frozen-artifact list. Tuning the comparator, the fixtures or the tape to
  make a faster version pass is the same failure as editing them to make a
  correct version pass.
