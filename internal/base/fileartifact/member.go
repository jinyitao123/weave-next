package fileartifact

import (
	"encoding/json"
	"fmt"
)

// MemberStateKey is the versioned artifact snapshot from an acknowledged tool
// receipt. It is never populated from the model's final prose.
const MemberStateKey = "__member_artifacts_v1"

// DecodeMemberReceipt recognizes an explicit complete export from a selected
// frozen tool. Ordinary tool output has no delivery meaning.
func DecodeMemberReceipt(content string) ([]File, bool, error) {
	var envelope map[string]json.RawMessage
	if json.Unmarshal([]byte(content), &envelope) != nil {
		return nil, false, nil
	}
	raw, present := envelope["weave_member_artifacts_v1"]
	if !present {
		return nil, false, nil
	}
	var files []File
	if err := json.Unmarshal(raw, &files); err != nil {
		return nil, true, fmt.Errorf("invalid member artifact export: %w", err)
	}
	if err := Validate(files); err != nil {
		return nil, true, err
	}
	return files, true, nil
}

func MemberFiles(state map[string]any) ([]File, error) {
	value, present := state[MemberStateKey]
	if !present {
		return nil, nil
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	var files []File
	if err := json.Unmarshal(raw, &files); err != nil {
		return nil, err
	}
	if err := Validate(files); err != nil {
		return nil, err
	}
	return files, nil
}
