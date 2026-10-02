package api

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/jinyitao123/weave/internal/base/frozen"
)

// The contract tests use a board-game move rather than any one product so the
// endpoint stays defined by its invariants, not by a consumer's vocabulary.
func gomokuContract(t *testing.T) decisionContract {
	t.Helper()
	contract := decisionContract{
		InputSchema:    json.RawMessage(`{"type":"object","additionalProperties":false,"required":["board","to_move","legal_moves"],"properties":{"board":{"type":"array"},"to_move":{"enum":["black","white"]},"legal_moves":{"type":"array","items":{"type":"object","required":["move_id","x","y"],"additionalProperties":false,"properties":{"move_id":{"type":"string"},"x":{"type":"integer"},"y":{"type":"integer"}}}}}}`),
		OutputSchema:   json.RawMessage(`{"type":"object","additionalProperties":false,"required":["move","reason"],"properties":{"move":{"type":"object","required":["id"],"properties":{"id":{"type":"string"}}},"reason":{"type":"string","maxLength":200}}}`),
		OptionsPointer: "/legal_moves", OptionIDField: "move_id", ChoicePointer: "/move/id",
		Instruction: "Prefer moves that extend your own line.",
	}
	if err := contract.normalize(); err != nil {
		t.Fatal(err)
	}
	return contract
}

func gomokuInput(extra map[string]any) []byte {
	input := map[string]any{"to_move": "black", "board": []any{}, "legal_moves": []any{map[string]any{"move_id": "h8", "x": 7, "y": 7}, map[string]any{"move_id": "h9", "x": 7, "y": 8}}}
	for key, value := range extra {
		input[key] = value
	}
	raw, _ := json.Marshal(input)
	return raw
}

func TestDecisionContractNormalizationRejectsUnboundedOrBrokenContracts(t *testing.T) {
	contract := gomokuContract(t)
	if contract.MaxOptions != maxDecisionOptions || contract.hash() != gomokuContract(t).hash() || len(contract.hash()) != 64 {
		t.Fatal("default option bound or stable contract hash missing")
	}
	for name, mutate := range map[string]func(*decisionContract){
		"schema":           func(c *decisionContract) { c.InputSchema = json.RawMessage(`{"type":"nope"}`) },
		"schema-not-obj":   func(c *decisionContract) { c.OutputSchema = json.RawMessage(`true`) },
		"external-ref":     func(c *decisionContract) { c.InputSchema = json.RawMessage(`{"$ref":"https://example.com/s.json"}`) },
		"options-pointer":  func(c *decisionContract) { c.OptionsPointer = "legal_moves" },
		"choice-pointer":   func(c *decisionContract) { c.ChoicePointer = "" },
		"field":            func(c *decisionContract) { c.OptionIDField = "move id" },
		"instruction-size": func(c *decisionContract) { c.Instruction = strings.Repeat("x", maxDecisionInstruction+1) },
		"max-options":      func(c *decisionContract) { c.MaxOptions = maxDecisionOptions + 1 },
	} {
		t.Run(name, func(t *testing.T) {
			candidate := gomokuContract(t)
			mutate(&candidate)
			if candidate.normalize() == nil {
				t.Fatal("broken contract accepted")
			}
		})
	}
}

func TestDecisionLanesAreOpaqueBoundedKeys(t *testing.T) {
	for lane, valid := range map[string]bool{"room-1": true, "room-7": true, "rnd_9f3a": true, "tenant:table:42": true, "A.b-c_d": true, "": false, "-x": false, "a b": false, "x/y": false, strings.Repeat("a", 65): false, "房间1": false} {
		if decisionLane.MatchString(lane) != valid {
			t.Fatalf("lane %q validity should be %v", lane, valid)
		}
	}
}

func TestDecisionInputIsSchemaCheckedAndFrozenReproducibly(t *testing.T) {
	contract := gomokuContract(t)
	raw := gomokuInput(nil)
	canonical, err := contract.validateInput(raw)
	if err != nil {
		t.Fatal(err)
	}
	// A caller reproduces the frozen bytes by re-encoding its top-level object.
	var object map[string]json.RawMessage
	_ = json.Unmarshal(raw, &object)
	expected, _ := json.Marshal(object)
	if string(canonical) != string(expected) {
		t.Fatalf("frozen input not reproducible:\n%s\n%s", canonical, expected)
	}
	for name, input := range map[string][]byte{
		"undeclared-field": gomokuInput(map[string]any{"opponent_private": []any{}}),
		"not-object":       []byte(`[1,2]`),
		"no-options":       gomokuInput(map[string]any{"legal_moves": []any{}}),
		"duplicate-option": gomokuInput(map[string]any{"legal_moves": []any{map[string]any{"move_id": "a", "x": 1, "y": 1}, map[string]any{"move_id": "a", "x": 2, "y": 2}}}),
		"empty-option-id":  gomokuInput(map[string]any{"legal_moves": []any{map[string]any{"move_id": "", "x": 1, "y": 1}}}),
		"schema-violation": gomokuInput(map[string]any{"to_move": "red"}),
	} {
		if _, err := contract.validateInput(input); err == nil {
			t.Fatalf("%s accepted", name)
		}
	}
	bounded := gomokuContract(t)
	bounded.MaxOptions = 1
	if _, err := bounded.validateInput(raw); err == nil {
		t.Fatal("option bound ignored")
	}
	if task := contract.task(canonical); !strings.Contains(task, "/legal_moves") || !strings.Contains(task, "extend your own line") || !strings.HasSuffix(task, string(canonical)) {
		t.Fatalf("task framing incomplete: %s", task)
	}
}

func TestDecisionChoiceMustMatchSchemaAndFrozenOptions(t *testing.T) {
	contract := gomokuContract(t)
	input, _ := contract.validateInput(gomokuInput(nil))
	if err := contract.validateChoice([]byte(`{"move":{"id":"h9"},"reason":"blocks"}`), input); err != nil {
		t.Fatal(err)
	}
	for _, output := range []string{`{"move":{"id":"z1"},"reason":"outside"}`, `{"move":{"id":"h9"},"reason":"x","patch":{}}`, `{"reason":"no choice"}`, `{"move":{"id":7},"reason":"wrong type"}`, `{"move":{"id":"h9"},"reason":"x"} {}`, ``} {
		if contract.validateChoice([]byte(output), input) == nil {
			t.Fatalf("invalid output accepted: %s", output)
		}
	}
}

func decisionBundle(id, role string) frozen.FrozenExecutionBundle {
	b := frozen.FrozenExecutionBundle{PrimaryModel: frozen.FrozenModelBinding{ProviderID: "provider", ModelID: "frozen-model"}, Agent: frozen.FrozenAgentRecord{AgentID: id, Role: role, Engine: "loom", Model: "frozen-model", Permissions: frozen.FrozenPermissions{Deny: []string{"*"}}}}
	if role == "worker" {
		b.Agent.OutputSchema = json.RawMessage(`{"type":"object"}`)
	}
	return b
}

const councilGraph = `{"schema_version":1,"entry_node_id":"fanout","input_contract":{"type":"text"},"output_contract":{"type":"text"},"nodes":[{"id":"fanout","type":"parallel","config":{"join_node_id":"join"}},{"id":"scout","type":"worker","label":"Scout","inputs":{"task":{"expected_type":"text","value":{"source":"run_input","path":""}}},"output":{"type":"text"},"config":{"agent_id":"scout","agent_version":1,"kind":"consult","result_requirement":"assess"}},{"id":"attack","type":"worker","label":"Attack","inputs":{"task":{"expected_type":"text","value":{"source":"run_input","path":""}}},"output":{"type":"text"},"config":{"agent_id":"attack","agent_version":1,"kind":"consult","result_requirement":"propose"}},{"id":"join","type":"join","config":{"policy":"all_success"}},{"id":"captain","type":"worker","label":"Captain","inputs":{"task":{"expected_type":"text","value":{"source":"run_input","path":""}}},"output":{"type":"text"},"config":{"agent_id":"captain","agent_version":1,"kind":"consult","result_requirement":"decide"}},{"id":"deliver","type":"deliver","config":{"result":{"source":"node_output","node_id":"captain","path":""}}}],"edges":[{"id":"a","from_node_id":"fanout","to_node_id":"scout","route":"branch"},{"id":"b","from_node_id":"fanout","to_node_id":"attack","route":"branch"},{"id":"c","from_node_id":"scout","to_node_id":"join","route":"join"},{"id":"d","from_node_id":"attack","to_node_id":"join","route":"join"},{"id":"e","from_node_id":"join","to_node_id":"captain","route":"success"},{"id":"f","from_node_id":"captain","to_node_id":"deliver","route":"success"}]}`

func TestDecisionWorkflowAdmitsTeamsButNoExtraAuthority(t *testing.T) {
	valid := frozen.ArtifactPayloadV1{GraphDefinition: json.RawMessage(councilGraph), Bundles: []frozen.FrozenExecutionBundle{decisionBundle("scout", "worker"), decisionBundle("attack", "worker"), decisionBundle("captain", "worker")}}
	graph, err := validateDecisionWorkflow(valid)
	if err != nil {
		t.Fatalf("team workflow rejected: %v", err)
	}
	if decider, node := decisionDecider(valid, graph); decider == nil || decider.Agent.AgentID != "captain" || node != "captain" {
		t.Fatal("delivered node's member is not reported as the decider")
	}
	for name, mutate := range map[string]func(*frozen.ArtifactPayloadV1){
		"tools": func(p *frozen.ArtifactPayloadV1) { p.Bundles[1].Agent.Permissions.Deny = nil },
		"memory": func(p *frozen.ArtifactPayloadV1) {
			p.Bundles[0].Agent.MemoryConfig = &frozen.FrozenMemoryConfig{Enabled: true}
		},
		"cli-engine": func(p *frozen.ArtifactPayloadV1) { p.Bundles[2].Agent.Engine = "codex" },
		"fallback":   func(p *frozen.ArtifactPayloadV1) { p.Bundles[0].Agent.Fallback.Models = []string{"other"} },
		"no-schema":  func(p *frozen.ArtifactPayloadV1) { p.Bundles[0].Agent.OutputSchema = nil },
		"external":   func(p *frozen.ArtifactPayloadV1) { p.DeliveryTargets = []frozen.FrozenDeliveryTarget{{}} },
		"human-wait": func(p *frozen.ArtifactPayloadV1) {
			p.GraphDefinition = json.RawMessage(strings.Replace(councilGraph, `"type":"join","config":{"policy":"all_success"}`, `"type":"wait","config":{"kind":"human","resume_schema":{"type":"object"}}`, 1))
		},
		"no-bundles": func(p *frozen.ArtifactPayloadV1) { p.Bundles = nil },
		"malformed": func(p *frozen.ArtifactPayloadV1) {
			p.GraphDefinition = json.RawMessage(`{"nodes":[{"type":"worker"}]}`)
		},
	} {
		t.Run(name, func(t *testing.T) {
			raw, _ := json.Marshal(valid)
			var candidate frozen.ArtifactPayloadV1
			_ = json.Unmarshal(raw, &candidate)
			mutate(&candidate)
			if _, err := validateDecisionWorkflow(candidate); err == nil {
				t.Fatal("expanded decision authority accepted")
			}
		})
	}
}
