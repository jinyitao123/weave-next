from http.server import ThreadingHTTPServer, BaseHTTPRequestHandler
from pathlib import Path
import json,hashlib,math,datetime,threading,time,uuid,os
ROOT=Path(os.environ['LOOM_PROBE_ROOT']); SOURCE=Path(os.environ['LOOM_PROBE_BASELINE']); LOCK=threading.Lock()
def log(event):
 with LOCK:
  with (ROOT/'tool-events.jsonl').open('a') as f:f.write(json.dumps({'at':datetime.datetime.now(datetime.timezone.utc).isoformat(),**event},ensure_ascii=False)+'\n')
def baseline():
 raw=SOURCE.read_bytes()
 # Parse only the exact numeric fields exercised by this bounded experiment.
 import re
 def field(name):
  return float(re.search(r'^\s*'+re.escape(name)+r': ([0-9.eE+-]+)\s*$',raw.decode(),re.M).group(1))
 return {'source':str(SOURCE),'sha256':hashlib.sha256(raw).hexdigest(),'speed_of_light_m_s':field('speed_of_light_m_s'),'distance_ly':field('proxima_distance_ly'),'mass_kg':1000000000,'speeds_c':[0.01,0.03,0.05]}
def calculate():
 b=baseline();return {'baseline_sha256':b['sha256'],'mass_kg':b['mass_kg'],'rows':[{'speed_c':v,'cruise_years':b['distance_ly']/v,'classical_kinetic_energy_j':0.5*b['mass_kg']*(v*b['speed_of_light_m_s'])**2} for v in b['speeds_c']]}
TOOLS=[{'name':n,'description':d,'inputSchema':{'type':'object','properties':{},'additionalProperties':False},'annotations':{'readOnlyHint':True,'destructiveHint':False,'idempotentHint':True,'openWorldHint':False}} for n,d in [('load_baseline','Read the real frozen baseline file and return its SHA-256 and selected constants. Call first.'),('calculate_scenarios','Compute classical kinetic energies and cruise times from that actual baseline for the three scenarios. Call second.'),('verify_results','Independently recompute the table using Decimal arithmetic, check relative errors below 1e-12, and return actual verification evidence. Call third.')]]
class Handler(BaseHTTPRequestHandler):
 protocol_version='HTTP/1.1'
 def log_message(self,*args):pass
 def do_GET(self):
  self.send_response(405);self.send_header('Content-Length','0');self.end_headers()
 def do_DELETE(self):
  self.send_response(200);self.send_header('Content-Length','0');self.end_headers()
 def do_POST(self):
  try:
   req=json.loads(self.rfile.read(int(self.headers.get('Content-Length',0))));method=req.get('method');rid=req.get('id')
   if rid is None:
    self.send_response(202);self.send_header('Content-Length','0');self.end_headers();return
   if method=='initialize': result={'protocolVersion':'2025-03-26','capabilities':{'tools':{'listChanged':False}},'serverInfo':{'name':'weave-loom-real-baseline-probe','version':'1.0.0'}}
   elif method=='tools/list':result={'tools':TOOLS}
   elif method=='ping':result={}
   elif method=='tools/call':
    name=req['params']['name'];call_id=str(uuid.uuid4());log({'event':'started','tool':name,'call_id':call_id})
    if name=='load_baseline':value=baseline()
    elif name=='calculate_scenarios':value=calculate()
    elif name=='verify_results':
     arm=ROOT/'arm-interruption'
     if arm.exists():
      arm.unlink();(ROOT/'interruption-ready.json').write_text(json.dumps({'call_id':call_id,'at':time.time()}));time.sleep(20)
     from decimal import Decimal,localcontext
     table=calculate();checks=[]
     with localcontext() as ctx:
      ctx.prec=40;b=baseline()
      for row in table['rows']:
       v=Decimal(str(row['speed_c']));c=Decimal(str(b['speed_of_light_m_s']));e=Decimal('0.5')*Decimal(str(b['mass_kg']))*(v*c)**2;y=Decimal(str(b['distance_ly']))/v
       error_e=abs(Decimal(str(row['classical_kinetic_energy_j']))-e)/e;error_y=abs(Decimal(str(row['cruise_years']))-y)/y
       checks.append({'speed_c':row['speed_c'],'energy_relative_error':float(error_e),'time_relative_error':float(error_y),'passed':error_e<Decimal('1e-12') and error_y<Decimal('1e-12')})
     value={'method':'40-digit Decimal independent recomputation','checks':checks,'passed':all(x['passed'] for x in checks),'table':table}
    else:raise ValueError('unknown tool')
    value['tool_receipt_id']=call_id;log({'event':'completed','tool':name,'call_id':call_id,'result':value});result={'content':[{'type':'text','text':json.dumps(value,ensure_ascii=False)}],'isError':False}
   else:raise ValueError('unsupported method')
   payload=json.dumps({'jsonrpc':'2.0','id':rid,'result':result},ensure_ascii=False).encode();self.send_response(200);self.send_header('Content-Type','application/json');self.send_header('Content-Length',str(len(payload)));self.end_headers();self.wfile.write(payload)
  except (BrokenPipeError,ConnectionResetError):pass
  except Exception as e:log({'event':'server_error','error':str(e)});self.send_response(500);self.send_header('Content-Length','0');self.end_headers()
if __name__=='__main__':
 print('MCP baseline probe ready on 127.0.0.1:18193',flush=True);ThreadingHTTPServer(('127.0.0.1',18193),Handler).serve_forever()
