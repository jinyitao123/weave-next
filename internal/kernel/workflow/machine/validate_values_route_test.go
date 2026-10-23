package machine

import (
	"os"
	"strings"
	"testing"
)

// structuralIssues runs the phases a graph passes before any proof is needed.
func structuralIssues(t *testing.T, raw string) []ValidationIssue {
	t.Helper()
	graph, report := DecodeGraphDefinitionV1([]byte(raw))
	if report != nil && len(report.Issues) != 0 {
		t.Fatalf("decode: %+v", report.Issues)
	}
	trigger, _ := DecodeTriggerConfigV1([]byte(`{"schema_version":1,"type":"conversation_explicit","config":{}}`))
	ctx := ValidationContext{Graph: graph, Trigger: trigger}
	for _, phase := range []func(ValidationContext) Report{
		validateTypedBasics, validateIdentityReferences, validateTopology, validateRoutes,
		validateNaturalLoops, validateParallelJoin, validateConditions,
		func(ctx ValidationContext) Report { return validateValuesAndSchemas(ctx, nil) },
	} {
		if issues := phase(ctx).Issues; len(issues) != 0 {
			return issues
		}
	}
	return nil
}

const failureBranchGraph = `{
	"schema_version":1,"entry_node_id":"draft",
	"input_contract":{"type":"text"},"output_contract":{"type":"text"},
	"nodes":[
		{"id":"draft","type":"worker","config":{"kind":"consult","agent_id":"writer","agent_version":1,"result_requirement":"起草"},"inputs":{"original":{"value":{"source":"run_input","path":""},"expected_type":"text"}},"output":{"type":"text"}},
		{"id":"review","type":"worker","config":{"kind":"consult","agent_id":"reviewer","agent_version":1,"result_requirement":"复核"},"inputs":{"draft":{"value":{"source":"node_output","node_id":"draft","path":""},"expected_type":"text"}},"output":{"type":"text"}},
		{"id":"deliver","type":"deliver","config":{"result":{"source":"node_output","node_id":"review","path":""}}},
		{"id":"fallback","type":"worker","config":{"kind":"consult","agent_id":"reviewer","agent_version":1,"result_requirement":"说明失败"},"inputs":{FALLBACK_INPUTS},"output":{"type":"text"}},
		{"id":"fallback-deliver","type":"deliver","config":{"result":FALLBACK_RESULT}}
	],
	"edges":[
		{"id":"a","from_node_id":"draft","to_node_id":"review","route":"success"},
		{"id":"b","from_node_id":"review","to_node_id":"deliver","route":"success"},
		{"id":"c","from_node_id":"draft","to_node_id":"fallback","route":"failure"},
		{"id":"d","from_node_id":"fallback","to_node_id":"fallback-deliver","route":"success"}
	]
}`

func failureBranch(inputs, result string) string {
	return strings.NewReplacer("FALLBACK_INPUTS", inputs, "FALLBACK_RESULT", result).Replace(failureBranchGraph)
}

func TestValuesOnAFailureRouteCannotReadTheFailedStep(t *testing.T) {
	original := `"original":{"value":{"source":"run_input","path":""},"expected_type":"text"}`
	readsDraft := `"draft":{"value":{"source":"node_output","node_id":"draft","path":""},"expected_type":"text"}`
	fallbackResult := `{"source":"node_output","node_id":"fallback","path":""}`
	draftResult := `{"source":"node_output","node_id":"draft","path":""}`

	if issues := structuralIssues(t, failureBranch(original, fallbackResult)); len(issues) != 0 {
		t.Fatalf("a failure branch reading the task input: %+v", issues)
	}
	for name, raw := range map[string]string{
		"input":    failureBranch(original+","+readsDraft, fallbackResult),
		"delivery": failureBranch(original, draftResult),
	} {
		issues := structuralIssues(t, raw)
		if len(issues) != 1 || issues[0].Code != CodeValueUnavailableOnRoute {
			t.Fatalf("%s reading the failed step: %+v", name, issues)
		}
	}
}

func TestValuesOnATimeoutRouteCannotReadTheExpiredWait(t *testing.T) {
	graph := func(timeoutResult string) string {
		return `{
			"schema_version":1,"entry_node_id":"approval",
			"input_contract":{"type":"text"},"output_contract":{"type":"text"},
			"nodes":[
				{"id":"approval","type":"wait","config":{"kind":"human","timeout_seconds":60,"task":{"title":"审批","instructions":"确认"},
					"resume_schema":{"type":"object","additionalProperties":false,"required":["comment"],"properties":{"comment":{"type":"string"}}}}},
				{"id":"approved","type":"deliver","config":{"result":{"source":"node_output","node_id":"approval","path":"/comment"}}},
				{"id":"expired","type":"deliver","config":{"result":` + timeoutResult + `}}
			],
			"edges":[
				{"id":"a","from_node_id":"approval","to_node_id":"approved","route":"success"},
				{"id":"b","from_node_id":"approval","to_node_id":"expired","route":"timeout"}
			]
		}`
	}
	if issues := structuralIssues(t, graph(`{"source":"literal","value":"审批超时"}`)); len(issues) != 0 {
		t.Fatalf("a timeout delivering a literal: %+v", issues)
	}
	issues := structuralIssues(t, graph(`{"source":"node_output","node_id":"approval","path":"/comment"}`))
	if len(issues) != 1 || issues[0].Code != CodeValueUnavailableOnRoute || issues[0].NodeID != "expired" {
		t.Fatalf("a timeout reading the wait: %+v", issues)
	}
}

// Without failure or timeout routes the availability sets are exactly the
// strict dominators, so graphs that never fail over are judged as before.
func TestOutputAvailabilityEqualsStrictDominanceWithoutFailureRoutes(t *testing.T) {
	raw, err := os.ReadFile("testdata/code_verify_loop.json")
	if err != nil {
		t.Fatal(err)
	}
	for _, source := range []string{string(raw), failureBranch(`"original":{"value":{"source":"run_input","path":""},"expected_type":"text"}`, `{"source":"literal","value":"失败"}`)} {
		graph, report := DecodeGraphDefinitionV1([]byte(source))
		if report != nil && len(report.Issues) != 0 {
			t.Fatalf("decode: %+v", report.Issues)
		}
		analysis := buildControlFlowAnalysis(graph)
		available := buildOutputAvailability(graph, analysis)
		failing := map[int]bool{}
		for _, edge := range graph.Edges {
			if !routeCarriesOutput(edge.Route) {
				failing[analysis.nodeIndex[edge.FromNodeID]] = true
			}
		}
		for consumer := range graph.Nodes {
			for producer := range graph.Nodes {
				dominated := producer != consumer && analysis.dominates(producer, consumer)
				if got := available[consumer].contains(producer); got != dominated && !failing[producer] {
					t.Fatalf("%s before %s: available %v, strictly dominates %v", graph.Nodes[producer].ID, graph.Nodes[consumer].ID, got, dominated)
				}
			}
		}
	}
}
