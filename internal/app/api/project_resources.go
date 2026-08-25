package api

import (
	"errors"
	"net/http"

	"github.com/jinyitao123/weave/internal/app/projects"
	"github.com/labstack/echo/v4"
)

func (s *Server) handleListProjectResources(c echo.Context) error {
	if s.Projects == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": "project_store_unavailable"})
	}
	resources, err := s.Projects.ListResources(
		c.Request().Context(), getTenant(c), c.Param("id"),
	)
	if err != nil {
		return respondProjectResourceError(c, err)
	}
	return c.JSON(http.StatusOK, map[string]any{"resources": resources})
}

func (s *Server) handleCreateProjectResource(c echo.Context) error {
	if s.Projects == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": "project_store_unavailable"})
	}
	var input projects.CreateResourceInput
	if err := c.Bind(&input); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid_request"})
	}
	resource, err := s.Projects.CreateResource(
		c.Request().Context(), getTenant(c), c.Param("id"), input,
	)
	if err != nil {
		return respondProjectResourceError(c, err)
	}
	return c.JSON(http.StatusCreated, resource)
}

func (s *Server) handleDeleteProjectResource(c echo.Context) error {
	if s.Projects == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": "project_store_unavailable"})
	}
	err := s.Projects.DeleteResource(
		c.Request().Context(), getTenant(c), c.Param("id"), c.Param("resourceID"),
	)
	if err != nil {
		return respondProjectResourceError(c, err)
	}
	return c.NoContent(http.StatusNoContent)
}

func respondProjectResourceError(c echo.Context, err error) error {
	switch {
	case errors.Is(err, projects.ErrNotFound), errors.Is(err, projects.ErrResourceNotFound):
		return c.JSON(http.StatusNotFound, map[string]string{"error": "project_resource_not_found"})
	case errors.Is(err, projects.ErrArchived):
		return c.JSON(http.StatusConflict, map[string]string{"error": "project_archived"})
	case errors.Is(err, projects.ErrResourceConflict):
		return c.JSON(http.StatusConflict, map[string]string{"error": "project_resource_conflict"})
	case errors.Is(err, projects.ErrInvalidResource):
		return c.JSON(http.StatusUnprocessableEntity, map[string]string{"error": "invalid_project_resource"})
	default:
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "project_resource_operation_failed"})
	}
}
