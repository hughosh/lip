from pathlib import Path
import sys,datetime,subprocess,json
repo=Path('/Users/hugh/kek/lip')
sys.path.insert(0,str(repo/'scripts'))
from verification_support import local_environment
from operator_stage import source_manifest,sha256
out=Path(__file__).parent
name=sys.argv[1];command=sys.argv[2:]
receipt={'started_at':datetime.datetime.now(datetime.timezone.utc).isoformat(),'command':command,'cwd':str(repo/'go'),'source_manifest_before':source_manifest(repo)[1]}
with (out/(name+'.log')).open('x') as f:
 result=subprocess.run(command,cwd=repo/'go',env=local_environment(),stdout=f,stderr=subprocess.STDOUT)
receipt.update({'finished_at':datetime.datetime.now(datetime.timezone.utc).isoformat(),'exit_code':result.returncode,'source_manifest_after':source_manifest(repo)[1],'log_sha256':sha256(out/(name+'.log'))})
(out/(name+'.json')).write_text(json.dumps(receipt,indent=2)+'\n')
print(json.dumps(receipt))
raise SystemExit(result.returncode)
