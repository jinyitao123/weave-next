package api

import (
	"context"
	"errors"
	"net/http"

	"github.com/labstack/echo/v4"
)

type developmentModelCatalogResponse struct {
	Version string   `json:"version"`
	Models  []string `json:"models"`
	// Entries adds, per model, the shared provider that serves it and that
	// provider's protocol ("openai" or "systemone"). Models stays the plain
	// name list it always was.
	Entries []developmentModelCatalogEntry `json:"entries"`
}

type developmentModelCatalogEntry struct {
	Model      string `json:"model"`
	ProviderID string `json:"provider_id"`
	Protocol   string `json:"protocol"`
}

// developmentModels lists the model names a workspace may bind: those with a
// current, enabled workspace provider revision. Credentials and provider
// internals stay behind the platform administration boundary.
func (s *Server) developmentModels(ctx context.Context, workspace string) ([]string, error) {
	entries, err := s.developmentModelEntries(ctx, workspace)
	if err != nil {
		return nil, err
	}
	return catalogModelNames(entries), nil
}

// developmentModelEntries is the catalog with its serving providers, ordered
// by model then provider.
func (s *Server) developmentModelEntries(ctx context.Context, workspace string) ([]developmentModelCatalogEntry, error) {
	pool := s.GetPool()
	if pool == nil {
		return nil, errors.New("development model catalog unavailable")
	}
	rows, err := pool.Query(ctx, `
		SELECT DISTINCT model, id, protocol
		FROM weave_provider_credentials,
		     unnest(models) AS model
		WHERE workspace_id=$1
		  AND credential_scope='workspace_service'
		  AND enabled
		  AND revoked_at IS NULL
		  AND deleted_at IS NULL
		  AND latest_revision > 0
		ORDER BY model, id
	`, workspace)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	entries := []developmentModelCatalogEntry{}
	for rows.Next() {
		var entry developmentModelCatalogEntry
		if err := rows.Scan(&entry.Model, &entry.ProviderID, &entry.Protocol); err != nil {
			return nil, err
		}
		entries = append(entries, entry)
	}
	return entries, rows.Err()
}

func catalogModelNames(entries []developmentModelCatalogEntry) []string {
	models := []string{}
	for _, entry := range entries {
		if len(models) == 0 || models[len(models)-1] != entry.Model {
			models = append(models, entry.Model)
		}
	}
	return models
}

func (s *Server) handleDevelopmentModelCatalog(c echo.Context) error {
	if s.GetPool() == nil {
		return workflowError(c, http.StatusServiceUnavailable, "development_model_catalog_unavailable", "development model catalog unavailable")
	}
	entries, err := s.developmentModelEntries(c.Request().Context(), getTenant(c))
	if err != nil {
		return workflowStoreFailure(c, err)
	}
	return c.JSON(http.StatusOK, developmentModelCatalogResponse{Version: "1", Models: catalogModelNames(entries), Entries: entries})
}
