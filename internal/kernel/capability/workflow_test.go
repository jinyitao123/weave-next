package capability

import (
	"encoding/json"
	"testing"

	"github.com/jinyitao123/weave/internal/base/frozen"
)

func TestValidateWorkflowSourceAllowsSingleModelMember(t *testing.T) {
	bundle := sourceBundle(t, "lead", "lead", nil)
	envelope := sourceEnvelope(t, singleMemberGraph, []frozen.FrozenExecutionBundle{bundle}, bundle.Agent.AgentID, bundle.Agent.AgentVersion)
	if err := ValidateWorkflowSource(envelope); err != nil {
		t.Fatalf("single-member source rejected: %v", err)
	}
}

func TestValidateWorkflowSourceAllowsDeterministicWorkflow(t *testing.T) {
	lead := sourceBundle(t, "team-lead", "lead", nil)
	envelope := sourceEnvelope(t, deterministicGraph, []frozen.FrozenExecutionBundle{lead}, lead.Agent.AgentID, lead.Agent.AgentVersion)
	if err := ValidateWorkflowSource(envelope); err != nil {
		t.Fatalf("deterministic source rejected: %v", err)
	}
}

func TestValidateWorkflowSourceAllowsParallelReview(t *testing.T) {
	first := sourceBundle(t, "review-one", "worker", nil)
	second := sourceBundle(t, "review-two", "worker", nil)
	envelope := sourceEnvelope(t, parallelReviewGraph, []frozen.FrozenExecutionBundle{first, second}, "team-lead", 0)
	if err := ValidateWorkflowSource(envelope); err != nil {
		t.Fatalf("parallel source rejected: %v", err)
	}
}

func TestValidateWorkflowSourceRejectsHumanWaitAndToolPermission(t *testing.T) {
	bundle := sourceBundle(t, "lead", "lead", nil)
	envelope := sourceEnvelope(t, humanWaitGraph, []frozen.FrozenExecutionBundle{bundle}, bundle.Agent.AgentID, bundle.Agent.AgentVersion)
	if err := ValidateWorkflowSource(envelope); err == nil {
		t.Fatal("human wait source was accepted")
	}

	allowed := sourceBundle(t, "lead", "lead", []string{"write_file"})
	envelope = sourceEnvelope(t, singleMemberGraph, []frozen.FrozenExecutionBundle{allowed}, allowed.Agent.AgentID, allowed.Agent.AgentVersion)
	if err := ValidateWorkflowSource(envelope); err == nil {
		t.Fatal("member with an allowed tool permission was accepted")
	}
}

func TestValidateWorkflowSourceRejectsUserOwnedPrimaryModelCredential(t *testing.T) {
	bundle := sourceBundleWithCredentialScope(t, "lead", "lead", nil, frozen.CredentialScopeUser)
	envelope := sourceEnvelope(t, singleMemberGraph, []frozen.FrozenExecutionBundle{bundle}, bundle.Agent.AgentID, bundle.Agent.AgentVersion)
	if err := ValidateWorkflowSource(envelope); err == nil {
		t.Fatal("workflow source accepted an employee-owned primary model credential")
	}
}

func TestValidateWorkflowSourceRejectsNonExplicitTriggerAndMissingJSONSchema(t *testing.T) {
	bundle := sourceBundle(t, "lead", "lead", nil)
	autoTrigger := json.RawMessage(`{"schema_version":1,"type":"conversation_auto","config":{"catalog_key":"default"}}`)
	envelope := sourceEnvelope(t, singleMemberGraph, []frozen.FrozenExecutionBundle{bundle}, bundle.Agent.AgentID, bundle.Agent.AgentVersion, autoTrigger)
	if err := ValidateWorkflowSource(envelope); err == nil {
		t.Fatal("non-explicit trigger was accepted")
	}

	var graph map[string]any
	if err := json.Unmarshal(singleMemberGraph, &graph); err != nil {
		t.Fatal(err)
	}
	output := graph["output_contract"].(map[string]any)
	delete(output, "schema")
	missingSchema, err := json.Marshal(graph)
	if err != nil {
		t.Fatal(err)
	}
	envelope = sourceEnvelope(t, missingSchema, []frozen.FrozenExecutionBundle{bundle}, bundle.Agent.AgentID, bundle.Agent.AgentVersion)
	if err := ValidateWorkflowSource(envelope); err == nil {
		t.Fatal("JSON output contract without a schema was accepted")
	}
}

var singleMemberGraph = json.RawMessage(`{
	"schema_version":1,
	"entry_node_id":"lead",
	"input_contract":{"type":"json","schema":{"type":"object","properties":{"prompt":{"type":"string"}},"required":["prompt"],"additionalProperties":false}},
	"output_contract":{"type":"json","schema":{"type":"object","properties":{"value":{"type":"string"}},"required":["value"],"additionalProperties":false}},
	"nodes":[
		{"id":"lead","type":"lead","config":{"instruction":"Return a concise result."},"output":{"type":"json","schema":{"type":"object","properties":{"value":{"type":"string"}},"required":["value"],"additionalProperties":false}}},
		{"id":"deliver","type":"deliver","config":{"result":{"source":"node_output","node_id":"lead","path":""}}}
	],
	"edges":[{"id":"lead-result","from_node_id":"lead","to_node_id":"deliver","route":"success"}]
}`)

var deterministicGraph = json.RawMessage(`{
	"schema_version":1,
	"entry_node_id":"map-input",
	"input_contract":{"type":"json","schema":{"type":"object","properties":{"value":{"type":"string"}},"required":["value"],"additionalProperties":false}},
	"output_contract":{"type":"json","schema":{"type":"object","properties":{"value":{"type":"string"}},"required":["value"],"additionalProperties":false}},
	"nodes":[
		{"id":"map-input","type":"transform","config":{"operation":"identity","value":{"source":"run_input","path":""}},"output":{"type":"json","schema":{"type":"object","properties":{"value":{"type":"string"}},"required":["value"],"additionalProperties":false}}},
		{"id":"deliver","type":"deliver","config":{"result":{"source":"node_output","node_id":"map-input","path":""}}}
	],
	"edges":[{"id":"map-result","from_node_id":"map-input","to_node_id":"deliver","route":"success"}]
}`)

var parallelReviewGraph = json.RawMessage(`{
	"schema_version":1,
	"entry_node_id":"fanout",
	"input_contract":{"type":"json","schema":{"type":"object","additionalProperties":true}},
	"output_contract":{"type":"json","schema":{"type":"object","additionalProperties":true}},
	"nodes":[
		{"id":"fanout","type":"parallel","config":{"join_node_id":"join"}},
		{"id":"review-one","type":"worker","config":{"agent_id":"review-one","agent_version":1,"kind":"dispatch","result_requirement":"Review the input."},"output":{"type":"json","schema":{"type":"object","additionalProperties":true}}},
		{"id":"review-two","type":"worker","config":{"agent_id":"review-two","agent_version":1,"kind":"dispatch","result_requirement":"Review the input."},"output":{"type":"json","schema":{"type":"object","additionalProperties":true}}},
		{"id":"join","type":"join","config":{"policy":"all_success"}},
		{"id":"deliver","type":"deliver","config":{"result":{"source":"run_input","path":""}}}
	],
	"edges":[
		{"id":"branch-one","from_node_id":"fanout","to_node_id":"review-one","route":"branch"},
		{"id":"branch-two","from_node_id":"fanout","to_node_id":"review-two","route":"branch"},
		{"id":"join-one","from_node_id":"review-one","to_node_id":"join","route":"join"},
		{"id":"join-two","from_node_id":"review-two","to_node_id":"join","route":"join"},
		{"id":"join-result","from_node_id":"join","to_node_id":"deliver","route":"success"}
	]
}`)

var humanWaitGraph = json.RawMessage(`{
	"schema_version":1,
	"entry_node_id":"wait",
	"input_contract":{"type":"text"},
	"output_contract":{"type":"text"},
	"nodes":[
		{"id":"wait","type":"wait","config":{"kind":"human","resume_schema":{"type":"object","additionalProperties":false},"task":{"title":"Review","instructions":"Review this item."}}},
		{"id":"deliver","type":"deliver","config":{"result":{"source":"run_input","path":""}}}
	],
	"edges":[{"id":"wait-result","from_node_id":"wait","to_node_id":"deliver","route":"success"}]
}`)

func sourceBundle(t *testing.T, id, role string, allow []string) frozen.FrozenExecutionBundle {
	return sourceBundleWithCredentialScope(t, id, role, allow, frozen.CredentialScopeWorkspaceService)
}

func sourceBundleWithCredentialScope(t *testing.T, id, role string, allow []string, scope frozen.CredentialScope) frozen.FrozenExecutionBundle {
	t.Helper()
	const workspaceID = "workspace"
	credential := frozen.CredentialReference{
		SchemaVersion: frozen.FrozenSchemaVersion,
		Scope:         scope,
		WorkspaceID:   workspaceID,
		Kind:          frozen.CredentialProviderAPIKey,
		ResourceID:    "system-provider",
		Slot:          "api_key",
	}
	if scope == frozen.CredentialScopeUser {
		credential.UserID = "employee-1"
	} else {
		credential.ServiceID = "system-model-provider"
	}
	model := frozen.FrozenModelBinding{Protocol: "openai-completions", ChatPath: "/chat/completions",
		SchemaVersion: frozen.FrozenSchemaVersion,
		WorkspaceID:   workspaceID, ProviderID: "system-provider", ProviderRevision: 2,
		ModelID: "model-" + id, BaseURL: "https://provider.example/v1", CredentialRef: credential,
	}
	modelHash, err := frozen.HashDTO(model, frozen.PreorderFrozenModelBinding)
	if err != nil {
		t.Fatalf("hash model binding: %v", err)
	}
	model.ContentHash = modelHash

	var allowed []string
	if allow != nil {
		allowed = append([]string(nil), allow...)
	}
	agent := frozen.FrozenAgentRecord{
		SchemaVersion: frozen.FrozenSchemaVersion,
		WorkspaceID:   workspaceID, AgentID: id, AgentVersion: 1, Name: id,
		Role: role, Engine: "loom", GraphType: "standard", Model: model.ModelID,
		Permissions:  frozen.FrozenPermissions{Deny: []string{"*"}, Allow: allowed},
		MemoryConfig: &frozen.FrozenMemoryConfig{Enabled: false, AutoRemember: false},
		Fallback:     frozen.FrozenFallback{},
		FactoryInput: json.RawMessage(`{}`),
	}
	agent, err = frozen.NormalizeFrozenAgentRecord(agent)
	if err != nil {
		t.Fatalf("normalize agent: %v", err)
	}
	agentHash, err := frozen.HashDTO(agent, frozen.PreorderFrozenAgentRecord)
	if err != nil {
		t.Fatalf("hash agent: %v", err)
	}
	version := agent.AgentVersion
	modelRevision := model.ProviderRevision
	_, dependencies, err := frozen.BuildFrozenDependencyManifest([]frozen.FrozenDependencyRef{
		{
			EnumeratedDependencyRef: frozen.EnumeratedDependencyRef{
				WorkspaceID: workspaceID, OwnerType: "agent", OwnerID: id, OwnerAgentVersion: &version,
				DependencyType: "agent", DependencyKey: id, DependencyVersion: &version,
			},
			ContentHash: agentHash,
		},
		{
			EnumeratedDependencyRef: frozen.EnumeratedDependencyRef{
				WorkspaceID: workspaceID, OwnerType: "agent", OwnerID: id, OwnerAgentVersion: &version,
				DependencyType: "model_binding", DependencyKey: model.ModelID, DependencyVersion: &modelRevision,
			},
			ContentHash: model.ContentHash,
		},
	})
	if err != nil {
		t.Fatalf("build dependency manifest: %v", err)
	}
	return frozen.FrozenExecutionBundle{
		SchemaVersion: frozen.FrozenSchemaVersion,
		FactoryKey:    frozen.FactoryKey{FactoryID: "standard", FactoryVersion: "1", CompilerABI: "weave-graph-abi-v1"},
		Agent:         agent, Skills: []frozen.FrozenSkill{}, MCPBindings: []frozen.FrozenMCPBinding{},
		PrimaryModel: model, FallbackModels: []frozen.FrozenModelBinding{},
		Credentials: []frozen.CredentialReference{credential}, Dependencies: dependencies,
		Capability: frozen.CapabilityManifest{SchemaVersion: frozen.CapabilityManifestSchemaVersion, Role: role, AgentContentHash: agentHash},
	}
}

func sourceEnvelope(t *testing.T, graph json.RawMessage, bundles []frozen.FrozenExecutionBundle, leadID string, leadVersion int64, triggerOverride ...json.RawMessage) frozen.ArtifactEnvelopeV1 {
	t.Helper()
	trigger := json.RawMessage(`{"schema_version":1,"type":"conversation_explicit","config":{}}`)
	if len(triggerOverride) > 0 {
		trigger = triggerOverride[0]
	}
	var leadHash string
	for _, bundle := range bundles {
		if bundle.Agent.AgentID == leadID && bundle.Agent.AgentVersion == leadVersion && leadVersion > 0 {
			var err error
			leadHash, err = frozen.HashDTO(bundle.Agent, frozen.PreorderFrozenAgentRecord)
			if err != nil {
				t.Fatalf("hash team lead: %v", err)
			}
		}
	}
	payload := frozen.ArtifactPayloadV1{
		SchemaVersion:   frozen.ArtifactSchemaVersion,
		TriggerConfig:   trigger,
		GraphDefinition: graph,
		Team: frozen.ArtifactTeamV1{
			WorkspaceID: "workspace", TeamID: "team", LeadAgentID: leadID,
			LeadAgentVersion: leadVersion, LeadAgentContentHash: leadHash,
			Workers: []frozen.FrozenTeamWorker{},
		},
		Bundles: bundles, DeliveryTargets: []frozen.FrozenDeliveryTarget{},
	}
	payloadJSON, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("encode payload: %v", err)
	}
	normalizedPayload, err := frozen.DecodeArtifactPayloadV1(payloadJSON)
	if err != nil {
		t.Fatalf("decode test payload: %v", err)
	}
	envelope := frozen.ArtifactEnvelopeV1{
		WorkspaceID: "workspace", WorkflowID: "workflow", WorkflowVersion: 1,
		ArtifactSchemaVersion:     frozen.ArtifactSchemaVersion,
		CanonicalizationAlgorithm: frozen.ArtifactCanonicalizationAlgorithm,
		CanonicalizationVersion:   frozen.ArtifactCanonicalizationVersion,
		HashAlgorithm:             frozen.ArtifactHashAlgorithm, Payload: payloadJSON,
	}
	envelope.ContentHash, err = frozen.ComputeArtifactContentHash(frozen.ArtifactEnvelopeHashInputV1{
		WorkspaceID: envelope.WorkspaceID, WorkflowID: envelope.WorkflowID, WorkflowVersion: envelope.WorkflowVersion,
		ArtifactSchemaVersion: envelope.ArtifactSchemaVersion, CanonicalizationAlgorithm: envelope.CanonicalizationAlgorithm,
		CanonicalizationVersion: envelope.CanonicalizationVersion, HashAlgorithm: envelope.HashAlgorithm, Payload: normalizedPayload,
	})
	if err != nil {
		t.Fatalf("hash envelope: %v", err)
	}
	return envelope
}
