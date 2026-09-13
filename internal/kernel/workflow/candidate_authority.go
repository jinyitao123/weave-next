package workflow

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jinyitao123/weave/internal/base/frozen"
	"github.com/jinyitao123/weave/internal/kernel/workflow/machine"
)

// ValidateResolvedCandidateTx refreshes exact-version resource proofs for
// a product-owned exact workflow version. It is a compiler-side product-read adapter,
// scheduled to move with CandidateBuilder into weave-server. It does not publish
// or admit, and its transaction must end before either kernel service call.
func (b *CandidateBuilder) ValidateResolvedCandidateTx(ctx context.Context, tx pgx.Tx, draft *PublicationDraftRead, envelope frozen.ArtifactEnvelopeV1) (machine.ValidationContext, error) {
	payload, err := frozen.DecodeArtifactEnvelopeV1(envelope)
	if err != nil {
		return machine.ValidationContext{}, err
	}
	if b == nil || b.workflows == nil || tx == nil {
		return machine.ValidationContext{}, errors.New("candidate authority unavailable")
	}
	if draft == nil || draft.Workflow.WorkspaceID != envelope.WorkspaceID || draft.Workflow.ID != envelope.WorkflowID || draft.Draft.Version != envelope.WorkflowVersion {
		return machine.ValidationContext{}, ErrVersionConflict
	}
	row, version := draft.Workflow, draft.Draft
	if row.Status == WorkflowStatusArchived || row.TeamID != payload.Team.TeamID {
		return machine.ValidationContext{}, ErrArchived
	}
	// The requested frozen graph must still represent this exact product version;
	// fresh dependency enumeration below cannot replace the envelope on a retry.
	requestedTrigger, err := frozen.CanonicalizeJSON(payload.TriggerConfig)
	if err != nil {
		return machine.ValidationContext{}, err
	}
	currentTrigger, err := frozen.CanonicalizeJSON(version.TriggerConfig)
	if err != nil {
		return machine.ValidationContext{}, err
	}
	requestedGraph, err := frozen.CanonicalizeJSON(payload.GraphDefinition)
	if err != nil {
		return machine.ValidationContext{}, err
	}
	currentGraph, err := frozen.CanonicalizeJSON(version.GraphDefinition)
	if err != nil {
		return machine.ValidationContext{}, err
	}
	if string(requestedTrigger) != string(currentTrigger) || string(requestedGraph) != string(currentGraph) {
		return machine.ValidationContext{}, ErrVersionConflict
	}
	candidate, report, err := b.buildResolvedCandidateTx(ctx, tx, CandidateInput{WorkspaceID: envelope.WorkspaceID, WorkflowID: envelope.WorkflowID, WorkflowVersion: envelope.WorkflowVersion}, draft, &machine.AgentVersionKey{AgentID: payload.Team.LeadAgentID, AgentVersion: payload.Team.LeadAgentVersion})
	if err != nil {
		return machine.ValidationContext{}, err
	}
	if candidate == nil || report == nil || len(report.Issues) != 0 {
		return machine.ValidationContext{}, errors.New("candidate authority validation failed")
	}
	if candidate.ContentHash != envelope.ContentHash {
		return machine.ValidationContext{}, ErrVersionConflict
	}
	return candidate.validation, nil
}
