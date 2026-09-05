// Package conversation persists the conversation records used by task execution.
package conversation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jinyitao123/weave/internal/app/projects"
)

const defaultChannel = "default"

const (
	// IntentCreateTeam marks conversations started from the product's new-team
	// entry point. Intent is persisted independently from transport channel.
	IntentCreateTeam = "create_team"
)

// Clock supplies timestamps for conversation state transitions.
type Clock interface {
	Now() time.Time
}

// RealClock is the production wall clock.
type RealClock struct{}

// Now returns the current wall-clock time.
func (RealClock) Now() time.Time { return time.Now() }

// Conversation is a product-facing message container.
type Conversation struct {
	ID              string    `json:"id"`
	WorkspaceID     string    `json:"workspace_id"`
	ProjectID       string    `json:"project_id"`
	AgentID         string    `json:"agent_id"`
	UserID          string    `json:"user_id"`
	Title           string    `json:"title"`
	Channel         string    `json:"channel"`
	Intent          string    `json:"intent,omitempty"`
	SessionKey      string    `json:"session_key,omitempty"`
	ParentMessageID string    `json:"parent_message_id,omitempty"`
	ThreadTitle     string    `json:"thread_title,omitempty"`
	ContentVersion  int       `json:"content_version"`
	CreatedAt       time.Time `json:"created_at"`
	UpdatedAt       time.Time `json:"updated_at"`
}

// Message is one user, assistant, or event entry in a conversation.
type Message struct {
	ID              string          `json:"id"`
	Seq             int64           `json:"seq"`
	ConversationID  string          `json:"conversation_id"`
	WorkspaceID     string          `json:"workspace_id"`
	Role            string          `json:"role"`
	Content         string          `json:"content"`
	ParentMessageID string          `json:"parent_message_id,omitempty"`
	Metadata        json.RawMessage `json:"metadata,omitempty"`
	EventID         string          `json:"event_id,omitempty"`
	LeaseEpoch      int64           `json:"lease_epoch,omitempty"`
	CreatedAt       time.Time       `json:"created_at"`
}

var (
	// ErrProjectAgentMismatch reports a client Agent that disagrees with Project ownership.
	ErrProjectAgentMismatch = errors.New("project avatar does not match conversation agent")
)

// Store persists product-facing conversation data.
type Store struct {
	pool  *pgxpool.Pool
	clock Clock
}

// New creates a conversation store.
func New(pool *pgxpool.Pool, clock Clock) *Store {
	if clock == nil {
		clock = RealClock{}
	}
	return &Store{pool: pool, clock: clock}
}

// EnsureConversation returns the default conversation for one workspace, agent, and user.
func (s *Store) EnsureConversation(ctx context.Context, workspaceID, agentID, userID string) (Conversation, error) {
	return s.EnsureConversationChannel(ctx, workspaceID, agentID, userID, defaultChannel)
}

// EnsureConversationChannel returns the channel conversation for one workspace, agent, and user.
func (s *Store) EnsureConversationChannel(ctx context.Context, workspaceID, agentID, userID, channel string) (Conversation, error) {
	project, err := projects.New(s.pool, s.clock).EnsureUnclassified(ctx, workspaceID, agentID)
	if err != nil {
		return Conversation{}, fmt.Errorf("ensure unclassified project: %w", err)
	}
	return s.EnsureProjectConversationChannel(
		ctx, workspaceID, project.ID, agentID, userID, channel,
	)
}

// EnsureProjectConversationChannel returns the root conversation for one Project.
func (s *Store) EnsureProjectConversationChannel(
	ctx context.Context,
	workspaceID, projectID, agentID, userID, channel string,
) (Conversation, error) {
	if channel == "" {
		channel = defaultChannel
	}
	now := s.clock.Now()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Conversation{}, fmt.Errorf("begin ensure conversation: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var projectAvatarID string
	var archivedAt *time.Time
	err = tx.QueryRow(ctx, `
		SELECT avatar_id, archived_at
		FROM weave_projects
		WHERE workspace_id=$1 AND id=$2
		FOR SHARE
	`, workspaceID, projectID).Scan(&projectAvatarID, &archivedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Conversation{}, projects.ErrNotFound
	}
	if err != nil {
		return Conversation{}, fmt.Errorf("lock conversation project: %w", err)
	}
	if archivedAt != nil {
		return Conversation{}, projects.ErrArchived
	}
	if projectAvatarID != agentID {
		return Conversation{}, ErrProjectAgentMismatch
	}

	conversationID := uuid.NewString()
	row := tx.QueryRow(ctx, `
		INSERT INTO weave_conversations (
			id, workspace_id, project_id, agent_id, user_id, channel, root_reuse_key,
			created_at, updated_at
		)
		SELECT $1, $2, $3, agent.id, $5, $6, 'singleton', $7, $7
		FROM weave_agents AS agent
		WHERE agent.workspace_id=$2 AND agent.id=$4
		  AND agent.role='avatar' AND agent.deleted=false
		ON CONFLICT (workspace_id, project_id, agent_id, user_id, channel, root_reuse_key)
			WHERE parent_message_id IS NULL AND root_reuse_key IS NOT NULL
		DO UPDATE SET id=weave_conversations.id
		RETURNING `+conversationColumns+`, (xmax = 0)`,
		conversationID, workspaceID, projectID, agentID, userID, channel, now)
	conversation, inserted, err := scanConversationWithInserted(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return Conversation{}, fmt.Errorf("agent %q not found in workspace %q", agentID, workspaceID)
	}
	if err != nil {
		return Conversation{}, fmt.Errorf("ensure conversation: %w", err)
	}
	if inserted {
		if err := touchProjectActivity(ctx, tx, workspaceID, projectID, now); err != nil {
			return Conversation{}, fmt.Errorf("update project activity: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return Conversation{}, fmt.Errorf("commit ensure conversation: %w", err)
	}
	return conversation, nil
}

// CreateProjectConversationChannel creates an independent root conversation.
// A repeated session key returns the same conversation so first-send retries
// cannot leave duplicate empty roots behind.
func (s *Store) CreateProjectConversationChannel(
	ctx context.Context,
	workspaceID, projectID, agentID, userID, channel, sessionKey string,
	intents ...string,
) (Conversation, error) {
	if channel == "" {
		channel = defaultChannel
	}
	if strings.TrimSpace(sessionKey) == "" {
		return Conversation{}, errors.New("session key is required")
	}
	intent := ""
	if len(intents) > 0 {
		intent = strings.TrimSpace(intents[0])
	}
	if intent != "" && intent != IntentCreateTeam {
		return Conversation{}, errors.New("unsupported conversation intent")
	}
	now := s.clock.Now()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Conversation{}, fmt.Errorf("begin create conversation: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var projectAvatarID string
	var archivedAt *time.Time
	err = tx.QueryRow(ctx, `
		SELECT avatar_id, archived_at
		FROM weave_projects
		WHERE workspace_id=$1 AND id=$2
		FOR SHARE
	`, workspaceID, projectID).Scan(&projectAvatarID, &archivedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Conversation{}, projects.ErrNotFound
	}
	if err != nil {
		return Conversation{}, fmt.Errorf("lock conversation project: %w", err)
	}
	if archivedAt != nil {
		return Conversation{}, projects.ErrArchived
	}
	if projectAvatarID != agentID {
		return Conversation{}, ErrProjectAgentMismatch
	}

	row := tx.QueryRow(ctx, `
		INSERT INTO weave_conversations (
			id, workspace_id, project_id, agent_id, user_id, channel, intent, session_key,
			created_at, updated_at
		)
		SELECT $1, $2, $3, agent.id, $5, $6, $7, $8, $9, $9
		FROM weave_agents AS agent
		WHERE agent.workspace_id=$2 AND agent.id=$4
		  AND agent.role='avatar' AND agent.deleted=false
		ON CONFLICT (workspace_id, session_key)
		DO UPDATE SET id=weave_conversations.id
		RETURNING `+conversationColumns+`, (xmax = 0)`,
		uuid.NewString(), workspaceID, projectID, agentID, userID, channel, intent, sessionKey, now)
	conversation, inserted, err := scanConversationWithInserted(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return Conversation{}, fmt.Errorf("agent %q not found in workspace %q", agentID, workspaceID)
	}
	if err != nil {
		return Conversation{}, fmt.Errorf("create conversation: %w", err)
	}
	if conversation.ProjectID != projectID || conversation.AgentID != agentID ||
		conversation.UserID != userID || conversation.Channel != channel ||
		conversation.Intent != intent ||
		conversation.ParentMessageID != "" {
		return Conversation{}, errors.New("session key already belongs to another conversation")
	}
	if inserted {
		if err := touchProjectActivity(ctx, tx, workspaceID, projectID, now); err != nil {
			return Conversation{}, fmt.Errorf("update project activity: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return Conversation{}, fmt.Errorf("commit create conversation: %w", err)
	}
	return conversation, nil
}

// BindSessionKey stores the unchanged loom session key used by a conversation.
func (s *Store) BindSessionKey(ctx context.Context, workspaceID, conversationID, sessionKey string) (Conversation, error) {
	row := s.pool.QueryRow(ctx, `
		UPDATE weave_conversations
		SET session_key=$3
		WHERE workspace_id=$1 AND id=$2
		RETURNING `+conversationColumns,
		workspaceID, conversationID, sessionKey)
	conversation, err := scanConversation(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return Conversation{}, fmt.Errorf("conversation %q not found", conversationID)
	}
	if err != nil {
		return Conversation{}, fmt.Errorf("bind conversation session: %w", err)
	}
	return conversation, nil
}

// GetConversation returns one workspace-scoped conversation.
func (s *Store) GetConversation(ctx context.Context, workspaceID, conversationID string) (Conversation, error) {
	row := s.pool.QueryRow(ctx, `
		SELECT `+conversationColumns+`
		FROM weave_conversations
		WHERE workspace_id=$1 AND id=$2
	`, workspaceID, conversationID)
	conversation, err := scanConversation(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return Conversation{}, fmt.Errorf("conversation %q not found", conversationID)
	}
	if err != nil {
		return Conversation{}, fmt.Errorf("get conversation: %w", err)
	}
	return conversation, nil
}

// AppendMessage writes a workspace-scoped message and updates its conversation timestamp.
func (s *Store) AppendMessage(ctx context.Context, message Message) (Message, error) {
	if message.ID == "" {
		message.ID = uuid.NewString()
	}
	if (message.EventID == "") != (message.LeaseEpoch == 0) || message.LeaseEpoch < 0 {
		return Message{}, fmt.Errorf("event_id and positive lease_epoch must be supplied together")
	}
	now := s.clock.Now()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Message{}, fmt.Errorf("begin append message: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	row := tx.QueryRow(ctx, `
		INSERT INTO weave_messages (
			id, conversation_id, workspace_id, role, content,
			parent_message_id, metadata, event_id, lease_epoch, created_at
		)
		SELECT $3, conversation.id, $1, $4, $5, NULLIF($6, ''), $7,
			NULLIF($8, ''), NULLIF($9, 0), $10
		FROM weave_conversations AS conversation
		WHERE conversation.workspace_id=$1 AND conversation.id=$2
		ON CONFLICT (workspace_id, conversation_id, event_id)
			WHERE event_id IS NOT NULL DO NOTHING
		RETURNING `+messageColumns,
		message.WorkspaceID, message.ConversationID, message.ID, message.Role,
		message.Content, message.ParentMessageID, nullableJSON(message.Metadata),
		message.EventID, message.LeaseEpoch, now)
	inserted, err := scanMessage(row)
	if errors.Is(err, pgx.ErrNoRows) && message.EventID != "" {
		inserted, err = scanMessage(tx.QueryRow(ctx, `
			SELECT `+messageColumns+` FROM weave_messages
			WHERE workspace_id=$1 AND conversation_id=$2 AND event_id=$3
		`, message.WorkspaceID, message.ConversationID, message.EventID))
		if err == nil {
			if err := tx.Commit(ctx); err != nil {
				return Message{}, fmt.Errorf("commit duplicate append message: %w", err)
			}
			return inserted, nil
		}
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return Message{}, fmt.Errorf("conversation %q not found", message.ConversationID)
	}
	if err != nil {
		return Message{}, fmt.Errorf("append message: %w", err)
	}
	title := ""
	if message.Role == "user" {
		title = normalizeConversationTitle(message.Content)
		if len([]rune(title)) > 80 {
			title = string([]rune(title)[:80])
		}
	}
	if _, err := tx.Exec(ctx, `
		UPDATE weave_conversations
		SET updated_at=$3,
			title=CASE
				WHEN title='' AND parent_message_id IS NULL AND $4<>''
				  AND NOT EXISTS (
					SELECT 1 FROM weave_messages AS prior
					WHERE prior.workspace_id=$1
					  AND prior.conversation_id=$2
					  AND prior.role='user' AND prior.id<>$5
					  AND prior.content ~ '[^[:space:]]'
				  )
				THEN $4
				ELSE title
			END
		WHERE workspace_id=$1 AND id=$2
	`, message.WorkspaceID, message.ConversationID, now, title, inserted.ID); err != nil {
		return Message{}, fmt.Errorf("update conversation activity: %w", err)
	}
	if err := touchConversationProjectActivity(ctx, tx, message.WorkspaceID, message.ConversationID, now); err != nil {
		return Message{}, fmt.Errorf("update project activity: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return Message{}, fmt.Errorf("commit append message: %w", err)
	}
	return inserted, nil
}

// CardRevision returns the persisted revision of one event message.
func (s *Store) CardRevision(
	ctx context.Context,
	workspaceID, conversationID, messageID string,
) (int, error) {
	var metadata json.RawMessage
	err := s.pool.QueryRow(ctx, `
		SELECT metadata
		FROM weave_messages
		WHERE workspace_id=$1 AND conversation_id=$2 AND id=$3 AND role='event'
	`, workspaceID, conversationID, messageID).Scan(&metadata)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, fmt.Errorf("event message %q not found", messageID)
	}
	if err != nil {
		return 0, fmt.Errorf("read event message revision: %w", err)
	}
	var card struct {
		Revision int `json:"revision"`
	}
	if err := json.Unmarshal(metadata, &card); err != nil {
		return 0, fmt.Errorf("decode event message revision: %w", err)
	}
	return card.Revision, nil
}

// UpdateCard implements fan-out card updates while preserving monotonic
// revisions and refusing to unfreeze a terminal card.
func (s *Store) UpdateCard(
	ctx context.Context,
	workspaceID, conversationID, messageID, content string,
	metadata json.RawMessage,
) error {
	now := s.clock.Now()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin update dispatch card: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var currentMetadata json.RawMessage
	err = tx.QueryRow(ctx, `
		SELECT metadata
		FROM weave_messages
		WHERE workspace_id=$1 AND conversation_id=$2 AND id=$3 AND role='event'
		FOR UPDATE
	`, workspaceID, conversationID, messageID).Scan(&currentMetadata)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("event message %q not found", messageID)
	}
	if err != nil {
		return fmt.Errorf("lock dispatch card: %w", err)
	}
	var current, next struct {
		Revision int  `json:"revision"`
		Terminal bool `json:"terminal"`
	}
	if err := json.Unmarshal(currentMetadata, &current); err != nil {
		return fmt.Errorf("decode current dispatch card: %w", err)
	}
	if err := json.Unmarshal(metadata, &next); err != nil {
		return fmt.Errorf("decode next dispatch card: %w", err)
	}
	if current.Terminal && !next.Terminal {
		return nil
	}
	var fields map[string]any
	if err := json.Unmarshal(metadata, &fields); err != nil {
		return fmt.Errorf("decode dispatch card fields: %w", err)
	}
	fields["revision"] = current.Revision + 1
	metadata, err = json.Marshal(fields)
	if err != nil {
		return fmt.Errorf("encode dispatch card fields: %w", err)
	}

	if _, err := tx.Exec(ctx, `
		UPDATE weave_messages
		SET content=$4, metadata=$5
		WHERE workspace_id=$1 AND conversation_id=$2 AND id=$3 AND role='event'
	`, workspaceID, conversationID, messageID, content, metadata); err != nil {
		return fmt.Errorf("update dispatch card: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE weave_conversations
		SET content_version=content_version+1, updated_at=$3
		WHERE workspace_id=$1 AND id=$2
	`, workspaceID, conversationID, now); err != nil {
		return fmt.Errorf("bump dispatch card content version: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit dispatch card update: %w", err)
	}
	return nil
}

// ListMessages lists a conversation's messages from oldest to newest.
func (s *Store) ListMessages(ctx context.Context, workspaceID, conversationID string, limit, offset int) ([]Message, error) {
	if limit <= 0 {
		limit = 50
	}
	if offset < 0 {
		offset = 0
	}
	rows, err := s.pool.Query(ctx, `
		SELECT `+messageColumns+`
		FROM weave_messages
		WHERE workspace_id=$1 AND conversation_id=$2
		ORDER BY seq
		LIMIT $3 OFFSET $4
	`, workspaceID, conversationID, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("list messages: %w", err)
	}
	defer rows.Close()

	messages := make([]Message, 0)
	for rows.Next() {
		message, err := scanMessage(rows)
		if err != nil {
			return nil, fmt.Errorf("scan message: %w", err)
		}
		messages = append(messages, message)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list messages: %w", err)
	}
	return messages, nil
}

func normalizeConversationTitle(content string) string {
	return strings.Join(strings.Fields(content), " ")
}

const conversationColumns = `
	id, workspace_id, project_id, agent_id, user_id, title, channel,
	intent, COALESCE(session_key, ''), COALESCE(parent_message_id, ''), thread_title,
	content_version, created_at, updated_at`

const messageColumns = `
	id, seq, conversation_id, workspace_id, role, content,
	COALESCE(parent_message_id, ''), metadata,
	COALESCE(event_id, ''), COALESCE(lease_epoch, 0), created_at`

type rowScanner interface {
	Scan(dest ...any) error
}

func scanConversation(row rowScanner) (Conversation, error) {
	var conversation Conversation
	err := row.Scan(
		&conversation.ID, &conversation.WorkspaceID, &conversation.ProjectID,
		&conversation.AgentID,
		&conversation.UserID, &conversation.Title, &conversation.Channel,
		&conversation.Intent,
		&conversation.SessionKey, &conversation.ParentMessageID, &conversation.ThreadTitle,
		&conversation.ContentVersion,
		&conversation.CreatedAt, &conversation.UpdatedAt,
	)
	return conversation, err
}

func scanConversationWithInserted(row rowScanner) (Conversation, bool, error) {
	var conversation Conversation
	var inserted bool
	err := row.Scan(
		&conversation.ID, &conversation.WorkspaceID, &conversation.ProjectID,
		&conversation.AgentID,
		&conversation.UserID, &conversation.Title, &conversation.Channel,
		&conversation.Intent,
		&conversation.SessionKey, &conversation.ParentMessageID, &conversation.ThreadTitle,
		&conversation.ContentVersion,
		&conversation.CreatedAt, &conversation.UpdatedAt,
		&inserted,
	)
	return conversation, inserted, err
}

func scanMessage(row rowScanner) (Message, error) {
	var message Message
	err := row.Scan(
		&message.ID, &message.Seq, &message.ConversationID, &message.WorkspaceID,
		&message.Role, &message.Content, &message.ParentMessageID,
		&message.Metadata, &message.EventID, &message.LeaseEpoch, &message.CreatedAt,
	)
	return message, err
}

func touchConversationProjectActivity(
	ctx context.Context,
	tx pgx.Tx,
	workspaceID, conversationID string,
	at time.Time,
) error {
	tag, err := tx.Exec(ctx, `
		UPDATE weave_projects AS project
		SET last_activity_at=GREATEST(COALESCE(project.last_activity_at, project.updated_at), $3)
		FROM weave_conversations AS conversation
		WHERE conversation.workspace_id=$1 AND conversation.id=$2
		  AND project.workspace_id=conversation.workspace_id
		  AND project.id=conversation.project_id
	`, workspaceID, conversationID, at)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("conversation %q project not found", conversationID)
	}
	return nil
}

func touchProjectActivity(ctx context.Context, tx pgx.Tx, workspaceID, projectID string, at time.Time) error {
	tag, err := tx.Exec(ctx, `
		UPDATE weave_projects
		SET last_activity_at=GREATEST(COALESCE(last_activity_at, updated_at), $3)
		WHERE workspace_id=$1 AND id=$2
	`, workspaceID, projectID, at)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return projects.ErrNotFound
	}
	return nil
}

func nullableJSON(value json.RawMessage) any {
	if len(value) == 0 {
		return nil
	}
	return value
}
