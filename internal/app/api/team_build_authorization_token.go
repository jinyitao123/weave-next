package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/jinyitao123/weave/internal/build/teambuild"
)

func blueprintRevisionTokenFromRevision(revision teambuild.BlueprintRevision) teambuild.BlueprintRevisionToken {
	return teambuild.BlueprintRevisionToken{
		RevisionNo:    revision.RevisionNo,
		BlueprintHash: revision.BlueprintHash,
		ChangeSetHash: revision.ChangeSetHash,
	}
}

func (s *Server) latestDisplayedBlueprintRevisionToken(
	ctx context.Context,
	workspaceID, conversationID, buildRunID string,
) (*teambuild.BlueprintRevisionToken, bool, error) {
	if s == nil || s.Conversations == nil || strings.TrimSpace(conversationID) == "" {
		return nil, false, nil
	}
	messages, err := s.Conversations.ListMessages(ctx, workspaceID, conversationID, 200, 0)
	if err != nil {
		return nil, false, fmt.Errorf("load conversation messages for blueprint authorization: %w", err)
	}
	for index := len(messages) - 1; index >= 0; index-- {
		message := messages[index]
		if message.Role != "assistant" || len(message.Metadata) == 0 {
			continue
		}
		token, tokenBuildRunID, ok := extractBlueprintRevisionToken(message.Metadata)
		if !ok {
			continue
		}
		if tokenBuildRunID != "" && tokenBuildRunID != buildRunID {
			continue
		}
		return &token, true, nil
	}
	return nil, false, nil
}

func extractBlueprintRevisionToken(metadata json.RawMessage) (teambuild.BlueprintRevisionToken, string, bool) {
	var object map[string]json.RawMessage
	if err := json.Unmarshal(metadata, &object); err != nil {
		return teambuild.BlueprintRevisionToken{}, "", false
	}
	var buildRunID string
	if raw := object["blueprint_build_run_id"]; len(raw) > 0 {
		_ = json.Unmarshal(raw, &buildRunID)
	}
	rawToken := object["blueprint_revision_token"]
	if len(rawToken) == 0 {
		return teambuild.BlueprintRevisionToken{}, "", false
	}
	var token teambuild.BlueprintRevisionToken
	if err := json.Unmarshal(rawToken, &token); err != nil {
		return teambuild.BlueprintRevisionToken{}, "", false
	}
	if token.RevisionNo <= 0 || token.BlueprintHash == "" || token.ChangeSetHash == "" {
		return teambuild.BlueprintRevisionToken{}, "", false
	}
	return token, buildRunID, true
}

func (s *Server) currentBlueprintRevisionToken(
	ctx context.Context,
	workspaceID, buildRunID string,
) (*teambuild.BlueprintRevisionToken, bool, error) {
	if s == nil || s.TeamBuild == nil {
		return nil, false, nil
	}
	revision, err := s.TeamBuild.GetLatestBlueprintRevision(ctx, workspaceID, buildRunID)
	if errors.Is(err, teambuild.ErrBuildRunNotFound) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	token := blueprintRevisionTokenFromRevision(revision)
	return &token, true, nil
}

type blueprintSummaryMetadata struct {
	Mode               string                           `json:"mode,omitempty"`
	TeamName           string                           `json:"team_name,omitempty"`
	Goal               string                           `json:"goal,omitempty"`
	Template           string                           `json:"template,omitempty"`
	Members            []blueprintSummaryMemberMetadata `json:"members,omitempty"`
	WorkflowSummary    string                           `json:"workflow_summary,omitempty"`
	AcceptanceCriteria []string                         `json:"acceptance_criteria,omitempty"`
}

type blueprintSummaryMemberMetadata struct {
	Ref              string   `json:"ref"`
	Name             string   `json:"name"`
	DisplayName      string   `json:"display_name"`
	Role             string   `json:"role"`
	Duty             []string `json:"duty,omitempty"`
	Responsibilities []string `json:"responsibilities,omitempty"`
	Capabilities     []string `json:"capabilities,omitempty"`
}

func (s *Server) blueprintAuthorizationMetadata(
	ctx context.Context,
	workspaceID, buildRunID string,
	token teambuild.BlueprintRevisionToken,
) map[string]any {
	metadata := map[string]any{
		"blueprint_build_run_id":           buildRunID,
		"blueprint_revision_token":         token,
			"blueprint_authorization_semantic": teambuild.AuthorizationContinueBuild,
	}
	if summary, ok := s.blueprintSummaryForAuthorization(ctx, workspaceID, buildRunID); ok {
		metadata["team_blueprint_summary"] = summary
	}
	return metadata
}

func (s *Server) blueprintSummaryForAuthorization(
	ctx context.Context,
	workspaceID, buildRunID string,
) (blueprintSummaryMetadata, bool) {
	if s == nil || s.TeamBuild == nil {
		return blueprintSummaryMetadata{}, false
	}
	revision, err := s.TeamBuild.GetLatestBlueprintRevision(ctx, workspaceID, buildRunID)
	if err != nil {
		return blueprintSummaryMetadata{}, false
	}
	var blueprint teambuild.TeamBlueprintV1
	if err := json.Unmarshal(revision.BlueprintJSON, &blueprint); err != nil {
		return blueprintSummaryMetadata{}, false
	}
	summary := blueprintSummaryMetadata{
		Mode:               blueprint.Mode,
		TeamName:           strings.TrimSpace(blueprint.NewTeamName),
		Goal:               strings.TrimSpace(blueprint.Purpose),
		Template:           strings.TrimSpace(blueprint.Workflow.Template),
		AcceptanceCriteria: nil,
	}
	if blueprint.Workflow.Mode == teambuild.BlueprintWorkflowCustom {
		summary.Template = teambuild.BlueprintWorkflowCustom
	}
	if params := blueprint.Workflow.TemplateParameters; params != nil {
		summary.WorkflowSummary = strings.TrimSpace(params.LeadInstruction)
	}
	summary.Members = make([]blueprintSummaryMemberMetadata, 0, len(blueprint.Members))
	for _, member := range blueprint.Members {
		summary.Members = append(summary.Members, blueprintSummaryMemberMetadata{
			Ref:              member.StableRef,
			Name:             member.Name,
			DisplayName:      member.DisplayName,
			Role:             member.Role,
			Duty:             append([]string(nil), member.Responsibilities...),
			Responsibilities: append([]string(nil), member.Responsibilities...),
			Capabilities:     append([]string(nil), member.Capabilities...),
		})
	}
	run, err := s.TeamBuild.GetBuildRun(ctx, workspaceID, buildRunID)
	if err == nil {
		summary.AcceptanceCriteria = append([]string(nil), run.Brief.SuccessCriteria...)
		if blueprint.Mode == teambuild.ModeOptimize {
			summary.TeamName = s.optimizeBlueprintSummaryTeamName(ctx, workspaceID, run)
		}
	}
	return summary, true
}

func (s *Server) optimizeBlueprintSummaryTeamName(ctx context.Context, workspaceID string, run teambuild.TeamBuildRun) string {
	teamID := strings.TrimSpace(run.Brief.TeamID)
	if s != nil && s.OrgStore != nil && teamID != "" {
		if team, err := s.OrgStore.GetTeam(ctx, workspaceID, teamID); err == nil {
			return strings.TrimSpace(team.Name)
		}
	}
	if run.Baseline != nil {
		return strings.TrimSpace(run.Baseline.Team.Name)
	}
	return ""
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}
