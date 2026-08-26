package org

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestFinalCreateTeamEvaluationPreservesLegacyDefault(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  string
	}{
		{name: "legacy omitted", input: "", want: TeamEvaluationEvaluated},
		{name: "legacy whitespace", input: "  ", want: TeamEvaluationEvaluated},
		{name: "template", input: TeamEvaluationUnevaluated, want: TeamEvaluationUnevaluated},
		{name: "evaluated explicit", input: TeamEvaluationEvaluated, want: TeamEvaluationEvaluated},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := finalCreateTeamEvaluation(tc.input); got != tc.want {
				t.Fatalf("finalCreateTeamEvaluation(%q) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}

func TestTeamJSONProjectsEvaluation(t *testing.T) {
	now := time.Now().UTC()
	encoded, err := json.Marshal(Team{
		ID: "team-1", Evaluation: TeamEvaluationEvaluated,
		EvaluationBuildRunID: "br-1", EvaluationContractHash: strings.Repeat("a", 64), EvaluatedAt: &now,
	})
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	if got := string(encoded); !strings.Contains(got, `"evaluation_build_run_id":"br-1"`) ||
		!strings.Contains(got, `"evaluation_contract_hash"`) || !strings.Contains(got, `"evaluated_at"`) {
		t.Fatalf("Team JSON = %s, want evaluation projection", got)
	}
}

func TestValidateCreateActiveTeamInputRejectsUnknownEvaluation(t *testing.T) {
	input := CreateActiveTeamInput{
		Name: "team", Objective: "objective", LeadAvatarID: "lead", Evaluation: "unknown",
		Workers: []InitialTeamWorker{{WorkerAgentID: "worker", AllowedKinds: []string{"consult"}, DefaultKind: "consult"}},
	}
	if err := validateCreateActiveTeamInput("workspace", input); err == nil {
		t.Fatal("validateCreateActiveTeamInput() error = nil, want invalid evaluation rejection")
	}
}
