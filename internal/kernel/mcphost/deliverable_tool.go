package mcphost

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/jinyitao123/loom/contract"
)

// SaveDeliverableTool is the builtin tool name agents call to declare the
// current turn's final deliverable.
const SaveDeliverableTool = "save_deliverable"

// DeclaredDeliverable is the shared buffer one DeliverableDeclareDispatcher
// records into. The chat turn reads it after the run completes and projects
// the declaration into the immutable deliverable store. The last call wins.
type DeclaredDeliverable struct {
	Title   string
	Content string
}

var saveDeliverableInputSchema = json.RawMessage(`{
	"type": "object",
	"properties": {
		"title": {"type": "string", "description": "Optional deliverable title"},
		"content": {"type": "string", "description": "Full final deliverable content"}
	},
	"required": ["content"]
}`)

// DeliverableDeclareDispatcher exposes the save_deliverable builtin tool so an
// agent explicitly declares when a turn produced a formal deliverable. Turns
// without a declaration project no deliverable.
type DeliverableDeclareDispatcher struct {
	declared *DeclaredDeliverable
}

// NewDeliverableDeclareDispatcher creates the dispatcher recording into
// declared. declared must not be nil and must outlive the run.
func NewDeliverableDeclareDispatcher(declared *DeclaredDeliverable) *DeliverableDeclareDispatcher {
	return &DeliverableDeclareDispatcher{declared: declared}
}

// ListTools returns the single save_deliverable tool.
func (d *DeliverableDeclareDispatcher) ListTools(context.Context) ([]contract.ToolDef, error) {
	return []contract.ToolDef{{
		Name: SaveDeliverableTool,
		Description: "仅当本轮对话产出了用户明确要求的正式成果（文档、报告、方案等）时调用，将其存档为不可变交付物。" +
			"闲聊、查询、调研、日常问答不要调用。 " +
			"Call this only when the turn produced a formal deliverable the user explicitly asked for " +
			"(document, report, plan, etc.); it is archived as an immutable deliverable. " +
			"Do not call it for chit-chat, lookups, research, or routine Q&A.",
		InputSchema: saveDeliverableInputSchema,
	}}, nil
}

// Dispatch records one deliverable declaration; the last call in a turn wins.
func (d *DeliverableDeclareDispatcher) Dispatch(_ context.Context, call contract.ToolCall) (*contract.ToolResult, error) {
	if call.Name != SaveDeliverableTool {
		return &contract.ToolResult{
			CallID: call.ID, Content: fmt.Sprintf("unknown tool %q", call.Name), IsError: true,
		}, nil
	}
	var input struct {
		Title   string `json:"title"`
		Content string `json:"content"`
	}
	if err := json.Unmarshal([]byte(call.Args), &input); err != nil {
		return &contract.ToolResult{CallID: call.ID, Content: "invalid input: " + err.Error(), IsError: true}, nil
	}
	if strings.TrimSpace(input.Content) == "" {
		return &contract.ToolResult{CallID: call.ID, Content: "content is required", IsError: true}, nil
	}
	d.declared.Title = strings.TrimSpace(input.Title)
	d.declared.Content = input.Content
	return &contract.ToolResult{
		CallID:  call.ID,
		Content: "已记录交付物声明，将在本轮结束时存档为不可变交付物。",
	}, nil
}

var _ contract.ToolDispatcher = (*DeliverableDeclareDispatcher)(nil)
