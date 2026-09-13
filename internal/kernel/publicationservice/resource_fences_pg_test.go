package publicationservice

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jinyitao123/weave/internal/base/execution"
	"github.com/jinyitao123/weave/internal/base/frozen"
	"github.com/jinyitao123/weave/internal/kernel/admissionfence"
	"github.com/jinyitao123/weave/internal/kernel/publication"
	"github.com/jinyitao123/weave/internal/kernel/workflow/machine"
)

type resourceTestAuthority struct {
	Authority
	published PublishedAuthority
	admitGate func(context.Context) error
	fenceGate func(context.Context, admissionfence.Command) error
}

func (a resourceTestAuthority) AuthorizePublished(ctx context.Context, r publication.PublishedRunRequest, e frozen.ArtifactEnvelopeV1) error {
	if a.admitGate != nil {
		if err := a.admitGate(ctx); err != nil {
			return err
		}
	}
	return a.published.AuthorizePublished(ctx, r, e)
}
func (a resourceTestAuthority) Authorize(ctx context.Context, op string, e frozen.ArtifactEnvelopeV1) (machine.ValidationContext, error) {
	if op == "candidate_run" && a.admitGate != nil {
		if err := a.admitGate(ctx); err != nil {
			return machine.ValidationContext{}, err
		}
	}
	return a.Authority.Authorize(ctx, op, e)
}
func (a resourceTestAuthority) AuthorizeFence(ctx context.Context, c admissionfence.Command) error {
	subject, err := execution.RequireSubject(ctx, c.WorkspaceID)
	if err != nil {
		return err
	}
	if a.fenceGate != nil {
		if err := a.fenceGate(ctx, c); err != nil {
			return err
		}
	}
	if subject.UserID != "alice" {
		return errors.New("operator not authorized")
	}
	return nil
}
func testFenceCommand(operation string, action admissionfence.Action, resources ...admissionfence.Resource) admissionfence.Command {
	return admissionfence.Command{Version: admissionfence.ContractVersion, WorkspaceID: "workspace", OperationID: operation, Action: action, Resources: resources}
}

func TestResourceFenceStopsAuthorizedPublishedAndCandidateAdmissionsRealPG(t *testing.T) {
	for _, candidate := range []bool{false, true} {
		for _, regrant := range []bool{false, true} {
			t.Run(fmt.Sprintf("candidate-%v-regrant-%v", candidate, regrant), func(t *testing.T) {
				ctx, service, pool, published := publishedPGFixture(t)
				var envelope frozen.ArtifactEnvelopeV1
				if err := pool.QueryRow(ctx, `SELECT jsonb_build_object('workspace_id',workspace_id,'workflow_id',workflow_id,'workflow_version',workflow_version,'artifact_schema_version',artifact_schema_version,'canonicalization_algorithm',canonicalization_algorithm,'canonicalization_version',canonicalization_version,'hash_algorithm',hash_algorithm,'content_hash',content_hash,'payload',payload) FROM weave_published_artifact_contents`).Scan(&envelope); err != nil {
					t.Fatal(err)
				}
				ready, release := make(chan struct{}), make(chan struct{})
				wrapper := resourceTestAuthority{Authority: service.authority, published: service.authority.(PublishedAuthority), admitGate: func(context.Context) error { close(ready); <-release; return nil }}
				service.authority = wrapper
				outcome := make(chan error, 1)
				go func() {
					if candidate {
						_, err := service.AdmitCandidate(ctx, publication.CandidateRunRequest{Version: publication.ContractVersion, RequestID: "candidate-fence", Candidate: envelope, Input: json.RawMessage(`"task"`), InputVersion: "v1", SourceRef: "build", Purpose: "evaluation"})
						outcome <- err
					} else {
						_, err := service.AdmitPublished(ctx, published)
						outcome <- err
					}
				}()
				<-ready
				block := testFenceCommand("block-team", admissionfence.Block, admissionfence.Team("team"))
				receipt, err := service.TransitionFence(ctx, block)
				if err != nil {
					t.Fatal(err)
				}
				if receipt.States[0].Epoch != 1 || !receipt.States[0].Blocked {
					t.Fatalf("block receipt %+v", receipt)
				}
				if regrant {
					grant := block
					grant.OperationID = "new-team-authorization"
					grant.Action = admissionfence.Regrant
					renewed, err := service.TransitionFence(ctx, grant)
					if err != nil || renewed.States[0].Epoch != 2 || renewed.States[0].Blocked {
						t.Fatalf("regrant %+v %v", renewed, err)
					}
				}
				close(release)
				err = <-outcome
				want := admissionfence.ErrBlocked
				if regrant {
					want = admissionfence.ErrChanged
				}
				if !errors.Is(err, want) {
					t.Fatalf("old proof crossed fence: %v want %v", err, want)
				}
				var count int
				if err = pool.QueryRow(ctx, `SELECT count(*) FROM weave_task_queue`).Scan(&count); err != nil || count != 0 {
					t.Fatalf("revoked admission queued %d %v", count, err)
				}
				// The newly authorized attempt captures the new generation, while all
				// previous grants stay stale. No execution identity was renewed by regrant.
				if regrant {
					wrapper.admitGate = nil
					service.authority = wrapper
					if _, err = service.AdmitPublished(ctx, published); err != nil {
						t.Fatal(err)
					}
				}
			})
		}
	}
}

func TestResourceFenceUnknownCommitReplayAndActorIsolationRealPG(t *testing.T) {
	ctx, service, pool, request := publishedPGFixture(t)
	service.authority = resourceTestAuthority{Authority: service.authority, published: service.authority.(PublishedAuthority)}
	block := testFenceCommand("remove-alice", admissionfence.Block, admissionfence.Actor(execution.Subject{UserID: "alice"}))
	first, err := service.TransitionFence(ctx, block)
	if err != nil {
		t.Fatal(err)
	}
	// Discarding the first response models an unknown commit. The operation ID
	// must recover its receipt without advancing the epoch again.
	again, err := service.TransitionFence(ctx, block)
	if err != nil || again.Digest != first.Digest || again.States[0] != first.States[0] {
		t.Fatalf("unknown commit repeated fence: %+v %v", again, err)
	}
	if _, err = service.AdmitPublished(ctx, request); !errors.Is(err, admissionfence.ErrBlocked) {
		t.Fatalf("blocked user admitted: %v", err)
	}
	bob := execution.WithSubject(ctx, execution.Subject{WorkspaceID: "workspace", UserID: "bob"})
	other := request
	other.RequestID = "bob-request"
	other.RunID = "bob-run"
	other.TaskID = "bob-task"
	if _, err = service.AdmitPublished(bob, other); err != nil {
		t.Fatalf("one user fence blocked another: %v", err)
	}
	if _, err = service.TransitionFence(bob, block); err == nil {
		t.Fatal("other user recovered operator receipt")
	}
	changed := block
	changed.Action = admissionfence.Regrant
	if _, err = service.TransitionFence(ctx, changed); !errors.Is(err, admissionfence.ErrConflict) {
		t.Fatalf("operation id reopens a block: %v", err)
	}
	var epoch int64
	if err = pool.QueryRow(ctx, `SELECT epoch FROM weave_resource_admission_fences WHERE resource_kind='actor_user' AND resource_id='alice'`).Scan(&epoch); err != nil || epoch != 1 {
		t.Fatalf("replay mutated epoch %d %v", epoch, err)
	}
	if _, err = pool.Exec(ctx, `UPDATE weave_resource_admission_fences SET blocked=false WHERE resource_kind='actor_user' AND resource_id='alice'`); err == nil {
		t.Fatal("blocked cleared without advancing generation")
	}
}

func TestResourceFenceLocksSerializeConcurrentAdmissionAndBlockRealPG(t *testing.T) {
	ctx, service, pool, template := publishedPGFixture(t)
	cfg := service.pool.Config()
	cfg.MaxConns = 4
	concurrent, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer concurrent.Close()
	service = &Service{pool: concurrent, authority: service.authority}
	service.authority = resourceTestAuthority{Authority: service.authority, published: service.authority.(PublishedAuthority)}
	for i := range 8 {
		grant := testFenceCommand(fmt.Sprintf("grant-%d", i), admissionfence.Regrant, admissionfence.Team("team"))
		if _, err := service.TransitionFence(ctx, grant); err != nil {
			t.Fatal(err)
		}
		request := template
		request.RequestID = fmt.Sprintf("race-request-%d", i)
		request.RunID = request.RequestID + "-run"
		request.TaskID = request.RequestID + "-task"
		var admitErr, blockErr error
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); _, admitErr = service.AdmitPublished(ctx, request) }()
		go func() {
			defer wg.Done()
			_, blockErr = service.TransitionFence(ctx, testFenceCommand(fmt.Sprintf("block-%d", i), admissionfence.Block, admissionfence.Team("team")))
		}()
		wg.Wait()
		if blockErr != nil {
			t.Fatal(blockErr)
		}
		if admitErr != nil && !errors.Is(admitErr, admissionfence.ErrBlocked) && !errors.Is(admitErr, admissionfence.ErrChanged) {
			t.Fatal(admitErr)
		}
		var count int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM weave_task_queue WHERE id=$1`, request.TaskID).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if (admitErr == nil && count != 1) || (admitErr != nil && count != 0) {
			t.Fatalf("partial admission count %d error %v", count, admitErr)
		}
		// Once Block returned, no later request may cross the boundary.
		request.RequestID += "-late"
		request.RunID += "-late"
		request.TaskID += "-late"
		if _, err := service.AdmitPublished(ctx, request); !errors.Is(err, admissionfence.ErrBlocked) {
			t.Fatalf("late request crossed completed block: %v", err)
		}
	}
}

func TestResourceFenceCredentialRevisionAndParentScopesRealPG(t *testing.T) {
	ctx, service, _, _ := publishedPGFixture(t)
	service.authority = resourceTestAuthority{Authority: service.authority, published: service.authority.(PublishedAuthority)}
	ref := frozen.CredentialReference{SchemaVersion: 1, WorkspaceID: "workspace", Kind: frozen.CredentialProviderAPIKey, ResourceID: "provider", Slot: "api_key", Scope: frozen.CredentialScopeUser, UserID: "alice"}
	other := ref
	other.UserID = "bob"
	resources := []admissionfence.Resource{admissionfence.Credential(ref, 7), admissionfence.Credential(ref, 8), admissionfence.CredentialResource(ref), admissionfence.Credential(other, 7), admissionfence.CredentialResource(other)}
	resources, err := admissionfence.Normalize(resources, true)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := service.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range resources {
		if err = ensureFenceTx(ctx, tx, "workspace", r); err != nil {
			t.Fatal(err)
		}
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	check := func(ref frozen.CredentialReference, version int64) error {
		t.Helper()
		keys, err := admissionfence.Normalize([]admissionfence.Resource{admissionfence.Credential(ref, version), admissionfence.CredentialResource(ref)}, true)
		if err != nil {
			t.Fatal(err)
		}
		stamps := make([]admissionfence.State, len(keys))
		for i, r := range keys {
			stamps[i] = admissionfence.State{Resource: r, Epoch: 0}
		}
		tx, err := service.pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		return checkResourceFencesTx(ctx, tx, "workspace", stamps)
	}
	if _, err = service.TransitionFence(ctx, testFenceCommand("retire-functional-v7", admissionfence.Block, admissionfence.Credential(ref, 7))); err != nil {
		t.Fatal(err)
	}
	if err = check(ref, 7); !errors.Is(err, admissionfence.ErrBlocked) {
		t.Fatalf("exact revision remained admitted %v", err)
	}
	if err = check(ref, 8); err != nil {
		t.Fatalf("exact revision fenced another version %v", err)
	}
	if err = check(other, 7); err != nil {
		t.Fatalf("credential owner scope crossed users %v", err)
	}
	if _, err = service.TransitionFence(ctx, testFenceCommand("revoke-all-provider-slots", admissionfence.Block, admissionfence.CredentialResource(ref))); err != nil {
		t.Fatal(err)
	}
	if err = check(ref, 8); !errors.Is(err, admissionfence.ErrBlocked) {
		t.Fatalf("parent revocation missed another revision %v", err)
	}
	if err = check(other, 7); err != nil {
		t.Fatalf("parent revocation crossed owners %v", err)
	}
}

func TestResourceFenceAccountRegrantDoesNotRestoreRemovedMembershipRealPG(t *testing.T) {
	ctx, service, _, request := publishedPGFixture(t)
	service.authority = resourceTestAuthority{Authority: service.authority, published: service.authority.(PublishedAuthority)}
	if _, err := service.TransitionFence(ctx, testFenceCommand("remove-membership", admissionfence.Block, admissionfence.Member("alice"))); err != nil {
		t.Fatal(err)
	}
	if _, err := service.TransitionFence(ctx, testFenceCommand("enable-account", admissionfence.Regrant, admissionfence.Actor(execution.Subject{UserID: "alice"}))); err != nil {
		t.Fatal(err)
	}
	if _, err := service.AdmitPublished(ctx, request); !errors.Is(err, admissionfence.ErrBlocked) {
		t.Fatalf("account re-enable restored membership: %v", err)
	}
	if _, err := service.TransitionFence(ctx, testFenceCommand("grant-new-membership", admissionfence.Regrant, admissionfence.Member("alice"))); err != nil {
		t.Fatal(err)
	}
	if _, err := service.AdmitPublished(ctx, request); err != nil {
		t.Fatalf("explicit new membership not usable: %v", err)
	}
}

func TestResourceFenceDelayedRegrantCannotReopenNewerRevocationRealPG(t *testing.T) {
	ctx, service, _, request := publishedPGFixture(t)
	ready, release := make(chan struct{}), make(chan struct{})
	service.authority = resourceTestAuthority{Authority: service.authority, published: service.authority.(PublishedAuthority), fenceGate: func(_ context.Context, c admissionfence.Command) error {
		if c.Action == admissionfence.Regrant {
			close(ready)
			<-release
		}
		return nil
	}}
	outcome := make(chan error, 1)
	go func() {
		_, err := service.TransitionFence(ctx, testFenceCommand("stale-regrant", admissionfence.Regrant, admissionfence.Team("team")))
		outcome <- err
	}()
	<-ready
	if _, err := service.TransitionFence(ctx, testFenceCommand("newer-revocation", admissionfence.Block, admissionfence.Team("team"))); err != nil {
		t.Fatal(err)
	}
	close(release)
	if err := <-outcome; !errors.Is(err, admissionfence.ErrChanged) {
		t.Fatalf("old authorization reopened newer revocation: %v", err)
	}
	if _, err := service.AdmitPublished(ctx, request); !errors.Is(err, admissionfence.ErrBlocked) {
		t.Fatalf("revocation no longer blocked: %v", err)
	}
}
