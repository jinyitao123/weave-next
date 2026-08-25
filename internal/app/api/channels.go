package api

import (
	"errors"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jinyitao123/weave/internal/kernel/registry"
	"github.com/labstack/echo/v4"
)

type channelNameRequest struct {
	Name string `json:"name"`
}

type reorderChannelsRequest struct {
	OrderedIDs []string `json:"ordered_ids"`
}

func (s *Server) handleListChannels(c echo.Context) error {
	channels, err := s.Registry.ListChannels(c.Request().Context(), getTenant(c), c.Param("name"))
	if errors.Is(err, pgx.ErrNoRows) {
		return c.JSON(http.StatusNotFound, map[string]string{"error": "agent not found"})
	}
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}
	return c.JSON(http.StatusOK, channels)
}

func (s *Server) handleCreateChannel(c echo.Context) error {
	name, ok := bindChannelName(c)
	if !ok {
		return nil
	}
	if name == "default" {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "default channel already exists"})
	}
	channel, err := s.Registry.CreateChannel(c.Request().Context(), getTenant(c), c.Param("name"), name)
	if errors.Is(err, pgx.ErrNoRows) {
		return c.JSON(http.StatusNotFound, map[string]string{"error": "agent not found"})
	}
	if isUniqueViolation(err) {
		return c.JSON(http.StatusConflict, map[string]string{"error": "channel name already exists"})
	}
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}
	return c.JSON(http.StatusCreated, channel)
}

func (s *Server) handleRenameChannel(c echo.Context) error {
	if c.Param("id") == "default" {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "default channel cannot be renamed"})
	}
	name, ok := bindChannelName(c)
	if !ok {
		return nil
	}
	if name == "default" {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "channel cannot be renamed to default"})
	}
	channel, err := s.Registry.RenameChannel(c.Request().Context(), getTenant(c), c.Param("name"), c.Param("id"), name)
	if errors.Is(err, pgx.ErrNoRows) {
		return c.JSON(http.StatusNotFound, map[string]string{"error": "channel or agent not found"})
	}
	if isUniqueViolation(err) {
		return c.JSON(http.StatusConflict, map[string]string{"error": "channel name already exists"})
	}
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}
	return c.JSON(http.StatusOK, channel)
}

func (s *Server) handleDeleteChannel(c echo.Context) error {
	if c.Param("id") == "default" {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "default channel cannot be deleted"})
	}
	err := s.Registry.DeleteChannel(c.Request().Context(), getTenant(c), c.Param("name"), c.Param("id"))
	if errors.Is(err, pgx.ErrNoRows) {
		return c.JSON(http.StatusNotFound, map[string]string{"error": "channel or agent not found"})
	}
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}
	return c.JSON(http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleReorderChannels(c echo.Context) error {
	var req reorderChannelsRequest
	if err := c.Bind(&req); err != nil || len(req.OrderedIDs) == 0 {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "ordered_ids must be a non-empty array"})
	}
	if req.OrderedIDs[0] != "default" {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "default channel must be first"})
	}
	seen := make(map[string]struct{}, len(req.OrderedIDs))
	for _, id := range req.OrderedIDs {
		if id == "" {
			return c.JSON(http.StatusBadRequest, map[string]string{"error": "ordered_ids must contain non-empty strings"})
		}
		if _, duplicate := seen[id]; duplicate {
			return c.JSON(http.StatusBadRequest, map[string]string{"error": "ordered_ids must not contain duplicates"})
		}
		seen[id] = struct{}{}
	}
	err := s.Registry.ReorderChannels(c.Request().Context(), getTenant(c), c.Param("name"), req.OrderedIDs)
	if errors.Is(err, registry.ErrInvalidChannelOrder) {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "ordered_ids must contain every channel exactly once with default first"})
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return c.JSON(http.StatusNotFound, map[string]string{"error": "agent not found"})
	}
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}
	return c.NoContent(http.StatusNoContent)
}

func bindChannelName(c echo.Context) (string, bool) {
	var req channelNameRequest
	if err := c.Bind(&req); err != nil {
		_ = c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return "", false
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		_ = c.JSON(http.StatusBadRequest, map[string]string{"error": "name is required"})
		return "", false
	}
	return name, true
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}
