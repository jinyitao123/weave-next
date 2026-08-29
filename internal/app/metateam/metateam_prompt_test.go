package metateam

import (
	"reflect"
	"strings"
	"testing"

	"github.com/jinyitao123/weave/internal/build/teambuild"
)

func TestRetiredConstructionRolesMatchBuildControlledResources(t *testing.T) {
	configSchema := teambuild.ConfigEngineerOutputSchema()
	if ConfigEngineerPrompt != teambuild.ConfigEngineerPrompt ||
		!sameOutputSchemaCanonical(&configEngineerOutputSchema, &configSchema) {
		t.Fatal("build-controlled config engineer drifted from the frozen registry role")
	}
	if GraphDesignerPrompt != teambuild.GraphDesignerPrompt ||
		!reflect.DeepEqual(graphDesignerDefinition(), teambuild.GraphDesignerDefinition()) {
		t.Fatal("build-controlled graph designer drifted from the frozen registry role")
	}
}

func TestTeamArchitectPromptSeparatesCreateAndOptimizeProtocols(t *testing.T) {
	createStart := strings.Index(TeamArchitectPrompt, "create 分支：")
	optimizeStart := strings.Index(TeamArchitectPrompt, "optimize 分支：")
	if createStart < 0 || optimizeStart <= createStart {
		t.Fatalf("prompt does not contain ordered create/optimize branches")
	}
	createBranch := TeamArchitectPrompt[createStart:optimizeStart]
	for _, required := range []string{"team-template/v1 YAML 草稿", "tf_render_template_draft", "禁止调用 tf_submit_brief、tf_blueprint_plan"} {
		if !strings.Contains(createBranch, required) {
			t.Fatalf("create branch missing %q", required)
		}
	}
	optimizeBranch := TeamArchitectPrompt[optimizeStart:]
	for _, legacy := range []string{
		"optimize 在提交 brief 前必须读取目标团队、完整 roster 与相关 workflow",
		"tf_submit_brief 成功返回 build_run_id 后，本轮必须立刻视为已存在 planning BuildRun",
		"必须先调用 tf_blueprint_plan 提交 compact_blueprint，冻结 optimize 团队的 roster 与 stable_ref 绑定",
		"提交恢复纪律：若 tf_submit_brief 返回 code=server_floor_roundtrip_invalid",
	} {
		if !strings.Contains(optimizeBranch, legacy) {
			t.Fatalf("optimize branch lost legacy protocol %q", legacy)
		}
	}
}

func TestIsAgentNameMatchesOnlyGuideAndRetainedMetaTeamIdentities(t *testing.T) {
	for _, name := range []string{
		TeamArchitectName, ConfigEngineerName, GraphDesignerName,
		EvalDebuggerName, SemanticJudgeName, BlueprintPatchPlannerName,
	} {
		if !IsAgentName(name) {
			t.Fatalf("IsAgentName(%q) = false", name)
		}
	}
	for _, name := range []string{TeamName, "__graph_designer", "__anything_else", "worker"} {
		if IsAgentName(name) {
			t.Fatalf("IsAgentName(%q) = true", name)
		}
	}
}

func TestNewMetaAgentRecordUsesPlatformVisibility(t *testing.T) {
	for _, builtin := range metaTeamAgents() {
		if visibility := newMetaAgentRecord(builtin).Visibility; visibility != "platform" {
			t.Fatalf("newMetaAgentRecord(%q).Visibility = %q", builtin.name, visibility)
		}
	}
}
