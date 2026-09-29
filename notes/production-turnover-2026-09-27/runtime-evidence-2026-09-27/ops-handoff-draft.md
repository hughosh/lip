# Operations handoff — September 27 continuation

No independent acknowledgment watchdog is installed. The monitored harness is stopped. Healthchecks accepted an explicit failure signal at 17:04:53 UTC (`../heartbeat-stopped-current.json`, HTTP 200, exact `OK`). This establishes transport acceptance only; no new dashboard state, delivery to a person, or human acknowledgment is claimed. Do not send success pings for a stopped source.

The existing [operations command handoff](../../../active-continuation-2026-09-26/ops-operator-handoff.md) remains applicable. Use `BACKUP_AFTER_SECONDS=300`, the existing runbook requirement. The genuinely missing information is the configuration location of the primary HTTPS route and an independent backup HTTPS route, plus the named operators responsible for each. Secret URLs should remain outside receipts. The earlier question for those inputs remains unanswered; no substitute recipient or acknowledgment has been invented.

The account-free HTTP drill exercises primary 202 acceptance, synthetic acknowledgment, nonacknowledgment through the deadline, backup 202 acceptance after durable state reload, and primary 503 followed by backup acceptance. The source adapter's existing tests cover missing/corrupt/unreadable stores, bounded reads, restart idempotence and recovery without autoacknowledgment. See `../ops-python-tests.txt`. These are loopback fixtures, not the required external drill.

Once actual routes and operators are configured, use the existing handoff to ingest a unique SEV1 drill incident, retain primary delivery and the human response, then repeat with primary acknowledgment deliberately withheld through five minutes. Retain independent backup delivery and acknowledgment. Exercise primary transport failure and restart the watchdog before the deadline. Run delivery even when the source adapter reports failure. Record external evidence references with `ack`; provider acceptance alone must never call it. Install and observe the independent scheduler before claiming a running watchdog.

For an unusable ownership store with possible exposure, stop the writer, preserve the original store/WAL/SHM/journal/latch and errors, and obtain complete independent account-wide orders, fills and positions. Do not provision over the store or infer flatness from unreadability. `scripts/ops_recovery.py` now creates an offline recovery plan from retained facts:

```sh
cd /Users/hugh/kek/lip
/Users/hugh/kek/.venv/bin/python scripts/ops_recovery.py \
  --db "$PRESERVED_DB" --journal "$PRESERVED_JOURNAL" --latch "$PRESERVED_LATCH" \
  --snapshot "$INDEPENDENT_ACCOUNT_SNAPSHOT" --ownership "$RETAINED_OWNERSHIP"
```

The file paths must be absolute. The input schema and exercised corrupt-store fixture are in [the recovery plan notes](../ops-recovery-plan.md). Add `--final-snapshot "$LATER_COMPLETE_FLAT_SNAPSHOT"` only when that later independent observation exists. The script validates input assertions; it cannot authenticate completeness, restore a store, execute cleanup, or attest a human response. Exact retained `(coid, order_id, ticker, side)` identities identify owned obligations; prefix-shaped and unmatched orders remain excluded. Mixed or unknown fill attribution remains unresolved. The position plan supplies an obligation, not an executable price or size.

Actual recovery still requires operator-executed owned-only cleanup with fresh authoritative quantity/depth/fees, restoration and reconciliation of the genuine ledger, later complete account-wide zero-order/zero-position proof, and human alarm disposition before a separate restart decision. The assistant session did not execute financial actions or create real exposure to manufacture that evidence. `lip-9vc` remains blocked on external and real-exposure evidence.
