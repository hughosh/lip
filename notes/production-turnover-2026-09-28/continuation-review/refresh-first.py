#!/usr/bin/env python3
"""Operator convenience: GET-only refresh and exact decision binding; never starts a writer."""
import datetime as dt
import hashlib
import json
from pathlib import Path
import shlex
import subprocess

ROOT = Path('/Users/hugh/kek/lip')
HERE = Path(__file__).resolve().parent
PY = '/Users/hugh/kek/.venv/bin/python'
BIN = ROOT / 'notes/production-turnover-2026-09-28/candidate-2/harness'
BUILD = BIN.parent / 'build-identity.json'
TICKER = 'KXBROSFT-26OCT08-T110'


def main():
    stamp = dt.datetime.now(dt.timezone.utc).strftime('%Y%m%dT%H%M%S.%fZ')
    book = HERE / f'market-book-series-{stamp}.json'
    log = HERE / f'preparation-{stamp}.log'
    commands = [
        [PY, 'scripts/public_market_snapshot.py', '--ticker', TICKER, '--out', str(book)],
        [PY, 'scripts/operator_stage.py', '--stage', 'first', '--ticker', TICKER,
         '--size', '12', '--binary', str(BIN), '--candidate-receipt', str(BUILD),
         '--book', str(book), '--evidence-root', str(HERE / 'operator-stages')],
    ]
    print('Refreshing public program/book/fees and complete authenticated account truth (GET only).', flush=True)
    with log.open('x') as output:
        for command in commands:
            result = subprocess.run(command, cwd=ROOT, stdout=output, stderr=subprocess.STDOUT)
            if result.returncode:
                print(f'Preparation failed; retain and inspect {log}. No launch is permitted.')
                return result.returncode
    marker = next(row for row in log.read_text().splitlines() if row.startswith('Candidate identity: '))
    identity_path = Path(marker.split(': ', 1)[1])
    identity = json.loads(identity_path.read_text())
    stage = identity_path.parent.parent
    config = json.loads(Path(identity['config_path']).read_text())
    account = json.loads(Path(identity['account_report']).read_text())
    expected = {'rung': 'sizing', 'ticker': TICKER, 's': 12.0, 'n_markets': 1,
                'capital_source': 'selected_shard_balance', 's_max': 48,
                'inv_soft': 3, 'inv_hard': 7, 'inv_kill': 18, 'pnl_kill': -15,
                'heartbeat_s': 30}
    if (any(config.get(k) != v for k, v in expected.items()) or config.get('turnover', False)
        or identity['binary_sha256'] != 'e3f8a697a20c3153f5c2a8650094b949ff57b985f94b154a7defdad22e9ef361'
        or identity['source_manifest_sha256'] != '6047b85a4306c70bd7048c8ed15e41e2ab87b8a21075a8a550747a8c13e1c709'
        or not account['account_scope_complete'] or not account['flat']):
        raise ValueError('prepared stage differs from the reviewed envelope; do not launch')
    review = HERE / 'first-writer-review.md'
    parse = lambda s: dt.datetime.fromisoformat(s.replace('Z', '+00:00'))
    times = [parse(account['started_at'])]
    for name in ('programs.json', 'candidate.json'):
        times.append(parse(json.loads((identity_path.parent / name).read_text())['started_at_utc']))
    times += [parse(row['at_utc']) for row in json.loads(book.read_text())['calls'].values()]
    deadline = min(times) + dt.timedelta(seconds=60)
    decision = {
        'at_utc': dt.datetime.now(dt.timezone.utc).isoformat(),
        'decision': 'technical evidence accepted for operator-run attended first-owned-fill stage only; operator reviews current inputs before activation',
        'review': str(review), 'review_sha256': hashlib.sha256(review.read_bytes()).hexdigest(),
        'binary_sha256': identity['binary_sha256'], 'source_manifest_sha256': identity['source_manifest_sha256'],
        'config_path': identity['config_path'], 'config_sha256': identity['config_sha256'],
        'identity_path': str(identity_path), 'identity_sha256': hashlib.sha256(identity_path.read_bytes()).hexdigest(),
        'operator_attendance': 'Hugh confirmed in conversation; not a claim of launch or completed review',
        'operator_launch_observed': False, 'no_financial_action_by_helper': True,
        'refresh_after_utc': deadline.isoformat(), 'primary_ntfy_only': True, 'unattended_permitted': False,
        'q01_config_sha256': '47b3b73244755998afab6d6d27376a15b96c6eb7d99fd680eafcef776ae67135',
    }
    (identity_path.parent / 'candidate-decision.json').write_text(json.dumps(decision, indent=2) + '\n')
    (HERE / 'current-stage.txt').write_text(str(stage) + '\n')
    print('Config SHA256:', identity['config_sha256'])
    print('Fresh complete account ended:', account['ended_at'])
    print('Funding:', (identity_path.parent / 'funding-arithmetic.json').read_text())
    print('Read current market/rules/fees/depth:', identity_path.parent)
    print('Refresh again if not starting by', deadline.isoformat())
    print('OPERATOR START BLOCK ONLY (printed, never executed):')
    print(f'STAGE={shlex.quote(str(stage))}')
    print(f'BIN={shlex.quote(str(BIN))}')
    print(f'test "$(date +%s)" -lt {int(deadline.timestamp())} &&')
    print('"$BIN" -config "$STAGE/config.json" -provision &&')
    print('install -m 600 /dev/null "$STAGE/runtime/live_ok" && {')
    print('  date -u > "$STAGE/evidence/operator-start-time.txt"')
    print('  "$BIN" -config "$STAGE/config.json" -rung sizing -live > "$STAGE/evidence/harness.log" 2>&1 &')
    print('  echo $! > "$STAGE/evidence/harness.pid"')
    print('  cat "$STAGE/evidence/harness.pid"')
    print('}')
    print('Remain attended. Drain target: 45 minutes; unresolved exposure keeps cleanup available.')
    print('Preserved full preparation/restart/exit reference:', log)
    return 0


if __name__ == '__main__':
    raise SystemExit(main())
