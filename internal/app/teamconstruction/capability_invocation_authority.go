package teamconstruction

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	appcapabilities "github.com/jinyitao123/weave/internal/app/capabilities"
	"github.com/jinyitao123/weave/internal/base/execution"
	"github.com/jinyitao123/weave/internal/base/frozen"
	"github.com/jinyitao123/weave/internal/kernel/publication"
	"github.com/jinyitao123/weave/internal/kernel/workflow"
	"github.com/jinyitao123/weave/internal/kernel/workflow/machine"
)

func capabilityApplicationID(subject execution.Subject) string {
	return strings.TrimPrefix(subject.ServiceID, "capability-app:")
}

func isCapabilityApplicationSubject(subject execution.Subject) bool {
	return strings.HasPrefix(subject.ServiceID, "capability-app:") && capabilityApplicationID(subject) != ""
}

func (a *PublicationAuthority) authorizeCapabilityApplicationInvocationTx(
	ctx context.Context,
	tx pgx.Tx,
	subject execution.Subject,
	request publication.PublishedRunRequest,
	envelope frozen.ArtifactEnvelopeV1,
) error {
	if !isCapabilityApplicationSubject(subject) || subject.WorkspaceID != envelope.WorkspaceID ||
		request.Trigger.Type != "manual" || request.Trigger.SourceRef != capabilityApplicationID(subject) {
		return execution.ErrSubjectMismatch
	}
	var invocationID, credentialID, capabilityID, definitionHash, workflowID, inputHash, admissionRequestID, taskID string
	var capabilityRevision int64
	var workflowVersion int
	err := tx.QueryRow(ctx, `SELECT invocation_id,credential_id,capability_id,revision,definition_hash,workflow_id,
	 workflow_version,input_hash,admission_request_id,task_id
	 FROM weave_capability_workflow_invocations
	 WHERE workspace_id=$1 AND application_id=$2 AND run_id=$3 FOR SHARE`,
		subject.WorkspaceID, capabilityApplicationID(subject), request.RunID).Scan(
		&invocationID, &credentialID, &capabilityID, &capabilityRevision, &definitionHash,
		&workflowID, &workflowVersion, &inputHash, &admissionRequestID, &taskID)
	if errors.Is(err, pgx.ErrNoRows) {
		return execution.ErrSubjectMismatch
	}
	if err != nil {
		return err
	}
	if request.RequestID != admissionRequestID || request.ContextKey != invocationID || request.TaskID != taskID ||
		request.InputVersion != "sha256:"+inputHash || workflowID != envelope.WorkflowID || workflowVersion != envelope.WorkflowVersion ||
		request.Revision != publication.CandidateRevision(envelope) {
		return publication.ErrRequestConflict
	}
	var appID string
	if err := tx.QueryRow(ctx, `SELECT id FROM weave_capability_apps WHERE workspace_id=$1 AND id=$2 AND enabled FOR SHARE`,
		subject.WorkspaceID, capabilityApplicationID(subject)).Scan(&appID); err != nil {
		return appcapabilities.ErrAccessDenied
	}
	var granted bool
	if err := tx.QueryRow(ctx, `SELECT enabled FROM weave_capability_grants WHERE workspace_id=$1 AND app_id=$2
	 AND capability_id=$3 AND revision=$4 AND enabled FOR SHARE`, subject.WorkspaceID, appID, capabilityID, capabilityRevision).Scan(&granted); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return appcapabilities.ErrAccessDenied
		}
		return err
	}
	var storedHash string
	var raw []byte
	if err := tx.QueryRow(ctx, `SELECT definition_hash,definition FROM weave_capability_revisions
	 WHERE workspace_id=$1 AND capability_id=$2 AND revision=$3`, subject.WorkspaceID, capabilityID, capabilityRevision).Scan(&storedHash, &raw); err != nil {
		return err
	}
	if storedHash != definitionHash {
		return publication.ErrRequestConflict
	}
	var definition struct {
		Workflow *struct {
			WorkflowID      string `json:"workflow_id"`
			WorkflowVersion int    `json:"workflow_version"`
		} `json:"workflow"`
	}
	if err := json.Unmarshal(raw, &definition); err != nil || definition.Workflow == nil ||
		definition.Workflow.WorkflowID != workflowID || definition.Workflow.WorkflowVersion != workflowVersion {
		return publication.ErrRequestConflict
	}
	return nil
}

// AuthorizePublishedCredential allows only a credential frozen into the exact
// granted workflow capability revision. Provider ownership is still checked
// against the provider catalog after this invocation-specific grant check.
func (a *PublicationAuthority) AuthorizePublishedCredential(
	ctx context.Context,
	request publication.PublishedRunRequest,
	envelope frozen.ArtifactEnvelopeV1,
	ref frozen.CredentialReference,
) error {
	subject, err := execution.RequireSubject(ctx, envelope.WorkspaceID)
	if err != nil {
		return err
	}
	if !isCapabilityApplicationSubject(subject) {
		return a.AuthorizeCredential(ctx, envelope, ref)
	}
	if ref.Kind != frozen.CredentialProviderAPIKey || ref.Scope != frozen.CredentialScopeWorkspaceService {
		return frozen.ErrCredentialSubjectDenied
	}
	payload, err := frozen.DecodeArtifactEnvelopeV1(envelope)
	if err != nil {
		return err
	}
	graph, report := machine.DecodeGraphDefinitionV1(payload.GraphDefinition)
	if report != nil && len(report.Issues) > 0 {
		return errors.New("published capability workflow graph is invalid")
	}
	trigger, report := machine.DecodeTriggerConfigV1(payload.TriggerConfig)
	if report != nil && len(report.Issues) > 0 {
		return errors.New("published capability workflow trigger is invalid")
	}
	if trigger.Type != machine.TriggerConversationExplicit || trigger.Delivery != nil {
		return errors.New("published capability workflow trigger does not match the pure invocation contract")
	}
	scope := workflow.CandidateCredentialScope{WorkspaceID: envelope.WorkspaceID, TeamID: payload.Team.TeamID,
		WorkflowID: envelope.WorkflowID, WorkflowVersion: envelope.WorkflowVersion,
		Lead: machine.AgentVersionKey{AgentID: payload.Team.LeadAgentID, AgentVersion: payload.Team.LeadAgentVersion}}
	for _, reference := range machine.ReferencedBundles(scope.Lead, graph) {
		scope.Agents = append(scope.Agents, reference.Key)
	}
	proof := publicationCredentialProof{actor: subject, scope: scope, expiresAt: time.Now().Add(time.Minute)}
	for _, bundle := range payload.Bundles {
		proof.references = append(proof.references, bundle.Credentials...)
	}
	for _, target := range payload.DeliveryTargets {
		proof.references = append(proof.references, target.AccessRef)
		for _, binding := range target.CredentialBindings {
			proof.references = append(proof.references, binding.CredentialRef)
		}
	}
	return a.verifyPublishedCredentialProof(ctx, proof, ref, request, envelope)
}
