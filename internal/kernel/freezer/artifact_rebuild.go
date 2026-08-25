package freezer

import (
	"github.com/jinyitao123/weave/internal/kernel/compiler"
	"github.com/jinyitao123/weave/internal/base/frozen"
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
	if bundle.Agent.Model != "" {
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
