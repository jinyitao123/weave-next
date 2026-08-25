package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/big"

	"github.com/jinyitao123/loom"
	"github.com/jinyitao123/weave/internal/kernel/loomruntime"
	"github.com/jinyitao123/weave/internal/base/snapshot"
)

func legacyRootTerminalAttribution(
	workspaceID string,
) (loomruntime.TerminalAttribution, error) {
	return loomruntime.NewTerminalAttribution(loomruntime.TerminalAttributionInput{
		Scope:       loomruntime.TerminalAttributionLegacyUnattributed,
		WorkspaceID: workspaceID,
	}, nil)
}

type runAssociationsEvidence struct {
	SchemaVersion    json.RawMessage `json:"schema_version"`
	ParentRunID      json.RawMessage `json:"parent_run_id"`
	SourceSnapshotID json.RawMessage `json:"source_snapshot_id"`
	TaskGroupID      json.RawMessage `json:"task_group_id"`
}

func terminalAttributionFromSnapshot(
	workspaceID string,
	snap snapshot.TeamRunSnapshot,
	parentSeq *int64,
) (loomruntime.TerminalAttribution, error) {
	if workspaceID == "" ||
		snap.WorkspaceID == "" ||
		snap.WorkspaceID != workspaceID {
		return loomruntime.TerminalAttribution{}, fmt.Errorf(
			"terminal snapshot workspace does not match request workspace",
		)
	}
	if snap.SnapshotSchemaVersion != 2 {
		return loomruntime.TerminalAttribution{}, fmt.Errorf(
			"terminal snapshot schema version %d is unsupported",
			snap.SnapshotSchemaVersion,
		)
	}
	if snap.RunID == "" || snap.TeamID == "" {
		return loomruntime.TerminalAttribution{}, fmt.Errorf(
			"terminal snapshot requires run_id and team_id",
		)
	}
	associations, err := decodeRunAssociationsEvidence(snap.RunAssociations)
	if err != nil {
		return loomruntime.TerminalAttribution{}, fmt.Errorf(
			"decode terminal run associations: %w",
			err,
		)
	}
	parentRunID, err := decodeNullableAssociation(
		"parent_run_id",
		associations.ParentRunID,
	)
	if err != nil {
		return loomruntime.TerminalAttribution{}, err
	}
	if _, err := decodeNullableAssociation(
		"source_snapshot_id",
		associations.SourceSnapshotID,
	); err != nil {
		return loomruntime.TerminalAttribution{}, err
	}
	taskGroupID, err := decodeNullableAssociation(
		"task_group_id",
		associations.TaskGroupID,
	)
	if err != nil {
		return loomruntime.TerminalAttribution{}, err
	}
	if (parentRunID == nil) != (parentSeq == nil) {
		return loomruntime.TerminalAttribution{}, fmt.Errorf(
			"terminal snapshot parent_run_id requires an exact durable parent_seq pair",
		)
	}

	input := loomruntime.TerminalAttributionInput{
		WorkspaceID:   workspaceID,
		TeamID:        stringPointer(snap.TeamID),
		RunSnapshotID: stringPointer(snap.RunID),
		ParentRunID:   parentRunID,
		ParentSeq:     copyInt64Pointer(parentSeq),
		TaskGroupID:   taskGroupID,
	}
	if parentRunID != nil {
		input.AggregationParentRunID = stringPointer(*parentRunID)
	}
	if conversationID := terminalConversationIDFromSnapshot(snap); conversationID != "" {
		input.ConversationID = stringPointer(conversationID)
	}
	evidence := loomruntime.TerminalSnapshotEvidence{
		RunID:       snap.RunID,
		WorkspaceID: snap.WorkspaceID,
		TeamID:      snap.TeamID,
		Mode:        snap.Mode,
		ParentRunID: copyStringPointer(parentRunID),
		TaskGroupID: copyStringPointer(taskGroupID),
	}
	switch snap.Mode {
	case "fixed_workflow":
		if snap.WorkflowID == "" || snap.WorkflowVersion < 1 {
			return loomruntime.TerminalAttribution{}, fmt.Errorf(
				"fixed_workflow terminal snapshot requires a workflow pair",
			)
		}
		input.Scope = loomruntime.TerminalAttributionFixedWorkflow
		input.WorkflowID = stringPointer(snap.WorkflowID)
		input.WorkflowVersion = intPointer(snap.WorkflowVersion)
		evidence.WorkflowID = stringPointer(snap.WorkflowID)
		evidence.WorkflowVersion = intPointer(snap.WorkflowVersion)
	case "free_collab":
		if snap.WorkflowID != "" || snap.WorkflowVersion != 0 {
			return loomruntime.TerminalAttribution{}, fmt.Errorf(
				"free_collab terminal snapshot requires the workflow pair to be absent",
			)
		}
		input.Scope = loomruntime.TerminalAttributionTeamFreeCollab
	default:
		return loomruntime.TerminalAttribution{}, fmt.Errorf(
			"unsupported terminal snapshot mode %q",
			snap.Mode,
		)
	}
	return loomruntime.NewTerminalAttribution(input, &evidence)
}

func terminalConversationIDFromSnapshot(snap snapshot.TeamRunSnapshot) string {
	var trigger struct {
		SchemaVersion int    `json:"schema_version"`
		Type          string `json:"type"`
		SourceRef     string `json:"source_ref"`
	}
	if err := decodeExactJSON(snap.TriggerSourceV2, &trigger); err != nil ||
		trigger.SchemaVersion != 1 ||
		trigger.Type != "conversation_explicit" {
		return ""
	}
	return trigger.SourceRef
}

func decodeRunAssociationsEvidence(
	raw json.RawMessage,
) (runAssociationsEvidence, error) {
	var associations runAssociationsEvidence
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&associations); err != nil {
		return runAssociationsEvidence{}, err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return runAssociationsEvidence{}, fmt.Errorf("trailing JSON content")
	}
	for name, value := range map[string]json.RawMessage{
		"schema_version":     associations.SchemaVersion,
		"parent_run_id":      associations.ParentRunID,
		"source_snapshot_id": associations.SourceSnapshotID,
		"task_group_id":      associations.TaskGroupID,
	} {
		if len(bytes.TrimSpace(value)) == 0 {
			return runAssociationsEvidence{}, fmt.Errorf("missing field %q", name)
		}
	}
	var schemaNumber json.Number
	decoder = json.NewDecoder(bytes.NewReader(associations.SchemaVersion))
	decoder.UseNumber()
	if err := decoder.Decode(&schemaNumber); err != nil ||
		!jsonNumberEqualsOne(schemaNumber) {
		return runAssociationsEvidence{}, fmt.Errorf("schema_version must be numeric 1")
	}
	return associations, nil
}

func jsonNumberEqualsOne(number json.Number) bool {
	value, _, err := big.ParseFloat(number.String(), 10, 256, big.ToNearestEven)
	if err != nil {
		return false
	}
	one := new(big.Float).SetPrec(256).SetInt64(1)
	return value.Cmp(one) == 0
}

func decodeNullableAssociation(
	name string,
	raw json.RawMessage,
) (*string, error) {
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil, nil
	}
	var value string
	decoder := json.NewDecoder(bytes.NewReader(raw))
	if err := decoder.Decode(&value); err != nil || value == "" {
		return nil, fmt.Errorf("%s must be null or a non-empty string", name)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return nil, fmt.Errorf("%s has trailing JSON content", name)
	}
	return stringPointer(value), nil
}

func stringPointer(value string) *string {
	copied := value
	return &copied
}

func intPointer(value int) *int {
	copied := value
	return &copied
}

func copyStringPointer(value *string) *string {
	if value == nil {
		return nil
	}
	return stringPointer(*value)
}

func copyInt64Pointer(value *int64) *int64 {
	if value == nil {
		return nil
	}
	copied := *value
	return &copied
}

type loomTerminalRecordStore struct {
	store loom.Store
}

func (adapter loomTerminalRecordStore) ReadValue(
	ctx context.Context,
	namespace string,
	key string,
) ([]byte, bool, error) {
	return readLoomStoreValue(ctx, adapter.store, namespace, key)
}

func (adapter loomTerminalRecordStore) ListKeys(
	ctx context.Context,
	namespace string,
) ([]string, error) {
	if adapter.store == nil {
		return nil, fmt.Errorf("terminal store is unavailable")
	}
	return adapter.store.List(ctx, namespace, "")
}

func (adapter loomTerminalRecordStore) MutateValue(
	ctx context.Context,
	namespace string,
	key string,
	mutate func(current []byte, present bool) (next []byte, err error),
) error {
	if adapter.store == nil {
		return fmt.Errorf("terminal store is unavailable")
	}
	return adapter.store.Tx(ctx, func(tx loom.Store) error {
		current, present, err := readLoomStoreValue(ctx, tx, namespace, key)
		if err != nil {
			return err
		}
		next, err := mutate(current, present)
		if err != nil {
			return err
		}
		if next == nil {
			return tx.Delete(ctx, namespace, key)
		}
		return tx.Put(ctx, namespace, key, next)
	})
}

func readLoomStoreValue(
	ctx context.Context,
	store loom.Store,
	namespace string,
	key string,
) ([]byte, bool, error) {
	if store == nil {
		return nil, false, fmt.Errorf("terminal store is unavailable")
	}
	value, err := store.Get(ctx, namespace, key)
	if err == nil {
		return bytes.Clone(value), true, nil
	}
	keys, listErr := store.List(ctx, namespace, key)
	if listErr != nil {
		return nil, false, fmt.Errorf(
			"read terminal %q/%q: %v (verify absence: %w)",
			namespace,
			key,
			err,
			listErr,
		)
	}
	for _, listedKey := range keys {
		if listedKey == key {
			return nil, false, fmt.Errorf(
				"read existing terminal %q/%q: %w",
				namespace,
				key,
				err,
			)
		}
	}
	return nil, false, nil
}

func (s *Server) rootTerminalSink() (loomruntime.TerminalSink, error) {
	if s.StoreExt != nil {
		return loomruntime.NewLineageTerminalSink(s.StoreExt)
	}
	return loomruntime.NewLineageTerminalSink(
		loomTerminalRecordStore{store: s.Store},
	)
}
