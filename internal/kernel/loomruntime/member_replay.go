package loomruntime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jinyitao123/loom"
	"github.com/jinyitao123/loom/contract"
	"github.com/jinyitao123/loom/stdlib"
	"github.com/jinyitao123/weave/internal/base/frozen"
)

// Member replay re-runs one member's tool loop from its first step against the
// operations the run already recorded, so an operator can see what the loop
// would do with that history without any model, tool or Forge call and without
// writing to the database. It answers three questions: does the loop still
// produce the same requests (determinism), is every request a history a
// provider accepts, and where did the recorded run stop.

// MemberJournalEntry is one recorded operation of a member run.
type MemberJournalEntry struct {
	Segment   string          `json:"segment"`
	Cursor    int64           `json:"cursor"`
	Kind      string          `json:"kind"`
	Input     json.RawMessage `json:"input"`
	InputHash string          `json:"input_hash"`
	Response  json.RawMessage `json:"response,omitempty"`
	Attempts  int64           `json:"attempts"`
}

// Outcomes of a replay.
const (
	ReplayCompleted      = "completed"
	ReplayJournalEnded   = "journal_ended"
	ReplayOutcomeUnknown = "outcome_unknown"
	ReplayModelLost      = "model_response_lost"
	ReplayDiverged       = "diverged"
	ReplayInvalidHistory = "invalid_history"
	ReplayLoopError      = "loop_error"
)

// MemberReplayReport is the operator's view of one replay.
type MemberReplayReport struct {
	MemberRunID     string   `json:"member_run_id"`
	Segment         string   `json:"segment"`
	Segments        []string `json:"segments"`
	Operations      int      `json:"operations"`
	Replayed        int      `json:"replayed"`
	Outcome         string   `json:"outcome"`
	Detail          string   `json:"detail,omitempty"`
	Output          string   `json:"output,omitempty"`
	HistoryProblems []string `json:"history_problems,omitempty"`
}

type memberJournalQuerier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// ReadMemberJournal returns a member's recorded operations in execution order.
func ReadMemberJournal(ctx context.Context, db memberJournalQuerier, workspaceID, memberRunID string) ([]MemberJournalEntry, error) {
	rows, err := db.Query(ctx, `SELECT key,value FROM loom_store WHERE namespace=$1 AND starts_with(key,$2) ORDER BY key`,
		"member-operation:"+workspaceID, memberRunID+"/")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	entries := []MemberJournalEntry{}
	for rows.Next() {
		var key string
		var value []byte
		if err := rows.Scan(&key, &value); err != nil {
			return nil, err
		}
		var op memberOperation
		if err := json.Unmarshal(value, &op); err != nil {
			return nil, fmt.Errorf("journal entry %q is not readable: %w", key, err)
		}
		rest := strings.TrimPrefix(key, memberRunID+"/")
		cut := strings.LastIndex(rest, "/")
		if cut < 0 {
			return nil, fmt.Errorf("journal key %q has no cursor", key)
		}
		var cursor int64
		if _, err := fmt.Sscanf(rest[cut+1:], "%d", &cursor); err != nil {
			return nil, fmt.Errorf("journal key %q has an invalid cursor", key)
		}
		entries = append(entries, MemberJournalEntry{Segment: rest[:cut], Cursor: cursor, Kind: op.Kind, Input: op.Input,
			InputHash: op.InputHash, Response: op.Response, Attempts: op.Attempts})
	}
	return entries, rows.Err()
}

var (
	errReplayJournalEnded = errors.New("replay reached the end of the recorded journal")
	errReplayModelLost    = errors.New("recorded model operation has no response")
	errReplayDiverged     = errors.New("replay diverged from the recorded journal")
	errReplayLive         = errors.New("replay attempted a live call")
)

// ReplayMemberSegment replays one journal segment of a member run. The first
// segment starts from the member's recorded initial state; later segments start
// from a checkpointed loop state that a journal alone does not carry, so they
// are listed but not replayed.
func ReplayMemberSegment(ctx context.Context, memberRunID string, entries []MemberJournalEntry, initialState json.RawMessage) *MemberReplayReport {
	report := &MemberReplayReport{MemberRunID: memberRunID, Outcome: ReplayLoopError}
	seen := map[string]bool{}
	for _, entry := range entries {
		if !seen[entry.Segment] {
			seen[entry.Segment] = true
			report.Segments = append(report.Segments, entry.Segment)
		}
	}
	if len(entries) == 0 {
		report.Outcome, report.Detail = ReplayJournalEnded, "the member recorded no operations"
		return report
	}
	report.Segment = entries[0].Segment
	segment := []MemberJournalEntry{}
	for _, entry := range entries {
		if entry.Segment == report.Segment {
			segment = append(segment, entry)
		}
	}
	report.Operations = len(segment)

	var firstModel *contract.ChatRequest
	for _, entry := range segment {
		if entry.Kind == string(stdlib.OperationModel) {
			var request contract.ChatRequest
			if err := json.Unmarshal(entry.Input, &request); err != nil {
				report.Detail = "the first recorded model request is unreadable: " + err.Error()
				return report
			}
			firstModel = &request
			break
		}
	}
	if firstModel == nil {
		report.Outcome, report.Detail = ReplayJournalEnded, "the segment recorded no model request"
		return report
	}
	state, err := decodeReplayState(initialState)
	if err != nil {
		report.Detail = err.Error()
		return report
	}
	opts := stdlib.ToolLoopOpts{Model: firstModel.Model, MaxTokens: firstModel.MaxTokens, OutputSchema: firstModel.Schema,
		Effort: firstModel.Effort, MaxIterations: len(segment) + 20}
	if len(firstModel.Messages) > 0 && firstModel.Messages[0].Role == "system" {
		if initial, _ := state["messages"].([]contract.Message); len(initial) == 0 || initial[0].Role != "system" {
			opts.SystemPrompt = firstModel.Messages[0].Content
		}
	}
	journal := &replayJournal{entries: segment, report: report}
	replayCtx := context.WithValue(ctx, replayActiveKey{}, true)
	step := stdlib.NewToolLoopStep(
		stdlib.NewJournaledLLM(replayLiveLLM{}, journal),
		stdlib.NewJournaledToolDispatcher(replayTools{defs: firstModel.Tools}, journal, stdlib.JournaledToolOpts{SerializeWhenActive: true}),
		opts)
	delta, err := step(replayCtx, state)
	report.Replayed = journal.cursor
	switch {
	case err == nil:
		report.Outcome = ReplayCompleted
		if output, ok := delta["output"].(string); ok {
			report.Output = output
		} else if delta["__yield"] == true {
			report.Outcome, report.Detail = ReplayJournalEnded, "the loop paused at a slice or budget boundary, as the recorded run did"
		}
	case errors.Is(err, errReplayDiverged):
		report.Outcome, report.Detail = ReplayDiverged, err.Error()
	case errors.Is(err, errReplayJournalEnded):
		report.Outcome, report.Detail = ReplayJournalEnded, "the loop asked for an operation the run never recorded; the recorded run stopped here"
	case errors.Is(err, errReplayModelLost):
		report.Outcome, report.Detail = ReplayModelLost, err.Error()
	case errors.Is(err, stdlib.ErrJournalOutcomeUnknown), errors.Is(err, ErrMemberOutcomeUnknown):
		report.Outcome, report.Detail = ReplayOutcomeUnknown, err.Error()
	default:
		report.Detail = err.Error()
	}
	if len(report.HistoryProblems) > 0 && report.Outcome != ReplayDiverged {
		report.Outcome = ReplayInvalidHistory
	}
	return report
}

func decodeReplayState(raw json.RawMessage) (loom.State, error) {
	state := loom.State{}
	if len(raw) > 0 && string(raw) != "null" {
		if err := json.Unmarshal(raw, &state); err != nil {
			return nil, fmt.Errorf("the member's initial state is unreadable: %w", err)
		}
	}
	if rawMessages, present := state["messages"]; present {
		encoded, err := json.Marshal(rawMessages)
		if err != nil {
			return nil, err
		}
		var messages []contract.Message
		if err := json.Unmarshal(encoded, &messages); err != nil {
			return nil, fmt.Errorf("the member's initial messages are unreadable: %w", err)
		}
		state["messages"] = messages
	}
	return state, nil
}

type replayActiveKey struct{}

// replayLiveLLM is what a replay would call if the journal ever let a model
// request through; it never should, so reaching it is a defect in the replay.
type replayLiveLLM struct{}

func (replayLiveLLM) Chat(context.Context, contract.ChatRequest) (*contract.ChatResponse, error) {
	return nil, errReplayLive
}

func (replayLiveLLM) Stream(context.Context, contract.ChatRequest) (<-chan contract.StreamChunk, error) {
	return nil, errReplayLive
}

type replayTools struct{ defs []contract.ToolDef }

func (tools replayTools) ListTools(context.Context) ([]contract.ToolDef, error) {
	return tools.defs, nil
}

func (replayTools) Dispatch(context.Context, contract.ToolCall) (*contract.ToolResult, error) {
	return nil, errReplayLive
}

// replayJournal serves recorded responses by position and reports the first
// place the replayed loop disagrees with the record.
type replayJournal struct {
	entries []MemberJournalEntry
	cursor  int
	report  *MemberReplayReport
}

func (*replayJournal) Active(ctx context.Context) bool {
	active, _ := ctx.Value(replayActiveKey{}).(bool)
	return active
}

func (journal *replayJournal) Execute(_ context.Context, operation stdlib.JournalOperation, _ stdlib.JournalPerform) (json.RawMessage, error) {
	index := journal.cursor
	if index >= len(journal.entries) {
		return nil, errReplayJournalEnded
	}
	entry := journal.entries[index]
	input := operation.Input
	if call, ok := input.(contract.ToolCall); ok {
		args, err := frozen.CanonicalizeJSON([]byte(call.Args))
		if err != nil {
			return nil, fmt.Errorf("%w: tool call %q has arguments that are not JSON", errReplayDiverged, call.ID)
		}
		call.Args = string(args)
		input = call
	}
	raw, err := json.Marshal(input)
	if err != nil {
		return nil, err
	}
	if operation.Kind == stdlib.OperationModel {
		var request contract.ChatRequest
		if json.Unmarshal(raw, &request) == nil {
			if problem := validateProviderHistory(request.Messages); problem != nil {
				journal.report.HistoryProblems = append(journal.report.HistoryProblems, fmt.Sprintf("operation %d: %v", index+1, problem))
			}
		}
	}
	hash, err := frozen.HashCanonicalJSON(raw)
	if err != nil {
		return nil, err
	}
	if entry.Kind != string(operation.Kind) || entry.InputHash != hash {
		return nil, fmt.Errorf("%w at operation %d: %s", errReplayDiverged, index+1, describeDivergence(entry, operation.Kind, raw))
	}
	journal.cursor++
	if len(entry.Response) == 0 {
		if operation.Kind == stdlib.OperationTool {
			return nil, fmt.Errorf("operation %d: %w", index+1, stdlib.ErrJournalOutcomeUnknown)
		}
		return nil, fmt.Errorf("operation %d: %w", index+1, errReplayModelLost)
	}
	return entry.Response, nil
}

// describeDivergence says where two model requests first differ, since that is
// what an operator needs to find the cause.
func describeDivergence(entry MemberJournalEntry, kind stdlib.OperationKind, replayed json.RawMessage) string {
	if entry.Kind != string(kind) {
		return fmt.Sprintf("the record has a %s operation where the loop now issues a %s operation", entry.Kind, kind)
	}
	if kind != stdlib.OperationModel {
		return "the tool call differs from the recorded one"
	}
	var recorded, now contract.ChatRequest
	if json.Unmarshal(entry.Input, &recorded) != nil || json.Unmarshal(replayed, &now) != nil {
		return "the model request differs from the recorded one"
	}
	if recorded.Model != now.Model {
		return fmt.Sprintf("model %q now, %q recorded", now.Model, recorded.Model)
	}
	shared := len(recorded.Messages)
	if len(now.Messages) < shared {
		shared = len(now.Messages)
	}
	for position := 0; position < shared; position++ {
		before, _ := json.Marshal(recorded.Messages[position])
		after, _ := json.Marshal(now.Messages[position])
		if string(before) != string(after) {
			return fmt.Sprintf("message %d (%s) differs from the record", position+1, now.Messages[position].Role)
		}
	}
	if len(recorded.Messages) != len(now.Messages) {
		return fmt.Sprintf("the loop now sends %d messages where the record has %d", len(now.Messages), len(recorded.Messages))
	}
	return "the request differs from the record outside the messages (tools, limits or schema)"
}

// validateProviderHistory checks the structure OpenAI-compatible providers,
// DeepSeek included, reject with a 400: every assistant tool call is answered by
// exactly one tool message before the next non-tool message, and a tool message
// answers only an open call.
func validateProviderHistory(messages []contract.Message) error {
	seen, open := map[string]bool{}, map[string]bool{}
	for position, message := range messages {
		switch message.Role {
		case "system", "user":
			if len(open) > 0 {
				return fmt.Errorf("message %d: %s message while tool calls are unanswered", position+1, message.Role)
			}
		case "assistant":
			if len(open) > 0 {
				return fmt.Errorf("message %d: assistant message while tool calls are unanswered", position+1)
			}
			for _, call := range message.ToolCalls {
				if call.ID == "" || call.Name == "" || seen[call.ID] {
					return fmt.Errorf("message %d: invalid or duplicate tool call %q", position+1, call.ID)
				}
				if !json.Valid([]byte(call.Args)) {
					return fmt.Errorf("message %d: tool call %q has invalid JSON arguments", position+1, call.ID)
				}
				seen[call.ID], open[call.ID] = true, true
			}
		case "tool":
			if !open[message.ToolCallID] {
				return fmt.Errorf("message %d: tool result %q answers no open call", position+1, message.ToolCallID)
			}
			delete(open, message.ToolCallID)
		default:
			return fmt.Errorf("message %d: unknown role %q", position+1, message.Role)
		}
	}
	if len(open) > 0 {
		return fmt.Errorf("history ends with %d unanswered tool calls", len(open))
	}
	return nil
}
