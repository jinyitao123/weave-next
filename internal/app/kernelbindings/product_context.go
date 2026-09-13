// Package kernelbindings supplies product-owned reads and projections to the
// kernel without giving its packages access to account or conversation tables.
package kernelbindings

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jinyitao123/weave/internal/base/deliverable"
	"github.com/jinyitao123/weave/internal/base/storeext"
	"github.com/jinyitao123/weave/internal/kernel/fanout"
	"github.com/jinyitao123/weave/internal/kernel/org"
	"github.com/jinyitao123/weave/internal/kernel/registry"
)

func NewRegistry(pool *pgxpool.Pool) *registry.AgentRegistry {
	return registry.New(pool, registry.WithWorkspaceMemberVerifier(WorkspaceMemberExists))
}

func WorkspaceMemberExists(ctx context.Context, q registry.OwnerQuery, workspaceID, userID string) (bool, error) {
	var exists bool
	err := q.QueryRow(ctx, `SELECT EXISTS (
 SELECT 1 FROM weave_members AS member JOIN weave_users AS owner_user
 ON owner_user.id=member.user_id AND owner_user.tenant_id=member.workspace_id
 WHERE member.workspace_id=$1 AND member.user_id=$2)`, workspaceID, userID).Scan(&exists)
	return exists, err
}

func NewOrganization(pool *pgxpool.Pool) *org.Store {
	return org.NewStore(pool, org.WithMemberProfiles(func(ctx context.Context, workspaceID string, ids []string) (map[string]org.MemberProfile, error) {
		rows, err := pool.Query(ctx, `SELECT id, username, COALESCE(display_name, '') FROM weave_users WHERE tenant_id=$1 AND id=ANY($2::text[])`, workspaceID, ids)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		profiles := make(map[string]org.MemberProfile)
		for rows.Next() {
			var id string
			var profile org.MemberProfile
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
	return []deliverable.StoreOption{deliverable.WithConversationOwner(ConversationOwner)}
}

func NewFanout(pool *pgxpool.Pool, clock fanout.Clock) *fanout.Store {
	return fanout.New(pool, clock, fanout.WithConversationProject(ConversationProject))
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
