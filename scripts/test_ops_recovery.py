"""Account-free unusable-store recovery preflight fixtures."""

from pathlib import Path
import copy
from contextlib import redirect_stdout
from io import StringIO
import json
import tempfile
import unittest

from scripts import ops_recovery


class RecoveryPreflightTest(unittest.TestCase):
    def setUp(self):
        temp = tempfile.TemporaryDirectory()
        self.addCleanup(temp.cleanup)
        self.root = Path(temp.name)
        self.db = self.root / "harness.db"
        self.journal = self.root / "anomalies.jsonl"
        self.latch = self.root / "halt.latch"
        self.journal.write_text('existing alarm\n')
        self.latch.write_text('stopped\n')

    def test_missing_zero_and_corrupt_store_stay_blocked_and_unchanged(self):
        for content, expected in ((None, "missing"), (b"", "zero_length"),
                                  (b"broken ledger", "invalid_header")):
            with self.subTest(expected=expected):
                if content is None:
                    self.db.unlink(missing_ok=True)
                else:
                    self.db.write_bytes(content)
                result = ops_recovery.inspect(self.db, self.journal, self.latch)
                self.assertEqual(result["store_condition"], expected)
                self.assertTrue(result["recovery_status"].startswith("BLOCKED"))
                self.assertEqual(self.db.read_bytes() if content is not None else None, content)
                self.assertEqual(self.journal.read_text(), 'existing alarm\n')
                self.assertEqual(self.latch.read_text(), 'stopped\n')

    def test_restored_header_does_not_claim_integrity_or_flatness(self):
        self.db.write_bytes(b"SQLite format 3\x00" + b"\x00" * 84)
        result = ops_recovery.inspect(self.db, self.journal, self.latch)
        self.assertEqual(result["store_condition"], "header_present_integrity_unverified")
        self.assertIn("independent account-wide orders, fills and positions",
                      " ".join(result["required_evidence"]))
        self.assertTrue(result["recovery_status"].startswith("BLOCKED"))

    def test_paths_must_be_explicit_absolute(self):
        with self.assertRaises(ops_recovery.RecoveryError):
            ops_recovery.inspect(Path("harness.db"), self.journal, self.latch)


class RecoveryPlanTest(unittest.TestCase):
    def setUp(self):
        self.owned = {"coid": "owned-1", "order_id": "order-1", "ticker": "TEST-A",
                      "side": "yes"}
        self.foreign = {"coid": "owned-1-prefix-foreign", "order_id": "order-2",
                        "ticker": "TEST-B", "side": "no"}
        self.snapshot = {
            "scope": "account_wide", "independent": True, "source_ref": "fixture/account-1",
            "observed_at": "2026-09-27T01:00:00Z",
            "complete": {"open_orders": True, "fills": True, "held_positions": True},
            "open_orders": [self.owned, self.foreign], "fills": [self.owned],
            "held_positions": [{"ticker": "TEST-A", "side": "yes"}],
        }
        self.ownership = {"source_ref": "fixture/retained-order-ledger",
                          "owned_orders": [self.owned]}

    def test_corrupt_store_owned_foreign_and_held_drill_then_flat_report(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            db, journal, latch = (root / name for name in
                                  ("harness.db", "anomalies.jsonl", "halt.latch"))
            db.write_bytes(b"corrupt fixture ledger")
            journal.write_text("fixture alarm\n")
            latch.write_text("fixture stopped\n")
            final = copy.deepcopy(self.snapshot)
            final["observed_at"] = "2026-09-27T01:05:00Z"
            final["source_ref"] = "fixture/account-final"
            final["open_orders"] = []
            final["held_positions"] = []
            for name, body in (("snapshot.json", self.snapshot),
                               ("ownership.json", self.ownership), ("final.json", final)):
                (root / name).write_text(json.dumps(body))
            self.assertEqual(ops_recovery.inspect(db, journal, latch)["store_condition"],
                             "invalid_header")
            output_text = StringIO()
            with redirect_stdout(output_text):
                exit_code = ops_recovery.main([
                    "--db", str(db), "--journal", str(journal), "--latch", str(latch),
                    "--snapshot", str(root / "snapshot.json"),
                    "--ownership", str(root / "ownership.json"),
                    "--final-snapshot", str(root / "final.json")])
            self.assertEqual(exit_code, 0)
            command_result = json.loads(output_text.getvalue())
            self.assertEqual(command_result["store_condition"], "invalid_header")
            self.assertEqual(command_result["plan"]["status"],
                             "OFFLINE_PLAN_VALIDATED_REAL_RECOVERY_BLOCKED")
            output = ops_recovery.plan(self.snapshot, self.ownership, final)
            self.assertEqual(output["owned_only_cancellation_identities"], [self.owned])
            self.assertEqual(output["excluded_unproven_open_orders"], [self.foreign])
            self.assertEqual(output["held_position_reduction_obligations"], [
                {"ticker": "TEST-A", "side": "yes",
                 "size": "UNKNOWN_REQUIRES_FRESH_ACCOUNT_TRUTH"}])
            self.assertTrue(output["final_snapshot_evidence"]["later_complete_flat_snapshot"])
            self.assertIn("REAL_RECOVERY_BLOCKED", output["status"])
            self.assertIn("no recovery or human acknowledgment attested",
                          output["final_snapshot_evidence"]["meaning"])
            self.assertEqual(db.read_bytes(), b"corrupt fixture ledger")
            self.assertEqual(journal.read_text(), "fixture alarm\n")
            self.assertEqual(latch.read_text(), "fixture stopped\n")

    def test_missing_or_contradictory_ownership_is_rejected(self):
        for change in (lambda x: x["owned_orders"][0].pop("order_id"),
                       lambda x: x["owned_orders"][0].update(order_id="wrong"),
                       lambda x: x["owned_orders"][0].update(side="no")):
            evidence = copy.deepcopy(self.ownership)
            change(evidence)
            with self.assertRaises(ops_recovery.RecoveryError):
                ops_recovery.plan(self.snapshot, evidence)

    def test_incomplete_or_nonindependent_snapshot_is_rejected(self):
        for change in (lambda x: x["complete"].update(fills=False),
                       lambda x: x.update(independent=False),
                       lambda x: x.update(scope="selected_market")):
            snapshot = copy.deepcopy(self.snapshot)
            change(snapshot)
            with self.assertRaises(ops_recovery.RecoveryError):
                ops_recovery.plan(snapshot, self.ownership)

    def test_position_without_owned_fill_remains_unproven(self):
        snapshot = copy.deepcopy(self.snapshot)
        snapshot["fills"] = []
        result = ops_recovery.plan(snapshot, self.ownership)
        self.assertEqual(result["held_position_reduction_obligations"], [])
        self.assertEqual(result["unproven_held_positions"],
                         [{"ticker": "TEST-A", "side": "yes"}])

    def test_final_must_be_later_and_complete(self):
        final = copy.deepcopy(self.snapshot)
        final["open_orders"] = []
        final["held_positions"] = []
        with self.assertRaises(ops_recovery.RecoveryError):
            ops_recovery.plan(self.snapshot, self.ownership, final)
        final["observed_at"] = "2026-09-27T01:05:00Z"
        final["complete"]["open_orders"] = False
        with self.assertRaises(ops_recovery.RecoveryError):
            ops_recovery.plan(self.snapshot, self.ownership, final)


if __name__ == "__main__":
    unittest.main()
