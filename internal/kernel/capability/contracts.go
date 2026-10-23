// Package capability owns capability definitions: a published contract that
// pins one already-published workflow, plus its input and output schemas.
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
	Workflow      *WorkflowReference  `json:"workflow,omitempty"`
	Roles         []Role              `json:"roles"`
	Steps         []Step              `json:"steps"`
	Relations     []Relation          `json:"relations"`
	Runtime       RuntimeRequirement  `json:"runtime"`
	Resources     ResourceRequirement `json:"resources"`
	Result        *ValueRef           `json:"result,omitempty"`
}

// WorkflowReference pins a capability revision to one already-published
// workflow. The workflow remains the sole execution definition.
type WorkflowReference struct {
	WorkflowID      string `json:"workflow_id"`
	WorkflowVersion int    `json:"workflow_version"`
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
	return validateWorkflowReference(d)
}

// Drafts may differ from a publishable definition only in what Validate adds
// later; both require the workflow reference.
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
	return validateWorkflowReference(d)
}

// Inline roles, steps and relations were retired: every capability runs one
// already-published workflow. The inline fields stay on Definition, always
// empty, because published revisions hash the whole document.
func validateWorkflowReference(d Definition) error {
	if d.Workflow == nil || strings.TrimSpace(d.Workflow.WorkflowID) == "" ||
		strings.TrimSpace(d.Workflow.WorkflowID) != d.Workflow.WorkflowID || d.Workflow.WorkflowVersion < 1 {
		return invalid("workflow", "workflow_id and positive workflow_version are required")
	}
	if len(d.Roles) != 0 || len(d.Steps) != 0 || len(d.Relations) != 0 || d.Result != nil {
		return invalid("workflow", "cannot be combined with inline roles, steps, relations, or result")
	}
	if d.Runtime.Model != "" || d.Runtime.Engine != "" || len(d.Runtime.Capabilities) != 0 || d.Runtime.Pool != "" ||
		len(d.Resources.ToolIDs) != 0 || len(d.Resources.Tools) != 0 ||
		len(d.Resources.DataRefs) != 0 || len(d.Resources.CredentialRefs) != 0 {
		return invalid("workflow", "cannot be combined with inline runtime or resources")
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
