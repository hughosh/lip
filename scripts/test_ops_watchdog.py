import contextlib
import io
import json
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
import tempfile
import threading
import unittest
from unittest import mock

from scripts import ops_watchdog as w


PRIMARY = "https://primary.example/incident"
BACKUP = "https://backup.example/incident"
T0 = 1_700_000_000_000


def event(event_id="event-1", incident_id="incident-1", severity="SEV1"):
    return {"event_id": event_id, "incident_id": incident_id,
            "source": "local-test", "severity": severity, "message": "disk unsafe"}


class WatchdogTest(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.path = Path(self.tmp.name) / "incidents.json"
        w.init(self.path)

    def ingest(self):
        self.assertTrue(w.ingest(self.path, event(), T0))

    def test_primary_nonack_pages_backup_at_deadline(self):
        self.ingest()
        calls = []
        sender = lambda url, payload: calls.append((url, payload["incident_id"], payload["role"])) or True
        self.assertEqual([x["role"] for x in w.tick(self.path, PRIMARY, BACKUP, 300_000, T0, sender)], ["primary"])
        self.assertEqual(w.tick(self.path, PRIMARY, BACKUP, 300_000, T0 + 299_999, sender), [])
        self.assertEqual([x["role"] for x in w.tick(self.path, PRIMARY, BACKUP, 300_000, T0 + 300_000, sender)], ["backup"])
        self.assertEqual(calls, [(PRIMARY, "incident-1", "primary"), (BACKUP, "incident-1", "backup")])

    def test_primary_only_has_no_backup_attempt_or_automatic_ack(self):
        self.ingest()
        calls = []
        sender = lambda url, payload: calls.append((url, payload["role"])) or True
        first = w.tick(self.path, PRIMARY, None, 300_000, T0, sender)
        self.assertEqual([(x["role"], x["backup_unconfigured"]) for x in first],
                         [("primary", True)])
        self.assertEqual(w.tick(self.path, PRIMARY, None, 300_000, T0 + 300_000, sender), [])
        self.assertEqual(calls, [(PRIMARY, "primary")])
        item = w.status(self.path)["incidents"]["incident-1"]
        self.assertTrue(item["backup"]["unconfigured"])
        self.assertEqual(item["backup"]["attempts"], 0)
        self.assertIsNone(item["ack"])
        # A later explicit two-route tick can still page the backup at the
        # original deadline, even after a process reload.
        later = w.tick(self.path, PRIMARY, BACKUP, 300_000, T0 + 300_001, sender)
        self.assertEqual([x["role"] for x in later], ["backup"])
        self.assertFalse(w.status(self.path)["incidents"]["incident-1"]["backup"]["unconfigured"])
        self.assertEqual(calls[-1], (BACKUP, "backup"))

    def test_primary_only_failed_send_retries_from_durable_state(self):
        self.ingest()
        calls = []
        sender = lambda url, payload: calls.append((url, payload["role"])) or False
        self.assertFalse(w.tick(self.path, PRIMARY, None, 300_000, T0, sender)[0]["transport_accepted"])
        self.assertEqual(w.tick(self.path, PRIMARY, None, 300_000, T0 + 29_999, sender), [])
        retry = w.tick(self.path, PRIMARY, None, 300_000, T0 + 300_000, sender)
        self.assertEqual([x["role"] for x in retry], ["primary"])
        self.assertEqual(calls, [(PRIMARY, "primary"), (PRIMARY, "primary")])
        self.assertEqual(w.status(self.path)["incidents"]["incident-1"]["primary"]["attempts"], 2)

    def test_cli_requires_explicit_route_mode_and_reports_primary_only(self):
        self.ingest()
        args = ["--state", str(self.path), "tick", "--primary-url", PRIMARY]
        for suffix in ([], ["--backup-url", BACKUP, "--primary-only"]):
            with contextlib.redirect_stderr(io.StringIO()):
                with self.assertRaises(SystemExit) as raised:
                    w.main(args + suffix)
            self.assertEqual(raised.exception.code, 2)
        out = io.StringIO()
        with mock.patch.object(w, "https_send", return_value=True), \
             mock.patch.object(w.time, "time", return_value=T0 / 1000), \
             contextlib.redirect_stdout(out):
            self.assertEqual(w.main(args + ["--primary-only"]), 0)
        result = json.loads(out.getvalue())
        self.assertTrue(result["backup_unconfigured"])
        self.assertEqual([x["role"] for x in result["attempts"]], ["primary"])
        self.assertIsNone(w.status(self.path)["incidents"]["incident-1"]["ack"])

    def test_primary_ack_suppresses_backup_and_recovery_does_not_resolve(self):
        self.ingest()
        w.tick(self.path, PRIMARY, BACKUP, 300_000, T0, lambda *_: True)
        self.assertTrue(w.ingest(self.path, event("recovered", severity="RECOVERY"), T0 + 10_000))
        self.assertIsNone(w.status(self.path)["incidents"]["incident-1"]["ack"])
        self.assertTrue(w.ack(self.path, "incident-1", "primary", "operator-A", "provider-receipt-1", T0 + 20_000))
        self.assertEqual(w.tick(self.path, PRIMARY, BACKUP, 300_000, T0 + 300_000, lambda *_: self.fail("sent after ack")), [])

    def test_backup_ack_and_conflicting_ack_refused(self):
        self.ingest()
        w.tick(self.path, PRIMARY, BACKUP, 300_000, T0 + 300_000, lambda *_: True)
        self.assertTrue(w.ack(self.path, "incident-1", "backup", "operator-B", "provider-receipt-2", T0 + 301_000))
        self.assertFalse(w.ack(self.path, "incident-1", "backup", "operator-B", "provider-receipt-2", T0 + 302_000))
        with self.assertRaises(w.StateError):
            w.ack(self.path, "incident-1", "primary", "operator-A", "different", T0 + 303_000)

    def test_failed_primary_still_pages_backup_and_retries(self):
        self.ingest()
        calls = []
        def sender(url, payload):
            calls.append(payload["role"])
            return url == BACKUP
        w.tick(self.path, PRIMARY, BACKUP, 300_000, T0, sender)
        self.assertEqual(w.tick(self.path, PRIMARY, BACKUP, 300_000, T0 + 29_999, sender), [])
        w.tick(self.path, PRIMARY, BACKUP, 300_000, T0 + 30_000, sender)
        result = w.tick(self.path, PRIMARY, BACKUP, 300_000, T0 + 300_000, sender)
        self.assertEqual([x["role"] for x in result], ["primary", "backup"])
        self.assertEqual(calls, ["primary", "primary", "primary", "backup"])

    def test_restart_preserves_schedule_and_duplicate_event(self):
        self.ingest()
        self.assertFalse(w.ingest(self.path, event(), T0 + 10_000))
        w.tick(self.path, PRIMARY, BACKUP, 300_000, T0, lambda *_: True)
        self.assertEqual(len(w.status(self.path)["events"]), 1)
        # Each call reloads from disk, representing a fresh process.
        result = w.tick(self.path, PRIMARY, BACKUP, 300_000, T0 + 300_000, lambda *_: True)
        self.assertEqual([x["role"] for x in result], ["backup"])
        with self.assertRaises(w.StateError):
            w.ingest(self.path, event(incident_id="another"), T0 + 1)
        altered = event()
        altered["message"] = "changed message"
        with self.assertRaises(w.StateError):
            w.ingest(self.path, altered, T0 + 1)

    def test_failed_durable_write_prevents_external_send(self):
        self.ingest()
        with mock.patch.object(w, "save", side_effect=OSError("disk full")):
            with self.assertRaises(OSError):
                w.tick(self.path, PRIMARY, BACKUP, 300_000, T0,
                       lambda *_: self.fail("sent before durable attempt"))

    def test_clock_rewind_pages_backup_and_retries_failed_primary(self):
        self.ingest()
        w.tick(self.path, PRIMARY, BACKUP, 300_000, T0, lambda *_: False)
        result = w.tick(self.path, PRIMARY, BACKUP, 300_000, T0 - 3_300_000,
                        lambda *_: True)
        self.assertEqual([x["role"] for x in result], ["primary", "backup"])
        self.assertTrue(all(x["clock_regressed"] for x in result))
        # Reloaded state retains delivery dedup even while wall remains behind.
        self.assertEqual(w.tick(self.path, PRIMARY, BACKUP, 300_000,
                                T0 - 3_299_000, lambda *_: self.fail("duplicate")), [])

    def test_corrupt_or_missing_state_refuses_all_mutations(self):
        self.ingest()
        self.path.write_text("{broken", encoding="utf-8")
        with self.assertRaises(w.StateError):
            w.tick(self.path, PRIMARY, BACKUP, 300_000, T0, lambda *_: self.fail("sent with corrupt state"))
        with self.assertRaises(w.StateError):
            w.ack(self.path, "incident-1", "primary", "operator-A", "receipt", T0)
        self.path.unlink()
        with self.assertRaises(w.StateError):
            w.ingest(self.path, event("event-2"), T0)

    def test_endpoints_must_be_distinct_https(self):
        self.ingest()
        for primary, backup in (("http://primary.example", BACKUP), (PRIMARY, PRIMARY),
                                ("https://user:pass@primary.example", BACKUP)):
            with self.assertRaises(w.StateError):
                w.tick(self.path, primary, backup, 300_000, T0, lambda *_: self.fail("invalid endpoint used"))

    def test_atomic_state_is_private_json(self):
        self.ingest()
        self.assertEqual(self.path.stat().st_mode & 0o777, 0o600)
        self.assertEqual(json.loads(self.path.read_text())["events"]["event-1"]["incident_id"], "incident-1")

    def test_local_http_receiver_ack_nonack_backup_failure_and_restart(self):
        # Loopback HTTP is confined to this test. Production tick still requires
        # distinct HTTPS endpoints; bypass only that validation in the fixture.
        received = []
        class Receiver(BaseHTTPRequestHandler):
            def do_POST(self):
                payload = json.loads(self.rfile.read(int(self.headers["Content-Length"])))
                received.append((self.path, payload))
                self.send_response(503 if self.path == "/fail-primary" else 202)
                self.end_headers()

            def log_message(self, *_):
                pass

        server = ThreadingHTTPServer(("127.0.0.1", 0), Receiver)
        thread = threading.Thread(target=server.serve_forever, daemon=True)
        thread.start()
        self.addCleanup(thread.join, timeout=2)
        self.addCleanup(server.server_close)
        self.addCleanup(server.shutdown)
        base = f"http://127.0.0.1:{server.server_port}"
        with mock.patch.object(w, "_endpoint", side_effect=lambda url: url):
            w.ingest(self.path, event("ack-event", "ack-incident"), T0)
            sent = w.tick(self.path, base + "/primary", base + "/backup", 300_000, T0)
            self.assertEqual([(x["role"], x["transport_accepted"]) for x in sent], [("primary", True)])
            self.assertIsNone(w.status(self.path)["incidents"]["ack-incident"]["ack"])
            # This is a synthetic fixture assertion, not an external human ack.
            w.ack(self.path, "ack-incident", "primary", "fixture-operator", "fixture-response", T0 + 1)
            w.ingest(self.path, event("nonack-event", "nonack-incident"), T0 + 2)
            w.tick(self.path, base + "/primary", base + "/backup", 300_000, T0 + 2)
            self.assertIsNone(w.status(self.path)["incidents"]["nonack-incident"]["ack"])
            self.assertEqual(w.tick(self.path, base + "/primary", base + "/backup", 300_000,
                                    T0 + 300_001), [])
            # Each tick reloads durable state, including this post-restart call.
            escalated = w.tick(self.path, base + "/primary", base + "/backup", 300_000,
                               T0 + 300_002)
            self.assertEqual([(x["incident_id"], x["role"]) for x in escalated],
                             [("nonack-incident", "backup")])
            w.ingest(self.path, event("failed-event", "failed-incident"), T0 + 400_000)
            failed = w.tick(self.path, base + "/fail-primary", base + "/backup", 300_000,
                            T0 + 400_000)
            self.assertEqual([(x["role"], x["transport_accepted"]) for x in failed],
                             [("primary", False)])
            backup = w.tick(self.path, base + "/fail-primary", base + "/backup", 300_000,
                            T0 + 700_000)
            self.assertIn({"incident_id": "failed-incident", "role": "backup",
                           "transport_accepted": True, "clock_regressed": False}, backup)
        self.assertEqual(sum(path == "/backup" for path, _ in received), 2)
        self.assertTrue(all(payload["incident_id"] for _, payload in received))


if __name__ == "__main__":
    unittest.main()
