package accesschange

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jinyitao123/weave/internal/base/db"
	"github.com/jinyitao123/weave/internal/base/execution"
	"github.com/jinyitao123/weave/internal/base/frozen"
	"github.com/jinyitao123/weave/internal/base/testutil"
	"github.com/jinyitao123/weave/internal/kernel/admissionfence"
	"github.com/jinyitao123/weave/internal/kernel/publicationservice"
	"github.com/jinyitao123/weave/internal/kernel/workflow/machine"
)

type realFenceAuthority struct{ store *Store }

func (a realFenceAuthority) Authorize(context.Context, string, frozen.ArtifactEnvelopeV1) (machine.ValidationContext, error) {
	return machine.ValidationContext{}, errors.New("not a publication fixture")
}
func (a realFenceAuthority) AuthorizeFence(ctx context.Context, c admissionfence.Command) error {
	return a.store.AuthorizeFence(ctx, c)
}

type lostFenceResponse struct {
	service admissionfence.Service
	lose    atomic.Bool
	action  admissionfence.Action
}

func (k *lostFenceResponse) TransitionFence(ctx context.Context, c admissionfence.Command) (admissionfence.Receipt, error) {
	r, err := k.service.TransitionFence(ctx, c)
	if err == nil && c.Action == k.action && k.lose.CompareAndSwap(true, false) {
		return admissionfence.Receipt{}, errors.New("response lost after kernel commit")
	}
	return r, err
}
func accessPG(t *testing.T) (context.Context, *pgxpool.Pool, Flow) {
	t.Helper()
	ctx, cancel := context.WithTimeout(execution.WithSubject(context.Background(), execution.Subject{WorkspaceID: "ws", UserID: "alice"}), 20*time.Second)
	t.Cleanup(cancel)
	pool := testutil.PostgresPool(t)
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	_, err := pool.Exec(ctx, `INSERT INTO weave_workspaces(id,slug,name)VALUES('ws','ws','Workspace');INSERT INTO weave_users(id,tenant_id,username,password,role)VALUES('alice','ws','alice','x','admin'),('bob','ws','bob','x','member'),('carol','ws','carol','x','member');INSERT INTO weave_members(workspace_id,user_id,role)VALUES('ws','alice','owner'),('ws','bob','member'),('ws','carol','member')`)
	if err != nil {
		t.Fatal(err)
	}
	cfg := pool.Config()
	cfg.MaxConns = 1
	single, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(single.Close)
	store := NewStore(single)
	connection, err := url.Parse(os.Getenv("TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	query := connection.Query()
	query.Set("search_path", cfg.ConnConfig.RuntimeParams["search_path"])
	connection.RawQuery = query.Encode()
	kernel, err := publicationservice.Open(ctx, connection.String(), realFenceAuthority{store: store})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(kernel.Close)
	return ctx, pool, Flow{Store: store, Kernel: kernel}
}
func userIntent(id, user string, disabled bool) Intent {
	mutation, _ := json.Marshal(map[string]bool{"disabled": disabled})
	digest, _ := SourceDigest(json.RawMessage(mutation))
	i := Intent{WorkspaceID: "ws", OperationID: id, Kind: "user.access", TargetID: user, Mutation: mutation, SourceDigest: digest}
	resource := admissionfence.Actor(execution.Subject{UserID: user})
	if disabled {
		i.Block = []admissionfence.Resource{resource}
	} else {
		i.Grant = []admissionfence.Resource{resource}
	}
	return i
}
func setDisabled(user string, disabled bool) Mutation {
	return func(ctx context.Context, tx pgx.Tx) (json.RawMessage, error) {
		_, err := tx.Exec(ctx, `UPDATE weave_users SET disabled=$2 WHERE id=$1`, user, disabled)
		return json.RawMessage(`{"saved":true}`), err
	}
}
func fenceEpoch(t *testing.T, ctx context.Context, pool *pgxpool.Pool, user string) (int64, bool) {
	t.Helper()
	var epoch int64
	var blocked bool
	if err := pool.QueryRow(ctx, `SELECT epoch,blocked FROM weave_resource_admission_fences WHERE workspace_id='ws' AND resource_kind='actor_user' AND resource_id=$1 AND resource_version='*'`, user).Scan(&epoch, &blocked); err != nil {
		t.Fatal(err)
	}
	return epoch, blocked
}

func TestAccessChangeUnknownCommitFailureAndTwoUserIsolationRealPG(t *testing.T) {
	ctx, pool, flow := accessPG(t)
	unreliable := &lostFenceResponse{service: flow.Kernel, action: admissionfence.Block}
	unreliable.lose.Store(true)
	flow.Kernel = unreliable
	intent := userIntent("disable-bob", "bob", true)
	if _, err := flow.Apply(ctx, intent, setDisabled("bob", true)); err == nil {
		t.Fatal("unknown kernel result claimed complete")
	}
	if epoch, blocked := fenceEpoch(t, ctx, pool, "bob"); epoch != 1 || !blocked {
		t.Fatalf("lost block %d %v", epoch, blocked)
	}
	var disabled bool
	if err := pool.QueryRow(ctx, `SELECT disabled FROM weave_users WHERE id='bob'`).Scan(&disabled); err != nil || disabled {
		t.Fatalf("mutation before block receipt %v %v", disabled, err)
	}
	_, err := flow.Apply(ctx, userIntent("enable-bob-too-early", "bob", false), setDisabled("bob", false))
	var pending *PendingError
	if !errors.As(err, &pending) || pending.OperationID != intent.OperationID {
		t.Fatalf("pending operation not retained: %v", err)
	}
	// Product rollback does not undo the successful Kernel fence.
	if _, err = flow.Apply(ctx, intent, func(context.Context, pgx.Tx) (json.RawMessage, error) { return nil, errors.New("product write failed") }); err == nil {
		t.Fatal("failed product write claimed complete")
	}
	var state string
	var receipt bool
	if err = pool.QueryRow(ctx, `SELECT state,block_receipt IS NOT NULL FROM weave_access_change_operations WHERE operation_id='disable-bob'`).Scan(&state, &receipt); err != nil || state != "blocked" || !receipt {
		t.Fatalf("lost checkpoint %s %v %v", state, receipt, err)
	}
	result, err := flow.Apply(ctx, intent, setDisabled("bob", true))
	if err != nil || result.State != "completed" {
		t.Fatalf("same intent did not recover %+v %v", result, err)
	}
	if epoch, blocked := fenceEpoch(t, ctx, pool, "bob"); epoch != 1 || !blocked {
		t.Fatalf("retry repeated block %d %v", epoch, blocked)
	}
	again, err := flow.Apply(ctx, intent, func(context.Context, pgx.Tx) (json.RawMessage, error) {
		t.Error("completed mutation repeated")
		return nil, nil
	})
	if err != nil || again.State != "completed" {
		t.Fatal(err)
	}
	other := execution.WithSubject(ctx, execution.Subject{WorkspaceID: "ws", UserID: "carol"})
	if _, err = flow.Apply(other, intent, setDisabled("bob", false)); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("other user recovered administrator change %v", err)
	}
	altered := intent
	altered.Mutation = json.RawMessage(`{"disabled":false}`)
	if _, err = flow.Apply(ctx, altered, setDisabled("bob", false)); !errors.Is(err, ErrConflict) {
		t.Fatalf("old id accepted changed mutation %v", err)
	}
	if _, err = pool.Exec(ctx, `UPDATE weave_access_change_operations SET block_receipt='{}' WHERE operation_id='disable-bob'`); err == nil {
		t.Fatal("committed receipt replaced")
	}
}

func TestAccessChangeRegrantNeedsCommittedNewAuthorizationAndCannotReplayOverNewBlockRealPG(t *testing.T) {
	ctx, pool, flow := accessPG(t)
	disable := userIntent("disable-1", "bob", true)
	if _, err := flow.Apply(ctx, disable, setDisabled("bob", true)); err != nil {
		t.Fatal(err)
	}
	grant := userIntent("new-authorization", "bob", false)
	if _, err := flow.Kernel.TransitionFence(ctx, fenceCommand(grant, admissionfence.Regrant)); err == nil {
		t.Fatal("unpersisted grant accepted")
	}
	if _, err := flow.Store.reserve(ctx, grant); err != nil {
		t.Fatal(err)
	}
	if _, err := flow.Kernel.TransitionFence(ctx, fenceCommand(grant, admissionfence.Regrant)); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("regrant before product commit %v", err)
	}
	unreliable := &lostFenceResponse{service: flow.Kernel, action: admissionfence.Regrant}
	unreliable.lose.Store(true)
	flow.Kernel = unreliable
	if _, err := flow.Apply(ctx, grant, setDisabled("bob", false)); err == nil {
		t.Fatal("unknown regrant claimed complete")
	}
	if epoch, blocked := fenceEpoch(t, ctx, pool, "bob"); epoch != 2 || blocked {
		t.Fatalf("regrant failed %d %v", epoch, blocked)
	}
	secondDisable := userIntent("disable-2", "bob", true)
	var pending *PendingError
	if _, err := flow.Apply(ctx, secondDisable, setDisabled("bob", true)); !errors.As(err, &pending) {
		t.Fatalf("new mutation overtook unconfirmed regrant %v", err)
	}
	if result, err := flow.Apply(ctx, grant, func(context.Context, pgx.Tx) (json.RawMessage, error) {
		t.Error("committed product authorization repeated")
		return nil, nil
	}); err != nil || result.State != "completed" {
		t.Fatalf("regrant recovery %+v %v", result, err)
	}
	if _, err := flow.Apply(ctx, secondDisable, setDisabled("bob", true)); err != nil {
		t.Fatal(err)
	}
	if _, err := flow.Kernel.TransitionFence(ctx, fenceCommand(grant, admissionfence.Regrant)); !errors.Is(err, ErrConflict) {
		t.Fatalf("superseded grant recovered over newer revoke %v", err)
	}
	if _, err := flow.Apply(ctx, grant, setDisabled("bob", false)); err != nil {
		t.Fatal(err)
	}
	if epoch, blocked := fenceEpoch(t, ctx, pool, "bob"); epoch != 3 || !blocked {
		t.Fatalf("old operation reopened resource %d %v", epoch, blocked)
	}
}

func TestAccessChangeMultipleResourcesKeepBlockReceiptAcrossProductRollbackRealPG(t *testing.T) {
	ctx, pool, flow := accessPG(t)
	intent := userIntent("disable-two", "bob", true)
	intent.Block = append(intent.Block, admissionfence.Actor(execution.Subject{UserID: "carol"}))
	mutate := func(ctx context.Context, tx pgx.Tx) (json.RawMessage, error) {
		if _, err := tx.Exec(ctx, `UPDATE weave_users SET disabled=true WHERE id='bob'`); err != nil {
			return nil, err
		}
		return nil, errors.New("second product mutation failed")
	}
	if _, err := flow.Apply(ctx, intent, mutate); err == nil {
		t.Fatal("partial product change accepted")
	}
	for _, user := range []string{"bob", "carol"} {
		if epoch, blocked := fenceEpoch(t, ctx, pool, user); epoch != 1 || !blocked {
			t.Fatalf("resource %s not conservatively blocked %d %v", user, epoch, blocked)
		}
	}
	var disabled int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM weave_users WHERE disabled`).Scan(&disabled); err != nil || disabled != 0 {
		t.Fatalf("partial product mutation committed %d %v", disabled, err)
	}
	result, err := flow.Apply(ctx, intent, func(ctx context.Context, tx pgx.Tx) (json.RawMessage, error) {
		_, err := tx.Exec(ctx, `UPDATE weave_users SET disabled=true WHERE id IN ('bob','carol')`)
		return json.RawMessage(`{"saved":true}`), err
	})
	if err != nil || result.BlockReceipt == nil || len(result.BlockReceipt.States) != 2 || result.State != "completed" {
		t.Fatalf("multi-resource resume %+v %v", result, err)
	}
}

func TestAccessChangeRejectedIntentLeavesNoFenceOrPendingResourceRealPG(t *testing.T) {
	ctx, pool, flow := accessPG(t)
	flow.Validate = func(context.Context, pgx.Tx) error { return errors.New("target no longer accepts this change") }
	_, err := flow.Apply(ctx, userIntent("invalid-target", "bob", true), setDisabled("bob", true))
	var rejected *RejectedError
	if !errors.As(err, &rejected) {
		t.Fatalf("invalid intent not rejected early: %v", err)
	}
	for _, table := range []string{"weave_access_change_operations", "weave_access_change_resources", "weave_resource_admission_operations", "weave_resource_admission_fences"} {
		var n int
		if err := pool.QueryRow(ctx, "SELECT count(*) FROM "+table).Scan(&n); err != nil || n != 0 {
			t.Fatalf("rejected intent left %s %d %v", table, n, err)
		}
	}
}

func TestAccessChangeCurrentPersonalCredentialOwnerAndForeignReceiptRealPG(t *testing.T) {
	ctx, _, flow := accessPG(t)
	bob := execution.WithSubject(ctx, execution.Subject{WorkspaceID: "ws", UserID: "bob"})
	ref := frozen.CredentialReference{SchemaVersion: 1, WorkspaceID: "ws", Kind: frozen.CredentialProviderAPIKey, ResourceID: "personal-provider", Slot: "api_key", Scope: frozen.CredentialScopeUser, UserID: "bob"}
	mutation := json.RawMessage(`{"revoked":true}`)
	digest, _ := SourceDigest(mutation)
	intent := Intent{WorkspaceID: "ws", OperationID: "revoke-personal-key", Kind: "provider.revoke", TargetID: "personal-provider", Mutation: mutation, SourceDigest: digest, Block: []admissionfence.Resource{admissionfence.CredentialResource(ref)}}
	if _, err := flow.Apply(bob, intent, func(context.Context, pgx.Tx) (json.RawMessage, error) { return json.RawMessage(`{}`), nil }); err != nil {
		t.Fatal(err)
	}
	if _, err := flow.Store.Get(ctx, "ws", intent.OperationID); !errors.Is(err, execution.ErrSubjectMismatch) {
		t.Fatalf("administrator received another actor's receipt: %v", err)
	}
	ref.UserID = "carol"
	intent.OperationID = "spoof-another-user"
	intent.Block = []admissionfence.Resource{admissionfence.CredentialResource(ref)}
	if _, err := flow.Apply(bob, intent, nil); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("personal actor fenced another user's credential: %v", err)
	}
	ref.Scope = frozen.CredentialScopeWorkspaceService
	ref.UserID = ""
	ref.ServiceID = "provider:shared"
	intent.OperationID = "spoof-shared-service"
	intent.Block = []admissionfence.Resource{admissionfence.CredentialResource(ref)}
	if _, err := flow.Apply(bob, intent, nil); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("personal actor fenced workspace service credential: %v", err)
	}
}

type pausedFence struct {
	service        admissionfence.Service
	ready, release chan struct{}
}

func (k pausedFence) TransitionFence(ctx context.Context, c admissionfence.Command) (admissionfence.Receipt, error) {
	r, err := k.service.TransitionFence(ctx, c)
	if err == nil {
		close(k.ready)
		<-k.release
	}
	return r, err
}
func TestAccessChangeConcurrentOperatorCannotOvertakeUnconfirmedFenceRealPG(t *testing.T) {
	ctx, pool, flow := accessPG(t)
	cfg := flow.Store.pool.Config()
	cfg.MaxConns = 4
	concurrent, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer concurrent.Close()
	flow.Store = NewStore(concurrent)
	ready, release := make(chan struct{}), make(chan struct{})
	waiting := flow
	waiting.Kernel = pausedFence{service: flow.Kernel, ready: ready, release: release}
	outcome := make(chan error, 1)
	go func() {
		_, err := waiting.Apply(ctx, userIntent("first-revoke", "bob", true), setDisabled("bob", true))
		outcome <- err
	}()
	<-ready
	_, err = flow.Apply(ctx, userIntent("concurrent-regrant", "bob", false), setDisabled("bob", false))
	var pending *PendingError
	if !errors.As(err, &pending) || pending.OperationID != "first-revoke" {
		t.Fatalf("unconfirmed block was overtaken %v", err)
	}
	close(release)
	if err = <-outcome; err != nil {
		t.Fatal(err)
	}
	if epoch, blocked := fenceEpoch(t, ctx, pool, "bob"); epoch != 1 || !blocked {
		t.Fatalf("concurrent operation changed fence %d %v", epoch, blocked)
	}
}
