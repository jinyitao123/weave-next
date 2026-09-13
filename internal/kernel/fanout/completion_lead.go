package fanout

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
)

func (s *Store) resolveCompletionLead(ctx context.Context, tx pgx.Tx, group Group) (CompletionLead, error) {
	rows, err := tx.Query(ctx, `
 SELECT DISTINCT COALESCE(source.lead_avatar_id, ''), COALESCE(source.lead_avatar_version, 0)
 FROM weave_run_terminal_markers AS marker
 JOIN weave_team_run_snapshots AS source
   ON source.workspace_id=marker.workspace_id AND source.run_id=marker.run_snapshot_id
 WHERE marker.workspace_id=$1 AND marker.task_group_id=$2 AND marker.phase='final'
   AND (source.lead_avatar_id IS NOT NULL OR source.lead_avatar_version IS NOT NULL)
 LIMIT 2`, group.WorkspaceID, group.ID)
	if err != nil {
		return CompletionLead{}, err
	}
	defer rows.Close()
	var frozen []CompletionLead
	for rows.Next() {
		lead := CompletionLead{TeamFreeCollab: true}
		if err := rows.Scan(&lead.AgentID, &lead.AgentVersion); err != nil {
			return CompletionLead{}, err
		}
		frozen = append(frozen, lead)
	}
	if err := rows.Err(); err != nil {
		return CompletionLead{}, err
	}
	rows.Close()
	if len(frozen) > 1 {
		return CompletionLead{}, ErrCompletionLeadAmbiguous
	}
	if len(frozen) == 1 {
		return validateCompletionLead(frozen[0])
	}
	if s.completionLead == nil {
		return CompletionLead{}, ErrCompletionLeadUnavailable
	}
	lead, err := s.completionLead(ctx, tx, group.WorkspaceID, group.AvatarAgent)
	if err != nil {
		return CompletionLead{}, err
	}
	return validateCompletionLead(lead)
}

func validateCompletionLead(lead CompletionLead) (CompletionLead, error) {
	if strings.TrimSpace(lead.AgentID) == "" || lead.AgentVersion <= 0 {
		return CompletionLead{}, fmt.Errorf("task group completion identity is incomplete")
	}
	return lead, nil
}
