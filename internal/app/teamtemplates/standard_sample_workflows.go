package teamtemplates

import (
	"encoding/json"

	"github.com/jinyitao123/weave/internal/build/teamforge"
	"github.com/jinyitao123/weave/internal/kernel/workflow/machine"
)

func synthesisSampleDeclarativeSpec(firstRef, firstRequirement, secondRef, secondRequirement, finalizerRef, finalizerRequirement string) *teamforge.DeclarativeWorkflowSpecV1 {
	text := &machine.OutputContract{Type: machine.ValueText}
	return &teamforge.DeclarativeWorkflowSpecV1{
		SchemaVersion: 1, EntryNodeID: "parallel", InputContract: *text, OutputContract: *text,
		Nodes: []teamforge.DeclarativeWorkflowNodeV1{
			{ID: "parallel", Type: machine.NodeParallel, Label: "并行执行", Config: sampleConfig(machine.ParallelConfig{JoinNodeID: "join"})},
			sampleWorkerNode("first", firstRef, firstRequirement, machine.WorkerDispatch, map[string]machine.InputBinding{"task": sampleRunInput()}),
			sampleWorkerNode("second", secondRef, secondRequirement, machine.WorkerDispatch, map[string]machine.InputBinding{"task": sampleRunInput()}),
			{ID: "join", Type: machine.NodeJoin, Label: "结果汇聚", Config: sampleConfig(machine.JoinConfig{Policy: machine.JoinAllSuccess})},
			sampleWorkerNode("finalizer", finalizerRef, finalizerRequirement, machine.WorkerConsult, map[string]machine.InputBinding{
				"task":    sampleRunInput(),
				"results": {ExpectedType: machine.ValueJSON, Value: machine.ValueRef{Source: machine.ValueNodeOutput, NodeID: "join"}},
			}),
			{ID: "deliver", Type: machine.NodeDeliver, Label: "交付", Config: sampleConfig(machine.DeliverConfig{Result: machine.ValueRef{Source: machine.ValueNodeOutput, NodeID: "finalizer"}})},
		},
		Edges: []teamforge.DeclarativeWorkflowEdgeV1{
			{From: "parallel", To: "first", Route: machine.RouteBranch},
			{From: "parallel", To: "second", Route: machine.RouteBranch},
			{From: "first", To: "join", Route: machine.RouteJoin},
			{From: "second", To: "join", Route: machine.RouteJoin},
			{From: "join", To: "finalizer", Route: machine.RouteSuccess},
			{From: "finalizer", To: "deliver", Route: machine.RouteSuccess},
		},
	}
}

func reworkSampleDeclarativeSpec(primaryRef, primaryRequirement, reviewerRef, reviewerRequirement string, maxIterations int64) *teamforge.DeclarativeWorkflowSpecV1 {
	text := &machine.OutputContract{Type: machine.ValueText}
	current, previous := machine.IterationCurrent, machine.IterationPrevious
	return &teamforge.DeclarativeWorkflowSpecV1{
		SchemaVersion: 1, EntryNodeID: "rework", InputContract: *text, OutputContract: *text,
		Nodes: []teamforge.DeclarativeWorkflowNodeV1{
			{ID: "rework", Type: machine.NodeLoop, Label: "审校返修", Config: sampleConfig(machine.LoopConfig{
				MaxIterations: maxIterations, LatchNodeID: "latch",
				ContinuePredicate: machine.Predicate{
					Left:     machine.ValueRef{Source: machine.ValueNodeOutput, NodeID: "reviewer", Iteration: current},
					Operator: machine.OperatorContains,
					Right:    &machine.ValueRef{Source: machine.ValueLiteral, Value: json.RawMessage(`"REVISE"`)},
				},
			})},
			sampleWorkerNode("primary", primaryRef, primaryRequirement, machine.WorkerConsult, map[string]machine.InputBinding{
				"task": sampleRunInput(),
				"feedback": {ExpectedType: machine.ValueText, Value: machine.ValueRef{
					Source: machine.ValueNodeOutput, NodeID: "reviewer", Iteration: previous,
					Default: &machine.ValueRef{Source: machine.ValueLiteral, Value: json.RawMessage(`""`)},
				}},
			}),
			sampleWorkerNode("reviewer", reviewerRef, reviewerRequirement+"；末行仅输出 PASS 或 REVISE", machine.WorkerConsult, map[string]machine.InputBinding{
				"candidate": {ExpectedType: machine.ValueText, Value: machine.ValueRef{Source: machine.ValueNodeOutput, NodeID: "primary", Iteration: current}},
				"task":      sampleRunInput(),
			}),
			{ID: "latch", Type: machine.NodeTransform, Label: "锁定成稿", Output: text, Config: sampleConfig(machine.TransformConfig{
				Operation: machine.TransformIdentity,
				Value:     &machine.ValueRef{Source: machine.ValueNodeOutput, NodeID: "primary", Iteration: current},
			})},
			{ID: "deliver", Type: machine.NodeDeliver, Label: "交付", Config: sampleConfig(machine.DeliverConfig{Result: machine.ValueRef{Source: machine.ValueNodeOutput, NodeID: "rework", Path: "/latch_result"}})},
		},
		Edges: []teamforge.DeclarativeWorkflowEdgeV1{
			{From: "rework", To: "primary", Route: machine.RouteBody},
			{From: "primary", To: "reviewer", Route: machine.RouteSuccess},
			{From: "reviewer", To: "latch", Route: machine.RouteSuccess},
			{From: "latch", To: "rework", Route: machine.RouteBack},
			{From: "rework", To: "deliver", Route: machine.RouteExit},
		},
	}
}

func sampleWorkerNode(id, stableRef, requirement string, kind machine.WorkerKind, inputs map[string]machine.InputBinding) teamforge.DeclarativeWorkflowNodeV1 {
	return teamforge.DeclarativeWorkflowNodeV1{
		ID: id, Type: machine.NodeWorker, StableRef: stableRef, Inputs: inputs,
		Output: &machine.OutputContract{Type: machine.ValueText},
		Config: sampleConfig(teamforge.DeclarativeWorkerConfigV1{Kind: kind, ResultRequirement: requirement}),
	}
}

func sampleRunInput() machine.InputBinding {
	return machine.InputBinding{ExpectedType: machine.ValueText, Value: machine.ValueRef{Source: machine.ValueRunInput}}
}

func sampleConfig(value any) json.RawMessage {
	raw, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return raw
}
