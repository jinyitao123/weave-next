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
	"github.com/jinyitao123/weave/internal/base/capability"
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
	Name        string `json:"name"`
	RoleIndex   int    `json:"role_index"`
	Kind        string `json:"kind"`
	Instruction string `json:"instruction"`
	DependsOn   []int  `json:"depends_on"`
	UsesInput   bool   `json:"uses_input"`
	UsesSteps   []int  `json:"uses_steps"`
}

type generatedCapabilityProposal struct {
	Name         string                     `json:"name"`
	Description  string                     `json:"description"`
	InputFields  []generatedCapabilityField `json:"input_fields"`
	OutputFields []generatedCapabilityField `json:"output_fields"`
	Roles        []generatedCapabilityRole  `json:"roles"`
	Steps        []generatedCapabilityStep  `json:"steps"`
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
    "steps":{"type":"array","minItems":1,"maxItems":24,"items":{"type":"object","additionalProperties":false,"required":["name","role_index","kind","instruction","depends_on","uses_input","uses_steps"],"properties":{"name":{"type":"string","minLength":1,"maxLength":160},"role_index":{"type":"integer","minimum":0,"maximum":7},"kind":{"enum":["worker","collect"]},"instruction":{"type":"string","maxLength":4000},"depends_on":{"type":"array","maxItems":23,"items":{"type":"integer","minimum":0,"maximum":23}},"uses_input":{"type":"boolean"},"uses_steps":{"type":"array","maxItems":23,"items":{"type":"integer","minimum":0,"maximum":23}}}}}
  },
  "$defs":{"field":{"type":"object","additionalProperties":false,"required":["key","label","description","type","required"],"properties":{"key":{"type":"string","minLength":1,"maxLength":80},"label":{"type":"string","minLength":1,"maxLength":160},"description":{"type":"string","maxLength":500},"type":{"enum":["string","number","integer","boolean","object","array"]},"required":{"type":"boolean"}}}}
}`)

const capabilityGenerationSystemPrompt = `你是企业能力设计师。把用户描述转换为可执行的多角色业务能力方案。
只设计当前可运行的串行、并行和汇合步骤。不得设计条件分支、循环、人工等待、工具调用或外部凭据。
角色写清职责，步骤写清可直接执行的任务。worker 步骤必须有完整 instruction；collect 步骤只汇总已有结果且 instruction 为空。
depends_on 和 uses_steps 只能引用当前步骤之前的下标。控制依赖和数据来源分别填写，不得因为使用原始输入而虚构步骤依赖。
优先使用 2 到 6 个角色和 2 到 12 个步骤；只有需求确实简单时才减少。字段名使用稳定的英文 snake_case，字段标签和说明使用用户语言。
输出必须严格满足给定结构。`

func (s *Server) handleGenerateCapability(c echo.Context) error {
	if s.Models == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"code": "capability_generation_unavailable"})
	}
	var request generateCapabilityRequest
	if err := decodeCapabilityBody(c, &request); err != nil || strings.TrimSpace(request.Prompt) == "" || len(request.Prompt) > 12000 {
		return c.JSON(http.StatusBadRequest, map[string]string{"code": "capability_request_invalid"})
	}
	workspaceID, _ := c.Get("tenant").(string)
	if err := s.validateCapabilityRuntime(c.Request().Context(), workspaceID, capability.RuntimeRequirement{Engine: "loom", Model: request.Model}); err != nil {
		return c.JSON(http.StatusUnprocessableEntity, map[string]string{"code": "capability_runtime_unavailable"})
	}
	llm, err := s.Models.ForWorkspace(c.Request().Context(), workspaceID)
	if err != nil {
		return capabilityHTTPError(c, err)
	}
	temperature := 0.2
	generationContext, cancel := context.WithTimeout(c.Request().Context(), 90*time.Second)
	defer cancel()
	response, err := llm.Chat(generationContext, contract.ChatRequest{
		Model: request.Model,
		Messages: []contract.Message{
			{Role: "system", Content: capabilityGenerationSystemPrompt},
			{Role: "user", Content: request.Prompt},
		},
		Schema: &capabilityGenerationSchema, MaxTokens: 5000, Temperature: &temperature,
	})
	if err != nil {
		return c.JSON(http.StatusBadGateway, map[string]string{"code": "capability_generation_failed"})
	}
	if response == nil {
		return c.JSON(http.StatusBadGateway, map[string]string{"code": "capability_generation_failed"})
	}
	var proposal generatedCapabilityProposal
	if err := decodeGeneratedCapability(response.Content, &proposal); err != nil {
		return c.JSON(http.StatusUnprocessableEntity, map[string]string{"code": "capability_generation_invalid"})
	}
	definition, err := buildGeneratedCapability(proposal, request.Model)
	if err != nil {
		return c.JSON(http.StatusUnprocessableEntity, map[string]string{"code": "capability_generation_invalid"})
	}
	return c.JSON(http.StatusOK, map[string]any{"definition": definition})
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
		if generated.Kind == "collect" {
			kind, instruction = capability.StepTransform, ""
		} else if generated.Kind != "worker" || instruction == "" {
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
			kind := capability.RelationSequence
			if len(dependencies) > 1 {
				kind = capability.RelationJoin
			}
			relations = append(relations, capability.Relation{From: stepIDs[dependency], To: stepIDs[index], Kind: kind})
		}
		steps[index] = capability.Step{ID: stepIDs[index], Name: strings.TrimSpace(generated.Name), RoleID: roles[generated.RoleIndex].ID, Kind: kind, Instruction: instruction, InputBindings: bindings}
	}
	inputSchema, err := generatedObjectSchema(proposal.InputFields)
	if err != nil {
		return capability.Definition{}, err
	}
	outputSchema, err := generatedObjectSchema(proposal.OutputFields)
	if err != nil {
		return capability.Definition{}, err
	}
	definition := capability.Definition{
		SchemaVersion: capability.SchemaVersionV1, CapabilityID: uuid.NewString(),
		Name: strings.TrimSpace(proposal.Name), Description: strings.TrimSpace(proposal.Description),
		InputSchema: inputSchema, OutputSchema: outputSchema, Roles: roles, Steps: steps, Relations: relations,
		Runtime: capability.RuntimeRequirement{Engine: "loom", Model: model}, Resources: capability.ResourceRequirement{},
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
