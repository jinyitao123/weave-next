package runtimes

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jinyitao123/weave/internal/base/execution"
	"github.com/jinyitao123/weave/internal/kernel/config"
	"github.com/jinyitao123/weave/internal/kernel/engine"
	"github.com/jinyitao123/weave/internal/kernel/execenv"
	"github.com/jinyitao123/weave/internal/kernel/execspec"
	"github.com/jinyitao123/weave/internal/kernel/registry"
	"github.com/jinyitao123/weave/internal/kernel/secret"
)

// Keep local CLI execution aligned with remote engine tasks and TeamRun agent
// nodes so deployment topology does not change the effective timeout contract.
const localEngineExecTimeout = 45 * time.Minute

// LocalExecutor executes an external CLI engine in the server's local runtime.
type LocalExecutor struct {
	workspacesRoot string
	oneapiBase     string
	boundaryBase   string
	oneapiKey      string
}

// NewLocalExecutor creates a local CLI engine executor.
func NewLocalExecutor(workspacesRoot, oneapiBase, boundaryBase, oneapiKey string) *LocalExecutor {
	return &LocalExecutor{
		workspacesRoot: workspacesRoot,
		oneapiBase:     oneapiBase,
		boundaryBase:   boundaryBase,
		oneapiKey:      oneapiKey,
	}
}

// ExecRemote executes an already-resolved CLI agent record locally. Its method
// shape matches the remote executor so callers can select either execution path.
func (e *LocalExecutor) ExecRemote(
	ctx context.Context,
	tenant string,
	rec *registry.AgentRecord,
	stamp execution.AgentExecutionStamp,
	prompt string,
	attachments []execspec.Attachment,
) (engine.RunResult, error) {
	if err := validateAgentExecutionStamp(tenant, rec, stamp); err != nil {
		return engine.RunResult{}, fmt.Errorf("local engine executor: %w", err)
	}
	subject, err := execution.RequireSubject(ctx, tenant)
	if err != nil {
		return engine.RunResult{}, err
	}
	workDir, menv, err := execenv.Materialize(ctx, e.workspacesRoot, rec, prompt, attachments)
	if err != nil {
		return engine.RunResult{}, err
	}
	if err := execenv.WriteEngineConfig(rec.Engine, workDir, rec, e.oneapiBase, e.boundaryBase, e.oneapiKey); err != nil {
		return engine.RunResult{}, err
	}
	if menv == nil {
		menv = make(map[string]string)
	}
	menv["OPENAI_BASE_URL"] = e.oneapiBase
	menv["OPENAI_API_KEY"] = e.oneapiKey
	menv["ONEAPI_API_KEY"] = e.oneapiKey
	for idx := range rec.MCPServers {
		if token := secret.BoundaryToken(tenant, rec.Name, idx); token != "" {
			menv[fmt.Sprintf("WEAVE_MCP_BOUNDARY_TOKEN_%d", idx)] = token
		}
	}

	cliPath := config.ResolveEngineCLIPath(rec.Engine)
	backend, err := engine.New(rec.Engine, cliPath)
	if err != nil {
		return engine.RunResult{}, fmt.Errorf("engine %q: %v", rec.Engine, err)
	}
	outputsBefore := SnapshotOutputArtifacts(workDir)
	result, runErr := backend.Run(ctx, engine.RunSpec{
		Subject:       subject,
		WorkDir:       workDir,
		Prompt:        promptWithAttachmentNotice(prompt, attachments),
		Model:         rec.Model,
		Env:           menv,
		Timeout:       localEngineExecTimeout,
		EngineVersion: engine.BinaryVersion(ctx, cliPath),
	})
	// Keep files produced before a failed local engine invocation available for
	// diagnosis. Callers retain the failed status and must not treat them as final.
	runErr = errors.Join(runErr, CollectRunOutputArtifacts(workDir, outputsBefore, &result))
	if result.Status != "completed" {
		if runErr != nil {
			return result, runErr
		}
		return result, errors.New(result.Err)
	}
	return result, runErr
}
