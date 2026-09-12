package workflowdispatch

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jinyitao123/weave/internal/base/deliverable"
	"github.com/jinyitao123/weave/internal/base/frozen"
	"github.com/jinyitao123/weave/internal/base/snapshot"
	"github.com/jinyitao123/weave/internal/kernel/workflow/machine"
)

func (service *Service) freezeDeliveryContractTx(
	ctx context.Context,
	tx pgx.Tx,
	snap *snapshot.TeamRunSnapshot,
	request PersistRequest,
) error {
	var envelope frozen.ArtifactEnvelopeV1
	if err := tx.QueryRow(ctx, `SELECT workspace_id, workflow_id, workflow_version,
		artifact_schema_version, canonicalization_algorithm, canonicalization_version,
		hash_algorithm, content_hash, payload FROM weave_published_artifact_contents
		WHERE workspace_id=$1 AND workflow_id=$2 AND workflow_version=$3`,
		snap.WorkspaceID, snap.ArtifactWorkflowID, snap.ArtifactWorkflowVersion).Scan(
		&envelope.WorkspaceID, &envelope.WorkflowID, &envelope.WorkflowVersion,
		&envelope.ArtifactSchemaVersion, &envelope.CanonicalizationAlgorithm, &envelope.CanonicalizationVersion,
		&envelope.HashAlgorithm, &envelope.ContentHash, &envelope.Payload,
	); err != nil {
		return fmt.Errorf("read frozen delivery output contract: %w", err)
	}
	payload, err := frozen.DecodeArtifactEnvelopeV1(envelope)
	if err != nil {
		return err
	}
	graph, report := machine.DecodeGraphDefinitionV1(payload.GraphDefinition)
	if report != nil && len(report.Issues) > 0 {
		return fmt.Errorf("frozen delivery graph is invalid")
	}
	contract := &deliverable.DeliveryContract{
		Version: 1, Coverage: "incomplete",
		Limitations: []string{"explicit_user_delivery_scope_missing"},
	}
	if graph.DeliveryContract != nil {
		contract = deliverable.CloneDeliveryContract(graph.DeliveryContract)
	}
	if len(request.DeliveryContract) > 0 && string(request.DeliveryContract) != "{}" && string(request.DeliveryContract) != "null" {
		contract, err = deliverable.DecodeDeliveryContract(request.DeliveryContract)
		if err != nil {
			return fmt.Errorf("read registered delivery contract: %w", err)
		}
	}
	contract.Output = deliverable.OutputRequirement{
		Type: string(graph.OutputContract.Type), Schema: graph.OutputContract.Schema,
	}
	if err := deliverable.ValidateDeliveryContract(contract); err != nil {
		return fmt.Errorf("validate frozen delivery contract: %w", err)
	}
	_, err = service.Deliverables.FreezeContractTx(ctx, tx, deliverable.ContractBinding{
		WorkspaceID: snap.WorkspaceID, RunSnapshotID: snap.RunID,
		InputRevisionID: request.InputRevisionID,
		WorkflowID:      snap.WorkflowID, WorkflowVersion: snap.WorkflowVersion,
		PublishedDigest: envelope.ContentHash, Contract: contract,
	})
	return err
}
