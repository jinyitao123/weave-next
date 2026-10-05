package businessaction

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	"github.com/jackc/pgx/v5"
	"github.com/jinyitao123/loom/contract"
	"github.com/jinyitao123/weave/internal/base/execution"
	"github.com/jinyitao123/weave/internal/base/frozen"
	"github.com/jinyitao123/weave/internal/kernel/mcphost"
	"github.com/jinyitao123/weave/internal/kernel/publication"
	"github.com/jinyitao123/weave/internal/kernel/workflow/machine"
)

// DevelopmentAction is a frozen, credential-free action definition. The
// explicit simulation flag only grants access to the isolated trial dispatcher.
type DevelopmentAction struct {
	CapabilityID         string        `json:"capability_id"`
	Name                 string        `json:"name"`
	ObjectName           string        `json:"object_name"`
	Label                string        `json:"label,omitempty"`
	Description          string        `json:"description,omitempty"`
	RequiresRecord       bool          `json:"requires_record,omitempty"`
	RequiresConfirmation bool          `json:"requires_confirmation,omitempty"`
	SimulationAuthorized bool          `json:"simulation_authorized,omitempty"`
	Params               []actionParam `json:"params,omitempty"`
}

// ValidateDevelopmentActions verifies that a trial freezes exactly the Forge
// actions referenced by its candidate graph. Extra definitions are rejected.
func ValidateDevelopmentActions(requested []string, supplied []DevelopmentAction) ([]DevelopmentAction, error) {
	want := make(map[string]struct{}, len(requested))
	for _, id := range requested {
		if _, err := parseAction(id); err != nil {
			return nil, err
		}
		want[id] = struct{}{}
	}
	byID := make(map[string]DevelopmentAction, len(supplied))
	for _, item := range supplied {
		parsed, err := parseAction(item.CapabilityID)
		if err != nil {
			return nil, err
		}
		if _, ok := want[item.CapabilityID]; !ok {
			return nil, fmt.Errorf("调试动作不属于当前团队配置")
		}
		if _, exists := byID[item.CapabilityID]; exists {
			return nil, fmt.Errorf("调试动作目录包含重复能力")
		}
		if item.Name != parsed.actionName || item.ObjectName != parsed.objectName {
			return nil, fmt.Errorf("调试动作定义与能力标识不一致")
		}
		if err := validateActionMetadata(actionMetadata{Name: item.Name, ObjectName: item.ObjectName, Label: item.Label,
			Description: item.Description, RequiresRecord: item.RequiresRecord, RequiresConfirmation: item.RequiresConfirmation, Params: item.Params}); err != nil {
			return nil, fmt.Errorf("调试动作 %q 的输入定义无效: %v", item.CapabilityID, err)
		}
		byID[item.CapabilityID] = item
	}
	if len(byID) != len(want) {
		return nil, fmt.Errorf("当前团队使用了尚未提供调试定义的 Forge 业务能力")
	}
	result := make([]DevelopmentAction, 0, len(want))
	for id := range want {
		result = append(result, byID[id])
	}
	sort.Slice(result, func(i, j int) bool { return result[i].CapabilityID < result[j].CapabilityID })
	return result, nil
}

type developmentTrialScope struct {
	Actions         []DevelopmentAction
	InputRevisionID string
}

func (s *Store) developmentDispatcher(ctx context.Context, requested []string, bindings []frozen.BusinessCapabilityBinding) (contract.ToolDispatcher, bool, error) {
	trial, ok, err := s.resolveDevelopmentTrial(ctx)
	if err != nil || !ok {
		return nil, ok, err
	}
	byID := make(map[string]DevelopmentAction, len(trial.Actions))
	for _, action := range trial.Actions {
		byID[action.CapabilityID] = action
	}
	selectedIDs := make([]string, 0, len(requested))
	selectedActions := make([]DevelopmentAction, 0, len(requested))
	for _, id := range requested {
		action, exists := byID[id]
		if !exists {
			return nil, true, fmt.Errorf("%w: developer trial action definition is unavailable", mcphost.ErrFailClosed)
		}
		if action.SimulationAuthorized {
			selectedIDs = append(selectedIDs, id)
			selectedActions = append(selectedActions, action)
		}
	}
	if len(selectedIDs) == 0 {
		return nil, true, nil
	}
	d, err := newDevelopmentDispatcherWithBindings(selectedIDs, selectedActions, bindings, trial.InputRevisionID)
	return d, true, err
}

func (s *Store) resolveDevelopmentTrial(ctx context.Context) (developmentTrialScope, bool, error) {
	if s == nil || s.pool == nil || s.tasks == nil {
		return developmentTrialScope{}, false, fmt.Errorf("%w: business delegation store unavailable", mcphost.ErrFailClosed)
	}
	current, ok := execution.CurrentTaskFromContext(ctx)
	if !ok || current.Subject.UserID == "" {
		return developmentTrialScope{}, false, nil
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return developmentTrialScope{}, false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err = s.tasks.ValidateCurrentTaskTx(ctx, tx); err != nil {
		return developmentTrialScope{}, false, fmt.Errorf("%w: current employee task changed", mcphost.ErrFailClosed)
	}
	var actionsRaw, requestRaw, admissionRaw []byte
	var storedDigest, trialActor, trialTeam, trialRequestID, runID, runSnapshotID, workflowID string
	var workflowVersion int
	// Fanout and resume tasks inherit the exact trial grant from the immutable
	// root source task, not from caller-controlled task context.
	err = tx.QueryRow(ctx, `SELECT t.business_actions,t.request_digest,t.request,t.actor_id,t.team_id,t.request_id::text,
		r.run_id,r.run_snapshot_id,r.workflow_id,r.workflow_version,p.receipt
		FROM weave_task_queue q
		JOIN weave_team_runs r ON r.workspace_id=q.workspace_id AND r.run_snapshot_id=q.run_snapshot_id
		JOIN weave_task_queue root ON root.workspace_id=r.workspace_id AND root.id=r.source_task_id
		JOIN weave_team_development_trials t ON t.workspace_id=root.workspace_id AND root.context_key='development:'||t.request_id::text
		JOIN weave_kernel_publication_requests p ON p.workspace_id=root.workspace_id AND p.request_id=root.context_key AND p.operation='candidate_run'
		WHERE q.workspace_id=$1 AND q.id=$2 AND t.actor_id=$3
		  AND root.source_ref='team-development:'||t.team_id
		  AND p.receipt->>'run_id'=r.run_id AND p.receipt->>'run_snapshot_id'=r.run_snapshot_id`,
		current.WorkspaceID, current.ID, current.Subject.UserID).Scan(&actionsRaw, &storedDigest, &requestRaw, &trialActor, &trialTeam, &trialRequestID,
		&runID, &runSnapshotID, &workflowID, &workflowVersion, &admissionRaw)
	if errors.Is(err, pgx.ErrNoRows) {
		return developmentTrialScope{}, false, nil
	}
	if err != nil {
		return developmentTrialScope{}, false, err
	}
	var request publication.CandidateRunRequest
	var admission publication.AdmissionReceipt
	var actions []DevelopmentAction
	if json.Unmarshal(requestRaw, &request) != nil || json.Unmarshal(admissionRaw, &admission) != nil || json.Unmarshal(actionsRaw, &actions) != nil ||
		trialActor == "" || trialActor != current.Subject.UserID || request.RequestID != "development:"+trialRequestID ||
		request.SourceRef != "team-development:"+trialTeam || request.Purpose != "developer-trial" ||
		request.Candidate.WorkspaceID != current.WorkspaceID || request.Candidate.WorkflowID != workflowID || request.Candidate.WorkflowVersion != workflowVersion ||
		admission.RunID != runID || admission.RunSnapshotID != runSnapshotID ||
		admission.Verify(execution.WithSubject(ctx, execution.Subject{WorkspaceID: current.WorkspaceID, UserID: trialActor}), request) != nil {
		return developmentTrialScope{}, false, fmt.Errorf("%w: developer trial admission binding mismatch", mcphost.ErrFailClosed)
	}
	payload, err := frozen.DecodeArtifactEnvelopeV1(request.Candidate)
	if err != nil {
		return developmentTrialScope{}, false, fmt.Errorf("%w: developer trial candidate is invalid", mcphost.ErrFailClosed)
	}
	graph, report := machine.DecodeGraphDefinitionV1(payload.GraphDefinition)
	if report != nil && len(report.Issues) > 0 {
		return developmentTrialScope{}, false, fmt.Errorf("%w: developer trial workflow is invalid", mcphost.ErrFailClosed)
	}
	actions, err = ValidateDevelopmentActions(machine.GraphBusinessCapabilities(graph, payload), actions)
	if err != nil {
		return developmentTrialScope{}, false, fmt.Errorf("%w: %v", mcphost.ErrFailClosed, err)
	}
	digest, err := DevelopmentTrialRequestDigest(ctx, request, actions)
	if err != nil || digest != storedDigest {
		return developmentTrialScope{}, false, fmt.Errorf("%w: developer trial action scope changed", mcphost.ErrFailClosed)
	}
	var input string
	if json.Unmarshal(request.Input, &input) != nil {
		return developmentTrialScope{}, false, fmt.Errorf("%w: developer trial input binding is invalid", mcphost.ErrFailClosed)
	}
	encodedInput, _ := json.Marshal(input)
	inputHash := sha256.Sum256(encodedInput)
	if request.InputVersion != hex.EncodeToString(inputHash[:]) {
		return developmentTrialScope{}, false, fmt.Errorf("%w: developer trial input binding mismatch", mcphost.ErrFailClosed)
	}
	if err = tx.Commit(ctx); err != nil {
		return developmentTrialScope{}, false, err
	}
	return developmentTrialScope{Actions: actions, InputRevisionID: request.InputVersion}, true, nil
}

func developmentCatalog(actions []DevelopmentAction) map[string]actionMetadata {
	catalog := make(map[string]actionMetadata, len(actions))
	for _, item := range actions {
		catalog[item.ObjectName+"."+item.Name] = actionMetadata{Name: item.Name, ObjectName: item.ObjectName, Label: item.Label,
			Description: item.Description, RequiresRecord: item.RequiresRecord, RequiresConfirmation: item.RequiresConfirmation, Params: item.Params}
	}
	return catalog
}

type developmentHost struct {
	syntheticMaterialCount int
	trackOutcomes          bool
}

func (developmentHost) ListTools(context.Context) ([]contract.ToolDef, error) { return nil, nil }
func (h developmentHost) Dispatch(_ context.Context, call contract.ToolCall) (*contract.ToolResult, error) {
	if call.Name != "run_action" {
		return &contract.ToolResult{CallID: call.ID, ToolName: call.Name, Content: "开发调试只允许模拟已绑定的业务动作", IsError: true}, nil
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(call.Args), &payload); err != nil {
		return &contract.ToolResult{CallID: call.ID, ToolName: call.Name, Content: "调试动作输入无效", IsError: true}, nil
	}
	payload["ok"] = true
	payload["simulated"] = true
	payload["message"] = fmt.Sprintf("隔离模拟调用已完成，使用 %d 份合成材料；未访问 Forge，也未写入业务数据。", h.syntheticMaterialCount)
	content, _ := json.Marshal(payload)
	return &contract.ToolResult{CallID: call.ID, ToolName: call.Name, Content: string(content)}, nil
}

func newDevelopmentDispatcher(ids []string, actions []DevelopmentAction) (*dispatcher, error) {
	return newDispatcherWithNotice(developmentHost{}, ids, developmentCatalog(actions), "开发调试会记录本次调用，但不会访问 Forge 或写入业务数据。")
}

func newDevelopmentDispatcherWithBindings(ids []string, actions []DevelopmentAction, bindings []frozen.BusinessCapabilityBinding, inputRevisionID string) (*dispatcher, error) {
	checksum := sha256.Sum256([]byte("development-trial-material"))
	resources := []delegatedResource{{Type: "forge-file", ID: "development-trial-material", Name: "试跑样例材料.txt", Bytes: 1, SHA256: hex.EncodeToString(checksum[:])}}
	d, err := newDispatcherWithBindings(developmentHost{syntheticMaterialCount: len(resources), trackOutcomes: true}, ids, developmentCatalog(actions), bindings, resources, "本次已明确选择的开发模拟动作；不会访问 Forge 或写入业务数据。")
	if err != nil {
		return nil, err
	}
	d.trackOutcomes = true
	d.inputRevisionID = inputRevisionID
	return d, nil
}

// DevelopmentTrialRequestDigest binds the candidate request, frozen action
// definitions, and each explicit simulation authorization in the existing
// trial request digest. Callers must pass normalized actions.
func DevelopmentTrialRequestDigest(ctx context.Context, request publication.CandidateRunRequest, actions []DevelopmentAction) (string, error) {
	requestDigest, err := request.Fingerprint(ctx)
	if err != nil {
		return "", err
	}
	encoded, err := json.Marshal(struct {
		RequestDigest string              `json:"request_digest"`
		Actions       []DevelopmentAction `json:"actions"`
	}{requestDigest, actions})
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:]), nil
}
