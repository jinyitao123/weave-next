package deliverable

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jinyitao123/weave/internal/base/fileartifact"
)

func fixtureContract() *DeliveryContract {
	return &DeliveryContract{Version: 1, Coverage: CoverageExplicit, Output: OutputRequirement{Type: "text"},
		RequiredArtifacts: []ArtifactRequirement{{ID: "report", Path: "report.md", ContentType: "text/markdown", Contains: []string{"verified content"}}},
		RequiredChecks:    []CheckSpec{{ID: "effects", VerifierID: "fixture.effects", VerifierVersion: "v1"}}, ExternalEffectsCheckID: "effects"}
}

func fixtureEffectsRegistry(t *testing.T, status VerificationStatus) *VerifierRegistry {
	t.Helper()
	registry := NewVerifierRegistry()
	if err := registry.RegisterExternalEffects("fixture.effects", "v1", func(context.Context, VerificationInput) (CheckResult, error) {
		return CheckResult{Status: status, Reason: "observed_external_state", Evidence: json.RawMessage(`{"scope":"isolated-fixture","complete":true,"unexpected":[]}`)}, nil
	}); err != nil {
		t.Fatal(err)
	}
	return registry
}

func fixtureOutputs() []WorkflowOutput {
	source := ArtifactSource{TaskID: "task-1", ParentRunID: "run-1", RunSnapshotID: "snapshot-1", ResultDigest: strings.Repeat("a", 64)}
	base := WorkflowOutput{WorkspaceID: "workspace-1", RunID: "run-1", RunSnapshotID: "snapshot-1", NodeID: "deliver", NodeType: "deliver", Final: true}
	file, summary := base, base
	file.Artifact = &WorkflowArtifact{Path: "report.md", ContentType: "text/markdown", Content: "verified content", Sources: []ArtifactSource{source}}
	summary.Output = "PASS"
	summary.Sources = []ArtifactSource{source}
	return []WorkflowOutput{file, summary}
}

func fixtureCandidate(t *testing.T, outputs []WorkflowOutput) Candidate {
	t.Helper()
	candidate, err := BuildCandidate(outputs)
	if err != nil {
		t.Fatal(err)
	}
	return candidate
}

func TestVerificationFourOutcomes(t *testing.T) {
	for _, scenario := range []struct {
		name                      string
		missing, unchecked, extra bool
		want                      VerificationStatus
	}{
		{"missing required file", true, false, false, VerificationFailed},
		{"PASS text with unchecked condition", false, true, false, VerificationUnknown},
		{"benchmark full score with unexpected write", false, false, true, VerificationFailed},
		{"all required conditions observed", false, false, false, VerificationPassed},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			contract := fixtureContract()
			outputs := fixtureOutputs()
			status := VerificationPassed
			if scenario.missing {
				outputs = outputs[1:]
			}
			if scenario.unchecked {
				contract.RequiredChecks = append(contract.RequiredChecks, CheckSpec{ID: "professional_review", VerifierID: "not-installed", VerifierVersion: "v1"})
			}
			if scenario.extra {
				status = VerificationFailed
			}
			report, err := Verify(context.Background(), contract, fixtureCandidate(t, outputs), fixtureEffectsRegistry(t, status))
			if err != nil {
				t.Fatal(err)
			}
			if report.Status != scenario.want {
				t.Fatalf("got %s, want %s: %+v", report.Status, scenario.want, report.Checks)
			}
		})
	}
}

func TestVerificationCannotInferCoverageOrEffects(t *testing.T) {
	candidate := fixtureCandidate(t, fixtureOutputs())
	for _, scenario := range []string{"missing contract", "incomplete contract", "non-effects adapter", "wrong adapter version", "empty evidence", "missing summary source"} {
		t.Run(scenario, func(t *testing.T) {
			contract := fixtureContract()
			registry := fixtureEffectsRegistry(t, VerificationPassed)
			current := candidate
			switch scenario {
			case "missing contract":
				contract = nil
			case "incomplete contract":
				contract.Coverage = CoverageIncomplete
			case "non-effects adapter":
				registry = NewVerifierRegistry()
				_ = registry.Register("fixture.effects", "v1", func(context.Context, VerificationInput) (CheckResult, error) {
					return CheckResult{Status: VerificationPassed, Evidence: json.RawMessage(`{"schema":true}`)}, nil
				})
			case "wrong adapter version":
				contract.RequiredChecks[0].VerifierVersion = "v2"
			case "empty evidence":
				registry = NewVerifierRegistry()
				_ = registry.RegisterExternalEffects("fixture.effects", "v1", func(context.Context, VerificationInput) (CheckResult, error) {
					return CheckResult{Status: VerificationPassed}, nil
				})
			case "missing summary source":
				current.OutputSources = nil
			}
			report, err := Verify(context.Background(), contract, current, registry)
			if err != nil {
				t.Fatal(err)
			}
			if report.Status != VerificationUnknown {
				t.Fatalf("got %s: %+v", report.Status, report.Checks)
			}
		})
	}
}

func TestVerificationCollectionLimitsAreSpecific(t *testing.T) {
	for _, scenario := range []struct {
		name, path string
		remove     bool
		want       VerificationStatus
	}{
		{"required file exceeds supported limit", "report.md", true, VerificationUnknown},
		{"unrelated file limit does not excuse missing delivery", "large.pdf", true, VerificationFailed},
		{"unrelated limit does not invalidate existing required file", "large.pdf", false, VerificationPassed},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			outputs := fixtureOutputs()
			source := outputs[1].Sources[0]
			outputs[1].SourceObservations = []SourceObservation{{Source: source, Collection: &fileartifact.CollectionEvidence{SchemaVersion: 1, Complete: false,
				Limits: fileartifact.CollectionLimits{MaxFiles: 128, MaxFileBytes: 256 * 1024, MaxTotalBytes: 512 * 1024}, Issues: []fileartifact.CollectionIssue{{Path: scenario.path, Kind: "limit", Reason: "unsupported_file_type"}}}}}
			if scenario.remove {
				outputs = outputs[1:]
			}
			report, err := Verify(context.Background(), fixtureContract(), fixtureCandidate(t, outputs), fixtureEffectsRegistry(t, VerificationPassed))
			if err != nil {
				t.Fatal(err)
			}
			if report.Status != scenario.want {
				t.Fatalf("got %s: %+v", report.Status, report.Checks)
			}
		})
	}
}

func TestVerificationVersionIdentityAndFrozenLiteral(t *testing.T) {
	registry := fixtureEffectsRegistry(t, VerificationPassed)
	contract := fixtureContract()
	first := fixtureCandidate(t, fixtureOutputs())
	a, err := Verify(context.Background(), contract, first, registry)
	if err != nil {
		t.Fatal(err)
	}
	b, err := Verify(context.Background(), contract, first, registry)
	if err != nil {
		t.Fatal(err)
	}
	if a.ID != b.ID || a.RevisionID != b.RevisionID {
		t.Fatal("identical observations changed immutable identity")
	}
	outputs := fixtureOutputs()
	outputs[0].Artifact.Content += " revised"
	changed, err := Verify(context.Background(), contract, fixtureCandidate(t, outputs), registry)
	if err != nil {
		t.Fatal(err)
	}
	if changed.RevisionID == a.RevisionID {
		t.Fatal("changed bytes retained old delivery revision")
	}
	outputs = fixtureOutputs()[1:]
	outputs[0].Sources = nil
	outputs[0].Output = "literal output"
	raw, _ := json.Marshal(outputs[0].Output)
	outputs[0].Selection = &OutputSelection{Kind: "literal", ValueDigest: digest(raw)}
	contract.RequiredArtifacts = nil
	literal, err := Verify(context.Background(), contract, fixtureCandidate(t, outputs), registry)
	if err != nil {
		t.Fatal(err)
	}
	if literal.Status != VerificationPassed {
		t.Fatalf("frozen literal failed: %+v", literal.Checks)
	}
}

func TestVerificationChecksBoundaries(t *testing.T) {
	for _, id := range []string{"_schema", "artifact:report"} {
		contract := fixtureContract()
		contract.RequiredArtifacts[0].ID = id
		if ValidateDeliveryContract(contract) == nil {
			t.Fatalf("reserved artifact ID %s accepted", id)
		}
	}
	contract := fixtureContract()
	contract.ExternalEffectsCheckID = "unknown"
	if ValidateDeliveryContract(contract) == nil {
		t.Fatal("unbound effects check accepted")
	}
	registry := NewVerifierRegistry()
	release := make(chan struct{})
	defer close(release)
	_ = registry.RegisterExternalEffects("fixture.effects", "v1", func(context.Context, VerificationInput) (CheckResult, error) {
		<-release
		return CheckResult{}, errors.New("released")
	})
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	start := time.Now()
	report, err := Verify(ctx, fixtureContract(), fixtureCandidate(t, fixtureOutputs()), registry)
	if err != nil {
		t.Fatal(err)
	}
	if report.Status != VerificationUnknown || time.Since(start) > time.Second {
		t.Fatal("verifier timeout was not bounded")
	}
}

func TestCanonicalJSONDigestPreservesLargeIntegers(t *testing.T) {
	a, err := CanonicalJSONDigest([]byte(`{"n":9007199254740993,"a":1}`))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := CanonicalJSONDigest([]byte(`{ "a":1, "n":9007199254740993 }`))
	c, _ := CanonicalJSONDigest([]byte(`{"n":9007199254740992,"a":1}`))
	if a != b || a == c {
		t.Fatal("JSON result hashing lost identity or precision")
	}
	if _, err := CanonicalJSONDigest([]byte(`{} {}`)); err == nil {
		t.Fatal("trailing JSON accepted")
	}
}

func TestVerifierCannotMutateFrozenRequirementsOrCandidate(t *testing.T) {
	contract := fixtureContract()
	outputs := fixtureOutputs()
	outputs[1].SourceObservations = []SourceObservation{{Source: outputs[1].Sources[0], Collection: &fileartifact.CollectionEvidence{SchemaVersion: 1, Complete: true, Limits: fileartifact.CollectionLimits{MaxFiles: 128, MaxFileBytes: 256 * 1024, MaxTotalBytes: 512 * 1024}}}}
	candidate := fixtureCandidate(t, outputs)
	registry := NewVerifierRegistry()
	_ = registry.RegisterExternalEffects("fixture.effects", "v1", func(_ context.Context, input VerificationInput) (CheckResult, error) {
		input.Contract.RequiredArtifacts[0].Path = "changed.md"
		input.Contract.RequiredArtifacts[0].Contains[0] = "changed condition"
		input.Candidate.SourceObservations[0].Collection.Complete = false
		input.Candidate.Artifacts[0].Sources[0].TaskID = "changed-source"
		return CheckResult{Status: VerificationPassed, Evidence: json.RawMessage(`{"complete":true}`)}, nil
	})
	report, err := Verify(context.Background(), contract, candidate, registry)
	if err != nil {
		t.Fatal(err)
	}
	if contract.RequiredArtifacts[0].Path != "report.md" || contract.RequiredArtifacts[0].Contains[0] != "verified content" || !candidate.SourceObservations[0].Collection.Complete || candidate.Artifacts[0].Sources[0].TaskID != "task-1" {
		t.Fatal("verifier rewrote frozen facts")
	}
	raw, _ := json.Marshal(report.Candidate)
	if report.RevisionID != "delivery_"+digest(raw) {
		t.Fatal("verifier changed candidate after version identity was calculated")
	}
}
