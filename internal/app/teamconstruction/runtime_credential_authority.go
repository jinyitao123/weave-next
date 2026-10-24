package teamconstruction

import (
	"context"
	"errors"
	"slices"

	"github.com/jackc/pgx/v5"
	appcapabilities "github.com/jinyitao123/weave/internal/app/capabilities"
	"github.com/jinyitao123/weave/internal/base/frozen"
	"github.com/jinyitao123/weave/internal/kernel/teamrun"
	"github.com/jinyitao123/weave/internal/kernel/workflow"
	"github.com/jinyitao123/weave/internal/kernel/workflow/machine"
)

var _ appcapabilities.RuntimeTeamAuthority = (*PublicationAuthority)(nil)

// PrepareRuntimeCredential reads the team scope a capability run's published
// workflow froze, so a model call's full check covers what admission
// checked: the team and its lead, the members and their exact versions, and
// that a member selects the provider. The scope comes from the immutable
// artifact; the check reads the live rows in the resolver's transaction.
func (a *PublicationAuthority) PrepareRuntimeCredential(ctx context.Context, tx pgx.Tx, run teamrun.TeamRun, ref frozen.CredentialReference) (appcapabilities.RuntimeTeamCheck, error) {
	if a == nil || a.pool == nil {
		return appcapabilities.RuntimeTeamCheck{}, errors.New("publication authority unavailable")
	}
	scope, err := a.runtimeCredentialScope(ctx, tx, run, ref)
	if err != nil {
		return appcapabilities.RuntimeTeamCheck{}, err
	}
	return appcapabilities.RuntimeTeamCheck{TeamID: scope.TeamID,
		Authorize: func(ctx context.Context, tx pgx.Tx) error {
			var active bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM weave_teams WHERE workspace_id=$1 AND id=$2 AND status='active')`, run.WorkspaceID, scope.TeamID).Scan(&active); err != nil {
				return err
			}
			if !active {
				return errors.New("published team is not active")
			}
			return a.verifyCredentialScopeTx(ctx, tx, run.WorkspaceID, scope, ref)
		}}, nil
}

type runtimeScopeKey struct {
	workspaceID, workflowID, teamID string
	workflowVersion                 int
	ref                             frozen.CredentialReference
}

// runtimeScopesLimit bounds the scopes one process keeps; each is derived
// from an immutable published artifact and read again when dropped.
const runtimeScopesLimit = 4096

// runtimeCredentialScope derives the team scope from the run's immutable
// published artifact once per workflow version and credential.
func (a *PublicationAuthority) runtimeCredentialScope(ctx context.Context, tx pgx.Tx, run teamrun.TeamRun, ref frozen.CredentialReference) (workflow.CandidateCredentialScope, error) {
	key := runtimeScopeKey{workspaceID: run.WorkspaceID, workflowID: run.WorkflowID, teamID: run.TeamID, workflowVersion: run.WorkflowVersion, ref: ref}
	a.runtimeScopesMu.Lock()
	scope, known := a.runtimeScopes[key]
	a.runtimeScopesMu.Unlock()
	if known {
		return scope, nil
	}
	artifact, err := workflow.NewArtifactStore(a.pool, nil).GetArtifactTx(ctx, tx, run.WorkspaceID, run.WorkflowID, run.WorkflowVersion)
	if err != nil {
		return workflow.CandidateCredentialScope{}, err
	}
	payload, err := frozen.DecodeArtifactEnvelopeV1(frozen.ArtifactEnvelopeV1{WorkspaceID: artifact.WorkspaceID, WorkflowID: artifact.WorkflowID,
		WorkflowVersion: artifact.WorkflowVersion, ArtifactSchemaVersion: artifact.ArtifactSchemaVersion,
		CanonicalizationAlgorithm: artifact.CanonicalizationAlgorithm, CanonicalizationVersion: artifact.CanonicalizationVersion,
		HashAlgorithm: artifact.HashAlgorithm, ContentHash: artifact.ContentHash, Payload: artifact.Payload})
	if err != nil {
		return workflow.CandidateCredentialScope{}, err
	}
	if payload.Team.WorkspaceID != run.WorkspaceID || payload.Team.TeamID != run.TeamID {
		return workflow.CandidateCredentialScope{}, errors.New("published team differs from the run's team")
	}
	graph, report := machine.DecodeGraphDefinitionV1(payload.GraphDefinition)
	if report != nil && len(report.Issues) > 0 {
		return workflow.CandidateCredentialScope{}, errors.New("published capability workflow graph is invalid")
	}
	scope = workflow.CandidateCredentialScope{WorkspaceID: run.WorkspaceID, TeamID: payload.Team.TeamID,
		WorkflowID: run.WorkflowID, WorkflowVersion: run.WorkflowVersion,
		Lead: machine.AgentVersionKey{AgentID: payload.Team.LeadAgentID, AgentVersion: payload.Team.LeadAgentVersion}}
	for _, reference := range machine.ReferencedBundles(scope.Lead, graph) {
		scope.Agents = append(scope.Agents, reference.Key)
	}
	frozenRef := false
	for _, bundle := range payload.Bundles {
		frozenRef = frozenRef || slices.Contains(bundle.Credentials, ref)
	}
	if !frozenRef {
		return workflow.CandidateCredentialScope{}, errors.New("credential is not frozen into the published workflow")
	}
	a.runtimeScopesMu.Lock()
	if a.runtimeScopes == nil || len(a.runtimeScopes) >= runtimeScopesLimit {
		a.runtimeScopes = make(map[runtimeScopeKey]workflow.CandidateCredentialScope)
	}
	a.runtimeScopes[key] = scope
	a.runtimeScopesMu.Unlock()
	return scope, nil
}
