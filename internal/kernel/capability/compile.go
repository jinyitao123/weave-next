package capability

import (
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"
)

// Plan is the immutable execution view consumed by runtime adapters.
type Plan struct {
	Runtime        RuntimeRequirement
	Resources      ResourceRequirement
	InputSchema    json.RawMessage
	OutputSchema   json.RawMessage
	Result         *ValueRef
	CapabilityID   string
	Revision       int64
	DefinitionHash string
	Steps          []PlanStep
	Relations      []Relation
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
	Condition       *Predicate
	ToolID          string
	ApprovalTitle   string
}

func Compile(revision PublishedRevision) (Plan, error) {
	if err := revision.Validate(); err != nil {
		return Plan{}, err
	}
	definition := revision.Definition
	if len(definition.Steps) > 32 {
		return Plan{}, fmt.Errorf("%w: maximum 32 steps", ErrInvalidRevision)
	}
	for _, schema := range []json.RawMessage{definition.InputSchema, definition.OutputSchema} {
		if _, err := compileSchema(schema); err != nil {
			return Plan{}, fmt.Errorf("%w: %v", ErrInvalidRevision, err)
		}
	}
	roles := map[string]Role{}
	steps := map[string]Step{}
	for _, role := range definition.Roles {
		roles[role.ID] = role
	}
	for _, step := range definition.Steps {
		if len(step.OutputSchema) > 0 {
			if _, err := compileSchema(step.OutputSchema); err != nil {
				return Plan{}, fmt.Errorf("%w: step %s schema: %v", ErrInvalidRevision, step.ID, err)
			}
		}
		if step.Kind == StepTool && !slices.ContainsFunc(definition.Resources.Tools, func(ref ToolReference) bool { return ref.ToolName == step.ToolID }) {
			return Plan{}, fmt.Errorf("%w: tool %s is not declared", ErrInvalidRevision, step.ToolID)
		}
		steps[step.ID] = step
	}
	dependencies := map[string][]string{}
	adjacency := map[string][]string{}
	indegree := map[string]int{}
	for id := range steps {
		indegree[id] = 0
	}
	for _, relation := range definition.Relations {
		if relation.Kind == RelationLoop {
			continue
		}
		dependencies[relation.To] = appendUnique(dependencies[relation.To], relation.From)
		adjacency[relation.From] = appendUnique(adjacency[relation.From], relation.To)
		indegree[relation.To]++
	}
	ready := []string{}
	for id, degree := range indegree {
		if degree == 0 {
			ready = append(ready, id)
		}
	}
	sort.Strings(ready)
	ordered := []string{}
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
	position := map[string]int{}
	for i, id := range ordered {
		position[id] = i
	}
	for _, relation := range definition.Relations {
		if relation.Kind == RelationLoop && position[relation.To] > position[relation.From] {
			return Plan{}, fmt.Errorf("%w: loop must point to an earlier step", ErrInvalidRevision)
		}
		if relation.Kind == RelationCondition && steps[relation.From].Kind != StepCondition {
			return Plan{}, fmt.Errorf("%w: condition relation must start at a condition step", ErrInvalidRevision)
		}
	}
	plan := Plan{Runtime: definition.Runtime, Resources: definition.Resources, InputSchema: definition.InputSchema, OutputSchema: definition.OutputSchema, Result: cloneRef(definition.Result), CapabilityID: revision.CapabilityID, Revision: revision.Revision, DefinitionHash: revision.DefinitionHash, Relations: append([]Relation(nil), definition.Relations...)}
	ancestors := map[string]map[string]bool{}
	for _, id := range ordered {
		step := steps[id]
		ancestors[id] = map[string]bool{}
		for _, dep := range dependencies[id] {
			ancestors[id][dep] = true
			for a := range ancestors[dep] {
				ancestors[id][a] = true
			}
		}
		for _, ref := range step.InputBindings {
			if err := validatePlanRef(ref, id, ancestors, steps); err != nil {
				return Plan{}, err
			}
		}
		if step.Condition != nil {
			if err := validatePlanRef(step.Condition.Left, id, ancestors, steps); err != nil {
				return Plan{}, err
			}
			if step.Condition.Right != nil {
				if err := validatePlanRef(*step.Condition.Right, id, ancestors, steps); err != nil {
					return Plan{}, err
				}
			}
			switch step.Condition.Operator {
			case "eq", "ne", "gt", "gte", "lt", "lte", "truthy", "empty":
			default:
				return Plan{}, fmt.Errorf("%w: unsupported predicate operator", ErrInvalidRevision)
			}
		}
		role := roles[step.RoleID]
		plan.Steps = append(plan.Steps, PlanStep{RoleName: role.Name, RoleDescription: role.Description, Dependencies: append([]string(nil), dependencies[id]...), InputBindings: step.InputBindings, OutputSchema: step.OutputSchema, ID: step.ID, RoleID: step.RoleID, Kind: step.Kind, Instruction: step.Instruction, MaxIterations: step.MaxIterations, Condition: step.Condition, ToolID: step.ToolID, ApprovalTitle: step.ApprovalTitle})
	}
	if plan.Result == nil {
		for i := len(plan.Steps) - 1; i >= 0; i-- {
			if plan.Steps[i].Kind == StepDeliver {
				plan.Result = &ValueRef{Source: "step_output", StepID: plan.Steps[i].ID}
				break
			}
		}
	}
	return plan, nil
}

func validatePlanRef(ref ValueRef, current string, ancestors map[string]map[string]bool, steps map[string]Step) error {
	if err := validPointer(ref.Path); err != nil {
		return err
	}
	switch ref.Source {
	case "input":
		if ref.StepID != "" {
			return fmt.Errorf("%w: run input cannot specify a step", ErrInvalidRevision)
		}
	case "step_output":
		if ref.StepID != current && !ancestors[current][ref.StepID] {
			return fmt.Errorf("%w: input must reference an ancestor", ErrInvalidRevision)
		}
	case "literal":
		if len(ref.Literal) == 0 || !json.Valid(ref.Literal) {
			return fmt.Errorf("%w: invalid literal", ErrInvalidRevision)
		}
	default:
		return fmt.Errorf("%w: unsupported input source", ErrInvalidRevision)
	}
	return nil
}
func cloneRef(ref *ValueRef) *ValueRef {
	if ref == nil {
		return nil
	}
	c := *ref
	c.Literal = append(json.RawMessage(nil), ref.Literal...)
	return &c
}
func appendUnique(values []string, value string) []string {
	for _, v := range values {
		if v == value {
			return values
		}
	}
	return append(values, value)
}
func containsString(values []string, value string) bool {
	value = strings.TrimSpace(value)
	for _, v := range values {
		if strings.TrimSpace(v) == value {
			return true
		}
	}
	return false
}
