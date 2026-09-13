// Package capability owns capability definitions, compilation, and resumable
// execution semantics. It is independent of concrete member engines and of
// product services; capabilityruntime binds its steps to execution engines.
package capability

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/jinyitao123/weave/internal/base/frozen"
)

const SchemaVersionV1 = 1

type StepKind string

const (
	StepWorker    StepKind = "worker"
	StepTransform StepKind = "transform"
	StepCondition StepKind = "condition"
	StepWait      StepKind = "wait"
	StepTool      StepKind = "tool"
	StepDeliver   StepKind = "deliver"
)

type RelationKind string

const (
	RelationSequence  RelationKind = "sequence"
	RelationParallel  RelationKind = "parallel"
	RelationCondition RelationKind = "condition"
	RelationJoin      RelationKind = "join"
	RelationLoop      RelationKind = "loop"
)

var (
	ErrInvalidDefinition = errors.New("invalid capability definition")
	ErrInvalidRevision   = errors.New("invalid published capability revision")
)

// Definition is the developer-visible, mutable capability document. Runtime,
// model, provider, and credential details are represented as requirements and
// references rather than vendor-specific configuration.
type Definition struct {
	SchemaVersion int                 `json:"schema_version"`
	CapabilityID  string              `json:"capability_id"`
	Name          string              `json:"name"`
	Description   string              `json:"description,omitempty"`
	InputSchema   json.RawMessage     `json:"input_schema"`
	OutputSchema  json.RawMessage     `json:"output_schema"`
	Roles         []Role              `json:"roles"`
	Steps         []Step              `json:"steps"`
	Relations     []Relation          `json:"relations"`
	Runtime       RuntimeRequirement  `json:"runtime"`
	Resources     ResourceRequirement `json:"resources"`
	Result        *ValueRef           `json:"result,omitempty"`
}

type Role struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
}

type Step struct {
	ID            string              `json:"id"`
	Name          string              `json:"name"`
	RoleID        string              `json:"role_id"`
	Kind          StepKind            `json:"kind"`
	Instruction   string              `json:"instruction,omitempty"`
	InputBindings map[string]ValueRef `json:"input_bindings,omitempty"`
	OutputSchema  json.RawMessage     `json:"output_schema,omitempty"`
	MaxIterations int                 `json:"max_iterations,omitempty"`
	Condition     *Predicate          `json:"condition,omitempty"`
	ToolID        string              `json:"tool_id,omitempty"`
	ApprovalTitle string              `json:"approval_title,omitempty"`
}

type ValueRef struct {
	Source  string          `json:"source"` // input | step_output | literal
	Path    string          `json:"path,omitempty"`
	StepID  string          `json:"step_id,omitempty"`
	Literal json.RawMessage `json:"literal,omitempty"`
}

// Predicate is intentionally small and portable. It is evaluated by the
// capability runner without asking a model to decide deterministic routing.
type Predicate struct {
	Left     ValueRef  `json:"left"`
	Operator string    `json:"operator"` // eq | ne | gt | gte | lt | lte | truthy | empty
	Right    *ValueRef `json:"right,omitempty"`
}

type Relation struct {
	From string       `json:"from"`
	To   string       `json:"to"`
	Kind RelationKind `json:"kind"`
	When *bool        `json:"when,omitempty"`
}

type RuntimeRequirement struct {
	Model        string   `json:"model,omitempty"`
	Engine       string   `json:"engine,omitempty"`
	Capabilities []string `json:"capabilities,omitempty"`
	Pool         string   `json:"pool,omitempty"`
}

type ResourceRequirement struct {
	// ToolIDs is retained only as draft display metadata. It never grants
	// execution authority; published tool steps require explicit Tools.
	ToolIDs        []string        `json:"tool_ids,omitempty"`
	Tools          []ToolReference `json:"tools,omitempty"`
	DataRefs       []string        `json:"data_refs,omitempty"`
	CredentialRefs []string        `json:"credential_refs,omitempty"`
}

type ToolReference struct {
	MCPServerID string `json:"mcp_server_id"`
	ToolName    string `json:"tool_name"`
}

// PublishedRevision is the immutable identity used by an invocation. The
// definition is copied at publication time; callers must not mutate it after
// construction.
type PublishedRevision struct {
	SchemaVersion  int        `json:"schema_version"`
	CapabilityID   string     `json:"capability_id"`
	Revision       int64      `json:"revision"`
	DefinitionHash string     `json:"definition_hash"`
	Definition     Definition `json:"definition"`
}

func (d Definition) Validate() error {
	if d.SchemaVersion != SchemaVersionV1 {
		return invalid("schema_version", "must be 1")
	}
	if strings.TrimSpace(d.CapabilityID) == "" {
		return invalid("capability_id", "is required")
	}
	if strings.TrimSpace(d.Name) == "" {
		return invalid("name", "is required")
	}
	if err := validateObjectSchema("input_schema", d.InputSchema); err != nil {
		return err
	}
	if err := validateObjectSchema("output_schema", d.OutputSchema); err != nil {
		return err
	}
	if len(d.Roles) == 0 || len(d.Steps) == 0 {
		return invalid("roles/steps", "must contain at least one role and step")
	}
	roles := make(map[string]struct{}, len(d.Roles))
	for i, role := range d.Roles {
		if strings.TrimSpace(role.ID) == "" || strings.TrimSpace(role.Name) == "" {
			return invalid(fmt.Sprintf("roles[%d]", i), "id and name are required")
		}
		if _, exists := roles[role.ID]; exists {
			return invalid("roles", "contains duplicate id "+role.ID)
		}
		roles[role.ID] = struct{}{}
	}
	steps := make(map[string]struct{}, len(d.Steps))
	toolServers := make(map[string]string, len(d.Resources.Tools))
	for i, ref := range d.Resources.Tools {
		if ref.MCPServerID == "" || ref.ToolName == "" || strings.TrimSpace(ref.MCPServerID) != ref.MCPServerID || strings.TrimSpace(ref.ToolName) != ref.ToolName {
			return invalid(fmt.Sprintf("resources.tools[%d]", i), "requires exact MCP server and tool names")
		}
		if _, exists := toolServers[ref.ToolName]; exists {
			return invalid("resources.tools", "contains an ambiguous tool name "+ref.ToolName)
		}
		toolServers[ref.ToolName] = ref.MCPServerID
	}
	for i, step := range d.Steps {
		if strings.TrimSpace(step.ID) == "" || strings.TrimSpace(step.Name) == "" {
			return invalid(fmt.Sprintf("steps[%d]", i), "id and name are required")
		}
		if _, exists := steps[step.ID]; exists {
			return invalid("steps", "contains duplicate id "+step.ID)
		}
		if _, exists := roles[step.RoleID]; !exists {
			return invalid("steps."+step.ID+".role_id", "references an unknown role")
		}
		switch step.Kind {
		case StepWorker, StepTransform, StepCondition, StepWait, StepTool, StepDeliver:
		default:
			return invalid("steps."+step.ID+".kind", "is unsupported")
		}
		if step.Kind == StepWorker && strings.TrimSpace(step.Instruction) == "" {
			return invalid("steps."+step.ID+".instruction", "is required for worker steps")
		}
		if step.Kind == StepCondition && step.Condition == nil {
			return invalid("steps."+step.ID+".condition", "is required for condition steps")
		}
		if step.Kind == StepTool && strings.TrimSpace(step.ToolID) == "" {
			return invalid("steps."+step.ID+".tool_id", "is required for tool steps")
		}
		if step.Kind == StepTool {
			if _, exists := toolServers[step.ToolID]; !exists {
				return invalid("steps."+step.ID+".tool_id", "requires an explicit MCP server and tool resource")
			}
		}
		if step.Kind == StepWait && strings.TrimSpace(step.ApprovalTitle) == "" {
			return invalid("steps."+step.ID+".approval_title", "is required for wait steps")
		}
		if step.MaxIterations < 0 {
			return invalid("steps."+step.ID+".max_iterations", "cannot be negative")
		}
		if step.Kind == StepWait && step.MaxIterations != 0 {
			return invalid("steps."+step.ID+".max_iterations", "is not valid for wait steps")
		}
		steps[step.ID] = struct{}{}
	}
	for i, relation := range d.Relations {
		if _, exists := steps[relation.From]; !exists {
			return invalid(fmt.Sprintf("relations[%d].from", i), "references an unknown step")
		}
		if _, exists := steps[relation.To]; !exists {
			return invalid(fmt.Sprintf("relations[%d].to", i), "references an unknown step")
		}
		switch relation.Kind {
		case RelationSequence, RelationParallel, RelationCondition, RelationJoin, RelationLoop:
		default:
			return invalid(fmt.Sprintf("relations[%d].kind", i), "is unsupported")
		}
		if relation.Kind == RelationCondition && relation.When == nil {
			return invalid(fmt.Sprintf("relations[%d].when", i), "is required for condition relations")
		}
		if relation.Kind == RelationLoop {
			bounded := false
			for _, step := range d.Steps {
				if step.ID == relation.To && step.MaxIterations > 0 {
					bounded = true
				}
			}
			if !bounded {
				return invalid(fmt.Sprintf("relations[%d]", i), "loop target must declare max_iterations")
			}
		}
	}
	if d.Result != nil {
		if err := validateValueRef(*d.Result, steps); err != nil {
			return invalid("result", err.Error())
		}
	}
	return nil
}

func validateValueRef(ref ValueRef, steps map[string]struct{}) error {
	switch ref.Source {
	case "input":
		if ref.StepID != "" {
			return errors.New("input cannot name a step")
		}
	case "step_output":
		if _, ok := steps[ref.StepID]; !ok {
			return errors.New("references an unknown step")
		}
	case "literal":
		if len(ref.Literal) == 0 || !json.Valid(ref.Literal) {
			return errors.New("literal must contain valid JSON")
		}
	default:
		return errors.New("source is unsupported")
	}
	return validPointer(ref.Path)
}

// Drafts can be incomplete while their author is editing roles and steps.
// Execution and publication continue to require the full Validate contract.
func (d Definition) ValidateDraft() error {
	if d.SchemaVersion != SchemaVersionV1 {
		return invalid("schema_version", "must be 1")
	}
	if strings.TrimSpace(d.CapabilityID) == "" || strings.TrimSpace(d.Name) == "" {
		return invalid("capability", "id and name are required")
	}
	if err := validateObjectSchema("input_schema", d.InputSchema); err != nil {
		return err
	}
	if err := validateObjectSchema("output_schema", d.OutputSchema); err != nil {
		return err
	}
	if len(d.Steps) > 32 || len(d.Roles) > 64 {
		return invalid("definition", "exceeds draft size limits")
	}
	return nil
}

func Publish(d Definition, revision int64) (PublishedRevision, error) {
	// Detach every slice, map and RawMessage from the mutable draft.
	encoded, err := json.Marshal(d)
	if err != nil {
		return PublishedRevision{}, fmt.Errorf("%w: encode definition: %v", ErrInvalidDefinition, err)
	}
	var copied Definition
	if err := json.Unmarshal(encoded, &copied); err != nil {
		return PublishedRevision{}, err
	}
	d = copied
	d.Resources.ToolIDs = append([]string(nil), d.Resources.ToolIDs...)
	d.Resources.Tools = append([]ToolReference(nil), d.Resources.Tools...)
	d.Resources.DataRefs = append([]string(nil), d.Resources.DataRefs...)
	d.Resources.CredentialRefs = append([]string(nil), d.Resources.CredentialRefs...)
	SortResourceIDs(&d.Resources)
	if err := d.Validate(); err != nil {
		return PublishedRevision{}, err
	}
	if revision < 1 || revision > frozen.MaxJCSSafeInteger {
		return PublishedRevision{}, fmt.Errorf("%w: revision must be positive", ErrInvalidRevision)
	}
	definitionHash, err := frozen.HashCanonicalJSON(encodedDefinition(d))
	if err != nil {
		return PublishedRevision{}, fmt.Errorf("%w: definition canonicalization: %v", ErrInvalidRevision, err)
	}
	return PublishedRevision{SchemaVersion: SchemaVersionV1, CapabilityID: d.CapabilityID, Revision: revision, DefinitionHash: definitionHash, Definition: d}, nil
}

func (r PublishedRevision) Validate() error {
	if r.SchemaVersion != SchemaVersionV1 || r.Revision < 1 || r.CapabilityID == "" {
		return ErrInvalidRevision
	}
	if r.Definition.CapabilityID != r.CapabilityID {
		return fmt.Errorf("%w: capability id mismatch", ErrInvalidRevision)
	}
	if err := r.Definition.Validate(); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidRevision, err)
	}
	raw, err := json.Marshal(r.Definition)
	if err != nil {
		return fmt.Errorf("%w: malformed definition", ErrInvalidRevision)
	}
	want, err := frozen.HashCanonicalJSON(raw)
	if err != nil || want != r.DefinitionHash {
		return fmt.Errorf("%w: definition hash mismatch", ErrInvalidRevision)
	}
	return nil
}

func invalid(path, message string) error {
	return fmt.Errorf("%w: %s %s", ErrInvalidDefinition, path, message)
}

func validateObjectSchema(path string, raw json.RawMessage) error {
	if len(raw) == 0 {
		return invalid(path, "is required")
	}
	var value map[string]any
	if err := json.Unmarshal(raw, &value); err != nil || value == nil {
		return invalid(path, "must be a JSON object")
	}
	return nil
}

func encodedDefinition(value Definition) []byte {
	// Only used after Publish has successfully marshaled and detached the DTO.
	encoded, _ := json.Marshal(value)
	return encoded
}

// SortResourceIDs provides deterministic ordering for callers before hashing
// definitions that treat resource references as sets.
func SortResourceIDs(requirement *ResourceRequirement) {
	if requirement == nil {
		return
	}
	sort.Strings(requirement.ToolIDs)
	sort.Slice(requirement.Tools, func(i, j int) bool {
		if requirement.Tools[i].MCPServerID == requirement.Tools[j].MCPServerID {
			return requirement.Tools[i].ToolName < requirement.Tools[j].ToolName
		}
		return requirement.Tools[i].MCPServerID < requirement.Tools[j].MCPServerID
	})
	sort.Strings(requirement.DataRefs)
	sort.Strings(requirement.CredentialRefs)
}
