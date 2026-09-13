package kernelbindings

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jinyitao123/weave/internal/app/agentcatalog"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jinyitao123/weave/internal/base/deliverable"
	"github.com/jinyitao123/weave/internal/base/testutil"
	"github.com/jinyitao123/weave/internal/kernel/fanout"

	orgstore "github.com/jinyitao123/weave/internal/app/org"
)

func productContextFixture(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool := testutil.PostgresPool(t)
	_, err := pool.Exec(context.Background(), `
 CREATE TABLE weave_users(id text PRIMARY KEY, tenant_id text NOT NULL, username text NOT NULL, display_name text);
 CREATE TABLE weave_workspaces(id text PRIMARY KEY, slug text, name text, created_at timestamptz DEFAULT now());
 CREATE TABLE weave_members(workspace_id text, user_id text, role text, created_at timestamptz DEFAULT now());
 CREATE TABLE weave_conversations(id text PRIMARY KEY, workspace_id text, user_id text, parent_message_id text, project_id text);
 CREATE TABLE weave_projects(id text PRIMARY KEY, workspace_id text, updated_at timestamptz, last_activity_at timestamptz);
 CREATE TABLE weave_task_group(id text PRIMARY KEY, workspace_id text, project_id text, avatar_agent text, user_id text, status text, original_request text, quorum int, deadline_at timestamptz, group_outcome text, card_message_id text, conversation_id text, created_at timestamptz, updated_at timestamptz, resolved_at timestamptz);
 INSERT INTO weave_users VALUES ('alice','a','alice','Alice'),('bob','b','bob','Bob');
 INSERT INTO weave_members(workspace_id,user_id,role) VALUES ('a','alice','user'),('a','bob','user'),('a','removed','user');
 INSERT INTO weave_workspaces(id,slug,name) VALUES ('a','a','A'),('b','b','B');
 INSERT INTO weave_conversations VALUES ('conversation-a','a','alice',NULL,'project-a'),('conversation-b','b','bob',NULL,'project-b'),('thread-a','a','alice','parent','project-a');
 INSERT INTO weave_projects VALUES ('project-a','a','2026-09-01T00:00:00Z',NULL),('project-b','b','2026-09-01T00:00:00Z',NULL);
 `)
	if err != nil {
		t.Fatal(err)
	}
	return pool
}

func TestProductDirectoryRejectsCrossWorkspaceAndUsesCallerTransaction(t *testing.T) {
	pool := productContextFixture(t)
	ctx := context.Background()
	r := NewRegistry(pool)
	for _, tc := range []struct {
		user string
		want bool
	}{{"alice", true}, {"bob", false}, {"removed", false}} {
		got, err := r.IsWorkspaceMember(ctx, "a", tc.user)
		if err != nil || got != tc.want {
			t.Fatalf("%s: %v %v", tc.user, got, err)
		}
	}
	if _, err := agentcatalog.New(pool).IsWorkspaceMember(ctx, "a", "alice"); !errors.Is(err, agentcatalog.ErrOwnerDirectoryUnavailable) {
		t.Fatalf("unbound owner directory accepted: %v", err)
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `INSERT INTO weave_users VALUES ('pending','a','pending','Pending'); INSERT INTO weave_members(workspace_id,user_id,role) VALUES ('a','pending','user')`); err != nil {
		t.Fatal(err)
	}
	if got, err := WorkspaceMemberExists(ctx, tx, "a", "pending"); err != nil || !got {
		t.Fatalf("transaction-local account unavailable: %v %v", got, err)
	}
	if got, err := r.IsWorkspaceMember(ctx, "a", "pending"); err != nil || got {
		t.Fatalf("uncommitted account leaked: %v %v", got, err)
	}
	members, err := NewOrganization(pool).ListMembers(ctx, "a")
	if err != nil {
		t.Fatal(err)
	}
	if len(members) != 3 {
		t.Fatalf("members: %+v", members)
	}
	for _, m := range members {
		if m.UserID == "alice" {
			if m.Username != "alice" || m.DisplayName != "Alice" || m.Deleted {
				t.Fatalf("live profile: %+v", m)
			}
		} else if !m.Deleted || m.Username != "" || m.DisplayName != "" {
			t.Fatalf("foreign/deleted profile leaked: %+v", m)
		}
	}
	if _, err := orgstore.NewStore(pool).ListMembers(ctx, "a"); !errors.Is(err, orgstore.ErrMemberProfilesUnavailable) {
		t.Fatalf("unbound profile directory accepted: %v", err)
	}
}

func TestConversationPortsAndActivityPreserveWorkspaceAndRollback(t *testing.T) {
	pool := productContextFixture(t)
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	for _, tc := range []struct {
		id    string
		found bool
	}{{"conversation-a", true}, {"conversation-b", false}, {"thread-a", false}, {"missing", false}} {
		user, found, err := ConversationOwner(ctx, tx, "a", tc.id)
		if err != nil || found != tc.found || (found && user != "alice") {
			t.Fatalf("owner %s: %s %v %v", tc.id, user, found, err)
		}
	}
	if project, err := ConversationProject(ctx, tx, "a", "conversation-a"); err != nil || project != "project-a" {
		t.Fatalf("project: %s %v", project, err)
	}
	if project, err := ConversationProject(ctx, tx, "a", "conversation-b"); err != nil || project != "" {
		t.Fatalf("foreign project: %s %v", project, err)
	}
	at := time.Date(2026, 9, 14, 0, 0, 0, 0, time.UTC)
	records := WithTerminalActivity(nil)
	if err := records.ProjectTerminalActivityTx(ctx, tx, "a", "conversation-a", at); err != nil {
		t.Fatal(err)
	}
	if err := records.ProjectTerminalActivityTx(ctx, tx, "a", "conversation-b", at); err != nil {
		t.Fatal(err)
	}
	var a, b *time.Time
	if err := tx.QueryRow(ctx, `SELECT last_activity_at FROM weave_projects WHERE id='project-a'`).Scan(&a); err != nil {
		t.Fatal(err)
	}
	if err := tx.QueryRow(ctx, `SELECT last_activity_at FROM weave_projects WHERE id='project-b'`).Scan(&b); err != nil {
		t.Fatal(err)
	}
	if a == nil || !a.Equal(at) || b != nil {
		t.Fatalf("projection scope: %v %v", a, b)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT last_activity_at FROM weave_projects WHERE id='project-a'`).Scan(&a); err != nil || a != nil {
		t.Fatalf("rolled back activity persisted: %v %v", a, err)
	}
}

func TestFanoutAssociationRequiresExplicitProductPort(t *testing.T) {
	pool := productContextFixture(t)
	ctx := context.Background()
	request := fanout.Group{WorkspaceID: "a", AvatarAgent: "lead", UserID: "alice", ConversationID: "conversation-a"}
	if _, err := fanout.New(pool, nil).CreateGroup(ctx, request); !errors.Is(err, fanout.ErrConversationProjectUnavailable) {
		t.Fatalf("unbound association: %v", err)
	}
	created, err := NewFanout(pool, nil).CreateGroup(ctx, request)
	if err != nil || created.ProjectID != "project-a" {
		t.Fatalf("association: %+v %v", created, err)
	}
	request.ConversationID = "conversation-b"
	created, err = NewFanout(pool, nil).CreateGroup(ctx, request)
	if err != nil || created.ProjectID != "" {
		t.Fatalf("foreign association: %+v %v", created, err)
	}
	request.ConversationID = ""
	if _, err = fanout.New(pool, nil).CreateGroup(ctx, request); err != nil {
		t.Fatalf("standalone fanout requires product data: %v", err)
	}
}

func workflowDeliverableFixture(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool := productContextFixture(t)
	_, err := pool.Exec(context.Background(), `
 CREATE TABLE weave_teams(id text, workspace_id text, lead_avatar_id text);
 CREATE TABLE weave_agents(id text, workspace_id text, name text, display_name text);
 CREATE TABLE weave_team_run_snapshots(run_id text, workspace_id text, team_id text, project_id text, mode text, trigger_source_v2 jsonb, artifact_workflow_id text, artifact_workflow_version int);
 CREATE TABLE weave_published_artifact_contents(workspace_id text, workflow_id text, workflow_version int, payload jsonb);
 CREATE TABLE weave_final_deliverables(id text PRIMARY KEY, workspace_id text, project_id text, conversation_id text, user_id text, lead_avatar_id text, session_id text, event_id text, run_id text, run_snapshot_id text, title text, content text, content_type text, metadata jsonb, created_at timestamptz);
 INSERT INTO weave_teams VALUES ('team','a','lead');
 INSERT INTO weave_agents VALUES ('lead','a','lead','Report owner'),('foreign','b','other','Private name');
 INSERT INTO weave_published_artifact_contents VALUES ('a','workflow',1,'{"team":{"workspace_id":"a","team_id":"team","lead_agent_id":"lead"}}'),('a','workflow',2,'{"team":{"workspace_id":"a","team_id":"team","lead_agent_id":"new-lead"}}');
 INSERT INTO weave_team_run_snapshots VALUES ('snapshot','a','team','project-a','fixed_workflow','{"type":"conversation_explicit","source_ref":"conversation-a"}','workflow',1);`)
	if err != nil {
		t.Fatal(err)
	}
	return pool
}

func TestWorkflowDeliverableRequiresProductOwnerAndKeepsAttribution(t *testing.T) {
	pool := workflowDeliverableFixture(t)
	ctx := context.Background()
	output := deliverable.WorkflowOutput{WorkspaceID: "a", RunID: "run", RunSnapshotID: "snapshot", NodeID: "deliver", NodeLabel: "Report", Output: "final report", Final: true}
	if err := deliverable.New(pool).RecordWorkflowOutput(ctx, output); !errors.Is(err, deliverable.ErrConversationOwnerUnavailable) {
		t.Fatalf("unbound owner accepted: %v", err)
	}
	store := deliverable.New(pool, DeliverableOptions()...)
	if err := store.RecordWorkflowOutput(ctx, output); err != nil {
		t.Fatal(err)
	}
	var user, conversation, lead string
	if err := pool.QueryRow(ctx, `SELECT user_id,conversation_id,lead_avatar_id FROM weave_final_deliverables`).Scan(&user, &conversation, &lead); err != nil || user != "alice" || conversation != "conversation-a" || lead != "lead" {
		t.Fatalf("attribution: %s %s %s %v", user, conversation, lead, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE weave_team_run_snapshots SET trigger_source_v2='{"type":"conversation_explicit","source_ref":"conversation-b"}'`); err != nil {
		t.Fatal(err)
	}
	output.RunID = "foreign"
	if err := store.RecordWorkflowOutput(ctx, output); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM weave_final_deliverables`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("foreign conversation generated artifact: %d %v", count, err)
	}
}

func TestWorkflowDeliverableKeepsFrozenLeadAfterDirectoryChange(t *testing.T) {
	pool := workflowDeliverableFixture(t)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `UPDATE weave_teams SET lead_avatar_id='new-lead'; ALTER TABLE weave_teams RENAME TO product_team_directory`); err != nil {
		t.Fatal(err)
	}
	output := deliverable.WorkflowOutput{WorkspaceID: "a", RunID: "run", RunSnapshotID: "snapshot", NodeID: "deliver", AgentID: "lead", Output: "report", Final: true}
	store := deliverable.New(pool, DeliverableOptions()...)
	if err := store.RecordWorkflowOutput(ctx, output); err != nil {
		t.Fatal(err)
	}
	var lead, title string
	if err := pool.QueryRow(ctx, `SELECT lead_avatar_id,title FROM weave_final_deliverables`).Scan(&lead, &title); err != nil || lead != "lead" || title != "最终产物 · Report owner" {
		t.Fatalf("frozen lead or projected title: %q %q %v", lead, title, err)
	}
	// Without the optional presentation directory the ledger still accepts a
	// frozen node label; neither mutable product table is required by Kernel.
	if _, err := pool.Exec(ctx, `ALTER TABLE weave_agents RENAME TO product_agent_directory`); err != nil {
		t.Fatal(err)
	}
	output.NodeID = "explicit-label"
	output.NodeLabel = "Published node"
	if err := store.RecordWorkflowOutput(ctx, output); err != nil {
		t.Fatalf("published label queried mutable directory: %v", err)
	}
	output.NodeID = "fallback-label"
	output.NodeLabel = ""
	if err := deliverable.New(pool, deliverable.WithConversationOwner(ConversationOwner)).RecordWorkflowOutput(ctx, output); err != nil {
		t.Fatalf("optional label required product directory: %v", err)
	}
}

func TestWorkflowDeliverableRejectsMissingOrMismatchedFrozenIdentity(t *testing.T) {
	for _, tc := range []struct{ name, change string }{
		{"missing version", `DELETE FROM weave_published_artifact_contents WHERE workflow_version=1`},
		{"wrong team", `UPDATE weave_published_artifact_contents SET payload=jsonb_set(payload,'{team,team_id}','"foreign"') WHERE workflow_version=1`},
		{"wrong workspace", `UPDATE weave_published_artifact_contents SET payload=jsonb_set(payload,'{team,workspace_id}','"b"') WHERE workflow_version=1`},
		{"missing lead", `UPDATE weave_published_artifact_contents SET payload=payload #- '{team,lead_agent_id}' WHERE workflow_version=1`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := workflowDeliverableFixture(t)
			ctx := context.Background()
			if _, err := pool.Exec(ctx, tc.change); err != nil {
				t.Fatal(err)
			}
			output := deliverable.WorkflowOutput{WorkspaceID: "a", RunID: "run", RunSnapshotID: "snapshot", NodeID: "deliver", NodeLabel: "Report", Output: "report", Final: true}
			err := deliverable.New(pool, DeliverableOptions()...).RecordWorkflowOutput(ctx, output)
			if !errors.Is(err, deliverable.ErrWorkflowArtifactUnavailable) {
				t.Fatalf("missing frozen identity accepted: %v", err)
			}
			var count int
			if err := pool.QueryRow(ctx, `SELECT count(*) FROM weave_final_deliverables`).Scan(&count); err != nil || count != 0 {
				t.Fatalf("invalid artifact persisted: %d %v", count, err)
			}
		})
	}
}

func TestAgentLabelUsesWorkspaceAndCallerTransaction(t *testing.T) {
	pool := workflowDeliverableFixture(t)
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `UPDATE weave_agents SET display_name='Pending label' WHERE id='lead'`); err != nil {
		t.Fatal(err)
	}
	if label, err := AgentLabel(ctx, tx, "a", "lead"); err != nil || label != "Pending label" {
		t.Fatalf("transaction label: %q %v", label, err)
	}
	if label, err := AgentLabel(ctx, tx, "a", "foreign"); err != nil || label != "" {
		t.Fatalf("foreign label: %q %v", label, err)
	}
}
