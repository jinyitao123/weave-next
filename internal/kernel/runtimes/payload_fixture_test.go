package runtimes

import "github.com/jinyitao123/weave/internal/kernel/engine"

// CLIEngineExecResult builds the stored result of a completed CLI task for
// fixtures; production results arrive as runtime receipts.
func CLIEngineExecResult(result engine.RunResult) EngineExecResult {
	return EngineExecResult{
		SessionID:                result.SessionID,
		ArtifactCollection:       result.ArtifactCollection,
		Output:                   result.Output,
		ReportedModels:           append([]string(nil), result.ReportedModels...),
		RetrySafeBeforeExecution: result.RetrySafeBeforeExecution,
		Status:                   result.Status,
		Error:                    result.Err,
		UsageReceipt:             result.Usage,
		Diagnostics:              append([]engine.Diagnostic(nil), result.Diagnostics...),
		Events:                   append([]engine.Event(nil), result.Events...),
		Artifacts:                append([]engine.Artifact(nil), result.Artifacts...),
	}
}
