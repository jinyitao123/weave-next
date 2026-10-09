package machine

import (
	"os"
	"testing"
)

// The admin console's starter team sends verification failures back to the
// coding member through this bounded loop; the graph must stay valid.
func TestCodeVerifyLoopGraphIsStructurallyValid(t *testing.T) {
	raw, err := os.ReadFile("testdata/code_verify_loop.json")
	if err != nil {
		t.Fatal(err)
	}
	graph, report := DecodeGraphDefinitionV1(raw)
	if report != nil && len(report.Issues) != 0 {
		t.Fatalf("decode: %+v", report.Issues)
	}
	trigger, triggerReport := DecodeTriggerConfigV1([]byte(`{"schema_version":1,"type":"conversation_explicit","config":{}}`))
	if triggerReport != nil && len(triggerReport.Issues) != 0 {
		t.Fatalf("trigger: %+v", triggerReport.Issues)
	}
	ctx := ValidationContext{Graph: graph, Trigger: trigger}
	for _, phase := range []func(ValidationContext) Report{
		validateTypedBasics, validateIdentityReferences, validateTopology, validateRoutes,
		validateNaturalLoops, validateParallelJoin, validateConditions,
		func(ctx ValidationContext) Report { return validateValuesAndSchemas(ctx, nil) },
	} {
		if issues := phase(ctx).Issues; len(issues) != 0 {
			t.Fatalf("issues: %+v", issues)
		}
	}
}
