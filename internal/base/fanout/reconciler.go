package fanout

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jinyitao123/weave/internal/base/execution"
	"github.com/jinyitao123/weave/internal/base/realtime"
	"github.com/jinyitao123/weave/internal/base/taskqueue"
)

// LegEnqueuer accepts the synthesis task produced when a group resolves.
type LegEnqueuer interface {
	EnqueueTx(ctx context.Context, tx pgx.Tx, task *taskqueue.Task) error
}

// CardUpdater persists an in-conversation event card without coupling the
// reconciler to the conversation package.
type CardUpdater interface {
	UpdateCard(ctx context.Context, workspaceID, conversationID, messageID, content string, metadata json.RawMessage) error
}

type cardRevisionReader interface {
	CardRevision(ctx context.Context, workspaceID, conversationID, messageID string) (int, error)
}

// Reconciler closes ready task groups and schedules their synthesis run.
type Reconciler struct {
	store        *Store
	enqueuer     LegEnqueuer
	transactions coordinatorTransactions
	clock        Clock
	cards        CardUpdater
	hub          *realtime.Hub
	revisionMu   sync.Mutex
	revisions    map[string]int
}

// NewReconciler creates the task-group completion reconciler.
func NewReconciler(store *Store, enqueuer LegEnqueuer, cards CardUpdater, hub *realtime.Hub) *Reconciler {
	clock := Clock(RealClock{})
	if store != nil && store.clock != nil {
		clock = store.clock
	}
	return &Reconciler{
		store: store, enqueuer: enqueuer, transactions: store.pool, clock: clock, cards: cards, hub: hub,
		revisions: make(map[string]int),
	}
}

// RefreshCard re-renders the current leg state into the group's event message.
func (r *Reconciler) RefreshCard(ctx context.Context, workspaceID, groupID string) error {
	snapshot, err := r.store.Snapshot(ctx, workspaceID, groupID)
	if err != nil {
		return err
	}
	if snapshot.Group.Status == StatusResolved {
		return nil
	}
	return r.updateCard(ctx, snapshot, false)
}

// ReconcileGroup resolves one ready group. TryFinalize elects exactly one
// caller to enqueue the synthesis task.
func (r *Reconciler) ReconcileGroup(ctx context.Context, workspaceID, groupID string, force bool) error {
	if !force {
		ready, err := r.store.ReadyToResolve(ctx, workspaceID, groupID)
		if err != nil {
			return err
		}
		if !ready {
			return nil
		}
	}

	won, err := r.store.TryFinalize(ctx, workspaceID, groupID)
	if err != nil {
		return err
	}
	if !won {
		existing, err := r.store.Snapshot(ctx, workspaceID, groupID)
		if err != nil {
			return err
		}
		if existing.Group.Status != StatusResolving {
			return nil
		}
	}
	completed, err := r.store.CompletedLegCount(ctx, workspaceID, groupID)
	if err != nil {
		return err
	}
	group, err := r.store.Snapshot(ctx, workspaceID, groupID)
	if err != nil {
		return err
	}
	quorumReached := group.Group.Quorum > 0 && completed >= group.Group.Quorum
	if force || quorumReached {
		if _, err := r.store.CutPending(ctx, workspaceID, groupID); err != nil {
			return err
		}
	}
	snapshot, err := r.store.Snapshot(ctx, workspaceID, groupID)
	if err != nil {
		return err
	}
	completed = completedLegs(snapshot.Legs)
	unsuccessful := len(snapshot.Legs) - completed
	partial := force || unsuccessful > 0 && !quorumReached
	var outcome string
	if partial {
		outcome = fmt.Sprintf("partial: %d/%d succeeded, %d failed or incomplete", completed, len(snapshot.Legs), unsuccessful)
	} else if quorumReached {
		outcome = fmt.Sprintf("quorum reached: %d/%d succeeded, %d failed or incomplete", completed, len(snapshot.Legs), unsuccessful)
	} else {
		outcome = fmt.Sprintf("completed: %d/%d succeeded, %d failed or incomplete", completed, len(snapshot.Legs), unsuccessful)
	}
	snapshot.Group.GroupOutcome = outcome
	completionID, err := deriveLegacyGroupCompletionID(workspaceID, snapshot.Group.ID)
	if err != nil {
		return err
	}
	// The completion consumer derives the originating room and parent run from
	// durable task-group/terminal facts, then commits only through the session
	// outbox. The internal synthesis prompt never becomes a room message.
	payload, err := json.Marshal(taskqueue.ChatExecRequest{
		Agent:      snapshot.Group.AvatarAgent,
		UserID:     snapshot.Group.UserID,
		Message:    synthesisPrompt(snapshot),
		NoDispatch: true,
		// Task-group truth for the delivery path: the executor appends a
		// plain-text provenance line (group id + worker roster + run short
		// code) to the report it lands in the room session. The key literals
		// are duplicated in internal/api/jobs.go on purpose — taskqueue stays
		// free of fan-out vocabulary.
		Context: map[string]any{
			"task_group_id":       snapshot.Group.ID,
			"task_group_workers":  taskGroupWorkerNames(snapshot.Legs),
			"group_completion_id": completionID,
		},
	})
	if err != nil {
		return fmt.Errorf("encode task group synthesis request: %w", err)
	}
	committed, err := r.commitLegacyCompletion(
		ctx, snapshot.Group, outcome, completionID, payload,
	)
	if err != nil {
		return err
	}
	if !committed {
		return nil
	}
	if err := r.updateCard(ctx, snapshot, true); err != nil {
		return err
	}
	return nil
}

func deriveLegacyGroupCompletionID(workspaceID, groupID string) (string, error) {
	if invalidHashField(workspaceID) || invalidHashField(groupID) {
		return "", workflowError(ErrorInvalidRequest, "legacy completion identity must be non-empty")
	}
	return prefixedDigest(
		"fgc1_", "weave/fanout-legacy-completion/v1", workspaceID, groupID,
	), nil
}

func (r *Reconciler) commitLegacyCompletion(
	ctx context.Context,
	group Group,
	outcome string,
	completionID string,
	payload json.RawMessage,
) (bool, error) {
	if r == nil || r.transactions == nil || r.enqueuer == nil {
		return false, fmt.Errorf("task group completion dependencies are unavailable")
	}
	tx, err := r.transactions.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("begin task group completion: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var status string
	if err := tx.QueryRow(ctx, `
		SELECT status
		FROM weave_task_group
		WHERE workspace_id=$1 AND id=$2
		FOR UPDATE
	`, group.WorkspaceID, group.ID).Scan(&status); err != nil {
		return false, fmt.Errorf("lock task group completion: %w", err)
	}
	switch status {
	case StatusResolved:
		return false, nil
	case StatusResolving:
	default:
		return false, fmt.Errorf("task group %q is not resolving", group.ID)
	}

	var agentID string
	var agentVersion int
	var teamFreeCollab bool
	if err := tx.QueryRow(ctx, `
		SELECT completion_identity.agent_id,
			completion_identity.agent_version,
			completion_identity.team_free_collab
		FROM (
			SELECT source.lead_avatar_id AS agent_id,
				source.lead_avatar_version AS agent_version,
				true AS team_free_collab,
				0 AS identity_order
			FROM weave_run_terminal_markers AS marker
			JOIN weave_team_run_snapshots AS source
			  ON source.workspace_id=marker.workspace_id
			 AND source.run_id=marker.run_snapshot_id
			JOIN weave_agents AS frozen_lead
			  ON frozen_lead.workspace_id=source.workspace_id
			 AND frozen_lead.id=source.lead_avatar_id
			 AND frozen_lead.name=$3
			WHERE marker.workspace_id=$1 AND marker.task_group_id=$2
				AND marker.phase='final'
				AND source.lead_avatar_id IS NOT NULL
				AND source.lead_avatar_version IS NOT NULL
			UNION ALL
			SELECT agent.id,agent.version,EXISTS(
				SELECT 1 FROM weave_teams
				WHERE workspace_id=agent.workspace_id
					AND lead_avatar_id=agent.id
					AND status='active'
			),1
			FROM weave_agents AS agent
			WHERE agent.workspace_id=$1 AND agent.name=$3 AND agent.deleted=false
		) AS completion_identity
		ORDER BY completion_identity.identity_order
		LIMIT 1
	`, group.WorkspaceID, group.ID, group.AvatarAgent).Scan(
		&agentID, &agentVersion, &teamFreeCollab,
	); err != nil {
		return false, fmt.Errorf("resolve task group completion lead identity: %w", err)
	}
	scope := execution.ScopeLegacyOrchestrator
	if teamFreeCollab {
		scope = execution.ScopeTeamFreeCollab
	}
	if err := r.enqueuer.EnqueueTx(ctx, tx, &taskqueue.Task{
		ID:                    completionID,
		WorkspaceID:           group.WorkspaceID,
		Agent:                 group.AvatarAgent,
		AgentID:               agentID,
		AgentVersion:          agentVersion,
		IdentityKind:          taskqueue.IdentityAgent,
		IdentitySchemaVersion: 2,
		ExecutionScope:        scope,
		ContextKey:            completionID,
		Source:                "fanout_completion",
		Kind:                  "chat",
		Status:                taskqueue.StatusQueued,
		Payload:               payload,
	}); err != nil {
		return false, fmt.Errorf("enqueue task group synthesis: %w", err)
	}
	tag, err := tx.Exec(ctx, `
		UPDATE weave_task_group
		SET status=$3,group_outcome=$4,resolved_at=$5,updated_at=$5
		WHERE workspace_id=$1 AND id=$2 AND status=$6
	`, group.WorkspaceID, group.ID, StatusResolved, outcome, r.clock.Now(), StatusResolving)
	if err != nil {
		return false, fmt.Errorf("resolve task group: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return false, fmt.Errorf("task group %q lost resolving ownership", group.ID)
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("commit task group completion: %w", err)
	}
	return true, nil
}

// SweepDeadlines times out overdue legs and force-resolves overdue groups at
// the supplied time.
func (r *Reconciler) SweepDeadlines(ctx context.Context, now time.Time) error {
	if _, err := r.store.TimeOutOverdueLegs(ctx, now); err != nil {
		return err
	}
	groups, err := r.store.GroupsPastDeadline(ctx, now)
	if err != nil {
		return err
	}
	for _, group := range groups {
		if err := r.ReconcileGroup(ctx, group.WorkspaceID, group.ID, true); err != nil {
			return err
		}
	}
	return nil
}

func (r *Reconciler) updateCard(ctx context.Context, snapshot Snapshot, terminal bool) error {
	group := snapshot.Group
	if group.CardMessageID == "" || group.ConversationID == "" || r.cards == nil {
		return nil
	}
	revision, err := r.nextCardRevision(ctx, group)
	if err != nil {
		return err
	}
	content, metadata := renderCard(group, snapshot.Legs, revision, terminal)
	if err := r.cards.UpdateCard(
		ctx, group.WorkspaceID, group.ConversationID, group.CardMessageID, content, metadata,
	); err != nil {
		return fmt.Errorf("update task group card: %w", err)
	}
	if r.hub != nil {
		r.hub.Publish(group.WorkspaceID, group.UserID, realtime.Event{
			Type:           "message",
			ConversationID: group.ConversationID,
			Payload: map[string]string{
				"message_id": group.CardMessageID,
				"group_id":   group.ID,
			},
		})
	}
	return nil
}

func (r *Reconciler) nextCardRevision(ctx context.Context, group Group) (int, error) {
	if reader, ok := r.cards.(cardRevisionReader); ok {
		current, err := reader.CardRevision(ctx, group.WorkspaceID, group.ConversationID, group.CardMessageID)
		if err != nil {
			return 0, fmt.Errorf("read task group card revision: %w", err)
		}
		return current + 1, nil
	}
	key := group.WorkspaceID + "\x00" + group.ID
	r.revisionMu.Lock()
	defer r.revisionMu.Unlock()
	r.revisions[key]++
	return r.revisions[key], nil
}

// taskGroupWorkerNames returns the leg roster in snapshot order — the worker
// list the delivery path cites in its provenance line.
func taskGroupWorkerNames(legs []LegSnapshot) []string {
	names := make([]string, 0, len(legs))
	for _, leg := range legs {
		names = append(names, leg.Agent)
	}
	return names
}

func synthesisPrompt(snapshot Snapshot) string {
	var prompt strings.Builder
	completed := completedLegs(snapshot.Legs)
	unsuccessful := len(snapshot.Legs) - completed
	partial := strings.HasPrefix(snapshot.Group.GroupOutcome, "partial:")
	if partial {
		prompt.WriteString("你派出的团队已部分完成，以下是当前结果，请明确按部分完成向用户汇报。\n\n")
	} else {
		prompt.WriteString("你派出的团队已完成，以下是各自结果，请综合后向用户汇报。\n\n")
	}
	fmt.Fprintf(&prompt, "任务组：%s\n成功：%d；失败或未完成：%d；总计：%d\n原始请求：\n%s\n\n各成员结果：\n", snapshot.Group.ID, completed, unsuccessful, len(snapshot.Legs), snapshot.OriginalRequest)
	for _, leg := range snapshot.Legs {
		summary := ""
		if leg.Status == taskqueue.StatusCompleted {
			summary = legOutput(leg.Result)
		} else {
			summary = leg.Error
		}
		if summary == "" {
			summary = "无结果"
		}
		fmt.Fprintf(&prompt, "- 员工：%s\n  状态：%s\n  结果摘要：%s\n", leg.Agent, leg.Status, summary)
	}
	return prompt.String()
}

func completedLegs(legs []LegSnapshot) int {
	completed := 0
	for _, leg := range legs {
		if leg.Status == taskqueue.StatusCompleted {
			completed++
		}
	}
	return completed
}

func legOutput(result json.RawMessage) string {
	if len(result) == 0 {
		return ""
	}
	var wrapped struct {
		Output json.RawMessage `json:"output"`
	}
	if err := json.Unmarshal(result, &wrapped); err != nil || len(wrapped.Output) == 0 {
		return string(result)
	}
	var output string
	if err := json.Unmarshal(wrapped.Output, &output); err == nil {
		return output
	}
	return string(wrapped.Output)
}
