package teameval

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/jinyitao123/weave/internal/kernel/registry"
	"github.com/jinyitao123/weave/internal/kernel/workflow"
	"github.com/jinyitao123/weave/internal/kernel/workflow/machine"
)

// skillNamespacePrefix mirrors the platform legacy skill namespace.
const skillNamespacePrefix = "skill:"

// evidenceNamespacePrefix mirrors the platform run-evidence namespace.
const evidenceNamespacePrefix = "audit:"

// loadEvaluation reads the frozen configuration behind one evaluation:
// the build run, the exact team, the lead version, every roster worker's
// exact agent version, and each workflow with its evaluated version. Reads
// are lenient: a missing or unresolvable record lands a nil entry plus its
// read error so the gates can emit evidence-backed failures instead of
// aborting the whole evaluation.
func (e *GateEvaluator) loadEvaluation(
	ctx context.Context,
	workspaceID, buildRunID, teamID string,
) (EvaluatedTeam, error) {
	if e == nil {
		return EvaluatedTeam{}, fmt.Errorf("gate evaluator is not configured")
	}
	if e.deps.BuildRuns == nil || e.deps.Teams == nil || e.deps.Roster == nil ||
		e.deps.Agents == nil || e.deps.Workflows == nil {
		return EvaluatedTeam{}, fmt.Errorf("gate evaluator stores are not configured")
	}
	if strings.TrimSpace(workspaceID) == "" || strings.TrimSpace(buildRunID) == "" ||
		strings.TrimSpace(teamID) == "" {
		return EvaluatedTeam{}, fmt.Errorf("workspace_id, build_run_id, and team_id are required")
	}

	var out EvaluatedTeam
	buildRun, err := e.deps.BuildRuns.GetBuildRun(ctx, workspaceID, buildRunID)
	if err != nil {
		return EvaluatedTeam{}, fmt.Errorf("read build run %q: %w", buildRunID, err)
	}
	out.BuildRun = buildRun

	teams, err := e.deps.Teams.ListTeams(ctx, workspaceID)
	if err != nil {
		return EvaluatedTeam{}, fmt.Errorf("list teams: %w", err)
	}
	for i := range teams {
		if teams[i].ID == teamID {
			out.Team = &teams[i]
			break
		}
	}
	if out.Team == nil {
		return out, nil
	}

	agentsList, err := e.deps.Agents.List(ctx, workspaceID)
	if err != nil {
		return EvaluatedTeam{}, fmt.Errorf("list agents: %w", err)
	}
	agentByID := make(map[string]registry.AgentRecord, len(agentsList))
	for _, record := range agentsList {
		agentByID[record.ID] = record
	}

	// Lead avatar at the exact version the team read pins.
	if record, ok := agentByID[out.Team.LeadAvatarID]; ok {
		exact, readErr := e.deps.Agents.GetVersion(
			ctx, workspaceID, record.ID, record.Version,
		)
		if readErr != nil {
			out.LeadErr = readErr.Error()
		} else {
			out.Lead = exact
		}
	} else {
		out.LeadErr = fmt.Sprintf("lead avatar %q not found", out.Team.LeadAvatarID)
	}

	roster, err := e.deps.Roster.ListByTeam(ctx, workspaceID, teamID)
	if err != nil {
		return EvaluatedTeam{}, fmt.Errorf("read roster for team %q: %w", teamID, err)
	}
	out.Workers = make([]EvaluatedWorker, 0, len(roster))
	for _, worker := range roster {
		entry := EvaluatedWorker{TeamWorker: worker}
		if record, ok := agentByID[worker.WorkerAgentID]; ok {
			exact, readErr := e.deps.Agents.GetVersion(
				ctx, workspaceID, record.ID, record.Version,
			)
			if readErr != nil {
				entry.Err = readErr.Error()
			} else {
				entry.Agent = exact
			}
		} else {
			entry.Err = fmt.Sprintf("worker agent %q not found", worker.WorkerAgentID)
		}
		out.Workers = append(out.Workers, entry)
	}
	sort.Slice(out.Workers, func(i, j int) bool {
		return out.Workers[i].WorkerAgentID() < out.Workers[j].WorkerAgentID()
	})

	teamWorkflows, err := e.deps.Workflows.ListByTeam(ctx, workspaceID, teamID)
	if err != nil {
		return EvaluatedTeam{}, fmt.Errorf("list team workflows: %w", err)
	}
	// Candidate qualification is scoped to the exact workflow identity frozen
	// into this build run's scenario snapshots. Other active drafts owned by the
	// same team are unrelated assets and must not contaminate the candidate's
	// gates. Legacy evaluations without build-bound snapshots retain the
	// historical team-wide behavior.
	evaluatedVersions := make(map[string]int)
	if e.deps.Snapshots != nil {
		snapshots, listErr := e.deps.Snapshots.ListByTeam(ctx, workspaceID, teamID)
		if listErr != nil {
			return EvaluatedTeam{}, fmt.Errorf("list team run snapshots: %w", listErr)
		}
		for _, runSnapshot := range snapshots {
			if runSnapshot.BuildRunID != buildRunID || runSnapshot.WorkflowID == "" || runSnapshot.WorkflowVersion < 1 {
				continue
			}
			if version, exists := evaluatedVersions[runSnapshot.WorkflowID]; exists && version != runSnapshot.WorkflowVersion {
				return EvaluatedTeam{}, fmt.Errorf(
					"build run %q snapshots disagree on workflow %q version",
					buildRunID, runSnapshot.WorkflowID,
				)
			}
			evaluatedVersions[runSnapshot.WorkflowID] = runSnapshot.WorkflowVersion
		}
	}
	out.Workflows = make([]EvaluatedWorkflow, 0, len(teamWorkflows))
	if len(teamWorkflows) > 0 {
		versions, err := e.deps.Workflows.ListVersionsByWorkflows(
			ctx, workspaceID, workflowIDs(teamWorkflows),
		)
		if err != nil {
			return EvaluatedTeam{}, fmt.Errorf("list workflow versions: %w", err)
		}
		versionsByWorkflow := groupVersions(versions)
		for _, wf := range teamWorkflows {
			if wf.Status == workflow.WorkflowStatusArchived {
				continue
			}
			if len(evaluatedVersions) != 0 {
				if _, selected := evaluatedVersions[wf.ID]; !selected {
					continue
				}
			}
			entry := EvaluatedWorkflow{Workflow: wf}
			if version, selected := evaluatedVersions[wf.ID]; selected {
				entry.Version = workflowVersion(version, versionsByWorkflow[wf.ID])
			} else {
				entry.Version = selectWorkflowVersion(wf, versionsByWorkflow[wf.ID])
			}
			if entry.Version != nil {
				entry.Trigger.DecodeErr, entry.Trigger.Trigger = decodeTrigger(entry.Version.TriggerConfig)
				entry.Graph.DecodeErr, entry.Graph.Graph = decodeGraph(entry.Version.GraphDefinition)
			} else {
				entry.Graph.DecodeErr = "workflow has no readable version"
				entry.Trigger.DecodeErr = "workflow has no readable version"
			}
			out.Workflows = append(out.Workflows, entry)
		}
		sort.Slice(out.Workflows, func(i, j int) bool {
			return out.Workflows[i].Workflow.ID < out.Workflows[j].Workflow.ID
		})
	}
	return out, nil
}

func workflowVersion(version int, versions []workflow.TeamWorkflowVersion) *workflow.TeamWorkflowVersion {
	for i := range versions {
		if versions[i].Version == version {
			return &versions[i]
		}
	}
	return nil
}

// WorkerAgentID is a helper on EvaluatedWorker.
func (w EvaluatedWorker) WorkerAgentID() string {
	return w.TeamWorker.WorkerAgentID
}

// AgentName returns the agent's stable name when the exact record is loaded.
func (w EvaluatedWorker) AgentName() string {
	if w.Agent == nil {
		return w.TeamWorker.WorkerAgentID
	}
	return w.Agent.Name
}

func workflowIDs(workflows []workflow.TeamWorkflow) []string {
	ids := make([]string, 0, len(workflows))
	for _, wf := range workflows {
		ids = append(ids, wf.ID)
	}
	return ids
}

func groupVersions(versions []workflow.TeamWorkflowVersion) map[string][]workflow.TeamWorkflowVersion {
	grouped := make(map[string][]workflow.TeamWorkflowVersion)
	for _, version := range versions {
		grouped[version.WorkflowID] = append(grouped[version.WorkflowID], version)
	}
	return grouped
}

// selectWorkflowVersion prefers the published version (the frozen content a
// business run consumes) and falls back to the highest draft when nothing is
// published yet (draft-phase evaluation).
func selectWorkflowVersion(
	wf workflow.TeamWorkflow,
	versions []workflow.TeamWorkflowVersion,
) *workflow.TeamWorkflowVersion {
	if wf.PublishedVersion != nil {
		for i := range versions {
			if versions[i].Version == *wf.PublishedVersion {
				return &versions[i]
			}
		}
	}
	if len(versions) == 0 {
		return nil
	}
	selected := versions[0]
	for i := 1; i < len(versions); i++ {
		if versions[i].Version > selected.Version {
			selected = versions[i]
		}
	}
	return &selected
}

func decodeTrigger(raw json.RawMessage) (string, machine.TriggerConfig) {
	trigger, report := machine.DecodeTriggerConfigV1(raw)
	if report != nil && len(report.Issues) != 0 {
		return firstIssueMessage(report), machine.TriggerConfig{}
	}
	return "", trigger
}

func decodeGraph(raw json.RawMessage) (string, machine.GraphDefinition) {
	graph, report := machine.DecodeGraphDefinitionV1(raw)
	if report != nil && len(report.Issues) != 0 {
		return firstIssueMessage(report), machine.GraphDefinition{}
	}
	return "", graph
}

func firstIssueMessage(report *machine.Report) string {
	if report == nil || len(report.Issues) == 0 {
		return ""
	}
	return report.Issues[0].Code + ": " + report.Issues[0].Message
}

// referencedAgentKeys returns every exact agent version a workflow graph
// references (worker + handoff nodes).
func referencedAgentKeys(graph GraphDefinition) []machine.AgentVersionKey {
	seen := make(map[machine.AgentVersionKey]bool)
	var keys []machine.AgentVersionKey
	for _, node := range graph.Nodes {
		switch node.Type {
		case machine.NodeWorker:
			config, ok := node.Config.(machine.WorkerConfig)
			if !ok {
				continue
			}
			key := machine.AgentVersionKey{AgentID: config.AgentID, AgentVersion: config.AgentVersion}
			if !seen[key] {
				seen[key] = true
				keys = append(keys, key)
			}
		case machine.NodeHandoff:
			config, ok := node.Config.(machine.HandoffConfig)
			if !ok {
				continue
			}
			key := machine.AgentVersionKey{AgentID: config.AgentID, AgentVersion: config.AgentVersion}
			if !seen[key] {
				seen[key] = true
				keys = append(keys, key)
			}
		}
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].AgentID != keys[j].AgentID {
			return keys[i].AgentID < keys[j].AgentID
		}
		return keys[i].AgentVersion < keys[j].AgentVersion
	})
	return keys
}
