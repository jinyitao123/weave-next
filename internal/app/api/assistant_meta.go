package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"reflect"
)

type assistantAgentContextKey struct{}

type assistantMetadata struct {
	Grounding json.RawMessage `json:"grounding,omitempty"`
	Blocks    []ContentBlock  `json:"blocks,omitempty"`
}

// mergeRuntimeAssignmentMetadata keeps the concrete execution environment on
// the durable assistant message instead of limiting it to the terminal SSE
// event. That makes the assignment auditable after refresh and in history.
func mergeRuntimeAssignmentMetadata(metadata json.RawMessage, assignment any) json.RawMessage {
	value := reflect.ValueOf(assignment)
	if !value.IsValid() || ((value.Kind() == reflect.Pointer || value.Kind() == reflect.Interface) && value.IsNil()) {
		return metadata
	}
	var fields map[string]any
	if len(metadata) > 0 {
		if err := json.Unmarshal(metadata, &fields); err != nil {
			slog.Warn("assistant metadata could not be decoded for runtime assignment", "error", err)
			return metadata
		}
	}
	if fields == nil {
		fields = map[string]any{}
	}
	fields["runtime_assignment"] = assignment
	encoded, err := json.Marshal(fields)
	if err != nil {
		slog.Warn("assistant runtime assignment metadata could not be encoded", "error", err)
		return metadata
	}
	return encoded
}

func contextWithAssistantAgent(ctx context.Context, agentName string) context.Context {
	return context.WithValue(ctx, assistantAgentContextKey{}, agentName)
}

func assistantAgentFromContext(ctx context.Context) string {
	agentName, _ := ctx.Value(assistantAgentContextKey{}).(string)
	return agentName
}

// buildAssistantMetadata combines renderable blocks and grounding findings in
// one product-message metadata object.
func (s *Server) buildAssistantMetadata(ctx context.Context, workspaceID, content string) json.RawMessage {
	blocks := filterContentBlocks(ParseBlocks(content), assistantAgentFromContext(ctx))
	grounding := s.groundAssistant(ctx, workspaceID, content)
	return mergeAssistantMetadata(blocks, grounding)
}

func mergeAssistantMetadata(blocks []ContentBlock, grounding json.RawMessage) json.RawMessage {
	if len(blocks) == 0 && len(grounding) == 0 {
		return nil
	}

	metadata := assistantMetadata{Blocks: blocks}
	if len(grounding) > 0 {
		var wrapper struct {
			Grounding json.RawMessage `json:"grounding"`
		}
		if err := json.Unmarshal(grounding, &wrapper); err != nil {
			slog.Warn("assistant grounding metadata could not be merged", "error", err)
		} else {
			metadata.Grounding = wrapper.Grounding
		}
	}
	if len(metadata.Blocks) == 0 && len(metadata.Grounding) == 0 {
		return nil
	}

	encoded, err := json.Marshal(metadata)
	if err != nil {
		slog.Warn("assistant metadata could not be encoded", "error", err)
		return nil
	}
	return encoded
}
