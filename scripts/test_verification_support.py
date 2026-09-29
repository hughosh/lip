"""Isolation tests use subprocesses, never the trading client or conductor."""
import ast
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest
from unittest.mock import patch

from verification_support import verification_lock, local_environment


class VerificationIsolation(unittest.TestCase):
    def test_competing_process_refused_but_inherited_fd_is_allowed(self):
        with tempfile.TemporaryDirectory() as tmp:
            snippet = ('from verification_support import verification_lock\n'
                       'import sys\n'
                       'with verification_lock(sys.argv[1]): print("acquired")\n')
            with verification_lock(tmp) as fd:
                cmd = [sys.executable, '-c', snippet, tmp]
                env = {**os.environ, 'PYTHONPATH': str(Path(__file__).parent),
                       'LIP_VERIFY_LOCK_FD': str(fd), 'TMPDIR': tmp}
                blocked = subprocess.run(cmd, env=env, capture_output=True, text=True)
                self.assertNotEqual(blocked.returncode, 0)
                self.assertIn('another verification owns', blocked.stderr)
                child = subprocess.run(cmd, env=env, pass_fds=(fd,), capture_output=True, text=True)
                self.assertEqual(child.returncode, 0, child.stderr)
            released = subprocess.run(cmd, env=env, capture_output=True, text=True)
            self.assertEqual(released.returncode, 0, released.stderr)

    def test_environment_drops_credentials_and_injected_go_flags(self):
        with patch.dict(os.environ, {'KALSHI_KEY': 'test-secret', 'GOFLAGS': '-tags=unsafe',
                                     'PYTHONPATH': '/untrusted'}):
            env = local_environment()
        self.assertNotIn('KALSHI_KEY', env)
        self.assertNotIn('PYTHONPATH', env)
        self.assertEqual(env['GOFLAGS'], '-p=2')
        self.assertEqual(env['GOPROXY'], 'off')

    def test_conductor_entry_refuses_before_historical_code(self):
        # Inspect syntax only: the user forbids running the conductor, even dry-run.
        path = Path(__file__).resolve().parent.parent / 'loop/conductor.py'
        tree = ast.parse(path.read_text())
        main = next(n for n in tree.body if isinstance(n, ast.FunctionDef) and n.name == 'main')
        self.assertIsInstance(main.body[0], ast.Expr)
        self.assertEqual(main.body[0].value.func.id, 'print')
        self.assertIsInstance(main.body[1], ast.Return)
        self.assertEqual(main.body[1].value.value, 2)


if __name__ == '__main__':
    unittest.main()
