package teamforge

// Employee internal-graph draft carrier (plan §10.2.2). The AgentRecord has
// no draft state machine, so this package owns one in dispatcher memory:
// drafts are keyed by build_run_id + agent name, step operations accumulate
// on the draft, and only tf_graph_commit validates the whole graph and
// PutTx's a new immutable agent version. Uncommitted drafts never touch the
// database.

import (
	"encoding/json"
	"sync"

	"github.com/jinyitao123/weave/internal/kernel/registry"
)

// graphDraft is one in-memory working copy of an employee internal graph
// together with its output contract. It is a plain value type; the owning
// store replaces the whole value on every successful mutation.
type graphDraft struct {
	BuildRunID     string
	AgentName      string
	Graph          registry.GraphDefinition
	OutputContract json.RawMessage
}

// graphDraftStore is the dispatcher-owned in-memory draft table. Drafts are
// deliberately not persisted: an uncommitted draft has no platform
// representation, and the model re-runs tf_graph_begin to restore context.
type graphDraftStore struct {
	mu     sync.Mutex
	drafts map[string]*graphDraft
}

func newGraphDraftStore() *graphDraftStore {
	return &graphDraftStore{drafts: make(map[string]*graphDraft)}
}

// graphDraftKey binds one draft to its build run and target agent.
func graphDraftKey(buildRunID, agentName string) string {
	return buildRunID + ":" + agentName
}

func (s *graphDraftStore) get(buildRunID, agentName string) *graphDraft {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.drafts[graphDraftKey(buildRunID, agentName)]
}

func (s *graphDraftStore) put(d *graphDraft) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.drafts[graphDraftKey(d.BuildRunID, d.AgentName)] = d
}

func (s *graphDraftStore) delete(buildRunID, agentName string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.drafts, graphDraftKey(buildRunID, agentName))
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
