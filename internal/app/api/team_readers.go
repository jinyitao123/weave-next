package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strconv"

	"github.com/jinyitao123/loom"
	"github.com/jinyitao123/weave/internal/kernel/loomruntime"
	"github.com/jinyitao123/weave/internal/base/snapshot"
	"github.com/labstack/echo/v4"
)

type aggregationMode string

const (
	aggregationModeRootSubtree  aggregationMode = "root-subtree"
	aggregationModeAllExclusive aggregationMode = "all-exclusive"
)

var errTeamAggregationInvariant = errors.New("aggregation_invariant_defect")
var errTeamDomainNotFound = errors.New("team_domain_not_found")
var errTeamReadStoreUnavailable = errors.New("read_store_unavailable")

const teamCostUsageBasis = "confirmed_logical_usage"

type teamView string

const (
	teamViewTeam     teamView = "team"
	teamViewWorkflow teamView = "workflow"
	teamViewRun      teamView = "run"
	teamViewLeg      teamView = "leg"
)

type teamSelector struct {
	View            teamView
	TeamID          string
	WorkflowID      string
	WorkflowVersion int
	RunSnapshotID   string
	LegRunID        string
	AggregationMode aggregationMode
}

type teamExpectedRun struct {
	WorkspaceID            string
	RunID                  string
	Agent                  string
	AttributionScope       loomruntime.TerminalAttributionScope
	TeamID                 *string
	WorkflowID             *string
	WorkflowVersion        *int
	RunSnapshotID          *string
	ParentRunID            *string
	ParentSeq              *int64
	AggregationParentRunID *string
	TaskGroupID            *string
	Registered             bool
}

type teamDiagnostics struct {
	ExpectedRunCount                int             `json:"expected_run_count"`
	PresentTerminalCount            int             `json:"present_terminal_count"`
	TerminalCompleteRunIDs          []string        `json:"terminal_complete_run_ids"`
	TerminalStageYieldedRunIDs      []string        `json:"terminal_stage_yielded_run_ids"`
	TerminalPendingActiveRunIDs     []string        `json:"terminal_pending_active_run_ids"`
	TerminalReconcilingRunIDs       []string        `json:"terminal_reconciling_run_ids"`
	TerminalMissingRunIDs           []string        `json:"terminal_missing_run_ids"`
	TerminalProjectionBlockedRunIDs []string        `json:"terminal_projection_blocked_run_ids"`
	TerminalAssociationDefectRunIDs []string        `json:"terminal_association_defect_run_ids"`
	TerminalLineageDefectRunIDs     []string        `json:"terminal_lineage_defect_run_ids"`
	TerminalLivenessUnknownRunIDs   []string        `json:"terminal_liveness_unknown_run_ids"`
	PreAttributionRunIDs            []string        `json:"pre_attribution_run_ids"`
	YieldedRunIDs                   []string        `json:"yielded_run_ids"`
	CorruptTerminalRunIDs           []string        `json:"corrupt_terminal_run_ids"`
	AggregationComplete             bool            `json:"aggregation_complete"`
	AggregationMode                 aggregationMode `json:"aggregation_mode"`
}

type teamRunRow struct {
	RunID          string                       `json:"run_id"`
	Classification string                       `json:"classification"`
	Terminal       *loomruntime.TerminalEntryV3 `json:"terminal,omitempty"`
}

type teamAgentUsage struct {
	Agent string                    `json:"agent"`
	Runs  int                       `json:"runs"`
	Usage loomruntime.TerminalUsage `json:"usage"`
}

type teamReadReport struct {
	Rows                []teamRunRow
	Totals              *loomruntime.TerminalUsage
	ByAgent             []teamAgentUsage
	Diagnostics         teamDiagnostics
	UsageBasis          string
	ContainsInterrupted bool
}

type teamExpectedSetSource interface {
	resolve(context.Context, string, teamSelector) ([]teamExpectedRun, error)
}

type teamTerminalValueLoader interface {
	load(context.Context, string, []string) (map[string][]byte, error)
}

type teamReader struct {
	expected  teamExpectedSetSource
	terminals teamTerminalValueLoader
	lifecycle loomruntime.RunLifecycleReader
}

type teamSnapshotReader interface {
	ListByTeam(context.Context, string, string) ([]snapshot.TeamRunSnapshot, error)
	ListByWorkflow(context.Context, string, string, string, int) ([]snapshot.TeamRunSnapshot, error)
	GetByRunID(context.Context, string, string) (*snapshot.TeamRunSnapshot, error)
}

type snapshotRegistryTeamExpectedSource struct {
	snapshots teamSnapshotReader
	registry  loomruntime.ExpectedRunRegistry
}

type teamBulkValueStore interface {
	GetByKeys(context.Context, string, []string) (map[string][]byte, error)
}

type pgTeamTerminalLoader struct {
	store teamBulkValueStore
}

type loomStoreTeamTerminalLoader struct {
	store loom.Store
}

func (source snapshotRegistryTeamExpectedSource) resolve(
	ctx context.Context,
	workspaceID string,
	selector teamSelector,
) ([]teamExpectedRun, error) {
	if source.snapshots == nil || source.registry == nil {
		return nil, errors.New("team expected-run source is not configured")
	}
	var snapshots []snapshot.TeamRunSnapshot
	switch selector.View {
	case teamViewTeam:
		listed, err := source.snapshots.ListByTeam(ctx, workspaceID, selector.TeamID)
		if err != nil {
			return nil, fmt.Errorf("list team snapshots: %w", err)
		}
		snapshots = listed
	case teamViewWorkflow:
		listed, err := source.snapshots.ListByWorkflow(
			ctx,
			workspaceID,
			selector.TeamID,
			selector.WorkflowID,
			selector.WorkflowVersion,
		)
		if err != nil {
			return nil, fmt.Errorf("list workflow snapshots: %w", err)
		}
		snapshots = listed
	case teamViewRun:
		selected, err := source.snapshots.GetByRunID(ctx, workspaceID, selector.RunSnapshotID)
		if err != nil {
			if errors.Is(err, snapshot.ErrNotFound) {
				return nil, fmt.Errorf("%w: run snapshot %q", errTeamDomainNotFound, selector.RunSnapshotID)
			}
			return nil, fmt.Errorf("get run snapshot: %w", err)
		}
		snapshots = []snapshot.TeamRunSnapshot{*selected}
	case teamViewLeg:
		record, present, err := source.registry.Get(ctx, workspaceID, selector.LegRunID)
		if err != nil {
			return nil, fmt.Errorf("get expected leg: %w", err)
		}
		if !present || record.RunSnapshotID == nil {
			return nil, fmt.Errorf("%w: leg %q", errTeamDomainNotFound, selector.LegRunID)
		}
		selected, err := source.snapshots.GetByRunID(ctx, workspaceID, *record.RunSnapshotID)
		if err != nil {
			if errors.Is(err, snapshot.ErrNotFound) {
				return nil, fmt.Errorf("%w: leg snapshot %q", errTeamDomainNotFound, *record.RunSnapshotID)
			}
			return nil, fmt.Errorf("get leg run snapshot: %w", err)
		}
		snapshots = []snapshot.TeamRunSnapshot{*selected}
	default:
		return nil, fmt.Errorf("unsupported team view %q", selector.View)
	}

	sort.Slice(snapshots, func(left, right int) bool {
		return snapshots[left].RunID < snapshots[right].RunID
	})
	expected := make([]teamExpectedRun, 0)
	for _, selected := range snapshots {
		root, err := teamExpectedRootFromSnapshot(selected)
		if err != nil {
			return nil, err
		}
		records, err := source.registry.ListBySnapshot(ctx, workspaceID, selected.RunID)
		if err != nil {
			return nil, fmt.Errorf("list expected snapshot runs %q: %w", selected.RunID, err)
		}
		rootIndex := len(expected)
		expected = append(expected, root)
		for _, record := range records {
			run := teamExpectedRunFromRegistry(record)
			if run.RunID == root.RunID && sameTeamExpectedSnapshotFacts(root, run) {
				expected[rootIndex] = run
				continue
			}
			expected = append(expected, run)
		}
	}
	return expected, nil
}

func teamExpectedRootFromSnapshot(selected snapshot.TeamRunSnapshot) (teamExpectedRun, error) {
	root := teamExpectedRun{
		WorkspaceID:   selected.WorkspaceID,
		RunID:         selected.RunID,
		TeamID:        teamStringPointer(selected.TeamID),
		RunSnapshotID: teamStringPointer(selected.RunID),
	}
	switch selected.Mode {
	case "fixed_workflow":
		if selected.WorkflowID == "" || selected.WorkflowVersion < 1 {
			return teamExpectedRun{}, fmt.Errorf("snapshot %q has invalid fixed workflow selector", selected.RunID)
		}
		root.AttributionScope = loomruntime.TerminalAttributionFixedWorkflow
		root.WorkflowID = teamStringPointer(selected.WorkflowID)
		root.WorkflowVersion = teamIntPointer(selected.WorkflowVersion)
	case "free_collab":
		root.AttributionScope = loomruntime.TerminalAttributionTeamFreeCollab
	default:
		return teamExpectedRun{}, fmt.Errorf("snapshot %q has unsupported mode %q", selected.RunID, selected.Mode)
	}
	return root, nil
}

func teamExpectedRunFromRegistry(record loomruntime.ExpectedRunRecordV1) teamExpectedRun {
	return teamExpectedRun{
		WorkspaceID:            record.WorkspaceID,
		RunID:                  record.RunID,
		Agent:                  record.Agent,
		AttributionScope:       record.AttributionScope,
		TeamID:                 cloneTeamString(record.TeamID),
		WorkflowID:             cloneTeamString(record.WorkflowID),
		WorkflowVersion:        cloneTeamInt(record.WorkflowVersion),
		RunSnapshotID:          cloneTeamString(record.RunSnapshotID),
		ParentRunID:            cloneTeamString(record.ParentRunID),
		ParentSeq:              cloneTeamInt64(record.ParentSeq),
		AggregationParentRunID: cloneTeamString(record.AggregationParentRunID),
		TaskGroupID:            cloneTeamString(record.TaskGroupID),
		Registered:             true,
	}
}

func sameTeamExpectedSnapshotFacts(root, registered teamExpectedRun) bool {
	return root.WorkspaceID == registered.WorkspaceID &&
		root.RunID == registered.RunID &&
		root.AttributionScope == registered.AttributionScope &&
		sameTeamString(root.TeamID, registered.TeamID) &&
		sameTeamString(root.WorkflowID, registered.WorkflowID) &&
		sameTeamInt(root.WorkflowVersion, registered.WorkflowVersion) &&
		sameTeamString(root.RunSnapshotID, registered.RunSnapshotID) &&
		registered.AggregationParentRunID == nil
}

func (loader pgTeamTerminalLoader) load(
	ctx context.Context,
	workspaceID string,
	runIDs []string,
) (map[string][]byte, error) {
	if loader.store == nil {
		return nil, errors.New("PG team terminal loader is not configured")
	}
	values, err := loader.store.GetByKeys(ctx, "audit:"+workspaceID, runIDs)
	if err != nil {
		return nil, fmt.Errorf("bulk load team terminals: %w", err)
	}
	if values == nil {
		values = make(map[string][]byte)
	}
	return values, nil
}

func (loader loomStoreTeamTerminalLoader) load(
	ctx context.Context,
	workspaceID string,
	runIDs []string,
) (map[string][]byte, error) {
	if loader.store == nil {
		return nil, errors.New("fallback team terminal loader is not configured")
	}
	namespace := "audit:" + workspaceID
	keys, err := loader.store.List(ctx, namespace, "")
	if err != nil {
		return nil, fmt.Errorf("list team terminal presence: %w", err)
	}
	present := make(map[string]struct{}, len(keys))
	for _, key := range keys {
		present[key] = struct{}{}
	}
	values := make(map[string][]byte)
	for _, runID := range runIDs {
		if _, exists := present[runID]; !exists {
			continue
		}
		value, err := loader.store.Get(ctx, namespace, runID)
		if err != nil {
			return nil, fmt.Errorf("get present team terminal %q: %w", runID, err)
		}
		values[runID] = value
	}
	return values, nil
}

func (reader teamReader) read(
	ctx context.Context,
	workspaceID string,
	selector teamSelector,
) (teamReadReport, error) {
	if reader.expected == nil {
		return teamReadReport{}, errors.New("team expected-run source is not configured")
	}
	if reader.terminals == nil {
		return teamReadReport{}, errors.New("team terminal loader is not configured")
	}
	expected, err := reader.expected.resolve(ctx, workspaceID, selector)
	if err != nil {
		return teamReadReport{}, fmt.Errorf("resolve team expected runs: %w", err)
	}
	for _, run := range expected {
		if run.WorkspaceID != workspaceID {
			return teamReadReport{}, fmt.Errorf(
				"resolve team expected runs: run %q crosses workspace boundary",
				run.RunID,
			)
		}
	}
	runIDs := make([]string, len(expected))
	for index := range expected {
		runIDs[index] = expected[index].RunID
	}
	sort.Strings(runIDs)
	raw, err := reader.terminals.load(ctx, workspaceID, runIDs)
	if err != nil {
		return teamReadReport{}, fmt.Errorf("load team terminal values: %w", err)
	}
	lifecycleByRunID, err := reader.readTeamLifecycles(ctx, workspaceID, expected)
	if err != nil {
		return teamReadReport{}, err
	}
	return classifyAndAggregateTeam(expected, raw, lifecycleByRunID, selector.AggregationMode)
}

func (reader teamReader) readTeamLifecycles(
	ctx context.Context,
	workspaceID string,
	expected []teamExpectedRun,
) (map[string]loomruntime.RunLifecycleItem, error) {
	snapshots := make(map[string]struct{})
	for _, run := range expected {
		if !run.Registered {
			continue
		}
		if run.RunSnapshotID == nil || *run.RunSnapshotID == "" {
			return nil, fmt.Errorf("registered run %q has no run snapshot", run.RunID)
		}
		snapshots[*run.RunSnapshotID] = struct{}{}
	}
	if len(snapshots) == 0 {
		return map[string]loomruntime.RunLifecycleItem{}, nil
	}
	if reader.lifecycle == nil {
		return nil, fmt.Errorf("%w: team lifecycle reader is not configured", errTeamReadStoreUnavailable)
	}
	snapshotIDs := make([]string, 0, len(snapshots))
	for snapshotID := range snapshots {
		snapshotIDs = append(snapshotIDs, snapshotID)
	}
	sort.Strings(snapshotIDs)
	items := make(map[string]loomruntime.RunLifecycleItem)
	for _, snapshotID := range snapshotIDs {
		result, err := reader.lifecycle.ReadBySnapshot(ctx, loomruntime.RunLifecycleQuery{
			WorkspaceID:   workspaceID,
			RunSnapshotID: snapshotID,
		})
		if err != nil {
			var readErr *loomruntime.RunLifecycleReadError
			if errors.As(err, &readErr) && readErr.Retryable {
				return nil, fmt.Errorf("%w: lifecycle snapshot %q: %v", errTeamReadStoreUnavailable, snapshotID, err)
			}
			return nil, fmt.Errorf("read lifecycle snapshot %q: %w", snapshotID, err)
		}
		for _, item := range result.Runs {
			if _, duplicate := items[item.Expected.RunID]; duplicate {
				return nil, fmt.Errorf("lifecycle reader duplicated run %q", item.Expected.RunID)
			}
			items[item.Expected.RunID] = item
		}
	}
	return items, nil
}

func classifyAndAggregateTeam(
	expected []teamExpectedRun,
	rawByRunID map[string][]byte,
	lifecycleByRunID map[string]loomruntime.RunLifecycleItem,
	mode aggregationMode,
) (teamReadReport, error) {
	diagnostics := newTeamDiagnostics(mode)
	expectedByID := make(map[string]teamExpectedRun, len(expected))
	lineageDefects := make(map[string]struct{})
	for _, run := range expected {
		current, duplicate := expectedByID[run.RunID]
		if duplicate {
			if !sameTeamExpectedRun(current, run) {
				lineageDefects[run.RunID] = struct{}{}
			}
			continue
		}
		expectedByID[run.RunID] = run
	}
	markTeamLineageCycles(expectedByID, lineageDefects)

	runIDs := make([]string, 0, len(expectedByID))
	for runID := range expectedByID {
		runIDs = append(runIDs, runID)
	}
	sort.Strings(runIDs)
	diagnostics.ExpectedRunCount = len(runIDs)

	rows := make([]teamRunRow, 0, len(runIDs))
	aggregationComplete := true
	containsInterrupted := false
	for _, runID := range runIDs {
		expectedRun := expectedByID[runID]
		raw, present := rawByRunID[runID]
		if present {
			diagnostics.PresentTerminalCount++
		}
		inspection := loomruntime.InspectTerminalRecord(present, raw)
		terminalClassification := inspection.Classification
		if terminalClassification == loomruntime.TerminalRecordLineageDefect {
			var lineageEntry loomruntime.TerminalEntryV3
			if json.Unmarshal(raw, &lineageEntry) == nil &&
				!teamTerminalAssociationMatches(expectedRun, lineageEntry) {
				terminalClassification = loomruntime.TerminalRecordAssociationDefect
				inspection.Classification = terminalClassification
			}
		}
		item, hasLifecycle := lifecycleByRunID[runID]
		grandfathered := !expectedRun.Registered ||
			(hasLifecycle && teamLifecycleGrandfathered(item, inspection))
		classification := teamLifecycleClassification(
			expectedRun,
			item,
			hasLifecycle,
			inspection,
			grandfathered,
		)
		var entry *loomruntime.TerminalEntryV3
		if teamClassificationIsIncluded(classification) &&
			terminalClassification == loomruntime.TerminalRecordValid {
			entry = inspection.Entry
		}
		if entry != nil && !teamTerminalAssociationMatches(expectedRun, *entry) {
			classification = string(loomruntime.RunLifecycleTerminalAssociationDefect)
			entry = nil
		}
		if entry != nil && !teamTerminalLineageMatches(expectedRun, *entry) {
			classification = string(loomruntime.RunLifecycleTerminalLineageDefect)
			entry = nil
		}
		if entry != nil && expectedRun.AggregationParentRunID != nil {
			parent, parentPresent := expectedByID[*expectedRun.AggregationParentRunID]
			if !parentPresent || !sameTeamDomain(expectedRun, parent) {
				classification = string(loomruntime.RunLifecycleTerminalLineageDefect)
				entry = nil
			}
		}
		if _, defective := lineageDefects[runID]; defective {
			classification = string(loomruntime.RunLifecycleTerminalLineageDefect)
			entry = nil
		}
		if teamClassificationIsIncluded(classification) && entry == nil {
			classification = teamLifecycleClassificationFromTerminal(terminalClassification)
		}
		appendTeamDiagnostic(
			&diagnostics,
			classification,
			runID,
			item,
			hasLifecycle,
			terminalClassification,
			!expectedRun.Registered && expectedRun.AggregationParentRunID == nil,
		)
		if !teamClassificationIsIncluded(classification) {
			aggregationComplete = false
		}
		if entry != nil && hasLifecycle &&
			item.EvidenceKind != nil &&
			*item.EvidenceKind == loomruntime.TerminalMarkerEvidenceCheckpoint &&
			item.MarkerAuditState != nil &&
			*item.MarkerAuditState == loomruntime.TerminalMarkerAuditMaterialized {
			containsInterrupted = true
		}
		rows = append(rows, teamRunRow{
			RunID:          runID,
			Classification: classification,
			Terminal:       entry,
		})
	}

	diagnostics.AggregationComplete = aggregationComplete
	report := teamReadReport{
		Rows:                rows,
		Diagnostics:         diagnostics,
		UsageBasis:          teamCostUsageBasis,
		ContainsInterrupted: containsInterrupted,
	}
	if !diagnostics.AggregationComplete {
		return report, nil
	}
	rootTotals, rootByAgent, err := aggregateTeamRootSubtree(expectedByID, rows)
	if err != nil {
		report.Diagnostics.AggregationComplete = false
		return report, fmt.Errorf("%w: root-subtree: %v", errTeamAggregationInvariant, err)
	}
	exclusiveTotals, exclusiveByAgent, err := aggregateTeamAllExclusive(expectedByID, rows)
	if err != nil {
		report.Diagnostics.AggregationComplete = false
		return report, fmt.Errorf("%w: all-exclusive: %v", errTeamAggregationInvariant, err)
	}
	if rootTotals != exclusiveTotals || !sameTeamAgentUsage(rootByAgent, exclusiveByAgent) {
		report.Diagnostics.AggregationComplete = false
		return report, fmt.Errorf("%w: aggregation modes do not reconcile", errTeamAggregationInvariant)
	}
	switch mode {
	case aggregationModeRootSubtree:
		report.Totals = &rootTotals
		report.ByAgent = rootByAgent
	case aggregationModeAllExclusive:
		report.Totals = &exclusiveTotals
		report.ByAgent = exclusiveByAgent
	default:
		report.Diagnostics.AggregationComplete = false
		return report, fmt.Errorf("%w: unsupported aggregation mode %q", errTeamAggregationInvariant, mode)
	}
	return report, nil
}

func teamLifecycleGrandfathered(
	item loomruntime.RunLifecycleItem,
	inspection loomruntime.TerminalRecordInspection,
) bool {
	if item.Classification != loomruntime.RunLifecycleTerminalLivenessUnknown ||
		inspection.Classification != loomruntime.TerminalRecordValid ||
		inspection.Entry == nil {
		return false
	}
	for _, diagnostic := range item.Diagnostics {
		if diagnostic.Code == loomruntime.RunLifecycleDiagnosticPreAttribution &&
			diagnostic.Fact == loomruntime.RunLifecycleFactReceiptAbsent {
			return true
		}
	}
	return false
}

func teamLifecycleClassification(
	expected teamExpectedRun,
	item loomruntime.RunLifecycleItem,
	hasLifecycle bool,
	inspection loomruntime.TerminalRecordInspection,
	grandfathered bool,
) string {
	if expected.Registered && hasLifecycle &&
		!sameTeamExpectedRun(expected, teamExpectedRunFromRegistry(item.Expected)) {
		return string(loomruntime.RunLifecycleTerminalAssociationDefect)
	}
	if grandfathered &&
		inspection.Classification == loomruntime.TerminalRecordValid &&
		inspection.Entry != nil {
		return "grandfathered"
	}
	if !expected.Registered {
		return teamLifecycleClassificationFromTerminal(inspection.Classification)
	}
	if !hasLifecycle {
		return string(loomruntime.RunLifecycleTerminalLivenessUnknown)
	}
	switch item.Classification {
	case loomruntime.RunLifecycleTerminalComplete,
		loomruntime.RunLifecycleTerminalStageYielded,
		loomruntime.RunLifecycleTerminalPendingActive,
		loomruntime.RunLifecycleTerminalReconciling,
		loomruntime.RunLifecycleTerminalMissing,
		loomruntime.RunLifecycleTerminalProjectionBlocked,
		loomruntime.RunLifecycleTerminalAssociationDefect,
		loomruntime.RunLifecycleTerminalLineageDefect,
		loomruntime.RunLifecycleTerminalLivenessUnknown:
		return string(item.Classification)
	default:
		return string(loomruntime.RunLifecycleTerminalLivenessUnknown)
	}
}

func teamLifecycleClassificationFromTerminal(
	classification loomruntime.TerminalRecordClassification,
) string {
	switch classification {
	case loomruntime.TerminalRecordMissing:
		return string(loomruntime.RunLifecycleTerminalMissing)
	case loomruntime.TerminalRecordLineageDefect:
		return string(loomruntime.RunLifecycleTerminalLineageDefect)
	case loomruntime.TerminalRecordAssociationDefect,
		loomruntime.TerminalRecordCorrupt:
		return string(loomruntime.RunLifecycleTerminalAssociationDefect)
	case loomruntime.TerminalRecordPreAttribution,
		loomruntime.TerminalRecordValid:
		return string(loomruntime.RunLifecycleTerminalLivenessUnknown)
	default:
		return string(loomruntime.RunLifecycleTerminalLivenessUnknown)
	}
}

func teamClassificationIsIncluded(classification string) bool {
	return classification == "grandfathered" ||
		classification == string(loomruntime.RunLifecycleTerminalComplete) ||
		classification == string(loomruntime.RunLifecycleTerminalStageYielded)
}

func selectTeamAggregationMode(values url.Values) (aggregationMode, error) {
	modes, present := values["aggregation_mode"]
	if !present || len(modes) != 1 {
		return "", errors.New("exactly one aggregation_mode is required")
	}
	mode := aggregationMode(modes[0])
	if mode != aggregationModeRootSubtree && mode != aggregationModeAllExclusive {
		return "", fmt.Errorf("unsupported aggregation_mode %q", modes[0])
	}
	return mode, nil
}

func parseTeamSelector(
	values url.Values,
	legRunID string,
	usageEndpoint bool,
) (teamSelector, bool, error) {
	teamKeys := []string{
		"view",
		"team_id",
		"workflow_id",
		"workflow_version",
		"run_snapshot_id",
		"aggregation_mode",
	}
	teamAware := false
	for _, key := range teamKeys {
		if _, present := values[key]; present {
			teamAware = true
		}
	}
	if !teamAware {
		return teamSelector{}, false, nil
	}
	viewValue, err := singleTeamQueryValue(values, "view")
	if err != nil {
		return teamSelector{}, true, err
	}
	mode, err := selectTeamAggregationMode(values)
	if err != nil {
		return teamSelector{}, true, err
	}
	selector := teamSelector{
		View:            teamView(viewValue),
		AggregationMode: mode,
	}
	switch selector.View {
	case teamViewTeam:
		if legRunID != "" {
			return teamSelector{}, true, errors.New("team view is not valid for run detail")
		}
		selector.TeamID, err = requiredTeamQueryValue(values, "team_id")
		if err != nil {
			return teamSelector{}, true, err
		}
		if teamQueryPresent(values, "workflow_id", "workflow_version", "run_snapshot_id") {
			return teamSelector{}, true, errors.New("team view forbids workflow and run selectors")
		}
	case teamViewWorkflow:
		if legRunID != "" {
			return teamSelector{}, true, errors.New("workflow view is not valid for run detail")
		}
		selector.TeamID, err = requiredTeamQueryValue(values, "team_id")
		if err != nil {
			return teamSelector{}, true, err
		}
		selector.WorkflowID, err = requiredTeamQueryValue(values, "workflow_id")
		if err != nil {
			return teamSelector{}, true, err
		}
		version, err := requiredTeamQueryValue(values, "workflow_version")
		if err != nil {
			return teamSelector{}, true, err
		}
		selector.WorkflowVersion, err = strconv.Atoi(version)
		if err != nil || selector.WorkflowVersion < 1 {
			return teamSelector{}, true, errors.New("workflow_version must be an integer >= 1")
		}
		if teamQueryPresent(values, "run_snapshot_id") {
			return teamSelector{}, true, errors.New("workflow view forbids run_snapshot_id")
		}
	case teamViewRun:
		if legRunID != "" {
			return teamSelector{}, true, errors.New("run view is not valid for run detail")
		}
		selector.RunSnapshotID, err = requiredTeamQueryValue(values, "run_snapshot_id")
		if err != nil {
			return teamSelector{}, true, err
		}
		if teamQueryPresent(values, "team_id", "workflow_id", "workflow_version") {
			return teamSelector{}, true, errors.New("run view forbids team and workflow selectors")
		}
	case teamViewLeg:
		if usageEndpoint || legRunID == "" {
			return teamSelector{}, true, errors.New("leg view is only valid for run detail")
		}
		if teamQueryPresent(values, "team_id", "workflow_id", "workflow_version", "run_snapshot_id") {
			return teamSelector{}, true, errors.New("leg view forbids team, workflow, and run selectors")
		}
		selector.LegRunID = legRunID
	default:
		return teamSelector{}, true, fmt.Errorf("unsupported team view %q", viewValue)
	}
	for _, key := range teamKeys {
		if _, present := values[key]; present && len(values[key]) != 1 {
			return teamSelector{}, true, fmt.Errorf("query parameter %q must appear exactly once", key)
		}
	}
	return selector, true, nil
}

func singleTeamQueryValue(values url.Values, key string) (string, error) {
	all, present := values[key]
	if !present || len(all) != 1 {
		return "", fmt.Errorf("query parameter %q must appear exactly once", key)
	}
	return all[0], nil
}

func requiredTeamQueryValue(values url.Values, key string) (string, error) {
	value, err := singleTeamQueryValue(values, key)
	if err != nil {
		return "", err
	}
	if value == "" {
		return "", fmt.Errorf("query parameter %q must be non-empty", key)
	}
	return value, nil
}

func teamQueryPresent(values url.Values, keys ...string) bool {
	for _, key := range keys {
		if _, present := values[key]; present {
			return true
		}
	}
	return false
}

type teamRunsResponse struct {
	Runs                []teamRunRow               `json:"runs"`
	Total               int                        `json:"total"`
	Limit               int                        `json:"limit"`
	Offset              int                        `json:"offset"`
	Totals              *loomruntime.TerminalUsage `json:"totals,omitempty"`
	ByAgent             *[]teamAgentUsage          `json:"by_agent,omitempty"`
	UsageBasis          string                     `json:"usage_basis"`
	ContainsInterrupted bool                       `json:"contains_interrupted"`
	Diagnostics         teamDiagnostics            `json:"diagnostics"`
}

type teamLegResponse struct {
	Run                 teamRunRow                 `json:"run"`
	Totals              *loomruntime.TerminalUsage `json:"totals,omitempty"`
	ByAgent             *[]teamAgentUsage          `json:"by_agent,omitempty"`
	UsageBasis          string                     `json:"usage_basis"`
	ContainsInterrupted bool                       `json:"contains_interrupted"`
	Diagnostics         teamDiagnostics            `json:"diagnostics"`
}

type teamUsageResponse struct {
	Totals              *loomruntime.TerminalUsage `json:"totals,omitempty"`
	ByAgent             *[]teamAgentUsage          `json:"by_agent,omitempty"`
	UsageBasis          string                     `json:"usage_basis"`
	ContainsInterrupted bool                       `json:"contains_interrupted"`
	Diagnostics         teamDiagnostics            `json:"diagnostics"`
}

func (s *Server) handleTeamAwareRuns(
	c echo.Context,
	selector teamSelector,
	limit int,
	offset int,
) error {
	report, err := s.readTeamAware(c, selector)
	if err != nil {
		return writeTeamReadError(c, report, err)
	}
	rows := report.Rows
	if offset >= len(rows) {
		rows = []teamRunRow{}
	} else {
		end := offset + limit
		if end > len(rows) {
			end = len(rows)
		}
		rows = rows[offset:end]
	}
	return c.JSON(http.StatusOK, teamRunsResponse{
		Runs:                rows,
		Total:               report.Diagnostics.ExpectedRunCount,
		Limit:               limit,
		Offset:              offset,
		Totals:              report.Totals,
		ByAgent:             teamAgentUsageResponse(report.ByAgent),
		UsageBasis:          report.UsageBasis,
		ContainsInterrupted: report.ContainsInterrupted,
		Diagnostics:         report.Diagnostics,
	})
}

func (s *Server) handleTeamAwareLeg(c echo.Context, selector teamSelector) error {
	report, err := s.readTeamAware(c, selector)
	if err != nil {
		return writeTeamReadError(c, report, err)
	}
	for _, row := range report.Rows {
		if row.RunID == selector.LegRunID {
			return c.JSON(http.StatusOK, teamLegResponse{
				Run:                 row,
				Totals:              report.Totals,
				ByAgent:             teamAgentUsageResponse(report.ByAgent),
				UsageBasis:          report.UsageBasis,
				ContainsInterrupted: report.ContainsInterrupted,
				Diagnostics:         report.Diagnostics,
			})
		}
	}
	return c.JSON(http.StatusNotFound, map[string]string{"error": errTeamDomainNotFound.Error()})
}

func (s *Server) handleTeamAwareUsage(c echo.Context, selector teamSelector) error {
	report, err := s.readTeamAware(c, selector)
	if err != nil {
		return writeTeamReadError(c, report, err)
	}
	return c.JSON(http.StatusOK, teamUsageResponse{
		Totals:              report.Totals,
		ByAgent:             teamAgentUsageResponse(report.ByAgent),
		UsageBasis:          report.UsageBasis,
		ContainsInterrupted: report.ContainsInterrupted,
		Diagnostics:         report.Diagnostics,
	})
}

func (s *Server) readTeamAware(
	c echo.Context,
	selector teamSelector,
) (teamReadReport, error) {
	if s.TeamReader == nil {
		return teamReadReport{}, errors.New("team reader is not configured")
	}
	return s.TeamReader.read(c.Request().Context(), getTenant(c), selector)
}

func writeTeamReadError(c echo.Context, report teamReadReport, err error) error {
	switch {
	case errors.Is(err, errTeamAggregationInvariant):
		return c.JSON(http.StatusConflict, struct {
			Error       string          `json:"error"`
			Diagnostics teamDiagnostics `json:"diagnostics"`
		}{
			Error:       errTeamAggregationInvariant.Error(),
			Diagnostics: report.Diagnostics,
		})
	case errors.Is(err, errTeamDomainNotFound):
		return c.JSON(http.StatusNotFound, map[string]string{"error": errTeamDomainNotFound.Error()})
	case errors.Is(err, errTeamReadStoreUnavailable):
		return c.JSON(http.StatusServiceUnavailable, struct {
			Error     string `json:"error"`
			Retryable bool   `json:"retryable"`
		}{
			Error:     errTeamReadStoreUnavailable.Error(),
			Retryable: true,
		})
	default:
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "team_reader_unavailable"})
	}
}

func teamAgentUsageResponse(values []teamAgentUsage) *[]teamAgentUsage {
	if values == nil {
		return nil
	}
	return &values
}

func aggregateTeamAllExclusive(
	expectedByID map[string]teamExpectedRun,
	rows []teamRunRow,
) (loomruntime.TerminalUsage, []teamAgentUsage, error) {
	contributions := make([]teamUsageContribution, 0, len(rows))
	for _, row := range rows {
		if row.Terminal == nil {
			return loomruntime.TerminalUsage{}, nil, fmt.Errorf("run %q has no valid terminal", row.RunID)
		}
		if _, expected := expectedByID[row.RunID]; !expected {
			return loomruntime.TerminalUsage{}, nil, fmt.Errorf("run %q is not expected", row.RunID)
		}
		contributions = append(contributions, teamUsageContribution{
			RunID: row.RunID,
			Agent: row.Terminal.Agent,
			Usage: row.Terminal.SelfExclusive,
		})
	}
	if len(contributions) != len(expectedByID) {
		return loomruntime.TerminalUsage{}, nil, fmt.Errorf(
			"exclusive contributions = %d, want %d",
			len(contributions),
			len(expectedByID),
		)
	}
	return foldTeamContributions(contributions)
}

type teamUsageContribution struct {
	RunID string
	Agent string
	Usage loomruntime.TerminalUsage
}

func foldTeamContributions(
	contributions []teamUsageContribution,
) (loomruntime.TerminalUsage, []teamAgentUsage, error) {
	sort.Slice(contributions, func(left, right int) bool {
		return contributions[left].RunID < contributions[right].RunID
	})
	total := loomruntime.TerminalUsage{}
	agents := make(map[string]teamAgentUsage)
	for _, contribution := range contributions {
		next, err := addTeamUsage(total, contribution.Usage)
		if err != nil {
			return loomruntime.TerminalUsage{}, nil, fmt.Errorf(
				"run %q totals: %w",
				contribution.RunID,
				err,
			)
		}
		total = next
		if err := addTeamAgentContribution(
			agents,
			contribution.RunID,
			contribution.Agent,
			contribution.Usage,
		); err != nil {
			return loomruntime.TerminalUsage{}, nil, err
		}
	}
	return total, normalizeTeamAgentUsage(agents), nil
}

func aggregateTeamRootSubtree(
	expectedByID map[string]teamExpectedRun,
	rows []teamRunRow,
) (loomruntime.TerminalUsage, []teamAgentUsage, error) {
	entries := make(map[string]loomruntime.TerminalEntryV3, len(rows))
	for _, row := range rows {
		if row.Terminal == nil {
			return loomruntime.TerminalUsage{}, nil, fmt.Errorf("run %q has no valid terminal", row.RunID)
		}
		entries[row.RunID] = *row.Terminal
	}

	rootByRun, err := teamAggregationRoots(expectedByID)
	if err != nil {
		return loomruntime.TerminalUsage{}, nil, err
	}
	rootIDs := make([]string, 0)
	for runID, rootID := range rootByRun {
		if runID == rootID {
			rootIDs = append(rootIDs, runID)
		}
	}
	sort.Strings(rootIDs)

	contributions := make([]teamUsageContribution, 0, len(expectedByID))
	for _, rootID := range rootIDs {
		root := entries[rootID]
		rootContributions := []teamUsageContribution{{
			RunID: rootID,
			Agent: root.Agent,
			Usage: root.SelfExclusive,
		}}
		if root.RunID != rootID {
			return loomruntime.TerminalUsage{}, nil, fmt.Errorf(
				"root %q terminal identifies run %q",
				rootID,
				root.RunID,
			)
		}

		seen := make(map[string]struct{}, len(root.ChildBreakdown))
		for _, descendant := range root.ChildBreakdown {
			if descendant.RunID == rootID {
				return loomruntime.TerminalUsage{}, nil, fmt.Errorf("root %q breakdown contains itself", rootID)
			}
			if _, duplicate := seen[descendant.RunID]; duplicate {
				return loomruntime.TerminalUsage{}, nil, fmt.Errorf(
					"root %q breakdown duplicates %q",
					rootID,
					descendant.RunID,
				)
			}
			seen[descendant.RunID] = struct{}{}
			expected, exists := expectedByID[descendant.RunID]
			entry, terminalExists := entries[descendant.RunID]
			if !exists || !terminalExists || rootByRun[descendant.RunID] != rootID {
				return loomruntime.TerminalUsage{}, nil, fmt.Errorf(
					"root %q breakdown has unexpected descendant %q",
					rootID,
					descendant.RunID,
				)
			}
			if descendant.Agent != entry.Agent ||
				descendant.SelfExclusive != entry.SelfExclusive ||
				descendant.ParentRunID != teamStringValue(expected.ParentRunID) ||
				descendant.ParentSeq != teamInt64Value(expected.ParentSeq) ||
				!sameTeamString(descendant.TeamID, expected.TeamID) ||
				!sameTeamString(descendant.WorkflowID, expected.WorkflowID) ||
				!sameTeamInt(descendant.WorkflowVersion, expected.WorkflowVersion) ||
				!sameTeamString(descendant.RunSnapshotID, expected.RunSnapshotID) {
				return loomruntime.TerminalUsage{}, nil, fmt.Errorf(
					"root %q breakdown disagrees with descendant %q",
					rootID,
					descendant.RunID,
				)
			}
			rootContributions = append(rootContributions, teamUsageContribution{
				RunID: descendant.RunID,
				Agent: descendant.Agent,
				Usage: descendant.SelfExclusive,
			})
		}
		for runID, expectedRootID := range rootByRun {
			if expectedRootID == rootID && runID != rootID {
				if _, present := seen[runID]; !present {
					return loomruntime.TerminalUsage{}, nil, fmt.Errorf(
						"root %q breakdown is missing descendant %q",
						rootID,
						runID,
					)
				}
			}
		}
		subtreeTotal, _, err := foldTeamContributions(rootContributions)
		if err != nil {
			return loomruntime.TerminalUsage{}, nil, fmt.Errorf("root %q totals: %w", rootID, err)
		}
		if subtreeTotal != root.SubtreeTotal {
			return loomruntime.TerminalUsage{}, nil, fmt.Errorf(
				"root %q canonical subtree total does not match stored total",
				rootID,
			)
		}
		contributions = append(contributions, rootContributions...)
	}
	if len(contributions) != len(expectedByID) {
		return loomruntime.TerminalUsage{}, nil, fmt.Errorf(
			"root contributions = %d, want %d",
			len(contributions),
			len(expectedByID),
		)
	}
	return foldTeamContributions(contributions)
}

func teamAggregationRoots(
	expectedByID map[string]teamExpectedRun,
) (map[string]string, error) {
	rootByRun := make(map[string]string, len(expectedByID))
	for runID := range expectedByID {
		path := make(map[string]struct{})
		currentID := runID
		for {
			if _, cycle := path[currentID]; cycle {
				return nil, fmt.Errorf("aggregation cycle contains %q", currentID)
			}
			path[currentID] = struct{}{}
			current, exists := expectedByID[currentID]
			if !exists {
				return nil, fmt.Errorf("aggregation parent %q is not expected", currentID)
			}
			if current.AggregationParentRunID == nil {
				rootByRun[runID] = currentID
				break
			}
			currentID = *current.AggregationParentRunID
		}
	}
	return rootByRun, nil
}

func markTeamLineageCycles(
	expectedByID map[string]teamExpectedRun,
	defects map[string]struct{},
) {
	markTeamCycles(expectedByID, defects, func(run teamExpectedRun) *string {
		return run.AggregationParentRunID
	})
	markTeamCycles(expectedByID, defects, func(run teamExpectedRun) *string {
		return run.ParentRunID
	})
}

func markTeamCycles(
	expectedByID map[string]teamExpectedRun,
	defects map[string]struct{},
	parentOf func(teamExpectedRun) *string,
) {
	for runID := range expectedByID {
		path := make([]string, 0)
		pathIndex := make(map[string]int)
		currentID := runID
		for {
			if index, cycle := pathIndex[currentID]; cycle {
				for _, defectiveRunID := range path[index:] {
					defects[defectiveRunID] = struct{}{}
				}
				break
			}
			current, present := expectedByID[currentID]
			if !present || parentOf(current) == nil {
				break
			}
			pathIndex[currentID] = len(path)
			path = append(path, currentID)
			currentID = *parentOf(current)
		}
	}
}

func addTeamAgentContribution(
	agents map[string]teamAgentUsage,
	runID string,
	agent string,
	usage loomruntime.TerminalUsage,
) error {
	if agent == "" {
		return fmt.Errorf("run %q has empty agent", runID)
	}
	current := agents[agent]
	next, err := addTeamUsage(current.Usage, usage)
	if err != nil {
		return fmt.Errorf("agent %q run %q: %w", agent, runID, err)
	}
	current.Agent = agent
	current.Runs++
	current.Usage = next
	agents[agent] = current
	return nil
}

func normalizeTeamAgentUsage(agents map[string]teamAgentUsage) []teamAgentUsage {
	result := make([]teamAgentUsage, 0, len(agents))
	for _, usage := range agents {
		result = append(result, usage)
	}
	sort.Slice(result, func(left, right int) bool {
		return result[left].Agent < result[right].Agent
	})
	return result
}

func sameTeamAgentUsage(left, right []teamAgentUsage) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func addTeamUsage(
	left loomruntime.TerminalUsage,
	right loomruntime.TerminalUsage,
) (loomruntime.TerminalUsage, error) {
	input, ok := addTeamInt(left.InputTokens, right.InputTokens)
	if !ok {
		return loomruntime.TerminalUsage{}, errors.New("input_tokens overflows int")
	}
	output, ok := addTeamInt(left.OutputTokens, right.OutputTokens)
	if !ok {
		return loomruntime.TerminalUsage{}, errors.New("output_tokens overflows int")
	}
	cost := left.CostUSD + right.CostUSD
	if left.CostUSD < 0 || right.CostUSD < 0 || cost < 0 ||
		math.IsNaN(left.CostUSD) || math.IsNaN(right.CostUSD) || math.IsNaN(cost) ||
		math.IsInf(left.CostUSD, 0) || math.IsInf(right.CostUSD, 0) || math.IsInf(cost, 0) {
		return loomruntime.TerminalUsage{}, errors.New("cost_usd must remain finite and non-negative")
	}
	return loomruntime.TerminalUsage{
		InputTokens:  input,
		OutputTokens: output,
		CostUSD:      cost,
	}, nil
}

func addTeamInt(left, right int) (int, bool) {
	if right > 0 && left > math.MaxInt-right {
		return 0, false
	}
	if right < 0 && left < math.MinInt-right {
		return 0, false
	}
	return left + right, true
}

func teamStringValue(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func teamInt64Value(value *int64) int64 {
	if value == nil {
		return 0
	}
	return *value
}

func newTeamDiagnostics(mode aggregationMode) teamDiagnostics {
	return teamDiagnostics{
		TerminalCompleteRunIDs:          []string{},
		TerminalStageYieldedRunIDs:      []string{},
		TerminalPendingActiveRunIDs:     []string{},
		TerminalReconcilingRunIDs:       []string{},
		TerminalMissingRunIDs:           []string{},
		TerminalProjectionBlockedRunIDs: []string{},
		TerminalAssociationDefectRunIDs: []string{},
		TerminalLineageDefectRunIDs:     []string{},
		TerminalLivenessUnknownRunIDs:   []string{},
		PreAttributionRunIDs:            []string{},
		YieldedRunIDs:                   []string{},
		CorruptTerminalRunIDs:           []string{},
		AggregationMode:                 mode,
	}
}

func appendTeamDiagnostic(
	diagnostics *teamDiagnostics,
	classification string,
	runID string,
	item loomruntime.RunLifecycleItem,
	hasLifecycle bool,
	terminalClassification loomruntime.TerminalRecordClassification,
	syntheticRoot bool,
) {
	switch classification {
	case string(loomruntime.RunLifecycleTerminalComplete):
		diagnostics.TerminalCompleteRunIDs = append(diagnostics.TerminalCompleteRunIDs, runID)
	case string(loomruntime.RunLifecycleTerminalStageYielded):
		diagnostics.TerminalStageYieldedRunIDs = append(diagnostics.TerminalStageYieldedRunIDs, runID)
		diagnostics.YieldedRunIDs = append(diagnostics.YieldedRunIDs, runID)
	case string(loomruntime.RunLifecycleTerminalPendingActive):
		diagnostics.TerminalPendingActiveRunIDs = append(diagnostics.TerminalPendingActiveRunIDs, runID)
	case string(loomruntime.RunLifecycleTerminalReconciling):
		diagnostics.TerminalReconcilingRunIDs = append(diagnostics.TerminalReconcilingRunIDs, runID)
	case string(loomruntime.RunLifecycleTerminalMissing):
		diagnostics.TerminalMissingRunIDs = append(diagnostics.TerminalMissingRunIDs, runID)
	case string(loomruntime.RunLifecycleTerminalProjectionBlocked):
		diagnostics.TerminalProjectionBlockedRunIDs = append(diagnostics.TerminalProjectionBlockedRunIDs, runID)
	case string(loomruntime.RunLifecycleTerminalAssociationDefect):
		diagnostics.TerminalAssociationDefectRunIDs = append(diagnostics.TerminalAssociationDefectRunIDs, runID)
	case string(loomruntime.RunLifecycleTerminalLineageDefect):
		diagnostics.TerminalLineageDefectRunIDs = append(diagnostics.TerminalLineageDefectRunIDs, runID)
	case string(loomruntime.RunLifecycleTerminalLivenessUnknown):
		diagnostics.TerminalLivenessUnknownRunIDs = append(diagnostics.TerminalLivenessUnknownRunIDs, runID)
	}
	preAttribution := syntheticRoot ||
		terminalClassification == loomruntime.TerminalRecordPreAttribution
	corruptTerminal := terminalClassification == loomruntime.TerminalRecordCorrupt
	if hasLifecycle {
		for _, diagnostic := range item.Diagnostics {
			preAttribution = preAttribution ||
				diagnostic.Code == loomruntime.RunLifecycleDiagnosticPreAttribution
			corruptTerminal = corruptTerminal ||
				diagnostic.Code == loomruntime.RunLifecycleDiagnosticTerminalCorrupt
		}
	}
	if preAttribution {
		diagnostics.PreAttributionRunIDs = append(diagnostics.PreAttributionRunIDs, runID)
	}
	if corruptTerminal {
		diagnostics.CorruptTerminalRunIDs = append(diagnostics.CorruptTerminalRunIDs, runID)
	}
}

func teamTerminalAssociationMatches(
	expected teamExpectedRun,
	entry loomruntime.TerminalEntryV3,
) bool {
	return entry.RunID == expected.RunID &&
		entry.Tenant == expected.WorkspaceID &&
		(expected.Agent == "" || entry.Agent == expected.Agent) &&
		entry.AttributionScope == expected.AttributionScope &&
		sameTeamString(entry.TeamID, expected.TeamID) &&
		sameTeamString(entry.WorkflowID, expected.WorkflowID) &&
		sameTeamInt(entry.WorkflowVersion, expected.WorkflowVersion) &&
		sameTeamString(entry.RunSnapshotID, expected.RunSnapshotID) &&
		sameTeamString(entry.TaskGroupID, expected.TaskGroupID)
}

func teamTerminalLineageMatches(
	expected teamExpectedRun,
	entry loomruntime.TerminalEntryV3,
) bool {
	return sameTeamString(entry.ParentRunID, expected.ParentRunID) &&
		sameTeamInt64(entry.ParentSeq, expected.ParentSeq) &&
		sameTeamString(entry.AggregationParentRunID, expected.AggregationParentRunID)
}

func sameTeamExpectedRun(left, right teamExpectedRun) bool {
	return left.WorkspaceID == right.WorkspaceID &&
		left.RunID == right.RunID &&
		left.Agent == right.Agent &&
		left.AttributionScope == right.AttributionScope &&
		sameTeamString(left.TeamID, right.TeamID) &&
		sameTeamString(left.WorkflowID, right.WorkflowID) &&
		sameTeamInt(left.WorkflowVersion, right.WorkflowVersion) &&
		sameTeamString(left.RunSnapshotID, right.RunSnapshotID) &&
		sameTeamString(left.ParentRunID, right.ParentRunID) &&
		sameTeamInt64(left.ParentSeq, right.ParentSeq) &&
		sameTeamString(left.AggregationParentRunID, right.AggregationParentRunID) &&
		sameTeamString(left.TaskGroupID, right.TaskGroupID) &&
		left.Registered == right.Registered
}

func sameTeamDomain(left, right teamExpectedRun) bool {
	return left.WorkspaceID == right.WorkspaceID &&
		sameTeamString(left.TeamID, right.TeamID) &&
		sameTeamString(left.WorkflowID, right.WorkflowID) &&
		sameTeamInt(left.WorkflowVersion, right.WorkflowVersion) &&
		sameTeamString(left.RunSnapshotID, right.RunSnapshotID)
}

func sameTeamString(left, right *string) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}

func sameTeamInt(left, right *int) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}

func sameTeamInt64(left, right *int64) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}

func teamStringPointer(value string) *string {
	return &value
}

func teamIntPointer(value int) *int {
	return &value
}

func cloneTeamString(value *string) *string {
	if value == nil {
		return nil
	}
	return teamStringPointer(*value)
}

func cloneTeamInt(value *int) *int {
	if value == nil {
		return nil
	}
	return teamIntPointer(*value)
}

func cloneTeamInt64(value *int64) *int64 {
	if value == nil {
		return nil
	}
	cloned := *value
	return &cloned
}
