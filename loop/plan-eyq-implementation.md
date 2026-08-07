# lip-eyq implementation plan — transactional startup authority

Operator directive 2026-08-07: **get the harness up and running ASAP, regardless
of the LIP programme end date.** This supersedes round 19's PARK verdict and the
end-date gate. Round 19's structural live guards are retained — they are what
separates "running" from "losing money unattended", which is the failure this
whole spec was written from.

Critical path to a runnable binary: **lip-eyq → lip-bw0 → lip-3af**.

Source of the directive: codex round-12 deep dive
(`loop/codex-12-deepdive.log:5241-5399`), recorded verbatim on bead `lip-eyq`,
plus the two integration items and the resolve walk assigned to eyq by the
round-17 v3 adjudication.

## Drift corrected before starting (verified this session)

1. **Catalogue target is 130, not the design's "78/78".** That figure was written
   when the catalogue held 72 entries. It is now 124 (verified by `ast.parse`).
   eyq adds 6 → **130**, with `M-L-STARTUPCAUSE` and `M-L-CAUSELESS` RETARGETED
   rather than added (both verified present, `scripts/harness_negative_control.py`
   lines 780 and 770).
2. **`harness/quote` is editable.** `READONLY_TREES` is `go/core go/feed go/store
   go/cmd/rig`; `FROZEN` is Python + testdata only. Adding `RiskKnown` to
   `quote.GlobalInput` is legal. `quote.NextGlobal` stays PURE — no disk I/O
   moves into `quote` (design §2, explicit).

## Order of work

Sections are the round-12 design's own numbering. Each lands with its tests
failing-first before the repair, per `loop/protocol/RULES.md`.

### STATUS 2026-08-07

- **A — DONE except the `Startup` coordinator** (`Step`/`State`, which is carried
  into B because it is the same rewrite). `RiskKnown` gate, `StopCommit`,
  `CommitStop`, `Advance` + its forge guard, `SignalController` commit-then-
  advance, and all 9 test call sites migrated. build / vet / gofmt / check.py
  green; `go test ./harness/...` green across all 9 packages.
  - Failing-first evidence recorded below.
  - Three existing tests AMENDED (recorded, with reasons).
- **B–G — NOT STARTED.**

#### Failing-first evidence (section A)

    TestWindingDownRequiresKnownFlatTruthBeforeDrained
      before: "flat and clean, truth NOT established" -> DRAINED, want WINDING_DOWN
      after:  pass

    TestAdvanceRefusesAnUnjustifiedHaltedState
      before (guard replaced by `if false {`):
        WINDING_DOWN -> published as DRAINED with nothing latched
        DRAINED      -> published as DRAINED with nothing latched
      after:  pass

#### Tests amended in section A (all recorded, none weakened)

1. `quote/machine_test.go TestGlobalDrainAndReturn` — sets `RiskKnown: true` on
   the two cases that expect DRAINED. It asserts the INVENTORY rule; the
   truth-established rule is the new test's job.
2. `quote/machine_test.go TestDrainRequiresOrdersConfirmedAbsent` — same, on the
   table input.
3. `lifecycle/latch_test.go TestLatchWriteFailureBlocks...` — the stop is now
   expressed in BOTH places (`CommitStop` for durability, `in.Stop` for the
   transition), and the advance's refusal class is `STOP_CAUSE_MISSING` while
   `LATCH_WRITE_FAILED` moves to the commit. Same property, split across the two
   calls that now own its two halves.

### A. Boundary (design §2)
- Replace `GlobalController.Decide(GlobalInput, StopCause)` with:
  - `CommitStop(StopCause) StopCommit` — makes the cause durable, updates the
    monotone cached latch. Nothing else.
  - `Advance(quote.GlobalInput) GlobalDecision` — injects the cached latch, calls
    `NextGlobal`. MUST reject an unlatched stop request, and MUST reject a
    purported existing state of `WINDING_DOWN`/`DRAINED` supplied by a caller.
- `StopCommit{Durable, BlockAdding, RetryLatch, Anomalies}` replaces the
  overloaded zero-`StopCause` sentinel. Delete the sentinel meaning from the
  public API.
- `SignalController`: commit first, advance second.
- `Startup` becomes the stateful coordinator: `Step(ctx, time.Time) Attempt` and
  `State() quote.GlobalState`. **Remove `Run(..., quote.GlobalInput, ...)`.**
  Global state is private, starts at `STARTING`, and is updated only from a
  committed decision. A caller cannot inject, reset, or replay it.
- Add zero-conservative `RiskKnown bool` to `quote.GlobalInput`. `WINDING_DOWN`
  may enter `DRAINED` only when `RiskKnown` is true AND both risk flags are
  false. Positive inventory or order evidence still returns `DRAINED` →
  `WINDING_DOWN`.

### B. Startup as one transaction (design §3)
Split orchestration into `startup.go`; `adopt.go` retains policy/adoption types.
Each step, in this order:
1. positions, orders, fills, balance — in that order.
2. Retain the balance error long enough to classify complete raw orders/fills.
3. Commit a foreign-fill cause IMMEDIATELY after classification, before checking
   the balance error or attempting conversion.
4. Commit known owned-taker evidence before any fee conversion that can fail.
5. Commit `startup_fill_history` the moment fill reconciliation discovers it.
6. After a latch-write failure, continue safe cancellation work where facts
   suffice — but NEVER return an Adoption.
7. Carry causes across discarded rewalks and retries.
8. Invoke `Advance` ONCE PER STEP — not once per cause — with internally derived
   facts.

Only a complete, final, no-cancel pass sets `RiskKnown=true`. Derive
`AnyInventory` from seeded positions and `AnyLiveOrder` from final kept orders.
An incomplete latched attempt stays `WINDING_DOWN`, never false-`DRAINED`.

### C. Lifecycle authority ahead of policy (design §4)
`AdoptionFacts` gains defensively copied `Excluded []string` and
`AddingPermitted bool`; `Selected` becomes the effective selection after foreign
exclusions. The LIFECYCLE LAYER, not the injected policy, enforces:
- at `q == 0` every resting order is adding;
- at `q != 0` the adding side comes from `quote.AddingSide`;
- a globally halted or ticker-excluded adding order cannot be kept — convert
  `AdoptionKeep` to verified cancellation;
- a valid reducer may remain, including in an excluded market with inventory;
- `Managed` is built from effective selection, positions, and final kept orders.

`AddingPermitted` is false for a loaded/new latch, failed latch persistence, or
halted coordinator state. Zero value stays false.

### D. The startup resolve walk (round-17 assignment)
This is what makes `adopt.go`'s existing refusal-to-conclude TERMINATE instead of
blocking forever.
- Walk `GET /portfolio/orders` UNFILTERED — `Order.ClientOrderID` is present for
  terminal orders (the `ConfirmCoid` pattern). No new REST endpoint.
- `hstore.BindOrder(coid, orderID, nowMs)` for every coid the walk matches.
- `hstore.ResolveReservationAbandoned(coid, abandonedMs)` for any coid the
  exchange never saw, AFTER confirm retries are exhausted.
- Conclude FOREIGN only when the unresolved set is empty.
- Read-only accessors already exist: `Store.UnresolvedReservations()`,
  `Ownership.UnresolvedCount()`, `Store.BindListedOrder`. No new SQL surface.

### E. Two integration items (round-17 assignment)
1. **Drain before close.** Shutdown order is `Pending()==0` → cancel the writer →
   `Close`. `Close` refuses while the FIFO is non-empty, by design. v3 item 7
   changed only the CRASH path (a writer that returns now fails every queued
   record loudly and empties the queue, so `Close` after writer exit succeeds);
   the ORDERLY path must still drain first.
2. **SEV1 `STORE_RECORD_REJECTED` off `Result.Err != nil`**, not off parsing
   `Health().LastError()`. Every terminal Result is a record that is GONE.

### F. Test methodology (design §5) — the heaviest section
- **Remove `startingInput()` and migrate all 25 `Startup` call sites.** No
  startup test may supply `GlobalInput`.
- Preserve every existing test and assertion; add stateful traces using ONE
  coordinator across attempts:
  third failure → `UNKNOWN_RISK` → successful recovery; loaded latch + repeated
  REST failures; discovered cause then balance/conversion/policy/sweep/rewalk
  failures; multiple causes with inventory and live orders; revoked adding order
  vs preserved reducer; foreign exclusion affecting policy and management.
- New: `TestStartupOwnsStateAcrossRetrySequence`,
  `TestWindingDownRequiresKnownFlatTruthBeforeDrained`,
  `TestStartupPublicSurfaceCannotAcceptOrResetGlobalInput`.
- Assertions must cover event ordering, state history, durability and cancel
  counts — NOT merely `Err != nil`.

### G. Ratchets (design §6) → catalogue 130
Retarget: `M-L-STARTUPCAUSE` → immediate `CommitStop`; `M-L-CAUSELESS` → the new
`Advance` guard. Add six compiling production-only entries:

| ID | Mutation |
|---|---|
| `M-L-CAUSEFAIL` | postpone cause commitment until final success |
| `M-L-FALSEFLAT` | ignore `RiskKnown` when draining |
| `M-L-STOPKEEP` | preserve a revoked adding order |
| `M-L-EXCLUDESELECT` | use raw selection |
| `M-L-STATERESET` | reset coordinator state to `STARTING` at each `Step` |
| `M-L-STATEFORGE` | add an exported state setter / input bypass |

Each needs a NAMED catcher test and must compile.

## Standing constraints (unchanged)

- `notes/harness-spec.md` SHA-pinned — never edit.
- `go/core`, `go/feed`, `go/store`, `go/cmd/rig` hash-pinned — editing fails gates.
- Every non-test Go file ends `// confidence: high`. `scripts/check.py` rejects
  `t.Skip`, `testing.Short`, stub panics, `_ = err`.
- New SQLite test DBs under `t.TempDir()` only. Never touch `lip/rig.db` or any
  `*.db`.
- **Re-run the `ast.parse` anchor audit after ANY signature change** — this
  refactor changes many. Five anchors rotted silently during v3; `str.replace`
  no-ops on a missing anchor, so a rotted anchor tests nothing.
- Do NOT edit anything under `go/` while gates or the negative control is
  running.

## Definition of done

`./loop/gates.sh` GREEN, full catalogue **130/130 producing declared outcomes**,
failing-first evidence recorded for every non-pin test, and `lip-bw0` unblocked.
