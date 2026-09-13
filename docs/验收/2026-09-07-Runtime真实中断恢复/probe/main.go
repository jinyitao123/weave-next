// Runtime member acceptance driver: delegates inference, tools, leases and
// completion to the unchanged production Executor/API/daemon/CLI path.
package main

import (
 "context"
 "encoding/json"
 "fmt"
 "net/http"
 "os"
 "os/signal"
 "path/filepath"
 "strings"
 "syscall"
 "time"

 "github.com/jinyitao123/loom/pgstore"
 "github.com/jinyitao123/weave/internal/app/api"
 "github.com/jinyitao123/weave/internal/base/db"
 "github.com/jinyitao123/weave/internal/base/execution"
 "github.com/jinyitao123/weave/internal/kernel/fanout"
 "github.com/jinyitao123/weave/internal/base/snapshot"
 "github.com/jinyitao123/weave/internal/kernel/taskqueue"
 "github.com/jinyitao123/weave/internal/kernel/config"
 "github.com/jinyitao123/weave/internal/kernel/engine"
 "github.com/jinyitao123/weave/internal/kernel/registry"
 "github.com/jinyitao123/weave/internal/kernel/runtimes"
)
type settings struct { Root string `json:"root"`; Inputs string `json:"inputs"`; DatabaseURL string `json:"database_url"`; Port string `json:"port"`; RuntimeID string `json:"runtime_id"` }
func must(err error){if err!=nil {fmt.Fprintln(os.Stderr,err);os.Exit(1)}}
func read(path string,v any){b,e:=os.ReadFile(path);must(e);must(json.Unmarshal(b,v))}
func write(path string,v any){b,e:=json.MarshalIndent(v,"","  ");must(e);must(os.MkdirAll(filepath.Dir(path),0700));must(os.WriteFile(path,append(b,'\n'),0600))}
func main(){
 if len(os.Args)<3 {panic("usage: driver init|serve|run SETTINGS [case [result-filename]]")}
 var cfg settings; read(os.Args[2],&cfg)
 store,err:=pgstore.New(cfg.DatabaseURL);must(err);defer store.Close()
 ctx,stop:=signal.NotifyContext(context.Background(),syscall.SIGINT,syscall.SIGTERM);defer stop()
 switch os.Args[1] {
 case "init":
  must(store.Migrate(ctx));must(db.Migrate(ctx,store.Pool()))
  initialize(ctx,store,cfg,os.Args[2])
 case "serve":
  c,err:=config.Load();must(err);c.Port=cfg.Port;c.DatabaseURL=cfg.DatabaseURL
  s:=api.NewServer(c,store,nil);s.Tasks=taskqueue.New(store.Pool(),nil,time.Minute);s.Runtimes=runtimes.NewStore(store.Pool());s.Fanout=fanout.New(store.Pool(),fanout.RealClock{})
  s.ConfigureTeamRunWorkers() // wires actual activity/recovery handlers; does not start team workflow workers
  _,err=s.Tasks.RecoverStale(ctx);must(err)
  go func(){ticker:=time.NewTicker(30*time.Second);defer ticker.Stop();for{select{case<-ctx.Done():return;case<-ticker.C:_,e:=s.Tasks.RecoverStale(ctx);if e!=nil{fmt.Fprintln(os.Stderr,e)}}}}()
  go func(){<-ctx.Done();shutdown,cancel:=context.WithTimeout(context.Background(),5*time.Second);defer cancel();_ = s.Echo.Shutdown(shutdown)}()
  err=s.Echo.Start("127.0.0.1:"+cfg.Port);if err!=nil&&err!=http.ErrServerClosed{must(err)}
 case "run":
  if len(os.Args)<4 {panic("case required")}; name:=os.Args[3]; if name!="N"&&name!="C"&&name!="M"&&name!="M2" {panic("invalid case")}
  var rec registry.AgentRecord;read(filepath.Join(cfg.Inputs,"agent-record.json"),&rec);rec.RuntimeID=cfg.RuntimeID;rec.RuntimePolicyMode="strict_pin"
  prompt,err:=os.ReadFile(filepath.Join(cfg.Inputs,"member-prompt.txt"));must(err);contract,err:=os.ReadFile(filepath.Join(cfg.Inputs,"delivery-contract.txt"));must(err)
  snap:="runtime-value-"+name
  call:=execution.WithInvocationID(ctx,snap+"/engineering/0")
  call=execution.WithNodeID(call,"engineering")
  call=execution.WithInputTaskIDs(call,[]string{snap+"-lead",snap+"-reference"})
  call=execution.WithAttemptLineage(call,snap+"/engineering/0","")
  taskID:=execution.EngineTaskID(rec.WorkspaceID,execution.InvocationID(call))
  started:=time.Now().UTC()
  meta:=map[string]any{"pid":os.Getpid(),"started_at":started,"invocation_id":execution.InvocationID(call),"task_id":taskID,"snapshot_id":snap,"case":name}
  filename:="driver-result.json";if len(os.Args)>4{filename=os.Args[4]}; if filepath.Base(filename)!=filename{panic("invalid result name")}
  write(filepath.Join(cfg.Root,"cases",name,filename+".start.json"),meta)
  result,runErr:=runtimes.NewExecutor(taskqueue.New(store.Pool(),nil,time.Minute),runtimes.NewStore(store.Pool()),"","").ExecRemote(call,rec.WorkspaceID,&rec,execution.AgentExecutionStamp{AgentID:rec.ID,AgentVersion:rec.Version,ExecutionScope:execution.ScopeTeamWorkerLeaf,RunSnapshotID:snap},string(prompt)+"\n"+string(contract),nil)
  meta["finished_at"]=time.Now().UTC();meta["elapsed_seconds"]=time.Since(started).Seconds();meta["result"]=result
  if runErr!=nil{meta["error"]=runErr.Error()}
  write(filepath.Join(cfg.Root,"cases",name,filename),meta)
  if runErr!=nil{fmt.Fprintln(os.Stderr,runErr);os.Exit(1)}
  fmt.Println("completed",name,taskID)
 default:panic("unknown mode")
 }
}
func initialize(ctx context.Context,store *pgstore.PGStore,cfg settings,cfgPath string){
 pool:=store.Pool();var rec registry.AgentRecord;read(filepath.Join(cfg.Inputs,"agent-record.json"),&rec)
 _,err:=pool.Exec(ctx,`INSERT INTO weave_workspaces(id,slug,name) VALUES($1,$1,'Runtime recovery acceptance')`,rec.WorkspaceID);must(err)
 spec,err:=json.Marshal(rec.Spec);must(err)
 _,err=pool.Exec(ctx,`INSERT INTO weave_agents(id,workspace_id,name,role,spec) VALUES($1,$2,$3,'worker',$4::jsonb)`,rec.ID,rec.WorkspaceID,rec.Name,string(spec));must(err)
 _,err=pool.Exec(ctx,`INSERT INTO weave_agent_versions(agent_id,workspace_id,version,spec) VALUES($1,$2,$3,$4::jsonb)`,rec.ID,rec.WorkspaceID,rec.Version,string(spec));must(err)
 _,err=pool.Exec(ctx,`INSERT INTO weave_teams(id,workspace_id,name) VALUES('runtime-value-team',$1,'Runtime value fixture')`,rec.WorkspaceID);must(err)
 runtime,token,err:=runtimes.NewStore(pool).Create(ctx,rec.WorkspaceID,"Runtime value isolated Mac");must(err);cfg.RuntimeID=runtime.ID;write(cfgPath,cfg)
 must(os.WriteFile(filepath.Join(cfg.Root,"runtime-token"),[]byte(token),0600))
 var upstream []runtimes.InputFile;read(filepath.Join(cfg.Inputs,"upstream-files.json"),&upstream)
 var references []struct{Path string `json:"path"`};read(filepath.Join(cfg.Inputs,"reference-files.json"),&references)
 queue:=taskqueue.New(pool,nil,time.Minute)
 for _,name:=range []string{"N","C","M"}{
  snap:="runtime-value-"+name
  _,err=snapshot.NewStore(pool).Create(ctx,snapshot.TeamRunSnapshot{RunID:snap,WorkspaceID:rec.WorkspaceID,TeamID:"runtime-value-team",SnapshotSchemaVersion:2,Mode:"free_collab",LeadAvatarID:rec.ID,LeadAvatarVersion:rec.Version,WorkerVersions:json.RawMessage(`{}`),TeamWorkerSnapshot:json.RawMessage(`[]`),InlineDependencies:json.RawMessage(`{}`),RuntimeAssignment:json.RawMessage(`{}`),AdmissionDecision:json.RawMessage(`{"schema_version":1,"team_active":true,"workflow_active":null,"workers_enabled":true,"version_blocked":null,"decided_at":"2026-09-07T00:00:00Z"}`),RunAssociations:json.RawMessage(`{"schema_version":1,"parent_run_id":null,"source_snapshot_id":null,"task_group_id":null}`),TriggerSourceV2:json.RawMessage(`{"schema_version":1,"type":"api","source_ref":"runtime-acceptance-fixture"}`) });must(err)
  _,err=pool.Exec(ctx,`INSERT INTO weave_team_runs(workspace_id,run_id,status,team_id,workflow_id,workflow_version,run_snapshot_id,source_kind,source_task_id,establish_idempotency_key,current_executor_id,created_at,updated_at) VALUES($1,$2,'running','runtime-value-team','member-audit-fixture',1,$2,'api',$2,$2,'acceptance-driver',NOW(),NOW())`,rec.WorkspaceID,snap);must(err)
  for _,node:=range []string{"lead","reference"}{
   artifacts:=[]engine.Artifact{}
   if node=="lead" {for _,f:=range upstream {artifacts=append(artifacts,engine.Artifact{Path:strings.TrimPrefix(f.Path,"lead/"),Content:f.Content,ContentType:f.ContentType})}} else {for _,f:=range references {b,e:=os.ReadFile(filepath.Join(cfg.Inputs,f.Path));must(e);artifacts=append(artifacts,engine.Artifact{Path:strings.TrimPrefix(f.Path,"reference/"),Content:string(b),ContentType:"text/plain"})}}
   payload,_:=json.Marshal(runtimes.EngineExecRequest{Engine:engine.Claude,NodeID:node})
   id:=snap+"-"+node
   must(queue.Enqueue(ctx,&taskqueue.Task{ID:id,WorkspaceID:rec.WorkspaceID,Agent:rec.Name,AgentID:rec.ID,AgentVersion:rec.Version,IdentityKind:taskqueue.IdentityAgent,IdentitySchemaVersion:2,ExecutionScope:execution.ScopeTeamWorkerLeaf,RunSnapshotID:snap,Kind:"engine_exec",Payload:payload}))
   result,_:=json.Marshal(runtimes.EngineExecResult{Status:"completed",Output:"Imported immutable input fixture; no model executed",Artifacts:artifacts})
   _,err=pool.Exec(ctx,`UPDATE weave_task_queue SET status='completed',result=$2::jsonb,completed_at=NOW(),updated_at=NOW() WHERE id=$1`,id,string(result));must(err)
  }
 }
 fmt.Println("initialized isolated runtime",runtime.ID)
}
