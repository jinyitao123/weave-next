package machine

import (
	"encoding/json"
	"errors"
	"github.com/jinyitao123/weave/internal/base/frozen"
	"github.com/jinyitao123/weave/internal/kernel/deliverycheck"
	"reflect"
)

// ErrBusinessReceiptCheckRequired rejects publishing a graph whose members are
// bound to Forge actions without a declared receipt completion check: a
// mandatory action written only in a prompt is not a machine completion rule.
var ErrBusinessReceiptCheckRequired = errors.New("graph members bound to business actions require a declared business receipt completion check")

// RequireBusinessReceiptGraph is the publication rule. Already-published
// artifacts keep ValidateBusinessReceiptGraph on load; authorized dispatch to
// them is refused separately when they lack the declaration.
func RequireBusinessReceiptGraph(graph GraphDefinition, payload frozen.ArtifactPayloadV1) error {
	check, err := deliverycheck.BusinessReceiptCheck(graph.DeliveryContract)
	if err != nil {
		return err
	}
	if check == nil && len(GraphBusinessCapabilities(graph, payload)) > 0 {
		return ErrBusinessReceiptCheckRequired
	}
	return ValidateBusinessReceiptGraph(graph, payload)
}

// DeclaresBusinessReceiptCheck reports whether a frozen graph carries a valid
// receipt completion check, the precondition for authorizing its actions.
func DeclaresBusinessReceiptCheck(graph GraphDefinition) bool {
	check, err := deliverycheck.BusinessReceiptCheck(graph.DeliveryContract)
	return err == nil && check != nil
}

// GraphBusinessCapabilities lists the Forge actions bound to members the graph
// actually uses; actions of unused bundle members cannot be executed by a run.
func GraphBusinessCapabilities(graph GraphDefinition, payload frozen.ArtifactPayloadV1) []string {
	used := map[string]bool{}
	for _, node := range graph.Nodes {
		switch cfg := node.Config.(type) {
		case LeadConfig:
			used[payload.Team.LeadAgentID] = true
		case WorkerConfig:
			used[cfg.AgentID] = true
		}
	}
	available := []string{}
	for _, bundle := range payload.Bundles {
		if used[bundle.Agent.AgentID] {
			available = append(available, bundle.Agent.BusinessCapabilityIDs...)
		}
	}
	return available
}

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
	nodes := map[string]Node{}
	deliveries := 0
	for _, node := range graph.Nodes {
		nodes[node.ID] = node
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
	return deliverycheck.ValidateBusinessReceiptCapabilities(graph.DeliveryContract, GraphBusinessCapabilities(graph, payload))
}
