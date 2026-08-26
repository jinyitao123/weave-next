package teamforge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jinyitao123/loom/contract"
	"github.com/jinyitao123/weave/internal/build/teamtemplate"
	"gopkg.in/yaml.v3"
)

const ToolRenderTemplateDraft = "tf_render_template_draft"

var renderTemplateDraftInputSchema = json.RawMessage(`{
  "type":"object",
  "additionalProperties":false,
  "required":["yaml"],
  "properties":{
    "yaml":{"type":"string","description":"Complete team-template/v1 YAML draft."}
  }
}`)

type RenderTemplateDraftToolsDispatcher struct {
	workspaceID string
	agent       string
	audit       AuditRecorder
}

type renderTemplateDraftArgs struct {
	YAML string `json:"yaml"`
}

type renderTemplateDraftMember struct {
	Name        string `json:"name"`
	DisplayName string `json:"display_name"`
	Role        string `json:"role"`
}

type renderTemplateDraftPreview struct {
	Name            string                      `json:"name"`
	DisplayName     string                      `json:"display_name"`
	Purpose         string                      `json:"purpose"`
	Topology        string                      `json:"topology"`
	Lead            string                      `json:"lead"`
	Members         []renderTemplateDraftMember `json:"members"`
	SuccessCriteria []string                    `json:"success_criteria"`
	MaxCostUSD      float64                     `json:"max_cost_usd"`
}

type renderTemplateDraftResult struct {
	SchemaVersion  int                        `json:"schema_version"`
	Status         string                     `json:"status"`
	IdempotencyKey string                     `json:"idempotency_key"`
	YAML           string                     `json:"yaml"`
	Preview        renderTemplateDraftPreview `json:"preview"`
	NextAction     string                     `json:"next_action"`
}

func NewRenderTemplateDraftTools(workspaceID, agentName string, audit AuditRecorder) *RenderTemplateDraftToolsDispatcher {
	return &RenderTemplateDraftToolsDispatcher{workspaceID: workspaceID, agent: agentName, audit: audit}
}

func (d *RenderTemplateDraftToolsDispatcher) ListTools(context.Context) ([]contract.ToolDef, error) {
	if d == nil || strings.TrimSpace(d.workspaceID) == "" || strings.TrimSpace(d.agent) == "" {
		return nil, nil
	}
	return []contract.ToolDef{{
		Name: ToolRenderTemplateDraft,
		Description: "校验完整 team-template/v1 YAML 草稿并渲染 Console 审阅预览。只验证和规范化草稿，不创建 BuildRun、不写入 Agent/Team/Workflow；create 分支完成发现后必须调用本工具，成功后等待用户在 Console 一键提交。" +
			" Validate a complete team-template/v1 YAML draft and render its Console review preview. This never creates or mutates platform assets.",
		InputSchema: renderTemplateDraftInputSchema,
	}}, nil
}

func (d *RenderTemplateDraftToolsDispatcher) Dispatch(ctx context.Context, call contract.ToolCall) (result *contract.ToolResult, err error) {
	defer func() {
		if d != nil {
			recordAudit(ctx, d.audit, d.workspaceID, d.agent, call, result, err, "")
		}
	}()
	if call.Name != ToolRenderTemplateDraft {
		return toolError(call.ID, fmt.Sprintf("unknown tool %q", call.Name)), nil
	}
	if d == nil || strings.TrimSpace(d.workspaceID) == "" || strings.TrimSpace(d.agent) == "" {
		return toolError(call.ID, "tf_render_template_draft is unavailable"), nil
	}
	var args renderTemplateDraftArgs
	if err := strictRaw(json.RawMessage(call.Args), &args); err != nil {
		return toolError(call.ID, "invalid tf_render_template_draft arguments: "+err.Error()), nil
	}
	compilation, err := teamtemplate.CompileYAML([]byte(args.YAML))
	if err != nil {
		var validation *teamtemplate.ValidationError
		if errors.As(err, &validation) {
			payload, marshalErr := json.Marshal(map[string]any{
				"code": "template_draft_invalid", "problems": validation.Problems,
				"guidance": "修正列出的全部字段问题后重新调用 tf_render_template_draft；不要改走 tf_submit_brief 或 tf_blueprint_plan。",
			})
			if marshalErr == nil {
				return toolError(call.ID, string(payload)), nil
			}
		}
		return toolError(call.ID, err.Error()), nil
	}
	rendered, err := yaml.Marshal(compilation.Template)
	if err != nil {
		return toolError(call.ID, "render normalized template draft: "+err.Error()), nil
	}
	members := make([]renderTemplateDraftMember, 0, len(compilation.Template.Members))
	for _, member := range compilation.Template.Members {
		members = append(members, renderTemplateDraftMember{
			Name: member.Name, DisplayName: member.DisplayName, Role: member.Role,
		})
	}
	return toolJSON(call.ID, renderTemplateDraftResult{
		SchemaVersion:  1,
		Status:         "ready_for_review",
		IdempotencyKey: uuid.NewString(),
		YAML:           string(rendered),
		Preview: renderTemplateDraftPreview{
			Name: compilation.Template.Name, DisplayName: compilation.Template.DisplayName,
			Purpose: compilation.Template.Purpose, Topology: compilation.Template.Template,
			Lead: compilation.Template.Lead, Members: members,
			SuccessCriteria: append([]string(nil), compilation.Template.Delivery.SuccessCriteria...),
			MaxCostUSD:      compilation.Template.Budget.MaxCostUSD,
		},
		NextAction: "review_and_submit_from_console",
	})
}
