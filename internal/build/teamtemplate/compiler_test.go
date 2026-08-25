package teamtemplate

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/jinyitao123/weave/internal/build/teambuild"
)

const validResearchTemplateYAML = `schema: team-template/v1
name: market-research-trio
display_name: 市场调研三人组
purpose: 持续产出竞品监控报告
template: research_synthesis
template_parameters:
  lead_instruction: 组织协调调研与成稿
  parallel_worker_refs: [researcher, analyst]
  finalizer_ref: editor
  result_requirements:
    researcher: 给出可核验的一手来源
    analyst: 完成交叉验证
    editor: 汇总成结构化报告
members:
  - name: coordinator
    display_name: 协调员
    role: avatar
    responsibilities: [协调分工, 汇总交付]
    capabilities: [task_delegation]
    model_ref: model-a
  - name: researcher
    display_name: 调研员
    role: worker
    responsibilities: [检索与事实收集]
    capabilities: [web_search]
    execution_policy:
      engine_class: standard
      execution_mode: toolloop
  - name: analyst
    display_name: 分析员
    role: worker
    responsibilities: [归纳与交叉验证]
    capabilities: [analysis]
  - name: editor
    display_name: 调研编辑
    role: worker
    responsibilities: [汇总与交付]
    capabilities: [report_writing]
lead: coordinator
delivery:
  success_criteria: [数据可溯源, 覆盖全部指定竞品]
budget:
  max_cost_usd: 2.5
`

func TestCompileYAMLDeterministicAndValid(t *testing.T) {
	first, err := CompileYAML([]byte(validResearchTemplateYAML))
	if err != nil {
		t.Fatalf("CompileYAML() error = %v", err)
	}
	second, err := CompileYAML([]byte(validResearchTemplateYAML))
	if err != nil {
		t.Fatalf("second CompileYAML() error = %v", err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatal("CompileYAML() output is not deterministic")
	}
	if _, _, _, err := teambuild.ValidateBuildRunDrafts(first.Brief, first.Contract); err != nil {
		t.Fatalf("compiled drafts are invalid: %v", err)
	}
	if err := teambuild.ValidateTeamBlueprintV1(first.Blueprint); err != nil {
		t.Fatalf("compiled blueprint is invalid: %v", err)
	}
	if len(first.Contract.HardGates) != len(teambuild.DefaultFloorHardGates()) {
		t.Fatalf("hard gates = %d, want floor-only %d", len(first.Contract.HardGates), len(teambuild.DefaultFloorHardGates()))
	}
	if len(first.Contract.PublicScenarios) != 1 || !strings.Contains(first.Contract.PublicScenarios[0].ID, "placeholder") {
		t.Fatalf("public scenarios = %#v, want explicit post-evaluation placeholder", first.Contract.PublicScenarios)
	}
}

func TestCompileYAMLFieldMappings(t *testing.T) {
	compiled, err := CompileYAML([]byte(validResearchTemplateYAML))
	if err != nil {
		t.Fatalf("CompileYAML() error = %v", err)
	}
	params := compiled.Blueprint.Workflow.TemplateParameters
	cases := []struct {
		name string
		got  any
		want any
	}{
		{name: "schema to normalized template", got: compiled.Template.Schema, want: SchemaV1},
		{name: "name to brief team", got: compiled.Brief.NewTeamName, want: "market-research-trio"},
		{name: "name to blueprint team", got: compiled.Blueprint.NewTeamName, want: "market-research-trio"},
		{name: "name to write prefix", got: compiled.Brief.AllowedAssets.NamePrefix, want: "market-research-trio"},
		{name: "purpose to business direction", got: compiled.Brief.BusinessDirection, want: "持续产出竞品监控报告"},
		{name: "purpose to blueprint", got: compiled.Blueprint.Purpose, want: "持续产出竞品监控报告"},
		{name: "topology", got: compiled.Blueprint.Workflow.Template, want: teambuild.BlueprintTemplateResearchSummary},
		{name: "lead", got: compiled.Blueprint.LeadRef, want: "coordinator"},
		{name: "lead instruction", got: params.LeadInstruction, want: "组织协调调研与成稿"},
		{name: "parallel refs", got: params.ParallelWorkerRefs, want: []string{"researcher", "analyst"}},
		{name: "finalizer ref", got: params.FinalizerRef, want: "editor"},
		{name: "result requirements", got: params.ResultRequirements, want: map[string]string{
			"researcher": "给出可核验的一手来源", "analyst": "完成交叉验证", "editor": "汇总成结构化报告",
		}},
		{name: "success criteria to brief", got: compiled.Brief.SuccessCriteria, want: []string{"数据可溯源", "覆盖全部指定竞品"}},
		{name: "success criteria to rubric", got: []string{compiled.Contract.Rubric[0].Name, compiled.Contract.Rubric[1].Name}, want: []string{"数据可溯源", "覆盖全部指定竞品"}},
		{name: "round budget", got: compiled.Brief.RoundBudget.MaxCostUSD, want: 2.5},
		{name: "total budget", got: compiled.Brief.TotalBudget.MaxCostUSD, want: 2.5},
		{name: "stable ref derived from member name", got: compiled.Blueprint.Members[0].StableRef, want: "coordinator"},
		{name: "member asset inside prefix", got: compiled.Blueprint.Members[0].Name, want: "market-research-trio-coordinator"},
		{name: "member display name", got: compiled.Blueprint.Members[0].DisplayName, want: "协调员"},
		{name: "member role", got: compiled.Blueprint.Members[0].Role, want: teambuild.BlueprintMemberRoleAvatar},
		{name: "management fixed to managed", got: compiled.Blueprint.Members[0].ManagementMode, want: teambuild.BlueprintManagementManaged},
		{name: "responsibilities", got: compiled.Blueprint.Members[0].Responsibilities, want: []string{"协调分工", "汇总交付"}},
		{name: "capabilities", got: compiled.Blueprint.Members[0].Capabilities, want: []string{"task_delegation"}},
		{name: "model ref", got: compiled.Blueprint.Members[0].ModelRef, want: "model-a"},
		{name: "default execution engine", got: compiled.Blueprint.Members[0].ExecutionPolicy.EngineClass, want: teambuild.BlueprintEngineStandard},
		{name: "default execution mode", got: compiled.Blueprint.Members[0].ExecutionPolicy.ExecutionMode, want: teambuild.BlueprintExecutionToolLoop},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if !reflect.DeepEqual(tc.got, tc.want) {
				t.Fatalf("mapping got %#v, want %#v", tc.got, tc.want)
			}
		})
	}
}

func TestCompileSupportsAllBuiltinTopologies(t *testing.T) {
	maxIterations := 3
	base := Template{
		Schema: SchemaV1, Name: "builtin-team", DisplayName: "内置团队", Purpose: "稳定交付业务结果",
		Members: []Member{
			{Name: "lead", DisplayName: "负责人", Role: teambuild.BlueprintMemberRoleAvatar, Responsibilities: []string{"协调与交付"}, Capabilities: []string{"delegation"}},
			{Name: "maker", DisplayName: "执行者", Role: teambuild.BlueprintMemberRoleWorker, Responsibilities: []string{"执行"}, Capabilities: []string{"produce"}},
			{Name: "reviewer", DisplayName: "评审者", Role: teambuild.BlueprintMemberRoleWorker, Responsibilities: []string{"评审"}, Capabilities: []string{"review"}},
			{Name: "synthesizer", DisplayName: "汇总者", Role: teambuild.BlueprintMemberRoleWorker, Responsibilities: []string{"汇总"}, Capabilities: []string{"synthesis"}},
		},
		Lead: "lead", Delivery: Delivery{SuccessCriteria: []string{"结果完整"}}, Budget: Budget{MaxCostUSD: 1},
	}
	cases := []struct {
		name     string
		topology string
		params   TemplateParameters
	}{
		{name: "delivery rework", topology: teambuild.BlueprintTemplateDeliveryRework, params: TemplateParameters{
			LeadInstruction: "协调返工", PrimaryRef: "maker", ReviewerRef: "reviewer", MaxIterations: &maxIterations,
			ResultRequirements: map[string]string{"maker": "产出结果", "reviewer": "给出结论"},
		}},
		{name: "creative critique", topology: teambuild.BlueprintTemplateCreativeRework, params: TemplateParameters{
			LeadInstruction: "协调创作", PrimaryRef: "maker", ReviewerRef: "reviewer", MaxIterations: &maxIterations,
			ResultRequirements: map[string]string{"maker": "产出内容", "reviewer": "给出批注"},
		}},
		{name: "parallel review", topology: teambuild.BlueprintTemplateParallelReview, params: TemplateParameters{
			LeadInstruction: "协调并行评审", ParallelWorkerRefs: []string{"maker", "reviewer"}, FinalizerRef: "synthesizer",
			ResultRequirements: map[string]string{"maker": "给出意见", "reviewer": "给出意见", "synthesizer": "汇总结论"},
		}},
		{name: "research synthesis", topology: teambuild.BlueprintTemplateResearchSummary, params: TemplateParameters{
			LeadInstruction: "协调并行调研", ParallelWorkerRefs: []string{"maker", "reviewer"}, FinalizerRef: "synthesizer",
			ResultRequirements: map[string]string{"maker": "给出材料", "reviewer": "交叉验证", "synthesizer": "汇总报告"},
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			input := base
			input.Template = tc.topology
			input.TemplateParameters = tc.params
			compiled, err := Compile(input)
			if err != nil {
				t.Fatalf("Compile() error = %v", err)
			}
			if got := compiled.Blueprint.Workflow.Template; got != tc.topology {
				t.Fatalf("workflow template = %q, want %q", got, tc.topology)
			}
		})
	}
}

func TestParseYAMLLimits(t *testing.T) {
	alias := strings.Replace(validResearchTemplateYAML,
		"capabilities: [task_delegation]",
		"capabilities: &caps [task_delegation]\n  - name: broken\n    display_name: 占位\n    role: worker\n    responsibilities: [占位]\n    capabilities: *caps", 1)
	deep := "schema: team-template/v1\nunknown:\n"
	for i := 0; i < MaxYAMLDepth+1; i++ {
		deep += strings.Repeat("  ", i+1) + "level:\n"
	}
	deep += strings.Repeat("  ", MaxYAMLDepth+2) + "value: true\n"

	cases := []struct {
		name string
		data []byte
		code string
	}{
		{name: "empty", data: nil, code: "template_yaml_empty"},
		{name: "too large", data: []byte(strings.Repeat("x", MaxYAMLBytes+1)), code: "template_yaml_too_large"},
		{name: "alias", data: []byte(alias), code: "template_yaml_alias_forbidden"},
		{name: "depth", data: []byte(deep), code: "template_yaml_depth_exceeded"},
		{name: "unknown root", data: []byte(validResearchTemplateYAML + "surprise: true\n"), code: "template_yaml_unknown_field"},
		{name: "unknown nested", data: []byte(strings.Replace(validResearchTemplateYAML, "execution_mode: toolloop", "execution_mode: toolloop\n      surprise: true", 1)), code: "template_yaml_unknown_field"},
		{name: "multiple documents", data: []byte(validResearchTemplateYAML + "---\n{}\n"), code: "template_yaml_multiple_documents"},
		{name: "malformed", data: []byte("schema: [\n"), code: "template_yaml_invalid"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseYAML(tc.data)
			assertProblemCode(t, err, tc.code)
		})
	}
}

func TestCompileReportsFieldProblemsTogether(t *testing.T) {
	input := strings.NewReplacer(
		"schema: team-template/v1", "schema: wrong",
		"name: market-research-trio", "name: __bad",
		"lead: coordinator", "lead: researcher",
		"max_cost_usd: 2.5", "max_cost_usd: 0",
	).Replace(validResearchTemplateYAML)
	_, err := CompileYAML([]byte(input))
	for _, code := range []string{
		"template_schema_invalid", "template_reserved_name", "template_lead_role_invalid", "template_budget_invalid",
	} {
		assertProblemCode(t, err, code)
	}
}

func TestCompileRejectsCompactBlueprintSemanticMismatch(t *testing.T) {
	input := strings.Replace(validResearchTemplateYAML, "finalizer_ref: editor", "finalizer_ref: missing", 1)
	_, err := CompileYAML([]byte(input))
	assertProblemPath(t, err, "/template_parameters/finalizer_ref")
}

func TestCompileRejectsAvatarAsWorkerNode(t *testing.T) {
	input := strings.Replace(validResearchTemplateYAML, "finalizer_ref: editor", "finalizer_ref: coordinator", 1)
	_, err := CompileYAML([]byte(input))
	assertProblemCode(t, err, "template_worker_ref_role_invalid")
	assertProblemPath(t, err, "/template_parameters/finalizer_ref")
}

func assertProblemCode(t *testing.T, err error, want string) {
	t.Helper()
	var validation *ValidationError
	if !errors.As(err, &validation) {
		t.Fatalf("error = %v, want ValidationError containing %q", err, want)
	}
	for _, problem := range validation.Problems {
		if problem.Code == want {
			return
		}
	}
	t.Fatalf("problems = %#v, want code %q", validation.Problems, want)
}

func assertProblemPath(t *testing.T, err error, want string) {
	t.Helper()
	var validation *ValidationError
	if !errors.As(err, &validation) {
		t.Fatalf("error = %v, want ValidationError containing path %q", err, want)
	}
	for _, problem := range validation.Problems {
		if problem.Path == want {
			return
		}
	}
	t.Fatalf("problems = %#v, want path %q", validation.Problems, want)
}
