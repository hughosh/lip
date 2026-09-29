#!/usr/bin/env python3
"""Read-only preflight for an unusable harness ownership store.

This records file facts and the evidence still required for attended recovery.
It never opens SQLite, provisions a store, infers ownership, or declares flatness.
"""

from __future__ import annotations

import argparse
from datetime import datetime, timezone
import hashlib
import json
from pathlib import Path
import stat
import sys


class RecoveryError(Exception):
    pass


def _file_fact(path: Path) -> dict:
    if not path.is_absolute():
        raise RecoveryError("all paths must be absolute")
    try:
        info = path.lstat()
    except FileNotFoundError:
        return {"path": str(path), "status": "missing"}
    if not stat.S_ISREG(info.st_mode):
        return {"path": str(path), "status": "not_regular_file"}
    digest = hashlib.sha256()
    try:
        with path.open("rb") as handle:
            header = handle.read(20)
            digest.update(header)
            for chunk in iter(lambda: handle.read(1024 * 1024), b""):
                digest.update(chunk)
    except OSError:
        return {"path": str(path), "status": "unreadable"}
    return {"path": str(path), "status": "readable", "bytes": info.st_size,
            "sha256": digest.hexdigest(), "header_hex": header.hex()}


def inspect(db: Path, journal: Path, latch: Path) -> dict:
    paths = {"store": db, "wal": Path(str(db) + "-wal"),
             "shm": Path(str(db) + "-shm"), "journal": journal, "latch": latch}
    facts = {name: _file_fact(path) for name, path in paths.items()}
    store = facts["store"]
    if store["status"] != "readable":
        condition = store["status"]
    elif store["bytes"] == 0:
        condition = "zero_length"
    elif not bytes.fromhex(store["header_hex"]).startswith(b"SQLite format 3\x00"):
        condition = "invalid_header"
    elif bytes.fromhex(store["header_hex"])[18:20] == b"\x02\x02" and any(
            facts[name]["status"] != "readable" for name in ("wal", "shm")):
        condition = "wal_sidecars_missing"
    else:
        condition = "header_present_integrity_unverified"
    # File facts can change during a live write. The operator must stop the
    # writer and preserve the original files before using this as evidence.
    for fact in facts.values():
        fact.pop("header_hex", None)
    return {
        "store_condition": condition,
        "recovery_status": "BLOCKED_PENDING_INDEPENDENT_ACCOUNT_TRUTH_AND_OPERATOR_REVIEW",
        "files": facts,
        "required_evidence": [
            "writer stopped and unreviewed restart prevented; stop/latch receipt",
            "original store, WAL, SHM, journal, errors and hashes preserved",
            "timestamped complete independent account-wide orders, fills and positions",
            "ownership ledger restored from real evidence and reconciled to exchange truth",
            "owned-only cancellation/reduction receipts and unresolved exposure disposition",
            "final complete flat account truth and human alarm disposition before restart",
        ],
        "restriction": "No provisioning, new adds, flatness claim, or restart from this preflight",
    }


IDENTITY = ("coid", "order_id", "ticker", "side")


def _object(value, label: str) -> dict:
    if not isinstance(value, dict):
        raise RecoveryError(f"{label} must be an object")
    return value


def _identity(value, label: str) -> tuple[str, str, str, str]:
    row = _object(value, label)
    result = []
    for field in IDENTITY:
        item = row.get(field)
        if not isinstance(item, str) or not item.strip():
            raise RecoveryError(f"{label}.{field} must be a nonempty string")
        result.append(item)
    if result[3] not in ("yes", "no"):
        raise RecoveryError(f"{label}.side must be yes or no")
    return tuple(result)


def _rows(value, label: str) -> list:
    if not isinstance(value, list):
        raise RecoveryError(f"{label} must be an array")
    return value


def _time(value, label: str) -> datetime:
    if not isinstance(value, str) or not value.endswith("Z"):
        raise RecoveryError(f"{label} must be a UTC timestamp ending in Z")
    try:
        parsed = datetime.fromisoformat(value.replace("Z", "+00:00"))
    except ValueError as exc:
        raise RecoveryError(f"{label} is invalid") from exc
    if parsed.tzinfo != timezone.utc:
        raise RecoveryError(f"{label} must be UTC")
    return parsed


def _snapshot(value, label: str) -> dict:
    snap = _object(value, label)
    if snap.get("scope") != "account_wide" or snap.get("independent") is not True:
        raise RecoveryError(f"{label} must assert independent account-wide scope")
    if not isinstance(snap.get("source_ref"), str) or not snap["source_ref"].strip():
        raise RecoveryError(f"{label}.source_ref is required")
    observed = _time(snap.get("observed_at"), f"{label}.observed_at")
    complete = _object(snap.get("complete"), f"{label}.complete")
    for field in ("open_orders", "fills", "held_positions"):
        if complete.get(field) is not True:
            raise RecoveryError(f"{label}.complete.{field} must be true")
    orders = _rows(snap.get("open_orders"), f"{label}.open_orders")
    fills = _rows(snap.get("fills"), f"{label}.fills")
    positions = _rows(snap.get("held_positions"), f"{label}.held_positions")
    seen_coid = {}
    seen_order_id = {}
    for field, rows in (("open_orders", orders), ("fills", fills)):
        for index, row in enumerate(rows):
            identity = _identity(row, f"{label}.{field}[{index}]")
            for seen, key in ((seen_coid, identity[0]), (seen_order_id, identity[1])):
                previous = seen.get(key)
                if previous is not None and previous != identity:
                    raise RecoveryError(f"{label} has contradictory order identity {key}")
                seen[key] = identity
    held = set()
    for index, row in enumerate(positions):
        item = _object(row, f"{label}.held_positions[{index}]")
        ticker, side = item.get("ticker"), item.get("side")
        if not isinstance(ticker, str) or not ticker.strip() or side not in ("yes", "no"):
            raise RecoveryError(f"{label}.held_positions[{index}] needs ticker and yes/no side")
        pair = (ticker, side)
        if pair in held:
            raise RecoveryError(f"{label} repeats held position {pair}")
        held.add(pair)
    return {"observed": observed, "orders": orders, "fills": fills, "held": held}


def _identity_dict(identity: tuple[str, str, str, str]) -> dict:
    return dict(zip(IDENTITY, identity))


def plan(snapshot: dict, ownership: dict, final_snapshot: dict | None = None) -> dict:
    """Validate stated offline evidence; never authorize a write or attest recovery."""
    current = _snapshot(snapshot, "snapshot")
    retained = _object(ownership, "ownership")
    if not isinstance(retained.get("source_ref"), str) or not retained["source_ref"].strip():
        raise RecoveryError("ownership.source_ref is required")
    owned = set()
    for index, row in enumerate(_rows(retained.get("owned_orders"), "ownership.owned_orders")):
        owned.add(_identity(row, f"ownership.owned_orders[{index}]"))
    observed = {_identity(row, "snapshot order/fill") for row in
                current["orders"] + current["fills"]}
    unproven = owned - observed
    if unproven:
        raise RecoveryError("retained ownership includes identities absent from independent orders/fills")
    cancellations = sorted(owned & {_identity(row, "open order") for row in current["orders"]})
    exclusions = sorted({_identity(row, "open order") for row in current["orders"]} - owned)
    owned_fill_pairs = {(row["ticker"], row["side"]) for row in current["fills"]
                        if _identity(row, "fill") in owned}
    reductions = sorted(current["held"] & owned_fill_pairs)
    unproven_positions = sorted(current["held"] - owned_fill_pairs)
    blockers = ["No live action is authorized by an offline plan",
                "Owned-only cancellation receipts and independent post-action reconciliation are absent",
                "Held exposure requires attended size/price and attribution review before any reduction",
                "Restored ownership ledger, original store preservation, stop/latch and human alarm disposition remain unverified"]
    if exclusions:
        blockers.append("Open orders without exact retained ownership are excluded; provenance remains unresolved")
    if unproven_positions:
        blockers.append("Held positions without matching owned-fill evidence have unresolved ownership")
    final_evidence = None
    if final_snapshot is not None:
        final = _snapshot(final_snapshot, "final_snapshot")
        if final["observed"] <= current["observed"]:
            raise RecoveryError("final_snapshot must be later than snapshot")
        final_evidence = {"later_complete_flat_snapshot": not final["orders"] and not final["held"],
                          "observed_at": final_snapshot["observed_at"],
                          "source_ref": final_snapshot["source_ref"],
                          "meaning": "snapshot evidence only; no recovery or human acknowledgment attested"}
        if final["orders"] or final["held"]:
            blockers.append("Final account snapshot still shows open orders or held positions")
    else:
        blockers.append("Later complete independent flat account snapshot absent")
    return {
        "status": "OFFLINE_PLAN_VALIDATED_REAL_RECOVERY_BLOCKED",
        "snapshot_observed_at": snapshot["observed_at"],
        "owned_only_cancellation_identities": [_identity_dict(row) for row in cancellations],
        "held_position_reduction_obligations": [dict(ticker=ticker, side=side,
                                                      size="UNKNOWN_REQUIRES_FRESH_ACCOUNT_TRUTH")
                                                for ticker, side in reductions],
        "excluded_unproven_open_orders": [_identity_dict(row) for row in exclusions],
        "unproven_held_positions": [dict(ticker=ticker, side=side) for ticker, side in unproven_positions],
        "final_snapshot_evidence": final_evidence,
        "blockers": blockers,
        "complete_clean_exit_criteria": [
            "writer stopped, restart prevented, original store/WAL/SHM/journal/latch/errors preserved",
            "real ownership ledger restored and reconciled to complete independent account truth",
            "owned-only cancellation and attended reduction receipts reconciled; foreign orders left untouched",
            "later complete independent account-wide flat orders and positions verified",
            "human alarm disposition and separately authorized restart decision recorded",
        ],
        "restriction": "No order placement, cancellation, reduction size/price, acknowledgment, flatness attestation or restart authorization",
    }


def _read_json(path: Path) -> dict:
    if not path.is_absolute():
        raise RecoveryError("all paths must be absolute")
    with path.open("r", encoding="utf-8") as handle:
        return json.load(handle)


def main(argv=None) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--db", required=True, type=Path)
    parser.add_argument("--journal", required=True, type=Path)
    parser.add_argument("--latch", required=True, type=Path)
    parser.add_argument("--snapshot", type=Path, help="offline independent account snapshot JSON")
    parser.add_argument("--ownership", type=Path, help="offline retained ownership JSON")
    parser.add_argument("--final-snapshot", type=Path, help="optional later account snapshot JSON")
    args = parser.parse_args(argv)
    try:
        if bool(args.snapshot) != bool(args.ownership) or (args.final_snapshot and not args.snapshot):
            raise RecoveryError("--snapshot and --ownership must be supplied together")
        result = inspect(args.db, args.journal, args.latch)
        if args.snapshot:
            result["plan"] = plan(_read_json(args.snapshot), _read_json(args.ownership),
                                  _read_json(args.final_snapshot) if args.final_snapshot else None)
        print(json.dumps(result, sort_keys=True))
        return 0
    except (RecoveryError, OSError, ValueError) as exc:
        print(f"ops_recovery: {exc}", file=sys.stderr)
        return 2


if __name__ == "__main__":
    raise SystemExit(main())
