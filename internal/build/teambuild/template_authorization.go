package teambuild

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
)

// AuthorizeTemplateBuildRun is the only server-owned template_auto entry.
// The real authenticated user remains confirmed_by; the platform decision
// principal and reason are generated and persisted by AuthorizeBuildRun.
func (s *Store) AuthorizeTemplateBuildRun(
	ctx context.Context,
	workspaceID, buildRunID, confirmedBy string,
	revisionToken BlueprintRevisionToken,
	policy TemplateAuthorizationPolicy,
) (TeamBuildRun, BuildAuthorizationReceipt, error) {
	return s.AuthorizeBuildRun(ctx, workspaceID, buildRunID, confirmedBy, nil, AuthorizeOptions{
		Authority:       AuthorizationTemplateAuto,
		RevisionToken:   &revisionToken,
		DecisionSubject: TemplateAuthorizerSubject,
		TemplatePolicy:  &policy,
	})
}

func (s *Store) reserveTemplateAuthorizationTx(
	ctx context.Context,
	tx pgx.Tx,
	workspaceID, buildRunID string,
	requestedBudget float64,
	policy TemplateAuthorizationPolicy,
	now time.Time,
) (string, error) {
	// Template instantiation only materializes a user-confirmed, frozen team and
	// workflow. The declared budget governs later team runs; it is not consumed
	// by this deterministic operation and therefore must not gate team creation.
	_ = ctx
	_ = tx
	_ = workspaceID
	_ = buildRunID
	_ = requestedBudget
	_ = policy
	_ = now
	return "user-confirmed frozen template materialization; declared budget applies only to later team runs", nil
}
