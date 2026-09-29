#!/usr/bin/env python3
"""Standalone, operator-run incident adapter. It is not wired to the harness.

The state is authoritative for notification scheduling, not for exchange risk.
An HTTPS 2xx means transport acceptance; only ``ack`` records a human claim.
"""

from __future__ import annotations

import argparse
import contextlib
import fcntl
import json
import os
from pathlib import Path
import tempfile
import time
import urllib.error
import urllib.parse
import urllib.request


SCHEMA = 1
RETRY_MS = 30_000
SEVERITIES = {"SEV1", "MISSED_HEARTBEAT", "RECOVERY"}


class StateError(Exception):
    pass


def _fields(obj, names, where):
    if not isinstance(obj, dict) or set(obj) != set(names):
        raise StateError(f"invalid {where} fields")


def _string(value, where):
    if not isinstance(value, str) or not value or len(value) > 256:
        raise StateError(f"invalid {where}")
    return value


def validate(state):
    _fields(state, ("schema", "events", "incidents"), "state")
    if state["schema"] != SCHEMA or not isinstance(state["events"], dict) or not isinstance(state["incidents"], dict):
        raise StateError("unsupported or corrupt state")
    for event_id, event in state["events"].items():
        _string(event_id, "event ID")
        parse_event(event)
        if event["event_id"] != event_id or event["incident_id"] not in state["incidents"]:
            raise StateError("event refers to missing incident")
    for incident_id, item in state["incidents"].items():
        _string(incident_id, "incident ID")
        _fields(item, ("source", "severity", "message", "first_seen_ms", "primary", "backup", "ack"), "incident")
        _string(item["source"], "source")
        _string(item["message"], "message")
        if not isinstance(item["severity"], str) or item["severity"] not in SEVERITIES - {"RECOVERY"}:
            raise StateError("invalid incident severity")
        if type(item["first_seen_ms"]) is not int or item["first_seen_ms"] <= 0:
            raise StateError("invalid incident time")
        for role in ("primary", "backup"):
            delivery = item[role]
            expected = {"attempts", "last_attempt_ms", "sent_ms"}
            if role == "backup" and "unconfigured" in delivery:
                expected.add("unconfigured")
                if type(delivery["unconfigured"]) is not bool:
                    raise StateError("invalid backup configuration")
            _fields(delivery, expected, role)
            if type(delivery["attempts"]) is not int or delivery["attempts"] < 0:
                raise StateError("invalid attempt count")
            for key in ("last_attempt_ms", "sent_ms"):
                v = delivery[key]
                if v is not None and (type(v) is not int or v <= 0):
                    raise StateError("invalid delivery time")
            if delivery["attempts"] == 0 and delivery["last_attempt_ms"] is not None:
                raise StateError("attempt time without attempt")
            if delivery["sent_ms"] is not None and delivery["attempts"] == 0:
                raise StateError("delivery without attempt")
        ack = item["ack"]
        if ack is not None:
            _fields(ack, ("role", "operator", "receipt", "at_ms"), "ack")
            if ack["role"] not in ("primary", "backup"):
                raise StateError("invalid ack role")
            _string(ack["operator"], "operator")
            _string(ack["receipt"], "ack receipt")
            if type(ack["at_ms"]) is not int or ack["at_ms"] <= 0:
                raise StateError("invalid ack time")
    return state


def load(path: Path):
    try:
        with path.open("r", encoding="utf-8") as handle:
            return validate(json.load(handle))
    except FileNotFoundError as exc:
        raise StateError("state is missing; run init explicitly") from exc
    except (OSError, UnicodeError, ValueError, TypeError) as exc:
        raise StateError("state cannot be read or is corrupt") from exc


@contextlib.contextmanager
def locked(path: Path):
    if not path.is_absolute():
        raise StateError("state path must be absolute")
    if not path.parent.is_dir():
        raise StateError("state parent directory is missing")
    fd = os.open(str(path) + ".lock", os.O_CREAT | os.O_RDWR, 0o600)
    try:
        fcntl.flock(fd, fcntl.LOCK_EX)
        yield
    finally:
        fcntl.flock(fd, fcntl.LOCK_UN)
        os.close(fd)


def save(path: Path, state):
    validate(state)
    payload = (json.dumps(state, sort_keys=True, separators=(",", ":")) + "\n").encode()
    fd, name = tempfile.mkstemp(prefix=path.name + ".", dir=path.parent)
    try:
        os.fchmod(fd, 0o600)
        with os.fdopen(fd, "wb") as handle:
            handle.write(payload)
            handle.flush()
            os.fsync(handle.fileno())
        os.replace(name, path)
        dir_fd = os.open(path.parent, os.O_RDONLY)
        try:
            os.fsync(dir_fd)
        finally:
            os.close(dir_fd)
    finally:
        if os.path.exists(name):
            os.unlink(name)


def init(path: Path):
    with locked(path):
        if path.exists():
            raise StateError("state already exists")
        save(path, {"schema": SCHEMA, "events": {}, "incidents": {}})


def parse_event(raw):
    _fields(raw, ("event_id", "incident_id", "source", "severity", "message"), "event")
    for key in ("event_id", "incident_id", "source", "message"):
        _string(raw[key], key)
    if not isinstance(raw["severity"], str) or raw["severity"] not in SEVERITIES:
        raise StateError("invalid severity")
    return raw


def ingest(path: Path, raw, now_ms: int):
    event = parse_event(raw)
    with locked(path):
        state = load(path)
        event_id, incident_id = event["event_id"], event["incident_id"]
        if event_id in state["events"]:
            if state["events"][event_id] != event:
                raise StateError("event ID reused with different content")
            return False
        item = state["incidents"].get(incident_id)
        if item is None:
            if event["severity"] == "RECOVERY":
                raise StateError("recovery for unknown incident")
            item = {
                "source": event["source"], "severity": event["severity"],
                "message": event["message"], "first_seen_ms": now_ms,
                "primary": {"attempts": 0, "last_attempt_ms": None, "sent_ms": None},
                "backup": {"attempts": 0, "last_attempt_ms": None, "sent_ms": None},
                "ack": None,
            }
            state["incidents"][incident_id] = item
        elif event["source"] != item["source"]:
            raise StateError("incident source changed")
        elif event["severity"] not in (item["severity"], "RECOVERY"):
            raise StateError("incident severity changed")
        state["events"][event_id] = event
        save(path, state)
        return True


def ack(path: Path, incident_id: str, role: str, operator: str, receipt: str, now_ms: int):
    _string(incident_id, "incident ID")
    _string(operator, "operator")
    _string(receipt, "ack receipt")
    if role not in ("primary", "backup"):
        raise StateError("ack role must be primary or backup")
    with locked(path):
        state = load(path)
        item = state["incidents"].get(incident_id)
        if item is None:
            raise StateError("unknown incident")
        if item["ack"] is not None:
            if item["ack"]["role"] == role and item["ack"]["operator"] == operator and item["ack"]["receipt"] == receipt:
                return False
            raise StateError("incident already acknowledged")
        item["ack"] = {"role": role, "operator": operator, "receipt": receipt, "at_ms": now_ms}
        save(path, state)
        return True


def _endpoint(url):
    parsed = urllib.parse.urlsplit(url)
    if parsed.scheme != "https" or not parsed.hostname or parsed.username or parsed.password or parsed.fragment:
        raise StateError("notification endpoint must be an HTTPS URL without credentials or fragment")
    return url


class _NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, request, fp, code, msg, headers, newurl):
        return None


def https_send(url, payload):
    req = urllib.request.Request(url, data=json.dumps(payload).encode(), headers={"Content-Type": "application/json"}, method="POST")
    try:
        with urllib.request.build_opener(_NoRedirect).open(req, timeout=10) as response:
            return 200 <= response.status < 300
    except (urllib.error.URLError, OSError, ValueError):
        return False


def tick(path: Path, primary_url: str, backup_url: str | None, timeout_ms: int, now_ms: int, send=https_send):
    _endpoint(primary_url)
    if backup_url is not None:
        _endpoint(backup_url)
        if primary_url == backup_url:
            raise StateError("primary and backup endpoints must differ")
    if type(timeout_ms) is not int or timeout_ms <= 0:
        raise StateError("backup timeout must be positive")
    outcomes = []
    with locked(path):
        state = load(path)
        for incident_id, item in state["incidents"].items():
            unconfigured = backup_url is None
            if item["backup"].get("unconfigured") != unconfigured:
                item["backup"]["unconfigured"] = unconfigured
                save(path, state)
            if item["ack"] is not None:
                continue
            # Persisted wall deadlines cannot measure elapsed time after a
            # backward clock step. Fail toward paging: release the backup
            # immediately and allow failed delivery retries on this tick.
            # Each operator/scheduler tick is bounded to one attempt per role;
            # successful deliveries still deduplicate normally. This must run
            # from an independent scheduler/provider, not from the trader.
            latest = max([item["first_seen_ms"]] + [
                item[role]["last_attempt_ms"] or 0
                for role in ("primary", "backup")])
            clock_regressed = now_ms < latest
            routes = (("primary", primary_url),) if unconfigured else (("primary", primary_url), ("backup", backup_url))
            for role, url in routes:
                delivery = item[role]
                if role == "backup" and delivery["attempts"] == 0 and not clock_regressed and now_ms < item["first_seen_ms"] + timeout_ms:
                    continue
                if delivery["sent_ms"] is not None:
                    continue
                if not clock_regressed and delivery["last_attempt_ms"] is not None and now_ms < delivery["last_attempt_ms"] + RETRY_MS:
                    continue
                # Durable attempt intent precedes every external side effect.
                delivery["attempts"] += 1
                delivery["last_attempt_ms"] = now_ms
                save(path, state)
                payload = {"incident_id": incident_id, "role": role, "source": item["source"],
                           "severity": item["severity"], "message": item["message"],
                           "first_seen_ms": item["first_seen_ms"]}
                try:
                    sent = bool(send(url, payload))
                except Exception:
                    sent = False
                if sent:
                    delivery["sent_ms"] = now_ms
                    save(path, state)
                outcome = {"incident_id": incident_id, "role": role,
                           "transport_accepted": sent,
                           "clock_regressed": clock_regressed}
                if unconfigured:
                    outcome["backup_unconfigured"] = True
                outcomes.append(outcome)
    return outcomes


def status(path: Path):
    with locked(path):
        return load(path)


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--state", required=True, type=Path)
    sub = parser.add_subparsers(dest="command", required=True)
    sub.add_parser("init")
    ingest_parser = sub.add_parser("ingest")
    ingest_parser.add_argument("--event-file", required=True, type=Path)
    tick_parser = sub.add_parser("tick")
    tick_parser.add_argument("--primary-url", required=True)
    tick_routes = tick_parser.add_mutually_exclusive_group(required=True)
    tick_routes.add_argument("--backup-url")
    tick_routes.add_argument("--primary-only", action="store_true")
    tick_parser.add_argument("--backup-after-seconds", type=int, default=300)
    ack_parser = sub.add_parser("ack")
    ack_parser.add_argument("--incident-id", required=True)
    ack_parser.add_argument("--role", required=True, choices=("primary", "backup"))
    ack_parser.add_argument("--operator", required=True)
    ack_parser.add_argument("--receipt", required=True)
    sub.add_parser("status")
    args = parser.parse_args(argv)
    try:
        now_ms = int(time.time() * 1000)
        if args.command == "init":
            init(args.state)
            result = {"initialized": True}
        elif args.command == "ingest":
            with args.event_file.open("r", encoding="utf-8") as handle:
                result = {"ingested": ingest(args.state, json.load(handle), now_ms)}
        elif args.command == "tick":
            result = {"attempts": tick(args.state, args.primary_url, args.backup_url,
                                       args.backup_after_seconds * 1000, now_ms, send=https_send),
                      "backup_unconfigured": args.primary_only}
        elif args.command == "ack":
            result = {"acknowledged": ack(args.state, args.incident_id, args.role,
                                          args.operator, args.receipt, now_ms)}
        else:
            result = status(args.state)
        print(json.dumps(result, sort_keys=True))
        return 0
    except (StateError, OSError, UnicodeError, ValueError) as exc:
        # Transport endpoints can be bearer secrets. Never print raw exceptions.
        print(f"ops_watchdog: {exc if isinstance(exc, StateError) else 'input or filesystem failure'}", file=os.sys.stderr)
        return 2


if __name__ == "__main__":
    raise SystemExit(main())
