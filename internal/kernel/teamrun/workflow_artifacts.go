package teamrun

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/jinyitao123/loom"
	"github.com/jinyitao123/weave/internal/base/deliverable"
	"github.com/jinyitao123/weave/internal/base/fileartifact"
	"github.com/jinyitao123/weave/internal/kernel/taskqueue"
)

// Only artifacts belonging to the exact selected physical results can become
// final deliverables. No runtime host filesystem is consulted on recovery.
func (r *WorkflowSerialRuntime) workflowArtifactLoader(run TeamRun) func(context.Context, []string) ([]deliverable.WorkflowArtifact, error) {
	return func(ctx context.Context, ids []string) ([]deliverable.WorkflowArtifact, error) {
		var artifacts []deliverable.WorkflowArtifact
		seenTasks, seenPaths := map[string]bool{}, map[string]int{}
		for _, id := range ids {
			if seenTasks[id] {
				continue
			}
			seenTasks[id] = true
			files, source, err := r.workflowArtifactSource(ctx, run, id)
			if err != nil {
				return nil, err
			}
			for _, artifact := range files {
				if index, exists := seenPaths[artifact.Path]; exists {
					previous := &artifacts[index]
					if previous.Content != artifact.Content || previous.ContentType != artifact.ContentType {
						return nil, fmt.Errorf("conflicting final file %q", artifact.Path)
					}
					previous.Sources = append(previous.Sources, source)
					continue
				}
				seenPaths[artifact.Path] = len(artifacts)
				artifacts = append(artifacts, deliverable.WorkflowArtifact{Path: artifact.Path, ContentType: artifact.ContentType, Content: artifact.Content, Sources: []deliverable.ArtifactSource{source}})
			}
		}
		return artifacts, nil
	}
}

// Sources are also retained for selected results that export no files.
func (r *WorkflowSerialRuntime) workflowArtifactObservationLoader(run TeamRun) func(context.Context, []string) ([]deliverable.SourceObservation, error) {
	return func(ctx context.Context, ids []string) ([]deliverable.SourceObservation, error) {
		var sources []deliverable.SourceObservation
		seen := map[string]bool{}
		for _, id := range ids {
			if seen[id] {
				continue
			}
			seen[id] = true
			_, source, collection, err := r.workflowArtifactSourceEvidence(ctx, run, id)
			if err != nil {
				return nil, err
			}
			sources = append(sources, deliverable.SourceObservation{Source: source, Collection: collection})
		}
		return sources, nil
	}
}

func (r *WorkflowSerialRuntime) workflowArtifactSourceEvidence(ctx context.Context, run TeamRun, id string) ([]fileartifact.File, deliverable.ArtifactSource, *fileartifact.CollectionEvidence, error) {
	source := deliverable.ArtifactSource{ParentRunID: run.RunID, RunSnapshotID: run.RunSnapshotID}
	if memberID, ok := strings.CutPrefix(id, "member:"); ok {
		source.MemberRunID = memberID
		files, digest, err := r.completedMemberArtifactResult(ctx, run, memberID)
		source.ResultDigest = digest
		return files, source, nil, err
	}
	if r.Tasks == nil {
		return nil, source, nil, fmt.Errorf("file source task store unavailable")
	}
	task, err := r.Tasks.Get(ctx, run.WorkspaceID, id)
	if err != nil {
		return nil, source, nil, err
	}
	if task == nil || task.ID != id || task.WorkspaceID != run.WorkspaceID || task.RunSnapshotID != run.RunSnapshotID || task.Kind != "engine_exec" || task.Status != taskqueue.StatusCompleted {
		return nil, source, nil, fmt.Errorf("file source %s does not belong to this completed execution", id)
	}
	var result struct {
		Status             string                           `json:"status"`
		Error              string                           `json:"error"`
		Artifacts          []fileartifact.File              `json:"artifacts"`
		ArtifactCollection *fileartifact.CollectionEvidence `json:"artifact_collection"`
	}
	if err := json.Unmarshal(task.Result, &result); err != nil {
		return nil, source, nil, err
	}
	if (result.Status != "" && result.Status != "completed") || result.Error != "" {
		return nil, source, nil, fmt.Errorf("file source %s has no completed engine result", id)
	}
	if err := fileartifact.Validate(result.Artifacts); err != nil {
		return nil, source, nil, err
	}
	if err := fileartifact.CollectionError(result.ArtifactCollection); err != nil {
		return nil, source, nil, err
	}
	digest, err := workflowResultDigest(task.Result)
	if err != nil {
		return nil, source, nil, err
	}
	source.TaskID, source.ResultDigest = task.ID, digest
	return result.Artifacts, source, result.ArtifactCollection, nil
}

func (r *WorkflowSerialRuntime) workflowArtifactSource(ctx context.Context, run TeamRun, id string) ([]fileartifact.File, deliverable.ArtifactSource, error) {
	files, source, _, err := r.workflowArtifactSourceEvidence(ctx, run, id)
	return files, source, err
}

// Canonicalize persisted JSON without converting large integers to float64.
func workflowResultDigest(raw json.RawMessage) (string, error) {
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return "", fmt.Errorf("artifact source result is null")
	}
	return deliverable.CanonicalJSONDigest(raw)
}

func (r *WorkflowSerialRuntime) workflowDeliveryRecorder(run TeamRun) func(context.Context, string, string, string, any, []deliverable.WorkflowArtifact, []deliverable.SourceObservation, *deliverable.OutputSelection) error {
	if r == nil || r.OutputRecorder == nil {
		return nil
	}
	recorder, ok := r.OutputRecorder.(interface {
		RecordVerifiedWorkflowOutputs(context.Context, []deliverable.WorkflowOutput, deliverable.VerificationFence) (deliverable.VerificationReport, error)
	})
	if !ok {
		return nil
	}
	return func(ctx context.Context, nodeID, nodeLabel, nodeType string, output any, artifacts []deliverable.WorkflowArtifact, observations []deliverable.SourceObservation, selection *deliverable.OutputSelection) error {
		base := deliverable.WorkflowOutput{WorkspaceID: run.WorkspaceID, RunID: run.RunID, RunSnapshotID: run.RunSnapshotID, NodeID: nodeID, NodeLabel: nodeLabel, NodeType: nodeType, Final: true, CreatedAt: r.now()}
		bundle := make([]deliverable.WorkflowOutput, 0, len(artifacts)+1)
		for _, artifact := range artifacts {
			item := base
			copy := artifact
			item.Artifact = &copy
			bundle = append(bundle, item)
		}
		base.Output = output
		base.SourceObservations = observations
		for _, observation := range observations {
			base.Sources = append(base.Sources, observation.Source)
		}
		base.Selection = selection
		bundle = append(bundle, base)
		executorID := ""
		if run.CurrentExecutorID != nil {
			executorID = *run.CurrentExecutorID
		}
		_, err := recorder.RecordVerifiedWorkflowOutputs(ctx, bundle, deliverable.VerificationFence{
			WorkspaceID: run.WorkspaceID, RunID: run.RunID, RunSnapshotID: run.RunSnapshotID,
			TeamRunGeneration: int64(run.Generation), ExecutionLeaseEpoch: int64(run.ExecutionLeaseEpoch),
			ResumeGeneration: int64(run.ResumeGeneration), ExecutorID: executorID,
		})
		return err
	}
}

// Resolve only an immutable completed member result in this team's snapshot.
func (r *WorkflowSerialRuntime) completedMemberArtifactResult(ctx context.Context, run TeamRun, memberID string) ([]fileartifact.File, string, error) {
	if r.Transactions == nil {
		return nil, "", fmt.Errorf("member artifact store unavailable")
	}
	tx, err := r.Transactions.Begin(ctx)
	if err != nil {
		return nil, "", err
	}
	defer tx.Rollback(ctx)
	var raw []byte
	if err := tx.QueryRow(ctx, `SELECT result FROM weave_workflow_member_runs WHERE workspace_id=$1 AND parent_run_id=$2 AND run_snapshot_id=$3 AND member_run_id=$4 AND result IS NOT NULL`, run.WorkspaceID, run.RunID, run.RunSnapshotID, memberID).Scan(&raw); err != nil {
		return nil, "", err
	}
	var stored struct {
		Result loom.RunResult `json:"result"`
		Error  string         `json:"error"`
	}
	if err := json.Unmarshal(raw, &stored); err != nil {
		return nil, "", err
	}
	if stored.Error != "" || stored.Result.RunID != memberID || stored.Result.StopReason != loom.StopCompleted {
		return nil, "", fmt.Errorf("member artifact source is not a completed result")
	}
	files, err := fileartifact.MemberFiles(stored.Result.State)
	if err != nil {
		return nil, "", err
	}
	digest, err := workflowResultDigest(raw)
	return files, digest, err
}
