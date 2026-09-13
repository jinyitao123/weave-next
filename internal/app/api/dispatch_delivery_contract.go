package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/jinyitao123/weave/internal/base/deliverable"
	"github.com/jinyitao123/weave/internal/base/frozen"
)

// A per-run contract is accepted only from the exact Host-bound user text.
// The model-facing dispatch tool has no field that can replace these facts.
// Published workflow defaults are selected later during admission, and output
// shape is always inherited from the admitted published graph.
func parseDispatchDeliveryContract(task string) (*deliverable.DeliveryContract, error) {
	const marker = "```weave-delivery-contract-v1"
	var body strings.Builder
	inside, found, closed := false, false, false
	for _, line := range strings.Split(task, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == marker {
			if found {
				return nil, fmt.Errorf("only one delivery contract is allowed in the bound input")
			}
			inside, found = true, true
			continue
		}
		if inside && trimmed == "```" {
			inside, closed = false, true
			continue
		}
		if inside {
			body.WriteString(line)
			body.WriteByte('\n')
		}
	}
	if !found {
		return nil, nil
	}
	if !closed || body.Len() == 0 || body.Len() > 64*1024 {
		return nil, fmt.Errorf("delivery contract must be a closed JSON block of at most 64 KiB")
	}
	canonical, err := frozen.CanonicalizeJSON([]byte(body.String()))
	if err != nil {
		return nil, fmt.Errorf("delivery contract JSON is invalid: %w", err)
	}
	// This input DTO intentionally excludes Output and all evidence/results.
	var wire struct {
		Version                int                               `json:"version"`
		Coverage               string                            `json:"coverage"`
		RequiredArtifacts      []deliverable.ArtifactRequirement `json:"required_artifacts"`
		RequiredChecks         []deliverable.CheckSpec           `json:"required_checks"`
		ExternalEffects        string                            `json:"external_effects"`
		ExternalEffectsCheckID string                            `json:"external_effects_check_id"`
		Limitations            []string                          `json:"limitations"`
	}
	decoder := json.NewDecoder(bytes.NewReader(canonical))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&wire); err != nil {
		return nil, fmt.Errorf("delivery contract fields are invalid: %w", err)
	}
	contract := &deliverable.DeliveryContract{
		Version: wire.Version, Coverage: wire.Coverage, RequiredArtifacts: wire.RequiredArtifacts,
		RequiredChecks: wire.RequiredChecks, ExternalEffects: wire.ExternalEffects, ExternalEffectsCheckID: wire.ExternalEffectsCheckID,
		Limitations: wire.Limitations,
	}
	if err := deliverable.ValidateDeliveryContract(contract); err != nil {
		return nil, err
	}
	return contract, nil
}

func dispatchRequestedDeliveryContract(request teamDispatchRequest) (*deliverable.DeliveryContract, error) {
	if request.inputBinding == nil || len(request.inputBinding.DeliveryContract) == 0 || string(request.inputBinding.DeliveryContract) == "{}" || string(request.inputBinding.DeliveryContract) == "null" {
		return nil, nil
	}
	return deliverable.DecodeDeliveryContract(request.inputBinding.DeliveryContract)
}
