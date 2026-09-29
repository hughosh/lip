#!/usr/bin/env python3
"""Bounded read-only support for the human-attended stage; not an independent watchdog."""
import datetime as dt
import json
from pathlib import Path
import sqlite3
import subprocess
import time

STAGE = Path('/Users/hugh/kek/lip/notes/production-turnover-2026-09-28/continuation-review/operator-stages/first-KXBROSFT-26OCT08-T110-20260928T193111.232608Z')
DB = STAGE / 'runtime/harness.db'
PID = '70087'
DEADLINE = dt.datetime.fromisoformat('2026-09-28T20:18:03+00:00')


def snapshot():
    now = dt.datetime.now(dt.timezone.utc)
    ps = subprocess.run(['ps', '-p', PID, '-o', 'pid=,lstart=,command='], capture_output=True, text=True)
    process = ps.stdout.strip()
    row = {'at_utc': now.isoformat(), 'process': process, 'latch_exists': (STAGE / 'runtime/harness.halt').exists()}
    # Do not cause SQLite to create sidecars for a closed or changing store.
    if Path(str(DB) + '-wal').exists() and Path(str(DB) + '-shm').exists():
        with sqlite3.connect(DB.as_uri() + '?mode=ro', uri=True, timeout=1) as connection:
            connection.execute('PRAGMA query_only=ON')
            connection.row_factory = sqlite3.Row
            connection.execute('BEGIN')
            row['owned_orders'] = connection.execute('SELECT COUNT(*) FROM owned_order').fetchone()[0]
            row['fills'] = [dict(x) for x in connection.execute('SELECT * FROM our_fill ORDER BY rowid DESC LIMIT 30')]
            row['states'] = [dict(x) for x in connection.execute('SELECT * FROM state_event ORDER BY rowid DESC LIMIT 8')]
            row['sev1'] = [dict(x) for x in connection.execute('SELECT * FROM anomaly WHERE sev=2 ORDER BY rowid DESC LIMIT 20')]
            row['recent_anomalies'] = [dict(x) for x in connection.execute("SELECT anomaly_id,class,sev,first_ms,delivered_ms,text FROM anomaly WHERE class != 'FOREIGN_FILL_INHERITED' ORDER BY rowid DESC LIMIT 5")]
            row['balance_polls'] = connection.execute('SELECT COUNT(*) FROM balance_poll').fetchone()[0]
            connection.rollback()
    else:
        row['store_read'] = 'skipped: WAL sidecars absent; no store mutation attempted'
    return row


def main():
    last_key = None
    last_output = 0
    path = STAGE / 'evidence/read-only-observer.jsonl'
    while True:
        try:
            row = snapshot()
        except Exception as exc:
            row = {'at_utc': dt.datetime.now(dt.timezone.utc).isoformat(), 'read_error_class': type(exc).__name__}
        with path.open('a') as stream:
            stream.write(json.dumps(row, sort_keys=True) + '\n')
        key = json.dumps({k: row.get(k) for k in ('process', 'latch_exists', 'owned_orders', 'fills', 'states', 'sev1', 'read_error_class')}, sort_keys=True)
        if key != last_key or time.monotonic() - last_output >= 50:
            print(json.dumps(row, sort_keys=True), flush=True)
            last_key, last_output = key, time.monotonic()
        if row.get('fills') or row.get('sev1') or row.get('latch_exists') or row.get('process') == '':
            print('Observation event: review now; no process action taken.', flush=True)
            return
        if dt.datetime.now(dt.timezone.utc) >= DEADLINE:
            print('Operator drain target reached; observer took no signal/trade/cancellation action.', flush=True)
            return
        time.sleep(10)


if __name__ == '__main__':
    main()
