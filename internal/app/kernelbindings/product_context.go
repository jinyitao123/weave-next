// Package kernelbindings supplies product-owned reads and projections to the
// kernel without giving its packages access to account or conversation tables.
package kernelbindings

import (
	"context"
	"errors"
	"time"

	"github.com/jinyitao123/weave/internal/app/agentcatalog"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jinyitao123/weave/internal/base/deliverable"
	"github.com/jinyitao123/weave/internal/base/storeext"
	"github.com/jinyitao123/weave/internal/kernel/fanout"

	orgstore "github.com/jinyitao123/weave/internal/app/org"
)

func NewRegistry(pool *pgxpool.Pool) *agentcatalog.AgentRegistry {
	return agentcatalog.New(pool, agentcatalog.WithWorkspaceMemberVerifier(WorkspaceMemberExists))
}

func WorkspaceMemberExists(ctx context.Context, q agentcatalog.OwnerQuery, workspaceID, userID string) (bool, error) {
	var exists bool
	err := q.QueryRow(ctx, `SELECT EXISTS (
 SELECT 1 FROM weave_members AS member JOIN weave_users AS owner_user
 ON owner_user.id=member.user_id AND owner_user.tenant_id=member.workspace_id
 WHERE member.workspace_id=$1 AND member.user_id=$2)`, workspaceID, userID).Scan(&exists)
	return exists, err
}

func NewOrganization(pool *pgxpool.Pool) *orgstore.Store {
	return orgstore.NewStore(pool, orgstore.WithMemberProfiles(func(ctx context.Context, workspaceID string, ids []string) (map[string]orgstore.MemberProfile, error) {
		rows, err := pool.Query(ctx, `SELECT id, username, COALESCE(display_name, '') FROM weave_users WHERE tenant_id=$1 AND id=ANY($2::text[])`, workspaceID, ids)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		profiles := make(map[string]orgstore.MemberProfile)
		for rows.Next() {
			var id string
			var profile orgstore.MemberProfile
			if err := rows.Scan(&id, &profile.Username, &profile.DisplayName); err != nil {
				return nil, err
			}
			profiles[id] = profile
		}
		return profiles, rows.Err()
	}))
}

func ConversationOwner(ctx context.Context, tx pgx.Tx, workspaceID, conversationID string) (string, bool, error) {
	var userID string
	err := tx.QueryRow(ctx, `SELECT user_id FROM weave_conversations WHERE workspace_id=$1 AND id=$2 AND parent_message_id IS NULL`, workspaceID, conversationID).Scan(&userID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	return userID, err == nil, err
}

func DeliverableOptions() []deliverable.StoreOption {
	return []deliverable.StoreOption{
		deliverable.WithConversationOwner(ConversationOwner),
		deliverable.WithAgentLabel(AgentLabel),
	}
}

func NewFanout(pool *pgxpool.Pool, clock fanout.Clock) *fanout.Store {
	return fanout.New(pool, clock,
		fanout.WithConversationProject(ConversationProject),
		fanout.WithCompletionLeadResolver(CompletionLead),
	)
}

func ConversationProject(ctx context.Context, tx pgx.Tx, workspaceID, conversationID string) (string, error) {
	var projectID string
	err := tx.QueryRow(ctx, `SELECT COALESCE(project_id, '') FROM weave_conversations WHERE workspace_id=$1 AND id=$2`, workspaceID, conversationID).Scan(&projectID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return projectID, err
}

// TerminalRecords retains every platform store operation and adds only the
// product activity projection consumed by the terminal coordinator.
type TerminalRecords struct{ *storeext.PGExt }

func WithTerminalActivity(records *storeext.PGExt) *TerminalRecords {
	return &TerminalRecords{PGExt: records}
}
func (*TerminalRecords) ProjectTerminalActivityTx(ctx context.Context, tx pgx.Tx, workspaceID, conversationID string, at time.Time) error {
	_, err := tx.Exec(ctx, `UPDATE weave_projects AS project
 SET last_activity_at=GREATEST(COALESCE(project.last_activity_at, project.updated_at), $3)
 FROM weave_conversations AS conversation
 WHERE conversation.workspace_id=$1 AND conversation.id=$2
 AND project.workspace_id=conversation.workspace_id AND project.id=conversation.project_id`, workspaceID, conversationID, at)
	return err
}

// AgentLabel is a product presentation projection, scoped to the execution's
// workspace. A removed member's display name does not change frozen ownership.
func AgentLabel(ctx context.Context, tx pgx.Tx, workspaceID, agentID string) (string, error) {
	var label string
	err := tx.QueryRow(ctx, `SELECT COALESCE(NULLIF(BTRIM(display_name), ''), name)
 FROM weave_agents WHERE workspace_id=$1 AND id=$2`, workspaceID, agentID).Scan(&label)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return label, err
}

// CompletionLead supplies the product directory fallback for a group without
// a frozen source. It cannot override an existing snapshot's version binding.
func CompletionLead(ctx context.Context, tx pgx.Tx, workspaceID, avatarName string) (fanout.CompletionLead, error) {
	var lead fanout.CompletionLead
	err := tx.QueryRow(ctx, `SELECT agent.id,agent.version,EXISTS(
 SELECT 1 FROM weave_teams WHERE workspace_id=agent.workspace_id
 AND lead_avatar_id=agent.id AND status='active')
 FROM weave_agents AS agent WHERE agent.workspace_id=$1 AND agent.name=$2 AND agent.deleted=false`,
		workspaceID, avatarName).Scan(&lead.AgentID, &lead.AgentVersion, &lead.TeamFreeCollab)
	return lead, err
}
