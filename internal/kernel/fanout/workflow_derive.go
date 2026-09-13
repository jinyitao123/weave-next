package fanout

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jinyitao123/weave/internal/base/frozen"
)

func DeriveGeneration(workspaceID, parentRunID, nodeID string, previousCheckpointSequence, nodeEntryOrdinal int64) (string, error) {
	if invalidHashField(workspaceID) || invalidHashField(parentRunID) || invalidHashField(nodeID) {
		return "", workflowError(ErrorInvalidRequest, "generation identity must be non-empty")
	}
	if err := validateZeroBasedJCS(previousCheckpointSequence); err != nil {
		return "", err
	}
	if err := validateZeroBasedJCS(nodeEntryOrdinal); err != nil {
		return "", err
	}
	return prefixedDigest("fg1_", "weave/fanout-generation/v1", workspaceID, parentRunID, nodeID,
		strconv.FormatInt(previousCheckpointSequence, 10), strconv.FormatInt(nodeEntryOrdinal, 10)), nil
}

func DeriveGroupCompletionID(workspaceID, parentRunID, nodeID, generation string) (string, error) {
	if invalidHashField(workspaceID) || invalidHashField(parentRunID) || invalidHashField(nodeID) || invalidHashField(generation) {
		return "", workflowError(ErrorInvalidRequest, "completion identity must be non-empty")
	}
	return prefixedDigest("fgc1_", "weave/fanout-completion/v1", workspaceID, parentRunID, nodeID, generation), nil
}

func DeriveLegTaskID(groupID, branchID, generation string) (string, error) {
	if invalidHashField(groupID) || invalidHashField(branchID) || invalidHashField(generation) {
		return "", workflowError(ErrorInvalidRequest, "leg task identity must be non-empty")
	}
	return prefixedDigest("flt1_", "weave/fanout-leg-task/v1", groupID, branchID, generation), nil
}

func DeriveResumeTaskID(workspaceID, parentRunID, claimID string) (string, error) {
	if invalidHashField(workspaceID) || invalidHashField(parentRunID) || invalidHashField(claimID) {
		return "", workflowError(ErrorInvalidRequest, "resume task identity must be non-empty")
	}
	return prefixedDigest("frt1_", "weave/fanout-resume-task/v1", workspaceID, parentRunID, claimID), nil
}

func prefixedDigest(prefix, domain string, fields ...string) string {
	hash := sha256.New()
	_, _ = hash.Write([]byte(domain))
	for _, field := range fields {
		_, _ = hash.Write([]byte{0})
		_, _ = hash.Write([]byte(field))
	}
	return prefix + hex.EncodeToString(hash.Sum(nil))
}

type canonicalPlan struct {
	WorkflowID                 string       `json:"workflow_id"`
	WorkflowVersion            int64        `json:"workflow_version"`
	RunSnapshotID              string       `json:"run_snapshot_id"`
	NodeID                     string       `json:"node_id"`
	PreviousCheckpointSequence int64        `json:"previous_checkpoint_sequence"`
	NodeEntryOrdinal           int64        `json:"node_entry_ordinal"`
	CreatorAttemptGeneration   int64        `json:"creator_attempt_generation"`
	CreatorAttemptID           string       `json:"creator_attempt_id"`
	ActivationDeadlineAt       string       `json:"activation_deadline_at"`
	JoinPolicy                 JoinPolicy   `json:"join_policy"`
	Legs                       []PlannedLeg `json:"legs"`
}

func DerivePlanHash(req PrepareParkRequest) (string, error) {
	if err := validatePrepareRequest(req); err != nil {
		return "", err
	}
	legs := append([]PlannedLeg(nil), req.Legs...)
	sort.Slice(legs, func(i, j int) bool { return legs[i].BranchOrdinal < legs[j].BranchOrdinal })
	plan := canonicalPlan{
		WorkflowID: req.WorkflowID, WorkflowVersion: req.WorkflowVersion,
		RunSnapshotID: req.RunSnapshotID, NodeID: req.NodeID,
		PreviousCheckpointSequence: req.PreviousCheckpointSequence,
		NodeEntryOrdinal:           req.NodeEntryOrdinal,
		CreatorAttemptGeneration:   req.CreatorAttemptGeneration,
		CreatorAttemptID:           req.CreatorAttemptID,
		ActivationDeadlineAt:       req.ActivationDeadline.UTC().Format(time.RFC3339Nano),
		JoinPolicy:                 normalizePolicy(req.JoinPolicy), Legs: legs,
	}
	canonical, err := frozen.CanonicalizePreordered(plan)
	if err != nil {
		return "", workflowError(ErrorInvalidRequest, "canonicalize plan: %v", err)
	}
	digest := sha256.Sum256(canonical)
	return "fip1_" + hex.EncodeToString(digest[:]), nil
}

func validatePrepareRequest(req PrepareParkRequest) error {
	if invalidHashField(req.WorkspaceID) || invalidHashField(req.ParentRunID) || invalidHashField(req.WorkflowID) ||
		invalidHashField(req.RunSnapshotID) || invalidHashField(req.NodeID) || invalidHashField(req.CreatorAttemptID) || req.ResumeToken == "" {
		return workflowError(ErrorInvalidRequest, "prepare identity must be non-empty")
	}
	if req.ActivationDeadline.IsZero() {
		return workflowError(ErrorInvalidRequest, "activation deadline must be non-zero")
	}
	if _, err := uuid.Parse(req.CreatorAttemptID); err != nil {
		return workflowError(ErrorInvalidRequest, "creator attempt ID must be a UUID")
	}
	if err := validatePositiveJCS(req.WorkflowVersion); err != nil {
		return err
	}
	if err := validatePositiveJCS(req.CreatorAttemptGeneration); err != nil {
		return err
	}
	if err := validateZeroBasedJCS(req.PreviousCheckpointSequence); err != nil {
		return err
	}
	if err := validateZeroBasedJCS(req.NodeEntryOrdinal); err != nil {
		return err
	}
	if len(req.Legs) == 0 {
		return workflowError(ErrorInvalidRequest, "at least one leg is required")
	}
	if err := validatePolicy(req.JoinPolicy, len(req.Legs)); err != nil {
		return err
	}
	ordinals := make(map[int]struct{}, len(req.Legs))
	branches := make(map[string]struct{}, len(req.Legs))
	legs := make(map[string]struct{}, len(req.Legs))
	for _, leg := range req.Legs {
		if leg.LegID == "" || leg.BranchID == "" || leg.BranchOrdinal < 0 || leg.BranchOrdinal >= len(req.Legs) {
			return workflowError(ErrorInvalidRequest, "invalid leg identity or branch ordinal")
		}
		if err := validateZeroBasedJCS(int64(leg.BranchOrdinal)); err != nil {
			return err
		}
		if _, exists := ordinals[leg.BranchOrdinal]; exists {
			return workflowError(ErrorInvalidRequest, "duplicate branch ordinal %d", leg.BranchOrdinal)
		}
		if _, exists := branches[leg.BranchID]; exists {
			return workflowError(ErrorInvalidRequest, "duplicate branch %q", leg.BranchID)
		}
		if _, exists := legs[leg.LegID]; exists {
			return workflowError(ErrorInvalidRequest, "duplicate leg %q", leg.LegID)
		}
		ordinals[leg.BranchOrdinal], branches[leg.BranchID], legs[leg.LegID] = struct{}{}, struct{}{}, struct{}{}
		for name, raw := range map[string]json.RawMessage{
			"frozen_bundle_ref": leg.FrozenBundleRef, "input_ref": leg.InputRef, "may_yield_proof": leg.MayYieldProof,
		} {
			if !isJSONObject(raw) {
				return workflowError(ErrorInvalidRequest, "%s must be a JSON object", name)
			}
		}
		var proof struct {
			MayYield *bool `json:"may_yield"`
		}
		if err := json.Unmarshal(leg.MayYieldProof, &proof); err != nil || proof.MayYield == nil || *proof.MayYield {
			return workflowError(ErrorBranchShapeUnsupported, "leg %q lacks may_yield=false proof", leg.LegID)
		}
	}
	return nil
}

func invalidHashField(value string) bool {
	return value == "" || strings.IndexByte(value, 0) >= 0
}

func isJSONObject(raw json.RawMessage) bool {
	var value map[string]json.RawMessage
	return len(raw) > 0 && json.Unmarshal(raw, &value) == nil && value != nil
}

func validatePolicy(policy JoinPolicy, legCount int) error {
	if policy.MaxDeadlineSeconds > frozen.MaxJCSSafeInteger {
		return workflowError(ErrorInvalidRequest, "max deadline exceeds JCS safe integer")
	}
	switch policy.Kind {
	case JoinAllSuccess, JoinFailFast:
		if policy.Quorum != 0 || policy.MaxDeadlineSeconds <= 0 || policy.DeadlineAt.IsZero() {
			return workflowError(ErrorInvalidRequest, "%s requires max deadline and no quorum", policy.Kind)
		}
	case JoinQuorum:
		if policy.Quorum < 1 || policy.Quorum > legCount || policy.MaxDeadlineSeconds <= 0 || policy.DeadlineAt.IsZero() {
			return workflowError(ErrorInvalidRequest, "invalid quorum policy")
		}
	case JoinDeadline:
		if policy.Quorum != 0 || policy.DeadlineAt.IsZero() {
			return workflowError(ErrorInvalidRequest, "deadline policy requires deadline_at and no quorum")
		}
	default:
		return workflowError(ErrorInvalidRequest, "unknown join policy %q", policy.Kind)
	}
	return nil
}

func normalizePolicy(policy JoinPolicy) JoinPolicy {
	policy.DeadlineAt = policy.DeadlineAt.UTC()
	return policy
}

func validateZeroBasedJCS(value int64) error {
	if err := frozen.ValidateJCSSafeIntegerRange(value, 0, frozen.MaxJCSSafeInteger); err != nil {
		return workflowError(ErrorInvalidRequest, "%v", err)
	}
	return nil
}

func validatePositiveJCS(value int64) error {
	if err := frozen.ValidateJCSSafeIntegerRange(value, 1, frozen.MaxJCSSafeInteger); err != nil {
		return workflowError(ErrorInvalidRequest, "%v", err)
	}
	return nil
}

func canonicalJSON(value any) (json.RawMessage, error) {
	raw, err := frozen.CanonicalizePreordered(value)
	if err != nil {
		return nil, fmt.Errorf("canonicalize JSON: %w", err)
	}
	return json.RawMessage(raw), nil
}
