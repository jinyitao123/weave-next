// Package runtimebridge translates platform-owned task and registry records to
// the storage-free runtime Host protocol. It is the only place where those two
// sides are allowed to meet.
package runtimebridge

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jinyitao123/loom/contract"
	"github.com/jinyitao123/weave/internal/kernel/engine"
	"github.com/jinyitao123/weave/internal/kernel/execenv"
	"github.com/jinyitao123/weave/internal/kernel/runtimeprotocol"
	"github.com/jinyitao123/weave/internal/kernel/runtimes"
	"github.com/jinyitao123/weave/internal/kernel/taskqueue"
)

// Claim converts a claimed platform task and its already-authorized redacted
// payload. Token minting and queue ownership stay with the API adapter.
func Claim(task *taskqueue.Task, redacted json.RawMessage) (*runtimeprotocol.ExecutionClaim, error) {
	if task == nil || task.Kind != "engine_exec" || task.Status != taskqueue.StatusRunning || task.WorkerID == "" || task.LeaseExpiresAt == nil {
		return nil, errors.New("runtime bridge requires one active engine claim")
	}
	var request runtimes.EngineExecRequest
	if err := json.Unmarshal(redacted, &request); err != nil || request.Record == nil {
		return nil, errors.New("runtime bridge received an invalid redacted payload")
	}
	frozenAgent, err := json.Marshal(request.Record)
	if err != nil {
		return nil, err
	}
	claim := &runtimeprotocol.ExecutionClaim{
		SchemaVersion: runtimeprotocol.ClaimSchemaV1, TaskID: task.ID, WorkspaceID: task.WorkspaceID,
		Subject: task.Subject, ClaimEpoch: task.ClaimEpoch, LeaseIssuedAt: task.UpdatedAt,
		LeaseExpiresAt: *task.LeaseExpiresAt, DeadlineAt: task.DeadlineAt, RunSnapshotID: task.RunSnapshotID,
		Agent: runtimeprotocol.AgentIdentity{ID: task.AgentID, Version: task.AgentVersion, Name: task.Agent, ExecutionScope: task.ExecutionScope},
		Request: runtimeprotocol.ExecutionRequest{
			SchemaVersion: runtimeprotocol.RequestSchemaV1, Engine: runtimes.CanonicalEngine(request.Engine), Model: request.Model,
			Prompt: request.Prompt, OutputSchema: request.OutputSchema, FrozenAgent: frozenAgent, FrozenAgentHash: fmt.Sprintf("%x", sha256.Sum256(frozenAgent)),
			LogicalInvocationID: request.LogicalInvocationID, NodeID: request.NodeID, TimeoutSeconds: request.TimeoutSeconds,
			EngineVersion: request.EngineVersion, Attachments: attachments(request.Attachments), InputFiles: inputFiles(request.InputFiles),
			TaskMCP: taskMCP(request.TaskMCP), Loom: loomInput(request.Loom),
		},
	}
	if err := claim.Validate(); err != nil {
		return nil, err
	}
	return claim, nil
}

// Result converts a validated wire receipt to the platform's current result
// carrier. The API adapter must call receipt.ValidateFor before accepting it.
func Result(receipt runtimeprotocol.ExecutionReceipt) runtimes.EngineExecResult {
	return runtimes.EngineExecResult{
		ClaimEpoch: receipt.ClaimEpoch, Subject: receipt.Subject, SessionID: receipt.SessionID,
		ArtifactCollection: receipt.ArtifactCollection, Output: receipt.Output, RetrySafeBeforeExecution: receipt.RetrySafeBeforeExecution,
		ReportedModels: append([]string(nil), receipt.ReportedModels...), StopReason: receipt.StopReason, Usage: receipt.Usage,
		RunID: receipt.RunID, Status: receipt.Status, Error: receipt.Error, UsageReceipt: receipt.UsageReceipt,
		Diagnostics: append([]engine.Diagnostic(nil), receipt.Diagnostics...),
		Events:      append([]engine.Event(nil), receipt.Events...), Artifacts: append([]engine.Artifact(nil), receipt.Artifacts...),
	}
}

func attachments(values []runtimes.EngineExecAttachment) []runtimeprotocol.Attachment {
	result := make([]runtimeprotocol.Attachment, len(values))
	for i, value := range values {
		result[i] = runtimeprotocol.Attachment{ID: value.ID, Filename: value.Filename}
	}
	return result
}

func inputFiles(values []runtimes.InputFile) []runtimeprotocol.InputFile {
	result := make([]runtimeprotocol.InputFile, len(values))
	for i, value := range values {
		result[i] = runtimeprotocol.InputFile{TaskID: value.TaskID, NodeID: value.NodeID, Path: value.Path, ContentType: value.ContentType, SHA256: value.SHA256, Content: value.Content}
	}
	return result
}

func taskMCP(values []execenv.TaskMCPTarget) []runtimeprotocol.TaskMCPTarget {
	result := make([]runtimeprotocol.TaskMCPTarget, len(values))
	for i, value := range values {
		result[i] = runtimeprotocol.TaskMCPTarget{URL: value.URL, Token: value.Token}
	}
	return result
}

func loomInput(value *runtimes.LoomExecInput) *runtimeprotocol.LoomInput {
	if value == nil {
		return nil
	}
	return &runtimeprotocol.LoomInput{Messages: append([]contract.Message(nil), value.Messages...), LastUserMessage: value.LastUserMessage,
		SessionID: value.SessionID, ConversationID: value.ConversationID, Profile: value.Profile, Effort: value.Effort,
		Context: value.Context, NoDispatch: value.NoDispatch, MCPServerCount: value.MCPServerCount}
}
