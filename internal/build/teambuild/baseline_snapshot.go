package teambuild

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	org "github.com/jinyitao123/weave/internal/kernel/orgspec"
	"github.com/jinyitao123/weave/internal/kernel/workflow"
)

// SetBaselineSources binds the exact-read stores the server-side optimize
// baseline builder needs. Production wires OrgStore after NewServer, so the
// API authorize handler binds the sources before each authorize; the binding
// is idempotent and guarded for concurrent access. Create-mode authorize never
// consults the sources.
func (s *Store) SetBaselineSources(
	orgStore OrganizationBaselineReader,
	agents AgentBaselineReader,
	workflows WorkflowBaselineReader,
	artifacts workflow.PublicationReader,
) {
	s.baselineMu.Lock()
	defer s.baselineMu.Unlock()
	s.orgStore = orgStore
	s.agents = agents
	s.workflows = workflows
	s.artifacts = artifacts
}

// PreviewCompilerBaseline captures the server-owned planning baseline. It
// accepts only the build-run identity, so callers cannot inject a snapshot or
// timestamp. Immutable workflow facts are frozen first; the run and mutable
// product facts are then locked and compared before the snapshot is accepted.
func (s *Store) PreviewCompilerBaseline(
	ctx context.Context,
	workspaceID, buildRunID string,
) (CompilerBaselinePreview, error) {
	if strings.TrimSpace(workspaceID) == "" || strings.TrimSpace(buildRunID) == "" {
		return CompilerBaselinePreview{}, fmt.Errorf("preview compiler baseline: %w", ErrBuildRunNotFound)
	}
	initial, err := s.GetBuildRun(ctx, workspaceID, buildRunID)
	if err != nil {
		return CompilerBaselinePreview{}, fmt.Errorf("preview compiler baseline: %w", err)
	}
	if initial.Status != StatusPlanning {
		return CompilerBaselinePreview{}, fmt.Errorf("preview compiler baseline: %w", ErrBuildRunNotPlanning)
	}
	if initial.Mode == ModeCreate {
		return CompilerBaselinePreview{BaselineHash: emptyCreateBaselineHashV1}, nil
	}
	if initial.Mode != ModeOptimize {
		return CompilerBaselinePreview{}, fmt.Errorf("preview compiler baseline: invalid mode %q", initial.Mode)
	}
	capturedAt := s.clock.Now().UTC().Round(time.Microsecond)
	prepared, err := s.freezeBaselineWorkflows(ctx, workspaceID, initial.Brief)
	if err != nil {
		return CompilerBaselinePreview{}, fmt.Errorf("preview compiler baseline: %w", err)
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return CompilerBaselinePreview{}, fmt.Errorf("begin preview compiler baseline: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var mode, status, briefHash string
	err = tx.QueryRow(ctx, `
		SELECT mode, status, brief_hash
		FROM weave_team_build_runs
		WHERE workspace_id=$1 AND build_run_id=$2
		FOR UPDATE
	`, workspaceID, buildRunID).Scan(&mode, &status, &briefHash)
	if errors.Is(err, pgx.ErrNoRows) {
		return CompilerBaselinePreview{}, fmt.Errorf("preview compiler baseline: %w", ErrBuildRunNotFound)
	}
	if err != nil {
		return CompilerBaselinePreview{}, fmt.Errorf("preview compiler baseline: %w", err)
	}
	if status != StatusPlanning {
		return CompilerBaselinePreview{}, fmt.Errorf("preview compiler baseline: %w", ErrBuildRunNotPlanning)
	}
	if mode != initial.Mode || briefHash != initial.BriefHash {
		return CompilerBaselinePreview{}, fmt.Errorf("preview compiler baseline: %w", ErrBuildRunNotPlanning)
	}
	// PostgreSQL timestamptz persists microsecond precision. Bind the preview
	// snapshot to that precision before hashing so authorization can recapture
	// the same instant after the timestamp has made a database round trip.
	snapshot, err := s.captureBaselineTxAt(ctx, tx, workspaceID, initial.Brief, capturedAt, prepared)
	if err != nil {
		return CompilerBaselinePreview{}, fmt.Errorf("preview compiler baseline: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return CompilerBaselinePreview{}, fmt.Errorf("commit preview compiler baseline: %w", err)
	}
	snapshotCopy := snapshot
	capturedAtCopy := capturedAt
	return CompilerBaselinePreview{
		BaselineHash: snapshot.ContentHash,
		Snapshot:     &snapshotCopy,
		CapturedAt:   &capturedAtCopy,
	}, nil
}

// VerifyEvaluationBaselineTx recaptures the complete authorization baseline
// under the caller's publication transaction and locks. The returned hash is
// the proof token accepted by MarkPublishedTx; ordinary runs return an empty
// token. Publication must call this before inserting publication facts.
func (s *Store) VerifyEvaluationBaselineTx(
	ctx context.Context,
	tx pgx.Tx,
	workspaceID, buildRunID string,
) (string, error) {
	if tx == nil {
		return "", errors.New("verify evaluation baseline: transaction is required")
	}
	var status string
	var evaluationOnly bool
	var briefRaw, baselineRaw []byte
	err := tx.QueryRow(ctx, `
		SELECT status, evaluation_only, brief_json, baseline_snapshot_json
		FROM weave_team_build_runs
		WHERE workspace_id=$1 AND build_run_id=$2
		FOR UPDATE
	`, workspaceID, buildRunID).Scan(&status, &evaluationOnly, &briefRaw, &baselineRaw)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", fmt.Errorf("verify evaluation baseline: %w", ErrBuildRunNotFound)
	}
	if err != nil {
		return "", fmt.Errorf("verify evaluation baseline: %w", err)
	}
	if !evaluationOnly {
		return "", nil
	}
	if status != StatusPublishing {
		return "", fmt.Errorf("verify evaluation baseline: %w: run is not publishing", ErrEvaluationPublishCAS)
	}
	var brief BuildBrief
	if err := json.Unmarshal(briefRaw, &brief); err != nil {
		return "", fmt.Errorf("verify evaluation baseline: decode brief: %w", err)
	}
	var frozen BaselineSnapshot
	if err := json.Unmarshal(baselineRaw, &frozen); err != nil {
		return "", fmt.Errorf("verify evaluation baseline: decode frozen baseline: %w", err)
	}
	capturedAt, err := time.Parse(time.RFC3339Nano, frozen.CapturedAt)
	if err != nil {
		return "", fmt.Errorf("verify evaluation baseline: decode captured_at: %w", err)
	}
	workflowIDs := make([]string, 0, len(frozen.WorkflowIdentities))
	for _, identity := range frozen.WorkflowIdentities {
		workflowIDs = append(workflowIDs, identity.ID)
	}
	prepared := frozenBaselineWorkflowFacts{workflowIDs: workflowIDs, identities: frozen.WorkflowIdentities, versions: frozen.WorkflowVersions, refs: frozen.Workflows}
	current, err := s.captureBaselineTxAt(ctx, tx, workspaceID, brief, capturedAt, prepared)
	if err != nil {
		if errors.Is(err, workflow.ErrVersionConflict) {
			return "", fmt.Errorf("verify evaluation baseline: %w: workflow catalog changed", ErrEvaluationBaselineChanged)
		}
		return "", fmt.Errorf("verify evaluation baseline: %w", err)
	}
	if current.ContentHash != frozen.ContentHash {
		return "", fmt.Errorf("verify evaluation baseline: %w: frozen=%s current=%s",
			ErrEvaluationBaselineChanged, frozen.ContentHash, current.ContentHash)
	}
	return frozen.ContentHash, nil
}

// captureBaselineTxAt verifies the pre-read immutable workflow facts against
// locked product revisions, then reads the live Team, Roster and Agents in the
// caller-owned transaction and builds the complete optimize snapshot.
func (s *Store) captureBaselineTxAt(
	ctx context.Context,
	tx pgx.Tx,
	workspaceID string,
	brief BuildBrief,
	capturedAt time.Time,
	prepared frozenBaselineWorkflowFacts,
) (BaselineSnapshot, error) {
	if !validUTCTimestamp(capturedAt) {
		return BaselineSnapshot{}, fmt.Errorf("capture baseline: %w: captured_at must be a valid UTC timestamp", ErrBaselineSnapshotInvalid)
	}
	orgStore, agents, workflows, _ := s.baselineSources()
	if !baselineReaderAvailable(orgStore) || !baselineReaderAvailable(agents) || workflows == nil {
		return BaselineSnapshot{}, fmt.Errorf("%w", ErrBaselineSourceUnavailable)
	}
	if tx == nil {
		return BaselineSnapshot{}, fmt.Errorf(
			"capture baseline: %w", ErrBaselineSnapshotFailed,
		)
	}
	scope := cloneAssetScope(brief.AllowedAssets)

	if err := workflows.VerifyBaselineWorkflowsTx(ctx, tx, workspaceID, prepared.workflowIDs, prepared.identities, prepared.versions); err != nil {
		return BaselineSnapshot{}, fmt.Errorf("capture baseline: verify workflow facts: %w", errors.Join(ErrBaselineSnapshotFailed, err))
	}
	workflowReads := append([]BaselineWorkflowRef(nil), prepared.refs...)

	// Lock workspace + team + roster rows in the same order as roster writers
	// (workspace -> team), then read the authoritative team and dispatch rules.
	workers, err := agents.ResolveTeamWorkersForShareTx(
		ctx, tx, workspaceID, brief.TeamID,
	)
	if err != nil {
		return BaselineSnapshot{}, fmt.Errorf(
			"capture baseline: %w: resolve team roster: %v",
			ErrBaselineSnapshotFailed,
			err,
		)
	}
	team, err := orgStore.GetTeamTx(ctx, tx, workspaceID, brief.TeamID)
	if err != nil {
		if errors.Is(err, org.ErrTeamNotFound) {
			return BaselineSnapshot{}, fmt.Errorf(
				"capture baseline: %w: %v", ErrBaselineTargetNotFound, err,
			)
		}
		return BaselineSnapshot{}, fmt.Errorf(
			"capture baseline: %w: %v", ErrBaselineSnapshotFailed, err,
		)
	}
	rules, err := orgStore.GetTeamDispatchRulesTx(ctx, tx, workspaceID, brief.TeamID)
	if err != nil {
		return BaselineSnapshot{}, fmt.Errorf(
			"capture baseline: %w: %v", ErrBaselineSnapshotFailed, err,
		)
	}
	if team.WorkspaceID != workspaceID || team.ID != brief.TeamID {
		return BaselineSnapshot{}, fmt.Errorf(
			"capture baseline: %w: cross-workspace target team",
			ErrBaselineSnapshotInvalid,
		)
	}

	// Assemble the roster (lead + every team-worker relation, including
	// disabled) and the exact agent version pins.
	if strings.TrimSpace(team.LeadAvatarID) == "" {
		return BaselineSnapshot{}, fmt.Errorf(
			"capture baseline: %w: team %q has no lead avatar",
			ErrBaselineSnapshotInvalid,
			team.ID,
		)
	}
	roster := []BaselineRosterEntry{{
		AgentID: team.LeadAvatarID,
		Role:    "lead",
		Enabled: true,
	}}
	agentIDs := []string{team.LeadAvatarID}
	for _, worker := range workers {
		if worker.WorkspaceID != workspaceID || worker.TeamID != team.ID {
			return BaselineSnapshot{}, fmt.Errorf(
				"capture baseline: %w: roster row of agent %q is not scoped to the target team",
				ErrBaselineSnapshotInvalid,
				worker.WorkerAgentID,
			)
		}
		if worker.WorkerAgentID == team.LeadAvatarID {
			return BaselineSnapshot{}, fmt.Errorf(
				"capture baseline: %w: lead %q also appears as a roster worker",
				ErrBaselineSnapshotInvalid,
				team.LeadAvatarID,
			)
		}
		roster = append(roster, BaselineRosterEntry{
			AgentID:            worker.WorkerAgentID,
			Role:               "worker",
			Duty:               worker.Duty,
			WhenToUse:          worker.WhenToUse,
			ContextInstruction: worker.ContextInstruction,
			AllowedKinds:       append([]string(nil), worker.AllowedKinds...),
			DefaultKind:        worker.DefaultKind,
			ResultRequirement:  worker.ResultRequirement,
			Enabled:            worker.Enabled,
		})
		agentIDs = append(agentIDs, worker.WorkerAgentID)
	}
	sort.Strings(agentIDs)
	heads, err := agents.ResolveAgentHeadVersionsTx(ctx, tx, workspaceID, agentIDs)
	if err != nil {
		return BaselineSnapshot{}, fmt.Errorf(
			"capture baseline: %w: %v", ErrBaselineSnapshotFailed, err,
		)
	}
	pins := make([]BaselineAgentPin, 0, len(agentIDs))
	for _, agentID := range agentIDs {
		version := heads[agentID]
		content, err := agents.ResolveAgentVersionContentTx(
			ctx, tx, workspaceID, agentID, version,
		)
		if err != nil {
			return BaselineSnapshot{}, fmt.Errorf(
				"capture baseline: %w: pin agent %q@%d: %v",
				ErrBaselineSnapshotFailed,
				agentID,
				version,
				err,
			)
		}
		pins = append(pins, BaselineAgentPin{
			AgentID:     content.AgentID,
			Name:        content.Name,
			Version:     content.Version,
			ContentHash: content.ContentHash,
			Engine:      content.Engine,
			RuntimeID:   content.RuntimeID,
			Model:       content.Model,
		})
	}

	snapshot := BaselineSnapshot{
		SchemaVersion: schemaVersion,
		WorkspaceID:   workspaceID,
		Team: BaselineTeamRef{
			WorkspaceID:     team.WorkspaceID,
			TeamID:          team.ID,
			Name:            team.Name,
			Objective:       team.Objective,
			PrimaryScenario: team.PrimaryScenario,
			SuccessCriteria: team.SuccessCriteria,
			LeadAvatarID:    team.LeadAvatarID,
			Status:          team.Status,
			UpdatedAt:       formatTimestamp(team.UpdatedAt),
		},
		DispatchRules: BaselineDispatchRules{
			LegTimeoutSec:    rules.LegTimeoutSec,
			GroupDeadlineSec: rules.GroupDeadlineSec,
			Quorum:           rules.Quorum,
		},
		Roster:             roster,
		AgentPins:          pins,
		Workflows:          workflowReads,
		WorkflowIdentities: append([]workflow.TeamWorkflow(nil), prepared.identities...),
		WorkflowVersions:   append([]workflow.TeamWorkflowVersion(nil), prepared.versions...),
		AssetScope:         scope,
		CapturedAt:         capturedAt.UTC().Format(time.RFC3339Nano),
	}
	sort.Slice(snapshot.Roster, func(i, j int) bool {
		return snapshot.Roster[i].AgentID < snapshot.Roster[j].AgentID
	})
	sort.Slice(snapshot.AgentPins, func(i, j int) bool {
		return snapshot.AgentPins[i].AgentID < snapshot.AgentPins[j].AgentID
	})
	sort.Slice(snapshot.Workflows, func(i, j int) bool {
		return snapshot.Workflows[i].WorkflowID < snapshot.Workflows[j].WorkflowID
	})

	if err := validateBaselineSnapshot(snapshot); err != nil {
		return BaselineSnapshot{}, fmt.Errorf(
			"capture baseline: %w: %v", ErrBaselineSnapshotInvalid, err,
		)
	}
	if err := validateBaselineClosure(snapshot, brief, workspaceID); err != nil {
		return BaselineSnapshot{}, err
	}
	contentHash, err := snapshot.Hash()
	if err != nil {
		return BaselineSnapshot{}, fmt.Errorf(
			"capture baseline: %w: %v", ErrBaselineSnapshotInvalid, err,
		)
	}
	snapshot.ContentHash = contentHash
	if err := validateBaselineSnapshot(snapshot); err != nil {
		return BaselineSnapshot{}, fmt.Errorf(
			"capture baseline: %w: %v", ErrBaselineSnapshotInvalid, err,
		)
	}
	return snapshot, nil
}

func (s *Store) baselineSources() (
	orgStore OrganizationBaselineReader,
	agents AgentBaselineReader,
	workflows WorkflowBaselineReader,
	artifacts workflow.PublicationReader,
) {
	s.baselineMu.RLock()
	defer s.baselineMu.RUnlock()
	return s.orgStore, s.agents, s.workflows, s.artifacts
}

type frozenBaselineWorkflowFacts struct {
	workflowIDs []string
	identities  []workflow.TeamWorkflow
	versions    []workflow.TeamWorkflowVersion
	refs        []BaselineWorkflowRef
}

// freezeBaselineWorkflows reads immutable artifacts and the product catalog
// before the product transaction starts. The transaction later verifies the
// exact product identities and complete version set before using these facts.
func (s *Store) freezeBaselineWorkflows(
	ctx context.Context, workspaceID string, brief BuildBrief,
) (frozenBaselineWorkflowFacts, error) {
	_, _, workflows, artifacts := s.baselineSources()
	if workflows == nil || artifacts == nil {
		return frozenBaselineWorkflowFacts{}, ErrBaselineSourceUnavailable
	}
	teamID, scope := brief.TeamID, cloneAssetScope(brief.AllowedAssets)
	workflowIDs := make([]string, 0)
	for _, ref := range scope.Refs {
		if ref.Kind == "workflow" && ref.ID != "" {
			workflowIDs = append(workflowIDs, ref.ID)
		}
	}
	sort.Strings(workflowIDs)
	workflowIDs = compactStrings(workflowIDs)

	versions, err := workflows.ListVersionsByWorkflows(ctx, workspaceID, workflowIDs)
	if err != nil {
		return frozenBaselineWorkflowFacts{}, fmt.Errorf(
			"capture baseline: %w: list workflow versions: %v",
			ErrBaselineSnapshotFailed,
			err,
		)
	}

	facts := frozenBaselineWorkflowFacts{workflowIDs: make([]string, 0, len(workflowIDs)), identities: make([]workflow.TeamWorkflow, 0, len(workflowIDs)), versions: versions, refs: make([]BaselineWorkflowRef, 0, len(workflowIDs))}
	for _, workflowID := range workflowIDs {
		var draft *workflow.TeamWorkflowVersion
		draftVersion := findWorkflowDraftVersion(versions, workflowID)
		if draftVersion != nil {
			draft = findWorkflowVersion(versions, workflowID, *draftVersion)
		}
		workflowRow, err := workflows.Get(ctx, workspaceID, workflowID)
		if err != nil {
			if errors.Is(err, workflow.ErrNotFound) && workflowID == FirstOptimizeWorkflowID(teamID) {
				facts.versions = removeWorkflowVersions(facts.versions, workflowID)
				continue
			}
			return frozenBaselineWorkflowFacts{}, fmt.Errorf("capture baseline: %w: read workflow %q: %v", ErrBaselineSnapshotFailed, workflowID, err)
		}
		identity := *workflowRow
		if identity.WorkspaceID != workspaceID || identity.ID != workflowID {
			return frozenBaselineWorkflowFacts{}, fmt.Errorf(
				"capture baseline: %w: workflow %q is not scoped to workspace %q",
				ErrBaselineSnapshotInvalid,
				workflowID,
				workspaceID,
			)
		}
		facts.workflowIDs = append(facts.workflowIDs, workflowID)
		facts.identities = append(facts.identities, identity)
		ref := BaselineWorkflowRef{
			WorkflowID: identity.ID,
			TeamID:     identity.TeamID,
			Name:       identity.Name,
			Status:     identity.Status,
			UpdatedAt:  formatTimestamp(identity.UpdatedAt),
		}
		if identity.PublishedVersion != nil {
			publishedVersion := *identity.PublishedVersion
			versionRow := findWorkflowVersion(versions, workflowID, publishedVersion)
			if versionRow == nil {
				return frozenBaselineWorkflowFacts{}, fmt.Errorf(
					"capture baseline: %w: read workflow %q published version: %v",
					ErrBaselineSnapshotFailed,
					workflowID,
					workflow.ErrNotFound,
				)
			}
			artifact, err := artifacts.GetArtifact(
				ctx, workspaceID, workflowID, publishedVersion,
			)
			if err != nil {
				return frozenBaselineWorkflowFacts{}, fmt.Errorf(
					"capture baseline: %w: read workflow %q published artifact: %v",
					ErrBaselineSnapshotFailed,
					workflowID,
					err,
				)
			}
			dependencies, err := artifacts.ListDependencies(
				ctx, workspaceID, workflowID, publishedVersion,
			)
			if err != nil {
				return frozenBaselineWorkflowFacts{}, fmt.Errorf(
					"capture baseline: %w: read workflow %q dependencies: %v",
					ErrBaselineSnapshotFailed,
					workflowID,
					err,
				)
			}
			versionHash, err := hashWorkflowContent(
				versionRow.TriggerConfig, versionRow.GraphDefinition,
			)
			if err != nil {
				return frozenBaselineWorkflowFacts{}, fmt.Errorf(
					"capture baseline: %w: hash workflow %q published content: %v",
					ErrBaselineSnapshotInvalid,
					workflowID,
					err,
				)
			}
			published := &BaselineWorkflowPublished{
				Version:     publishedVersion,
				Trigger:     versionRow.TriggerConfig,
				Graph:       versionRow.GraphDefinition,
				ContentHash: versionHash,
				Artifact: BaselineWorkflowArtifact{
					ArtifactSchemaVersion:     artifact.ArtifactSchemaVersion,
					CanonicalizationAlgorithm: artifact.CanonicalizationAlgorithm,
					CanonicalizationVersion:   artifact.CanonicalizationVersion,
					HashAlgorithm:             artifact.HashAlgorithm,
					ContentHash:               artifact.ContentHash,
					Payload:                   artifact.Payload,
				},
				Dependencies: make([]BaselineWorkflowDependency, 0, len(dependencies)),
			}
			for _, dependency := range dependencies {
				published.Dependencies = append(published.Dependencies,
					BaselineWorkflowDependency{
						OwnerType:         dependency.OwnerType,
						OwnerID:           dependency.OwnerID,
						OwnerAgentVersion: dependency.OwnerAgentVersion,
						DependencyType:    dependency.DependencyType,
						DependencyKey:     dependency.DependencyKey,
						DependencyVersion: dependency.DependencyVersion,
						ContentHash:       dependency.ContentHash,
					},
				)
			}
			ref.Published = published
		}
		if draft != nil {
			draftHash, err := hashWorkflowContent(
				draft.TriggerConfig, draft.GraphDefinition,
			)
			if err != nil {
				return frozenBaselineWorkflowFacts{}, fmt.Errorf(
					"capture baseline: %w: hash workflow %q draft content: %v",
					ErrBaselineSnapshotInvalid,
					workflowID,
					err,
				)
			}
			ref.Draft = &BaselineWorkflowDraft{
				Version:     draft.Version,
				Trigger:     draft.TriggerConfig,
				Graph:       draft.GraphDefinition,
				UpdatedAt:   formatTimestamp(draft.UpdatedAt),
				ContentHash: draftHash,
			}
		}
		facts.refs = append(facts.refs, ref)
	}
	return facts, nil
}

func findWorkflowVersion(versions []workflow.TeamWorkflowVersion, workflowID string, version int) *workflow.TeamWorkflowVersion {
	for index := range versions {
		if versions[index].WorkflowID == workflowID && versions[index].Version == version {
			copy := versions[index]
			return &copy
		}
	}
	return nil
}

func removeWorkflowVersions(versions []workflow.TeamWorkflowVersion, workflowID string) []workflow.TeamWorkflowVersion {
	filtered := versions[:0]
	for _, version := range versions {
		if version.WorkflowID != workflowID {
			filtered = append(filtered, version)
		}
	}
	return filtered
}

func findWorkflowDraftVersion(
	versions []workflow.TeamWorkflowVersion,
	workflowID string,
) *int {
	for index := range versions {
		version := versions[index]
		if version.WorkflowID == workflowID &&
			version.Status == workflow.VersionStatusDraft {
			candidate := version.Version
			return &candidate
		}
	}
	return nil
}

func compactStrings(values []string) []string {
	if len(values) < 2 {
		return values
	}
	compact := values[:1]
	for _, value := range values[1:] {
		if value != compact[len(compact)-1] {
			compact = append(compact, value)
		}
	}
	return compact
}

func hashWorkflowContent(trigger, graph json.RawMessage) (string, error) {
	return hashDocument(struct {
		Trigger json.RawMessage `json:"trigger"`
		Graph   json.RawMessage `json:"graph"`
	}{trigger, graph})
}

func formatTimestamp(value time.Time) string {
	return value.UTC().Format(time.RFC3339Nano)
}
