// Package teamassets implements atomic product asset commands for team builders.
package teamassets

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jinyitao123/weave/internal/app/agentcatalog"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jinyitao123/weave/internal/base/frozen"
	"github.com/jinyitao123/weave/internal/build/teamforge"
	"github.com/jinyitao123/weave/internal/kernel/credentials"
	"github.com/jinyitao123/weave/internal/kernel/engine"
)

// AgentWriter owns provider resolution and the immutable agent write transaction.
// Neither the builder nor a tool dispatcher can commit only half of the command.
type AgentWriter struct {
	Pool     *pgxpool.Pool
	Registry *agentcatalog.AgentRegistry
}

var _ teamforge.AgentWriter = (*AgentWriter)(nil)

func (w *AgentWriter) CommitAgent(ctx context.Context, request teamforge.AgentWriteRequest) (teamforge.AgentWriteResult, error) {
	if w == nil || w.Pool == nil || w.Registry == nil {
		return teamforge.AgentWriteResult{}, errors.New("agent registry write is unavailable")
	}
	tx, err := w.Pool.Begin(ctx)
	if err != nil {
		return teamforge.AgentWriteResult{}, fmt.Errorf("begin write transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	record := request.Record
	if request.InternalGraph && engine.IsCLIEngine(record.Engine) {
		return teamforge.AgentWriteResult{}, fmt.Errorf("%w: agent %q is on engine %q; employee internal graphs require the loom engine", teamforge.ErrWriteCLIGraphConflict, record.Name, record.Engine)
	}
	if record.Model != "" {
		binding, err := credentials.ResolveModelRevisionTx(ctx, tx, request.WorkspaceID, record.Model)
		if err != nil {
			return teamforge.AgentWriteResult{}, fmt.Errorf("%w: model %q: %v", teamforge.ErrWriteModelUnresolvable, record.Model, err)
		}
		if err := validateEngineModelBinding(record.Engine, binding); err != nil {
			return teamforge.AgentWriteResult{}, err
		}
	}
	if err := w.Registry.PutTx(ctx, tx, request.WorkspaceID, &record); err != nil {
		return teamforge.AgentWriteResult{}, err
	}
	encoded, err := json.Marshal(record)
	if err != nil {
		return teamforge.AgentWriteResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return teamforge.AgentWriteResult{}, fmt.Errorf("commit write transaction: %w", err)
	}
	return teamforge.AgentWriteResult{Record: record, JSON: encoded}, nil
}

func validateEngineModelBinding(engineName string, binding frozen.FrozenModelBinding) error {
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
		return fmt.Errorf("%w: engine %q requires a model from provider %q, got model %q from provider %q; read tf_list_capabilities.agent_model_policy and providers before retrying", teamforge.ErrWriteModelEngineMismatch, engineName, requiredProvider, binding.ModelID, binding.ProviderID)
	}
	return nil
}
