package machine

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestDecodeGraphDefinitionAcceptsOnlyKnownResultProtocol(t *testing.T) {
	base := `{"schema_version":1,"entry_node_id":"worker","input_contract":{"type":"text"},"output_contract":{"type":"text"},"nodes":[{"id":"worker","type":"worker","config":{"agent_id":"worker","agent_version":1,"kind":"consult","result_requirement":"inspect"},"output":{"type":"text"}},{"id":"deliver","type":"deliver","config":{"result":{"source":"node_output","node_id":"worker","path":""}}}],"edges":[{"id":"success","from_node_id":"worker","to_node_id":"deliver","route":"success"}]}`
	withProtocol := strings.Replace(base, `"input_contract"`, `"result_protocol":"workbench_result_v1","input_contract"`, 1)
	graph, report := DecodeGraphDefinitionV1(json.RawMessage(withProtocol))
	if report != nil && len(report.Issues) != 0 {
		t.Fatalf("decode opt-in graph: %+v", report.Issues)
	}
	if graph.ResultProtocol != ResultProtocolWorkbenchV1 {
		t.Fatalf("result_protocol=%q", graph.ResultProtocol)
	}
	unknown := strings.Replace(withProtocol, ResultProtocolWorkbenchV1, "workbench_result_v2", 1)
	if _, report := DecodeGraphDefinitionV1(json.RawMessage(unknown)); report == nil || len(report.Issues) == 0 || report.Issues[0].Code != CodeEnumInvalid {
		t.Fatalf("unknown protocol was not rejected: %+v", report)
	}
	if _, report := DecodeGraphDefinitionV1(json.RawMessage(base)); report != nil && len(report.Issues) != 0 {
		t.Fatalf("legacy graph without result_protocol changed behavior: %+v", report.Issues)
	}
}

func TestWorkbenchResultProtocolRequiresExactFinalAndSourceContracts(t *testing.T) {
	contract := OutputContract{Type: ValueJSON, Schema: WorkbenchResultSchemaV1()}
	graph := GraphDefinition{
		ResultProtocol: ResultProtocolWorkbenchV1,
		OutputContract: contract,
		Nodes: []Node{
			{ID: "worker", Type: NodeWorker, Output: &contract},
			{ID: "deliver", Type: NodeDeliver, Config: DeliverConfig{Result: ValueRef{Source: ValueNodeOutput, NodeID: "worker"}}},
		},
		Edges: []Edge{{ID: "success", FromNodeID: "worker", ToNodeID: "deliver", Route: RouteSuccess}},
	}
	var report Report
	validateResultProtocol(&report, graph)
	if len(report.Issues) != 0 {
		t.Fatalf("valid result protocol rejected: %+v", report.Issues)
	}

	badGraph := graph
	badGraph.OutputContract = OutputContract{Type: ValueText}
	report = Report{}
	validateResultProtocol(&report, badGraph)
	if !hasIssueAtPath(report, "/output_contract", CodeContractInvalid) {
		t.Fatalf("wrong graph output contract accepted: %+v", report.Issues)
	}

	badGraph = graph
	badGraph.Nodes = append([]Node(nil), graph.Nodes...)
	badSource := *graph.Nodes[0].Output
	badSource.Type = ValueText
	badGraph.Nodes[0].Output = &badSource
	report = Report{}
	validateResultProtocol(&report, badGraph)
	if !hasIssueAtPath(report, "/nodes/1/config/result", CodeContractInvalid) {
		t.Fatalf("wrong selected source contract accepted: %+v", report.Issues)
	}

	badGraph = graph
	badGraph.Nodes = append([]Node(nil), graph.Nodes...)
	badDeliver := badGraph.Nodes[1]
	badDeliver.Config = DeliverConfig{Result: ValueRef{Source: ValueNodeOutput, NodeID: "worker", Path: "/summary"}}
	badGraph.Nodes[1] = badDeliver
	report = Report{}
	validateResultProtocol(&report, badGraph)
	if !hasIssueAtPath(report, "/nodes/1/config/result", CodeContractInvalid) {
		t.Fatalf("partial selected source accepted: %+v", report.Issues)
	}

	badGraph = graph
	badGraph.Edges = append(append([]Edge(nil), graph.Edges...), Edge{ID: "other", FromNodeID: "worker", ToNodeID: "follow", Route: RouteSuccess})
	report = Report{}
	validateResultProtocol(&report, badGraph)
	if !hasIssueAtPath(report, "/nodes/1/config/result", CodeContractInvalid) {
		t.Fatalf("source with another downstream path accepted: %+v", report.Issues)
	}

	badGraph = graph
	badGraph.Nodes = append([]Node(nil), graph.Nodes...)
	badSourceNode := badGraph.Nodes[0]
	badSourceNode.Type = NodeTransform
	badGraph.Nodes[0] = badSourceNode
	report = Report{}
	validateResultProtocol(&report, badGraph)
	if !hasIssueAtPath(report, "/nodes/1/config/result", CodeContractInvalid) {
		t.Fatalf("non-agent source node accepted: %+v", report.Issues)
	}
}

func hasIssueAtPath(report Report, path, code string) bool {
	for _, issue := range report.Issues {
		if issue.Path == path && issue.Code == code {
			return true
		}
	}
	return false
}

func TestNormalizeWorkbenchResultV1BoundsAndDisposition(t *testing.T) {
	valid, normalized, err := NormalizeWorkbenchResultV1([]byte(`{"disposition":"needs_input","summary":"  缺少合同版本  ","missing_items":["  已签署合同  "]}`))
	if err != nil || valid.Summary != "缺少合同版本" || len(valid.MissingItems) != 1 || valid.MissingItems[0] != "已签署合同" {
		t.Fatalf("normalize result=%+v err=%v", valid, err)
	}
	var normalizedObject map[string]any
	if err := json.Unmarshal(normalized, &normalizedObject); err != nil || normalizedObject["summary"] != "缺少合同版本" {
		t.Fatalf("normalized output=%s err=%v", normalized, err)
	}

	tooLongSummary, _ := json.Marshal(WorkbenchResultV1{Disposition: "complete", Summary: strings.Repeat("界", 1001), MissingItems: []string{}})
	tooManyItems, _ := json.Marshal(WorkbenchResultV1{Disposition: "needs_input", Summary: "补充材料", MissingItems: []string{"1", "2", "3", "4", "5", "6", "7", "8", "9"}})
	tooLongItem, _ := json.Marshal(WorkbenchResultV1{Disposition: "needs_input", Summary: "补充材料", MissingItems: []string{strings.Repeat("x", 201)}})
	needsInputWithoutItems := []byte(`{"disposition":"needs_input","summary":"补充材料","missing_items":[]}`)
	completeWithItems := []byte(`{"disposition":"complete","summary":"已完成检查","missing_items":["其他材料"]}`)
	missingItemsField := []byte(`{"disposition":"complete","summary":"已完成检查"}`)
	nullItemsField := []byte(`{"disposition":"complete","summary":"已完成检查","missing_items":null}`)
	for name, raw := range map[string][]byte{
		"summary limit":          tooLongSummary,
		"item count limit":       tooManyItems,
		"item length limit":      tooLongItem,
		"needs_input items":      needsInputWithoutItems,
		"complete items":         completeWithItems,
		"missing required field": missingItemsField,
		"null items":             nullItemsField,
	} {
		t.Run(name, func(t *testing.T) {
			if _, _, err := NormalizeWorkbenchResultV1(raw); err == nil {
				t.Fatal("invalid result accepted")
			}
		})
	}
}

func TestEncodeWorkbenchResultMetadataV1PreservesEmptyItemsArray(t *testing.T) {
	result := WorkbenchResultV1{Disposition: "complete", Summary: "本轮检查已完成", MissingItems: []string{}}
	metadata := EncodeWorkbenchResultMetadataV1(result)
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(metadata, &fields); err != nil {
		t.Fatal(err)
	}
	if string(fields["missing_items"]) != "[]" {
		t.Fatalf("complete result missing_items must serialize as an empty array: %s", metadata)
	}
}
