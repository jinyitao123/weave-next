package teamconstruction

import (
	"context"
	"errors"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jinyitao123/weave/internal/app/agentcatalog"
	"github.com/jinyitao123/weave/internal/base/execution"
	"github.com/jinyitao123/weave/internal/base/frozen"
	"github.com/jinyitao123/weave/internal/kernel/credentials"
	"github.com/jinyitao123/weave/internal/kernel/registry"
	"github.com/jinyitao123/weave/internal/kernel/workflow"
	"github.com/jinyitao123/weave/internal/kernel/workflow/machine"
)

// A proof is deliberately process-local and short-lived. No grant field exists
// in an API or kernel request. Every use rereads current actor/team/resource state.
type publicationCredentialProof struct {
	actor      execution.Subject
	scope      workflow.CandidateCredentialScope
	references []frozen.CredentialReference
	expiresAt  time.Time
}

func (a *PublicationAuthority) AuthorizeCandidateCredential(ctx context.Context, scope workflow.CandidateCredentialScope, ref frozen.CredentialReference) error {
	subject, err := execution.RequireSubject(ctx, scope.WorkspaceID)
	if err != nil {
		return err
	}
	proof := publicationCredentialProof{actor: subject, scope: scope, references: []frozen.CredentialReference{ref}, expiresAt: time.Now().Add(time.Minute)}
	return a.verifyCredentialProof(ctx, proof, ref)
}

// AuthorizeCredential is the kernel's pure credential-authorization port. The
// kernel calls it only after validating the complete frozen candidate facts.
func (a *PublicationAuthority) AuthorizeCredential(ctx context.Context, envelope frozen.ArtifactEnvelopeV1, ref frozen.CredentialReference) error {
	subject, err := execution.RequireSubject(ctx, envelope.WorkspaceID)
	if err != nil {
		return err
	}
	payload, err := frozen.DecodeArtifactEnvelopeV1(envelope)
	if err != nil {
		return err
	}
	graph, report := machine.DecodeGraphDefinitionV1(payload.GraphDefinition)
	if report != nil && len(report.Issues) > 0 {
		return errors.New("credential graph invalid")
	}
	trigger, report := machine.DecodeTriggerConfigV1(payload.TriggerConfig)
	if report != nil && len(report.Issues) > 0 {
		return errors.New("credential trigger invalid")
	}
	scope := workflow.CandidateCredentialScope{WorkspaceID: envelope.WorkspaceID, TeamID: payload.Team.TeamID, Lead: machine.AgentVersionKey{AgentID: payload.Team.LeadAgentID, AgentVersion: payload.Team.LeadAgentVersion}}
	for _, reference := range machine.ReferencedBundles(scope.Lead, graph) {
		scope.Agents = append(scope.Agents, reference.Key)
	}
	if trigger.Delivery != nil {
		scope.DeliveryTargetID = trigger.Delivery.Ref
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
	return a.verifyCredentialProof(ctx, proof, ref)
}

func (a *PublicationAuthority) verifyCredentialProof(ctx context.Context, proof publicationCredentialProof, ref frozen.CredentialReference) error {
	if a == nil || a.pool == nil || !time.Now().Before(proof.expiresAt) || !slices.Contains(proof.references, ref) {
		return execution.ErrSubjectMismatch
	}
	subject, err := execution.RequireSubject(ctx, proof.scope.WorkspaceID)
	if err != nil {
		return err
	}
	if subject != proof.actor || ref.WorkspaceID != subject.WorkspaceID {
		return execution.ErrSubjectMismatch
	}
	if err = frozen.ValidateCredentialReference(ref); err != nil {
		return err
	}
	if ref.Scope == frozen.CredentialScopeUser {
		if err = credentials.AuthorizeReference(ctx, ref); err != nil {
			return err
		}
	}
	// During draft compilation the server reuses its own read/materialization
	// transaction so freshly created exact versions are visible. That private
	// transaction is never attached to a kernel Publish/Admit request.
	tx, borrowed := ctx.Value(publicationProductReadTxKey{}).(pgx.Tx)
	if !borrowed {
		tx, err = a.pool.Begin(ctx)
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback(ctx) }()
	}
	if err = authorizePublicationActorTx(ctx, tx, subject.WorkspaceID, proof.scope.TeamID); err != nil {
		return err
	}
	var leadID string
	if err = tx.QueryRow(ctx, `SELECT lead_avatar_id FROM weave_teams WHERE workspace_id=$1 AND id=$2`, subject.WorkspaceID, proof.scope.TeamID).Scan(&leadID); err != nil {
		return err
	}
	if leadID != proof.scope.Lead.AgentID {
		return execution.ErrSubjectMismatch
	}
	var records []registry.AgentRecord
	catalog := agentcatalog.New(a.pool)
	for _, key := range proof.scope.Agents {
		if key.AgentID != leadID {
			var member bool
			if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM weave_team_workers WHERE workspace_id=$1 AND team_id=$2 AND worker_agent_id=$3 AND enabled)`, subject.WorkspaceID, proof.scope.TeamID, key.AgentID).Scan(&member); err != nil {
				return err
			}
			if !member {
				return execution.ErrSubjectMismatch
			}
		}
		version := key.AgentVersion
		record, err := catalog.ResolveAgentVersionTx(ctx, tx, subject.WorkspaceID, key.AgentID, &version)
		if err != nil {
			return err
		}
		records = append(records, *record)
	}
	allowed, err := credentialSelectedByTeamTx(ctx, tx, proof.scope, records, ref)
	if err != nil {
		return err
	}
	if !allowed {
		return execution.ErrSubjectMismatch
	}
	return nil
}

func credentialSelectedByTeamTx(ctx context.Context, tx pgx.Tx, scope workflow.CandidateCredentialScope, records []registry.AgentRecord, ref frozen.CredentialReference) (bool, error) {
	var enabled bool
	switch ref.Kind {
	case frozen.CredentialProviderAPIKey:
		var models []string
		err := tx.QueryRow(ctx, `SELECT models FROM weave_provider_credentials WHERE workspace_id=$1 AND id=$2 AND credential_scope=$3 AND credential_user_id=$4 AND credential_service_id=$5 AND enabled AND revoked_at IS NULL AND deleted_at IS NULL`, ref.WorkspaceID, ref.ResourceID, ref.Scope, ref.UserID, ref.ServiceID).Scan(&models)
		if err != nil {
			return false, err
		}
		for _, record := range records {
			for _, model := range append([]string{record.Model}, record.FallbackModels...) {
				if model != "" && slices.Contains(models, model) {
					return true, nil
				}
			}
		}
	case frozen.CredentialRuntimeAccess:
		if ref.Scope != frozen.CredentialScopeWorkspaceService || ref.ServiceID != "runtime:"+ref.ResourceID {
			return false, nil
		}
		bound := slices.ContainsFunc(records, func(record registry.AgentRecord) bool { return record.RuntimeID == ref.ResourceID })
		if !bound {
			return false, nil
		}
		err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM weave_runtimes WHERE workspace_id=$1 AND id=$2 AND enabled AND revoked_at IS NULL AND deleted_at IS NULL)`, ref.WorkspaceID, ref.ResourceID).Scan(&enabled)
		return enabled, err
	case frozen.CredentialMCPServerAccess:
		if ref.Scope != frozen.CredentialScopeWorkspaceService || ref.ServiceID != "mcp:"+ref.ResourceID {
			return false, nil
		}
		bound := false
		for _, record := range records {
			for _, server := range record.MCPServers {
				bound = bound || server.ServerID == ref.ResourceID
			}
		}
		if !bound {
			return false, nil
		}
		err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM weave_mcp_servers WHERE workspace_id=$1 AND id=$2 AND enabled AND revoked_at IS NULL AND deleted_at IS NULL)`, ref.WorkspaceID, ref.ResourceID).Scan(&enabled)
		return enabled, err
	case frozen.CredentialDeliveryTargetAccess:
		if ref.Scope != frozen.CredentialScopeWorkspaceService || ref.ServiceID != "delivery:"+ref.ResourceID || scope.DeliveryTargetID != ref.ResourceID {
			return false, nil
		}
		err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM weave_delivery_targets WHERE workspace_id=$1 AND id=$2 AND enabled AND revoked_at IS NULL AND deleted_at IS NULL)`, ref.WorkspaceID, ref.ResourceID).Scan(&enabled)
		return enabled, err
	}
	return false, nil
}
