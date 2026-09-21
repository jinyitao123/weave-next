package api

import (
	"net/http"

	"github.com/labstack/echo/v4"
)

type developmentModelCatalogResponse struct {
	Version string   `json:"version"`
	Models  []string `json:"models"`
}

// handleDevelopmentModelCatalog exposes only model names that have a current,
// enabled workspace provider revision. Credentials and provider internals stay
// behind the platform administration boundary.
func (s *Server) handleDevelopmentModelCatalog(c echo.Context) error {
	pool := s.GetPool()
	if pool == nil {
		return workflowError(c, http.StatusServiceUnavailable, "development_model_catalog_unavailable", "development model catalog unavailable")
	}
	rows, err := pool.Query(c.Request().Context(), `
		SELECT DISTINCT model
		FROM weave_provider_credentials,
		     unnest(models) AS model
		WHERE workspace_id=$1
		  AND credential_scope='workspace_service'
		  AND enabled
		  AND revoked_at IS NULL
		  AND deleted_at IS NULL
		  AND latest_revision > 0
		ORDER BY model
	`, getTenant(c))
	if err != nil {
		return workflowStoreFailure(c, err)
	}
	defer rows.Close()
	models := []string{}
	for rows.Next() {
		var model string
		if err := rows.Scan(&model); err != nil {
			return workflowStoreFailure(c, err)
		}
		models = append(models, model)
	}
	if err := rows.Err(); err != nil {
		return workflowStoreFailure(c, err)
	}
	return c.JSON(http.StatusOK, developmentModelCatalogResponse{Version: "1", Models: models})
}
