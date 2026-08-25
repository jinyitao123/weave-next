package api

import (
	"net/http"

	"github.com/jinyitao123/weave/internal/kernel/registry"
	"github.com/labstack/echo/v4"
)

func (s *Server) handleGetMemorySlots(c echo.Context) error {
	record, err := s.Registry.Get(c.Request().Context(), getTenant(c), c.Param("name"))
	if err != nil {
		return c.JSON(http.StatusNotFound, map[string]string{"error": err.Error()})
	}
	slots := record.MemorySlots
	if slots == nil {
		slots = []registry.MemorySlot{}
	}
	return c.JSON(http.StatusOK, slots)
}

func (s *Server) handlePutMemorySlots(c echo.Context) error {
	var slots []registry.MemorySlot
	if err := c.Bind(&slots); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid request body"})
	}
	ctx := c.Request().Context()
	record, err := s.Registry.Get(ctx, getTenant(c), c.Param("name"))
	if err != nil {
		return c.JSON(http.StatusNotFound, map[string]string{"error": err.Error()})
	}
	record.MemorySlots = slots
	if err := s.Registry.Put(ctx, getTenant(c), record); err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}
	if record.MemorySlots == nil {
		record.MemorySlots = []registry.MemorySlot{}
	}
	return c.JSON(http.StatusOK, record.MemorySlots)
}

func (s *Server) handleGetMemoryProfile(c echo.Context) error {
	userID := c.QueryParam("user")
	if userID == "" {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "user is required"})
	}
	ctx := c.Request().Context()
	workspaceID := getTenant(c)
	record, err := s.Registry.Get(ctx, workspaceID, c.Param("name"))
	if err != nil {
		return c.JSON(http.StatusNotFound, map[string]string{"error": err.Error()})
	}
	profile, err := s.OwnerMem.Get(ctx, workspaceID, record.ID, userID)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}
	return c.JSON(http.StatusOK, profile)
}
