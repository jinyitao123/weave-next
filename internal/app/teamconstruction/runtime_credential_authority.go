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
func (a *PublicationAuthority) PrepareRuntimeCredential(ctx context.Context, _ pgx.Tx, run teamrun.TeamRun, ref frozen.CredentialReference) (appcapabilities.RuntimeTeamCheck, error) {
	if a == nil || a.pool == nil {
		return appcapabilities.RuntimeTeamCheck{}, errors.New("publication authority unavailable")
	}
	artifact, err := workflow.NewArtifactStore(a.pool, nil).GetArtifact(ctx, run.WorkspaceID, run.WorkflowID, run.WorkflowVersion)
	if err != nil {
		return appcapabilities.RuntimeTeamCheck{}, err
	}
	payload, err := frozen.DecodeArtifactEnvelopeV1(frozen.ArtifactEnvelopeV1{WorkspaceID: artifact.WorkspaceID, WorkflowID: artifact.WorkflowID,
		WorkflowVersion: artifact.WorkflowVersion, ArtifactSchemaVersion: artifact.ArtifactSchemaVersion,
		CanonicalizationAlgorithm: artifact.CanonicalizationAlgorithm, CanonicalizationVersion: artifact.CanonicalizationVersion,
		HashAlgorithm: artifact.HashAlgorithm, ContentHash: artifact.ContentHash, Payload: artifact.Payload})
	if err != nil {
		return appcapabilities.RuntimeTeamCheck{}, err
	}
	if payload.Team.WorkspaceID != run.WorkspaceID || payload.Team.TeamID != run.TeamID {
		return appcapabilities.RuntimeTeamCheck{}, errors.New("published team differs from the run's team")
	}
	graph, report := machine.DecodeGraphDefinitionV1(payload.GraphDefinition)
	if report != nil && len(report.Issues) > 0 {
		return appcapabilities.RuntimeTeamCheck{}, errors.New("published capability workflow graph is invalid")
	}
	scope := workflow.CandidateCredentialScope{WorkspaceID: run.WorkspaceID, TeamID: payload.Team.TeamID,
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
		return appcapabilities.RuntimeTeamCheck{}, errors.New("credential is not frozen into the published workflow")
	}
	return appcapabilities.RuntimeTeamCheck{TeamID: scope.TeamID,
		Authorize: func(ctx context.Context, tx pgx.Tx) error {
			return a.verifyCredentialScopeTx(ctx, tx, run.WorkspaceID, scope, ref)
		}}, nil
}
