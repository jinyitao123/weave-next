package teambuild

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestInitialExecutionStrategy(t *testing.T) {
	cases := []struct {
		name      string
		mode      string
		requested string
		want      string
		wantError bool
	}{
		{name: "legacy create default", mode: ModeCreate, want: ExecutionStrategyCompilerV1},
		{name: "legacy optimize default", mode: ModeOptimize, want: ExecutionStrategyLegacy},
		{name: "template create", mode: ModeCreate, requested: ExecutionStrategyTemplateInstantiate, want: ExecutionStrategyTemplateInstantiate},
		{name: "template optimize rejected", mode: ModeOptimize, requested: ExecutionStrategyTemplateInstantiate, wantError: true},
		{name: "unknown rejected", mode: ModeCreate, requested: "unknown", wantError: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := initialExecutionStrategy(tc.mode, tc.requested)
			if (err != nil) != tc.wantError {
				t.Fatalf("initialExecutionStrategy() error = %v, wantError %v", err, tc.wantError)
			}
			if got != tc.want {
				t.Fatalf("initialExecutionStrategy() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestTemplateBuildRunFinalizesWithoutPublish(t *testing.T) {
	ctx := context.Background()
	store := newAuthorizeTestStore(t)
	run, err := store.CreateBuildRun(ctx, "workspace-1", "template-finalize-ready", CreateRunParams{
		Brief: authorizeTestBrief(ModeCreate), Contract: authorizeTestContract(),
		ExpiresAt: store.clock.Now().Add(time.Hour), CreatedBy: "admin-1",
		ExecutionStrategy: ExecutionStrategyTemplateInstantiate,
	})
	if err != nil {
		t.Fatalf("CreateBuildRun() error = %v", err)
	}
	if run.ExecutionStrategy != ExecutionStrategyTemplateInstantiate {
		t.Fatalf("execution strategy = %q", run.ExecutionStrategy)
	}
	if _, err := store.pool.Exec(ctx, `
		UPDATE weave_team_build_runs
		SET status='round_running', confirmed_by='user-1',
			authorization_authority='reviewed_blueprint', authorized_revision_no=1,
			authorized_blueprint_hash=$3, authorized_change_set_hash=$4
		WHERE workspace_id=$1 AND build_run_id=$2
	`, run.WorkspaceID, run.BuildRunID, strings.Repeat("a", 64), strings.Repeat("b", 64)); err != nil {
		t.Fatalf("prepare running template: %v", err)
	}
	finalized, err := store.MarkTemplateInstantiated(ctx, run.WorkspaceID, run.BuildRunID, "platform", FinalRef{Ref: "team-1", TeamID: "team-1"})
	if err != nil {
		t.Fatalf("MarkTemplateInstantiated() error = %v", err)
	}
	if finalized.Status != StatusPassed || finalized.PublishEligible || finalized.FinalRef == nil || finalized.FinalRef.TeamID != "team-1" {
		t.Fatalf("finalized run = %#v", finalized)
	}
}
