// Package deliverable reads immutable final outputs projected from the session outbox.
package deliverable

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var ErrNotFound = errors.New("final deliverable not found")

// FinalDeliverable is one immutable, user-visible final output.
type FinalDeliverable struct {
	ID             string          `json:"id"`
	WorkspaceID    string          `json:"workspace_id"`
	ProjectID      string          `json:"project_id,omitempty"`
	ConversationID string          `json:"conversation_id,omitempty"`
	UserID         string          `json:"user_id"`
	LeadAvatarID   string          `json:"lead_avatar_id"`
	SessionID      string          `json:"session_id"`
	EventID        string          `json:"event_id"`
	RunID          string          `json:"run_id"`
	RunSnapshotID  string          `json:"run_snapshot_id"`
	Title          string          `json:"title"`
	Content        string          `json:"content"`
	ContentType    string          `json:"content_type"`
	Metadata       json.RawMessage `json:"metadata"`
	CreatedAt      time.Time       `json:"created_at"`
}

// WorkflowOutput describes one immutable output emitted by a published
// workflow node. Conversation-triggered workflow outputs are projected into
// the same user-visible artifact ledger as explicitly declared deliverables;
// metadata distinguishes intermediate stages from the final delivery.
type WorkflowOutput struct {
	WorkspaceID   string
	RunID         string
	RunSnapshotID string
	NodeID        string
	NodeLabel     string
	NodeType      string
	AgentID       string
	Output        any
	Artifact      *WorkflowArtifact
	Final         bool
	CreatedAt     time.Time
}

// WorkflowArtifact is one runtime-produced file whose path is relative to the
// worker's reserved outputs/ directory.
type WorkflowArtifact struct {
	Path        string
	ContentType string
	Content     string
}

// ListFilter narrows a workspace-scoped deliverable list.
type ListFilter struct {
	ProjectID      string
	ConversationID string
	RunID          string
	Limit          int
	Offset         int
}

// Store reads immutable final deliverables.
type Store struct {
	pool *pgxpool.Pool
}

// New creates a final deliverable store.
func New(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

// RecordWorkflowOutput persists one output from a user-triggered published
// workflow. Conversation and manual team dispatches create visible artifacts;
// schedule, API validation, and candidate evaluation runs intentionally do not.
// Replays are idempotent by run, node, artifact kind, and content hash.
func (s *Store) RecordWorkflowOutput(ctx context.Context, output WorkflowOutput) error {
	if s == nil || s.pool == nil {
		return errors.New("workflow deliverable store is unavailable")
	}
	output.WorkspaceID = strings.TrimSpace(output.WorkspaceID)
	output.RunID = strings.TrimSpace(output.RunID)
	output.RunSnapshotID = strings.TrimSpace(output.RunSnapshotID)
	output.NodeID = strings.TrimSpace(output.NodeID)
	if output.WorkspaceID == "" || output.RunID == "" || output.RunSnapshotID == "" || output.NodeID == "" {
		return errors.New("workflow deliverable identity is incomplete")
	}

	content, contentType, err := encodeWorkflowOutput(output.Output)
	artifactPath := ""
	if output.Artifact != nil {
		artifactPath = strings.TrimSpace(output.Artifact.Path)
		content = output.Artifact.Content
		contentType = strings.TrimSpace(output.Artifact.ContentType)
		if contentType == "" {
			contentType = "text/plain"
		}
	}
	if err != nil {
		return fmt.Errorf("encode workflow deliverable: %w", err)
	}
	if strings.TrimSpace(content) == "" {
		return nil
	}

	var projectID, triggerType, sourceRef, leadAvatarID string
	err = s.pool.QueryRow(ctx, `
		SELECT COALESCE(snapshot.project_id, ''),
		       snapshot.trigger_source_v2->>'type',
		       snapshot.trigger_source_v2->>'source_ref', team.lead_avatar_id
		FROM weave_team_run_snapshots AS snapshot
		JOIN weave_teams AS team
		  ON team.workspace_id=snapshot.workspace_id AND team.id=snapshot.team_id
		WHERE snapshot.workspace_id=$1 AND snapshot.run_id=$2
		  AND snapshot.mode='fixed_workflow'
	`, output.WorkspaceID, output.RunSnapshotID).Scan(
		&projectID, &triggerType, &sourceRef, &leadAvatarID,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("resolve workflow deliverable trigger: %w", err)
	}
	var conversationID, userID string
	switch triggerType {
	case "conversation_explicit":
		err = s.pool.QueryRow(ctx, `
			SELECT id, user_id FROM weave_conversations
			WHERE workspace_id=$1 AND id=$2 AND parent_message_id IS NULL
		`, output.WorkspaceID, sourceRef).Scan(&conversationID, &userID)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("resolve workflow deliverable conversation: %w", err)
		}
	case "manual":
		userID = strings.TrimSpace(sourceRef)
		if userID == "" || userID == "manual" {
			return nil
		}
	default:
		return nil
	}

	kind := "stage"
	titlePrefix := "阶段产物"
	if output.Final {
		kind = "final"
		titlePrefix = "最终产物"
	}
	label := strings.TrimSpace(output.NodeLabel)
	if label == "" && strings.TrimSpace(output.AgentID) != "" {
		err := s.pool.QueryRow(ctx, `
			SELECT COALESCE(NULLIF(BTRIM(display_name), ''), name)
			FROM weave_agents
			WHERE workspace_id=$1 AND id=$2
		`, output.WorkspaceID, strings.TrimSpace(output.AgentID)).Scan(&label)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("resolve workflow deliverable agent label: %w", err)
		}
	}
	if label == "" {
		label = workflowNodeFallbackLabel(output.NodeType)
	}
	title := titlePrefix + " · " + label
	if artifactPath != "" {
		title = artifactPath
	}
	metadata, err := json.Marshal(map[string]any{
		"source":        "published_workflow",
		"artifact_kind": kind,
		"node_id":       output.NodeID,
		"node_label":    label,
		"node_type":     strings.TrimSpace(output.NodeType),
		"filename":      artifactPath,
	})
	if err != nil {
		return fmt.Errorf("encode workflow deliverable metadata: %w", err)
	}
	contentDigest := sha256.Sum256([]byte(content))
	eventID := "workflow-" + kind + ":" + output.RunID + ":" + output.NodeID + ":" + artifactPath + ":" + hex.EncodeToString(contentDigest[:12])
	idDigest := sha256.Sum256([]byte(strings.Join([]string{
		output.WorkspaceID, output.RunID, eventID,
	}, "\x1f")))
	id := fmt.Sprintf("deliverable_%x", idDigest[:16])
	createdAt := output.CreatedAt.UTC()
	if createdAt.IsZero() {
		createdAt = time.Now().UTC()
	}
	var nullableProjectID any
	if projectID != "" {
		nullableProjectID = projectID
	}
	var nullableConversationID any
	if conversationID != "" {
		nullableConversationID = conversationID
	}
	sessionID := conversationID
	if sessionID == "" {
		sessionID = "workflow:" + output.RunSnapshotID
	}
	_, err = s.pool.Exec(ctx, `
		INSERT INTO weave_final_deliverables (
			id, workspace_id, project_id, conversation_id, user_id, lead_avatar_id,
			session_id, event_id, run_id, run_snapshot_id, title, content,
			content_type, metadata, created_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15)
		ON CONFLICT DO NOTHING
	`, id, output.WorkspaceID, nullableProjectID, nullableConversationID, userID, leadAvatarID,
		sessionID, eventID, output.RunID, output.RunSnapshotID, title, content,
		contentType, metadata, createdAt)
	if err != nil {
		return fmt.Errorf("insert workflow deliverable: %w", err)
	}
	return nil
}

func workflowNodeFallbackLabel(nodeType string) string {
	switch strings.TrimSpace(nodeType) {
	case "lead":
		return "需求统筹"
	case "worker":
		return "协作成员"
	case "transform":
		return "结果整理"
	case "parallel":
		return "并行执行"
	case "join":
		return "结果汇聚"
	case "loop":
		return "校验返修"
	case "deliver":
		return "交付"
	default:
		return "工作流阶段"
	}
}

func encodeWorkflowOutput(output any) (string, string, error) {
	if text, ok := output.(string); ok {
		trimmed := strings.TrimSpace(text)
		switch {
		case LooksLikeHTMLDocument(trimmed):
			return text, "text/html", nil
		case strings.HasPrefix(strings.ToLower(trimmed), "<svg"):
			return text, "image/svg+xml", nil
		default:
			return text, "text/markdown", nil
		}
	}
	encoded, err := json.MarshalIndent(output, "", "  ")
	if err != nil {
		return "", "", err
	}
	return string(encoded), "application/json", nil
}

const deliverableColumns = `
	id, workspace_id, project_id, conversation_id, user_id, lead_avatar_id,
	session_id, event_id, run_id, run_snapshot_id, title, content,
	content_type, metadata, created_at
`

// Get returns one deliverable from one workspace.
func (s *Store) Get(ctx context.Context, workspaceID, id string) (FinalDeliverable, error) {
	deliverable, err := scanDeliverable(s.pool.QueryRow(ctx, `
		SELECT `+deliverableColumns+`
		FROM weave_final_deliverables
		WHERE workspace_id=$1 AND id=$2 AND btrim(content) <> ''
	`, workspaceID, strings.TrimSpace(id)))
	if errors.Is(err, pgx.ErrNoRows) {
		return FinalDeliverable{}, ErrNotFound
	}
	if err != nil {
		return FinalDeliverable{}, fmt.Errorf("get final deliverable: %w", err)
	}
	return deliverable, nil
}

// List returns final deliverables in newest-first order.
func (s *Store) List(
	ctx context.Context,
	workspaceID string,
	filter ListFilter,
) ([]FinalDeliverable, error) {
	if filter.Limit <= 0 || filter.Limit > 200 {
		filter.Limit = 50
	}
	if filter.Offset < 0 {
		filter.Offset = 0
	}
	rows, err := s.pool.Query(ctx, `
		SELECT `+deliverableColumns+`
		FROM weave_final_deliverables
		WHERE workspace_id=$1
		  AND btrim(content) <> ''
		  AND ($2='' OR project_id=$2)
		  AND ($3='' OR conversation_id=$3)
		  AND ($4='' OR run_id=$4)
		ORDER BY created_at DESC, id DESC
		LIMIT $5 OFFSET $6
	`, workspaceID, strings.TrimSpace(filter.ProjectID),
		strings.TrimSpace(filter.ConversationID), strings.TrimSpace(filter.RunID), filter.Limit, filter.Offset)
	if err != nil {
		return nil, fmt.Errorf("list final deliverables: %w", err)
	}
	defer rows.Close()

	items := make([]FinalDeliverable, 0)
	for rows.Next() {
		item, err := scanDeliverable(rows)
		if err != nil {
			return nil, fmt.Errorf("scan final deliverable: %w", err)
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list final deliverable rows: %w", err)
	}
	return items, nil
}

type rowScanner interface {
	Scan(...any) error
}

func scanDeliverable(row rowScanner) (FinalDeliverable, error) {
	var result FinalDeliverable
	var projectID, conversationID *string
	var metadata []byte
	err := row.Scan(
		&result.ID,
		&result.WorkspaceID,
		&projectID,
		&conversationID,
		&result.UserID,
		&result.LeadAvatarID,
		&result.SessionID,
		&result.EventID,
		&result.RunID,
		&result.RunSnapshotID,
		&result.Title,
		&result.Content,
		&result.ContentType,
		&metadata,
		&result.CreatedAt,
	)
	if err != nil {
		return FinalDeliverable{}, err
	}
	if projectID != nil {
		result.ProjectID = *projectID
	}
	if conversationID != nil {
		result.ConversationID = *conversationID
	}
	result.Metadata = append(json.RawMessage(nil), metadata...)
	return result, nil
}
