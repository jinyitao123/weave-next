package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"time"

	"github.com/google/uuid"
	"github.com/jinyitao123/loom/contract"
	"github.com/jinyitao123/weave/internal/kernel/compiler"
	"github.com/jinyitao123/weave/internal/kernel/loomruntime"
	"github.com/jinyitao123/weave/internal/kernel/schedule"
	"github.com/labstack/echo/v4"
)

var timeOfDayRe = regexp.MustCompile(`^([01][0-9]|2[0-3]):[0-5][0-9]$`)

type agentScheduleRequest struct {
	ID        string     `json:"id"`
	Agent     string     `json:"agent"`
	Message   string     `json:"message"`
	Kind      string     `json:"kind"`
	TimeOfDay string     `json:"time_of_day"`
	RunAt     *time.Time `json:"run_at"`
	Timezone  string     `json:"timezone"`
	Enabled   *bool      `json:"enabled"`
}

func (s *Server) handleListAgentSchedules(c echo.Context) error {
	if s.AgentSchedules == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": "agent schedule store not available"})
	}
	list, err := s.AgentSchedules.List(c.Request().Context(), getTenant(c))
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}
	return c.JSON(http.StatusOK, list)
}

func (s *Server) handleUpsertAgentSchedule(c echo.Context) error {
	if s.AgentSchedules == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": "agent schedule store not available"})
	}
	var req agentScheduleRequest
	decoder := json.NewDecoder(c.Request().Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&req); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": err.Error()})
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		if err == nil {
			err = fmt.Errorf("request body must contain one JSON object")
		}
		return c.JSON(http.StatusBadRequest, map[string]string{"error": err.Error()})
	}
	if req.Agent == "" {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "agent is required"})
	}
	if req.Kind != schedule.KindDaily && req.Kind != schedule.KindOnce {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "kind must be daily or once"})
	}
	if req.Kind == schedule.KindDaily && !timeOfDayRe.MatchString(req.TimeOfDay) {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "time_of_day must be HH:MM (00:00-23:59)"})
	}
	if req.Kind == schedule.KindDaily && req.Timezone == "" {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "timezone is required for daily schedules"})
	}
	if req.Kind == schedule.KindOnce && req.RunAt == nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "run_at is required for once schedules"})
	}
	enabled := true
	if req.Enabled != nil {
		enabled = *req.Enabled
	}
	item, err := s.AgentSchedules.Upsert(
		c.Request().Context(),
		getTenant(c),
		schedule.UpsertSchedule{
			ID:         req.ID,
			TargetKind: schedule.TargetAgent,
			Agent:      req.Agent,
			Message:    req.Message,
			Kind:       req.Kind,
			TimeOfDay:  req.TimeOfDay,
			RunAt:      req.RunAt,
			Timezone:   req.Timezone,
			Enabled:    enabled,
		},
	)
	if err != nil {
		status := http.StatusInternalServerError
		switch {
		case errors.Is(err, schedule.ErrInvalidSchedule):
			status = http.StatusBadRequest
		case errors.Is(err, schedule.ErrScheduleNotFound):
			status = http.StatusNotFound
		}
		return c.JSON(status, map[string]string{"error": err.Error()})
	}
	return c.JSON(http.StatusOK, item)
}

func (s *Server) handleDeleteAgentSchedule(c echo.Context) error {
	if s.AgentSchedules == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": "agent schedule store not available"})
	}
	id := c.QueryParam("id")
	if id == "" {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "id required"})
	}
	if err := s.AgentSchedules.Delete(c.Request().Context(), getTenant(c), id); err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, schedule.ErrScheduleNotFound) {
			status = http.StatusNotFound
		}
		return c.JSON(status, map[string]string{"error": err.Error()})
	}
	return c.NoContent(http.StatusNoContent)
}

// RunAgentOnce 以"调度员"身份跑一次 agent 的业务流，返回 run ID。
// 从 handleChat 非流式路径提炼的最小执行（无 session 装载/保存、无 memory）：
// Registry.Get → buildToolDispatcher → loomruntime.Run（带 Store，终态台账自动落库）。
// yield（停在人审）StopReason=yielded 且 err 为 nil，属正常停点不算错。
// NOTE: no production callers — 调度执行现走 taskqueue/ExecuteChat,去留另议。
func (s *Server) RunAgentOnce(ctx context.Context, tenant, agent, message string) (string, error) {
	rec, err := s.Registry.Get(ctx, tenant, agent)
	if err != nil {
		return "", fmt.Errorf("agent %q not found: %w", agent, err)
	}
	var teamExecution *teamSessionExecution
	if !teamAssemblerDisabled() {
		sessionID := uuid.NewString()
		sessionKey := tenant + ":scheduler:" + rec.Name + ":" + sessionID
		teamExecution, err = s.acquireTeamSession(
			ctx, rec, tenant, "", nil, "scheduler", sessionID, sessionKey, "schedule:"+sessionID,
		)
		if err != nil {
			return "", err
		}
		ctx = contextWithTeamSessionExecution(ctx, teamExecution)
	}

	llm, err := s.llmFor(ctx, tenant)
	if err != nil {
		return "", err
	}
	tools := s.buildToolDispatcher(rec, tenant, "scheduler", "", llm, s.memoryFor(ctx, tenant), false, ctx)
	compileOpts := compiler.CompileOpts{
		Effort:               contract.EffortMedium,
		Store:                s.Store,
		SubAgentStepResolver: s.resolveSubAgent,
		AgentRunner:          s.compilerAgentRunner(tenant, "scheduler", llm, s.memoryFor(ctx, tenant), nil),
	}
	var terminalAttribution loomruntime.TerminalAttribution
	if teamExecution != nil {
		terminalAttribution = teamExecution.TerminalAttribution
	} else {
		terminalAttribution, err = legacyRootTerminalAttribution(tenant)
		if err != nil {
			return "", err
		}
	}
	terminalSink, err := s.rootTerminalSink()
	if err != nil {
		return "", err
	}
	result, runErr := s.runChatSession(ctx, tenant, rec, loomruntime.Input{
		Messages:        []contract.Message{{Role: "user", Content: message}},
		LastUserMessage: message,
		UserID:          "scheduler",
	}, terminalAttribution, loomruntime.Dependencies{
		LLM:                llm,
		Tools:              tools,
		Store:              s.Store,
		TerminalSink:       terminalSink,
		LifecycleHook:      s.RunLifecycleHook,
		SkillVersionReader: s.Skills,
		CompileOpts:        compileOpts,
	}, teamExecution)
	if !result.Ran() && teamExecution == nil {
		return "", runErr
	}
	// 带 result 的错误说明 run 记录已落库（audit 可见其失败态）：记日志但仍返回 RunID。
	if runErr != nil {
		slog.Warn("scheduled agent run ended with error", "agent", agent, "run_id", result.RunID, "err", runErr)
	}
	if teamExecution != nil {
		if err := s.finishTeamSessionExecution(ctx, teamExecution, result, message, nil, runErr); err != nil {
			return "", err
		}
	}
	return result.RunID, nil
}
