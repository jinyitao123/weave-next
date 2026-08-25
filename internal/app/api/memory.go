package api

import (
	"context"
	"net/http"

	"github.com/jinyitao123/weave/internal/kernel/compiler"
	"github.com/jinyitao123/weave/internal/kernel/memory"
	"github.com/jinyitao123/weave/internal/kernel/registry"
	"github.com/labstack/echo/v4"
)

const memoryDisabledMessage = "记忆未启用 · 嵌入服务未配置"

func configureProjectMemory(
	opts *compiler.CompileOpts,
	workspaceID string,
	avatar *registry.AgentRecord,
	projectID string,
) {
	if opts == nil || opts.MemoryService == nil || avatar == nil ||
		workspaceID == "" || avatar.ID == "" || projectID == "" {
		return
	}
	namespace := memory.ProjectNamespace(workspaceID, avatar.ID, projectID)
	opts.MemoryReadSources = append(opts.MemoryReadSources, memory.RetrieveSource{
		Namespace: namespace,
		Label:     "当前 Project 记忆",
		Source:    "project",
	})
	if opts.AutoRemember {
		opts.MemoryWriteNamespace = namespace
		opts.MemoryWriteSource = "project_auto"
	}
}

func (s *Server) projectMemoryIDForRun(
	ctx context.Context,
	workspaceID, runID string,
	execution *teamSessionExecution,
) (string, error) {
	if execution != nil {
		return execution.Snapshot.ProjectID, nil
	}
	projectID, _, err := s.runProjectAttribution(ctx, workspaceID, runID)
	return projectID, err
}

func disabledMemoryResponse(c echo.Context) error {
	return c.JSON(http.StatusOK, struct {
		Enabled  bool  `json:"enabled"`
		Memories []any `json:"memories"`
	}{
		Enabled:  false,
		Memories: []any{},
	})
}

// resolveMemoryNamespace looks up the agent's memory scope and builds the
// appropriate namespace. Falls back to tenant-level if agent is not found.
func (s *Server) resolveMemoryNamespace(c echo.Context, tenant, agentName string) string {
	scope := ""
	if rec, err := s.Registry.Get(c.Request().Context(), tenant, agentName); err == nil && rec.MemoryConfig != nil {
		scope = rec.MemoryConfig.Scope
	}
	return memory.ScopedNamespace(tenant, getUserID(c), agentName, "", scope)
}

// MemoryCreateRequest is the input to create a memory.
type MemoryCreateRequest struct {
	Content  string         `json:"content"`
	Metadata map[string]any `json:"metadata,omitempty"`
}

// MemorySearchRequest is the input to search memories.
type MemorySearchRequest struct {
	Query string `json:"query"`
	TopK  int    `json:"top_k"`
}

func (s *Server) handleListMemories(c echo.Context) error {
	tenant := getTenant(c)
	memSvc, err := s.memoryForStrict(c.Request().Context(), tenant)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}
	if memSvc == nil {
		return disabledMemoryResponse(c)
	}

	agentName := c.Param("name")
	ns := s.resolveMemoryNamespace(c, tenant, agentName)

	records, err := memSvc.ListAll(c.Request().Context(), ns, 100, 0)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}

	// Return empty array instead of null.
	type memoryResponse struct {
		ID          string         `json:"id"`
		Content     string         `json:"content"`
		Metadata    map[string]any `json:"metadata,omitempty"`
		CreatedAt   string         `json:"created_at"`
		AccessedAt  string         `json:"accessed_at"`
		AccessCount int            `json:"access_count"`
	}
	out := make([]memoryResponse, 0, len(records))
	for _, r := range records {
		out = append(out, memoryResponse{
			ID:          r.ID,
			Content:     r.Content,
			Metadata:    r.Metadata,
			CreatedAt:   r.CreatedAt.Format("2006-01-02T15:04:05Z"),
			AccessedAt:  r.AccessedAt.Format("2006-01-02T15:04:05Z"),
			AccessCount: r.AccessCount,
		})
	}
	return c.JSON(http.StatusOK, out)
}

func (s *Server) handleCreateMemory(c echo.Context) error {
	tenant := getTenant(c)
	memSvc, err := s.memoryForStrict(c.Request().Context(), tenant)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}
	if memSvc == nil {
		return c.JSON(http.StatusNotImplemented, map[string]string{"error": memoryDisabledMessage})
	}

	var req MemoryCreateRequest
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid request body"})
	}
	if req.Content == "" {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "content is required"})
	}

	agentName := c.Param("name")
	ns := s.resolveMemoryNamespace(c, tenant, agentName)

	id, err := memSvc.Remember(c.Request().Context(), ns, req.Content, req.Metadata)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}

	return c.JSON(http.StatusCreated, map[string]string{"id": id})
}

func (s *Server) handleDeleteMemory(c echo.Context) error {
	tenant := getTenant(c)
	memSvc, err := s.memoryForStrict(c.Request().Context(), tenant)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}
	if memSvc == nil {
		return c.JSON(http.StatusNotImplemented, map[string]string{"error": memoryDisabledMessage})
	}

	agentName := c.Param("name")
	memoryID := c.Param("id")
	ns := s.resolveMemoryNamespace(c, tenant, agentName)

	if err := memSvc.ForgetOne(c.Request().Context(), ns, memoryID); err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}
	return c.NoContent(http.StatusNoContent)
}

func (s *Server) handleSearchMemories(c echo.Context) error {
	tenant := getTenant(c)
	memSvc, err := s.memoryForStrict(c.Request().Context(), tenant)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}
	if memSvc == nil {
		return disabledMemoryResponse(c)
	}

	var req MemorySearchRequest
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid request body"})
	}
	if req.Query == "" {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "query is required"})
	}
	if req.TopK <= 0 {
		req.TopK = 5
	}

	agentName := c.Param("name")
	ns := s.resolveMemoryNamespace(c, tenant, agentName)

	records, err := memSvc.Recall(c.Request().Context(), ns, req.Query, req.TopK)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}

	type searchResult struct {
		ID          string         `json:"id"`
		Content     string         `json:"content"`
		Score       float64        `json:"score"`
		Metadata    map[string]any `json:"metadata,omitempty"`
		CreatedAt   string         `json:"created_at"`
		AccessedAt  string         `json:"accessed_at"`
		AccessCount int            `json:"access_count"`
	}
	out := make([]searchResult, 0, len(records))
	for _, r := range records {
		out = append(out, searchResult{
			ID:          r.ID,
			Content:     r.Content,
			Score:       r.Score,
			Metadata:    r.Metadata,
			CreatedAt:   r.CreatedAt.Format("2006-01-02T15:04:05Z"),
			AccessedAt:  r.AccessedAt.Format("2006-01-02T15:04:05Z"),
			AccessCount: r.AccessCount,
		})
	}
	return c.JSON(http.StatusOK, out)
}
