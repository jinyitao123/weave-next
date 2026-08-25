package api

import (
	"errors"
	"net/http"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jinyitao123/weave/internal/kernel/registry"
	"github.com/labstack/echo/v4"
)

type agentLinkResponse struct {
	ID            string `json:"id"`
	SourceAgentID string `json:"source_agent_id"`
	TargetAgentID string `json:"target_agent_id"`
	Type          string `json:"type"`
	Instruction   string `json:"instruction"`
}

type createAgentLinkRequest struct {
	SourceAgentID string `json:"source_agent_id"`
	TargetAgentID string `json:"target_agent_id"`
	Type          string `json:"type"`
	Instruction   string `json:"instruction"`
}

type updateAgentLinkRequest struct {
	Instruction *string `json:"instruction"`
}

func (s *Server) handleListAgentLinks(c echo.Context) error {
	links, err := s.Registry.ListLinks(c.Request().Context(), getTenant(c))
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}
	responses := make([]agentLinkResponse, 0, len(links))
	for _, link := range links {
		responses = append(responses, agentLinkToResponse(link))
	}
	return c.JSON(http.StatusOK, responses)
}

func (s *Server) handleCreateAgentLink(c echo.Context) error {
	var req createAgentLinkRequest
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid request body"})
	}
	if req.SourceAgentID == "" || req.TargetAgentID == "" {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "source_agent_id and target_agent_id are required"})
	}
	if req.SourceAgentID == req.TargetAgentID {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "cannot link an agent to itself"})
	}
	if req.Type == "" {
		req.Type = "peer"
	}
	if req.Type != "peer" && req.Type != "manages" {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "type must be peer or manages"})
	}

	link, err := s.Registry.CreateLink(
		c.Request().Context(), getTenant(c), req.SourceAgentID, req.TargetAgentID, req.Type, req.Instruction,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return c.JSON(http.StatusNotFound, map[string]string{"error": "source or target agent not found in workspace"})
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return c.JSON(http.StatusConflict, map[string]string{"error": "link already exists between these agents"})
	}
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}
	return c.JSON(http.StatusCreated, agentLinkToResponse(*link))
}

func (s *Server) handleUpdateAgentLink(c echo.Context) error {
	var req updateAgentLinkRequest
	if err := c.Bind(&req); err != nil || req.Instruction == nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "instruction is required"})
	}
	link, err := s.Registry.UpdateLinkInstruction(c.Request().Context(), getTenant(c), c.Param("id"), *req.Instruction)
	if errors.Is(err, pgx.ErrNoRows) {
		return c.JSON(http.StatusNotFound, map[string]string{"error": "agent link not found"})
	}
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}
	return c.JSON(http.StatusOK, agentLinkToResponse(*link))
}

func (s *Server) handleDeleteAgentLink(c echo.Context) error {
	deleted, err := s.Registry.DeleteLink(c.Request().Context(), getTenant(c), c.Param("id"))
	if errors.Is(err, pgx.ErrNoRows) {
		return c.JSON(http.StatusNotFound, map[string]string{"error": "agent link not found"})
	}
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}
	return c.JSON(http.StatusOK, agentLinkToResponse(deleted.Link))
}

func (s *Server) handleCleanupOrphanWorkers(c echo.Context) error {
	orphans, err := s.Registry.ListOrphanWorkers(c.Request().Context(), getTenant(c))
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}
	if orphans == nil {
		orphans = []registry.OrphanWorker{}
	}
	return c.JSON(http.StatusOK, map[string]any{"orphans": orphans})
}

func agentLinkToResponse(link registry.Link) agentLinkResponse {
	return agentLinkResponse{
		ID:            link.ID,
		SourceAgentID: link.FromAgentID,
		TargetAgentID: link.ToAgentID,
		Type:          link.Type,
		Instruction:   link.Instruction,
	}
}
