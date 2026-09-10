package teamtemplate

import (
	"errors"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strings"

	"github.com/jinyitao123/weave/internal/base/deliverable"
	"github.com/jinyitao123/weave/internal/build/teambuild"
)

var templateIdentifierPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)

// CompileYAML parses, validates, and compiles one template without I/O.
func CompileYAML(data []byte) (Compilation, error) {
	template, err := ParseYAML(data)
	if err != nil {
		return Compilation{}, err
	}
	return Compile(template)
}

// Compile deterministically maps a template into teambuild planning
// documents. It never reads or writes platform state.
func Compile(template Template) (Compilation, error) {
	normalized := normalizeTemplate(template)
	if err := Validate(normalized); err != nil {
		return Compilation{}, err
	}

	brief := compileBrief(normalized)
	contract := compileContract(normalized)
	blueprint := compileBlueprint(normalized)

	c := &problemCollector{}
	if _, normalizedContract, _, err := teambuild.ValidateBuildRunDrafts(brief, contract); err != nil {
		c.add("/", "template_build_drafts_invalid", err.Error())
	} else {
		contract = normalizedContract
	}
	if err := teambuild.ValidateTeamBlueprintV1(blueprint); err != nil {
		var validation *teambuild.BlueprintValidationError
		if errors.As(err, &validation) {
			c.addBlueprintProblems(validation)
		} else {
			c.add("/", "template_blueprint_invalid", err.Error())
		}
	}
	if err := c.err(); err != nil {
		return Compilation{}, err
	}
	return Compilation{Template: normalized, Brief: brief, Contract: contract, Blueprint: blueprint}, nil
}

// Validate reports all independently detectable schema problems.
func Validate(template Template) error {
	c := &problemCollector{}
	if template.Schema != SchemaV1 {
		c.add("/schema", "template_schema_invalid", "schema must be team-template/v1")
	}
	validateIdentifier(c, "/name", template.Name, "team name")
	if len(template.Name)+len("-workflow") > 64 {
		c.add("/name", "template_name_too_long", "name must leave room for the derived -workflow asset within 64 characters")
	}
	if strings.TrimSpace(template.DisplayName) == "" {
		c.add("/display_name", "template_display_name_required", "display_name is required")
	}
	if strings.TrimSpace(template.Purpose) == "" {
		c.add("/purpose", "template_purpose_required", "purpose is required")
	}
	validateTemplateParameters(c, template.Template, template.TemplateParameters)
	validateMembers(c, template)
	validateStringList(c, "/delivery/success_criteria", template.Delivery.SuccessCriteria, true)
	if contract, err := template.Delivery.Contract.platformContract(); err != nil {
		c.add("/delivery/contract", "template_delivery_contract_invalid", err.Error())
	} else if err := deliverable.ValidateDeliveryContract(contract); err != nil {
		c.add("/delivery/contract", "template_delivery_contract_invalid", err.Error())
	}
	if template.Budget.MaxCostUSD <= 0 || math.IsNaN(template.Budget.MaxCostUSD) || math.IsInf(template.Budget.MaxCostUSD, 0) {
		c.add("/budget/max_cost_usd", "template_budget_invalid", "max_cost_usd must be finite and greater than zero")
	}
	return c.err()
}

func validateTemplateParameters(c *problemCollector, template string, params TemplateParameters) {
	switch template {
	case teambuild.BlueprintTemplateDeliveryRework, teambuild.BlueprintTemplateCreativeRework,
		teambuild.BlueprintTemplateParallelReview, teambuild.BlueprintTemplateResearchSummary:
	default:
		c.add("/template", "template_topology_invalid", "template must name one of the four built-in topologies")
	}
	if params.LeadInstruction == "" {
		c.add("/template_parameters/lead_instruction", "template_lead_instruction_required", "lead_instruction is required")
	}
	if len(params.ResultRequirements) == 0 {
		c.add("/template_parameters/result_requirements", "template_result_requirements_required", "result_requirements are required")
	}
	keys := make([]string, 0, len(params.ResultRequirements))
	for key := range params.ResultRequirements {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if strings.TrimSpace(params.ResultRequirements[key]) == "" {
			c.add("/template_parameters/result_requirements/"+jsonPointerSegment(key), "template_result_requirement_blank", "result requirement must not be blank")
		}
	}
}

func validateMembers(c *problemCollector, template Template) {
	if len(template.Members) == 0 {
		c.add("/members", "template_members_required", "at least one member is required")
		return
	}
	seen := map[string]bool{}
	members := map[string]Member{}
	for i, member := range template.Members {
		base := fmt.Sprintf("/members/%d", i)
		validateIdentifier(c, base+"/name", member.Name, "member name")
		if strings.HasPrefix(member.Name, "__") {
			c.add(base+"/name", "template_reserved_name", "member name must not use the reserved __ prefix")
		}
		if seen[member.Name] {
			c.add(base+"/name", "template_member_name_duplicate", "member name must be unique")
		} else {
			seen[member.Name] = true
			members[member.Name] = member
		}
		if len(template.Name)+1+len(member.Name) > 64 {
			c.add(base+"/name", "template_member_asset_name_too_long", "derived member asset name must not exceed 64 characters")
		}
		if member.DisplayName == "" {
			c.add(base+"/display_name", "template_member_display_name_required", "display_name is required")
		}
		switch member.Role {
		case teambuild.BlueprintMemberRoleAvatar, teambuild.BlueprintMemberRoleWorker:
		default:
			c.add(base+"/role", "template_member_role_invalid", "role must be avatar or worker")
		}
		validateStringList(c, base+"/responsibilities", member.Responsibilities, true)
		validateStringList(c, base+"/capabilities", member.Capabilities, true)
	}
	lead, ok := members[template.Lead]
	if template.Lead == "" {
		c.add("/lead", "template_lead_required", "lead is required")
	} else if !ok {
		c.add("/lead", "template_lead_unknown", "lead must reference a member name")
	} else if lead.Role != teambuild.BlueprintMemberRoleAvatar {
		c.add("/lead", "template_lead_role_invalid", "lead must reference an avatar member")
	}
	validateExecutableMemberRefs(c, template, members)
}

// Built-in workflow templates compile primary/reviewer/parallel/finalizer
// references into machine worker nodes. The platform authorization snapshot
// deliberately derives worker-node proofs only from the TeamWorker roster;
// the lead avatar is represented separately as ValidationContext.Lead and is
// therefore not a legal target for one of those nodes.
func validateExecutableMemberRefs(c *problemCollector, template Template, members map[string]Member) {
	params := template.TemplateParameters
	refs := make([]struct {
		path string
		ref  string
	}, 0, len(params.ParallelWorkerRefs)+3)
	appendRef := func(path, ref string) {
		if ref != "" {
			refs = append(refs, struct {
				path string
				ref  string
			}{path: path, ref: ref})
		}
	}
	appendRef("/template_parameters/primary_ref", params.PrimaryRef)
	appendRef("/template_parameters/reviewer_ref", params.ReviewerRef)
	for i, ref := range params.ParallelWorkerRefs {
		appendRef(fmt.Sprintf("/template_parameters/parallel_worker_refs/%d", i), ref)
	}
	appendRef("/template_parameters/finalizer_ref", params.FinalizerRef)

	seen := make(map[string]string, len(refs))
	for _, executable := range refs {
		member, ok := members[executable.ref]
		if !ok {
			c.add(executable.path, "template_worker_ref_unknown", "workflow executable reference must identify a member name")
			continue
		}
		if member.Role != teambuild.BlueprintMemberRoleWorker {
			c.add(executable.path, "template_worker_ref_role_invalid", "workflow executable reference must identify a worker member")
		}
		if prior, duplicate := seen[executable.ref]; duplicate {
			c.add(executable.path, "template_worker_ref_duplicate", "workflow executable reference duplicates "+prior)
		} else {
			seen[executable.ref] = executable.path
		}
	}
}

func validateIdentifier(c *problemCollector, path, value, label string) {
	if !templateIdentifierPattern.MatchString(value) {
		c.add(path, "template_identifier_invalid", label+" must match ^[a-z0-9][a-z0-9_-]*$")
	}
	if strings.HasPrefix(value, "__") {
		c.add(path, "template_reserved_name", label+" must not use the reserved __ prefix")
	}
}

func validateStringList(c *problemCollector, path string, values []string, required bool) {
	if required && len(values) == 0 {
		c.add(path, "template_list_required", "at least one value is required")
		return
	}
	seen := map[string]bool{}
	for i, value := range values {
		if value == "" {
			c.add(fmt.Sprintf("%s/%d", path, i), "template_list_value_blank", "value must not be blank")
		} else if seen[value] {
			c.add(fmt.Sprintf("%s/%d", path, i), "template_list_value_duplicate", "value must be unique")
		}
		seen[value] = true
	}
}

func normalizeTemplate(template Template) Template {
	template.Schema = strings.TrimSpace(template.Schema)
	template.Name = strings.TrimSpace(template.Name)
	template.DisplayName = strings.TrimSpace(template.DisplayName)
	template.Purpose = strings.TrimSpace(template.Purpose)
	template.Template = strings.TrimSpace(template.Template)
	template.Lead = strings.TrimSpace(template.Lead)
	template.TemplateParameters.LeadInstruction = strings.TrimSpace(template.TemplateParameters.LeadInstruction)
	template.TemplateParameters.PrimaryRef = strings.TrimSpace(template.TemplateParameters.PrimaryRef)
	template.TemplateParameters.ReviewerRef = strings.TrimSpace(template.TemplateParameters.ReviewerRef)
	template.TemplateParameters.FinalizerRef = strings.TrimSpace(template.TemplateParameters.FinalizerRef)
	template.TemplateParameters.ParallelWorkerRefs = trimStrings(template.TemplateParameters.ParallelWorkerRefs)
	if template.TemplateParameters.ResultRequirements != nil {
		values := make(map[string]string, len(template.TemplateParameters.ResultRequirements))
		for key, value := range template.TemplateParameters.ResultRequirements {
			values[strings.TrimSpace(key)] = strings.TrimSpace(value)
		}
		template.TemplateParameters.ResultRequirements = values
	}
	for i := range template.Members {
		member := &template.Members[i]
		member.Name = strings.TrimSpace(member.Name)
		member.DisplayName = strings.TrimSpace(member.DisplayName)
		member.Role = strings.TrimSpace(member.Role)
		member.Responsibilities = trimStrings(member.Responsibilities)
		member.Capabilities = trimStrings(member.Capabilities)
		member.ModelRef = strings.TrimSpace(member.ModelRef)
		if member.ExecutionPolicy != nil {
			policy := *member.ExecutionPolicy
			policy.EngineClass = strings.TrimSpace(policy.EngineClass)
			policy.Engine = strings.TrimSpace(policy.Engine)
			policy.ExecutionMode = strings.TrimSpace(policy.ExecutionMode)
			policy.RuntimeRef = strings.TrimSpace(policy.RuntimeRef)
			policy.InternalGraphRef = strings.TrimSpace(policy.InternalGraphRef)
			member.ExecutionPolicy = &policy
		}
	}
	template.Delivery.SuccessCriteria = trimStrings(template.Delivery.SuccessCriteria)
	if template.Delivery.Contract != nil {
		contract := *template.Delivery.Contract
		contract.Coverage = strings.TrimSpace(contract.Coverage)
		contract.ExternalEffects = strings.TrimSpace(contract.ExternalEffects)
		contract.ExternalEffectsCheckID = strings.TrimSpace(contract.ExternalEffectsCheckID)
		contract.Limitations = trimStrings(contract.Limitations)
		template.Delivery.Contract = &contract
	}
	return template
}

func trimStrings(values []string) []string {
	out := make([]string, len(values))
	for i, value := range values {
		out[i] = strings.TrimSpace(value)
	}
	return out
}

func compileBrief(template Template) teambuild.BuildBrief {
	budget := teambuild.Budget{MaxCostUSD: template.Budget.MaxCostUSD}
	return teambuild.BuildBrief{
		SchemaVersion: 1, Mode: teambuild.ModeCreate,
		BusinessDirection: template.Purpose, Task: template.Purpose,
		WorkflowBuildMode: teambuild.WorkflowBuildModeBlueprint,
		NewTeamName:       template.Name,
		SuccessCriteria:   append([]string(nil), template.Delivery.SuccessCriteria...),
		AllowedAssets: teambuild.AssetScope{
			AllowedKinds: []string{"agent", "team", "workflow"},
			NamePrefix:   template.Name,
		},
		RoundBudget: budget, TotalBudget: budget,
	}
}

func compileContract(template Template) teambuild.EvaluationContract {
	rubric := make([]teambuild.RubricDimension, len(template.Delivery.SuccessCriteria))
	for i, criterion := range template.Delivery.SuccessCriteria {
		rubric[i] = teambuild.RubricDimension{
			ID: fmt.Sprintf("success_criterion_%02d", i+1), Name: criterion,
			Description: criterion, MaxScore: 100, PassThreshold: 80,
		}
	}
	expected := "交付物满足冻结的成功标准：" + strings.Join(template.Delivery.SuccessCriteria, "；")
	return teambuild.EvaluationContract{
		SchemaVersion: 2, HardGates: teambuild.DefaultFloorHardGates(), Rubric: rubric,
		PublicScenarios: []teambuild.Scenario{{
			ID: "post_evaluation_placeholder", Input: "后置评测占位输入，评测前替换为真实业务场景。", Expected: expected,
		}},
		PerturbationRules: []string{"后置评测时补充一个实质不同的业务输入"},
		PerturbationScenarios: []teambuild.PerturbationScenario{{
			ID: "post_evaluation_placeholder_perturbed", BaseScenarioID: "post_evaluation_placeholder",
			Rule: "后置评测时补充一个实质不同的业务输入", Input: "后置评测扰动占位输入，评测前替换为实质不同的真实业务场景。", Expected: expected,
		}},
		HiddenScenarioCount:    0,
		SevereDefectDefinition: "交付失败、关键事实不可验证，或任一冻结成功标准完全未满足。",
		RunCount:               1, MaxIterations: 3,
		PassRules:         []string{"全部 floor hard gates 通过且每个 rubric 维度达到阈值。"},
		BlockRules:        []string{"缺少后置评测所需的真实场景或可验证证据时阻断评测。"},
		InfraFailureRules: []string{"基础设施失败不计为业务质量失败，修复后重新执行。"},
	}
}

func compileBlueprint(template Template) teambuild.TeamBlueprintV1 {
	members := make([]teambuild.BlueprintMemberV1, len(template.Members))
	for i, member := range template.Members {
		policy := teambuild.BlueprintExecutionPolicyV1{
			EngineClass: teambuild.BlueprintEngineStandard, ExecutionMode: teambuild.BlueprintExecutionToolLoop,
		}
		if member.ExecutionPolicy != nil {
			policy = teambuild.BlueprintExecutionPolicyV1{
				EngineClass:      member.ExecutionPolicy.EngineClass,
				Engine:           member.ExecutionPolicy.Engine,
				ExecutionMode:    member.ExecutionPolicy.ExecutionMode,
				RuntimeRef:       member.ExecutionPolicy.RuntimeRef,
				InternalGraphRef: member.ExecutionPolicy.InternalGraphRef,
			}
		}
		members[i] = teambuild.BlueprintMemberV1{
			StableRef: member.Name, Name: template.Name + "-" + member.Name,
			DisplayName: member.DisplayName, Role: member.Role,
			ManagementMode:   teambuild.BlueprintManagementManaged,
			Responsibilities: append([]string(nil), member.Responsibilities...),
			Capabilities:     append([]string(nil), member.Capabilities...),
			ModelRef:         member.ModelRef, ExecutionPolicy: policy,
		}
	}
	params := teambuild.BlueprintWorkflowTemplateParametersV1{
		LeadInstruction:    template.TemplateParameters.LeadInstruction,
		PrimaryRef:         template.TemplateParameters.PrimaryRef,
		ReviewerRef:        template.TemplateParameters.ReviewerRef,
		ParallelWorkerRefs: append([]string(nil), template.TemplateParameters.ParallelWorkerRefs...),
		FinalizerRef:       template.TemplateParameters.FinalizerRef,
		MaxIterations:      template.TemplateParameters.MaxIterations,
		ResultRequirements: copyStringMap(template.TemplateParameters.ResultRequirements),
	}
	deliveryContract, _ := template.Delivery.Contract.platformContract()
	if deliveryContract != nil {
		deliveryContract.Output = deliverable.OutputRequirement{Type: "text"}
	}
	return teambuild.TeamBlueprintV1{
		SchemaVersion: teambuild.BlueprintSchemaVersionV1,
		Mode:          teambuild.ModeCreate, NewTeamName: template.Name, TeamDisplayName: template.DisplayName,
		Purpose: template.Purpose, Members: members, LeadRef: template.Lead,
		Workflow: teambuild.BlueprintWorkflowV1{
			Mode: teambuild.BlueprintWorkflowTemplate, Template: template.Template, TemplateParameters: &params,
			DeliveryContract: deliveryContract,
		},
		RevisionPolicy: teambuild.BlueprintRevisionPolicyV1{
			MaxRevisions: 2, AllowedPatchPaths: defaultPatchPaths(members, params),
		},
	}
}

func defaultPatchPaths(members []teambuild.BlueprintMemberV1, params teambuild.BlueprintWorkflowTemplateParametersV1) []string {
	paths := []string{"/purpose", "/workflow/template_parameters/lead_instruction"}
	if params.MaxIterations != nil {
		paths = append(paths, "/workflow/template_parameters/max_iterations")
	}
	for _, member := range members {
		ref := member.StableRef
		paths = append(paths, "/members/"+ref+"/responsibilities", "/members/"+ref+"/capabilities")
		if _, ok := params.ResultRequirements[ref]; ok {
			paths = append(paths, "/workflow/template_parameters/result_requirements/"+ref)
		}
	}
	return paths
}

func copyStringMap(values map[string]string) map[string]string {
	if values == nil {
		return nil
	}
	out := make(map[string]string, len(values))
	for key, value := range values {
		out[key] = value
	}
	return out
}
