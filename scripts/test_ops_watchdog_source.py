"""Read-only source adapter checks against the actual hstore SQLite schema."""

from contextlib import closing
from pathlib import Path
import re
import sqlite3
import tempfile
import unittest

from scripts import ops_watchdog as watchdog
from scripts import ops_watchdog_source as source


T0 = 1_700_000_000_000
SCHEMA_GO = Path(__file__).resolve().parents[1] / "go/harness/hstore/schema.go"


class SourceAdapterTest(unittest.TestCase):
    def setUp(self):
        temp = tempfile.TemporaryDirectory()
        self.addCleanup(temp.cleanup)
        self.root = Path(temp.name)
        self.db_path = self.root / "harness.db"
        self.state_path = self.root / "watchdog.json"
        watchdog.init(self.state_path)
        schema = re.search(r"const Schema = `([\s\S]*?)`", SCHEMA_GO.read_text())
        self.assertIsNotNone(schema)
        with closing(sqlite3.connect(self.db_path)) as db, db:
            db.executescript(schema.group(1))
            db.execute("PRAGMA user_version = 3")
            db.execute("INSERT INTO run VALUES (?, ?, ?)", ("run-A", T0, b"{}"))
            self._insert(db, "sev1-a", 2, "FOREIGN_FILL", "KX-A", "unmanaged fill", 10, None)
            self._insert(db, "sev2", 1, "READ_THROTTLED", "", "managed", 20, None)
            self._insert(db, "sev1-b", 2, "DISK_HEADROOM", "", "storage unsafe", 30, T0 + 30)
        self.source_id = "desk-one"

    def _insert(self, db, anomaly_id, sev, klass, ticker, detail, offset, journaled):
        db.execute(
            "INSERT INTO anomaly (anomaly_id, run_id, class, sev, ticker, text, "
            "first_ms, journaled_ms, delivered_ms, attempts, last_attempt_ms, "
            "suppressed_count) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 0, NULL, 0)",
            (anomaly_id, "run-A", klass, sev, ticker, detail, T0 + offset,
             journaled, T0 + offset if anomaly_id == "sev1-b" else None),
        )

    def test_imports_committed_sev1_even_if_unjournaled_or_delivered(self):
        before = self.db_path.read_bytes()
        result = source.run_once(self.state_path, self.db_path, self.source_id, T0 + 100)
        self.assertEqual((result["scanned"], result["ingested"]), (2, 2))
        self.assertEqual(self.db_path.read_bytes(), before)
        state = watchdog.status(self.state_path)
        self.assertEqual(len(state["incidents"]), 2)
        self.assertTrue(all(item["ack"] is None for item in state["incidents"].values()))
        self.assertEqual(source.run_once(self.state_path, self.db_path, self.source_id,
                                         T0 + 200)["ingested"], 0)
        self.assertEqual(len(watchdog.status(self.state_path)["events"]), 2)

    def test_mutable_journal_and_delivery_fields_do_not_change_event(self):
        source.run_once(self.state_path, self.db_path, self.source_id, T0 + 100)
        with closing(sqlite3.connect(self.db_path)) as db, db:
            db.execute("UPDATE anomaly SET journaled_ms=?, delivered_ms=?, attempts=1 "
                       "WHERE anomaly_id='sev1-a'", (T0 + 101, T0 + 102))
        self.assertEqual(source.run_once(self.state_path, self.db_path, self.source_id,
                                         T0 + 200)["ingested"], 0)

    def test_missing_or_corrupt_source_persists_outage_and_recovery_does_not_ack(self):
        self.db_path.rename(self.root / "saved.db")
        first = source.run_once(self.state_path, self.db_path, self.source_id, T0 + 100)
        self.assertFalse(first["source_available"])
        self.assertTrue(first["source_unavailable_ingested"])
        self.assertFalse(source.run_once(self.state_path, self.db_path, self.source_id,
                                         T0 + 200)["source_unavailable_ingested"])
        self.assertEqual(len(watchdog.status(self.state_path)["incidents"]), 1)
        (self.root / "saved.db").rename(self.db_path)
        recovered = source.run_once(self.state_path, self.db_path, self.source_id,
                                    T0 + 300)
        self.assertTrue(recovered["source_recovery_ingested"])
        state = watchdog.status(self.state_path)
        outage_id = source._outage_ids(self.source_id, 1)[1]
        self.assertIsNone(state["incidents"][outage_id]["ack"])
        self.db_path.write_bytes(b"not a SQLite database")
        second = source.run_once(self.state_path, self.db_path, self.source_id,
                                 T0 + 400)
        self.assertFalse(second["source_available"])
        self.assertIn(source._outage_ids(self.source_id, 2)[1],
                      watchdog.status(self.state_path)["incidents"])

    def test_source_identity_separates_incidents(self):
        source.run_once(self.state_path, self.db_path, "desk-one", T0 + 100)
        source.run_once(self.state_path, self.db_path, "desk-two", T0 + 200)
        self.assertEqual(len(watchdog.status(self.state_path)["incidents"]), 4)

    def test_missing_source_does_not_create_database(self):
        missing = self.root / "absent.db"
        result = source.run_once(self.state_path, missing, self.source_id, T0 + 100)
        self.assertFalse(result["source_available"])
        self.assertFalse(missing.exists())

    def test_wal_without_sidecars_refuses_before_readonly_open(self):
        with closing(sqlite3.connect(self.db_path)) as db:
            self.assertEqual(db.execute("PRAGMA journal_mode=WAL").fetchone()[0], "wal")
        wal = Path(str(self.db_path) + "-wal")
        shm = Path(str(self.db_path) + "-shm")
        self.assertFalse(wal.exists())
        self.assertFalse(shm.exists())
        result = source.run_once(self.state_path, self.db_path, self.source_id, T0 + 100)
        self.assertFalse(result["source_available"])
        self.assertFalse(wal.exists())
        self.assertFalse(shm.exists())

    def test_live_wal_reads_committed_sev1_before_checkpoint(self):
        writer = sqlite3.connect(self.db_path)
        try:
            self.assertEqual(writer.execute("PRAGMA journal_mode=WAL").fetchone()[0], "wal")
            self._insert(writer, "sev1-live", 2, "FOREIGN_ORDER", "KX-B",
                         "live WAL anomaly", 40, None)
            writer.commit()
            self.assertTrue(Path(str(self.db_path) + "-wal").exists())
            self.assertTrue(Path(str(self.db_path) + "-shm").exists())
            result = source.run_once(self.state_path, self.db_path, self.source_id,
                                     T0 + 100)
            self.assertTrue(result["source_available"])
            self.assertEqual(result["scanned"], 3)
            self.assertEqual(result["ingested"], 3)
        finally:
            writer.close()


if __name__ == "__main__":
    unittest.main()
