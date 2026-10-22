package frozen

import (
	"encoding/json"
	"errors"
	"reflect"
	"testing"
)

func cachedEnvelopeFixture(t *testing.T, teamID string) ArtifactEnvelopeV1 {
	t.Helper()
	payload := ArtifactPayloadV1{SchemaVersion: 1, TriggerConfig: json.RawMessage(`{"type":"manual"}`),
		GraphDefinition: json.RawMessage(`{"schema_version":1,"nodes":[]}`), Team: ArtifactTeamV1{WorkspaceID: "workspace", TeamID: teamID}}
	encoded, err := Canonicalize(payload, PreorderArtifactPayloadV1)
	if err != nil {
		t.Fatal(err)
	}
	hash, err := ComputeArtifactContentHash(ArtifactEnvelopeHashInputV1{WorkspaceID: "workspace", WorkflowID: "workflow", WorkflowVersion: 1,
		ArtifactSchemaVersion: 1, CanonicalizationAlgorithm: ArtifactCanonicalizationAlgorithm, CanonicalizationVersion: 1, HashAlgorithm: "sha256", Payload: payload})
	if err != nil {
		t.Fatal(err)
	}
	return ArtifactEnvelopeV1{WorkspaceID: "workspace", WorkflowID: "workflow", WorkflowVersion: 1, ArtifactSchemaVersion: 1,
		CanonicalizationAlgorithm: ArtifactCanonicalizationAlgorithm, CanonicalizationVersion: 1,
		HashAlgorithm: "sha256", ContentHash: hash, Payload: encoded}
}

func TestDecodeArtifactEnvelopeCacheReturnsIndependentCopies(t *testing.T) {
	envelope := cachedEnvelopeFixture(t, "team-copies")
	first, err := DecodeArtifactEnvelopeV1(envelope)
	if err != nil {
		t.Fatal(err)
	}
	want, err := DecodeArtifactPayloadV1(envelope.Payload)
	if err != nil {
		t.Fatal(err)
	}
	first.GraphDefinition[0] = 'X'
	first.Team.TeamID = "changed"
	first.Team.Workers = append(first.Team.Workers, FrozenTeamWorker{})
	for range 2 {
		again, err := DecodeArtifactEnvelopeV1(envelope)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(again, want) {
			t.Fatalf("cached decode differs from a fresh decode:\n got %+v\nwant %+v", again, want)
		}
		again.GraphDefinition[0] = 'Y'
	}
}

func TestDecodeArtifactEnvelopeCacheStillVerifies(t *testing.T) {
	envelope := cachedEnvelopeFixture(t, "team-verified")
	if _, err := DecodeArtifactEnvelopeV1(envelope); err != nil {
		t.Fatal(err)
	}
	// Same header and claimed hash, different payload: a cache hit must not
	// stand in for verifying it.
	tampered := envelope
	tampered.Payload = cachedEnvelopeFixture(t, "team-other").Payload
	if _, err := DecodeArtifactEnvelopeV1(tampered); !errors.Is(err, ErrFrozenContentHashMismatch) {
		t.Fatalf("tampered payload decoded: %v", err)
	}
	moved := envelope
	moved.WorkflowVersion = 2
	if _, err := DecodeArtifactEnvelopeV1(moved); !errors.Is(err, ErrFrozenContentHashMismatch) {
		t.Fatalf("envelope moved to another version decoded: %v", err)
	}
}

func TestCloneArtifactPayloadPreservesShapeAndSharesNothing(t *testing.T) {
	runtime := &FrozenRuntimeBinding{}
	payload := ArtifactPayloadV1{SchemaVersion: 1, GraphDefinition: json.RawMessage(`{}`),
		Team:    ArtifactTeamV1{TeamID: "team", Workers: []FrozenTeamWorker{}},
		Bundles: []FrozenExecutionBundle{{Runtime: runtime, Skills: []FrozenSkill{{}}}, {}}}
	clone := cloneArtifactPayload(payload)
	if !reflect.DeepEqual(clone, payload) {
		t.Fatalf("clone differs:\n got %+v\nwant %+v", clone, payload)
	}
	if clone.Team.Workers == nil || clone.Bundles[1].Skills != nil || clone.TriggerConfig != nil {
		t.Fatal("clone did not keep nil and empty apart")
	}
	if clone.Bundles[0].Runtime == runtime || &clone.Bundles[0].Skills[0] == &payload.Bundles[0].Skills[0] || &clone.GraphDefinition[0] == &payload.GraphDefinition[0] {
		t.Fatal("clone shares memory with the original")
	}
}

// TestArtifactPayloadTypesAreDeepCopyable keeps the artifact types plain data:
// deepCopyValue cannot set unexported fields and must not copy funcs or
// channels, so a field like that has to be handled before it is added.
func TestArtifactPayloadTypesAreDeepCopyable(t *testing.T) {
	seen := map[reflect.Type]bool{}
	var walk func(reflect.Type, string)
	walk = func(typ reflect.Type, path string) {
		if seen[typ] {
			return
		}
		seen[typ] = true
		switch typ.Kind() {
		case reflect.Func, reflect.Chan, reflect.UnsafePointer:
			t.Errorf("%s: %s cannot be deep-copied", path, typ)
		case reflect.Struct:
			for i := range typ.NumField() {
				field := typ.Field(i)
				if !field.IsExported() {
					t.Errorf("%s.%s: unexported field cannot be deep-copied", path, field.Name)
					continue
				}
				walk(field.Type, path+"."+field.Name)
			}
		case reflect.Slice, reflect.Array, reflect.Pointer:
			walk(typ.Elem(), path+"[]")
		case reflect.Map:
			walk(typ.Key(), path+"{key}")
			walk(typ.Elem(), path+"{}")
		}
	}
	walk(reflect.TypeOf(ArtifactPayloadV1{}), "ArtifactPayloadV1")
}
