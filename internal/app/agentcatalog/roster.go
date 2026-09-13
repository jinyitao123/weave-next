package agentcatalog

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/jinyitao123/weave/internal/kernel/registry"
)

func validateTeamRosterWorkerInputs(workers []registry.TeamRosterWorkerInput) error {
	seen := make(map[string]struct{}, len(workers))
	for index, worker := range workers {
		if worker.WorkerAgentID == "" {
			return fmt.Errorf("worker agent ID is required at index %d", index)
		}
		if _, duplicate := seen[worker.WorkerAgentID]; duplicate {
			return fmt.Errorf("agent %q appears more than once in roster", worker.WorkerAgentID)
		}
		seen[worker.WorkerAgentID] = struct{}{}
		if err := validateTeamWorkerKinds(worker.AllowedKinds, worker.DefaultKind); err != nil {
			return fmt.Errorf("worker %q: %w", worker.WorkerAgentID, err)
		}
	}
	return nil
}

// UpdateTeamRoster applies product seed roster state. User-facing roster
// changes use ApplyTeamRosterCommand with an explicit idempotency receipt.
func (r *AgentRegistry) UpdateTeamRoster(
	ctx context.Context,
	workspaceID, teamID, leadAgentID string,
	workerInputs []registry.TeamRosterWorkerInput,
) error {
	workers := cloneTeamRosterWorkers(workerInputs)
	for index := range workers {
		sortKinds(workers[index].AllowedKinds)
	}
	sort.Slice(workers, func(i, j int) bool {
		return workers[i].WorkerAgentID < workers[j].WorkerAgentID
	})
	if err := validateTeamRosterWorkerInputs(workers); err != nil {
		return err
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	team, err := lockRosterTeamForMutation(ctx, tx, workspaceID, teamID)
	if errors.Is(err, registry.ErrTeamWorkerNotFound) {
		return fmt.Errorf("team %q not found in workspace %q", teamID, workspaceID)
	}
	if err != nil {
		return fmt.Errorf("lock team %q: %w", teamID, err)
	}
	if team.Status == "archived" {
		return fmt.Errorf("%w: team %q", registry.ErrArchivedTeamImmutable, teamID)
	}
	managerID, err := findLegacyRosterManager(ctx, tx, workspaceID, teamID, team.LeadAvatarID)
	if err != nil {
		return err
	}
	if err := lockAndValidateRosterAgents(ctx, tx, workspaceID, leadAgentID, workers, managerID); err != nil {
		return err
	}
	current, err := readLockedRosterWorkers(ctx, tx, workspaceID, teamID)
	if err != nil {
		return err
	}
	outcome, err := applyTeamRosterMutationTx(ctx, tx, teamRosterMutationRequest{
		WorkspaceID: workspaceID,
		TeamID:      teamID,
		Team:        team,
		LeadAgentID: leadAgentID,
		Status:      "active",
		Workers:     workers,
		Current:     current,
		ManagerID:   managerID,
		AllowRemove: true,
	})
	if err != nil {
		return err
	}
	if err := verifyTeamRosterMutation(ctx, tx, workspaceID, teamID, outcome); err != nil {
		return err
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit team roster: %w", err)
	}
	return nil
}
