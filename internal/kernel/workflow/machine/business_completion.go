package machine

import (
	"encoding/json"
	"errors"
	"github.com/jinyitao123/weave/internal/base/frozen"
	"github.com/jinyitao123/weave/internal/kernel/deliverycheck"
	"reflect"
)

// This version is explicitly bound to the immutable Workbench three-field
// result protocol. Other result shapes cannot silently skip its completion gate.
func ValidateBusinessReceiptGraph(graph GraphDefinition, payload frozen.ArtifactPayloadV1) error {
	check, err := deliverycheck.BusinessReceiptCheck(graph.DeliveryContract)
	if err != nil || check == nil {
		return err
	}
	same := func(c OutputContract) bool {
		var actual, want any
		return c.Type == ValueJSON && json.Unmarshal(c.Schema, &actual) == nil && json.Unmarshal(WorkbenchResultSchemaV1(), &want) == nil && reflect.DeepEqual(actual, want)
	}
	if graph.ResultProtocol != ResultProtocolWorkbenchV1 || !same(graph.OutputContract) {
		return errors.New("business receipt check requires workbench_result_v1")
	}
	used := map[string]bool{}
	nodes := map[string]Node{}
	deliveries := 0
	for _, node := range graph.Nodes {
		nodes[node.ID] = node
		switch cfg := node.Config.(type) {
		case LeadConfig:
			used[payload.Team.LeadAgentID] = true
		case WorkerConfig:
			used[cfg.AgentID] = true
		}
	}
	for _, node := range graph.Nodes {
		if node.Type != NodeDeliver {
			continue
		}
		deliveries++
		cfg, ok := node.Config.(DeliverConfig)
		source := nodes[cfg.Result.NodeID]
		if !ok || cfg.Result.Source != ValueNodeOutput || cfg.Result.Path != "" || source.Output == nil || !same(*source.Output) || (source.Type != NodeLead && source.Type != NodeWorker) {
			return errors.New("business receipt check requires a final agent source")
		}
	}
	if deliveries != 1 {
		return errors.New("business receipt check requires one final source")
	}
	available := []string{}
	for _, bundle := range payload.Bundles {
		if used[bundle.Agent.AgentID] {
			available = append(available, bundle.Agent.BusinessCapabilityIDs...)
		}
	}
	return deliverycheck.ValidateBusinessReceiptCapabilities(graph.DeliveryContract, available)
}
