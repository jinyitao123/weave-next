package teambuild

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// BuildAuthorizationReceipt is minted only by Store.AuthorizeBuildRun inside
// the same atomic transition. Its replay material is intentionally private so
// only the Store can mint a valid credential; server-side validation always
// re-checks the bound facts against the persisted TeamBuildRun.
type BuildAuthorizationReceipt struct {
	workspaceID   string
	buildRunID    string
	contractHash  string
	mode          string
	authority     string
	revisionToken *BlueprintRevisionToken
	assetScope    AssetScope
	roundBudget   Budget
	totalBudget   Budget
	expiresAt     time.Time
	confirmedBy   string
	createdAt     time.Time
}

// Valid reports whether the receipt carries complete minted material.
func (r BuildAuthorizationReceipt) Valid() bool {
	return r.workspaceID != "" && r.buildRunID != "" && r.contractHash != "" &&
		(r.mode == ModeCreate || r.mode == ModeOptimize) &&
		r.confirmedBy != "" && !r.expiresAt.IsZero() && !r.createdAt.IsZero()
}

// WorkspaceID exposes the workspace bound to this receipt.
func (r BuildAuthorizationReceipt) WorkspaceID() string { return r.workspaceID }

// BuildRunID exposes the control record bound to this receipt.
func (r BuildAuthorizationReceipt) BuildRunID() string { return r.buildRunID }

// ContractHash exposes the frozen contract hash bound to this receipt.
func (r BuildAuthorizationReceipt) ContractHash() string { return r.contractHash }

// Mode exposes the build mode bound to this receipt.
func (r BuildAuthorizationReceipt) Mode() string { return r.mode }

// Authority exposes how the run was moved into execution.
func (r BuildAuthorizationReceipt) Authority() string { return r.authority }

// RevisionToken exposes the reviewed blueprint revision bound to this receipt,
// if the authorization was a reviewed-blueprint authorization.
func (r BuildAuthorizationReceipt) RevisionToken() *BlueprintRevisionToken {
	if r.revisionToken == nil {
		return nil
	}
	token := *r.revisionToken
	return &token
}

// AssetScope exposes the frozen asset scope bound to this receipt.
func (r BuildAuthorizationReceipt) AssetScope() AssetScope {
	return cloneAssetScope(r.assetScope)
}

// RoundBudget exposes the per-round budget bound to this receipt.
func (r BuildAuthorizationReceipt) RoundBudget() Budget { return r.roundBudget }

// TotalBudget exposes the total-task budget bound to this receipt.
func (r BuildAuthorizationReceipt) TotalBudget() Budget { return r.totalBudget }

// ExpiresAt exposes the expiry bound to this receipt.
func (r BuildAuthorizationReceipt) ExpiresAt() time.Time { return r.expiresAt }

// ConfirmedBy exposes the workspace admin who confirmed the task.
func (r BuildAuthorizationReceipt) ConfirmedBy() string { return r.confirmedBy }

// CreatedAt exposes the minting time of this receipt.
func (r BuildAuthorizationReceipt) CreatedAt() time.Time { return r.createdAt }

// ReissueReceipt recomposes the BuildAuthorizationReceipt for one persisted
// run from its frozen row (the receipt's fields are private, so only the
// store can mint a valid credential). The run must be live
// (authorized/round_running/publishing), confirmed by a workspace admin, and
// unexpired; the brief and contract canonical hashes are recomputed without
// reapplying the current document-policy revision, then compared with the
// stored values. Authorization already validated the documents before
// freezing them; reissue must not invalidate an authentic in-flight run when
// a later deployment evolves the floor definitions. A tampered row still
// cannot mint a receipt for different content. Server-side validation
// (ValidateReceipt) re-checks every bound fact against the persisted run on
// each tool call, so reissuing does not widen the attack surface.
func (s *Store) ReissueReceipt(
	ctx context.Context,
	workspaceID, buildRunID string,
) (BuildAuthorizationReceipt, error) {
	run, err := s.GetBuildRun(ctx, workspaceID, buildRunID)
	if err != nil {
		return BuildAuthorizationReceipt{}, fmt.Errorf("reissue build receipt: %w", err)
	}
	switch run.Status {
	case StatusAuthorized, StatusRoundRunning, StatusPublishing:
	default:
		if terminalStatuses[run.Status] {
			return BuildAuthorizationReceipt{}, fmt.Errorf(
				"reissue build receipt: build run %q is %s: %w",
				buildRunID, run.Status, ErrReceiptTerminal,
			)
		}
		return BuildAuthorizationReceipt{}, fmt.Errorf(
			"reissue build receipt: build run %q is %s: %w",
			buildRunID, run.Status, ErrReceiptNotIssued,
		)
	}
	if strings.TrimSpace(run.ConfirmedBy) == "" {
		return BuildAuthorizationReceipt{}, fmt.Errorf(
			"reissue build receipt: build run %q has no confirmed_by: %w",
			buildRunID, ErrReceiptInvalid,
		)
	}
	if !s.clock.Now().Before(run.ExpiresAt) {
		return BuildAuthorizationReceipt{}, fmt.Errorf(
			"reissue build receipt: build run %q expired at %s: %w",
			buildRunID, run.ExpiresAt.Format(time.RFC3339), ErrReceiptExpired,
		)
	}
	recomputedBriefHash, err := hashDocument(run.Brief)
	if err != nil {
		return BuildAuthorizationReceipt{}, fmt.Errorf("reissue build receipt: recompute brief hash: %w", err)
	}
	recomputedContractHash, err := hashDocument(run.Contract)
	if err != nil {
		return BuildAuthorizationReceipt{}, fmt.Errorf("reissue build receipt: recompute contract hash: %w", err)
	}
	if recomputedBriefHash != run.BriefHash {
		return BuildAuthorizationReceipt{}, fmt.Errorf(
			"reissue build receipt: stored brief hash does not match frozen content: %w",
			ErrReceiptBriefHashMismatch,
		)
	}
	if recomputedContractHash != run.ContractHash {
		return BuildAuthorizationReceipt{}, fmt.Errorf(
			"reissue build receipt: stored contract hash does not match frozen content: %w",
			ErrReceiptContractHashMismatch,
		)
	}
	return BuildAuthorizationReceipt{
		workspaceID:   workspaceID,
		buildRunID:    buildRunID,
		contractHash:  run.ContractHash,
		mode:          run.Mode,
		authority:     run.Authorization.Authority,
		revisionToken: cloneBlueprintRevisionToken(run.Authorization.RevisionToken),
		assetScope:    cloneAssetScope(run.AssetScope),
		roundBudget:   run.RoundBudget,
		totalBudget:   run.TotalBudget,
		expiresAt:     run.ExpiresAt,
		confirmedBy:   run.ConfirmedBy,
		createdAt:     run.CreatedAt,
	}, nil
}

// ValidateReceipt is the server-side gate future build tools call before any
// write. It re-checks every fact the receipt binds against the persisted
// run: workspace, contract hash, mode, budgets, expiry, non-terminal status,
// and the requested asset's position inside the frozen asset scope.
func (s *Store) ValidateReceipt(
	ctx context.Context,
	workspaceID string,
	receipt BuildAuthorizationReceipt,
	assetRef AssetRef,
) error {
	if !receipt.Valid() {
		return fmt.Errorf("validate build receipt: %w", ErrReceiptInvalid)
	}
	if receipt.WorkspaceID() != workspaceID {
		return fmt.Errorf("validate build receipt: %w", ErrReceiptWorkspaceMismatch)
	}
	run, err := s.GetBuildRun(ctx, workspaceID, receipt.BuildRunID())
	if err != nil {
		return fmt.Errorf("validate build receipt: %w", err)
	}
	if terminalStatuses[run.Status] {
		return fmt.Errorf("validate build receipt: %w", ErrReceiptTerminal)
	}
	if !receipt.ExpiresAt().Equal(run.ExpiresAt) {
		return fmt.Errorf("validate build receipt: %w", ErrReceiptExpiryMismatch)
	}
	if !s.clock.Now().Before(run.ExpiresAt) {
		return fmt.Errorf("validate build receipt: %w", ErrReceiptExpired)
	}
	if receipt.ContractHash() != run.ContractHash {
		return fmt.Errorf("validate build receipt: %w", ErrReceiptContractHashMismatch)
	}
	if receipt.Authority() != run.Authorization.Authority {
		return fmt.Errorf("validate build receipt: %w", ErrBlueprintRevisionMismatch)
	}
	if !sameBlueprintRevisionToken(receipt.RevisionToken(), run.Authorization.RevisionToken) {
		return fmt.Errorf("validate build receipt: %w", ErrBlueprintRevisionMismatch)
	}
	if receipt.Mode() != run.Mode {
		return fmt.Errorf("validate build receipt: %w", ErrReceiptModeMismatch)
	}
	if receipt.RoundBudget() != run.RoundBudget || receipt.TotalBudget() != run.TotalBudget {
		return fmt.Errorf("validate build receipt: %w", ErrReceiptBudgetMismatch)
	}
	assetRef = ResolveAssetScopeRef(run, assetRef)
	if !run.AssetScope.Contains(assetRef) {
		return fmt.Errorf("validate build receipt: %w", ErrReceiptAssetOutOfScope)
	}
	return nil
}

func sameBlueprintRevisionToken(left, right *BlueprintRevisionToken) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}
