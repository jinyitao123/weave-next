package freezer

import (
	"encoding/json"
	"testing"

	"github.com/jinyitao123/weave/internal/base/frozen"
)

func TestRebuildArtifactResolverKeepsCLINativeModelsWithoutProviderBindings(t *testing.T) {
	for _, engineName := range []string{"claude", "codex", "opencode"} {
		t.Run(engineName, func(t *testing.T) {
			bundle := rebuildBundle(t, engineName)
			resolver, err := RebuildArtifactResolver(bundle)
			if err != nil {
				t.Fatalf("native CLI model became an empty provider dependency: %v", err)
			}
			agent, err := resolver.Agent(t.Context(), bundle.Agent.AgentID, 1)
			if err != nil || agent.Model != "native-primary" || len(agent.Fallback.Models) != 1 || agent.Fallback.Models[0] != "native-backup" {
				t.Fatalf("native model hints changed: %+v %v", agent, err)
			}
			for _, ref := range bundle.Dependencies.Dependencies {
				if ref.DependencyType == "runtime_binding" {
					runtime, err := resolver.RuntimeBinding(t.Context(), ref)
					if err != nil || runtime.RuntimeID != "runtime-1" || runtime.Engine != engineName {
						t.Fatalf("bound runtime lost: %+v %v", runtime, err)
					}
				}
			}
			bundle.PrimaryModel = frozen.FrozenModelBinding{ModelID: "forged-provider-model"}
			if _, err := RebuildArtifactResolver(bundle); err == nil {
				t.Fatal("unexpected CLI provider binding silently ignored")
			}
		})
	}
}

func TestRebuildArtifactResolverStillRequiresLoomFrozenProviderModel(t *testing.T) {
	bundle := rebuildBundle(t, "loom")
	resolver, err := RebuildArtifactResolver(bundle)
	if err != nil {
		t.Fatal(err)
	}
	for _, ref := range bundle.Dependencies.Dependencies {
		if ref.DependencyType != "model_binding" {
			continue
		}
		model, err := resolver.ModelBinding(t.Context(), ref)
		if err != nil || model.ModelID != bundle.Agent.Model || model.ProviderID != "provider-1" {
			t.Fatalf("Loom provider binding lost: %+v %v", model, err)
		}
	}
	bundle.PrimaryModel = frozen.FrozenModelBinding{}
	if _, err := RebuildArtifactResolver(bundle); err == nil {
		t.Fatal("missing Loom provider binding accepted")
	}
}

func rebuildBundle(t *testing.T, engineName string) frozen.FrozenExecutionBundle {
	t.Helper()
	version := int64(1)
	agent := frozen.FrozenAgentRecord{SchemaVersion: 1, WorkspaceID: "ws", AgentID: "worker-1", AgentVersion: 1, Name: "worker", Role: "worker", Engine: engineName, Model: "native-primary", GraphType: "standard", FactoryInput: json.RawMessage(`{}`)}
	bundle := frozen.FrozenExecutionBundle{}
	refs := []frozen.FrozenDependencyRef{}
	add := func(kind, key, hash string) {
		refs = append(refs, frozen.FrozenDependencyRef{EnumeratedDependencyRef: frozen.EnumeratedDependencyRef{WorkspaceID: "ws", OwnerType: "agent", OwnerID: agent.AgentID, OwnerAgentVersion: &version, DependencyType: kind, DependencyKey: key, DependencyVersion: &version}, ContentHash: hash})
	}
	if engineName == "loom" {
		binding := frozen.FrozenModelBinding{SchemaVersion: 1, WorkspaceID: "ws", ProviderID: "provider-1", ProviderRevision: 1, ModelID: agent.Model, BaseURL: "https://fixture.invalid/v1", CredentialRef: frozen.CredentialReference{SchemaVersion: 1, WorkspaceID: "ws", Kind: frozen.CredentialProviderAPIKey, ResourceID: "provider-1", Slot: "api_key"}}
		hash, err := frozen.HashDTO(binding, frozen.PreorderFrozenModelBinding)
		if err != nil {
			t.Fatal(err)
		}
		binding.ContentHash = hash
		bundle.PrimaryModel = binding
		add("model_binding", binding.ModelID, hash)
	} else {
		agent.RuntimeID = "runtime-1"
		agent.Fallback.Models = []string{"native-backup"}
		runtime := frozen.FrozenRuntimeBinding{SchemaVersion: 1, WorkspaceID: "ws", RuntimeID: agent.RuntimeID, Engine: engineName, RuntimeRevision: 1, AccessRef: frozen.CredentialReference{SchemaVersion: 1, WorkspaceID: "ws", Kind: frozen.CredentialRuntimeAccess, ResourceID: agent.RuntimeID, Slot: "access"}}
		hash, err := frozen.HashDTO(runtime, frozen.PreorderFrozenRuntimeBinding)
		if err != nil {
			t.Fatal(err)
		}
		runtime.ContentHash = hash
		bundle.Runtime = &runtime
		add("runtime_binding", runtime.RuntimeID, hash)
	}
	normalized, err := frozen.NormalizeFrozenAgentRecord(agent)
	if err != nil {
		t.Fatal(err)
	}
	bundle.Agent = normalized
	hash, err := frozen.HashDTO(bundle.Agent, frozen.PreorderFrozenAgentRecord)
	if err != nil {
		t.Fatal(err)
	}
	add("agent", agent.AgentID, hash)
	_, bundle.Dependencies, err = frozen.BuildFrozenDependencyManifest(refs)
	if err != nil {
		t.Fatal(err)
	}
	return bundle
}
