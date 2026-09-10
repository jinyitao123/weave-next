// Package deliveryverify assembles the application's trusted, read-only delivery checks.
package deliveryverify

import (
	"context"
	"encoding/json"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jinyitao123/weave/internal/base/deliverable"
	"github.com/jinyitao123/weave/internal/kernel/workflow/machine"
)

// NewStore registers application checks while keeping workflow schema machinery
// out of the runtime-independent deliverable package.
func NewStore(pool *pgxpool.Pool) *deliverable.Store {
	return deliverable.NewWithVerifiers(pool, NewRegistry())
}

// NewRegistry includes the application's built-in checks. Explicitly configured
// read-only integrations can register their own versioned checks before use.
func NewRegistry() *deliverable.VerifierRegistry {
	registry := deliverable.NewVerifierRegistry()
	if err := registry.Register("weave.output-schema", "v1", verifyOutputSchema); err != nil {
		panic(err) // Static registration has no deployment-dependent inputs.
	}
	return registry
}

func verifyOutputSchema(_ context.Context, input deliverable.VerificationInput) (deliverable.CheckResult, error) {
	_, problems := machine.ValidateRuntimeOutput(machine.OutputContract{
		Type: machine.ValueType(input.Contract.Output.Type), Schema: input.Contract.Output.Schema,
	}, input.Candidate.Output)
	status, reason := deliverable.VerificationPassed, "published_output_schema_satisfied"
	if len(problems) != 0 {
		status, reason = deliverable.VerificationFailed, "published_output_schema_failed"
	}
	evidence, err := json.Marshal(struct {
		OutputDigest string `json:"output_digest"`
		Problems     any    `json:"problems"`
	}{OutputDigest: input.Candidate.OutputDigest, Problems: problems})
	return deliverable.CheckResult{Status: status, Reason: reason, Evidence: evidence}, err
}
