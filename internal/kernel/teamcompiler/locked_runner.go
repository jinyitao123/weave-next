package teamcompiler

import (
	"context"
	"errors"

	"github.com/jinyitao123/loom"
	"github.com/jinyitao123/loom/stdlib"
	"github.com/jinyitao123/weave/internal/kernel/compiler"
	"github.com/jinyitao123/weave/internal/base/frozen"
)

type lockedWorkerKey struct {
	agentID string
	version int64
}

type frozenLockedWorkerRunner struct {
	workspaceID   string
	runSnapshotID string
	workers       map[lockedWorkerKey]FrozenTeamWorker
	bundles       map[lockedWorkerKey]frozen.FrozenExecutionBundle
	resolver      *closureFrozenResolver
	buildOpts     compiler.FrozenBuildOpts
}

func NewFrozenLockedWorkerRunner(closure FrozenWorkerClosure) (LockedWorkerRunner, error) {
	if closure.WorkspaceID == "" || closure.RunSnapshotID == "" ||
		isNilValue(closure.BuildOpts.CheckpointStore) {
		return nil, ErrTeamCompileInputInvalid
	}
	runner := &frozenLockedWorkerRunner{
		workspaceID:   closure.WorkspaceID,
		runSnapshotID: closure.RunSnapshotID,
		workers:       make(map[lockedWorkerKey]FrozenTeamWorker, len(closure.Workers)),
		bundles:       make(map[lockedWorkerKey]frozen.FrozenExecutionBundle, len(closure.Bundles)),
		buildOpts:     closure.BuildOpts,
	}
	for _, worker := range closure.Workers {
		if !worker.EnabledAtSnapshot {
			continue
		}
		key := lockedWorkerKey{
			agentID: worker.WorkerAgentID,
			version: worker.WorkerAgentVersion,
		}
		if key.agentID == "" || key.version < 1 {
			return nil, ErrTeamCompileInputInvalid
		}
		if _, duplicate := runner.workers[key]; duplicate {
			return nil, ErrTeamCompileInputInvalid
		}
		runner.workers[key] = worker
	}
	for _, bundle := range closure.Bundles {
		key := lockedWorkerKey{
			agentID: bundle.Agent.AgentID,
			version: bundle.Agent.AgentVersion,
		}
		if bundle.Agent.WorkspaceID != closure.WorkspaceID ||
			key.agentID == "" || key.version < 1 {
			return nil, ErrTeamWorkerVersionUnavailable
		}
		if _, duplicate := runner.bundles[key]; duplicate {
			return nil, ErrTeamWorkerVersionUnavailable
		}
		runner.bundles[key] = bundle
	}
	runner.resolver = newClosureFrozenResolver(closure.Bundles)
	return runner, nil
}

func (r *frozenLockedWorkerRunner) StepFor(
	ctx context.Context,
	ref LockedWorkerRef,
) (loom.Step, error) {
	if r == nil {
		return nil, ErrTeamLiveResolutionForbidden
	}
	if ref.WorkerAgentID == "" || ref.WorkerAgentVersion < 1 {
		return nil, ErrTeamLiveResolutionForbidden
	}
	if ref.WorkspaceID != r.workspaceID {
		return nil, ErrTeamCompileIdentityMismatch
	}
	if ref.RunSnapshotID != r.runSnapshotID {
		return nil, ErrTeamInteractionSnapshotMismatch
	}
	key := lockedWorkerKey{
		agentID: ref.WorkerAgentID,
		version: ref.WorkerAgentVersion,
	}
	worker, workerExists := r.workers[key]
	bundle, bundleExists := r.bundles[key]
	if !workerExists || !bundleExists {
		return nil, ErrTeamWorkerVersionUnavailable
	}
	if err := ValidateWorkerRoleProof(worker, bundle); err != nil {
		return nil, err
	}
	if bundle.Capability.MayInvokeAgent || len(bundle.Capability.AgentStepIDs) != 0 {
		return nil, ErrTeamWorkerGraphIncompatible
	}
	buildOpts := r.buildOpts
	buildOpts.Tools = NewLockedWorkerDispatcher(buildOpts.Tools)
	graph, err := compiler.CompileFrozen(ctx, bundle, r.resolver, buildOpts)
	if err != nil {
		return nil, normalizeLockedWorkerCompileError(err)
	}
	if graph == nil {
		return nil, ErrTeamWorkerFactoryIncompatible
	}
	return stdlib.NewSubGraphStep(graph, buildOpts.CheckpointStore), nil
}

func normalizeLockedWorkerCompileError(err error) error {
	switch {
	case errors.Is(err, compiler.ErrFrozenDependencyUndeclared):
		return codedError(CodeTeamFrozenDependencyUndeclared, err)
	case errors.Is(err, compiler.ErrFactoryUnknown),
		errors.Is(err, compiler.ErrFactoryAmbiguous),
		errors.Is(err, compiler.ErrFactoryABIIncompatible),
		errors.Is(err, compiler.ErrDependencyUnenumerable),
		errors.Is(err, compiler.ErrFactoryCompileFailed),
		errors.Is(err, compiler.ErrFrozenManifestMismatch),
		errors.Is(err, compiler.ErrFrozenCapabilityMismatch):
		return codedError(CodeTeamWorkerFactoryIncompatible, err)
	default:
		return codedError(CodeTeamWorkerFactoryIncompatible, err)
	}
}

type closureFrozenResolver struct {
	bundles []frozen.FrozenExecutionBundle
}

func newClosureFrozenResolver(
	bundles []frozen.FrozenExecutionBundle,
) *closureFrozenResolver {
	return &closureFrozenResolver{
		bundles: append([]frozen.FrozenExecutionBundle(nil), bundles...),
	}
}

func (r *closureFrozenResolver) Agent(
	_ context.Context,
	agentID string,
	version int64,
) (frozen.FrozenAgentRecord, error) {
	for _, bundle := range r.bundles {
		if bundle.Agent.AgentID == agentID && bundle.Agent.AgentVersion == version {
			return bundle.Agent, nil
		}
	}
	return frozen.FrozenAgentRecord{}, compiler.ErrFrozenDependencyUndeclared
}

func (r *closureFrozenResolver) Skill(
	_ context.Context,
	ref frozen.FrozenDependencyRef,
) (frozen.FrozenSkill, error) {
	for _, bundle := range r.bundles {
		for _, value := range bundle.Skills {
			if value.ContentHash == ref.ContentHash &&
				value.WorkspaceID == ref.WorkspaceID {
				return value, nil
			}
		}
	}
	return frozen.FrozenSkill{}, compiler.ErrFrozenDependencyUndeclared
}

func (r *closureFrozenResolver) MCPBinding(
	_ context.Context,
	ref frozen.FrozenDependencyRef,
) (frozen.FrozenMCPBinding, error) {
	for _, bundle := range r.bundles {
		for _, value := range bundle.MCPBindings {
			if value.ContentHash == ref.ContentHash &&
				value.WorkspaceID == ref.WorkspaceID {
				return value, nil
			}
		}
	}
	return frozen.FrozenMCPBinding{}, compiler.ErrFrozenDependencyUndeclared
}

func (r *closureFrozenResolver) ModelBinding(
	_ context.Context,
	ref frozen.FrozenDependencyRef,
) (frozen.FrozenModelBinding, error) {
	for _, bundle := range r.bundles {
		values := append(
			[]frozen.FrozenModelBinding{bundle.PrimaryModel},
			bundle.FallbackModels...,
		)
		for _, value := range values {
			if value.ContentHash == ref.ContentHash &&
				value.WorkspaceID == ref.WorkspaceID {
				return value, nil
			}
		}
	}
	return frozen.FrozenModelBinding{}, compiler.ErrFrozenDependencyUndeclared
}

func (r *closureFrozenResolver) RuntimeBinding(
	_ context.Context,
	ref frozen.FrozenDependencyRef,
) (frozen.FrozenRuntimeBinding, error) {
	for _, bundle := range r.bundles {
		if bundle.Runtime != nil &&
			bundle.Runtime.ContentHash == ref.ContentHash &&
			bundle.Runtime.WorkspaceID == ref.WorkspaceID {
			return *bundle.Runtime, nil
		}
	}
	return frozen.FrozenRuntimeBinding{}, compiler.ErrFrozenDependencyUndeclared
}

func (r *closureFrozenResolver) DeliveryTarget(
	_ context.Context,
	_ frozen.FrozenDependencyRef,
) (frozen.FrozenDeliveryTarget, error) {
	return frozen.FrozenDeliveryTarget{}, compiler.ErrFrozenDependencyUndeclared
}

var _ LockedWorkerRunner = (*frozenLockedWorkerRunner)(nil)
var _ compiler.FrozenResolver = (*closureFrozenResolver)(nil)
