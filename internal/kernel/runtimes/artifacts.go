package runtimes

import (
	"github.com/jinyitao123/weave/internal/kernel/engine"
	"github.com/jinyitao123/weave/internal/kernel/runtimehost"
)

type OutputArtifactSnapshot = runtimehost.OutputArtifactSnapshot

func SnapshotOutputArtifacts(workDir string) OutputArtifactSnapshot {
	return runtimehost.SnapshotOutputArtifacts(workDir)
}

func CollectOutputArtifacts(workDir string) []engine.Artifact {
	return runtimehost.CollectOutputArtifacts(workDir)
}

func CollectOutputArtifactsSince(workDir string, before OutputArtifactSnapshot, finalAnswer ...string) []engine.Artifact {
	return runtimehost.CollectOutputArtifactsSince(workDir, before, finalAnswer...)
}

func CollectRunOutputArtifacts(workDir string, before OutputArtifactSnapshot, result *engine.RunResult) error {
	return runtimehost.CollectRunOutputArtifacts(workDir, before, result)
}
