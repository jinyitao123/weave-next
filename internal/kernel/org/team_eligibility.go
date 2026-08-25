package org

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/jinyitao123/weave/internal/kernel/registry"
)

// teamAgentSnapshot captures the agent fields consulted by the team
// eligibility predicates. CreateActiveTeam fills it from locked rows; the
// team-creation-options read model fills it from live rows.
type teamAgentSnapshot struct {
	workspaceID string
	role        string
	deleted     bool
	record      registry.AgentRecord
}

// teamLeadAvailable reports whether the snapshot identifies a live avatar in
// the workspace. It is the single source of the ErrTeamLeadUnavailable rule.
func teamLeadAvailable(workspaceID string, agent teamAgentSnapshot, found bool) bool {
	return found && agent.workspaceID == workspaceID && agent.role == "avatar" && !agent.deleted
}

// teamWorkerAvailable reports whether the snapshot identifies a live worker in
// the workspace. It is the single source of the ErrTeamWorkerUnavailable
// membership rule.
func teamWorkerAvailable(workspaceID string, agent teamAgentSnapshot, found bool) bool {
	return found && agent.workspaceID == workspaceID && agent.role == "worker" && !agent.deleted
}

// teamWorkerRecordCompatible enforces the nested-orchestration ban for team
// workers. It is the single source of the ErrTeamWorkerUnavailable
// compatibility rule shared with the team-creation-options read model.
func teamWorkerRecordCompatible(rec *registry.AgentRecord) error {
	return registry.ValidateTeamWorkerAgentRecord(rec)
}

// teamLeadQuerier is satisfied by both *pgxpool.Pool and pgx.Tx so the
// lead-conflict rule keeps a single source inside and outside transactions.
type teamLeadQuerier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// teamLeadConflict reports whether the avatar already leads an active team in
// the workspace. It is the single source of the ErrTeamLeadConflict rule;
// archived teams do not occupy a lead.
func teamLeadConflict(ctx context.Context, q teamLeadQuerier, workspaceID, leadAvatarID string) (bool, error) {
	var conflict bool
	if err := q.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1
			FROM weave_teams
			WHERE workspace_id=$1 AND lead_avatar_id=$2 AND status='active'
		)
	`, workspaceID, leadAvatarID).Scan(&conflict); err != nil {
		return false, err
	}
	return conflict, nil
}
