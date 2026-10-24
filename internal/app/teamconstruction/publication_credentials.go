package teamconstruction

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jinyitao123/weave/internal/app/agentcatalog"
	"github.com/jinyitao123/weave/internal/base/execution"
	"github.com/jinyitao123/weave/internal/base/frozen"
	"github.com/jinyitao123/weave/internal/kernel/credentials"
	"github.com/jinyitao123/weave/internal/kernel/publication"
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
	scope := workflow.CandidateCredentialScope{
		WorkspaceID: envelope.WorkspaceID, TeamID: payload.Team.TeamID,
		WorkflowID: envelope.WorkflowID, WorkflowVersion: envelope.WorkflowVersion,
		Lead: machine.AgentVersionKey{AgentID: payload.Team.LeadAgentID, AgentVersion: payload.Team.LeadAgentVersion},
	}
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
	return a.verifyCredentialProofForInvocation(ctx, proof, ref, nil, nil)
}

func (a *PublicationAuthority) verifyPublishedCredentialProof(
	ctx context.Context,
	proof publicationCredentialProof,
	ref frozen.CredentialReference,
	request publication.PublishedRunRequest,
	envelope frozen.ArtifactEnvelopeV1,
) error {
	return a.verifyCredentialProofForInvocation(ctx, proof, ref, &request, &envelope)
}

func (a *PublicationAuthority) verifyCredentialProofForInvocation(
	ctx context.Context,
	proof publicationCredentialProof,
	ref frozen.CredentialReference,
	request *publication.PublishedRunRequest,
	envelope *frozen.ArtifactEnvelopeV1,
) error {
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
	if ref.Scope == frozen.CredentialScopeUser && request == nil {
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
	if request != nil && envelope != nil {
		err = a.authorizeCapabilityApplicationInvocationTx(ctx, tx, subject, *request, *envelope)
	} else {
		err = authorizePublicationActorTx(ctx, tx, subject.WorkspaceID, proof.scope.TeamID)
	}
	if err != nil {
		return err
	}
	return a.verifyCredentialScopeTx(ctx, tx, subject.WorkspaceID, proof.scope, ref)
}

// verifyCredentialScopeTx checks a credential against a team scope: the
// team's current lead, its enabled (or staged) members, their exact
// versions, and that one of them selects the credential.
func (a *PublicationAuthority) verifyCredentialScopeTx(ctx context.Context, tx pgx.Tx, workspaceID string, scope workflow.CandidateCredentialScope, ref frozen.CredentialReference) error {
	var leadID string
	if err := tx.QueryRow(ctx, `SELECT lead_avatar_id FROM weave_teams WHERE workspace_id=$1 AND id=$2`, workspaceID, scope.TeamID).Scan(&leadID); err != nil {
		return err
	}
	if leadID != scope.Lead.AgentID {
		return execution.ErrSubjectMismatch
	}
	catalog := agentcatalog.New(a.pool)
	stagedRoster, err := readStagedDevelopmentRosterTx(ctx, tx, scope)
	if err != nil {
		return err
	}
	// The scope's members and their exact versions are read in one statement
	// each, not once per member.
	workers := make([]string, 0, len(scope.Agents))
	keys := make([]agentcatalog.AgentVersionKey, 0, len(scope.Agents))
	for _, key := range scope.Agents {
		if key.AgentID != leadID {
			workers = append(workers, key.AgentID)
		}
		keys = append(keys, agentcatalog.AgentVersionKey{AgentID: key.AgentID, Version: key.AgentVersion})
	}
	enabled := map[string]bool{}
	if len(workers) > 0 {
		rows, err := tx.Query(ctx, `SELECT worker_agent_id FROM weave_team_workers WHERE workspace_id=$1 AND team_id=$2 AND enabled AND worker_agent_id=ANY($3)`, workspaceID, scope.TeamID, workers)
		if err != nil {
			return err
		}
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return err
			}
			enabled[id] = true
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
	}
	for _, id := range workers {
		if !enabled[id] && !stagedRosterAllowsAgent(stagedRoster, id) {
			return execution.ErrSubjectMismatch
		}
	}
	records, err := catalog.ResolveAgentVersionsTx(ctx, tx, workspaceID, keys)
	if err != nil {
		return err
	}
	allowed, err := credentialSelectedByTeamTx(ctx, tx, scope, records, ref)
	if err != nil {
		return err
	}
	if !allowed {
		return execution.ErrSubjectMismatch
	}
	return nil
}

func readStagedDevelopmentRosterTx(ctx context.Context, tx pgx.Tx, scope workflow.CandidateCredentialScope) (*registry.PublicationTeamRead, error) {
	if scope.WorkflowID == "" || scope.WorkflowVersion < 1 {
		return nil, nil
	}
	var raw []byte
	// A retained candidate is authoring evidence after publication; only a
	// draft may use it to authorize members not yet in the live roster.
	err := tx.QueryRow(ctx, `SELECT candidate.team_read FROM weave_team_development_candidates candidate
	 JOIN weave_team_workflow_versions version ON version.workspace_id=candidate.workspace_id
	  AND version.workflow_id=candidate.workflow_id AND version.version=candidate.workflow_version
	 WHERE candidate.workspace_id=$1 AND candidate.workflow_id=$2 AND candidate.workflow_version=$3
	  AND candidate.team_id=$4 AND version.status='draft'`, scope.WorkspaceID, scope.WorkflowID, scope.WorkflowVersion, scope.TeamID).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var team registry.PublicationTeamRead
	if err = json.Unmarshal(raw, &team); err != nil {
		return nil, err
	}
	if team.WorkspaceID != scope.WorkspaceID || team.TeamID != scope.TeamID || team.LeadAvatarID != scope.Lead.AgentID || team.LeadAvatarVersion != scope.Lead.AgentVersion {
		return nil, execution.ErrSubjectMismatch
	}
	return &team, nil
}

func stagedRosterAllowsAgent(team *registry.PublicationTeamRead, agentID string) bool {
	if team == nil {
		return false
	}
	return slices.ContainsFunc(team.Workers, func(worker registry.TeamWorker) bool {
		return worker.Enabled && worker.WorkerAgentID == agentID
	})
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
