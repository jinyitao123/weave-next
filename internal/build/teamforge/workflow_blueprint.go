package teamforge

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/jinyitao123/weave/internal/base/deliverable"
	"github.com/jinyitao123/weave/internal/kernel/workflow/machine"
)

// WorkflowBlueprintTemplate names the small, stable workflow shapes exposed
// to the meta-team. Templates compile into the machine graph; callers never
// need to author node routes, loop latches, or ValueRefs themselves.
type WorkflowBlueprintTemplate string

const (
	WorkflowBlueprintDeliveryRework  WorkflowBlueprintTemplate = "delivery_rework_loop"
	WorkflowBlueprintParallelReview  WorkflowBlueprintTemplate = "parallel_review"
	WorkflowBlueprintCreativeRework  WorkflowBlueprintTemplate = "creative_critique_loop"
	WorkflowBlueprintResearchSummary WorkflowBlueprintTemplate = "research_synthesis"
)

// WorkflowBlueprintWorker is the only agent reference the model writes.
// ResultRequirement is a business instruction, not graph structure.
type WorkflowBlueprintWorker struct {
	AgentID           string `json:"agent_id"`
	AgentVersion      int64  `json:"agent_version"`
	ResultRequirement string `json:"result_requirement"`
}

// WorkflowBlueprint is deliberately smaller than machine.GraphDefinition.
// Rework templates require Primary + Reviewer + MaxIterations;
// synthesis templates require ParallelWorkers + Finalizer.
type WorkflowBlueprint struct {
	Template         WorkflowBlueprintTemplate     `json:"template"`
	LeadInstruction  string                        `json:"lead_instruction"`
	Primary          *WorkflowBlueprintWorker      `json:"primary,omitempty"`
	Reviewer         *WorkflowBlueprintWorker      `json:"reviewer,omitempty"`
	ParallelWorkers  []WorkflowBlueprintWorker     `json:"parallel_workers"`
	Finalizer        *WorkflowBlueprintWorker      `json:"finalizer,omitempty"`
	MaxIterations    *int64                        `json:"max_iterations,omitempty"`
	DeliveryContract *deliverable.DeliveryContract `json:"delivery_contract,omitempty"`
}

// WorkflowBlueprintProblem is intentionally field-oriented and compact so a
// model can repair all invalid inputs in one retry.
type WorkflowBlueprintProblem struct {
	Path    string `json:"path"`
	Code    string `json:"code"`
	Message string `json:"message"`
	Hint    string `json:"hint"`
}

// CompiledWorkflowBlueprint is the deterministic machine input produced by a
// valid blueprint.
type CompiledWorkflowBlueprint struct {
	Trigger machine.TriggerConfig
	Graph   machine.GraphDefinition
}

// ValidateWorkflowBlueprint reports every blueprint-level issue at once.
// Machine validation still runs after compilation; this layer only makes the
// small semantic input repairable without exposing the machine schema.
func ValidateWorkflowBlueprint(blueprint WorkflowBlueprint) []WorkflowBlueprintProblem {
	var problems []WorkflowBlueprintProblem
	add := func(path, code, message, hint string) {
		problems = append(problems, WorkflowBlueprintProblem{
			Path: path, Code: code, Message: message, Hint: hint,
		})
	}
	if err := deliverable.ValidateDeliveryContract(blueprint.DeliveryContract); err != nil {
		add("/delivery_contract", "blueprint_delivery_contract_invalid", err.Error(), "修正交付物、检查项和外部副作用声明。")
	}
	if blueprint.DeliveryContract != nil && blueprint.DeliveryContract.Output.Type != "text" {
		add("/delivery_contract/output", "blueprint_delivery_output_mismatch", "built-in workflow output is text", "将交付合同 output.type 设为 text。")
	}

	if strings.TrimSpace(blueprint.LeadInstruction) == "" {
		add("/lead_instruction", "blueprint_lead_instruction_required",
			"lead_instruction is required",
			"填写负责人如何拆解、协调和验收这类任务。")
	}
	seen := make(map[string]string)
	validateWorker := func(path string, worker *WorkflowBlueprintWorker) {
		if worker == nil {
			return
		}
		worker.AgentID = strings.TrimSpace(worker.AgentID)
		if worker.AgentID == "" {
			add(path+"/agent_id", "blueprint_agent_id_required",
				"agent_id is required", "使用 tf_list_agents 返回的真实 agent id。")
		} else if prior, exists := seen[worker.AgentID]; exists {
			add(path+"/agent_id", "blueprint_agent_duplicate",
				fmt.Sprintf("agent_id duplicates %s", prior),
				"每个结构位置使用不同的 agent；若确需复用请改用定制入口。")
		} else {
			seen[worker.AgentID] = path
		}
		if worker.AgentVersion < 1 {
			add(path+"/agent_version", "blueprint_agent_version_invalid",
				"agent_version must be at least 1",
				"使用 tf_get_agent_version 核实后填写已存在的精确正整数版本。")
		}
		if strings.TrimSpace(worker.ResultRequirement) == "" {
			add(path+"/result_requirement", "blueprint_result_requirement_required",
				"result_requirement is required",
				"用一句话说明该成员必须返回的结果和完成标准。")
		}
	}

	validateWorker("/primary", blueprint.Primary)
	validateWorker("/reviewer", blueprint.Reviewer)
	for index := range blueprint.ParallelWorkers {
		validateWorker(fmt.Sprintf("/parallel_workers/%d", index), &blueprint.ParallelWorkers[index])
	}
	validateWorker("/finalizer", blueprint.Finalizer)

	switch blueprint.Template {
	case WorkflowBlueprintDeliveryRework, WorkflowBlueprintCreativeRework:
		if blueprint.Primary == nil {
			add("/primary", "blueprint_primary_required",
				"primary is required for rework templates",
				"提供负责首稿和返修的主执行 worker。")
		}
		if blueprint.Reviewer == nil {
			add("/reviewer", "blueprint_reviewer_required",
				"reviewer is required for rework templates",
				"提供负责验收产物并以 PASS 或 REVISE 结束回复的 worker。")
		}
		if len(blueprint.ParallelWorkers) != 0 {
			add("/parallel_workers", "blueprint_parallel_workers_forbidden",
				"parallel_workers is not used by rework templates",
				"删除 parallel_workers；顺序返修由 primary 与 reviewer 组成。")
		}
		if blueprint.Finalizer != nil {
			add("/finalizer", "blueprint_finalizer_forbidden",
				"finalizer is not used by rework templates",
				"删除 finalizer；返修模板直接交付主执行者通过校验的产物。")
		}
		if blueprint.MaxIterations == nil {
			add("/max_iterations", "blueprint_max_iterations_required",
				"max_iterations is required for rework templates",
				"填写 1 到 5 的返修轮次上限。")
		} else if *blueprint.MaxIterations < 1 || *blueprint.MaxIterations > 5 {
			add("/max_iterations", "blueprint_max_iterations_invalid",
				"max_iterations must be between 1 and 5",
				"将返修轮次改为 1 到 5。")
		}
	case WorkflowBlueprintParallelReview, WorkflowBlueprintResearchSummary:
		if blueprint.Primary != nil {
			add("/primary", "blueprint_primary_forbidden",
				"primary is not used by synthesis templates",
				"删除 primary；并行成员直接接收负责人简报。")
		}
		if blueprint.Reviewer != nil {
			add("/reviewer", "blueprint_reviewer_forbidden",
				"reviewer is not used by synthesis templates",
				"删除 reviewer；并行结果由 finalizer 统一汇总。")
		}
		if len(blueprint.ParallelWorkers) < 2 {
			add("/parallel_workers", "blueprint_parallel_workers_too_few",
				"parallel_workers requires at least two workers",
				"至少提供两个可并行执行的 worker 精确版本。")
		}
		if blueprint.Finalizer == nil {
			add("/finalizer", "blueprint_finalizer_required",
				"finalizer is required for synthesis templates",
				"提供负责整合并行结果并形成最终交付的 worker。")
		}
		if blueprint.MaxIterations != nil {
			add("/max_iterations", "blueprint_max_iterations_forbidden",
				"max_iterations is only used by rework templates",
				"删除 max_iterations；若需要返修请选择 rework 模板。")
		}
	default:
		add("/template", "blueprint_template_invalid",
			fmt.Sprintf("unsupported template %q", blueprint.Template),
			"使用 delivery_rework_loop、parallel_review、creative_critique_loop 或 research_synthesis。")
	}

	return problems
}

// CompileWorkflowBlueprint deterministically creates a machine graph after
// blueprint validation. The returned graph must still pass the platform's
// full proof-backed workflow validator before it can be committed.
func CompileWorkflowBlueprint(blueprint WorkflowBlueprint) (CompiledWorkflowBlueprint, []WorkflowBlueprintProblem) {
	if problems := ValidateWorkflowBlueprint(blueprint); len(problems) != 0 {
		return CompiledWorkflowBlueprint{}, problems
	}
	compiled := CompiledWorkflowBlueprint{
		Trigger: machine.TriggerConfig{
			SchemaVersion: machine.SchemaVersionV1,
			Type:          machine.TriggerConversationExplicit,
			Config:        machine.ConversationExplicitConfig{},
		},
	}
	switch blueprint.Template {
	case WorkflowBlueprintDeliveryRework, WorkflowBlueprintCreativeRework:
		compiled.Graph = compileReworkBlueprint(blueprint)
	case WorkflowBlueprintParallelReview, WorkflowBlueprintResearchSummary:
		compiled.Graph = compileSynthesisBlueprint(blueprint)
	}
	return compiled, nil
}

func compileReworkBlueprint(blueprint WorkflowBlueprint) machine.GraphDefinition {
	textOutput := machine.OutputContract{Type: machine.ValueText}
	current := machine.IterationCurrent
	previous := machine.IterationPrevious
	primary := *blueprint.Primary
	primary.ResultRequirement = withBlueprintProtocol(
		primary.ResultRequirement,
		"Use the feedback input to revise the artifact when it is non-empty.",
	)
	reviewer := *blueprint.Reviewer
	reviewer.ResultRequirement = withBlueprintProtocol(
		reviewer.ResultRequirement,
		"Use only the stated acceptance criteria, the artifact, and evidence directly referenced by the artifact. Do not search for unspecified documents or requirements. Once every stated criterion has a verdict, stop verification and return the conclusion immediately.",
	)
	reviewer.ResultRequirement = withBlueprintProtocol(
		reviewer.ResultRequirement,
		"End the final line with exactly PASS when accepted or REVISE when another iteration is required.",
	)
	graph := machine.GraphDefinition{
		SchemaVersion:    machine.SchemaVersionV1,
		EntryNodeID:      "lead",
		InputContract:    textOutput,
		OutputContract:   textOutput,
		DeliveryContract: deliverable.CloneDeliveryContract(blueprint.DeliveryContract),
		Nodes: []machine.Node{
			{ID: "lead", Type: machine.NodeLead, Label: "Coordinate", Inputs: blueprintLeadInputs(), Output: &textOutput,
				Config: machine.LeadConfig{Instruction: blueprint.LeadInstruction}},
			{ID: "rework", Type: machine.NodeLoop, Label: "Bounded rework",
				Config: machine.LoopConfig{
					MaxIterations: *blueprint.MaxIterations,
					LatchNodeID:   "latch",
					ContinuePredicate: machine.Predicate{
						Left:     machine.ValueRef{Source: machine.ValueNodeOutput, NodeID: "reviewer", Path: "", Iteration: current},
						Operator: machine.OperatorContains,
						Right:    &machine.ValueRef{Source: machine.ValueLiteral, Value: json.RawMessage(`"REVISE"`)},
					},
				}},
			blueprintWorkerNode("primary", "Primary work", primary, machine.WorkerConsult,
				map[string]machine.InputBinding{
					"brief": textNodeInput("lead", "", ""),
					"feedback": {
						ExpectedType: machine.ValueText,
						Value: machine.ValueRef{
							Source: machine.ValueNodeOutput, NodeID: "reviewer", Path: "", Iteration: previous,
							Default: &machine.ValueRef{Source: machine.ValueLiteral, Value: json.RawMessage(`""`)},
						},
					},
				}),
			blueprintWorkerNode("reviewer", "Review", reviewer, machine.WorkerConsult,
				map[string]machine.InputBinding{"artifact": textNodeInput("primary", "", current)}),
			{ID: "latch", Type: machine.NodeTransform, Label: "Capture accepted artifact", Output: &textOutput,
				Config: machine.TransformConfig{
					Operation: machine.TransformIdentity,
					Value:     &machine.ValueRef{Source: machine.ValueNodeOutput, NodeID: "primary", Path: "", Iteration: current},
				}},
			{ID: "deliver", Type: machine.NodeDeliver, Label: "Deliver",
				Config: machine.DeliverConfig{Result: machine.ValueRef{Source: machine.ValueNodeOutput, NodeID: "rework", Path: "/latch_result"}}},
		},
		Edges: []machine.Edge{
			blueprintEdge("lead-rework", "lead", "rework", machine.RouteSuccess),
			blueprintEdge("rework-primary", "rework", "primary", machine.RouteBody),
			blueprintEdge("rework-deliver", "rework", "deliver", machine.RouteExit),
			blueprintEdge("primary-reviewer", "primary", "reviewer", machine.RouteSuccess),
			blueprintEdge("reviewer-latch", "reviewer", "latch", machine.RouteSuccess),
			blueprintEdge("latch-rework", "latch", "rework", machine.RouteBack),
		},
	}
	return graph
}

func withBlueprintProtocol(requirement, protocol string) string {
	base := strings.TrimSpace(requirement)
	for strings.Contains(base, protocol) {
		base = strings.TrimSpace(strings.Replace(base, protocol, "", 1))
	}
	if base == "" {
		return protocol
	}
	return base + " " + protocol
}

func compileSynthesisBlueprint(blueprint WorkflowBlueprint) machine.GraphDefinition {
	textOutput := machine.OutputContract{Type: machine.ValueText}
	originalTask := blueprintLeadInputs()["run_input"]
	finalizer := *blueprint.Finalizer
	finalizer.ResultRequirement = withBlueprintProtocol(finalizer.ResultRequirement, synthesisNodeProtocol)
	graph := machine.GraphDefinition{
		SchemaVersion:    machine.SchemaVersionV1,
		EntryNodeID:      "lead",
		InputContract:    textOutput,
		OutputContract:   textOutput,
		DeliveryContract: deliverable.CloneDeliveryContract(blueprint.DeliveryContract),
		Nodes: []machine.Node{
			{ID: "lead", Type: machine.NodeLead, Label: "Coordinate", Inputs: blueprintLeadInputs(), Output: &textOutput,
				Config: machine.LeadConfig{Instruction: withBlueprintProtocol(blueprint.LeadInstruction, synthesisNodeProtocol)}},
			{ID: "parallel", Type: machine.NodeParallel, Label: "Parallel work",
				Config: machine.ParallelConfig{JoinNodeID: "join"}},
			{ID: "join", Type: machine.NodeJoin, Label: "Collect results",
				Config: machine.JoinConfig{Policy: machine.JoinAllSuccess}},
			blueprintWorkerNode("finalizer", "Synthesize", finalizer, machine.WorkerConsult,
				map[string]machine.InputBinding{
					"run_input": originalTask,
					"results":   {ExpectedType: machine.ValueJSON, Value: machine.ValueRef{Source: machine.ValueNodeOutput, NodeID: "join", Path: ""}},
				}),
			{ID: "deliver", Type: machine.NodeDeliver, Label: "Deliver",
				Config: machine.DeliverConfig{Result: machine.ValueRef{Source: machine.ValueNodeOutput, NodeID: "finalizer", Path: ""}}},
		},
		Edges: []machine.Edge{
			blueprintEdge("lead-parallel", "lead", "parallel", machine.RouteSuccess),
			blueprintEdge("join-finalizer", "join", "finalizer", machine.RouteSuccess),
			blueprintEdge("finalizer-deliver", "finalizer", "deliver", machine.RouteSuccess),
		},
	}
	for index, worker := range blueprint.ParallelWorkers {
		id := fmt.Sprintf("parallel-%02d", index+1)
		worker.ResultRequirement = withBlueprintProtocol(worker.ResultRequirement, synthesisNodeProtocol)
		graph.Nodes = append(graph.Nodes, blueprintWorkerNode(
			id, fmt.Sprintf("Parallel work %d", index+1), worker, machine.WorkerDispatch,
			map[string]machine.InputBinding{"run_input": originalTask, "brief": textNodeInput("lead", "", "")},
		))
		graph.Edges = append(graph.Edges,
			blueprintEdge("parallel-"+id, "parallel", id, machine.RouteBranch),
			blueprintEdge(id+"-join", id, "join", machine.RouteJoin),
		)
	}
	return graph
}

const synthesisNodeProtocol = "Execute only this node's assigned work and return that work as your response. The platform invokes teammates, waits for their results, and saves outputs; you do not need Weave dispatch or Workbench UI tools. For coordination, return a work brief instead of calling teammates or reporting missing platform controls. Treat run_input as the original task and source material; brief/results are upstream analysis and do not replace those materials."

func blueprintLeadInputs() map[string]machine.InputBinding {
	return map[string]machine.InputBinding{
		"run_input": {
			ExpectedType: machine.ValueText,
			Value: machine.ValueRef{
				Source: machine.ValueRunInput,
				Path:   "",
			},
		},
	}
}

func blueprintWorkerNode(
	id, label string,
	worker WorkflowBlueprintWorker,
	kind machine.WorkerKind,
	inputs map[string]machine.InputBinding,
) machine.Node {
	textOutput := machine.OutputContract{Type: machine.ValueText}
	return machine.Node{
		ID: id, Type: machine.NodeWorker, Label: label, Inputs: inputs, Output: &textOutput,
		Config: machine.WorkerConfig{
			AgentID: strings.TrimSpace(worker.AgentID), AgentVersion: worker.AgentVersion,
			Kind: kind, ResultRequirement: strings.TrimSpace(worker.ResultRequirement),
		},
	}
}

func textNodeInput(nodeID, path string, iteration machine.Iteration) machine.InputBinding {
	return machine.InputBinding{
		ExpectedType: machine.ValueText,
		Value:        machine.ValueRef{Source: machine.ValueNodeOutput, NodeID: nodeID, Path: path, Iteration: iteration},
	}
}

func blueprintEdge(id, from, to string, route machine.EdgeRoute) machine.Edge {
	return machine.Edge{ID: id, FromNodeID: from, ToNodeID: to, Route: route}
}
