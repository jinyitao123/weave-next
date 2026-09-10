package deliveryverify

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/jinyitao123/weave/internal/base/deliverable"
)

func TestOutputVerifierChecksFrozenSchemaAgainstActualOutput(t *testing.T) {
	input := deliverable.VerificationInput{Contract: deliverable.DeliveryContract{Output: deliverable.OutputRequirement{
		Type: "json", Schema: json.RawMessage(`{"type":"object","required":["invoice"],"properties":{"invoice":{"type":"string","const":"INV-440"}}}`),
	}}}
	for _, test := range []struct {
		output string
		want   deliverable.VerificationStatus
	}{
		{`{"invoice":"INV-440"}`, deliverable.VerificationPassed},
		{`{"invoice":"VBR-52","review":"PASS"}`, deliverable.VerificationFailed},
		{`{"review":"PASS"}`, deliverable.VerificationFailed},
	} {
		input.Candidate.Output = json.RawMessage(test.output)
		result, err := verifyOutputSchema(context.Background(), input)
		if err != nil || result.Status != test.want || !json.Valid(result.Evidence) {
			t.Fatalf("output %s: %+v %v", test.output, result, err)
		}
	}
}
