package api

import (
	"context"
	"time"

	"github.com/jinyitao123/weave/internal/base/teamrun"
)

// Expose saved progress only. Checkpoint state contains private model context
// and is never included in the Workbench activity response.
func (s *Server) projectMemberCheckpoints(ctx context.Context, run teamrun.TeamRun, members []runActivityMember) {
	if s.StoreExt == nil {
		return
	}
	tx, err := s.StoreExt.BeginTx(ctx)
	if err != nil {
		return
	}
	defer func() { _ = tx.Rollback(ctx) }()
	rows, err := tx.Query(ctx, `SELECT DISTINCT ON(m.node_id) m.node_id,m.member_run_id,c.updated_at
		FROM weave_workflow_member_runs m
		JOIN weave_run_attempt_leases l ON l.workspace_id=m.workspace_id AND l.run_id=m.member_run_id
		JOIN loom_store c ON c.namespace='checkpoint:'||l.graph_name AND c.key=m.member_run_id
		WHERE m.workspace_id=$1 AND m.parent_run_id=$2 AND m.run_snapshot_id=$3 AND m.checkpoint_seq>0
		ORDER BY m.node_id,m.created_at DESC,m.member_run_id DESC`, run.WorkspaceID, run.RunID, run.RunSnapshotID)
	if err != nil {
		return
	}
	defer rows.Close()
	type progress struct {
		node, id string
		saved    time.Time
	}
	items := []progress{}
	for rows.Next() {
		var item progress
		if err := rows.Scan(&item.node, &item.id, &item.saved); err != nil {
			return
		}
		items = append(items, item)
	}
	if rows.Err() != nil {
		return
	}
	for _, item := range items {
		for i := range members {
			for j := range members[i].Stages {
				stage := &members[i].Stages[j]
				if stage.NodeID == item.node {
					stage.MemberRunID, stage.CheckpointSavedAt = item.id, &item.saved
				}
			}
		}
	}
}
