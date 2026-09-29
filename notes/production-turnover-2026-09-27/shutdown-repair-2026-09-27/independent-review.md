# Independent bounded shutdown review — root adjudication

A fresh read-only Sol worker inspected the production close boundary without edits or tests. Root independently verified the direct balance producer and accepted these concrete findings:

- `balanceTelemetry.pollOnce` records independently of the owner and can race close, then raise `BALANCE_POLL_FAILED`.
- The monitor and `runResults` remain active, allowing a late anomaly submission after store close. Pausing only the owner can itself raise `OWNER_STALLED`.
- The final result/anomaly sweeps do not quiesce all submitters before `hstore.Shutdown` cancels the writer.
- `rig.close` stops alerts before Shutdown and releases the instance lock on a refused Shutdown, contradicting its recovery/ownership comments.

The temporary owner-only pause was removed. The retained health-probe lock is a partial repair, not full lifecycle qualification. Required next regression: a serving lifecycle with monitor, balance and result handling active across an intentionally held final-close boundary; assert no post-close record loss/submission, and refused close preserves writer, observation, alerts and instance ownership for a later successful stop. No source gate or q01 promotion follows this review.
