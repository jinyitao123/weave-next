package api

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jinyitao123/weave/internal/base/deliverable"
	"github.com/jinyitao123/weave/internal/kernel/teamrun"
	"github.com/labstack/echo/v4"
)

const (
	// The activity view shows at most this much of each output and this many
	// outputs; neither bounds what is kept.
	developmentTrialStageOutputMaxBytes = 64 * 1024
	developmentTrialStageOutputMaxCount = 64
	// developmentTrialOutputBodyMaxBytes is what one trial output may keep
	// whole. A larger output is recorded as a storage failure.
	developmentTrialOutputBodyMaxBytes = 16 * 1024 * 1024
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
	body         []byte
}

// developmentTrialStageOutputView is one output as the activity shows it.
// Content is a preview when Truncated is set; FullContent says whether the
// whole body can be read: "stored", "storage_failed" (the output was larger
// than a trial may keep) or "not_stored" (recorded before whole bodies were
// kept).
type developmentTrialStageOutputView struct {
	NodeID      string `json:"-"`
	MemberID    string `json:"-"`
	NodeLabel   string `json:"-"`
	OutputID    string `json:"output_id"`
	FullContent string `json:"full_content"`
	Kind        string `json:"kind"`
	Path        string `json:"path,omitempty"`
	ContentType string `json:"content_type"`
	Content     string `json:"content"`
	// ContentEncoding is "base64" when a whole body that is not UTF-8 text
	// is returned; empty otherwise.
	ContentEncoding string    `json:"content_encoding,omitempty"`
	ContentSHA      string    `json:"content_sha256"`
	ContentBytes    int64     `json:"content_bytes"`
	Truncated       bool      `json:"truncated"`
	CreatedAt       time.Time `json:"created_at"`
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

// RecordWorkflowOutputsTx records outputs inside the caller's transaction: a
// development trial's in its own store, any other run's through the
// fallback store's transactional recorder.
func (recorder *developmentTrialWorkflowOutputRecorder) RecordWorkflowOutputsTx(ctx context.Context, tx pgx.Tx, outputs []deliverable.WorkflowOutput) error {
	if recorder == nil || recorder.pool == nil {
		return errors.New("workflow output recorder is unavailable")
	}
	var others []deliverable.WorkflowOutput
	for _, output := range outputs {
		trial, found, err := developmentTrialForRun(ctx, tx, output.WorkspaceID, output.RunID)
		if err != nil {
			return fmt.Errorf("resolve development trial output owner: %w", err)
		}
		if found {
			if err := persistDevelopmentTrialStageOutput(ctx, tx, trial, output); err != nil {
				return err
			}
			continue
		}
		others = append(others, output)
	}
	if len(others) == 0 || recorder.fallback == nil {
		return nil
	}
	if fallback, ok := recorder.fallback.(teamrun.WorkflowOutputTxRecorder); ok {
		return fallback.RecordWorkflowOutputsTx(ctx, tx, others)
	}
	for _, output := range others {
		if err := recorder.fallback.RecordWorkflowOutput(ctx, output); err != nil {
			return err
		}
	}
	return nil
}

// developmentTrialQueries is a pool or a transaction.
type developmentTrialQueries interface {
	QueryRow(context.Context, string, ...any) pgx.Row
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
}

func developmentTrialForRun(ctx context.Context, queries developmentTrialQueries, workspaceID, runID string) (developmentTrialRun, bool, error) {
	if pool, isPool := queries.(*pgxpool.Pool); queries == nil || isPool && pool == nil ||
		strings.TrimSpace(workspaceID) == "" || strings.TrimSpace(runID) == "" {
		return developmentTrialRun{}, false, nil
	}
	var trial developmentTrialRun
	err := queries.QueryRow(ctx, `SELECT trial.workspace_id,trial.team_id,trial.request_id::text,trial.actor_id,
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

func persistDevelopmentTrialStageOutput(ctx context.Context, queries developmentTrialQueries, trial developmentTrialRun, output deliverable.WorkflowOutput) error {
	recorded, err := encodeDevelopmentTrialStageOutput(output)
	if err != nil {
		return err
	}
	state, body := "stored", recorded.body
	if len(body) > developmentTrialOutputBodyMaxBytes {
		state, body = "storage_failed", nil
	}
	tag, err := queries.Exec(ctx, `INSERT INTO weave_team_development_trial_output_bodies
		(workspace_id,request_id,output_id,team_id,actor_id,run_id,node_id,member_id,node_label,node_type,
		 kind,path,content_type,content_sha256,content_bytes,body_state,body,created_at)
		SELECT trial.workspace_id,trial.request_id,$7,trial.team_id,trial.actor_id,$8,$9,$10,$11,$12,
		 $13,$14,$15,$16,$17,$18,$19,$20
		FROM weave_team_development_trials AS trial
		WHERE trial.workspace_id=$1 AND trial.team_id=$2 AND trial.request_id=$3::uuid AND trial.actor_id=$4
		  AND trial.revision=$5 AND trial.workflow_id=$6
		ON CONFLICT (workspace_id,request_id,output_id) DO NOTHING`,
		output.WorkspaceID, trial.TeamID, trial.RequestID, trial.ActorID, trial.Revision, trial.WorkflowID,
		recorded.OutputID, recorded.RunID, recorded.NodeID, recorded.MemberID, recorded.NodeLabel, recorded.NodeType,
		recorded.Kind, recorded.Path, recorded.ContentType, recorded.ContentSHA, recorded.ContentBytes, state, body, recorded.CreatedAt)
	if err != nil {
		return fmt.Errorf("persist development trial stage output: %w", err)
	}
	if tag.RowsAffected() == 0 {
		var exists bool
		if err = queries.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM weave_team_development_trial_output_bodies
			WHERE workspace_id=$1 AND request_id=$2::uuid AND output_id=$3)`,
			output.WorkspaceID, trial.RequestID, recorded.OutputID).Scan(&exists); err != nil {
			return fmt.Errorf("check development trial stage output: %w", err)
		}
		if !exists {
			return errors.New("development trial output owner changed")
		}
	}
	return nil
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
	content, truncated := developmentTrialOutputPreview(full)
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
		Truncated: truncated, CreatedAt: createdAt, body: full,
	}, nil
}

// developmentTrialOutputPreview is the part of an output the activity shows:
// at most developmentTrialStageOutputMaxBytes of valid UTF-8, flagged when
// anything was cut or replaced.
func developmentTrialOutputPreview(full []byte) (string, bool) {
	content, truncated := string(full), false
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
	return content, truncated
}

// readDevelopmentTrialStageOutputs returns one trial run's outputs in the
// order they were produced, starting at offset and at most limit of them,
// with the total count and whether the old index dropped outputs. Outputs
// recorded before whole bodies were kept come from the trial row's index.
func readDevelopmentTrialStageOutputs(ctx context.Context, pool *pgxpool.Pool, trial developmentTrialRun, runID string, offset, limit int) ([]developmentTrialStageOutputView, int, bool, error) {
	var raw []byte
	var storedTruncated bool
	err := pool.QueryRow(ctx, `SELECT stage_outputs,stage_outputs_truncated
		FROM weave_team_development_trials
		WHERE workspace_id=$1 AND team_id=$2 AND request_id=$3::uuid AND actor_id=$4
		  AND revision=$5 AND workflow_id=$6`,
		trial.WorkspaceID, trial.TeamID, trial.RequestID, trial.ActorID, trial.Revision, trial.WorkflowID,
	).Scan(&raw, &storedTruncated)
	if err != nil {
		return nil, 0, false, err
	}
	var stored map[string]developmentTrialStageOutput
	if err = json.Unmarshal(raw, &stored); err != nil {
		return nil, 0, false, fmt.Errorf("decode development trial activity outputs: %w", err)
	}
	views := make([]developmentTrialStageOutputView, 0, len(stored))
	for _, output := range stored {
		if output.RunID != runID {
			continue
		}
		full := "stored"
		if output.Truncated {
			full = "not_stored"
		}
		views = append(views, developmentTrialStageOutputView{
			NodeID: output.NodeID, MemberID: output.MemberID, NodeLabel: output.NodeLabel,
			OutputID: output.OutputID, FullContent: full,
			Kind: output.Kind, Path: output.Path, ContentType: output.ContentType,
			Content: output.Content, ContentSHA: output.ContentSHA, ContentBytes: output.ContentBytes,
			Truncated: output.Truncated, CreatedAt: output.CreatedAt,
		})
	}
	rows, err := pool.Query(ctx, `SELECT output_id,node_id,member_id,node_label,kind,path,content_type,
			content_sha256,content_bytes,body_state,COALESCE(substring(body from 1 for $5),''::bytea),created_at
		FROM weave_team_development_trial_output_bodies
		WHERE workspace_id=$1 AND request_id=$2::uuid AND actor_id=$3 AND run_id=$4`,
		trial.WorkspaceID, trial.RequestID, trial.ActorID, runID, developmentTrialStageOutputMaxBytes+4)
	if err != nil {
		return nil, 0, false, fmt.Errorf("read development trial outputs: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var view developmentTrialStageOutputView
		var head []byte
		if err = rows.Scan(&view.OutputID, &view.NodeID, &view.MemberID, &view.NodeLabel, &view.Kind, &view.Path, &view.ContentType,
			&view.ContentSHA, &view.ContentBytes, &view.FullContent, &head, &view.CreatedAt); err != nil {
			return nil, 0, false, fmt.Errorf("scan development trial output: %w", err)
		}
		view.Content, view.Truncated = developmentTrialOutputPreview(head)
		view.Truncated = view.Truncated || int64(len(head)) < view.ContentBytes
		views = append(views, view)
	}
	if err = rows.Err(); err != nil {
		return nil, 0, false, fmt.Errorf("read development trial outputs: %w", err)
	}
	sort.Slice(views, func(i, j int) bool {
		if views[i].CreatedAt.Equal(views[j].CreatedAt) {
			if views[i].NodeID == views[j].NodeID {
				return views[i].OutputID < views[j].OutputID
			}
			return views[i].NodeID < views[j].NodeID
		}
		return views[i].CreatedAt.Before(views[j].CreatedAt)
	})
	total := len(views)
	offset = min(max(offset, 0), total)
	views = views[offset:]
	if len(views) > limit {
		views = views[:limit]
	}
	return views, total, storedTruncated, nil
}

// listDevelopmentTrialStageOutputs is the activity's view: the first page of
// outputs, partial when an output is shown as a preview or more outputs
// exist than the page holds.
func listDevelopmentTrialStageOutputs(ctx context.Context, pool *pgxpool.Pool, trial developmentTrialRun, runID string) ([]developmentTrialStageOutputView, bool, error) {
	views, total, legacyOverflow, err := readDevelopmentTrialStageOutputs(ctx, pool, trial, runID, 0, developmentTrialStageOutputMaxCount)
	if err != nil {
		return nil, false, err
	}
	partial := legacyOverflow || total > len(views)
	for _, view := range views {
		partial = partial || view.Truncated
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

// handleListDevelopmentTrialOutputs pages through every output of one trial
// run, beyond the first page the activity shows.
func (s *Server) handleListDevelopmentTrialOutputs(c echo.Context) error {
	trial, ok, err := s.developmentTrialOfRun(c)
	if err != nil || !ok {
		return err
	}
	offset := 0
	if raw := c.QueryParam("offset"); raw != "" {
		if offset, err = strconv.Atoi(raw); err != nil || offset < 0 {
			return c.JSON(http.StatusBadRequest, map[string]string{"error": "offset_invalid"})
		}
	}
	views, total, legacyOverflow, err := readDevelopmentTrialStageOutputs(c.Request().Context(), s.GetPool(), trial, c.Param("id"), offset, developmentTrialStageOutputMaxCount)
	if err != nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": "run_read_unavailable"})
	}
	return c.JSON(http.StatusOK, map[string]any{
		"outputs": views, "offset": offset, "total": total,
		"next_offset": func() any {
			if next := offset + len(views); next < total {
				return next
			}
			return nil
		}(),
		// Outputs that an earlier version dropped once a trial had 64 of them.
		"earlier_outputs_dropped": legacyOverflow,
	})
}

// handleGetDevelopmentTrialOutput returns one trial output whole.
func (s *Server) handleGetDevelopmentTrialOutput(c echo.Context) error {
	trial, ok, err := s.developmentTrialOfRun(c)
	if err != nil || !ok {
		return err
	}
	ctx := c.Request().Context()
	var view developmentTrialStageOutputView
	var body []byte
	err = s.GetPool().QueryRow(ctx, `SELECT output_id,kind,path,content_type,content_sha256,content_bytes,body_state,body,created_at
		FROM weave_team_development_trial_output_bodies
		WHERE workspace_id=$1 AND request_id=$2::uuid AND actor_id=$3 AND run_id=$4 AND output_id=$5`,
		trial.WorkspaceID, trial.RequestID, trial.ActorID, c.Param("id"), c.Param("output"),
	).Scan(&view.OutputID, &view.Kind, &view.Path, &view.ContentType, &view.ContentSHA, &view.ContentBytes, &view.FullContent, &body, &view.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		// An output recorded before whole bodies were kept has only its preview.
		views, _, _, readErr := readDevelopmentTrialStageOutputs(ctx, s.GetPool(), trial, c.Param("id"), 0, math.MaxInt)
		if readErr != nil {
			return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": "run_read_unavailable"})
		}
		for _, legacy := range views {
			if legacy.OutputID == c.Param("output") {
				return c.JSON(http.StatusOK, legacy)
			}
		}
		return c.JSON(http.StatusNotFound, map[string]string{"error": "output_not_found"})
	}
	if err != nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": "run_read_unavailable"})
	}
	if view.FullContent == "stored" {
		view.Content = string(body)
		if !utf8.Valid(body) {
			// Bytes that are not text travel as base64 so none is altered.
			view.Content = base64.StdEncoding.EncodeToString(body)
			view.ContentEncoding = "base64"
		}
	}
	return c.JSON(http.StatusOK, view)
}

// developmentTrialOfRun resolves the trial behind the run in the path for a
// developer of this workspace. A run that is not a development trial is not
// found here; the response is written when ok is false.
func (s *Server) developmentTrialOfRun(c echo.Context) (developmentTrialRun, bool, error) {
	if !callerAtLeast(c, roleDeveloper) {
		return developmentTrialRun{}, false, c.JSON(http.StatusForbidden, map[string]string{"error": "insufficient permissions"})
	}
	trial, found, err := developmentTrialForRun(c.Request().Context(), s.GetPool(), getTenant(c), c.Param("id"))
	if err != nil {
		return developmentTrialRun{}, false, c.JSON(http.StatusServiceUnavailable, map[string]string{"error": "run_read_unavailable"})
	}
	if !found {
		return developmentTrialRun{}, false, c.JSON(http.StatusNotFound, map[string]string{"error": "run_not_found"})
	}
	return trial, true, nil
}
