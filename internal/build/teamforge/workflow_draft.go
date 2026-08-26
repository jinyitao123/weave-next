package teamforge

// Team-workflow draft carrier (plan §10.2.3). The platform has no node-level
// incremental API for workflows — drafts are only replaced wholesale through
// workflow.Store.UpdateDraft with a version+timestamp CAS — so this package
// owns one draft per build_run_id + workflow_id in dispatcher memory. Node,
// edge, binding, contract, and trigger operations accumulate on the typed
// machine model; tf_wf_commit encodes the whole draft back into
// trigger_config + graph_definition RawMessages and lands one UpdateDraft.

import (
	"encoding/json"
	"sync"
	"time"

	"github.com/jinyitao123/weave/internal/kernel/workflow/machine"
)

// workflowDraft is one in-memory working copy of a team workflow draft
// together with its version and CAS token. It is a plain value type; the
// owning store replaces the whole value on every successful mutation.
type workflowDraft struct {
	BuildRunID string
	WorkflowID string
	Version    int
	Trigger    machine.TriggerConfig
	Graph      machine.GraphDefinition
	UpdatedAt  time.Time
	CreatedBy  string
}

// workflowDraftStore is the dispatcher-owned in-memory draft table. Drafts
// are deliberately not persisted: an uncommitted draft has no platform
// representation, and the model re-runs tf_wf_begin to restore context.
type workflowDraftStore struct {
	mu     sync.Mutex
	drafts map[string]*workflowDraft
}

func newWorkflowDraftStore() *workflowDraftStore {
	return &workflowDraftStore{drafts: make(map[string]*workflowDraft)}
}

// workflowDraftKey binds one draft to its build run and target workflow.
func workflowDraftKey(buildRunID, workflowID string) string {
	return buildRunID + ":" + workflowID
}

func (s *workflowDraftStore) get(buildRunID, workflowID string) *workflowDraft {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.drafts[workflowDraftKey(buildRunID, workflowID)]
}

func (s *workflowDraftStore) put(d *workflowDraft) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.drafts[workflowDraftKey(d.BuildRunID, d.WorkflowID)] = d
}

func (s *workflowDraftStore) delete(buildRunID, workflowID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.drafts, workflowDraftKey(buildRunID, workflowID))
}

// cloneWorkflowDraft deep-copies a draft so batch mutations and summaries
// never alias the stored model. machine values contain interfaces and
// maps/slices, so the copy walks every field explicitly.
func cloneWorkflowDraft(d *workflowDraft) *workflowDraft {
	if d == nil {
		return nil
	}
	cp := *d
	cp.Trigger = cloneTriggerConfig(d.Trigger)
	cp.Graph = cloneMachineGraph(d.Graph)
	return &cp
}

func cloneTriggerConfig(trigger machine.TriggerConfig) machine.TriggerConfig {
	cp := trigger
	cp.Delivery = cloneDelivery(trigger.Delivery)
	switch config := trigger.Config.(type) {
	case machine.ConversationExplicitConfig:
		cp.Config = config
	case machine.ConversationAutoConfig:
		cp.Config = config
	case machine.ScheduleConfig:
		cp.Config = config
	case machine.APIConfig:
		cp.Config = config
	case machine.EventConfig:
		cp.Config = config
	}
	return cp
}

func cloneDelivery(delivery *machine.Delivery) *machine.Delivery {
	if delivery == nil {
		return nil
	}
	cp := *delivery
	return &cp
}

func cloneMachineGraph(graph machine.GraphDefinition) machine.GraphDefinition {
	nodes := make([]machine.Node, len(graph.Nodes))
	for i, node := range graph.Nodes {
		nodes[i] = cloneMachineNode(node)
	}
	edges := make([]machine.Edge, len(graph.Edges))
	for i, edge := range graph.Edges {
		edges[i] = cloneMachineEdge(edge)
	}
	return machine.GraphDefinition{
		SchemaVersion:  graph.SchemaVersion,
		EntryNodeID:    graph.EntryNodeID,
		InputContract:  cloneOutputContract(graph.InputContract),
		OutputContract: cloneOutputContract(graph.OutputContract),
		Nodes:          nodes,
		Edges:          edges,
	}
}

func cloneMachineNode(node machine.Node) machine.Node {
	cp := node
	cp.Output = cloneOutputContractPtr(node.Output)
	cp.Inputs = make(map[string]machine.InputBinding, len(node.Inputs))
	for name, binding := range node.Inputs {
		cp.Inputs[name] = machine.InputBinding{
			ExpectedType: binding.ExpectedType,
			Value:        cloneValueRef(binding.Value),
		}
	}
	cp.Config = cloneNodeConfig(node.Config)
	return cp
}

func cloneNodeConfig(config machine.NodeConfig) machine.NodeConfig {
	switch typed := config.(type) {
	case machine.LeadConfig:
		return typed
	case machine.WorkerConfig:
		return typed
	case machine.TransformConfig:
		cp := typed
		cp.Value = cloneValueRefPtr(typed.Value)
		if typed.Fields != nil {
			cp.Fields = make(map[string]machine.ValueRef, len(typed.Fields))
			for name, ref := range typed.Fields {
				cp.Fields[name] = cloneValueRef(ref)
			}
		}
		if typed.Items != nil {
			cp.Items = make([]machine.ValueRef, len(typed.Items))
			for i, ref := range typed.Items {
				cp.Items[i] = cloneValueRef(ref)
			}
		}
		return cp
	case machine.ConditionConfig:
		return typed
	case machine.ParallelConfig:
		return typed
	case machine.JoinConfig:
		cp := typed
		cp.SuccessCount = cloneInt64Ptr(typed.SuccessCount)
		cp.DeadlineSeconds = cloneInt64Ptr(typed.DeadlineSeconds)
		return cp
	case machine.WaitConfig:
		cp := typed
		cp.ResumeSchema = append(json.RawMessage(nil), typed.ResumeSchema...)
		cp.TimeoutSeconds = cloneInt64Ptr(typed.TimeoutSeconds)
		if typed.Task != nil {
			task := *typed.Task
			cp.Task = &task
		}
		return cp
	case machine.LoopConfig:
		cp := typed
		cp.ContinuePredicate = clonePredicate(typed.ContinuePredicate)
		return cp
	case machine.DeliverConfig:
		cp := typed
		cp.Result = cloneValueRef(typed.Result)
		return cp
	case machine.HandoffConfig:
		cp := typed
		cp.TimeoutSeconds = cloneInt64Ptr(typed.TimeoutSeconds)
		return cp
	default:
		return nil
	}
}

func cloneMachineEdge(edge machine.Edge) machine.Edge {
	cp := edge
	cp.Priority = cloneInt64Ptr(edge.Priority)
	cp.Predicate = clonePredicatePtr(edge.Predicate)
	return cp
}

func cloneOutputContractPtr(contract *machine.OutputContract) *machine.OutputContract {
	if contract == nil {
		return nil
	}
	cp := cloneOutputContract(*contract)
	return &cp
}

func cloneOutputContract(contract machine.OutputContract) machine.OutputContract {
	cp := contract
	cp.Schema = append(json.RawMessage(nil), contract.Schema...)
	return cp
}

func cloneValueRefPtr(ref *machine.ValueRef) *machine.ValueRef {
	if ref == nil {
		return nil
	}
	cp := cloneValueRef(*ref)
	return &cp
}

func cloneValueRef(ref machine.ValueRef) machine.ValueRef {
	cp := ref
	cp.Value = append(json.RawMessage(nil), ref.Value...)
	cp.Default = cloneValueRefPtr(ref.Default)
	return cp
}

func clonePredicatePtr(predicate *machine.Predicate) *machine.Predicate {
	if predicate == nil {
		return nil
	}
	cp := clonePredicate(*predicate)
	return &cp
}

func clonePredicate(predicate machine.Predicate) machine.Predicate {
	cp := predicate
	cp.Left = cloneValueRef(predicate.Left)
	cp.Right = cloneValueRefPtr(predicate.Right)
	return cp
}

func cloneInt64Ptr(value *int64) *int64 {
	if value == nil {
		return nil
	}
	cp := *value
	return &cp
}
