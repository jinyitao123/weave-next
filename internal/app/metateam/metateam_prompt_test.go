package metateam

import (
	"strings"
	"testing"
)

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
