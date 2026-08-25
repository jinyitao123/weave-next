package org

import (
	"context"
	"encoding/json"
	"fmt"
)

// Ineligibility reasons reported by the team-creation-options read model.
// They are stable API values; each maps to the CreateActiveTeam rejection
// enforced by the shared predicates in team_eligibility.go.
const (
	// TeamCreationReasonAlreadyLeadsActiveTeam marks a lead candidate that
	// already leads an active team (teamLeadConflict / ErrTeamLeadConflict).
	TeamCreationReasonAlreadyLeadsActiveTeam = "already_leads_active_team"
	// TeamCreationReasonNestedOrchestration marks a worker whose agent record
	// fails teamWorkerRecordCompatible (ErrTeamWorkerUnavailable), whether the
	// record declares sub_agents or its graph contains a worker step.
	TeamCreationReasonNestedOrchestration = "nested_orchestration"
)

// TeamCreationLeadCandidate is one live workspace avatar and its eligibility
// to lead a new team.
type TeamCreationLeadCandidate struct {
	ID               string `json:"id"`
	Name             string `json:"name"`
	DisplayName      string `json:"display_name"`
	Eligible         bool   `json:"eligible"`
	IneligibleReason string `json:"ineligible_reason"`
}

// TeamCreationTeamReference names one team holding a worker membership row.
type TeamCreationTeamReference struct {
	TeamID   string `json:"team_id"`
	TeamName string `json:"team_name"`
}

// TeamCreationWorkerOption is one live workspace worker and its eligibility
// to join a new team. Being referenced by other teams never affects
// eligibility; the references are reported factually.
type TeamCreationWorkerOption struct {
	ID                string                      `json:"id"`
	Name              string                      `json:"name"`
	DisplayName       string                      `json:"display_name"`
	Engine            string                      `json:"engine"`
	Eligible          bool                        `json:"eligible"`
	IneligibleReason  string                      `json:"ineligible_reason"`
	ReferencedByTeams []TeamCreationTeamReference `json:"referenced_by_teams"`
}

// TeamCreationOptions is the workspace-scoped creation candidate panorama.
type TeamCreationOptions struct {
	LeadCandidates []TeamCreationLeadCandidate `json:"lead_candidates"`
	WorkerPool     []TeamCreationWorkerOption  `json:"worker_pool"`
}

// TeamCreationOptions returns the lead candidates and worker pool of a
// workspace, both sorted by name. Eligibility is computed with the same
// predicates that CreateActiveTeam enforces, so the read model cannot drift
// from the write path.
func (s *Store) TeamCreationOptions(ctx context.Context, workspaceID string) (TeamCreationOptions, error) {
	options := TeamCreationOptions{
		LeadCandidates: make([]TeamCreationLeadCandidate, 0),
		WorkerPool:     make([]TeamCreationWorkerOption, 0),
	}

	type candidate struct {
		id          string
		name        string
		displayName string
		snapshot    teamAgentSnapshot
	}
	leads := make([]candidate, 0)
	workers := make([]candidate, 0)
	rows, err := s.pool.Query(ctx, `
		SELECT id, name, display_name, role, deleted, spec
		FROM weave_agents
		WHERE workspace_id=$1
		ORDER BY name, id
	`, workspaceID)
	if err != nil {
		return TeamCreationOptions{}, fmt.Errorf("list team creation agents: %w", err)
	}
	for rows.Next() {
		var entry candidate
		var data []byte
		if err := rows.Scan(
			&entry.id, &entry.name, &entry.displayName,
			&entry.snapshot.role, &entry.snapshot.deleted, &data,
		); err != nil {
			rows.Close()
			return TeamCreationOptions{}, fmt.Errorf("scan team creation agent: %w", err)
		}
		entry.snapshot.workspaceID = workspaceID
		if err := json.Unmarshal(data, &entry.snapshot.record); err != nil {
			rows.Close()
			return TeamCreationOptions{}, fmt.Errorf("decode team creation agent %q: %w", entry.id, err)
		}
		switch {
		case teamLeadAvailable(workspaceID, entry.snapshot, true):
			leads = append(leads, entry)
		case teamWorkerAvailable(workspaceID, entry.snapshot, true):
			workers = append(workers, entry)
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return TeamCreationOptions{}, fmt.Errorf("list team creation agents: %w", err)
	}
	rows.Close()

	for _, lead := range leads {
		entry := TeamCreationLeadCandidate{
			ID:          lead.id,
			Name:        lead.name,
			DisplayName: lead.displayName,
			Eligible:    true,
		}
		conflict, err := teamLeadConflict(ctx, s.pool, workspaceID, lead.id)
		if err != nil {
			return TeamCreationOptions{}, fmt.Errorf("check active team lead: %w", err)
		}
		if conflict {
			entry.Eligible = false
			entry.IneligibleReason = TeamCreationReasonAlreadyLeadsActiveTeam
		}
		options.LeadCandidates = append(options.LeadCandidates, entry)
	}

	references := make(map[string][]TeamCreationTeamReference)
	referenceRows, err := s.pool.Query(ctx, `
		SELECT worker.worker_agent_id, team.id, team.name
		FROM weave_team_workers AS worker
		JOIN weave_teams AS team
		  ON team.workspace_id=worker.workspace_id AND team.id=worker.team_id
		WHERE worker.workspace_id=$1
		ORDER BY team.name, team.id
	`, workspaceID)
	if err != nil {
		return TeamCreationOptions{}, fmt.Errorf("list team worker references: %w", err)
	}
	for referenceRows.Next() {
		var agentID string
		var reference TeamCreationTeamReference
		if err := referenceRows.Scan(&agentID, &reference.TeamID, &reference.TeamName); err != nil {
			referenceRows.Close()
			return TeamCreationOptions{}, fmt.Errorf("scan team worker reference: %w", err)
		}
		references[agentID] = append(references[agentID], reference)
	}
	if err := referenceRows.Err(); err != nil {
		referenceRows.Close()
		return TeamCreationOptions{}, fmt.Errorf("list team worker references: %w", err)
	}
	referenceRows.Close()

	for _, worker := range workers {
		entry := TeamCreationWorkerOption{
			ID:                worker.id,
			Name:              worker.name,
			DisplayName:       worker.displayName,
			Engine:            worker.snapshot.record.Engine,
			Eligible:          true,
			ReferencedByTeams: make([]TeamCreationTeamReference, 0),
		}
		if teamReferences, ok := references[worker.id]; ok {
			entry.ReferencedByTeams = teamReferences
		}
		if err := teamWorkerRecordCompatible(&worker.snapshot.record); err != nil {
			entry.Eligible = false
			entry.IneligibleReason = TeamCreationReasonNestedOrchestration
		}
		options.WorkerPool = append(options.WorkerPool, entry)
	}

	return options, nil
}
