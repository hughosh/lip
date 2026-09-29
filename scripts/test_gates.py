"""Contract tests for local gate orchestration; no real gate command runs."""
from __future__ import annotations

from contextlib import redirect_stdout, redirect_stderr
from io import StringIO
import json
from pathlib import Path
import tempfile
from types import SimpleNamespace
import unittest
from unittest.mock import patch

try:
    from scripts import run_gates
except ModuleNotFoundError:  # Direct script invocation avoids repo select.py shadowing stdlib.
    import run_gates


class TestModes(unittest.TestCase):
    def test_mode_composition(self):
        names = lambda mode: [stage[0] for stage in run_gates.stages(mode)]
        self.assertEqual(names('static'), ['check.py', 'gate-tests', 'isolation-tests', 'catalogue-anchors'])
        self.assertEqual(names('quick'), ['check.py', 'gate-tests', 'isolation-tests', 'catalogue-anchors', 'build',
                                          'vet', 'gofmt', 'test-module'])
        self.assertEqual(names('race'), names('quick') + ['test-race'])
        self.assertEqual(names('audit'), names('race') + ['negative-control'])
        self.assertEqual(names('candidate'), names('race') + ['safety-mutations'])
        for mode in ('static', 'quick', 'race', 'candidate'):
            self.assertNotIn('negative-control', names(mode))
        for mode in ('quick', 'static'):
            self.assertNotIn('test-race', names(mode))
        for mode in ('quick', 'race', 'candidate', 'audit'):
            self.assertFalse(any('--build-only' in command
                                 for _, _, command in run_gates.stages(mode)))

    def test_default_is_fast_and_release_requires_an_explicit_flag(self):
        with patch.object(run_gates, 'run', return_value=0) as runner:
            self.assertEqual(run_gates.main([]), 0)
            runner.assert_called_once_with('quick')
        with patch.object(run_gates, 'run', return_value=0) as runner:
            self.assertEqual(run_gates.main(['--release']), 0)
            runner.assert_called_once_with('candidate')

    def test_invalid_args_never_run(self):
        for args in (['--unknown'], ['--quick', '--race'], ['--quick', 'x']):
            with self.subTest(args=args), patch.object(run_gates, 'run') as runner:
                with redirect_stderr(StringIO()):
                    self.assertEqual(run_gates.main(args), 2)
                runner.assert_not_called()


class TestReceipts(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        (self.root / 'go').mkdir()
        (self.root / 'scripts').mkdir()
        (self.root / 'scripts/test.go').write_text('package fixture\n')
        self.calls = []

    def fake_command(self, command, **kwargs):
        self.calls.append(command)
        kwargs['stdout'].write(b'mock command output\n')
        return SimpleNamespace(returncode=0)

    def receipt(self):
        folders = sorted((self.root / 'loop/gates-out').iterdir())
        current = [folder for folder in folders if (folder / 'receipt.json').exists()]
        return folders, json.loads((current[-1] / 'receipt.json').read_text())

    def test_quick_pass_preserves_old_logs_and_cannot_claim_release(self):
        old = self.root / 'loop/gates-out/prior-run'
        old.mkdir(parents=True)
        (old / 'build.log').write_text('keep me')
        with patch.object(run_gates.subprocess, 'run', side_effect=self.fake_command):
            with redirect_stdout(StringIO()) as stdout:
                self.assertEqual(run_gates.run('quick', self.root), 0)
        folders, receipt = self.receipt()
        self.assertEqual(len(folders), 2)
        self.assertEqual((old / 'build.log').read_text(), 'keep me')
        self.assertFalse(receipt['release_checks_complete'])
        self.assertFalse(receipt['live_eligible'])
        self.assertFalse(receipt['advance_eligible'])
        self.assertEqual(len(receipt['steps']), 8)
        self.assertIn('RELEASE_CHECKS: REVIEW_REQUIRED', stdout.getvalue())
        self.assertTrue(all(Path(s['log']).exists() and s['exit_code'] == 0
                            for s in receipt['steps']))

    def test_failure_stops_before_expensive_stages(self):
        def fail_first(command, **kwargs):
            self.calls.append(command)
            kwargs['stdout'].write(b'explanatory failure\n')
            return SimpleNamespace(returncode=17)
        with patch.object(run_gates.subprocess, 'run', side_effect=fail_first):
            with redirect_stdout(StringIO()), redirect_stderr(StringIO()):
                self.assertEqual(run_gates.run('audit', self.root), 1)
        _, receipt = self.receipt()
        self.assertEqual(len(self.calls), 1)
        self.assertEqual(receipt['steps'][0]['exit_code'], 17)
        self.assertFalse(receipt['release_checks_complete'])
        self.assertIn('explanatory failure', Path(receipt['steps'][0]['log']).read_text())

    def test_source_drift_stops_and_records_fingerprints(self):
        def change_source(command, **kwargs):
            kwargs['stdout'].write(b'ok\n')
            (self.root / 'scripts/new_untracked.go').write_text('package changed\n')
            return SimpleNamespace(returncode=0)
        with patch.object(run_gates.subprocess, 'run', side_effect=change_source):
            with redirect_stdout(StringIO()), redirect_stderr(StringIO()):
                self.assertEqual(run_gates.run('static', self.root), 1)
        _, receipt = self.receipt()
        self.assertEqual(receipt['source_drift_detected_after'], 'check.py')
        self.assertNotEqual(receipt['source_sha256_before'], receipt['source_sha256_after'])
        self.assertEqual(len(receipt['steps']), 1)

    def test_interrupted_first_stage_leaves_incomplete_receipt(self):
        with patch.object(run_gates.subprocess, 'run', side_effect=KeyboardInterrupt):
            with self.assertRaises(KeyboardInterrupt):
                run_gates.run('static', self.root)
        _, receipt = self.receipt()
        self.assertEqual(receipt['verdict'], 'INCOMPLETE')
        self.assertEqual(receipt['active_command']['name'], 'check.py')
        self.assertFalse(receipt['release_checks_complete'])

    def test_full_pass_is_code_evidence_and_uses_unique_mutation_report(self):
        with patch.object(run_gates.subprocess, 'run', side_effect=self.fake_command):
            with redirect_stdout(StringIO()):
                self.assertEqual(run_gates.run('audit', self.root), 0)
        _, receipt = self.receipt()
        self.assertTrue(receipt['all_mutations_complete'])
        self.assertFalse(receipt['release_checks_complete'])
        self.assertFalse(receipt['live_eligible'])
        self.assertIn('--out', self.calls[-1])
        self.assertIn('loop/gates-out/', self.calls[-1][-1])

    def test_fingerprint_ignores_collector_tapes_but_keeps_test_fixtures(self):
        baseline = run_gates.source_fingerprint(self.root)
        (self.root / 'raw-live.jsonl.gz').write_bytes(b'collector tape')
        self.assertEqual(run_gates.source_fingerprint(self.root), baseline)
        (self.root / 'testdata').mkdir()
        (self.root / 'testdata/gate-tape.jsonl.gz').write_bytes(b'test fixture')
        self.assertNotEqual(run_gates.source_fingerprint(self.root), baseline)

    def test_static_pass_is_local_only(self):
        with patch.object(run_gates.subprocess, 'run', side_effect=self.fake_command):
            with redirect_stdout(StringIO()):
                self.assertEqual(run_gates.run('static', self.root), 0)
        _, receipt = self.receipt()
        self.assertEqual(receipt['verdict'], 'PASS')
        self.assertFalse(receipt['release_checks_complete'])
        self.assertEqual(len(receipt['steps']), 4)


if __name__ == '__main__':
    unittest.main()
