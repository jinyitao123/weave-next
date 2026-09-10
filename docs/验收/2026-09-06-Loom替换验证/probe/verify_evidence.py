from pathlib import Path
import json,hashlib,collections,math
r=Path(__file__).resolve().parents[1]/'evidence'
def read(n):return json.loads((r/n).read_text())
def events(c):return [json.loads(x) for x in (r/(c+'-events.jsonl')).read_text().splitlines()]
checks={}
checks['cli_product_run_succeeded']=read('cli-activity.json')['status']=='succeeded'
checks['loom_product_run_failed']=read('loom-activity.json')['status']=='failed'
frozen=read('loom-frozen-payload.json');worker=next(b for b in frozen['bundles'] if b['agent']['name'].endswith('-physics'))
saved=read('state.json')['physics_loom']
checks['mcp_config_lost_during_freeze']=len(saved['mcp_servers'])==1 and worker['agent']['engine']=='loom' and worker['mcp_bindings']==[]
request=read('loom-first-inference-request.json')['prompt'].split('<loom_llm_request>\n',1)[1].split('\n</loom_llm_request>',1)[0]
checks['product_loom_request_had_no_tools']=not json.loads(request).get('tools')
core=read('toolloop-run-result.json')['result'];checks['kernel_toolloop_completed']=core['StopReason']=='completed'
receipts=[read(p.name) for p in sorted(r.glob('toolloop-inference-*.json'))]
names=[]
for rec in receipts:
 obj=json.loads(rec['output']);names += [c['name'] for c in obj['tool_calls']]
checks['kernel_model_requested_exact_three_tools']=names==['load_baseline','calculate_scenarios','verify_results']
checks['no_native_tool_execution']=all(not x['native_tool_attempt'] and x['status']=='completed' for x in receipts)
checks['memory_restart_failed_as_expected']=read('mem-crash-exit.json')['exit_code']==86 and read('mem-resume-exit.json')['exit_code']==1 and 'checkpoint not found' in read('mem-resume-result.json')['error']
ordinary=collections.Counter(e.get('step') for e in events('pg') if e['kind']=='real_tool_completed')
checks['ordinary_checkpoint_reenters_last_step']=ordinary=={'load':1,'calculate':2,'verify':1} and read('pg-resume-exit.json')['exit_code']==0
safe=collections.Counter(e.get('step') for e in events('pgsafe') if e['kind']=='real_tool_completed')
checks['explicit_safe_point_avoids_duplicate_tools']=safe=={'load':1,'calculate':1,'verify':1}
source=read('pgsafe-resume-result.json');fork=read('pgsafe-fork-result.json');crash=read('pgsafe-crash.json')
checks['safe_resume_is_new_process_same_run']=source['pid']!=crash['pid'] and source['result']['RunID']==crash['run_id'] and source['result']['StopReason']=='completed'
checks['fork_new_identity_and_lineage']=fork['result']['RunID']!=source['result']['RunID'] and fork['result']['State']['__parent_run']==source['result']['RunID'] and fork['result']['State']['__parent_seq']==3 and fork['result']['Steps']==1
checks['fork_reuses_all_tool_receipts']=all(fork['result']['State'][key]==source['result']['State'][key] for key in ['load','calculate','verify'])
checks['fork_regenerates_summary']=fork['result']['State']['output']!=source['result']['State']['output'] and 'scenario_note' in fork['result']['State']
checks['source_checkpoint_unchanged']=read('pgsafe-fork-source-integrity.json')['unchanged']
checks['source_matches_actual_input']=json.loads(source['result']['State']['load'])['sha256']==hashlib.sha256((r/'baseline.yaml').read_bytes()).hexdigest()
verification=json.loads(source['result']['State']['verify'])
checks['numerical_tool_verification']=verification['passed'] and all(x['passed'] and x['energy_relative_error']<1e-12 and x['time_relative_error']<1e-12 for x in verification['checks'])
summary={'checks':checks,'passed':all(checks.values()),'core_report_content_caveat':{'reported_kinetic_relativistic_correction_percent_at_005c':0.09,'independently_computed_percent':100*((1/math.sqrt(1-.05**2)-1)/(.5*.05**2)-1),'assessment':'Tool calculations passed, but this unsolicited statement in the raw model report is incorrect.'},'ordinary_tool_counts':dict(ordinary),'safe_tool_counts':dict(safe),'configured_engine':'claude','kernel_model_receipts':sorted(set(m for x in receipts for m in x['reported_models'])),'product_cli_run':read('state.json')['cli_run']['run_id'],'product_loom_run':read('state.json')['loom_run']['run_id'],'safe_resume_run':source['result']['RunID'],'fork_run':fork['result']['RunID']}
(r/'verification-summary.json').write_text(json.dumps(summary,ensure_ascii=False,indent=2));print(json.dumps(summary,ensure_ascii=False,indent=2));assert summary['passed']
