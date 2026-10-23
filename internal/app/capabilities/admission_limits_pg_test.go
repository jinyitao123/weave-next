package capabilities

import (
	"context"
	"errors"
	"sync"
	"testing"
)

type limitedApp struct{ id, credential string }

func (f *quotaSlotFixture) addApplication(name string) limitedApp {
	f.t.Helper()
	access := NewAccessStore(f.pool)
	app, err := access.CreateApplication(f.t.Context(), f.ws, "quota-user", name)
	if err != nil {
		f.t.Fatal(err)
	}
	credential, _, err := access.IssueCredential(f.t.Context(), f.ws, "quota-user", app.ID, name+" key", []string{"invoke"})
	if err != nil {
		f.t.Fatal(err)
	}
	return limitedApp{app.ID, credential.ID}
}

func (f *quotaSlotFixture) setLimit(app string, limit *int) {
	f.t.Helper()
	if err := NewAccessStore(f.pool).SetApplicationActiveLimit(f.t.Context(), f.ws, "quota-admin", app, limit); err != nil {
		f.t.Fatal(err)
	}
}

// admitAs runs one admission of app as Invoke does: on a full scope it
// refreshes both scopes and retries once, and then reports which scope refused.
func (f *quotaSlotFixture) admitAs(ctx context.Context, app limitedApp) (request string, refused error, err error) {
	request = f.request()
	for attempt := 0; ; attempt++ {
		err := f.tryAdmitAs(ctx, request, app)
		full := errors.Is(err, errQuotaSlotsFull) || errors.Is(err, errApplicationQuotaSlotsFull)
		if !full {
			return request, nil, err
		}
		if attempt == 1 {
			return "", err, nil
		}
		if err := refreshQuotaSlots(ctx, f.pool, f.ws, app.id); err != nil {
			return "", nil, err
		}
	}
}

func (f *quotaSlotFixture) tryAdmitAs(ctx context.Context, request string, app limitedApp) error {
	tx, err := f.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	exec := func(sql string, args ...any) error { _, err := tx.Exec(ctx, sql, args...); return err }
	invocation, err := f.insertInvocationFor(exec, request, app.id, app.credential)
	if err != nil {
		return err
	}
	if err := acquireCapabilityQuotaSlot(ctx, tx, f.ws, slotHolder{Kind: slotHolderWorkflow, ApplicationID: app.id, ID: invocation}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// admitAllAs runs n admissions of app at once; it returns the admitted
// requests and how many each scope refused.
func (f *quotaSlotFixture) admitAllAs(app limitedApp, n int) (admitted []string, byApplication, byWorkspace int) {
	f.t.Helper()
	var mu sync.Mutex
	var failures []error
	var wg sync.WaitGroup
	for range n {
		wg.Go(func() {
			request, refused, err := f.admitAs(f.t.Context(), app)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err != nil:
				failures = append(failures, err)
			case errors.Is(refused, errApplicationQuotaSlotsFull):
				byApplication++
			case errors.Is(refused, errQuotaSlotsFull):
				byWorkspace++
			default:
				admitted = append(admitted, request)
			}
		})
	}
	wg.Wait()
	if len(failures) > 0 {
		f.t.Fatal(errors.Join(failures...))
	}
	return admitted, byApplication, byWorkspace
}

func newLimitFixture(t *testing.T, workspaceLimit int) *quotaSlotFixture {
	t.Helper()
	f := newQuotaSlotFixture(t, workspaceLimit)
	if _, err := f.pool.Exec(t.Context(), `INSERT INTO weave_users(id,tenant_id,username,password,role) VALUES('quota-admin','quota-slots','admin','unused','admin')`); err != nil {
		t.Fatal(err)
	}
	return f
}

func intPointer(value int) *int { return &value }

// One application reaching its own limit is refused for that reason, and a
// second application still gets what the workspace has left (#94).
func TestApplicationLimitRefusesOnlyItsApplicationRealPG(t *testing.T) {
	f := newLimitFixture(t, 5)
	game, desk := f.addApplication("game"), f.addApplication("desk")
	f.setLimit(game.id, intPointer(2))

	admitted, byApplication, byWorkspace := f.admitAllAs(game, 10)
	if len(admitted) != 2 || byApplication != 8 || byWorkspace != 0 {
		t.Fatalf("game: admitted %d, refused by application %d, by workspace %d", len(admitted), byApplication, byWorkspace)
	}
	others, byApplication, byWorkspace := f.admitAllAs(desk, 10)
	if len(others) != 3 || byApplication != 0 || byWorkspace != 7 {
		t.Fatalf("desk: admitted %d, refused by application %d, by workspace %d", len(others), byApplication, byWorkspace)
	}
	// Ending the game's work frees its slots in both scopes.
	for _, request := range admitted {
		f.end(request)
	}
	again, byApplication, byWorkspace := f.admitAllAs(game, 4)
	if len(again) != 2 || byApplication != 2 || byWorkspace != 0 {
		t.Fatalf("after ending: admitted %d, refused by application %d, by workspace %d", len(again), byApplication, byWorkspace)
	}
}

// A limit set while work is in flight counts that work; lowering it evicts
// nothing; removing it leaves the workspace quota alone in force.
func TestApplicationLimitCountsInFlightWorkAndChangesSafelyRealPG(t *testing.T) {
	f := newLimitFixture(t, 10)
	game := f.addApplication("game")
	first, _, _ := f.admitAllAs(game, 3)
	if len(first) != 3 {
		t.Fatalf("unlimited admitted %d of 3", len(first))
	}
	f.setLimit(game.id, intPointer(3))
	if admitted, byApplication, _ := f.admitAllAs(game, 2); len(admitted) != 0 || byApplication != 2 {
		t.Fatalf("limit 3 with 3 in flight admitted %d", len(admitted))
	}
	f.setLimit(game.id, intPointer(1))
	f.end(first[0])
	if admitted, _, _ := f.admitAllAs(game, 1); len(admitted) != 0 {
		t.Fatal("lowered limit admitted while in-flight work is still above it")
	}
	f.end(first[1])
	f.end(first[2])
	if admitted, _, _ := f.admitAllAs(game, 3); len(admitted) != 1 {
		t.Fatalf("lowered limit 1 admitted %d once the work ended", len(admitted))
	}
	f.setLimit(game.id, nil)
	if admitted, _, byWorkspace := f.admitAllAs(game, 12); len(admitted) != 9 || byWorkspace != 3 {
		t.Fatalf("without a limit admitted %d (workspace refused %d), want the workspace's 9 left", len(admitted), byWorkspace)
	}
}

func TestApplicationLimitIsAnAdministratorSettingRealPG(t *testing.T) {
	f := newLimitFixture(t, 5)
	game := f.addApplication("game")
	access := NewAccessStore(f.pool)
	if err := access.SetApplicationActiveLimit(t.Context(), f.ws, "quota-user", game.id, intPointer(2)); !errors.Is(err, ErrAccessDenied) {
		t.Fatalf("developer set the limit: %v", err)
	}
	if err := access.SetApplicationActiveLimit(t.Context(), f.ws, "quota-admin", game.id, intPointer(0)); err == nil {
		t.Fatal("accepted a zero limit")
	}
	if err := access.SetApplicationActiveLimit(t.Context(), f.ws, "quota-admin", "app_missing", intPointer(2)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("limited a missing application: %v", err)
	}
	f.setLimit(game.id, intPointer(2))
	snapshot, err := access.Snapshot(t.Context(), f.ws, "quota-admin")
	if err != nil {
		t.Fatal(err)
	}
	for _, app := range snapshot.Apps {
		if app.ID == game.id && (app.MaxActiveInvocations == nil || *app.MaxActiveInvocations != 2) {
			t.Fatalf("snapshot shows limit %v", app.MaxActiveInvocations)
		}
		if app.ID == f.app && app.MaxActiveInvocations != nil {
			t.Fatalf("unlimited application shows limit %v", *app.MaxActiveInvocations)
		}
	}
}
