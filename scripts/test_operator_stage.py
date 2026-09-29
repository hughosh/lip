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


def account_report():
    stamp = NOW.isoformat().replace("+00:00", "Z")
    paths = ["/portfolio/balance", "/portfolio/subaccounts/balances",
             f"/markets/{TICKER}", "/portfolio/orders", "/portfolio/positions",
             "/portfolio/fills"]
    return {
        "started_at": stamp, "ended_at": stamp,
        "requests": [{"at": stamp, "path": p, "status": 200} for p in paths],
        "aggregate_balance_status": {"outcome": "complete"},
        "subaccount_balances_status": {"outcome": "complete"},
        "selected_ticker": TICKER,
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


def programs_report(selected=True):
    stamp = NOW.isoformat().replace("+00:00", "Z")
    out = {
        "complete": True, "pages": 1, "started_at_utc": stamp,
        "completed_at_utc": stamp,
        "programs": [{"market_ticker": TICKER, "incentive_type": "liquidity",
                      "start_date": "2026-09-26T00:00:00Z",
                      "end_date": "2026-09-28T00:00:00Z"}],
    }
    if selected:
        out["selected"] = {
            "ticker": TICKER,
            "market": {"ticker": TICKER, "status": "active"},
            "event": {"event_ticker": "KXTEST"},
            "series": {"ticker": "KXTEST"},
        }
    return out


def book_report():
    stamp = NOW.isoformat()
    return {"ticker": TICKER, "read_only": True, "completed_at_utc": stamp, "calls": {
        "market": {"path": f"/markets/{TICKER}", "at_utc": stamp, "status": 200, "body": {"market": {
            "ticker": TICKER, "status": "active", "event_ticker": "KXTEST",
            "price_level_structure": "linear_cent"}}},
        "orderbook": {"path": f"/markets/{TICKER}/orderbook", "at_utc": stamp, "status": 200, "body": {"orderbook_fp": {
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
    """The pre-lip-8q0 operator block: its print statements copied verbatim."""
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
    else:
        print("  # Start only after a human adjudicates complete account truth and clears the existing latch; this helper never clears it.")
        print("  # R2 operator attestation accepted; artifact contents and claimed events remain unverified by this helper.")
        print("  # The pilot rung does not stop on first owned fill.")
    print("  # After verifying every stage process has exited and account truth is complete and flat:")
    print(f"  rm -f {quote(config['paths']['live_ok'])}  # only this stage's write sentinel; retain store, latch and evidence")
    return out


def legacy_commands(lines):
    """Old indented lines minus standalone comments and inline '  # ...' suffixes."""
    return [line[2:].split("  # ", 1)[0] for line in lines
            if line.startswith("  ") and not line[2:].startswith("#")]


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
    # What the in-prepare snapshot writes; distinct from book.json (best yes bid 45c).
    snapshot_book = book_report()
    snapshot_book["note"] = "captured inside prepare"
    snapshot_book["calls"]["orderbook"]["body"]["orderbook_fp"]["yes_dollars"] = [["0.45", "30"]]

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
        if tool == "incentives" and len(command) == 1:
            return subprocess.CompletedProcess(command, 0, json.dumps(programs_report(False)), "")
        if tool == "incentives":
            return subprocess.CompletedProcess(command, 0, json.dumps(programs_report()), "")
        if tool == "accountcheck":
            output = Path(command[command.index("-out") + 1])
            output.write_text(json.dumps(account_report()))
            return subprocess.CompletedProcess(command, 0, "", "accountcheck report written")
        raise AssertionError(f"unexpected command: {command}")

    def args(stage_name, *extra, with_book=True):
        book_args = ["--book", str(book)] if with_book else []
        return stage.parser().parse_args([
            "--stage", stage_name, "--ticker", TICKER, "--size", "2.5",
            "--binary", str(binary), *book_args,
            "--candidate-receipt", str(build_receipt),
            "--evidence-root", str(evidence_root), "--repo", str(repo), *extra,
        ])

    return types.SimpleNamespace(repo=repo, binary=binary, source_hash=source_hash,
                                 commands=commands, cwds=cwds, runner=fake_runner, args=args,
                                 book=book, snapshot_book=snapshot_book, built=built)


def write_r2_receipt(fx, first_evidence):
    """An R2 receipt bound to the first stage that prepare() just produced."""
    directory = first_evidence.parent.parent / "r2-receipt"
    directory.mkdir()
    proof = directory / "proof.json"
    proof.write_text('{"observed":true}')
    identity_sha = stage.sha256(first_evidence / "identity.json")
    receipt = {"candidate": {"ticker": TICKER, "source_manifest_sha256": fx.source_hash,
                             "binary_sha256": stage.sha256(fx.binary),
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
            stage.load_prior_stage(config_path, TICKER, "src", binary, stage.sha256(binary))
            for bad in (str(Path(directory) / "old.db"), "../../outside.db"):
                config["paths"]["db"] = bad
                config_path.write_text(json.dumps(config))
                identity["config_sha256"] = stage.sha256(config_path)
                (base / "evidence" / "identity.json").write_text(json.dumps(identity))
                with self.assertRaisesRegex(ValueError, "db path"):
                    stage.load_prior_stage(config_path, TICKER, "src", binary, stage.sha256(binary))
            config["paths"]["db"] = str(base / "runtime" / "harness.db")
            config_path.write_text(json.dumps(config))
            identity["config_sha256"] = stage.sha256(config_path)
            (base / "evidence" / "identity.json").write_text(json.dumps(identity))
            (base / "runtime" / "harness.db").symlink_to(binary)
            with self.assertRaisesRegex(ValueError, "db path"):
                stage.load_prior_stage(config_path, TICKER, "src", binary, stage.sha256(binary))

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
        legacy = legacy_operator_lines(
            args, config, exe=stage.quote(fx.binary.resolve()),
            cfg=stage.quote(evidence.parent / "config.json"), evidence=evidence,
            repo=fx.repo.resolve(), ticker=TICKER)
        expected = legacy_commands(legacy)
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
        self.assertEqual(len(phases), 10 if args.stage == "first" else 5)
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
                           "--r2-receipt", str(write_r2_receipt(fx, first_evidence)))
            with contextlib.redirect_stdout(io.StringIO()) as output:
                evidence = stage.prepare(args, runner=fx.runner, clock=lambda: NOW)
            self.assertEqual(json.loads((evidence.parent / "config.json").read_text())["rung"], "pilot")
            self.assertTrue(all("-live" not in command for command in fx.commands))
            self.assert_paste_safe_phases(args, output.getvalue(), evidence, fx)

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
