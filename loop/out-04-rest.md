## DECISION

Do not advance. I found six unsafe current behaviors and one untested gate mutation. None requires a spec patch, and I did not count deferred lifecycle or scenario-exchange work.

1. **Malformed create acknowledgements erase live exposure.** [write.go](/Users/hugh/kek/lip/go/harness/rest/write.go:174) marks every 2xx `ACKED` before validating its body. In [applyAck](/Users/hugh/kek/lip/go/harness/rest/write.go:305), `200 {}` or `200 null` leaves `Remaining=0`, no error, and narrows `MaxLive` to zero. It also schedules no reconciliation. If a 12-contract order at 99c actually landed, a replacement can double exposure to 24 contracts/$23.76; repeated burst dispatch can consume the $100 account. This directly contradicts H-ORD-2’s rule that an unparseable response is `UNKNOWN`.

2. **A structurally incomplete portfolio page is accepted as complete truth.** [decodePage](/Users/hugh/kek/lip/go/harness/rest/page.go:241) accepts an absent cursor when neither known key appears, while [missing or null item arrays](/Users/hugh/kek/lip/go/harness/rest/page.go:273) become empty. Thus `200 {}` and `200 null` are complete empty walks. The measured contract says terminal pages retain an explicitly empty cursor and the declared arrays ([pagecontract.json](/Users/hugh/kek/lip/notes/pagecontract.json:166)). One such positions response can overwrite `q=+12` with flat and abandon its reducer; one such orders response makes an ignored cancel look clean while an $11.88 order remains fillable.

3. **`post_only` is not structural on the actual send path.** `CreateOrder` has exported mutable fields ([wire.go](/Users/hugh/kek/lip/go/harness/rest/wire.go:94)), and `Create` only parses `Count` before marshaling ([write.go](/Users/hugh/kek/lip/go/harness/rest/write.go:128)). The exact compiling mutation `body.PostOnly = false` immediately inside `Create` is not inspected by any write-path test; the existing A1 test checks only the constructor. One 12-contract taker at 99c exposes $11.88 plus fees; repeated dispatch is bounded only by the $100 balance. The same hole permits `maker` STP, arbitrary coids, zero/negative counts, and malformed prices.

4. **JSON `null` bypasses two fail-closed checks.** At [read.go](/Users/hugh/kek/lip/go/harness/rest/read.go:498), unmarshalling `is_taker:null` into a `bool` succeeds and leaves `false`; the missing-field test does not cover it. At [Balance](/Users/hugh/kek/lip/go/harness/rest/read.go:585), `balance:null` similarly succeeds as zero. The former can hide continued taker execution until the $100 account is exhausted; the latter can leave a held 12-contract position without a funded reducer for the 60-second balance interval, or indefinitely if repeated.

5. **The sweep can lose its requested target during ownership filtering.** [cancel.go](/Users/hugh/kek/lip/go/harness/rest/cancel.go:224) builds a requested-ID set, but then searches only `read.Ours()`. If the verifying 200 still contains the requested `order_id` while its `client_order_id` is missing or malformed, the order is classified foreign, omitted from `StillResting`, and `Clean=true`. An ignored 12-contract cancel can therefore be followed by a replacement, producing 24 contracts/$23.76 of exposure. Preserve the requested-only meaning of `Clean`; match requested IDs before applying ownership filtering.

6. **The zero value of `Walk` means “complete.”** `WalkComplete` is zero ([page.go](/Users/hugh/kek/lip/go/harness/rest/page.go:30)), so `var w Walk; w.Replaces()` is true. The specified rest-worker response channel makes zero values reachable through an unchecked closed-channel receive or an unpopulated response. That can replace positions or orders with empty state and abandon up to the full $100 account. The zero outcome must be failed/unknown.

7. **The real HTTP transport has no gate.** Every test substitutes [scriptedDoer](/Users/hugh/kek/lip/go/harness/rest/stub_test.go:27); none reaches `HTTPDoer`. Exact mutation: change [client.go](/Users/hugh/kek/lip/go/harness/rest/client.go:319) to return `Status: http.StatusOK` for every response. By test inventory it should survive all 68 tests. In production it turns the recovery 409 into a fake 200; combined with defect 1, the original live order becomes `MaxLive=0`. This mutation must be executed before being recorded as a settled survivor.

The HTTP-level seam, `NotSent`, cursor/identity guards, abandonment-style `MaxPages`, exact price parsing, full fills walk, same-coid retry, requested-only sweep semantics, and keeping startup adoption above `rest` are defensible. The reflection test around `CancelResult` is useful but cannot prove a future caller will not subtract `ReducedBy` from its separate `Order`; that remains a lifecycle gate.

## IMPLEMENTATION DIRECTIVE

1. On pristine copies, run the two exact mutations above: `body.PostOnly=false` and forced HTTP status 200. Record the named catching test or the survivor before creating Beads work.

2. Add red tests for:

   - `{}`, `null`, missing fields, wrong coid, negative counts, and unrecognised 2xx create acknowledgements;
   - missing/null cursor and every missing/null declared item array;
   - `is_taker:null` and `balance:null`;
   - a requested sweep target returned with missing/non-ours coid;
   - zero-valued `Walk`, `OrdersResult`, `PositionsResult`, and `FillsResult`;
   - real `HTTPDoer` method, URL, query-excluded signature path, body, raw status/body, and redirect behavior.

3. Make create acknowledgements strict: only observed 200/201 shapes with nonempty order ID, matching coid, required nonnegative counts, and bounded quantities may narrow `MaxLive`. Every other 2xx remains `UNKNOWN`, full-size, reconcile-now, and same-coid recoverable.

4. Require the measured page shape. Make create payloads opaque or revalidate every invariant immediately before transmission. Decode required booleans/numbers through pointers so `null` is distinguishable. Make zero `Walk` failed. Match requested sweep IDs before ownership classification.

5. Keep each current unsafe implementation form as its permanent negative mutation, run full gates, then update/close only the resulting Beads issues. Do not edit the spec or frozen trees.

This sandbox was read-only: Go could not create its build directory, and Beads could not open its Dolt database. I made no file, issue, or spec changes; mutation execution must occur in the implementation turn.

## ADVANCE: NO

The layer can currently convert valid-but-incomplete HTTP 200 responses into “flat,” “cancelled,” “maker,” or “zero live,” and the production transport itself is outside the 68-test surface.

## MOBILE RELAY

NO — do not advance `harness/rest`.

- `200 {}` on create becomes ACKED with `MaxLive=0`; a live 12-lot can be replaced, producing 24 contracts/$23.76.
- `200 {}` or missing cursor/list keys becomes complete empty portfolio truth, abandoning inventory or falsely clearing a cancel.
- `CreateOrder` is mutable at dispatch; `post_only=false` is a compiling, apparently surviving mutation. One 99c 12-lot risks $11.88; the account caps repeated loss at $100.
- `is_taker:null` reads false; `balance:null` reads zero; a requested order can disappear from a sweep merely because its coid is missing.
- Zero-valued walks are “complete,” and no test exercises the real signed HTTP transport.

Next action: run the named mutations, add the red tests, fix the seven items, and return full gate output.