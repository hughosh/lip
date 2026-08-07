# Red-team brief: the `harness/quote` pure logic

You are reviewing new Go code against a written specification. Be adversarial.
The author is not looking for reassurance; four earlier findings from this kind
of review were the entire return on a much more expensive process, and three of
them were latent defects in work already believed finished.

## Context

This is a market-making harness for Kalshi's Liquidity Incentive Program. It
exists because a previous bot (`probebot.py`) lost money by *cancelling its
resting orders and stopping its own monitor loop* while holding inventory — it
removed the exit and kept the risk, then went blind for 6.14 hours. Every rule
in the spec that looks paranoid is that defect, viewed from a different angle.

The pilot is $100 on one market. **The only hard external loss bound is the
account balance**, because both `inv_kill` and `pnl_kill` merely enter
`WINDING_DOWN` — they stop *adding*, they do not flatten.

## What to read

1. `/Users/hugh/kek/lip/notes/harness-spec.md` — the design contract, 2,283
   lines. §4 (coordinates), §5 (state machines), §6 (quoting), §9 (close),
   §10.2/10.3 (capital), §12 (halt semantics), §16 (parameters), §17 V1/V3/V5
   are the relevant parts.
2. The code under review, all in `/Users/hugh/kek/lip/go/harness/`:
   - `num/qty.go` — exact quantities (recently changed: `ParseQty`)
   - `cfg/params.go` + `params_test.go` — §16
   - `quote/skew.go` + `skew_test.go` — §6.2
   - `quote/external.go` + `external_test.go` — H-Q-10
   - `quote/cross.go` + `cross_test.go` — H-CO-6
   - `quote/requote.go` + `requote_test.go` — §6.5
   - `quote/machine.go` + `machine_test.go` — §5.1, §5.2, §9
   - `quote/state.go`, `quote/queue.go` — pre-existing, for context only

`notes/pilot-plan.md` records what is deliberately deferred; do not report
deferred work as missing.

## The three readings I most want attacked

Each is a place the spec was ambiguous and I chose. Say if I chose wrong, and
say what the consequence is in dollars or in abandoned inventory.

1. **`QUOTING` is symmetric at `S` on both sides** (`skew.go`, `SizesFor`).
   §5.2's table says "size S at touch" in both columns. I did NOT apply
   `size_R`'s `|q|` cap to the side opposite the position while in `QUOTING`,
   arguing that H-Q-5a and A12 are scoped to a *designated reducer*
   ("no fill sequence can change the sign of `q` **via a reducer**") and
   `QUOTING` designates none. The consequence I accept: at `q = +3` a full fill
   of the 12-contract NO quote lands at `q = −9`, past `inv_hard`, straight into
   `REDUCING`. Is that right, or does A12 bind unconditionally?

2. **H-Q-8's stranded brake fires in both directions** (`requote.go`, `Decide`).
   The rule says our price is "`stale_bid_ticks` or more **behind** the external
   touch" but its rationale describes the opposite case — "our own order is the
   only thing holding a price level nobody else wants" is our order *above* the
   touch. I implemented `|distance| >= stale_bid_ticks`. Is the union right?

3. **A self-crossing touch clamps rather than dropping the quote**
   (`requote.go` + `cross.go:MaxOpposite`). When H-CO-6 forbids the touch, I
   place at the highest non-crossing price instead of not placing. Argued from
   A4: on a reducing side an exit a few ticks back beats no exit.

## What else to look for, in priority order

1. **Any mutation that would survive these tests.** §17 V5 is a negative
   control: name a semantic change to the code under review that violates a
   spec rule and that every test here still passes. This is the highest-value
   thing you can produce. M13 and M14 came from exactly this question.
2. **Any place the code contradicts the spec's prose.** Note that §5.2's
   *diagram* has already been found to contradict H-CLOSE-2 once; prefer the
   prose and say so if I followed a picture.
3. **Ordering hazards.** Several functions depend on the order of their tests
   (`Decide`'s stranded-before-favourable, `NextMarket`'s close-lead-before-
   ladder). Is any ordering wrong, or is any dependency undocumented?
4. **Boundary conditions.** Inclusive/exclusive edges on `inv_soft`, `inv_hard`,
   `close_lead`, `final_lead`, `stale_bid_ticks`, the debounce, and the 1..99
   price range.
5. **Anything where a test verifies the cheap half of a claim.** A prior lesson:
   a guard that greps for a test symbol proves the symbol exists and nothing
   about what it covers.

## What NOT to report

- Missing packages (`rest`, `wsx`, `hstore`, `ping`, `sim`) — not written yet.
- Anything listed as cut or deferred in `notes/pilot-plan.md`.
- Style, naming, comment density, or test verbosity. The comments are
  deliberately long; they carry the argument.

## Output

Markdown. For each finding: a stable ID, severity (P1 = can lose money or
abandon inventory, P2 = wrong but bounded, P3 = cosmetic), the file and symbol,
the spec rule it violates, a concrete failure sequence with numbers, and the
smallest fix. Put a one-line verdict at the top: is this layer safe to build the
REST and websocket layers on top of?

If you believe a reading of mine is correct, say so explicitly and briefly —
knowing which ones survived scrutiny is worth as much as the findings.
