package registry

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jinyitao123/weave/internal/base/frozen"
	"github.com/jinyitao123/weave/internal/kernel/revocation"
)

// countAffectedPublishedVersionsTx scans immutable published artifacts without
// taking workflow locks and counts actual graph references for each changed
// TeamWorker relationship.
func countAffectedPublishedVersionsTx(
	ctx context.Context,
	tx pgx.Tx,
	workspaceID, teamID string,
	workerIDs []string,
) (map[string]int, error) {
	counts := make(map[string]int, len(workerIDs))
	targets := make(map[string]struct{}, len(workerIDs))
	for _, workerID := range workerIDs {
		counts[workerID] = 0
		targets[workerID] = struct{}{}
	}
	if len(targets) == 0 {
		return counts, nil
	}

	rows, err := tx.Query(ctx, `
		SELECT
			artifact.workflow_id,
			artifact.workflow_version,
			artifact.artifact_schema_version,
			artifact.canonicalization_algorithm,
			artifact.canonicalization_version,
			artifact.hash_algorithm,
			artifact.content_hash,
			artifact.payload
		FROM weave_published_artifact_contents AS artifact
		JOIN weave_team_workflows AS workflow
		  ON workflow.workspace_id=artifact.workspace_id
		 AND workflow.id=artifact.workflow_id
		WHERE artifact.workspace_id=$1 AND workflow.team_id=$2
		ORDER BY artifact.workflow_id COLLATE "C", artifact.workflow_version
	`, workspaceID, teamID)
	if err != nil {
		return nil, fmt.Errorf("scan roster revocation impact: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var (
			workflowID                string
			version                   int
			artifactSchemaVersion     int
			canonicalizationAlgorithm string
			canonicalizationVersion   int
			hashAlgorithm             string
			contentHash               string
			payloadRaw                []byte
		)
		if err := rows.Scan(
			&workflowID,
			&version,
			&artifactSchemaVersion,
			&canonicalizationAlgorithm,
			&canonicalizationVersion,
			&hashAlgorithm,
			&contentHash,
			&payloadRaw,
		); err != nil {
			return nil, fmt.Errorf("scan roster revocation artifact: %w", err)
		}
		payload, err := frozen.DecodeArtifactEnvelopeV1(frozen.ArtifactEnvelopeV1{
			WorkspaceID:               workspaceID,
			WorkflowID:                workflowID,
			WorkflowVersion:           version,
			ArtifactSchemaVersion:     artifactSchemaVersion,
			CanonicalizationAlgorithm: canonicalizationAlgorithm,
			CanonicalizationVersion:   canonicalizationVersion,
			HashAlgorithm:             hashAlgorithm,
			ContentHash:               contentHash,
			Payload:                   payloadRaw,
		})
		if err != nil {
			return nil, fmt.Errorf(
				"decode published artifact %q version %d: %w", workflowID, version, err,
			)
		}
		if payload.Team.WorkspaceID != workspaceID || payload.Team.TeamID != teamID {
			return nil, fmt.Errorf(
				"published artifact %q version %d team does not match workflow",
				workflowID,
				version,
			)
		}
		references, err := revocation.ExtractGraphReferences(payload.GraphDefinition)
		if err != nil {
			return nil, fmt.Errorf(
				"extract published artifact %q version %d references: %w",
				workflowID,
				version,
				err,
			)
		}
		referencedTargets := make(map[string]struct{})
		for _, reference := range references {
			if _, target := targets[reference.WorkerAgentID]; target {
				referencedTargets[reference.WorkerAgentID] = struct{}{}
			}
		}
		for workerID := range referencedTargets {
			counts[workerID]++
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("scan roster revocation impact: %w", err)
	}
	return counts, nil
}
