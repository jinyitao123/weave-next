package teamforge

// Employee internal-graph validator compatibility surface (校验器复用纪律).
// The complete check list lives in internal/teameval/graph_validate.go as
// teameval.ValidateEmployeeGraph; this package re-exports the types and
// codes and delegates the function so the teamforge tool layer and its tests
// keep one implementation (no second copy).

import (
	"github.com/jinyitao123/weave/internal/kernel/registry"
	"github.com/jinyitao123/weave/internal/build/teameval"
)

// Stable validator codes. The codes are the machine contract between the
// validator and the model: messages and hints may evolve, codes must not.
const (
	GraphCodeEntryMissing           = teameval.GraphCodeEntryMissing
	GraphCodeEntryTargetMissing     = teameval.GraphCodeEntryTargetMissing
	GraphCodeStepNameRequired       = teameval.GraphCodeStepNameRequired
	GraphCodeDuplicateStepName      = teameval.GraphCodeDuplicateStepName
	GraphCodeUnknownStepType        = teameval.GraphCodeUnknownStepType
	GraphCodeWorkerForbidden        = teameval.GraphCodeWorkerForbidden
	GraphCodeNextConditionExclusive = teameval.GraphCodeNextConditionExclusive
	GraphCodeNextTargetMissing      = teameval.GraphCodeNextTargetMissing
	GraphCodeConditionTargetMissing = teameval.GraphCodeConditionTargetMissing
	GraphCodeStepUnreachable        = teameval.GraphCodeStepUnreachable
	GraphCodeStepMissingOutEdge     = teameval.GraphCodeStepMissingOutEdge
	GraphCodeSelfLoop               = teameval.GraphCodeSelfLoop
	GraphCodeCycleWithoutCondition  = teameval.GraphCodeCycleWithoutCondition
	GraphCodeConditionKeyUnprovable = teameval.GraphCodeConditionKeyUnprovable
	GraphCodeStepConfigInvalid      = teameval.GraphCodeStepConfigInvalid

	GraphCodeWarningNoInputExtraction = teameval.GraphCodeWarningNoInputExtraction
	GraphCodeWarningNoOutput          = teameval.GraphCodeWarningNoOutput
	GraphCodeWarningNoSelfCheck       = teameval.GraphCodeWarningNoSelfCheck
)

// GraphProblem is one structured validation finding. Path locates the field
// (graph.entry, graph.steps[<name>].config.<key>, ...), Step names the owning
// step when there is one, and Hint is the repair guidance for the model.
type GraphProblem = teameval.GraphProblem

// GraphValidation is the full validator result: errors block a commit,
// warnings only enter the report.
type GraphValidation = teameval.GraphValidation

// validateEmployeeGraph runs the shared employee internal-graph check list
// over one graph definition.
func validateEmployeeGraph(def *registry.GraphDefinition) GraphValidation {
	return teameval.ValidateEmployeeGraph(def)
}
