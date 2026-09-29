#!/usr/bin/env python3
"""Prepare fresh attended-stage evidence and print operator-only commands.

This helper performs only public GETs through ``incentives`` and authenticated
GETs through ``accountcheck``. It never starts the harness, creates live_ok,
clears a latch, provisions a store, or sends an order.
"""
from __future__ import annotations

import argparse
import datetime as dt
from decimal import Decimal, InvalidOperation, ROUND_CEILING
import hashlib
import json
import os
from pathlib import Path
import re
import shlex
import subprocess
import sys
from typing import Any, Callable


ROOT = Path(__file__).resolve().parents[1]
GO_DIR = ROOT / "go"
TEMPLATE = ROOT / "notes/live-continuation-evidence-2026-09-26/q01-v3/config.json"
TICKER_RE = re.compile(r"^[A-Za-z0-9][A-Za-z0-9._-]*$")
R2_FIELDS = ("orders", "fills", "fees", "reduction", "clean_exit", "restart", "alarm")
RUNTIME_FILES = {"db": "harness.db", "anomaly_log": "anomaly.jsonl",
                 "latch": "harness.halt", "lock": "harness.lock",
                 "live_ok": "live_ok", "stop": "harness.stop"}
# H-PING-1 (harness-spec.md:1821): hourly SEV3 heartbeat; the dead-man monitor is 3600s/1200s grace.
FIRST_STAGE_HEARTBEAT_S = 3600
# H-SEL-6 mid 10c-90c and H-SEL-7 yes_bid + no_bid <= 99 (harness-spec.md:1402-1403),
# mirroring go/cmd/harness/turnover_selection.go:183-188.
SEL_MID_MIN_CENTS, SEL_MID_MAX_CENTS, SEL_BID_SUM_MAX_CENTS = Decimal(10), Decimal(90), Decimal(99)


def parse_time(value: str) -> dt.datetime:
    parsed = dt.datetime.fromisoformat(value.replace("Z", "+00:00"))
    if parsed.tzinfo is None:
        raise ValueError("timestamp has no timezone")
    return parsed.astimezone(dt.timezone.utc)


def require_fresh_time(value: str, now: dt.datetime, max_age_s: int,
                       label: str) -> dt.datetime:
    """Require an observation inside the preflight window, allowing 30s clock skew."""
    at = parse_time(value)
    if at < now - dt.timedelta(seconds=max_age_s) or at > now + dt.timedelta(seconds=30):
        raise ValueError(f"{label} is stale or in the future")
    return at


def sha256(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as stream:
        for chunk in iter(lambda: stream.read(1024 * 1024), b""):
            digest.update(chunk)
    return digest.hexdigest()


def source_manifest(repo: Path) -> tuple[list[dict[str, str]], str, str, str]:
    status = subprocess.run(
        ["git", "status", "--short"], cwd=repo, check=True,
        capture_output=True, text=True,
    ).stdout
    head = subprocess.run(
        ["git", "rev-parse", "HEAD"], cwd=repo, check=True,
        capture_output=True, text=True,
    ).stdout.strip()
    names = subprocess.run(
        ["git", "ls-files", "--cached", "--others", "--exclude-standard", "-z"],
        cwd=repo, check=True, capture_output=True,
    ).stdout.split(b"\0")
    go_files = sorted({n.decode() for n in names if n and n.decode().startswith("go/")
                       and (n.decode().endswith(".go")
                            or n.decode() in {"go/go.mod", "go/go.sum"})})
    rows = [{"path": name, "sha256": sha256(repo / name)} for name in go_files]
    encoded = "".join(f"{row['sha256']}  {row['path']}\n" for row in rows).encode()
    return rows, hashlib.sha256(encoded).hexdigest(), head, status


def run_json(command: list[str], cwd: Path, output: Path | None = None,
             runner: Callable[..., Any] = subprocess.run) -> dict[str, Any]:
    result = runner(command, cwd=cwd, check=True, capture_output=True, text=True)
    raw = result.stdout
    if output is not None:
        output.write_text(raw, encoding="utf-8")
    try:
        value = json.loads(raw)
    except json.JSONDecodeError as exc:
        raise ValueError(f"{command[0]} returned invalid JSON") from exc
    if not isinstance(value, dict):
        raise ValueError(f"{command[0]} returned a non-object JSON report")
    return value


def _complete(report: dict[str, Any], field: str) -> bool:
    value = report.get(field)
    return isinstance(value, dict) and value.get("outcome") == "complete"


def validate_account(report: dict[str, Any], ticker: str, now: dt.datetime,
                     max_age_s: int) -> None:
    if report.get("selected_ticker") != ticker:
        raise ValueError("accountcheck ticker does not match selected candidate")
    if report.get("account_scope_complete") is not True or report.get("flat") is not True:
        raise ValueError("accountcheck must prove complete account-wide flatness")
    for field in ("aggregate_balance_status", "subaccount_balances_status",
                  "selected_balance_status", "market_funding_status",
                  "resting_orders_status", "fills_status"):
        if not _complete(report, field):
            raise ValueError(f"accountcheck {field} is not complete")
    market_status = report.get("market_status")
    if not isinstance(market_status, dict) or market_status.get("outcome") != "complete":
        raise ValueError("accountcheck market identity is not complete")
    try:
        balance = Decimal(report["selected_balance"])
        funding = int(report["market_funding_available_micro"])
        funding_observed = parse_time(report["market_funding_observed_at"])
    except (KeyError, TypeError, ValueError, InvalidOperation) as exc:
        raise ValueError("selected balance or market funding is missing/malformed") from exc
    if balance <= 0 or funding <= 0:
        raise ValueError("selected-shard spendable balance and funding must be positive")
    if ((now - funding_observed).total_seconds() > max_age_s
            or funding_observed > now + dt.timedelta(seconds=30)):
        raise ValueError("selected-shard funding observation is stale")
    positions = report.get("positions_by_subaccount")
    if not isinstance(positions, dict) or not positions or any(
            not isinstance(v, dict) or v.get("outcome") != "complete"
            for v in positions.values()):
        raise ValueError("accountcheck positions are incomplete")
    decoder = report.get("production_decoder")
    if not isinstance(decoder, dict) or any(
            not isinstance(decoder.get(key), dict)
            or decoder[key].get("outcome") != "complete"
            for key in ("orders", "primary_positions")):
        raise ValueError("production account decoders are incomplete")
    started = require_fresh_time(report["started_at"], now, max_age_s, "accountcheck start")
    ended = require_fresh_time(report["ended_at"], now, max_age_s, "accountcheck end")
    if ended < started:
        raise ValueError("accountcheck report is stale or has invalid time bounds")
    requests = report.get("requests")
    if not isinstance(requests, list) or not requests:
        raise ValueError("accountcheck has no request evidence")
    for request in requests:
        if not isinstance(request, dict) or request.get("status") != 200:
            raise ValueError("accountcheck contains a failed request")
        at = require_fresh_time(request["at"], now, max_age_s, "accountcheck request")
        if at < started or at > ended:
            raise ValueError("accountcheck request timestamp falls outside report bounds")


def validate_program_list(report: dict[str, Any], ticker: str, now: dt.datetime,
                          max_age_s: int) -> None:
    if report.get("complete") is not True or not report.get("pages"):
        raise ValueError("public incentive walk is incomplete")
    started = require_fresh_time(report["started_at_utc"], now, max_age_s, "public incentive start")
    completed = require_fresh_time(report["completed_at_utc"], now, max_age_s, "public incentive end")
    if completed < started:
        raise ValueError("public incentive walk is stale")
    programs = report.get("programs")
    if not isinstance(programs, list) or not any(
            isinstance(row, dict) and row.get("market_ticker") == ticker
            and row.get("incentive_type") == "liquidity"
            and parse_time(row["start_date"]) <= now < parse_time(row["end_date"])
            for row in programs):
        raise ValueError("candidate has no currently active liquidity program")


def validate_program_detail(report: dict[str, Any], ticker: str, now: dt.datetime,
                            max_age_s: int) -> None:
    validate_program_list(report, ticker, now, max_age_s)
    selected = report.get("selected")
    if not isinstance(selected, dict) or selected.get("ticker") != ticker:
        raise ValueError("candidate details do not match selected ticker")
    market = selected.get("market")
    if not isinstance(market, dict) or market.get("status") != "active":
        raise ValueError("selected market is not active")
    series = selected.get("series")
    if not isinstance(series, dict):
        raise ValueError("selected series metadata is missing")


def check_book_prices(ob: dict[str, Any]) -> None:
    """H-SEL-6/H-SEL-7 on each side's best (highest) bid, in exact Decimal cents.

    Captured orderbook_fp levels are ascending by price, but the best bid is taken
    as the max level so list order is never assumed.
    """
    best = {}
    for side in ("yes_dollars", "no_dollars"):
        levels = ob.get(side)
        if not isinstance(levels, list) or not levels:
            raise ValueError(f"orderbook {side} depth is empty")
        best[side] = (max(Decimal(str(level[0])) for level in levels) * 100).to_integral_value()
    by, bn = best["yes_dollars"], best["no_dollars"]
    reasons = []
    if by + bn > SEL_BID_SUM_MAX_CENTS:
        reasons.append(f"H-SEL-7 bid_sum {by + bn}c > {SEL_BID_SUM_MAX_CENTS}c")
    mid = (by + 100 - bn) / 2
    if mid < SEL_MID_MIN_CENTS or mid > SEL_MID_MAX_CENTS:
        reasons.append(f"H-SEL-6 mid {mid}c outside {SEL_MID_MIN_CENTS}c-{SEL_MID_MAX_CENTS}c")
    if reasons:
        raise ValueError(f"book fails mandatory selection filter: {'; '.join(reasons)} "
                         f"(best yes_bid {by}c, best no_bid {bn}c)")


def validate_book(book: dict[str, Any], ticker: str, now: dt.datetime,
                  max_age_s: int, size: float) -> None:
    """Validate the collector's GET routes and market -> event -> series chain."""
    if book.get("ticker") != ticker or book.get("read_only") is not True:
        raise ValueError("book capture ticker does not match selected candidate")
    calls = book.get("calls")
    if not isinstance(calls, dict):
        raise ValueError("book capture has no calls")
    for key in ("market", "orderbook", "event", "series"):
        item = calls.get(key)
        if not isinstance(item, dict) or item.get("status") != 200:
            raise ValueError(f"book capture {key} read is incomplete")
        require_fresh_time(item["at_utc"], now, max_age_s, f"book capture {key} read")
    market_body = calls["market"].get("body")
    market = market_body.get("market") if isinstance(market_body, dict) else None
    if not isinstance(market, dict):
        raise ValueError("book market body is missing")
    if market.get("ticker") != ticker or market.get("status") != "active":
        raise ValueError("book market identity/status is invalid")
    if market.get("price_level_structure") != "linear_cent":
        raise ValueError("market price structure is incompatible with cent orders")
    event_ticker = market.get("event_ticker")
    event_body = calls["event"].get("body")
    event = event_body.get("event") if isinstance(event_body, dict) else None
    series_ticker = event.get("series_ticker") if isinstance(event, dict) else None
    if (not isinstance(event_ticker, str) or not TICKER_RE.fullmatch(event_ticker)
            or not isinstance(series_ticker, str) or not TICKER_RE.fullmatch(series_ticker)
            or event.get("event_ticker") != event_ticker):
        raise ValueError("book market/event/series identity chain is invalid")
    expected_paths = {"market": f"/markets/{ticker}",
                      "orderbook": f"/markets/{ticker}/orderbook",
                      "event": f"/events/{event_ticker}",
                      "series": f"/series/{series_ticker}"}
    for key, expected in expected_paths.items():
        if calls[key].get("path") != expected:
            raise ValueError(f"book {key} request path does not match candidate identity")
    orderbook_body = calls["orderbook"].get("body")
    ob = orderbook_body.get("orderbook_fp") if isinstance(orderbook_body, dict) else None
    if not isinstance(ob, dict):
        raise ValueError("orderbook_fp is missing")
    for side in ("yes_dollars", "no_dollars"):
        levels = ob.get(side)
        if not isinstance(levels, list) or not levels:
            raise ValueError(f"orderbook {side} depth is empty")
        depth = Decimal("0")
        for level in levels:
            if not isinstance(level, list) or len(level) != 2:
                raise ValueError(f"orderbook {side} has a malformed level")
            try:
                price, quantity = Decimal(str(level[0])), Decimal(str(level[1]))
            except InvalidOperation as exc:
                raise ValueError(f"orderbook {side} has a malformed price/quantity") from exc
            if not price.is_finite() or price <= 0 or price >= 1 or (price * 100) != (price * 100).to_integral_value():
                raise ValueError(f"orderbook {side} contains a non-cent price")
            if not quantity.is_finite() or quantity <= 0:
                raise ValueError(f"orderbook {side} contains non-positive depth")
            depth += quantity
        if depth < Decimal(str(size)):
            raise ValueError(f"orderbook {side} depth is below selected S")
    check_book_prices(ob)
    series_body = calls["series"].get("body")
    series = series_body.get("series") if isinstance(series_body, dict) else None
    if (not isinstance(series, dict) or series.get("ticker") != series_ticker
            or series.get("fee_type") != "quadratic"):
        raise ValueError("series fee metadata is missing or unsupported")
    completed = require_fresh_time(book["completed_at_utc"], now, max_age_s,
                                   "book capture completion")
    if any(parse_time(calls[key]["at_utc"]) > completed for key in expected_paths):
        raise ValueError("book request falls after capture completion")
    multiplier = series.get("fee_multiplier")
    if (type(multiplier) not in (int, float)
            or not Decimal(str(multiplier)).is_finite() or multiplier < 0):
        raise ValueError("series fee multiplier is missing")


def exit_arithmetic(book: dict[str, Any], size: float) -> dict[str, Any]:
    """Illustrative taker exits at captured depth, never an execution claim."""
    calls = book["calls"]
    ob = calls["orderbook"]["body"]["orderbook_fp"]
    series = calls["series"]["body"]["series"]
    multiplier = Decimal(str(series["fee_multiplier"]))
    exits = {}
    for side in ("yes", "no"):
        remaining = Decimal(str(size))
        gross, fee = Decimal(0), Decimal(0)
        used = []
        for price, depth in sorted(
                ((Decimal(str(p)), Decimal(str(q))) for p, q in ob[side + "_dollars"]),
                reverse=True):
            quantity = min(remaining, depth)
            if quantity <= 0:
                break
            cost = quantity * price
            raw_fee = Decimal("0.07") * multiplier * quantity * price * (1 - price)
            # The official schedule rounds fee + position cost to centicents.
            rounded_fee = (cost + raw_fee).quantize(Decimal("0.0001"), rounding=ROUND_CEILING) - cost
            gross += cost
            fee += rounded_fee
            remaining -= quantity
            used.append({"price_usd": str(price), "contracts": str(quantity),
                         "estimated_fee_usd": str(rounded_fee)})
        if remaining > 0:
            raise ValueError("captured exit depth cannot cover S")
        exits[side] = {"levels": used, "gross_usd": str(gross),
                       "estimated_taker_fee_usd": str(fee), "net_proceeds_usd": str(gross - fee)}
    return {"size_contracts": str(size), "series_fee_type": series["fee_type"],
            "series_fee_multiplier": str(multiplier), "exits": exits,
            "fee_schedule": "https://kalshi.com/docs/kalshi-fee-schedule.pdf",
            "formula": "0.07 * series_multiplier * contracts * price * (1-price); round fee+cost upward to $0.0001",
            "limitations": "Hypothetical taker exits at unchanged captured bids, rounded per displayed level. Actual matching, fill splits, fees and depth can differ. Excludes entry cost and rewards; not trading profit or a maker execution promise."}


def validate_build_receipt(path: Path, source_hash: str, binary: Path,
                           binary_hash: str) -> None:
    try:
        receipt = json.loads(path.read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError) as exc:
        raise ValueError("a current candidate build-identity receipt is required") from exc
    if (receipt.get("source_manifest_sha256") != source_hash
            or Path(receipt.get("binary_path", "")).resolve() != binary
            or receipt.get("binary_sha256") != binary_hash):
        raise ValueError("candidate build receipt does not match current source and binary")


def load_prior_stage(prior_config: Path, ticker: str, source_hash: str,
                     binary: Path, binary_hash: str) -> tuple[dict[str, Any], dict[str, Any]]:
    """Bind R2 to one first-stage identity and its dedicated, non-symlink runtime."""
    prior_path = prior_config.absolute()
    stage_dir = prior_path.parent
    if (prior_path.name != "config.json" or stage_dir.resolve() != stage_dir
            or prior_path.is_symlink()
            or stage_dir.is_symlink() or (stage_dir / "runtime").is_symlink()
            or (stage_dir / "evidence").is_symlink()):
        raise ValueError("prior config must belong to a dedicated first-stage directory")
    identity_path = stage_dir / "evidence" / "identity.json"
    if identity_path.is_symlink():
        raise ValueError("first-stage identity must not be a symlink")
    try:
        config = json.loads(prior_path.read_text(encoding="utf-8"))
        identity = json.loads(identity_path.read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError) as exc:
        raise ValueError("R2 requires a first-stage config and identity.json") from exc
    if not isinstance(config, dict) or not isinstance(identity, dict):
        raise ValueError("first-stage config and identity must be objects")
    config_hash = sha256(prior_path)
    if (identity.get("stage") != "first" or identity.get("rung") != "sizing"
            or identity.get("ticker") != ticker or config.get("ticker") != ticker
            or config.get("rung") != "sizing"
            or identity.get("config_path") != str(prior_path)
            or identity.get("config_sha256") != config_hash
            or identity.get("source_manifest_sha256") != source_hash
            or identity.get("binary_path") != str(binary)
            or identity.get("binary_sha256") != binary_hash):
        raise ValueError("prior config does not match first-stage candidate identity")
    try:
        parse_time(identity["created_at_utc"])
    except (KeyError, TypeError, ValueError) as exc:
        raise ValueError("first-stage identity has no valid creation timestamp") from exc
    paths = config.get("paths")
    runtime = stage_dir / "runtime"
    if not isinstance(paths, dict) or not runtime.is_dir():
        raise ValueError("prior config has no dedicated runtime paths")
    for key, filename in RUNTIME_FILES.items():
        target = runtime / filename
        supplied = paths.get(key)
        if (not isinstance(supplied, str) or supplied != str(target)
                or target.is_symlink() or target.parent.resolve() != runtime.resolve()):
            raise ValueError(f"prior config {key} path is not the first-stage runtime path")
    identity["identity_sha256"] = sha256(identity_path)
    return config, identity


def load_r2_receipt(path: Path, ticker: str, source_hash: str, binary_hash: str,
                    prior_config_hash: str, first_identity: dict[str, Any],
                    now: dt.datetime) -> dict[str, Any]:
    """Check operator attestation shape and file identity, not event truth in files."""
    try:
        receipt = json.loads(path.read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError) as exc:
        raise ValueError("R2 requires a readable operator evidence receipt") from exc
    if not isinstance(receipt, dict):
        raise ValueError("R2 operator evidence receipt must be an object")
    identity = receipt.get("candidate", {})
    if (not isinstance(identity, dict) or identity.get("ticker") != ticker
            or identity.get("source_manifest_sha256") != source_hash
            or identity.get("binary_sha256") != binary_hash
            or identity.get("config_sha256") != prior_config_hash
            or identity.get("first_stage_identity_sha256") != first_identity["identity_sha256"]):
        raise ValueError("R2 receipt is not bound to the first-stage candidate identity")
    first_created = parse_time(first_identity["created_at_utc"])
    acceptance = receipt.get("acceptance", {})
    if not isinstance(acceptance, dict):
        raise ValueError("R2 receipt acceptance must be an object")
    missing = []
    for field in R2_FIELDS:
        entry = acceptance.get(field)
        if (not isinstance(entry, dict)
                or entry.get("operator_outcome") not in ("passed", "observed")
                or not isinstance(entry.get("operator"), str) or not entry["operator"].strip()
                or not isinstance(entry.get("observation"), str)
                or not entry["observation"].strip()):
            missing.append(field)
            continue
        try:
            at = parse_time(entry["recorded_at_utc"])
            if at < first_created or at > now + dt.timedelta(seconds=30):
                raise ValueError("outside first-stage acceptance interval")
        except (KeyError, TypeError, ValueError):
            missing.append(field)
            continue
        evidence = entry.get("evidence")
        if not isinstance(evidence, list) or not evidence:
            missing.append(field)
            continue
        for item in evidence:
            if (not isinstance(item, dict) or not isinstance(item.get("path"), str)
                    or not isinstance(item.get("sha256"), str)
                    or item.get("first_stage_identity_sha256") != first_identity["identity_sha256"]):
                missing.append(field)
                break
            if Path(item["path"]).is_absolute():
                missing.append(field)
                break
            evidence_path = path.parent / item["path"]
            try:
                evidence_path.resolve().relative_to(path.parent.resolve())
                if (evidence_path.is_symlink() or not evidence_path.is_file()
                        or sha256(evidence_path) != item["sha256"]):
                    missing.append(field)
                    break
            except (OSError, ValueError):
                missing.append(field)
                break
    if missing:
        raise ValueError("R2 receipt acceptance incomplete: " + ", ".join(missing))
    receipt["artifact_content_verified"] = False
    receipt["artifact_content_note"] = ("Operator observations are attestations; file hashes "
                                        "verify referenced bytes, not trades, fees or alarm delivery.")
    return receipt


def quote(value: str | Path) -> str:
    return shlex.quote(str(value))


PREFLIGHT_TOOLS = ("incentives", "accountcheck")


def prepare(args: argparse.Namespace, *, runner: Callable[..., Any] = subprocess.run,
            clock: Callable[[], dt.datetime] = lambda: dt.datetime.now(dt.timezone.utc),
            timer: Callable[[], dt.datetime] = lambda: dt.datetime.now(dt.timezone.utc)) -> Path:
    """``clock`` drives freshness validation; ``timer`` only stamps prep-timing.json."""
    repo = Path(args.repo).resolve()
    ticker = args.ticker
    if not TICKER_RE.fullmatch(ticker):
        raise ValueError("invalid market ticker")
    size = float(args.size)
    if not (0 < size <= 12):
        raise ValueError("size must be positive and no greater than 12")
    binary = Path(args.binary).resolve()
    if not binary.is_file() or not os.access(binary, os.X_OK):
        raise ValueError("reviewed executable binary is required")
    if not args.candidate_receipt:
        raise ValueError("coordinator-supplied candidate build identity receipt is required")
    if args.stage == "r2":
        if not args.prior_config or not Path(args.prior_config).is_file():
            raise ValueError("R2 requires --prior-config from the completed first stage")
        if not args.r2_receipt or not Path(args.r2_receipt).is_file():
            raise ValueError("R2 commands are withheld until an operator evidence receipt exists")
    if args.max_age_seconds <= 0:
        raise ValueError("max-age-seconds must be positive")
    # --book keeps the pre-lip-e2t path: an operator capture taken before prepare().
    # Without it, prepare() captures the book itself after the tool build (below).
    book_path = Path(args.book).resolve() if args.book else None
    book = json.loads(book_path.read_text(encoding="utf-8")) if book_path else None
    max_age = args.max_age_seconds
    root = Path(args.evidence_root).resolve()
    stamp = clock().astimezone(dt.timezone.utc).strftime("%Y%m%dT%H%M%S.%fZ")
    stage_dir = root / f"{args.stage}-{ticker}-{stamp}"
    if stage_dir.exists():
        raise ValueError(f"evidence directory already exists: {stage_dir}")
    stage_dir.mkdir(parents=True, mode=0o700)
    runtime = stage_dir / "runtime"
    evidence = stage_dir / "evidence"
    runtime.mkdir(mode=0o700)
    evidence.mkdir(mode=0o700)

    source_rows, source_hash, head, git_status = source_manifest(repo)
    binary_hash = sha256(binary)
    validate_build_receipt(Path(args.candidate_receipt).resolve(), source_hash, binary, binary_hash)
    prior_identity = None
    r2_receipt = None

    if args.stage == "first":
        config = json.loads(TEMPLATE.read_text(encoding="utf-8"))
        config["rung"] = "sizing"
        config["heartbeat_s"] = FIRST_STAGE_HEARTBEAT_S
        config["paths"] = dict(config["paths"])
        for key, filename in RUNTIME_FILES.items():
            config["paths"][key] = str(runtime / filename)
    else:
        prior_path = Path(args.prior_config).absolute()
        config, prior_identity = load_prior_stage(prior_path, ticker, source_hash,
                                                   binary, binary_hash)
        config["paths"] = dict(config["paths"])
        r2_receipt = load_r2_receipt(Path(args.r2_receipt).absolute(), ticker,
                                     source_hash, binary_hash, sha256(prior_path),
                                     prior_identity, clock().astimezone(dt.timezone.utc))
    default_key = Path.home() / ".kalshi/kalshi.pem"
    default_env = Path.home() / ".kalshi/env"
    if (Path(config["paths"]["key"]).expanduser().resolve() != default_key.resolve()
            or Path(config["paths"]["env"]).expanduser().resolve() != default_env.resolve()):
        raise ValueError("accountcheck uses ~/.kalshi credentials; config must identify the same credentials")
    config["ticker"] = ticker
    config["s"] = size
    if args.stage == "r2":
        config["rung"] = "pilot"

    # lip-e2t: compile is not a read. Build every preflight tool before the first
    # timed read so compile time never sits inside the max-age freshness window.
    events: list[dict[str, str]] = []

    def mark(event: str) -> None:
        events.append({"event": event,
                       "at_utc": timer().astimezone(dt.timezone.utc).isoformat()})

    go_dir = GO_DIR if repo == ROOT else repo / "go"
    tools_dir = stage_dir / "tools"
    tools_dir.mkdir(mode=0o700)
    tools = {name: tools_dir / name for name in PREFLIGHT_TOOLS}
    mark("tool_build_start")
    for name, tool in tools.items():
        runner(["go", "build", "-trimpath", "-o", str(tool), f"./cmd/{name}"],
               cwd=go_dir, check=True, capture_output=True, text=True)
    mark("tool_build_end")
    tool_hashes = {name: sha256(tool) for name, tool in tools.items()}

    now = clock().astimezone(dt.timezone.utc)
    book_dest = evidence / "market-book-series.json"
    if book_path is None:
        mark("book_capture_start")
        runner([sys.executable, str(repo / "scripts" / "public_market_snapshot.py"),
                "--ticker", ticker, "--out", str(book_dest)],
               cwd=repo, check=True, capture_output=True, text=True)
        mark("book_capture_end")
        book = json.loads(book_dest.read_text(encoding="utf-8"))
    programs_all_path = evidence / "programs.json"
    programs_selected_path = evidence / "candidate.json"
    mark("programs_read_start")
    programs_all = run_json([str(tools["incentives"])], go_dir, programs_all_path, runner)
    mark("programs_read_end")
    mark("candidate_read_start")
    programs_selected = run_json([str(tools["incentives"]), "-ticker", ticker],
                                 go_dir, programs_selected_path, runner)
    mark("candidate_read_end")
    validate_program_list(programs_all, ticker, clock().astimezone(dt.timezone.utc), max_age)
    # The ticker-specific response must also report a complete, current selection.
    validate_program_detail(programs_selected, ticker, clock().astimezone(dt.timezone.utc), max_age)
    validate_book(book, ticker, clock().astimezone(dt.timezone.utc), max_age, size)
    if book_path is not None:
        book_dest.write_bytes(book_path.read_bytes())
    (evidence / "exit-arithmetic.json").write_text(json.dumps(exit_arithmetic(book, size), indent=2) + "\n")

    account_path = evidence / "account-preflight.json"
    account_cmd = [str(tools["accountcheck"]), "-out", str(account_path),
                   "-ticker", ticker, "-fills"]
    mark("account_read_start")
    runner(account_cmd, cwd=go_dir, check=True, capture_output=True, text=True)
    mark("account_read_end")
    account = json.loads(account_path.read_text(encoding="utf-8"))
    validate_account(account, ticker, clock().astimezone(dt.timezone.utc), max_age)
    # Mirror H-CAP-8's conservative start checks in exact decimal arithmetic.
    # The executable still derives and validates its cap again at startup.
    cash = Decimal(account["market_funding_available_micro"]) / Decimal(1_000_000)
    reserve = Decimal(str(config.get("capital_reserve", "0.25")))
    n_markets = int(config.get("n_markets", 1))
    if n_markets != 1 or config.get("capital_source") != "selected_shard_balance":
        raise ValueError("operator stage requires one market and selected-shard funding")
    deployable = cash * (1 - reserve)
    worst_entry = Decimal(str(size)) * Decimal("0.99")
    round_trip = Decimal(str(size))
    if not (0 <= reserve < 1) or worst_entry > deployable or round_trip > cash:
        raise ValueError("current selected-shard balance cannot fund S with reserve and exits")
    funding_math = {"cash_usd": str(cash), "reserve_usd": str(cash * reserve),
                    "deployable_usd": str(deployable), "worst_entry_usd": str(worst_entry),
                    "round_trip_bound_usd": str(round_trip),
                    "note": "H-CAP-8 admission bound; moved exit prices and fees can exceed it. Startup checks cash again."}
    (evidence / "funding-arithmetic.json").write_text(json.dumps(funding_math, indent=2) + "\n")

    _, source_hash_after, _, _ = source_manifest(repo)
    if source_hash_after != source_hash or sha256(binary) != binary_hash:
        raise ValueError("candidate source or binary changed during read-only preflight")
    # The account read is last. Recheck all four reports against one final clock
    # so a slow read cannot leave the public book/programs stale at handoff.
    final_now = clock().astimezone(dt.timezone.utc)
    validate_program_list(programs_all, ticker, final_now, max_age)
    validate_program_detail(programs_selected, ticker, final_now, max_age)
    validate_book(book, ticker, final_now, max_age, size)
    validate_account(account, ticker, final_now, max_age)

    config_path = stage_dir / "config.json"
    config_path.write_text(json.dumps(config, indent=2) + "\n", encoding="utf-8")
    config_hash = sha256(config_path)
    identity = {
        "created_at_utc": now.isoformat(), "stage": args.stage, "ticker": ticker,
        "rung": config["rung"], "size": size, "git_head": head,
        "git_status": git_status, "go_source_manifest": source_rows,
        "source_manifest_sha256": source_hash, "binary_path": str(binary),
        "binary_sha256": binary_hash, "config_path": str(config_path),
        "config_sha256": config_hash, "account_report": str(account_path),
        "program_report": str(programs_selected_path), "book_report": str(book_dest),
        "selected_exchange_index": account.get("exchange_index"),
        "selected_balance": account.get("selected_balance"),
        "market_funding_available_micro": account.get("market_funding_available_micro"),
        "tool_binaries": tool_hashes,
    }
    if args.stage == "r2":
        identity["first_stage_identity_sha256"] = prior_identity["identity_sha256"]
        identity["r2_attestation"] = {"receipt": str(Path(args.r2_receipt).absolute()),
                                      "artifact_content_verified": False,
                                      "artifact_content_note": r2_receipt["artifact_content_note"]}
    (evidence / "identity.json").write_text(json.dumps(identity, indent=2) + "\n", encoding="utf-8")
    (evidence / "source-status.txt").write_text(git_status, encoding="utf-8")
    (evidence / "source-manifest.json").write_text(json.dumps(source_rows, indent=2) + "\n", encoding="utf-8")
    # The oldest source-reported observation the final recheck binds; launch
    # before it ages past max_age.
    oldest = min([parse_time(book["calls"][key]["at_utc"]) for key in ("market", "orderbook", "event", "series")]
                 + [parse_time(programs_all["started_at_utc"]), parse_time(programs_selected["started_at_utc"]),
                    parse_time(account["started_at"]), parse_time(account["market_funding_observed_at"])])
    mark("prepare_end")
    timing = {"book_source": "prepare snapshot" if book_path is None else f"--book {book_path}",
              "book_completed_at_utc": book["completed_at_utc"], "max_age_seconds": max_age,
              "oldest_observation_at_utc": oldest.isoformat(),
              "launch_deadline_utc": (oldest + dt.timedelta(seconds=max_age)).isoformat(),
              "events": events}
    (evidence / "prep-timing.json").write_text(json.dumps(timing, indent=2) + "\n", encoding="utf-8")

    print_operator_phases(args.stage, operator_phases(
        args.stage, exe=quote(binary), cfg=quote(config_path), rung=config["rung"],
        live_ok=quote(config["paths"]["live_ok"]), evidence=evidence, repo=repo, ticker=ticker),
        evidence)
    return evidence


# Operator guidance is printed as prose phase headers, never as shell comments:
# zsh leaves interactivecomments off, so a pasted "# ..." runs as words. Prose
# avoids every shell metacharacter and starts with a word that is not a command
# (macOS paths are case-insensitive, so e.g. "Paste" would run /usr/bin/paste).
COMMAND_INDENT = "    "


def operator_phases(stage_name: str, *, exe: str, cfg: str, rung: str, live_ok: str,
                    evidence: Path, repo: Path, ticker: str) -> list[tuple[str, list[str], list[str]]]:
    """Return (title, prose checks, commands); a phase never spans a human checkpoint."""
    first = stage_name == "first"
    pid_file = quote(evidence / "harness.pid")
    log_file = quote(evidence / "harness.log")
    phases: list[tuple[str, list[str], list[str]]] = []
    if first:
        phases.append(("provision this stage store.", [], [f"{exe} -config {cfg} -provision"]))
    arm_checks = []
    if not first:
        arm_checks += [
            "CHECK: start only after a human adjudicates complete account truth and clears the existing latch. This helper never clears it.",
            "NOTE: R2 operator attestation accepted. Artifact contents and claimed events remain unverified by this helper.",
        ]
    arm_checks.append("CHECK: arm only after reviewing current account, book, fees, limits and candidate identity.")
    phases.append(("arm and start the harness.", arm_checks, [
        f"install -m 600 /dev/null {live_ok}",
        f"{exe} -config {cfg} -rung {rung} -live > {log_file} 2>&1 &",
        f"echo $! > {pid_file}",
    ]))
    if first:
        observe = ["CHECK: remain present. Observe fills and the durable first-fill stop before planned shutdown."]
    else:
        observe = ["CHECK: remain present and observe fills before planned shutdown.",
                   "NOTE: the pilot rung does not stop on first owned fill."]
    phases.append(("show the harness process before signaling it.", observe, [
        f"ps -p \"$(cat {pid_file})\" -o pid=,command=",
    ]))
    phases.append(("stop the harness and wait for it in the SAME operator shell that started it.", [
        "CHECK: the ps output shows the operator-verified harness child. Verify identity before signaling.",
        "NOTE: await clean drain. Unknown or nonflat truth keeps cancellation and reduction attended.",
    ], [
        f"kill -TERM \"$(cat {pid_file})\"",
        f"wait \"$(cat {pid_file})\"; echo $? > {quote(evidence / 'exit-status.txt')}",
    ]))
    phases.append(("record account truth after the drain.", [
        "CHECK: the wait above has returned.",
    ], [
        f"cd {quote(repo / 'go')}",
        f"go run ./cmd/accountcheck -ticker {quote(ticker)} -fills -out {quote(evidence / 'account-after-drain.json')}",
    ]))
    if first:
        restart_pid = quote(evidence / "restart.pid")
        phases.append(("attended retained-latch restart.", [
            "CHECK: the drain exit status and the account report after drain have been read.",
        ], [
            f"{exe} -config {cfg} -rung sizing -resume 'first-owned-fill stop validation' -live > {quote(evidence / 'restart.log')} 2>&1 &",
            f"echo $! > {restart_pid}",
        ]))
        phases.append(("show the restarted child before signaling it.", [
            "CHECK: observe retained-latch WINDING_DOWN/adoption and no new adds. A latched process can idle DRAINED until signaled.",
        ], [
            f"ps -p \"$(cat {restart_pid})\" -o pid=,command=",
        ]))
        phases.append(("stop the restarted child and wait for it in the SAME operator shell.", [
            "CHECK: the ps output shows the restarted child. Verify identity before signaling.",
        ], [
            f"kill -TERM \"$(cat {restart_pid})\"",
            f"wait \"$(cat {restart_pid})\"; echo $? > {quote(evidence / 'restart-exit-status.txt')}",
        ]))
        phases.append(("record account truth after the restart, still in the go directory.", [
            "CHECK: only after the attended retained-latch restart has exited cleanly.",
            "NOTE: R2 remains gated on the candidate-bound first-stage evidence receipt and human latch adjudication.",
        ], [
            f"go run ./cmd/accountcheck -ticker {quote(ticker)} -fills -out {quote(evidence / 'account-after-restart.json')}",
        ]))
    phases.append(("remove the write sentinel of this stage.", [
        "CHECK: only after verifying every stage process has exited and account truth is complete and flat.",
        "NOTE: this removes only the write sentinel of this stage. Retain the store, latch and evidence.",
    ], [
        f"rm -f {live_ok}",
    ]))
    return phases


def print_operator_phases(stage_name: str, phases: list[tuple[str, list[str], list[str]]],
                          evidence: Path) -> None:
    print(f"Prepared {stage_name} evidence: {evidence}")
    print(f"Candidate identity: {evidence / 'identity.json'}")
    print("Operator-only commands, not executed by this helper.")
    print("RULE: paste one phase at a time, only its indented command lines, and finish its CHECK lines first.")
    for number, (title, prose, commands) in enumerate(phases, 1):
        print()
        print(f"PHASE {number} of {len(phases)}: {title}")
        for line in prose:
            print(line)
        for command in commands:
            print(f"{COMMAND_INDENT}{command}")


def parser() -> argparse.ArgumentParser:
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument("--stage", choices=("first", "r2"), required=True)
    p.add_argument("--ticker", required=True, help="fresh operator-selected active ticker")
    p.add_argument("--size", required=True, type=float, help="balance-derived S, 0 < S <= 12")
    p.add_argument("--binary", required=True, help="reviewed executable candidate binary")
    p.add_argument("--candidate-receipt", required=True, help="coordinator build identity JSON for this source and binary")
    p.add_argument("--book", help="operator book capture (back-compat); omit so prepare captures "
                   "it after building the preflight tools")
    p.add_argument("--evidence-root", default=str(ROOT / "notes/active-continuation-2026-09-26/operator-stages"))
    p.add_argument("--max-age-seconds", type=int, default=60)
    p.add_argument("--prior-config", help="completed first-stage config; required for R2")
    p.add_argument("--r2-receipt", help="candidate-bound first-stage evidence receipt; required for R2")
    p.add_argument("--repo", default=str(ROOT), help=argparse.SUPPRESS)
    return p


def main(argv: list[str] | None = None) -> int:
    args = parser().parse_args(argv)
    try:
        prepare(args)
    except (OSError, ValueError, KeyError, TypeError, subprocess.CalledProcessError) as exc:
        print(f"operator_stage: {exc}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
