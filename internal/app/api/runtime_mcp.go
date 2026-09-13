package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/jinyitao123/weave/internal/base/execution"
	"github.com/jinyitao123/weave/internal/base/frozen"
	"github.com/jinyitao123/weave/internal/kernel/mcphost"
	"github.com/jinyitao123/weave/internal/kernel/runtimes"
	"github.com/jinyitao123/weave/internal/kernel/secret"
	"github.com/labstack/echo/v4"
)

// handleRuntimeTaskMCP reuses the governed gateway for legacy Loom tasks and
// published CLI tasks. Each request rechecks the current claim before effects.
func (s *Server) handleRuntimeTaskMCP(c echo.Context) error {
	runtime, task, err := s.currentMCPTask(c)
	if err != nil {
		return err
	}

	var payload runtimes.EngineExecRequest
	if decodeErr := json.Unmarshal(task.Payload, &payload); decodeErr != nil {
		return c.JSON(http.StatusNotFound, map[string]string{"error": "MCP server not found"})
	}
	// The MCP facade is loom-only; CLI engines reach MCP through their own
	// per-server boundary tokens delivered in the engine env.
	if (payload.FrozenMCP == nil && runtimes.CanonicalEngine(payload.Engine) != runtimes.EngineLoom) || payload.Record == nil {
		return c.JSON(http.StatusNotFound, map[string]string{"error": "MCP server not found"})
	}

	idx, convErr := strconv.Atoi(c.Param("idx"))
	if convErr != nil || idx < 0 || idx >= len(payload.Record.MCPServers) {
		return c.JSON(http.StatusNotFound, map[string]string{"error": "MCP server not found"})
	}

	if payload.FrozenMCP != nil {
		if _, scoped := c.Get(taskMCPClaimsContextKey).(secret.TaskMCPClaims); !scoped {
			return echo.NewHTTPError(http.StatusForbidden, "task MCP token required")
		}
		binding := payload.FrozenMCP.Bindings[idx]
		pool := s.GetPool()
		if pool == nil {
			pool = s.Pool
		}
		if pool == nil || s.MCPRegistry == nil {
			return echo.NewHTTPError(http.StatusServiceUnavailable, "MCP credentials unavailable")
		}
		if binding.AccessRef.WorkspaceID != task.WorkspaceID || binding.AccessRef.ResourceID != binding.ServerID {
			return echo.NewHTTPError(http.StatusForbidden, "MCP reference mismatch")
		}
		if task.Subject.UserID == "" || task.Subject.ServiceID != "" {
			return echo.NewHTTPError(http.StatusForbidden, "MCP user delegation required")
		}
		authorized := runtimeMCPReferenceContext(c.Request().Context(), task.Subject, binding.AccessRef)
		tx, err := pool.Begin(authorized)
		if err != nil {
			return echo.NewHTTPError(http.StatusServiceUnavailable, "MCP credentials unavailable")
		}
		defer func() { _ = tx.Rollback(authorized) }()
		material, err := s.MCPRegistry.ResolveMCPAccessTx(authorized, tx, binding.AccessRef)
		if err != nil {
			return echo.NewHTTPError(http.StatusForbidden, "MCP access unavailable")
		}
		if err := tx.Commit(authorized); err != nil {
			return echo.NewHTTPError(http.StatusServiceUnavailable, "MCP credentials unavailable")
		}

		headers := map[string]string{}
		for name, value := range material.Headers() {
			headers[name] = string(value)
		}
		dispatcher, err := s.mcpAccessFactory().BuildFrozenServer(task.WorkspaceID, payload.Record, binding, headers, func(ctx context.Context) error {
			ctx = runtimeMCPReferenceContext(ctx, task.Subject, binding.AccessRef)
			gate, err := pool.Begin(ctx)
			if err != nil {
				return err
			}
			defer func() { _ = gate.Rollback(ctx) }()
			if err := s.MCPRegistry.ValidateReferenceTx(ctx, gate, binding.AccessRef); err != nil {
				return err
			}
			if err := gate.Commit(ctx); err != nil {
				return err
			}
			_, _, err = s.currentMCPTask(c)
			return err
		})
		if err != nil {
			return echo.NewHTTPError(http.StatusForbidden, "MCP contract unavailable")
		}
		return handleStableGatewayRPC(c, dispatcher)
	}
	// The governing workspace is the runtime's own workspace (lease-verified),
	// never a value read out of the task payload.
	tenant := runtime.WorkspaceID
	broker := mcphost.NewToolBroker(s.mcpAccessFactory())
	dispatcher, buildErr := broker.BuildMCPServerAt(
		c.Request().Context(),
		mcphost.ToolBrokerRequest{WorkspaceID: tenant, Agent: payload.Record},
		idx,
	)
	if buildErr != nil {
		return c.JSON(http.StatusNotFound, map[string]string{"error": "MCP server not found"})
	}
	return handleStableGatewayRPC(c, dispatcher)
}

// A task-scoped MCP token proves one current claim and one frozen binding. It
// may delegate that exact workspace service reference for the task's user, but
// never acts as a general credential grant.
func runtimeMCPReferenceContext(ctx context.Context, subject execution.Subject, expected frozen.CredentialReference) context.Context {
	return frozen.WithServiceReferenceAuthorization(ctx, func(_ context.Context, actual execution.Subject, ref frozen.CredentialReference) error {
		if subject.Validate() != nil || subject.UserID == "" || subject.ServiceID != "" ||
			actual != subject || ref != expected || ref.WorkspaceID != subject.WorkspaceID {
			return frozen.ErrCredentialSubjectDenied
		}
		return nil
	})
}
