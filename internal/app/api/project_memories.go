package api

import (
	"errors"
	"net/http"
	"strings"

	"github.com/jinyitao123/weave/internal/kernel/memory"
	"github.com/jinyitao123/weave/internal/app/projects"
	"github.com/labstack/echo/v4"
)

type projectMemoryResponse struct {
	ID          string         `json:"id"`
	Content     string         `json:"content"`
	Score       float64        `json:"score,omitempty"`
	Metadata    map[string]any `json:"metadata,omitempty"`
	CreatedAt   string         `json:"created_at"`
	AccessedAt  string         `json:"accessed_at"`
	AccessCount int            `json:"access_count"`
}

func (s *Server) projectMemoryNamespace(c echo.Context) (string, error) {
	if s.Projects == nil {
		return "", errors.New("project store is unavailable")
	}
	project, err := s.Projects.Get(c.Request().Context(), getTenant(c), c.Param("id"))
	if err != nil {
		return "", err
	}
	return memory.ProjectNamespace(project.WorkspaceID, project.AvatarID, project.ID), nil
}

func projectMemoryRecords(records []memory.Record) []projectMemoryResponse {
	result := make([]projectMemoryResponse, 0, len(records))
	for _, record := range records {
		result = append(result, projectMemoryResponse{
			ID: record.ID, Content: record.Content, Score: record.Score,
			Metadata:    record.Metadata,
			CreatedAt:   record.CreatedAt.UTC().Format("2006-01-02T15:04:05Z"),
			AccessedAt:  record.AccessedAt.UTC().Format("2006-01-02T15:04:05Z"),
			AccessCount: record.AccessCount,
		})
	}
	return result
}

func (s *Server) handleListProjectMemories(c echo.Context) error {
	memSvc, err := s.memoryForStrict(c.Request().Context(), getTenant(c))
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "project_memory_unavailable"})
	}
	if memSvc == nil {
		return disabledMemoryResponse(c)
	}
	namespace, err := s.projectMemoryNamespace(c)
	if err != nil {
		return respondProjectMemoryError(c, err)
	}
	records, err := memSvc.ListAll(c.Request().Context(), namespace, 100, 0)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "project_memory_list_failed"})
	}
	return c.JSON(http.StatusOK, projectMemoryRecords(records))
}

func (s *Server) handleCreateProjectMemory(c echo.Context) error {
	memSvc, err := s.memoryForStrict(c.Request().Context(), getTenant(c))
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "project_memory_unavailable"})
	}
	if memSvc == nil {
		return c.JSON(http.StatusNotImplemented, map[string]string{"error": memoryDisabledMessage})
	}
	var request MemoryCreateRequest
	if err := c.Bind(&request); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid_request"})
	}
	request.Content = strings.TrimSpace(request.Content)
	if request.Content == "" {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "content_required"})
	}
	namespace, err := s.projectMemoryNamespace(c)
	if err != nil {
		return respondProjectMemoryError(c, err)
	}
	metadata := make(map[string]any, len(request.Metadata)+1)
	for key, value := range request.Metadata {
		metadata[key] = value
	}
	metadata["source"] = "project_manual"
	id, err := memSvc.Remember(c.Request().Context(), namespace, request.Content, metadata)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "project_memory_create_failed"})
	}
	return c.JSON(http.StatusCreated, map[string]string{"id": id})
}

func (s *Server) handleDeleteProjectMemory(c echo.Context) error {
	memSvc, err := s.memoryForStrict(c.Request().Context(), getTenant(c))
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "project_memory_unavailable"})
	}
	if memSvc == nil {
		return c.JSON(http.StatusNotImplemented, map[string]string{"error": memoryDisabledMessage})
	}
	namespace, err := s.projectMemoryNamespace(c)
	if err != nil {
		return respondProjectMemoryError(c, err)
	}
	if err := memSvc.ForgetOne(c.Request().Context(), namespace, c.Param("memoryID")); err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "project_memory_delete_failed"})
	}
	return c.NoContent(http.StatusNoContent)
}

func (s *Server) handleSearchProjectMemories(c echo.Context) error {
	memSvc, err := s.memoryForStrict(c.Request().Context(), getTenant(c))
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "project_memory_unavailable"})
	}
	if memSvc == nil {
		return disabledMemoryResponse(c)
	}
	var request MemorySearchRequest
	if err := c.Bind(&request); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid_request"})
	}
	request.Query = strings.TrimSpace(request.Query)
	if request.Query == "" {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "query_required"})
	}
	if request.TopK <= 0 {
		request.TopK = 5
	}
	namespace, err := s.projectMemoryNamespace(c)
	if err != nil {
		return respondProjectMemoryError(c, err)
	}
	records, err := memSvc.Recall(c.Request().Context(), namespace, request.Query, request.TopK)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "project_memory_search_failed"})
	}
	return c.JSON(http.StatusOK, projectMemoryRecords(records))
}

func respondProjectMemoryError(c echo.Context, err error) error {
	if errors.Is(err, projects.ErrNotFound) {
		return c.JSON(http.StatusNotFound, map[string]string{"error": "project_not_found"})
	}
	return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": "project_memory_unavailable"})
}
