import collections
import contextlib
import datetime as dt
import hashlib
import importlib.util
import io
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import types
import unittest
from unittest import mock


MODULE_PATH = Path(__file__).with_name("operator_stage.py")
SPEC = importlib.util.spec_from_file_location("operator_stage", MODULE_PATH)
stage = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(stage)


UTC = dt.timezone.utc
NOW = dt.datetime(2026, 9, 27, 1, 0, 0, tzinfo=UTC)
TICKER = "KXTEST-26SEP"
# The freshly chosen market an R2 stage can run on, distinct from its first stage's.
OTHER_TICKER = "KXOTHER-26OCT"


def account_report(ticker=TICKER):
    stamp = NOW.isoformat().replace("+00:00", "Z")
    paths = ["/portfolio/balance", "/portfolio/subaccounts/balances",
             f"/markets/{ticker}", "/portfolio/orders", "/portfolio/positions",
             "/portfolio/fills"]
    return {
        "started_at": stamp, "ended_at": stamp,
        "requests": [{"at": stamp, "path": p, "status": 200} for p in paths],
        "aggregate_balance_status": {"outcome": "complete"},
        "subaccount_balances_status": {"outcome": "complete"},
        "selected_ticker": ticker,
        "market_status": {"outcome": "complete"},
        "selected_balance_status": {"outcome": "complete"},
        "market_funding_status": {"outcome": "complete"},
        "resting_orders_status": {"outcome": "complete"},
        "fills_status": {"outcome": "complete"},
        "positions_by_subaccount": {"0": {"outcome": "complete"}},
        "production_decoder": {"orders": {"outcome": "complete"},
                               "primary_positions": {"outcome": "complete"}},
        "account_scope_complete": True, "flat": True,
        "exchange_index": 0, "selected_balance": "10000",
        "market_funding_available_micro": "100000000",
        "market_funding_observed_at": NOW.isoformat(),
    }


def programs_report(selected=True, ticker=TICKER):
    stamp = NOW.isoformat().replace("+00:00", "Z")
    out = {
        "complete": True, "pages": 1, "started_at_utc": stamp,
        "completed_at_utc": stamp,
        "programs": [{"market_ticker": ticker, "incentive_type": "liquidity",
                      "start_date": "2026-09-26T00:00:00Z",
                      "end_date": "2026-09-28T00:00:00Z"}],
    }
    if selected:
        out["selected"] = {
            "ticker": ticker,
            "market": {"ticker": ticker, "status": "active"},
            "event": {"event_ticker": "KXTEST"},
            "series": {"ticker": "KXTEST"},
        }
    return out


def book_report(ticker=TICKER):
    stamp = NOW.isoformat()
    return {"ticker": ticker, "read_only": True, "completed_at_utc": stamp, "calls": {
        "market": {"path": f"/markets/{ticker}", "at_utc": stamp, "status": 200, "body": {"market": {
            "ticker": ticker, "status": "active", "event_ticker": "KXTEST",
            "price_level_structure": "linear_cent"}}},
        "orderbook": {"path": f"/markets/{ticker}/orderbook", "at_utc": stamp, "status": 200, "body": {"orderbook_fp": {
            "yes_dollars": [["0.43", "5"], ["0.44", "20"]],
            "no_dollars": [["0.53", "5"], ["0.54", "20"]]}}},
        "event": {"path": "/events/KXTEST", "at_utc": stamp, "status": 200,
                  "body": {"event": {"event_ticker": "KXTEST", "series_ticker": "KXTEST"}}},
        "series": {"path": "/series/KXTEST", "at_utc": stamp, "status": 200, "body": {"series": {
            "ticker": "KXTEST", "fee_type": "quadratic", "fee_multiplier": 1}}},
    }}


# Characters that make a pasted prose line do more than "command not found".
FORBIDDEN_PROSE = set("#'\"$`;&|<>()") | set("!*?[]")
# Prose must not start with a word that resolves as a command on a case-insensitive PATH.
PROSE_LEADS = ("Prepared ", "Candidate identity: ", "Operator-only commands,", "RULE: ",
               "PHASE ", "CHECK: ", "NOTE: ")


def legacy_operator_lines(args, config, *, exe, cfg, evidence, repo, ticker):
    """The pre-lip-8q0 first-stage operator block: its print statements copied verbatim.

    R2 has no legacy block: its flow changed to a fresh store and a crash drill, and
    r2_operator_commands spells that one out.
    """
    assert args.stage == "first", "R2 has no legacy operator block"
    quote = stage.quote
    out = []
    print = out.append
    pid_file = quote(evidence / "harness.pid")
    log_file = evidence / "harness.log"
    print(f"Prepared {args.stage} evidence: {evidence}")
    print(f"Candidate identity: {evidence / 'identity.json'}")
    print("Operator-only commands (not executed):")
    if args.stage == "first":
        print(f"  {exe} -config {cfg} -provision")
    print("  # OPERATOR: arm only after reviewing current account, book, fees, limits and candidate identity.")
    print(f"  install -m 600 /dev/null {quote(config['paths']['live_ok'])}")
    print(f"  {exe} -config {cfg} -rung {config['rung']} -live > {quote(log_file)} 2>&1 &")
    print(f"  echo $! > {pid_file}")
    print("  # Remain present; observe fills and the durable first-fill stop before planned shutdown.")
    print(f"  ps -p \"$(cat {pid_file})\" -o pid=,command=  # verify identity before signaling")
    print(f"  kill -TERM \"$(cat {pid_file})\"  # operator-verified harness child PID")
    print(f"  # Await clean drain; unknown/nonflat truth keeps cancellation and reduction attended.")
    print(f"  wait \"$(cat {pid_file})\"; echo $? > {quote(evidence / 'exit-status.txt')}  # same operator shell")
    print(f"  cd {quote(repo / 'go')}")
    print(f"  go run ./cmd/accountcheck -ticker {quote(ticker)} -fills -out {quote(evidence / 'account-after-drain.json')}")
    if args.stage == "first":
        restart_pid = quote(evidence / "restart.pid")
        print(f"  {exe} -config {cfg} -rung sizing -resume 'first-owned-fill stop validation' -live > {quote(evidence / 'restart.log')} 2>&1 &")
        print(f"  echo $! > {restart_pid}")
        print("  # Observe retained-latch WINDING_DOWN/adoption and no new adds. A latched process can idle DRAINED until signaled.")
        print(f"  ps -p \"$(cat {restart_pid})\" -o pid=,command=  # verify the restarted child")
        print(f"  kill -TERM \"$(cat {restart_pid})\"")
        print(f"  wait \"$(cat {restart_pid})\"; echo $? > {quote(evidence / 'restart-exit-status.txt')}")
        print("  # Only after the attended retained-latch restart has exited cleanly:")
        print(f"  go run ./cmd/accountcheck -ticker {quote(ticker)} -fills -out {quote(evidence / 'account-after-restart.json')}")
        print("  # R2 remains gated on the candidate-bound first-stage evidence receipt and human latch adjudication.")
    print("  # After verifying every stage process has exited and account truth is complete and flat:")
    print(f"  rm -f {quote(config['paths']['live_ok'])}  # only this stage's write sentinel; retain store, latch and evidence")
    return out


def legacy_commands(lines):
    """Old indented lines minus standalone comments and inline '  # ...' suffixes."""
    return [line[2:].split("  # ", 1)[0] for line in lines
            if line.startswith("  ") and not line[2:].startswith("#")]


def r2_operator_commands(config, *, exe, cfg, evidence, repo, ticker):
    """The R2 commands in paste order, spelled out apart from operator_stage.

    Provision its own store, cycle, SIGKILL the harness, read the account with the prebuilt
    tool, restart WITHOUT the resume flag, SIGTERM the restarted child, read the account
    again and drop this stage's write sentinel.
    """
    quote = stage.quote
    pid_file, restart_pid = quote(evidence / "harness.pid"), quote(evidence / "restart.pid")
    live_ok = quote(config["paths"]["live_ok"])
    accountcheck = quote(evidence.parent / "tools" / "accountcheck")
    return [
        f"{exe} -config {cfg} -provision",
        f"install -m 600 /dev/null {live_ok}",
        f"{exe} -config {cfg} -rung pilot -live > {quote(evidence / 'harness.log')} 2>&1 &",
        f"echo $! > {pid_file}",
        f"ps -p \"$(cat {pid_file})\" -o pid=,command=",
        f"kill -KILL \"$(cat {pid_file})\"",
        f"wait \"$(cat {pid_file})\"; echo $? > {quote(evidence / 'exit-status.txt')}",
        f"cd {quote(repo / 'go')}",
        f"{accountcheck} -ticker {quote(ticker)} -fills -out {quote(evidence / 'account-after-crash.json')}",
        f"{exe} -config {cfg} -rung pilot -live > {quote(evidence / 'restart.log')} 2>&1 &",
        f"echo $! > {restart_pid}",
        f"ps -p \"$(cat {restart_pid})\" -o pid=,command=",
        f"kill -TERM \"$(cat {restart_pid})\"",
        f"wait \"$(cat {restart_pid})\"; echo $? > {quote(evidence / 'restart-exit-status.txt')}",
        f"go run ./cmd/accountcheck -ticker {quote(ticker)} -fills -out {quote(evidence / 'account-after-restart.json')}",
        f"rm -f {live_ok}",
    ]


def parse_phases(output):
    """[(PHASE header, prose lines, command lines)] from the printed operator block."""
    phases = []
    for line in output.splitlines():
        if line.startswith("PHASE "):
            phases.append((line, [], []))
        elif phases and line.startswith(stage.COMMAND_INDENT):
            phases[-1][2].append(line[len(stage.COMMAND_INDENT):])
        elif phases and line:
            phases[-1][1].append(line)
    return phases


def build_prepare_fixture(base):
    """A committed fixture repo, binary, build receipt, book and read-only fake runner."""
    repo = base / "repo"
    (repo / "go").mkdir(parents=True)
    (repo / "go/go.mod").write_text("module fixture\n")
    (repo / "go/main.go").write_text("package main\n")
    subprocess.run(["git", "init", "-q"], cwd=repo, check=True)
    subprocess.run(["git", "-c", "user.name=test", "-c", "user.email=test@example.com",
                    "add", "go/go.mod", "go/main.go"], cwd=repo, check=True)
    subprocess.run(["git", "-c", "user.name=test", "-c", "user.email=test@example.com",
                    "commit", "-qm", "fixture"], cwd=repo, check=True)
    binary = base / "harness"
    binary.write_text("fixture binary")
    binary.chmod(0o755)
    _, source_hash, _, _ = stage.source_manifest(repo)
    build_receipt = base / "build-identity.json"
    build_receipt.write_text(json.dumps({
        "source_manifest_sha256": source_hash,
        "binary_path": str(binary.resolve()),
        "binary_sha256": stage.sha256(binary),
    }))
    book = base / "book.json"
    book.write_text(json.dumps(book_report()))
    evidence_root = base / "evidence"
    commands = []
    cwds = []
    built = {}  # tool path -> cmd name, filled only by the fake `go build`
    market = {"ticker": TICKER}  # the market args() selects and the fake program list serves
    # What the in-prepare snapshot writes; distinct from book.json (best yes bid 45c).
    snapshot_book = book_report()
    snapshot_book["note"] = "captured inside prepare"
    snapshot_book["calls"]["orderbook"]["body"]["orderbook_fp"]["yes_dollars"] = [["0.45", "30"]]

    def use_ticker(ticker):
        """Move to another market, as R2 is pointed at a freshly chosen one.

        Later args() select it and book.json shows its book. The in-prepare snapshot
        (with_book=False) stays on TICKER.
        """
        market["ticker"] = ticker
        book.write_text(json.dumps(book_report(ticker)))

    def fake_runner(command, cwd, check, capture_output, text):
        commands.append(command)
        cwds.append(Path(cwd))
        if command[0:4] == ["go", "build", "-trimpath", "-o"] and len(command) == 6:
            output, name = Path(command[4]), command[5].removeprefix("./cmd/")
            output.write_text(f"fixture {name} binary")
            output.chmod(0o755)
            built[str(output)] = name
            return subprocess.CompletedProcess(command, 0, "", "")
        if command[0] == sys.executable and command[1].endswith("public_market_snapshot.py"):
            with Path(command[command.index("--out") + 1]).open("x") as stream:
                json.dump(snapshot_book, stream)
            return subprocess.CompletedProcess(command, 0, "receipt written", "")
        tool = built.get(command[0])
        asked = command[command.index("-ticker") + 1] if "-ticker" in command else None
        if tool == "incentives" and len(command) == 1:
            return subprocess.CompletedProcess(
                command, 0, json.dumps(programs_report(False, market["ticker"])), "")
        if tool == "incentives":
            return subprocess.CompletedProcess(command, 0, json.dumps(programs_report(ticker=asked)), "")
        if tool == "accountcheck":
            output = Path(command[command.index("-out") + 1])
            output.write_text(json.dumps(account_report(asked)))
            return subprocess.CompletedProcess(command, 0, "", "accountcheck report written")
        raise AssertionError(f"unexpected command: {command}")

    def args(stage_name, *extra, with_book=True):
        book_args = ["--book", str(book)] if with_book else []
        return stage.parser().parse_args([
            "--stage", stage_name, "--ticker", market["ticker"], "--size", "2.5",
            "--binary", str(binary), *book_args,
            "--candidate-receipt", str(build_receipt),
            "--evidence-root", str(evidence_root), "--repo", str(repo), *extra,
        ])

    return types.SimpleNamespace(repo=repo, binary=binary, source_hash=source_hash,
                                 commands=commands, cwds=cwds, runner=fake_runner, args=args,
                                 book=book, snapshot_book=snapshot_book, built=built,
                                 build_receipt=build_receipt, use_ticker=use_ticker)


def write_candidate(fx, *, source=None, binary=None, predecessor=None):
    """Move the fixture to another candidate and rewrite its build receipt.

    ``source`` replaces go/main.go (a new Go source manifest), ``binary`` the binary bytes;
    None leaves that half as it is. ``predecessor`` is the (source_manifest_sha256,
    binary_sha256) pair the receipt records, or None for an older receipt without one.
    Returns this candidate's own (source_manifest_sha256, binary_sha256) pair.
    """
    if source is not None:
        (fx.repo / "go/main.go").write_text(source)
    if binary is not None:
        fx.binary.write_text(binary)
    _, fx.source_hash, _, _ = stage.source_manifest(fx.repo)
    pair = (fx.source_hash, stage.sha256(fx.binary))
    receipt = {"source_manifest_sha256": pair[0], "binary_path": str(fx.binary.resolve()),
               "binary_sha256": pair[1]}
    if predecessor is not None:
        receipt["predecessor"] = {"source_manifest_sha256": predecessor[0],
                                  "binary_sha256": predecessor[1],
                                  "build_identity": "fixture predecessor build-identity.json"}
    fx.build_receipt.write_text(json.dumps(receipt))
    return pair


def tree_state(root):
    """Every path under root mapped to its file hash (None for a directory)."""
    return {str(path.relative_to(root)): stage.sha256(path) if path.is_file() else None
            for path in sorted(root.rglob("*"))}


def age_first_stage(first_evidence, **config_changes):
    """Rewrite a prepared first stage's config as an older stage would have it, re-pinned.

    Its identity.json hashes the new config, so the stage still binds and a receipt written
    afterwards attests the aged stage.
    """
    config_path = first_evidence.parent / "config.json"
    config = json.loads(config_path.read_text())
    config.update(config_changes)
    config_path.write_text(json.dumps(config, indent=2) + "\n")
    identity_path = first_evidence / "identity.json"
    identity = json.loads(identity_path.read_text())
    identity["config_sha256"] = stage.sha256(config_path)
    identity_path.write_text(json.dumps(identity, indent=2) + "\n")


def write_prior_stage(directory, config_changes=None, identity_changes=None):
    """A hand-written first stage with a dedicated runtime; returns its config path.

    It ran candidate pair ("src-a", "bin-a") on TICKER from a binary path that is long gone.
    The changes override config or identity fields, and the identity hashes the config as
    written, so a change is the only thing that can make it refuse.
    """
    base = Path(directory).resolve() / "first-stage"
    (base / "runtime").mkdir(parents=True)
    (base / "evidence").mkdir()
    config_path = base / "config.json"
    config = {"ticker": TICKER, "rung": "sizing",
              "paths": {key: str(base / "runtime" / name) for key, name in stage.RUNTIME_FILES.items()}}
    config.update(config_changes or {})
    config_path.write_text(json.dumps(config))
    identity = {"stage": "first", "rung": "sizing", "ticker": TICKER,
                "created_at_utc": NOW.isoformat(), "config_path": str(config_path),
                "config_sha256": stage.sha256(config_path),
                "source_manifest_sha256": "src-a", "binary_path": "/gone/harness-a",
                "binary_sha256": "bin-a"}
    identity.update(identity_changes or {})
    (base / "evidence" / "identity.json").write_text(json.dumps(identity))
    return config_path


def write_r2_receipt(first_evidence, name="r2-receipt"):
    """An R2 receipt bound to the first stage that prepare() just produced.

    It attests that first stage, so its candidate block is that stage's own ticker, source
    manifest and binary, whichever candidate the R2 stage later runs.
    """
    directory = first_evidence.parent.parent / name
    directory.mkdir()
    proof = directory / "proof.json"
    proof.write_text('{"observed":true}')
    identity_sha = stage.sha256(first_evidence / "identity.json")
    first = json.loads((first_evidence / "identity.json").read_text())
    receipt = {"candidate": {"ticker": first["ticker"],
                             "source_manifest_sha256": first["source_manifest_sha256"],
                             "binary_sha256": first["binary_sha256"],
                             "config_sha256": stage.sha256(first_evidence.parent / "config.json"),
                             "first_stage_identity_sha256": identity_sha},
               "acceptance": {field: {"operator_outcome": "observed", "operator": "fixture operator",
                                      "observation": f"Operator inspected {field} for this stage",
                                      "recorded_at_utc": NOW.isoformat(),
                                      "evidence": [{"path": "proof.json", "sha256": stage.sha256(proof),
                                                    "first_stage_identity_sha256": identity_sha}]}
                              for field in stage.R2_FIELDS}}
    path = directory / "receipt.json"
    path.write_text(json.dumps(receipt))
    return path


class OperatorStageTests(unittest.TestCase):
    def test_account_scope_requires_complete_flat_fresh_truth(self):
        report = account_report()
        stage.validate_account(report, TICKER, NOW + dt.timedelta(seconds=5), 60)
        report["flat"] = False
        with self.assertRaisesRegex(ValueError, "flatness"):
            stage.validate_account(report, TICKER, NOW + dt.timedelta(seconds=5), 60)

    def test_account_scope_rejects_stale_or_partial_reports(self):
        report = account_report()
        with self.assertRaisesRegex(ValueError, "stale"):
            stage.validate_account(report, TICKER, NOW + dt.timedelta(seconds=61), 60)
        report = account_report()
        report["positions_by_subaccount"]["0"]["outcome"] = "partial"
        with self.assertRaisesRegex(ValueError, "positions"):
            stage.validate_account(report, TICKER, NOW + dt.timedelta(seconds=5), 60)

    def test_account_rejects_old_start_and_request_inside_long_report(self):
        old = (NOW - dt.timedelta(minutes=10)).isoformat()
        report = account_report()
        report["started_at"] = old
        report["requests"][0]["at"] = old
        with self.assertRaisesRegex(ValueError, "start is stale"):
            stage.validate_account(report, TICKER, NOW, 60)
        report["started_at"] = NOW.isoformat()
        with self.assertRaisesRegex(ValueError, "request is stale"):
            stage.validate_account(report, TICKER, NOW, 60)
        report["requests"][0]["at"] = (NOW + dt.timedelta(seconds=31)).isoformat()
        with self.assertRaisesRegex(ValueError, "request is stale or in the future"):
            stage.validate_account(report, TICKER, NOW, 60)

    def test_public_candidate_and_book_must_be_current_and_cent_compatible(self):
        stage.validate_program_list(programs_report(False), TICKER, NOW, 60)
        stage.validate_program_detail(programs_report(), TICKER, NOW, 60)
        stage.validate_book(book_report(), TICKER, NOW, 60, 2)
        bad = book_report()
        bad["calls"]["market"]["body"]["market"]["price_level_structure"] = "subcent"
        with self.assertRaisesRegex(ValueError, "cent orders"):
            stage.validate_book(bad, TICKER, NOW, 60, 2)
        shallow = book_report()
        shallow["calls"]["orderbook"]["body"]["orderbook_fp"]["yes_dollars"] = [["0.44", "1"]]
        with self.assertRaisesRegex(ValueError, "depth is below"):
            stage.validate_book(shallow, TICKER, NOW, 60, 2)

    def test_book_rejects_wrong_request_routes_and_identity_chain(self):
        bad = book_report()
        bad["calls"]["orderbook"]["path"] = "/markets/OTHER/orderbook"
        with self.assertRaisesRegex(ValueError, "request path"):
            stage.validate_book(bad, TICKER, NOW, 60, 2)
        bad = book_report()
        bad["calls"]["event"]["body"]["event"]["series_ticker"] = "OTHER"
        with self.assertRaisesRegex(ValueError, "request path"):
            stage.validate_book(bad, TICKER, NOW, 60, 2)
        bad = book_report()
        bad["calls"]["series"]["body"]["series"]["ticker"] = "OTHER"
        with self.assertRaisesRegex(ValueError, "series fee metadata"):
            stage.validate_book(bad, TICKER, NOW, 60, 2)

    def book_with(self, yes, no):
        book = book_report()
        book["calls"]["orderbook"]["body"]["orderbook_fp"] = {"yes_dollars": yes, "no_dollars": no}
        return book

    def test_book_refuses_2c_97c_mid_below_h_sel_6(self):
        # 2026-09-29: best bids 2c/97c, levels ascending as the API returns them.
        bad = self.book_with([["0.01", "1048.15"], ["0.02", "624"]],
                             [["0.01", "14248.36"], ["0.43", "69"], ["0.96", "1144"], ["0.97", "220"]])
        with self.assertRaisesRegex(ValueError, r"H-SEL-6 mid 2\.5c outside 10c-90c "
                                    r"\(best yes_bid 2c, best no_bid 97c\)"):
            stage.validate_book(bad, TICKER, NOW, 60, 2)

    def test_book_44c_54c_passes_selection_filters(self):
        stage.validate_book(self.book_with([["0.44", "20"]], [["0.54", "20"]]), TICKER, NOW, 60, 2)

    def test_book_selection_filter_boundaries(self):
        passing = [((9, 89), "mid 10"), ((89, 9), "mid 90"), ((45, 54), "bid sum 99")]
        for (yes, no), label in passing:
            with self.subTest(label):
                stage.validate_book(self.book_with([[f"0.{yes:02d}", "20"]], [[f"0.{no:02d}", "20"]]),
                                    TICKER, NOW, 60, 2)
        failing = [((8, 89), r"H-SEL-6 mid 9\.5c"), ((89, 8), r"H-SEL-6 mid 90\.5c"),
                   ((46, 54), r"H-SEL-7 bid_sum 100c > 99c \(")]
        for (yes, no), pattern in failing:
            with self.subTest(pattern):
                with self.assertRaisesRegex(ValueError, pattern):
                    stage.validate_book(self.book_with([[f"0.{yes:02d}", "20"]], [[f"0.{no:02d}", "20"]]),
                                        TICKER, NOW, 60, 2)

    def test_book_selection_uses_max_level_not_list_order(self):
        # Descending order still passes on the true best bids (44c/54c).
        stage.validate_book(self.book_with([["0.44", "20"], ["0.02", "20"]],
                                           [["0.54", "20"], ["0.10", "20"]]), TICKER, NOW, 60, 2)
        # The first listed level would pass; the true best yes bid (60c) breaks H-SEL-7.
        with self.assertRaisesRegex(ValueError, r"H-SEL-7 bid_sum 114c"):
            stage.validate_book(self.book_with([["0.44", "20"], ["0.60", "20"]], [["0.54", "20"]]),
                                TICKER, NOW, 60, 2)

    def test_r2_receipt_must_be_candidate_bound_and_evidence_backed(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "receipt.json"
            proof = path.parent / "proof.json"
            proof.write_text('{"observed":true}')
            proof_hash = stage.sha256(proof)
            identity = {"created_at_utc": (NOW - dt.timedelta(hours=3)).isoformat(),
                        "identity_sha256": "first-id"}
            receipt = {"candidate": {"ticker": TICKER, "source_manifest_sha256": "src",
                                     "binary_sha256": "bin", "config_sha256": "cfg",
                                     "first_stage_identity_sha256": "first-id"},
                       "acceptance": {field: {"operator_outcome": "observed", "operator": "fixture operator",
                                              "observation": f"Operator inspected {field} for this stage",
                                              "recorded_at_utc": NOW.isoformat(),
                                              "evidence": [{"path": "proof.json", "sha256": proof_hash,
                                                            "first_stage_identity_sha256": "first-id"}]}
                                      for field in stage.R2_FIELDS}}
            path.write_text(json.dumps(receipt))
            loaded = stage.load_r2_receipt(path, TICKER, "src", "bin", "cfg", identity, NOW)
            self.assertIs(loaded["artifact_content_verified"], False)
            receipt["acceptance"]["alarm"]["evidence"][0]["sha256"] = "bad"
            path.write_text(json.dumps(receipt))
            with self.assertRaisesRegex(ValueError, "alarm"):
                stage.load_r2_receipt(path, TICKER, "src", "bin", "cfg", identity, NOW)

    def test_r2_receipt_rejects_before_stage_future_and_reused_artifacts(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "receipt.json"
            proof = path.parent / "proof.json"
            proof.write_text("fixture")
            identity = {"created_at_utc": NOW.isoformat(), "identity_sha256": "first-id"}
            entry = {"operator_outcome": "observed", "operator": "operator",
                     "observation": "Inspected stage output and exchange response",
                     "recorded_at_utc": NOW.isoformat(),
                     "evidence": [{"path": "proof.json", "sha256": stage.sha256(proof),
                                   "first_stage_identity_sha256": "first-id"}]}
            receipt = {"candidate": {"ticker": TICKER, "source_manifest_sha256": "src",
                                     "binary_sha256": "bin", "config_sha256": "cfg",
                                     "first_stage_identity_sha256": "first-id"},
                       "acceptance": {field: dict(entry) for field in stage.R2_FIELDS}}
            receipt["acceptance"]["orders"]["recorded_at_utc"] = (NOW - dt.timedelta(seconds=1)).isoformat()
            path.write_text(json.dumps(receipt))
            with self.assertRaisesRegex(ValueError, "orders"):
                stage.load_r2_receipt(path, TICKER, "src", "bin", "cfg", identity, NOW)
            receipt["acceptance"]["orders"]["recorded_at_utc"] = (NOW + dt.timedelta(seconds=31)).isoformat()
            path.write_text(json.dumps(receipt))
            with self.assertRaisesRegex(ValueError, "orders"):
                stage.load_r2_receipt(path, TICKER, "src", "bin", "cfg", identity, NOW)
            receipt["acceptance"]["orders"]["recorded_at_utc"] = NOW.isoformat()
            receipt["acceptance"]["orders"]["evidence"] = [{"path": "proof.json",
                "sha256": stage.sha256(proof), "first_stage_identity_sha256": "other-id"}]
            path.write_text(json.dumps(receipt))
            with self.assertRaisesRegex(ValueError, "orders"):
                stage.load_r2_receipt(path, TICKER, "src", "bin", "cfg", identity, NOW)
            receipt["acceptance"]["orders"]["evidence"][0]["first_stage_identity_sha256"] = "first-id"
            receipt["acceptance"]["orders"]["evidence"][0]["path"] = "../proof.json"
            path.write_text(json.dumps(receipt))
            with self.assertRaisesRegex(ValueError, "orders"):
                stage.load_r2_receipt(path, TICKER, "src", "bin", "cfg", identity, NOW)

    def test_prior_stage_rejects_reused_and_escaping_paths(self):
        with tempfile.TemporaryDirectory() as directory:
            base = Path(directory).resolve() / "first-stage"
            (base / "runtime").mkdir(parents=True)
            (base / "evidence").mkdir()
            binary = Path(directory) / "binary"
            binary.write_text("binary")
            config_path = base / "config.json"
            config = {"ticker": TICKER, "rung": "sizing",
                      "paths": {key: str(base / "runtime" / name)
                                for key, name in stage.RUNTIME_FILES.items()}}
            config_path.write_text(json.dumps(config))
            identity = {"stage": "first", "rung": "sizing", "ticker": TICKER,
                        "created_at_utc": NOW.isoformat(), "config_path": str(config_path),
                        "config_sha256": stage.sha256(config_path),
                        "source_manifest_sha256": "src", "binary_path": str(binary),
                        "binary_sha256": stage.sha256(binary)}
            (base / "evidence" / "identity.json").write_text(json.dumps(identity))
            candidate = ("src", stage.sha256(binary))
            stage.load_prior_stage(config_path, candidate)
            for bad in (str(Path(directory) / "old.db"), "../../outside.db"):
                config["paths"]["db"] = bad
                config_path.write_text(json.dumps(config))
                identity["config_sha256"] = stage.sha256(config_path)
                (base / "evidence" / "identity.json").write_text(json.dumps(identity))
                with self.assertRaisesRegex(ValueError, "db path"):
                    stage.load_prior_stage(config_path, candidate)
            config["paths"]["db"] = str(base / "runtime" / "harness.db")
            config_path.write_text(json.dumps(config))
            identity["config_sha256"] = stage.sha256(config_path)
            (base / "evidence" / "identity.json").write_text(json.dumps(identity))
            (base / "runtime" / "harness.db").symlink_to(binary)
            with self.assertRaisesRegex(ValueError, "db path"):
                stage.load_prior_stage(config_path, candidate)

    def test_prior_stage_binds_to_the_current_candidate_or_its_predecessor_as_a_pair(self):
        predecessor, current = ("src-a", "bin-a"), ("src-b", "bin-b")
        cases = [  # (prior manifest, prior binary, binding or None for a refusal)
            ("src-b", "bin-b", "same_candidate"), ("src-a", "bin-a", "predecessor"),
            ("src-b", "bin-a", None),  # current manifest with the predecessor binary
            ("src-a", "bin-b", None),  # predecessor manifest with the current binary
            ("src-b", "bin-x", None), ("src-x", "bin-b", None),
            ("src-a", "bin-x", None), ("src-x", "bin-a", None), ("src-x", "bin-x", None)]
        for source, binary, binding in cases:
            with self.subTest(source=source, binary=binary), tempfile.TemporaryDirectory() as directory:
                config_path = write_prior_stage(directory, identity_changes={
                    "source_manifest_sha256": source, "binary_sha256": binary})
                if binding is None:
                    with self.assertRaisesRegex(
                            ValueError, "neither the current candidate nor its recorded predecessor"):
                        stage.load_prior_stage(config_path, current, predecessor)
                else:
                    _, identity = stage.load_prior_stage(config_path, current, predecessor)
                    self.assertEqual(identity["binding"], binding)
        # A receipt that records no predecessor leaves only the current candidate to bind.
        with tempfile.TemporaryDirectory() as directory:
            config_path = write_prior_stage(directory)  # ran ("src-a", "bin-a")
            with self.assertRaisesRegex(ValueError, "neither the current candidate"):
                stage.load_prior_stage(config_path, current)
            with self.assertRaisesRegex(ValueError, "neither the current candidate"):
                stage.load_prior_stage(config_path, current, None)
            self.assertEqual(stage.load_prior_stage(config_path, predecessor)[1]["binding"],
                             "same_candidate")

    def test_prior_stage_binds_through_the_recorded_predecessor_chain(self):
        # lip-14o: candidate-7's first stage was run by candidate-5, two receipts back.
        current = ("src-c", "bin-c")
        chain = [("src-b", "bin-b"), ("src-a", "bin-a")]  # nearest first
        cases = [  # (prior manifest, prior binary, binding or None for a refusal, depth)
            ("src-c", "bin-c", "same_candidate", 0), ("src-b", "bin-b", "predecessor", 1),
            ("src-a", "bin-a", "predecessor", 2),
            ("src-a", "bin-b", None, None), ("src-b", "bin-a", None, None),  # mixed pairs
            ("src-x", "bin-x", None, None)]
        for source, binary, binding, depth in cases:
            with self.subTest(source=source, binary=binary), tempfile.TemporaryDirectory() as directory:
                config_path = write_prior_stage(directory, identity_changes={
                    "source_manifest_sha256": source, "binary_sha256": binary})
                if binding is None:
                    with self.assertRaisesRegex(
                            ValueError, "neither the current candidate nor its recorded predecessor"):
                        stage.load_prior_stage(config_path, current, chain)
                else:
                    _, identity = stage.load_prior_stage(config_path, current, chain)
                    self.assertEqual((identity["binding"], identity["predecessor_depth"]),
                                     (binding, depth))
        # The one-pair form still binds at depth 1 and records it.
        with tempfile.TemporaryDirectory() as directory:
            config_path = write_prior_stage(directory)  # ran ("src-a", "bin-a")
            _, identity = stage.load_prior_stage(config_path, current, ("src-a", "bin-a"))
            self.assertEqual((identity["binding"], identity["predecessor_depth"]), ("predecessor", 1))

    def test_receipt_predecessors_walks_only_attested_receipts(self):
        def receipt(pair, predecessor=None, build_identity=None):
            body = {"source_manifest_sha256": pair[0], "binary_sha256": pair[1]}
            if predecessor is not None:
                body["predecessor"] = {"source_manifest_sha256": predecessor[0],
                                       "binary_sha256": predecessor[1],
                                       "build_identity": build_identity}
            return body

        def load(root, name):
            return json.loads((root / name).read_text()), root / name

        a, b, c = ("src-a", "bin-a"), ("src-b", "bin-b"), ("src-c", "bin-c")
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            (root / "a.json").write_text(json.dumps(receipt(a)))
            (root / "b.json").write_text(json.dumps(receipt(b, a, str(root / "a.json"))))
            # A relative build_identity resolves against the receipt that names it.
            (root / "c.json").write_text(json.dumps(receipt(c, b, "b.json")))
            self.assertEqual(stage.receipt_predecessors(*load(root, "c.json")), [b, a])
            # No recorded predecessor: nothing to bind beyond the candidate.
            self.assertEqual(stage.receipt_predecessors(receipt(c), root / "c.json"), [])
            # A named receipt that cannot be read ends the walk after the first hop, which
            # is what an older receipt and the fixture receipts give.
            (root / "d.json").write_text(json.dumps(
                receipt(("src-d", "bin-d"), c, str(root / "missing.json"))))
            self.assertEqual(stage.receipt_predecessors(*load(root, "d.json")), [c])
            # A readable receipt that attests a different pair refuses the whole chain.
            (root / "e.json").write_text(json.dumps(
                receipt(("src-e", "bin-e"), ("src-b", "bin-x"), str(root / "b.json"))))
            with self.assertRaisesRegex(ValueError, "does not attest"):
                stage.receipt_predecessors(*load(root, "e.json"))
            # A cycle refuses the chain.
            (root / "x.json").write_text(json.dumps(
                receipt(("src-x", "bin-x"), ("src-y", "bin-y"), str(root / "y.json"))))
            (root / "y.json").write_text(json.dumps(
                receipt(("src-y", "bin-y"), ("src-x", "bin-x"), str(root / "x.json"))))
            with self.assertRaisesRegex(ValueError, "cyclic"):
                stage.receipt_predecessors(*load(root, "x.json"))
            # The depth is bounded.
            with self.assertRaisesRegex(ValueError, "deeper than"):
                stage.receipt_predecessors(*load(root, "c.json"), max_depth=1)

    def test_prior_stage_ticker_and_binary_path_are_free_but_must_be_consistent(self):
        candidate = ("src-a", "bin-a")
        with tempfile.TemporaryDirectory() as directory:
            config_path = write_prior_stage(directory, {"ticker": "KXOLD-26AUG"},
                                            {"ticker": "KXOLD-26AUG"})
            config, identity = stage.load_prior_stage(config_path, candidate)
            self.assertEqual((config["ticker"], identity["ticker"], identity["binding"]),
                             ("KXOLD-26AUG", "KXOLD-26AUG", "same_candidate"))
            self.assertFalse(Path(identity["binary_path"]).exists())
        for label, config_changes, identity_changes in [
                ("identity and config name different markets", {"ticker": "KXOLD-26AUG"}, {}),
                ("no ticker anywhere", {"ticker": None}, {"ticker": None}),
                ("unusable ticker", {"ticker": "bad ticker"}, {"ticker": "bad ticker"})]:
            with self.subTest(label), tempfile.TemporaryDirectory() as directory:
                config_path = write_prior_stage(directory, config_changes, identity_changes)
                with self.assertRaisesRegex(ValueError, "does not match first-stage candidate identity"):
                    stage.load_prior_stage(config_path, candidate)

    def test_prior_stage_must_still_be_a_sizing_first_stage_of_its_own_config(self):
        candidate = ("src-a", "bin-a")
        for label, config_changes, identity_changes in [
                ("an r2 stage", {}, {"stage": "r2"}),
                ("identity rung pilot", {}, {"rung": "pilot"}),
                ("config rung pilot", {"rung": "pilot"}, {}),
                ("config moved", {}, {"config_path": "/elsewhere/config.json"}),
                ("config edited after its identity", {}, {"config_sha256": "0" * 64})]:
            with self.subTest(label), tempfile.TemporaryDirectory() as directory:
                config_path = write_prior_stage(directory, config_changes, identity_changes)
                with self.assertRaisesRegex(ValueError, "does not match first-stage candidate identity"):
                    stage.load_prior_stage(config_path, candidate)
        with tempfile.TemporaryDirectory() as directory:
            config_path = write_prior_stage(directory, identity_changes={"created_at_utc": "yesterday"})
            with self.assertRaisesRegex(ValueError, "creation timestamp"):
                stage.load_prior_stage(config_path, candidate)

    def test_receipt_predecessor_reads_the_recorded_pair_and_refuses_a_malformed_one(self):
        pair = {"source_manifest_sha256": "s" * 64, "binary_sha256": "b" * 64}
        self.assertIsNone(stage.receipt_predecessor({}))  # an older receipt
        self.assertIsNone(stage.receipt_predecessor({"predecessor": None}))
        recorded = {**pair, "build_identity": "/notes/candidate-5/build-identity.json"}
        self.assertEqual(stage.receipt_predecessor({"predecessor": recorded}), ("s" * 64, "b" * 64))
        for bad in ("candidate-5", [], {}, {"binary_sha256": "b" * 64},
                    {"source_manifest_sha256": "s" * 64}, {**pair, "binary_sha256": ""},
                    {**pair, "source_manifest_sha256": 7}):
            with self.subTest(bad=bad), self.assertRaisesRegex(ValueError, "predecessor needs"):
                stage.receipt_predecessor({"predecessor": bad})

    def test_r2_receipt_candidate_block_must_match_the_first_stage_field_by_field(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "receipt.json"
            proof = path.parent / "proof.json"
            proof.write_text("fixture")
            identity = {"created_at_utc": NOW.isoformat(), "identity_sha256": "first-id"}
            entry = {"operator_outcome": "observed", "operator": "operator",
                     "observation": "Inspected stage output and exchange response",
                     "recorded_at_utc": NOW.isoformat(),
                     "evidence": [{"path": "proof.json", "sha256": stage.sha256(proof),
                                   "first_stage_identity_sha256": "first-id"}]}
            candidate = {"ticker": TICKER, "source_manifest_sha256": "src", "binary_sha256": "bin",
                         "config_sha256": "cfg", "first_stage_identity_sha256": "first-id"}
            receipt = {"candidate": candidate,
                       "acceptance": {field: dict(entry) for field in stage.R2_FIELDS}}
            path.write_text(json.dumps(receipt))
            stage.load_r2_receipt(path, TICKER, "src", "bin", "cfg", identity, NOW)
            for field in candidate:
                with self.subTest(field):
                    receipt["candidate"] = {**candidate, field: "other"}
                    path.write_text(json.dumps(receipt))
                    with self.assertRaisesRegex(ValueError,
                                                "not bound to the first-stage candidate identity"):
                        stage.load_r2_receipt(path, TICKER, "src", "bin", "cfg", identity, NOW)

    def test_exit_arithmetic_walks_current_depth_and_rounds_fees(self):
        book = book_report()
        book["calls"]["orderbook"]["body"]["orderbook_fp"] = {
            "yes_dollars": [["0.71", "100"], ["0.72", "660.45"]],
            "no_dollars": [["0.23", "100"], ["0.27", "37"]],
        }
        result = stage.exit_arithmetic(book, 12)
        self.assertEqual(result["exits"]["yes"]["estimated_taker_fee_usd"], "0.1694")
        self.assertEqual(result["exits"]["yes"]["net_proceeds_usd"], "8.4706")
        self.assertEqual(result["exits"]["no"]["estimated_taker_fee_usd"], "0.1656")
        book["calls"]["orderbook"]["body"]["orderbook_fp"]["no_dollars"][1][1] = "2"
        result = stage.exit_arithmetic(book, 12)
        self.assertEqual([x["contracts"] for x in result["exits"]["no"]["levels"]], ["2", "10"])
        self.assertEqual(result["exits"]["no"]["gross_usd"], "2.84")

    def test_prepare_runs_only_read_tools_and_prints_operator_commands(self):
        with tempfile.TemporaryDirectory() as directory:
            base = Path(directory)
            fx = build_prepare_fixture(base)
            args = fx.args("first")
            commands = fx.commands
            fake_runner = fx.runner
            with contextlib.redirect_stdout(io.StringIO()) as output:
                result = stage.prepare(args, runner=fake_runner, clock=lambda: NOW)
            self.assertTrue((result.parent / "config.json").is_file())
            self.assertTrue((result / "identity.json").is_file())
            config = json.loads((result.parent / "config.json").read_text())
            self.assertEqual(config["rung"], "sizing")
            self.assertEqual(config["s"], 2.5)
            self.assertFalse(Path(config["paths"]["live_ok"]).exists())
            self.assertTrue(all("-live" not in command for command in commands))
            self.assertIn("-rung sizing -live", output.getvalue())
            args.evidence_root = str(base / "late-evidence")
            ticks = 0

            def late_clock():
                nonlocal ticks
                ticks += 1
                return NOW if ticks < 7 else NOW + dt.timedelta(seconds=61)

            with self.assertRaisesRegex(ValueError, "public incentive start is stale"):
                stage.prepare(args, runner=fake_runner, clock=late_clock)

    def test_prepare_first_stage_config_heartbeat_is_hourly(self):
        with tempfile.TemporaryDirectory() as directory:
            fx = build_prepare_fixture(Path(directory))
            with contextlib.redirect_stdout(io.StringIO()):
                evidence = stage.prepare(fx.args("first"), runner=fx.runner, clock=lambda: NOW)
            config = json.loads((evidence.parent / "config.json").read_text())
            self.assertEqual(config["heartbeat_s"], 3600)

    def assert_paste_safe_phases(self, args, output, evidence, fx):
        config = json.loads((evidence.parent / "config.json").read_text())
        shared = dict(exe=stage.quote(fx.binary.resolve()), cfg=stage.quote(evidence.parent / "config.json"),
                      evidence=evidence, repo=fx.repo.resolve(), ticker=args.ticker)
        if args.stage == "first":
            expected = legacy_commands(legacy_operator_lines(args, config, **shared))
        else:
            expected = r2_operator_commands(config, **shared)
        self.assertTrue(expected[-1].startswith("rm -f "))
        commands, prose = [], []
        for line in output.splitlines():
            if not line:
                continue
            if line.startswith(stage.COMMAND_INDENT):
                self.assertFalse(line[len(stage.COMMAND_INDENT)].isspace(), line)
                commands.append(line[len(stage.COMMAND_INDENT):])
            else:
                self.assertFalse(line[0].isspace(), f"ambiguous indentation: {line!r}")
                prose.append(line)
        for command in commands:
            self.assertNotIn("#", command)
        for line in prose:
            self.assertEqual(set(line) & FORBIDDEN_PROSE, set(), line)
            self.assertTrue(line.startswith(PROSE_LEADS), line)
        self.assertEqual(collections.Counter(commands), collections.Counter(expected))
        self.assertEqual(commands, expected)
        self.assertTrue(any("one phase at a time" in line for line in prose))
        # Checkpoints are phase boundaries: a ps is never pasted with its kill, and
        # a live start never shares a phase with the observation or signal before it.
        phases = []
        for line in output.splitlines():
            if line.startswith("PHASE "):
                phases.append([])
            elif line.startswith(stage.COMMAND_INDENT):
                phases[-1].append(line.strip())
        self.assertEqual(len(phases), 10)
        for phase in phases:
            self.assertFalse(any(c.startswith("ps ") for c in phase)
                             and any(c.startswith("kill ") for c in phase), phase)
            if any(" -live " in c for c in phase):
                self.assertFalse(any(c.startswith(("ps ", "kill ", "wait ", "go run ")) for c in phase), phase)

    def test_prepare_first_stage_prints_paste_safe_phases(self):
        with tempfile.TemporaryDirectory() as directory:
            fx = build_prepare_fixture(Path(directory))
            args = fx.args("first")
            with contextlib.redirect_stdout(io.StringIO()) as output:
                evidence = stage.prepare(args, runner=fx.runner, clock=lambda: NOW)
            self.assert_paste_safe_phases(args, output.getvalue(), evidence, fx)

    def test_prepare_r2_stage_prints_paste_safe_phases(self):
        with tempfile.TemporaryDirectory() as directory:
            fx = build_prepare_fixture(Path(directory))
            with contextlib.redirect_stdout(io.StringIO()):
                first_evidence = stage.prepare(fx.args("first"), runner=fx.runner, clock=lambda: NOW)
            args = fx.args("r2", "--prior-config", str(first_evidence.parent / "config.json"),
                           "--r2-receipt", str(write_r2_receipt(first_evidence)))
            with contextlib.redirect_stdout(io.StringIO()) as output:
                evidence = stage.prepare(args, runner=fx.runner, clock=lambda: NOW)
            self.assertEqual(json.loads((evidence.parent / "config.json").read_text())["rung"], "pilot")
            self.assertTrue(all("-live" not in command for command in fx.commands))
            self.assert_paste_safe_phases(args, output.getvalue(), evidence, fx)

    def prepare_first(self, fx):
        with contextlib.redirect_stdout(io.StringIO()):
            return stage.prepare(fx.args("first"), runner=fx.runner, clock=lambda: NOW)

    def prepare_r2(self, fx, first_evidence, receipt=None, root=None):
        """Prepare an R2 stage on first_evidence; a distinct root lets one test try several."""
        args = fx.args("r2", "--prior-config", str(first_evidence.parent / "config.json"),
                       "--r2-receipt", str(receipt or write_r2_receipt(first_evidence)))
        if root is not None:
            args.evidence_root = str(root)
        with contextlib.redirect_stdout(io.StringIO()) as output:
            evidence = stage.prepare(args, runner=fx.runner, clock=lambda: NOW)
        return evidence, output.getvalue()

    def assert_fresh_store(self, evidence, first_evidence, ticker):
        """The R2 config is its first stage's with only store paths, rung, heartbeat, market and S changed."""
        config = json.loads((evidence.parent / "config.json").read_text())
        prior = json.loads((first_evidence.parent / "config.json").read_text())
        runtime = evidence.parent / "runtime"
        self.assertNotEqual(runtime, first_evidence.parent / "runtime")
        for key, filename in stage.RUNTIME_FILES.items():
            self.assertEqual(config["paths"][key], str(runtime / filename), key)
            self.assertNotEqual(config["paths"][key], prior["paths"][key], key)
        self.assertEqual({k: v for k, v in config["paths"].items() if k not in stage.RUNTIME_FILES},
                         {k: v for k, v in prior["paths"].items() if k not in stage.RUNTIME_FILES})
        self.assertEqual((config["rung"], config["heartbeat_s"], config["ticker"], config["s"]),
                         ("pilot", 3600, ticker, 2.5))
        changed = {"paths", "rung", "heartbeat_s", "ticker", "s"}
        self.assertEqual({k: v for k, v in config.items() if k not in changed},
                         {k: v for k, v in prior.items() if k not in changed})
        self.assertEqual(list(runtime.iterdir()), [])  # the helper provisions nothing

    def test_r2_same_candidate_first_stage_binds_with_a_fresh_store_on_any_market(self):
        with tempfile.TemporaryDirectory() as directory:
            base = Path(directory)
            fx = build_prepare_fixture(base)
            first_evidence = self.prepare_first(fx)
            first = json.loads((first_evidence / "identity.json").read_text())
            prior_tree = tree_state(first_evidence.parent)
            receipt = write_r2_receipt(first_evidence)
            for number, ticker in enumerate((TICKER, OTHER_TICKER)):
                with self.subTest(ticker=ticker):
                    fx.use_ticker(ticker)
                    evidence, output = self.prepare_r2(fx, first_evidence, receipt,
                                                       root=base / f"r2-{number}")
                    identity = json.loads((evidence / "identity.json").read_text())
                    self.assertEqual(identity["ticker"], ticker)
                    self.assertEqual(identity["first_stage_binding"], "same_candidate")
                    self.assertEqual(identity["first_stage_ticker"], TICKER)
                    self.assertEqual(identity["first_stage_binary_sha256"], first["binary_sha256"])
                    self.assertEqual(identity["first_stage_source_manifest_sha256"],
                                     first["source_manifest_sha256"])
                    self.assert_fresh_store(evidence, first_evidence, ticker)
                    self.assertNotIn(str(first_evidence.parent), output)
            self.assertEqual(tree_state(first_evidence.parent), prior_tree)

    def test_r2_accepts_a_first_stage_of_the_recorded_predecessor_on_a_fresh_store_and_market(self):
        with tempfile.TemporaryDirectory() as directory:
            fx = build_prepare_fixture(Path(directory))
            first_evidence = self.prepare_first(fx)
            # An older first stage: the template's 30 s heartbeat, another S and its own
            # tuning, which the R2 config keeps except for heartbeat and S.
            age_first_stage(first_evidence, heartbeat_s=30, s=7, inv_soft=4)
            first_dir = first_evidence.parent
            first = json.loads((first_evidence / "identity.json").read_text())
            first_pair = (first["source_manifest_sha256"], first["binary_sha256"])
            prior_tree = tree_state(first_dir)
            # The next candidate: another Go source manifest and other binary bytes, its
            # receipt naming the candidate the first stage ran as predecessor.
            current_pair = write_candidate(fx, source="package main\n// next candidate\n",
                                           binary="next candidate binary", predecessor=first_pair)
            self.assertNotEqual(current_pair[0], first_pair[0])
            self.assertNotEqual(current_pair[1], first_pair[1])
            fx.use_ticker(OTHER_TICKER)
            evidence, output = self.prepare_r2(fx, first_evidence)
            identity = json.loads((evidence / "identity.json").read_text())
            self.assertTrue(evidence.parent.name.startswith(f"r2-{OTHER_TICKER}-"))
            self.assertEqual((identity["stage"], identity["rung"], identity["ticker"]),
                             ("r2", "pilot", OTHER_TICKER))
            self.assertEqual((identity["source_manifest_sha256"], identity["binary_sha256"]), current_pair)
            self.assertEqual(identity["first_stage_binding"], "predecessor")
            self.assertEqual(identity["first_stage_config"], str(first_dir / "config.json"))
            self.assertEqual(identity["first_stage_ticker"], TICKER)
            self.assertEqual(identity["first_stage_source_manifest_sha256"], first_pair[0])
            self.assertEqual(identity["first_stage_binary_sha256"], first_pair[1])
            self.assertEqual(identity["first_stage_identity_sha256"],
                             stage.sha256(first_evidence / "identity.json"))
            self.assertIs(identity["r2_attestation"]["artifact_content_verified"], False)
            self.assert_fresh_store(evidence, first_evidence, OTHER_TICKER)
            aged = json.loads((first_dir / "config.json").read_text())
            config = json.loads((evidence.parent / "config.json").read_text())
            self.assertEqual((aged["heartbeat_s"], aged["s"], aged["inv_soft"]), (30, 7, 4))
            self.assertEqual((config["heartbeat_s"], config["s"], config["inv_soft"]), (3600, 2.5, 4))
            self.assertNotIn(str(first_dir), output)
            # Nothing was created in or changed under the prior stage directory.
            self.assertEqual(tree_state(first_dir), prior_tree)

    def test_r2_refuses_a_first_stage_outside_the_candidate_and_its_recorded_predecessor(self):
        with tempfile.TemporaryDirectory() as directory:
            base = Path(directory)
            fx = build_prepare_fixture(base)
            first_evidence = self.prepare_first(fx)
            first_pair = (fx.source_hash, stage.sha256(fx.binary))
            write_candidate(fx, source="package main\n// next candidate\n",
                            binary="next candidate binary")
            fx.use_ticker(OTHER_TICKER)
            receipt = write_r2_receipt(first_evidence)
            cases = [("no predecessor recorded", None),
                     ("a predecessor that is not the first stage", ("f" * 64, "e" * 64)),
                     ("the first stage manifest with another binary", (first_pair[0], "e" * 64)),
                     ("the first stage binary with another manifest", ("f" * 64, first_pair[1]))]
            for number, (label, predecessor) in enumerate(cases):
                with self.subTest(label):
                    write_candidate(fx, predecessor=predecessor)
                    with self.assertRaisesRegex(
                            ValueError, "neither the current candidate nor its recorded predecessor"):
                        self.prepare_r2(fx, first_evidence, receipt, root=base / f"refused-{number}")
            # Only the recorded predecessor differs from the control that binds.
            write_candidate(fx, predecessor=first_pair)
            evidence, _ = self.prepare_r2(fx, first_evidence, receipt, root=base / "accepted")
            self.assertEqual(json.loads((evidence / "identity.json").read_text())["first_stage_binding"],
                             "predecessor")

    def test_r2_refuses_a_mixed_pair_of_predecessor_and_current_halves(self):
        new_source, new_binary = "package main\n// new manifest\n", "new binary"
        scenarios = [("predecessor binary with the current manifest", {"source": new_source},
                      {"binary": new_binary}),
                     ("predecessor manifest with the current binary", {"binary": new_binary},
                      {"source": new_source})]
        for label, first_change, current_change in scenarios:
            with self.subTest(label), tempfile.TemporaryDirectory() as directory:
                fx = build_prepare_fixture(Path(directory))
                origin = (fx.source_hash, stage.sha256(fx.binary))  # recorded as the predecessor
                write_candidate(fx, **first_change)  # the first stage runs half of the change
                first_evidence = self.prepare_first(fx)
                current_pair = write_candidate(fx, predecessor=origin, **current_change)
                first = json.loads((first_evidence / "identity.json").read_text())
                first_pair = (first["source_manifest_sha256"], first["binary_sha256"])
                self.assertNotIn(first_pair, (origin, current_pair))
                self.assertIn(first_pair[0], (origin[0], current_pair[0]))
                self.assertIn(first_pair[1], (origin[1], current_pair[1]))
                with self.assertRaisesRegex(
                        ValueError, "neither the current candidate nor its recorded predecessor"):
                    self.prepare_r2(fx, first_evidence)

    def test_r2_receipt_naming_the_current_candidate_is_refused_for_a_predecessor_stage(self):
        with tempfile.TemporaryDirectory() as directory:
            base = Path(directory)
            fx = build_prepare_fixture(base)
            first_evidence = self.prepare_first(fx)
            first_pair = (fx.source_hash, stage.sha256(fx.binary))
            current_pair = write_candidate(fx, source="package main\n// next candidate\n",
                                           binary="next candidate binary", predecessor=first_pair)
            fx.use_ticker(OTHER_TICKER)
            cases = {"the whole current candidate": {"ticker": OTHER_TICKER,
                                                     "source_manifest_sha256": current_pair[0],
                                                     "binary_sha256": current_pair[1]},
                     "only the current source manifest": {"source_manifest_sha256": current_pair[0]},
                     "only the current binary": {"binary_sha256": current_pair[1]},
                     "only the new market": {"ticker": OTHER_TICKER}}
            for number, (label, changes) in enumerate(cases.items()):
                with self.subTest(label):
                    receipt = write_r2_receipt(first_evidence, name=f"receipt-{number}")
                    body = json.loads(receipt.read_text())
                    body["candidate"].update(changes)
                    receipt.write_text(json.dumps(body))
                    with self.assertRaisesRegex(
                            ValueError, "not bound to the first-stage candidate identity"):
                        self.prepare_r2(fx, first_evidence, receipt, root=base / f"refused-{number}")

    def test_malformed_receipt_predecessor_refuses_r2_but_not_the_first_stage(self):
        with tempfile.TemporaryDirectory() as directory:
            base = Path(directory)
            fx = build_prepare_fixture(base)
            first_evidence = self.prepare_first(fx)
            body = json.loads(fx.build_receipt.read_text())
            body["predecessor"] = {"binary_sha256": "b" * 64}
            fx.build_receipt.write_text(json.dumps(body))
            with self.assertRaisesRegex(ValueError, "predecessor needs"):
                self.prepare_r2(fx, first_evidence, root=base / "refused")
            args = fx.args("first")
            args.evidence_root = str(base / "first-again")
            with contextlib.redirect_stdout(io.StringIO()):
                stage.prepare(args, runner=fx.runner, clock=lambda: NOW)

    def test_prepare_r2_phases_drill_a_sigkill_crash_then_restart_without_resume(self):
        with tempfile.TemporaryDirectory() as directory:
            fx = build_prepare_fixture(Path(directory))
            first_evidence = self.prepare_first(fx)
            evidence, output = self.prepare_r2(fx, first_evidence)
            phases = parse_phases(output)
            self.assertEqual([len(commands) for _, _, commands in phases],
                             [1, 3, 1, 2, 2, 2, 1, 2, 1, 1])
            prose = [" ".join(lines) for _, lines, _ in phases]
            cmds = [commands for _, _, commands in phases]
            stage_dir = evidence.parent
            # Provision the stage's own store, then arm and start on the pilot rung.
            self.assertTrue(cmds[0][0].endswith(f" -config {stage.quote(stage_dir / 'config.json')} -provision"))
            self.assertEqual(cmds[1][0], f"install -m 600 /dev/null {stage.quote(stage_dir / 'runtime' / 'live_ok')}")
            self.assertIn(" -rung pilot -live > ", cmds[1][1])
            self.assertTrue(cmds[1][1].endswith(f"{stage.quote(evidence / 'harness.log')} 2>&1 &"))
            self.assertIn("operator attestation", prose[1])
            self.assertIn("file hashes", prose[1])
            # Cycle first: three round trips, and a global stop means skipping the drill.
            self.assertEqual(len(cmds[2]), 1)
            self.assertTrue(cmds[2][0].startswith("ps -p "))
            self.assertIn("harness.pid", cmds[2][0])
            self.assertIn("at least three round trips, each an owned fill reduced back to flat", prose[2])
            self.assertIn("pilot rung does not stop on first owned fill", prose[2])
            self.assertIn("global stop latched during the cycles, skip the crash drill", prose[2])
            # The crash itself: SIGKILL, waited in the same shell, with the market quoting flat.
            self.assertTrue(cmds[3][0].startswith("kill -KILL "))
            self.assertIn("harness.pid", cmds[3][0])
            self.assertTrue(cmds[3][1].startswith("wait "))
            self.assertTrue(cmds[3][1].endswith(f"> {stage.quote(evidence / 'exit-status.txt')}"))
            self.assertIn("quoting flat with its adding orders resting", prose[3])
            # The account read before the restart uses the prebuilt tool, not go run.
            self.assertEqual(cmds[4][0], f"cd {stage.quote(fx.repo.resolve() / 'go')}")
            self.assertTrue(cmds[4][1].startswith(f"{stage.quote(stage_dir / 'tools' / 'accountcheck')} "))
            self.assertTrue(cmds[4][1].endswith(f"-out {stage.quote(evidence / 'account-after-crash.json')}"))
            self.assertIn("resting orders the restart must adopt", prose[4])
            self.assertIn("cancel them by hand and follow the runbook", prose[4])
            # A SIGKILL writes no latch, so the restart carries no resume flag anywhere.
            self.assertIn(" -rung pilot -live > ", cmds[5][0])
            self.assertTrue(cmds[5][0].endswith(f"{stage.quote(evidence / 'restart.log')} 2>&1 &"))
            self.assertEqual(cmds[5][1], f"echo $! > {stage.quote(evidence / 'restart.pid')}")
            self.assertIn("no latch file exists", prose[5])
            self.assertNotIn("-resume", output)
            # Observe recovery-only adoption to DRAINED before signaling the restarted child.
            self.assertIn("restart.pid", cmds[6][0])
            for expected in ("adoption of the resting orders", "FUNDING_LIMITS with recovery_only true",
                             "funding_recovery latch", "WINDING_DOWN", "adding orders cancelled",
                             "inventory reduced to flat", "DRAINED with no new adds"):
                self.assertIn(expected, prose[6])
            self.assertTrue(cmds[7][0].startswith("kill -TERM "))
            self.assertIn("restart.pid", cmds[7][0])
            self.assertTrue(cmds[7][1].startswith("wait "))
            self.assertTrue(cmds[7][1].endswith(f"> {stage.quote(evidence / 'restart-exit-status.txt')}"))
            self.assertTrue(cmds[8][0].startswith("go run ./cmd/accountcheck "))
            self.assertTrue(cmds[8][0].endswith(f"-out {stage.quote(evidence / 'account-after-restart.json')}"))
            self.assertEqual(cmds[9], [f"rm -f {stage.quote(stage_dir / 'runtime' / 'live_ok')}"])
            # One SIGKILL for the harness, one SIGTERM for the restart, in that order, and the
            # only go run comes after the restart.
            flat = [command for commands in cmds for command in commands]
            self.assertEqual([c.split(" ", 2)[1] for c in flat if c.startswith("kill ")], ["-KILL", "-TERM"])
            self.assertEqual([number for number, commands in enumerate(cmds)
                              if any(c.startswith("go run ") for c in commands)], [8])

    # lip-e2t: compile time and the book capture must not straddle the freshness window.
    BUILDS = ["build ./cmd/incentives", "build ./cmd/accountcheck"]
    READS = ["read incentives", "read incentives -ticker", "read accountcheck"]

    def command_kinds(self, fx):
        kinds = []
        for command in fx.commands:
            if command[0:2] == ["go", "build"]:
                kinds.append("build " + command[-1])
            elif command[0] == sys.executable:
                kinds.append("snapshot")
            else:
                kinds.append("read " + fx.built[command[0]]
                             + (" -ticker" if command[1:2] == ["-ticker"] else ""))
        return kinds

    def test_prepare_builds_both_tools_before_first_read_and_snapshot(self):
        for with_book in (True, False):
            with self.subTest(with_book=with_book), tempfile.TemporaryDirectory() as directory:
                fx = build_prepare_fixture(Path(directory))
                with contextlib.redirect_stdout(io.StringIO()):
                    evidence = stage.prepare(fx.args("first", with_book=with_book),
                                             runner=fx.runner, clock=lambda: NOW)
                tools = evidence.parent / "tools"
                go_dir = fx.repo.resolve() / "go"
                self.assertEqual(fx.commands[:2], [
                    ["go", "build", "-trimpath", "-o", str(tools / name), f"./cmd/{name}"]
                    for name in ("incentives", "accountcheck")])
                self.assertEqual(fx.cwds[:2], [go_dir, go_dir])
                self.assertEqual(self.command_kinds(fx)[:2], self.BUILDS)
                self.assertFalse(any(k.startswith("build") for k in self.command_kinds(fx)[2:]))
                self.assertFalse(any(command[0:2] == ["go", "run"] for command in fx.commands))
                for command, cwd in zip(fx.commands[2:], fx.cwds[2:]):
                    if command[0] != sys.executable:
                        self.assertEqual(Path(command[0]).parent, tools)
                        self.assertEqual(cwd, go_dir)

    def test_prepare_without_book_snapshots_after_build_before_reads(self):
        with tempfile.TemporaryDirectory() as directory:
            fx = build_prepare_fixture(Path(directory))
            args = fx.args("first", with_book=False)
            self.assertIsNone(args.book)
            with mock.patch.object(stage, "validate_book", wraps=stage.validate_book) as checked, \
                    contextlib.redirect_stdout(io.StringIO()):
                evidence = stage.prepare(args, runner=fx.runner, clock=lambda: NOW)
            self.assertEqual(self.command_kinds(fx), self.BUILDS + ["snapshot"] + self.READS)
            book_dest = evidence / "market-book-series.json"
            self.assertEqual(fx.commands[2], [
                sys.executable, str(fx.repo.resolve() / "scripts" / "public_market_snapshot.py"),
                "--ticker", TICKER, "--out", str(book_dest)])
            self.assertEqual(json.loads(book_dest.read_text()), fx.snapshot_book)
            # First pass plus the final-clock recheck, both on the snapshot output.
            self.assertEqual(checked.call_count, 2)
            for call in checked.call_args_list:
                self.assertEqual(call.args[0], fx.snapshot_book)
            exits = json.loads((evidence / "exit-arithmetic.json").read_text())
            self.assertEqual(exits["exits"]["yes"]["levels"][0]["price_usd"], "0.45")
            identity = json.loads((evidence / "identity.json").read_text())
            self.assertEqual(identity["book_report"], str(book_dest))
            fx.snapshot_book["calls"]["orderbook"]["body"]["orderbook_fp"]["yes_dollars"] = [["0.60", "30"]]
            args = fx.args("first", with_book=False)
            args.evidence_root = str(Path(directory) / "refused")
            with self.assertRaisesRegex(ValueError, r"H-SEL-7 bid_sum 114c"):
                stage.prepare(args, runner=fx.runner, clock=lambda: NOW)

    def test_prepare_with_book_makes_no_snapshot_call(self):
        with tempfile.TemporaryDirectory() as directory:
            fx = build_prepare_fixture(Path(directory))
            with contextlib.redirect_stdout(io.StringIO()):
                evidence = stage.prepare(fx.args("first"), runner=fx.runner, clock=lambda: NOW)
            self.assertEqual(self.command_kinds(fx), self.BUILDS + self.READS)
            self.assertFalse(any("public_market_snapshot.py" in part
                                 for command in fx.commands for part in command))
            self.assertEqual((evidence / "market-book-series.json").read_bytes(), fx.book.read_bytes())
            timing = json.loads((evidence / "prep-timing.json").read_text())
            self.assertEqual(timing["book_source"], f"--book {fx.book.resolve()}")
            self.assertNotIn("book_capture_start", [e["event"] for e in timing["events"]])

    def test_prepare_identity_records_tool_binary_hashes(self):
        with tempfile.TemporaryDirectory() as directory:
            fx = build_prepare_fixture(Path(directory))
            with contextlib.redirect_stdout(io.StringIO()):
                evidence = stage.prepare(fx.args("first", with_book=False),
                                         runner=fx.runner, clock=lambda: NOW)
            identity = json.loads((evidence / "identity.json").read_text())
            tools = evidence.parent / "tools"
            self.assertEqual(identity["tool_binaries"],
                             {name: stage.sha256(tools / name) for name in ("incentives", "accountcheck")})
            for name in ("incentives", "accountcheck"):
                self.assertEqual(identity["tool_binaries"][name],
                                 hashlib.sha256(f"fixture {name} binary".encode()).hexdigest())

    def test_prepare_writes_ordered_prep_timing(self):
        with tempfile.TemporaryDirectory() as directory:
            fx = build_prepare_fixture(Path(directory))
            ticks = [NOW]

            def timer():
                ticks[0] += dt.timedelta(seconds=1)
                return ticks[0]

            with contextlib.redirect_stdout(io.StringIO()):
                evidence = stage.prepare(fx.args("first", with_book=False), runner=fx.runner,
                                         clock=lambda: NOW, timer=timer)
            timing = json.loads((evidence / "prep-timing.json").read_text())
            self.assertEqual([e["event"] for e in timing["events"]], [
                "tool_build_start", "tool_build_end", "book_capture_start", "book_capture_end",
                "programs_read_start", "programs_read_end", "candidate_read_start",
                "candidate_read_end", "account_read_start", "account_read_end", "prepare_end"])
            self.assertEqual([stage.parse_time(e["at_utc"]) for e in timing["events"]],
                             [NOW + dt.timedelta(seconds=i) for i in range(1, 12)])
            self.assertEqual(timing["book_source"], "prepare snapshot")
            self.assertEqual(timing["max_age_seconds"], 60)
            self.assertEqual(stage.parse_time(timing["launch_deadline_utc"]), NOW + dt.timedelta(seconds=60))
            # The default wall-clock timer also yields ordered stamps.
            args = fx.args("first", with_book=False)
            args.evidence_root = str(Path(directory) / "wall")
            with contextlib.redirect_stdout(io.StringIO()):
                evidence = stage.prepare(args, runner=fx.runner, clock=lambda: NOW)
            stamps = [stage.parse_time(e["at_utc"]) for e in
                      json.loads((evidence / "prep-timing.json").read_text())["events"]]
            self.assertEqual(len(stamps), 11)
            self.assertEqual(stamps, sorted(stamps))


if __name__ == "__main__":
    unittest.main()
