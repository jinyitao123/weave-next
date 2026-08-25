package api

import (
	"errors"
	"net/http"
	"strings"

	"github.com/jinyitao123/weave/internal/app/projects"
	"github.com/labstack/echo/v4"
)

type createProjectRequest struct {
	TeamID      string `json:"team_id"`
	AvatarID    string `json:"avatar_id"`
	Name        string `json:"name"`
	Description string `json:"description"`
}

type updateProjectRequest struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

type moveProjectRequest struct {
	TeamID   string `json:"team_id"`
	AvatarID string `json:"avatar_id"`
}

type ensureUnclassifiedProjectRequest struct {
	TeamID   string `json:"team_id"`
	AvatarID string `json:"avatar_id"`
}

type projectCollaboratorRequest struct {
	TeamID string `json:"team_id"`
}

func (s *Server) handleListProjects(c echo.Context) error {
	if s.Projects == nil {
		return projectUnavailable(c)
	}
	includeArchived, ok := parseProjectBool(c.QueryParam("include_archived"))
	if !ok {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid_include_archived"})
	}
	items, err := s.Projects.List(c.Request().Context(), getTenant(c), projects.ListFilter{
		AvatarID:        c.QueryParam("avatar_id"),
		TeamID:          c.QueryParam("team_id"),
		IncludeArchived: includeArchived,
	})
	if err != nil {
		return projectFailure(c, err)
	}
	return c.JSON(http.StatusOK, map[string]any{"projects": items})
}

func (s *Server) handleCreateProject(c echo.Context) error {
	if s.Projects == nil {
		return projectUnavailable(c)
	}
	var request createProjectRequest
	if err := c.Bind(&request); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid_request"})
	}
	var project projects.Project
	var err error
	if strings.TrimSpace(request.TeamID) != "" {
		project, err = s.Projects.CreateForTeam(
			c.Request().Context(), getTenant(c), request.TeamID, request.Name, request.Description,
		)
	} else {
		project, err = s.Projects.Create(
			c.Request().Context(), getTenant(c), request.AvatarID, request.Name, request.Description,
		)
	}
	if err != nil {
		return projectFailure(c, err)
	}
	return c.JSON(http.StatusCreated, project)
}

func (s *Server) handleEnsureUnclassifiedProject(c echo.Context) error {
	if s.Projects == nil {
		return projectUnavailable(c)
	}
	var request ensureUnclassifiedProjectRequest
	if err := c.Bind(&request); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid_request"})
	}
	var project projects.Project
	var err error
	if strings.TrimSpace(request.TeamID) != "" {
		project, err = s.Projects.EnsureUnclassifiedForTeam(
			c.Request().Context(), getTenant(c), request.TeamID,
		)
	} else {
		project, err = s.Projects.EnsureUnclassified(
			c.Request().Context(), getTenant(c), request.AvatarID,
		)
	}
	if err != nil {
		return projectFailure(c, err)
	}
	return c.JSON(http.StatusOK, project)
}

func (s *Server) handleGetProject(c echo.Context) error {
	if s.Projects == nil {
		return projectUnavailable(c)
	}
	project, err := s.Projects.Get(c.Request().Context(), getTenant(c), c.Param("id"))
	if err != nil {
		return projectFailure(c, err)
	}
	return c.JSON(http.StatusOK, project)
}

func (s *Server) handleUpdateProject(c echo.Context) error {
	if s.Projects == nil {
		return projectUnavailable(c)
	}
	var request updateProjectRequest
	if err := c.Bind(&request); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid_request"})
	}
	project, err := s.Projects.Update(
		c.Request().Context(), getTenant(c), c.Param("id"), request.Name, request.Description,
	)
	if err != nil {
		return projectFailure(c, err)
	}
	return c.JSON(http.StatusOK, project)
}

func (s *Server) handleArchiveProject(c echo.Context) error {
	if s.Projects == nil {
		return projectUnavailable(c)
	}
	if _, err := s.Projects.Archive(c.Request().Context(), getTenant(c), c.Param("id")); err != nil {
		return projectFailure(c, err)
	}
	return c.NoContent(http.StatusNoContent)
}

func (s *Server) handleRestoreProject(c echo.Context) error {
	if s.Projects == nil {
		return projectUnavailable(c)
	}
	project, err := s.Projects.Restore(c.Request().Context(), getTenant(c), c.Param("id"))
	if err != nil {
		return projectFailure(c, err)
	}
	return c.JSON(http.StatusOK, project)
}

func (s *Server) handleMoveProject(c echo.Context) error {
	if s.Projects == nil {
		return projectUnavailable(c)
	}
	var request moveProjectRequest
	if err := c.Bind(&request); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid_request"})
	}
	var project projects.Project
	var err error
	if strings.TrimSpace(request.TeamID) != "" {
		project, err = s.Projects.MoveForTeam(
			c.Request().Context(), getTenant(c), c.Param("id"), request.TeamID, getUserID(c),
		)
	} else {
		project, err = s.Projects.Move(
			c.Request().Context(), getTenant(c), c.Param("id"), request.AvatarID, getUserID(c),
		)
	}
	if err != nil {
		return projectFailure(c, err)
	}
	return c.JSON(http.StatusOK, project)
}

func (s *Server) handleListProjectCollaborators(c echo.Context) error {
	if s.Projects == nil {
		return projectUnavailable(c)
	}
	items, err := s.Projects.ListCollaborators(c.Request().Context(), getTenant(c), c.Param("id"))
	if err != nil {
		return projectFailure(c, err)
	}
	return c.JSON(http.StatusOK, map[string]any{"collaborators": items})
}

func (s *Server) handleAddProjectCollaborator(c echo.Context) error {
	if s.Projects == nil {
		return projectUnavailable(c)
	}
	var request projectCollaboratorRequest
	if err := c.Bind(&request); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid_request"})
	}
	item, err := s.Projects.AddCollaborator(
		c.Request().Context(), getTenant(c), c.Param("id"), request.TeamID, getUserID(c),
	)
	if err != nil {
		return projectFailure(c, err)
	}
	return c.JSON(http.StatusCreated, item)
}

func (s *Server) handleRemoveProjectCollaborator(c echo.Context) error {
	if s.Projects == nil {
		return projectUnavailable(c)
	}
	if err := s.Projects.RemoveCollaborator(
		c.Request().Context(), getTenant(c), c.Param("id"), c.Param("teamId"),
	); err != nil {
		return projectFailure(c, err)
	}
	return c.NoContent(http.StatusNoContent)
}

func parseProjectBool(raw string) (bool, bool) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", "0", "false":
		return false, true
	case "1", "true":
		return true, true
	default:
		return false, false
	}
}

func projectUnavailable(c echo.Context) error {
	return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": "projects_unavailable"})
}

func projectFailure(c echo.Context, err error) error {
	switch {
	case errors.Is(err, projects.ErrInvalidName):
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "project_name_required"})
	case errors.Is(err, projects.ErrNotFound):
		return c.JSON(http.StatusNotFound, map[string]string{"error": "project_not_found"})
	case errors.Is(err, projects.ErrInvalidAvatar):
		return c.JSON(http.StatusUnprocessableEntity, map[string]string{"error": "invalid_project_avatar"})
	case errors.Is(err, projects.ErrInvalidTeam):
		return c.JSON(http.StatusUnprocessableEntity, map[string]string{"error": "invalid_project_team"})
	case errors.Is(err, projects.ErrNameConflict):
		return c.JSON(http.StatusConflict, map[string]string{"error": "project_name_conflict"})
	case errors.Is(err, projects.ErrCollaboratorConflict):
		return c.JSON(http.StatusConflict, map[string]string{"error": "project_collaborator_conflict"})
	default:
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "project_operation_failed"})
	}
}
