package teamconstruction

import (
	"context"
	"errors"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jinyitao123/weave/internal/app/workflowcatalog"
	"github.com/jinyitao123/weave/internal/base/execution"
	"github.com/jinyitao123/weave/internal/base/frozen"
	"github.com/jinyitao123/weave/internal/kernel/workflow"
	"github.com/jinyitao123/weave/internal/kernel/workflow/machine"
)

// PublicationAuthority refreshes product access and exact dependency proofs;
// these read transactions are never shared with the kernel's write transaction.
type publicationProductReadTxKey struct{}

type PublicationAuthority struct {
	pool    *pgxpool.Pool
	builder *workflowcatalog.CandidateBuilder
}

func NewPublicationAuthority(pool *pgxpool.Pool, builder *workflowcatalog.CandidateBuilder) *PublicationAuthority {
	authority := &PublicationAuthority{pool: pool}
	if builder != nil {
		authority.builder = builder.WithCredentialAuthority(authority)
	}
	return authority
}

func (a *PublicationAuthority) Authorize(ctx context.Context, _ string, envelope frozen.ArtifactEnvelopeV1) (machine.ValidationContext, error) {
	if a == nil || a.pool == nil || a.builder == nil {
		return machine.ValidationContext{}, errors.New("publication authority unavailable")
	}
	tx, err := a.pool.Begin(ctx)
	if err != nil {
		return machine.ValidationContext{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	payload, err := frozen.DecodeArtifactEnvelopeV1(envelope)
	if err != nil {
		return machine.ValidationContext{}, err
	}
	if err = authorizePublicationActorTx(ctx, tx, envelope.WorkspaceID, payload.Team.TeamID); err != nil {
		return machine.ValidationContext{}, err
	}
	var draft workflow.PublicationDraftRead
	err = tx.QueryRow(ctx, `SELECT w.workspace_id,w.id,w.team_id,w.status,w.updated_at,v.workspace_id,v.workflow_id,v.version,v.status,v.trigger_config,v.graph_definition,v.updated_at FROM weave_team_workflows w JOIN weave_team_workflow_versions v ON v.workspace_id=w.workspace_id AND v.workflow_id=w.id WHERE w.workspace_id=$1 AND w.id=$2 AND v.version=$3 FOR UPDATE OF w,v`, envelope.WorkspaceID, envelope.WorkflowID, envelope.WorkflowVersion).Scan(&draft.Workflow.WorkspaceID, &draft.Workflow.ID, &draft.Workflow.TeamID, &draft.Workflow.Status, &draft.Workflow.UpdatedAt, &draft.Draft.WorkspaceID, &draft.Draft.WorkflowID, &draft.Draft.Version, &draft.Draft.Status, &draft.Draft.TriggerConfig, &draft.Draft.GraphDefinition, &draft.Draft.UpdatedAt)
	if err != nil {
		return machine.ValidationContext{}, err
	}
	return a.builder.ValidateResolvedCandidateTx(context.WithValue(ctx, publicationProductReadTxKey{}, tx), tx, &draft, envelope)
}

func (a *PublicationAuthority) AuthorizeProduct(ctx context.Context, command PublicationCommand) error {
	if a == nil || a.pool == nil {
		return errors.New("publication authority unavailable")
	}
	tx, borrowed := ctx.Value(publicationProductReadTxKey{}).(pgx.Tx)
	if !borrowed {
		var err error
		tx, err = a.pool.Begin(ctx)
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback(ctx) }()
	}
	return authorizePublicationActorTx(ctx, tx, command.Request.Candidate.WorkspaceID, command.Target.TeamID)
}

func authorizePublicationActorTx(ctx context.Context, tx pgx.Tx, workspaceID, teamID string) error {
	subject, err := execution.RequireSubject(ctx, workspaceID)
	if err != nil {
		return err
	}
	var active bool
	if subject.UserID != "" {
		err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM weave_users WHERE id=$1 AND tenant_id=$2 AND NOT COALESCE(disabled,false))`, subject.UserID, workspaceID).Scan(&active)
	} else if strings.HasPrefix(subject.ServiceID, "api-key:") {
		err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM weave_api_keys WHERE id=$1 AND tenant_id=$2 AND (expires_at IS NULL OR expires_at>now()))`, strings.TrimPrefix(subject.ServiceID, "api-key:"), workspaceID).Scan(&active)
	}
	if strings.HasPrefix(subject.ServiceID, "workflow-schedule:") {
		err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM weave_schedule s JOIN weave_team_workflows w ON w.workspace_id=s.workspace_id AND w.id=s.target_workflow_id WHERE s.workspace_id=$1 AND s.id=$2 AND s.enabled AND s.target_kind='team_workflow' AND w.team_id=$3)`, workspaceID, strings.TrimPrefix(subject.ServiceID, "workflow-schedule:"), teamID).Scan(&active)
	}
	if err != nil {
		return err
	}
	if !active {
		return errors.New("publication actor authorization denied")
	}
	err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM weave_teams WHERE workspace_id=$1 AND id=$2 AND status IN ('active','building'))`, workspaceID, teamID).Scan(&active)
	if err != nil {
		return err
	}
	if !active {
		return errors.New("publication team authorization denied")
	}
	return nil
}

// BuildCandidateTx compiles a product draft with the current team's scoped
// resource authorization. Its caller still owns only a product transaction.
func (a *PublicationAuthority) BuildCandidateTx(ctx context.Context, tx pgx.Tx, input workflow.CandidateInput) (*workflow.PublicationCandidate, *machine.Report, error) {
	if a == nil || a.builder == nil {
		return nil, nil, errors.New("publication authority unavailable")
	}
	return a.builder.BuildTx(context.WithValue(ctx, publicationProductReadTxKey{}, tx), tx, input)
}
