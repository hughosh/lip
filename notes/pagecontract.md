# V1.8a — the per-endpoint pagination contract, measured

**Status:** ANSWERED 2026-08-04/05, read-only. This closes the last blocking
fact-finding item in `harness-spec.md` §17 V8.
**Method:** `scripts/pagecontract.py`, GETs only, through
`kalshi.Client(dry_run=True)`, which structurally cannot transmit a write.
**Raw report:** `notes/pagecontract.json`. 169 GETs, 0 writes.

H-PAGE-1 clause 4 required that "ordering, retention, deduplication and
concurrent-insert behaviour are established per endpoint by V1.8a and
**recorded, not assumed**." This is that record.

---

## 0. The headline: there is no single contract, and this repo already
## held both halves of the answer while believing each was universal

| Endpoint | Cursor field | Item key(s) |
|---|---|---|
| `/portfolio/positions` | **`cursor`** | `market_positions` **and** `event_positions` |
| `/portfolio/orders` | **`cursor`** | `orders` |
| `/portfolio/fills` | **`cursor`** | `fills` |
| `/markets/trades` | **`cursor`** | `trades` |
| `/incentive_programs` | **`next_cursor`** | `incentive_programs` |

`probebot.py:1123` reads `next_cursor`; `scripts/opportunity.py:54` reads
`cursor`. **Both are correct — for different endpoints, and each is silently
wrong for the other.** A reader using the wrong key sees an empty cursor,
concludes the walk is complete, and returns page one as if it were the whole
answer. That is `probebot.py:1100`'s incident reproduced by a typo rather than
by a missing loop, and it is exactly why guessing this was not acceptable.

The two families also differ in cursor encoding, limit semantics, and
invalid-input handling (§2, §3, §4). They are two different backend services and
must be coded as two different contracts.

---

## 1. Population sizes — a single page is nowhere near the answer

`/incentive_programs?status=active` returned **4,170 active programs** over 21
pages at `limit=200`.

That figure indicts live code in this repository:

| Caller | Read | Sees |
|---|---|---|
| `score.py:27` (**frozen**) | `limit=200`, single page | 4.8% |
| `scripts/capacity.py:52` | `limit=200`, single page | 4.8% |
| `probebot.py:1113` | `limit=1000` + `next_cursor` walk | 100% (correct) |
| `scripts/opportunity.py:51` | `limit=200` + `cursor` walk | **page 1 only** — wrong key |

`probebot.py:1104` recorded the population "grew past 1000" mid-probe. It is now
**4,170**. A market-selection pass that reads one page is choosing from 4.8% of
the universe while believing it ranked all of it — and H-SEL-1 ranks by
`pool ÷ field qualifying score`, so the best market is not preferentially near
the front. (`score.py` is frozen and stays as it is; this is recorded so the
harness does not inherit the pattern.)

Account-side populations are small today: 1 position row, 9 orders, 15 fills.
**They are small enough that no portfolio endpoint paginates at
`limit=200` today**, which is precisely the condition under which a missing
cursor loop passes every test and then fails in production.

---

## 2. Cursors are keyset, not offset — decoded

Every cursor is base64url-encoded protobuf. Two distinct shapes:

**Portfolio family** (`fills`, `orders`, `trades`) — `{id, timestamp}`:

```
12 12  0a 10 1582884a1f647c20fab4b40468ad5df6   field2.field1 = 16-byte record id
1a 0c  08 c990a5d306 10 98c8ecdb01               field3 = {ts_sec, ts_nsec}
```

The embedded id `1582884a-1f64-7c20-fab4-b40468ad5df6` is **exactly the
`trade_id` V1.8 sampled** — the cursor is keyed on the last record returned.

**Programs family** (`incentive_programs`) — `{timestamp, uuid}`:

```
0a 0c  08 c88ac9d306 10 98dd87a402               field1 = {ts_sec, ts_nsec}
12 24  "7dd26bd2-61b9-49ba-8227-2e09451329d4"    field2 = uuid as ASCII
```

Decoded timestamps descend across successive pages
(`20:02:16Z` → `14:02:03Z`), confirming descending order on a creation time that
is **not exposed in the payload** — which is why no payload field tests as
monotonic for that endpoint.

**Both are composite keyset cursors `(sort_key, tiebreak_id)`. Neither is an
offset.** This is the fact that determines concurrent-insert behaviour, and it
is the favourable answer.

---

## 3. Limits — different rules per family

| limit | positions | orders | fills | incentive_programs |
|---|---|---|---|---|
| 0 | accepted | **HTTP 400** | **HTTP 400** | accepted → defaults to 100 |
| 1 … 1000 | accepted | accepted | accepted | accepted |
| 1001 | accepted | **HTTP 400** | **HTTP 400** | accepted → returns 1001 |
| 5000 | accepted | **HTTP 400** | **HTTP 400** | accepted → returns all 4,170 |

- Portfolio `orders`/`fills`: valid range is **1…1000**; outside it is a hard 400.
- `incentive_programs`: **no upper bound observed** — `limit=5000` returned the
  entire population in one page with an empty `next_cursor`.
- `positions` accepted every value including 0 and 5000 and always returned its
  single row. With one row on the account, **whether it honours `limit` at all
  is unestablished** (§6).

`limit` may be **changed mid-walk safely**: page 1 at `limit=1` followed by page
2 at `limit=5` returned 5 further records with **zero overlap**. Keyset cursors
do not encode page size.

---

## 4. Invalid cursors — the two families behave oppositely, and one fails silently

**This is the finding with teeth, and H-PAGE-1 does not currently cover it.**

| Cursor sent to `/portfolio/fills` | Result |
|---|---|
| `"garbage!!"` | **HTTP 200, page 1** |
| `""` | HTTP 200, page 1 |
| a valid cursor truncated to 20 chars | **HTTP 200, page 1** |
| a cursor minted by `/incentive_programs` | **HTTP 200, page 1** |
| a valid cursor (control) | HTTP 200, page 2 ✓ |

Every unparseable cursor returned trade_id `1582884a-…`, the *first* record — the
endpoint **silently ignores a cursor it cannot parse and rewinds the walk to the
beginning**. It does not error.

The consequence, demonstrated by injecting one corrupted cursor at page 3 of a
`limit=1` walk:

```
pages=8  items=8  unique=5    duplicates: 3
VERDICT: NON-TERMINATING / duplicating
```

A walker that carries a corrupted cursor **loops forever, re-reading page 1 and
accumulating duplicates.** Against `/portfolio/fills` that is duplicated fill
attribution; against `/portfolio/orders` it is a walk that never completes, so
under H-PAGE-1 clause 2 it never replaces state and every downstream truth clock
runs to `truth_max_age_s`.

`/incentive_programs` does the opposite and is safer: garbage → **HTTP 400**, a
well-formed cursor from the wrong family → **HTTP 500**. It fails loudly.

**Required of `harness/rest`:** a cursor walk must detect a rewind rather than
trust the endpoint to reject one. Two independent guards, both cheap:

1. **Never resume a walk from a mutated cursor.** A cursor is used exactly as
   received or the walk is abandoned and the prior state marked stale.
2. **Assert forward progress.** Track the identity of the first record of each
   page; if a page repeats an identity already seen in this walk, the walk has
   rewound — abandon it, preserve prior state, mark stale, and emit
   `SEV2 CURSOR_REWIND`. Never emit the accumulated duplicates.

A page cap alone is not sufficient: it converts a silent infinite loop into a
silent truncation, and H-PAGE-1 clause 3 forbids treating a partial read as an
answer.

---

## 5. Ordering, dedup and concurrent inserts

**Ordering.** `orders` and `fills` are **descending by `created_time`** (`fills`
also descending by `ts`). `incentive_programs` is descending by a hidden
creation timestamp visible only inside the cursor. All are **newest-first**.

**Dedup and completeness.** For every endpoint, a fine-grained walk and a
coarse walk returned **identical sets with zero duplicates**:

| Endpoint | fine walk | coarse walk | sets equal | dupes |
|---|---|---|---|---|
| positions | 1 item / 1 page @1 | 1 item / 1 page @200 | ✓ | none |
| orders | 9 items / 9 pages @1 | 9 items / 1 page @200 | ✓ | none |
| fills | 15 items / 15 pages @1 | 15 items / 1 page @200 | ✓ | none |
| incentive_programs | 4,170 / 84 pages @50 | 4,170 / 21 pages @200 | ✓ | none |

**Concurrent insert — measured, not inferred.** `/markets/trades` unfiltered is
append-only and exchange-wide, so it inserts rows during a walk without us
writing anything. Ten pages at `limit=5`, sleeping 2s between pages (≈20s of
live churn):

```
total=50  unique=50  duplicates: []
VERDICT: keyset-stable
```

Adjacent pages shared a boundary microsecond
(`…04:43:59.800634` was both page 1's last and page 2's first record, as two
*different* trades) and still did not duplicate — the composite id tiebreak
handles same-timestamp ties correctly.

**Therefore, for a newest-first keyset cursor:** a record inserted mid-walk
sorts *above* the walk's anchor and is simply **not seen** by that walk. It
cannot duplicate an already-walked record and cannot displace one into being
skipped. A complete walk is a **consistent suffix as of its start**, not a
torn read.

This is the answer H-PAGE-1 needed. The residual exposure is **staleness, not
corruption**: a fill landing during a fills walk is invisible until the next
walk. That is bounded by `truth_max_age_s` (60s) against a 5s poll and needs no
new rule.

---

## 6. What is NOT established, and must not be assumed

1. **`/portfolio/positions` pagination is untested.** The account holds one
   position row, so the endpoint never paginated. Its `limit` handling differs
   from `orders`/`fills` (it accepts 0 and 5000 without complaint), so it is
   plausibly a different implementation again. **Walk it with the same
   guards and never rely on one page.**
2. **`positions` returns TWO arrays under ONE cursor** — `market_positions` and
   `event_positions`. Whether they paginate together, independently, or whether
   the shared cursor terminates on the exhaustion of only one is **unknown and
   untestable at one row each.** The harness reads `market_positions` for `q`;
   it must not terminate the walk on `market_positions` being empty while the
   cursor is still non-empty.
3. **Retention is unestablished.** All 15 fills on the account are visible, so
   no retention horizon was reachable. `backfill_h` = 24h is well inside
   anything plausible; do not infer more.
4. **Snapshot isolation across the whole walk is not documented.** §5's result
   shows a newest-first keyset walk is not *torn* by inserts. It says nothing
   about deletions or in-place updates mid-walk (an order transitioning
   `resting → executed` between pages could be missed by a status-filtered
   walk). H-POS-4 already handles this correctly by replacing the order map only
   on a complete walk and treating a missing order as filled-or-cancelled.
5. **`status=cancelled` silently returns 0; `status=canceled` returns 2.** The
   endpoint does not reject an unknown status value — it answers "nothing".
   Filter values are US spelling: observed valid are `resting` (0),
   `canceled` (2), `executed` (7); unfiltered total 9 = 2 + 7. **A typo'd filter
   is indistinguishable from an empty result**, which is the "absence is not
   evidence" trap (H-ORD-2a) in a new place. Status strings used by the harness
   are pinned in a test against these counts.

---

## 7. Spec consequences

| Change | Where |
|---|---|
| Cursor field name is per-endpoint (`cursor` vs `next_cursor`) | H-PAGE-1 clause 4 |
| Invalid cursor → silent rewind on the portfolio family; walks must assert forward progress | **new H-PAGE-1a** |
| Cursors are keyset; a complete walk is a consistent suffix as of its start | H-PAGE-1 clause 4 |
| `positions` carries two arrays under one cursor; do not terminate on one | H-PAGE-1a, H-ORD-5 step 1 |
| `orders`/`fills` `limit` range is 1…1000; 0 and >1000 are HTTP 400 | `harness/rest` |
| Unknown `status` values return empty rather than erroring | H-PAGE-1a |
| 4,170 active programs — selection must walk, not sample | H-SEL-2 |

V1.8a is answered. No blocking fact-finding item remains before the order layer.
