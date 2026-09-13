package capability

import (
	"encoding/json"
	"fmt"
	"sort"
)

// Plan is the immutable execution view consumed by runtime adapters. It keeps
// developer semantics while removing mutable draft concerns.
type Plan struct {
	Runtime        RuntimeRequirement
	InputSchema    json.RawMessage
	OutputSchema   json.RawMessage
	CapabilityID   string
	Revision       int64
	DefinitionHash string
	Steps          []PlanStep
}

type PlanStep struct {
	RoleName        string
	RoleDescription string
	Dependencies    []string
	InputBindings   map[string]ValueRef
	OutputSchema    json.RawMessage
	ID              string
	RoleID          string
	Kind            StepKind
	Instruction     string
	MaxIterations   int
}

func Compile(revision PublishedRevision) (Plan, error) {
	if err := revision.Validate(); err != nil {
		return Plan{}, err
	}
	definition := revision.Definition
	roles := map[string]Role{}
	for _, role := range definition.Roles {
		roles[role.ID] = role
	}
	if len(definition.Steps) > 32 {
		return Plan{}, fmt.Errorf("%w: maximum 32 steps", ErrInvalidRevision)
	}
	if len(definition.Resources.ToolIDs)+len(definition.Resources.DataRefs)+len(definition.Resources.CredentialRefs) != 0 {
		return Plan{}, fmt.Errorf("%w: resource execution is not connected", ErrInvalidRevision)
	}
	for _, schema := range []json.RawMessage{definition.InputSchema, definition.OutputSchema} {
		if _, err := compileSchema(schema); err != nil {
			return Plan{}, fmt.Errorf("%w: %v", ErrInvalidRevision, err)
		}
	}
	dependencies := map[string][]string{}
	steps := make(map[string]Step, len(definition.Steps))
	for _, step := range definition.Steps {
		if step.Kind != StepWorker && step.Kind != StepTransform && step.Kind != StepDeliver {
			return Plan{}, fmt.Errorf("%w: step kind %s is not executable yet", ErrInvalidRevision, step.Kind)
		}
		if step.Kind != StepWorker && step.Instruction != "" {
			return Plan{}, fmt.Errorf("%w: transform and deliver require input bindings without instructions", ErrInvalidRevision)
		}
		if step.MaxIterations != 0 {
			return Plan{}, fmt.Errorf("%w: iterative execution is not connected", ErrInvalidRevision)
		}
		if len(step.OutputSchema) > 0 {
			if _, err := compileSchema(step.OutputSchema); err != nil {
				return Plan{}, fmt.Errorf("%w: step %s schema: %v", ErrInvalidRevision, step.ID, err)
			}
		}
		steps[step.ID] = step
	}
	indegree := make(map[string]int, len(steps))
	adjacency := make(map[string][]string, len(steps))
	for id := range steps {
		indegree[id] = 0
	}
	for _, relation := range definition.Relations {
		if relation.Kind == RelationLoop || relation.Kind == RelationCondition {
			return Plan{}, fmt.Errorf("%w: control relation %s is not executable yet", ErrInvalidRevision, relation.Kind)
		}
		dependencies[relation.To] = append(dependencies[relation.To], relation.From)
		adjacency[relation.From] = append(adjacency[relation.From], relation.To)
		indegree[relation.To]++
	}
	ready := make([]string, 0)
	for id, degree := range indegree {
		if degree == 0 {
			ready = append(ready, id)
		}
	}
	sort.Strings(ready)
	ordered := make([]string, 0, len(steps))
	for len(ready) > 0 {
		id := ready[0]
		ready = ready[1:]
		ordered = append(ordered, id)
		neighbors := append([]string(nil), adjacency[id]...)
		sort.Strings(neighbors)
		for _, next := range neighbors {
			indegree[next]--
			if indegree[next] == 0 {
				ready = append(ready, next)
				sort.Strings(ready)
			}
		}
	}
	if len(ordered) != len(steps) {
		return Plan{}, fmt.Errorf("%w: non-loop relations contain a cycle", ErrInvalidRevision)
	}
	plan := Plan{Runtime: definition.Runtime, InputSchema: definition.InputSchema, OutputSchema: definition.OutputSchema, CapabilityID: revision.CapabilityID, Revision: revision.Revision, DefinitionHash: revision.DefinitionHash, Steps: make([]PlanStep, 0, len(ordered))}
	ancestors := map[string]map[string]bool{}
	for _, id := range ordered {
		step := steps[id]
		ancestors[id] = map[string]bool{}
		for _, dep := range dependencies[id] {
			ancestors[id][dep] = true
			for ancestor := range ancestors[dep] {
				ancestors[id][ancestor] = true
			}
		}
		for _, ref := range step.InputBindings {
			if ref.Source != "input" && ref.Source != "step_output" {
				return Plan{}, fmt.Errorf("%w: unsupported input source", ErrInvalidRevision)
			}
			if ref.Source == "step_output" && !ancestors[id][ref.StepID] {
				return Plan{}, fmt.Errorf("%w: input must reference an ancestor", ErrInvalidRevision)
			}
			if ref.Source == "input" && ref.StepID != "" {
				return Plan{}, fmt.Errorf("%w: run input cannot specify a step", ErrInvalidRevision)
			}
			if err := validPointer(ref.Path); err != nil {
				return Plan{}, err
			}
		}
		plan.Steps = append(plan.Steps, PlanStep{RoleName: roles[step.RoleID].Name, RoleDescription: roles[step.RoleID].Description, Dependencies: dependencies[id], InputBindings: step.InputBindings, OutputSchema: step.OutputSchema, ID: step.ID, RoleID: step.RoleID, Kind: step.Kind, Instruction: step.Instruction})
	}
	return plan, nil
}
