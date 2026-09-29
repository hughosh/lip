from pathlib import Path
import datetime as dt,json
p=Path(__file__).resolve().parent
d=json.loads((p/'evidence.json').read_text())
r={'observed_at_utc':dt.datetime.now(dt.timezone.utc).isoformat(),'updated_at':d['updated_at'],'active_seconds':sum(s['active_nanos'] for s in d['segments'])/1e9,'pids':[s['pid'] for s in d['segments']],'finalized_at':d.get('finalized_at'),'portfolio':d['portfolio'],'monitor':d['monitor'],'would_write_observations':sum(e['observations'] for e in d['would_writes'] or []),'above_guard_non_get':sum(x['count'] for x in d['attempted_http'] if x['method']!='GET'),'below_guard_non_get':sum(x['count'] for x in d['http'] if x['method']!='GET'),'transported_gets':sum(x['count'] for x in d['http'] if x['method']=='GET'),'sev1':[e for e in d['events'] if e['name'].startswith('SEV1:')],'selection':[e for e in d['events'] if 'TURNOVER_SELECTION' in e['name']],'ws':[e for e in d['events'] if e['name'].startswith('websocket:')]}
with (p/'progress.jsonl').open('a') as f:f.write(json.dumps(r)+'\n')
print(json.dumps(r,indent=2))
