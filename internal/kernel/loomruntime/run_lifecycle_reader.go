package loomruntime

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type PGRunLifecycleReader struct {
	pool     *pgxpool.Pool
	observer RunLifecycleObserver
}

func NewPGRunLifecycleReader(
	pool *pgxpool.Pool,
	observer RunLifecycleObserver,
) (*PGRunLifecycleReader, error) {
	if pool == nil {
		return nil, fmt.Errorf("run lifecycle PostgreSQL pool is required")
	}
	return &PGRunLifecycleReader{pool: pool, observer: observer}, nil
}

func (reader *PGRunLifecycleReader) ReadBySnapshot(
	ctx context.Context,
	query RunLifecycleQuery,
) (RunLifecycleResult, error) {
	if query.WorkspaceID == "" || query.RunSnapshotID == "" {
		return RunLifecycleResult{}, &RunLifecycleReadError{
			Stage:         RunLifecycleReadValidate,
			WorkspaceID:   query.WorkspaceID,
			RunSnapshotID: query.RunSnapshotID,
			Retryable:     false,
			Err: errors.Join(
				ErrRunLifecycleInvalidQuery,
				fmt.Errorf("workspace_id and run_snapshot_id must be non-empty"),
			),
		}
	}
	items, observedAt, failure := reader.readRunLifecycleTx(
		ctx,
		query.WorkspaceID,
		func(txCtx context.Context, tx pgx.Tx) ([]runLifecycleClaim, error) {
			return readRunLifecycleClaims(
				txCtx,
				tx,
				query.WorkspaceID,
				func(record ExpectedRunRecordV1) bool {
					return record.RunSnapshotID != nil &&
						*record.RunSnapshotID == query.RunSnapshotID
				},
			)
		},
	)
	if failure != nil {
		return RunLifecycleResult{}, failure.toReadError(
			query.WorkspaceID,
			query.RunSnapshotID,
			"",
		)
	}
	result := RunLifecycleResult{
		WorkspaceID:   query.WorkspaceID,
		RunSnapshotID: query.RunSnapshotID,
		ObservedAt:    observedAt,
		Runs:          items,
	}
	reader.observe(ctx, result)
	return result, nil
}

// ReadByAgent reads every admitted run whose registry claim is attributed to
// the given agent, reusing the exact claim/fact/classification pipeline of
// ReadBySnapshot. Lifecycle observations stay snapshot-scoped and are not
// emitted for agent reads.
func (reader *PGRunLifecycleReader) ReadByAgent(
	ctx context.Context,
	query RunLifecycleAgentQuery,
) (RunLifecycleAgentResult, error) {
	if query.WorkspaceID == "" || query.Agent == "" {
		return RunLifecycleAgentResult{}, &RunLifecycleReadError{
			Stage:       RunLifecycleReadValidate,
			WorkspaceID: query.WorkspaceID,
			Agent:       query.Agent,
			Retryable:   false,
			Err: errors.Join(
				ErrRunLifecycleInvalidQuery,
				fmt.Errorf("workspace_id and agent must be non-empty"),
			),
		}
	}
	items, observedAt, failure := reader.readRunLifecycleTx(
		ctx,
		query.WorkspaceID,
		func(txCtx context.Context, tx pgx.Tx) ([]runLifecycleClaim, error) {
			return readRunLifecycleClaims(
				txCtx,
				tx,
				query.WorkspaceID,
				func(record ExpectedRunRecordV1) bool {
					return record.Agent == query.Agent
				},
			)
		},
	)
	if failure != nil {
		return RunLifecycleAgentResult{}, failure.toReadError(
			query.WorkspaceID,
			"",
			query.Agent,
		)
	}
	return RunLifecycleAgentResult{
		WorkspaceID: query.WorkspaceID,
		Agent:       query.Agent,
		ObservedAt:  observedAt,
		Runs:        items,
	}, nil
}

// runLifecycleStageFailure carries the typed failure facts out of the shared
// read pipeline so each public entry point can wrap them with its own query
// identity.
type runLifecycleStageFailure struct {
	stage     RunLifecycleReadStage
	runID     string
	retryable bool
	sentinel  error
	err       error
}

func (failure *runLifecycleStageFailure) toReadError(
	workspaceID string,
	runSnapshotID string,
	agent string,
) error {
	joined := failure.sentinel
	if failure.err != nil {
		joined = errors.Join(failure.sentinel, failure.err)
	}
	return &RunLifecycleReadError{
		Stage:         failure.stage,
		WorkspaceID:   workspaceID,
		RunSnapshotID: runSnapshotID,
		Agent:         agent,
		RunID:         failure.runID,
		Retryable:     failure.retryable,
		Err:           joined,
	}
}

// readRunLifecycleTx runs the shared read-only pipeline: open a repeatable
// read transaction, fix the observation timestamp, load the authoritative
// claims, then read and classify every run's facts.
func (reader *PGRunLifecycleReader) readRunLifecycleTx(
	ctx context.Context,
	workspaceID string,
	loadClaims func(context.Context, pgx.Tx) ([]runLifecycleClaim, error),
) ([]RunLifecycleItem, time.Time, *runLifecycleStageFailure) {
	if reader == nil || reader.pool == nil {
		return nil, time.Time{}, &runLifecycleStageFailure{
			stage:     RunLifecycleReadBegin,
			retryable: true,
			sentinel:  ErrRunLifecycleStoreUnavailable,
			err:       fmt.Errorf("PostgreSQL pool is unavailable"),
		}
	}

	tx, err := reader.pool.BeginTx(ctx, pgx.TxOptions{
		IsoLevel:   pgx.RepeatableRead,
		AccessMode: pgx.ReadOnly,
	})
	if err != nil {
		return nil, time.Time{}, &runLifecycleStageFailure{
			stage:     RunLifecycleReadBegin,
			retryable: true,
			sentinel:  ErrRunLifecycleStoreUnavailable,
			err:       err,
		}
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var observedAt time.Time
	if err := tx.QueryRow(ctx, "SELECT statement_timestamp()").Scan(
		&observedAt,
	); err != nil {
		return nil, time.Time{}, &runLifecycleStageFailure{
			stage:     RunLifecycleReadObserve,
			retryable: true,
			sentinel:  ErrRunLifecycleStoreUnavailable,
			err:       err,
		}
	}
	observedAt = observedAt.UTC()

	claims, err := loadClaims(ctx, tx)
	if err != nil {
		var claimErr *runLifecycleClaimReadError
		if errors.As(err, &claimErr) && claimErr.corrupt {
			return nil, time.Time{}, &runLifecycleStageFailure{
				stage:     RunLifecycleReadClaims,
				runID:     claimErr.runID,
				retryable: false,
				sentinel:  ErrRunLifecycleExpectedSetCorrupt,
				err:       claimErr.err,
			}
		}
		return nil, time.Time{}, &runLifecycleStageFailure{
			stage:     RunLifecycleReadClaims,
			retryable: true,
			sentinel:  ErrRunLifecycleStoreUnavailable,
			err:       err,
		}
	}

	items, failure := readRunLifecycleItems(ctx, tx, workspaceID, claims, observedAt)
	if failure != nil {
		return nil, time.Time{}, failure
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, time.Time{}, &runLifecycleStageFailure{
			stage:     RunLifecycleReadCommit,
			retryable: true,
			sentinel:  ErrRunLifecycleStoreUnavailable,
			err:       err,
		}
	}
	return items, observedAt, nil
}

// readRunLifecycleItems reads the index/receipt/lease/marker/audit facts for
// the authoritative claims and classifies each run with the shared nine-state
// classifier.
func readRunLifecycleItems(
	ctx context.Context,
	tx pgx.Tx,
	workspaceID string,
	claims []runLifecycleClaim,
	observedAt time.Time,
) ([]RunLifecycleItem, *runLifecycleStageFailure) {
	indexKeys := make([]string, len(claims))
	receiptKeys := make([]string, len(claims))
	runIDs := make([]string, len(claims))
	for index, claim := range claims {
		indexKeys[index] = claim.indexKey
		receiptKeys[index] = claim.receiptKey
		runIDs[index] = claim.record.RunID
	}

	factFailure := func(err error) *runLifecycleStageFailure {
		return &runLifecycleStageFailure{
			stage:     RunLifecycleReadFacts,
			retryable: true,
			sentinel:  ErrRunLifecycleStoreUnavailable,
			err:       err,
		}
	}
	indexes, err := readRunLifecycleValues(
		ctx,
		tx,
		expectedRunNamespace(workspaceID),
		indexKeys,
	)
	if err != nil {
		return nil, &runLifecycleStageFailure{
			stage:     RunLifecycleReadIndexes,
			retryable: true,
			sentinel:  ErrRunLifecycleStoreUnavailable,
			err:       err,
		}
	}
	receipts, err := readRunLifecycleValues(
		ctx,
		tx,
		a4AdmissionReceiptNamespace(workspaceID),
		receiptKeys,
	)
	if err != nil {
		return nil, factFailure(err)
	}
	leases, err := readRunLifecycleLeases(
		ctx,
		tx,
		workspaceID,
		runIDs,
	)
	if err != nil {
		return nil, factFailure(err)
	}
	markers, err := readRunLifecycleMarkers(
		ctx,
		tx,
		workspaceID,
		runIDs,
	)
	if err != nil {
		return nil, factFailure(err)
	}
	audits, err := readRunLifecycleValues(
		ctx,
		tx,
		"audit:"+workspaceID,
		runIDs,
	)
	if err != nil {
		return nil, factFailure(err)
	}

	items := make([]RunLifecycleItem, 0, len(claims))
	for _, claim := range claims {
		facts := runLifecycleFacts{
			expected:     claim.record,
			claimBytes:   bytes.Clone(claim.value),
			indexState:   runLifecycleIndexValid,
			receiptState: runLifecycleReceiptAbsent,
			audit:        InspectTerminalRecord(false, nil),
		}
		indexValue, indexPresent := indexes[claim.indexKey]
		switch {
		case !indexPresent:
			facts.indexState = runLifecycleIndexMissing
		default:
			indexed, inspectErr := inspectExpectedRunPhysicalRecord(
				indexValue,
				workspaceID,
				claim.record.RunID,
				claim.indexKey,
				false,
			)
			if inspectErr != nil ||
				compareExpectedRunIdentity(indexed, claim.record) != nil ||
				!bytes.Equal(indexValue, claim.value) {
				facts.indexState = runLifecycleIndexMismatch
			}
		}

		receiptValue, receiptPresent := receipts[claim.receiptKey]
		if receiptPresent {
			receipt, decodeErr := decodeA4AdmissionReceiptV1(receiptValue)
			switch {
			case decodeErr != nil:
				facts.receiptState = runLifecycleReceiptCorrupt
			case validateA4AdmissionReceiptClaimBinding(
				receipt,
				a4AdmissionReceiptNamespace(workspaceID),
				claim.receiptKey,
				claim.record,
				claim.value,
				claim.indexKey,
			) != nil:
				facts.receiptState = runLifecycleReceiptClaimMismatch
			default:
				facts.receiptState = runLifecycleReceiptValid
			}
		}

		if leaseFact, present := leases[claim.record.RunID]; present {
			facts.lease = leaseFact.lease
			facts.leaseCorrupt = leaseFact.corrupt
		}
		if markerFact, present := markers[claim.record.RunID]; present {
			facts.marker = markerFact.marker
			facts.markerCorrupt = markerFact.corrupt
		}
		if auditValue, present := audits[claim.record.RunID]; present {
			facts.audit = InspectTerminalRecord(true, auditValue)
		}
		items = append(
			items,
			classifyRunLifecycle(facts, observedAt),
		)
	}
	return items, nil
}

type runLifecycleClaim struct {
	record     ExpectedRunRecordV1
	value      []byte
	claimKey   string
	indexKey   string
	receiptKey string
}

type runLifecycleClaimReadError struct {
	runID   string
	corrupt bool
	err     error
}

func (err *runLifecycleClaimReadError) Error() string {
	return err.err.Error()
}

func (err *runLifecycleClaimReadError) Unwrap() error {
	return err.err
}

func readRunLifecycleClaims(
	ctx context.Context,
	tx pgx.Tx,
	workspaceID string,
	keep func(ExpectedRunRecordV1) bool,
) ([]runLifecycleClaim, error) {
	rows, err := tx.Query(
		ctx,
		`SELECT key,value
		 FROM loom_store
		 WHERE namespace=$1 AND key LIKE 'v1/id/%'
		 ORDER BY key`,
		expectedRunNamespace(workspaceID),
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	claims := make([]runLifecycleClaim, 0)
	seenRunIDs := make(map[string]struct{})
	for rows.Next() {
		var key string
		var value []byte
		if err := rows.Scan(&key, &value); err != nil {
			return nil, err
		}
		runID, err := decodeRunLifecycleClaimKey(key)
		if err != nil {
			return nil, &runLifecycleClaimReadError{
				corrupt: true,
				err:     err,
			}
		}
		if _, duplicate := seenRunIDs[runID]; duplicate {
			return nil, &runLifecycleClaimReadError{
				runID:   runID,
				corrupt: true,
				err:     fmt.Errorf("duplicate decoded expected run_id %q", runID),
			}
		}
		seenRunIDs[runID] = struct{}{}
		record, err := inspectExpectedRunPhysicalRecord(
			value,
			workspaceID,
			runID,
			key,
			true,
		)
		if err != nil {
			return nil, &runLifecycleClaimReadError{
				runID:   runID,
				corrupt: true,
				err:     err,
			}
		}
		if !keep(record) {
			continue
		}
		claims = append(claims, runLifecycleClaim{
			record:     record,
			value:      bytes.Clone(value),
			claimKey:   key,
			indexKey:   expectedRunDomainKey(record),
			receiptKey: a4AdmissionReceiptKey(runID),
		})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.Slice(claims, func(i, j int) bool {
		return claims[i].record.RunID < claims[j].record.RunID
	})
	return claims, nil
}

func decodeRunLifecycleClaimKey(key string) (string, error) {
	const prefix = "v1/id/"
	if !strings.HasPrefix(key, prefix) {
		return "", fmt.Errorf("expected run claim key has invalid prefix")
	}
	component := strings.TrimPrefix(key, prefix)
	if component == "" {
		return "", fmt.Errorf("expected run claim key has empty run component")
	}
	decoded, err := base64.RawURLEncoding.DecodeString(component)
	if err != nil || len(decoded) == 0 || !utf8.Valid(decoded) {
		return "", fmt.Errorf("expected run claim key has invalid run component")
	}
	if base64.RawURLEncoding.EncodeToString(decoded) != component {
		return "", fmt.Errorf("expected run claim key is not canonical")
	}
	return string(decoded), nil
}

func readRunLifecycleValues(
	ctx context.Context,
	tx pgx.Tx,
	namespace string,
	keys []string,
) (map[string][]byte, error) {
	rows, err := tx.Query(
		ctx,
		`SELECT key,value
		 FROM loom_store
		 WHERE namespace=$1 AND key=ANY($2::text[])
		 ORDER BY key`,
		namespace,
		keys,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	values := make(map[string][]byte, len(keys))
	for rows.Next() {
		var key string
		var value []byte
		if err := rows.Scan(&key, &value); err != nil {
			return nil, err
		}
		if _, duplicate := values[key]; duplicate {
			return nil, fmt.Errorf("duplicate loom_store key %q", key)
		}
		values[key] = bytes.Clone(value)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return values, nil
}

type runLifecycleLeaseFact struct {
	lease   *RunAttemptLease
	corrupt bool
}

func readRunLifecycleLeases(
	ctx context.Context,
	tx pgx.Tx,
	workspaceID string,
	runIDs []string,
) (map[string]runLifecycleLeaseFact, error) {
	rows, err := tx.Query(
		ctx,
		`SELECT run_id,to_jsonb(lease_row)
		 FROM weave_run_attempt_leases AS lease_row
		 WHERE workspace_id=$1 AND run_id=ANY($2::text[])
		 ORDER BY run_id`,
		workspaceID,
		runIDs,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	facts := make(map[string]runLifecycleLeaseFact, len(runIDs))
	for rows.Next() {
		var runID string
		var raw []byte
		if err := rows.Scan(&runID, &raw); err != nil {
			return nil, err
		}
		if runID == "" {
			return nil, fmt.Errorf("attempt lease row has no decodable run_id")
		}
		if _, duplicate := facts[runID]; duplicate {
			return nil, fmt.Errorf("duplicate attempt lease for run %q", runID)
		}
		var lease RunAttemptLease
		if decodeErr := decodeRunLifecycleRow(raw, &lease); decodeErr != nil ||
			lease.RunID != runID ||
			lease.WorkspaceID != workspaceID ||
			ValidateRunAttemptLease(lease) != nil {
			facts[runID] = runLifecycleLeaseFact{corrupt: true}
			continue
		}
		leaseCopy := lease
		facts[runID] = runLifecycleLeaseFact{lease: &leaseCopy}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return facts, nil
}

type runLifecycleMarkerFact struct {
	marker  *TerminalMarkerV1
	corrupt bool
}

func readRunLifecycleMarkers(
	ctx context.Context,
	tx pgx.Tx,
	workspaceID string,
	runIDs []string,
) (map[string]runLifecycleMarkerFact, error) {
	rows, err := tx.Query(
		ctx,
		`SELECT run_id,to_jsonb(marker_row)
		 FROM weave_run_terminal_markers AS marker_row
		 WHERE workspace_id=$1 AND run_id=ANY($2::text[])
		 ORDER BY run_id`,
		workspaceID,
		runIDs,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	facts := make(map[string]runLifecycleMarkerFact, len(runIDs))
	for rows.Next() {
		var runID string
		var raw []byte
		if err := rows.Scan(&runID, &raw); err != nil {
			return nil, err
		}
		if runID == "" {
			return nil, fmt.Errorf("terminal marker row has no decodable run_id")
		}
		if _, duplicate := facts[runID]; duplicate {
			return nil, fmt.Errorf("duplicate terminal marker for run %q", runID)
		}
		var marker TerminalMarkerV1
		if decodeErr := decodeRunLifecycleRow(raw, &marker); decodeErr != nil ||
			marker.RunID != runID ||
			marker.WorkspaceID != workspaceID ||
			ValidateTerminalMarkerV1(marker) != nil {
			facts[runID] = runLifecycleMarkerFact{corrupt: true}
			continue
		}
		markerCopy := marker
		facts[runID] = runLifecycleMarkerFact{marker: &markerCopy}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return facts, nil
}

func decodeRunLifecycleRow(raw []byte, target any) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return err
	}
	canonical := make(map[string]json.RawMessage, len(fields))
	for key, value := range fields {
		parts := strings.Split(key, "_")
		var name strings.Builder
		for _, part := range parts {
			if part == "" {
				return fmt.Errorf("invalid lifecycle row field %q", key)
			}
			name.WriteString(strings.ToUpper(part[:1]))
			name.WriteString(part[1:])
		}
		canonical[name.String()] = value
	}
	encoded, err := json.Marshal(canonical)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(encoded, target); err != nil {
		return err
	}
	return nil
}

func (reader *PGRunLifecycleReader) observe(
	ctx context.Context,
	result RunLifecycleResult,
) {
	if reader == nil || reader.observer == nil {
		return
	}
	for _, item := range result.Runs {
		codes := make([]RunLifecycleDiagnosticCode, 0, len(item.Diagnostics))
		for _, diagnostic := range item.Diagnostics {
			if len(codes) == 0 || codes[len(codes)-1] != diagnostic.Code {
				codes = append(codes, diagnostic.Code)
			}
		}
		observation := RunLifecycleObservation{
			WorkspaceID:        result.WorkspaceID,
			RunSnapshotID:      result.RunSnapshotID,
			RunID:              item.Expected.RunID,
			ObservedAt:         result.ObservedAt,
			Classification:     item.Classification,
			DiagnosticCodes:    codes,
			AttemptGeneration:  cloneLifecycleInt64(item.AttemptGeneration),
			MarkerSource:       cloneLifecycleMarkerSource(item.MarkerSource),
			MarkerStatus:       cloneLifecycleMarkerStatus(item.MarkerStatus),
			CheckpointSeq:      cloneLifecycleInt64(item.CheckpointSeq),
			RetryCount:         cloneLifecycleInt64(item.RetryCount),
			MarkerAuditState:   cloneLifecycleMarkerAuditState(item.MarkerAuditState),
			MarkerLineageState: cloneLifecycleMarkerLineageState(item.MarkerLineageState),
			LastErrorCode:      cloneLifecycleString(item.LastErrorCode),
		}
		observeRunLifecycleSafely(ctx, reader.observer, observation)
	}
}

func observeRunLifecycleSafely(
	ctx context.Context,
	observer RunLifecycleObserver,
	observation RunLifecycleObservation,
) {
	defer func() { _ = recover() }()
	observer.ObserveRunLifecycle(ctx, observation)
}

func cloneLifecycleMarkerSource(
	value *TerminalMarkerSource,
) *TerminalMarkerSource {
	if value == nil {
		return nil
	}
	cloned := *value
	return &cloned
}

func cloneLifecycleMarkerStatus(
	value *TerminalMarkerStatus,
) *TerminalMarkerStatus {
	if value == nil {
		return nil
	}
	cloned := *value
	return &cloned
}

func cloneLifecycleMarkerAuditState(
	value *TerminalMarkerAuditState,
) *TerminalMarkerAuditState {
	if value == nil {
		return nil
	}
	cloned := *value
	return &cloned
}

func cloneLifecycleMarkerLineageState(
	value *TerminalMarkerLineageState,
) *TerminalMarkerLineageState {
	if value == nil {
		return nil
	}
	cloned := *value
	return &cloned
}
