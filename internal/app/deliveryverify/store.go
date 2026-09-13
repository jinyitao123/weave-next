// Package deliveryverify assembles the application's trusted, read-only delivery checks.
package deliveryverify

import (
	"context"
	"encoding/json"
	"github.com/jinyitao123/weave/internal/app/kernelbindings"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jinyitao123/weave/internal/base/deliverable"
	"github.com/jinyitao123/weave/internal/kernel/deliverycheck"
	"github.com/jinyitao123/weave/internal/kernel/workflow/machine"
)

// NewStore registers application checks while keeping workflow schema machinery
// out of the runtime-independent deliverable package.
func NewStore(pool *pgxpool.Pool) *deliverable.Store {
	return deliverable.NewWithVerifiers(pool, newRegistry(frozenInputReader(pool)), kernelbindings.DeliverableOptions()...)
}

// NewRegistry includes the application's built-in checks. Explicitly configured
// read-only integrations can register their own versioned checks before use.
func NewRegistry() *deliverable.VerifierRegistry { return newRegistry(nil) }

func newRegistry(read deliverycheck.InputReader) *deliverable.VerifierRegistry {
	registry := deliverable.NewVerifierRegistry()
	if err := registry.Register("weave.output-schema", "v1", verifyOutputSchema); err != nil {
		panic(err) // Static registration has no deployment-dependent inputs.
	}
	if err := registry.Register(deliverycheck.ID, deliverycheck.Version, deliverycheck.Verifier(read)); err != nil {
		panic(err)
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

// Read the same original source task selected by the workflow runtime, scoped
// to the exact execution and snapshot. Resume messages cannot replace this input.
func frozenInputReader(pool *pgxpool.Pool) deliverycheck.InputReader {
	return func(ctx context.Context, c deliverable.Candidate) (json.RawMessage, error) {
		var raw json.RawMessage
		err := pool.QueryRow(ctx, `SELECT q.payload FROM weave_team_runs r
   JOIN weave_task_queue q ON q.workspace_id=r.workspace_id AND q.id=r.source_task_id
   WHERE r.workspace_id=$1 AND r.run_id=$2 AND r.run_snapshot_id=$3
   AND q.run_snapshot_id=r.run_snapshot_id AND octet_length(q.payload::text)<=1048576`,
			c.WorkspaceID, c.RunID, c.RunSnapshotID).Scan(&raw)
		return raw, err
	}
}
