package runtimes_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jinyitao123/weave/internal/base/execution"
	"github.com/jinyitao123/weave/internal/base/frozen"
	"github.com/jinyitao123/weave/internal/base/taskqueue"
	"github.com/jinyitao123/weave/internal/base/teamrun"
	"github.com/jinyitao123/weave/internal/kernel/compiler"
	"github.com/jinyitao123/weave/internal/kernel/engine"
	"github.com/jinyitao123/weave/internal/kernel/execenv"
	"github.com/jinyitao123/weave/internal/kernel/registry"
	"github.com/jinyitao123/weave/internal/kernel/runtimes"
	"github.com/jinyitao123/weave/internal/kernel/workflow"
)

type collectedDeliveryExecutor struct{ result engine.RunResult }

func (e collectedDeliveryExecutor) ExecRemote(context.Context, string, *registry.AgentRecord, execution.AgentExecutionStamp, string, []execenv.Attachment) (engine.RunResult, error) {
	return e.result, nil
}

type deliveryArtifactReader struct {
	artifact *workflow.PublishedArtifactContent
}

func (r deliveryArtifactReader) GetArtifact(context.Context, string, string, int) (*workflow.PublishedArtifactContent, error) {
	return r.artifact, nil
}

type noDeliveryExecution struct{}

func (noDeliveryExecution) Build(context.Context, frozen.FrozenExecutionBundle, workflow.RuntimeCredentialResolver) (compiler.FrozenBuildOpts, io.Closer, error) {
	return compiler.FrozenBuildOpts{}, nil, errors.New("delivery resume must not execute an agent")
}

// Exercise the real collector, remote carrier, CLI projection, checkpoint
// decoder, and final workflow boundary without any model or business database.
func TestWorkflowFinalDeliveryUsesCurrentStageFileEvidenceAfterResume(t *testing.T) {
	for _, test := range []struct {
		name  string
		file  bool
		old   bool
		relay bool
	}{
		{name: "final file missing"},
		{name: "final file readable", file: true},
		{name: "earlier stage file cannot satisfy later reference", old: true},
		{name: "transform cannot bypass missing file", relay: true},
		{name: "transform preserves readable file", relay: true, file: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			workDir := t.TempDir()
			file := filepath.Join(workDir, "outputs", "acceptance.md")
			if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
				t.Fatal(err)
			}
			body := "读书会定于2026年9月9日14:30至15:00举行，共12人，其中3人远程；负责人和材料链接待确定。"
			if test.old {
				if err := os.WriteFile(file, []byte("Earlier stage's material"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			before := runtimes.SnapshotOutputArtifacts(workDir)
			if test.file {
				if err := os.WriteFile(file, []byte(body), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			// Deliberately omit creation verbs: final selection, not wording,
			// must enforce whether this file was actually collected.
			result := engine.RunResult{Status: "completed", Output: "最终简报：`outputs/acceptance.md`。"}
			runtimes.CollectRunOutputArtifacts(workDir, before, &result)
			if result.Status != "completed" {
				t.Fatalf("reference should defer to final boundary: %#v", result)
			}
			wire, err := json.Marshal(runtimes.CLIEngineExecResult(result))
			if err != nil {
				t.Fatal(err)
			}
			var remote runtimes.EngineExecResult
			if err := json.Unmarshal(wire, &remote); err != nil {
				t.Fatal(err)
			}
			entry, err := workflow.NewRuntimeCLIEntry(collectedDeliveryExecutor{remote.EngineRunResult()},
				&registry.AgentRecord{WorkspaceID: "workspace-1", ID: "finalizer", Version: 1}, execution.AgentExecutionStamp{})
			if err != nil {
				t.Fatal(err)
			}
			accounted, err := entry.ExecuteAccounted(t.Context(), "finalize")
			if err != nil {
				t.Fatal(err)
			}
			output, err := accounted.TextOutput()
			if err != nil {
				t.Fatal(err)
			}
			encoded, _ := json.Marshal(output)
			checkpoint := teamrun.WorkflowCheckpointV1{
				SchemaVersion: 1, RunID: "run-delivery", NodeID: "deliver", WrittenAt: time.Now(),
				Stamp:            teamrun.WorkflowRunStamp{WorkspaceID: "workspace-1", WorkflowID: "workflow-1", WorkflowVersion: 1, RunSnapshotID: "run-delivery"},
				CompletedOutputs: map[string]json.RawMessage{"lead": json.RawMessage(`"Later deliver outputs/acceptance.md"`), "finalizer": encoded},
				DeliveryErrors:   map[string]string{"lead": `delivery_artifact_uncollected: file_not_collected: "outputs/acceptance.md"`},
			}
			if accounted.DeliveryError != "" {
				checkpoint.DeliveryErrors["finalizer"] = accounted.DeliveryError
			}
			if test.relay {
				checkpoint.NodeID = "relay"
			}
			stored, _ := json.Marshal(checkpoint)
			checkpoint, err = teamrun.DecodeWorkflowCheckpointV1(stored)
			if err != nil {
				t.Fatal(err)
			}
			runtime := deliveryBoundaryRuntime(t, test.relay)
			run := teamrun.TeamRun{WorkspaceID: "workspace-1", RunID: "run-delivery", RunSnapshotID: "run-delivery", WorkflowID: "workflow-1", WorkflowVersion: 1}
			got, err := runtime.ResumeCheckpoint(t.Context(), run, &taskqueue.Task{RunSnapshotID: run.RunSnapshotID, Payload: json.RawMessage(`"business input"`)}, checkpoint)
			if test.file {
				var delivered struct{ Output string }
				if json.Unmarshal(got.Output, &delivered) != nil || err != nil || got.Status != teamrun.RuntimeCompleted || delivered.Output != body {
					t.Fatalf("real file not delivered, or lead's plan blocked delivery: result=%#v err=%v", got, err)
				}
			} else if err == nil || got.Status != teamrun.RuntimeFailed || !strings.Contains(err.Error(), "delivery_artifact_uncollected") {
				t.Fatalf("missing current file passed final boundary: result=%#v err=%v", got, err)
			}
		})
	}
}

func deliveryBoundaryRuntime(t *testing.T, relay bool) *teamrun.WorkflowSerialRuntime {
	t.Helper()
	payload := frozen.ArtifactPayloadV1{
		SchemaVersion: 1, Team: frozen.ArtifactTeamV1{WorkspaceID: "workspace-1", TeamID: "team-1", LeadAgentID: "lead"},
		Bundles: []frozen.FrozenExecutionBundle{}, DeliveryTargets: []frozen.FrozenDeliveryTarget{},
		TriggerConfig: json.RawMessage(`{"schema_version":1,"type":"conversation_explicit","config":{}}`),
		GraphDefinition: json.RawMessage(`{"schema_version":1,"entry_node_id":"finalizer","input_contract":{"type":"text"},"output_contract":{"type":"text"},"nodes":[
		{"id":"finalizer","type":"transform","config":{"operation":"identity","value":{"source":"run_input","path":""}},"output":{"type":"text"}},
		{"id":"deliver","type":"deliver","config":{"result":{"source":"node_output","node_id":"finalizer","path":""}}}],
		"edges":[{"id":"final-deliver","from_node_id":"finalizer","to_node_id":"deliver","route":"success"}]}`),
	}
	if relay {
		graph := string(payload.GraphDefinition)
		graph = strings.Replace(graph, `"node_id":"finalizer"`, `"node_id":"relay"`, 1)
		graph = strings.Replace(graph, `"nodes":[`, `"nodes":[{"id":"relay","type":"transform","config":{"operation":"identity","value":{"source":"node_output","node_id":"finalizer","path":""}},"output":{"type":"text"}},`, 1)
		graph = strings.Replace(graph, `"to_node_id":"deliver"`, `"to_node_id":"relay"`, 1)
		graph = strings.Replace(graph, `"edges":[`, `"edges":[{"id":"relay-deliver","from_node_id":"relay","to_node_id":"deliver","route":"success"},`, 1)
		payload.GraphDefinition = json.RawMessage(graph)
	}
	hash, err := frozen.ComputeArtifactContentHash(frozen.ArtifactEnvelopeHashInputV1{
		WorkspaceID: "workspace-1", WorkflowID: "workflow-1", WorkflowVersion: 1,
		ArtifactSchemaVersion: 1, CanonicalizationAlgorithm: frozen.ArtifactCanonicalizationAlgorithm,
		CanonicalizationVersion: frozen.ArtifactCanonicalizationVersion, HashAlgorithm: frozen.ArtifactHashAlgorithm, Payload: payload,
	})
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := frozen.Canonicalize(payload, frozen.PreorderArtifactPayloadV1)
	if err != nil {
		t.Fatal(err)
	}
	return &teamrun.WorkflowSerialRuntime{
		Artifacts: deliveryArtifactReader{&workflow.PublishedArtifactContent{
			WorkspaceID: "workspace-1", WorkflowID: "workflow-1", WorkflowVersion: 1, ArtifactSchemaVersion: 1,
			CanonicalizationAlgorithm: frozen.ArtifactCanonicalizationAlgorithm, CanonicalizationVersion: frozen.ArtifactCanonicalizationVersion,
			HashAlgorithm: frozen.ArtifactHashAlgorithm, ContentHash: hash, Payload: encoded,
		}},
		Loader: &workflow.RuntimeLoader{}, HostFactory: noDeliveryExecution{},
		CredentialResolvers: func(string) (workflow.RuntimeCredentialResolver, error) { return nil, nil },
	}
}
