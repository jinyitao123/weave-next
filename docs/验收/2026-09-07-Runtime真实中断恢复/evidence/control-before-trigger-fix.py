#!/usr/bin/env python3
"""Owns only isolated acceptance processes. Never deletes a run workspace."""
import datetime as dt, hashlib, json, os, re, signal, subprocess, sys, time, urllib.request
from pathlib import Path
REPORT=Path(__file__).resolve().parents[1]
ENVIRONMENT=json.loads((REPORT/'evidence/environment.json').read_text())
ROOT=Path(ENVIRONMENT['root']); CONFIG=ROOT/'settings.json'
CFG=json.loads(CONFIG.read_text()); DB=ENVIRONMENT['database_name']
def now():return dt.datetime.now(dt.timezone.utc).isoformat()
def write(path,value):
 path.parent.mkdir(parents=True,exist_ok=True)
 temp=path.with_suffix(path.suffix+'.tmp');temp.write_text(json.dumps(value,ensure_ascii=False,indent=2)+'\n');temp.replace(path)
def append(path,value):
 path.parent.mkdir(parents=True,exist_ok=True)
 with path.open('a') as f:f.write(json.dumps(value,ensure_ascii=False)+'\n')
def query(sql):
 raw=subprocess.check_output(['psql','-d',DB,'-Atc',sql],text=True)
 return json.loads(raw) if raw.strip() else None

def process_table():
 result=[]
 for line in subprocess.check_output(['ps','-axo','pid=,ppid=,pgid=,command='],text=True).splitlines():
  parts=line.strip().split(None,3)
  if len(parts)==4:result.append({'pid':int(parts[0]),'ppid':int(parts[1]),'pgid':int(parts[2]),'command':parts[3]})
 return result

def env():
 value={**os.environ,**json.loads((ROOT/'private-env.json').read_text()),'DATABASE_URL':CFG['database_url'],'PORT':CFG['port']}
 value['WEAVE_RUNTIME_TOKEN']=(ROOT/'runtime-token').read_text()
 return value

def launch(role,command):
 logdir=ROOT/'logs';logdir.mkdir(exist_ok=True)
 sequence=len(list(logdir.glob(role+'-*.stdout.log')))+1
 with (logdir/f'{role}-{sequence}.stdout.log').open('w') as out,(logdir/f'{role}-{sequence}.stderr.log').open('w') as err:
  proc=subprocess.Popen(command,stdout=out,stderr=err,env=env(),start_new_session=True)
 row={'at':now(),'role':role,'pid':proc.pid,'command':command,'sequence':sequence}
 append(ROOT/'process-journal.jsonl',row)
 state=json.loads((ROOT/'processes.json').read_text()) if (ROOT/'processes.json').exists() else {}
 state[role]=row;write(ROOT/'processes.json',state)
 return proc

def start_api():
 p=launch('api',[str(ROOT/'driver'),'serve',str(CONFIG)])
 for _ in range(60):
  if p.poll() is not None:raise RuntimeError('isolated API startup failed; inspect its stderr')
  try:
   with urllib.request.urlopen('http://127.0.0.1:'+CFG['port']+'/v1/health',timeout=1) as response:
    if response.status==200:return p
  except Exception:pass
  time.sleep(.25)
 raise RuntimeError('isolated API not ready')

def services():
 start_api()
 p=launch('runtime',[str(ROOT/'weave'),'runtime','--server','http://127.0.0.1:'+CFG['port'],'--workspaces-root',str(ROOT/'runtime-workspaces'),'--concurrency','1'])
 for _ in range(90):
  if p.poll() is not None:raise RuntimeError('isolated Runtime startup failed')
  runtime=query("SELECT row_to_json(t) FROM (SELECT id,engines,engine_capabilities,last_heartbeat_at FROM weave_runtimes WHERE id='"+CFG['runtime_id']+"') t")
  if runtime and runtime['last_heartbeat_at'] and 'claude' in runtime['engines']:
   write(REPORT/'evidence/runtime-capabilities.json',runtime);print('isolated API and Runtime ready',flush=True);return
  time.sleep(1)
 raise RuntimeError('Runtime did not advertise Claude capability')

def files_snapshot(work):
 rows=[]
 if not work.exists():return rows
 for p in sorted((work/'outputs').rglob('*')):
  if p.is_file() and '__pycache__' not in p.parts and p.suffix not in ['.pyc']:
   try:b=p.read_bytes()
   except (OSError,FileNotFoundError):continue
   rows.append({'path':str(p.relative_to(work)),'sha256':hashlib.sha256(b).hexdigest(),'bytes':len(b)})
 return rows

def case_state(case):
 return query("SELECT row_to_json(t) FROM (SELECT id,status,error,worker_id,runtime_id,created_at,started_at,completed_at,lease_expires_at FROM weave_task_queue WHERE run_snapshot_id='runtime-value-"+case+"' AND payload->>'node_id'='engineering' ORDER BY created_at DESC LIMIT 1)t")
def events(case):
 return query("SELECT coalesce(json_agg(t),'[]'::json) FROM (SELECT detail,occurred_at FROM weave_team_run_activity_events WHERE run_id='runtime-value-"+case+"' AND kind='runtime_public' ORDER BY (detail->>'task_seq')::bigint)t")
def find_milestone(items,files):
 source=[f for f in files if f['path'].startswith('outputs/model/') and f['path'].endswith(('.py','.js','.ts','.go')) and f['bytes']>0]
 if len(source)<2:return None
 calls={x['detail']['event'].get('call_id'):x for x in items if x['detail']['event'].get('kind')=='tool_call'}
 for result in items:
  event=result['detail']['event'];call=calls.get(event.get('call_id'))
  if event.get('kind')!='tool_result' or event.get('status')!='ok' or not call:continue
  before=call['detail']['event'];command=before.get('input','')
  if before.get('tool')!='Bash':continue
  if not re.search(r'unittest|pytest|test_model',command,re.I):continue
  if not re.search(r'\bOK\b|\d+ passed|tests? passed',event.get('output',''),re.I):continue
  if re.search(r'FAILED \(|FAILURES|ERROR collecting',event.get('output','')):continue
  return {'tool_call':call,'tool_result':result,'model_source_files':len(source)}
 return None

def kill_owned(pid,role):
 row=next((p for p in process_table() if p['pid']==pid),None)
 assert row and str(ROOT) in row['command'],('unexpected process identity',pid,role)
 append(ROOT/'process-journal.jsonl',{'at':now(),'action':'SIGKILL','role':role,**row})
 os.kill(pid,signal.SIGKILL)

def fault(case,driver,files,milestone):
 state=json.loads((ROOT/'processes.json').read_text());table=process_table();runtime_id=state['runtime']['pid']
 children=[p for p in table if p['ppid']==runtime_id and '--output-format stream-json' in p['command']]
 assert len(children)==1,('Cannot identify sole active CLI',children)
 cli=children[0]
 entry={'at':now(),'case':case,'milestone':milestone,'files':files,'runtime_pid':runtime_id,'cli':cli,'api_pid':state['api']['pid'],'driver_pid':driver.pid,'processes':[p for p in table if p['pid'] in [runtime_id,cli['pid'],state['api']['pid'],driver.pid] or p['pgid']==cli['pgid']]}
 write(ROOT/'cases'/case/'fault-before.json',entry)
 if case=='C':
  kill_owned(driver.pid,'driver-C');driver.wait(timeout=10)
  kill_owned(state['api']['pid'],'api')
  # Declared outage, not artificial task work. Runtime/CLI remain untouched.
  time.sleep(10)
  start_api()
  resumed=launch('driver-C-resume',[str(ROOT/'driver'),'run',str(CONFIG),'C','driver-resumed-result.json'])
  after={**entry,'restarted_at':now(),'new_driver_pid':resumed.pid,'cli_still_same':any(p['pid']==cli['pid'] for p in process_table())}
  write(ROOT/'cases'/case/'fault-after.json',after)
  return resumed
 if case=='M':
  assert cli['pgid']==cli['pid'],'unexpected shared CLI process group'
  assert cli['pgid']!=os.getpgrp()
  append(ROOT/'process-journal.jsonl',{'at':now(),'action':'SIGKILL_GROUP','role':'member-M',**cli})
  os.killpg(cli['pgid'],signal.SIGKILL)
  write(ROOT/'cases'/case/'fault-after.json',{'at':now(),'signal':'SIGKILL','process_group':cli['pgid'],'runtime_preserved':True})
 return driver

def run(case):
 assert case in ['N','C','M']
 directory=ROOT/'cases'/case;directory.mkdir(parents=True,exist_ok=False)
 p=launch('driver-'+case,[str(ROOT/'driver'),'run',str(CONFIG),case])
 began=time.monotonic();last_files={};triggered=False;milestone_seen=False;last_print=0
 rec=json.loads((REPORT/'inputs/agent-record.json').read_text())
 work=None
 try:
  while True:
   task=case_state(case)
   if task:
    work=ROOT/'runtime-workspaces/.invocations'/task['id']/rec['workspace_id']/rec['name']/'workdir'
    items=events(case);files=files_snapshot(work)
    hashes={f['path']:f['sha256'] for f in files}
    if hashes!=last_files:
     append(directory/'filesystem-events.jsonl',{'at':now(),'files':files});last_files=hashes
    mark=find_milestone(items,files)
    if mark and not milestone_seen:
     milestone_seen=True
     write(directory/'milestone.json',{'observed_at':now(),'files':files,**mark})
     print(case,'model-test milestone reached',flush=True)
     if case!='N' and task['status']=='running' and p.poll() is None:
      p=fault(case,p,files,mark);triggered=True
    if time.monotonic()-last_print>=30:
     last_print=time.monotonic();print(json.dumps({'case':case,'task_status':task['status'],'events':len(items),'files':len(files),'milestone':milestone_seen,'injected':triggered,'elapsed_seconds':round(time.monotonic()-began)},ensure_ascii=False),flush=True)
    if p.poll() is not None:
     # Allow result/event acknowledgement tail to become visible.
     time.sleep(1)
     receipt=query("SELECT row_to_json(t) FROM (SELECT * FROM weave_task_queue WHERE id='"+task['id']+"')t")
     write(directory/'task-receipt.json',receipt);write(directory/'public-events.json',events(case));write(directory/'final-files.json',files_snapshot(work))
     result={'case':case,'finished_at':now(),'driver_exit':p.returncode,'milestone_observed':milestone_seen,'fault_injected':triggered,'task_id':task['id'],'workdir':str(work),'elapsed_seconds':round(time.monotonic()-began,3),'status':'engine_completed' if receipt.get('result',{}).get('status')=='completed' else 'engine_failed','independent_acceptance':'pending'}
     if case=='M' and triggered:
      reconnect=launch('driver-M-reconnect',[str(ROOT/'driver'),'run',str(CONFIG),'M','driver-reconnect-result.json']);result['same_invocation_reconnect_exit']=reconnect.wait(timeout=30)
      result['engineering_physical_tasks']=query("SELECT count(*) FROM weave_task_queue WHERE run_snapshot_id='runtime-value-M' AND payload->>'node_id'='engineering'")
     write(directory/'status.json',result);print(json.dumps(result,ensure_ascii=False),flush=True);return
   elif p.poll() is not None:raise RuntimeError('driver exited before task admission')
   if time.monotonic()-began>2820:raise TimeoutError('watcher deadline exceeded; investigate remaining owned processes')
   time.sleep(1)
 except BaseException as error:
  write(directory/'watcher-interruption.json',{'at':now(),'error':type(error).__name__+': '+str(error),'driver_pid':p.pid,'driver_exit':p.poll(),'injected':triggered})
  raise
if __name__=='__main__':
 if sys.argv[1]=='services':services()
 elif sys.argv[1]=='run':run(sys.argv[2])
 else:raise SystemExit('services | run N|C|M')
