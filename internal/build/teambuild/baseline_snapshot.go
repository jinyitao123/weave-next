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
	"github.com/jinyitao123/weave/internal/kernel/registry"
	"github.com/jinyitao123/weave/internal/kernel/workflow"
)

// SetBaselineSources binds the exact-read stores the server-side optimize
// baseline builder needs. Production wires OrgStore after NewServer, so the
// API authorize handler binds the sources before each authorize; the binding
// is idempotent and guarded for concurrent access. Create-mode authorize never
// consults the sources.
func (s *Store) SetBaselineSources(
	orgStore OrganizationBaselineReader,
	agents *registry.AgentRegistry,
	workflows *workflow.Store,
) {
	s.baselineMu.Lock()
	defer s.baselineMu.Unlock()
	s.orgStore = orgStore
	s.agents = agents
	s.workflows = workflows
}

// PreviewCompilerBaseline captures the server-owned planning baseline. It
// accepts only the build-run identity, so callers cannot inject a snapshot or
// timestamp. The run lock serializes this preview with authorization while
// live asset locks make the optimize snapshot internally consistent.
func (s *Store) PreviewCompilerBaseline(
	ctx context.Context,
	workspaceID, buildRunID string,
) (CompilerBaselinePreview, error) {
	if strings.TrimSpace(workspaceID) == "" || strings.TrimSpace(buildRunID) == "" {
		return CompilerBaselinePreview{}, fmt.Errorf("preview compiler baseline: %w", ErrBuildRunNotFound)
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return CompilerBaselinePreview{}, fmt.Errorf("begin preview compiler baseline: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var mode, status string
	var briefRaw []byte
	err = tx.QueryRow(ctx, `
		SELECT mode, status, brief_json
		FROM weave_team_build_runs
		WHERE workspace_id=$1 AND build_run_id=$2
		FOR UPDATE
	`, workspaceID, buildRunID).Scan(&mode, &status, &briefRaw)
	if errors.Is(err, pgx.ErrNoRows) {
		return CompilerBaselinePreview{}, fmt.Errorf("preview compiler baseline: %w", ErrBuildRunNotFound)
	}
	if err != nil {
		return CompilerBaselinePreview{}, fmt.Errorf("preview compiler baseline: %w", err)
	}
	if status != StatusPlanning {
		return CompilerBaselinePreview{}, fmt.Errorf("preview compiler baseline: %w", ErrBuildRunNotPlanning)
	}
	if mode == ModeCreate {
		if err := tx.Commit(ctx); err != nil {
			return CompilerBaselinePreview{}, fmt.Errorf("commit preview compiler baseline: %w", err)
		}
		return CompilerBaselinePreview{BaselineHash: emptyCreateBaselineHashV1}, nil
	}
	if mode != ModeOptimize {
		return CompilerBaselinePreview{}, fmt.Errorf("preview compiler baseline: invalid mode %q", mode)
	}
	var brief BuildBrief
	if err := json.Unmarshal(briefRaw, &brief); err != nil {
		return CompilerBaselinePreview{}, fmt.Errorf("preview compiler baseline: decode stored brief: %w", err)
	}
	// PostgreSQL timestamptz persists microsecond precision. Bind the preview
	// snapshot to that precision before hashing so authorization can recapture
	// the same instant after the timestamp has made a database round trip.
	capturedAt := s.clock.Now().UTC().Round(time.Microsecond)
	snapshot, err := s.captureBaselineTxAt(ctx, tx, workspaceID, brief, capturedAt)
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
	var frozen BaselineSnapshot
	if err := json.Unmarshal(briefRaw, &brief); err != nil {
		return "", fmt.Errorf("verify evaluation baseline: decode brief: %w", err)
	}
	if err := json.Unmarshal(baselineRaw, &frozen); err != nil {
		return "", fmt.Errorf("verify evaluation baseline: decode frozen baseline: %w", err)
	}
	capturedAt, err := time.Parse(time.RFC3339Nano, frozen.CapturedAt)
	if err != nil {
		return "", fmt.Errorf("verify evaluation baseline: decode captured_at: %w", err)
	}
	current, err := s.captureBaselineTxAt(ctx, tx, workspaceID, brief, capturedAt)
	if err != nil {
		return "", fmt.Errorf("verify evaluation baseline: %w", err)
	}
	if current.ContentHash != frozen.ContentHash {
		return "", fmt.Errorf("verify evaluation baseline: %w: frozen=%s current=%s",
			ErrEvaluationBaselineChanged, frozen.ContentHash, current.ContentHash)
	}
	return frozen.ContentHash, nil
}

// captureBaselineTx reads the live target Team, Roster, referenced Agents,
// and Workflows inside the caller-owned authorize transaction, then builds,
// hashes, validates, and closure-checks the full optimize baseline. The
// caller owns the transaction and must commit or roll back afterwards; any
// error leaves the run in planning.
func (s *Store) captureBaselineTx(
	ctx context.Context,
	tx pgx.Tx,
	workspaceID string,
	brief BuildBrief,
) (BaselineSnapshot, error) {
	return s.captureBaselineTxAt(ctx, tx, workspaceID, brief, s.clock.Now().UTC())
}

func (s *Store) captureBaselineTxAt(
	ctx context.Context,
	tx pgx.Tx,
	workspaceID string,
	brief BuildBrief,
	capturedAt time.Time,
) (BaselineSnapshot, error) {
	if !validUTCTimestamp(capturedAt) {
		return BaselineSnapshot{}, fmt.Errorf("capture baseline: %w: captured_at must be a valid UTC timestamp", ErrBaselineSnapshotInvalid)
	}
	orgStore, agents, workflows := s.baselineSources()
	if !organizationBaselineReaderAvailable(orgStore) || agents == nil || workflows == nil {
		return BaselineSnapshot{}, fmt.Errorf("%w", ErrBaselineSourceUnavailable)
	}
	if tx == nil {
		return BaselineSnapshot{}, fmt.Errorf(
			"capture baseline: %w", ErrBaselineSnapshotFailed,
		)
	}
	scope := cloneAssetScope(brief.AllowedAssets)

	// Fast-fail existence check without locking: the authoritative locked
	// team read below is the one captured by the snapshot.
	if _, err := orgStore.GetTeam(ctx, workspaceID, brief.TeamID); err != nil {
		if errors.Is(err, org.ErrTeamNotFound) {
			return BaselineSnapshot{}, fmt.Errorf(
				"capture baseline: %w: %v", ErrBaselineTargetNotFound, err,
			)
		}
		return BaselineSnapshot{}, fmt.Errorf(
			"capture baseline: %w: %v", ErrBaselineSnapshotFailed, err,
		)
	}

	// Lock and read the scoped workflows before the team/roster rows so the
	// lock order matches the publication candidate builder (workspace ->
	// workflow -> team) and cannot deadlock against a concurrent publish.
	workflowReads, err := s.captureBaselineWorkflowsTx(
		ctx, tx, workflows, workspaceID, brief.TeamID, scope,
	)
	if err != nil {
		return BaselineSnapshot{}, err
	}

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
		Roster:     roster,
		AgentPins:  pins,
		Workflows:  workflowReads,
		AssetScope: scope,
		CapturedAt: capturedAt.UTC().Format(time.RFC3339Nano),
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
	agents *registry.AgentRegistry,
	workflows *workflow.Store,
) {
	s.baselineMu.RLock()
	defer s.baselineMu.RUnlock()
	return s.orgStore, s.agents, s.workflows
}

// captureBaselineWorkflowsTx locks and reads every scoped workflow's identity,
// published facts, and mutable draft inside the caller-owned transaction. The
// published artifact/dependency/version rows are immutable once written, so
// after the workflow row is locked (through the draft lock when one exists)
// the exact published reads are stable. A workflow without a draft has no
// publish path, so its published identity can only change through archive; the
// snapshot therefore reflects the state at read time and any later mutation
// fails the downstream candidate admission instead of being silently frozen.
func (s *Store) captureBaselineWorkflowsTx(
	ctx context.Context,
	tx pgx.Tx,
	workflows *workflow.Store,
	workspaceID, teamID string,
	scope AssetScope,
) ([]BaselineWorkflowRef, error) {
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
		return nil, fmt.Errorf(
			"capture baseline: %w: list workflow versions: %v",
			ErrBaselineSnapshotFailed,
			err,
		)
	}

	refs := make([]BaselineWorkflowRef, 0, len(workflowIDs))
	for _, workflowID := range workflowIDs {
		var (
			identity workflow.TeamWorkflow
			draft    *workflow.TeamWorkflowVersion
		)
		draftVersion := findWorkflowDraftVersion(versions, workflowID)
		if draftVersion != nil {
			read, err := workflows.ResolvePublicationDraftTx(
				ctx, tx, workspaceID, workflowID, *draftVersion,
			)
			if err != nil {
				return nil, fmt.Errorf(
					"capture baseline: %w: lock workflow %q draft: %v",
					ErrBaselineSnapshotFailed,
					workflowID,
					err,
				)
			}
			identity = read.Workflow
			draft = &read.Draft
		} else {
			workflowRow, err := workflows.Get(ctx, workspaceID, workflowID)
			if err != nil {
				if errors.Is(err, workflow.ErrNotFound) && workflowID == FirstOptimizeWorkflowID(teamID) {
					continue
				}
				return nil, fmt.Errorf(
					"capture baseline: %w: read workflow %q: %v",
					ErrBaselineSnapshotFailed,
					workflowID,
					err,
				)
			}
			identity = *workflowRow
		}
		if identity.WorkspaceID != workspaceID || identity.ID != workflowID {
			return nil, fmt.Errorf(
				"capture baseline: %w: workflow %q is not scoped to workspace %q",
				ErrBaselineSnapshotInvalid,
				workflowID,
				workspaceID,
			)
		}
		ref := BaselineWorkflowRef{
			WorkflowID: identity.ID,
			TeamID:     identity.TeamID,
			Name:       identity.Name,
			Status:     identity.Status,
			UpdatedAt:  formatTimestamp(identity.UpdatedAt),
		}
		if identity.PublishedVersion != nil {
			publishedVersion := *identity.PublishedVersion
			versionRow, err := workflows.GetVersion(
				ctx, workspaceID, workflowID, publishedVersion,
			)
			if err != nil {
				return nil, fmt.Errorf(
					"capture baseline: %w: read workflow %q published version: %v",
					ErrBaselineSnapshotFailed,
					workflowID,
					err,
				)
			}
			artifact, err := workflows.GetArtifact(
				ctx, workspaceID, workflowID, publishedVersion,
			)
			if err != nil {
				return nil, fmt.Errorf(
					"capture baseline: %w: read workflow %q published artifact: %v",
					ErrBaselineSnapshotFailed,
					workflowID,
					err,
				)
			}
			dependencies, err := workflows.ListDependencies(
				ctx, workspaceID, workflowID, publishedVersion,
			)
			if err != nil {
				return nil, fmt.Errorf(
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
				return nil, fmt.Errorf(
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
				return nil, fmt.Errorf(
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
		refs = append(refs, ref)
	}
	return refs, nil
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
