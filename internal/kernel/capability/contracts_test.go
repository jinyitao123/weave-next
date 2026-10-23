package capability

import (
	"encoding/json"
	"errors"
	"testing"
)

func fixtureDefinition() Definition {
	return Definition{
		SchemaVersion: 1, CapabilityID: "cap-review", Name: "Review",
		InputSchema: json.RawMessage(`{"type":"object"}`), OutputSchema: json.RawMessage(`{"type":"object"}`),
		Workflow: &WorkflowReference{WorkflowID: "review-flow", WorkflowVersion: 1},
	}
}

func TestDefinitionRequiresAPublishedWorkflow(t *testing.T) {
	inline := fixtureDefinition()
	inline.Workflow = nil
	inline.Roles = []Role{{ID: "author", Name: "Author"}}
	inline.Steps = []Step{{ID: "write", Name: "Write", RoleID: "author", Kind: StepWorker, Instruction: "write"}}
	if err := inline.Validate(); !errors.Is(err, ErrInvalidDefinition) {
		t.Fatalf("inline definition validated: %v", err)
	}
	if err := inline.ValidateDraft(); !errors.Is(err, ErrInvalidDefinition) {
		t.Fatalf("inline draft validated: %v", err)
	}
	mixed := fixtureDefinition()
	mixed.Steps = inline.Steps
	if err := mixed.Validate(); !errors.Is(err, ErrInvalidDefinition) {
		t.Fatalf("workflow combined with inline steps validated: %v", err)
	}
	if err := fixtureDefinition().Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestPublishFreezesDefinitionHash(t *testing.T) {
	d := fixtureDefinition()
	revision, err := Publish(d, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := revision.Validate(); err != nil {
		t.Fatal(err)
	}
	revision.Definition.Name = "changed"
	if err := revision.Validate(); !errors.Is(err, ErrInvalidRevision) {
		t.Fatalf("expected frozen hash mismatch, got %v", err)
	}
}

func TestPublishedDefinitionDetachesDraft(t *testing.T) {
	d := fixtureDefinition()
	r, err := Publish(d, 1)
	if err != nil {
		t.Fatal(err)
	}
	d.InputSchema[0] = '!'
	d.Workflow.WorkflowVersion = 2
	if err := r.Validate(); err != nil || r.Definition.Workflow.WorkflowVersion != 1 {
		t.Fatalf("draft mutation changed publication: %v", err)
	}
}
