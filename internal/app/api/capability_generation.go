package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jinyitao123/loom/contract"
	"github.com/jinyitao123/weave/internal/base/execution"
	"github.com/jinyitao123/weave/internal/kernel/capability"
	"github.com/jinyitao123/weave/internal/kernel/mcphost"
	"github.com/labstack/echo/v4"
)

type generateCapabilityRequest struct {
	Prompt string `json:"prompt"`
	Model  string `json:"model"`
}

type generatedCapabilityField struct {
	Key         string `json:"key"`
	Label       string `json:"label"`
	Description string `json:"description"`
	Type        string `json:"type"`
	Required    bool   `json:"required"`
}

type generatedCapabilityRole struct {
	Name             string `json:"name"`
	Responsibilities string `json:"responsibilities"`
}

type generatedCapabilityStep struct {
	Name           string `json:"name"`
	RoleIndex      int    `json:"role_index"`
	Kind           string `json:"kind"`
	Instruction    string `json:"instruction"`
	DependsOn      []int  `json:"depends_on"`
	UsesInput      bool   `json:"uses_input"`
	UsesSteps      []int  `json:"uses_steps"`
	BranchWhen     *bool  `json:"branch_when"`
	ConditionPath  string `json:"condition_path"`
	ConditionOp    string `json:"condition_operator"`
	ConditionValue any    `json:"condition_value"`
	ToolID         string `json:"tool_id"`
	MCPServerID    string `json:"mcp_server_id"`
	ApprovalTitle  string `json:"approval_title"`
	LoopTo         *int   `json:"loop_to"`
	MaxIterations  int    `json:"max_iterations"`
}

type generatedCapabilityProposal struct {
	Name         string                     `json:"name"`
	Description  string                     `json:"description"`
	InputFields  []generatedCapabilityField `json:"input_fields"`
	OutputFields []generatedCapabilityField `json:"output_fields"`
	Roles        []generatedCapabilityRole  `json:"roles"`
	Steps        []generatedCapabilityStep  `json:"steps"`
}

type capabilityGenerationTool struct {
	MCPServerID string          `json:"mcp_server_id"`
	ToolName    string          `json:"tool_name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema"`
	ReadOnly    bool            `json:"read_only"`
}

var capabilityGenerationSchema = json.RawMessage(`{
  "type":"object","additionalProperties":false,
  "required":["name","description","input_fields","output_fields","roles","steps"],
  "properties":{
    "name":{"type":"string","minLength":1,"maxLength":160},
    "description":{"type":"string","minLength":1,"maxLength":1000},
    "input_fields":{"type":"array","maxItems":24,"items":{"$ref":"#/$defs/field"}},
    "output_fields":{"type":"array","maxItems":24,"items":{"$ref":"#/$defs/field"}},
    "roles":{"type":"array","minItems":1,"maxItems":8,"items":{"type":"object","additionalProperties":false,"required":["name","responsibilities"],"properties":{"name":{"type":"string","minLength":1,"maxLength":120},"responsibilities":{"type":"string","minLength":1,"maxLength":1000}}}},
    "steps":{"type":"array","minItems":1,"maxItems":24,"items":{"type":"object","additionalProperties":false,"required":["name","role_index","kind","instruction","depends_on","uses_input","uses_steps","branch_when","condition_path","condition_operator","condition_value","tool_id","mcp_server_id","approval_title","loop_to","max_iterations"],"properties":{"name":{"type":"string","minLength":1,"maxLength":160},"role_index":{"type":"integer","minimum":0,"maximum":7},"kind":{"enum":["worker","collect","condition","approval","tool"]},"instruction":{"type":"string","maxLength":4000},"depends_on":{"type":"array","maxItems":23,"items":{"type":"integer","minimum":0,"maximum":23}},"uses_input":{"type":"boolean"},"uses_steps":{"type":"array","maxItems":23,"items":{"type":"integer","minimum":0,"maximum":23}},"branch_when":{"type":["boolean","null"]},"condition_path":{"type":"string"},"condition_operator":{"enum":["eq","ne","gt","gte","lt","lte","truthy","empty",""]},"condition_value":{},"tool_id":{"type":"string"},"mcp_server_id":{"type":"string"},"approval_title":{"type":"string"},"loop_to":{"type":["integer","null"],"minimum":0,"maximum":23},"max_iterations":{"type":"integer","minimum":0,"maximum":20}}}}}
  },
  "$defs":{"field":{"type":"object","additionalProperties":false,"required":["key","label","description","type","required"],"properties":{"key":{"type":"string","minLength":1,"maxLength":80},"label":{"type":"string","minLength":1,"maxLength":160},"description":{"type":"string","maxLength":500},"type":{"enum":["string","number","integer","boolean","object","array"]},"required":{"type":"boolean"}}}}
}`)

const capabilityGenerationSystemPrompt = `你是企业能力与团队流程设计师。把用户描述转换为可执行的多角色业务能力方案。
可用步骤为 worker、collect、condition、approval、tool。worker 负责需要判断、创作、研究或操作环境的工作；collect 只整理绑定值；condition 使用 condition_path 和 operator 做确定性判断；approval 表示必须由人确认后才能继续；tool 调用已登记工具，必须填写准确的 mcp_server_id 和 tool_id。不得只凭裸工具名猜测服务或权限。
branch_when 只用于当前步骤依赖 condition 时选择 true 或 false 分支，否则为 null。loop_to 为 null 表示不循环；需要循环时由 condition 步骤指回更早步骤，并给出 1 到 20 的 max_iterations。循环必须有明确停止条件。
最后一步必须是 worker，负责把前面结果整理为完整业务交付，并严格返回 output_fields。不得把内部流程状态作为最终结果。
depends_on 和 uses_steps 只能引用当前步骤之前的下标。控制依赖和数据来源分别填写。工具和外部访问只在业务确实需要时使用，不得声称已经取得尚未执行的结果。
按实际任务选取最少必要角色和步骤，确定性任务允许只有一个 worker。在生成方案时判断执行路径是否明确，不要额外创建通用分类步骤或模型调用。固定计算、格式转换、模板填充和文档明确的接口调用直接执行；外部访问、权限要求或副作用风险本身不是方法不确定。只有具体未知阻止选择可执行路径时才安排方法探索；混合任务只探索未知部分，已知且独立的步骤不等待探索。探索指令写明未知问题、所需证据、允许的备选方法、约定预算内的尝试上限和停止条件；可复用的已验证方法不对每条数据重复研究。遵守用户禁止联网或研究的约束，输入错误、权限不足和临时故障分别走校验、补充权限或有限重试，不自动升级研究。只有证据表明原方法失效且授权允许时才重新研究；不得静默扩大工具、预算或业务范围。真实人工确认使用 approval，实际条件和循环使用相应控制步骤，不能只写在角色名称或说明里。字段名使用稳定的英文 snake_case，字段标签和说明使用用户语言。输出必须严格满足给定结构。`

func (s *Server) handleGenerateCapability(c echo.Context) error {
	var request generateCapabilityRequest
	if err := decodeCapabilityBody(c, &request); err != nil || strings.TrimSpace(request.Prompt) == "" || len(request.Prompt) > 12000 {
		return c.JSON(http.StatusBadRequest, map[string]string{"code": "capability_request_invalid"})
	}
	workspaceID, _ := c.Get("tenant").(string)
	generationContext, cancel := context.WithTimeout(c.Request().Context(), 5*time.Minute)
	defer cancel()
	systemPrompt := s.capabilityGenerationPrompt(generationContext, workspaceID)
	var content string
	usedRemote := false
	if s.Models != nil && request.Model != "" {
		if available, _ := s.Models.CanResolve(generationContext, workspaceID, request.Model); available {
			llm, err := s.Models.ForWorkspace(generationContext, workspaceID)
			if err != nil {
				return capabilityHTTPError(c, err)
			}
			temperature := 0.2
			response, err := llm.Chat(generationContext, contract.ChatRequest{
				Model: request.Model,
				Messages: []contract.Message{
					{Role: "system", Content: systemPrompt},
					{Role: "user", Content: request.Prompt},
				},
				Schema: &capabilityGenerationSchema, MaxTokens: 5000, Temperature: &temperature,
			})
			if err != nil || response == nil {
				return c.JSON(http.StatusBadGateway, map[string]string{"code": "capability_generation_failed"})
			}
			content = response.Content
		}
	}
	if content == "" {
		if s.Runtimes == nil || s.engineExecutorFor(true) == nil {
			return c.JSON(http.StatusUnprocessableEntity, map[string]string{"code": "capability_runtime_unavailable"})
		}
		record, err := s.capabilityRunner().SelectRuntime(generationContext, workspaceID, capability.RuntimeRequirement{Engine: "codex"})
		if err != nil {
			return c.JSON(http.StatusUnprocessableEntity, map[string]string{"code": "capability_runtime_unavailable"})
		}
		structured, ok := s.engineExecutorFor(true).(mcphost.StructuredRemoteEngineExecutor)
		if !ok {
			return c.JSON(http.StatusUnprocessableEntity, map[string]string{"code": "capability_runtime_unavailable"})
		}
		record.Spec.SystemPrompt = systemPrompt
		record.OutputSchema = &capabilityGenerationSchema
		result, err := structured.ExecRemoteStructured(generationContext, workspaceID, record, execution.AgentExecutionStamp{
			AgentID: record.ID, AgentVersion: record.Version, ExecutionScope: execution.ScopeTeamWorkerLeaf, RunSnapshotID: "capability-plan-" + uuid.NewString(),
		}, request.Prompt, nil, capabilityGenerationSchema)
		if err != nil {
			return c.JSON(http.StatusBadGateway, map[string]string{"code": "capability_generation_failed"})
		}
		content = result.Output
		usedRemote = true
	}
	var proposal generatedCapabilityProposal
	if err := decodeGeneratedCapability(content, &proposal); err != nil {
		return c.JSON(http.StatusUnprocessableEntity, map[string]string{"code": "capability_generation_invalid"})
	}
	definition, err := buildGeneratedCapability(proposal, request.Model)
	if err != nil {
		return c.JSON(http.StatusUnprocessableEntity, map[string]string{"code": "capability_generation_invalid"})
	}
	if usedRemote {
		definition.Runtime = capability.RuntimeRequirement{Engine: "codex"}
	}
	return c.JSON(http.StatusOK, map[string]any{"definition": definition})
}

func (s *Server) capabilityGenerationPrompt(ctx context.Context, workspaceID string) string {
	available := []capabilityGenerationTool{}
	if s.MCPRegistry != nil {
		servers, err := s.MCPRegistry.ListMetadata(ctx, workspaceID)
		if err == nil {
			for _, server := range servers {
				if !server.Enabled || server.RevokedAt != nil || server.DeletedAt != nil || server.LastHandshakeAt == nil || server.Transport != "streamable_http" {
					continue
				}
				catalog, catalogErr := s.MCPRegistry.Catalog(ctx, workspaceID, server.ID)
				if catalogErr != nil {
					continue
				}
				for _, tool := range catalog.Tools {
					available = append(available, capabilityGenerationTool{MCPServerID: server.ID, ToolName: tool.Name,
						Description: tool.Description, InputSchema: tool.InputSchema, ReadOnly: tool.ReadOnlyHint != nil && *tool.ReadOnlyHint})
				}
			}
		}
	}
	raw, _ := json.Marshal(available)
	return capabilityGenerationSystemPrompt + "\n当前工作区可直接使用的工具如下：" + string(raw) +
		"\n只有确实需要且出现在该列表中的工具才可生成 tool 步骤；列表为空时不得生成 tool 步骤。原样复制 mcp_server_id 和 tool_name，tool_id 必须等于 tool_name。"
}

func decodeGeneratedCapability(content string, target *generatedCapabilityProposal) error {
	content = strings.TrimSpace(content)
	start, end := strings.IndexByte(content, '{'), strings.LastIndexByte(content, '}')
	if start < 0 || end < start {
		return errors.New("generated capability is not an object")
	}
	decoder := json.NewDecoder(strings.NewReader(content[start : end+1]))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if decoder.Decode(&struct{}{}) == nil {
		return errors.New("generated capability contains trailing data")
	}
	return nil
}

func buildGeneratedCapability(proposal generatedCapabilityProposal, model string) (capability.Definition, error) {
	if strings.TrimSpace(proposal.Name) == "" || strings.TrimSpace(proposal.Description) == "" || len(proposal.Roles) == 0 || len(proposal.Roles) > 8 || len(proposal.Steps) == 0 || len(proposal.Steps) > 24 || len(proposal.InputFields) > 24 || len(proposal.OutputFields) > 24 {
		return capability.Definition{}, capability.ErrInvalidDefinition
	}
	roles := make([]capability.Role, len(proposal.Roles))
	for index, role := range proposal.Roles {
		if strings.TrimSpace(role.Name) == "" || strings.TrimSpace(role.Responsibilities) == "" {
			return capability.Definition{}, capability.ErrInvalidDefinition
		}
		roles[index] = capability.Role{ID: "role-" + uuid.NewString(), Name: strings.TrimSpace(role.Name), Description: strings.TrimSpace(role.Responsibilities)}
	}
	steps := make([]capability.Step, len(proposal.Steps))
	stepIDs := make([]string, len(proposal.Steps))
	for index := range stepIDs {
		stepIDs[index] = "step-" + uuid.NewString()
	}
	relations := make([]capability.Relation, 0)
	for index, generated := range proposal.Steps {
		if generated.RoleIndex < 0 || generated.RoleIndex >= len(roles) || strings.TrimSpace(generated.Name) == "" {
			return capability.Definition{}, capability.ErrInvalidDefinition
		}
		kind := capability.StepWorker
		instruction := strings.TrimSpace(generated.Instruction)
		var condition *capability.Predicate
		switch generated.Kind {
		case "worker":
			if instruction == "" {
				return capability.Definition{}, capability.ErrInvalidDefinition
			}
		case "collect":
			kind, instruction = capability.StepTransform, ""
		case "approval":
			kind, instruction = capability.StepWait, ""
			if strings.TrimSpace(generated.ApprovalTitle) == "" {
				return capability.Definition{}, capability.ErrInvalidDefinition
			}
		case "tool":
			kind, instruction = capability.StepTool, ""
			if strings.TrimSpace(generated.ToolID) == "" {
				return capability.Definition{}, capability.ErrInvalidDefinition
			}
		case "condition":
			kind, instruction = capability.StepCondition, ""
			left := capability.ValueRef{Source: "input", Path: generated.ConditionPath}
			condition = &capability.Predicate{Left: left, Operator: generated.ConditionOp}
			if generated.ConditionOp != "truthy" && generated.ConditionOp != "empty" {
				raw, err := json.Marshal(generated.ConditionValue)
				if err != nil {
					return capability.Definition{}, err
				}
				right := capability.ValueRef{Source: "literal", Literal: raw}
				condition.Right = &right
			}
		default:
			return capability.Definition{}, capability.ErrInvalidDefinition
		}
		dependencies := uniqueEarlierIndexes(generated.DependsOn, index)
		bindings := map[string]capability.ValueRef{}
		if generated.UsesInput || index == 0 {
			bindings["原始材料"] = capability.ValueRef{Source: "input"}
		}
		uses := uniqueEarlierIndexes(generated.UsesSteps, index)
		for _, dependency := range dependencies {
			if !containsIndex(uses, dependency) {
				uses = append(uses, dependency)
			}
		}
		for _, source := range uses {
			bindings[proposal.Steps[source].Name] = capability.ValueRef{Source: "step_output", StepID: stepIDs[source]}
			if !containsIndex(dependencies, source) {
				dependencies = append(dependencies, source)
			}
		}
		for _, dependency := range dependencies {
			relationKind := capability.RelationSequence
			if proposal.Steps[dependency].Kind == "condition" && generated.BranchWhen != nil {
				relationKind = capability.RelationCondition
			} else if len(dependencies) > 1 {
				relationKind = capability.RelationJoin
			}
			relations = append(relations, capability.Relation{From: stepIDs[dependency], To: stepIDs[index], Kind: relationKind, When: generated.BranchWhen})
		}
		steps[index] = capability.Step{ID: stepIDs[index], Name: strings.TrimSpace(generated.Name), RoleID: roles[generated.RoleIndex].ID, Kind: kind, Instruction: instruction, InputBindings: bindings, Condition: condition, ToolID: strings.TrimSpace(generated.ToolID), ApprovalTitle: strings.TrimSpace(generated.ApprovalTitle)}
		if generated.LoopTo != nil {
			if kind != capability.StepCondition || *generated.LoopTo >= index || generated.MaxIterations < 1 {
				return capability.Definition{}, capability.ErrInvalidDefinition
			}
			steps[*generated.LoopTo].MaxIterations = generated.MaxIterations
			relations = append(relations, capability.Relation{From: stepIDs[index], To: stepIDs[*generated.LoopTo], Kind: capability.RelationLoop})
		}
	}
	inputSchema, err := generatedObjectSchema(proposal.InputFields)
	if err != nil {
		return capability.Definition{}, err
	}
	outputSchema, err := generatedObjectSchema(proposal.OutputFields)
	if err != nil {
		return capability.Definition{}, err
	}
	if len(steps) == 0 || steps[len(steps)-1].Kind != capability.StepWorker {
		return capability.Definition{}, capability.ErrInvalidDefinition
	}
	steps[len(steps)-1].OutputSchema = outputSchema
	toolIDs := []string{}
	toolRefs := []capability.ToolReference{}
	for _, step := range steps {
		if step.Kind == capability.StepTool && !containsStringValue(toolIDs, step.ToolID) {
			toolIDs = append(toolIDs, step.ToolID)
		}
	}
	for _, generated := range proposal.Steps {
		if generated.Kind == "tool" {
			serverID, toolID := strings.TrimSpace(generated.MCPServerID), strings.TrimSpace(generated.ToolID)
			if serverID == "" || toolID == "" {
				return capability.Definition{}, capability.ErrInvalidDefinition
			}
			duplicate := false
			for _, ref := range toolRefs {
				if ref.ToolName == toolID {
					if ref.MCPServerID != serverID {
						return capability.Definition{}, capability.ErrInvalidDefinition
					}
					duplicate = true
				}
			}
			if !duplicate {
				toolRefs = append(toolRefs, capability.ToolReference{MCPServerID: serverID, ToolName: toolID})
			}
		}
	}
	runtime := capability.RuntimeRequirement{Engine: "loom", Model: model}
	if len(toolIDs) > 0 {
		runtime = capability.RuntimeRequirement{Engine: "codex"}
	}
	definition := capability.Definition{
		SchemaVersion: capability.SchemaVersionV1, CapabilityID: uuid.NewString(),
		Name: strings.TrimSpace(proposal.Name), Description: strings.TrimSpace(proposal.Description),
		InputSchema: inputSchema, OutputSchema: outputSchema, Roles: roles, Steps: steps, Relations: relations,
		Runtime: runtime, Resources: capability.ResourceRequirement{ToolIDs: toolIDs, Tools: toolRefs},
		Result: &capability.ValueRef{Source: "step_output", StepID: steps[len(steps)-1].ID},
	}
	published, err := capability.Publish(definition, 1)
	if err != nil {
		return capability.Definition{}, fmt.Errorf("validate generated capability: %w", err)
	}
	if _, err := capability.Compile(published); err != nil {
		return capability.Definition{}, fmt.Errorf("compile generated capability: %w", err)
	}
	return definition, nil
}

func uniqueEarlierIndexes(values []int, current int) []int {
	seen := map[int]bool{}
	result := make([]int, 0, len(values))
	for _, value := range values {
		if value >= 0 && value < current && !seen[value] {
			seen[value] = true
			result = append(result, value)
		}
	}
	return result
}

func containsIndex(values []int, target int) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

var generatedFieldKey = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]{0,79}$`)

func generatedObjectSchema(fields []generatedCapabilityField) (json.RawMessage, error) {
	properties := map[string]any{}
	required := []string{}
	for index, field := range fields {
		key := strings.TrimSpace(field.Key)
		if key == "" {
			key = fmt.Sprintf("field_%d", index+1)
		}
		if !generatedFieldKey.MatchString(key) || strings.TrimSpace(field.Label) == "" {
			return nil, capability.ErrInvalidDefinition
		}
		if _, exists := properties[key]; exists {
			return nil, capability.ErrInvalidDefinition
		}
		typeName := field.Type
		switch typeName {
		case "string", "number", "integer", "boolean", "object", "array":
		default:
			return nil, capability.ErrInvalidDefinition
		}
		property := map[string]any{"type": typeName, "title": strings.TrimSpace(field.Label)}
		if field.Description != "" {
			property["description"] = strings.TrimSpace(field.Description)
		}
		if typeName == "object" {
			property["properties"] = map[string]any{}
		}
		if typeName == "array" {
			property["items"] = map[string]any{"type": "string"}
		}
		properties[key] = property
		if field.Required {
			required = append(required, key)
		}
	}
	return json.Marshal(map[string]any{"type": "object", "properties": properties, "required": required})
}

func containsStringValue(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}
