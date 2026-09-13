package teameval

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/jinyitao123/weave/internal/base/deliverable"
	"github.com/jinyitao123/weave/internal/kernel/taskqueue"
	"github.com/jinyitao123/weave/internal/kernel/teamrun"
)

// terminalRunStatuses is the legal terminal state set of a team run. Two
// vocabularies are legal here: the run-row words (teamrun.Status.Terminal:
// succeeded, failed, cancelled, abandoned) and the loomruntime schema-v3
// terminal-record words written by commitFrozenNormalTerminal ("success"
// for a normal success — see internal/loomruntime/normal_terminal_coordinator.go).
var terminalRunStatuses = map[string]bool{
	"succeeded": true,
	"success":   true,
	"failed":    true,
	"cancelled": true,
	"abandoned": true,
}

// isSuccessRecordStatus reports whether a terminal run record status means
// success in either vocabulary (run row "succeeded" / schema-v3 "success").
func isSuccessRecordStatus(status string) bool {
	return status == "succeeded" || status == "success"
}

// allowedAuditStatuses are the non-violation audit statuses for allowed tool
// calls. Anything else (error/denied/...) is a governance violation.
var allowedAuditStatuses = map[string]bool{
	"ok":       true,
	"executed": true,
}

// terminalRunRecord is the minimal read shape of the schema-v3 terminal run
// record stored in the audit:<workspace> namespace.
type terminalRunRecord struct {
	SchemaVersion   int     `json:"schema_version"`
	RunID           string  `json:"run_id"`
	Agent           string  `json:"agent"`
	TeamID          *string `json:"team_id,omitempty"`
	WorkflowID      *string `json:"workflow_id,omitempty"`
	WorkflowVersion *int    `json:"workflow_version,omitempty"`
	RunSnapshotID   *string `json:"run_snapshot_id,omitempty"`
	Status          string  `json:"status"`
	StopReason      string  `json:"stop_reason"`
}

// gateRunTerminalConsistent checks the build run's test-run evidence: each
// snapshot must have a run record at a legal terminal state, the run/task
// identities must agree with the snapshot, and the artifact must resolve to
// a deliverable owned by the same run (plan §8.1: run/task/产物身份一致).
func (e *GateEvaluator) gateRunTerminalConsistent(ctx context.Context, ev EvaluatedTeam) GateResult {
	evidence := fmt.Sprintf("build_run=%s team=%s", ev.BuildRun.BuildRunID, ev.TeamID())
	if ev.Team == nil {
		return Fail(GateRunTerminalConsistent, evidence, "team not found")
	}
	if e.deps.Snapshots == nil || e.deps.Runs == nil || e.deps.Tasks == nil || e.deps.Deliverables == nil {
		return Fail(GateRunTerminalConsistent, evidence, "evidence stores are unavailable")
	}
	snapshots, err := e.deps.Snapshots.ListByTeam(ctx, ev.WorkspaceID(), ev.TeamID())
	if err != nil {
		return Fail(GateRunTerminalConsistent, evidence, "read run snapshots: "+err.Error())
	}
	var runSnapshots []snapshotRef
	for _, snap := range snapshots {
		if snap.BuildRunID == ev.BuildRun.BuildRunID && snap.RunID != "" {
			runSnapshots = append(runSnapshots, snapshotRef{
				RunSnapshotID: snap.RunID, WorkflowID: snap.WorkflowID,
				BuildRoundNo: snap.BuildRoundNo, CreatedAt: snap.CreatedAt,
			})
		}
	}
	if len(runSnapshots) == 0 {
		return Fail(GateRunTerminalConsistent, evidence, "build run has no test-run snapshot evidence")
	}
	runSnapshots, err = e.authoritativeScenarioSnapshots(ctx, ev.WorkspaceID(), runSnapshots)
	if err != nil {
		return Fail(GateRunTerminalConsistent, evidence, "select authoritative scenario evidence: "+err.Error())
	}
	for _, snap := range runSnapshots {
		ref := fmt.Sprintf("%s run_snapshot_id=%s", evidence, snap.RunSnapshotID)
		record, err := e.readRunRecord(ctx, ev.WorkspaceID(), snap.RunSnapshotID)
		if err != nil {
			return Fail(GateRunTerminalConsistent, ref, "run record is missing or unreadable: "+err.Error())
		}
		if !terminalRunStatuses[record.Status] {
			return Fail(
				GateRunTerminalConsistent,
				ref,
				fmt.Sprintf("run has not reached a legal terminal state (status %q)", record.Status),
			)
		}
		if record.RunID == "" {
			return Fail(GateRunTerminalConsistent, ref, "run record identity does not match the snapshot run id")
		}
		ref = fmt.Sprintf("%s run_id=%s", ref, record.RunID)
		if record.RunSnapshotID == nil || *record.RunSnapshotID != snap.RunSnapshotID {
			got := "<missing>"
			if record.RunSnapshotID != nil {
				got = *record.RunSnapshotID
			}
			return Fail(
				GateRunTerminalConsistent,
				ref,
				fmt.Sprintf("run record snapshot identity %q does not match snapshot run id %q",
					got, snap.RunSnapshotID),
			)
		}
		if record.TeamID != nil && *record.TeamID != ev.TeamID() {
			return Fail(
				GateRunTerminalConsistent,
				ref,
				fmt.Sprintf("run record team %q does not match evaluated team %q",
					*record.TeamID, ev.TeamID()),
			)
		}
		if snap.WorkflowID != "" && record.WorkflowID != nil && *record.WorkflowID != snap.WorkflowID {
			return Fail(
				GateRunTerminalConsistent,
				ref,
				fmt.Sprintf("run record workflow %q does not match snapshot workflow %q",
					*record.WorkflowID, snap.WorkflowID),
			)
		}
		tasks, err := e.tasksForRun(ctx, ev.WorkspaceID(), record.RunID, snap.RunSnapshotID)
		if err != nil {
			return Fail(GateRunTerminalConsistent, ref, "read run tasks: "+err.Error())
		}
		if len(tasks) == 0 {
			return Fail(GateRunTerminalConsistent, ref, terminalFailureDetail(record, "run has no task evidence"))
		}
		for _, task := range tasks {
			taskRef := fmt.Sprintf("%s task=%s", ref, task.ID)
			if task.RunID != "" && task.RunID != record.RunID {
				return Fail(
					GateRunTerminalConsistent,
					taskRef,
					fmt.Sprintf("task/run identity mismatch: task run_id %q != terminal run id %q",
						task.RunID, record.RunID),
				)
			}
			if task.RunSnapshotID != snap.RunSnapshotID {
				return Fail(
					GateRunTerminalConsistent,
					taskRef,
					fmt.Sprintf("task/snapshot identity mismatch: task run_snapshot_id %q != snapshot id %q",
						task.RunSnapshotID, snap.RunSnapshotID),
				)
			}
			if snap.WorkflowID != "" && task.WorkflowID != "" && task.WorkflowID != snap.WorkflowID {
				return Fail(
					GateRunTerminalConsistent,
					taskRef,
					fmt.Sprintf("task/run identity mismatch: task workflow %q != snapshot workflow %q",
						task.WorkflowID, snap.WorkflowID),
				)
			}
			if !task.IsTerminal() {
				return Fail(
					GateRunTerminalConsistent,
					taskRef,
					fmt.Sprintf("task has not reached a terminal state (status %q)", task.Status),
				)
			}
		}
		deliverables, err := e.deps.Deliverables.List(
			ctx, ev.WorkspaceID(), deliverableListFilter(),
		)
		if err != nil {
			return Fail(
				GateRunTerminalConsistent,
				ref,
				"read deliverables: "+err.Error(),
			)
		}
		var matched *deliverable.FinalDeliverable
		for i := range deliverables {
			if deliverables[i].RunSnapshotID == snap.RunSnapshotID {
				matched = &deliverables[i]
				break
			}
		}
		if matched == nil {
			return Fail(
				GateRunTerminalConsistent,
				ref,
				terminalFailureDetail(record, "run has no artifact evidence (no deliverable bound to the snapshot)"),
			)
		}
		if matched.RunID != record.RunID {
			return Fail(
				GateRunTerminalConsistent,
				fmt.Sprintf("%s artifact=%s", ref, matched.ID),
				fmt.Sprintf("artifact/run identity mismatch: deliverable run_id %q != terminal run id %q",
					matched.RunID, record.RunID),
			)
		}
	}
	return Pass(GateRunTerminalConsistent, evidence, "every test run reached a legal terminal state with consistent run/task/artifact identity")
}

// gateNoGovernanceViolation checks the relevant run's audit trail: no
// denied/unauthorized tool calls and no swallowed errors (a run reported as
// succeeded despite failed tasks) (plan §8.1).
func (e *GateEvaluator) gateNoGovernanceViolation(ctx context.Context, ev EvaluatedTeam) GateResult {
	evidence := fmt.Sprintf("build_run=%s team=%s", ev.BuildRun.BuildRunID, ev.TeamID())
	if ev.Team == nil {
		return Fail(GateNoGovernanceViolation, evidence, "team not found")
	}
	if e.deps.Audit == nil {
		return Fail(GateNoGovernanceViolation, evidence, "audit store is unavailable")
	}
	agentNames := make([]string, 0, len(ev.Workers)+1)
	if ev.Lead != nil {
		agentNames = append(agentNames, ev.Lead.Name)
	}
	for _, worker := range ev.Workers {
		agentNames = append(agentNames, worker.AgentName())
	}
	for _, agent := range agentNames {
		entries, err := e.deps.Audit.List(ctx, ev.WorkspaceID(), agent, 1000, 0)
		if err != nil {
			return Fail(GateNoGovernanceViolation, evidence, "read audit trail: "+err.Error())
		}
		for _, entry := range entries {
			if allowedAuditStatuses[entry.Status] {
				continue
			}
			return Fail(
				GateNoGovernanceViolation,
				fmt.Sprintf("%s agent=%s audit=%s tool=%s", evidence, agent, entry.ID, entry.Tool),
				fmt.Sprintf("governance violation recorded: audit status %q: %s", entry.Status, entry.Detail),
			)
		}
	}
	if e.deps.Runs != nil && e.deps.Tasks != nil && e.deps.Snapshots != nil {
		snapshots, err := e.deps.Snapshots.ListByTeam(ctx, ev.WorkspaceID(), ev.TeamID())
		if err == nil {
			for _, snap := range snapshots {
				if snap.BuildRunID != ev.BuildRun.BuildRunID || snap.RunID == "" {
					continue
				}
				record, readErr := e.readRunRecord(ctx, ev.WorkspaceID(), snap.RunID)
				if readErr != nil || !isSuccessRecordStatus(record.Status) {
					continue
				}
				tasks, taskErr := e.tasksForRun(ctx, ev.WorkspaceID(), record.RunID, snap.RunID)
				if taskErr != nil {
					continue
				}
				for _, task := range tasks {
					if task.IsTerminal() && (task.Status == taskqueue.StatusFailed ||
						task.Status == taskqueue.StatusTimedOut ||
						task.Status == taskqueue.StatusCancelled) {
						return Fail(
							GateNoGovernanceViolation,
							fmt.Sprintf("%s run=%s task=%s", evidence, snap.RunID, task.ID),
							fmt.Sprintf("swallowed error: run succeeded while task ended %q (%s)",
								task.Status, task.Error),
						)
					}
				}
			}
		}
	}
	return Pass(GateNoGovernanceViolation, evidence, "no denied tool calls or swallowed errors in the evaluated run evidence")
}

// deliverableListFilter lists every workspace deliverable (bounded).
func deliverableListFilter() deliverable.ListFilter {
	return deliverable.ListFilter{Limit: 200}
}

// snapshotRef is the projection of one run snapshot the evidence gates need.
type snapshotRef struct {
	RunSnapshotID string
	WorkflowID    string
	BuildRoundNo  int
	CreatedAt     time.Time
}

// authoritativeScenarioSnapshots collapses only exact candidate-scenario
// retries within one build round. The root task carries the contract-frozen
// scenario_id and input_version; a strictly later snapshot for that exact
// identity is the retry authority. Unidentified snapshots and timestamp ties
// remain in the evidence set so ambiguity continues to fail closed.
func (e *GateEvaluator) authoritativeScenarioSnapshots(
	ctx context.Context,
	workspaceID string,
	snapshots []snapshotRef,
) ([]snapshotRef, error) {
	type scenarioIdentity struct {
		RoundNo      int
		ScenarioID   string
		InputVersion string
	}
	identities := make(map[string]scenarioIdentity, len(snapshots))
	latest := make(map[scenarioIdentity]time.Time)
	for _, snap := range snapshots {
		tasks, err := e.tasksForRun(ctx, workspaceID, snap.RunSnapshotID, snap.RunSnapshotID)
		if err != nil {
			return nil, err
		}
		var identity scenarioIdentity
		identified := false
		for _, task := range tasks {
			var payload struct {
				Mode         string `json:"mode"`
				ScenarioID   string `json:"scenario_id"`
				InputVersion string `json:"input_version"`
			}
			if json.Unmarshal(task.Payload, &payload) != nil ||
				payload.Mode != "candidate_scenario_test" ||
				strings.TrimSpace(payload.ScenarioID) == "" ||
				strings.TrimSpace(payload.InputVersion) == "" {
				continue
			}
			identity = scenarioIdentity{
				RoundNo: snap.BuildRoundNo, ScenarioID: payload.ScenarioID,
				InputVersion: payload.InputVersion,
			}
			identified = true
			break
		}
		if !identified {
			continue
		}
		identities[snap.RunSnapshotID] = identity
		if current, exists := latest[identity]; !exists || snap.CreatedAt.After(current) {
			latest[identity] = snap.CreatedAt
		}
	}

	selected := make([]snapshotRef, 0, len(snapshots))
	for _, snap := range snapshots {
		identity, identified := identities[snap.RunSnapshotID]
		if !identified || snap.CreatedAt.Equal(latest[identity]) {
			selected = append(selected, snap)
		}
	}
	return selected, nil
}

// terminalFailureDetail retains the exact teamrun error code when a later
// evidence layer is absent. Evidence incompleteness must never replace the
// runtime cause with a business-quality diagnosis.
func terminalFailureDetail(record terminalRunRecord, detail string) string {
	code := teamrun.ErrorCode(strings.TrimSpace(record.StopReason))
	if !isSuccessRecordStatus(record.Status) && teamrun.ValidateErrorCode(code) {
		return fmt.Sprintf("original_error_code=%s; %s", code, detail)
	}
	return detail
}

// readRunRecord reads and decodes one schema-v3 terminal record from the
// audit:<workspace> namespace.
func (e *GateEvaluator) readRunRecord(
	ctx context.Context,
	workspaceID, runID string,
) (terminalRunRecord, error) {
	var record terminalRunRecord
	data, err := e.deps.Runs.Get(ctx, evidenceNamespacePrefix+workspaceID, runID)
	if err != nil {
		return record, err
	}
	if err := json.Unmarshal(data, &record); err != nil {
		return record, fmt.Errorf("decode terminal record: %w", err)
	}
	if record.SchemaVersion != 3 {
		return record, fmt.Errorf("unsupported terminal record schema_version %d", record.SchemaVersion)
	}
	if strings.TrimSpace(record.RunID) == "" || strings.TrimSpace(record.Status) == "" {
		return record, fmt.Errorf("terminal record is missing run_id or status")
	}
	return record, nil
}

// tasksForRun returns every task bound to one run id.
func (e *GateEvaluator) tasksForRun(
	ctx context.Context,
	workspaceID, runID, runSnapshotID string,
) ([]taskqueue.Task, error) {
	const batch = 500
	var matched []taskqueue.Task
	offset := 0
	for {
		tasks, _, err := e.deps.Tasks.List(ctx, workspaceID, batch, offset)
		if err != nil {
			return nil, err
		}
		for _, task := range tasks {
			if task.RunID == runID || task.RunSnapshotID == runSnapshotID {
				matched = append(matched, task)
			}
		}
		if len(tasks) < batch {
			return matched, nil
		}
		offset += len(tasks)
	}
}
