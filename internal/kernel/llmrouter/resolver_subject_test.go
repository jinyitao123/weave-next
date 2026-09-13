package llmrouter

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/jinyitao123/loom/contract"
	"github.com/jinyitao123/weave/internal/base/execution"
	"github.com/jinyitao123/weave/internal/base/frozen"
)

type subjectProviderSource struct {
	calls   atomic.Int64
	service bool
	block   <-chan struct{}
	started chan<- struct{}
}

func (s *subjectProviderSource) List(ctx context.Context, workspace string) ([]ProviderConfig, error) {
	s.calls.Add(1)
	subject, err := execution.RequireSubject(ctx, workspace)
	if err != nil {
		return nil, err
	}
	if s.started != nil {
		s.started <- struct{}{}
	}
	if s.block != nil {
		<-s.block
	}
	cfg := ProviderConfig{ID: "same-provider-id", Models: []string{"model"}, APIKey: subject.UserID, CredentialScope: frozen.CredentialScopeUser, CredentialUserID: subject.UserID}
	if s.service {
		cfg.APIKey = "shared"
		cfg.CredentialScope = frozen.CredentialScopeWorkspaceService
		cfg.CredentialUserID = ""
		cfg.CredentialServiceID = "shared-models"
	}
	return []ProviderConfig{cfg}, nil
}

type markerProvider struct {
	marker string
	calls  atomic.Int64
}

func (p *markerProvider) Chat(context.Context, contract.ChatRequest) (*contract.ChatResponse, error) {
	p.calls.Add(1)
	return &contract.ChatResponse{Content: p.marker}, nil
}
func (p *markerProvider) Stream(context.Context, contract.ChatRequest) (<-chan contract.StreamChunk, error) {
	p.calls.Add(1)
	ch := make(chan contract.StreamChunk)
	close(ch)
	return ch, nil
}

func TestResolverSeparatesUsersAndRefusesCrossSubjectSnapshot(t *testing.T) {
	prior := NewProviderClient
	var mu sync.Mutex
	clients := []*markerProvider{}
	NewProviderClient = func(cfg ProviderConfig) contract.LLM {
		p := &markerProvider{marker: cfg.APIKey}
		mu.Lock()
		clients = append(clients, p)
		mu.Unlock()
		return p
	}
	t.Cleanup(func() { NewProviderClient = prior })
	source := &subjectProviderSource{}
	resolver := NewResolver(New("model"))
	resolver.SetSource(source)
	alice := execution.WithSubject(context.Background(), execution.Subject{WorkspaceID: "workspace", UserID: "alice"})
	bob := execution.WithSubject(context.Background(), execution.Subject{WorkspaceID: "workspace", UserID: "bob"})
	a, err := resolver.ForWorkspace(alice, "workspace")
	if err != nil {
		t.Fatal(err)
	}
	b, err := resolver.ForWorkspace(bob, "workspace")
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		ctx  context.Context
		llm  contract.LLM
		want string
	}{{alice, a, "alice"}, {bob, b, "bob"}} {
		response, err := test.llm.Chat(test.ctx, contract.ChatRequest{Model: "model"})
		if err != nil || response.Content != test.want {
			t.Fatalf("route crossed user: %+v %v", response, err)
		}
	}
	if a == b || len(clients) != 2 {
		t.Fatal("provider session/client reused between users")
	}
	if _, err := a.Chat(bob, contract.ChatRequest{Model: "model"}); err == nil {
		t.Fatal("alice snapshot accepted bob")
	}
	if _, err := a.Stream(bob, contract.ChatRequest{Model: "model"}); err == nil {
		t.Fatal("alice stream accepted bob")
	}
	if _, err := resolver.ForWorkspace(context.Background(), "workspace"); err == nil {
		t.Fatal("missing subject fell back to workspace model")
	}
	again, err := resolver.ForWorkspace(alice, "workspace")
	if err != nil || again != a || source.calls.Load() != 2 {
		t.Fatal("personal cache not scoped or reusable")
	}
	resolver.Invalidate("workspace")
	if _, err := a.Chat(alice, contract.ChatRequest{Model: "model"}); err == nil {
		t.Fatal("held snapshot bypassed credential invalidation")
	}
	if _, err := resolver.ForWorkspace(alice, "workspace"); err != nil {
		t.Fatal(err)
	}
	if _, err := resolver.ForWorkspace(bob, "workspace"); err != nil {
		t.Fatal(err)
	}
	if source.calls.Load() != 4 {
		t.Fatal("workspace invalidation missed a user's cache")
	}
}

func TestServiceRoutesRequireFreshExplicitAuthorization(t *testing.T) {
	prior := NewProviderClient
	NewProviderClient = func(cfg ProviderConfig) contract.LLM { return &markerProvider{marker: cfg.APIKey} }
	t.Cleanup(func() { NewProviderClient = prior })
	source := &subjectProviderSource{service: true}
	resolver := NewResolver(New("model"))
	resolver.SetSource(source)
	alice := execution.WithSubject(context.Background(), execution.Subject{WorkspaceID: "workspace", UserID: "alice"})
	denied, err := resolver.ForWorkspace(alice, "workspace")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := denied.Chat(alice, contract.ChatRequest{Model: "model"}); err == nil {
		t.Fatal("implicit workspace service fallback")
	}
	var revoked atomic.Bool
	authorized := frozen.WithServiceReferenceAuthorization(alice, func(_ context.Context, s execution.Subject, ref frozen.CredentialReference) error {
		if !revoked.Load() && s.UserID == "alice" && ref.ServiceID == "shared-models" && ref.ResourceID == "same-provider-id" {
			return nil
		}
		return errors.New("denied")
	})
	first, err := resolver.ForWorkspace(authorized, "workspace")
	if err != nil {
		t.Fatal(err)
	}
	second, err := resolver.ForWorkspace(authorized, "workspace")
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatal("unversioned service grant cached")
	}
	if result, err := first.Chat(authorized, contract.ChatRequest{Model: "model"}); err != nil || result.Content != "shared" {
		t.Fatalf("explicit shared route failed: %+v %v", result, err)
	}
	revoked.Store(true)
	if _, err := first.Chat(authorized, contract.ChatRequest{Model: "model"}); err == nil {
		t.Fatal("previous service snapshot bypassed revocation")
	}
	if _, err := first.Chat(alice, contract.ChatRequest{Model: "model"}); err == nil {
		t.Fatal("snapshot carried an earlier context's grant")
	}
}

func TestResolverDoesNotReturnCredentialSnapshotInvalidatedDuringBuild(t *testing.T) {
	prior := NewProviderClient
	NewProviderClient = func(cfg ProviderConfig) contract.LLM { return &markerProvider{marker: cfg.APIKey} }
	t.Cleanup(func() { NewProviderClient = prior })
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	source := &subjectProviderSource{started: started, block: release}
	resolver := NewResolver(New("model"))
	resolver.SetSource(source)
	ctx := execution.WithSubject(context.Background(), execution.Subject{WorkspaceID: "workspace", UserID: "alice"})
	done := make(chan error, 1)
	go func() { _, err := resolver.ForWorkspace(ctx, "workspace"); done <- err }()
	<-started
	resolver.Invalidate("workspace")
	close(release)
	if err := <-done; err == nil {
		t.Fatal("stale decrypted credential escaped after invalidation")
	}
}
