# Account-free operations work, 2026-09-27

The new `scripts/ops_recovery.py` is a read-only preflight for an explicitly
named harness store, anomaly journal, and latch. It records presence, size and
SHA-256 of the store and sidecars, recognizes missing/empty/invalid-header and
WAL-sidecar-missing cases. Successful preflight output reports a blocked recovery status with the
operator evidence required by `notes/cr1-operations-runbook.md`; malformed inputs
or file-read errors instead exit with an error and no status JSON. A SQLite header
alone is reported as integrity unverified. The tool never opens a SQLite
connection, repairs or provisions a store, reads credentials, starts a writer,
or calls an exchange. Its file facts are observations, not an atomic snapshot;
stop the writer and retain the original files before using them as evidence.

Account-free fixture command:

```sh
/Users/hugh/kek/.venv/bin/python scripts/ops_recovery.py \
  --db /Users/hugh/kek/lip/notes/production-turnover-2026-09-27/ops-fixture/harness.db \
  --journal /Users/hugh/kek/lip/notes/production-turnover-2026-09-27/ops-fixture/anomalies.jsonl \
  --latch /Users/hugh/kek/lip/notes/production-turnover-2026-09-27/ops-fixture/halt.latch
```

The saved [fixture result](ops-recovery-corrupt-fixture.json) reports
`invalid_header` and a blocked status. The three fixture files are retained in
`ops-fixture/`. The affected [Python test receipt](ops-python-tests.txt) has
21 passing tests. In addition to existing source-outage and mocked watchdog
checks, the new loopback HTTP test exercises primary 202 acceptance, a synthetic
test acknowledgment, primary nonacknowledgment through the backup deadline,
backup 202 acceptance after state reload, and primary 503 followed by backup
acceptance. The test bypasses HTTPS endpoint validation only inside its fixture;
the production command still requires two distinct HTTPS URLs. No test
acknowledgment is an external human acknowledgment.

For real recovery, the missing inputs remain an actual stopped-writer/latch
receipt, preserved unusable store and errors, timestamped complete independent
account-wide orders/fills/positions, recovered ownership ledger and reconciliation,
owned-only cleanup receipts, final complete flat account truth, and human alarm
disposition. Without them, no restart or flatness conclusion follows from this
work. For external escalation, the handoff still lacks named primary and
independent backup recipients/routes, deployed scheduler, provider delivery and
response receipts, verified dashboard state, and an end-to-end nonacknowledgment
drill. The runbook requires five-minute backup escalation; the operational
`BACKUP_AFTER_SECONDS=300` setting and actual route behavior remain unverified.
