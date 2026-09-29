# Runtime shutdown repair — September 28

`lip-3dw` fixes the producer/store lifetime mismatch exposed by the retained September 27 q01 failure. The owner-only health lock was insufficient: balance telemetry, monitor output, terminal results, delivery attempts and detached transport callbacks could still submit. The q01 assessor was not changed.

## Producer inventory and final-close order

| Producer / submission | Production seam | Final-close coordination |
| --- | --- | --- |
| Owner startup/read/fill/write/signal/turnover callbacks; fill ledger, state mirrors, listed-order binding, anomaly sink | `go/cmd/harness/run.go`, `shutdown.go` | A close request enters the sole owner select at a callback boundary. It rechecks fresh known flat truth and no live/ambiguous/inflight obligations, then parks normal callbacks through close. |
| Dispatcher reservations and order bindings | `dispatch.go` | Input group cancellation and join; authorization check excludes unresolved dispatched work. |
| Balance telemetry, including a request that returns after cancellation | `balance_telemetry.go` | Input group joins the callback before store closure. Timeout refuses close; replacement waits for its own predecessor while independent observers resume. |
| WS supervisor, portfolio poller, schedule/program/turnover/terminal/crosscheck loops | `run.go` input group | Cancel and join all loops before final close; preserve channels on refusal, invalidate feed generation and reacquire reads. |
| Monitor samples and anomalies | `monitor.go`, runtime monitor sink | Keep monitor alive during input unwind, then cancel and join before final sweeps. Resume on refusal. |
| Result handler, including rejection-generated anomalies and dispatch permits | `shutdown.go` | Join the single result consumer, process terminal outcomes and drain newly accepted anomaly records until settled. |
| Alert delivery records and alert effects | `alerts.go`, `harness/ping/service.go` | Run one serialized final delivery step, cancel/join the alert loop, drain its terminal outcomes. New failure evidence causes refusal and another delivery opportunity, not quiet closure. Restore same alert service/retry state on refusal. |
| Detached net/http dial / DNS-fallback reporter | `runtime.go`, `dial_barrier.go` | Shared REST/WS DialContext registration fences new dials and waits for active callbacks. Resume admission on refusal. This handles Transport work that outlives its requesting goroutine. |
| Qualification checkpoint writer | `qualification.go` | Independent artifact writer, not a trading-store producer. Join before final evidence finalization after successful close. |
| Drain loop / stop refusal anomaly | `shutdown.go` | Waits for owner close result; emits refusal only after restoration. Reattempts at bounded cadence; no concurrent drain tick can submit during its own close. |

The store writer remains outside the paused groups. Accepted records drain while the writer is alive; terminal failures remain visible. Only successful Store.Shutdown and writer join permit releasing the instance lock. A failed final close after writer shutdown remains sticky, so a subsequent idempotent Close cannot falsely claim recovery. The refused-close path restores independent observation and alerting without overlapping a callback still returning from cancellation. Unexpected writer death still latches `store_unhealthy` and remains observed.

The 2-second final-close deadline is a refusal boundary, not permission to discard records or exit. It does not bound a genuinely stuck callback's lifetime; its successor waits for it. Real-exposure unusable-store recovery remains unproved.

## Evidence and honest failures

Production-seam regressions cover successful close with accepted records, blocked balance callback and cancellation timeout, detached DNS fallback, SQLite write-lock refusal/retry, continued account/monitor/alert observation, real unexpected writer exit, direct lock/alert retention, and a final delivery record's permanent rejection. The tests use actual store admission/writer/results behavior and production lifecycle composition.

Affected packages passed; lifecycle tests passed under `-race -count=3`. After a stronger detached-dial assertion, its two tests passed `-race -count=10`. The full final candidate gate supplies final-source module/race/safety evidence. Full mutation catalogue was not run.

The first selected mutation run caught 13/14. `M-3DW-DIAL-WAIT` survived because the test's immediate nonblocking close poll could release the callback before the broken close caught up. The stronger test holds the accepted callback and observes the transport barrier return; all fourteen selected mutations were caught on the investigated rerun. The original survivor remains in `negative-control-20260928T171704546855Z` and its report. The rerun receipt is `negative-control-20260928T172101631625Z`.

The first candidate gate failed the repository's required confidence trailers on two new Go files. Added comment-only trailers and built a separate candidate; neither the failed build identity nor gate receipt was replaced. Earlier sandbox/cache setup failures, stale catalogue-anchor failure, and the overstrong test expectation from development remain in this directory. These failures were not counted as behavioral mutation kills.

Named controls: M-3DW-INPUT-JOIN, M-3DW-INPUT-RESUME, M-3DW-LOCK-REFUSAL, M-3DW-ALERT-RESUME, M-3DW-DIAL-WAIT, M-3DW-DIAL-FENCE, M-3DW-FINAL-RESULTS, M-OPS-STORE-STOP, M-OPS-STORE-RECOVERED, M-SD-ORDERING, M-SD-UNPLANNED, M-6W5-CONCURRENTFINAL, M-6W5-CLOSEFIRST, M-7ZT-WSBYPASS. Existing final-alert and WS controls had stale textual anchors adapted to their unchanged intended fault; no assertion was relaxed.

The retained September 27 CR-2 candidate and failed 20m30.626s q01 remain failures. This repair does not reopen `lip-1in` or the interface audit, and local fixtures do not prove any financial stage.
