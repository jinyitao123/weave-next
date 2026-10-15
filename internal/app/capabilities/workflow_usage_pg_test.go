package capabilities

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jinyitao123/weave/internal/base/db"
	"github.com/jinyitao123/weave/internal/base/execution"
	"github.com/jinyitao123/weave/internal/base/testutil"
	"github.com/jinyitao123/weave/internal/kernel/loomruntime"
)

func TestReadWorkflowInvocationUsageUsesRootSubtreeAuditOnce(t *testing.T) {
	pool := workflowUsagePGPool(t)
	ctx := t.Context()
	record := workflowInvocationRecord{
		WorkspaceID: "workspace", WorkflowID: "workflow", WorkflowVersion: 3, RunID: "root-run",
	}
	rootSelf := execution.TerminalUsage{InputTokens: 4, OutputTokens: 1, CostUSD: 0.01}
	firstLeg := execution.TerminalUsage{InputTokens: 10, OutputTokens: 3, CostUSD: 0.02, ToolCalls: 1}
	secondLeg := execution.TerminalUsage{InputTokens: 20, OutputTokens: 4, CostUSD: 0.03, ToolCalls: 2}
	children := []execution.TerminalChildBreakdownV3{
		workflowUsageChild(record, "leg-one", "root-run", 0, "review-one", firstLeg),
		workflowUsageChild(record, "leg-two", "root-run", 1, "review-two", secondLeg),
	}
	root := workflowUsageEntry(record, "root-run", "lead", rootSelf, children, nil, nil, nil, nil)
	first := workflowUsageEntry(record, "leg-one", "review-one", firstLeg, []execution.TerminalChildBreakdownV3{}, nil, nil, stringPointer("root-run"), int64Pointer(0))
	second := workflowUsageEntry(record, "leg-two", "review-two", secondLeg, []execution.TerminalChildBreakdownV3{}, nil, nil, stringPointer("root-run"), int64Pointer(1))
	putWorkflowUsageAudit(t, pool, root)
	putWorkflowUsageAudit(t, pool, first)
	putWorkflowUsageAudit(t, pool, second)
	seedWorkflowUsageRecoveryAttempts(t, pool, record, "lead")

	got, err := readWorkflowInvocationUsage(ctx, pool, record)
	if err != nil {
		t.Fatalf("read invocation usage: %v", err)
	}
	want := execution.TerminalUsage{InputTokens: 34, OutputTokens: 8, CostUSD: 0.06, ToolCalls: 3}
	if got.PhysicalUsage == nil || *got.PhysicalUsage != want {
		t.Fatalf("physical usage = %#v, want root subtree total %#v (child audits must not be added again)", got.PhysicalUsage, want)
	}
	if !got.Complete || got.UnreportedAttempts == nil || *got.UnreportedAttempts != 0 {
		t.Fatalf("usage completeness = (%v,%v), want complete and zero unreported attempts", got.Complete, got.UnreportedAttempts)
	}
}

func TestReadWorkflowInvocationUsageKeepsIncompleteAndUnknownDistinct(t *testing.T) {
	pool := workflowUsagePGPool(t)
	ctx := t.Context()
	partialRecord := workflowInvocationRecord{
		WorkspaceID: "workspace", WorkflowID: "workflow", WorkflowVersion: 3, RunID: "partial-root",
	}
	self := execution.TerminalUsage{InputTokens: 5, OutputTokens: 2}
	leg := execution.TerminalUsage{InputTokens: 7, OutputTokens: 1}
	complete := false
	entry := workflowUsageEntry(partialRecord, partialRecord.RunID, "lead", self,
		[]execution.TerminalChildBreakdownV3{workflowUsageChild(partialRecord, "partial-leg", partialRecord.RunID, 0, "review", leg)},
		&complete, stringPointer("fanout leg attempt has no usage receipt"), nil, nil)
	putWorkflowUsageAudit(t, pool, entry)
	putWorkflowUsageAudit(t, pool, workflowUsageEntry(partialRecord, "partial-leg", "review", leg,
		[]execution.TerminalChildBreakdownV3{}, nil, nil, stringPointer(partialRecord.RunID), int64Pointer(0)))

	got, err := readWorkflowInvocationUsage(ctx, pool, partialRecord)
	if err != nil {
		t.Fatalf("read partial usage: %v", err)
	}
	want := execution.TerminalUsage{InputTokens: 12, OutputTokens: 3}
	if got.PhysicalUsage == nil || *got.PhysicalUsage != want || got.Complete || got.UnreportedAttempts != nil {
		t.Fatalf("partial usage = %#v, want measured subtotal with unknown attempt count", got)
	}

	missing := partialRecord
	missing.RunID = "missing-run"
	got, err = readWorkflowInvocationUsage(ctx, pool, missing)
	if err != nil {
		t.Fatalf("read missing usage: %v", err)
	}
	if got.PhysicalUsage != nil || got.UnreportedAttempts != nil || got.Complete {
		t.Fatalf("missing audit became a measured usage value: %#v", got)
	}

	corrupt := partialRecord
	corrupt.RunID = "corrupt-run"
	if _, err := pool.Exec(ctx, `INSERT INTO loom_store(namespace,key,value,updated_at) VALUES($1,$2,$3,now())`, "audit:"+corrupt.WorkspaceID, corrupt.RunID, []byte(`{"schema_version":3`)); err != nil {
		t.Fatalf("insert corrupt audit: %v", err)
	}
	got, err = readWorkflowInvocationUsage(ctx, pool, corrupt)
	if err != nil {
		t.Fatalf("read corrupt usage: %v", err)
	}
	if got.PhysicalUsage != nil || got.UnreportedAttempts != nil || got.Complete {
		t.Fatalf("corrupt audit became a measured usage value: %#v", got)
	}

	wrongSource := partialRecord
	wrongSource.WorkflowID = "other-workflow"
	got, err = readWorkflowInvocationUsage(ctx, pool, wrongSource)
	if err != nil {
		t.Fatalf("read mismatched source usage: %v", err)
	}
	if got.PhysicalUsage != nil || got.UnreportedAttempts != nil || got.Complete {
		t.Fatalf("mismatched audit identity was projected: %#v", got)
	}
}

func TestReadWorkflowInvocationUsageReportsKnownZeroForDeterministicRun(t *testing.T) {
	pool := workflowUsagePGPool(t)
	record := workflowInvocationRecord{WorkspaceID: "workspace", WorkflowID: "workflow", WorkflowVersion: 3, RunID: "deterministic-run"}
	putWorkflowUsageAudit(t, pool, workflowUsageEntry(record, record.RunID, "lead", execution.TerminalUsage{},
		[]execution.TerminalChildBreakdownV3{}, nil, nil, nil, nil))

	got, err := readWorkflowInvocationUsage(t.Context(), pool, record)
	if err != nil {
		t.Fatalf("read deterministic usage: %v", err)
	}
	if got.PhysicalUsage == nil || *got.PhysicalUsage != (execution.TerminalUsage{}) || !got.Complete ||
		got.UnreportedAttempts == nil || *got.UnreportedAttempts != 0 {
		t.Fatalf("known zero usage projection = %#v", got)
	}
}

func workflowUsagePGPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool := testutil.PostgresPool(t)
	if err := db.Migrate(t.Context(), pool); err != nil {
		t.Fatalf("migrate Weave test schema: %v", err)
	}
	return pool
}

func workflowUsageEntry(
	record workflowInvocationRecord,
	runID, agent string,
	self execution.TerminalUsage,
	children []execution.TerminalChildBreakdownV3,
	complete *bool,
	incompleteReason *string,
	parentRunID *string,
	parentSeq *int64,
) loomruntime.TerminalEntryV3 {
	teamID, workflowID, snapshotID := "team", record.WorkflowID, "snapshot"
	workflowVersion := record.WorkflowVersion
	entry := loomruntime.TerminalEntryV3{
		SchemaVersion: 3, RunID: runID, Agent: agent, Tenant: record.WorkspaceID,
		AttributionScope: loomruntime.TerminalAttributionFixedWorkflow,
		TeamID:           &teamID, WorkflowID: &workflowID, WorkflowVersion: &workflowVersion,
		RunSnapshotID: &snapshotID, Status: "success", StopReason: "completed",
		StartedAt: "2026-10-15T10:00:00Z", EndedAt: "2026-10-15T10:00:01Z", DurationMs: 1000,
		TokensIn: self.InputTokens, TokensOut: self.OutputTokens, CostUSD: self.CostUSD, ToolCalls: self.ToolCalls,
		UsageComplete: complete, ChildBreakdown: children, SelfExclusive: self,
		ParentRunID: parentRunID, ParentSeq: parentSeq, AggregationParentRunID: parentRunID,
	}
	if incompleteReason != nil {
		entry.UsageIncompleteReason = *incompleteReason
	}
	total := self
	for _, child := range children {
		total.InputTokens += child.SelfExclusive.InputTokens
		total.OutputTokens += child.SelfExclusive.OutputTokens
		total.CostUSD += child.SelfExclusive.CostUSD
		total.ToolCalls += child.SelfExclusive.ToolCalls
	}
	entry.SubtreeTotal = total
	return entry
}

func workflowUsageChild(
	record workflowInvocationRecord,
	runID, parentRunID string,
	parentSeq int64,
	agent string,
	usage execution.TerminalUsage,
) execution.TerminalChildBreakdownV3 {
	teamID, workflowID, snapshotID := "team", record.WorkflowID, "snapshot"
	workflowVersion := record.WorkflowVersion
	return execution.TerminalChildBreakdownV3{
		RunID: runID, ParentRunID: parentRunID, ParentSeq: parentSeq, Agent: agent,
		TeamID: &teamID, WorkflowID: &workflowID, WorkflowVersion: &workflowVersion,
		RunSnapshotID: &snapshotID, SelfExclusive: usage,
	}
}

func putWorkflowUsageAudit(t *testing.T, pool *pgxpool.Pool, entry loomruntime.TerminalEntryV3) {
	t.Helper()
	if err := loomruntime.ValidateTerminalV3(entry); err != nil {
		t.Fatalf("validate usage audit fixture: %v", err)
	}
	data, err := json.Marshal(entry)
	if err != nil {
		t.Fatalf("encode usage audit fixture: %v", err)
	}
	if _, err := pool.Exec(t.Context(), `INSERT INTO loom_store(namespace,key,value,updated_at) VALUES($1,$2,$3,now())`, "audit:"+entry.Tenant, entry.RunID, data); err != nil {
		t.Fatalf("insert usage audit fixture: %v", err)
	}
}

func seedWorkflowUsageRecoveryAttempts(t *testing.T, pool *pgxpool.Pool, record workflowInvocationRecord, agent string) {
	t.Helper()
	teamID, workflowID, snapshotID := "team", record.WorkflowID, "snapshot"
	workflowVersion := int32(record.WorkflowVersion)
	base := loomruntime.TerminalMarkerV1{
		WorkspaceID: record.WorkspaceID, RunID: record.RunID, SchemaVersion: 1, Agent: agent,
		AttributionScope: loomruntime.TerminalAttributionFixedWorkflow,
		TeamID:           &teamID, WorkflowID: &workflowID, WorkflowVersion: &workflowVersion, RunSnapshotID: &snapshotID,
		RunStartedAt: "2026-10-15T10:00:00Z", Source: loomruntime.TerminalMarkerSourceNormal,
		EvidenceKind:    loomruntime.TerminalMarkerEvidenceCheckpoint,
		CheckpointGraph: stringPointer("graph"), CheckpointSeq: int64Pointer(1),
		UsageInputTokens: 4, UsageOutputTokens: 1, UsageCostUSD: 0.01,
		AuditState: loomruntime.TerminalMarkerAuditMaterialized, AuditSchemaVersion: int16Pointer(3),
		LineageState: loomruntime.TerminalMarkerLineagePending,
	}
	firstAt := time.Date(2026, 10, 15, 10, 0, 1, 0, time.UTC)
	first := base
	first.AttemptGeneration, first.AttemptID = 1, uuid.New()
	first.Phase, first.Status, first.StopReason = loomruntime.TerminalMarkerPhaseYielded, loomruntime.TerminalMarkerStatusYielded, "awaiting_resume"
	first.TerminalAt, first.CheckpointSavedAt = firstAt, &firstAt
	first.CreatedAt, first.UpdatedAt = firstAt, firstAt
	second := base
	second.AttemptGeneration, second.AttemptID = 2, uuid.New()
	second.Phase, second.Status, second.StopReason = loomruntime.TerminalMarkerPhaseFinal, loomruntime.TerminalMarkerStatusSuccess, "completed"
	second.EvidenceKind = loomruntime.TerminalMarkerEvidenceRunResult
	second.CheckpointGraph, second.CheckpointSeq, second.CheckpointSavedAt = nil, nil, nil
	second.TerminalAt = firstAt.Add(time.Second)
	second.UsageInputTokens, second.UsageOutputTokens, second.UsageCostUSD = 7, 2, 0.02
	second.CreatedAt, second.UpdatedAt = firstAt, second.TerminalAt

	tx, err := pool.Begin(t.Context())
	if err != nil {
		t.Fatalf("begin recovery marker fixture: %v", err)
	}
	defer tx.Rollback(context.Background())
	store := loomruntime.NewPGTerminalStateStore()
	if _, err := store.ApplyTerminalMarkerTransition(t.Context(), tx, first); err != nil {
		t.Fatalf("insert yielded attempt marker: %v", err)
	}
	if _, err := store.ApplyTerminalMarkerTransition(t.Context(), tx, second); err != nil {
		t.Fatalf("advance recovered attempt marker: %v", err)
	}
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatalf("commit recovery marker fixture: %v", err)
	}
}

func stringPointer(value string) *string { return &value }
func int64Pointer(value int64) *int64    { return &value }
func int16Pointer(value int16) *int16    { return &value }
