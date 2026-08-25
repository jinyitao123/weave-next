package teamforge

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jinyitao123/loom/contract"
	"github.com/jinyitao123/weave/internal/base/deliverable"
	"github.com/jinyitao123/weave/internal/base/taskqueue"
)

const (
	evidenceNamespacePrefix = "audit:"
	defaultEvidenceLimit    = 10
	maxEvidenceLimit        = 50
	maxEvidenceTextRunes    = 1200
)

// Evidence section statuses. They give the model a semantic read on each
// domain so "nothing here" and "you have seen everything" are facts in the
// result, not something it must infer from empty arrays:
//
//	ok         — the section was read and returned data (possibly truncated).
//	not_found  — an exact filter matched nothing: the fact does not exist.
//	exhaustive — the section returned the complete domain (no truncation):
//	             re-querying with different wording returns the same data.
const (
	evidenceStatusOK         = "ok"
	evidenceStatusNotFound   = "not_found"
	evidenceStatusExhaustive = "exhaustive"
)

type usageEvidenceJSON struct {
	InputTokens  int64   `json:"input_tokens"`
	OutputTokens int64   `json:"output_tokens"`
	CostUSD      float64 `json:"cost_usd"`
	Runs         int     `json:"runs"`
}

type runEvidenceJSON struct {
	RunID              string                    `json:"run_id,omitempty"`
	Run                json.RawMessage           `json:"run,omitempty"`
	Runs               []json.RawMessage         `json:"runs,omitempty"`
	RunsStatus         string                    `json:"runs_status,omitempty"`
	Tasks              []taskEvidenceJSON        `json:"tasks"`
	TasksStatus        string                    `json:"tasks_status,omitempty"`
	Deliverables       []deliverableEvidenceJSON `json:"deliverables"`
	DeliverablesStatus string                    `json:"deliverables_status,omitempty"`
	Usage              usageEvidenceJSON         `json:"usage"`
	Limit              int                       `json:"limit"`
	Truncated          bool                      `json:"truncated"`
}

// Evidence projections deliberately omit unbounded task payload/result,
// deliverable bodies, and raw run state. The meta-team needs stable identities
// and outcomes for planning/evaluation; full user content is neither necessary
// nor safe to inject repeatedly into an LLM context. A short deliverable
// preview remains available as evidence.
type taskEvidenceJSON struct {
	ID              string     `json:"id"`
	Agent           string     `json:"agent,omitempty"`
	AgentID         string     `json:"agent_id,omitempty"`
	AgentVersion    int        `json:"agent_version,omitempty"`
	IdentityKind    string     `json:"identity_kind,omitempty"`
	ExecutionScope  string     `json:"execution_scope,omitempty"`
	WorkflowID      string     `json:"workflow_id,omitempty"`
	WorkflowVersion int        `json:"workflow_version,omitempty"`
	Status          string     `json:"status"`
	Error           string     `json:"error,omitempty"`
	RunID           string     `json:"run_id,omitempty"`
	CreatedAt       time.Time  `json:"created_at"`
	CompletedAt     *time.Time `json:"completed_at,omitempty"`
}

type deliverableEvidenceJSON struct {
	ID               string    `json:"id"`
	ConversationID   string    `json:"conversation_id,omitempty"`
	RunID            string    `json:"run_id"`
	RunSnapshotID    string    `json:"run_snapshot_id,omitempty"`
	Title            string    `json:"title"`
	ContentType      string    `json:"content_type"`
	ContentPreview   string    `json:"content_preview,omitempty"`
	ContentTruncated bool      `json:"content_truncated"`
	CreatedAt        time.Time `json:"created_at"`
}

// getRunEvidence implements tf_get_run_evidence: workspace-scoped, bounded
// reads of run/task/deliverable evidence plus a usage summary derived from
// run records. Filters by run_id or an exact asset ID; every section is capped
// at the requested limit, truncation is flagged, and each section reports a
// domain status (ok/not_found/exhaustive). Calls are metered by the
// dispatcher's per-domain evidence budget; a call whose budget is exhausted
// never touches the stores.
func (d *ReadToolsDispatcher) getRunEvidence(ctx context.Context, call contract.ToolCall) (*contract.ToolResult, error) {
	var input struct {
		RunID         string `json:"run_id"`
		TaskID        string `json:"task_id"`
		WorkflowID    string `json:"workflow_id"`
		AgentID       string `json:"agent_id"`
		DeliverableID string `json:"deliverable_id"`
		Limit         int    `json:"limit"`
	}
	if err := json.Unmarshal([]byte(call.Args), &input); err != nil {
		return toolError(call.ID, "invalid input: "+err.Error()), nil
	}
	input.RunID = strings.TrimSpace(input.RunID)
	if strings.HasPrefix(input.RunID, "br-") {
		return toolError(call.ID, "run not found。若你想查询构建运行状态，请使用 tf_get_build_context。"), nil
	}
	if input.RunID != "" && !strings.HasPrefix(input.RunID, "rt-") {
		if _, err := uuid.Parse(input.RunID); err != nil {
			return toolError(call.ID, "run_id 需要运行 ID（rt-…）；查询团队成员信息用 tf_get_team/tf_get_agent_version"), nil
		}
	}
	limit := input.Limit
	if limit <= 0 {
		limit = defaultEvidenceLimit
	}
	if limit > maxEvidenceLimit {
		limit = maxEvidenceLimit
	}
	hasExactFilter := input.RunID != "" || input.TaskID != "" || input.WorkflowID != "" ||
		input.AgentID != "" || input.DeliverableID != ""
	if !d.discovery && !hasExactFilter {
		return toolError(call.ID, "exact evidence filter is required for a bound build run (run_id, task_id, workflow_id, agent_id, or deliverable_id)"), nil
	}
	globalDiscovery := d.discovery && !hasExactFilter

	// Determine the primary evidence domain requested by this call and charge
	// the budget before any store access. A run-scoped query may return
	// related tasks/deliverables, but its search intent is the run domain;
	// counting it that way keeps the total budget reachable while still
	// preventing repeated run pivots.
	var domains []string
	switch {
	case input.TaskID != "" || input.WorkflowID != "" || input.AgentID != "":
		if d.deps.Tasks != nil {
			domains = append(domains, evidenceDomainTasks)
		}
	case input.DeliverableID != "":
		if d.deps.Deliverables != nil {
			domains = append(domains, evidenceDomainDeliverables)
		}
	case input.RunID != "":
		domains = append(domains, evidenceDomainRuns)
	case globalDiscovery:
		if d.deps.Runs != nil {
			domains = append(domains, evidenceDomainRuns)
		}
		if d.deps.Tasks != nil {
			domains = append(domains, evidenceDomainTasks)
		}
		if d.deps.Deliverables != nil {
			domains = append(domains, evidenceDomainDeliverables)
		}
	}
	if rejection, err := d.chargeEvidenceBudget(call, domains); rejection != nil || err != nil {
		return rejection, err
	}

	out := runEvidenceJSON{
		RunID:        input.RunID,
		Tasks:        []taskEvidenceJSON{},
		Deliverables: []deliverableEvidenceJSON{},
		Limit:        limit,
	}

	// Run evidence and usage come from the audit:<workspace> namespace,
	// the same read path as the platform runs API.
	if d.deps.Runs != nil && (input.RunID != "" || globalDiscovery) {
		usage, exhaustive, err := d.readRuns(ctx, input.RunID, limit, &out)
		if err != nil {
			return toolError(call.ID, err.Error()), nil
		}
		out.Usage = usage
		out.RunsStatus = evidenceSectionStatus(len(out.Runs)+lenNonEmpty(out.Run), input.RunID != "", exhaustive)
	}

	if d.deps.Tasks != nil && (input.TaskID != "" || input.RunID != "" || input.WorkflowID != "" || input.AgentID != "" || globalDiscovery) {
		tasks, complete, truncated, err := d.readTasks(
			ctx, input.TaskID, input.RunID, input.WorkflowID, input.AgentID, limit,
		)
		if err != nil {
			return toolError(call.ID, err.Error()), nil
		}
		out.Tasks = tasks
		out.TasksStatus = evidenceSectionStatus(len(tasks), input.TaskID != "" || input.RunID != "" || input.WorkflowID != "" || input.AgentID != "", complete)
		out.Truncated = out.Truncated || truncated
	}

	if d.deps.Deliverables != nil && (input.DeliverableID != "" || input.RunID != "" || globalDiscovery) {
		deliverables, complete, truncated, err := d.readDeliverables(ctx, input.DeliverableID, input.RunID, limit)
		if err != nil {
			return toolError(call.ID, err.Error()), nil
		}
		out.Deliverables = deliverables
		out.DeliverablesStatus = evidenceSectionStatus(len(deliverables), input.DeliverableID != "" || input.RunID != "", complete)
		out.Truncated = out.Truncated || truncated
	}
	return toolJSON(call.ID, out)
}

// evidenceSectionStatus classifies one returned section. complete means the
// read covered the whole domain without truncation.
func evidenceSectionStatus(count int, exactFilter bool, complete bool) string {
	if count == 0 {
		if complete && !exactFilter {
			return evidenceStatusExhaustive
		}
		if !complete {
			return evidenceStatusOK
		}
		return evidenceStatusNotFound
	}
	if complete {
		return evidenceStatusExhaustive
	}
	return evidenceStatusOK
}

func evidencePageSize(limit int) int {
	pageSize := limit + 1
	if pageSize < defaultEvidenceLimit {
		pageSize = defaultEvidenceLimit
	}
	if pageSize > maxEvidenceLimit {
		pageSize = maxEvidenceLimit
	}
	return pageSize
}

func trimEvidenceMatches[T any](items []T, limit int) []T {
	if len(items) <= limit {
		return items
	}
	return items[:limit]
}

func matchesTaskEvidence(task taskqueue.Task, runID, workflowID, agentID string) bool {
	if runID != "" && task.RunID != runID {
		return false
	}
	if workflowID != "" && task.WorkflowID != workflowID {
		return false
	}
	if agentID != "" && task.AgentID != agentID {
		return false
	}
	return true
}

func matchesDeliverableEvidence(item deliverable.FinalDeliverable, runID string) bool {
	return runID == "" || item.RunID == runID
}

func lenNonEmpty(raw json.RawMessage) int {
	if len(raw) == 0 {
		return 0
	}
	return 1
}

type runRecordUsage struct {
	RunID      string  `json:"run_id"`
	Agent      string  `json:"agent,omitempty"`
	Step       string  `json:"step,omitempty"`
	Status     string  `json:"status"`
	StopReason string  `json:"stop_reason,omitempty"`
	DurationMs int64   `json:"duration_ms"`
	StartedAt  string  `json:"started_at"`
	EndedAt    string  `json:"ended_at"`
	TokensIn   int64   `json:"tokens_in"`
	TokensOut  int64   `json:"tokens_out"`
	CostUSD    float64 `json:"cost_usd"`
}

func projectRunRecord(data []byte) (json.RawMessage, runRecordUsage) {
	var summary runRecordUsage
	_ = json.Unmarshal(data, &summary)
	projected, _ := json.Marshal(summary)
	return projected, summary
}

// readRuns populates out.Run (exact) or out.Runs (bounded listing) and
// returns the aggregated usage evidence. The second return reports whether
// the listing covered the complete run domain without truncation.
func (d *ReadToolsDispatcher) readRuns(
	ctx context.Context,
	runID string,
	limit int,
	out *runEvidenceJSON,
) (usageEvidenceJSON, bool, error) {
	namespace := evidenceNamespacePrefix + d.workspaceID
	usage := usageEvidenceJSON{}
	if runID != "" {
		data, err := d.deps.Runs.Get(ctx, namespace, runID)
		if err != nil {
			// The existing runs read path reports any read failure as a
			// missing run (the API returns 404 the same way).
			return usage, false, errRunNotFound
		}
		projected, recUsage := projectRunRecord(data)
		out.Run = projected
		usage.InputTokens = recUsage.TokensIn
		usage.OutputTokens = recUsage.TokensOut
		usage.CostUSD = recUsage.CostUSD
		usage.Runs = 1
		return usage, true, nil
	}

	keys, err := d.deps.Runs.List(ctx, namespace, "")
	if err != nil {
		return usage, false, err
	}
	complete := len(keys) <= limit
	if len(keys) > limit {
		keys = keys[:limit]
		out.Truncated = true
	}
	for _, key := range keys {
		data, err := d.deps.Runs.Get(ctx, namespace, key)
		if err != nil {
			continue
		}
		projected, recUsage := projectRunRecord(data)
		out.Runs = append(out.Runs, projected)
		usage.InputTokens += recUsage.TokensIn
		usage.OutputTokens += recUsage.TokensOut
		usage.CostUSD += recUsage.CostUSD
		usage.Runs++
	}
	return usage, complete, nil
}

var errRunNotFound = errors.New("run not found")

func (d *ReadToolsDispatcher) readTasks(
	ctx context.Context,
	taskID, runID, workflowID, agentID string,
	limit int,
) ([]taskEvidenceJSON, bool, bool, error) {
	if taskID != "" {
		task, err := d.deps.Tasks.Get(ctx, d.workspaceID, taskID)
		if err != nil {
			return []taskEvidenceJSON{}, true, false, nil
		}
		return []taskEvidenceJSON{projectTask(*task)}, true, false, nil
	}
	pageSize := evidencePageSize(limit)
	matched := make([]taskEvidenceJSON, 0, limit+1)
	for offset := 0; ; offset += pageSize {
		tasks, total, err := d.deps.Tasks.List(ctx, d.workspaceID, pageSize, offset)
		if err != nil {
			return nil, false, false, err
		}
		for _, task := range tasks {
			if matchesTaskEvidence(task, runID, workflowID, agentID) {
				matched = append(matched, projectTask(task))
			}
		}
		if len(matched) > limit {
			return trimEvidenceMatches(matched, limit), false, true, nil
		}
		if len(tasks) == 0 || offset+len(tasks) >= total {
			return matched, true, false, nil
		}
	}
}

func projectTask(task taskqueue.Task) taskEvidenceJSON {
	return taskEvidenceJSON{
		ID: task.ID, Agent: task.Agent, AgentID: task.AgentID, AgentVersion: task.AgentVersion,
		IdentityKind: string(task.IdentityKind), ExecutionScope: string(task.ExecutionScope),
		WorkflowID: task.WorkflowID, WorkflowVersion: task.WorkflowVersion,
		Status: task.Status, Error: truncateEvidenceText(task.Error), RunID: task.RunID,
		CreatedAt: task.CreatedAt, CompletedAt: task.CompletedAt,
	}
}

func (d *ReadToolsDispatcher) readDeliverables(
	ctx context.Context,
	deliverableID, runID string,
	limit int,
) ([]deliverableEvidenceJSON, bool, bool, error) {
	if deliverableID != "" {
		item, err := d.deps.Deliverables.Get(ctx, d.workspaceID, deliverableID)
		if err != nil {
			return []deliverableEvidenceJSON{}, true, false, nil
		}
		return []deliverableEvidenceJSON{projectDeliverable(item)}, true, false, nil
	}
	pageSize := evidencePageSize(limit)
	matched := make([]deliverableEvidenceJSON, 0, limit+1)
	for offset := 0; ; offset += pageSize {
		items, err := d.deps.Deliverables.List(ctx, d.workspaceID, deliverable.ListFilter{Limit: pageSize, Offset: offset})
		if err != nil {
			return nil, false, false, err
		}
		for _, item := range items {
			if matchesDeliverableEvidence(item, runID) {
				matched = append(matched, projectDeliverable(item))
			}
		}
		if len(matched) > limit {
			return trimEvidenceMatches(matched, limit), false, true, nil
		}
		if len(items) < pageSize {
			return matched, true, false, nil
		}
	}
}

func projectDeliverable(item deliverable.FinalDeliverable) deliverableEvidenceJSON {
	preview := truncateEvidenceText(item.Content)
	return deliverableEvidenceJSON{
		ID: item.ID, ConversationID: item.ConversationID, RunID: item.RunID,
		RunSnapshotID: item.RunSnapshotID, Title: item.Title, ContentType: item.ContentType,
		ContentPreview: preview, ContentTruncated: len([]rune(item.Content)) > len([]rune(preview)),
		CreatedAt: item.CreatedAt,
	}
}

func truncateEvidenceText(value string) string {
	runes := []rune(value)
	if len(runes) <= maxEvidenceTextRunes {
		return value
	}
	return string(runes[:maxEvidenceTextRunes])
}
