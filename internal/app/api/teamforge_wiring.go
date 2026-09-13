package api

import (
	"context"
	"errors"
	"log/slog"
	"strings"

	"github.com/jinyitao123/loom/contract"
	conversationstore "github.com/jinyitao123/weave/internal/app/conversation"
	"github.com/jinyitao123/weave/internal/app/metateam"
	"github.com/jinyitao123/weave/internal/app/teamassets"
	"github.com/jinyitao123/weave/internal/build/teambuild"
	"github.com/jinyitao123/weave/internal/build/teamforge"
)

type teamForgeOperatorContextKey struct{}

type teamForgeOperator struct {
	UserID string
	Admin  bool
}

func contextWithTeamForgeOperator(ctx context.Context, userID string, roles []string) context.Context {
	operator := teamForgeOperator{UserID: userID}
	for _, role := range roles {
		if role == "admin" {
			operator.Admin = true
			break
		}
	}
	return context.WithValue(ctx, teamForgeOperatorContextKey{}, operator)
}

func teamForgeOperatorFromContext(ctx context.Context) teamForgeOperator {
	operator, _ := ctx.Value(teamForgeOperatorContextKey{}).(teamForgeOperator)
	return operator
}

// teamForgeDeps assembles the production read-side Deps mapping from the
// Server stores (plan §10.1 / T08 survey): Agents→Registry,
// Teams/DispatchRules→OrgStore, Roster→TeamWorkerRepository,
// Workflows→Workflow, Skills/Runs→s.Store namespace reader (type assertion),
// MCPs→MCPRegistry, Providers→Credentials, Runtimes→Runtimes, Tasks→Tasks,
// Deliverables→Deliverables.
func (s *Server) teamForgeDeps() teamforge.Deps {
	deps := teamforge.Deps{
		Agents:        s.Registry,
		Teams:         s.OrgStore,
		Roster:        s.TeamWorkers,
		DispatchRules: s.OrgStore,
		Workflows:     s.Workflow,
		MCPs:          s.MCPRegistry,
		Providers:     s.Credentials,
		Runtimes:      s.Runtimes,
		Tasks:         s.Tasks,
		Deliverables:  s.Deliverables,
	}
	if ns, ok := s.Store.(teamforge.NamespaceReader); ok {
		deps.Skills = ns
		deps.Runs = ns
	}
	return deps
}

// teamForgeWriteDeps binds atomic product asset commands and narrow readers.
// Model resolution and the agent version commit remain inside AgentWriter.
func (s *Server) teamForgeWriteDeps() teamforge.WriteDeps {
	return teamforge.WriteDeps{
		Agents:        &teamassets.AgentWriter{Pool: s.Pool, Registry: s.Registry},
		AgentLoad:     s.Registry,
		Runtimes:      s.Runtimes,
		Teams:         s.OrgStore,
		TeamDesign:    s.OrgStore,
		Roster:        s.Registry,
		DispatchRules: s.OrgStore,
		Workflows:     s.Workflow,
	}
}

// teamForgePlatformTools returns the teamforge dispatchers one agent receives
// for one conversation. A brand-new conversation or a conversation whose
// latest run is terminal receives bounded replanning or discovery tools; a
// live run receives its status/role-specific set; a non-meta-team agent
// receives none. The
// wiring is deliberately fail-closed: a nil audit/store, failed latest lookup,
// active/nonterminal latest mismatch, or failed receipt reissue attaches no
// tools.
func (s *Server) teamForgePlatformTools(
	ctx context.Context,
	workspaceID, conversationID, agentName string,
) []contract.ToolDispatcher {
	if !s.metaTeamEnabled() || conversationID == "" || s.TeamBuild == nil || s.Audit == nil {
		return nil
	}
	run, err := s.TeamBuild.GetActiveBuildRunByConversation(ctx, workspaceID, conversationID)
	if err != nil {
		if errors.Is(err, teambuild.ErrBuildRunNotFound) {
			dispatchers := make([]contract.ToolDispatcher, 0, 3)
			latest, latestErr := s.TeamBuild.GetLatestBuildRunByConversation(ctx, workspaceID, conversationID)
			enableDiscoveryReads := false
			switch {
			case errors.Is(latestErr, teambuild.ErrBuildRunNotFound):
				enableDiscoveryReads = true
			case latestErr != nil:
				slog.Warn(
					"teamforge latest build run lookup failed; discovery tools disabled",
					"workspace", workspaceID, "conversation", conversationID,
					"agent", agentName, "error", latestErr,
				)
				return nil
			case teamForgeBuildRunStatusIsTerminal(latest.Status):
				enableDiscoveryReads = true
			default:
				slog.Warn(
					"teamforge latest build run is nonterminal without an active run; tools disabled",
					"workspace", workspaceID, "conversation", conversationID,
					"agent", agentName, "build_run", latest.BuildRunID,
					"status", latest.Status,
				)
				return nil
			}
			if enableDiscoveryReads && teamforge.DecideToolSet(agentName, teambuild.StatusPlanning).Read {
				if latestErr == nil && latest.Status == teambuild.StatusBlocked {
					dispatchers = append(dispatchers,
						teamforge.NewReplanningReadTools(workspaceID, conversationID, agentName, latest.BuildRunID, s.TeamBuild, s.Audit, s.teamForgeDeps()),
					)
				} else {
					dispatchers = append(dispatchers,
						teamforge.NewDiscoveryReadTools(workspaceID, agentName, s.Audit, s.teamForgeDeps()),
					)
				}
			}
			if enableDiscoveryReads && agentName == metateam.TeamArchitectName {
				dispatchers = append(dispatchers,
					teamforge.NewRenderTemplateDraftTools(workspaceID, agentName, s.Audit),
				)
			}
			if s.canSubmitTeamBuildBrief(ctx, workspaceID, conversationID, agentName) {
				operator := teamForgeOperatorFromContext(ctx)
				dispatchers = append(dispatchers, teamforge.NewSubmitBriefTools(
					workspaceID, conversationID, agentName, operator.UserID,
					s.TeamBuild, s.teamForgeDeps().Teams, s.teamForgeDeps().Roster, s.teamForgeDeps().Workflows, s.Audit, nil, 0,
				))
			}
			if planning := s.deferredBlueprintPlanningTool(workspaceID, conversationID, agentName); planning != nil {
				dispatchers = append(dispatchers, planning)
			}
			return dispatchers
		}
		slog.Warn(
			"teamforge active build run lookup failed; teamforge tools disabled",
			"workspace", workspaceID,
			"conversation", conversationID,
			"agent", agentName,
			"error", err,
		)
		return nil
	}
	decision := teamforge.DecideToolSetForMode(agentName, run.Status, run.Mode)
	if decision.IsZero() {
		return nil
	}
	if decision.HasWrites() && s.TeamForgeDrafts == nil {
		return nil
	}

	var receipt teambuild.BuildAuthorizationReceipt
	if decision.HasWrites() {
		receipt, err = s.TeamBuild.ReissueReceipt(ctx, workspaceID, run.BuildRunID)
		if err != nil {
			slog.Warn(
				"teamforge receipt reissue failed; teamforge tools disabled",
				"workspace", workspaceID,
				"run", run.BuildRunID,
				"agent", agentName,
				"error", err,
			)
			return nil
		}
	}

	deps := s.teamForgeDeps()
	writeDeps := s.teamForgeWriteDeps()
	dispatchers := make([]contract.ToolDispatcher, 0, 5)
	if decision.Read {
		if run.Status == teambuild.StatusPlanning {
			dispatchers = append(dispatchers,
				teamforge.NewPlanningReadTools(workspaceID, agentName, run.BuildRunID, s.TeamBuild, s.Audit, deps))
		} else {
			dispatchers = append(dispatchers,
				teamforge.NewReadTools(workspaceID, agentName, run.BuildRunID, s.TeamBuild, s.Audit, deps))
		}
	}
	if run.Status == teambuild.StatusPlanning {
		if planning := s.blueprintPlanningTool(workspaceID, run.BuildRunID, agentName); planning != nil {
			dispatchers = append(dispatchers, planning)
		}
	}
	if decision.AgentWrite {
		dispatchers = append(dispatchers,
			teamforge.NewWriteTools(workspaceID, agentName, receipt, s.TeamBuild, s.Audit, writeDeps))
	}
	if decision.TeamWrite {
		dispatchers = append(dispatchers,
			teamforge.NewTeamWriteTools(workspaceID, agentName, receipt, s.TeamBuild, s.Audit, deps, writeDeps))
	}
	if decision.GraphWrite {
		dispatchers = append(dispatchers,
			teamforge.NewGraphBuildTools(
				workspaceID, agentName, receipt, s.TeamBuild, s.Audit,
				writeDeps, s.TeamForgeDrafts.GraphDrafts(run.BuildRunID),
			))
	}
	if decision.WorkflowWrite {
		workflowTools := teamforge.NewWorkflowBuildTools(
			workspaceID, agentName, receipt, s.TeamBuild, s.Audit,
			deps, writeDeps, s.TeamForgeDrafts.WorkflowDrafts(run.BuildRunID),
		)
		if run.Brief.EffectiveWorkflowBuildMode() == teambuild.WorkflowBuildModeCustom {
			workflowTools = teamforge.NewCustomWorkflowBuildTools(
				workspaceID, agentName, receipt, s.TeamBuild, s.Audit,
				deps, writeDeps, s.TeamForgeDrafts.WorkflowDrafts(run.BuildRunID),
			)
		}
		dispatchers = append(dispatchers, workflowTools)
	}
	return dispatchers
}

func teamForgeBuildRunStatusIsTerminal(status string) bool {
	switch status {
	case teambuild.StatusPassed, teambuild.StatusBlocked, teambuild.StatusCancelled:
		return true
	default:
		return false
	}
}

func (s *Server) canSubmitTeamBuildBrief(
	ctx context.Context,
	workspaceID, conversationID, agentName string,
) bool {
	if s == nil || s.Conversations == nil || agentName != metateam.TeamArchitectName {
		return false
	}
	operator := teamForgeOperatorFromContext(ctx)
	if !operator.Admin || strings.TrimSpace(operator.UserID) == "" {
		return false
	}
	conversation, err := s.Conversations.GetConversation(ctx, workspaceID, conversationID)
	return err == nil && conversation.Intent == conversationstore.IntentCreateTeam
}
