# Operations drill and escalation handoff

The local failure and escalation paths have account-free evidence. No independent
watchdog service is installed or running, and no new external acknowledgment is
claimed. The primary and independent backup route identities and timeout still
need to be supplied. The current Healthchecks endpoint accepted an explicit
failure signal because the monitored harness and heartbeat source are stopped;
see `heartbeat-stopped.json`. Its dashboard state was not independently checked.
Do not send success pings while that source is stopped.

Use the reviewed stage's actual directory and two independently received HTTPS
notification routes. Keep endpoint secrets out of logs and receipts. The adapter
posts JSON containing incident ID, role, source, severity and message; confirm
the actual provider accepts that payload and produces a notification. Distinct
URLs alone do not establish independent recipients or providers.

```sh
cd /Users/hugh/kek/lip
PY=/Users/hugh/kek/.venv/bin/python
FIRST_DIR=/absolute/path/from/operator-stage-preparation
OPS="$FIRST_DIR/operations"
mkdir -p "$OPS"
STATE="$OPS/watchdog.json"
"$PY" scripts/ops_watchdog.py --state "$STATE" init
"$PY" scripts/ops_watchdog_source.py --state "$STATE" \
  --db "$FIRST_DIR/runtime/harness.db" --source-id lip-attended-first once
"$PY" scripts/ops_watchdog.py --state "$STATE" status
```

Initialize once. A missing or corrupt existing watchdog state requires
investigation, not reinitialization. `once` reads committed SEV1 anomalies from
the explicit harness store and replays them idempotently, including anomalies
not yet copied to the auxiliary journal or already accepted by primary HTTP
transport. It does not discover or open collector databases. It persists a
separate source-unavailable incident if the DB is missing, unreadable, corrupt,
uses an unknown schema, or exceeds the bounded scan. Recovery never acknowledges
the incident; a subsequent outage opens another incident.

SQLite uses a read-only connection with `query_only`. Live WAL reads require
existing WAL/SHM sidecars and retain committed records. SQLite read locks and a
sidecar-deletion race mean this is not an absolute no-filesystem-touch guarantee.
The adapter refuses a closed WAL DB whose sidecars are missing; inspect a
separately retained copy for historical analysis instead of changing the store.
The scan is bounded to 1,000 SEV1 rows and five seconds of query work. It does not
derive heartbeat health from database mtime. Feed actual provider missed-check-in
events through `ops_watchdog.py ingest --event-file` using a stable event and
incident ID; retain provider provenance.

After setting `PRIMARY_HTTPS_ROUTE`, `BACKUP_HTTPS_ROUTE` and the reviewed
`BACKUP_AFTER_SECONDS` in the operator's environment, an independent scheduler
must invoke the source adapter and this delivery step at a cadence shorter than
the escalation deadline. Neither command has been scheduled by this session.
Keep scheduling independent of the trader and retain an external dead-man for
whole-host failure. A source read failure must not prevent delivery of its new
source-unavailable incident: run delivery even when `once` exits 1.

```sh
"$PY" scripts/ops_watchdog.py --state "$STATE" tick \
  --primary-url "$PRIMARY_HTTPS_ROUTE" --backup-url "$BACKUP_HTTPS_ROUTE" \
  --backup-after-seconds "$BACKUP_AFTER_SECONDS"
```

For the external drill, ingest a uniquely identified SEV1 drill event, run tick,
and retain the provider receipt and primary operator response timestamp. For a
second incident, deliberately withhold acknowledgment, continue independent
ticks through the deadline, and retain backup delivery and acknowledgment. Test
primary transport failure as well. Restart the adapter between ingestion and
the deadline to prove recovery from the same durable state. Use `ack` only after
an actual operator response:

```sh
"$PY" scripts/ops_watchdog.py --state "$STATE" ack \
  --incident-id ACTUAL_INCIDENT_ID --role backup \
  --operator ACTUAL_OPERATOR --receipt ACTUAL_PROVIDER_RESPONSE_REFERENCE
"$PY" scripts/ops_watchdog.py --state "$STATE" status
```

An HTTP 2xx is transport acceptance. `ack` records an operator assertion and
receipt reference; the script cannot verify a provider conversation. Preserve
that external evidence. Recovery, a successful push, or a clock step does not
autoack. A backwards clock step pages the backup conservatively. Persist-before-
send prevents an undurable attempt from being silently forgotten; a crash after
provider acceptance and before local recording can duplicate an incident, whose
stable ID must be handled by the provider/operator.

The real SQLite contention drill proves failed INSERT → durable stop → retry
after recovery → latched restart refusal. Injected store-health and disk probes
cover stop/reducer preservation with a writable ledger. They do not prove a new
reducer reservation can be recorded on a truly full or lost store. Actual
exposure with an unusable ownership ledger still requires the independent,
attended recovery procedure in [the CR1 runbook](../cr1-operations-runbook.md).
Never provision over it or infer flatness from unavailable truth. Retain every
failed receipt, the old store and journals, independent orders/fills/positions,
owned-only cleanup, final complete flat truth and human disposition of alarms.
