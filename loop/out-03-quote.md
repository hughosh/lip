**Verdict: No — the pure layer can still re-place after final cancel, declare a live-order account drained, and create off-touch adding exposure.**

## HQL-001 — P1: final cancel can be undone

**File/symbol:** [machine.go:DueCloseActions](/Users/hugh/kek/lip/go/harness/quote/machine.go:471), interacting with [skew.go:SizesFor](/Users/hugh/kek/lip/go/harness/quote/skew.go:188)  
**Violates:** H-CLOSE-0, H-CLOSE-3; retained pilot close-jump scenario.

When both deadlines are already due, `DueCloseActions` returns:

```text
ActionFinalCancel, ActionCloseLead
```

That is the unsafe order. The close-lead action enters `SETTLING` and keeps the reducer; `SizesFor(Settling, q≠0)` subsequently emits that reducer unconditionally. There is no post-`final_lead` state or placement latch.

Concrete pilot sequence:

1. `q=+12`, reducer is 12 NO at 50c, close is now 30 seconds away.
2. Final cancel runs first and confirms the order absent.
3. Close-lead processing then enters `SETTLING`.
4. The next sizing pass requests another 12-contract NO reducer.
5. Presence restoration is not debounced, so it may rest into the final minute.
6. If YES is already knowably winning, a fill at 50c sacrifices **$6** relative to holding; worst case at 99c is **$11.88**.

The test at [machine_test.go:501](/Users/hugh/kek/lip/go/harness/quote/machine_test.go:501) pins the action labels in this dangerous order but never checks the resulting resting-order ledger. Separately, the sizing test insists `SETTLING` always emits a reducer. Both cheap halves pass while their composition violates H-CLOSE-3.

**Smallest fix:** execute close-lead effects before final cancel, and latch `final_lead` per market so all later placement/sizing decisions return no quote until trading closes. Add an end-state test asserting zero `RESTING`, `SENDING`, `UNKNOWN`, and unconfirmed-cancel quantity both immediately after catch-up and on the following tick.

## HQL-002 — P1: `DRAINED` does not require orders to be confirmed absent

**File/symbol:** [machine.go:NextGlobal](/Users/hugh/kek/lip/go/harness/quote/machine.go:403)  
**Violates:** H-FAIL-3, I1, the §5.1 `DRAINED` contract.

`WINDING_DOWN` transitions to `DRAINED` on `!AnyInventory` alone. `GlobalInput` has no condition for orders being exchange-confirmed absent.

Concrete pilot sequence:

1. The market is flat, with 12 YES at 50c and 12 NO at 49c resting.
2. SIGTERM enters `WINDING_DOWN`; cancels are dispatched.
3. Before their responses or sweep, `AnyInventory=false`.
4. The next call returns `DRAINED`, even though both adding orders remain fillable.
5. If the YES cancel was ignored and fills, the supposedly drained account acquires `q=+12`; settling NO loses the **$6** purchase cost.

Monitoring should eventually rediscover this, so it is not the old six-hour blindness—but the stop condition has still added risk after claiming to be drained.

`TestGlobalDrainAndReturn` supplies only `AnyInventory`; it cannot exercise H-FAIL-3.

**Smallest fix:** add `AllManagedOrdersAbsent` to `GlobalInput` and require both flat inventory and confirmed absence of every resting/sending/unknown/unconfirmed-cancel order before entering `DRAINED`.

## HQL-003 — P1: the self-cross clamp is wrongly applied to adding quotes

**File/symbol:** [requote.go:Decide](/Users/hugh/kek/lip/go/harness/quote/requote.go:202)  
**Violates:** H-Q-1; the stated A4 justification applies only to reducers.

The clamp is defensible for `RoleReducing`: an inferior exit can be safer than no exit. It is not defensible for an adding or symmetric `QUOTING` side, where A4 imposes no obligation. The current code ignores `Role` while clamping.

Concrete sequence:

1. `q=0`, normal symmetric `QUOTING`; our stale NO order remains at 45c.
2. External touches move to YES 60 / NO 39.
3. The YES side is absent and needs restoration.
4. Touch 60 would self-cross 45, so `Decide(RoleAdding)` places YES at the clamp, 54c.
5. That order is six ticks behind and earns inferior or zero scoring while remaining fillable.
6. When the field drops to 54, it can fill 12 contracts and create `q=+12`; settlement NO loses **$6.48** on that unremunerated fill.

The test at [requote_test.go:276](/Users/hugh/kek/lip/go/harness/quote/requote_test.go:276) describes a reducing-side rationale, but its helper sets `RoleAdding`. It therefore positively blesses the wrong behavior.

**Smallest fix:** clamp only when `RoleReducing`. For `RoleAdding`, block the placement and cancel/resolve the conflicting opposite order before restoring at the true touch.

## HQL-004 — P2: a schedule extension traps the market in `REDUCING`

**File/symbol:** [machine.go:afterSettling](/Users/hugh/kek/lip/go/harness/quote/machine.go:267)  
**Violates:** H-CLOSE-0’s moving-schedule semantics and the §5.2 inventory ladder.

Every nonzero position at or below `inv_hard` leaves `SETTLING` for `REDUCING`. But `REDUCING` can leave only at exactly zero, so the comment claiming the market “re-earns its adding side on the next tick” is false.

Concrete sequence:

1. `q=+5`, state `SETTLING`, close moves from 30 minutes away to five hours away.
2. `afterSettling` returns `REDUCING` with trigger `MTInvHard`, despite `5 < inv_hard=7`.
3. On every subsequent tick, `NextMarket` keeps nonzero `REDUCING` unchanged.
4. The correct state is `SKEWED`, with adding size 6 and reducer size 5.
5. If the reducer does not fill, the market remains one-sided for five hours. Per the spec’s example, that can cut reward rate from about 50% share to 33.3%, or gate the market out entirely.

The test at [machine_test.go:296](/Users/hugh/kek/lip/go/harness/quote/machine_test.go:296) checks only that the first result is neither `SETTLING` nor `QUOTING`; it never takes the promised next tick. This is a direct V5 survivor already present in the code.

**Smallest fix:** rederive the normal ladder: `Quoting` for `|q|≤inv_soft`, `Skewed` for `inv_soft<|q|≤inv_hard`, and `Reducing` above hard. Add a schedule-moved trigger and a two-tick regression.

## HQL-005 — P2: a durable latch is ignored while truth remains unreadable

**File/symbol:** [machine.go:NextGlobal](/Users/hugh/kek/lip/go/harness/quote/machine.go:380)  
**Violates:** H-HALT-4 and A14.

In `UNKNOWN_RISK`, the function returns early on unreadable/unreconciled truth before checking `Latched`.

Sequence:

1. Startup truth reads fail, entering `UNKNOWN_RISK`.
2. SIGTERM writes the durable latch.
3. Inputs are `Latched=true`, `TruthReadable=false`, `Reconciled=false`.
4. `NextGlobal` remains `UNKNOWN_RISK`, contradicting A14’s requirement that a disk latch imply `WINDING_DOWN` or `DRAINED`.

Immediate trading consequence is bounded—both states forbid placement—but the heartbeat and state audit falsely report that no durable stop governs the process.

**Smallest fix:** check `Latched` first in every nonterminal state. Extend the test matrix to cover a latch under all four truth/reconciliation combinations.

## The three disputed readings

1. **Symmetric `QUOTING`: correct.** The table explicitly requires `S` on both sides, and A12 is scoped to fills “via a reducer.” At `q=+3`, a 12-NO fill reaching `q=-9` is permitted ordinary quoting behavior; `|q|>7` then enters `REDUCING`. Applying the `|q|` cap here would contradict the symmetric table and effectively stop two-sided quoting at any nonzero position.

2. **H-Q-8 in both directions: correct.** “Regardless of which direction it moved” resolves the prose conflict. The inclusive eight-tick boundary and stranded-before-favourable ordering are also correct.

3. **Clamp: only correct for a reducer.** A4 supports keeping a non-crossing exit. It does not support placing a behind-touch adding quote; HQL-003 is the consequence of applying the reading universally.

I could not execute `go test ./harness/...`: the read-only sandbox denied creation of Go’s temporary build directory. The findings above come from static source/test tracing; no reviewed source files were modified.