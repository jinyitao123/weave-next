package teamforge

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/jinyitao123/weave/internal/kernel/workflow/machine"
)

const DeclarativePatternParallelJoinReviewLoopV1 = "parallel_join_review_loop"

// DeclarativeWorkflowPatternV1 is the high-level, planner-authored input for
// platform-owned workflow shapes. The platform, rather than an LLM, owns every
// control-flow edge, ValueRef, iteration marker, join binding, and latch.
type DeclarativeWorkflowPatternV1 struct {
	Kind            string                       `json:"kind"`
	LeadInstruction string                       `json:"lead_instruction"`
	ParallelWorkers []DeclarativePatternWorkerV1 `json:"parallel_workers"`
	PrimaryWorker   DeclarativePatternWorkerV1   `json:"primary_worker"`
	ReviewerWorker  DeclarativePatternWorkerV1   `json:"reviewer_worker"`
	MaxIterations   int                          `json:"max_iterations"`
}

type DeclarativePatternWorkerV1 struct {
	StableRef         string `json:"stable_ref"`
	Label             string `json:"label,omitempty"`
	ResultRequirement string `json:"result_requirement"`
}

// BuildDeclarativeWorkflowPatternV1 deterministically expands one supported
// high-level shape into the same graph form exercised by the compiler E2E
// suite. Business meaning stays in instructions and frozen stable refs; the
// formal workflow language is entirely platform-owned. The team lead remains
// immutable ownership metadata. Its instruction is materialized as a
// deterministic requirements node so this pattern never creates an
// unhosted Loom LLM execution step alongside Runtime-backed CLI workers.
func BuildDeclarativeWorkflowPatternV1(pattern DeclarativeWorkflowPatternV1) (DeclarativeWorkflowSpecV1, error) {
	if strings.TrimSpace(pattern.Kind) != DeclarativePatternParallelJoinReviewLoopV1 {
		return DeclarativeWorkflowSpecV1{}, fmt.Errorf("unsupported declarative workflow pattern %q", pattern.Kind)
	}
	if strings.TrimSpace(pattern.LeadInstruction) == "" {
		return DeclarativeWorkflowSpecV1{}, errors.New("declarative workflow pattern requires lead_instruction")
	}
	if len(pattern.ParallelWorkers) < 2 || len(pattern.ParallelWorkers) > 16 {
		return DeclarativeWorkflowSpecV1{}, errors.New("parallel_join_review_loop requires 2..16 parallel_workers")
	}
	if pattern.MaxIterations < 1 || pattern.MaxIterations > 5 {
		return DeclarativeWorkflowSpecV1{}, errors.New("parallel_join_review_loop max_iterations must be 1..5")
	}

	workers := append([]DeclarativePatternWorkerV1(nil), pattern.ParallelWorkers...)
	workers = append(workers, pattern.PrimaryWorker, pattern.ReviewerWorker)
	seen := make(map[string]struct{}, len(workers))
	for _, worker := range workers {
		ref := strings.TrimSpace(worker.StableRef)
		if !declarativeStableRefPattern.MatchString(ref) {
			return DeclarativeWorkflowSpecV1{}, fmt.Errorf("declarative workflow pattern worker stable_ref %q is invalid", worker.StableRef)
		}
		if strings.TrimSpace(worker.ResultRequirement) == "" {
			return DeclarativeWorkflowSpecV1{}, fmt.Errorf("declarative workflow pattern worker %q requires result_requirement", ref)
		}
		if _, exists := seen[ref]; exists {
			return DeclarativeWorkflowSpecV1{}, fmt.Errorf("declarative workflow pattern worker stable_ref %q is duplicated", ref)
		}
		seen[ref] = struct{}{}
	}

	text := &machine.OutputContract{Type: machine.ValueText}
	current := machine.IterationCurrent
	previous := machine.IterationPrevious
	pattern.PrimaryWorker.ResultRequirement = withBlueprintProtocol(
		pattern.PrimaryWorker.ResultRequirement,
		"Return a complete delivery-ready artifact on every iteration. Do not label the artifact as pending downstream verification; the workflow itself enforces the verification loop.",
	)
	pattern.ReviewerWorker.ResultRequirement = withBlueprintProtocol(
		pattern.ReviewerWorker.ResultRequirement,
		"Verify the candidate without replacing it as the delivered artifact. Return actionable feedback and end the final line with exactly PASS when accepted or REVISE when another iteration is required.",
	)
	leadInstruction, err := json.Marshal(strings.TrimSpace(pattern.LeadInstruction))
	if err != nil {
		return DeclarativeWorkflowSpecV1{}, fmt.Errorf("encode declarative workflow lead instruction: %w", err)
	}
	requirementsConfig, err := declarativePatternConfig(machine.TransformConfig{
		Operation: machine.TransformIdentity,
		Value: &machine.ValueRef{
			Source: machine.ValueLiteral,
			Value:  leadInstruction,
		},
	})
	if err != nil {
		return DeclarativeWorkflowSpecV1{}, err
	}
	parallelConfig, err := declarativePatternConfig(machine.ParallelConfig{JoinNodeID: "parallel-join"})
	if err != nil {
		return DeclarativeWorkflowSpecV1{}, err
	}
	joinConfig, err := declarativePatternConfig(machine.JoinConfig{Policy: machine.JoinAllSuccess})
	if err != nil {
		return DeclarativeWorkflowSpecV1{}, err
	}
	loopConfig, err := declarativePatternConfig(machine.LoopConfig{
		MaxIterations: int64(pattern.MaxIterations),
		LatchNodeID:   "result-latch",
		ContinuePredicate: machine.Predicate{
			Left: machine.ValueRef{
				Source: machine.ValueNodeOutput, NodeID: "reviewer", Iteration: current,
			},
			Operator: machine.OperatorContains,
			Right: &machine.ValueRef{
				Source: machine.ValueLiteral, Value: json.RawMessage(`"REVISE"`),
			},
		},
	})
	if err != nil {
		return DeclarativeWorkflowSpecV1{}, err
	}
	latchConfig, err := declarativePatternConfig(machine.TransformConfig{
		Operation: machine.TransformIdentity,
		Value: &machine.ValueRef{
			Source: machine.ValueNodeOutput, NodeID: "primary", Iteration: current,
		},
	})
	if err != nil {
		return DeclarativeWorkflowSpecV1{}, err
	}
	deliverConfig, err := declarativePatternConfig(machine.DeliverConfig{Result: machine.ValueRef{
		Source: machine.ValueNodeOutput, NodeID: "review-loop", Path: "/latch_result",
	}})
	if err != nil {
		return DeclarativeWorkflowSpecV1{}, err
	}

	spec := DeclarativeWorkflowSpecV1{
		SchemaVersion:  DeclarativeWorkflowSpecSchemaVersionV1,
		EntryNodeID:    "requirements",
		InputContract:  *text,
		OutputContract: *text,
		Nodes: []DeclarativeWorkflowNodeV1{
			{ID: "requirements", Type: machine.NodeTransform, Label: "运行合同", Output: text, Config: requirementsConfig},
			{ID: "parallel", Type: machine.NodeParallel, Label: "并行执行", Config: parallelConfig},
		},
		Edges: []DeclarativeWorkflowEdgeV1{
			{From: "requirements", To: "parallel", Route: machine.RouteSuccess},
		},
	}
	for index, worker := range pattern.ParallelWorkers {
		nodeID := fmt.Sprintf("parallel-worker-%d", index+1)
		node, nodeErr := declarativePatternWorkerNode(
			nodeID, worker, machine.WorkerDispatch,
			map[string]machine.InputBinding{
				"requirements": declarativePatternTextInput("requirements", ""),
				"run_input":    declarativePatternRunInput(),
			},
		)
		if nodeErr != nil {
			return DeclarativeWorkflowSpecV1{}, nodeErr
		}
		spec.Nodes = append(spec.Nodes, node)
		spec.Edges = append(spec.Edges,
			DeclarativeWorkflowEdgeV1{From: "parallel", To: nodeID, Route: machine.RouteBranch},
			DeclarativeWorkflowEdgeV1{From: nodeID, To: "parallel-join", Route: machine.RouteJoin},
		)
	}
	spec.Nodes = append(spec.Nodes,
		DeclarativeWorkflowNodeV1{ID: "parallel-join", Type: machine.NodeJoin, Label: "结果汇聚", Config: joinConfig},
		DeclarativeWorkflowNodeV1{ID: "review-loop", Type: machine.NodeLoop, Label: "校验返修", Config: loopConfig},
	)
	primary, err := declarativePatternWorkerNode(
		"primary", pattern.PrimaryWorker, machine.WorkerConsult,
		map[string]machine.InputBinding{
			"parallel_results": {
				ExpectedType: machine.ValueJSON,
				Value:        machine.ValueRef{Source: machine.ValueNodeOutput, NodeID: "parallel-join"},
			},
			"requirements": declarativePatternTextInput("requirements", ""),
			"run_input":    declarativePatternRunInput(),
			"review_feedback": {
				ExpectedType: machine.ValueText,
				Value: machine.ValueRef{
					Source: machine.ValueNodeOutput, NodeID: "reviewer", Iteration: previous,
					Default: &machine.ValueRef{Source: machine.ValueLiteral, Value: json.RawMessage(`""`)},
				},
			},
		},
	)
	if err != nil {
		return DeclarativeWorkflowSpecV1{}, err
	}
	reviewer, err := declarativePatternWorkerNode(
		"reviewer", pattern.ReviewerWorker, machine.WorkerConsult,
		map[string]machine.InputBinding{
			"candidate": {
				ExpectedType: machine.ValueText,
				Value: machine.ValueRef{
					Source: machine.ValueNodeOutput, NodeID: "primary", Iteration: current,
				},
			},
			"requirements": declarativePatternTextInput("requirements", ""),
			"run_input":    declarativePatternRunInput(),
		},
	)
	if err != nil {
		return DeclarativeWorkflowSpecV1{}, err
	}
	spec.Nodes = append(spec.Nodes,
		primary,
		reviewer,
		DeclarativeWorkflowNodeV1{ID: "result-latch", Type: machine.NodeTransform, Label: "结果锁存", Output: text, Config: latchConfig},
		DeclarativeWorkflowNodeV1{ID: "deliver", Type: machine.NodeDeliver, Label: "交付", Config: deliverConfig},
	)
	spec.Edges = append(spec.Edges,
		DeclarativeWorkflowEdgeV1{From: "parallel-join", To: "review-loop", Route: machine.RouteSuccess},
		DeclarativeWorkflowEdgeV1{From: "review-loop", To: "primary", Route: machine.RouteBody},
		DeclarativeWorkflowEdgeV1{From: "primary", To: "reviewer", Route: machine.RouteSuccess},
		DeclarativeWorkflowEdgeV1{From: "reviewer", To: "result-latch", Route: machine.RouteSuccess},
		DeclarativeWorkflowEdgeV1{From: "result-latch", To: "review-loop", Route: machine.RouteBack},
		DeclarativeWorkflowEdgeV1{From: "review-loop", To: "deliver", Route: machine.RouteExit},
	)
	return spec, nil
}

func declarativePatternWorkerNode(
	nodeID string,
	worker DeclarativePatternWorkerV1,
	kind machine.WorkerKind,
	inputs map[string]machine.InputBinding,
) (DeclarativeWorkflowNodeV1, error) {
	config, err := declarativePatternConfig(DeclarativeWorkerConfigV1{
		Kind: kind, ResultRequirement: strings.TrimSpace(worker.ResultRequirement),
	})
	if err != nil {
		return DeclarativeWorkflowNodeV1{}, err
	}
	return DeclarativeWorkflowNodeV1{
		ID: nodeID, Type: machine.NodeWorker, Label: strings.TrimSpace(worker.Label),
		StableRef: strings.TrimSpace(worker.StableRef), Inputs: inputs,
		Output: &machine.OutputContract{Type: machine.ValueText}, Config: config,
	}, nil
}

func declarativePatternTextInput(nodeID string, iteration machine.Iteration) machine.InputBinding {
	return machine.InputBinding{
		ExpectedType: machine.ValueText,
		Value:        machine.ValueRef{Source: machine.ValueNodeOutput, NodeID: nodeID, Iteration: iteration},
	}
}

func declarativePatternRunInput() machine.InputBinding {
	return machine.InputBinding{
		ExpectedType: machine.ValueText,
		Value:        machine.ValueRef{Source: machine.ValueRunInput},
	}
}

func declarativePatternConfig(value any) (json.RawMessage, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("encode declarative workflow pattern: %w", err)
	}
	return raw, nil
}
