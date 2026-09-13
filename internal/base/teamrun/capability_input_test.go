package teamrun

import (
	"encoding/json"
	"testing"
)

func TestCapabilityInvocationInputEnvelope(t *testing.T) {
	input, err := capabilityInvocationInput(json.RawMessage(
		`{"schema_version":1,"kind":"capability_invocation","input":{"order_id":"42"}}`,
	))
	if err != nil || string(input) != `{"order_id":"42"}` {
		t.Fatalf("unwrapped input=%s error=%v", input, err)
	}
	legacy := json.RawMessage(`{"schema_version":1,"kind":"candidate_payload","input":"keep"}`)
	unchanged, err := capabilityInvocationInput(legacy)
	if err != nil || string(unchanged) != string(legacy) {
		t.Fatalf("legacy input=%s error=%v", unchanged, err)
	}
	if _, err := capabilityInvocationInput(json.RawMessage(
		`{"schema_version":1,"kind":"capability_invocation","input":{},"extra":true}`,
	)); err == nil {
		t.Fatal("capability envelope with unknown fields was accepted")
	}
}
