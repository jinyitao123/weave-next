package teambuild

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"
)

const (
	BlueprintSchemaVersionV1 = 1

	BlueprintMemberRoleAvatar           = "avatar"
	BlueprintMemberRoleWorker           = "worker"
	BlueprintManagementManaged          = "managed"
	BlueprintManagementPreserveExisting = "preserve_existing"

	BlueprintEngineStandard = "standard"
	BlueprintEngineCLI      = "cli"

	BlueprintExecutionToolLoop      = "toolloop"
	BlueprintExecutionRuntime       = "runtime"
	BlueprintExecutionInternalGraph = "internal_graph"

	BlueprintWorkflowTemplate      = "template"
	BlueprintWorkflowCustom        = "custom"
	BlueprintWorkflowDeclarativeV1 = "declarative_v1"

	BlueprintTemplateDeliveryRework  = "delivery_rework_loop"
	BlueprintTemplateParallelReview  = "parallel_review"
	BlueprintTemplateCreativeRework  = "creative_critique_loop"
	BlueprintTemplateResearchSummary = "research_synthesis"

	BlueprintPatchFailureBusinessQuality = "business_quality_failure"
)

var (
	blueprintStableRefPattern = regexp.MustCompile(`^[a-z][a-z0-9_-]*$`)
	blueprintChinesePattern   = regexp.MustCompile(`\p{Han}`)
	canonicalSHA256Pattern    = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// TeamBlueprintV1 is the small, frozen desired-state document produced by
// the planning meta-team. It describes intent and compatibility policy; it
// does not contain persistence operations or low-level graph nodes.
type TeamBlueprintV1 struct {
	SchemaVersion   int                       `json:"schema_version"`
	Mode            string                    `json:"mode"`
	TeamID          string                    `json:"team_id,omitempty"`
	NewTeamName     string                    `json:"new_team_name,omitempty"`
	TeamDisplayName string                    `json:"team_display_name,omitempty"`
	Purpose         string                    `json:"purpose"`
	Members         []BlueprintMemberV1       `json:"members"`
	LeadRef         string                    `json:"lead_ref"`
	Workflow        BlueprintWorkflowV1       `json:"workflow"`
	RevisionPolicy  BlueprintRevisionPolicyV1 `json:"revision_policy"`
}

// BlueprintMemberV1 describes one stable logical team member. StableRef is
// used in patch paths so list reordering cannot change a revision target.
type BlueprintMemberV1 struct {
	StableRef        string                     `json:"stable_ref"`
	Name             string                     `json:"name"`
	DisplayName      string                     `json:"display_name"`
	Role             string                     `json:"role"`
	ManagementMode   string                     `json:"management_mode"`
	Responsibilities []string                   `json:"responsibilities"`
	Capabilities     []string                   `json:"capabilities"`
	ModelRef         string                     `json:"model_ref"`
	ExecutionPolicy  BlueprintExecutionPolicyV1 `json:"execution_policy"`
}

// BlueprintExecutionPolicyV1 is a closed, declarative compatibility choice.
// It proves only the execution shape needed before compilation; platform
// capability and dependency checks remain deterministic downstream gates.
type BlueprintExecutionPolicyV1 struct {
	EngineClass      string `json:"engine_class"`
	Engine           string `json:"engine,omitempty"`
	ExecutionMode    string `json:"execution_mode"`
	RuntimeRef       string `json:"runtime_ref,omitempty"`
	InternalGraphRef string `json:"internal_graph_ref,omitempty"`
}

// BlueprintWorkflowV1 selects either one platform template or one frozen
// custom specification. Custom mode is impossible without an independently
// typed administrator authorization fact bound to a scope hash.
type BlueprintWorkflowV1 struct {
	Mode                     string                                 `json:"mode"`
	Template                 string                                 `json:"template,omitempty"`
	TemplateParameters       *BlueprintWorkflowTemplateParametersV1 `json:"template_parameters,omitempty"`
	CustomSpecRef            string                                 `json:"custom_spec_ref,omitempty"`
	DeclarativeSpecHash      string                                 `json:"declarative_spec_hash,omitempty"`
	TemplateGapAuthorization *TemplateGapAuthorizationV1            `json:"template_gap_authorization,omitempty"`
}

// BlueprintWorkflowTemplateParametersV1 is the complete parameter surface
// accepted by the built-in templates. Arbitrary node, edge, route, and latch
// JSON deliberately cannot enter this contract.
type BlueprintWorkflowTemplateParametersV1 struct {
	LeadInstruction    string            `json:"lead_instruction"`
	PrimaryRef         string            `json:"primary_ref,omitempty"`
	ReviewerRef        string            `json:"reviewer_ref,omitempty"`
	ParallelWorkerRefs []string          `json:"parallel_worker_refs,omitempty"`
	FinalizerRef       string            `json:"finalizer_ref,omitempty"`
	MaxIterations      *int              `json:"max_iterations,omitempty"`
	ResultRequirements map[string]string `json:"result_requirements"`
}

// TemplateGapAuthorizationV1 is a platform-supplied authorization fact. Its
// presence, confirmation, authority, and scope binding are all required;
// neither a brief hash nor free-form model output can substitute for it.
type TemplateGapAuthorizationV1 struct {
	Confirmed    bool   `json:"confirmed"`
	AuthorityRef string `json:"authority_ref"`
	ScopeHash    string `json:"scope_hash"`
}

// BlueprintRevisionPolicyV1 bounds business-quality revision. Allowed paths
// are exact stable paths, not broad prefixes or positional list indexes.
type BlueprintRevisionPolicyV1 struct {
	MaxRevisions      int      `json:"max_revisions"`
	AllowedPatchPaths []string `json:"allowed_patch_paths"`
}

// BlueprintPatchV1 is a report-bound request to revise only authorized
// business-quality fields. It is a document contract, never an instruction
// to write storage directly.
type BlueprintPatchV1 struct {
	SchemaVersion    int                     `json:"schema_version"`
	SourceReportHash string                  `json:"source_report_hash"`
	FailureClass     string                  `json:"failure_class"`
	TargetPaths      []string                `json:"target_paths"`
	Changes          []BlueprintFieldPatchV1 `json:"changes"`
}

// BlueprintFieldPatchV1 is one compare-and-replace field proposal. The
// expected hash prevents a diagnosis from silently patching stale state.
type BlueprintFieldPatchV1 struct {
	Path              string          `json:"path"`
	ExpectedValueHash string          `json:"expected_value_hash"`
	ProposedValue     json.RawMessage `json:"proposed_value"`
	EvidenceRefs      []string        `json:"evidence_refs"`
	Reason            string          `json:"reason"`
}

// BlueprintProblem is one field-oriented contract violation.
type BlueprintProblem struct {
	Path    string `json:"path"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

// BlueprintValidationError exposes every detected contract problem in one
// errors.As-compatible value, allowing a planner to repair all fields once.
type BlueprintValidationError struct {
	Document string             `json:"document"`
	Problems []BlueprintProblem `json:"problems"`
}

func (e *BlueprintValidationError) Error() string {
	if e == nil {
		return "blueprint validation failed"
	}
	if len(e.Problems) == 0 {
		return e.Document + " validation failed"
	}
	return fmt.Sprintf("%s validation failed with %d problem(s): %s: %s",
		e.Document, len(e.Problems), e.Problems[0].Path, e.Problems[0].Message)
}

type blueprintProblemCollector struct {
	document string
	items    []BlueprintProblem
}

func (c *blueprintProblemCollector) add(path, code, message string) {
	c.items = append(c.items, BlueprintProblem{Path: path, Code: code, Message: message})
}

func (c *blueprintProblemCollector) err() error {
	if len(c.items) == 0 {
		return nil
	}
	problems := append([]BlueprintProblem(nil), c.items...)
	sort.SliceStable(problems, func(i, j int) bool {
		if problems[i].Path != problems[j].Path {
			return problems[i].Path < problems[j].Path
		}
		if problems[i].Code != problems[j].Code {
			return problems[i].Code < problems[j].Code
		}
		return problems[i].Message < problems[j].Message
	})
	return &BlueprintValidationError{Document: c.document, Problems: problems}
}

// ValidateTeamBlueprintV1 reports the complete blueprint problem list.
func ValidateTeamBlueprintV1(blueprint TeamBlueprintV1) error {
	c := &blueprintProblemCollector{document: "team_blueprint_v1"}
	if blueprint.SchemaVersion != BlueprintSchemaVersionV1 {
		c.add("/schema_version", "blueprint_schema_version_invalid", "schema_version must be 1")
	}
	validateBlueprintIdentity(c, blueprint)
	if strings.TrimSpace(blueprint.Purpose) == "" {
		c.add("/purpose", "blueprint_purpose_required", "purpose is required")
	}

	members := validateBlueprintMembers(c, blueprint.Mode, blueprint.Members)
	leadRef := strings.TrimSpace(blueprint.LeadRef)
	lead, leadExists := members[leadRef]
	if leadRef == "" {
		c.add("/lead_ref", "blueprint_lead_ref_required", "lead_ref is required")
	} else if !leadExists {
		c.add("/lead_ref", "blueprint_lead_ref_unknown", "lead_ref must identify a member stable_ref")
	} else if lead.Role != BlueprintMemberRoleAvatar {
		c.add("/lead_ref", "blueprint_lead_role_invalid", "lead_ref must identify an avatar member")
	}

	validateBlueprintWorkflow(c, blueprint.Workflow, members)
	validateBlueprintRevisionPolicy(c, blueprint.RevisionPolicy, members)
	return c.err()
}

func validateBlueprintIdentity(c *blueprintProblemCollector, blueprint TeamBlueprintV1) {
	teamID := strings.TrimSpace(blueprint.TeamID)
	newName := strings.TrimSpace(blueprint.NewTeamName)
	switch blueprint.Mode {
	case ModeCreate:
		if newName == "" {
			c.add("/new_team_name", "blueprint_new_team_name_required", "create mode requires new_team_name")
		}
		if teamID != "" {
			c.add("/team_id", "blueprint_team_id_forbidden", "create mode forbids team_id")
		}
	case ModeOptimize:
		if teamID == "" {
			c.add("/team_id", "blueprint_team_id_required", "optimize mode requires team_id")
		}
		if newName != "" {
			c.add("/new_team_name", "blueprint_new_team_name_forbidden", "optimize mode forbids new_team_name")
		}
		if strings.TrimSpace(blueprint.TeamDisplayName) != "" {
			c.add("/team_display_name", "blueprint_team_display_name_forbidden", "optimize mode forbids team_display_name")
		}
	default:
		c.add("/mode", "blueprint_mode_invalid", "mode must be create or optimize")
	}
}

func validateBlueprintMembers(c *blueprintProblemCollector, mode string, values []BlueprintMemberV1) map[string]BlueprintMemberV1 {
	members := make(map[string]BlueprintMemberV1, len(values))
	if len(values) == 0 {
		c.add("/members", "blueprint_members_required", "at least one member is required")
		return members
	}
	for index, member := range values {
		base := fmt.Sprintf("/members/%d", index)
		stableRef := strings.TrimSpace(member.StableRef)
		if !blueprintStableRefPattern.MatchString(stableRef) {
			c.add(base+"/stable_ref", "blueprint_member_stable_ref_invalid", "stable_ref must be a lowercase stable path segment")
		} else if _, exists := members[stableRef]; exists {
			c.add(base+"/stable_ref", "blueprint_member_stable_ref_duplicate", "stable_ref must be unique")
		} else {
			members[stableRef] = member
		}
		if strings.TrimSpace(member.Name) == "" {
			c.add(base+"/name", "blueprint_member_name_required", "name is required")
		}
		displayName := strings.TrimSpace(member.DisplayName)
		if displayName == "" {
			c.add(base+"/display_name", "blueprint_member_display_name_required", "display_name is required")
		} else if mode == ModeCreate && !blueprintChinesePattern.MatchString(displayName) {
			c.add(base+"/display_name", "blueprint_member_display_name_chinese_required", "create mode requires a Chinese display_name")
		}
		switch member.Role {
		case BlueprintMemberRoleAvatar, BlueprintMemberRoleWorker:
		default:
			c.add(base+"/role", "blueprint_member_role_invalid", "role must be avatar or worker")
		}
		switch member.ManagementMode {
		case BlueprintManagementManaged:
		case BlueprintManagementPreserveExisting:
			if mode == ModeCreate {
				c.add(base+"/management_mode", "blueprint_member_preserve_existing_forbidden", "create mode requires managed members")
			}
		default:
			c.add(base+"/management_mode", "blueprint_member_management_mode_invalid", "management_mode must be managed or preserve_existing")
		}
		validateNormalizedStringList(c, base+"/responsibilities", member.Responsibilities)
		validateNormalizedStringList(c, base+"/capabilities", member.Capabilities)
		validateBlueprintExecutionPolicy(c, base+"/execution_policy", member.ExecutionPolicy)
	}
	return members
}

func validateNormalizedStringList(c *blueprintProblemCollector, path string, values []string) {
	if len(values) == 0 {
		c.add(path, "blueprint_list_required", "at least one non-blank value is required")
		return
	}
	seen := make(map[string]bool, len(values))
	for index, value := range values {
		normalized := normalizeComparableText(value)
		itemPath := fmt.Sprintf("%s/%d", path, index)
		if normalized == "" {
			c.add(itemPath, "blueprint_list_value_blank", "value must not be blank")
		} else if seen[normalized] {
			c.add(itemPath, "blueprint_list_value_duplicate", "value duplicates another normalized entry")
		} else {
			seen[normalized] = true
		}
	}
}

func validateBlueprintExecutionPolicy(c *blueprintProblemCollector, path string, policy BlueprintExecutionPolicyV1) {
	valid := false
	switch {
	case policy.EngineClass == BlueprintEngineStandard && policy.ExecutionMode == BlueprintExecutionToolLoop:
		valid = strings.TrimSpace(policy.Engine) == "" && strings.TrimSpace(policy.RuntimeRef) == "" && strings.TrimSpace(policy.InternalGraphRef) == ""
	case policy.EngineClass == BlueprintEngineCLI && policy.ExecutionMode == BlueprintExecutionRuntime:
		valid = isBlueprintCLIEngine(policy.Engine) && strings.TrimSpace(policy.RuntimeRef) != "" && strings.TrimSpace(policy.InternalGraphRef) == ""
	case policy.EngineClass == BlueprintEngineStandard && policy.ExecutionMode == BlueprintExecutionInternalGraph:
		valid = strings.TrimSpace(policy.Engine) == "" && strings.TrimSpace(policy.RuntimeRef) == "" && strings.TrimSpace(policy.InternalGraphRef) != ""
	}
	if !valid {
		c.add(path, "blueprint_execution_policy_incompatible",
			"execution policy must be standard with blank engine, cli/runtime with engine codex|claude|opencode and runtime_ref, or standard/internal_graph with blank engine and internal_graph_ref")
	}
}

func isBlueprintCLIEngine(engine string) bool {
	switch strings.TrimSpace(engine) {
	case "codex", "claude", "opencode":
		return true
	default:
		return false
	}
}

func validateBlueprintWorkflow(c *blueprintProblemCollector, workflow BlueprintWorkflowV1, members map[string]BlueprintMemberV1) {
	switch workflow.Mode {
	case BlueprintWorkflowTemplate:
		if strings.TrimSpace(workflow.CustomSpecRef) != "" {
			c.add("/workflow/custom_spec_ref", "blueprint_workflow_custom_spec_forbidden", "template mode forbids custom_spec_ref")
		}
		if strings.TrimSpace(workflow.DeclarativeSpecHash) != "" {
			c.add("/workflow/declarative_spec_hash", "blueprint_workflow_declarative_spec_forbidden", "template mode forbids declarative_spec_hash")
		}
		if workflow.TemplateGapAuthorization != nil {
			c.add("/workflow/template_gap_authorization", "blueprint_workflow_gap_authorization_forbidden", "template mode forbids template-gap authorization")
		}
		validateBlueprintTemplate(c, workflow.Template, workflow.TemplateParameters, members)
	case BlueprintWorkflowCustom:
		if strings.TrimSpace(workflow.Template) != "" {
			c.add("/workflow/template", "blueprint_workflow_template_forbidden", "custom mode forbids template")
		}
		if workflow.TemplateParameters != nil {
			c.add("/workflow/template_parameters", "blueprint_workflow_template_parameters_forbidden", "custom mode forbids template_parameters")
		}
		if strings.TrimSpace(workflow.CustomSpecRef) == "" {
			c.add("/workflow/custom_spec_ref", "blueprint_workflow_custom_spec_required", "custom mode requires a frozen custom_spec_ref")
		}
		if strings.TrimSpace(workflow.DeclarativeSpecHash) != "" {
			c.add("/workflow/declarative_spec_hash", "blueprint_workflow_declarative_spec_forbidden", "custom mode forbids declarative_spec_hash")
		}
		validateTemplateGapAuthorization(c, workflow.TemplateGapAuthorization)
	case BlueprintWorkflowDeclarativeV1:
		if strings.TrimSpace(workflow.Template) != "" {
			c.add("/workflow/template", "blueprint_workflow_template_forbidden", "declarative_v1 mode forbids template")
		}
		if workflow.TemplateParameters != nil {
			c.add("/workflow/template_parameters", "blueprint_workflow_template_parameters_forbidden", "declarative_v1 mode forbids template_parameters")
		}
		if strings.TrimSpace(workflow.CustomSpecRef) != "" {
			c.add("/workflow/custom_spec_ref", "blueprint_workflow_custom_spec_forbidden", "declarative_v1 mode forbids custom_spec_ref")
		}
		if !isCanonicalSHA256(strings.TrimSpace(workflow.DeclarativeSpecHash)) {
			c.add("/workflow/declarative_spec_hash", "blueprint_workflow_declarative_spec_required", "declarative_v1 mode requires a canonical frozen spec hash")
		}
		if workflow.TemplateGapAuthorization != nil {
			c.add("/workflow/template_gap_authorization", "blueprint_workflow_gap_authorization_forbidden", "declarative_v1 mode forbids template-gap authorization")
		}
	default:
		c.add("/workflow/mode", "blueprint_workflow_mode_invalid", "workflow mode must be template, declarative_v1, or custom")
	}
}

func validateBlueprintTemplate(c *blueprintProblemCollector, template string, params *BlueprintWorkflowTemplateParametersV1, members map[string]BlueprintMemberV1) {
	known := false
	switch template {
	case BlueprintTemplateDeliveryRework, BlueprintTemplateParallelReview,
		BlueprintTemplateCreativeRework, BlueprintTemplateResearchSummary:
		known = true
	default:
		c.add("/workflow/template", "blueprint_workflow_template_invalid", "template is not supported")
	}
	if params == nil {
		c.add("/workflow/template_parameters", "blueprint_workflow_template_parameters_required", "template mode requires template_parameters")
		return
	}
	if strings.TrimSpace(params.LeadInstruction) == "" {
		c.add("/workflow/template_parameters/lead_instruction", "blueprint_workflow_lead_instruction_required", "lead_instruction is required")
	}
	validateTemplateMemberRef := func(path, ref string, required bool) {
		ref = strings.TrimSpace(ref)
		if ref == "" && required {
			c.add(path, "blueprint_workflow_member_ref_required", "member reference is required")
		} else if ref != "" {
			if _, exists := members[ref]; !exists {
				c.add(path, "blueprint_workflow_member_ref_unknown", "member reference must identify a member stable_ref")
			}
		}
	}
	if known {
		switch template {
		case BlueprintTemplateDeliveryRework, BlueprintTemplateCreativeRework:
			validateTemplateMemberRef("/workflow/template_parameters/primary_ref", params.PrimaryRef, true)
			validateTemplateMemberRef("/workflow/template_parameters/reviewer_ref", params.ReviewerRef, true)
			if len(params.ParallelWorkerRefs) != 0 {
				c.add("/workflow/template_parameters/parallel_worker_refs", "blueprint_workflow_parallel_refs_forbidden", "rework templates forbid parallel_worker_refs")
			}
			if strings.TrimSpace(params.FinalizerRef) != "" {
				c.add("/workflow/template_parameters/finalizer_ref", "blueprint_workflow_finalizer_ref_forbidden", "rework templates forbid finalizer_ref")
			}
			if params.MaxIterations == nil || *params.MaxIterations < 1 || *params.MaxIterations > 5 {
				c.add("/workflow/template_parameters/max_iterations", "blueprint_workflow_max_iterations_invalid", "rework templates require max_iterations between 1 and 5")
			}
		case BlueprintTemplateParallelReview, BlueprintTemplateResearchSummary:
			if strings.TrimSpace(params.PrimaryRef) != "" {
				c.add("/workflow/template_parameters/primary_ref", "blueprint_workflow_primary_ref_forbidden", "synthesis templates forbid primary_ref")
			}
			if strings.TrimSpace(params.ReviewerRef) != "" {
				c.add("/workflow/template_parameters/reviewer_ref", "blueprint_workflow_reviewer_ref_forbidden", "synthesis templates forbid reviewer_ref")
			}
			if len(params.ParallelWorkerRefs) < 2 {
				c.add("/workflow/template_parameters/parallel_worker_refs", "blueprint_workflow_parallel_refs_too_few", "synthesis templates require at least two parallel workers")
			}
			validateTemplateMemberRef("/workflow/template_parameters/finalizer_ref", params.FinalizerRef, true)
			if params.MaxIterations != nil {
				c.add("/workflow/template_parameters/max_iterations", "blueprint_workflow_max_iterations_forbidden", "synthesis templates forbid max_iterations")
			}
		}
	}

	seenParallel := map[string]bool{}
	for index, ref := range params.ParallelWorkerRefs {
		path := fmt.Sprintf("/workflow/template_parameters/parallel_worker_refs/%d", index)
		validateTemplateMemberRef(path, ref, true)
		ref = strings.TrimSpace(ref)
		if ref != "" && seenParallel[ref] {
			c.add(path, "blueprint_workflow_parallel_ref_duplicate", "parallel worker reference must be unique")
		}
		seenParallel[ref] = true
	}
	if len(params.ResultRequirements) == 0 {
		c.add("/workflow/template_parameters/result_requirements", "blueprint_workflow_result_requirements_required", "result_requirements are required")
	}
	resultRequirementRefs := make([]string, 0, len(params.ResultRequirements))
	for ref := range params.ResultRequirements {
		resultRequirementRefs = append(resultRequirementRefs, ref)
	}
	sort.Strings(resultRequirementRefs)
	for _, ref := range resultRequirementRefs {
		requirement := params.ResultRequirements[ref]
		path := "/workflow/template_parameters/result_requirements/" + ref
		if _, exists := members[ref]; !exists {
			c.add(path, "blueprint_workflow_result_requirement_ref_unknown", "result requirement key must identify a member stable_ref")
		}
		if strings.TrimSpace(requirement) == "" {
			c.add(path, "blueprint_workflow_result_requirement_blank", "result requirement must not be blank")
		}
	}
}

func validateTemplateGapAuthorization(c *blueprintProblemCollector, fact *TemplateGapAuthorizationV1) {
	if fact == nil {
		c.add("/workflow/template_gap_authorization", "blueprint_template_gap_authorization_required", "custom mode requires explicit administrator template-gap authorization")
		return
	}
	if !fact.Confirmed {
		c.add("/workflow/template_gap_authorization/confirmed", "blueprint_template_gap_not_confirmed", "template-gap authorization must be confirmed")
	}
	if strings.TrimSpace(fact.AuthorityRef) == "" {
		c.add("/workflow/template_gap_authorization/authority_ref", "blueprint_template_gap_authority_required", "authority_ref is required")
	}
	if !isCanonicalSHA256(fact.ScopeHash) {
		c.add("/workflow/template_gap_authorization/scope_hash", "blueprint_template_gap_scope_hash_invalid", "scope_hash must be a lowercase 64-hex sha256")
	}
}

func validateBlueprintRevisionPolicy(c *blueprintProblemCollector, policy BlueprintRevisionPolicyV1, members map[string]BlueprintMemberV1) {
	if policy.MaxRevisions < 1 || policy.MaxRevisions > maxContractIterations {
		c.add("/revision_policy/max_revisions", "blueprint_max_revisions_invalid", "max_revisions must be between 1 and 3")
	}
	if len(policy.AllowedPatchPaths) == 0 {
		c.add("/revision_policy/allowed_patch_paths", "blueprint_allowed_patch_paths_required", "at least one exact patch path is required")
	}
	seen := make(map[string]bool, len(policy.AllowedPatchPaths))
	for index, path := range policy.AllowedPatchPaths {
		problemPath := fmt.Sprintf("/revision_policy/allowed_patch_paths/%d", index)
		if !isCanonicalBlueprintPath(path) || !isAllowedBlueprintRevisionPath(path, members) || isForbiddenBlueprintPatchPath(path) {
			c.add(problemPath, "blueprint_allowed_patch_path_invalid", "allowed patch path must be an exact stable business-quality field path")
		} else if seen[path] {
			c.add(problemPath, "blueprint_allowed_patch_path_duplicate", "allowed patch path must be unique")
		}
		seen[path] = true
	}
}

// ValidateBlueprintPatchV1 reports intrinsic patch problems, target/change
// mismatches, and any path not explicitly authorized by revisionPolicy.
func ValidateBlueprintPatchV1(patch BlueprintPatchV1, revisionPolicy BlueprintRevisionPolicyV1) error {
	c := &blueprintProblemCollector{document: "blueprint_patch_v1"}
	if patch.SchemaVersion != BlueprintSchemaVersionV1 {
		c.add("/schema_version", "blueprint_patch_schema_version_invalid", "schema_version must be 1")
	}
	if !isCanonicalSHA256(patch.SourceReportHash) {
		c.add("/source_report_hash", "blueprint_patch_source_report_hash_invalid", "source_report_hash must be a lowercase 64-hex sha256")
	}
	if patch.FailureClass != BlueprintPatchFailureBusinessQuality {
		c.add("/failure_class", "blueprint_patch_failure_class_invalid", "only business_quality_failure can produce a blueprint patch")
	}
	if len(patch.TargetPaths) == 0 {
		c.add("/target_paths", "blueprint_patch_target_paths_required", "target_paths are required")
	}
	if len(patch.Changes) == 0 {
		c.add("/changes", "blueprint_patch_changes_required", "changes are required")
	}

	authorized := make(map[string]bool, len(revisionPolicy.AllowedPatchPaths))
	for _, path := range revisionPolicy.AllowedPatchPaths {
		if isCanonicalBlueprintPath(path) && isStructurallyAllowedBlueprintRevisionPath(path) && !isForbiddenBlueprintPatchPath(path) {
			authorized[path] = true
		}
	}
	targets := make(map[string]bool, len(patch.TargetPaths))
	for index, path := range patch.TargetPaths {
		problemPath := fmt.Sprintf("/target_paths/%d", index)
		if !isCanonicalBlueprintPath(path) || !isStructurallyAllowedBlueprintRevisionPath(path) || isForbiddenBlueprintPatchPath(path) {
			c.add(problemPath, "blueprint_patch_target_path_invalid", "target path must be canonical and must not change governance")
		}
		if targets[path] {
			c.add(problemPath, "blueprint_patch_target_path_duplicate", "target path must be unique")
		}
		if !authorized[path] {
			c.add(problemPath, "blueprint_patch_target_path_unauthorized", "target path is not authorized by revision policy")
		}
		targets[path] = true
	}

	changePaths := make(map[string]bool, len(patch.Changes))
	for index, change := range patch.Changes {
		base := fmt.Sprintf("/changes/%d", index)
		path := change.Path
		if !isCanonicalBlueprintPath(path) || !isStructurallyAllowedBlueprintRevisionPath(path) || isForbiddenBlueprintPatchPath(path) {
			c.add(base+"/path", "blueprint_patch_change_path_invalid", "change path must be canonical and must not change governance")
		}
		if changePaths[path] {
			c.add(base+"/path", "blueprint_patch_change_path_duplicate", "change path must be unique")
		}
		if !authorized[path] {
			c.add(base+"/path", "blueprint_patch_change_path_unauthorized", "change path is not authorized by revision policy")
		}
		changePaths[path] = true
		if !isCanonicalSHA256(change.ExpectedValueHash) {
			c.add(base+"/expected_value_hash", "blueprint_patch_expected_hash_invalid", "expected_value_hash must be a lowercase 64-hex sha256")
		}
		if len(change.ProposedValue) == 0 || !json.Valid(change.ProposedValue) {
			c.add(base+"/proposed_value", "blueprint_patch_proposed_value_invalid", "proposed_value must be valid JSON")
		}
		validatePatchEvidence(c, base+"/evidence_refs", change.EvidenceRefs)
		if strings.TrimSpace(change.Reason) == "" {
			c.add(base+"/reason", "blueprint_patch_reason_required", "reason is required")
		}
	}
	if !equalStringSets(targets, changePaths) {
		c.add("/target_paths", "blueprint_patch_target_paths_mismatch", "target_paths must exactly match change paths")
	}
	return c.err()
}

// ApplyBlueprintPatchV1 applies one compare-and-replace business revision to
// a deep copy of the latest persisted Blueprint. Stable member refs are
// resolved as logical path segments; positional array indexes are never
// accepted. The complete resulting Blueprint is validated before return.
func ApplyBlueprintPatchV1(blueprint TeamBlueprintV1, patch BlueprintPatchV1) (TeamBlueprintV1, error) {
	if err := ValidateTeamBlueprintV1(blueprint); err != nil {
		return TeamBlueprintV1{}, err
	}
	if err := ValidateBlueprintPatchV1(patch, blueprint.RevisionPolicy); err != nil {
		return TeamBlueprintV1{}, err
	}
	encoded, err := json.Marshal(blueprint)
	if err != nil {
		return TeamBlueprintV1{}, err
	}
	var revised TeamBlueprintV1
	if err := json.Unmarshal(encoded, &revised); err != nil {
		return TeamBlueprintV1{}, err
	}
	for index, change := range patch.Changes {
		current, err := blueprintPatchValue(revised, change.Path)
		if err != nil {
			return TeamBlueprintV1{}, fmt.Errorf("apply blueprint patch change %d: %w", index, err)
		}
		currentHash, err := hashDocument(current)
		if err != nil {
			return TeamBlueprintV1{}, fmt.Errorf("hash blueprint patch path %s: %w", change.Path, err)
		}
		if currentHash != change.ExpectedValueHash {
			return TeamBlueprintV1{}, fmt.Errorf("apply blueprint patch change %d: expected value hash mismatch at %s", index, change.Path)
		}
		if err := replaceBlueprintPatchValue(&revised, change.Path, change.ProposedValue); err != nil {
			return TeamBlueprintV1{}, fmt.Errorf("apply blueprint patch change %d: %w", index, err)
		}
	}
	if err := ValidateTeamBlueprintV1(revised); err != nil {
		return TeamBlueprintV1{}, err
	}
	return revised, nil
}

// BlueprintPatchValueHashV1 returns the canonical hash a planner must put in
// expected_value_hash for one authorized stable patch path.
func BlueprintPatchValueHashV1(blueprint TeamBlueprintV1, path string) (string, error) {
	if err := ValidateTeamBlueprintV1(blueprint); err != nil {
		return "", err
	}
	value, err := blueprintPatchValue(blueprint, path)
	if err != nil {
		return "", err
	}
	return hashDocument(value)
}

func blueprintPatchValue(blueprint TeamBlueprintV1, path string) (any, error) {
	switch path {
	case "/purpose":
		return blueprint.Purpose, nil
	case "/lead_ref":
		return blueprint.LeadRef, nil
	case "/workflow/custom_spec_ref":
		return blueprint.Workflow.CustomSpecRef, nil
	}
	parts := strings.Split(path, "/")
	if len(parts) == 4 && parts[1] == "members" {
		for _, member := range blueprint.Members {
			if member.StableRef != parts[2] {
				continue
			}
			switch parts[3] {
			case "responsibilities":
				return member.Responsibilities, nil
			case "capabilities":
				return member.Capabilities, nil
			case "model_ref":
				return member.ModelRef, nil
			case "execution_policy":
				return member.ExecutionPolicy, nil
			}
		}
	}
	params := blueprint.Workflow.TemplateParameters
	if params != nil && len(parts) == 4 && parts[1] == "workflow" && parts[2] == "template_parameters" {
		switch parts[3] {
		case "lead_instruction":
			return params.LeadInstruction, nil
		case "primary_ref":
			return params.PrimaryRef, nil
		case "reviewer_ref":
			return params.ReviewerRef, nil
		case "parallel_worker_refs":
			return params.ParallelWorkerRefs, nil
		case "finalizer_ref":
			return params.FinalizerRef, nil
		case "max_iterations":
			return params.MaxIterations, nil
		}
	}
	if params != nil && len(parts) == 5 && parts[1] == "workflow" && parts[2] == "template_parameters" && parts[3] == "result_requirements" {
		value, ok := params.ResultRequirements[parts[4]]
		if ok {
			return value, nil
		}
	}
	return nil, fmt.Errorf("blueprint patch path %q does not resolve", path)
}

func replaceBlueprintPatchValue(blueprint *TeamBlueprintV1, path string, raw json.RawMessage) error {
	decode := func(target any) error { return decodeStrictPatchValue(raw, target) }
	switch path {
	case "/purpose":
		return decode(&blueprint.Purpose)
	case "/lead_ref":
		return decode(&blueprint.LeadRef)
	case "/workflow/custom_spec_ref":
		return decode(&blueprint.Workflow.CustomSpecRef)
	}
	parts := strings.Split(path, "/")
	if len(parts) == 4 && parts[1] == "members" {
		for index := range blueprint.Members {
			member := &blueprint.Members[index]
			if member.StableRef != parts[2] {
				continue
			}
			switch parts[3] {
			case "responsibilities":
				return decode(&member.Responsibilities)
			case "capabilities":
				return decode(&member.Capabilities)
			case "model_ref":
				return decode(&member.ModelRef)
			case "execution_policy":
				return decode(&member.ExecutionPolicy)
			}
		}
	}
	params := blueprint.Workflow.TemplateParameters
	if params != nil && len(parts) == 4 && parts[1] == "workflow" && parts[2] == "template_parameters" {
		switch parts[3] {
		case "lead_instruction":
			return decode(&params.LeadInstruction)
		case "primary_ref":
			return decode(&params.PrimaryRef)
		case "reviewer_ref":
			return decode(&params.ReviewerRef)
		case "parallel_worker_refs":
			return decode(&params.ParallelWorkerRefs)
		case "finalizer_ref":
			return decode(&params.FinalizerRef)
		case "max_iterations":
			return decode(&params.MaxIterations)
		}
	}
	if params != nil && len(parts) == 5 && parts[1] == "workflow" && parts[2] == "template_parameters" && parts[3] == "result_requirements" {
		if _, ok := params.ResultRequirements[parts[4]]; ok {
			var value string
			if err := decode(&value); err != nil {
				return err
			}
			params.ResultRequirements[parts[4]] = value
			return nil
		}
	}
	return fmt.Errorf("blueprint patch path %q does not resolve", path)
}

func decodeStrictPatchValue(raw json.RawMessage, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("proposed value contains multiple JSON values")
		}
		return err
	}
	return nil
}

func validatePatchEvidence(c *blueprintProblemCollector, path string, refs []string) {
	if len(refs) == 0 {
		c.add(path, "blueprint_patch_evidence_required", "at least one evidence reference is required")
		return
	}
	seen := make(map[string]bool, len(refs))
	for index, ref := range refs {
		ref = strings.TrimSpace(ref)
		problemPath := fmt.Sprintf("%s/%d", path, index)
		if ref == "" {
			c.add(problemPath, "blueprint_patch_evidence_blank", "evidence reference must not be blank")
		} else if seen[ref] {
			c.add(problemPath, "blueprint_patch_evidence_duplicate", "evidence reference must be unique")
		}
		seen[ref] = true
	}
}

func equalStringSets(first, second map[string]bool) bool {
	if len(first) != len(second) {
		return false
	}
	for value := range first {
		if !second[value] {
			return false
		}
	}
	return true
}

func isCanonicalSHA256(value string) bool {
	return canonicalSHA256Pattern.MatchString(value)
}

func isCanonicalBlueprintPath(path string) bool {
	if path == "" || strings.TrimSpace(path) != path || !strings.HasPrefix(path, "/") ||
		strings.HasSuffix(path, "/") || strings.Contains(path, "//") || strings.Contains(path, "~") {
		return false
	}
	for _, segment := range strings.Split(path[1:], "/") {
		if segment == "" || strings.TrimSpace(segment) != segment {
			return false
		}
	}
	return true
}

func isForbiddenBlueprintPatchPath(path string) bool {
	for _, prefix := range []string{
		"/mode", "/team_id", "/new_team_name", "/schema_version",
		"/budget", "/asset_scope", "/allowed_assets", "/permissions", "/authorization",
		"/revision_policy", "/workflow/mode", "/workflow/template",
		"/workflow/template_gap_authorization", "/members/stable_ref",
		"/members/management_mode",
	} {
		if path == prefix || strings.HasPrefix(path, prefix+"/") {
			return true
		}
	}
	return false
}

func isAllowedBlueprintRevisionPath(path string, members map[string]BlueprintMemberV1) bool {
	if !isStructurallyAllowedBlueprintRevisionPath(path) {
		return false
	}
	if path == "/purpose" || path == "/lead_ref" || path == "/workflow/custom_spec_ref" {
		return true
	}
	parts := strings.Split(path, "/")
	if len(parts) == 4 && parts[1] == "members" {
		if _, exists := members[parts[2]]; !exists {
			return false
		}
		switch parts[3] {
		case "responsibilities", "capabilities", "model_ref", "execution_policy":
			return true
		}
	}
	if len(parts) == 4 && parts[1] == "workflow" && parts[2] == "template_parameters" {
		switch parts[3] {
		case "lead_instruction", "primary_ref", "reviewer_ref", "parallel_worker_refs", "finalizer_ref", "max_iterations":
			return true
		}
	}
	if len(parts) == 5 && parts[1] == "workflow" && parts[2] == "template_parameters" && parts[3] == "result_requirements" {
		_, exists := members[parts[4]]
		return exists
	}
	return false
}

func isStructurallyAllowedBlueprintRevisionPath(path string) bool {
	if path == "/purpose" || path == "/lead_ref" || path == "/workflow/custom_spec_ref" {
		return true
	}
	parts := strings.Split(path, "/")
	if len(parts) == 4 && parts[1] == "members" && blueprintStableRefPattern.MatchString(parts[2]) {
		switch parts[3] {
		case "responsibilities", "capabilities", "model_ref", "execution_policy":
			return true
		}
	}
	if len(parts) == 4 && parts[1] == "workflow" && parts[2] == "template_parameters" {
		switch parts[3] {
		case "lead_instruction", "primary_ref", "reviewer_ref", "parallel_worker_refs", "finalizer_ref", "max_iterations":
			return true
		}
	}
	return len(parts) == 5 && parts[1] == "workflow" && parts[2] == "template_parameters" &&
		parts[3] == "result_requirements" && blueprintStableRefPattern.MatchString(parts[4])
}

// BlueprintHash returns the canonical SHA-256 of a valid semantic blueprint.
// It normalizes order-insensitive collections on a deep copy.
func (blueprint TeamBlueprintV1) BlueprintHash() (string, error) {
	if err := ValidateTeamBlueprintV1(blueprint); err != nil {
		return "", err
	}
	return hashDocument(normalizeTeamBlueprint(blueprint))
}

// BlueprintPatchHash returns the canonical SHA-256 of the semantic patch. It
// does not authorize the patch; callers must separately validate it against
// the frozen revision policy before execution.
func (patch BlueprintPatchV1) BlueprintPatchHash() (string, error) {
	selfPolicy := BlueprintRevisionPolicyV1{
		MaxRevisions:      1,
		AllowedPatchPaths: append([]string(nil), patch.TargetPaths...),
	}
	if err := ValidateBlueprintPatchV1(patch, selfPolicy); err != nil {
		return "", err
	}
	return hashDocument(normalizeBlueprintPatch(patch))
}

func normalizeTeamBlueprint(value TeamBlueprintV1) TeamBlueprintV1 {
	normalized := value
	normalized.Mode = strings.TrimSpace(value.Mode)
	normalized.TeamID = strings.TrimSpace(value.TeamID)
	normalized.NewTeamName = normalizeText(value.NewTeamName)
	normalized.TeamDisplayName = normalizeText(value.TeamDisplayName)
	normalized.Purpose = normalizeText(value.Purpose)
	normalized.LeadRef = strings.TrimSpace(value.LeadRef)
	normalized.Members = make([]BlueprintMemberV1, len(value.Members))
	for index, member := range value.Members {
		member.StableRef = strings.TrimSpace(member.StableRef)
		member.Name = normalizeText(member.Name)
		member.DisplayName = normalizeText(member.DisplayName)
		member.Role = strings.TrimSpace(member.Role)
		member.ManagementMode = strings.TrimSpace(member.ManagementMode)
		member.ModelRef = strings.TrimSpace(member.ModelRef)
		member.Responsibilities = normalizeAndSortStrings(member.Responsibilities)
		member.Capabilities = normalizeAndSortStrings(member.Capabilities)
		member.ExecutionPolicy.EngineClass = strings.TrimSpace(member.ExecutionPolicy.EngineClass)
		member.ExecutionPolicy.Engine = strings.TrimSpace(member.ExecutionPolicy.Engine)
		member.ExecutionPolicy.ExecutionMode = strings.TrimSpace(member.ExecutionPolicy.ExecutionMode)
		member.ExecutionPolicy.RuntimeRef = strings.TrimSpace(member.ExecutionPolicy.RuntimeRef)
		member.ExecutionPolicy.InternalGraphRef = strings.TrimSpace(member.ExecutionPolicy.InternalGraphRef)
		normalized.Members[index] = member
	}
	sort.Slice(normalized.Members, func(i, j int) bool { return normalized.Members[i].StableRef < normalized.Members[j].StableRef })
	normalized.Workflow = normalizeBlueprintWorkflow(value.Workflow)
	normalized.RevisionPolicy.AllowedPatchPaths = append([]string(nil), value.RevisionPolicy.AllowedPatchPaths...)
	for index := range normalized.RevisionPolicy.AllowedPatchPaths {
		normalized.RevisionPolicy.AllowedPatchPaths[index] = strings.TrimSpace(normalized.RevisionPolicy.AllowedPatchPaths[index])
	}
	sort.Strings(normalized.RevisionPolicy.AllowedPatchPaths)
	return normalized
}

func normalizeBlueprintWorkflow(value BlueprintWorkflowV1) BlueprintWorkflowV1 {
	normalized := value
	normalized.Mode = strings.TrimSpace(value.Mode)
	normalized.Template = strings.TrimSpace(value.Template)
	normalized.CustomSpecRef = strings.TrimSpace(value.CustomSpecRef)
	normalized.DeclarativeSpecHash = strings.TrimSpace(value.DeclarativeSpecHash)
	if value.TemplateParameters != nil {
		params := *value.TemplateParameters
		params.LeadInstruction = normalizeText(params.LeadInstruction)
		params.PrimaryRef = strings.TrimSpace(params.PrimaryRef)
		params.ReviewerRef = strings.TrimSpace(params.ReviewerRef)
		params.FinalizerRef = strings.TrimSpace(params.FinalizerRef)
		params.ParallelWorkerRefs = normalizeAndSortRefs(params.ParallelWorkerRefs)
		if value.TemplateParameters.MaxIterations != nil {
			iterations := *value.TemplateParameters.MaxIterations
			params.MaxIterations = &iterations
		}
		params.ResultRequirements = make(map[string]string, len(value.TemplateParameters.ResultRequirements))
		for ref, requirement := range value.TemplateParameters.ResultRequirements {
			params.ResultRequirements[strings.TrimSpace(ref)] = normalizeText(requirement)
		}
		normalized.TemplateParameters = &params
	}
	if value.TemplateGapAuthorization != nil {
		fact := *value.TemplateGapAuthorization
		fact.AuthorityRef = strings.TrimSpace(fact.AuthorityRef)
		fact.ScopeHash = strings.TrimSpace(fact.ScopeHash)
		normalized.TemplateGapAuthorization = &fact
	}
	return normalized
}

func normalizeBlueprintPatch(value BlueprintPatchV1) BlueprintPatchV1 {
	normalized := value
	normalized.SourceReportHash = strings.TrimSpace(value.SourceReportHash)
	normalized.FailureClass = strings.TrimSpace(value.FailureClass)
	normalized.TargetPaths = append([]string(nil), value.TargetPaths...)
	for index := range normalized.TargetPaths {
		normalized.TargetPaths[index] = strings.TrimSpace(normalized.TargetPaths[index])
	}
	sort.Strings(normalized.TargetPaths)
	normalized.Changes = make([]BlueprintFieldPatchV1, len(value.Changes))
	for index, change := range value.Changes {
		change.Path = strings.TrimSpace(change.Path)
		change.ExpectedValueHash = strings.TrimSpace(change.ExpectedValueHash)
		change.ProposedValue = append(json.RawMessage(nil), change.ProposedValue...)
		change.EvidenceRefs = normalizeAndSortRefs(change.EvidenceRefs)
		change.Reason = normalizeText(change.Reason)
		normalized.Changes[index] = change
	}
	sort.Slice(normalized.Changes, func(i, j int) bool { return normalized.Changes[i].Path < normalized.Changes[j].Path })
	return normalized
}

func normalizeAndSortStrings(values []string) []string {
	normalized := make([]string, len(values))
	for index, value := range values {
		normalized[index] = normalizeText(value)
	}
	sort.Slice(normalized, func(i, j int) bool {
		return normalizeComparableText(normalized[i]) < normalizeComparableText(normalized[j])
	})
	return normalized
}

func normalizeAndSortRefs(values []string) []string {
	normalized := make([]string, len(values))
	for index, value := range values {
		normalized[index] = strings.TrimSpace(value)
	}
	sort.Strings(normalized)
	return normalized
}

func normalizeText(value string) string {
	return strings.Join(strings.Fields(value), " ")
}

func normalizeComparableText(value string) string {
	return strings.ToLower(normalizeText(value))
}
