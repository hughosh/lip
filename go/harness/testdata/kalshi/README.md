# Real Kalshi payloads

Captured from the live account on 2026-08-12 by `go/cmd/conform`, a read-only
sweep, and promoted here by `scripts/make_rest_testdata.py`. Do not hand-edit:
run `python3.12 scripts/make_rest_testdata.py --check` to verify these files are
what that script produces from `notes/conform-corpus.json` and `notes/conform.json`.

  fills.json                          15 record(s)
  orders.json                          8 record(s)
  market_positions.json                1 record(s)
  event_positions.json                 1 record(s)
  incentive_programs.json             27 record(s)
  incentive_programs_refused.json      6 record(s)

`incentive_programs.json` is a shape-preserving TRIM of the 3,276 active
programmes the account saw: up to 3 per distinct combination of
incentive type, description, discount factor, paid-out flag and Target Size.

`incentive_programs_refused.json` is the other half, and it is the reason this
directory is not an acceptance-only fixture set. The active walk this account
sees is 100% `liquidity` with a Target Size on every record, so acceptance alone
would rebuild the blind spot these fixtures exist to remove. These records are
real `volume` programmes carrying no `target_size_fp`; the decoder must REFUSE
them, and `Programs` being first-error-wins means the day one appears in the
active set, the harness does not boot.

## What was substituted

`user_id` is replaced by one fixed value. The opaque identifiers
(client_order_id, fill_id, id, market_id, order_id, trade_id) are replaced by synthetic UUIDs derived
deterministically from the originals, so equal values stay equal -- a fill whose
`fill_id` and `trade_id` matched on the wire still matches here.

Nothing the decoder interprets was touched. Prices, counts, fees, timestamps,
statuses, sides, tickers, types and `exchange_index` are byte-identical to what
the exchange sent, including the six-decimal `fee_cost` that `lip-9tr` was about.
`client_order_id` is substituted but remains a plain UUID, which is load-bearing:
these orders were not placed by this harness, and `rest.ParseCoid` requires the
`lipH` prefix with fixed-width fields, so no UUID can be mistaken for one of ours.

This is not an anonymisation guarantee. Market tickers and timestamps are
retained verbatim because the decoders key on them, and the unredacted corpus
remains in `notes/`. The narrow claim is the useful one: the directory that
travels with the Go code carries no account identifier.
