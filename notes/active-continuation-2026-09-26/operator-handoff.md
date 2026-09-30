# Attended first-fill and R2 operator handoff

Prepared candidate: `KXFEATURE-26GTA6-DON`, `S=12`, `rung=sizing`.
The concrete first-stage config is [config.json](operator-stages/first-KXFEATURE-26GTA6-DON-20260927T015607.852703Z/config.json);
[identity](operator-stages/first-KXFEATURE-26GTA6-DON-20260927T015607.852703Z/evidence/identity.json) records binary/source/config hashes.
[Exact operator command sequence](operator-preparation-20260927T015606325796Z.log) was printed, never executed.
The preparation account read ended at **2026-09-27T01:56:14.582286Z** with complete,
flat scope. Its market/account observations expire; rerun the preparation below
immediately before any writer, retaining this snapshot and creating a new stage.
After refreshing, use the **newly printed** stage directory and commands; the
saved concrete paths describe this preparation only. The first-stage runtime
has not been provisioned or armed. Do not paste the whole command sequence as a
single unattended script: its observation/drain boundaries are attended steps.
Use a bounded 45-minute initial observation window; no owned fill by that stop
point is inconclusive. Start planned drain then and stay until cleanup is proved.

This is a future operator procedure, not evidence that a live stage has run.
The user's live trading authorization is already recorded; no new trading
authorization is a prerequisite. The operator still chooses the current
market, fundable size, reviewed candidate, and attended window, and verifies
all gates. `-live` plus the dedicated `live_ok` file enables exchange writes.

Run from `/Users/hugh/kek/lip` with `/Users/hugh/kek/.venv/bin/python`.
`scripts/operator_stage.py` creates a unique evidence/runtime directory from
the archived q01-v3 template. It performs public and authenticated GETs and
prints separate operator-only commands. It does not rerun q01, provision the
store, create `live_ok`, start the harness, or clear a latch. The archived q01
receipt describes only its exact source, binary, and config; review candidate
changes and their relevant qualification before a first writer.

The coordinator supplies
`/Users/hugh/kek/lip/notes/active-continuation-2026-09-26/candidate/build-identity.json`
with `source_manifest_sha256`, `binary_path`, and `binary_sha256` matching the
current tracked and untracked Go source/module manifest and executable. It is
a build identity, not live qualification. The diagnostic candidate below must
still pass fresh program, book and account checks each time the helper runs. The snapshot path is unique because its script refuses an
existing output file.

```sh
cd /Users/hugh/kek/lip
PY=/Users/hugh/kek/.venv/bin/python
TICKER=KXFEATURE-26GTA6-DON
SIZE=12
BIN=/Users/hugh/kek/lip/notes/active-continuation-2026-09-26/candidate/harness
CANDIDATE_RECEIPT=/Users/hugh/kek/lip/notes/active-continuation-2026-09-26/candidate/build-identity.json
PREFLIGHT=/Users/hugh/kek/lip/notes/active-continuation-2026-09-26/operator-stages/preflight
mkdir -p "$PREFLIGHT"
BOOK="$PREFLIGHT/${TICKER}-$(date -u +%Y%m%dT%H%M%SZ)-market-book-series.json"
"$PY" scripts/public_market_snapshot.py --ticker "$TICKER" --out "$BOOK"
"$PY" scripts/operator_stage.py \
  --stage first --ticker "$TICKER" --size "$SIZE" \
  --binary "$BIN" --candidate-receipt "$CANDIDATE_RECEIPT" --book "$BOOK"
```

Record the printed stage directory and actual `config.json`,
`evidence/identity.json`, and account-preflight paths. The generated first
config uses `rung=sizing`, a unique DB/latch/runtime directory, and the
selected `S` (positive, at most 12). Do not substitute an archived config.
The helper checks a current liquidity program, active market, cent book with
depth on both sides, series fee metadata, complete account-wide flatness,
positive selected-shard balance/funding, and reserve/H-CAP-8 fundability.
Its default freshness limit is 60 seconds, so prepare again when evidence
expires. `accountcheck` uses `~/.kalshi` credentials; the config must name
the same paths. Inspect market rules, fees, account identity, all limits and
funding arithmetic and hypothetical exit costs in `exit-arithmetic.json`,
source/binary/config hashes, supervision, and working
external alarms before following any printed operator command.

Provision only the unique first-stage store with the printed `-provision`
command and inspect its result. Arm `live_ok` and start the reviewed binary
only while present for the whole stage. The `sizing` rung durably stops after
the first owned directional fill. Observe actual placement and post-only
behavior, order/fill IDs, fees, cancellation of adding orders, funded bounded
reduction, and authoritative flatness. A no-fill run is inconclusive for fill
handling. SIGTERM requests drain; it does not prove flatness. After the first
process exits, save a fresh complete account-wide orders/positions/fills
report and its timestamp. Retain exit status, DB/WAL/SHM,
anomaly journal, latch, logs, and actual alarm delivery and operator
acknowledgment evidence.

The printed restart command uses string-valued
`-resume 'first-owned-fill stop validation'`. The CLI records that reason
and permits a latched process to start in `WINDING_DOWN`; it does **not**
clear the latch or permit new adds. Before restart, verify the first process
has exited, inspect and preserve the latch, and get another fresh complete
account-wide truth read. Observe adoption, no new add writes, drain, alarms,
and the restarted process's exit. Then collect a **new** complete account-wide
orders/positions/fills report, with its own timestamp. The
first post-drain report cannot stand in for post-restart truth. Incomplete or
nonflat truth, or unresolved ownership, keeps cancellation/reduction attended
and blocks promotion.

For the required reads, set `FIRST_DIR` to the actual printed first-stage
directory and run these GET-only commands at their indicated times. Inspect
`account_scope_complete`, `flat`, each status field, all subaccount positions,
open orders, fill rows, and request timestamps in both saved reports. The
`accountcheck` report is an allowlisted exchange view; retain underlying raw
responses separately if the independent operator route provides them.

```sh
FIRST_DIR=/Users/hugh/kek/lip/notes/active-continuation-2026-09-26/operator-stages/first-KXFEATURE-26GTA6-DON-20260927T015607.852703Z
cd /Users/hugh/kek/lip/go
go run ./cmd/accountcheck -ticker "$TICKER" -fills -out "$FIRST_DIR/evidence/account-before-restart.json"
# Run the reviewed latched -resume command only after reviewing this report.
# After the restarted process exits:
go run ./cmd/accountcheck -ticker "$TICKER" -fills -out "$FIRST_DIR/evidence/account-after-restart.json"
```

Identify the exact harness PID before SIGTERM; never guess or use `pkill -f`.
After the **last** process is verified absent, remove only that unique stage's
`live_ok` arming file and record the removal. Do not remove it while a writer
or reducer may still need exchange writes. Preserve DB/WAL/SHM, journal,
halt latch, account reports, and receipts. Never automatically remove or
truncate the halt latch. Do not bootstrap launchd for this attended stage
without review of the exact installed job and restart policy: bootstrap
starts a writer.

R2 requires the completed first-stage config and an operator evidence receipt
bound to its ticker, source manifest hash, binary hash, and config SHA-256.
Every one of the seven acceptance fields must be an object with
`operator_outcome` of `passed` or `observed`, a nonempty `operator`, a
parseable timezone-aware `recorded_at_utc` within the first-stage observation
interval (no future attestation), a concrete `observation`, and a nonempty
evidence list. The candidate and each evidence item also bind to the SHA256
of the first stage’s `evidence/identity.json`.
Each item must have a relative `path` under the receipt's directory and the
real file's `sha256`; the helper checks the file and hash. Boolean acceptance
values fail. Populate this shape with actual candidate-bound records,
including provider alarm delivery and human acknowledgment, never planned
events. These illustrative hashes and timestamps must be replaced:

```json
{
  "candidate": {
    "ticker": "ACTUAL_FIRST_STAGE_TICKER",
    "source_manifest_sha256": "ACTUAL_FIRST_STAGE_SOURCE_HASH",
    "binary_sha256": "ACTUAL_FIRST_STAGE_BINARY_HASH",
    "config_sha256": "ACTUAL_FIRST_STAGE_CONFIG_HASH",
    "first_stage_identity_sha256": "SHA256_OF_FIRST_STAGE_EVIDENCE_IDENTITY_JSON"
  },
  "acceptance": {
    "orders": {
      "operator_outcome": "observed",
      "operator": "OPERATOR_NAME",
      "recorded_at_utc": "2026-09-27T00:00:00Z",
      "evidence": [
        {
          "path": "orders.json",
          "sha256": "ACTUAL_FILE_HASH",
          "first_stage_identity_sha256": "SHA256_OF_FIRST_STAGE_EVIDENCE_IDENTITY_JSON"
        }
      ],
      "observation": "Describe the actual orders observation and its attributable identifiers."
    },
    "fills": {
      "operator_outcome": "observed",
      "operator": "OPERATOR_NAME",
      "recorded_at_utc": "2026-09-27T00:00:00Z",
      "evidence": [
        {
          "path": "fills.json",
          "sha256": "ACTUAL_FILE_HASH",
          "first_stage_identity_sha256": "SHA256_OF_FIRST_STAGE_EVIDENCE_IDENTITY_JSON"
        }
      ],
      "observation": "Describe the actual fills observation and its attributable identifiers."
    },
    "fees": {
      "operator_outcome": "observed",
      "operator": "OPERATOR_NAME",
      "recorded_at_utc": "2026-09-27T00:00:00Z",
      "evidence": [
        {
          "path": "fees.json",
          "sha256": "ACTUAL_FILE_HASH",
          "first_stage_identity_sha256": "SHA256_OF_FIRST_STAGE_EVIDENCE_IDENTITY_JSON"
        }
      ],
      "observation": "Describe the actual fees observation and its attributable identifiers."
    },
    "reduction": {
      "operator_outcome": "observed",
      "operator": "OPERATOR_NAME",
      "recorded_at_utc": "2026-09-27T00:00:00Z",
      "evidence": [
        {
          "path": "reduction.json",
          "sha256": "ACTUAL_FILE_HASH",
          "first_stage_identity_sha256": "SHA256_OF_FIRST_STAGE_EVIDENCE_IDENTITY_JSON"
        }
      ],
      "observation": "Describe the actual reduction observation and its attributable identifiers."
    },
    "clean_exit": {
      "operator_outcome": "passed",
      "operator": "OPERATOR_NAME",
      "recorded_at_utc": "2026-09-27T00:00:00Z",
      "evidence": [
        {
          "path": "clean-exit-and-account.json",
          "sha256": "ACTUAL_FILE_HASH",
          "first_stage_identity_sha256": "SHA256_OF_FIRST_STAGE_EVIDENCE_IDENTITY_JSON"
        }
      ],
      "observation": "Describe the actual clean_exit observation and its attributable identifiers."
    },
    "restart": {
      "operator_outcome": "passed",
      "operator": "OPERATOR_NAME",
      "recorded_at_utc": "2026-09-27T00:00:00Z",
      "evidence": [
        {
          "path": "restart-and-account.json",
          "sha256": "ACTUAL_FILE_HASH",
          "first_stage_identity_sha256": "SHA256_OF_FIRST_STAGE_EVIDENCE_IDENTITY_JSON"
        }
      ],
      "observation": "Describe the actual restart observation and its attributable identifiers."
    },
    "alarm": {
      "operator_outcome": "observed",
      "operator": "OPERATOR_NAME",
      "recorded_at_utc": "2026-09-27T00:00:00Z",
      "evidence": [
        {
          "path": "provider-delivery-and-human-ack.json",
          "sha256": "ACTUAL_FILE_HASH",
          "first_stage_identity_sha256": "SHA256_OF_FIRST_STAGE_EVIDENCE_IDENTITY_JSON"
        }
      ],
      "observation": "Describe the actual alarm observation and its attributable identifiers."
    }
  }
}
```

After recording the actual first-stage directory, put the receipt and its
evidence files together. Capture a **new** public snapshot and GET-only
preflight. Use the same reviewed candidate and current account-derived size.
The coordinator fills the two actual first-stage paths when they exist:

```sh
cd /Users/hugh/kek/lip
PY=/Users/hugh/kek/.venv/bin/python
TICKER=KXFEATURE-26GTA6-DON
SIZE=12
BIN=/Users/hugh/kek/lip/notes/active-continuation-2026-09-26/candidate/harness
CANDIDATE_RECEIPT=/Users/hugh/kek/lip/notes/active-continuation-2026-09-26/candidate/build-identity.json
FIRST_CONFIG=/Users/hugh/kek/lip/notes/active-continuation-2026-09-26/operator-stages/first-KXFEATURE-26GTA6-DON-20260927T015607.852703Z/config.json
R2_RECEIPT=/Users/hugh/kek/lip/notes/active-continuation-2026-09-26/operator-stages/first-KXFEATURE-26GTA6-DON-20260927T015607.852703Z/evidence/first-stage-receipt.json
PREFLIGHT=/Users/hugh/kek/lip/notes/active-continuation-2026-09-26/operator-stages/preflight
mkdir -p "$PREFLIGHT"
BOOK="$PREFLIGHT/${TICKER}-$(date -u +%Y%m%dT%H%M%SZ)-r2-market-book-series.json"
"$PY" scripts/public_market_snapshot.py --ticker "$TICKER" --out "$BOOK"
"$PY" scripts/operator_stage.py \
  --stage r2 --ticker "$TICKER" --size "$SIZE" \
  --binary "$BIN" --candidate-receipt "$CANDIDATE_RECEIPT" \
  --book "$BOOK" --prior-config "$FIRST_CONFIG" --r2-receipt "$R2_RECEIPT"
```

> **Superseded 2026-09-30.** `--stage r2` now provisions a fresh store on a
> freshly chosen market. It binds to a first stage run on the current candidate
> or on the predecessor its build receipt records. Alarm receipt no longer
> gates attended stages (runbook, "Alert route"). See
> [operator-handoff-candidate-6.md](../first-fill-evidence-2026-09-28/operator-handoff-candidate-6.md).
> The paragraphs below are the 2026-09-26 design.

R2 derives `rung=pilot` while keeping the first-stage DB, latch, lock, and
sentinel paths. Receipt acceptance gates command printing, not independent
proof of its claimed events; inspect the underlying records. If a latch
remains, a human must establish complete exchange truth, resolve anomalies,
and separately adjudicate clearing it under §10.4 before new adds. The
helper provides no latch-removal command. Attend the add, owned fill,
reduce-to-flat, restart/adoption, alarm, and final fresh complete account-wide
no-orders/no-positions cycle. No fill or incomplete truth is inconclusive.

Stop or withhold promotion on stale/incomplete book or account data, unknown
orders/inventory, failed funding arithmetic, identity drift, unmatched
receipts, failed cancellation/reduction, missing clean exit, new adds after
the first-fill latch, unavailable disk/store or alert routes, or absent human
SEV1 response. For recovery, follow [the CR1 runbook](../cr1-operations-runbook.md):
a missing/corrupt store with possible exposure requires independent exchange
truth and attended reduction, never replacement provisioning. The real
exposure/store-loss drill and acknowledgment-aware primary/backup escalation
drill remain unproved; save actual provider delivery, backup escalation after
primary nonacknowledgment, and human acknowledgment timestamps and receipts.
These operations routes block unattended promotion. CR2 selection and
turnover across markets remain unconnected and unqualified. One-market stages
do not establish continuous participation through program expiry.

The R2 helper requires the original first-stage `evidence/identity.json` and
checks all inherited runtime paths against that stage. It explicitly records
`artifact_content_verified: false`: matching file hashes and operator
attestations do not independently establish real trades or provider delivery.
The coordinator/operator must adjudicate the underlying evidence. See
[operations drill commands](ops-operator-handoff.md) for the independent SEV1
source, primary/backup escalation and acknowledgment workflow.
