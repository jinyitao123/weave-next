package llmrouter

import (
	"crypto/sha256"
	"encoding/hex"

	"github.com/jinyitao123/loom/contract"
	"github.com/jinyitao123/weave/internal/base/frozen"
)

const protocolToolLimit = 16

// ModelProtocolObservation is the bounded host view of provider protocol facts.
// Content, headers and argument text are excluded; strings are enums/digests.
type ModelProtocolObservation struct {
	Version            int                       `json:"version"`
	Protocol           string                    `json:"protocol"`
	Complete           bool                      `json:"complete"`
	End                string                    `json:"end"`
	RequestSent        bool                      `json:"request_sent"`
	RequestToolCount   int                       `json:"request_tool_count"`
	RequestToolsSHA256 string                    `json:"request_tools_sha256,omitempty"`
	DataFrames         int                       `json:"data_frames"`
	ParseFailures      int                       `json:"parse_failures"`
	ToolDeltas         int                       `json:"tool_deltas"`
	ToolIndexes        []int                     `json:"tool_indexes"`
	AssembledCalls     int                       `json:"assembled_calls"`
	EmittedCalls       int                       `json:"emitted_calls"`
	FinishReason       string                    `json:"finish_reason,omitempty"`
	DoneSeen           bool                      `json:"done_seen"`
	Arguments          []ToolArgumentObservation `json:"arguments"`
	Truncated          bool                      `json:"truncated"`
	// Absent when not collected. Options are enums of the actual wire values;
	// content holds counts only, never text.
	RequestOptions *ModelRequestOptions     `json:"request_options,omitempty"`
	Content        *ModelContentObservation `json:"content,omitempty"`
}

type ModelRequestOptions struct {
	Thinking        string `json:"thinking,omitempty"`
	ReasoningEffort string `json:"reasoning_effort,omitempty"`
	ResponseFormat  string `json:"response_format,omitempty"`
	ToolChoice      string `json:"tool_choice,omitempty"`
}

type ModelContentObservation struct {
	ContentBytes            int `json:"content_bytes"`
	TextToolProtocolMarkers int `json:"text_tool_protocol_markers"`
	ReasoningFrames         int `json:"reasoning_frames"`
	ReasoningBytes          int `json:"reasoning_bytes"`
}

type ToolArgumentObservation struct {
	Index  int    `json:"index"`
	Bytes  int    `json:"bytes"`
	SHA256 string `json:"sha256,omitempty"`
}

type NormalizedToolProtocol struct {
	Index                    int    `json:"index"`
	CallIDSHA256             string `json:"call_id_sha256"`
	NameSHA256               string `json:"name_sha256"`
	ArgumentBytes            int    `json:"argument_bytes"`
	ArgumentsSHA256          string `json:"arguments_sha256"`
	CanonicalArgumentsSHA256 string `json:"canonical_arguments_sha256,omitempty"`
}

type NormalizedModelProtocol struct {
	Count     *int
	Error     bool
	Tools     []NormalizedToolProtocol
	Truncated bool
}

func boundedProtocolObservation(value contract.ProtocolObservation) ModelProtocolObservation {
	result := ModelProtocolObservation{Version: 1, Protocol: value.Protocol, Complete: value.Complete, End: value.End,
		RequestSent: value.RequestSent, RequestToolCount: value.RequestToolCount, RequestToolsSHA256: value.RequestToolsSHA256,
		DataFrames: value.DataFrames, ParseFailures: value.ParseFailures, ToolDeltas: value.ToolDeltas,
		AssembledCalls: value.AssembledCalls, EmittedCalls: value.EmittedCalls, FinishReason: value.FinishReason, DoneSeen: value.DoneSeen, Truncated: value.Truncated,
		ToolIndexes: []int{}, Arguments: []ToolArgumentObservation{},
	}
	if value.Version != 1 || (result.Protocol != "openai_chat" && result.Protocol != "openai_sse") {
		result.Protocol = "unknown"
		result.Complete = false
	}
	switch result.End {
	case "before_send", "response", "done", "transport_error", "cancel", "http_error", "read_error", "decode_error", "api_error", "empty_response", "scanner", "eof":
	default:
		result.End = "unknown"
		result.Complete = false
	}
	switch result.FinishReason {
	case "", "stop", "tool_calls", "length", "content_filter", "function_call", "unknown":
	default:
		result.FinishReason = "unknown"
		result.Complete = false
	}
	if !protocolSHA256(result.RequestToolsSHA256) {
		result.RequestToolsSHA256 = ""
		result.Complete = false
	}
	for _, number := range []int{result.RequestToolCount, result.DataFrames, result.ParseFailures, result.ToolDeltas, result.AssembledCalls, result.EmittedCalls} {
		if number < 0 {
			result.Complete = false
		}
	}
	for index, number := range value.ToolIndexes {
		if index >= protocolToolLimit {
			result.Truncated = true
			break
		}
		if number < 0 {
			result.Complete = false
			continue
		}
		result.ToolIndexes = append(result.ToolIndexes, number)
	}
	for index, argument := range value.Arguments {
		if index >= protocolToolLimit {
			result.Truncated = true
			break
		}
		if argument.Index < 0 || argument.Bytes < 0 || !protocolSHA256(argument.SHA256) {
			result.Complete = false
			continue
		}
		result.Arguments = append(result.Arguments, ToolArgumentObservation{argument.Index, argument.Bytes, argument.SHA256})
	}
	if options := value.RequestOptions; options != nil {
		result.RequestOptions = &ModelRequestOptions{
			Thinking:        protocolEnum(options.Thinking, "enabled", "disabled"),
			ReasoningEffort: protocolEnum(options.ReasoningEffort, "none", "minimal", "low", "medium", "high", "xhigh", "max"),
			ResponseFormat:  protocolEnum(options.ResponseFormat, "json_object", "json_schema", "text"),
			ToolChoice:      protocolEnum(options.ToolChoice, "auto", "none", "required", "tool"),
		}
	}
	if content := value.Content; content != nil {
		if content.ContentBytes < 0 || content.TextToolProtocolMarkers < 0 || content.ReasoningFrames < 0 || content.ReasoningBytes < 0 {
			result.Complete = false
		} else {
			observed := ModelContentObservation(*content)
			result.Content = &observed
		}
	}
	return result
}

// protocolEnum keeps known option values and folds anything else to "other".
func protocolEnum(value string, known ...string) string {
	if value == "" {
		return ""
	}
	for _, candidate := range known {
		if value == candidate {
			return value
		}
	}
	return "other"
}

func normalizedModelProtocol(response *contract.ChatResponse, err error) NormalizedModelProtocol {
	result := NormalizedModelProtocol{Error: err != nil}
	if response == nil {
		return result
	}
	count := len(response.ToolCalls)
	result.Count = &count
	for index, call := range response.ToolCalls {
		if index >= protocolToolLimit {
			result.Truncated = true
			break
		}
		tool := NormalizedToolProtocol{Index: index, CallIDSHA256: protocolDigest([]byte(call.ID)), NameSHA256: protocolDigest([]byte(call.Name)), ArgumentBytes: len(call.Args), ArgumentsSHA256: protocolDigest([]byte(call.Args))}
		if canonical, err := frozen.CanonicalizeJSON([]byte(call.Args)); err == nil {
			tool.CanonicalArgumentsSHA256 = protocolDigest(canonical)
		}
		result.Tools = append(result.Tools, tool)
	}
	return result
}

func protocolSHA256(value string) bool {
	if len(value) != 64 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}
func protocolDigest(value []byte) string {
	hash := sha256.Sum256(value)
	return hex.EncodeToString(hash[:])
}
