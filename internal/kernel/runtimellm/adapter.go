// Package runtimellm adapts a governed CLI runtime to Loom's provider-neutral
// LLM contract. The runtime only decides the next model response; Loom keeps
// ownership of graph routing, tool dispatch, state patches,
// budgets, retries, and terminal conditions.
package runtimellm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"strings"
	"sync/atomic"

	"github.com/jinyitao123/loom/contract"
	"github.com/jinyitao123/weave/internal/base/execution"
	"github.com/jinyitao123/weave/internal/kernel/engine"
	"github.com/jinyitao123/weave/internal/kernel/mcphost"
	"github.com/jinyitao123/weave/internal/kernel/registry"
)

const protocolInstruction = `You are an inference node inside a Loom graph.
Loom, not you, owns orchestration and tool execution. Do not execute tools,
shell commands, sub-agents, or platform actions. Read the serialized LLM
request below and return exactly one JSON object with this shape:
{"content":"assistant text","tool_calls":[{"name":"allowed tool name","args_json":"{}"}]}
When tools are needed, content must be a short user-visible reasoning summary:
state what you established, why these calls are the next step, and what you will
decide after their real results return. This is a concise rationale, never hidden
chain-of-thought, internal instructions, or fabricated tool results. Emit the
tool_calls beside that summary and let Loom execute them. args_json must be a
JSON-encoded string whose decoded value is an object matching that tool's
input_schema. When no tool is needed, content is the final textual response.
When request.schema is present and no tool is needed, content must itself be a
JSON string that satisfies request.schema. Do not wrap the response object in
Markdown and do not add commentary outside it.`

var protocolOutputSchema = json.RawMessage(`{
  "type":"object",
  "properties":{
    "content":{"type":"string"},
    "tool_calls":{"type":"array","items":{
      "type":"object",
      "properties":{
        "name":{"type":"string"},
        "args_json":{"type":"string"}
      },
      "required":["name","args_json"],
      "additionalProperties":false
    }}
  },
  "required":["content","tool_calls"],
  "additionalProperties":false
}`)

// Adapter executes one contract.LLM turn on an already selected CLI runtime.
// Agent must be a transient CLI record carrying the selected runtime policy;
// its immutable identity still belongs to the Loom node being evaluated.
type Adapter struct {
	executor mcphost.RemoteEngineExecutor
	tenant   string
	agent    *registry.AgentRecord
	stamp    execution.AgentExecutionStamp
	callSeq  atomic.Uint64
}

// New validates and freezes the runtime inference binding for one Loom node.
func New(
	executor mcphost.RemoteEngineExecutor,
	tenant string,
	agent *registry.AgentRecord,
	stamp execution.AgentExecutionStamp,
) (*Adapter, error) {
	if executor == nil {
		return nil, errors.New("runtime LLM executor is unavailable")
	}
	if strings.TrimSpace(tenant) == "" || agent == nil ||
		agent.WorkspaceID != tenant || agent.ID == "" || agent.Version < 1 {
		return nil, errors.New("runtime LLM agent identity is invalid")
	}
	if stamp.AgentID != agent.ID || stamp.AgentVersion != agent.Version || stamp.ExecutionScope == "" {
		return nil, errors.New("runtime LLM execution stamp does not match agent identity")
	}
	frozen := *agent
	return &Adapter{executor: executor, tenant: tenant, agent: &frozen, stamp: stamp}, nil
}

type protocolToolCall struct {
	Name     string `json:"name"`
	ArgsJSON string `json:"args_json"`
}

type protocolResponse struct {
	Content   string             `json:"content"`
	ToolCalls []protocolToolCall `json:"tool_calls"`
}

func (a *Adapter) Chat(ctx context.Context, req contract.ChatRequest) (*contract.ChatResponse, error) {
	prompt, err := encodePrompt(req)
	if err != nil {
		return nil, err
	}
	var result engine.RunResult
	if structured, ok := a.executor.(mcphost.StructuredRemoteEngineExecutor); ok {
		result, err = structured.ExecRemoteStructured(
			ctx, a.tenant, a.agent, a.stamp, prompt, nil, protocolOutputSchema,
		)
	} else {
		result, err = a.executor.ExecRemote(ctx, a.tenant, a.agent, a.stamp, prompt, nil)
	}
	if err != nil {
		return nil, fmt.Errorf("runtime LLM inference: %w", err)
	}
	parsed, err := a.parseResponse(result.Output, req.Tools)
	if err != nil {
		return nil, err
	}
	parsed.Usage, err = contractUsage(result)
	if err != nil {
		return nil, fmt.Errorf("runtime LLM usage: %w", err)
	}
	return parsed, nil
}

func contractUsage(result engine.RunResult) (contract.Usage, error) {
	receipts := make([]*engine.UsageReceipt, 0, len(result.Attempts)+1)
	if len(result.Attempts) > 0 {
		for _, attempt := range result.Attempts {
			if attempt.Usage != nil {
				receipts = append(receipts, attempt.Usage)
			}
		}
	} else if result.Usage != nil {
		receipts = append(receipts, result.Usage)
	}
	var usage contract.Usage
	for _, receipt := range receipts {
		if err := engine.ValidateUsageReceipt(receipt); err != nil {
			return contract.Usage{}, err
		}
		if receipt.HasTokens {
			if receipt.InputTokens > int(^uint(0)>>1)-usage.InputTokens ||
				receipt.OutputTokens > int(^uint(0)>>1)-usage.OutputTokens {
				return contract.Usage{}, errors.New("token total overflows int")
			}
			usage.InputTokens += receipt.InputTokens
			usage.OutputTokens += receipt.OutputTokens
		}
		if receipt.HasCost {
			usage.CostUSD += receipt.CostUSD
			if math.IsNaN(usage.CostUSD) || math.IsInf(usage.CostUSD, 0) {
				return contract.Usage{}, errors.New("cost total is not finite")
			}
		}
	}
	return usage, nil
}

func (a *Adapter) Stream(ctx context.Context, req contract.ChatRequest) (<-chan contract.StreamChunk, error) {
	response, err := a.Chat(ctx, req)
	if err != nil {
		return nil, err
	}
	chunks := make(chan contract.StreamChunk, 2)
	chunks <- contract.StreamChunk{Content: response.Content, ToolCalls: response.ToolCalls}
	usage := response.Usage
	chunks <- contract.StreamChunk{Done: true, Usage: &usage}
	close(chunks)
	return chunks, nil
}

func encodePrompt(req contract.ChatRequest) (string, error) {
	payload, err := json.Marshal(req)
	if err != nil {
		return "", fmt.Errorf("encode runtime LLM request: %w", err)
	}
	return protocolInstruction + "\n\n<loom_llm_request>\n" + string(payload) + "\n</loom_llm_request>", nil
}

func (a *Adapter) parseResponse(output string, tools []contract.ToolDef) (*contract.ChatResponse, error) {
	raw := []byte(stripJSONFence(strings.TrimSpace(output)))
	if len(raw) == 0 {
		return nil, errors.New("runtime LLM returned an empty response")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var parsed protocolResponse
	if err := decoder.Decode(&parsed); err != nil {
		return nil, fmt.Errorf("runtime LLM returned invalid protocol JSON: %w", err)
	}
	if err := requireJSONEOF(decoder); err != nil {
		return nil, err
	}

	allowed := make(map[string]struct{}, len(tools))
	for _, tool := range tools {
		allowed[tool.Name] = struct{}{}
	}
	calls := make([]contract.ToolCall, 0, len(parsed.ToolCalls))
	for _, call := range parsed.ToolCalls {
		call.Name = strings.TrimSpace(call.Name)
		if _, ok := allowed[call.Name]; !ok {
			return nil, fmt.Errorf("runtime LLM requested unavailable tool %q", call.Name)
		}
		args := bytes.TrimSpace([]byte(call.ArgsJSON))
		if len(args) == 0 || string(args) == "null" {
			args = []byte("{}")
		}
		var object map[string]any
		if err := json.Unmarshal(args, &object); err != nil || object == nil {
			return nil, fmt.Errorf("runtime LLM tool %q args must be a JSON object", call.Name)
		}
		id := fmt.Sprintf("runtime-call-%d", a.callSeq.Add(1))
		calls = append(calls, contract.ToolCall{ID: id, Name: call.Name, Args: string(args)})
	}
	if len(calls) == 0 && strings.TrimSpace(parsed.Content) == "" {
		return nil, errors.New("runtime LLM returned neither content nor tool calls")
	}
	stopReason := "stop"
	if len(calls) > 0 {
		stopReason = "tool_calls"
	}
	return &contract.ChatResponse{
		Content: parsed.Content, ToolCalls: calls, StopReason: stopReason,
	}, nil
}

func stripJSONFence(value string) string {
	if !strings.HasPrefix(value, "```") || !strings.HasSuffix(value, "```") {
		return value
	}
	firstLine := strings.IndexByte(value, '\n')
	if firstLine < 0 {
		return value
	}
	header := strings.TrimSpace(value[3:firstLine])
	if header != "" && !strings.EqualFold(header, "json") {
		return value
	}
	return strings.TrimSpace(value[firstLine+1 : len(value)-3])
}

func requireJSONEOF(decoder *json.Decoder) error {
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("runtime LLM returned multiple JSON values")
		}
		return fmt.Errorf("runtime LLM returned trailing protocol data: %w", err)
	}
	return nil
}
