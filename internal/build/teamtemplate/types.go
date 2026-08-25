// Package teamtemplate compiles the product-facing team template document
// into the immutable planning documents consumed by teambuild. It is a pure
// compiler: this package has no persistence or runtime dependencies.
package teamtemplate

import (
	"fmt"
	"sort"
	"strings"

	"github.com/jinyitao123/weave/internal/build/teambuild"
)

const (
	SchemaV1     = "team-template/v1"
	MaxYAMLBytes = 64 * 1024
	MaxYAMLDepth = 10
)

// Template is the complete team-template/v1 product contract. Business
// fields are required; model and execution policy are optional and receive
// deterministic platform defaults during compilation.
type Template struct {
	Schema             string             `yaml:"schema" json:"schema"`
	Name               string             `yaml:"name" json:"name"`
	DisplayName        string             `yaml:"display_name" json:"display_name"`
	Purpose            string             `yaml:"purpose" json:"purpose"`
	Template           string             `yaml:"template" json:"template"`
	TemplateParameters TemplateParameters `yaml:"template_parameters" json:"template_parameters"`
	Members            []Member           `yaml:"members" json:"members"`
	Lead               string             `yaml:"lead" json:"lead"`
	Delivery           Delivery           `yaml:"delivery" json:"delivery"`
	Budget             Budget             `yaml:"budget" json:"budget"`
}

// TemplateParameters maps one-for-one to the existing compact_blueprint
// template_parameters surface.
type TemplateParameters struct {
	LeadInstruction    string            `yaml:"lead_instruction" json:"lead_instruction"`
	PrimaryRef         string            `yaml:"primary_ref,omitempty" json:"primary_ref,omitempty"`
	ReviewerRef        string            `yaml:"reviewer_ref,omitempty" json:"reviewer_ref,omitempty"`
	ParallelWorkerRefs []string          `yaml:"parallel_worker_refs,omitempty" json:"parallel_worker_refs,omitempty"`
	FinalizerRef       string            `yaml:"finalizer_ref,omitempty" json:"finalizer_ref,omitempty"`
	MaxIterations      *int              `yaml:"max_iterations,omitempty" json:"max_iterations,omitempty"`
	ResultRequirements map[string]string `yaml:"result_requirements" json:"result_requirements"`
}

// Member maps one-for-one to a compact_blueprint member, except stable_ref
// and management_mode are server-owned derivations: stable_ref is name and
// create-mode management is always managed.
type Member struct {
	Name             string           `yaml:"name" json:"name"`
	DisplayName      string           `yaml:"display_name" json:"display_name"`
	Role             string           `yaml:"role" json:"role"`
	Responsibilities []string         `yaml:"responsibilities" json:"responsibilities"`
	Capabilities     []string         `yaml:"capabilities" json:"capabilities"`
	ModelRef         string           `yaml:"model_ref,omitempty" json:"model_ref,omitempty"`
	ExecutionPolicy  *ExecutionPolicy `yaml:"execution_policy,omitempty" json:"execution_policy,omitempty"`
}

// ExecutionPolicy maps one-for-one to BlueprintExecutionPolicyV1 while
// retaining explicit YAML field names.
type ExecutionPolicy struct {
	EngineClass      string `yaml:"engine_class" json:"engine_class"`
	Engine           string `yaml:"engine,omitempty" json:"engine,omitempty"`
	ExecutionMode    string `yaml:"execution_mode" json:"execution_mode"`
	RuntimeRef       string `yaml:"runtime_ref,omitempty" json:"runtime_ref,omitempty"`
	InternalGraphRef string `yaml:"internal_graph_ref,omitempty" json:"internal_graph_ref,omitempty"`
}

type Delivery struct {
	SuccessCriteria []string `yaml:"success_criteria" json:"success_criteria"`
}

type Budget struct {
	MaxCostUSD float64 `yaml:"max_cost_usd" json:"max_cost_usd"`
}

// Compilation is the complete deterministic output of the template compiler.
// Callers persist these documents only through the teamforge compiler/applier.
type Compilation struct {
	Template  Template                     `json:"template"`
	Brief     teambuild.BuildBrief         `json:"brief"`
	Contract  teambuild.EvaluationContract `json:"contract"`
	Blueprint teambuild.TeamBlueprintV1    `json:"blueprint"`
}

// Problem is one stable field-oriented parse or schema violation.
type Problem struct {
	Path    string `json:"path"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

// ValidationError exposes all independently detectable template problems.
type ValidationError struct {
	Problems []Problem `json:"problems"`
}

func (e *ValidationError) Error() string {
	if e == nil || len(e.Problems) == 0 {
		return "team template validation failed"
	}
	first := e.Problems[0]
	return fmt.Sprintf("team template validation failed with %d problem(s): %s: %s", len(e.Problems), first.Path, first.Message)
}

type problemCollector struct {
	items []Problem
}

func (c *problemCollector) add(path, code, message string) {
	if path == "" {
		path = "/"
	}
	c.items = append(c.items, Problem{Path: path, Code: code, Message: message})
}

func (c *problemCollector) addBlueprintProblems(err *teambuild.BlueprintValidationError) {
	for _, problem := range err.Problems {
		c.add(templatePathForBlueprintPath(problem.Path), problem.Code, problem.Message)
	}
}

func templatePathForBlueprintPath(path string) string {
	switch {
	case path == "/new_team_name":
		return "/name"
	case path == "/lead_ref":
		return "/lead"
	case path == "/workflow/template":
		return "/template"
	case strings.HasPrefix(path, "/workflow/template_parameters"):
		return strings.TrimPrefix(path, "/workflow")
	case strings.HasPrefix(path, "/members/") && strings.HasSuffix(path, "/stable_ref"):
		return strings.TrimSuffix(path, "/stable_ref") + "/name"
	default:
		return path
	}
}

func (c *problemCollector) err() error {
	if len(c.items) == 0 {
		return nil
	}
	items := append([]Problem(nil), c.items...)
	sort.SliceStable(items, func(i, j int) bool {
		if items[i].Path != items[j].Path {
			return items[i].Path < items[j].Path
		}
		if items[i].Code != items[j].Code {
			return items[i].Code < items[j].Code
		}
		return items[i].Message < items[j].Message
	})
	return &ValidationError{Problems: items}
}

func jsonPointerSegment(value string) string {
	value = strings.ReplaceAll(value, "~", "~0")
	return strings.ReplaceAll(value, "/", "~1")
}
