package capability

import (
	"fmt"
	"sort"
)

// Plan is the immutable execution view consumed by runtime adapters. It keeps
// developer semantics while removing mutable draft concerns.
type Plan struct {
	CapabilityID   string
	Revision       int64
	DefinitionHash string
	Steps          []PlanStep
}

type PlanStep struct {
	ID            string
	RoleID        string
	Kind          StepKind
	Instruction   string
	MaxIterations int
}

func Compile(revision PublishedRevision) (Plan, error) {
	if err := revision.Validate(); err != nil {
		return Plan{}, err
	}
	definition := revision.Definition
	steps := make(map[string]Step, len(definition.Steps))
	for _, step := range definition.Steps {
		steps[step.ID] = step
	}
	indegree := make(map[string]int, len(steps))
	adjacency := make(map[string][]string, len(steps))
	for id := range steps {
		indegree[id] = 0
	}
	for _, relation := range definition.Relations {
		if relation.Kind == RelationLoop {
			continue
		}
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
	plan := Plan{CapabilityID: revision.CapabilityID, Revision: revision.Revision, DefinitionHash: revision.DefinitionHash, Steps: make([]PlanStep, 0, len(ordered))}
	for _, id := range ordered {
		step := steps[id]
		plan.Steps = append(plan.Steps, PlanStep{ID: step.ID, RoleID: step.RoleID, Kind: step.Kind, Instruction: step.Instruction, MaxIterations: step.MaxIterations})
	}
	return plan, nil
}
