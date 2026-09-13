package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jinyitao123/weave/internal/app/teamconstruction"
	"github.com/jinyitao123/weave/internal/build/teambuild"
	"github.com/jinyitao123/weave/internal/build/teamrestore"
	"github.com/jinyitao123/weave/internal/kernel/workflow"
)

// workflowPublicationRestorer keeps rollback sequencing in Builder while the
// product draft transaction and durable kernel publication receipt remain in
// the app publication boundary.
type workflowPublicationRestorer struct {
	pool         *pgxpool.Pool
	workflows    *workflow.Store
	authority    *teamconstruction.PublicationAuthority
	publications *teamconstruction.ProductPublication
}

func (r workflowPublicationRestorer) RestoreWorkflow(
	ctx context.Context,
	workspaceID, buildRunID, teamID, operatorID string,
	ref teambuild.BaselineWorkflowRef,
) (teamrestore.WorkflowRestoreResult, error) {
	if ref.Published == nil || ref.WorkflowID == "" || ref.TeamID != teamID {
		return teamrestore.WorkflowRestoreResult{}, errors.New("invalid frozen workflow restore target")
	}
	requestHash := sha256.Sum256([]byte(buildRunID + "\x00" + ref.WorkflowID + "\x00" + ref.Published.ContentHash))
	requestID := "team-build-rollback:" + hex.EncodeToString(requestHash[:])
	if stored, found, err := r.publications.Lookup(ctx, workspaceID, requestID); err != nil {
		return teamrestore.WorkflowRestoreResult{}, err
	} else if found {
		return r.publishStored(ctx, ref.WorkflowID, stored.Command)
	}

	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return teamrestore.WorkflowRestoreResult{}, fmt.Errorf("begin workflow restore: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	draftResult, err := r.workflows.CreateRestoreDraftTx(ctx, tx, workspaceID, ref.WorkflowID, operatorID, ref.Published.ContentHash, workflow.DraftInput{
		TriggerConfig:   append(json.RawMessage(nil), ref.Published.Trigger...),
		GraphDefinition: append(json.RawMessage(nil), ref.Published.Graph...),
		CreatedBy:       operatorID,
	})
	if err != nil {
		return teamrestore.WorkflowRestoreResult{}, err
	}
	if draftResult.AlreadyRestored {
		var version int
		if err = tx.QueryRow(ctx, `SELECT COALESCE(published_version,0) FROM weave_team_workflows WHERE workspace_id=$1 AND id=$2`, workspaceID, ref.WorkflowID).Scan(&version); err != nil {
			return teamrestore.WorkflowRestoreResult{}, err
		}
		if err = tx.Commit(ctx); err != nil {
			return teamrestore.WorkflowRestoreResult{}, err
		}
		return teamrestore.WorkflowRestoreResult{WorkflowID: ref.WorkflowID, Version: version, Reason: "published content already at baseline"}, nil
	}
	candidate, report, err := r.authority.BuildCandidateTx(ctx, tx, workflow.CandidateInput{WorkspaceID: workspaceID, WorkflowID: ref.WorkflowID, WorkflowVersion: draftResult.Version.Version})
	if err != nil {
		return teamrestore.WorkflowRestoreResult{}, fmt.Errorf("build publication candidate: %w", err)
	}
	if candidate == nil || report == nil || len(report.Issues) != 0 {
		return teamrestore.WorkflowRestoreResult{}, errors.New("restored workflow did not pass publication checks")
	}
	command, err := teamconstruction.PublicationCommandForCandidate(requestID, candidate, teamconstruction.PublicationTarget{
		TeamID:               teamID,
		ExpectedAssetVersion: draftResult.Version.UpdatedAt.UTC().Format(time.RFC3339Nano),
	})
	if err != nil {
		return teamrestore.WorkflowRestoreResult{}, err
	}
	if _, err = r.publications.ReserveTx(ctx, tx, command); err != nil {
		return teamrestore.WorkflowRestoreResult{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return teamrestore.WorkflowRestoreResult{}, err
	}
	return r.publishStored(ctx, ref.WorkflowID, command)
}

func (r workflowPublicationRestorer) publishStored(ctx context.Context, workflowID string, command teamconstruction.PublicationCommand) (teamrestore.WorkflowRestoreResult, error) {
	record, err := r.publications.Publish(ctx, command)
	if err != nil {
		return teamrestore.WorkflowRestoreResult{}, err
	}
	if record.Receipt == nil {
		return teamrestore.WorkflowRestoreResult{}, errors.New("restored workflow publication receipt unavailable")
	}
	return teamrestore.WorkflowRestoreResult{WorkflowID: workflowID, Version: record.Receipt.Revision.WorkflowVersion, Restored: true}, nil
}
