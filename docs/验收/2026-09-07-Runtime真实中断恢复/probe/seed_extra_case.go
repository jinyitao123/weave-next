// Create a fresh M2 fixture after the original M ended in unplanned host sleep.
// Original snapshots, receipts, directories and failed results are untouched.
package main

import (
 "context"
 "encoding/json"
 "fmt"
 "os"
 "path/filepath"
 "time"

 "github.com/jinyitao123/loom/pgstore"
 "github.com/jinyitao123/weave/internal/base/snapshot"
 "github.com/jinyitao123/weave/internal/kernel/taskqueue"
 "github.com/jinyitao123/weave/internal/kernel/registry"
)

func main() {
 must := func(err error) { if err != nil { panic(err) } }
 if len(os.Args) != 2 { panic("usage: seed_extra_case SETTINGS") }
 var cfg struct { Inputs string `json:"inputs"`; DatabaseURL string `json:"database_url"` }
 raw, err := os.ReadFile(os.Args[1]); must(err); must(json.Unmarshal(raw, &cfg))
 var rec registry.AgentRecord
 raw, err = os.ReadFile(filepath.Join(cfg.Inputs, "agent-record.json")); must(err); must(json.Unmarshal(raw, &rec))
 store, err := pgstore.New(cfg.DatabaseURL); must(err); defer store.Close()
 ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second); defer cancel()
 snapshots := snapshot.NewStore(store.Pool())
 source, err := snapshots.GetByRunID(ctx, rec.WorkspaceID, "runtime-value-M"); must(err)
 source.RunID, source.CreatedAt = "runtime-value-M2", time.Now().UTC()
 queue := taskqueue.New(store.Pool(), nil, time.Minute)
 tx, err := store.Pool().Begin(ctx); must(err); defer tx.Rollback(ctx)
 _, err = snapshots.CreateTx(ctx, tx, *source); must(err)
 _, err = tx.Exec(ctx, `INSERT INTO weave_team_runs(workspace_id,run_id,status,team_id,workflow_id,workflow_version,run_snapshot_id,source_kind,source_task_id,establish_idempotency_key,current_executor_id,created_at,updated_at) VALUES($1,$2,'running','runtime-value-team','member-audit-fixture',1,$2,'api',$2,$2,'acceptance-driver',NOW(),NOW())`, rec.WorkspaceID, source.RunID); must(err)
 for _, node := range []string{"lead", "reference"} {
  input, err := queue.Get(ctx, rec.WorkspaceID, "runtime-value-M-"+node); must(err)
  if input.Status != taskqueue.StatusCompleted { panic("original immutable input fixture is not completed") }
  result := append(json.RawMessage(nil), input.Result...)
  input.ID, input.RunSnapshotID = source.RunID+"-"+node, source.RunID
  must(queue.EnqueueTx(ctx, tx, input))
  _, err = tx.Exec(ctx, `UPDATE weave_task_queue SET status='completed',result=$2::jsonb,completed_at=NOW(),updated_at=NOW() WHERE id=$1`, input.ID, string(result)); must(err)
 }
 must(tx.Commit(ctx))
 fmt.Println("Created M2 snapshot and the same immutable upstream fixtures; no model executed")
}
