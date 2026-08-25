package loomruntime

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"strconv"
)

const terminalSchemaVersionV3 = 3

// TerminalUsage is the shared usage shape for exclusive, breakdown, and subtree values.
type TerminalUsage struct {
	InputTokens  int     `json:"input_tokens"`
	OutputTokens int     `json:"output_tokens"`
	CostUSD      float64 `json:"cost_usd"`
}

// TerminalChildBreakdownV3 records one descendant's exclusive usage.
type TerminalChildBreakdownV3 struct {
	RunID           string        `json:"run_id"`
	ParentRunID     string        `json:"parent_run_id"`
	ParentSeq       int64         `json:"parent_seq"`
	Agent           string        `json:"agent"`
	TeamID          *string       `json:"team_id,omitempty"`
	WorkflowID      *string       `json:"workflow_id,omitempty"`
	WorkflowVersion *int          `json:"workflow_version,omitempty"`
	RunSnapshotID   *string       `json:"run_snapshot_id,omitempty"`
	SelfExclusive   TerminalUsage `json:"self_exclusive"`
}

// TerminalEntryV3 is the exact schema-v3 terminal candidate.
type TerminalEntryV3 struct {
	SchemaVersion          int                      `json:"schema_version"`
	RunID                  string                   `json:"run_id"`
	Agent                  string                   `json:"agent"`
	Tenant                 string                   `json:"tenant"`
	AttributionScope       TerminalAttributionScope `json:"attribution_scope"`
	TeamID                 *string                  `json:"team_id,omitempty"`
	WorkflowID             *string                  `json:"workflow_id,omitempty"`
	WorkflowVersion        *int                     `json:"workflow_version,omitempty"`
	RunSnapshotID          *string                  `json:"run_snapshot_id,omitempty"`
	ConversationID         *string                  `json:"conversation_id,omitempty"`
	ParentRunID            *string                  `json:"parent_run_id,omitempty"`
	ParentSeq              *int64                   `json:"parent_seq,omitempty"`
	AggregationParentRunID *string                  `json:"aggregation_parent_run_id,omitempty"`
	TaskGroupID            *string                  `json:"task_group_id,omitempty"`
	Status                 string                   `json:"status"`
	StopReason             string                   `json:"stop_reason"`
	StartedAt              string                   `json:"started_at"`
	EndedAt                string                   `json:"ended_at"`
	DurationMs             int64                    `json:"duration_ms"`
	TokensIn               int                      `json:"tokens_in"`
	TokensOut              int                      `json:"tokens_out"`
	CostUSD                float64                  `json:"cost_usd"`
	Step                   string                   `json:"step,omitempty"`
	Summary                string                   `json:"summary,omitempty"`
	// UsageComplete is nil (or true) when the entry's usage covers every
	// executed node, and explicitly false when some executed node has no
	// measurable usage receipt (e.g. candidate fanout legs or a CLI runtime
	// agent). UsageIncompleteReason names the unmeasured part. Old records
	// omit both fields and read as usage-complete.
	UsageComplete         *bool                      `json:"usage_complete,omitempty"`
	UsageIncompleteReason string                     `json:"usage_incomplete_reason,omitempty"`
	SelfExclusive         TerminalUsage              `json:"self_exclusive"`
	ChildBreakdown        []TerminalChildBreakdownV3 `json:"child_breakdown"`
	SubtreeTotal          TerminalUsage              `json:"subtree_total"`
}

// TerminalRecordClassification is the stable local inspection outcome.
type TerminalRecordClassification string

const (
	TerminalRecordValid             TerminalRecordClassification = "valid"
	TerminalRecordPreAttribution    TerminalRecordClassification = "pre_attribution"
	TerminalRecordMissing           TerminalRecordClassification = "terminal_missing"
	TerminalRecordAssociationDefect TerminalRecordClassification = "association_defect"
	TerminalRecordCorrupt           TerminalRecordClassification = "terminal_corrupt"
	TerminalRecordLineageDefect     TerminalRecordClassification = "lineage_defect"
)

// TerminalRecordInspection reports a pure schema classification without touching storage.
type TerminalRecordInspection struct {
	Classification TerminalRecordClassification
	Entry          *TerminalEntryV3
	Err            error
}

type terminalRecordValidationError struct {
	class TerminalRecordClassification
	msg   string
}

func (e *terminalRecordValidationError) Error() string { return e.msg }

func terminalRecordError(class TerminalRecordClassification, format string, args ...any) error {
	return &terminalRecordValidationError{class: class, msg: fmt.Sprintf(format, args...)}
}

// InspectTerminalRecord classifies one caller-provided audit value.
func InspectTerminalRecord(present bool, data []byte) TerminalRecordInspection {
	if !present {
		return TerminalRecordInspection{Classification: TerminalRecordMissing}
	}

	schemaVersion, err := inspectTerminalSchemaVersion(data)
	if err != nil {
		return TerminalRecordInspection{Classification: TerminalRecordCorrupt, Err: err}
	}
	if schemaVersion == 1 || schemaVersion == 2 {
		return TerminalRecordInspection{Classification: TerminalRecordPreAttribution}
	}
	if schemaVersion != terminalSchemaVersionV3 {
		return TerminalRecordInspection{
			Classification: TerminalRecordCorrupt,
			Err:            fmt.Errorf("unsupported terminal schema_version %d", schemaVersion),
		}
	}

	var entry TerminalEntryV3
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&entry); err != nil {
		return TerminalRecordInspection{
			Classification: TerminalRecordCorrupt,
			Err:            fmt.Errorf("decode terminal schema-v3: %w", err),
		}
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return TerminalRecordInspection{
			Classification: TerminalRecordCorrupt,
			Err:            fmt.Errorf("decode terminal schema-v3: trailing JSON content"),
		}
	}
	if err := validateTerminalV3JSONPresence(data); err != nil {
		class := TerminalRecordCorrupt
		if classified, ok := err.(*terminalRecordValidationError); ok {
			class = classified.class
		}
		return TerminalRecordInspection{Classification: class, Err: err}
	}
	if err := validateTerminalV3(entry); err != nil {
		class := TerminalRecordCorrupt
		if classified, ok := err.(*terminalRecordValidationError); ok {
			class = classified.class
		}
		return TerminalRecordInspection{Classification: class, Err: err}
	}
	return TerminalRecordInspection{Classification: TerminalRecordValid, Entry: &entry}
}

func inspectTerminalSchemaVersion(data []byte) (int, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return 0, fmt.Errorf("decode terminal record: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return 0, fmt.Errorf("decode terminal record: trailing JSON content")
	}
	object, ok := value.(map[string]any)
	if !ok || object == nil {
		return 0, fmt.Errorf("terminal record must be a JSON object")
	}
	rawVersion, exists := object["schema_version"]
	if !exists {
		return 0, fmt.Errorf("terminal schema_version is required")
	}
	number, ok := rawVersion.(json.Number)
	if !ok {
		return 0, fmt.Errorf("terminal schema_version must be an integer")
	}
	version, err := strconv.ParseInt(number.String(), 10, 32)
	if err != nil {
		return 0, fmt.Errorf("terminal schema_version must be an integer")
	}
	return int(version), nil
}

func validateTerminalV3JSONPresence(data []byte) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil || fields == nil {
		return fmt.Errorf("terminal schema-v3 must be a JSON object")
	}
	required := []string{
		"schema_version",
		"run_id",
		"agent",
		"tenant",
		"status",
		"stop_reason",
		"started_at",
		"ended_at",
		"duration_ms",
		"tokens_in",
		"tokens_out",
		"cost_usd",
		"self_exclusive",
		"child_breakdown",
		"subtree_total",
	}
	if err := requireTerminalJSONFields(fields, required); err != nil {
		return err
	}
	rawScope, scopeExists := fields["attribution_scope"]
	if !scopeExists || bytes.Equal(bytes.TrimSpace(rawScope), []byte("null")) {
		return terminalRecordError(
			TerminalRecordAssociationDefect,
			"terminal schema-v3 attribution_scope is required and must not be null",
		)
	}
	for _, optional := range []string{
		"team_id",
		"workflow_id",
		"workflow_version",
		"run_snapshot_id",
		"conversation_id",
		"parent_run_id",
		"parent_seq",
		"aggregation_parent_run_id",
		"task_group_id",
		"step",
		"summary",
	} {
		if raw, exists := fields[optional]; exists && bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			return fmt.Errorf("terminal schema-v3 field %q must be omitted instead of null", optional)
		}
	}
	for _, usageField := range []string{"self_exclusive", "subtree_total"} {
		if err := validateTerminalUsageJSON(fields[usageField], usageField); err != nil {
			return err
		}
	}

	var children []json.RawMessage
	if err := json.Unmarshal(fields["child_breakdown"], &children); err != nil {
		return fmt.Errorf("terminal child_breakdown must be an array: %w", err)
	}
	if children == nil {
		return fmt.Errorf("terminal child_breakdown must be a non-null array")
	}
	for index, rawChild := range children {
		var childFields map[string]json.RawMessage
		if err := json.Unmarshal(rawChild, &childFields); err != nil || childFields == nil {
			return fmt.Errorf("terminal child_breakdown[%d] must be a JSON object", index)
		}
		if err := requireTerminalJSONFields(childFields, []string{
			"run_id",
			"parent_run_id",
			"parent_seq",
			"agent",
			"self_exclusive",
		}); err != nil {
			return fmt.Errorf("terminal child_breakdown[%d]: %w", index, err)
		}
		for _, optional := range []string{"team_id", "workflow_id", "workflow_version", "run_snapshot_id"} {
			if raw, exists := childFields[optional]; exists && bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
				return fmt.Errorf(
					"terminal child_breakdown[%d] field %q must be omitted instead of null",
					index,
					optional,
				)
			}
		}
		if err := validateTerminalUsageJSON(
			childFields["self_exclusive"],
			fmt.Sprintf("child_breakdown[%d].self_exclusive", index),
		); err != nil {
			return err
		}
	}
	return nil
}

func requireTerminalJSONFields(fields map[string]json.RawMessage, required []string) error {
	for _, name := range required {
		raw, exists := fields[name]
		if !exists {
			return fmt.Errorf("terminal schema-v3 field %q is required", name)
		}
		if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			return fmt.Errorf("terminal schema-v3 field %q must not be null", name)
		}
	}
	return nil
}

func validateTerminalUsageJSON(data json.RawMessage, path string) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil || fields == nil {
		return fmt.Errorf("terminal %s must be a JSON object", path)
	}
	if err := requireTerminalJSONFields(fields, []string{"input_tokens", "output_tokens", "cost_usd"}); err != nil {
		return fmt.Errorf("terminal %s: %w", path, err)
	}
	return nil
}

// ValidateTerminalV3 validates an in-memory schema-v3 candidate.
func ValidateTerminalV3(entry TerminalEntryV3) error {
	return validateTerminalV3(entry)
}

func validateTerminalV3(entry TerminalEntryV3) error {
	if entry.SchemaVersion != terminalSchemaVersionV3 {
		return terminalRecordError(TerminalRecordCorrupt, "terminal schema_version = %d, want 3", entry.SchemaVersion)
	}
	for name, value := range map[string]string{
		"run_id":      entry.RunID,
		"agent":       entry.Agent,
		"tenant":      entry.Tenant,
		"status":      entry.Status,
		"stop_reason": entry.StopReason,
		"started_at":  entry.StartedAt,
		"ended_at":    entry.EndedAt,
	} {
		if value == "" {
			return terminalRecordError(TerminalRecordCorrupt, "terminal %s is required", name)
		}
	}
	if entry.DurationMs < 0 {
		return terminalRecordError(TerminalRecordCorrupt, "terminal duration_ms must be non-negative")
	}
	if err := validateTerminalUsage(entry.SelfExclusive); err != nil {
		return terminalRecordError(TerminalRecordCorrupt, "terminal self_exclusive: %v", err)
	}
	alias := TerminalUsage{
		InputTokens:  entry.TokensIn,
		OutputTokens: entry.TokensOut,
		CostUSD:      entry.CostUSD,
	}
	if err := validateTerminalUsage(alias); err != nil {
		return terminalRecordError(TerminalRecordCorrupt, "terminal usage alias: %v", err)
	}
	if alias != entry.SelfExclusive {
		return terminalRecordError(TerminalRecordCorrupt, "terminal usage alias does not equal self_exclusive")
	}
	if entry.UsageComplete != nil && !*entry.UsageComplete &&
		entry.UsageIncompleteReason == "" {
		return terminalRecordError(
			TerminalRecordCorrupt,
			"terminal usage_incomplete_reason is required when usage_complete is false",
		)
	}
	if entry.UsageIncompleteReason != "" &&
		(entry.UsageComplete == nil || *entry.UsageComplete) {
		return terminalRecordError(
			TerminalRecordCorrupt,
			"terminal usage_incomplete_reason requires usage_complete=false",
		)
	}
	if entry.ChildBreakdown == nil {
		return terminalRecordError(TerminalRecordCorrupt, "terminal child_breakdown must be a non-nil array")
	}
	if err := validateTerminalUsage(entry.SubtreeTotal); err != nil {
		return terminalRecordError(TerminalRecordCorrupt, "terminal subtree_total: %v", err)
	}
	for index, child := range entry.ChildBreakdown {
		if child.RunID == "" || child.ParentRunID == "" || child.Agent == "" {
			return terminalRecordError(
				TerminalRecordCorrupt,
				"terminal child_breakdown[%d] requires run_id, parent_run_id, and agent",
				index,
			)
		}
		if err := validateTerminalUsage(child.SelfExclusive); err != nil {
			return terminalRecordError(
				TerminalRecordCorrupt,
				"terminal child_breakdown[%d].self_exclusive: %v",
				index,
				err,
			)
		}
	}

	total := entry.SelfExclusive
	for index, child := range entry.ChildBreakdown {
		next, err := addTerminalUsage(total, child.SelfExclusive)
		if err != nil {
			return terminalRecordError(
				TerminalRecordCorrupt,
				"terminal child_breakdown[%d] overflows subtree_total: %v",
				index,
				err,
			)
		}
		total = next
	}
	if total != entry.SubtreeTotal {
		return terminalRecordError(
			TerminalRecordCorrupt,
			"terminal subtree_total does not equal self_exclusive plus descendant exclusive usage",
		)
	}
	if err := validateTerminalAssociation(entry); err != nil {
		return err
	}
	if err := validateTerminalLocalLineage(entry); err != nil {
		return err
	}
	return nil
}

func validateTerminalUsage(usage TerminalUsage) error {
	if usage.InputTokens < 0 {
		return fmt.Errorf("input_tokens must be non-negative")
	}
	if usage.OutputTokens < 0 {
		return fmt.Errorf("output_tokens must be non-negative")
	}
	if usage.CostUSD < 0 || math.IsNaN(usage.CostUSD) || math.IsInf(usage.CostUSD, 0) {
		return fmt.Errorf("cost_usd must be finite and non-negative")
	}
	return nil
}

func addTerminalUsage(left, right TerminalUsage) (TerminalUsage, error) {
	if err := validateTerminalUsage(left); err != nil {
		return TerminalUsage{}, err
	}
	if err := validateTerminalUsage(right); err != nil {
		return TerminalUsage{}, err
	}
	input, ok := safeAddInt(left.InputTokens, right.InputTokens)
	if !ok {
		return TerminalUsage{}, fmt.Errorf("input_tokens overflows int")
	}
	output, ok := safeAddInt(left.OutputTokens, right.OutputTokens)
	if !ok {
		return TerminalUsage{}, fmt.Errorf("output_tokens overflows int")
	}
	cost := left.CostUSD + right.CostUSD
	if math.IsNaN(cost) || math.IsInf(cost, 0) {
		return TerminalUsage{}, fmt.Errorf("cost_usd is not finite")
	}
	return TerminalUsage{InputTokens: input, OutputTokens: output, CostUSD: cost}, nil
}

func validateTerminalAssociation(entry TerminalEntryV3) error {
	for name, value := range map[string]*string{
		"team_id":                   entry.TeamID,
		"workflow_id":               entry.WorkflowID,
		"run_snapshot_id":           entry.RunSnapshotID,
		"conversation_id":           entry.ConversationID,
		"parent_run_id":             entry.ParentRunID,
		"aggregation_parent_run_id": entry.AggregationParentRunID,
		"task_group_id":             entry.TaskGroupID,
	} {
		if value != nil && *value == "" {
			return terminalRecordError(TerminalRecordAssociationDefect, "terminal %s must be non-empty when present", name)
		}
	}
	if entry.WorkflowVersion != nil && *entry.WorkflowVersion < 1 {
		return terminalRecordError(TerminalRecordAssociationDefect, "terminal workflow_version must be >= 1")
	}
	if (entry.WorkflowID == nil) != (entry.WorkflowVersion == nil) {
		return terminalRecordError(TerminalRecordAssociationDefect, "terminal workflow_id and workflow_version must be paired")
	}

	switch entry.AttributionScope {
	case "fixed_workflow":
		if entry.TeamID == nil || entry.RunSnapshotID == nil ||
			entry.WorkflowID == nil || entry.WorkflowVersion == nil {
			return terminalRecordError(
				TerminalRecordAssociationDefect,
				"fixed_workflow requires team_id, workflow pair, and run_snapshot_id",
			)
		}
	case "team_free_collab":
		if entry.TeamID == nil || entry.RunSnapshotID == nil {
			return terminalRecordError(
				TerminalRecordAssociationDefect,
				"team_free_collab requires team_id and run_snapshot_id",
			)
		}
		if entry.WorkflowID != nil || entry.WorkflowVersion != nil {
			return terminalRecordError(
				TerminalRecordAssociationDefect,
				"team_free_collab requires the workflow pair to be omitted",
			)
		}
	case "legacy_unattributed":
	default:
		return terminalRecordError(
			TerminalRecordAssociationDefect,
			"unknown terminal attribution_scope %q",
			entry.AttributionScope,
		)
	}
	return nil
}

func validateTerminalLocalLineage(entry TerminalEntryV3) error {
	if (entry.ParentRunID == nil) != (entry.ParentSeq == nil) {
		return terminalRecordError(TerminalRecordLineageDefect, "terminal parent_run_id and parent_seq must be paired")
	}
	if entry.ParentSeq != nil && *entry.ParentSeq < 0 {
		return terminalRecordError(TerminalRecordLineageDefect, "terminal parent_seq must be non-negative")
	}
	if entry.ParentRunID != nil && *entry.ParentRunID == entry.RunID {
		return terminalRecordError(TerminalRecordLineageDefect, "terminal parent_run_id must not reference the run itself")
	}
	if entry.AggregationParentRunID != nil {
		if entry.ParentRunID == nil || *entry.AggregationParentRunID != *entry.ParentRunID {
			return terminalRecordError(
				TerminalRecordLineageDefect,
				"terminal aggregation_parent_run_id must equal canonical parent_run_id",
			)
		}
	}

	seen := make(map[string]struct{}, len(entry.ChildBreakdown))
	for index, child := range entry.ChildBreakdown {
		if child.ParentSeq < 0 {
			return terminalRecordError(
				TerminalRecordLineageDefect,
				"terminal child_breakdown[%d].parent_seq must be non-negative",
				index,
			)
		}
		for name, value := range map[string]*string{
			"team_id":         child.TeamID,
			"workflow_id":     child.WorkflowID,
			"run_snapshot_id": child.RunSnapshotID,
		} {
			if value != nil && *value == "" {
				return terminalRecordError(
					TerminalRecordLineageDefect,
					"terminal child_breakdown[%d].%s must be non-empty when present",
					index,
					name,
				)
			}
		}
		if (child.WorkflowID == nil) != (child.WorkflowVersion == nil) {
			return terminalRecordError(
				TerminalRecordLineageDefect,
				"terminal child_breakdown[%d] workflow_id and workflow_version must be paired",
				index,
			)
		}
		if child.WorkflowVersion != nil && *child.WorkflowVersion < 1 {
			return terminalRecordError(
				TerminalRecordLineageDefect,
				"terminal child_breakdown[%d].workflow_version must be >= 1",
				index,
			)
		}
		if child.RunID == entry.RunID {
			return terminalRecordError(
				TerminalRecordLineageDefect,
				"terminal child_breakdown[%d] contains the parent entry",
				index,
			)
		}
		if child.RunID == child.ParentRunID {
			return terminalRecordError(
				TerminalRecordLineageDefect,
				"terminal child_breakdown[%d] is self-parented",
				index,
			)
		}
		if _, duplicate := seen[child.RunID]; duplicate {
			return terminalRecordError(
				TerminalRecordLineageDefect,
				"terminal child_breakdown contains duplicate run_id %q",
				child.RunID,
			)
		}
		seen[child.RunID] = struct{}{}
		if index > 0 && terminalChildLess(child, entry.ChildBreakdown[index-1]) {
			return terminalRecordError(
				TerminalRecordLineageDefect,
				"terminal child_breakdown is not sorted by parent_run_id, parent_seq, run_id",
			)
		}
		if entry.AttributionScope == "fixed_workflow" || entry.AttributionScope == "team_free_collab" {
			if !sameTerminalStringPointer(child.TeamID, entry.TeamID) ||
				!sameTerminalStringPointer(child.WorkflowID, entry.WorkflowID) ||
				!sameTerminalIntPointer(child.WorkflowVersion, entry.WorkflowVersion) ||
				!sameTerminalStringPointer(child.RunSnapshotID, entry.RunSnapshotID) {
				return terminalRecordError(
					TerminalRecordLineageDefect,
					"terminal child_breakdown[%d] crosses attribution domain",
					index,
				)
			}
		}
	}
	return nil
}

func terminalChildLess(left, right TerminalChildBreakdownV3) bool {
	if left.ParentRunID != right.ParentRunID {
		return left.ParentRunID < right.ParentRunID
	}
	if left.ParentSeq != right.ParentSeq {
		return left.ParentSeq < right.ParentSeq
	}
	return left.RunID < right.RunID
}

func sameTerminalStringPointer(left, right *string) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}

func sameTerminalIntPointer(left, right *int) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}
