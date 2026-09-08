package freezer

import (
	"github.com/jinyitao123/weave/internal/base/frozen"
	"github.com/jinyitao123/weave/internal/kernel/compiler"
)

// RebuildArtifactResolver reconstructs a resolver from one frozen bundle.
func RebuildArtifactResolver(
	bundle frozen.FrozenExecutionBundle,
) (compiler.FrozenResolver, error) {
	set := ArtifactDependencySet{
		Agents:      []frozen.FrozenAgentRecord{bundle.Agent},
		Skills:      bundle.Skills,
		MCPBindings: bundle.MCPBindings,
		ModelBindings: append(
			[]frozen.FrozenModelBinding(nil),
			bundle.FallbackModels...,
		),
	}
	// CLI model names are native runtime hints, just as in the standard frozen
	// enumerator. They do not create workspace provider-model dependencies.
	cli := bundle.Agent.Engine != "" && bundle.Agent.Engine != "loom"
	if cli && (bundle.PrimaryModel != (frozen.FrozenModelBinding{}) || len(bundle.FallbackModels) != 0) {
		return nil, newError(CodeFrozenManifestMismatch, nil)
	}
	if !cli && bundle.Agent.Model != "" {
		set.ModelBindings = append(
			[]frozen.FrozenModelBinding{bundle.PrimaryModel},
			set.ModelBindings...,
		)
	}
	if bundle.Runtime != nil {
		set.RuntimeBindings = []frozen.FrozenRuntimeBinding{*bundle.Runtime}
	}
	return NewArtifactResolver(bundle.Dependencies, set)
}
