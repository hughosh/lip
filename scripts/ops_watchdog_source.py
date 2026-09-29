#!/usr/bin/env python3
"""Import committed harness SEV1 anomalies into the independent ops watchdog.

Run ``once`` from an independent scheduler. This process opens the harness
SQLite database read-only; it never starts the harness, providers, or trading.
Run the watchdog's separate ``tick`` command to deliver/escalate incidents.
Neither a harness delivery timestamp nor a successful HTTP push is a human ack.
Missed provider heartbeats require a separate, explicit watchdog ingest.
"""

from __future__ import annotations

import argparse
from contextlib import closing
import hashlib
import json
from pathlib import Path
import re
import sqlite3
import stat
import sys
import time
from urllib.parse import quote

try:
    from scripts import ops_watchdog as watchdog
except ModuleNotFoundError:  # Direct invocation from scripts/.
    import ops_watchdog as watchdog


SCHEMA_VERSION = 3  # go/harness/hstore/schema.go
SEV1 = 2  # go/harness/risk/monitor.go
MAX_ROWS = 1_000
SCAN_SECONDS = 5
UNAVAILABLE_MESSAGE = (
    "SOURCE_UNAVAILABLE: harness anomaly SQLite source cannot be read; "
    "inspect the configured source"
)


class SourceError(Exception):
    pass


def _source(source_id: str) -> str:
    if not isinstance(source_id, str) or not re.fullmatch(r"[A-Za-z0-9._:-]{1,200}", source_id):
        raise SourceError("source ID must contain 1-200 letters, digits, dots, colons, dashes, or underscores")
    return "hstore:" + source_id


def _digest(*parts: str) -> str:
    data = json.dumps(parts, ensure_ascii=False, separators=(",", ":")).encode("utf-8")
    return hashlib.sha256(data).hexdigest()[:32]


def _anomaly_event(source_id: str, row: tuple) -> dict:
    anomaly_id, run_id, klass, ticker, detail, first_ms = row
    if (not all(isinstance(value, str) and value for value in
                (anomaly_id, run_id, klass, detail)) or
            not isinstance(ticker, str) or
            type(first_ms) is not int or first_ms <= 0):
        raise SourceError("harness anomaly contains invalid immutable fields")
    identity = _digest(source_id, anomaly_id)
    where = ticker or "account"
    message = f"{klass} {where} @ {first_ms}: {detail}"[:256]
    return {"event_id": f"hstore-event-{identity}",
            "incident_id": f"hstore-incident-{identity}",
            "source": _source(source_id), "severity": "SEV1", "message": message}


def read_events(db_path: Path, source_id: str) -> list[dict]:
    """Take one bounded, read-only SQLite snapshot of all committed SEV1 rows.

    The mutable journaled/delivered fields are intentionally ignored: a
    committed SEV1 remains actionable if the auxiliary journal failed, and a
    later delivery update must not change the watchdog event's identity/content.
    """
    _source(source_id)
    if not db_path.is_absolute():
        raise SourceError("database path must be absolute")
    try:
        if not stat.S_ISREG(db_path.stat().st_mode):
            raise SourceError("database source is not a regular file")
        with db_path.open("rb") as handle:
            header = handle.read(20)
        if len(header) != 20 or header[:16] != b"SQLite format 3\x00":
            raise SourceError("harness SQLite source has an invalid header")
        if header[18:20] == b"\x02\x02":
            # SQLite can create WAL sidecars even for a mode=ro connection if
            # the directory is writable. Refuse when either is absent instead
            # of letting this adapter create a collector-owned file.
            for suffix in ("-wal", "-shm"):
                sidecar = Path(str(db_path) + suffix)
                if not sidecar.exists() or not stat.S_ISREG(sidecar.stat().st_mode):
                    raise SourceError("WAL source lacks a readable sidecar")
        uri = "file:" + quote(str(db_path), safe="/") + "?mode=ro"
        deadline = time.monotonic() + SCAN_SECONDS
        with closing(sqlite3.connect(uri, uri=True, timeout=1, isolation_level=None)) as db:
            db.execute("PRAGMA query_only=ON")
            db.set_progress_handler(lambda: int(time.monotonic() >= deadline), 1000)
            db.execute("BEGIN")
            version = db.execute("PRAGMA user_version").fetchone()[0]
            if version != SCHEMA_VERSION:
                raise SourceError("unsupported harness store schema version")
            rows = db.execute(
                "SELECT anomaly_id, run_id, class, ticker, text, first_ms "
                "FROM anomaly WHERE sev = ? ORDER BY first_ms, anomaly_id LIMIT ?",
                (SEV1, MAX_ROWS + 1),
            ).fetchall()
            if len(rows) > MAX_ROWS:
                raise SourceError("harness SEV1 backlog exceeds bounded scan limit")
            events = [_anomaly_event(source_id, row) for row in rows]
            db.execute("ROLLBACK")
            return events
    except SourceError:
        raise
    except (OSError, sqlite3.Error, TypeError, ValueError) as exc:
        raise SourceError("harness SQLite source is missing, unreadable, or corrupt") from exc


def _outage_ids(source_id: str, number: int) -> tuple[str, str, str]:
    prefix = "hstore-source-" + _digest(source_id)
    return (f"{prefix}-unavailable-{number}",
            f"{prefix}-incident-{number}",
            f"{prefix}-recovered-{number}")


def _last_outage(state: dict, source_id: str) -> int:
    prefix = "hstore-source-" + _digest(source_id) + "-unavailable-"
    numbers = [int(event_id[len(prefix):]) for event_id in state["events"]
               if event_id.startswith(prefix) and
               re.fullmatch(r"[1-9][0-9]*", event_id[len(prefix):])]
    return max(numbers, default=0)


def _record_unavailable(state_path: Path, source_id: str, now_ms: int) -> bool:
    state = watchdog.status(state_path)
    number = _last_outage(state, source_id)
    if number:
        _, _, recovery_id = _outage_ids(source_id, number)
        if recovery_id in state["events"]:
            number += 1
    else:
        number = 1
    event_id, incident_id, _ = _outage_ids(source_id, number)
    return watchdog.ingest(state_path, {
        "event_id": event_id, "incident_id": incident_id,
        "source": _source(source_id), "severity": "SEV1",
        "message": UNAVAILABLE_MESSAGE,
    }, now_ms)


def _record_recovery(state_path: Path, source_id: str, now_ms: int) -> bool:
    state = watchdog.status(state_path)
    number = _last_outage(state, source_id)
    if not number:
        return False
    _, incident_id, event_id = _outage_ids(source_id, number)
    if event_id in state["events"]:
        return False
    return watchdog.ingest(state_path, {
        "event_id": event_id, "incident_id": incident_id,
        "source": _source(source_id), "severity": "RECOVERY",
        "message": "Harness anomaly SQLite source is readable again; human ack remains required",
    }, now_ms)


def run_once(state_path: Path, db_path: Path, source_id: str, now_ms: int) -> dict:
    """Import a whole snapshot; if it fails, persist a source outage incident."""
    try:
        events = read_events(db_path, source_id)
    except SourceError as exc:
        created = _record_unavailable(state_path, source_id, now_ms)
        return {"source_available": False, "source_unavailable_ingested": created,
                "reason": str(exc)}
    imported = 0
    try:
        for event in events:
            imported += int(watchdog.ingest(state_path, event, now_ms))
    except watchdog.StateError as exc:
        # A stored anomaly ID with changed immutable content is source trouble.
        # State corruption also fails this write loudly instead of inventing truth.
        created = _record_unavailable(state_path, source_id, now_ms)
        return {"source_available": False, "source_unavailable_ingested": created,
                "reason": f"harness event conflicts with persisted watchdog state: {exc}"}
    recovered = _record_recovery(state_path, source_id, now_ms)
    return {"source_available": True, "scanned": len(events),
            "ingested": imported, "source_recovery_ingested": recovered}


def main(argv=None) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--state", required=True, type=Path,
                        help="existing absolute ops_watchdog state path")
    parser.add_argument("--db", required=True, type=Path,
                        help="absolute harness SQLite store path")
    parser.add_argument("--source-id", required=True,
                        help="stable explicit identity for this harness store")
    parser.add_argument("command", choices=("once",))
    args = parser.parse_args(argv)
    try:
        result = run_once(args.state, args.db, args.source_id, int(time.time() * 1000))
        print(json.dumps(result, sort_keys=True))
        return 0 if result["source_available"] else 1
    except (watchdog.StateError, SourceError, OSError, ValueError) as exc:
        print(f"ops_watchdog_source: {exc}", file=sys.stderr)
        return 2


if __name__ == "__main__":
    raise SystemExit(main())
