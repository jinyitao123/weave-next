package loomruntime

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"strconv"

	"github.com/jinyitao123/loom"
)

// TerminalAttributionScope identifies the immutable attribution domain of a run.
type TerminalAttributionScope string

const (
	TerminalAttributionLegacyUnattributed TerminalAttributionScope = "legacy_unattributed"
	TerminalAttributionTeamFreeCollab     TerminalAttributionScope = "team_free_collab"
	TerminalAttributionFixedWorkflow      TerminalAttributionScope = "fixed_workflow"

	terminalAttributionStateKey = "__terminal_attribution"
)

// TerminalAttributionInput contains caller-resolved scalar attribution facts.
type TerminalAttributionInput struct {
	Scope                  TerminalAttributionScope
	WorkspaceID            string
	TeamID                 *string
	WorkflowID             *string
	WorkflowVersion        *int
	RunSnapshotID          *string
	ConversationID         *string
	ParentRunID            *string
	ParentSeq              *int64
	AggregationParentRunID *string
	TaskGroupID            *string
}

// TerminalSnapshotEvidence is the minimal immutable snapshot projection needed here.
type TerminalSnapshotEvidence struct {
	RunID           string
	WorkspaceID     string
	TeamID          string
	Mode            string
	WorkflowID      *string
	WorkflowVersion *int
	ParentRunID     *string
	TaskGroupID     *string
}

type terminalOptionalString struct {
	value   string
	present bool
}

type terminalOptionalInt struct {
	value   int
	present bool
}

type terminalOptionalInt64 struct {
	value   int64
	present bool
}

type terminalSnapshotFacts struct {
	runID           string
	workspaceID     string
	teamID          string
	mode            string
	workflowID      terminalOptionalString
	workflowVersion terminalOptionalInt
	parentRunID     terminalOptionalString
	taskGroupID     terminalOptionalString
}

type terminalAttributionCheckpointStamp struct {
	SchemaVersion          int                              `json:"schema_version"`
	Scope                  TerminalAttributionScope         `json:"scope"`
	WorkspaceID            string                           `json:"workspace_id"`
	TeamID                 *string                          `json:"team_id,omitempty"`
	WorkflowID             *string                          `json:"workflow_id,omitempty"`
	WorkflowVersion        *int                             `json:"workflow_version,omitempty"`
	RunSnapshotID          *string                          `json:"run_snapshot_id,omitempty"`
	ConversationID         *string                          `json:"conversation_id,omitempty"`
	ParentRunID            *string                          `json:"parent_run_id,omitempty"`
	ParentSeq              *int64                           `json:"parent_seq,omitempty"`
	AggregationParentRunID *string                          `json:"aggregation_parent_run_id,omitempty"`
	TaskGroupID            *string                          `json:"task_group_id,omitempty"`
	Snapshot               *terminalSnapshotCheckpointStamp `json:"snapshot,omitempty"`
}

type terminalSnapshotCheckpointStamp struct {
	RunID           string  `json:"run_id"`
	WorkspaceID     string  `json:"workspace_id"`
	TeamID          string  `json:"team_id"`
	Mode            string  `json:"mode"`
	WorkflowID      *string `json:"workflow_id,omitempty"`
	WorkflowVersion *int    `json:"workflow_version,omitempty"`
	ParentRunID     *string `json:"parent_run_id,omitempty"`
	TaskGroupID     *string `json:"task_group_id,omitempty"`
}

// TerminalAttribution is an opaque, validated, immutable attribution envelope.
type TerminalAttribution struct {
	scope                  TerminalAttributionScope
	workspaceID            string
	teamID                 terminalOptionalString
	workflowID             terminalOptionalString
	workflowVersion        terminalOptionalInt
	runSnapshotID          terminalOptionalString
	conversationID         terminalOptionalString
	parentRunID            terminalOptionalString
	parentSeq              terminalOptionalInt64
	aggregationParentRunID terminalOptionalString
	taskGroupID            terminalOptionalString
	snapshot               *terminalSnapshotFacts
}

// NewTerminalAttribution validates and defensively copies attribution facts.
func NewTerminalAttribution(
	input TerminalAttributionInput,
	snapshot *TerminalSnapshotEvidence,
) (TerminalAttribution, error) {
	if err := validateTerminalAttributionInput(input); err != nil {
		return TerminalAttribution{}, err
	}
	requiresSnapshot := input.Scope == TerminalAttributionFixedWorkflow ||
		input.Scope == TerminalAttributionTeamFreeCollab ||
		(input.Scope == TerminalAttributionLegacyUnattributed && terminalInputHasAssociation(input))
	if requiresSnapshot && snapshot == nil {
		return TerminalAttribution{}, fmt.Errorf("terminal attribution snapshot evidence is required")
	}

	var copiedSnapshot *terminalSnapshotFacts
	if snapshot != nil {
		validated, err := validateAndCopyTerminalSnapshot(*snapshot)
		if err != nil {
			return TerminalAttribution{}, err
		}
		if err := validateTerminalSnapshotMatchesInput(input, validated); err != nil {
			return TerminalAttribution{}, err
		}
		copiedSnapshot = &validated
	}

	return TerminalAttribution{
		scope:                  input.Scope,
		workspaceID:            input.WorkspaceID,
		teamID:                 copyTerminalOptionalString(input.TeamID),
		workflowID:             copyTerminalOptionalString(input.WorkflowID),
		workflowVersion:        copyTerminalOptionalInt(input.WorkflowVersion),
		runSnapshotID:          copyTerminalOptionalString(input.RunSnapshotID),
		conversationID:         copyTerminalOptionalString(input.ConversationID),
		parentRunID:            copyTerminalOptionalString(input.ParentRunID),
		parentSeq:              copyTerminalOptionalInt64(input.ParentSeq),
		aggregationParentRunID: copyTerminalOptionalString(input.AggregationParentRunID),
		taskGroupID:            copyTerminalOptionalString(input.TaskGroupID),
		snapshot:               copiedSnapshot,
	}, nil
}

func validateTerminalAttributionInput(input TerminalAttributionInput) error {
	switch input.Scope {
	case TerminalAttributionLegacyUnattributed,
		TerminalAttributionTeamFreeCollab,
		TerminalAttributionFixedWorkflow:
	default:
		return fmt.Errorf("unknown terminal attribution scope %q", input.Scope)
	}
	if input.WorkspaceID == "" {
		return fmt.Errorf("terminal attribution workspace_id is required")
	}
	for name, value := range map[string]*string{
		"team_id":                   input.TeamID,
		"workflow_id":               input.WorkflowID,
		"run_snapshot_id":           input.RunSnapshotID,
		"conversation_id":           input.ConversationID,
		"parent_run_id":             input.ParentRunID,
		"aggregation_parent_run_id": input.AggregationParentRunID,
		"task_group_id":             input.TaskGroupID,
	} {
		if value != nil && *value == "" {
			return fmt.Errorf("terminal attribution %s must be non-empty when present", name)
		}
	}
	if (input.WorkflowID == nil) != (input.WorkflowVersion == nil) {
		return fmt.Errorf("terminal attribution workflow_id and workflow_version must be paired")
	}
	if input.WorkflowVersion != nil && *input.WorkflowVersion < 1 {
		return fmt.Errorf("terminal attribution workflow_version must be >= 1")
	}
	if (input.ParentRunID == nil) != (input.ParentSeq == nil) {
		return fmt.Errorf("terminal attribution parent_run_id and parent_seq must be paired")
	}
	if input.ParentSeq != nil && *input.ParentSeq < 0 {
		return fmt.Errorf("terminal attribution parent_seq must be non-negative")
	}
	if input.AggregationParentRunID != nil {
		if input.ParentRunID == nil || *input.AggregationParentRunID != *input.ParentRunID {
			return fmt.Errorf("terminal attribution aggregation_parent_run_id must equal parent_run_id")
		}
	}

	switch input.Scope {
	case TerminalAttributionFixedWorkflow:
		if input.TeamID == nil || input.RunSnapshotID == nil ||
			input.WorkflowID == nil || input.WorkflowVersion == nil {
			return fmt.Errorf(
				"fixed_workflow attribution requires team_id, workflow pair, and run_snapshot_id",
			)
		}
	case TerminalAttributionTeamFreeCollab:
		if input.TeamID == nil || input.RunSnapshotID == nil {
			return fmt.Errorf("team_free_collab attribution requires team_id and run_snapshot_id")
		}
		if input.WorkflowID != nil || input.WorkflowVersion != nil {
			return fmt.Errorf("team_free_collab attribution requires the workflow pair to be omitted")
		}
	case TerminalAttributionLegacyUnattributed:
	}
	return nil
}

func terminalInputHasAssociation(input TerminalAttributionInput) bool {
	return input.TeamID != nil ||
		input.WorkflowID != nil ||
		input.WorkflowVersion != nil ||
		input.RunSnapshotID != nil
}

func validateAndCopyTerminalSnapshot(
	snapshot TerminalSnapshotEvidence,
) (terminalSnapshotFacts, error) {
	if snapshot.RunID == "" ||
		snapshot.WorkspaceID == "" ||
		snapshot.TeamID == "" {
		return terminalSnapshotFacts{}, fmt.Errorf(
			"terminal snapshot evidence requires run_id, workspace_id, and team_id",
		)
	}
	for name, value := range map[string]*string{
		"workflow_id":   snapshot.WorkflowID,
		"parent_run_id": snapshot.ParentRunID,
		"task_group_id": snapshot.TaskGroupID,
	} {
		if value != nil && *value == "" {
			return terminalSnapshotFacts{}, fmt.Errorf(
				"terminal snapshot evidence %s must be non-empty when present",
				name,
			)
		}
	}
	if (snapshot.WorkflowID == nil) != (snapshot.WorkflowVersion == nil) {
		return terminalSnapshotFacts{}, fmt.Errorf(
			"terminal snapshot evidence workflow_id and workflow_version must be paired",
		)
	}
	if snapshot.WorkflowVersion != nil && *snapshot.WorkflowVersion < 1 {
		return terminalSnapshotFacts{}, fmt.Errorf(
			"terminal snapshot evidence workflow_version must be >= 1",
		)
	}
	switch snapshot.Mode {
	case "fixed_workflow":
		if snapshot.WorkflowID == nil || snapshot.WorkflowVersion == nil {
			return terminalSnapshotFacts{}, fmt.Errorf(
				"fixed_workflow snapshot evidence requires a workflow pair",
			)
		}
	case "free_collab":
		if snapshot.WorkflowID != nil || snapshot.WorkflowVersion != nil {
			return terminalSnapshotFacts{}, fmt.Errorf(
				"free_collab snapshot evidence requires the workflow pair to be omitted",
			)
		}
	default:
		return terminalSnapshotFacts{}, fmt.Errorf(
			"unknown terminal snapshot evidence mode %q",
			snapshot.Mode,
		)
	}
	return terminalSnapshotFacts{
		runID:           snapshot.RunID,
		workspaceID:     snapshot.WorkspaceID,
		teamID:          snapshot.TeamID,
		mode:            snapshot.Mode,
		workflowID:      copyTerminalOptionalString(snapshot.WorkflowID),
		workflowVersion: copyTerminalOptionalInt(snapshot.WorkflowVersion),
		parentRunID:     copyTerminalOptionalString(snapshot.ParentRunID),
		taskGroupID:     copyTerminalOptionalString(snapshot.TaskGroupID),
	}, nil
}

func validateTerminalSnapshotMatchesInput(
	input TerminalAttributionInput,
	snapshot terminalSnapshotFacts,
) error {
	if snapshot.workspaceID != input.WorkspaceID {
		return fmt.Errorf("terminal snapshot workspace_id does not match attribution")
	}
	if !sameTerminalOptionalString(snapshot.taskGroupID, copyTerminalOptionalString(input.TaskGroupID)) {
		return fmt.Errorf("terminal snapshot task_group_id does not match attribution")
	}

	switch input.Scope {
	case TerminalAttributionFixedWorkflow:
		if snapshot.mode != "fixed_workflow" {
			return fmt.Errorf("terminal snapshot mode does not match fixed_workflow attribution")
		}
		if snapshot.teamID != *input.TeamID ||
			snapshot.runID != *input.RunSnapshotID ||
			!sameTerminalOptionalString(snapshot.workflowID, copyTerminalOptionalString(input.WorkflowID)) ||
			!sameTerminalOptionalInt(snapshot.workflowVersion, copyTerminalOptionalInt(input.WorkflowVersion)) {
			return fmt.Errorf("terminal snapshot domain does not match fixed_workflow attribution")
		}
	case TerminalAttributionTeamFreeCollab:
		if snapshot.mode != "free_collab" {
			return fmt.Errorf("terminal snapshot mode does not match team_free_collab attribution")
		}
		if snapshot.teamID != *input.TeamID ||
			snapshot.runID != *input.RunSnapshotID ||
			snapshot.workflowID.present ||
			snapshot.workflowVersion.present {
			return fmt.Errorf("terminal snapshot domain does not match team_free_collab attribution")
		}
	case TerminalAttributionLegacyUnattributed:
		if input.TeamID != nil && snapshot.teamID != *input.TeamID {
			return fmt.Errorf("terminal snapshot team_id does not match legacy attribution evidence")
		}
		if input.RunSnapshotID != nil && snapshot.runID != *input.RunSnapshotID {
			return fmt.Errorf("terminal snapshot run_id does not match legacy attribution evidence")
		}
		if input.WorkflowID != nil &&
			(!sameTerminalOptionalString(snapshot.workflowID, copyTerminalOptionalString(input.WorkflowID)) ||
				!sameTerminalOptionalInt(snapshot.workflowVersion, copyTerminalOptionalInt(input.WorkflowVersion))) {
			return fmt.Errorf("terminal snapshot workflow pair does not match legacy attribution evidence")
		}
	}
	return nil
}

func (attribution TerminalAttribution) clone() TerminalAttribution {
	cloned := attribution
	if attribution.snapshot != nil {
		snapshot := *attribution.snapshot
		cloned.snapshot = &snapshot
	}
	return cloned
}

func (attribution TerminalAttribution) inputCopy() TerminalAttributionInput {
	return TerminalAttributionInput{
		Scope:                  attribution.scope,
		WorkspaceID:            attribution.workspaceID,
		TeamID:                 attribution.teamID.pointer(),
		WorkflowID:             attribution.workflowID.pointer(),
		WorkflowVersion:        attribution.workflowVersion.pointer(),
		RunSnapshotID:          attribution.runSnapshotID.pointer(),
		ConversationID:         attribution.conversationID.pointer(),
		ParentRunID:            attribution.parentRunID.pointer(),
		ParentSeq:              attribution.parentSeq.pointer(),
		AggregationParentRunID: attribution.aggregationParentRunID.pointer(),
		TaskGroupID:            attribution.taskGroupID.pointer(),
	}
}

func bindTerminalAttribution(
	state loom.State,
	attribution TerminalAttribution,
) error {
	if state == nil {
		return fmt.Errorf("terminal attribution state is nil")
	}
	input := attribution.inputCopy()
	if err := validateTerminalAttributionInput(input); err != nil {
		return fmt.Errorf("invalid terminal attribution: %w", err)
	}
	stamp := terminalAttributionCheckpointStamp{
		SchemaVersion:          1,
		Scope:                  input.Scope,
		WorkspaceID:            input.WorkspaceID,
		TeamID:                 input.TeamID,
		WorkflowID:             input.WorkflowID,
		WorkflowVersion:        input.WorkflowVersion,
		RunSnapshotID:          input.RunSnapshotID,
		ConversationID:         input.ConversationID,
		ParentRunID:            input.ParentRunID,
		ParentSeq:              input.ParentSeq,
		AggregationParentRunID: input.AggregationParentRunID,
		TaskGroupID:            input.TaskGroupID,
	}
	if attribution.snapshot != nil {
		stamp.Snapshot = &terminalSnapshotCheckpointStamp{
			RunID:           attribution.snapshot.runID,
			WorkspaceID:     attribution.snapshot.workspaceID,
			TeamID:          attribution.snapshot.teamID,
			Mode:            attribution.snapshot.mode,
			WorkflowID:      attribution.snapshot.workflowID.pointer(),
			WorkflowVersion: attribution.snapshot.workflowVersion.pointer(),
			ParentRunID:     attribution.snapshot.parentRunID.pointer(),
			TaskGroupID:     attribution.snapshot.taskGroupID.pointer(),
		}
	}
	delete(state, terminalAttributionStateKey)
	state[terminalAttributionStateKey] = stamp
	return nil
}

func decodeTerminalAttributionCheckpointStamp(
	raw any,
) (TerminalAttribution, error) {
	data, err := json.Marshal(raw)
	if err != nil {
		return TerminalAttribution{}, fmt.Errorf("marshal checkpoint stamp: %w", err)
	}
	var stamp terminalAttributionCheckpointStamp
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&stamp); err != nil {
		return TerminalAttribution{}, fmt.Errorf("decode checkpoint stamp: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return TerminalAttribution{}, fmt.Errorf("decode checkpoint stamp: trailing JSON content")
	}
	if stamp.SchemaVersion != 1 {
		return TerminalAttribution{}, fmt.Errorf(
			"unsupported checkpoint stamp schema version %d",
			stamp.SchemaVersion,
		)
	}
	var snapshot *TerminalSnapshotEvidence
	if stamp.Snapshot != nil {
		snapshot = &TerminalSnapshotEvidence{
			RunID:           stamp.Snapshot.RunID,
			WorkspaceID:     stamp.Snapshot.WorkspaceID,
			TeamID:          stamp.Snapshot.TeamID,
			Mode:            stamp.Snapshot.Mode,
			WorkflowID:      stamp.Snapshot.WorkflowID,
			WorkflowVersion: stamp.Snapshot.WorkflowVersion,
			ParentRunID:     stamp.Snapshot.ParentRunID,
			TaskGroupID:     stamp.Snapshot.TaskGroupID,
		}
	}
	attribution, err := NewTerminalAttribution(TerminalAttributionInput{
		Scope:                  stamp.Scope,
		WorkspaceID:            stamp.WorkspaceID,
		TeamID:                 stamp.TeamID,
		WorkflowID:             stamp.WorkflowID,
		WorkflowVersion:        stamp.WorkflowVersion,
		RunSnapshotID:          stamp.RunSnapshotID,
		ConversationID:         stamp.ConversationID,
		ParentRunID:            stamp.ParentRunID,
		ParentSeq:              stamp.ParentSeq,
		AggregationParentRunID: stamp.AggregationParentRunID,
		TaskGroupID:            stamp.TaskGroupID,
	}, snapshot)
	if err != nil {
		return TerminalAttribution{}, fmt.Errorf("restore checkpoint stamp: %w", err)
	}
	return attribution, nil
}

func validateTerminalAttributionForResult(
	attribution TerminalAttribution,
	tenant string,
	result Result,
) error {
	return validateTerminalAttributionForState(
		attribution,
		tenant,
		result.RunID,
		result.State,
	)
}

func validateTerminalAttributionForState(
	attribution TerminalAttribution,
	tenant string,
	runID string,
	state loom.State,
) error {
	if err := validateTerminalAttributionInput(attribution.inputCopy()); err != nil {
		return terminalRecordError(TerminalRecordAssociationDefect, "invalid terminal attribution: %v", err)
	}
	if runID == "" {
		return terminalRecordError(TerminalRecordAssociationDefect, "terminal result run_id is required")
	}
	if state == nil {
		return terminalRecordError(TerminalRecordAssociationDefect, "terminal result State is required")
	}
	if tenant == "" ||
		tenant != attribution.workspaceID {
		return terminalRecordError(
			TerminalRecordAssociationDefect,
			"terminal namespace does not match attribution workspace_id",
		)
	}
	stateTenant, ok := state["tenant"].(string)
	if !ok || stateTenant == "" || stateTenant != tenant {
		return terminalRecordError(
			TerminalRecordAssociationDefect,
			"terminal State tenant does not match namespace",
		)
	}
	if rawRunID, exists := state["__run_id"]; exists {
		stateRunID, ok := rawRunID.(string)
		if !ok || stateRunID == "" || stateRunID != runID {
			return terminalRecordError(
				TerminalRecordAssociationDefect,
				"terminal State __run_id does not match result run_id",
			)
		}
	}

	stateParent, stateSeq, err := terminalCanonicalParentFromState(state)
	if err != nil {
		return terminalRecordError(TerminalRecordAssociationDefect, "%v", err)
	}
	if !sameTerminalOptionalString(stateParent, attribution.parentRunID) ||
		!sameTerminalOptionalInt64(stateSeq, attribution.parentSeq) {
		return terminalRecordError(
			TerminalRecordAssociationDefect,
			"terminal State canonical parent pair does not match attribution",
		)
	}
	if attribution.parentRunID.present && attribution.parentRunID.value == runID {
		return terminalRecordError(
			TerminalRecordAssociationDefect,
			"terminal attribution parent_run_id must not reference result run_id",
		)
	}
	if attribution.snapshot != nil && runID == attribution.snapshot.runID &&
		!sameTerminalOptionalString(attribution.parentRunID, attribution.snapshot.parentRunID) {
		return terminalRecordError(
			TerminalRecordAssociationDefect,
			"terminal snapshot-owned run parent does not match snapshot evidence",
		)
	}
	if err := validateTerminalExecutionScope(attribution, state); err != nil {
		return terminalRecordError(TerminalRecordAssociationDefect, "%v", err)
	}
	return nil
}

func terminalCanonicalParentFromState(
	state loom.State,
) (terminalOptionalString, terminalOptionalInt64, error) {
	rawParent, parentExists := state["__parent_run"]
	rawSeq, seqExists := state["__parent_seq"]
	if !parentExists {
		if _, legacyExists := state["__parent_run_id"]; legacyExists {
			return terminalOptionalString{}, terminalOptionalInt64{}, fmt.Errorf(
				"terminal State has legacy __parent_run_id without canonical __parent_run",
			)
		}
	}
	if parentExists != seqExists {
		return terminalOptionalString{}, terminalOptionalInt64{}, fmt.Errorf(
			"terminal State canonical parent_run and parent_seq must be paired",
		)
	}
	if !parentExists {
		return terminalOptionalString{}, terminalOptionalInt64{}, nil
	}
	parent, ok := rawParent.(string)
	if !ok || parent == "" {
		return terminalOptionalString{}, terminalOptionalInt64{}, fmt.Errorf(
			"terminal State __parent_run must be a non-empty string",
		)
	}
	seq, err := terminalParentSeq(rawSeq)
	if err != nil {
		return terminalOptionalString{}, terminalOptionalInt64{}, err
	}
	return terminalOptionalString{value: parent, present: true},
		terminalOptionalInt64{value: seq, present: true},
		nil
}

func terminalParentSeq(value any) (int64, error) {
	var parsed int64
	switch typed := value.(type) {
	case int:
		parsed = int64(typed)
	case int64:
		parsed = typed
	case json.Number:
		value, err := strconv.ParseInt(typed.String(), 10, 64)
		if err != nil {
			return 0, fmt.Errorf("terminal State __parent_seq must be a lossless integer")
		}
		parsed = value
	case float64:
		if math.IsNaN(typed) ||
			math.IsInf(typed, 0) ||
			typed != math.Trunc(typed) ||
			typed > float64(1<<53-1) ||
			typed < -float64(1<<53-1) {
			return 0, fmt.Errorf("terminal State __parent_seq must be a lossless integer")
		}
		parsed = int64(typed)
	default:
		return 0, fmt.Errorf("terminal State __parent_seq must be an integer")
	}
	if parsed < 0 {
		return 0, fmt.Errorf("terminal State __parent_seq must be non-negative")
	}
	return parsed, nil
}

func validateTerminalExecutionScope(
	attribution TerminalAttribution,
	state loom.State,
) error {
	rawScope, exists := state["__execution_scope"]
	if attribution.scope == TerminalAttributionLegacyUnattributed {
		if !exists {
			return nil
		}
		scope, ok := rawScope.(string)
		if !ok || scope != "legacy_orchestrator" {
			return fmt.Errorf(
				"legacy_unattributed State execution scope must be omitted or legacy_orchestrator",
			)
		}
		return nil
	}
	scope, ok := rawScope.(string)
	if !exists || !ok || scope == "" {
		return fmt.Errorf("team-scoped terminal State requires an execution scope")
	}
	switch attribution.scope {
	case TerminalAttributionFixedWorkflow:
		if scope != "team_worker_leaf" {
			return fmt.Errorf("fixed_workflow State execution scope must be team_worker_leaf")
		}
	case TerminalAttributionTeamFreeCollab:
		want := "team_free_collab"
		if attribution.aggregationParentRunID.present {
			want = "team_worker_leaf"
		}
		if scope != want {
			return fmt.Errorf("team_free_collab State execution scope must be %s", want)
		}
	}
	return nil
}

func copyTerminalOptionalString(value *string) terminalOptionalString {
	if value == nil {
		return terminalOptionalString{}
	}
	return terminalOptionalString{value: *value, present: true}
}

func copyTerminalOptionalInt(value *int) terminalOptionalInt {
	if value == nil {
		return terminalOptionalInt{}
	}
	return terminalOptionalInt{value: *value, present: true}
}

func copyTerminalOptionalInt64(value *int64) terminalOptionalInt64 {
	if value == nil {
		return terminalOptionalInt64{}
	}
	return terminalOptionalInt64{value: *value, present: true}
}

func (value terminalOptionalString) pointer() *string {
	if !value.present {
		return nil
	}
	copied := value.value
	return &copied
}

func (value terminalOptionalInt) pointer() *int {
	if !value.present {
		return nil
	}
	copied := value.value
	return &copied
}

func (value terminalOptionalInt64) pointer() *int64 {
	if !value.present {
		return nil
	}
	copied := value.value
	return &copied
}

func sameTerminalOptionalString(left, right terminalOptionalString) bool {
	return left.present == right.present && (!left.present || left.value == right.value)
}

func sameTerminalOptionalInt(left, right terminalOptionalInt) bool {
	return left.present == right.present && (!left.present || left.value == right.value)
}

func sameTerminalOptionalInt64(left, right terminalOptionalInt64) bool {
	return left.present == right.present && (!left.present || left.value == right.value)
}
