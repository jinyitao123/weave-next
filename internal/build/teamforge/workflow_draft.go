package teamforge

// Team-workflow draft carrier (plan §10.2.3). The platform has no node-level
// incremental API for workflows — drafts are only replaced wholesale through
// workflow.Store.UpdateDraft with a version+timestamp CAS — so this package
// owns one draft per workspace + build_run_id + workflow_id. Node,
// edge, binding, contract, and trigger operations accumulate on the typed
// machine model; tf_wf_commit encodes the whole draft back into
// trigger_config + graph_definition RawMessages and lands one UpdateDraft.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jinyitao123/weave/internal/base/deliverable"
	"github.com/jinyitao123/weave/internal/kernel/workflow/machine"
)

// workflowDraft is one working copy of a team workflow draft
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
	Revision   int64
}

// workflowDraftStore is bound to one workspace and build run. Persistence is
// provided by the build storage adapter.
type workflowDraftStore struct {
	persistence DraftPersistence
	workspaceID string
	buildRunID  string
}

type persistedWorkflowDraft struct {
	BuildRunID string          `json:"build_run_id"`
	WorkflowID string          `json:"workflow_id"`
	Version    int             `json:"version"`
	Trigger    json.RawMessage `json:"trigger"`
	Graph      json.RawMessage `json:"graph"`
	UpdatedAt  time.Time       `json:"updated_at"`
	CreatedBy  string          `json:"created_by"`
}

func (s *workflowDraftStore) get(ctx context.Context, buildRunID, workflowID string) (*workflowDraft, error) {
	if s == nil || s.persistence == nil {
		return nil, errors.New("workflow draft persistence is unavailable")
	}
	if err := validateDraftBinding(s.workspaceID, s.buildRunID, buildRunID, workflowID); err != nil {
		return nil, err
	}
	payload, revision, found, err := s.persistence.LoadDraft(ctx, s.workspaceID, s.buildRunID, draftKindTeamWorkflow, workflowID)
	if err != nil || !found {
		return nil, err
	}
	var stored persistedWorkflowDraft
	if err := json.Unmarshal(payload, &stored); err != nil {
		return nil, fmt.Errorf("decode workflow draft: %w", err)
	}
	if err := validateDraftBinding(s.workspaceID, s.buildRunID, stored.BuildRunID, stored.WorkflowID); err != nil || stored.WorkflowID != workflowID {
		return nil, errors.New("persisted workflow draft identity mismatch")
	}
	trigger, triggerReport := machine.DecodeTriggerConfigV1(stored.Trigger)
	graph, graphReport := machine.DecodeGraphDefinitionV1(stored.Graph)
	if (triggerReport != nil && len(triggerReport.Issues) != 0) ||
		(graphReport != nil && len(graphReport.Issues) != 0) {
		return nil, errors.New("persisted workflow draft failed strict decoding")
	}
	return &workflowDraft{
		BuildRunID: stored.BuildRunID, WorkflowID: stored.WorkflowID,
		Version: stored.Version, Trigger: trigger, Graph: graph,
		UpdatedAt: stored.UpdatedAt, CreatedBy: stored.CreatedBy, Revision: revision,
	}, nil
}

func (s *workflowDraftStore) put(ctx context.Context, d *workflowDraft) error {
	if s == nil || s.persistence == nil || d == nil {
		return errors.New("workflow draft persistence is unavailable")
	}
	if err := validateDraftBinding(s.workspaceID, s.buildRunID, d.BuildRunID, d.WorkflowID); err != nil {
		return err
	}
	trigger, graph, err := encodeWorkflowDraftJSON(d.Trigger, d.Graph)
	if err != nil {
		return fmt.Errorf("encode workflow draft: %w", err)
	}
	payload, err := json.Marshal(persistedWorkflowDraft{
		BuildRunID: d.BuildRunID, WorkflowID: d.WorkflowID, Version: d.Version,
		Trigger: trigger, Graph: graph, UpdatedAt: d.UpdatedAt, CreatedBy: d.CreatedBy,
	})
	if err != nil {
		return fmt.Errorf("encode workflow draft carrier: %w", err)
	}
	revision, err := s.persistence.SaveDraft(ctx, s.workspaceID, s.buildRunID, draftKindTeamWorkflow, d.WorkflowID, d.Revision, payload)
	if err == nil {
		d.Revision = revision
	}
	return err
}

func (s *workflowDraftStore) delete(ctx context.Context, d *workflowDraft) error {
	if s == nil || s.persistence == nil || d == nil {
		return errors.New("workflow draft persistence is unavailable")
	}
	if err := validateDraftBinding(s.workspaceID, s.buildRunID, d.BuildRunID, d.WorkflowID); err != nil {
		return err
	}
	return s.persistence.DeleteDraft(ctx, s.workspaceID, s.buildRunID, draftKindTeamWorkflow, d.WorkflowID, d.Revision)
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
		SchemaVersion:    graph.SchemaVersion,
		EntryNodeID:      graph.EntryNodeID,
		ResultProtocol:   graph.ResultProtocol,
		InputContract:    cloneOutputContract(graph.InputContract),
		OutputContract:   cloneOutputContract(graph.OutputContract),
		DeliveryContract: deliverable.CloneDeliveryContract(graph.DeliveryContract),
		Nodes:            nodes,
		Edges:            edges,
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
