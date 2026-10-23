package capabilities

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jinyitao123/weave/internal/app/agentcatalog"
	"github.com/jinyitao123/weave/internal/base/db"
	"github.com/jinyitao123/weave/internal/base/testutil"
	"github.com/jinyitao123/weave/internal/kernel/capability"
	"github.com/jinyitao123/weave/internal/kernel/registry"
)

// quotaSlotFixture admits workflow invocations the way reserve does: the
// admission request and invocation row are inserted in the transaction that
// then takes the quota slot. An admitted invocation stays in flight until its
// admission is closed by the kernel.
type quotaSlotFixture struct {
	t                         *testing.T
	pool                      *pgxpool.Pool
	store                     *PGStore
	ws, app, credential, hash string
	mu                        sync.Mutex
	next                      int
}

func newQuotaSlotFixture(t *testing.T, max int) *quotaSlotFixture {
	t.Helper()
	pool := testutil.PostgresPool(t)
	ctx := t.Context()
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	f := &quotaSlotFixture{t: t, pool: pool, store: NewPGStore(pool), ws: "quota-slots"}
	if _, err := pool.Exec(ctx, `INSERT INTO weave_workspaces(id,slug,name) VALUES('quota-slots','quota-slots','Quota Slots');
	 INSERT INTO weave_users(id,tenant_id,username,password,role) VALUES('quota-user','quota-slots','owner','unused','developer')`); err != nil {
		t.Fatal(err)
	}
	lead := &registry.AgentRecord{Name: "quota-lead", Role: "avatar", Engine: "loom", Model: "fixture", GraphType: "standard"}
	if err := agentcatalog.New(pool).Put(ctx, f.ws, lead); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO weave_teams(id,workspace_id,name,lead_avatar_id,status)
	 VALUES('quota-team','quota-slots','Quota Team',$1,'active');
	 INSERT INTO weave_team_workflows(workspace_id,id,team_id,name,status)
	 VALUES('quota-slots','flow','quota-team','Flow','active');
	 INSERT INTO weave_team_workflow_versions(workspace_id,workflow_id,version,status,trigger_config,graph_definition,created_by)
	 VALUES('quota-slots','flow',1,'draft','{"schema_version":1,"type":"conversation_explicit","config":{}}',
	 '{"schema_version":1,"entry_node_id":"deliver","input_contract":{"type":"json"},"output_contract":{"type":"json","schema":{"type":"object"}},"nodes":[{"id":"deliver","type":"deliver","config":{"result":{"source":"run_input","path":""}}}],"edges":[]}','quota-user');
	 UPDATE weave_team_workflow_versions SET status='published',published_at=now() WHERE workspace_id='quota-slots' AND workflow_id='flow' AND version=1;
	 UPDATE weave_team_workflows SET published_version=1 WHERE workspace_id='quota-slots' AND id='flow';
	 INSERT INTO weave_published_artifact_contents(workspace_id,workflow_id,workflow_version,artifact_schema_version,
	  canonicalization_algorithm,canonicalization_version,hash_algorithm,content_hash,payload)
	 VALUES('quota-slots','flow',1,1,'rfc8785+jcs-preorder',1,'sha256',repeat('a',64),'{}')`, lead.ID); err != nil {
		t.Fatal(err)
	}
	definitions := NewService(f.store)
	definition := capability.Definition{SchemaVersion: 1, CapabilityID: "quota-cap", Name: "Quota capability",
		InputSchema: json.RawMessage(`{"type":"object"}`), OutputSchema: json.RawMessage(`{"type":"object"}`),
		Workflow: &capability.WorkflowReference{WorkflowID: "flow", WorkflowVersion: 1}}
	if err := definitions.SaveDraft(ctx, DraftRequest{WorkspaceID: f.ws, Definition: definition}); err != nil {
		t.Fatal(err)
	}
	published, err := definitions.Publish(ctx, f.ws, definition.CapabilityID, 1)
	if err != nil {
		t.Fatal(err)
	}
	access := NewAccessStore(pool)
	app, err := access.CreateApplication(ctx, f.ws, "quota-user", "invoker")
	if err != nil {
		t.Fatal(err)
	}
	credential, _, err := access.IssueCredential(ctx, f.ws, "quota-user", app.ID, "invoker key", []string{"invoke"})
	if err != nil {
		t.Fatal(err)
	}
	f.app, f.credential, f.hash = app.ID, credential.ID, published.DefinitionHash
	if _, err := f.store.SetQuota(ctx, f.ws, Quota{MaxStepsPerInvocation: 100, MaxActiveInvocations: max}); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *quotaSlotFixture) request() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.next++
	return fmt.Sprintf("request-%04d", f.next)
}

// insertInvocation records an in-flight workflow invocation for request
// through exec.
func (f *quotaSlotFixture) insertInvocation(exec func(string, ...any) error, request string) (string, error) {
	return f.insertInvocationFor(exec, request, f.app, f.credential)
}

// insertInvocationFor records an in-flight workflow invocation of app.
func (f *quotaSlotFixture) insertInvocationFor(exec func(string, ...any) error, request, app, credential string) (string, error) {
	invocation := uuid.NewString()
	if err := exec(`INSERT INTO weave_workflow_admission_requests(workspace_id,request_id,actor_subject,request_digest,request,target)
	 VALUES($1,$2,'{"workspace_id":"quota-slots","user_id":"quota-user"}',repeat('a',64),'{}','{}')`, f.ws, "admission-"+request); err != nil {
		return "", err
	}
	if err := exec(`INSERT INTO weave_capability_workflow_invocations(
	 workspace_id,application_id,request_id,invocation_id,credential_id,capability_id,revision,definition_hash,
	 workflow_id,workflow_version,input,input_hash,admission_request_id,run_id,task_id)
	 VALUES($1,$2,$3,$4,$5,'quota-cap',1,$6,'flow',1,'{}'::jsonb,repeat('a',64),$7,$8,$9)`,
		f.ws, app, request, invocation, credential, f.hash, "admission-"+request, "run-"+request, "task-"+request); err != nil {
		return "", err
	}
	return invocation, nil
}

// admit runs one admission, refreshing the slots and retrying once when they
// are full as Invoke does, and reports whether it took a slot.
func (f *quotaSlotFixture) admit(ctx context.Context) (string, bool, error) {
	request := f.request()
	for attempt := 0; ; attempt++ {
		err := f.tryAdmit(ctx, request)
		if !errors.Is(err, errQuotaSlotsFull) {
			return request, err == nil, err
		}
		if attempt == 1 {
			return "", false, nil
		}
		if err := refreshQuotaSlots(ctx, f.pool, f.ws, ""); err != nil {
			return "", false, err
		}
	}
}

func (f *quotaSlotFixture) tryAdmit(ctx context.Context, request string) error {
	tx, err := f.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	exec := func(sql string, args ...any) error { _, err := tx.Exec(ctx, sql, args...); return err }
	invocation, err := f.insertInvocation(exec, request)
	if err != nil {
		return err
	}
	if err := acquireCapabilityQuotaSlot(ctx, tx, f.ws, slotHolder{Kind: slotHolderWorkflow, ApplicationID: f.app, ID: invocation}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// end closes the admission of request, so its invocation is no longer in flight.
func (f *quotaSlotFixture) end(request string) {
	f.t.Helper()
	if _, err := f.pool.Exec(f.t.Context(), `INSERT INTO weave_kernel_publication_requests(workspace_id,request_id,operation,actor_subject,request_digest,receipt)
	 VALUES($1,$2,'published_closed','{"workspace_id":"quota-slots","user_id":"quota-user"}',repeat('a',64),'{}')`, f.ws, "admission-"+request); err != nil {
		f.t.Fatal(err)
	}
}

// admitConcurrently runs n admissions at once and returns the admitted requests.
func (f *quotaSlotFixture) admitConcurrently(n int) []string {
	f.t.Helper()
	var mu sync.Mutex
	var admitted []string
	var failures []error
	var wg sync.WaitGroup
	for range n {
		wg.Go(func() {
			request, ok, err := f.admit(f.t.Context())
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				failures = append(failures, err)
			} else if ok {
				admitted = append(admitted, request)
			}
		})
	}
	wg.Wait()
	if len(failures) > 0 {
		f.t.Fatal(errors.Join(failures...))
	}
	return admitted
}

func (f *quotaSlotFixture) heldSlots() int {
	f.t.Helper()
	var held int
	if err := f.pool.QueryRow(f.t.Context(), `SELECT count(*) FROM weave_capability_quota_slots WHERE workspace_id=$1 AND holder_kind IS NOT NULL`, f.ws).Scan(&held); err != nil {
		f.t.Fatal(err)
	}
	return held
}

func TestQuotaSlotsAdmitExactlyTheQuotaUnderContentionRealPG(t *testing.T) {
	f := newQuotaSlotFixture(t, 5)
	admitted := f.admitConcurrently(40)
	if len(admitted) != 5 || f.heldSlots() != 5 {
		t.Fatalf("admitted %d of 40 with quota 5, %d slots held", len(admitted), f.heldSlots())
	}
	// Ended invocations release their slots to the next admissions, and only
	// as many as ended.
	f.end(admitted[0])
	f.end(admitted[1])
	if again := f.admitConcurrently(20); len(again) != 2 || f.heldSlots() != 5 {
		t.Fatalf("after two ended, admitted %d more, %d slots held", len(again), f.heldSlots())
	}
	// An admission that rolls back, as a crashed process does, holds nothing.
	f.end(admitted[2])
	if err := refreshQuotaSlots(t.Context(), f.pool, f.ws, ""); err != nil {
		t.Fatal(err)
	}
	tx, err := f.pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	exec := func(sql string, args ...any) error { _, err := tx.Exec(t.Context(), sql, args...); return err }
	request := f.request()
	invocation, err := f.insertInvocation(exec, request)
	if err != nil {
		t.Fatal(err)
	}
	if err := acquireCapabilityQuotaSlot(t.Context(), tx, f.ws, slotHolder{Kind: slotHolderWorkflow, ApplicationID: f.app, ID: invocation}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Rollback(t.Context()); err != nil {
		t.Fatal(err)
	}
	if again := f.admitConcurrently(5); len(again) != 1 {
		t.Fatalf("after a rolled back admission, admitted %d", len(again))
	}
}

// An admission that finds the slots full while another reclaim is releasing
// one waits for that release instead of being refused beside a free slot.
func TestQuotaSlotsRefuseOnlyAfterReleasesInProgressRealPG(t *testing.T) {
	f := newQuotaSlotFixture(t, 1)
	admitted := f.admitConcurrently(1)
	if len(admitted) != 1 {
		t.Fatalf("admitted %d with quota 1", len(admitted))
	}
	f.end(admitted[0])
	reclaim, err := f.pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reclaim.Rollback(t.Context()) }()
	if err := reclaimQuotaSlots(t.Context(), reclaim, quotaScope{WorkspaceID: f.ws}); err != nil {
		t.Fatal(err)
	}
	result := make(chan bool, 1)
	go func() {
		_, ok, err := f.admit(t.Context())
		if err != nil {
			t.Error(err)
		}
		result <- ok
	}()
	select {
	case ok := <-result:
		t.Fatalf("admission finished while a release was in progress: admitted=%v", ok)
	case <-time.After(500 * time.Millisecond):
	}
	if err := reclaim.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	if !<-result {
		t.Fatal("admission was refused although the release freed a slot")
	}
}

func TestQuotaSlotsFollowQuotaChangesWithoutEvictingRealPG(t *testing.T) {
	f := newQuotaSlotFixture(t, 4)
	admitted := f.admitConcurrently(10)
	if len(admitted) != 4 {
		t.Fatalf("admitted %d with quota 4", len(admitted))
	}
	// A cut keeps all four in flight and admits nothing until fewer than two
	// remain, while admissions contend with the change itself.
	var wg sync.WaitGroup
	wg.Go(func() {
		if _, err := f.store.SetQuota(t.Context(), f.ws, Quota{MaxStepsPerInvocation: 100, MaxActiveInvocations: 2}); err != nil {
			t.Error(err)
		}
	})
	during := f.admitConcurrently(10)
	wg.Wait()
	if len(during) != 0 || f.heldSlots() != 4 {
		t.Fatalf("with four in flight, a cut to two admitted %d and kept %d", len(during), f.heldSlots())
	}
	f.end(admitted[0])
	f.end(admitted[1])
	if again := f.admitConcurrently(10); len(again) != 0 {
		t.Fatalf("with two in flight and quota two, admitted %d", len(again))
	}
	f.end(admitted[2])
	if again := f.admitConcurrently(10); len(again) != 1 {
		t.Fatalf("with one in flight and quota two, admitted %d", len(again))
	}
	// A raise admits up to the new limit at once.
	if _, err := f.store.SetQuota(t.Context(), f.ws, Quota{MaxStepsPerInvocation: 100, MaxActiveInvocations: 6}); err != nil {
		t.Fatal(err)
	}
	if again := f.admitConcurrently(10); len(again) != 4 || f.heldSlots() != 6 {
		t.Fatalf("after a raise to six, admitted %d, %d slots held", len(again), f.heldSlots())
	}
	var slots int
	if err := f.pool.QueryRow(t.Context(), `SELECT count(*) FROM weave_capability_quota_slots WHERE workspace_id=$1`, f.ws).Scan(&slots); err != nil || slots != 6 {
		t.Fatalf("slot rows = %d %v, want 6", slots, err)
	}
}

func TestQuotaSlotsStartFromWorkAlreadyInFlightRealPG(t *testing.T) {
	f := newQuotaSlotFixture(t, 3)
	// Work admitted before slots existed: four in flight against a quota of
	// three, which a fresh workspace's first admission must respect.
	var before []string
	for range 4 {
		request := f.request()
		exec := func(sql string, args ...any) error { _, err := f.pool.Exec(t.Context(), sql, args...); return err }
		if _, err := f.insertInvocation(exec, request); err != nil {
			t.Fatal(err)
		}
		before = append(before, request)
	}
	var ready bool
	if err := f.pool.QueryRow(t.Context(), `SELECT slots_ready FROM weave_capability_quotas WHERE workspace_id=$1`, f.ws).Scan(&ready); err != nil || ready {
		t.Fatalf("slots ready before any admission: %v %v", ready, err)
	}
	if admitted := f.admitConcurrently(10); len(admitted) != 0 || f.heldSlots() != 4 {
		t.Fatalf("with four in flight and quota three, admitted %d, %d slots held", len(admitted), f.heldSlots())
	}
	f.end(before[0])
	f.end(before[1])
	if admitted := f.admitConcurrently(10); len(admitted) != 1 || f.heldSlots() != 3 {
		t.Fatalf("with two in flight and quota three, admitted %d, %d slots held", len(admitted), f.heldSlots())
	}
	// The slot beyond the quota goes once all the earlier work has ended.
	f.end(before[2])
	f.end(before[3])
	if err := refreshQuotaSlots(t.Context(), f.pool, f.ws, ""); err != nil {
		t.Fatal(err)
	}
	var slots, retired int
	if err := f.pool.QueryRow(t.Context(), `SELECT count(*),count(*) FILTER (WHERE retired) FROM weave_capability_quota_slots WHERE workspace_id=$1`, f.ws).Scan(&slots, &retired); err != nil || slots != 3 || retired != 0 {
		t.Fatalf("after the earlier work ended: %d slots, %d retired, %v", slots, retired, err)
	}
}

// When the slots fill up with work that has ended, a burst of admissions that
// all find them full must each be admitted once the ended work is reclaimed,
// not refused because another admission's reclaim was still releasing them.
func TestQuotaSlotsAdmitABurstOnceEndedWorkIsReclaimedRealPG(t *testing.T) {
	f := newQuotaSlotFixture(t, 20)
	first := f.admitConcurrently(20)
	if len(first) != 20 {
		t.Fatalf("admitted %d of 20", len(first))
	}
	for _, request := range first[:15] {
		f.end(request)
	}
	for round := 0; round < 3; round++ {
		burst := f.admitConcurrently(15)
		if len(burst) != 15 {
			t.Fatalf("round %d: admitted %d of 15 with 15 slots held by ended work", round, len(burst))
		}
		for _, request := range burst {
			f.end(request)
		}
	}
}
