package teamforge

// Draft-phase declarative factory identity compatibility surface (校验器复用
// 纪律). The stand-in descriptor and the shared descriptor registry live in
// internal/teameval/workflow_validate.go; this file only re-exports the
// factory key so the golden test keeps pinning the platform identity.

import "github.com/jinyitao123/weave/internal/base/frozen"

// Declarative factory identity constants mirror
// internal/declarative/frozen_factory.go and stay in lockstep with
// teameval.DeclarativeFactoryKey.
const (
	declarativeFactoryID      = "declarative"
	declarativeFactoryVersion = "1"
	declarativeCompilerABI    = "weave-graph-abi-v1"
	declarativeSchemaID       = "weave-declarative-factory-input"
)

var declarativeFactoryKey = frozen.FactoryKey{
	FactoryID:      declarativeFactoryID,
	FactoryVersion: declarativeFactoryVersion,
	CompilerABI:    declarativeCompilerABI,
}
