package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jinyitao123/weave/internal/base/deliverable"
	"github.com/jinyitao123/weave/internal/kernel/teamrun"
)

const (
	developmentTrialStageOutputMaxBytes = 64 * 1024
	developmentTrialStageOutputMaxCount = 64
)

type developmentTrialRun struct {
	WorkspaceID     string
	TeamID          string
	RequestID       string
	ActorID         string
	Revision        int64
	WorkflowID      string
	WorkflowVersion int
}

type developmentTrialStageOutput struct {
	OutputID     string    `json:"output_id"`
	RunID        string    `json:"run_id"`
	NodeID       string    `json:"node_id"`
	MemberID     string    `json:"member_id,omitempty"`
	NodeLabel    string    `json:"node_label"`
	NodeType     string    `json:"node_type"`
	Kind         string    `json:"kind"`
	Path         string    `json:"path,omitempty"`
	ContentType  string    `json:"content_type"`
	Content      string    `json:"content"`
	ContentSHA   string    `json:"content_sha256"`
	ContentBytes int64     `json:"content_bytes"`
	Truncated    bool      `json:"truncated"`
	CreatedAt    time.Time `json:"created_at"`
}

type developmentTrialStageOutputView struct {
	NodeID       string    `json:"-"`
	MemberID     string    `json:"-"`
	NodeLabel    string    `json:"-"`
	Kind         string    `json:"kind"`
	Path         string    `json:"path,omitempty"`
	ContentType  string    `json:"content_type"`
	Content      string    `json:"content"`
	ContentSHA   string    `json:"content_sha256"`
	ContentBytes int64     `json:"content_bytes"`
	Truncated    bool      `json:"truncated"`
	CreatedAt    time.Time `json:"created_at"`
}

// developmentTrialWorkflowOutputRecorder keeps candidate trial outputs with
// their developer-owned trial. Ordinary workflow outputs continue through the
// existing employee deliverable store.
type developmentTrialWorkflowOutputRecorder struct {
	pool     *pgxpool.Pool
	fallback teamrun.WorkflowOutputRecorder
}

// Preserve the runtime's verified-delivery capability when decorating stage
// recording. Losing this method silently selects its legacy single-output path.
func (recorder *developmentTrialWorkflowOutputRecorder) RecordVerifiedWorkflowOutputs(ctx context.Context, outputs []deliverable.WorkflowOutput, fence deliverable.VerificationFence) (deliverable.VerificationReport, error) {
	if recorder == nil {
		return deliverable.VerificationReport{}, errors.New("verified workflow output recorder unavailable")
	}
	verified, ok := recorder.fallback.(interface {
		RecordVerifiedWorkflowOutputs(context.Context, []deliverable.WorkflowOutput, deliverable.VerificationFence) (deliverable.VerificationReport, error)
	})
	if !ok {
		return deliverable.VerificationReport{}, errors.New("verified workflow output recorder unavailable")
	}
	report, err := verified.RecordVerifiedWorkflowOutputs(ctx, outputs, fence)
	if err != nil {
		return deliverable.VerificationReport{}, err
	}
	trial, found, err := developmentTrialForRun(ctx, recorder.pool, fence.WorkspaceID, fence.RunID)
	if err != nil {
		return deliverable.VerificationReport{}, fmt.Errorf("resolve development trial delivery owner: %w", err)
	}
	if found {
		for _, output := range outputs {
			if err := persistDevelopmentTrialStageOutput(ctx, recorder.pool, trial, output); err != nil {
				return deliverable.VerificationReport{}, err
			}
		}
	}
	return report, nil
}

func (recorder *developmentTrialWorkflowOutputRecorder) RecordWorkflowOutput(ctx context.Context, output deliverable.WorkflowOutput) error {
	if recorder == nil || recorder.pool == nil {
		if recorder != nil && recorder.fallback != nil {
			return recorder.fallback.RecordWorkflowOutput(ctx, output)
		}
		return nil
	}
	trial, found, err := developmentTrialForRun(ctx, recorder.pool, output.WorkspaceID, output.RunID)
	if err != nil {
		return fmt.Errorf("resolve development trial output owner: %w", err)
	}
	if !found {
		if recorder.fallback == nil {
			return nil
		}
		return recorder.fallback.RecordWorkflowOutput(ctx, output)
	}
	return persistDevelopmentTrialStageOutput(ctx, recorder.pool, trial, output)
}

func developmentTrialForRun(ctx context.Context, pool *pgxpool.Pool, workspaceID, runID string) (developmentTrialRun, bool, error) {
	if pool == nil || strings.TrimSpace(workspaceID) == "" || strings.TrimSpace(runID) == "" {
		return developmentTrialRun{}, false, nil
	}
	var trial developmentTrialRun
	err := pool.QueryRow(ctx, `SELECT trial.workspace_id,trial.team_id,trial.request_id::text,trial.actor_id,
		trial.revision,trial.workflow_id,run.workflow_version
		FROM weave_team_runs AS run
		JOIN weave_task_queue AS root
		  ON root.workspace_id=run.workspace_id AND root.id=run.source_task_id
		JOIN weave_team_development_trials AS trial
		  ON trial.workspace_id=root.workspace_id
		 AND root.context_key='development:' || trial.request_id::text
		 AND root.source_ref='team-development:' || trial.team_id
		WHERE run.workspace_id=$1 AND run.run_id=$2 AND run.source_kind='api'
		  AND run.workflow_id=trial.workflow_id`, workspaceID, runID).Scan(
		&trial.WorkspaceID, &trial.TeamID, &trial.RequestID, &trial.ActorID, &trial.Revision, &trial.WorkflowID, &trial.WorkflowVersion)
	if errors.Is(err, pgx.ErrNoRows) {
		return developmentTrialRun{}, false, nil
	}
	if err != nil {
		return developmentTrialRun{}, false, err
	}
	return trial, true, nil
}

func persistDevelopmentTrialStageOutput(ctx context.Context, pool *pgxpool.Pool, trial developmentTrialRun, output deliverable.WorkflowOutput) error {
	recorded, err := encodeDevelopmentTrialStageOutput(output)
	if err != nil {
		return err
	}
	encoded, err := json.Marshal(recorded)
	if err != nil {
		return fmt.Errorf("encode development trial stage output: %w", err)
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin development trial output write: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var raw []byte
	var truncated bool
	err = tx.QueryRow(ctx, `SELECT stage_outputs,stage_outputs_truncated
		FROM weave_team_development_trials
		WHERE workspace_id=$1 AND team_id=$2 AND request_id=$3::uuid AND actor_id=$4
		  AND revision=$5 AND workflow_id=$6 FOR UPDATE`,
		output.WorkspaceID, trial.TeamID, trial.RequestID, trial.ActorID, trial.Revision, trial.WorkflowID,
	).Scan(&raw, &truncated)
	if err != nil {
		return fmt.Errorf("read development trial output record: %w", err)
	}
	outputs := map[string]json.RawMessage{}
	if err = json.Unmarshal(raw, &outputs); err != nil {
		return fmt.Errorf("decode development trial output record: %w", err)
	}
	if _, exists := outputs[recorded.OutputID]; exists {
		return tx.Commit(ctx)
	}
	if len(outputs) >= developmentTrialStageOutputMaxCount {
		tag, updateErr := tx.Exec(ctx, `UPDATE weave_team_development_trials SET stage_outputs_truncated=true
			WHERE workspace_id=$1 AND team_id=$2 AND request_id=$3::uuid AND actor_id=$4
			  AND revision=$5 AND workflow_id=$6`,
			output.WorkspaceID, trial.TeamID, trial.RequestID, trial.ActorID, trial.Revision, trial.WorkflowID)
		if updateErr != nil {
			return fmt.Errorf("mark development trial outputs truncated: %w", updateErr)
		}
		if tag.RowsAffected() != 1 {
			return errors.New("development trial output owner changed")
		}
		return tx.Commit(ctx)
	}
	outputs[recorded.OutputID] = encoded
	updated, err := json.Marshal(outputs)
	if err != nil {
		return fmt.Errorf("encode development trial output index: %w", err)
	}
	tag, err := tx.Exec(ctx, `UPDATE weave_team_development_trials SET stage_outputs=$5::jsonb
		WHERE workspace_id=$1 AND team_id=$2 AND request_id=$3::uuid AND actor_id=$4
		  AND revision=$6 AND workflow_id=$7`,
		output.WorkspaceID, trial.TeamID, trial.RequestID, trial.ActorID, string(updated), trial.Revision, trial.WorkflowID)
	if err != nil {
		return fmt.Errorf("persist development trial stage output: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return errors.New("development trial output owner changed")
	}
	return tx.Commit(ctx)
}

func encodeDevelopmentTrialStageOutput(output deliverable.WorkflowOutput) (developmentTrialStageOutput, error) {
	if strings.TrimSpace(output.WorkspaceID) == "" || strings.TrimSpace(output.RunID) == "" || strings.TrimSpace(output.NodeID) == "" {
		return developmentTrialStageOutput{}, errors.New("development trial output identity is incomplete")
	}
	kind, path, contentType, content := "result", "", "text/markdown", ""
	if artifact := output.Artifact; artifact != nil {
		kind, path, contentType, content = "artifact", strings.TrimSpace(artifact.Path), strings.TrimSpace(artifact.ContentType), artifact.Content
		if contentType == "" {
			contentType = "text/plain"
		}
	} else if text, ok := output.Output.(string); ok {
		content = text
		trimmed := strings.TrimSpace(text)
		switch {
		case deliverable.LooksLikeHTMLDocument(trimmed):
			contentType = "text/html"
		case strings.HasPrefix(strings.ToLower(trimmed), "<svg"):
			contentType = "image/svg+xml"
		}
	} else {
		encoded, err := json.MarshalIndent(output.Output, "", "  ")
		if err != nil {
			return developmentTrialStageOutput{}, fmt.Errorf("encode development trial output: %w", err)
		}
		contentType, content = "application/json", string(encoded)
	}
	full := []byte(content)
	digest := sha256.Sum256(full)
	truncated := false
	if len(content) > developmentTrialStageOutputMaxBytes {
		cut := developmentTrialStageOutputMaxBytes
		for cut > 0 && !utf8.ValidString(content[:cut]) {
			cut--
		}
		content = content[:cut]
		truncated = true
	}
	if !utf8.ValidString(content) {
		content = strings.ToValidUTF8(content, "�")
		truncated = true
	}
	createdAt := output.CreatedAt.UTC()
	if createdAt.IsZero() {
		createdAt = time.Now().UTC()
	}
	identity := strings.Join([]string{output.RunID, output.NodeID, kind, path, hex.EncodeToString(digest[:])}, "\x00")
	outputIDHash := sha256.Sum256([]byte(identity))
	return developmentTrialStageOutput{
		OutputID: hex.EncodeToString(outputIDHash[:]), RunID: output.RunID,
		NodeID: output.NodeID, MemberID: output.AgentID, NodeLabel: output.NodeLabel, NodeType: output.NodeType,
		Kind: kind, Path: path, ContentType: contentType, Content: content,
		ContentSHA: hex.EncodeToString(digest[:]), ContentBytes: int64(len(full)),
		Truncated: truncated, CreatedAt: createdAt,
	}, nil
}

func listDevelopmentTrialStageOutputs(ctx context.Context, pool *pgxpool.Pool, trial developmentTrialRun, runID string) ([]developmentTrialStageOutputView, bool, error) {
	var raw []byte
	var storedTruncated bool
	err := pool.QueryRow(ctx, `SELECT stage_outputs,stage_outputs_truncated
		FROM weave_team_development_trials
		WHERE workspace_id=$1 AND team_id=$2 AND request_id=$3::uuid AND actor_id=$4
		  AND revision=$5 AND workflow_id=$6`,
		trial.WorkspaceID, trial.TeamID, trial.RequestID, trial.ActorID, trial.Revision, trial.WorkflowID,
	).Scan(&raw, &storedTruncated)
	if err != nil {
		return nil, false, err
	}
	var stored map[string]developmentTrialStageOutput
	if err = json.Unmarshal(raw, &stored); err != nil {
		return nil, false, fmt.Errorf("decode development trial activity outputs: %w", err)
	}
	outputs := make([]developmentTrialStageOutput, 0, len(stored))
	for _, output := range stored {
		if output.RunID == runID {
			outputs = append(outputs, output)
		}
	}
	sort.Slice(outputs, func(i, j int) bool {
		if outputs[i].CreatedAt.Equal(outputs[j].CreatedAt) {
			if outputs[i].NodeID == outputs[j].NodeID {
				return outputs[i].OutputID < outputs[j].OutputID
			}
			return outputs[i].NodeID < outputs[j].NodeID
		}
		return outputs[i].CreatedAt.Before(outputs[j].CreatedAt)
	})
	partial := storedTruncated || len(outputs) > developmentTrialStageOutputMaxCount
	if len(outputs) > developmentTrialStageOutputMaxCount {
		outputs = outputs[:developmentTrialStageOutputMaxCount]
	}
	views := make([]developmentTrialStageOutputView, 0, len(outputs))
	for _, output := range outputs {
		views = append(views, developmentTrialStageOutputView{
			NodeID: output.NodeID, MemberID: output.MemberID, NodeLabel: output.NodeLabel,
			Kind: output.Kind, Path: output.Path, ContentType: output.ContentType,
			Content: output.Content, ContentSHA: output.ContentSHA, ContentBytes: output.ContentBytes,
			Truncated: output.Truncated, CreatedAt: output.CreatedAt,
		})
		partial = partial || output.Truncated
	}
	return views, partial, nil
}

func projectDevelopmentTrialStageOutputs(
	members []runActivityMember,
	stages []runActivityStage,
	outputs []developmentTrialStageOutputView,
	partial bool,
) ([]runActivityStage, bool) {
	for _, output := range outputs {
		if output.MemberID != "" {
			for memberIndex := range members {
				if members[memberIndex].AgentID != output.MemberID {
					continue
				}
				stageIndex := -1
				for index := range members[memberIndex].Stages {
					if members[memberIndex].Stages[index].NodeID == output.NodeID {
						stageIndex = index
						break
					}
				}
				if stageIndex < 0 {
					name := strings.TrimSpace(output.NodeLabel)
					if name == "" {
						name = output.NodeID
					}
					members[memberIndex].Stages = append(members[memberIndex].Stages, runActivityMemberStage{
						NodeID: output.NodeID, Name: name, Status: "completed",
						Inputs: []runActivityMemberInputRef{}, Tools: []runActivityTool{},
					})
					stageIndex = len(members[memberIndex].Stages) - 1
				}
				members[memberIndex].Stages[stageIndex].Outputs = append(members[memberIndex].Stages[stageIndex].Outputs, output)
			}
		} else {
			for memberIndex := range members {
				for stageIndex := range members[memberIndex].Stages {
					if members[memberIndex].Stages[stageIndex].NodeID == output.NodeID {
						members[memberIndex].Stages[stageIndex].Outputs = append(members[memberIndex].Stages[stageIndex].Outputs, output)
					}
				}
			}
		}
		stageIndex := -1
		for index := range stages {
			if stages[index].NodeID == output.NodeID {
				stageIndex = index
				break
			}
		}
		if stageIndex < 0 {
			name := strings.TrimSpace(output.NodeLabel)
			if name == "" {
				name = output.NodeID
			}
			stages = append(stages, runActivityStage{NodeID: output.NodeID, Name: name, Status: "completed"})
			stageIndex = len(stages) - 1
		}
		stages[stageIndex].Outputs = append(stages[stageIndex].Outputs, output)
	}
	return stages, partial
}
