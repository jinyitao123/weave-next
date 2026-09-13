package deliverycheck

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/jinyitao123/weave/internal/base/deliverable"
)

func TestPublishedMechanicalAssertions(t *testing.T) {
	for _, test := range []struct {
		name, rule, output, input string
		want                      deliverable.VerificationStatus
	}{
		{"wrong total despite PASS", `{"actual":{"source":"output","path":"/total"},"operator":"equals","expected":{"source":"input","path":"/rows","reduce":"sum","field":"/amount"}}`, `{"total":294,"review":"PASS"}`, `{"rows":[{"amount":100},{"amount":195}]}`, deliverable.VerificationFailed},
		{"exact decimal arithmetic", `{"actual":{"source":"output","path":"/total"},"operator":"equals","expected":{"source":"input","reduce":"sum"}}`, `{"total":0.3}`, `[0.1,0.2]`, deliverable.VerificationPassed},
		{"no floating point rounding", `{"actual":{"source":"output"},"operator":"equals","expected":{"source":"literal","value":9007199254740993}}`, `9007199254740992`, ``, deliverable.VerificationFailed},
		{"count missing row", `{"actual":{"source":"output","reduce":"count"},"operator":"equals","expected":{"source":"input","reduce":"count"}}`, `["a"]`, `["a","b"]`, deliverable.VerificationFailed},
		{"set wrong member", `{"actual":{"source":"output","reduce":"values","field":"/id"},"operator":"set_equals","expected":{"source":"input"}}`, `[{"id":"a"},{"id":"c"}]`, `["a","b"]`, deliverable.VerificationFailed},
		{"set reordered", `{"actual":{"source":"output"},"operator":"set_equals","expected":{"source":"input"}}`, `[2,1]`, `[1.0,2]`, deliverable.VerificationPassed},
		{"duplicate row", `{"actual":{"source":"output","reduce":"values","field":"/id"},"operator":"unique"}`, `[{"id":"a"},{"id":"a"}]`, ``, deliverable.VerificationFailed},
		{"schema required field", `{"actual":{"source":"output"},"operator":"schema","schema":{"type":"object","required":["invoice"],"properties":{"invoice":{"type":"string","minLength":1}}}}`, `{"review":"PASS"}`, ``, deliverable.VerificationFailed},
		{"schema numeric bound", `{"actual":{"source":"output"},"operator":"schema","schema":{"type":"number","maximum":100}}`, `101`, ``, deliverable.VerificationFailed},
		{"schema over sum", `{"actual":{"source":"output","reduce":"sum"},"operator":"schema","schema":{"type":"number","const":0.3}}`, `[0.1,0.2]`, ``, deliverable.VerificationPassed},
		{"invalid numeric reference is unknown", `{"actual":{"source":"output"},"operator":"at_least","expected":{"source":"input"}}`, `5`, `"unavailable"`, deliverable.VerificationUnknown},
		{"range", `{"actual":{"source":"output"},"operator":"at_least","expected":{"source":"literal","value":5}}`, `5`, ``, deliverable.VerificationPassed},
		{"missing final field", `{"actual":{"source":"output","path":"/total"},"operator":"equals","expected":{"source":"literal","value":1}}`, `{"review":"PASS"}`, ``, deliverable.VerificationFailed},
		{"duplicate keys", `{"actual":{"source":"output","path":"/total"},"operator":"equals","expected":{"source":"literal","value":1}}`, `{"total":2,"total":1}`, ``, deliverable.VerificationFailed},
		{"unavailable expected field", `{"actual":{"source":"output"},"operator":"equals","expected":{"source":"input","path":"/total"}}`, `1`, `{}`, deliverable.VerificationUnknown},
	} {
		t.Run(test.name, func(t *testing.T) {
			fn := Verifier(func(context.Context, deliverable.Candidate) (json.RawMessage, error) {
				return json.RawMessage(test.input), nil
			})
			result, err := fn(context.Background(), deliverable.VerificationInput{Check: deliverable.CheckSpec{Parameters: json.RawMessage(test.rule)}, Candidate: deliverable.Candidate{Output: json.RawMessage(test.output)}})
			if err != nil || result.Status != test.want {
				t.Fatalf("got %+v %v", result, err)
			}
			if !json.Valid(result.Evidence) {
				t.Fatal("evidence missing")
			}
		})
	}
}

func TestRuleValidationRejectsAmbiguityCodeAndAmbientData(t *testing.T) {
	for _, raw := range []string{
		`{"actual":{"source":"input"},"operator":"unique"}`,
		`{"actual":{"source":"output"},"operator":"at_least","expected":{"source":"literal","value":"not a number"}}`,
		`{"actual":{"source":"output"},"operator":"eval","expected":{"source":"literal","value":"process.exit()"}}`,
		`{"actual":{"source":"output","path":"bad"},"operator":"unique"}`,
		`{"actual":{"source":"output","path":"/~2"},"operator":"unique"}`,
		`{"actual":{"source":"output"},"operator":"schema","schema":{"$ref":"file:///etc/passwd"}}`,
		`{"actual":{"source":"output"},"operator":"schema","schema":{"$ref":"https://example.com/schema"}}`,
		`{"actual":{"source":"output"},"operator":"equals","expected":{"source":"literal","value":1e999999999}}`,
		`{"actual":{"source":"output"},"operator":"unique","operator":"equals"}`,
		`{"actual":{"source":"output"},"operator":"unique","unexpected":true}`,
	} {
		if _, _, err := compile([]byte(raw)); err == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
	if _, err := parse([]byte(strings.Repeat("[", 65) + "0" + strings.Repeat("]", 65))); err == nil {
		t.Fatal("depth limit")
	}
}

func TestFinalArtifactAndUnavailableInput(t *testing.T) {
	rule := json.RawMessage(`{"actual":{"source":"artifact","artifact":"summary.json","path":"/total"},"operator":"equals","expected":{"source":"input","path":"/total"}}`)
	candidate := deliverable.Candidate{Artifacts: []deliverable.CandidateArtifact{{Path: "summary.json", Content: `{"total":7}`}}, Output: json.RawMessage(`"PASS"`)}
	input := deliverable.VerificationInput{Check: deliverable.CheckSpec{Parameters: rule}, Candidate: candidate}
	for _, reader := range []InputReader{nil, func(context.Context, deliverable.Candidate) (json.RawMessage, error) {
		return nil, errors.New("offline")
	}} {
		r, err := Verifier(reader)(context.Background(), input)
		if err != nil || r.Status != deliverable.VerificationUnknown {
			t.Fatalf("%+v %v", r, err)
		}
	}
	r, err := Verifier(func(context.Context, deliverable.Candidate) (json.RawMessage, error) {
		return json.RawMessage(`{"total":8}`), nil
	})(context.Background(), input)
	if err != nil || r.Status != deliverable.VerificationFailed || !strings.Contains(string(r.Evidence), `"input_digest"`) {
		t.Fatalf("%+v %v", r, err)
	}
}

func TestPublishedVersionIsRequired(t *testing.T) {
	c := &deliverable.DeliveryContract{Version: 1, Coverage: deliverable.CoverageExplicit, RequiredChecks: []deliverable.CheckSpec{{ID: "test", Title: "Published requirement", VerifierID: ID, VerifierVersion: "v2", Parameters: json.RawMessage(`{"actual":{"source":"output"},"operator":"unique"}`)}}}
	if err := ValidateContract(c); err == nil {
		t.Fatal("unsupported published verifier version accepted")
	}
	c.RequiredChecks[0].VerifierVersion = Version
	if err := ValidateContract(c); err != nil {
		t.Fatal(err)
	}
}
