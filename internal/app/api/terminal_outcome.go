package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/jinyitao123/weave/internal/build/teambuild"
)

const (
	terminalOutcomeSchemaVersion = 1
	maxAgentSummaryChars         = 2000

	turnStatePlanReady          = "plan_ready"
	turnStateNeedsClarification = "needs_clarification"
	turnStateBlocked            = "blocked"
	turnStateCompleted          = "completed"

	reasonBlueprintPlanBudgetExhausted    = "blueprint_plan_budget_exhausted"
	reasonBlueprintPlanningNoRevision     = "blueprint_planning_no_revision"
	reasonBlueprintPlanningTransitionFail = "blueprint_planning_transition_failed"
	reasonBlueprintValidationFailed       = "blueprint_validation_failed"
	reasonBlueprintChangeRequested        = "blueprint_change_requested"
	reasonCandidateRunFailed              = "candidate_run_failed"
	reasonInfrastructureFailure           = "infra_failure"
)

var (
	terminalHashFieldPattern  = regexp.MustCompile(`(?im)^[\t ]*(?:[-*][\t ]*)?(?:blueprint|change[_ ]?set|baseline)[_ ]?hash[\t ]*[:=][^\r\n]*(?:\r?\n|$)`)
	terminalUUIDPattern       = regexp.MustCompile(`(?i)\b[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}\b`)
	terminalPrefixedIDPattern = regexp.MustCompile(`(?i)\b(?:br|rt)-[a-z0-9][a-z0-9._:-]*\b`)
	terminalHexHashPattern    = regexp.MustCompile(`(?i)\b[0-9a-f]{64}\b`)
	terminalCredentialLabel   = regexp.MustCompile(`(?i)\b(?:build\s*run|blueprint[_ ]?hash|change[_ ]?set(?:[_ ]?hash)?|baseline[_ ]?hash|planned operations?|receipt|uuid)\b`)
	terminalChinesePattern    = regexp.MustCompile(`\p{Han}`)
)

// TerminalOutcome is the platform-authored conclusion of one create-team
// turn. BuildRunStatus is a persisted snapshot; TurnState describes the turn
// that just ended and therefore deliberately does not mirror in-flight run
// statuses one-for-one.
type TerminalOutcome struct {
	SchemaVersion  int                      `json:"schema_version"`
	TurnState      string                   `json:"turn_state"`
	BuildRunID     string                   `json:"build_run_id,omitempty"`
	BuildRunStatus string                   `json:"build_run_status,omitempty"`
	ReasonCode     string                   `json:"reason_code,omitempty"`
	AgentSummary   string                   `json:"agent_summary,omitempty"`
	Report         *TeamArchitectReport     `json:"report,omitempty"`
	ModeNote       *TerminalOutcomeModeNote `json:"mode_note,omitempty"`
}

type TeamArchitectReport struct {
	Conclusion string                      `json:"conclusion"`
	TeamName   string                      `json:"team_name"`
	Members    []TeamArchitectReportMember `json:"members"`
	NextAction string                      `json:"next_action"`
}

type TeamArchitectReportMember struct {
	Name string `json:"name"`
	Role string `json:"role"`
	Duty string `json:"duty"`
}

type TerminalOutcomeModeNote struct {
	Mode     string `json:"mode"`
	TeamName string `json:"team_name"`
}

func terminalOutcomeModeNote(run *teambuild.TeamBuildRun, teamName string) *TerminalOutcomeModeNote {
	if run == nil || run.Mode != teambuild.ModeOptimize {
		return nil
	}
	teamName = strings.TrimSpace(teamName)
	if teamName == "" && run.Baseline != nil {
		teamName = strings.TrimSpace(run.Baseline.Team.Name)
	}
	return &TerminalOutcomeModeNote{Mode: teambuild.ModeOptimize, TeamName: teamName}
}

func validateTeamArchitectReport(raw string, outcome TerminalOutcome) (*TeamArchitectReport, error) {
	var requiredFields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(strings.TrimSpace(raw)), &requiredFields); err != nil {
		return nil, err
	}
	for _, field := range []string{"conclusion", "team_name", "members", "next_action"} {
		if _, ok := requiredFields[field]; !ok {
			return nil, fmt.Errorf("team architect report is missing %s", field)
		}
	}
	decoder := json.NewDecoder(bytes.NewBufferString(strings.TrimSpace(raw)))
	decoder.DisallowUnknownFields()
	var report TeamArchitectReport
	if err := decoder.Decode(&report); err != nil {
		return nil, err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return nil, fmt.Errorf("team architect report must contain exactly one JSON object")
	}
	report.Conclusion = strings.TrimSpace(report.Conclusion)
	report.TeamName = strings.TrimSpace(report.TeamName)
	report.NextAction = strings.TrimSpace(report.NextAction)
	if report.Conclusion == "" || utf8.RuneCountInString(report.Conclusion) > 300 ||
		strings.ContainsAny(report.Conclusion, "\r\n") || terminalSentenceEndCount(report.Conclusion) > 1 ||
		!terminalChinesePattern.MatchString(report.Conclusion) {
		return nil, fmt.Errorf("team architect conclusion must be one bounded Chinese sentence")
	}
	if utf8.RuneCountInString(report.TeamName) > 200 {
		return nil, fmt.Errorf("team architect team_name is too long")
	}
	if report.Members == nil || len(report.Members) > 50 {
		return nil, fmt.Errorf("team architect members must be an array with at most 50 entries")
	}
	for index := range report.Members {
		member := &report.Members[index]
		member.Name = strings.TrimSpace(member.Name)
		member.Role = strings.TrimSpace(member.Role)
		member.Duty = strings.TrimSpace(member.Duty)
		if member.Name == "" || member.Role == "" || member.Duty == "" ||
			!terminalChinesePattern.MatchString(member.Name) ||
			!terminalChinesePattern.MatchString(member.Role) ||
			!terminalChinesePattern.MatchString(member.Duty) ||
			utf8.RuneCountInString(member.Name) > 100 || utf8.RuneCountInString(member.Role) > 100 || utf8.RuneCountInString(member.Duty) > 500 {
			return nil, fmt.Errorf("team architect member %d must use bounded Chinese display fields", index)
		}
	}
	wantAction := terminalNextAction(outcome.TurnState)
	if report.NextAction != wantAction {
		return nil, fmt.Errorf("team architect next_action %q does not match terminal state", report.NextAction)
	}
	for _, value := range teamArchitectReportStrings(report) {
		if containsTerminalMachineCredential(value) {
			return nil, fmt.Errorf("team architect report contains machine credentials")
		}
	}
	if terminalUUIDPattern.MatchString(report.TeamName) || terminalHexHashPattern.MatchString(report.TeamName) || terminalCredentialLabel.MatchString(report.TeamName) {
		return nil, fmt.Errorf("team architect report contains a machine team identifier")
	}
	return &report, nil
}

func terminalSentenceEndCount(value string) int {
	count := 0
	for _, marker := range []string{"。", "！", "？", "!", "?"} {
		count += strings.Count(value, marker)
	}
	return count
}

func containsTerminalMachineCredential(value string) bool {
	for _, pattern := range []*regexp.Regexp{
		terminalUUIDPattern, terminalPrefixedIDPattern, terminalHexHashPattern, terminalCredentialLabel,
	} {
		if pattern.MatchString(value) {
			return true
		}
	}
	return false
}

func teamArchitectReportStrings(report TeamArchitectReport) []string {
	values := []string{report.Conclusion, report.NextAction}
	for _, member := range report.Members {
		values = append(values, member.Name, member.Role, member.Duty)
	}
	return values
}

func teamArchitectReportForTerminal(raw string, outcome TerminalOutcome, summary *blueprintSummaryMetadata) *TeamArchitectReport {
	report, err := validateTeamArchitectReport(raw, outcome)
	if err == nil && teamArchitectReportMatchesSummary(report, summary) {
		return report
	}
	return platformTeamArchitectReport(outcome, summary)
}

func teamArchitectReportMatchesSummary(report *TeamArchitectReport, summary *blueprintSummaryMetadata) bool {
	if report == nil || summary == nil {
		return report != nil
	}
	if strings.TrimSpace(report.TeamName) != strings.TrimSpace(summary.TeamName) || len(report.Members) != len(summary.Members) {
		return false
	}
	wantNames := make(map[string]int, len(summary.Members))
	for _, member := range summary.Members {
		wantNames[strings.TrimSpace(firstNonEmpty(member.DisplayName, member.Name))]++
	}
	for _, member := range report.Members {
		name := strings.TrimSpace(member.Name)
		if wantNames[name] == 0 {
			return false
		}
		wantNames[name]--
	}
	return true
}

func terminalNextAction(turnState string) string {
	switch turnState {
	case turnStatePlanReady:
		return "等待继续"
	case turnStateBlocked:
		return "已受阻"
	case turnStateNeedsClarification:
		return "需澄清"
	default:
		return "已完成"
	}
}

func platformTeamArchitectReport(outcome TerminalOutcome, summary *blueprintSummaryMetadata) *TeamArchitectReport {
	report := &TeamArchitectReport{
		Conclusion: projectTerminalOutcome(outcome),
		Members:    []TeamArchitectReportMember{},
		NextAction: terminalNextAction(outcome.TurnState),
	}
	if summary == nil {
		if outcome.ModeNote != nil {
			report.TeamName = safeTerminalTeamName(outcome.ModeNote.TeamName)
		}
		return report
	}
	report.TeamName = safeTerminalTeamName(summary.TeamName)
	for _, member := range summary.Members {
		name := strings.TrimSpace(firstNonEmpty(member.DisplayName, member.Name))
		if containsTerminalMachineCredential(name) || !terminalChinesePattern.MatchString(name) {
			name = "团队成员"
		}
		role := "团队成员"
		if member.Role == teambuild.BlueprintMemberRoleAvatar {
			role = "团队负责人"
		}
		duty := "按团队方案承担分工"
		if len(member.Responsibilities) > 0 && strings.TrimSpace(member.Responsibilities[0]) != "" {
			duty = strings.TrimSpace(member.Responsibilities[0])
		}
		if containsTerminalMachineCredential(duty) || !terminalChinesePattern.MatchString(duty) {
			duty = "按团队方案承担分工"
		}
		report.Members = append(report.Members, TeamArchitectReportMember{Name: name, Role: role, Duty: duty})
	}
	return report
}

func safeTerminalTeamName(value string) string {
	value = strings.TrimSpace(value)
	if terminalUUIDPattern.MatchString(value) || terminalHexHashPattern.MatchString(value) || terminalCredentialLabel.MatchString(value) {
		return ""
	}
	return value
}

// terminalReasonCode is the only transition-ledger reason to public reason
// code mapping. It never returns raw ledger text.
func terminalReasonCode(reason string) string {
	normalized := strings.ToLower(strings.TrimSpace(reason))
	switch {
	case normalized == "":
		return ""
	case normalized == reasonBlueprintPlanBudgetExhausted || strings.Contains(normalized, reasonBlueprintPlanBudgetExhausted):
		return reasonBlueprintPlanBudgetExhausted
	case normalized == reasonBlueprintPlanningTransitionFail || strings.Contains(normalized, reasonBlueprintPlanningTransitionFail):
		return reasonBlueprintPlanningTransitionFail
	case normalized == reasonBlueprintPlanningNoRevision || strings.Contains(normalized, reasonBlueprintPlanningNoRevision):
		return reasonBlueprintPlanningNoRevision
	case normalized == reasonBlueprintValidationFailed || strings.Contains(normalized, reasonBlueprintValidationFailed):
		return reasonBlueprintValidationFailed
	case normalized == reasonBlueprintChangeRequested || strings.Contains(normalized, reasonBlueprintChangeRequested):
		return reasonBlueprintChangeRequested
	case isCandidateRunFailureReason(normalized):
		return reasonCandidateRunFailed
	case normalized == reasonInfrastructureFailure,
		strings.HasPrefix(normalized, "execution_failed:"),
		strings.Contains(normalized, "infrastructure"),
		strings.Contains(normalized, "infra_"),
		strings.Contains(normalized, "runtime_incompatible"),
		strings.Contains(normalized, "service_unavailable"):
		return reasonInfrastructureFailure
	default:
		slog.Warn("unknown terminal transition reason", "reason", reason)
		return ""
	}
}

func isCandidateRunFailureReason(reason string) bool {
	if reason == reasonCandidateRunFailed ||
		strings.Contains(reason, reasonCandidateRunFailed) ||
		strings.Contains(reason, "candidate test run failed") ||
		strings.Contains(reason, "candidate ref") ||
		reason == "evaluation_infrastructure_error" {
		return true
	}
	for _, code := range []string{
		"team_run_identity_mismatch",
		"team_run_runtime_incompatible",
		"team_run_unexpected_interactive_yield",
		"team_run_output_invalid",
		"team_run_node_output_invalid",
		"team_run_delivery_unavailable",
		"team_run_execution_unrecoverable",
	} {
		if strings.Contains(reason, code) {
			return true
		}
	}
	return false
}

func terminalOutcomeForCreateTeam(
	run *teambuild.TeamBuildRun,
	transitionReason string,
	discovery *discoveryOutput,
	hasBlueprint bool,
) TerminalOutcome {
	outcome := TerminalOutcome{SchemaVersion: terminalOutcomeSchemaVersion}
	reasonCode := terminalReasonCode(transitionReason)
	if run != nil {
		outcome.BuildRunID = run.BuildRunID
		outcome.BuildRunStatus = run.Status
	}

	switch {
	case reasonCode == reasonBlueprintPlanningTransitionFail:
		outcome.TurnState = turnStateBlocked
	case run == nil:
		if discovery != nil && discovery.Status == "blocked" {
			outcome.TurnState = turnStateBlocked
		} else {
			outcome.TurnState = turnStateNeedsClarification
		}
	case run.Status == teambuild.StatusBlocked || run.Status == "failed":
		outcome.TurnState = turnStateBlocked
	case discovery != nil && discovery.Status == "blocked":
		outcome.TurnState = turnStateBlocked
	case run.Status == teambuild.StatusPlanning && hasBlueprint:
		outcome.TurnState = turnStatePlanReady
	case discovery != nil && discovery.Status == "needs_clarification":
		outcome.TurnState = turnStateNeedsClarification
	default:
		outcome.TurnState = turnStateCompleted
	}

	outcome.ReasonCode = reasonCode
	if discovery != nil {
		outcome.AgentSummary = trustedAgentSummary(
			discovery.Summary, discovery.Status, outcome.TurnState,
		)
	}
	return outcome
}

func trustedAgentSummary(summary, discoveryStatus, turnState string) string {
	summary = strings.TrimSpace(summary)
	if summary == "" || utf8.RuneCountInString(summary) > maxAgentSummaryChars {
		return ""
	}
	if turnState == turnStateBlocked {
		return ""
	}
	wantState := ""
	switch discoveryStatus {
	case "planning_created":
		wantState = turnStatePlanReady
	case "needs_clarification":
		wantState = turnStateNeedsClarification
	case "blocked":
		wantState = turnStateBlocked
	}
	if wantState == "" || wantState != turnState {
		return ""
	}
	return redactTerminalMachineCredentials(summary)
}

func redactTerminalMachineCredentials(text string) string {
	text = terminalHashFieldPattern.ReplaceAllString(text, "")
	for _, pattern := range []*regexp.Regexp{
		terminalUUIDPattern,
		terminalPrefixedIDPattern,
		terminalHexHashPattern,
	} {
		text = pattern.ReplaceAllString(text, "（已省略）")
	}
	return strings.TrimSpace(text)
}

func terminalOutcomeAssistantFields(outcome TerminalOutcome) map[string]any {
	fields := map[string]any{"terminal_outcome": outcome}
	if outcome.BuildRunID != "" {
		fields["blueprint_build_run_id"] = outcome.BuildRunID
	}
	return fields
}

// projectTerminalOutcome is the compatibility projection for result.Output.
// Terminal Chinese copy is generated nowhere else in the backend.
func projectTerminalOutcome(outcome TerminalOutcome) string {
	switch outcome.TurnState {
	case turnStatePlanReady:
		return "团队方案已生成。请在下方卡片中点击「继续构建」。"
	case turnStateNeedsClarification:
		return "本轮尚未形成可执行方案或明确的待确认问题。请补充目标或关键约束后继续。"
	case turnStateBlocked:
		switch outcome.ReasonCode {
		case reasonCandidateRunFailed:
			return "试运行（业务验收）执行失败，本次构建已暂停。"
		case reasonBlueprintPlanBudgetExhausted:
			return "蓝图连续未通过平台校验，规划预算已耗尽，本次构建已暂停。"
		case reasonBlueprintPlanningNoRevision:
			return "本轮规划没有产出可批准的团队蓝图，本次构建已暂停。"
		case reasonBlueprintPlanningTransitionFail:
			return "团队蓝图规划未能写入终态，本次构建已暂停，请检查平台运行状态。"
		case reasonBlueprintValidationFailed:
			return "团队蓝图未通过平台校验，本次构建已暂停。"
		case reasonInfrastructureFailure:
			return "平台基础设施异常导致本轮无法继续，本次构建已暂停。"
		default:
			return "团队方案未能完成规划或构建，本次构建已暂停。"
		}
	case turnStateCompleted:
		return "本轮处理已完成。"
	default:
		return "本轮处理已完成。"
	}
}
