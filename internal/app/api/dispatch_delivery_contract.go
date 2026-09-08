package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jinyitao123/weave/internal/base/deliverable"
	"github.com/jinyitao123/weave/internal/base/frozen"
	"github.com/jinyitao123/weave/internal/base/snapshot"
	"github.com/jinyitao123/weave/internal/kernel/workflow/machine"
)

// A machine contract is accepted only from the exact Host-bound user text.
// The model-facing dispatch tool has no field that can replace these facts.
// Output shape is always inherited from the admitted published graph.
func parseDispatchDeliveryContract(task string) (*deliverable.DeliveryContract, error) {
	const marker = "```weave-delivery-contract-v1"
	var body strings.Builder
	inside, found, closed := false, false, false
	for _, line := range strings.Split(task, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == marker {
			if found {
				return nil, fmt.Errorf("only one delivery contract is allowed in the bound input")
			}
			inside, found = true, true
			continue
		}
		if inside && trimmed == "```" {
			inside, closed = false, true
			continue
		}
		if inside {
			body.WriteString(line)
			body.WriteByte('\n')
		}
	}
	if !found {
		return nil, nil
	}
	if !closed || body.Len() == 0 || body.Len() > 64*1024 {
		return nil, fmt.Errorf("delivery contract must be a closed JSON block of at most 64 KiB")
	}
	canonical, err := frozen.CanonicalizeJSON([]byte(body.String()))
	if err != nil {
		return nil, fmt.Errorf("delivery contract JSON is invalid: %w", err)
	}
	// This input DTO intentionally excludes Output and all evidence/results.
	var wire struct {
		Version                int                               `json:"version"`
		Coverage               string                            `json:"coverage"`
		RequiredArtifacts      []deliverable.ArtifactRequirement `json:"required_artifacts"`
		RequiredChecks         []deliverable.CheckSpec           `json:"required_checks"`
		ExternalEffectsCheckID string                            `json:"external_effects_check_id"`
		Limitations            []string                          `json:"limitations"`
	}
	decoder := json.NewDecoder(bytes.NewReader(canonical))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&wire); err != nil {
		return nil, fmt.Errorf("delivery contract fields are invalid: %w", err)
	}
	contract := &deliverable.DeliveryContract{
		Version: wire.Version, Coverage: wire.Coverage, RequiredArtifacts: wire.RequiredArtifacts,
		RequiredChecks: wire.RequiredChecks, ExternalEffectsCheckID: wire.ExternalEffectsCheckID,
		Limitations: wire.Limitations,
	}
	if err := deliverable.ValidateDeliveryContract(contract); err != nil {
		return nil, err
	}
	return contract, nil
}

func (s *Server) freezeDispatchDeliveryContractTx(ctx context.Context, tx pgx.Tx, snap *snapshot.TeamRunSnapshot, request teamDispatchRequest) error {
	if s.Deliverables == nil {
		return fmt.Errorf("delivery contract store is unavailable")
	}
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
	contract := &deliverable.DeliveryContract{Version: 1, Coverage: "incomplete", Limitations: []string{"explicit_user_delivery_scope_missing"}}
	if request.inputBinding != nil && len(request.inputBinding.DeliveryContract) > 0 && string(request.inputBinding.DeliveryContract) != "{}" && string(request.inputBinding.DeliveryContract) != "null" {
		contract = new(deliverable.DeliveryContract)
		if err := json.Unmarshal(request.inputBinding.DeliveryContract, contract); err != nil {
			return fmt.Errorf("read registered delivery contract: %w", err)
		}
	}
	contract.Output = deliverable.OutputRequirement{Type: string(graph.OutputContract.Type), Schema: graph.OutputContract.Schema}
	_, err = s.Deliverables.FreezeContractTx(ctx, tx, deliverable.ContractBinding{
		WorkspaceID: snap.WorkspaceID, RunSnapshotID: snap.RunID, InputRevisionID: request.InputRevisionID,
		WorkflowID: snap.WorkflowID, WorkflowVersion: snap.WorkflowVersion, PublishedDigest: envelope.ContentHash,
		Contract: contract,
	})
	return err
}
