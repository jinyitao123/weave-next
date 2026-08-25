package api

import (
	"net/http"

	"github.com/jinyitao123/weave/internal/app/schedules"
	"github.com/labstack/echo/v4"
)

func (s *Server) handleGetSchedule(c echo.Context) error {
	if s.ScheduleStore == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": "schedule store not available"})
	}
	tenant := getTenant(c)
	sourceURL := c.QueryParam("source_url")
	if sourceURL == "" {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "source_url required"})
	}

	sched, err := s.ScheduleStore.Get(c.Request().Context(), tenant, sourceURL)
	if err != nil {
		// Not found — return empty default
		return c.JSON(http.StatusOK, map[string]any{
			"source_url":  sourceURL,
			"sync_mode":   "manual",
			"frequency":   "daily",
			"time_of_day": "00:00",
			"strategy":    "full",
			"conflict":    "source",
			"tables":      []string{},
		})
	}
	return c.JSON(http.StatusOK, sched)
}

func (s *Server) handleListSchedules(c echo.Context) error {
	if s.ScheduleStore == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": "schedule store not available"})
	}
	tenant := getTenant(c)

	list, err := s.ScheduleStore.List(c.Request().Context(), tenant)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}
	if list == nil {
		list = []schedules.Schedule{}
	}
	return c.JSON(http.StatusOK, list)
}

func (s *Server) handleDeleteSchedule(c echo.Context) error {
	if s.ScheduleStore == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": "schedule store not available"})
	}
	tenant := getTenant(c)
	sourceURL := c.QueryParam("source_url")
	if sourceURL == "" {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "source_url required"})
	}

	if err := s.ScheduleStore.Delete(c.Request().Context(), tenant, sourceURL); err != nil {
		return c.JSON(http.StatusNotFound, map[string]string{"error": err.Error()})
	}
	return c.NoContent(http.StatusNoContent)
}

func (s *Server) handleUpsertSchedule(c echo.Context) error {
	if s.ScheduleStore == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": "schedule store not available"})
	}
	tenant := getTenant(c)

	var sched schedules.Schedule
	if err := c.Bind(&sched); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": err.Error()})
	}
	if sched.SourceURL == "" {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "source_url required"})
	}
	sched.Tenant = tenant

	if err := s.ScheduleStore.Upsert(c.Request().Context(), &sched); err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}
	return c.JSON(http.StatusOK, sched)
}
