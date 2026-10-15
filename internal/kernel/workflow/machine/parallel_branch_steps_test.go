package machine

import (
	"encoding/json"
	"strings"
	"testing"
)

const branchStepsGraph = `{"schema_version":1,"entry_node_id":"parallel","input_contract":{"type":"text"},"output_contract":{"type":"json"},
"nodes":[
 {"id":"parallel","type":"parallel","config":{"join_node_id":"join"}},
 {"id":"draft","type":"worker","config":{"agent_id":"writer","agent_version":1,"kind":"dispatch","result_requirement":"起草"},"inputs":{"task":{"value":{"source":"run_input","path":""},"expected_type":"text"}},"output":{"type":"text"}},
 {"id":"polish","type":"worker","config":{"agent_id":"editor","agent_version":1,"kind":"dispatch","result_requirement":"润色"},"inputs":{"draft":{"value":{"source":"node_output","node_id":"draft","path":""},"expected_type":"text"}},"output":{"type":"text"}},
 {"id":"check","type":"worker","config":{"agent_id":"checker","agent_version":1,"kind":"dispatch","result_requirement":"核对"},"inputs":{"task":{"value":{"source":"run_input","path":""},"expected_type":"text"}},"output":{"type":"text"}},
 {"id":"join","type":"join","config":{"policy":"all_success"},"output":{"type":"json"}},
 {"id":"deliver","type":"deliver","config":{"result":{"source":"node_output","node_id":"join","path":""}}}
],
"edges":[
 {"id":"to-draft","from_node_id":"parallel","to_node_id":"draft","route":"branch"},
 {"id":"to-check","from_node_id":"parallel","to_node_id":"check","route":"branch"},
 {"id":"draft-polish","from_node_id":"draft","to_node_id":"polish","route":"success"},
 {"id":"polish-join","from_node_id":"polish","to_node_id":"join","route":"join"},
 {"id":"check-join","from_node_id":"check","to_node_id":"join","route":"join"},
 {"id":"join-deliver","from_node_id":"join","to_node_id":"deliver","route":"success"}
]}`

func validateBranchShape(t *testing.T, raw string) Report {
	t.Helper()
	graph, report := DecodeGraphDefinitionV1(json.RawMessage(raw))
	if report != nil && len(report.Issues) != 0 {
		t.Fatalf("decode: %+v", report.Issues)
	}
	ctx := ValidationContext{Graph: graph}
	for _, phase := range []func(ValidationContext) Report{validateTopology, validateRoutes, validateNaturalLoops, validateParallelJoin,
		func(ctx ValidationContext) Report { return validateValuesAndSchemas(ctx, nil) }} {
		if report := phase(ctx); len(report.Issues) != 0 {
			return report
		}
	}
	return Report{}
}

func TestParallelBranchMayRunStepsInTurn(t *testing.T) {
	if report := validateBranchShape(t, branchStepsGraph); len(report.Issues) != 0 {
		t.Fatalf("a two-step branch was refused: %+v", report.Issues)
	}
	graph, _ := DecodeGraphDefinitionV1(json.RawMessage(branchStepsGraph))
	steps, err := ParallelBranchSteps(graph, "draft")
	if err != nil || len(steps) != 2 || steps[0].ID != "draft" || steps[1].ID != "polish" {
		t.Fatalf("branch steps = %+v, %v", steps, err)
	}
	if steps, err := ParallelBranchSteps(graph, "check"); err != nil || len(steps) != 1 {
		t.Fatalf("one-step branch = %+v, %v", steps, err)
	}
}

func TestParallelBranchStepsMustStayInTheirBranch(t *testing.T) {
	// The second step joins the other branch's first step instead of the join.
	crossing := strings.Replace(branchStepsGraph, `{"id":"polish-join","from_node_id":"polish","to_node_id":"join","route":"join"}`,
		`{"id":"polish-check","from_node_id":"polish","to_node_id":"check","route":"success"}`, 1)
	if report := validateBranchShape(t, crossing); len(report.Issues) == 0 {
		t.Fatal("a branch step that runs into another branch was accepted")
	}
	// A consult worker cannot sit inside a branch.
	consult := strings.Replace(branchStepsGraph, `"agent_id":"editor","agent_version":1,"kind":"dispatch"`, `"agent_id":"editor","agent_version":1,"kind":"consult"`, 1)
	if report := validateBranchShape(t, consult); len(report.Issues) == 0 {
		t.Fatal("a consult worker inside a branch was accepted")
	}
}
