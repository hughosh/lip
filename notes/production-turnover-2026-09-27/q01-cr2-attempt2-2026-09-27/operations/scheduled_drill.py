#!/usr/bin/env python3
"""Bounded, account-free ntfy drill; launchd invokes each tick independently.

No exchange requests, heartbeat success pings, or automatic acknowledgments.
Routes remain in the existing credential file, outside argv and receipts.
"""
import datetime
import importlib.util
import json
import os
from pathlib import Path
import re
import subprocess
import threading
import time
from http.server import BaseHTTPRequestHandler, HTTPServer
import urllib.request

ROOT = Path('/Users/hugh/kek/lip')
HERE = Path(__file__).resolve().parent
spec = importlib.util.spec_from_file_location('ops_watchdog', ROOT / 'scripts/ops_watchdog.py')
w = importlib.util.module_from_spec(spec)
spec.loader.exec_module(w)


def record(name, **values):
    row = dict(at=datetime.datetime.now(datetime.timezone.utc).isoformat(),
               event=name, pid=os.getpid(), **values)
    with (HERE / 'scheduler-events.jsonl').open('a') as f:
        f.write(json.dumps(row, sort_keys=True) + '\n')


class FailConnect(BaseHTTPRequestHandler):
    def do_CONNECT(self):
        self.send_response(503)
        self.end_headers()

    def log_message(self, *_):
        pass


def send(url, payload):
    fault = HERE / 'fail-primary-once'
    server = None
    opener = urllib.request.build_opener(w._NoRedirect())
    injected = fault.exists()
    if injected:
        fault.rename(HERE / 'fail-primary-once.used')
        server = HTTPServer(('127.0.0.1', 0), FailConnect)
        threading.Thread(target=server.serve_forever, daemon=True).start()
        opener = urllib.request.build_opener(
            urllib.request.ProxyHandler({'https': 'http://127.0.0.1:' + str(server.server_port)}),
            w._NoRedirect())
    req = urllib.request.Request(url, data=(payload['incident_id'] + ': ' + payload['message']).encode(),
        headers={'Content-Type': 'text/plain', 'Title': 'LIP operations drill', 'Priority': 'urgent'}, method='POST')
    receipt = dict(incident_id=payload['incident_id'], role=payload['role'],
                   injected_transport_fault=injected, human_acknowledgment=False)
    try:
        with opener.open(req, timeout=10) as response:
            body = json.loads(response.read(16384))
            accepted = 200 <= response.status < 300
            receipt.update(http_status=response.status, transport_accepted=accepted,
                           provider_message_id=body.get('id'), provider_time=body.get('time'))
    except Exception as exc:
        accepted = False
        receipt.update(transport_accepted=False, error_class=type(exc).__name__)
    finally:
        if server:
            server.shutdown()
            server.server_close()
    record('delivery', **receipt)
    return accepted


def main():
    control = json.loads((HERE / 'control.json').read_text())
    record('tick_start', ppid=os.getppid())
    if time.time() >= control['stop_after_unix']:
        record('bounded_stop', reason='drill deadline reached; no further sends')
        return
    topics = []
    for line in Path('/Users/hugh/.kalshi/env').read_text().splitlines():
        if line.startswith('NTFY_TOPIC='):
            topics.append(line.split('=', 1)[1].strip().strip('\"').strip("'"))
    if len(topics) != 1 or not re.fullmatch(r'[A-Za-z0-9_-]+', topics[0]):
        raise RuntimeError('invalid ntfy configuration')
    if control.get('source_db'):
        try:
            result = subprocess.run(['/Users/hugh/kek/.venv/bin/python',
                str(ROOT / 'scripts/ops_watchdog_source.py'), '--state', str(HERE / 'watchdog.json'),
                '--db', control['source_db'], '--source-id', 'cr2-get-only-qualification', 'once'],
                capture_output=True, text=True, timeout=10)
            record('source_read', exit_code=result.returncode,
                   result=json.loads(result.stdout) if result.stdout.strip() else None)
        except (subprocess.TimeoutExpired, OSError, ValueError) as exc:
            record('source_runner_failure', error_class=type(exc).__name__)
            w.ingest(HERE / 'watchdog.json', {
                'event_id': 'LIP-DRILL-SOURCE-RUNNER-20260927',
                'incident_id': 'LIP-DRILL-SOURCE-RUNNER-20260927',
                'source': 'bounded-external-ntfy-drill', 'severity': 'SEV1',
                'message': 'Read-only drill source adapter failed; inspect retained receipts'},
                int(time.time() * 1000))
        # A failed source still has a durable outage incident to deliver below.
    outcomes = w.tick(HERE / 'watchdog.json', 'https://ntfy.sh/' + topics[0], None,
                      300000, int(time.time() * 1000), send=send)
    state = w.status(HERE / 'watchdog.json')
    record('tick_complete', outcomes=outcomes, backup_configured=False,
           incidents={key: {'acknowledged': value['ack'] is not None,
                            'age_seconds': (time.time()*1000-value['first_seen_ms'])/1000,
                            'primary': value['primary'], 'backup': value['backup']}
                      for key, value in state['incidents'].items()})
    pause = HERE / 'pause-once'
    if pause.exists():
        pause.rename(HERE / 'pause-once.used')
        record('pause_for_verified_process_kill')
        time.sleep(45)


if __name__ == '__main__':
    try:
        main()
    except Exception as exc:
        record('failed', error_class=type(exc).__name__)
        raise SystemExit(1)
