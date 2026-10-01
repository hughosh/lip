# Delegate: latency instrumentation and recorded latency evidence

Scope (read-only):
- Code: `/Users/hugh/kek/lip/go/harness/qual/*.go` (non-test), `/Users/hugh/kek/lip/go/harness/netx/dns.go`,
  `/Users/hugh/kek/lip/go/harness/wsx/transport.go`, `/Users/hugh/kek/lip/go/harness/wsx/observation_gap.go`,
  `/Users/hugh/kek/lip/go/harness/wsx/read_throttle.go`, `/Users/hugh/kek/lip/go/harness/rest/sweep_trace.go`,
  `/Users/hugh/kek/lip/go/harness/hstore/*.go` (only to find which tables/columns store timings).
- Evidence: everything under `/Users/hugh/kek/lip/notes/` (markdown, json, jsonl, log). Use grep,
  do not read large files whole. Do NOT open any `*.db` file directly; if you must query one,
  use `sqlite3 'file:PATH?immutable=1&mode=ro'` only.
Do not modify anything except the output file.

Task A — instrumentation inventory. For each timing/latency quantity the code measures, one row:
| quantity | where measured (file:line) | clock (wall / monotonic / exchange ts) | where persisted (table.column or file) | unit |
Include: websocket frame receive timing, observation gaps, ping/pong or RTT, DNS resolution,
REST round-trip or request durations, order submit→ack, exchange fill ts→local observation,
sweep durations, any q01/qual metrics, and the F1 read deadline.

Task B — recorded measurements. Find every NUMBER already recorded in notes/ for:
order ack latency, fill observation lag (exchange ts vs local), websocket connect time,
frame cadence or inter-frame gaps, REST round trip, DNS resolution time, rate-limit (429)
observations, reconnect counts/intervals. One row each:
| quantity | value(s) | date/stage | source path:line |
Known anchors you must verify (quote the line): the 2026-10-01 MCI stage report at
`notes/first-fill-evidence-2026-09-28/operator-stages/r2-KXEPLRELEGATION-27-MCI-20261001T031001.344628Z/evidence/stage-report.md`
lines ~65 and ~79 (fills seen 4.1 s and 1.3 s after exchange ts); the sweep log at
`notes/lip-yca-review-evidence-2026-10-01/sweep/sweep.log` (~11 req/s, 8,135 GETs, 0 429s);
`notes/readiness-evidence-2026-09-26/api-review.json` lines ~22-23 (429 has no Retry-After).

Task C — gaps (<= 10 bullets): latency quantities a low-latency trading client would want
measured that this code does not measure or persist.

Output: `/Users/hugh/kek/lip/loop/run/client-portability/out/latency-inventory.md`.
Return in your final message only: row counts for A and B, the 3 most useful recorded numbers,
and the top 3 gaps (under 150 words).
