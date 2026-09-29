#!/usr/bin/env python3
"""Run local verification stages and preserve an auditable receipt per run."""
from __future__ import annotations

from datetime import datetime, timezone
import hashlib
import json
import os
from pathlib import Path
import subprocess
import sys
import time
import shutil
from verification_support import verification_lock, local_environment
from uuid import uuid4

ROOT = Path(__file__).resolve().parent.parent
PYTHON = Path('/Users/hugh/kek/.venv/bin/python')
GO_BIN = '/usr/local/bin/go'
EXCLUDED_DIRS = {'.git', '.beads', '.venv', '__pycache__', '.pytest_cache'}
SOURCE_SUFFIXES = {'.py', '.go', '.sh', '.md', '.mod', '.sum', '.json', '.yaml', '.yml', '.toml', '.tsv', '.sha256', '.gz', '.txt'}


def source_fingerprint(root: Path) -> str:
    """Hash source and policy files, including files not yet tracked by Git."""
    digest = hashlib.sha256()
    paths = []
    for directory, dirs, files in os.walk(root):
        dirs[:] = sorted(d for d in dirs if d not in EXCLUDED_DIRS
                         and (Path(directory) / d).relative_to(root).as_posix() not in
                         {'loop/gates-out', 'loop/state', 'loop/run', 'shadow', 'archive'}
                         and not ((Path(directory) / d).relative_to(root).parts[0] == 'notes'
                                  and '-evidence-' in d))
        for name in files:
            path = Path(directory) / name
            rel = path.relative_to(root)
            fixture = rel.parts[0] in {"go", "testdata"}
            if path.suffix in SOURCE_SUFFIXES and (path.suffix != ".gz" or fixture):
                paths.append(path)
    for path in sorted(paths):
        relative = path.relative_to(root).as_posix().encode()
        digest.update(len(relative).to_bytes(8, 'big'))
        digest.update(relative)
        digest.update(path.stat().st_size.to_bytes(8, 'big'))
        with path.open('rb') as source:
            for block in iter(lambda: source.read(1024 * 1024), b''):
                digest.update(block)
    return digest.hexdigest()


def stages(mode: str, root: Path = ROOT) -> list[tuple[str, Path, list[str]]]:
    py = str(PYTHON)
    static = [
        ('check.py', root, [py, 'scripts/check.py']),
        ('gate-tests', root, [py, 'scripts/test_gates.py']),
        ('isolation-tests', root, [py, 'scripts/test_verification_support.py']),
        ('catalogue-anchors', root, [py, 'scripts/test_harness_negative_control.py',
          'TestRealCatalogueAnchors', 'TestJSONVerdicts', 'TestExecutionCommands']),
    ]
    if mode == 'static':
        return static
    go = root / 'go'
    ordinary = [
        ('build', go, [GO_BIN, 'build', '-p', '2', './...']),
        ('vet', go, [GO_BIN, 'vet', '-p', '2', './...']),
        ('gofmt', go, ['bash', '-c',
          'files=$(gofmt -l harness cmd/harness cmd/incentives cmd/conform) && test -z "$files"']),
        ('test-module', go, [GO_BIN, 'test', '-count=1', '-json', '-timeout=5m', '-p', '2', './...']),
    ]
    if mode == 'quick':
        return static + ordinary
    race = [('test-race', go, [GO_BIN, 'test', '-race', '-count=1', '-json', '-timeout=5m', '-p', '2',
                               './harness/...', './cmd/harness/...'])]
    if mode == 'race':
        return static + ordinary + race
    if mode == 'audit':
        return static + ordinary + race + [
            ('negative-control', root, [py, 'scripts/harness_negative_control.py'])]
    safety = json.loads((ROOT / 'scripts/safety_mutations.json').read_text())['mutations']
    return static + ordinary + race + [
        ('safety-mutations', root, [py, 'scripts/harness_negative_control.py',
                                   '--only', ','.join(safety)])]


def run(mode: str, root: Path = ROOT) -> int:
    try:
        with verification_lock(root) as lock_fd:
            return run_locked(mode, root, lock_fd)
    except RuntimeError as exc:
        print(f'INCONCLUSIVE: {exc}', file=sys.stderr)
        return 2


def run_locked(mode: str, root: Path, lock_fd: int) -> int:
    run_dir = root / 'loop/gates-out' / (datetime.now(timezone.utc).strftime('%Y%m%dT%H%M%S%fZ') + '-' + uuid4().hex[:8])
    run_dir.mkdir(parents=True, exist_ok=False)
    started = time.monotonic()
    (run_dir / "receipt.json").write_text(json.dumps({
        "schema": 1, "mode": mode, "verdict": "INCOMPLETE",
        "active_command": {"name": "source-fingerprint"},
        "release_checks_complete": False, "live_eligible": False}) + "\n")
    before = source_fingerprint(root)
    clean_env = local_environment()
    receipt: dict = {
        'schema': 1, 'mode': mode, 'started_at_utc': datetime.now(timezone.utc).isoformat(),
        'source_sha256_before': before, 'steps': [],
        'release_checks_complete': False, 'candidate_smoke_complete': False,
        'all_mutations_complete': False, 'live_eligible': False,
        'advance_eligible': False,
        'environment': {**clean_env, 'CGO_ENABLED_race': '1'},
        'disk_free_bytes_start': shutil.disk_usage(root).free,
        'load_start': os.getloadavg(),
    }
    tool_hash = hashlib.sha256(Path(GO_BIN).read_bytes()).hexdigest()
    receipt['toolchain'] = {'go_binary': str(Path(GO_BIN).resolve()), 'go_sha256': tool_hash,
                            'python': sys.executable, 'python_version': sys.version}
    receipt['verdict'] = 'INCOMPLETE'
    (run_dir / 'receipt.json').write_text(json.dumps(receipt, indent=2) + '\n')
    failed = False
    for name, cwd, command in stages(mode, root):
        log = run_dir / f'{name}.log'
        start = time.monotonic()
        env = dict(clean_env)
        if name in ('negative-control', 'safety-mutations'):
            command = command + ['--out', str(run_dir / 'mutations.md')]
        # A catalogue self-test invokes a nested mutation CLI. It uses the
        # same inherited lock rather than starting a competing job.
        env['LIP_VERIFY_LOCK_FD'] = str(lock_fd)
        if name == 'build':
            env['CGO_ENABLED'] = '0'
        elif name == 'test-race':
            env['CGO_ENABLED'] = '1'
        receipt['active_command'] = {'name': name, 'command': command, 'log': str(log)}
        (run_dir / 'receipt.json').write_text(json.dumps(receipt, indent=2) + '\n')
        with log.open('wb') as output:
            try:
                code = subprocess.run(command, cwd=cwd, env=env,
                                      stdout=output, stderr=subprocess.STDOUT,
                                      check=False, pass_fds=(lock_fd,)).returncode
            except OSError as exc:
                output.write(f'Could not start command: {exc}\n'.encode())
                code = 127
        step = {'name': name, 'command': command, 'cwd': str(cwd),
                'exit_code': code, 'seconds': round(time.monotonic() - start, 3),
                'log': str(log)}
        receipt.pop('active_command', None)
        receipt['steps'].append(step)
        print(f"{'PASS' if code == 0 else 'FAIL'} {name} (rc={code}, {step['seconds']}s) — {log}", flush=True)
        if code != 0:
            failed = True
            print('\n'.join(log.read_text(errors='replace').splitlines()[-40:]), file=sys.stderr)
            break
        (run_dir / 'receipt.json').write_text(json.dumps(receipt, indent=2) + '\n')
        if source_fingerprint(root) != before:
            failed = True
            receipt['source_drift_detected_after'] = name
            print(f'FAIL source changed during {name}', file=sys.stderr)
            break
    after = source_fingerprint(root)
    if after != before or hashlib.sha256(Path(GO_BIN).read_bytes()).hexdigest() != tool_hash:
        failed = True
    receipt['duration_seconds'] = round(time.monotonic() - started, 3)
    receipt['disk_free_bytes_end'] = shutil.disk_usage(root).free
    receipt['source_sha256_after'] = after
    receipt['finished_at_utc'] = datetime.now(timezone.utc).isoformat()
    receipt['candidate_smoke_complete'] = mode == 'candidate' and not failed
    receipt['all_mutations_complete'] = mode == 'audit' and not failed
    receipt['verdict'] = 'PASS' if not failed else 'FAIL'
    (run_dir / 'receipt.json').write_text(json.dumps(receipt, indent=2) + '\n')
    print(f'RECEIPT: {run_dir / "receipt.json"}')
    print(f'LOCAL_CHECKS: {receipt["verdict"]}')
    print(f'CANDIDATE_SMOKE: {"COMPLETE" if receipt["candidate_smoke_complete"] else "NOT_RUN"}')
    print(f'ALL_MUTATIONS: {"COMPLETE" if receipt["all_mutations_complete"] else "NOT_RUN"}')
    print('RELEASE_CHECKS: REVIEW_REQUIRED')
    print('LIVE_ELIGIBLE: NO')
    print('ADVANCE_ELIGIBLE: NO')
    return int(failed)


def main(argv: list[str]) -> int:
    modes = {(): 'quick', ('--quick',): 'quick', ('--release',): 'candidate', ('--candidate',): 'candidate', ('--audit',): 'audit',
             ('--static',): 'static', ('--race',): 'race'}
    mode = modes.get(tuple(argv))
    if mode is None:
        print('Usage: loop/gates.sh [--static|--quick|--race|--candidate|--audit] (default: quick)', file=sys.stderr)
        return 2
    return run(mode)


if __name__ == '__main__':
    raise SystemExit(main(sys.argv[1:]))
