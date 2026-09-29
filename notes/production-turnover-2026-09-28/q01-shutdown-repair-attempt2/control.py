from pathlib import Path
import datetime as dt, hashlib, json, os, plistlib, re, shlex, signal, subprocess, sys, time
repo=Path('/Users/hugh/kek/lip'); out=repo/'notes/production-turnover-2026-09-28/q01-shutdown-repair-attempt2'
binary=repo/'notes/production-turnover-2026-09-28/candidate-2/harness'
label='com.lip.q01.shutdown.attempt2.20260928'; domain='gui/'+str(os.getuid()); service=domain+'/'+label
cfg=out/'config.json'; evidence=out/'evidence.json'
def record(event, **fields):
 with (out/'operator-actions.jsonl').open('a') as f:f.write(json.dumps(dict(at_utc=dt.datetime.now(dt.timezone.utc).isoformat(),event=event,**fields))+'\n')
def identity():
 c=json.loads(cfg.read_text()); assert not Path(c['paths']['live_ok']).exists()
 expected=json.loads((binary.parent/'build-identity.json').read_text())
 assert hashlib.sha256(binary.read_bytes()).hexdigest()==expected['binary_sha256']
 assert expected.get('candidate_gate_verdict')=='PASS'
 frozen=out/'prepared-identity.json'
 if frozen.exists():
  previous=json.loads(frozen.read_text())
  assert previous['config_sha256']==hashlib.sha256(cfg.read_bytes()).hexdigest()
  assert previous['binary_sha256']==expected['binary_sha256']
 return expected['binary_sha256']
def state(name):
 p=subprocess.run(['/bin/launchctl','print',service],capture_output=True,text=True)
 (out/(name+'.txt')).write_text(p.stdout+p.stderr)
 return p.stdout

def process(name):
 s=state(name+'-supervisor'); pid=int(re.search(r'\n\s*pid = (\d+)\n',s)[1])
 p=subprocess.run(['/bin/ps','-ww','-p',str(pid),'-o','pid=,lstart=,command='],capture_output=True,text=True,check=True).stdout
 argv=subprocess.run(['/bin/ps','-ww','-p',str(pid),'-o','command='],capture_output=True,text=True,check=True).stdout.strip()
 executable=subprocess.run(['/bin/ps','-ww','-p',str(pid),'-o','comm='],capture_output=True,text=True,check=True).stdout.strip()
 assert executable==str(binary),(pid,executable)
 assert shlex.split(argv)==[str(binary),'-supervised','-config',str(cfg),'-rung','sizing','-qualification',str(evidence)],argv
 (out/(name+'-process.txt')).write_text(p)
 return pid,p

action=sys.argv[1]
if action=='prepare':
 identity()
 p=subprocess.run([str(binary),'-provision','-config',str(cfg)],capture_output=True,text=True)
 (out/'provision.log').write_text(p.stdout+p.stderr);assert p.returncode==0
 st=json.loads((out/'tunnel-status.json').read_text())
 pl={'Label':label,'RunAtLoad':True,'KeepAlive':{'SuccessfulExit':False},'ThrottleInterval':3,'WorkingDirectory':str(out/'runtime'),'StandardOutPath':str(out/'supervised.out'),'StandardErrorPath':str(out/'supervised.err'),'EnvironmentVariables':{'HTTPS_PROXY':'http://127.0.0.1:'+str(st['port']),'NO_PROXY':'api.elections.kalshi.com,external-api.kalshi.com,ntfy.sh,hc-ping.com,localhost,127.0.0.1'},'ProgramArguments':['/usr/bin/caffeinate','-is',str(binary),'-supervised','-config',str(cfg),'-rung','sizing','-qualification',str(evidence)]}
 with (out/'supervisor.plist').open('xb') as f:plistlib.dump(pl,f)
 frozen={'binary_sha256':identity(),'config_sha256':hashlib.sha256(cfg.read_bytes()).hexdigest(),'live':False}
 (out/'prepared-identity.json').write_text(json.dumps(frozen,indent=2)+'\n');record('prepared',**frozen)
elif action=='start':
 identity();p=subprocess.run(['/bin/launchctl','bootstrap',domain,str(out/'supervisor.plist')],capture_output=True,text=True)
 record('bootstrap',returncode=p.returncode,stdout=p.stdout,stderr=p.stderr);assert p.returncode==0
elif action=='status':
 pid,p=process('status-'+str(time.time_ns()));d=json.loads(evidence.read_text())
 print(json.dumps({'pid':pid,'updated_at':d['updated_at'],'active_seconds':sum(s['active_nanos'] for s in d['segments'])/1e9,'segments':d['segments'],'portfolio':d['portfolio'],'monitor':d['monitor'],'events':d['events'],'metadata':d['metadata']},indent=2))
elif action=='disconnect':
 identity();pid,p=process('before-disconnect')
 # Exercise the actual socket between completed portfolio walks. The
 # protected stale-generation behavior is unchanged by this fixture choice.
 deadline=time.monotonic()+30
 while True:
  d=json.loads(evidence.read_text());last=dt.datetime.fromisoformat(d['portfolio']['last_walk_at'].replace('Z','+00:00'));age=(dt.datetime.now(dt.timezone.utc)-last).total_seconds()
  if .7 < age < 2.75:break
  assert time.monotonic()<deadline,'no fresh inter-poll window';time.sleep(.1)
 assert d['metadata']['live'] is False
 (out/'before-disconnect-evidence.json').write_text(json.dumps(d,indent=2)+'\n')
 (out/'tunnel-trigger').touch(exist_ok=False);record('forced_disconnect_requested',pid=pid,portfolio_age_seconds=age)
elif action in ('restart','drain','abort-drain'):
 identity();pid,p=process('before-'+action);d=json.loads(evidence.read_text());assert d['metadata']['live'] is False
 if action=='drain':assert sum(s['active_nanos'] for s in d['segments'])>=1230e9
 (out/('before-'+action+'-evidence.json')).write_text(json.dumps(d,indent=2)+'\n')
 sig=signal.SIGKILL if action=='restart' else signal.SIGTERM
 verify_pid,verify_p=process('verified-'+action)
 assert (verify_pid,verify_p)==(pid,p),'process changed before signal'
 os.kill(pid,sig);record('verified_'+action,pid=pid,signal=sig.name,process_identity=p,binary_sha256=identity())
elif action=='unload':
 s=state('before-unload');assert not re.search(r'\n\s*pid = \d+\n',s),'process still active'
 p=subprocess.run(['/bin/launchctl','bootout',service],capture_output=True,text=True);record('bootout',returncode=p.returncode,stdout=p.stdout,stderr=p.stderr);assert p.returncode==0
else:raise ValueError(action)
