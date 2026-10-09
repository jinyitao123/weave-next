package engine

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestOutputSchemaPromptAndFenceUnwrapping(t *testing.T) {
	schema := json.RawMessage(`{"type":"object"}`)
	if promptWithOutputSchema("验证", nil) != "验证" || !strings.HasSuffix(promptWithOutputSchema("验证", schema), `{"type":"object"}`) {
		t.Fatal("schema prompt changed")
	}
	for input, want := range map[string]string{
		"```json\n{\"passed\":true,\"summary\":\"ok\"}\n```": `{"passed":true,"summary":"ok"}`,
		"```\n{\"passed\":false}\n```":                       `{"passed":false}`,
		`{"passed":true}`:                                    `{"passed":true}`,
		"```json\nnot json\n```":                             "```json\nnot json\n```",
		"结论：通过":                                              "结论：通过",
	} {
		if got := unwrapJSONOutput(input); got != want {
			t.Errorf("unwrap(%q) = %q, want %q", input, got, want)
		}
	}
}
