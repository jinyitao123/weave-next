package api

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jinyitao123/weave/internal/base/execution"
	"github.com/jinyitao123/weave/internal/kernel/taskqueue"
	"github.com/jinyitao123/weave/internal/kernel/compiler"
	"github.com/jinyitao123/weave/internal/kernel/engine"
	"github.com/jinyitao123/weave/internal/kernel/execenv"
	"github.com/jinyitao123/weave/internal/kernel/runtimes"
	"github.com/jinyitao123/weave/internal/kernel/secret"
	"github.com/labstack/echo/v4"
)

const taskMCPClaimsContextKey = "task-mcp-claims"

func taskMCPBindingDigest(payload runtimes.EngineExecRequest, index int) (string, error) {
	if payload.FrozenMCP == nil || index < 0 || index >= len(payload.FrozenMCP.Bindings) {
		return "", errors.New("missing frozen MCP binding")
	}
	raw, err := json.Marshal(payload.FrozenMCP)
	if err != nil {
		return "", err
	}
	// The whole published invocation fences ordering and the selected server.
	digest := sha256.Sum256(raw)
	return hex.EncodeToString(digest[:]), nil
}

func validFrozenMCPTask(task *taskqueue.Task, payload runtimes.EngineExecRequest) bool {
	a := payload.FrozenMCP
	return task.Kind == "engine_exec" && engine.IsCLIEngine(payload.Engine) && task.RunSnapshotID != "" && task.ExecutionScope == execution.ScopeTeamWorkerLeaf && a != nil && payload.BoundMCP && a.FactoryKey == compiler.StandardFrozenCLIToolsKey() && a.WorkspaceID == task.WorkspaceID && a.AgentID == task.AgentID && a.AgentVersion == int64(task.AgentVersion) && a.RunSnapshotID == task.RunSnapshotID && payload.Record != nil && payload.Record.Engine == payload.Engine && payload.Record.ID == task.AgentID && payload.Record.Version == task.AgentVersion && payload.Record.WorkspaceID == task.WorkspaceID && len(a.Bindings) > 0 && len(a.Bindings) == len(payload.Record.MCPServers)
}

func activeMCPClaim(task *taskqueue.Task, runtime *runtimes.Runtime, now time.Time) bool {
	return task != nil && runtime != nil && runtime.Enabled && runtime.RevokedAt == nil && runtime.DeletedAt == nil && task.WorkspaceID == runtime.WorkspaceID && task.RuntimeID == runtime.ID && task.Status == taskqueue.StatusRunning && task.LeaseExpiresAt != nil && task.LeaseExpiresAt.After(now) && task.WorkerID == runtimes.RuntimeWorkerID(runtime.WorkspaceID, runtime.ID)
}

// Task URLs are relative in claim responses. The daemon anchors them to its
// authenticated server origin, avoiding request Host/header based URL minting.
func (s *Server) redactRuntimeClaim(task *taskqueue.Task) (json.RawMessage, error) {
	redacted, err := runtimes.RedactClaimPayload(task.Payload)
	if err != nil {
		return nil, err
	}
	var source, claimed runtimes.EngineExecRequest
	if json.Unmarshal(task.Payload, &source) != nil || source.FrozenMCP == nil {
		return redacted, nil
	}
	if !validFrozenMCPTask(task, source) || task.ClaimEpoch <= 0 {
		return nil, errors.New("invalid frozen MCP task claim")
	}
	if err := json.Unmarshal(redacted, &claimed); err != nil {
		return nil, err
	}
	for index := range source.FrozenMCP.Bindings {
		digest, err := taskMCPBindingDigest(source, index)
		if err != nil {
			return nil, err
		}
		token, err := secret.SignTaskMCPToken(secret.TaskMCPClaims{WorkspaceID: task.WorkspaceID, TaskID: task.ID, RuntimeID: task.RuntimeID, ServerIndex: index, BindingDigest: digest, ClaimEpoch: task.ClaimEpoch})
		if err != nil {
			return nil, err
		}
		claimed.TaskMCP = append(claimed.TaskMCP, execenv.TaskMCPTarget{URL: fmt.Sprintf("/v1/runtime/tasks/%s/mcp/%d", task.ID, index), Token: token})
	}
	return json.Marshal(claimed)
}

func (s *Server) taskMCPAuthMiddleware() echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			token := strings.TrimPrefix(c.Request().Header.Get(echo.HeaderAuthorization), "Bearer ")
			if strings.HasPrefix(token, secret.TaskMCPTokenPrefix) {
				claims, err := secret.VerifyTaskMCPToken(token)
				if err != nil {
					return echo.NewHTTPError(http.StatusUnauthorized, "invalid task MCP token")
				}
				c.Set(taskMCPClaimsContextKey, claims)
				return next(c)
			}
			return s.runtimeAuthMiddleware()(next)(c)
		}
	}
}

func (s *Server) currentMCPTask(c echo.Context) (*runtimes.Runtime, *taskqueue.Task, error) {
	deny := echo.NewHTTPError(http.StatusForbidden, "MCP task claim is no longer valid")
	claims, scoped := c.Get(taskMCPClaimsContextKey).(secret.TaskMCPClaims)
	if !scoped {
		runtime, task, err := s.claimedRuntimeTask(c)
		if err != nil {
			return nil, nil, err
		}
		if !activeMCPClaim(task, runtime, time.Now()) {
			return nil, nil, deny
		}
		return runtime, task, nil
	}
	index, err := strconv.Atoi(c.Param("idx"))
	if err != nil || claims.TaskID != c.Param("id") || claims.ServerIndex != index || s.Tasks == nil || s.Runtimes == nil {
		return nil, nil, deny
	}
	runtime, err := s.Runtimes.Get(c.Request().Context(), claims.WorkspaceID, claims.RuntimeID)
	if err != nil {
		return nil, nil, deny
	}
	task, err := s.Tasks.Get(c.Request().Context(), claims.WorkspaceID, claims.TaskID)
	if err != nil || !activeMCPClaim(task, runtime, time.Now()) || task.ClaimEpoch != claims.ClaimEpoch {
		return nil, nil, deny
	}
	var payload runtimes.EngineExecRequest
	if json.Unmarshal(task.Payload, &payload) != nil || !validFrozenMCPTask(task, payload) {
		return nil, nil, deny
	}
	digest, err := taskMCPBindingDigest(payload, index)
	if err != nil || digest != claims.BindingDigest {
		return nil, nil, deny
	}
	return runtime, task, nil
}
