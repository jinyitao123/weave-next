package teamconstruction

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/jinyitao123/loom"
	"github.com/jinyitao123/loom/contract"
	"github.com/jinyitao123/loom/stdlib"
	"github.com/jinyitao123/weave/internal/build/teambuild"
	"github.com/jinyitao123/weave/internal/kernel/compiler"
	"github.com/jinyitao123/weave/internal/kernel/declarative"
	"github.com/jinyitao123/weave/internal/kernel/registry"
)

func TestConstructionControlledResourcesMatchFrozenLegacyExecution(t *testing.T) {
	configRecord, err := constructionRoleRecord("workspace-1", teambuild.SourceRoleConfigEngineer)
	if err != nil {
		t.Fatal(err)
	}
	assertFrozenResourceHash(t, "config prompt", teambuild.ConfigEngineerPrompt, "3bd1336856ba4cae1c8480c8e29920b1951244b05c4d640bfd18ae9abd84fea9")
	assertFrozenJSONHash(t, "config schema", teambuild.ConfigEngineerOutputSchema(), "e401319abe813d98d7a85b1dfad16b7a558eb57ca8be444333cb0b8c0dab4b7d")
	assertFrozenDefinitionHash(t, "config graph", configRecord.GraphDefinition, "5c46d8db1e1ad498d613a7972dab984e8fd1d0ffa6d671097a0838c06443074a")
	legacyConfig := &registry.AgentRecord{
		Name: "__config_engineer", Model: "", Version: 1,
		Spec:         stdlib.AgentSpec{Identity: stdlib.IdentitySpec{Core: teambuild.ConfigEngineerPrompt}},
		OutputSchema: configRecord.OutputSchema,
		Compaction:   &registry.CompactionConfig{Enabled: false},
	}
	configOutput := `{"status":"CONFIG_OK","corrections":[],"missing_facts":[]}`
	oldOutput, oldRequests := runFrozenRecord(t, legacyConfig, loom.State{"last_user_message": "authorized plan"}, configOutput)
	newOutput, newRequests := runFrozenRecord(t, configRecord, loom.State{"last_user_message": "authorized plan"}, configOutput)
	if oldOutput != newOutput || oldOutput != configOutput || !reflect.DeepEqual(oldRequests, newRequests) {
		t.Fatalf("controlled config engineer transport drifted\nlegacy=%#v\ncontrolled=%#v", oldRequests, newRequests)
	}

	graphRecord, err := constructionRoleRecord("workspace-1", teambuild.SourceRoleGraphDesigner)
	if err != nil {
		t.Fatal(err)
	}
	assertFrozenResourceHash(t, "graph prompt", teambuild.GraphDesignerPrompt, "6f34a874cf8f11632d30fa4f4fe84d3efb84bed1f3b45ce14c22aa424ae87669")
	assertFrozenDefinitionHash(t, "graph definition", graphRecord.GraphDefinition, "70c47c71001fa171422e7c875f031020f21b77de60e6e485b4a97683f87b4301")
	graphState := loom.State{
		"last_user_message": "authorized plan", "meta_graph_planning_allowed": true,
		"meta_graph_declarative_planning": true,
	}
	graphOutput := `{"schema_version":1}`
	legacyGraph := *graphRecord
	legacyGraph.ID = "legacy-registry-record"
	legacyOutput, legacyRequests := runFrozenRecord(t, &legacyGraph, graphState, graphOutput)
	controlledOutput, controlledRequests := runFrozenRecord(t, graphRecord, graphState, graphOutput)
	if legacyOutput != controlledOutput || legacyOutput != graphOutput || !reflect.DeepEqual(legacyRequests, controlledRequests) {
		t.Fatalf("controlled graph designer transport drifted\nlegacy=%#v\ncontrolled=%#v", legacyRequests, controlledRequests)
	}
}

func assertFrozenResourceHash(t *testing.T, name, value, want string) {
	t.Helper()
	got := fmt.Sprintf("%x", sha256.Sum256([]byte(value)))
	if got != want {
		t.Fatalf("%s hash = %s, want %s", name, got, want)
	}
}

func assertFrozenJSONHash(t *testing.T, name string, value json.RawMessage, want string) {
	t.Helper()
	var decoded any
	if err := json.Unmarshal(value, &decoded); err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(decoded)
	if err != nil {
		t.Fatal(err)
	}
	assertFrozenResourceHash(t, name, string(encoded), want)
}

func assertFrozenDefinitionHash(t *testing.T, name string, definition *registry.GraphDefinition, want string) {
	t.Helper()
	encoded, err := json.Marshal(definition)
	if err != nil {
		t.Fatal(err)
	}
	assertFrozenResourceHash(t, name, string(encoded), want)
}

func TestSemanticJudgeControlledResourceMatchesFrozenLegacyExecution(t *testing.T) {
	prompt, err := os.ReadFile("testdata/semantic_judge_prompt_v1.txt")
	if err != nil {
		t.Fatal(err)
	}
	legacy := frozenLegacySemanticJudgeDefinition(strings.TrimSuffix(string(prompt), "\n"))
	controlled := teambuild.SemanticJudgeDefinition()
	if !reflect.DeepEqual(controlled, legacy) {
		t.Fatal("controlled semantic judge criteria drifted from the frozen legacy graph")
	}
	payload := `{"schema_version":1,"contract":{"rubric":[{"id":"quality","max_score":10}],"severe_defect_definition":"fabricated facts"},"scenarios":[{"scenario_id":"scenario-1","input":"question","expected":"grounded answer","terminal_status":"succeeded","output":"grounded answer"}]}`
	judgment := `{"schema_version":1,"rubric_scores":[{"dimension_id":"quality","score":10,"reason":"scenario-1 matches the expected answer"}],"severe_defects":[]}`
	oldOutput, oldRequests := runFrozenControlledGraph(t, legacy, payload, judgment)
	newOutput, newRequests := runFrozenControlledGraph(t, controlled, payload, judgment)
	if oldOutput != newOutput || oldOutput != judgment || !reflect.DeepEqual(oldRequests, newRequests) {
		t.Fatal("controlled semantic judge transport or output differs from legacy execution")
	}
	if len(newRequests) != 1 || newRequests[0].Model != "deepseek-flash" ||
		len(newRequests[0].Messages) != 1 || newRequests[0].Messages[0].Role != "user" {
		t.Fatalf("llm_call transport = %#v", newRequests)
	}
	var decoded teambuild.SemanticJudgeOutputV1
	if err := json.Unmarshal([]byte(newOutput), &decoded); err != nil || decoded.SchemaVersion != 1 || len(decoded.RubricScores) != 1 || decoded.SevereDefects == nil {
		t.Fatalf("controlled output schema changed: decoded=%#v err=%v", decoded, err)
	}
}

func TestBlueprintPatchPlannerControlledResourceMatchesFrozenLegacyExecution(t *testing.T) {
	prompt, err := os.ReadFile("testdata/blueprint_patch_planner_prompt_v1.txt")
	if err != nil {
		t.Fatal(err)
	}
	legacy := frozenLegacyBlueprintPatchPlannerDefinition(strings.TrimSuffix(string(prompt), "\n"))
	controlled := teambuild.BlueprintPatchPlannerDefinition()
	if !reflect.DeepEqual(controlled, legacy) {
		t.Fatal("controlled blueprint patch planner criteria drifted from the frozen legacy graph")
	}
	payload := `{"source_report_hash":"report-hash","evaluation_report":{"conclusion":"revise"},"typed_diagnosis":{"class":"business_quality_failure"},"current_blueprint":{"purpose":"old"},"expected_value_hash_by_path":{"/purpose":"value-hash"}}`
	patch := `{"schema_version":1,"source_report_hash":"report-hash","failure_class":"business_quality_failure","target_paths":["/purpose"],"changes":[{"path":"/purpose","expected_value_hash":"value-hash","proposed_value":"new","evidence_refs":["scenario-1"],"reason":"scenario-1"}]}`
	oldOutput, oldRequests := runFrozenControlledGraph(t, legacy, payload, patch)
	newOutput, newRequests := runFrozenControlledGraph(t, controlled, payload, patch)
	if oldOutput != newOutput || oldOutput != patch || !reflect.DeepEqual(oldRequests, newRequests) {
		t.Fatal("controlled blueprint patch planner transport or output differs from legacy execution")
	}
	if len(newRequests) != 1 || newRequests[0].Model != "deepseek-flash" ||
		len(newRequests[0].Messages) != 1 || newRequests[0].Messages[0].Role != "user" {
		t.Fatalf("llm_call transport = %#v", newRequests)
	}
	var decoded teambuild.BlueprintPatchV1
	if err := json.Unmarshal([]byte(newOutput), &decoded); err != nil || decoded.SchemaVersion != 1 || len(decoded.Changes) != 1 {
		t.Fatalf("controlled output schema changed: decoded=%#v err=%v", decoded, err)
	}
}

func runFrozenControlledGraph(t *testing.T, definition *registry.GraphDefinition, payload, output string) (string, []contract.ChatRequest) {
	t.Helper()
	declarative.Register()
	llm := &capturingJudgeLLM{output: output}
	graph, err := compiler.CompileAgent("workspace-1", &registry.AgentRecord{
		Name: "semantic-judge", GraphType: "declarative", GraphDefinition: definition,
	}, llm, nil, compiler.CompileOpts{})
	if err != nil {
		t.Fatal(err)
	}
	result, err := graph.Run(context.Background(), loom.State{"last_user_message": payload}, loom.NewMemStore())
	if err != nil {
		t.Fatal(err)
	}
	value, _ := result.State["output"].(string)
	return value, llm.requests
}

func runFrozenRecord(t *testing.T, record *registry.AgentRecord, state loom.State, output string) (string, []contract.ChatRequest) {
	t.Helper()
	declarative.Register()
	llm := &capturingJudgeLLM{output: output}
	graph, err := compiler.CompileAgent("workspace-1", record, llm, emptyConstructionTools{}, compiler.CompileOpts{})
	if err != nil {
		t.Fatal(err)
	}
	result, err := graph.Run(context.Background(), state, loom.NewMemStore())
	if err != nil {
		t.Fatal(err)
	}
	value, _ := result.State["output"].(string)
	return value, llm.requests
}

type emptyConstructionTools struct{}

func (emptyConstructionTools) ListTools(context.Context) ([]contract.ToolDef, error) { return nil, nil }
func (emptyConstructionTools) Dispatch(context.Context, contract.ToolCall) (*contract.ToolResult, error) {
	return nil, fmt.Errorf("unexpected tool call")
}

type capturingJudgeLLM struct {
	output   string
	requests []contract.ChatRequest
}

func (l *capturingJudgeLLM) Chat(_ context.Context, request contract.ChatRequest) (*contract.ChatResponse, error) {
	l.requests = append(l.requests, request)
	return &contract.ChatResponse{Content: l.output}, nil
}

func (*capturingJudgeLLM) Stream(context.Context, contract.ChatRequest) (<-chan contract.StreamChunk, error) {
	stream := make(chan contract.StreamChunk)
	close(stream)
	return stream, nil
}

func frozenLegacySemanticJudgeDefinition(prompt string) *registry.GraphDefinition {
	done, end := "done", ""
	return &registry.GraphDefinition{Entry: "score", Steps: []registry.StepDefinition{
		{Name: "score", Type: "llm_call", Display: "基于证据评分", Config: map[string]any{
			"prompt_template": prompt, "input_keys": []any{"last_user_message"}, "output_key": "score_report", "stream": false,
		}, Next: &done},
		{Name: "done", Type: "transform", Display: "完成评测报告", Config: map[string]any{"operations": []any{
			map[string]any{"op": "copy", "source": "score_report", "target": "output"},
			map[string]any{"op": "set", "target": "completion_status", "value": "report_grounded"},
		}}, Next: &end},
	}}
}

func frozenLegacyBlueprintPatchPlannerDefinition(prompt string) *registry.GraphDefinition {
	done, end := "done", ""
	return &registry.GraphDefinition{Entry: "plan_patch", Steps: []registry.StepDefinition{
		{Name: "plan_patch", Type: "llm_call", Display: "生成最小蓝图补丁", Config: map[string]any{
			"prompt_template": prompt, "input_keys": []any{"last_user_message"}, "output_key": "patch_output", "stream": false,
		}, Next: &done},
		{Name: "done", Type: "transform", Display: "交付严格补丁对象", Config: map[string]any{"operations": []any{
			map[string]any{"op": "copy", "source": "patch_output", "target": "output"},
			map[string]any{"op": "set", "target": "completion_status", "value": "patch_planned"},
		}}, Next: &end},
	}}
}
