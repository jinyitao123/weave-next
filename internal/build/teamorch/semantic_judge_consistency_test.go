package teamorch

import (
	"context"
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/jinyitao123/loom"
	"github.com/jinyitao123/loom/contract"
	"github.com/jinyitao123/weave/internal/build/teambuild"
	"github.com/jinyitao123/weave/internal/kernel/compiler"
	"github.com/jinyitao123/weave/internal/kernel/declarative"
	"github.com/jinyitao123/weave/internal/kernel/registry"
)

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
	oldOutput, oldRequests := runFrozenJudgeGraph(t, legacy, payload, judgment)
	newOutput, newRequests := runFrozenJudgeGraph(t, controlled, payload, judgment)
	if oldOutput != newOutput || oldOutput != judgment || !reflect.DeepEqual(oldRequests, newRequests) {
		t.Fatal("controlled semantic judge transport or output differs from legacy execution")
	}
	if len(newRequests) != 1 || newRequests[0].Model != "deepseek-v4-flash" ||
		len(newRequests[0].Messages) != 1 || newRequests[0].Messages[0].Role != "user" {
		t.Fatalf("llm_call transport = %#v", newRequests)
	}
	var decoded teambuild.SemanticJudgeOutputV1
	if err := json.Unmarshal([]byte(newOutput), &decoded); err != nil || decoded.SchemaVersion != 1 || len(decoded.RubricScores) != 1 || decoded.SevereDefects == nil {
		t.Fatalf("controlled output schema changed: decoded=%#v err=%v", decoded, err)
	}
}

func runFrozenJudgeGraph(t *testing.T, definition *registry.GraphDefinition, payload, output string) (string, []contract.ChatRequest) {
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
