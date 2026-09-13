package teamforge

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/jinyitao123/loom/contract"
)

// ToolWorkflowBlueprintBuild compiles and commits a small workflow blueprint.
// It is the default workflow construction surface for the meta-team.
const ToolWorkflowBlueprintBuild = "tf_wf_blueprint_build"

const (
	workflowBlueprintPhaseSchema    = "schema"
	workflowBlueprintPhaseBlueprint = "blueprint"
	workflowBlueprintPhaseValidate  = "validate"
)

type workflowBlueprintBuildInput struct {
	BuildRunID string               `json:"build_run_id"`
	WorkflowID string               `json:"workflow_id"`
	Create     *workflowBeginCreate `json:"create,omitempty"`
	Blueprint  WorkflowBlueprint    `json:"blueprint"`
}

type workflowBlueprintBuildResult struct {
	BuildRunID string                    `json:"build_run_id"`
	WorkflowID string                    `json:"workflow_id"`
	Tool       string                    `json:"tool"`
	BuildMode  string                    `json:"build_mode"`
	Template   WorkflowBlueprintTemplate `json:"template"`
	Committed  bool                      `json:"committed"`
	Version    int                       `json:"version"`
	UpdatedAt  time.Time                 `json:"updated_at"`
	NodeCount  int                       `json:"node_count"`
	EdgeCount  int                       `json:"edge_count"`
	Warnings   []WorkflowProblem         `json:"warnings"`
}

type workflowBlueprintBuildFailure struct {
	BuildRunID string                    `json:"build_run_id"`
	WorkflowID string                    `json:"workflow_id"`
	Tool       string                    `json:"tool"`
	BuildMode  string                    `json:"build_mode"`
	Template   WorkflowBlueprintTemplate `json:"template,omitempty"`
	Committed  bool                      `json:"committed"`
	Phase      string                    `json:"phase"`
	Errors     []WorkflowProblem         `json:"errors"`
	Warnings   []WorkflowProblem         `json:"warnings"`
}

func (d *WorkflowWriteToolsDispatcher) workflowBlueprintBuild(
	ctx context.Context,
	call contract.ToolCall,
) (*contract.ToolResult, error) {
	var input workflowBlueprintBuildInput
	if err := strictRaw(json.RawMessage(call.Args), &input); err != nil {
		return toolError(call.ID, workflowBlueprintFailureContent(
			input, workflowBlueprintPhaseSchema,
			[]WorkflowProblem{{Path: "/", Code: "blueprint_input_invalid", Message: err.Error(), Hint: "按工具 schema 修复未知或类型错误字段后重试。"}},
			nil,
		)), nil
	}
	input.WorkflowID = strings.TrimSpace(input.WorkflowID)
	if input.WorkflowID == "" {
		return toolError(call.ID, workflowBlueprintFailureContent(
			input, workflowBlueprintPhaseSchema,
			[]WorkflowProblem{{Path: "/workflow_id", Code: "blueprint_workflow_id_required", Message: "workflow_id is required", Hint: "使用授权范围内的精确 workflow id。"}},
			nil,
		)), nil
	}

	runID, err := d.gate.resolveBuildRunID(call.Args)
	if err != nil {
		return toolError(call.ID, err.Error()), nil
	}
	input.BuildRunID = runID
	if err := d.gateCheck(ctx, call, input.WorkflowID); err != nil {
		return toolError(call.ID, err.Error()), nil
	}

	compiled, blueprintProblems := CompileWorkflowBlueprint(input.Blueprint)
	if len(blueprintProblems) != 0 {
		problems := make([]WorkflowProblem, 0, len(blueprintProblems))
		for _, problem := range blueprintProblems {
			problems = append(problems, WorkflowProblem{
				Path: problem.Path, Code: problem.Code, Message: problem.Message, Hint: problem.Hint,
			})
		}
		return toolError(call.ID, workflowBlueprintFailureContent(
			input, workflowBlueprintPhaseBlueprint, problems, nil,
		)), nil
	}

	carrier, err := d.openWorkflowBuildCarrier(ctx, input.BuildRunID, input.WorkflowID, input.Create)
	if err != nil {
		return toolError(call.ID, err.Error()), nil
	}
	working := cloneWorkflowDraft(carrier)
	working.Trigger = compiled.Trigger
	working.Graph = compiled.Graph

	validation, err := d.validateWorkflowDraft(ctx, working)
	if err != nil {
		return toolError(call.ID, err.Error()), nil
	}
	warnings := append([]WorkflowProblem(nil), validation.Warnings...)
	warnings = append(warnings, workflowBuildReachabilityWarnings(working.Graph)...)
	if !validation.Valid {
		return toolError(call.ID, workflowBlueprintFailureContent(
			input, workflowBlueprintPhaseValidate, validation.Errors, warnings,
		)), nil
	}

	result := d.commitWorkflowDraft(ctx, call, &workflowCallInput{
		BuildRunID: input.BuildRunID,
		WorkflowID: input.WorkflowID,
	}, working)
	if result.IsError {
		return result, nil
	}
	var committed workflowCommitJSON
	if err := json.Unmarshal([]byte(result.Content), &committed); err != nil {
		return toolError(call.ID, ToolWorkflowBlueprintBuild+" commit result decode failed: "+err.Error()), nil
	}
	return toolJSON(call.ID, workflowBlueprintBuildResult{
		BuildRunID: input.BuildRunID, WorkflowID: input.WorkflowID,
		Tool: ToolWorkflowBlueprintBuild, BuildMode: "template", Template: input.Blueprint.Template,
		Committed: true, Version: committed.Version, UpdatedAt: committed.UpdatedAt,
		NodeCount: len(working.Graph.Nodes), EdgeCount: len(working.Graph.Edges), Warnings: warnings,
	})
}

func workflowBlueprintFailureContent(
	input workflowBlueprintBuildInput,
	phase string,
	problems, warnings []WorkflowProblem,
) string {
	payload := workflowBlueprintBuildFailure{
		BuildRunID: input.BuildRunID, WorkflowID: input.WorkflowID,
		Tool: ToolWorkflowBlueprintBuild, BuildMode: "template", Template: input.Blueprint.Template,
		Committed: false, Phase: phase, Errors: problems, Warnings: warnings,
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return fmt.Sprintf("%s failed (%s): %v", ToolWorkflowBlueprintBuild, phase, err)
	}
	return string(data)
}

var workflowBlueprintBuildInputSchema = mustSchemaJSON(
	ToolWorkflowBlueprintBuild,
	workflowBlueprintBuildInputSchemaDoc(),
)

func workflowBlueprintBuildInputSchemaDoc() map[string]any {
	worker := schemaObject(map[string]any{
		"agent_id": map[string]any{
			"type": "string", "description": "tf_list_agents 返回的真实 agent id",
		},
		"agent_version": map[string]any{
			"type": "integer", "description": "tf_get_agent_version 核实的精确正整数版本",
		},
		"result_requirement": map[string]any{
			"type": "string", "description": "该成员必须返回的结果和完成标准",
		},
	}, []string{"agent_id", "agent_version", "result_requirement"})
	blueprint := schemaObject(map[string]any{
		"delivery_contract": deliveryContractSchema(),
		"template": map[string]any{
			"type": "string",
			"enum": []string{
				string(WorkflowBlueprintDeliveryRework),
				string(WorkflowBlueprintParallelReview),
				string(WorkflowBlueprintCreativeRework),
				string(WorkflowBlueprintResearchSummary),
			},
			"description": "返修模板使用 primary+reviewer+max_iterations；汇总模板使用 parallel_workers+finalizer",
		},
		"lead_instruction": map[string]any{
			"type": "string", "description": "负责人如何拆解、协调和验收任务",
		},
		"primary":  worker,
		"reviewer": worker,
		"parallel_workers": map[string]any{
			"type": "array", "items": worker,
			"description": "仅汇总模板；至少两个并行成员",
		},
		"finalizer": map[string]any{
			"type": "object", "properties": worker["properties"], "required": worker["required"], "additionalProperties": false,
		},
		"max_iterations": map[string]any{
			"type": "integer", "description": "仅返修模板；1 到 5",
		},
	}, []string{"template", "lead_instruction"})
	return schemaObject(map[string]any{
		"build_run_id": buildRunIDSchema("可选；省略时绑定当前授权 run"),
		"workflow_id": map[string]any{
			"type": "string", "description": "授权范围内的目标 workflow id",
		},
		"create": schemaObject(map[string]any{
			"name": map[string]any{"type": "string"},
			"team_id": map[string]any{
				"type": "string", "description": "归属 team id",
			},
			"description": map[string]any{"type": "string"},
		}, []string{"name", "team_id"}),
		"blueprint": blueprint,
	}, []string{"workflow_id", "blueprint"})
}
