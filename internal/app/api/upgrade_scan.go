package api

import (
	"context"
	"errors"
	"log/slog"

	"github.com/jackc/pgx/v5"
	"github.com/jinyitao123/weave/internal/kernel/engine"
)

// WarnLegacyCrossWorkspaceConfig names, once at startup, the configuration a
// pre-workspace-scoping deployment must migrate. Before workspace scoping,
// API-persisted providers registered globally and the "default" workspace's
// embedder served every tenant; both are now workspace-scoped. This scan is
// diagnostics only — it mutates nothing and every failure is soft:
//
//   - an agent whose model / fallback models resolve in neither its own
//     workspace nor the env (system) namespace gets a warning naming the
//     agent, workspace and model;
//   - a workspace that used to inherit the "default" workspace's embedder and
//     now resolves to no memory service gets a warning with the migration
//     options.
func (s *Server) WarnLegacyCrossWorkspaceConfig(ctx context.Context) {
	if s.Registry == nil {
		return
	}
	workspaces, err := s.Registry.ListWorkspaces(ctx)
	if err != nil {
		slog.Warn("legacy cross-workspace config scan skipped", "error", err)
		return
	}

	defaultHasEmbedder := false
	if s.Credentials != nil {
		if _, err := s.Credentials.GetEmbedder(ctx, "default"); err == nil {
			defaultHasEmbedder = true
		}
	}
	systemEmbedder := s.Config != nil && s.Config.EmbedderURL != ""

	for _, ws := range workspaces {
		s.warnUnresolvableModels(ctx, ws)
		s.warnOrphanedMemory(ctx, ws, defaultHasEmbedder, systemEmbedder)
	}
}

func (s *Server) warnUnresolvableModels(ctx context.Context, ws string) {
	if s.Models == nil {
		return
	}
	agents, err := s.Registry.List(ctx, ws)
	if err != nil {
		slog.Warn("legacy config scan: cannot list agents", "workspace", ws, "error", err)
		return
	}
	for i := range agents {
		rec := &agents[i]
		if engine.IsCLIEngine(rec.Engine) {
			continue // CLI engines do not resolve models through llmrouter
		}
		models := make([]string, 0, len(rec.FallbackModels)+1)
		if rec.Model != "" {
			models = append(models, rec.Model)
		}
		models = append(models, rec.FallbackModels...)
		for _, model := range models {
			if model == "" {
				continue
			}
			ok, err := s.Models.CanResolve(ctx, ws, model)
			if err != nil {
				slog.Warn("legacy config scan: cannot resolve workspace providers",
					"workspace", ws, "error", err)
				return
			}
			if !ok {
				slog.Warn("agent references a model with no provider in its workspace — "+
					"providers are now workspace-scoped; configure the provider in this "+
					"workspace (POST /v1/providers) or via env keys",
					"workspace", ws, "agent", rec.Name, "model", model)
			}
		}
	}
}

func (s *Server) warnOrphanedMemory(ctx context.Context, ws string, defaultHasEmbedder, systemEmbedder bool) {
	if ws == "default" || !defaultHasEmbedder || systemEmbedder || s.Credentials == nil {
		return
	}
	_, err := s.Credentials.GetEmbedder(ctx, ws)
	if errors.Is(err, pgx.ErrNoRows) {
		slog.Warn("workspace has no embedder of its own — memory no longer inherits the "+
			"\"default\" workspace's embedder; copy the config into this workspace "+
			"(PUT /v1/embedder) or set EMBEDDER_URL as the system fallback",
			"workspace", ws)
	}
}
