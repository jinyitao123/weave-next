// Package conversation persists product-facing conversations, messages, and read state.
package conversation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
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
	ReadOnly        bool      `json:"read_only,omitempty"`
	// BuildRun is the latest build-run summary attached by the list/read path
	// when a persisted intent exists. It is read-only and never
	// persisted directly to the conversations table.
	BuildRun *BuildRunSummary `json:"build_run,omitempty"`
}

// BuildRunSummary carries the minimal latest build-run facts needed to
// render business-phase state in the conversation list sidebar without an
// extra round-trip per conversation.
type BuildRunSummary struct {
	BuildRunID string `json:"build_run_id"`
	Status     string `json:"status"`
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

// Unread is the unread counter for one user's conversation.
type Unread struct {
	WorkspaceID    string    `json:"workspace_id"`
	UserID         string    `json:"user_id"`
	ConversationID string    `json:"conversation_id"`
	UnreadCount    int       `json:"unread_count"`
	LastMessageID  string    `json:"last_message_id,omitempty"`
	UpdatedAt      time.Time `json:"updated_at"`
}

// ThreadSummary describes one reply thread rooted in a parent conversation message.
type ThreadSummary struct {
	ThreadID        string     `json:"thread_id"`
	ParentMessageID string     `json:"parent_message_id"`
	ThreadTitle     string     `json:"thread_title"`
	ReplyCount      int        `json:"reply_count"`
	LastReplyAt     *time.Time `json:"last_reply_at"`
	CreatedAt       time.Time  `json:"created_at"`
}

// FlaggedMessage is one user flag joined with its message and conversation context.
type FlaggedMessage struct {
	ID                string    `json:"id"`
	MessageID         string    `json:"message_id"`
	MessageContent    string    `json:"message_content"`
	MessageRole       string    `json:"message_role"`
	MessageCreatedAt  time.Time `json:"message_created_at"`
	ConversationID    string    `json:"conversation_id"`
	ConversationTitle string    `json:"conversation_title"`
	AgentID           string    `json:"agent_id"`
	AgentName         string    `json:"agent_name"`
	AgentAvatarURL    *string   `json:"agent_avatar_url"`
	FlaggedAt         time.Time `json:"flagged_at"`
}

var (
	// ErrConversationNotFound reports a conversation outside the requested scope.
	ErrConversationNotFound = errors.New("conversation not found")
	// ErrMessageNotFound reports a message outside the requested scope.
	ErrMessageNotFound = errors.New("message not found")
	// ErrProjectAgentMismatch reports a client Agent that disagrees with Project ownership.
	ErrProjectAgentMismatch = errors.New("project avatar does not match conversation agent")
	// ErrInvalidConversationTitle reports an empty or oversized title.
	ErrInvalidConversationTitle = errors.New("conversation title must contain 1 to 80 characters")
	// ErrConversationThreadImmutable reports an attempt to rename a reply thread.
	ErrConversationThreadImmutable = errors.New("conversation threads cannot be renamed")
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

// RenameConversation updates a user-owned root conversation title.
func (s *Store) RenameConversation(
	ctx context.Context, workspaceID, conversationID, userID, title string,
) (Conversation, error) {
	title = normalizeConversationTitle(title)
	if title == "" || len([]rune(title)) > 80 {
		return Conversation{}, ErrInvalidConversationTitle
	}
	row := s.pool.QueryRow(ctx, `
		UPDATE weave_conversations
		SET title=$4, updated_at=$5
		WHERE workspace_id=$1 AND id=$2 AND user_id=$3
		  AND parent_message_id IS NULL
		RETURNING `+conversationColumns,
		workspaceID, conversationID, userID, title, s.clock.Now())
	conversation, err := scanConversation(row)
	if err == nil {
		return conversation, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return Conversation{}, fmt.Errorf("rename conversation: %w", err)
	}

	var isThread bool
	err = s.pool.QueryRow(ctx, `
		SELECT parent_message_id IS NOT NULL
		FROM weave_conversations
		WHERE workspace_id=$1 AND id=$2 AND user_id=$3
	`, workspaceID, conversationID, userID).Scan(&isThread)
	if errors.Is(err, pgx.ErrNoRows) {
		return Conversation{}, ErrConversationNotFound
	}
	if err != nil {
		return Conversation{}, fmt.Errorf("classify conversation rename: %w", err)
	}
	if isThread {
		return Conversation{}, ErrConversationThreadImmutable
	}
	return Conversation{}, ErrConversationNotFound
}

// ListConversations lists one user's conversations by most recent activity.
func (s *Store) ListConversations(ctx context.Context, workspaceID, userID string) ([]Conversation, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT `+conversationColumns+`
		FROM weave_conversations
		WHERE workspace_id=$1 AND user_id=$2
		  AND parent_message_id IS NULL
		ORDER BY updated_at DESC, id
	`, workspaceID, userID)
	if err != nil {
		return nil, fmt.Errorf("list conversations: %w", err)
	}
	defer rows.Close()

	conversations := make([]Conversation, 0)
	for rows.Next() {
		conversation, err := scanConversation(rows)
		if err != nil {
			return nil, fmt.Errorf("scan conversation: %w", err)
		}
		conversations = append(conversations, conversation)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list conversations: %w", err)
	}
	return conversations, nil
}

// ListProjectConversations lists all root conversations in one workspace Project.
func (s *Store) ListProjectConversations(
	ctx context.Context,
	workspaceID, projectID string,
) ([]Conversation, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT `+conversationColumns+`
		FROM weave_conversations
		WHERE workspace_id=$1 AND project_id=$2
		  AND parent_message_id IS NULL
		ORDER BY updated_at DESC, id
	`, workspaceID, strings.TrimSpace(projectID))
	if err != nil {
		return nil, fmt.Errorf("list project conversations: %w", err)
	}
	defer rows.Close()

	conversations := make([]Conversation, 0)
	for rows.Next() {
		conversation, err := scanConversation(rows)
		if err != nil {
			return nil, fmt.Errorf("scan project conversation: %w", err)
		}
		conversations = append(conversations, conversation)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list project conversations: %w", err)
	}
	return conversations, nil
}

// ActiveBuildRunReader is the narrow read surface the conversation store
// needs to attach build-run summaries in batch.
type ActiveBuildRunReader interface {
	GetBuildRunSummary(ctx context.Context, workspaceID, conversationID string) (string, string, error)
}

// AttachBuildRunSummaries enumerates conversations whose intent is non-empty
// and for each conversation queries the latest build-run summary in a single
// loop. Conversations without any build run keep their BuildRun field nil.
// The caller must pass a reader implemented by teambuild.Store so the
// conversation package has no compile-time dependency on teambuild.
func AttachBuildRunSummaries(
	ctx context.Context,
	conversations []Conversation,
	reader ActiveBuildRunReader,
) {
	if len(conversations) == 0 || reader == nil {
		return
	}
	for i := range conversations {
		c := &conversations[i]
		if strings.TrimSpace(c.Intent) == "" {
			continue
		}
		buildRunID, status, err := reader.GetBuildRunSummary(ctx, c.WorkspaceID, c.ID)
		if err != nil || buildRunID == "" {
			continue
		}
		c.BuildRun = &BuildRunSummary{
			BuildRunID: buildRunID,
			Status:     status,
		}
	}
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

// UpdateEventMessage replaces one event message in place and invalidates the
// containing conversation's cached content in the same transaction.
func (s *Store) UpdateEventMessage(
	ctx context.Context,
	workspaceID, conversationID, messageID, content string,
	metadata json.RawMessage,
) error {
	now := s.clock.Now()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin update event message: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	tag, err := tx.Exec(ctx, `
		UPDATE weave_messages
		SET content=$4, metadata=$5
		WHERE workspace_id=$1 AND conversation_id=$2 AND id=$3 AND role='event'
	`, workspaceID, conversationID, messageID, content, nullableJSON(metadata))
	if err != nil {
		return fmt.Errorf("update event message: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("event message %q not found", messageID)
	}

	tag, err = tx.Exec(ctx, `
		UPDATE weave_conversations
		SET content_version=content_version+1, updated_at=$3
		WHERE workspace_id=$1 AND id=$2
	`, workspaceID, conversationID, now)
	if err != nil {
		return fmt.Errorf("bump conversation content version: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("conversation %q not found", conversationID)
	}
	if err := touchConversationProjectActivity(ctx, tx, workspaceID, conversationID, now); err != nil {
		return fmt.Errorf("update project activity: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit event message update: %w", err)
	}
	return nil
}

// MergeAssistantMessageMetadataFields merges a small set of derived metadata
// fields into one assistant message without changing its user-visible content.
// It is used for post-stream projections such as UI execution timelines.
func (s *Store) MergeAssistantMessageMetadataFields(
	ctx context.Context,
	workspaceID, conversationID, messageID string,
	fields map[string]any,
) (Message, error) {
	now := s.clock.Now()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Message{}, fmt.Errorf("begin update assistant message metadata: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var current Message
	err = tx.QueryRow(ctx, `
		SELECT id, seq, conversation_id, workspace_id, role, content,
			COALESCE(parent_message_id, ''), metadata, COALESCE(event_id, ''),
			COALESCE(lease_epoch, 0), created_at
		FROM weave_messages
		WHERE workspace_id=$1 AND conversation_id=$2 AND id=$3 AND role='assistant'
		FOR UPDATE
	`, workspaceID, conversationID, messageID).Scan(
		&current.ID, &current.Seq, &current.ConversationID, &current.WorkspaceID,
		&current.Role, &current.Content, &current.ParentMessageID, &current.Metadata,
		&current.EventID, &current.LeaseEpoch, &current.CreatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return Message{}, fmt.Errorf("assistant message %q not found", messageID)
	}
	if err != nil {
		return Message{}, fmt.Errorf("lock assistant message metadata: %w", err)
	}

	next := map[string]any{}
	if len(current.Metadata) > 0 {
		if err := json.Unmarshal(current.Metadata, &next); err != nil {
			return Message{}, fmt.Errorf("decode assistant message metadata: %w", err)
		}
	}
	for key, value := range fields {
		next[key] = value
	}
	metadata, err := json.Marshal(next)
	if err != nil {
		return Message{}, fmt.Errorf("encode assistant message metadata: %w", err)
	}

	if _, err := tx.Exec(ctx, `
		UPDATE weave_messages
		SET metadata=$4
		WHERE workspace_id=$1 AND conversation_id=$2 AND id=$3 AND role='assistant'
	`, workspaceID, conversationID, messageID, nullableJSON(metadata)); err != nil {
		return Message{}, fmt.Errorf("update assistant message metadata: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE weave_conversations
		SET content_version=content_version+1, updated_at=$3
		WHERE workspace_id=$1 AND id=$2
	`, workspaceID, conversationID, now); err != nil {
		return Message{}, fmt.Errorf("bump conversation content version: %w", err)
	}
	if err := touchConversationProjectActivity(ctx, tx, workspaceID, conversationID, now); err != nil {
		return Message{}, fmt.Errorf("update project activity: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return Message{}, fmt.Errorf("commit assistant message metadata update: %w", err)
	}
	current.Metadata = metadata
	return current, nil
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

// ListMessagesBefore lists messages older than a sequence or message ID cursor from newest to oldest.
func (s *Store) ListMessagesBefore(ctx context.Context, workspaceID, conversationID, before string, limit int) ([]Message, error) {
	if limit <= 0 {
		limit = 50
	}
	beforeSeq, err := strconv.ParseInt(before, 10, 64)
	if err != nil {
		err = s.pool.QueryRow(ctx, `
			SELECT seq
			FROM weave_messages
			WHERE workspace_id=$1 AND conversation_id=$2 AND id=$3
		`, workspaceID, conversationID, before).Scan(&beforeSeq)
		if errors.Is(err, pgx.ErrNoRows) {
			return []Message{}, nil
		}
		if err != nil {
			return nil, fmt.Errorf("resolve message cursor: %w", err)
		}
	}
	rows, err := s.pool.Query(ctx, `
		SELECT `+messageColumns+`
		FROM weave_messages
		WHERE workspace_id=$1 AND conversation_id=$2 AND seq < $3
		ORDER BY seq DESC
		LIMIT $4
	`, workspaceID, conversationID, beforeSeq, limit)
	if err != nil {
		return nil, fmt.Errorf("list messages before cursor: %w", err)
	}
	defer rows.Close()

	messages := make([]Message, 0)
	for rows.Next() {
		message, err := scanMessage(rows)
		if err != nil {
			return nil, fmt.Errorf("scan message before cursor: %w", err)
		}
		messages = append(messages, message)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list messages before cursor: %w", err)
	}
	return messages, nil
}

// ListEventMessagesBySessionKey returns the event messages (e.g. dispatch
// cards) of every conversation bound to one loom session key, oldest first.
// Read-only: it mirrors nothing and writes nothing.
func (s *Store) ListEventMessagesBySessionKey(ctx context.Context, workspaceID, sessionKey string) ([]Message, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT `+messageColumns+`
		FROM weave_messages
		WHERE workspace_id=$1 AND role='event' AND conversation_id IN (
			SELECT id FROM weave_conversations
			WHERE workspace_id=$1 AND session_key=$2
		)
		ORDER BY seq
	`, workspaceID, sessionKey)
	if err != nil {
		return nil, fmt.Errorf("list event messages by session key: %w", err)
	}
	defer rows.Close()

	messages := make([]Message, 0)
	for rows.Next() {
		message, err := scanMessage(rows)
		if err != nil {
			return nil, fmt.Errorf("scan event message: %w", err)
		}
		messages = append(messages, message)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list event messages by session key: %w", err)
	}
	return messages, nil
}

// CreateThread creates one idempotent thread conversation for a parent message.
func (s *Store) CreateThread(
	ctx context.Context,
	workspaceID, conversationID, userID, parentMessageID string,
) (Conversation, bool, error) {
	now := s.clock.Now()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Conversation{}, false, fmt.Errorf("begin create thread: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var projectID, agentID, channel, intent string
	err = tx.QueryRow(ctx, `
		SELECT project_id, agent_id, channel, intent
		FROM weave_conversations
		WHERE workspace_id=$1 AND id=$2 AND user_id=$3
	`, workspaceID, conversationID, userID).Scan(&projectID, &agentID, &channel, &intent)
	if errors.Is(err, pgx.ErrNoRows) {
		return Conversation{}, false, ErrConversationNotFound
	}
	if err != nil {
		return Conversation{}, false, fmt.Errorf("read parent conversation: %w", err)
	}

	var rootContent string
	err = tx.QueryRow(ctx, `
		SELECT content
		FROM weave_messages
		WHERE workspace_id=$1 AND conversation_id=$2 AND id=$3
	`, workspaceID, conversationID, parentMessageID).Scan(&rootContent)
	if errors.Is(err, pgx.ErrNoRows) {
		return Conversation{}, false, ErrMessageNotFound
	}
	if err != nil {
		return Conversation{}, false, fmt.Errorf("read thread parent message: %w", err)
	}

	threadID := uuid.NewString()
	row := tx.QueryRow(ctx, `
		INSERT INTO weave_conversations (
			id, workspace_id, project_id, agent_id, user_id, title, channel, intent,
			parent_message_id, thread_title, created_at, updated_at
		)
		VALUES ($1, $2, $3, $4, $5, '', $6, $7, $8, $9, $10, $10)
		ON CONFLICT (workspace_id, parent_message_id) WHERE parent_message_id IS NOT NULL
		DO NOTHING
		RETURNING `+conversationColumns,
		threadID, workspaceID, projectID, agentID, userID, channel, intent, parentMessageID,
		threadTitle(rootContent), now)
	thread, err := scanConversation(row)
	created := err == nil
	if errors.Is(err, pgx.ErrNoRows) {
		thread, err = scanConversation(tx.QueryRow(ctx, `
			SELECT `+conversationColumns+`
			FROM weave_conversations
			WHERE workspace_id=$1 AND parent_message_id=$2
		`, workspaceID, parentMessageID))
	}
	if err != nil {
		return Conversation{}, false, fmt.Errorf("create thread conversation: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return Conversation{}, false, fmt.Errorf("commit create thread: %w", err)
	}
	return thread, created, nil
}

// ListThreads lists reply summaries for messages in one workspace-scoped conversation.
func (s *Store) ListThreads(ctx context.Context, workspaceID, conversationID string) ([]ThreadSummary, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT thread.id, thread.parent_message_id, thread.thread_title,
		       COUNT(reply.id)::int, MAX(reply.created_at), thread.created_at
		FROM weave_conversations AS thread
		JOIN weave_messages AS root
		  ON root.workspace_id=thread.workspace_id
		 AND root.id=thread.parent_message_id
		 AND root.conversation_id=$2
		LEFT JOIN weave_messages AS reply
		  ON reply.workspace_id=thread.workspace_id
		 AND reply.conversation_id=thread.id
		WHERE thread.workspace_id=$1 AND thread.parent_message_id IS NOT NULL
		GROUP BY thread.id
		ORDER BY thread.created_at, thread.id
	`, workspaceID, conversationID)
	if err != nil {
		return nil, fmt.Errorf("list threads: %w", err)
	}
	defer rows.Close()

	items := make([]ThreadSummary, 0)
	for rows.Next() {
		var item ThreadSummary
		if err := rows.Scan(
			&item.ThreadID, &item.ParentMessageID, &item.ThreadTitle,
			&item.ReplyCount, &item.LastReplyAt, &item.CreatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan thread summary: %w", err)
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list threads: %w", err)
	}
	return items, nil
}

// FlagMessage flags a workspace-scoped message for one user and reports whether it was new.
func (s *Store) FlagMessage(ctx context.Context, workspaceID, userID, messageID string) (bool, error) {
	var exists bool
	if err := s.pool.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM weave_messages WHERE workspace_id=$1 AND id=$2
		)
	`, workspaceID, messageID).Scan(&exists); err != nil {
		return false, fmt.Errorf("find message to flag: %w", err)
	}
	if !exists {
		return false, ErrMessageNotFound
	}
	tag, err := s.pool.Exec(ctx, `
		INSERT INTO weave_message_flags (message_id, user_id, workspace_id)
		VALUES ($1, $2, $3)
		ON CONFLICT (message_id, user_id) DO NOTHING
	`, messageID, userID, workspaceID)
	if err != nil {
		return false, fmt.Errorf("flag message: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

// UnflagMessage removes one user's workspace-scoped flag if present.
func (s *Store) UnflagMessage(ctx context.Context, workspaceID, userID, messageID string) error {
	if _, err := s.pool.Exec(ctx, `
		DELETE FROM weave_message_flags
		WHERE workspace_id=$1 AND user_id=$2 AND message_id=$3
	`, workspaceID, userID, messageID); err != nil {
		return fmt.Errorf("unflag message: %w", err)
	}
	return nil
}

// ListFlaggedMessages lists one user's flags newest first and reports another page.
func (s *Store) ListFlaggedMessages(
	ctx context.Context, workspaceID, userID string, limit int,
) ([]FlaggedMessage, bool, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT flag.id, flag.message_id, message.content, message.role,
		       message.created_at, conversation.id, conversation.title,
		       conversation.agent_id, COALESCE(agent.name, ''),
		       NULLIF(agent.spec->>'avatar_url', ''), flag.created_at
		FROM weave_message_flags AS flag
		JOIN weave_messages AS message
		  ON message.workspace_id=flag.workspace_id AND message.id=flag.message_id
		JOIN weave_conversations AS conversation
		  ON conversation.workspace_id=flag.workspace_id AND conversation.id=message.conversation_id
		LEFT JOIN weave_agents AS agent
		  ON agent.workspace_id=conversation.workspace_id AND agent.id=conversation.agent_id
		WHERE flag.workspace_id=$1 AND flag.user_id=$2
		ORDER BY flag.created_at DESC, flag.id DESC
		LIMIT $3
	`, workspaceID, userID, limit+1)
	if err != nil {
		return nil, false, fmt.Errorf("list flagged messages: %w", err)
	}
	defer rows.Close()

	items := make([]FlaggedMessage, 0, limit)
	for rows.Next() {
		var item FlaggedMessage
		if err := rows.Scan(
			&item.ID, &item.MessageID, &item.MessageContent, &item.MessageRole,
			&item.MessageCreatedAt, &item.ConversationID, &item.ConversationTitle,
			&item.AgentID, &item.AgentName, &item.AgentAvatarURL, &item.FlaggedAt,
		); err != nil {
			return nil, false, fmt.Errorf("scan flagged message: %w", err)
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, false, fmt.Errorf("list flagged messages: %w", err)
	}
	hasMore := len(items) > limit
	if hasMore {
		items = items[:limit]
	}
	return items, hasMore, nil
}

// CountFlaggedMessages counts one user's flags in a workspace.
func (s *Store) CountFlaggedMessages(ctx context.Context, workspaceID, userID string) (int, error) {
	var count int
	if err := s.pool.QueryRow(ctx, `
		SELECT COUNT(*)::int
		FROM weave_message_flags
		WHERE workspace_id=$1 AND user_id=$2
	`, workspaceID, userID).Scan(&count); err != nil {
		return 0, fmt.Errorf("count flagged messages: %w", err)
	}
	return count, nil
}

func threadTitle(content string) string {
	title := strings.TrimSpace(content)
	runes := []rune(title)
	if len(runes) > 80 {
		title = string(runes[:80])
	}
	return title
}

func normalizeConversationTitle(content string) string {
	return strings.Join(strings.Fields(content), " ")
}

// MarkRead records the user's last-read message and clears that conversation's unread count.
func (s *Store) MarkRead(ctx context.Context, workspaceID, conversationID, userID, lastMessageID string) error {
	now := s.clock.Now()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin mark read: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	tag, err := tx.Exec(ctx, `
		INSERT INTO weave_conversation_read_state (
			conversation_id, user_id, last_read_message_id, last_read_at
		)
		SELECT conversation.id, $3, NULLIF($4, ''), $5
		FROM weave_conversations AS conversation
		WHERE conversation.workspace_id=$1 AND conversation.id=$2
		ON CONFLICT (conversation_id, user_id) DO UPDATE SET
			last_read_message_id=EXCLUDED.last_read_message_id,
			last_read_at=EXCLUDED.last_read_at
	`, workspaceID, conversationID, userID, lastMessageID, now)
	if err != nil {
		return fmt.Errorf("write conversation read state: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("conversation %q not found", conversationID)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE weave_inbox_unread
		SET unread_count=0, updated_at=$4
		WHERE workspace_id=$1 AND user_id=$2 AND conversation_id=$3
	`, workspaceID, userID, conversationID, now); err != nil {
		return fmt.Errorf("clear conversation unread: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit mark read: %w", err)
	}
	return nil
}

// BumpUnread increments one user's unread counter for a workspace-scoped conversation.
func (s *Store) BumpUnread(ctx context.Context, workspaceID, userID, conversationID, messageID string) error {
	now := s.clock.Now()
	tag, err := s.pool.Exec(ctx, `
		INSERT INTO weave_inbox_unread (
			workspace_id, user_id, conversation_id, unread_count, last_message_id, updated_at
		)
		SELECT $1, $2, conversation.id, 1, NULLIF($4, ''), $5
		FROM weave_conversations AS conversation
		WHERE conversation.workspace_id=$1 AND conversation.id=$3 AND conversation.user_id=$2
		ON CONFLICT (workspace_id, user_id, conversation_id) DO UPDATE SET
			unread_count=weave_inbox_unread.unread_count + 1,
			last_message_id=EXCLUDED.last_message_id,
			updated_at=EXCLUDED.updated_at
	`, workspaceID, userID, conversationID, messageID, now)
	if err != nil {
		return fmt.Errorf("bump conversation unread: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("conversation %q not found", conversationID)
	}
	return nil
}

// ListUnread lists conversations with unread messages for one workspace user.
func (s *Store) ListUnread(ctx context.Context, workspaceID, userID string) ([]Unread, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT workspace_id, user_id, conversation_id, unread_count,
		       COALESCE(last_message_id, ''), updated_at
		FROM weave_inbox_unread
		WHERE workspace_id=$1 AND user_id=$2 AND unread_count > 0
		ORDER BY updated_at DESC, conversation_id
	`, workspaceID, userID)
	if err != nil {
		return nil, fmt.Errorf("list conversation unread: %w", err)
	}
	defer rows.Close()

	unread := make([]Unread, 0)
	for rows.Next() {
		var item Unread
		if err := rows.Scan(
			&item.WorkspaceID, &item.UserID, &item.ConversationID,
			&item.UnreadCount, &item.LastMessageID, &item.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan conversation unread: %w", err)
		}
		unread = append(unread, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list conversation unread: %w", err)
	}
	return unread, nil
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
