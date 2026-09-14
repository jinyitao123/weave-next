package teamforge

// Employee internal-graph draft carrier (plan §10.2.2). The AgentRecord has
// no draft state machine, so this package owns a typed draft carrier:
// drafts are keyed by build_run_id + agent name, step operations accumulate
// on the draft, and only tf_graph_commit validates the whole graph and
// PutTx's a new immutable agent version. The carrier is persisted through the
// build storage port so it survives process restarts.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jinyitao123/weave/internal/kernel/registry"
)

// graphDraft is one working copy of an employee internal graph
// together with its output contract. It is a plain value type; the owning
// store replaces the whole value on every successful mutation.
type graphDraft struct {
	BuildRunID     string
	AgentName      string
	Graph          registry.GraphDefinition
	OutputContract json.RawMessage
	Revision       int64 `json:"-"`
}

// graphDraftStore is bound to one workspace and build run. Payload persistence
// stays behind DraftPersistence so domain tools do not depend on PG.
type graphDraftStore struct {
	persistence DraftPersistence
	workspaceID string
	buildRunID  string
}

func (s *graphDraftStore) get(ctx context.Context, buildRunID, agentName string) (*graphDraft, error) {
	if s == nil || s.persistence == nil {
		return nil, errors.New("graph draft persistence is unavailable")
	}
	if err := validateDraftBinding(s.workspaceID, s.buildRunID, buildRunID, agentName); err != nil {
		return nil, err
	}
	payload, revision, found, err := s.persistence.LoadDraft(ctx, s.workspaceID, s.buildRunID, draftKindAgentGraph, agentName)
	if err != nil || !found {
		return nil, err
	}
	var draft graphDraft
	if err := json.Unmarshal(payload, &draft); err != nil {
		return nil, fmt.Errorf("decode graph draft: %w", err)
	}
	if err := validateDraftBinding(s.workspaceID, s.buildRunID, draft.BuildRunID, draft.AgentName); err != nil || draft.AgentName != agentName {
		return nil, errors.New("persisted graph draft identity mismatch")
	}
	draft.Revision = revision
	return &draft, nil
}

func (s *graphDraftStore) put(ctx context.Context, d *graphDraft) error {
	if s == nil || s.persistence == nil || d == nil {
		return errors.New("graph draft persistence is unavailable")
	}
	if err := validateDraftBinding(s.workspaceID, s.buildRunID, d.BuildRunID, d.AgentName); err != nil {
		return err
	}
	payload, err := json.Marshal(d)
	if err != nil {
		return fmt.Errorf("encode graph draft: %w", err)
	}
	revision, err := s.persistence.SaveDraft(ctx, s.workspaceID, s.buildRunID, draftKindAgentGraph, d.AgentName, d.Revision, payload)
	if err == nil {
		d.Revision = revision
	}
	return err
}

func (s *graphDraftStore) delete(ctx context.Context, d *graphDraft) error {
	if s == nil || s.persistence == nil || d == nil {
		return errors.New("graph draft persistence is unavailable")
	}
	if err := validateDraftBinding(s.workspaceID, s.buildRunID, d.BuildRunID, d.AgentName); err != nil {
		return err
	}
	return s.persistence.DeleteDraft(ctx, s.workspaceID, s.buildRunID, draftKindAgentGraph, d.AgentName, d.Revision)
}

// cloneGraphDefinition deep-copies a graph so draft mutations never alias the
// loaded record or previously returned summaries.
func cloneGraphDefinition(def registry.GraphDefinition) registry.GraphDefinition {
	steps := make([]registry.StepDefinition, len(def.Steps))
	for i, step := range def.Steps {
		steps[i] = cloneStepDefinition(step)
	}
	return registry.GraphDefinition{Entry: def.Entry, Steps: steps}
}

func cloneStepDefinition(step registry.StepDefinition) registry.StepDefinition {
	cp := step
	cp.Config = cloneConfigMap(step.Config)
	if step.Next != nil {
		next := *step.Next
		cp.Next = &next
	}
	if step.Condition != nil {
		condition := *step.Condition
		if condition.TrueStep != nil {
			trueStep := *condition.TrueStep
			condition.TrueStep = &trueStep
		}
		if condition.FalseStep != nil {
			falseStep := *condition.FalseStep
			condition.FalseStep = &falseStep
		}
		cp.Condition = &condition
	}
	return cp
}

// cloneConfigMap deep-copies one JSON-derived config map. Config values are
// JSON-safe by construction (they arrive through typed tool arguments), so a
// marshal/unmarshal round trip is a correct and compact deep copy.
func cloneConfigMap(config map[string]any) map[string]any {
	if config == nil {
		return nil
	}
	data, err := json.Marshal(config)
	if err != nil {
		return config
	}
	var out map[string]any
	if err := json.Unmarshal(data, &out); err != nil {
		return config
	}
	return out
}

// graphDraftSummary is the "current draft" view returned by every graph write
// tool so the model can restore full context between calls.
type graphDraftSummary struct {
	BuildRunID     string                   `json:"build_run_id"`
	Agent          string                   `json:"agent"`
	Graph          registry.GraphDefinition `json:"graph"`
	OutputContract json.RawMessage          `json:"output_contract,omitempty"`
	StepCount      int                      `json:"step_count"`
	HasDraft       bool                     `json:"has_draft"`
}

func (d *GraphWriteToolsDispatcher) draftSummary(draft *graphDraft) graphDraftSummary {
	if draft == nil {
		return graphDraftSummary{HasDraft: false}
	}
	return graphDraftSummary{
		BuildRunID:     draft.BuildRunID,
		Agent:          draft.AgentName,
		Graph:          cloneGraphDefinition(draft.Graph),
		OutputContract: append(json.RawMessage(nil), draft.OutputContract...),
		StepCount:      len(draft.Graph.Steps),
		HasDraft:       true,
	}
}
