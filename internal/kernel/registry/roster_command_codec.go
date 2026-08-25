package registry

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/jinyitao123/weave/internal/base/frozen"
)

const teamRosterResultSchemaVersion = 1

type canonicalTeamRosterCommand struct {
	SchemaVersion     int                     `json:"schema_version"`
	WorkspaceID       string                  `json:"workspace_id"`
	TeamID            string                  `json:"team_id"`
	ExpectedUpdatedAt string                  `json:"expected_updated_at"`
	DesiredTeamStatus string                  `json:"desired_team_status"`
	LeadAgentID       string                  `json:"lead_agent_id"`
	Workers           []TeamRosterWorkerInput `json:"workers"`
	OperatorID        string                  `json:"operator_id"`
	Reason            string                  `json:"reason"`
}

func normalizeTeamRosterCommand(command TeamRosterCommand) (TeamRosterCommand, error) {
	normalized := command
	normalized.WorkspaceID = strings.TrimSpace(normalized.WorkspaceID)
	normalized.TeamID = strings.TrimSpace(normalized.TeamID)
	normalized.OperatorID = strings.TrimSpace(normalized.OperatorID)
	normalized.Reason = strings.TrimSpace(normalized.Reason)
	if normalized.IdempotencyKey != strings.TrimSpace(normalized.IdempotencyKey) {
		return TeamRosterCommand{}, fmt.Errorf("%w: idempotency_key must be trimmed", ErrTeamRosterInvalidRequest)
	}
	if normalized.WorkspaceID == "" || normalized.TeamID == "" || normalized.IdempotencyKey == "" ||
		normalized.OperatorID == "" || normalized.Reason == "" || normalized.LeadAgentID == "" {
		return TeamRosterCommand{}, fmt.Errorf("%w: command identity, operator, reason, and lead are required", ErrTeamRosterInvalidRequest)
	}
	if normalized.DesiredTeamStatus != "active" && normalized.DesiredTeamStatus != "archived" {
		return TeamRosterCommand{}, fmt.Errorf("%w: desired_team_status must be active or archived", ErrTeamRosterInvalidRequest)
	}
	if normalized.ExpectedUpdatedAt.IsZero() {
		return TeamRosterCommand{}, fmt.Errorf("%w: expected_updated_at is required", ErrTeamRosterInvalidRequest)
	}
	normalized.ExpectedUpdatedAt = normalized.ExpectedUpdatedAt.UTC().Truncate(time.Microsecond)
	if len(normalized.Workers) == 0 {
		return TeamRosterCommand{}, fmt.Errorf("%w: workers must not be empty", ErrTeamRosterInvalidRequest)
	}
	normalized.Workers = cloneTeamRosterWorkers(normalized.Workers)
	for index := range normalized.Workers {
		sortKinds(normalized.Workers[index].AllowedKinds)
	}
	sort.Slice(normalized.Workers, func(i, j int) bool {
		return normalized.Workers[i].WorkerAgentID < normalized.Workers[j].WorkerAgentID
	})
	if err := validateTeamRosterWorkerInputs(normalized.Workers); err != nil {
		return TeamRosterCommand{}, fmt.Errorf("%w: %v", ErrTeamRosterInvalidRequest, err)
	}
	return normalized, nil
}

func teamRosterCommandHash(command TeamRosterCommand) (TeamRosterCommand, string, error) {
	normalized, err := normalizeTeamRosterCommand(command)
	if err != nil {
		return TeamRosterCommand{}, "", err
	}
	semantic := canonicalTeamRosterCommand{
		SchemaVersion:     teamRosterResultSchemaVersion,
		WorkspaceID:       normalized.WorkspaceID,
		TeamID:            normalized.TeamID,
		ExpectedUpdatedAt: formatTeamRosterTime(normalized.ExpectedUpdatedAt),
		DesiredTeamStatus: normalized.DesiredTeamStatus,
		LeadAgentID:       normalized.LeadAgentID,
		Workers:           normalized.Workers,
		OperatorID:        normalized.OperatorID,
		Reason:            normalized.Reason,
	}
	raw, err := json.Marshal(semantic)
	if err != nil {
		return TeamRosterCommand{}, "", fmt.Errorf("encode team roster command: %w", err)
	}
	canonical, err := frozen.CanonicalizeJSON(raw)
	if err != nil {
		return TeamRosterCommand{}, "", fmt.Errorf("canonicalize team roster command: %w", err)
	}
	sum := sha256.Sum256(canonical)
	return normalized, hex.EncodeToString(sum[:]), nil
}

func encodeTeamRosterResult(result TeamRosterResult) ([]byte, error) {
	if err := validateTeamRosterResult(result); err != nil {
		return nil, err
	}
	return json.Marshal(result)
}

func decodeTeamRosterResult(raw []byte) (*TeamRosterResult, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var result TeamRosterResult
	if err := decoder.Decode(&result); err != nil {
		return nil, fmt.Errorf("decode frozen team roster response: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			return nil, errors.New("decode frozen team roster response: trailing JSON")
		}
		return nil, fmt.Errorf("decode frozen team roster response: %w", err)
	}
	if err := validateTeamRosterResult(result); err != nil {
		return nil, fmt.Errorf("decode frozen team roster response: %w", err)
	}
	return &result, nil
}

func validateTeamRosterResult(result TeamRosterResult) error {
	if result.SchemaVersion != teamRosterResultSchemaVersion || result.TeamID == "" ||
		(result.TeamStatus != "active" && result.TeamStatus != "archived") || result.LeadAgentID == "" {
		return errors.New("invalid team roster response identity")
	}
	parsedTime, err := time.Parse("2006-01-02T15:04:05.000000Z", result.UpdatedAt)
	if err != nil || formatTeamRosterTime(parsedTime) != result.UpdatedAt {
		return errors.New("invalid team roster response updated_at")
	}
	workers := cloneTeamRosterWorkers(result.Workers)
	for index := range workers {
		sortKinds(workers[index].AllowedKinds)
	}
	sort.Slice(workers, func(i, j int) bool { return workers[i].WorkerAgentID < workers[j].WorkerAgentID })
	if len(workers) == 0 || !reflect.DeepEqual(workers, result.Workers) || validateTeamRosterWorkerInputs(workers) != nil {
		return errors.New("team roster response workers are not canonical")
	}
	affected := append([]TeamRosterAffectedWorker{}, result.AffectedWorkers...)
	sort.Slice(affected, func(i, j int) bool { return affected[i].WorkerAgentID < affected[j].WorkerAgentID })
	if !reflect.DeepEqual(affected, result.AffectedWorkers) {
		return errors.New("team roster response affected workers are not canonical")
	}
	for index, worker := range affected {
		if worker.WorkerAgentID == "" || worker.AffectedPublishedVersionCount < 0 || worker.RevocationImpactURL == "" ||
			(index > 0 && affected[index-1].WorkerAgentID == worker.WorkerAgentID) {
			return errors.New("invalid team roster response affected worker")
		}
	}
	if result.Changed != (result.AuditID != nil) {
		return errors.New("team roster response changed/audit mismatch")
	}
	if !result.Changed && len(result.AffectedWorkers) != 0 {
		return errors.New("team roster no-op response has affected workers")
	}
	if result.AuditID != nil && *result.AuditID == "" {
		return errors.New("team roster response audit ID is empty")
	}
	return nil
}

func cloneTeamRosterWorkers(workers []TeamRosterWorkerInput) []TeamRosterWorkerInput {
	cloned := make([]TeamRosterWorkerInput, len(workers))
	copy(cloned, workers)
	for index := range cloned {
		cloned[index].AllowedKinds = append([]string(nil), cloned[index].AllowedKinds...)
	}
	return cloned
}

func sortKinds(kinds []string) {
	sort.Slice(kinds, func(i, j int) bool { return teamWorkerKindRank(kinds[i]) < teamWorkerKindRank(kinds[j]) })
}

func teamWorkerKindRank(kind string) int {
	switch kind {
	case "consult":
		return 0
	case "dispatch":
		return 1
	case "handoff":
		return 2
	default:
		return 3
	}
}

func formatTeamRosterTime(value time.Time) string {
	return value.UTC().Truncate(time.Microsecond).Format("2006-01-02T15:04:05.000000Z")
}
