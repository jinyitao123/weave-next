package teamforge

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/jinyitao123/loom/contract"
)

const validRenderedTemplateDraftYAML = `schema: team-template/v1
name: editorial-team
display_name: 编辑团队
purpose: 稳定产出经过审校的中文文章
template: delivery_rework_loop
template_parameters:
  lead_instruction: 澄清需求并汇报最终结果
  primary_ref: writer
  reviewer_ref: reviewer
  max_iterations: 2
  result_requirements:
    writer: 交付完整文章
    reviewer: 给出通过或返修结论
members:
  - name: lead
    display_name: 主编
    role: avatar
    responsibilities: [澄清目标]
    capabilities: [任务协调]
  - name: writer
    display_name: 作者
    role: worker
    responsibilities: [撰写文章]
    capabilities: [中文写作]
  - name: reviewer
    display_name: 审校
    role: worker
    responsibilities: [审校文章]
    capabilities: [质量检查]
lead: lead
delivery:
  success_criteria: [文章完整且事实一致]
budget:
  max_cost_usd: 2
`

func TestRenderTemplateDraftValidatesAndRendersPreview(t *testing.T) {
	dispatcher := NewRenderTemplateDraftTools("workspace-1", "__team_architect", nil)
	args, err := json.Marshal(map[string]string{"yaml": validRenderedTemplateDraftYAML})
	if err != nil {
		t.Fatal(err)
	}
	result, err := dispatcher.Dispatch(context.Background(), contract.ToolCall{
		ID: "call-1", Name: ToolRenderTemplateDraft, Args: string(args),
	})
	if err != nil {
		t.Fatalf("Dispatch() error = %v", err)
	}
	if result.IsError {
		t.Fatalf("Dispatch() result = %#v", result)
	}
	var payload renderTemplateDraftResult
	if err := json.Unmarshal([]byte(result.Content), &payload); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if payload.Status != "ready_for_review" || payload.SchemaVersion != 1 {
		t.Fatalf("status = %q schema = %d", payload.Status, payload.SchemaVersion)
	}
	if payload.IdempotencyKey == "" || payload.Preview.Name != "editorial-team" || len(payload.Preview.Members) != 3 {
		t.Fatalf("payload = %#v", payload)
	}
	if !strings.Contains(payload.YAML, "schema: team-template/v1") || !strings.Contains(payload.YAML, "display_name: 编辑团队") {
		t.Fatalf("rendered YAML = %q", payload.YAML)
	}
}

func TestRenderTemplateDraftReturnsM1SchemaProblems(t *testing.T) {
	dispatcher := NewRenderTemplateDraftTools("workspace-1", "__team_architect", nil)
	result, err := dispatcher.Dispatch(context.Background(), contract.ToolCall{
		ID: "call-1", Name: ToolRenderTemplateDraft, Args: `{"yaml":"schema: wrong"}`,
	})
	if err != nil {
		t.Fatalf("Dispatch() error = %v", err)
	}
	if !result.IsError || !strings.Contains(result.Content, `"code":"template_draft_invalid"`) ||
		!strings.Contains(result.Content, `"problems"`) {
		t.Fatalf("Dispatch() result = %#v", result)
	}
}
