package teamforge

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jinyitao123/weave/internal/base/db"
	"github.com/jinyitao123/weave/internal/base/execution"
	"github.com/jinyitao123/weave/internal/base/testutil"
	"github.com/jinyitao123/weave/internal/build/teambuild"
	"github.com/jinyitao123/weave/internal/kernel/registry"
	"github.com/jinyitao123/weave/internal/kernel/workflow/machine"
)

func TestDraftRegistryPersistsAcrossRestartAndIsolatesWorkspacesRealPG(t *testing.T) {
	pool := testutil.PostgresPool(t)
	ctx := context.Background()
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	store := teambuild.New(pool, teambuild.RealClock{})
	createDraftTestRun(t, execution.WithSubject(ctx, execution.Subject{WorkspaceID: "draft-ws-a", UserID: "alice"}), store, "draft-ws-a", "shared-run")
	createDraftTestRun(t, execution.WithSubject(ctx, execution.Subject{WorkspaceID: "draft-ws-b", UserID: "bob"}), store, "draft-ws-b", "shared-run")

	registryA := NewDraftRegistry(store)
	a := registryA.GraphDrafts("draft-ws-a", "shared-run")
	b := registryA.GraphDrafts("draft-ws-b", "shared-run")
	draftA := &graphDraft{BuildRunID: "shared-run", AgentName: "writer", Graph: registry.GraphDefinition{Entry: "a"}}
	draftB := &graphDraft{BuildRunID: "shared-run", AgentName: "writer", Graph: registry.GraphDefinition{Entry: "b"}}
	if err := a.put(ctx, draftA); err != nil {
		t.Fatal(err)
	}
	if err := b.put(ctx, draftB); err != nil {
		t.Fatal(err)
	}
	workflowDrafts := registryA.WorkflowDrafts("draft-ws-a", "shared-run")
	workflowCarrier := &workflowDraft{
		BuildRunID: "shared-run", WorkflowID: "flow", Version: 2,
		Trigger: machine.TriggerConfig{SchemaVersion: 1, Type: machine.TriggerConversationExplicit, Config: machine.ConversationExplicitConfig{}},
		Graph: machine.GraphDefinition{
			SchemaVersion: 1, EntryNodeID: "lead",
			InputContract: machine.OutputContract{Type: machine.ValueText}, OutputContract: machine.OutputContract{Type: machine.ValueText},
			Nodes: []machine.Node{
				{ID: "lead", Type: machine.NodeLead, Config: machine.LeadConfig{Instruction: "answer"}},
				{ID: "deliver", Type: machine.NodeDeliver, Config: machine.DeliverConfig{Result: machine.ValueRef{Source: machine.ValueNodeOutput, NodeID: "lead"}}},
			},
			Edges: []machine.Edge{{ID: "done", FromNodeID: "lead", ToNodeID: "deliver", Route: machine.RouteSuccess}},
		},
		UpdatedAt: time.Now().UTC(), CreatedBy: "alice",
	}
	if err := workflowDrafts.put(ctx, workflowCarrier); err != nil {
		t.Fatal(err)
	}

	// A new registry models a process restart: it has no in-memory state.
	restarted := NewDraftRegistry(teambuild.New(pool, teambuild.RealClock{}))
	loadedA, err := restarted.GraphDrafts("draft-ws-a", "shared-run").get(ctx, "shared-run", "writer")
	if err != nil {
		t.Fatal(err)
	}
	loadedB, err := restarted.GraphDrafts("draft-ws-b", "shared-run").get(ctx, "shared-run", "writer")
	if err != nil {
		t.Fatal(err)
	}
	if loadedA == nil || loadedA.Graph.Entry != "a" || loadedB == nil || loadedB.Graph.Entry != "b" {
		t.Fatalf("workspace draft isolation failed: a=%+v b=%+v", loadedA, loadedB)
	}
	loadedWorkflow, err := restarted.WorkflowDrafts("draft-ws-a", "shared-run").get(ctx, "shared-run", "flow")
	if err != nil {
		t.Fatal(err)
	}
	if loadedWorkflow == nil || loadedWorkflow.WorkflowID != "flow" || loadedWorkflow.Graph.EntryNodeID != "lead" {
		t.Fatalf("workflow draft did not survive restart: %+v", loadedWorkflow)
	}
}

func TestDraftRegistryConcurrentCASAndLostResponseReplayRealPG(t *testing.T) {
	pool := testutil.PostgresPool(t)
	ctx := context.Background()
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	store := teambuild.New(pool, teambuild.RealClock{})
	createDraftTestRun(t, ctx, store, "draft-cas-ws", "draft-cas-run")
	drafts := NewDraftRegistry(store).GraphDrafts("draft-cas-ws", "draft-cas-run")
	initial := &graphDraft{BuildRunID: "draft-cas-run", AgentName: "writer", Graph: registry.GraphDefinition{Entry: "initial"}}
	if err := drafts.put(ctx, initial); err != nil {
		t.Fatal(err)
	}
	left, err := drafts.get(ctx, "draft-cas-run", "writer")
	if err != nil {
		t.Fatal(err)
	}
	right, err := drafts.get(ctx, "draft-cas-run", "writer")
	if err != nil {
		t.Fatal(err)
	}
	left.Graph.Entry = "left"
	right.Graph.Entry = "right"

	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for _, candidate := range []*graphDraft{left, right} {
		wg.Add(1)
		go func(candidate *graphDraft) {
			defer wg.Done()
			errs <- drafts.put(ctx, candidate)
		}(candidate)
	}
	wg.Wait()
	close(errs)
	var succeeded int
	for err := range errs {
		if err == nil {
			succeeded++
			continue
		}
		if !errors.Is(err, teambuild.ErrBuildDraftConflict) {
			t.Fatalf("unexpected concurrent write error: %v", err)
		}
	}
	if succeeded != 1 {
		t.Fatalf("successful concurrent writes = %d, want 1", succeeded)
	}

	winner, err := drafts.get(ctx, "draft-cas-run", "writer")
	if err != nil {
		t.Fatal(err)
	}
	staleRevision := winner.Revision - 1
	winner.Revision = staleRevision
	if err := drafts.put(ctx, winner); err != nil {
		t.Fatalf("replay after lost response must be idempotent: %v", err)
	}
	afterReplay, err := drafts.get(ctx, "draft-cas-run", "writer")
	if err != nil {
		t.Fatal(err)
	}
	if afterReplay.Revision != staleRevision+1 {
		t.Fatalf("replay changed revision: got %d want %d", afterReplay.Revision, staleRevision+1)
	}
}

func createDraftTestRun(t *testing.T, ctx context.Context, store *teambuild.Store, workspaceID, buildRunID string) {
	t.Helper()
	brief := teambuild.BuildBrief{
		SchemaVersion: 1, Mode: teambuild.ModeCreate,
		BusinessDirection: "build a durable team", Task: "assemble a team",
		NewTeamName: "draft-team", SuccessCriteria: []string{"team is usable"},
		AllowedAssets: teambuild.AssetScope{AllowedKinds: []string{"team", "agent", "workflow"}, NamePrefix: "draft-team"},
		RoundBudget:   teambuild.Budget{MaxInputTokens: 1000}, TotalBudget: teambuild.Budget{MaxInputTokens: 5000},
	}
	contract := teambuild.EvaluationContract{
		SchemaVersion: 2,
		HardGates:     teambuild.DefaultFloorHardGates(),
		Rubric: []teambuild.RubricDimension{{
			ID: "quality", Name: "Quality", Description: "Useful result", MaxScore: 10, PassThreshold: 7,
		}},
		PublicScenarios:   []teambuild.Scenario{{ID: "normal", Input: "input", Expected: "output"}},
		PerturbationRules: []string{"use an equivalent wording"},
		PerturbationScenarios: []teambuild.PerturbationScenario{{
			ID: "normal-wording", BaseScenarioID: "normal", Rule: "use an equivalent wording", Input: "equivalent input", Expected: "output",
		}},
		SevereDefectDefinition: "the output is unusable",
		RunCount:               1, MaxIterations: 3,
		PassRules: []string{"quality passes"}, BlockRules: []string{"severe defect"}, InfraFailureRules: []string{"retry infrastructure"},
	}
	if _, err := store.CreateBuildRun(ctx, workspaceID, buildRunID, teambuild.CreateRunParams{
		Brief: brief, Contract: contract, ExpiresAt: time.Now().Add(time.Hour), CreatedBy: "builder",
	}); err != nil {
		encoded, _ := json.Marshal(contract)
		t.Fatalf("create build run: %v contract=%s", err, encoded)
	}
}
