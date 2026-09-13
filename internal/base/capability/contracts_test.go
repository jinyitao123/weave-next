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
		Roles: []Role{{ID: "author", Name: "Author"}, {ID: "reviewer", Name: "Reviewer"}},
		Steps: []Step{
			{ID: "write", Name: "Write", RoleID: "author", Kind: StepWorker, Instruction: "write"},
			{ID: "check", Name: "Check", RoleID: "reviewer", Kind: StepWorker, Instruction: "check", MaxIterations: 2},
		},
		Relations: []Relation{{From: "write", To: "check", Kind: RelationSequence}},
	}
}

func TestDefinitionValidateRejectsUnboundedLoop(t *testing.T) {
	d := fixtureDefinition()
	d.Relations = []Relation{{From: "write", To: "check", Kind: RelationLoop}}
	d.Steps[1].MaxIterations = 0
	if err := d.Validate(); !errors.Is(err, ErrInvalidDefinition) {
		t.Fatalf("expected invalid definition, got %v", err)
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

func TestPublishUsesCanonicalDefinitionHash(t *testing.T) {
	one := fixtureDefinition()
	two := fixtureDefinition()
	one.Resources.ToolIDs = []string{"a", "z"}
	two.Resources.ToolIDs = []string{"z", "a"}
	first, err := Publish(one, 1)
	if err != nil {
		t.Fatal(err)
	}
	second, err := Publish(two, 1)
	if err != nil {
		t.Fatal(err)
	}
	if first.DefinitionHash != second.DefinitionHash {
		t.Fatal("resource order should not change the published hash")
	}
}

func TestCompileProducesStablePlanAndRejectsNonLoopCycle(t *testing.T) {
	d := fixtureDefinition()
	d.Steps[1].MaxIterations = 0
	revision, err := Publish(d, 1)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := Compile(revision)
	if err != nil || len(plan.Steps) != 2 || plan.Steps[0].ID != "write" || plan.Steps[1].ID != "check" {
		t.Fatalf("plan=%+v err=%v", plan, err)
	}
	d.Relations = []Relation{{From: "write", To: "check", Kind: RelationSequence}, {From: "check", To: "write", Kind: RelationCondition}}
	revision, err = Publish(d, 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Compile(revision); !errors.Is(err, ErrInvalidRevision) {
		t.Fatalf("expected cycle rejection, got %v", err)
	}
}
