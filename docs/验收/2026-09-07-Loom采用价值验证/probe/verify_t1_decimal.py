#!/usr/bin/env python3
"""Post-run independent numerical check, separate from probe's Go verifier."""
from decimal import Decimal as D, getcontext
import hashlib, json, re
from pathlib import Path
getcontext().prec=60
ROOT=Path(__file__).resolve().parents[1]
rows=[]
for result_path in sorted((ROOT/'evidence/batch-02').glob('*/result.json')):
    result=json.loads(result_path.read_text())
    if result['task']!='t1':continue
    case=result_path.parent
    source=case/'workspace/inputs/baseline.json'
    baseline=json.loads(source.read_text(),parse_float=D,parse_int=D)
    calculation=json.loads((case/'workspace/outputs/calculation.json').read_text(),parse_float=D,parse_int=D)
    checks=[]
    for beta,row in zip(baseline['cruise_speed_c'],calculation['rows'],strict=True):
        energy=(baseline['mass_kg']/2)*baseline['speed_of_light_m_s']**2*beta**2
        years=baseline['distance_ly']/beta
        checks.append({'beta':str(beta),'energy_j':str(energy),'cruise_years':str(years),'passed':row['beta']==beta and abs(row['kinetic_energy_j']/energy-1)<D('1e-12') and abs(row['cruise_years']/years-1)<D('1e-12')})
    # Printed numeric values in mixed JSON/Chinese prose are checked independently.
    printed=[D(x) for x in re.findall(r'(?<![A-Za-z0-9_])\d+(?:\.\d+)?(?:[eE][+-]?\d+)?',result['final'])]
    values=[D(c[k]) for c in checks for k in ['energy_j','cruise_years']]
    final_numbers=all(any(abs(n/value-1)<D('1e-12') for n in printed) for value in values)
    passed=all(c['passed'] for c in checks) and final_numbers and calculation['baseline_id']==baseline['baseline_id'] and len(checks)==3 and calculation['mass_kg']==baseline['mass_kg']
    rows.append({'case':case.name,'input_sha256':hashlib.sha256(source.read_bytes()).hexdigest(),'passed':passed,'final_numbers_present_and_correct':final_numbers,'checks':checks,'text_review':'unblinded audit; independent_formula wording in probe receipt is overstated; same Go implementation was used at run time'})
assert len(rows)==4 and all(r['passed'] for r in rows)
(ROOT/'evidence/t1-independent-decimal-audit.json').write_text(json.dumps({'method':'separate Python Decimal 60-digit post-run calculation; not pre-registered blind review','rows':rows},ensure_ascii=False,indent=2)+'\n')
print(json.dumps({'checked':len(rows),'numerically_passed':sum(r['passed'] for r in rows)}))
