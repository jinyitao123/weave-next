package teamrun

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jinyitao123/weave/internal/base/deliverable"
	"github.com/jinyitao123/weave/internal/base/fileartifact"
	"github.com/jinyitao123/weave/internal/base/taskqueue"
)

// Only artifacts belonging to the exact selected physical results can become
// final deliverables. No runtime host filesystem is consulted on recovery.
func (r *WorkflowSerialRuntime) workflowArtifactLoader(run TeamRun) func(context.Context, []string) ([]deliverable.WorkflowArtifact, error) {
	return func(ctx context.Context, ids []string) ([]deliverable.WorkflowArtifact, error) {
		var artifacts []deliverable.WorkflowArtifact
		seenTasks, seenPaths := map[string]bool{}, map[string]string{}
		for _, id := range ids {
			if seenTasks[id] {
				continue
			}
			seenTasks[id] = true
			if r.Tasks == nil {
				return nil, fmt.Errorf("file source task store unavailable")
			}
			task, err := r.Tasks.Get(ctx, run.WorkspaceID, id)
			if err != nil {
				return nil, err
			}
			if task.RunSnapshotID != run.RunSnapshotID || task.Kind != "engine_exec" || task.Status != taskqueue.StatusCompleted {
				return nil, fmt.Errorf("file source %s does not belong to this completed execution", id)
			}
			var result struct {
				Artifacts []fileartifact.File `json:"artifacts"`
			}
			if err := json.Unmarshal(task.Result, &result); err != nil {
				return nil, err
			}
			if err := fileartifact.Validate(result.Artifacts); err != nil {
				return nil, err
			}
			for _, artifact := range result.Artifacts {
				if previous, exists := seenPaths[artifact.Path]; exists {
					if previous != artifact.Content {
						return nil, fmt.Errorf("conflicting final file %q", artifact.Path)
					}
					continue
				}
				seenPaths[artifact.Path] = artifact.Content
				artifacts = append(artifacts, deliverable.WorkflowArtifact{Path: artifact.Path, ContentType: artifact.ContentType, Content: artifact.Content})
			}
		}
		return artifacts, nil
	}
}

func (r *WorkflowSerialRuntime) workflowDeliveryRecorder(run TeamRun) func(context.Context, string, string, string, any, []deliverable.WorkflowArtifact) error {
	if r == nil || r.OutputRecorder == nil {
		return nil
	}
	recorder, ok := r.OutputRecorder.(interface {
		RecordWorkflowOutputs(context.Context, []deliverable.WorkflowOutput) error
	})
	if !ok {
		return nil
	}
	return func(ctx context.Context, nodeID, nodeLabel, nodeType string, output any, artifacts []deliverable.WorkflowArtifact) error {
		base := deliverable.WorkflowOutput{WorkspaceID: run.WorkspaceID, RunID: run.RunID, RunSnapshotID: run.RunSnapshotID, NodeID: nodeID, NodeLabel: nodeLabel, NodeType: nodeType, Final: true, CreatedAt: r.now()}
		bundle := make([]deliverable.WorkflowOutput, 0, len(artifacts)+1)
		for _, artifact := range artifacts {
			item := base
			copy := artifact
			item.Artifact = &copy
			bundle = append(bundle, item)
		}
		base.Output = output
		bundle = append(bundle, base)
		return recorder.RecordWorkflowOutputs(ctx, bundle)
	}
}
