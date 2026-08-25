package teamforge

// This file is the shared write-tool skeleton for the three-layer write
// tools (plan §10.2). T03 (agent assembly) builds on it today; T04 (employee
// internal graphs) and T05 (team workflows) must reuse these helpers instead
// of copying them:
//   - WriteGate.authorize              — per-call BuildAuthorizationReceipt gate
//   - WriteGate.requireBuildRunContext — call-envelope ↔ receipt run binding
//   - WriteGate.writeTx                — begin/commit/rollback orchestration
//   - WriteGate.recordAudit / recordAudit — audit every outcome (T02 convention)
//   - toolError and the validation helpers — IsError results and shared rules
//
// Every write call carries build_run_id, is gated on a receipt minted by
// teambuild.Store.AuthorizeBuildRun (its fields are private, so it cannot be
// forged), validates the target asset against the receipt scope, and writes
// only through the narrow write interfaces in deps.go.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jinyitao123/loom/contract"
	"github.com/jinyitao123/weave/internal/kernel/engine"
	"github.com/jinyitao123/weave/internal/base/frozen"
	"github.com/jinyitao123/weave/internal/kernel/registry"
	"github.com/jinyitao123/weave/internal/kernel/runtimes"
	"github.com/jinyitao123/weave/internal/build/teambuild"
)

// Sentinel write-rejection errors. Errors.Is distinguishes each rejection
// class; every rejection is surfaced to the LLM as an IsError ToolResult.
var (
	// ErrWriteReceiptRejected reports that teambuild.Store.ValidateReceipt
	// refused the target asset for the dispatcher's bound receipt.
	ErrWriteReceiptRejected = errors.New("build write receipt rejected")
	// ErrWriteBuildRunMismatch reports a call whose build_run_id does not
	// match the build run bound to the dispatcher's receipt.
	ErrWriteBuildRunMismatch = errors.New("build_run_id does not match the authorization receipt")
	// ErrWriteFieldForbidden reports a parameter outside the assembly
	// whitelist (graph_definition, platform-managed fields, ...).
	ErrWriteFieldForbidden = errors.New("field is outside the agent assembly whitelist")
	// ErrWriteNameInvalid reports an agent name failing the platform regex
	// (^[a-z0-9][a-z0-9_-]*$ with a 64-character cap).
	ErrWriteNameInvalid = errors.New("invalid agent name")
	// ErrWriteRoleInvalid reports a role other than worker or avatar.
	ErrWriteRoleInvalid = errors.New("role must be worker or avatar")
	// ErrWriteAvatarInvalid reports an avatar carrying a graph or sub-agents.
	ErrWriteAvatarInvalid = errors.New("avatar capability constraint violated")
	// ErrWriteEngineInvalid reports an engine outside ""/loom/opencode/codex/claude.
	ErrWriteEngineInvalid = errors.New("invalid engine")
	// ErrWriteRuntimeInvalid reports a broken engine/runtime binding.
	ErrWriteRuntimeInvalid = errors.New("invalid agent runtime binding")
	// ErrWriteModelUnresolvable reports a model with no workspace provider
	// revision (F2 gate moved from freeze time to assembly time).
	ErrWriteModelUnresolvable = errors.New("model has no workspace provider revision")
	// ErrWriteModelEngineMismatch reports a model binding that is valid in the
	// workspace but cannot be consumed by the selected execution engine. CLI
	// engines do not use arbitrary workspace provider credentials: codex and
	// opencode are materialized against the server's OpenAI-compatible gateway,
	// while claude uses the server's Anthropic provider.
	ErrWriteModelEngineMismatch = errors.New("model provider is incompatible with the execution engine")
	// ErrWriteCLIGraphConflict reports an attempt to move an agent with a
	// non-empty graph_definition onto a CLI engine (F13 gate).
	ErrWriteCLIGraphConflict = errors.New("CLI engine cannot host an internal graph")
	// ErrWriteBuiltinAssetForbidden reports an attempt to modify a built-in
	// platform asset through the build tools. Assets whose names carry the
	// reserved "__" prefix (the meta team employees and team, the built-in
	// graph designer, ...) are owned by the platform seed and are never
	// writable through teamforge write tools, no matter what the receipt
	// scope claims.
	ErrWriteBuiltinAssetForbidden = errors.New("内置平台资产不可通过建设工具修改")
)

// agentNameRe mirrors the platform agent-name contract (api/agents.go). The
// api handlers are not importable, so the rule is re-implemented here.
var agentNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)

// WriteGate is the shared write-tool skeleton. The workspace, calling agent,
// and BuildAuthorizationReceipt are fixed at construction time; every write
// call re-checks the receipt, the call envelope, and the assembled record
// before committing one immutable version.
type WriteGate struct {
	workspaceID string
	agent       string
	receipt     teambuild.BuildAuthorizationReceipt
	validator   ReceiptValidator
	audit       AuditRecorder
	deps        WriteDeps
}

// ReceiptValidator re-checks one bound receipt against the persisted run and
// the requested asset. *teambuild.Store satisfies it.
type ReceiptValidator interface {
	ValidateReceipt(
		ctx context.Context,
		workspaceID string,
		receipt teambuild.BuildAuthorizationReceipt,
		assetRef teambuild.AssetRef,
	) error
}

func newWriteGate(
	workspaceID, agentName string,
	receipt teambuild.BuildAuthorizationReceipt,
	validator ReceiptValidator,
	audit AuditRecorder,
	deps WriteDeps,
) *WriteGate {
	return &WriteGate{
		workspaceID: workspaceID,
		agent:       agentName,
		receipt:     receipt,
		validator:   validator,
		audit:       audit,
		deps:        deps,
	}
}

// authorize is the per-call receipt gate: the target asset must sit inside
// the frozen asset scope of a live, non-terminal, unexpired build run bound
// to the dispatcher's receipt.
func (g *WriteGate) authorize(ctx context.Context, name string) error {
	return g.authorizeRef(ctx, teambuild.AssetRef{Kind: "agent", Name: name})
}

// authorizeRef is the kind-aware variant used by the team and workflow write
// tools: the caller names the exact asset kind (agent, team, workflow) and
// either its stable ID or its create-mode name. The receipt scope check is
// identical to authorize; only the AssetRef kind and identity differ.
func (g *WriteGate) authorizeRef(ctx context.Context, ref teambuild.AssetRef) error {
	// Built-in platform assets (reserved "__" name prefix) are rejected
	// before any scope is consulted: authorizeRef is the single confluence
	// point of every write tool (agent assembly, employee graphs, team and
	// workflow), so one guard covers all of them. Refs carrying only a
	// stable ID have no built-in name and flow through the normal scope
	// check; optimize-mode briefs are already blocked from naming "__"
	// assets by the teambuild contract-side guard.
	if strings.HasPrefix(ref.Name, "__") {
		return fmt.Errorf("%w: %q", ErrWriteBuiltinAssetForbidden, ref.Name)
	}
	if g.validator == nil {
		return errors.New("write receipt gate unavailable")
	}
	if err := g.validator.ValidateReceipt(
		ctx, g.workspaceID, g.receipt, ref,
	); err != nil {
		return fmt.Errorf("%w: %v", ErrWriteReceiptRejected, err)
	}
	return nil
}

// authorizeCreate is the new-asset guard shared by the create tools
// (ticket T15C): a create call is denied unless the frozen scope explicitly
// allows new assets. Create-mode scopes declare a NamePrefix naming
// namespace; optimize-mode scopes pin exact existing refs and therefore
// never allow a create. Zero receipts (unit-test fakes) are structurally
// invalid and skip the guard; the production gate always re-validates the
// minted receipt before any write.
func (g *WriteGate) authorizeCreate() error {
	if g.receipt.Valid() && g.receipt.AssetScope().NamePrefix == "" {
		return errors.New(
			"create is not allowed: the build authorization scope does not allow new assets",
		)
	}
	return nil
}

// authorizeExactCreate permits one new asset whose kind and stable ID were
// explicitly frozen into the receipt. This is narrower than the create-mode
// NamePrefix grant and exists for optimize runs that introduce their team's
// deterministic first workflow.
func (g *WriteGate) authorizeExactCreate(ctx context.Context, ref teambuild.AssetRef) error {
	if !g.receipt.Valid() {
		return nil
	}
	scope := g.receipt.AssetScope()
	if scope.NamePrefix == "" {
		allowed := false
		for _, candidate := range scope.Refs {
			if candidate.Kind == ref.Kind && candidate.ID != "" && candidate.ID == ref.ID {
				allowed = true
				break
			}
		}
		if !allowed {
			return errors.New("create is not allowed: the build authorization scope does not name the new asset")
		}
	}
	return g.authorizeRef(ctx, ref)
}

// resolveAgentTarget loads one agent by its stable name or its stable ID
// (ticket T15C). The optimize loop's scope pins agents by stable ID, so the
// update/graph tools resolve the caller-supplied identity first and gate on
// AssetRef{ID+Name}; a name-only ref cannot match an ID-only scope entry.
func (g *WriteGate) resolveAgentTarget(
	ctx context.Context,
	nameOrID string,
) (*registry.AgentRecord, error) {
	if g.deps.AgentLoad == nil {
		return nil, errors.New("agent registry read is unavailable")
	}
	record, err := g.deps.AgentLoad.Get(ctx, g.workspaceID, nameOrID)
	if err == nil {
		return record, nil
	}
	if !isAgentNotFound(err) {
		return nil, err
	}
	agents, listErr := g.deps.AgentLoad.List(ctx, g.workspaceID)
	if listErr != nil {
		return nil, listErr
	}
	for i := range agents {
		if agents[i].ID == nameOrID {
			return &agents[i], nil
		}
	}
	return nil, err
}

// requireBuildRunContext binds the call envelope to the receipt's run. A
// zero receipt is skipped here on purpose: it is structurally invalid and is
// rejected by authorize before this check runs; only minted receipts bind a
// run, and for them an omitted build_run_id is filled from the binding while
// a provided id must match exactly.
func (g *WriteGate) requireBuildRunContext(rawArgs string) error {
	if g.receipt.BuildRunID() == "" {
		return nil
	}
	return requireBuildRunContextFor(rawArgs, g.receipt.BuildRunID())
}

// requireBuildRunContextFor is the pure envelope-binding rule, exposed for
// direct unit testing without a minted receipt. Context passing is the
// platform's responsibility: the call may omit build_run_id (it is bound by
// construction), but a provided id that contradicts the bound run is
// rejected.
func requireBuildRunContextFor(rawArgs, boundRunID string) error {
	runID, err := buildRunIDFromArgs(rawArgs)
	if err != nil {
		return err
	}
	if runID == "" {
		return nil
	}
	if runID != boundRunID {
		return fmt.Errorf(
			"%w: call carries %q, receipt is bound to %q",
			ErrWriteBuildRunMismatch, runID, boundRunID,
		)
	}
	return nil
}

// resolveBuildRunID fills the call envelope's build_run_id from the
// construction-bound receipt run when the caller omits it, and rejects a
// provided id that contradicts the bound run. Zero receipts bind nothing: a
// provided id is accepted and an omitted one stays required so unit-test
// fakes never guess a run.
func (g *WriteGate) resolveBuildRunID(rawArgs string) (string, error) {
	runID, err := buildRunIDFromArgs(rawArgs)
	if err != nil {
		return "", err
	}
	if runID == "" {
		if g.receipt.BuildRunID() == "" {
			return "", errors.New("build_run_id is required")
		}
		return g.receipt.BuildRunID(), nil
	}
	if err := g.requireBuildRunContext(rawArgs); err != nil {
		return "", err
	}
	return runID, nil
}

// writeTx orchestrates one atomic write: begin, mutate, commit. Any error
// from mutate (model resolution, PutTx, ...) rolls the whole transaction
// back, so a failed validation never lands a partial version.
func (g *WriteGate) writeTx(ctx context.Context, mutate func(ctx context.Context, tx pgx.Tx) error) error {
	if g.deps.Pool == nil {
		return errors.New("write transaction store unavailable")
	}
	tx, err := g.deps.Pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin write transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := mutate(ctx, tx); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit write transaction: %w", err)
	}
	return nil
}

// recordAudit writes one best-effort audit row for every write outcome
// (success and rejection alike), following the T02 convention.
func (g *WriteGate) recordAudit(
	ctx context.Context,
	call contract.ToolCall,
	result *contract.ToolResult,
	err error,
) {
	recordAudit(ctx, g.audit, g.workspaceID, g.agent, call, result, err, g.receipt.BuildRunID())
}

// validateWriteRecord runs the write-time validation stack shared by all
// assembly writers: engine name, engine/runtime binding, avatar
// capabilities, and the F13 CLI-vs-graph guard. All checks run before any
// version is committed; model resolution (F2) additionally runs inside the
// write tx right before PutTx.
func (g *WriteGate) validateWriteRecord(
	ctx context.Context,
	rec *registry.AgentRecord,
	existing *registry.AgentRecord,
	engineGiven bool,
) error {
	if err := validateEngineNameWrite(rec.Engine); err != nil {
		return err
	}
	if err := g.validateRuntimeBinding(ctx, rec); err != nil {
		return err
	}
	if err := validateAvatarCapabilitiesWrite(rec); err != nil {
		return err
	}
	if err := g.validateCLIGraphConflict(existing, rec, engineGiven); err != nil {
		return err
	}
	return nil
}

// validateAgentNameWrite enforces the platform agent-name contract:
// ^[a-z0-9][a-z0-9_-]*$ with a 64-character cap.
func validateAgentNameWrite(name string) error {
	if name == "" {
		return fmt.Errorf("%w: name is required", ErrWriteNameInvalid)
	}
	if len(name) > 64 || !agentNameRe.MatchString(name) {
		return fmt.Errorf(
			"%w: %q must match ^[a-z0-9][a-z0-9_-]*$ and be at most 64 characters",
			ErrWriteNameInvalid, name,
		)
	}
	return nil
}

// validateAgentRoleWrite enforces the worker|avatar role contract.
func validateAgentRoleWrite(role string) error {
	if role != "worker" && role != "avatar" {
		return fmt.Errorf("%w: got %q", ErrWriteRoleInvalid, role)
	}
	return nil
}

// validateEngineNameWrite enforces the pinned engine vocabulary
// (""/loom/opencode/codex/claude, internal/engine/engine.go).
func validateEngineNameWrite(engineName string) error {
	switch engineName {
	case "", "loom", engine.OpenCode, engine.Codex, engine.Claude:
		return nil
	default:
		return fmt.Errorf(
			"%w: %q must be one of \"\", loom, opencode, codex, claude",
			ErrWriteEngineInvalid, engineName,
		)
	}
}

// validateRuntimeBinding replicates the api/agents.go runtime contract:
// runtime_id is non-empty exactly when engine is a CLI engine, and the
// runtime must exist in this workspace. The api handlers are not importable,
// so the rule is re-implemented here against runtimes.Store.Get.
func (g *WriteGate) validateRuntimeBinding(ctx context.Context, rec *registry.AgentRecord) error {
	cli := engine.IsCLIEngine(rec.Engine)
	if rec.RuntimeID == "" && cli {
		return fmt.Errorf("%w: CLI engine %q requires runtime_id", ErrWriteRuntimeInvalid, rec.Engine)
	}
	if rec.RuntimeID != "" && !cli {
		return fmt.Errorf(
			"%w: runtime_id requires engine to be exactly claude, codex, or opencode",
			ErrWriteRuntimeInvalid,
		)
	}
	if rec.RuntimeID == "" {
		return nil
	}
	if g.deps.Runtimes == nil {
		return fmt.Errorf("%w: runtime store not configured", ErrWriteRuntimeInvalid)
	}
	if _, err := g.deps.Runtimes.Get(ctx, g.workspaceID, rec.RuntimeID); err != nil {
		if errors.Is(err, runtimes.ErrRuntimeUnavailable) {
			return fmt.Errorf(
				"%w: runtime %q does not exist in this workspace",
				ErrWriteRuntimeInvalid, rec.RuntimeID,
			)
		}
		return fmt.Errorf("validate runtime %q: %w", rec.RuntimeID, err)
	}
	return nil
}

// validateAvatarCapabilitiesWrite replicates the avatar constraint from
// api/agents.go: an avatar cannot define a custom execution graph nor
// configure sub-agents. Graph and sub-agent fields are outside the assembly
// whitelist, so this guard also protects API-created avatars during updates.
func validateAvatarCapabilitiesWrite(rec *registry.AgentRecord) error {
	if rec.Role != "avatar" {
		return nil
	}
	if rec.GraphDefinition != nil {
		return fmt.Errorf("%w: avatar cannot define a custom execution graph", ErrWriteAvatarInvalid)
	}
	if len(rec.SubAgents) > 0 {
		return fmt.Errorf("%w: avatar cannot configure sub-agents", ErrWriteAvatarInvalid)
	}
	return nil
}

// validateCLIGraphConflict is the F13 pre-gate: an agent that already carries
// a non-empty GraphDefinition must not be moved onto a CLI engine by an
// assembly patch. graph_definition itself is rejected at the parameter level
// (the employee internal-graph tools own it).
func (g *WriteGate) validateCLIGraphConflict(existing, rec *registry.AgentRecord, engineGiven bool) error {
	if !engineGiven || existing == nil || existing.GraphDefinition == nil {
		return nil
	}
	if engine.IsCLIEngine(rec.Engine) {
		return fmt.Errorf(
			"%w: agent %q has a non-empty graph_definition; engine %q cannot host an internal graph "+
				"(keep the loom engine or change the graph with the employee internal-graph tools)",
			ErrWriteCLIGraphConflict, rec.Name, rec.Engine,
		)
	}
	return nil
}

// resolveModel is the F2 write gate: every written version's model must
// resolve to a workspace ProviderRevision before PutTx lands it. The check
// runs inside the write tx so the provider head cannot be revoked between
// validation and commit.
func (g *WriteGate) resolveModel(ctx context.Context, tx pgx.Tx, rec *registry.AgentRecord) error {
	if rec.Model == "" {
		return nil
	}
	if g.deps.Models == nil {
		return errors.New("model resolver unavailable")
	}
	binding, err := g.deps.Models.ResolveModelRevisionTx(ctx, tx, g.workspaceID, rec.Model)
	if err != nil {
		return fmt.Errorf("%w: model %q: %v", ErrWriteModelUnresolvable, rec.Model, err)
	}
	if err := validateEngineModelBindingWrite(rec.Engine, binding); err != nil {
		return err
	}
	return nil
}

func validateEngineModelBindingWrite(engineName string, binding frozen.FrozenModelBinding) error {
	var requiredProvider string
	switch engineName {
	case engine.Codex, engine.OpenCode:
		requiredProvider = "system/openai"
	case engine.Claude:
		requiredProvider = "system/anthropic"
	default:
		return nil
	}
	if binding.ProviderID != requiredProvider {
		return fmt.Errorf(
			"%w: engine %q requires a model from provider %q, got model %q from provider %q; "+
				"read tf_list_capabilities.agent_model_policy and providers before retrying",
			ErrWriteModelEngineMismatch,
			engineName,
			requiredProvider,
			binding.ModelID,
			binding.ProviderID,
		)
	}
	return nil
}

// applyAgentContextDefaultsWrite replicates the platform context-management
// defaults (api/agents.go): compaction and memory are enabled unless the
// record explicitly configures them.
func applyAgentContextDefaultsWrite(rec *registry.AgentRecord) {
	if rec.Compaction == nil {
		rec.Compaction = registry.DefaultCompactionConfig()
	}
	if rec.MemoryConfig == nil {
		rec.MemoryConfig = registry.DefaultMemoryConfig()
	}
}

// isAgentNotFound reports the registry.Get missing-asset shape. registry.Get
// has no exported sentinel, so the teamforge loader treats its stable
// "not found for tenant" message as the missing-asset signal.
func isAgentNotFound(err error) bool {
	return err != nil && strings.Contains(err.Error(), "not found for tenant")
}

// toolError converts a rejection into the shared IsError ToolResult
// convention used by every write tool (and the read dispatcher).
func toolError(callID, content string) *contract.ToolResult {
	return &contract.ToolResult{CallID: callID, Content: content, IsError: true}
}

// recordAudit writes one best-effort audit row for every dispatch outcome
// (success and rejection alike). Detail follows the platform convention:
// status ok|error, redacted raw args, 200-rune truncation. Shared by the
// read dispatcher (T02) and every write tool.
func recordAudit(
	ctx context.Context,
	audit AuditRecorder,
	workspaceID, agent string,
	call contract.ToolCall,
	result *contract.ToolResult,
	err error,
	boundRunID string,
) {
	if audit == nil {
		return
	}
	status := "ok"
	detail := ""
	switch {
	case err != nil:
		status = "error"
		detail = err.Error()
	case result != nil && result.IsError:
		status = "error"
		detail = result.Content
	default:
		detail = "ok"
	}
	if runID, parseErr := buildRunIDFromArgs(call.Args); parseErr == nil {
		if runID == "" {
			// The call omitted build_run_id and was bound to the
			// construction-time run; record the actually-used id.
			runID = boundRunID
		}
		if runID != "" {
			detail = joinAuditDetail("build_run_id="+runID, detail)
		}
	}
	detail = sanitizeAuditDetail(detail, call.Args)
	if err := audit.Record(ctx, workspaceID, agent, call.Name, status, detail); err != nil {
		slog.Warn("teamforge audit record failed",
			"workspace_id", workspaceID,
			"agent", agent,
			"tool", call.Name,
			"error", err,
		)
	}
}
