# Delegate: harness-spec transport clauses

Scope (read-only, this file only): `/Users/hugh/kek/lip/notes/harness-spec.md` (2,386 lines).
Do not open other files. Do not modify anything except the output file.

Task: list every clause that constrains the TRANSPORT layers of the client, i.e. anything about:
REST client/HTTP (signing, timeouts, retries, throttle/rate limits, pagination), the order
write protocol (client order ids, create/cancel/amend, acks, timeouts, dedupe), the websocket
session (subscribe, reconnect, read deadline / F1 backstop, sequence gaps, resubscribe),
coordinate systems and wire encoding (H-CO-*), self-trade prevention, never-self-cross,
the write guard / live sentinel, startup adoption of exchange orders, fill attribution,
halt semantics that bind the socket or REST, and the §16 parameters that configure any of it.

For each clause produce one table row:
| clause id or § | lines | one-line summary (<= 25 words, no paraphrase beyond that) | class |
where class is:
- G  = generic to ANY Kalshi trading client (would be kept in an extracted transport module)
- L  = specific to the LIP market-making strategy (would be dropped or moved to the strategy)
- M  = mixed: a generic mechanism parameterised by a LIP-specific policy (say which part is which)

Then a short section "Parameters (§16)" listing the transport-related parameters with their
line numbers and defaults as written.

Then a section "Judgement calls" (<= 8 bullets): clauses where the G/L/M classification is
debatable and why.

Output: write the result to
`/Users/hugh/kek/lip/loop/run/client-portability/out/spec-transport-clauses.md`.
Every row must carry a line number that a reader can `sed -n` to check. Return in your final
message only: the row count, the G/L/M counts, and the three clauses you think matter most
for extraction. Keep the final message under 150 words.
