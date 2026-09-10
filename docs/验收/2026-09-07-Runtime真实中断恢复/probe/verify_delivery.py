#!/usr/bin/env python3
"""Independent artifact checks. Does not promote engine completion to PASS."""
import argparse, datetime as dt, hashlib, json, math, os, shlex, shutil, subprocess, tempfile
from decimal import Decimal as D, getcontext
from pathlib import Path
import xml.etree.ElementTree as ET
REPORT=Path(__file__).resolve().parents[1]
ROOT=Path(json.loads((REPORT/'evidence/environment.json').read_text())['root'])
getcontext().prec=60

def verify(case):
 directory=ROOT/'cases'/case
 status=json.loads((directory/'status.json').read_text());work=Path(status['workdir']);out=work/'outputs';checks=[]
 evidence=directory/'independent';evidence.mkdir(exist_ok=True)
 def check(name,passed,detail=None):checks.append({'check':name,'passed':passed,'detail':detail})
 def resolve(value):
  value=str(value);p=(work/value).resolve() if value.startswith('outputs/') else (out/value).resolve()
  if not p.is_relative_to(out.resolve()):raise ValueError('output path escapes delivery')
  return p
 source=REPORT/'inputs/upstream/lead/model/baseline_frozen.yaml';target=out/'model/baseline_frozen.yaml'
 check('exact_frozen_baseline',target.exists() and hashlib.sha256(source.read_bytes()).digest()==hashlib.sha256(target.read_bytes()).digest(),str(target))
 manifests=list(out.glob('verification/member_acceptance.json'))
 check('machine_manifest_present',len(manifests)==1)
 if not manifests:
  result={'case':case,'at':dt.datetime.now(dt.timezone.utc).isoformat(),'checks':checks,'browser_acceptance':'NOT_RUN','passed':False};(evidence/'artifact-checks.json').write_text(json.dumps(result,ensure_ascii=False,indent=2)+'\n');return result
 try:m=json.loads(manifests[0].read_text())
 except (ValueError,UnicodeError) as e:
  check('machine_manifest_parse',False,str(e));m={}
 check('trace_keys',m.get('baseline_id')=='corona-baseline-1.0.0' and str(m.get('baseline_version'))=='1.0.0' and m.get('content_digest')==hashlib.sha256(source.read_bytes()).hexdigest())
 refs={'travel_time_yr':D('4.25')/D('.03'),'kinetic_energy_1mt_j':D('0.5')*D('1e9')*(D('.03')*D('299792458'))**2,'kinetic_energy_5mt_j':D('0.5')*D('5e9')*(D('.03')*D('299792458'))**2,'artificial_gravity_m_s2':D(str((2*math.pi*2/60)**2*1000)),'dust_impact_energy_j':D('.5')*D('1e-6')*(D('.03')*D('299792458'))**2}
 actual=m.get('reference_checks',{})
 for name,value in refs.items():
  got=actual.get(name)
  if isinstance(got,dict):got=got.get('value',got.get('actual',got.get('result')))
  try:error=abs(D(str(got))/value-1);passed=error<(D('.001') if name=='travel_time_yr' else D('.01'))
  except Exception:error=None;passed=False
  check('reference_'+name,passed,{'actual':got,'independent_expected':str(value),'relative_error':str(error) if error is not None else None,'relative_tolerance':('0.001' if name=='travel_time_yr' else '0.01')})
 command=m.get('model_test_command','')
 if isinstance(command,str):
  try:
   parts=shlex.split(command, comments=True)
   if parts[:3]==["cd","outputs/model","&&"]:parts=parts[3:]
   # The caller reviews generated source separately before this checker runs.
   if not parts or Path(parts[0]).name not in ['python3','python'] or any(x in parts for x in [';','&&','||','|']):raise ValueError('manual command review required')
   with tempfile.TemporaryDirectory(prefix='runtime-value-delivery-tests-') as temp:
    copied=Path(temp)/'outputs';shutil.copytree(out,copied,ignore=shutil.ignore_patterns('__pycache__','*.pyc'))
    p=subprocess.run(parts,cwd=copied/'model',capture_output=True,text=True,timeout=120,env={**os.environ,'PYTHONDONTWRITEBYTECODE':'1'})
   (evidence/'model-tests.stdout.log').write_text(p.stdout);(evidence/'model-tests.stderr.log').write_text(p.stderr)
   check('model_tests_rerun',p.returncode==0,{'command':parts,'exit_code':p.returncode,'cwd':str(out/'model')})
  except Exception as e:check('model_tests_rerun',False,str(e))
 svg=list((out/'drawings').rglob('*.svg'));check('at_least_three_svg',len(svg)>=3,len(svg))
 for p in svg:
  try:
   root=ET.parse(p).getroot();text=p.read_text();valid=root.tag.endswith('svg')
   check('svg_parse:'+p.name,valid)
   check('svg_concept_mark:'+p.name,('概念' in text or 'concept' in text.lower()))
  except Exception as e:check('svg_parse:'+p.name,False,str(e))
 scad=list((out/'drawings').rglob('*.scad'));check('openscad_source',bool(scad))
 executable=Path('/Applications/OpenSCAD.app/Contents/MacOS/OpenSCAD')
 if executable.exists() and scad:
  with tempfile.TemporaryDirectory(prefix='runtime-value-scad-') as temp:
   for p in scad:
    result=subprocess.run([str(executable),'-o',str(Path(temp)/(p.stem+'.csg')),str(p)],capture_output=True,text=True,timeout=60,env={**os.environ,'QT_QPA_PLATFORM':'offscreen'})
    (evidence/(p.stem+'-openscad.log')).write_text(result.stdout+result.stderr)
    check('openscad_compile:'+p.name,result.returncode==0,{'exit_code':result.returncode})
 elif scad:
  declared=json.dumps(m.get('not_run',[]),ensure_ascii=False).lower()
  check('openscad_not_run_declared',('openscad' in declared or '.scad' in declared),{'status':'NOT_RUN','reason':'OpenSCAD executable absent; registered contract permits explicit NOT_RUN','binary':str(executable)})
 try:
  app=resolve(m.get('app_entry','app/index.html'))
  check('app_entry_present',app.is_file(),str(app))
 except Exception as e:check('app_entry_present',False,str(e))
 result={'case':case,'at':dt.datetime.now(dt.timezone.utc).isoformat(),'checks':checks,'browser_acceptance':'NOT_RUN','artifact_checks_passed':all(c['passed'] for c in checks),'passed':False,'scope':'backend artifact checks; independent browser manipulation/export and traceability review remain required'}
 (evidence/'artifact-checks.json').write_text(json.dumps(result,ensure_ascii=False,indent=2)+'\n')
 return result
if __name__=='__main__':
 p=argparse.ArgumentParser();p.add_argument('case',choices=['N','C','M','M2']);args=p.parse_args();r=verify(args.case);print(json.dumps(r,ensure_ascii=False,indent=2))
