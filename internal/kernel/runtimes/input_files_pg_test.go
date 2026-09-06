package runtimes

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/jinyitao123/weave/internal/base/snapshot"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jinyitao123/weave/internal/base/db"
	"github.com/jinyitao123/weave/internal/base/execution"
	"github.com/jinyitao123/weave/internal/base/taskqueue"
	"github.com/jinyitao123/weave/internal/base/testutil"
	"github.com/jinyitao123/weave/internal/kernel/engine"
	"github.com/jinyitao123/weave/internal/kernel/registry"
)

func TestUpstreamFilesCrossRuntimeFromExactDurableReceiptRealPG(t *testing.T) {
	pool := testutil.PostgresPool(t)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
INSERT INTO weave_workspaces(id,slug,name) VALUES('ws','ws','ws');
INSERT INTO weave_agents(id,workspace_id,name,role,spec) VALUES('agent','ws','worker','worker','{}');
INSERT INTO weave_agent_versions(agent_id,workspace_id,version,spec) VALUES('agent','ws',1,'{}');
`); err != nil {
		t.Fatal(err)
	}
	// Use the production parameter protocol for the runtime/task stores;
	// the fixture pool uses simple protocol for its multi-statement seeds.
	cfg := pool.Config()
	cfg.ConnConfig.DefaultQueryExecMode = pgx.QueryExecModeCacheStatement
	runtimePool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer runtimePool.Close()
	store := NewStore(runtimePool)
	var ids []string
	for _, name := range []string{"First", "Backup"} {
		runtime, _, err := store.Create(ctx, "ws", name)
		if err != nil {
			t.Fatal(err)
		}
		if err := store.HelloWithCapabilities(ctx, "ws", runtime.ID, []string{engine.Claude}, []EngineCapability{{Engine: engine.Claude, BinaryPath: "/fixture/claude", BinaryVersion: "fixture", ProtocolVersion: "1", AuthMode: AuthModeOAuth, EndpointClass: "fixture"}}, 1); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `UPDATE weave_runtimes SET pool_id='pool',functional_revision=functional_revision+1 WHERE id=$1`, runtime.ID); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, runtime.ID)
	}
	queue := taskqueue.New(runtimePool, nil, time.Minute)

	if _, err := pool.Exec(ctx, `INSERT INTO weave_teams(id,workspace_id,name) VALUES('team','ws','team')`); err != nil {
		t.Fatal(err)
	}
	_, err = snapshot.NewStore(pool).Create(ctx, snapshot.TeamRunSnapshot{
		RunID: "snapshot", WorkspaceID: "ws", TeamID: "team", SnapshotSchemaVersion: 2, Mode: "free_collab", LeadAvatarID: "agent", LeadAvatarVersion: 1,
		WorkerVersions: json.RawMessage(`{}`), TeamWorkerSnapshot: json.RawMessage(`[]`), InlineDependencies: json.RawMessage(`{}`), RuntimeAssignment: json.RawMessage(`{}`),
		AdmissionDecision: json.RawMessage(`{"schema_version":1,"team_active":true,"workflow_active":null,"workers_enabled":true,"version_blocked":null,"decided_at":"2026-09-05T00:00:00Z"}`),
		RunAssociations:   json.RawMessage(`{"schema_version":1,"parent_run_id":null,"source_snapshot_id":null,"task_group_id":null}`), TriggerSourceV2: json.RawMessage(`{"schema_version":1,"type":"api","source_ref":"fixture"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	workerDone := make(chan error, 1)
	sourceID := ""
	baseline := "version: 1.0.0\nspeeds: [0.05]\n"
	go func() {
		for count := 0; count < 2 && ctx.Err() == nil; {
			for _, runtimeID := range ids {
				workerID := RuntimeWorkerID("ws", runtimeID)
				task, err := queue.Claim(ctx, workerID, taskqueue.ClaimFilter{Kind: "engine_exec", WorkspaceID: "ws", RuntimeID: runtimeID})
				if err != nil {
					workerDone <- err
					return
				}
				if task == nil {
					continue
				}
				var request EngineExecRequest
				if err := json.Unmarshal(task.Payload, &request); err != nil {
					workerDone <- err
					return
				}
				result := engine.RunResult{Status: "completed", Output: "reviewed exact baseline"}
				if count == 0 {
					sourceID = task.ID
					result.Output = "outputs/baseline_frozen.yaml"
					result.Artifacts = []engine.Artifact{{Path: "baseline_frozen.yaml", ContentType: "application/yaml", Content: baseline}}
				} else {
					if runtimeID != ids[1] || len(request.InputFiles) != 1 || request.InputFiles[0].TaskID != sourceID || request.InputFiles[0].Path != "lead/baseline_frozen.yaml" || request.InputFiles[0].Content != baseline || !strings.Contains(request.Prompt, "inputs/lead/baseline_frozen.yaml") {
						workerDone <- fmt.Errorf("downstream did not receive exact source: %+v", request.InputFiles)
						return
					}
					if err := ValidateInputFiles(request.InputFiles); err != nil {
						workerDone <- err
						return
					}
				}
				raw, _ := json.Marshal(CLIEngineExecResult(result))
				if err := queue.CompleteClaimed(ctx, task.ID, workerID, raw, ""); err != nil {
					workerDone <- err
					return
				}
				count++
			}
			time.Sleep(10 * time.Millisecond)
		}
		workerDone <- nil
	}()
	executor := NewExecutor(queue, store, "", "")
	record := &registry.AgentRecord{ID: "agent", Name: "worker", WorkspaceID: "ws", Version: 1, Engine: engine.Claude, RuntimeID: ids[0], RuntimePolicyMode: "strict_pin"}
	stamp := execution.AgentExecutionStamp{AgentID: "agent", AgentVersion: 1, ExecutionScope: execution.ScopeTeamFreeCollab, RunSnapshotID: "snapshot"}
	sourceCtx := execution.WithNodeID(execution.WithInvocationID(ctx, "snapshot/lead/0"), "lead")
	first, err := executor.ExecRemote(sourceCtx, "ws", record, stamp, "freeze baseline", nil)
	if err != nil {
		t.Fatal(err)
	}
	record.RuntimeID = ids[1]
	downstreamCtx := execution.WithInputTaskIDs(execution.WithNodeID(execution.WithInvocationID(ctx, "snapshot/review/0"), "review"), []string{first.Attempts[0].AttemptID})
	if _, err := executor.ExecRemote(downstreamCtx, "ws", record, stamp, "review", nil); err != nil {
		t.Fatal(err)
	}
	if err := <-workerDone; err != nil {
		t.Fatal(err)
	}
	if _, err := executor.inputFiles(downstreamCtx, "ws", "another-snapshot"); err == nil {
		t.Fatal("cross-run source accepted")
	}
}
