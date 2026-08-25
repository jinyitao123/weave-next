package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/jackc/pgx/v5"
	"github.com/jinyitao123/weave/internal/kernel/credentials"
	"github.com/jinyitao123/weave/internal/kernel/memory"
	"github.com/labstack/echo/v4"
)

// EmbedderConfig remains an API alias for compatibility.
type EmbedderConfig = credentials.EmbedderConfig

// CredentialEmbedderSource adapts the credentials store to
// memory.EmbedderSource (the memory package deliberately does not import
// credentials).
type CredentialEmbedderSource struct {
	Store *credentials.Store
}

// Get returns the decrypted workspace embedder settings; ok=false when the
// workspace has none configured.
func (s CredentialEmbedderSource) Get(ctx context.Context, workspaceID string) (memory.EmbedderSettings, bool, error) {
	cfg, err := s.Store.GetEmbedder(ctx, workspaceID)
	if errors.Is(err, pgx.ErrNoRows) {
		return memory.EmbedderSettings{}, false, nil
	}
	if err != nil {
		return memory.EmbedderSettings{}, false, err
	}
	return memory.EmbedderSettings{
		BaseURL:   cfg.BaseURL,
		APIKey:    cfg.APIKey,
		Model:     cfg.Model,
		Dimension: cfg.Dimension,
	}, true, nil
}

func (s *Server) handleGetEmbedder(c echo.Context) error {
	if s.Credentials == nil {
		return credentialStoreUnavailable(c)
	}
	cfg, err := s.Credentials.GetEmbedder(c.Request().Context(), getTenant(c))
	if errors.Is(err, pgx.ErrNoRows) {
		return c.JSON(http.StatusOK, EmbedderConfig{
			BaseURL:   s.Config.EmbedderURL,
			Model:     s.Config.EmbedderModel,
			Dimension: s.Config.EmbedderDimension,
		})
	}
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}
	cfg.APIKey = credentials.MaskedKey
	return c.JSON(http.StatusOK, cfg)
}

func (s *Server) handleUpdateEmbedder(c echo.Context) error {
	if s.Credentials == nil {
		return credentialStoreUnavailable(c)
	}
	var req EmbedderConfig
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid request body"})
	}
	if req.BaseURL == "" {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "url is required"})
	}
	if req.Model == "" {
		req.Model = "text-embedding-3-small"
	}
	if req.Dimension <= 0 {
		req.Dimension = 1536
	}

	ctx := c.Request().Context()
	tenant := getTenant(c)
	// The single loom_memory table pins one dimension for the whole
	// deployment; reject configurations that could never be materialized.
	// Without a PG pool (s.Embedders == nil) the config is persisted only,
	// matching the previous pool-nil behavior.
	if s.Embedders != nil {
		if dim, err := s.Embedders.TableDimension(ctx); err == nil && dim > 0 && dim != req.Dimension {
			return c.JSON(http.StatusBadRequest, map[string]string{
				"error": fmt.Sprintf("dimension %d does not match existing memory table dimension %d", req.Dimension, dim),
			})
		}
	}

	if err := s.Credentials.UpsertEmbedder(ctx, tenant, req); err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "failed to persist: " + err.Error()})
	}
	if s.Embedders != nil {
		s.Embedders.Invalidate(tenant)
		// Eager build keeps the previous "PUT materializes + migrates"
		// behavior and surfaces configuration errors immediately.
		if _, err := s.Embedders.ForWorkspace(ctx, tenant); err != nil {
			return c.JSON(http.StatusInternalServerError, map[string]string{"error": "failed to apply embedder: " + err.Error()})
		}
		slog.Info("embedding provider configured via API", "workspace", tenant, "model", req.Model, "dimension", req.Dimension)
	}
	return c.JSON(http.StatusOK, map[string]string{"status": "updated"})
}

func (s *Server) handleDeleteEmbedder(c echo.Context) error {
	if s.Credentials == nil {
		return credentialStoreUnavailable(c)
	}
	tenant := getTenant(c)
	if err := s.Credentials.DeleteEmbedder(c.Request().Context(), tenant); err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "failed to delete embedder: " + err.Error()})
	}
	// Only this workspace falls back (to the system embedder or none);
	// other workspaces keep their cached services.
	if s.Embedders != nil {
		s.Embedders.Invalidate(tenant)
	}
	slog.Info("embedding provider removed via API", "workspace", tenant)
	return c.NoContent(http.StatusNoContent)
}
