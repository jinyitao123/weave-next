// Package kernelbindings supplies product-owned reads and projections to the
// kernel without giving its packages access to account tables.
package kernelbindings

import (
	"context"
	"errors"

	"github.com/jinyitao123/weave/internal/app/agentcatalog"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jinyitao123/weave/internal/base/deliverable"
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

func DeliverableOptions() []deliverable.StoreOption {
	return []deliverable.StoreOption{
		deliverable.WithAgentLabel(AgentLabel),
	}
}

func NewFanout(pool *pgxpool.Pool, clock fanout.Clock) *fanout.Store {
	return fanout.New(pool, clock)
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
