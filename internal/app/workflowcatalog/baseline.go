package workflowcatalog

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"sort"

	"github.com/jackc/pgx/v5"
	"github.com/jinyitao123/weave/internal/base/frozen"
	"github.com/jinyitao123/weave/internal/kernel/workflow"
)

// VerifyBaselineWorkflowsTx checks previously captured product facts while
// holding only product locks. Frozen artifact reads must finish before this
// transaction begins. An absent workflow is a checked fact, not a wildcard.
func (s *Store) VerifyBaselineWorkflowsTx(ctx context.Context, tx pgx.Tx, workspaceID string, workflowIDs []string, identities []workflow.TeamWorkflow, versions []workflow.TeamWorkflowVersion) error {
	if tx == nil || workspaceID == "" {
		return errors.New("workflow baseline transaction and workspace are required")
	}
	var lockedWorkspace string
	if err := tx.QueryRow(ctx, `SELECT id FROM weave_workspaces WHERE id=$1 FOR SHARE`, workspaceID).Scan(&lockedWorkspace); err != nil {
		return err
	}
	rows, err := tx.Query(ctx, `SELECT `+workflowColumns+` FROM weave_team_workflows WHERE workspace_id=$1 AND id=ANY($2) ORDER BY id FOR SHARE`, workspaceID, workflowIDs)
	if err != nil {
		return err
	}
	actualIdentities := []workflow.TeamWorkflow{}
	for rows.Next() {
		value, err := scanWorkflow(rows)
		if err != nil {
			rows.Close()
			return err
		}
		actualIdentities = append(actualIdentities, *value)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	rows, err = tx.Query(ctx, `SELECT `+versionColumns+` FROM weave_team_workflow_versions WHERE workspace_id=$1 AND workflow_id=ANY($2) ORDER BY workflow_id,version FOR SHARE`, workspaceID, workflowIDs)
	if err != nil {
		return err
	}
	actualVersions := []workflow.TeamWorkflowVersion{}
	for rows.Next() {
		value, err := scanVersion(rows)
		if err != nil {
			rows.Close()
			return err
		}
		actualVersions = append(actualVersions, *value)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	expectedIdentities := append([]workflow.TeamWorkflow{}, identities...)
	expectedVersions := append([]workflow.TeamWorkflowVersion{}, versions...)
	sort.Slice(expectedIdentities, func(i, j int) bool { return expectedIdentities[i].ID < expectedIdentities[j].ID })
	sort.Slice(expectedVersions, func(i, j int) bool {
		if expectedVersions[i].WorkflowID != expectedVersions[j].WorkflowID {
			return expectedVersions[i].WorkflowID < expectedVersions[j].WorkflowID
		}
		return expectedVersions[i].Version < expectedVersions[j].Version
	})
	for _, pair := range [][2]any{{actualIdentities, expectedIdentities}, {actualVersions, expectedVersions}} {
		left, err := json.Marshal(pair[0])
		if err != nil {
			return err
		}
		right, err := json.Marshal(pair[1])
		if err != nil {
			return err
		}
		left, err = frozen.CanonicalizeJSON(left)
		if err != nil {
			return err
		}
		right, err = frozen.CanonicalizeJSON(right)
		if err != nil {
			return err
		}
		if !bytes.Equal(left, right) {
			return workflow.ErrVersionConflict
		}
	}
	return nil
}
