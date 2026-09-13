package capabilityruntime

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"

	"github.com/jinyitao123/weave/internal/kernel/capability"
	"github.com/jinyitao123/weave/internal/kernel/registry"
)

func (r *Runner) ValidateRuntime(ctx context.Context, workspace string, requirement capability.RuntimeRequirement) error {
	engine := strings.TrimSpace(requirement.Engine)
	if engine == "" || engine == "loom" {
		if r.CanResolveModel == nil || requirement.Model == "" {
			return errors.New("capability_model_unavailable")
		}
		available, err := r.CanResolveModel(ctx, workspace, requirement.Model)
		if err != nil || !available {
			return errors.New("capability_model_unavailable")
		}
		return nil
	}
	if engine != "codex" && engine != "claude" && engine != "opencode" {
		return errors.New("capability_runtime_unsupported")
	}
	if r.ListRuntimes == nil || r.Remote == nil {
		return errors.New("capability_runtime_unavailable")
	}
	_, err := r.SelectRuntime(ctx, workspace, requirement)
	if err != nil {
		return errors.New("capability_runtime_unavailable")
	}
	return nil
}

func (r *Runner) SelectRuntime(ctx context.Context, workspace string, requirement capability.RuntimeRequirement) (*registry.AgentRecord, error) {
	if r.ListRuntimes == nil {
		return nil, errors.New("capability_runtime_unavailable")
	}
	items, err := r.ListRuntimes(ctx, workspace)
	if err != nil {
		return nil, err
	}
	for _, item := range items {
		if !item.Enabled || !item.Online || item.RevokedAt != nil || item.HealthStatus == "quarantined" || item.ActiveSlots >= item.TotalSlots {
			continue
		}
		if requirement.Pool != "" && item.PoolID != requirement.Pool && item.ID != requirement.Pool {
			continue
		}
		available := false
		for _, engine := range item.Engines {
			if engine == requirement.Engine {
				available = true
				break
			}
		}
		if !available {
			continue
		}
		sum := sha256.Sum256([]byte(workspace + "\x00" + item.ID + "\x00" + requirement.Engine))
		return &registry.AgentRecord{ID: fmt.Sprintf("capability-runtime-%x", sum[:12]), Version: 1, Name: fmt.Sprintf("capability-%x", sum[:8]), WorkspaceID: workspace, Engine: requirement.Engine, Model: requirement.Model, RuntimeID: item.ID, RuntimePolicyMode: "strict_pin", RuntimePoolID: item.PoolID, Role: "worker", Permissions: registry.PermissionConfig{Allow: []string{"*"}}}, nil
	}
	return nil, errors.New("no eligible capability runtime")
}
