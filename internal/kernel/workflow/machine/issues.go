package machine

import "sort"

const (
	PhaseDTO                 = 1
	PhaseIdentityReferences  = 2
	PhaseTopology            = 3
	PhaseRoutes              = 4
	PhaseNaturalLoops        = 5
	PhaseParallelJoin        = 6
	PhaseConditions          = 7
	PhaseValuesAndSchemas    = 8
	PhaseTriggerCapabilities = 9
	PhaseAuthorization       = 10
	PhaseAgentVersions       = 11
	PhaseDependencies        = 12
	PhaseFactories           = 13
	PhaseCanonicalization    = 14
	PhaseAtomicPublish       = 15
)

const (
	CodeJSONInvalid                       = "workflow_json_invalid"
	CodeMultipleJSONValues                = "workflow_json_multiple_values"
	CodeDuplicateField                    = "workflow_field_duplicate"
	CodeUnknownField                      = "workflow_field_unknown"
	CodeFieldRequired                     = "workflow_field_required"
	CodeTypeInvalid                       = "workflow_type_invalid"
	CodeSchemaVersionRequired             = "workflow_schema_version_required"
	CodeSchemaVersionUnsupported          = "workflow_schema_version_unsupported"
	CodeDiscriminatorRequired             = "workflow_discriminator_required"
	CodeEnumInvalid                       = "workflow_enum_invalid"
	CodeDeliveryRequired                  = "workflow_delivery_required"
	CodeDeliveryForbidden                 = "workflow_delivery_forbidden"
	CodeUnsafeInteger                     = "workflow_integer_unsafe"
	CodeUnicodeInvalid                    = "workflow_unicode_invalid"
	CodeNestingLimit                      = "workflow_json_nesting_limit"
	CodeGraphEmpty                        = "workflow_graph_empty"
	CodeGraphTooLarge                     = "workflow_graph_too_large"
	CodeComplexityBudgetExceeded          = "workflow_complexity_budget_exceeded"
	CodeIntegerInvalid                    = "workflow_integer_invalid"
	CodeOutputRequired                    = "workflow_output_required"
	CodeOutputForbidden                   = "workflow_output_forbidden"
	CodeContractInvalid                   = "workflow_contract_invalid"
	CodeNodeIDDuplicate                   = "workflow_node_id_duplicate"
	CodeEdgeIDDuplicate                   = "workflow_edge_id_duplicate"
	CodeEntryNotFound                     = "workflow_entry_not_found"
	CodeEdgeSourceNotFound                = "workflow_edge_source_not_found"
	CodeEdgeTargetNotFound                = "workflow_edge_target_not_found"
	CodeNodeReferenceNotFound             = "workflow_node_reference_not_found"
	CodeNodeUnreachable                   = "workflow_node_unreachable"
	CodeEdgeCardinalityInvalid            = "workflow_edge_cardinality_invalid"
	CodeSuccessPathUnterminated           = "workflow_success_path_unterminated"
	CodeSuccessTerminalMissing            = "workflow_success_terminal_missing"
	CodeRouteNotAllowed                   = "workflow_route_not_allowed"
	CodeEdgePriorityForbidden             = "workflow_edge_priority_forbidden"
	CodeEdgePredicateForbidden            = "workflow_edge_predicate_forbidden"
	CodeTimeoutRouteWithoutTimeout        = "workflow_timeout_route_without_timeout"
	CodeBackRouteInvalid                  = "workflow_back_route_invalid"
	CodeCycleInvalid                      = "workflow_cycle_invalid"
	CodeLoopBackMissing                   = "workflow_loop_back_missing"
	CodeLoopBackDuplicate                 = "workflow_loop_back_duplicate"
	CodeLoopBackInvalid                   = "workflow_loop_back_invalid"
	CodeLoopLatchInvalid                  = "workflow_loop_latch_invalid"
	CodeLoopBodyInvalid                   = "workflow_loop_body_invalid"
	CodeLoopSideEntry                     = "workflow_loop_side_entry"
	CodeLoopSideExit                      = "workflow_loop_side_exit"
	CodeLoopNested                        = "workflow_loop_nested"
	CodeLoopOverlapping                   = "workflow_loop_overlapping"
	CodeParallelBranchCountInvalid        = "workflow_parallel_branch_count_invalid"
	CodeParallelBranchNotDispatchWorker   = "workflow_parallel_branch_not_dispatch_worker"
	CodeParallelBranchNotExclusive        = "workflow_parallel_branch_not_exclusive"
	CodeParallelNested                    = "workflow_parallel_nested"
	CodeParallelJoinInvalid               = "workflow_parallel_join_invalid"
	CodeParallelJoinShared                = "workflow_parallel_join_shared"
	CodeJoinInputsMismatch                = "workflow_join_inputs_mismatch"
	CodeJoinPolicyInvalid                 = "workflow_join_policy_invalid"
	CodeJoinQuorumInvalid                 = "workflow_join_quorum_invalid"
	CodeJoinDeadlineInvalid               = "workflow_join_deadline_invalid"
	CodeConditionCaseMissing              = "workflow_condition_case_missing"
	CodeConditionDefaultMissing           = "workflow_condition_default_missing"
	CodeConditionDefaultDuplicate         = "workflow_condition_default_duplicate"
	CodeConditionPriorityRequired         = "workflow_condition_priority_required"
	CodeConditionPriorityDuplicate        = "workflow_condition_priority_duplicate"
	CodePredicateRequired                 = "workflow_predicate_required"
	CodePredicateRightRequired            = "workflow_predicate_right_required"
	CodePredicateRightForbidden           = "workflow_predicate_right_forbidden"
	CodePredicateOperatorInvalid          = "workflow_predicate_operator_invalid"
	CodeSchemaKeywordUnsupported          = "workflow_schema_keyword_unsupported"
	CodeSchemaKeywordInvalid              = "workflow_schema_keyword_invalid"
	CodeSchemaTypeUnsupported             = "workflow_schema_type_unsupported"
	CodeSchemaRequiredDuplicate           = "workflow_schema_required_duplicate"
	CodeSchemaEnumDuplicate               = "workflow_schema_enum_duplicate"
	CodeOutputContractIncompatible        = "workflow_output_contract_incompatible"
	CodeJSONPointerInvalid                = "workflow_json_pointer_invalid"
	CodeJSONPointerUnprovable             = "workflow_json_pointer_unprovable"
	CodeValueSourceUnavailable            = "workflow_value_source_unavailable"
	CodeValueNotDominating                = "workflow_value_not_dominating"
	CodeValueLoopScopeInvalid             = "workflow_value_loop_scope_invalid"
	CodeValueIterationRequired            = "workflow_value_iteration_required"
	CodeValueIterationForbidden           = "workflow_value_iteration_forbidden"
	CodeValuePreviousDefaultRequired      = "workflow_value_previous_default_required"
	CodeValueDefaultForbidden             = "workflow_value_default_forbidden"
	CodeValueTypeIncompatible             = "workflow_value_type_incompatible"
	CodeSessionRequired                   = "workflow_session_required"
	CodeCapabilityProofMissing            = "workflow_capability_proof_missing"
	CodeCapabilityUnprovable              = "workflow_capability_unprovable"
	CodeInteractiveCapabilityForbidden    = "workflow_interactive_capability_forbidden"
	CodeCrossAgentCapabilityForbidden     = "workflow_cross_agent_capability_forbidden"
	CodeNodeUnauthorized                  = "workflow_node_unauthorized"
	CodeAuthorizationProofMissing         = "workflow_authorization_proof_missing"
	CodeAuthorizationWorkspaceMismatch    = "workflow_authorization_workspace_mismatch"
	CodeAuthorizationCorrupt              = "workflow_authorization_corrupt"
	CodeAuthorizationUnprovable           = "workflow_authorization_unprovable"
	CodeReferenceProofMissing             = "workflow_reference_proof_missing"
	CodeReferenceNotFound                 = "workflow_reference_not_found"
	CodeReferenceCorrupt                  = "workflow_reference_corrupt"
	CodeReferenceUnprovable               = "workflow_reference_unprovable"
	CodeTeamInactive                      = "workflow_team_inactive"
	CodeAgentVersionProofMissing          = "workflow_agent_version_proof_missing"
	CodeAgentVersionNotFound              = "workflow_agent_version_not_found"
	CodeAgentVersionCorrupt               = "workflow_agent_version_corrupt"
	CodeAgentVersionUnprovable            = "workflow_agent_version_unprovable"
	CodeAgentSubAgentsForbidden           = "workflow_agent_sub_agents_forbidden"
	CodeAgentWorkerStepForbidden          = "workflow_agent_worker_step_forbidden"
	CodeAgentOutputSchemaInvalid          = "workflow_agent_output_schema_invalid"
	CodeHandoffOutputContractIncompatible = "workflow_handoff_output_contract_incompatible"
	CodeDependencyProofMissing            = "workflow_dependency_proof_missing"
	CodeDependencyUnprovable              = "workflow_dependency_unprovable"
	CodeDependencyUnenumerable            = "workflow_dependency_unenumerable"
	CodeSkillVersionRequired              = "workflow_skill_version_required"
	CodeProviderRevisionRequired          = "workflow_provider_revision_required"
	CodeCredentialUnavailable             = "workflow_credential_unavailable"
	CodeCredentialVersionUnsupported      = "workflow_credential_version_unsupported"
	CodeDependencyVersionRequired         = "workflow_dependency_version_required"
	CodeDependencyNotFound                = "workflow_dependency_not_found"
	CodeDependencyCorrupt                 = "workflow_dependency_corrupt"
	CodeFrozenDependencyIncomplete        = "workflow_frozen_dependency_incomplete"
	CodeFactoryProofMissing               = "workflow_factory_proof_missing"
	CodeFactoryUnknown                    = "workflow_factory_unknown"
	CodeFactoryABIIncompatible            = "workflow_factory_abi_incompatible"
	CodeFactoryUnprovable                 = "workflow_factory_unprovable"
	CodeFactoryInputInvalid               = "workflow_factory_input_invalid"
	CodeFrozenDependencyUndeclared        = "workflow_frozen_dependency_undeclared"
	CodeFrozenManifestMismatch            = "workflow_frozen_manifest_mismatch"
	CodeFrozenCapabilityMismatch          = "workflow_frozen_capability_mismatch"
)

type ValidationIssue struct {
	Phase      int    `json:"phase"`
	Path       string `json:"path"`
	NodeID     string `json:"node_id,omitempty"`
	Code       string `json:"code"`
	Message    string `json:"message"`
	Occurrence int    `json:"occurrence"`
}

type Report struct {
	Issues      []ValidationIssue `json:"issues"`
	occurrences *occurrenceIndex
}

type occurrenceKey struct {
	phase int
	path  string
	code  string
}

type occurrenceIndex struct {
	owner      *Report
	issueCount int
	counts     map[occurrenceKey]int
}

func (r *Report) Add(phase int, path, code, message string) {
	r.AddNode(phase, path, "", code, message)
}

func (r *Report) AddNode(phase int, path, nodeID, code, message string) {
	if r == nil {
		return
	}
	r.detachIssuesForAppend()
	index := r.ensureOccurrenceIndex()
	key := occurrenceKey{phase: phase, path: path, code: code}
	occurrence := index.counts[key]
	r.Issues = append(r.Issues, ValidationIssue{
		Phase:      phase,
		Path:       path,
		NodeID:     nodeID,
		Code:       code,
		Message:    message,
		Occurrence: occurrence,
	})
	index.counts[key] = occurrence + 1
	index.issueCount++
}

func (r *Report) detachIssuesForAppend() {
	indexCannotProveOwnership := r.occurrences == nil && cap(r.Issues) > 0
	indexBelongsToValueCopy := r.occurrences != nil && r.occurrences.owner != r
	indexWasInvalidated := r.occurrences != nil && r.occurrences.issueCount != len(r.Issues)
	if !indexCannotProveOwnership && !indexBelongsToValueCopy && !indexWasInvalidated {
		return
	}
	r.Issues = append([]ValidationIssue(nil), r.Issues...)
	r.occurrences = nil
}

func (r *Report) ensureOccurrenceIndex() *occurrenceIndex {
	if r.occurrences != nil &&
		r.occurrences.owner == r &&
		r.occurrences.issueCount == len(r.Issues) {
		return r.occurrences
	}

	counts := make(map[occurrenceKey]int)
	for _, issue := range r.Issues {
		key := occurrenceKey{phase: issue.Phase, path: issue.Path, code: issue.Code}
		counts[key]++
	}
	r.occurrences = &occurrenceIndex{
		owner:      r,
		issueCount: len(r.Issues),
		counts:     counts,
	}
	return r.occurrences
}

func (r *Report) Sort() {
	if r == nil {
		return
	}
	r.Issues = append([]ValidationIssue(nil), r.Issues...)
	r.occurrences = nil
	r.ensureOccurrenceIndex()
	sort.SliceStable(r.Issues, func(i, j int) bool {
		left, right := r.Issues[i], r.Issues[j]
		if left.Phase != right.Phase {
			return left.Phase < right.Phase
		}
		if left.Path != right.Path {
			return left.Path < right.Path
		}
		if left.Code != right.Code {
			return left.Code < right.Code
		}
		return left.Occurrence < right.Occurrence
	})
}
