# Red-team brief: the `harness/rest` I/O layer

You are the primary intellectual driver for this review. You decide what matters,
what advances, and what the operator is asked next. The material below is
supplied as labelled evidence, deliberately unranked — do not treat the order of
any list as a claim about importance.

Be adversarial. The author is not looking for reassurance. The previous review of
this kind (`loop/out-03-quote.md`) found five real defects in work already
believed finished, and that was the entire return on a much more expensive
process.

---

## USER STATEMENT (verbatim)

> Red-team review of the completed harness/rest I/O layer (lip-5pv, lip-293,
> lip-cd6, lip-xr6 — all closed this session). Project root /Users/hugh/kek/lip,
> Go module /Users/hugh/kek/lip/go. Gates GREEN, 68 tests in harness/rest.
> Nothing committed.

---

## CONTEXT

This is a market-making harness for Kalshi's Liquidity Incentive Program. It
exists because a previous bot (`probebot.py`) lost money by cancelling its
resting orders and stopping its own monitor loop while holding inventory — it
removed the exit and kept the risk, then went blind for 6.14 hours. Every rule in
the spec that looks paranoid is that defect viewed from a different angle.

The pilot is $100 on one market. **The only hard external loss bound is the
account balance**, because both `inv_kill` and `pnl_kill` merely enter
`WINDING_DOWN` — they stop *adding*, they do not flatten.

This layer is the harness's entire exchange surface. Nothing above it can be
correct if this is wrong, and unlike the pure logic layer its failures are
mostly invisible in testing: the endpoints answer HTTP 200 while lying.

---

## WHAT TO READ

1. `/Users/hugh/kek/lip/notes/harness-spec.md` — the design contract, 2,283
   lines, SHA-pinned. Relevant: §4 (coordinates, H-CO-1..H-CO-5), §7 (order
   lifecycle: H-ORD-1, H-ORD-2/2a/2b, H-ORD-4/4a, H-ORD-5/5a/5b/5c, H-ORD-6/8/9),
   §8.2–8.3 (H-POS-1..4, H-PAGE-1, H-PAGE-1a), §11 (H-FAIL-1..5), §16
   (parameters), §17 (V1.1, V1.2, V1.7, V1.7a, V1.8, V1.8a/b/c, A1, A10–A14,
   M4, M12, M16, M17, M21, M22, M23).
2. `/Users/hugh/kek/lip/notes/pagecontract.md` — the measured, per-endpoint
   pagination contract (169 GETs, 0 writes). §6 lists what is explicitly NOT
   established and must not be assumed.
3. `/Users/hugh/kek/lip/notes/pilot-plan.md` — §3 (the scenario exchange that
   replaces V2), §6 (what must not be cut at any capital), §7 (build order), §8
   (deliberately skipped). Do not report deferred work as missing.
4. `/Users/hugh/kek/lip/go/core/rig.go:13-39` — load-bearing prose: neither a
   print at our price nor a trade-through proves our private order filled.
5. The code under review, all in `/Users/hugh/kek/lip/go/harness/rest/`:
   - `client.go` — the `Doer` seam, `NotSent`, the V1.8c endpoint table, pinned
     status strings, the live `net/http` + `feed.Signer` transport
   - `page.go` — the guarded cursor walk (H-PAGE-1 / H-PAGE-1a)
   - `read.go` — price parsing, orders, positions, fills, balance
   - `write.go` — create classification and same-coid recovery
   - `cancel.go` — cancel, `reduced_by`, and the verified sweep
   - `wire.go`, `coid.go` — the pure half, reviewed and closed earlier
   - `*_test.go` — 68 tests; `stub_test.go` holds the scripted transport
6. For context only, not under review: `harness/num`, `harness/cfg`,
   `harness/quote`, `harness/risk`. `go/core`, `go/feed`, `go/store`,
   `go/cmd/rig` are hash-pinned and cannot be edited.
7. Verified-in-production Python consumers this layer was built against:
   `probebot.py:775-796` (resting-order shape), `:800-807` (positions),
   `:486-491` (create response shape), `:1100-1123` (the pagination incident
   report and the `next_cursor` walk), `kalshi.py:91-155` (signing, endpoint
   paths), `kalshi.py:243-253` (cancel).

---

## OBSERVED TOOL FACTS

- `./loop/gates.sh` → `VERDICT: GREEN` (build, vet, gofmt, test-race, check.py,
  negative-control all PASS).
- `go test ./harness/rest/ -count=1 -v` → 68 `--- PASS`, 0 failures.
- Nothing is committed. `harness/rest/` is entirely untracked.
- Beads `lip-5pv`, `lip-293`, `lip-cd6`, `lip-xr6` were closed this session.
  `lip-0ns` (P3) was filed for the fills-walk scaling note below.
- Mutations hand-verified this session by applying the mutation, observing the
  named test fail, and reverting: M16, M22, M23 (page.go), M4, M17 (write.go),
  M12 (cancel.go), plus two non-catalogue mutations — trusting `side` over
  `book_side`, and defaulting a missing `is_taker` to false.

---

## IMPLEMENTATION CHOICES MADE BY CLAUDE

Listed as facts about the artifact, in file order, not ranked. Several are places
where the spec or the measured contract was silent and a choice was made; the
alternative is stated where one exists. Decide yourself which of these matter.

**client.go**

1. The `Doer` seam is at the HTTP level (`Request` → `(Response, error)`, status
   code plus raw bytes) rather than at a typed level (`Orders() ([]Order, error)`).
   Rationale recorded in the code: a typed seam would make the rewind, the
   cursor-key trap, the 409, and the dropped response untestable.
2. A non-2xx status is a `Response`, never an `error`. `error` means no answer.
3. `NotSent` is a distinct error type for requests that provably never left the
   process (could not be constructed, could not be signed). `Create` classifies
   it as a definite rejection rather than as `UNKNOWN`. The alternative is to
   treat every transport error as `UNKNOWN`.
4. `Endpoint` carries `IDFields` per item key. `event_positions` and
   `incentive_programs` have no measured identity field and fall back to the
   record's raw bytes as identity.
5. `MaxPages` (1000 per endpoint) exists as a circuit breaker that abandons the
   walk and returns no answer. The code argues this is not the page cap
   H-PAGE-1a forbids, because it never truncates.
6. `PageLimit` is 1000 for orders/fills, 200 for positions, 1000 for programs.
7. The cursor is sent as the `cursor` query parameter on both families; only the
   response field name differs.
8. Status strings are pinned to `resting`, `canceled`, `executed`;
   `ValidateStatus` refuses anything else on the send path, not just in tests.

**page.go**

9. Forward progress is asserted by the identity of each page's first record.
10. A second guard was added beyond the spec text: a repeated cursor also aborts
    the walk. It exists to cover a records-free page, which has no first-record
    identity.
11. If the endpoint's declared cursor field is absent but a different known
    cursor key is present, the walk fails loudly rather than concluding it
    finished. This is what makes M22 detectable on page one.
12. A record missing its declared identity field fails the whole walk.
13. `Walk.Items` is empty unless the outcome is `WalkComplete`; a rewound walk
    discards its accumulated records.

**read.go**

14. `ParsePrice4` parses dollar strings to 1e-4 units with integer arithmetic
    only, never through `float64`, and refuses more than four decimals rather
    than rounding.
15. A fractional (non-integer-cent) resting price is reported as
    `SEV2 BOOK_PRICE_GRANULARITY` and the order is still returned, carrying its
    exact `Price4`. The alternative is to reject the order.
16. `book_side` is authoritative over `side`/`outcome_side`, per
    `probebot.py:783-789`.
17. When both `yes_price_dollars` and `no_price_dollars` are present, they must
    sum to exactly 10000/1e4 or the record is refused. This cross-check is not
    in the spec; it was added.
18. One undecodable order invalidates the entire walk rather than being skipped.
19. `Positions()` takes no ticker filter, by H-ORD-5b.
20. A position that fails `num.ParseQty` fails the walk rather than defaulting
    to zero.
21. A fill with no `trade_id`, or with no `is_taker`, fails the walk.
22. `Fills()` walks to exhaustion and then filters by `since` client-side,
    because a server-side early stop would make the walk incomplete under
    H-PAGE-1 clause 2. This does not scale with account history; filed as
    `lip-0ns`.
23. `scalar()` accepts both a JSON string and a bare JSON number for the same
    field, and treats JSON null as absent.

**write.go**

24. `CreateUnknown` is the zero value of `CreateOutcome`.
25. The 409 is matched on `error.code == "order_already_exists"`, not on the
    status alone. A 409 carrying any other code is classified `UNKNOWN`, not
    `REJECTED`.
26. Every 409 triggers a confirming read *inside* `Create`, over orders of every
    status, unfiltered. Neither an incomplete confirming read nor a complete one
    that finds nothing downgrades the 409.
27. `MaxLive` is narrowed to the acked `remaining_count` on a clean 2xx, and
    held at the full requested count on every other non-rejected outcome.
28. Other definite 4xx responses return immediately without consuming the retry
    budget; 5xx and unrecognised statuses consume it.
29. The retry loop re-sends a pre-marshalled byte-identical payload, so no code
    path can regenerate the coid.

**cancel.go**

30. `CancelResult` deliberately has no field describing size, position, fill, or
    remainder, so the H-ORD-4a subtraction is unexpressible. A test asserts this
    by reflection over the struct's field names.
31. `Cancel()` makes exactly one attempt; the retry lives in the sweep.
32. A 404 on DELETE is `CancelGone` — a definite answer about that order — and
    is not an error.
33. `sweepRetries` is a hard-coded 1, not a parameter.
34. `CancelAndSweep` partitions what the verifying read returns into the
    requested orders and `OtherOurs`, and cancels only the requested set.
    `Clean` means "none of the requested orders still rests", not "nothing of
    ours rests". This was changed late in the session on the argument that
    cancelling everything the verifying read turned up would cancel a reducing
    quote on a halt.

---

## WHAT IS NOT BUILT YET

Per `notes/pilot-plan.md` §7, still ahead: `risk/pnl` (H-HALT-5), the A1–A4 and
A6–A14 invariants, the websocket layer, the scenario exchange, lifecycle safety
(halt latch, startup adoption, single-instance lock), the five-record store, and
wiring `cmd/harness`. `cmd/harness/main.go` deliberately exits 2 until wired.

Startup adoption (H-ORD-5) is *not* implemented in this layer — `rest` classifies
orders as ours or foreign and stops there. Whether that division of labour is
right is in scope for you to judge.

---

## WHAT TO RETURN

You are the primary driver. Structure your response with these four headings:

    DECISION
    IMPLEMENTATION DIRECTIVE
    ADVANCE: YES or NO
    MOBILE RELAY

`MOBILE RELAY` should normally be at most 180 words, lead with the verdict, use
at most five bullets, and end with at most one next action or user question. It
will be shown to the operator verbatim on a phone.

For each defect, state the consequence concretely — in dollars, in duplicated
inventory, in abandoned inventory, or in time spent blind — rather than as a
severity label alone. If a choice above is defensible, say so and move on; the
scarce resource is the operator's attention, not your output length.
