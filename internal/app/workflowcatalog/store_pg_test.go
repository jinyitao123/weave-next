package workflowcatalog

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"

	"github.com/jinyitao123/weave/internal/base/db"
	"github.com/jinyitao123/weave/internal/base/testutil"
	"github.com/jinyitao123/weave/internal/kernel/workflow"
)

func TestCatalogDraftCASAndDependencyDeletionRealPG(t *testing.T) {
	pool := testutil.PostgresPool(t)
	ctx := context.Background()
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO weave_workspaces(id,slug,name) VALUES('catalog','catalog','Catalog');
 INSERT INTO weave_agents(id,workspace_id,name,role,spec) VALUES('lead','catalog','lead','avatar','{}');
 INSERT INTO weave_teams(id,workspace_id,name,lead_avatar_id,status) VALUES('team','catalog','Team','lead','building')`); err != nil {
		t.Fatal(err)
	}
	store := New(pool, nil, workflow.NewArtifactStore(pool, nil))
	input := workflow.DraftInput{TriggerConfig: json.RawMessage(`{"schema_version":1}`), GraphDefinition: json.RawMessage(`{"schema_version":1}`), CreatedBy: "alice"}
	draft, err := store.Create(ctx, &workflow.TeamWorkflow{WorkspaceID: "catalog", ID: "draft", TeamID: "team", Name: "Draft"}, input)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.Get(ctx, "other", "draft"); !errors.Is(err, workflow.ErrNotFound) {
		t.Fatal("foreign catalog row visible", err)
	}
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := store.UpdateDraft(ctx, "catalog", "draft", draft.Version, draft.UpdatedAt, input)
			results <- err
		}()
	}
	wg.Wait()
	close(results)
	succeeded, conflicted := 0, 0
	for err := range results {
		if err == nil {
			succeeded++
		} else if errors.Is(err, workflow.ErrVersionConflict) {
			conflicted++
		} else {
			t.Fatal(err)
		}
	}
	if succeeded != 1 || conflicted != 1 {
		t.Fatalf("draft edit CAS success=%d conflict=%d", succeeded, conflicted)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO weave_draft_workflow_dependencies(workspace_id,workflow_id,workflow_version,owner_type,owner_id,dependency_type,dependency_key,content_hash) VALUES('catalog','draft',1,'workflow','draft','factory','draft-factory',repeat('a',64))`); err != nil {
		t.Fatal(err)
	}
	if err = store.DeletePureDraft(ctx, "catalog", "draft"); err != nil {
		t.Fatal("draft dependency prevented product deletion", err)
	}
	if _, err = store.Get(ctx, "catalog", "draft"); !errors.Is(err, workflow.ErrNotFound) {
		t.Fatal("deleted draft remained visible", err)
	}
}

func TestCatalogDeletionRetainsPendingPublicationAndAdmissionRequestsRealPG(t *testing.T) {
	pool := testutil.PostgresPool(t)
	ctx := context.Background()
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO weave_workspaces(id,slug,name) VALUES('catalog','catalog','Catalog');
 INSERT INTO weave_agents(id,workspace_id,name,role,spec) VALUES('lead','catalog','lead','avatar','{}');
 INSERT INTO weave_teams(id,workspace_id,name,lead_avatar_id,status) VALUES('team','catalog','Team','lead','building')`); err != nil {
		t.Fatal(err)
	}
	store := New(pool, nil, workflow.NewArtifactStore(pool, nil))
	for _, operation := range []string{"publish", "candidate"} {
		_, err := store.Create(ctx, &workflow.TeamWorkflow{WorkspaceID: "catalog", ID: operation, TeamID: "team", Name: operation}, workflow.DraftInput{TriggerConfig: json.RawMessage(`{"schema_version":1}`), GraphDefinition: json.RawMessage(`{"schema_version":1}`), CreatedBy: "alice"})
		if err != nil {
			t.Fatal(err)
		}
		if operation == "publish" {
			_, err = pool.Exec(ctx, `INSERT INTO weave_team_publication_requests(workspace_id,request_id,actor_subject,request_digest,command,state) VALUES('catalog','publish','{"workspace_id":"catalog","user_id":"alice"}',repeat('a',64),'{"request":{"candidate":{"workflow_id":"publish"}}}','pending')`)
		} else {
			_, err = pool.Exec(ctx, `INSERT INTO weave_team_candidate_requests(workspace_id,request_id,actor_subject,request_digest,target,request) VALUES('catalog','candidate','{"workspace_id":"catalog","user_id":"alice"}',repeat('a',64),'{}','{"candidate":{"workflow_id":"candidate"}}')`)
		}
		if err != nil {
			t.Fatal(err)
		}
		if err = store.DeletePureDraft(ctx, "catalog", operation); !errors.Is(err, workflow.ErrNotPureDraft) {
			t.Fatalf("%s intent lost its draft: %v", operation, err)
		}
	}
}
