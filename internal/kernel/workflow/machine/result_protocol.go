package machine

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"unicode/utf8"
)

const ResultProtocolWorkbenchV1 = "workbench_result_v1"

var workbenchResultSchemaV1 = json.RawMessage(`{"type":"object","additionalProperties":false,"required":["disposition","summary","missing_items"],"properties":{"disposition":{"type":"string","enum":["complete","needs_input"]},"summary":{"type":"string"},"missing_items":{"type":"array","items":{"type":"string"}}}}`)

// WorkbenchResultV1 is the immutable structured inspection result emitted by
// an opted-in workflow's selected final source node.
type WorkbenchResultV1 struct {
	Disposition  string   `json:"disposition"`
	Summary      string   `json:"summary"`
	MissingItems []string `json:"missing_items"`
}

// WorkbenchResultMetadataV1 is the exact projection copied into the verified
// final deliverable metadata. Protocol identity is kept out of the business
// output itself.
type WorkbenchResultMetadataV1 struct {
	Protocol     string   `json:"protocol"`
	Disposition  string   `json:"disposition"`
	Summary      string   `json:"summary"`
	MissingItems []string `json:"missing_items"`
}

func WorkbenchResultSchemaV1() json.RawMessage {
	return append(json.RawMessage(nil), workbenchResultSchemaV1...)
}

func isWorkbenchResultContract(contract OutputContract) bool {
	if contract.Type != ValueJSON || len(contract.Schema) == 0 {
		return false
	}
	var actual, expected any
	if json.Unmarshal(contract.Schema, &actual) != nil || json.Unmarshal(workbenchResultSchemaV1, &expected) != nil {
		return false
	}
	return reflect.DeepEqual(actual, expected)
}

// NormalizeWorkbenchResultV1 applies semantic bounds that the frozen machine
// JSON Schema subset intentionally does not express. The returned output has
// trimmed human text and is the value persisted as this run's final result.
func NormalizeWorkbenchResultV1(raw []byte) (WorkbenchResultV1, json.RawMessage, error) {
	var result WorkbenchResultV1
	if issue := inspectJSON(raw); issue != nil {
		return WorkbenchResultV1{}, nil, fmt.Errorf("inspect workbench result: %w", issue)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
		return WorkbenchResultV1{}, nil, errors.New("workbench result must be a JSON object")
	}
	for _, field := range []string{"disposition", "summary", "missing_items"} {
		if _, present := fields[field]; !present {
			return WorkbenchResultV1{}, nil, fmt.Errorf("workbench result is missing %q", field)
		}
	}
	var items []json.RawMessage
	itemsRaw := bytes.TrimSpace(fields["missing_items"])
	if json.Unmarshal(itemsRaw, &items) != nil || items == nil || len(itemsRaw) == 0 || itemsRaw[0] != '[' {
		return WorkbenchResultV1{}, nil, errors.New("workbench result missing_items must be an array")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&result); err != nil {
		return WorkbenchResultV1{}, nil, fmt.Errorf("decode workbench result: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return WorkbenchResultV1{}, nil, errors.New("workbench result must contain one JSON value")
	}
	result.Summary = strings.TrimSpace(result.Summary)
	if runeCount := utf8.RuneCountInString(result.Summary); runeCount < 1 || runeCount > 1000 {
		return WorkbenchResultV1{}, nil, errors.New("workbench result summary must contain 1 to 1000 characters")
	}
	if len(result.MissingItems) > 8 {
		return WorkbenchResultV1{}, nil, errors.New("workbench result may contain at most 8 missing items")
	}
	for index, item := range result.MissingItems {
		item = strings.TrimSpace(item)
		if runeCount := utf8.RuneCountInString(item); runeCount < 1 || runeCount > 200 {
			return WorkbenchResultV1{}, nil, fmt.Errorf("workbench result missing_items[%d] must contain 1 to 200 characters", index)
		}
		result.MissingItems[index] = item
	}
	switch result.Disposition {
	case "complete":
		if len(result.MissingItems) != 0 {
			return WorkbenchResultV1{}, nil, errors.New("complete workbench result must not list missing items")
		}
	case "needs_input":
		if len(result.MissingItems) == 0 {
			return WorkbenchResultV1{}, nil, errors.New("needs_input workbench result must list at least one missing item")
		}
	default:
		return WorkbenchResultV1{}, nil, errors.New("workbench result disposition is invalid")
	}
	normalized, err := json.Marshal(result)
	if err != nil {
		return WorkbenchResultV1{}, nil, fmt.Errorf("encode normalized workbench result: %w", err)
	}
	return result, normalized, nil
}

func EncodeWorkbenchResultMetadataV1(result WorkbenchResultV1) json.RawMessage {
	metadata, _ := json.Marshal(WorkbenchResultMetadataV1{
		Protocol: ResultProtocolWorkbenchV1, Disposition: result.Disposition,
		Summary: result.Summary, MissingItems: append([]string{}, result.MissingItems...),
	})
	return metadata
}

func validateResultProtocol(report *Report, graph GraphDefinition) {
	if graph.ResultProtocol == "" {
		return
	}
	if graph.ResultProtocol != ResultProtocolWorkbenchV1 {
		report.Add(PhaseDTO, "/result_protocol", CodeEnumInvalid, "unsupported result_protocol")
		return
	}
	if !isWorkbenchResultContract(graph.OutputContract) {
		report.Add(PhaseDTO, "/output_contract", CodeContractInvalid, "workbench_result_v1 requires its fixed JSON output contract")
	}

	nodes := make(map[string]Node, len(graph.Nodes))
	for _, node := range graph.Nodes {
		nodes[node.ID] = node
	}
	deliveries := 0
	for index, node := range graph.Nodes {
		if node.Type != NodeDeliver {
			continue
		}
		deliveries++
		config, ok := node.Config.(DeliverConfig)
		path := fmt.Sprintf("/nodes/%d/config/result", index)
		if !ok || config.Result.Source != ValueNodeOutput || config.Result.NodeID == "" || config.Result.Path != "" {
			report.AddNode(PhaseDTO, path, node.ID, CodeContractInvalid, "workbench_result_v1 deliver must select a complete node output")
			continue
		}
		source, present := nodes[config.Result.NodeID]
		if !present || source.Output == nil || !isWorkbenchResultContract(*source.Output) {
			report.AddNode(PhaseDTO, path, node.ID, CodeContractInvalid, "workbench_result_v1 source node must declare its fixed JSON output contract")
			continue
		}
		if source.Type != NodeLead && source.Type != NodeWorker {
			report.AddNode(PhaseDTO, path, node.ID, CodeContractInvalid, "workbench_result_v1 source node must be a lead or worker")
			continue
		}
		outgoing := 0
		directSuccess := false
		for _, edge := range graph.Edges {
			if edge.FromNodeID != source.ID {
				continue
			}
			outgoing++
			directSuccess = edge.ToNodeID == node.ID && edge.Route == RouteSuccess
		}
		if outgoing != 1 || !directSuccess {
			report.AddNode(PhaseDTO, path, node.ID, CodeContractInvalid, "workbench_result_v1 source must connect directly and only to its deliver node")
		}
	}
	if deliveries == 0 {
		report.Add(PhaseDTO, "/nodes", CodeContractInvalid, "workbench_result_v1 requires a deliver node")
	}
}
