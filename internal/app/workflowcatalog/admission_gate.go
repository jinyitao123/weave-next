package workflowcatalog

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jinyitao123/weave/internal/kernel/revocation"
	workflowdef "github.com/jinyitao123/weave/internal/kernel/workflow"
)

// EvaluateFixedWorkflowAdmissionTx applies the live relation and version
// gates after the caller has locked workspace, workflow, version, and Team.
func (s *Store) EvaluateFixedWorkflowAdmissionTx(
	ctx context.Context,
	tx pgx.Tx,
	request workflowdef.FixedWorkflowAdmissionRequest) error {
	if interfaceNil(tx) || request.WorkspaceID == "" || request.TeamID == "" ||
		request.WorkflowID == "" || request.WorkflowVersion < 1 || len(request.GraphDefinition) == 0 {
		return errors.New("fixed workflow admission gate request is invalid")
	}
	for _, identity := range []string{request.WorkspaceID, request.TeamID, request.WorkflowID} {
		if identity != strings.TrimSpace(identity) {
			return errors.New("fixed workflow admission gate identity is invalid")
		}
	}

	if err := evaluateFixedWorkflowRosterGateTx(
		ctx, tx, request.WorkspaceID, request.TeamID,
		request.GraphDefinition, request.WorkflowVersion,
	); err != nil {
		return err
	}

	if s.artifacts == nil {
		return errors.New("frozen publication reader unavailable")
	}
	admission, err := s.artifacts.ReadAdmission(ctx, request.WorkspaceID, request.WorkflowID, request.WorkflowVersion)
	if err != nil {
		return err
	}
	if admission.Blocked {
		return &workflowdef.FixedWorkflowAdmissionDenial{
			ReasonCode:      workflowdef.FixedWorkflowAdmissionVersionBlocked,
			WorkflowVersion: request.WorkflowVersion,
		}
	}
	return nil
}

// evaluateFixedWorkflowRosterGateTx locks every TeamWorker referenced by the
// frozen graph and rejects disabled or missing workers. It is the shared
// roster half of the fixed-workflow gate: the published admission path also
// checks the version-blocked admission status, while the candidate test-run
// path (whose draft versions can never carry an admission status row) only
// runs this roster gate.
func evaluateFixedWorkflowRosterGateTx(
	ctx context.Context,
	tx pgx.Tx,
	workspaceID, teamID string,
	graphDefinition []byte,
	workflowVersion int,
) error {
	references, err := revocation.ExtractGraphReferences(graphDefinition)
	if err != nil {
		return fmt.Errorf("decode fixed workflow admission references: %w", err)
	}
	workerIDs := make([]string, 0)
	for _, reference := range references {
		if len(workerIDs) == 0 || workerIDs[len(workerIDs)-1] != reference.WorkerAgentID {
			workerIDs = append(workerIDs, reference.WorkerAgentID)
		}
	}
	sort.Strings(workerIDs)
	if err := lockFixedWorkflowReferencedWorkers(
		ctx, tx, workspaceID, teamID, workerIDs, workflowVersion,
	); err != nil {
		return err
	}
	return nil
}

func lockFixedWorkflowReferencedWorkers(
	ctx context.Context,
	tx pgx.Tx,
	workspaceID, teamID string,
	workerIDs []string,
	workflowVersion int,
) error {
	if len(workerIDs) == 0 {
		return nil
	}
	rows, err := tx.Query(ctx, `
		SELECT worker_agent_id, enabled
		FROM weave_team_workers
		WHERE workspace_id=$1 AND team_id=$2 AND worker_agent_id=ANY($3)
		ORDER BY worker_agent_id COLLATE "C"
		FOR SHARE
	`, workspaceID, teamID, workerIDs)
	if err != nil {
		return fmt.Errorf("lock referenced TeamWorkers: %w", err)
	}
	defer rows.Close()

	locked := 0
	for rows.Next() {
		var workerID string
		var enabled bool
		if err := rows.Scan(&workerID, &enabled); err != nil {
			return fmt.Errorf("read referenced TeamWorker: %w", err)
		}
		if locked >= len(workerIDs) || workerID != workerIDs[locked] || !enabled {
			return &workflowdef.FixedWorkflowAdmissionDenial{
				ReasonCode:      workflowdef.FixedWorkflowAdmissionTeamWorkerDisabled,
				WorkflowVersion: workflowVersion,
			}
		}
		locked++
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("read referenced TeamWorkers: %w", err)
	}
	if locked != len(workerIDs) {
		return &workflowdef.FixedWorkflowAdmissionDenial{
			ReasonCode:      workflowdef.FixedWorkflowAdmissionTeamWorkerDisabled,
			WorkflowVersion: workflowVersion,
		}
	}
	return nil
}
