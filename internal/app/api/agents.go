package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"

	"github.com/jinyitao123/weave/internal/app/metateam"
	"github.com/jinyitao123/weave/internal/kernel/engine"
	"github.com/jinyitao123/weave/internal/kernel/mcphost"
	"github.com/jinyitao123/weave/internal/kernel/mcpregistry"
	"github.com/jinyitao123/weave/internal/kernel/registry"
	"github.com/jinyitao123/weave/internal/kernel/runtimes"
	"github.com/labstack/echo/v4"
)

var agentNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)

var errInvalidAgentRuntime = errors.New("invalid agent runtime binding")

type agentWriteRequest struct {
	registry.AgentRecord
	ownerUserIDPresent     bool
	subAgentsPresent       bool
	skillRefsPresent       bool
	enginePresent          bool
	modelPresent           bool
	runtimeIDPresent       bool
	mcpServersPresent      bool
	permissionsPresent     bool
	toolLoopControlPresent bool
}

func (r *agentWriteRequest) UnmarshalJSON(data []byte) error {
	type agentRecordAlias registry.AgentRecord
	var rec agentRecordAlias
	if err := json.Unmarshal(data, &rec); err != nil {
		return err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	r.AgentRecord = registry.AgentRecord(rec)
	_, r.ownerUserIDPresent = fields["owner_user_id"]
	_, r.subAgentsPresent = fields["sub_agents"]
	_, r.skillRefsPresent = fields["skill_refs"]
	_, r.enginePresent = fields["engine"]
	_, r.modelPresent = fields["model"]
	_, r.runtimeIDPresent = fields["runtime_id"]
	_, r.mcpServersPresent = fields["mcp_servers"]
	_, r.permissionsPresent = fields["permissions"]
	_, r.toolLoopControlPresent = fields["tool_loop_control"]
	return nil
}

func isValidAgentName(name string) bool {
	return len(name) <= 64 && agentNameRe.MatchString(name)
}

// applyAgentContextDefaults fills platform-default context-management settings
// when an AgentRecord omits them: compaction and memory are enabled by default.
// Explicitly configured values (including Enabled=false) are preserved.
func applyAgentContextDefaults(rec *registry.AgentRecord) {
	if rec.Compaction == nil {
		rec.Compaction = registry.DefaultCompactionConfig()
	}
	if rec.MemoryConfig == nil {
		rec.MemoryConfig = registry.DefaultMemoryConfig()
	}
}

// applyAgentModelDefault keeps in-process Loom's historical default while
// allowing an external CLI Runtime to select its own authenticated default
// model. A CLI Agent with an empty model is intentional: it must not acquire a
// workspace Provider dependency merely because it was created through the
// public AgentRecord API.
func applyAgentModelDefault(rec *registry.AgentRecord) {
	if rec.Model == "" && !engine.IsCLIEngine(rec.Engine) {
		rec.Model = "deepseek-flash"
	}
}

func validateAgentRole(rec *registry.AgentRecord) bool {
	if rec.Role == "" {
		rec.Role = "worker"
	}
	return rec.Role == "worker" || rec.Role == "avatar"
}

func validateAgentVisibility(rec *registry.AgentRecord) bool {
	if rec.Visibility == "" {
		rec.Visibility = registry.VisibilityPublic
	}
	return registry.ValidAgentVisibility(rec.Visibility)
}

func validateAvatarCapabilities(rec *registry.AgentRecord) error {
	if rec.Role != "avatar" {
		return nil
	}
	if rec.GraphDefinition != nil {
		return errors.New("avatar cannot define a custom execution graph")
	}
	if len(rec.SubAgents) > 0 {
		return errors.New("avatar cannot configure sub-agents")
	}
	return nil
}

func (s *Server) validateAgentRuntime(ctx context.Context, tenant string, rec *registry.AgentRecord) error {
	if rec.RuntimeID == "" {
		return nil
	}
	if !engine.IsCLIEngine(rec.Engine) {
		return fmt.Errorf(
			"%w: runtime_id requires engine to be exactly claude, codex, or opencode",
			errInvalidAgentRuntime,
		)
	}
	if s.Runtimes == nil {
		return errors.New("runtime store not configured")
	}
	if _, err := s.Runtimes.Get(ctx, tenant, rec.RuntimeID); err != nil {
		if errors.Is(err, runtimes.ErrRuntimeUnavailable) {
			return fmt.Errorf(
				"%w: runtime %q does not exist in this workspace",
				errInvalidAgentRuntime,
				rec.RuntimeID,
			)
		}
		return fmt.Errorf("validate runtime %q: %w", rec.RuntimeID, err)
	}
	return nil
}

func agentRuntimeValidationResponse(c echo.Context, err error) error {
	if errors.Is(err, errInvalidAgentRuntime) {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": err.Error()})
	}
	return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
}

func (s *Server) agentMCPResolver() mcphost.AccessResolver {
	if s.MCPResolver != nil {
		return s.MCPResolver
	}
	return mcpregistry.NewResolver(s.MCPRegistry)
}

func (s *Server) validateAgentMCPAccess(ctx context.Context, tenant string, rec *registry.AgentRecord) error {
	if rec == nil || len(rec.MCPServers) == 0 {
		return nil
	}
	_, err := s.agentMCPResolver().ResolveAgent(ctx, tenant, rec)
	return err
}

func agentMCPValidationResponse(c echo.Context, err error) error {
	status := http.StatusConflict
	if errors.Is(err, mcpregistry.ErrKeyUnavailable) || errors.Is(err, mcpregistry.ErrResolverUnavailable) {
		status = http.StatusServiceUnavailable
	}
	return c.JSON(status, map[string]string{"error": err.Error()})
}

func agentOwnerValidationResponse(c echo.Context, err error) error {
	if errors.Is(err, registry.ErrOwnerNotWorkspaceMember) {
		return c.JSON(http.StatusUnprocessableEntity, map[string]string{"error": err.Error()})
	}
	return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
}

// agentWritePutErrorResponse maps registry write failures onto HTTP: owner
// membership and SkillRef binding violations are client errors (422), while
// anything else stays a 500.
func agentWritePutErrorResponse(c echo.Context, err error) error {
	if errors.Is(err, registry.ErrOwnerNotWorkspaceMember) {
		return agentOwnerValidationResponse(c, err)
	}
	if errors.Is(err, registry.ErrSkillVersionRequired) {
		return c.JSON(http.StatusUnprocessableEntity, map[string]string{"error": err.Error()})
	}
	return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
}

func canChangeAgentOwner(c echo.Context) bool {
	if c.Get(authSourceContextKey) != authSourceJWT {
		return false
	}
	roles, _ := c.Get("roles").([]string)
	for _, role := range roles {
		if role == "admin" || role == "owner" {
			return true
		}
	}
	return false
}

func ownerUserIDsEqual(left, right *string) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}

func (s *Server) validateAgentOwner(ctx context.Context, tenant string, ownerUserID *string) error {
	if ownerUserID == nil {
		return nil
	}
	isMember, err := s.Registry.IsWorkspaceMember(ctx, tenant, *ownerUserID)
	if err != nil {
		return err
	}
	if !isMember {
		return fmt.Errorf("%w: user %q is not a member of workspace %q", registry.ErrOwnerNotWorkspaceMember, *ownerUserID, tenant)
	}
	return nil
}

func (s *Server) setAgentOwnerOnCreate(ctx context.Context, c echo.Context, tenant string, rec *registry.AgentRecord, ownerPresent bool) error {
	if ownerPresent {
		return s.validateAgentOwner(ctx, tenant, rec.OwnerUserID)
	}
	requesterID := getUserID(c)
	if requesterID == "" {
		return nil
	}
	isMember, err := s.Registry.IsWorkspaceMember(ctx, tenant, requesterID)
	if err != nil {
		return err
	}
	if isMember {
		rec.OwnerUserID = &requesterID
	}
	return nil
}

func (s *Server) handleListAgents(c echo.Context) error {
	tenant := getTenant(c)
	records, err := s.Registry.List(c.Request().Context(), tenant)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}
	if records == nil {
		records = []registry.AgentRecord{}
	}
	visible := records[:0]
	includePlatform := c.QueryParam("include") == "platform"
	for _, record := range records {
		if record.Name == metateam.BlueprintPatchPlannerName {
			continue
		}
		if !includePlatform && record.Visibility == registry.VisibilityPlatform {
			continue
		}
		visible = append(visible, record)
	}
	return c.JSON(http.StatusOK, visible)
}

func (s *Server) handleGetAgent(c echo.Context) error {
	tenant := getTenant(c)
	name := c.Param("name")
	if name == metateam.BlueprintPatchPlannerName {
		return c.JSON(http.StatusNotFound, map[string]string{"error": "agent not found"})
	}
	rec, err := s.Registry.Get(c.Request().Context(), tenant, name)
	if err != nil {
		return c.JSON(http.StatusNotFound, map[string]string{"error": err.Error()})
	}
	return c.JSON(http.StatusOK, rec)
}

func (s *Server) handleCreateAgent(c echo.Context) error {
	tenant := getTenant(c)
	var req agentWriteRequest
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid request body"})
	}
	rec := req.AgentRecord
	if rec.Name == "" {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "name is required"})
	}
	if !isValidAgentName(rec.Name) {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "name must be lowercase alphanumeric, hyphens, or underscores (e.g. my-agent-1)"})
	}
	if !validateAgentRole(&rec) {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "role must be worker or avatar"})
	}
	if !validateAgentVisibility(&rec) {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "visibility must be public, internal_tool, or platform"})
	}
	if err := validateAvatarCapabilities(&rec); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": err.Error()})
	}
	applyAgentModelDefault(&rec)
	applyAgentContextDefaults(&rec)
	if rec.GraphDefinition != nil {
		if err := rec.GraphDefinition.Validate(); err != nil {
			return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid graph_definition: " + err.Error()})
		}
	}
	if err := s.validateAgentRuntime(c.Request().Context(), tenant, &rec); err != nil {
		return agentRuntimeValidationResponse(c, err)
	}
	if err := s.validateAgentMCPAccess(c.Request().Context(), tenant, &rec); err != nil {
		return agentMCPValidationResponse(c, err)
	}
	if err := s.setAgentOwnerOnCreate(c.Request().Context(), c, tenant, &rec, req.ownerUserIDPresent); err != nil {
		return agentOwnerValidationResponse(c, err)
	}
	if err := s.Registry.Put(c.Request().Context(), tenant, &rec); err != nil {
		return agentWritePutErrorResponse(c, err)
	}
	return c.JSON(http.StatusCreated, rec)
}

func (s *Server) handleUpdateAgent(c echo.Context) error {
	tenant := getTenant(c)
	name := c.Param("name")
	if name == metateam.BlueprintPatchPlannerName {
		return c.JSON(http.StatusForbidden, map[string]string{"error": "platform-internal agent"})
	}
	var req agentWriteRequest
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid request body"})
	}
	incoming := req.AgentRecord
	incoming.Name = name
	if !validateAgentRole(&incoming) {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "role must be worker or avatar"})
	}
	if incoming.Visibility != "" && !registry.ValidAgentVisibility(incoming.Visibility) {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "visibility must be public, internal_tool, or platform"})
	}
	if err := validateAvatarCapabilities(&incoming); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": err.Error()})
	}

	if incoming.GraphDefinition != nil {
		if err := incoming.GraphDefinition.Validate(); err != nil {
			return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid graph_definition: " + err.Error()})
		}
	}

	// Merge: preserve fields outside the mutable agent configuration contract.
	// Load existing record and overlay incoming non-zero values.
	ctx := c.Request().Context()
	existing, _ := s.Registry.Get(ctx, tenant, name)
	if existing != nil {
		ownerChanged := req.ownerUserIDPresent && !ownerUserIDsEqual(existing.OwnerUserID, incoming.OwnerUserID)
		if ownerChanged {
			if !canChangeAgentOwner(c) {
				return c.JSON(http.StatusForbidden, map[string]string{"error": "insufficient permissions to change owner_user_id"})
			}
			if err := s.validateAgentOwner(ctx, tenant, incoming.OwnerUserID); err != nil {
				return agentOwnerValidationResponse(c, err)
			}
		}
		merged := mergeAgentRecordWithPresence(existing, &incoming, agentMergePresence{
			subAgentsPresent:       req.subAgentsPresent,
			skillRefsPresent:       req.skillRefsPresent,
			enginePresent:          req.enginePresent,
			modelPresent:           req.modelPresent,
			runtimeIDPresent:       req.runtimeIDPresent,
			mcpServersPresent:      req.mcpServersPresent,
			permissionsPresent:     req.permissionsPresent,
			toolLoopControlPresent: req.toolLoopControlPresent,
		})
		applyAgentContextDefaults(merged)
		if err := validateAvatarCapabilities(merged); err != nil {
			return c.JSON(http.StatusBadRequest, map[string]string{"error": err.Error()})
		}
		if req.ownerUserIDPresent {
			merged.OwnerUserID = incoming.OwnerUserID
		}
		if err := s.validateAgentRuntime(ctx, tenant, merged); err != nil {
			return agentRuntimeValidationResponse(c, err)
		}
		if err := s.validateAgentMCPAccess(ctx, tenant, merged); err != nil {
			return agentMCPValidationResponse(c, err)
		}
		if err := s.Registry.Put(ctx, tenant, merged); err != nil {
			return agentWritePutErrorResponse(c, err)
		}
		return c.JSON(http.StatusOK, merged)
	}

	// No existing record — treat as create.
	if !validateAgentVisibility(&incoming) {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "visibility must be public, internal_tool, or platform"})
	}
	if err := s.setAgentOwnerOnCreate(ctx, c, tenant, &incoming, req.ownerUserIDPresent); err != nil {
		return agentOwnerValidationResponse(c, err)
	}
	applyAgentModelDefault(&incoming)
	applyAgentContextDefaults(&incoming)
	if err := s.validateAgentRuntime(ctx, tenant, &incoming); err != nil {
		return agentRuntimeValidationResponse(c, err)
	}
	if err := s.validateAgentMCPAccess(ctx, tenant, &incoming); err != nil {
		return agentMCPValidationResponse(c, err)
	}
	if err := s.Registry.Put(ctx, tenant, &incoming); err != nil {
		return agentWritePutErrorResponse(c, err)
	}
	return c.JSON(http.StatusOK, incoming)
}

// mergeAgentRecord overlays incoming values onto existing.
//
// API-managed fields are always overwritten (zero value = clear).
// Fields outside the partial update contract use non-zero override to avoid
// accidental clearing when the frontend doesn't send them.
func mergeAgentRecord(
	existing, incoming *registry.AgentRecord,
	subAgentsPresent, skillRefsPresent bool,
) *registry.AgentRecord {
	return mergeAgentRecordWithPresence(existing, incoming, agentMergePresence{
		subAgentsPresent:       subAgentsPresent,
		skillRefsPresent:       skillRefsPresent,
		enginePresent:          true,
		runtimeIDPresent:       true,
		mcpServersPresent:      true,
		permissionsPresent:     true,
		toolLoopControlPresent: true,
	})
}

type agentMergePresence struct {
	subAgentsPresent       bool
	skillRefsPresent       bool
	enginePresent          bool
	modelPresent           bool
	runtimeIDPresent       bool
	mcpServersPresent      bool
	permissionsPresent     bool
	toolLoopControlPresent bool
}

func mergeAgentRecordWithPresence(
	existing, incoming *registry.AgentRecord,
	presence agentMergePresence,
) *registry.AgentRecord {
	merged := *existing // start with all existing values

	// ── API-managed fields: always overwrite (zero = clear) ──

	merged.Name = incoming.Name
	// DisplayName uses non-zero override (like Model): partial patches from
	// config tabs that omit display_name must not clear a set name.
	if incoming.DisplayName != "" {
		merged.DisplayName = incoming.DisplayName
	}
	if presence.enginePresent {
		merged.Engine = incoming.Engine
	}
	if presence.runtimeIDPresent {
		merged.RuntimeID = incoming.RuntimeID
	}
	if incoming.Model != "" || (presence.modelPresent && engine.IsCLIEngine(merged.Engine)) {
		merged.Model = incoming.Model
	}
	merged.Tags = incoming.Tags

	// Spec: field-level merge — preserve Identity.Extended
	if incoming.Spec.Identity.Core != "" || incoming.Spec.SystemPrompt != "" {
		merged.Spec.Identity.Core = incoming.Spec.Identity.Core
		merged.Spec.SystemPrompt = incoming.Spec.SystemPrompt
	}
	if incoming.Spec.Skills != nil {
		merged.Spec.Skills = incoming.Spec.Skills
	}
	if incoming.Spec.Profiles != nil {
		merged.Spec.Profiles = incoming.Spec.Profiles
	}

	if presence.mcpServersPresent {
		merged.MCPServers = incoming.MCPServers
	}
	if presence.permissionsPresent {
		merged.Permissions = incoming.Permissions
	}
	if incoming.MemoryConfig != nil {
		merged.MemoryConfig = incoming.MemoryConfig
	}
	if incoming.MemorySlots != nil {
		merged.MemorySlots = incoming.MemorySlots
	}
	if incoming.Guard != nil {
		merged.Guard = incoming.Guard
	}
	if incoming.Compaction != nil {
		merged.Compaction = incoming.Compaction
	}
	merged.MaxCostUSD = incoming.MaxCostUSD
	merged.MaxTokens = incoming.MaxTokens
	merged.MaxOutputTokens = incoming.MaxOutputTokens
	if presence.toolLoopControlPresent {
		merged.ToolLoopControl = incoming.ToolLoopControl
	}
	merged.StepBudget = incoming.StepBudget
	merged.MaxToolRepeats = incoming.MaxToolRepeats
	merged.FallbackModels = incoming.FallbackModels
	merged.FallbackRetries = incoming.FallbackRetries

	// ── Organization-managed fields: only the roster endpoint may change these ──

	merged.TeamID = existing.TeamID
	merged.Role = existing.Role

	// ── Fields outside the partial update contract: non-zero override only ──
	// These move to always-overwrite when the Advanced tab ships.

	if incoming.OutputSchema != nil {
		merged.OutputSchema = incoming.OutputSchema
	}
	if presence.subAgentsPresent {
		merged.SubAgents = incoming.SubAgents
	}
	if presence.skillRefsPresent {
		merged.SkillRefs = incoming.SkillRefs
	}
	if incoming.GraphType != "" {
		merged.GraphType = incoming.GraphType
	}
	if incoming.GraphDefinition != nil {
		merged.GraphDefinition = incoming.GraphDefinition
	}
	if incoming.Visibility != "" {
		merged.Visibility = incoming.Visibility
	}

	return &merged
}

func (s *Server) handleDeleteAgent(c echo.Context) error {
	tenant := getTenant(c)
	name := c.Param("name")
	if name == metateam.BlueprintPatchPlannerName {
		return c.JSON(http.StatusForbidden, map[string]string{"error": "platform-internal agent"})
	}
	if err := s.Registry.Delete(c.Request().Context(), tenant, name); err != nil {
		return c.JSON(http.StatusNotFound, map[string]string{"error": err.Error()})
	}
	return c.JSON(http.StatusNoContent, nil)
}

type managedRequest struct {
	Worker      string `json:"worker"`
	Instruction string `json:"instruction,omitempty"`
	Kind        string `json:"kind,omitempty"`
}

func (s *Server) handleListManaged(c echo.Context) error {
	records, err := s.Registry.ListManaged(c.Request().Context(), getTenant(c), c.Param("name"))
	if err != nil {
		return c.JSON(http.StatusNotFound, map[string]string{"error": err.Error()})
	}
	if records == nil {
		records = []registry.ManagedAgent{}
	}
	return c.JSON(http.StatusOK, records)
}

func (s *Server) handleLinkManages(c echo.Context) error {
	var req managedRequest
	if err := c.Bind(&req); err != nil || req.Worker == "" {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "worker is required"})
	}
	if req.Kind == "" {
		req.Kind = "dispatch"
	}
	if req.Kind != "consult" && req.Kind != "dispatch" && req.Kind != "handoff" {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "kind must be consult, dispatch, or handoff"})
	}
	if err := s.Registry.LinkManages(c.Request().Context(), getTenant(c), c.Param("name"), req.Worker, req.Instruction, req.Kind); err != nil {
		if errors.Is(err, registry.ErrTeamRosterWriteRequired) || errors.Is(err, registry.ErrTeamNotActive) {
			return c.JSON(http.StatusConflict, map[string]string{"error": err.Error()})
		}
		return c.JSON(http.StatusNotFound, map[string]string{"error": err.Error()})
	}
	return c.NoContent(http.StatusNoContent)
}

func (s *Server) handleUnlinkManages(c echo.Context) error {
	if err := s.Registry.UnlinkManages(c.Request().Context(), getTenant(c), c.Param("name"), c.Param("worker")); err != nil {
		if errors.Is(err, registry.ErrTeamRosterWriteRequired) {
			return writeRevocationError(
				c,
				http.StatusConflict,
				"team_roster_write_required",
				"team worker changes must use the complete roster command",
				nil,
			)
		}
		return c.JSON(http.StatusNotFound, map[string]string{"error": err.Error()})
	}
	return c.NoContent(http.StatusNoContent)
}
