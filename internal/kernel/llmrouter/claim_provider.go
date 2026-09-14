package llmrouter

import (
	"context"
	"errors"

	"github.com/jinyitao123/weave/internal/base/execution"
	"github.com/jinyitao123/weave/internal/base/frozen"
)

// ResolveProviderConfiguration selects the same authorized provider as Chat.
// It is an in-process Host injection port, never a browser or wire response.
// Callers resolve again for every physical claim; secrets must not be cached
// by workspace, persisted in task payloads, or inherited from ambient env.
func (r *Resolver) ResolveProviderConfiguration(ctx context.Context, workspaceID, model string) (ProviderConfig, error) {
	subject, err := execution.RequireSubject(ctx, workspaceID)
	if err != nil {
		return ProviderConfig{}, err
	}
	r.mu.RLock()
	source := r.source
	r.mu.RUnlock()
	snapshot, _, err := r.build(ctx, subject, source)
	if err != nil {
		return ProviderConfig{}, err
	}
	snapshot.router.mu.RLock()
	defer snapshot.router.mu.RUnlock()
	if model == "" {
		model = snapshot.router.fallback
	}
	provider := snapshot.router.providers[snapshot.router.models[model]]
	if provider == nil || provider.APIKey == "" || provider.BaseURL == "" {
		return ProviderConfig{}, errors.New("model provider credentials are unavailable")
	}
	if err := frozen.AuthorizeCredentialReference(ctx, providerReference(workspaceID, *provider)); err != nil {
		return ProviderConfig{}, err
	}
	copy := *provider
	copy.Models = append([]string(nil), provider.Models...)
	return copy, nil
}
