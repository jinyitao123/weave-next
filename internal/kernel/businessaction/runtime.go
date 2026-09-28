// Package businessaction exposes published Forge actions as task-scoped Loom
// tools. It never exposes Forge's generic run_action arguments to the model:
// the published capability fixes actionName and objectName server-side.
package businessaction

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jinyitao123/loom/contract"
	"github.com/jinyitao123/weave/internal/base/execution"
	"github.com/jinyitao123/weave/internal/base/frozen"
	"github.com/jinyitao123/weave/internal/kernel/compiler"
	"github.com/jinyitao123/weave/internal/kernel/mcphost"
	"github.com/jinyitao123/weave/internal/kernel/secret"
	"github.com/jinyitao123/weave/internal/kernel/taskqueue"
	"github.com/jinyitao123/weave/internal/kernel/workflow"
)

const capabilityPrefix = "forge:action:"

var toolPart = regexp.MustCompile(`[^a-z0-9_]+`)
var frozenSHA256 = regexp.MustCompile(`^[0-9a-f]{64}$`)

type Store struct {
	pool  *pgxpool.Pool
	tasks *taskqueue.Store
	key   []byte
	now   func() time.Time
}

func NewStore(pool *pgxpool.Pool, tasks *taskqueue.Store, key []byte) *Store {
	return &Store{pool: pool, tasks: tasks, key: append([]byte(nil), key...), now: time.Now}
}

type Factory struct {
	Inner         workflow.RuntimeHostFactory
	Store         *Store
	MaterialStore *Store
}

func (f Factory) Build(ctx context.Context, bundle frozen.FrozenExecutionBundle, resolver workflow.RuntimeCredentialResolver) (compiler.FrozenBuildOpts, io.Closer, error) {
	opts, closer, err := f.Inner.Build(ctx, bundle, resolver)
	return f.attach(ctx, bundle, opts, closer, err)
}

func (f Factory) BuildWithLLM(ctx context.Context, bundle frozen.FrozenExecutionBundle, resolver workflow.RuntimeCredentialResolver, llm contract.LLM) (compiler.FrozenBuildOpts, io.Closer, error) {
	inner, ok := f.Inner.(workflow.RuntimeHostFactoryWithLLM)
	if !ok {
		return compiler.FrozenBuildOpts{}, nil, fmt.Errorf("%w: runtime host cannot preserve business tools for assigned inference", mcphost.ErrFailClosed)
	}
	opts, closer, err := inner.BuildWithLLM(ctx, bundle, resolver, llm)
	return f.attach(ctx, bundle, opts, closer, err)
}

func (f Factory) attach(ctx context.Context, bundle frozen.FrozenExecutionBundle, opts compiler.FrozenBuildOpts, closer io.Closer, err error) (compiler.FrozenBuildOpts, io.Closer, error) {
	if err != nil {
		return opts, closer, err
	}
	useMaterialRead := bundle.Agent.Engine == "loom"
	useBusinessActions := len(bundle.Agent.BusinessCapabilityIDs) > 0
	if !useMaterialRead && !useBusinessActions {
		return opts, closer, nil
	}
	dispatchers := []contract.ToolDispatcher{opts.Tools}
	if useMaterialRead {
		materialStore := f.MaterialStore
		if materialStore == nil {
			materialStore = f.Store
		}
		var materialDispatcher contract.ToolDispatcher
		var readErr error
		if materialStore != nil {
			materialDispatcher, readErr = materialStore.MaterialReadDispatcher(ctx)
		} else {
			materialDispatcher, readErr = newMaterialReadDispatcher(nil, frozenMaterialReadScope{}, map[string]frozenMaterialReadFile{})
		}
		if readErr != nil {
			if closer != nil {
				_ = closer.Close()
			}
			return compiler.FrozenBuildOpts{}, nil, readErr
		}
		if materialDispatcher != nil {
			dispatchers = append(dispatchers, materialDispatcher)
		}
	}
	if useBusinessActions {
		if f.Store == nil {
			if closer != nil {
				_ = closer.Close()
			}
			return compiler.FrozenBuildOpts{}, nil, fmt.Errorf("%w: business delegation store unavailable", mcphost.ErrFailClosed)
		}
		dispatcher, dispatchErr := f.Store.dispatcher(ctx, bundle.Agent.BusinessCapabilityIDs, bundle.Agent.BusinessCapabilityBindings)
		if dispatchErr != nil {
			if closer != nil {
				_ = closer.Close()
			}
			return compiler.FrozenBuildOpts{}, nil, dispatchErr
		}
		if dispatcher != nil {
			dispatchers = append(dispatchers, dispatcher)
		}
	}
	if len(dispatchers) > 1 {
		opts.Tools = mcphost.NewCompositeDispatcher(dispatchers...)
	}
	return opts, closer, nil
}

type delegation struct {
	inputRevisionID string
	issuer          string
	token           []byte
	actions         []string
	resources       []delegatedResource
}

type delegatedResource struct {
	Type       string `json:"type"`
	MaterialID string `json:"materialId,omitempty"`
	ID         string `json:"id"`
	Name       string `json:"name,omitempty"`
	MediaType  string `json:"mediaType,omitempty"`
	Bytes      int64  `json:"bytes,omitempty"`
	SHA256     string `json:"sha256"`
	ObjectName string `json:"object_name,omitempty"`
}

func (s *Store) dispatcher(ctx context.Context, requested []string, bindings []frozen.BusinessCapabilityBinding) (contract.ToolDispatcher, error) {
	if actions, ok, err := s.resolveDevelopmentTrial(ctx, requested); err != nil {
		return nil, err
	} else if ok {
		return newDevelopmentDispatcherWithBindings(requested, actions, bindings)
	}
	bound, err := s.resolve(ctx, requested)
	if err != nil {
		return nil, err
	}
	if len(bound.actions) == 0 {
		clear(bound.token)
		return nil, nil
	}
	endpoint, err := url.Parse(bound.issuer)
	if err != nil || endpoint.Scheme == "" || endpoint.Host == "" || endpoint.User != nil ||
		(endpoint.Scheme != "http" && endpoint.Scheme != "https") {
		clear(bound.token)
		return nil, fmt.Errorf("%w: Forge delegation issuer is invalid", mcphost.ErrFailClosed)
	}
	endpoint.Path = "/api/v1/mcp"
	endpoint.RawPath, endpoint.RawQuery, endpoint.Fragment = "", "", ""
	headers := map[string]string{"Authorization": "Bearer " + string(bound.token)}
	clear(bound.token)
	runAction := contract.ToolDef{
		Name: "run_action", Description: "Invoke the server-selected Forge business action.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"actionName":{"type":"string"},"objectName":{"type":"string"},"recordId":{"type":"string"},"params":{"type":"object"}},"required":["actionName","objectName"],"additionalProperties":false}`),
	}
	listActions := contract.ToolDef{
		Name: "list_actions", Description: "Read the employee-visible Forge business action catalog.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`),
		ReadOnly:    true,
	}
	toolContract, err := mcphost.NewToolContract([]contract.ToolDef{listActions, runAction})
	if err != nil {
		clearHeader(headers)
		return nil, err
	}
	host := mcphost.NewHTTPHost(endpoint.String(), mcphost.WithHeaders(headers), mcphost.WithFilter([]string{"list_actions", "run_action"}), mcphost.WithToolContract(toolContract), mcphost.WithUnknownDispatchOutcome(),
		mcphost.WithDispatchGuard(func(callCtx context.Context) error {
			return s.validate(callCtx, bound.inputRevisionID, bound.actions)
		}))
	clearHeader(headers)
	catalog, err := readActionCatalog(ctx, host)
	if err != nil {
		return nil, err
	}
	dispatcher, err := newDispatcherWithResourcesAndBindings(host, bound.actions, catalog, bound.resources, bindings)
	if err != nil {
		return nil, err
	}
	dispatcher.trackOutcomes = true
	dispatcher.inputRevisionID = bound.inputRevisionID
	return dispatcher, nil
}

func (s *Store) resolveDevelopmentTrial(ctx context.Context, requested []string) ([]DevelopmentAction, bool, error) {
	if s == nil || s.pool == nil || s.tasks == nil {
		return nil, false, fmt.Errorf("%w: business delegation store unavailable", mcphost.ErrFailClosed)
	}
	current, ok := execution.CurrentTaskFromContext(ctx)
	if !ok || current.Subject.UserID == "" {
		return nil, false, nil
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err = s.tasks.ValidateCurrentTaskTx(ctx, tx); err != nil {
		return nil, false, fmt.Errorf("%w: current employee task changed", mcphost.ErrFailClosed)
	}
	var raw []byte
	// Fanout legs and resume tasks replace the source task's context key with
	// their coordination group. Resolve the development trial through the
	// immutable TeamRun source task so every task in the same frozen run sees
	// the same isolated action catalog.
	err = tx.QueryRow(ctx, `SELECT t.business_actions
		FROM weave_task_queue q
		JOIN weave_team_runs r
		  ON r.workspace_id=q.workspace_id
		 AND r.run_snapshot_id=q.run_snapshot_id
		JOIN weave_task_queue root
		  ON root.workspace_id=r.workspace_id
		 AND root.id=r.source_task_id
		JOIN weave_team_development_trials t
		  ON t.workspace_id=root.workspace_id
		 AND root.context_key='development:' || t.request_id::text
		WHERE q.workspace_id=$1 AND q.id=$2 AND t.actor_id=$3
		  AND root.source_ref='team-development:' || t.team_id`,
		current.WorkspaceID, current.ID, current.Subject.UserID).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	var actions []DevelopmentAction
	if err = json.Unmarshal(raw, &actions); err != nil {
		return nil, false, fmt.Errorf("%w: development action catalog is invalid", mcphost.ErrFailClosed)
	}
	actions, err = ValidateDevelopmentActions(requested, actions)
	if err != nil {
		return nil, false, fmt.Errorf("%w: %v", mcphost.ErrFailClosed, err)
	}
	if err = tx.Commit(ctx); err != nil {
		return nil, false, err
	}
	return actions, true, nil
}

func (s *Store) resolve(ctx context.Context, requested []string) (delegation, error) {
	if s == nil || s.pool == nil || s.tasks == nil || len(s.key) != 32 {
		return delegation{}, fmt.Errorf("%w: business delegation store unavailable", mcphost.ErrFailClosed)
	}
	current, ok := execution.CurrentTaskFromContext(ctx)
	if !ok || current.Subject.UserID == "" {
		return delegation{}, fmt.Errorf("%w: current employee task is required", mcphost.ErrFailClosed)
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return delegation{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := s.tasks.ValidateCurrentTaskTx(ctx, tx); err != nil {
		return delegation{}, fmt.Errorf("%w: current employee task changed", mcphost.ErrFailClosed)
	}
	var inputRevisionID, issuer, ciphertext, digest string
	var actionsRaw, resourcesRaw []byte
	var expiresAt time.Time
	err = tx.QueryRow(ctx, `SELECT d.input_revision_id,d.issuer,d.credential_ciphertext,d.credential_sha256,d.allowed_actions,d.resources,d.expires_at
		FROM weave_task_queue q
		JOIN weave_run_delivery_state r ON r.workspace_id=q.workspace_id AND r.run_snapshot_id=q.run_snapshot_id
		JOIN weave_task_business_delegations d ON d.workspace_id=r.workspace_id AND d.input_revision_id=r.input_revision_id
		WHERE q.workspace_id=$1 AND q.id=$2 AND d.user_id=$3 AND d.revoked_at IS NULL`,
		current.WorkspaceID, current.ID, current.Subject.UserID).Scan(&inputRevisionID, &issuer, &ciphertext, &digest, &actionsRaw, &resourcesRaw, &expiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return delegation{}, fmt.Errorf("%w: task has no active Forge delegation", mcphost.ErrFailClosed)
	}
	if err != nil {
		return delegation{}, err
	}
	if !expiresAt.After(s.now().UTC()) {
		return delegation{}, fmt.Errorf("%w: Forge task delegation expired", mcphost.ErrFailClosed)
	}
	var taskAllowed []string
	if err := json.Unmarshal(actionsRaw, &taskAllowed); err != nil {
		return delegation{}, fmt.Errorf("%w: task business action scope is invalid", mcphost.ErrFailClosed)
	}
	allowed := intersectActions(requested, taskAllowed)
	resources, err := decodeDelegatedResources(resourcesRaw, inputRevisionID)
	if err != nil {
		return delegation{}, fmt.Errorf("%w: %v", mcphost.ErrFailClosed, err)
	}
	token, err := secret.Open(s.key, ciphertext)
	if err != nil {
		return delegation{}, fmt.Errorf("%w: Forge task credential unavailable", mcphost.ErrFailClosed)
	}
	hash := sha256.Sum256(token)
	if hex.EncodeToString(hash[:]) != digest {
		clear(token)
		return delegation{}, fmt.Errorf("%w: Forge task credential digest mismatch", mcphost.ErrFailClosed)
	}
	if err := tx.Commit(ctx); err != nil {
		clear(token)
		return delegation{}, err
	}
	return delegation{inputRevisionID: inputRevisionID, issuer: issuer, token: token, actions: allowed, resources: resources}, nil
}

func decodeDelegatedResources(raw []byte, inputRevisionID string) ([]delegatedResource, error) {
	var stored []delegatedResource
	if err := json.Unmarshal(raw, &stored); err != nil || len(stored) == 0 || len(stored) > 10 {
		return nil, errors.New("task business resources are invalid")
	}
	verifiedInput := false
	resources := make([]delegatedResource, 0, len(stored)-1)
	seen := make(map[string]struct{}, len(stored))
	seenMaterialIDs := make(map[string]struct{}, len(stored))
	var totalFileBytes int64
	for _, item := range stored {
		item.Type, item.MaterialID, item.ID = strings.TrimSpace(item.Type), strings.TrimSpace(item.MaterialID), strings.TrimSpace(item.ID)
		item.Name, item.MediaType, item.SHA256 = strings.TrimSpace(item.Name), strings.TrimSpace(item.MediaType), strings.TrimSpace(item.SHA256)
		if item.ID == "" || item.ID != strings.TrimSpace(item.ID) || !frozenSHA256.MatchString(item.SHA256) {
			return nil, errors.New("task business resources are invalid")
		}
		key := item.Type + "\x1f" + item.ID
		if _, duplicate := seen[key]; duplicate {
			return nil, errors.New("task business resources contain duplicates")
		}
		seen[key] = struct{}{}
		switch item.Type {
		case "dispatch-input":
			if verifiedInput || item.ID != inputRevisionID || item.Name != "" || item.Bytes != 0 ||
				item.MaterialID != "" || item.MediaType != "" || item.ObjectName != "" {
				return nil, errors.New("task input resource does not match the active delegation")
			}
			verifiedInput = true
		case "forge-file":
			if item.Name == "" || len(item.Name) > 255 || item.Bytes < 1 || item.Bytes > 2<<20 || item.ObjectName != "" ||
				item.MaterialID != "" && !frozenMaterialIDPattern.MatchString(item.MaterialID) ||
				item.MediaType != "" && !supportedMaterialMediaType(item.MediaType) {
				return nil, errors.New("task Forge resource is invalid")
			}
			if item.MaterialID != "" {
				if _, duplicate := seenMaterialIDs[item.MaterialID]; duplicate {
					return nil, errors.New("task Forge material IDs are duplicated")
				}
				seenMaterialIDs[item.MaterialID] = struct{}{}
			}
			totalFileBytes += item.Bytes
			if totalFileBytes > 8<<20 {
				return nil, errors.New("task Forge materials exceed the frozen byte limit")
			}
			resources = append(resources, item)
		case "forge-record":
			if item.Name != "" || item.Bytes != 0 || item.MaterialID != "" || item.MediaType != "" {
				return nil, errors.New("task Forge resource is invalid")
			}
			if err := validateRecordResource(item); err != nil {
				return nil, err
			}
			resources = append(resources, item)
		default:
			return nil, errors.New("task business resource type is unsupported")
		}
	}
	if !verifiedInput {
		return nil, errors.New("task input resource is missing")
	}
	return resources, nil
}

func (s *Store) validate(ctx context.Context, inputRevisionID string, requested []string) error {
	if s == nil || s.pool == nil || s.tasks == nil {
		return errors.New("business delegation store unavailable")
	}
	current, ok := execution.CurrentTaskFromContext(ctx)
	if !ok || current.Subject.UserID == "" {
		return execution.ErrCurrentTaskMismatch
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := s.tasks.ValidateCurrentTaskTx(ctx, tx); err != nil {
		return err
	}
	var actionsRaw []byte
	var expiresAt time.Time
	err = tx.QueryRow(ctx, `SELECT d.allowed_actions,d.expires_at FROM weave_task_queue q
		JOIN weave_run_delivery_state r ON r.workspace_id=q.workspace_id AND r.run_snapshot_id=q.run_snapshot_id
		JOIN weave_task_business_delegations d ON d.workspace_id=r.workspace_id AND d.input_revision_id=r.input_revision_id
		WHERE q.workspace_id=$1 AND q.id=$2 AND d.user_id=$3 AND d.input_revision_id=$4 AND d.revoked_at IS NULL`,
		current.WorkspaceID, current.ID, current.Subject.UserID, inputRevisionID).Scan(&actionsRaw, &expiresAt)
	if err != nil {
		return err
	}
	var allowed []string
	if json.Unmarshal(actionsRaw, &allowed) != nil || !containsAll(allowed, requested) || !expiresAt.After(s.now().UTC()) {
		return errors.New("business delegation no longer authorizes this action")
	}
	return tx.Commit(ctx)
}

type dispatcher struct {
	host            contract.ToolDispatcher
	tools           []contract.ToolDef
	byTool          map[string]action
	bound           *mcphost.ToolContract
	records         map[string]string
	recordHashes    map[string]string
	params          map[string]map[string]any
	trackOutcomes   bool
	inputRevisionID string
}

type action struct{ capabilityID, objectName, actionName, label string }

type actionParam struct {
	Name        string   `json:"name"`
	Field       string   `json:"field,omitempty"`
	Label       string   `json:"label,omitempty"`
	Type        string   `json:"type,omitempty"`
	Multiple    bool     `json:"multiple,omitempty"`
	Required    bool     `json:"required,omitempty"`
	Description string   `json:"description,omitempty"`
	Enum        []string `json:"enum,omitempty"`
}

type actionMetadata struct {
	Name                 string        `json:"name"`
	ObjectName           string        `json:"objectName"`
	Label                string        `json:"label,omitempty"`
	Description          string        `json:"description,omitempty"`
	RequiresRecord       bool          `json:"requiresRecord,omitempty"`
	RequiresConfirmation bool          `json:"requiresConfirmation,omitempty"`
	Params               []actionParam `json:"params,omitempty"`
}

// DevelopmentAction is a credential-free Forge action definition frozen with
// one developer trial. It is used only to construct an isolated tool contract;
// the development dispatcher never contacts Forge or writes business data.
type DevelopmentAction struct {
	CapabilityID         string        `json:"capability_id"`
	Name                 string        `json:"name"`
	ObjectName           string        `json:"object_name"`
	Label                string        `json:"label,omitempty"`
	Description          string        `json:"description,omitempty"`
	RequiresRecord       bool          `json:"requires_record,omitempty"`
	RequiresConfirmation bool          `json:"requires_confirmation,omitempty"`
	Params               []actionParam `json:"params,omitempty"`
}

// ValidateDevelopmentActions verifies that a trial freezes exactly the Forge
// actions referenced by its candidate members. Extra definitions are rejected
// so a trial request cannot widen the published member capability set.
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

func developmentCatalog(actions []DevelopmentAction) map[string]actionMetadata {
	catalog := make(map[string]actionMetadata, len(actions))
	for _, item := range actions {
		catalog[item.ObjectName+"."+item.Name] = actionMetadata{Name: item.Name, ObjectName: item.ObjectName, Label: item.Label,
			Description: item.Description, RequiresRecord: item.RequiresRecord, RequiresConfirmation: item.RequiresConfirmation, Params: item.Params}
	}
	return catalog
}

func readActionCatalog(ctx context.Context, host contract.ToolDispatcher) (map[string]actionMetadata, error) {
	result, err := host.Dispatch(ctx, contract.ToolCall{ID: "forge-action-catalog", Name: "list_actions", Args: `{}`})
	if err != nil {
		return nil, fmt.Errorf("%w: Forge action catalog unavailable: %v", mcphost.ErrFailClosed, err)
	}
	if result == nil || result.IsError {
		message := "empty response"
		if result != nil && strings.TrimSpace(result.Content) != "" {
			message = strings.TrimSpace(result.Content)
		}
		return nil, fmt.Errorf("%w: Forge action catalog unavailable: %s", mcphost.ErrFailClosed, message)
	}
	var payload struct {
		Actions []json.RawMessage `json:"actions"`
	}
	if err := json.Unmarshal([]byte(result.Content), &payload); err != nil {
		return nil, fmt.Errorf("%w: Forge action catalog is invalid", mcphost.ErrFailClosed)
	}
	catalog := make(map[string]actionMetadata, len(payload.Actions))
	for _, raw := range payload.Actions {
		var item actionMetadata
		if err := json.Unmarshal(raw, &item); err != nil {
			return nil, fmt.Errorf("%w: Forge action catalog contains an invalid action", mcphost.ErrFailClosed)
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(raw, &fields); err != nil {
			return nil, fmt.Errorf("%w: Forge action catalog contains an invalid action", mcphost.ErrFailClosed)
		}
		if required, exists := fields["requiresRecord"]; !exists {
			// Unknown metadata must never broaden a business action's record scope.
			item.RequiresRecord = true
		} else if string(required) != "true" && string(required) != "false" {
			return nil, fmt.Errorf("%w: Forge action record requirement is invalid", mcphost.ErrFailClosed)
		}
		if strings.TrimSpace(item.Name) == "" || strings.TrimSpace(item.ObjectName) == "" ||
			item.Name != strings.TrimSpace(item.Name) || item.ObjectName != strings.TrimSpace(item.ObjectName) {
			continue
		}
		key := item.ObjectName + "." + item.Name
		if _, exists := catalog[key]; exists {
			return nil, fmt.Errorf("%w: Forge action catalog contains a duplicate action", mcphost.ErrFailClosed)
		}
		catalog[key] = item
	}
	return catalog, nil
}

func newDispatcher(host contract.ToolDispatcher, ids []string, catalog map[string]actionMetadata) (*dispatcher, error) {
	return newDispatcherWithNotice(host, ids, catalog, "仅在当前员工明确授权且本次固定材料已核对时调用。")
}

func newDispatcherWithResources(host contract.ToolDispatcher, ids []string, catalog map[string]actionMetadata, resources []delegatedResource) (*dispatcher, error) {
	return newDispatcherWithResourcesAndBindings(host, ids, catalog, resources, nil)
}

func newDispatcherWithResourcesAndBindings(host contract.ToolDispatcher, ids []string, catalog map[string]actionMetadata, resources []delegatedResource, bindings []frozen.BusinessCapabilityBinding) (*dispatcher, error) {
	notice := "仅在当前员工明确授权且本次固定材料已核对时调用。"
	if len(resources) > 0 {
		encoded, err := json.Marshal(resources)
		if err != nil {
			return nil, fmt.Errorf("%w: task business resources cannot be projected", mcphost.ErrFailClosed)
		}
		notice += " 本任务已验证并冻结以下资源。需要材料参数时，必须从这里逐项使用对应的 id、name、sha256 和 bytes，不得猜测或替换：" + string(encoded)
	}
	d, err := newDispatcherWithBindings(host, ids, catalog, bindings, resources, notice)
	if err != nil {
		return nil, err
	}
	if err := d.bindRecords(catalog, resources); err != nil {
		return nil, fmt.Errorf("%w: %v", mcphost.ErrFailClosed, err)
	}
	return d, nil
}

func newDispatcherWithNotice(host contract.ToolDispatcher, ids []string, catalog map[string]actionMetadata, notice string) (*dispatcher, error) {
	return newDispatcherWithBindings(host, ids, catalog, nil, nil, notice)
}

func newDispatcherWithBindings(host contract.ToolDispatcher, ids []string, catalog map[string]actionMetadata, bindings []frozen.BusinessCapabilityBinding, resources []delegatedResource, notice string) (*dispatcher, error) {
	selected := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		selected[id] = struct{}{}
	}
	relevantBindings := make([]frozen.BusinessCapabilityBinding, 0, len(bindings))
	for _, binding := range bindings {
		if _, ok := selected[binding.CapabilityID]; ok {
			relevantBindings = append(relevantBindings, binding)
		}
	}
	normalized, err := frozen.NormalizeBusinessCapabilityBindings(relevantBindings, ids)
	if err != nil {
		return nil, fmt.Errorf("%w: invalid published business parameter bindings: %v", mcphost.ErrFailClosed, err)
	}
	bindingByCapability := make(map[string][]frozen.BusinessCapabilityParameterBinding, len(normalized))
	for _, binding := range normalized {
		bindingByCapability[binding.CapabilityID] = binding.Parameters
	}
	d := &dispatcher{host: host, byTool: map[string]action{}, params: map[string]map[string]any{}}
	ordered := append([]string(nil), ids...)
	sort.Strings(ordered)
	for _, id := range ordered {
		parsed, err := parseAction(id)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", mcphost.ErrFailClosed, err)
		}
		name := virtualToolName(parsed)
		if _, exists := d.byTool[name]; exists {
			return nil, fmt.Errorf("%w: Forge capability tool collision", mcphost.ErrFailClosed)
		}
		metadata, ok := catalog[parsed.objectName+"."+parsed.actionName]
		if !ok {
			return nil, fmt.Errorf("%w: published Forge action %q is unavailable to the current employee", mcphost.ErrFailClosed, id)
		}
		injected, err := bindActionParameters(metadata, bindingByCapability[id], resources)
		if err != nil {
			return nil, fmt.Errorf("%w: Forge action %q has invalid published parameter bindings: %v", mcphost.ErrFailClosed, id, err)
		}
		schema, err := actionInputSchemaWithBindings(metadata, bindingByCapability[id])
		if err != nil {
			return nil, fmt.Errorf("%w: Forge action %q has invalid input metadata: %v", mcphost.ErrFailClosed, id, err)
		}
		description := strings.TrimSpace(metadata.Description)
		if description == "" {
			description = strings.TrimSpace(metadata.Label)
		}
		if description == "" {
			description = fmt.Sprintf("执行 Forge 业务动作 %s。", parsed.actionName)
		}
		parsed.label = strings.TrimSpace(metadata.Label)
		d.byTool[name] = parsed
		d.params[name] = injected
		d.tools = append(d.tools, contract.ToolDef{
			Name:        name,
			Description: description + " " + notice,
			InputSchema: schema,
		})
	}
	bound, err := mcphost.NewToolContract(d.tools)
	if err != nil {
		return nil, err
	}
	d.bound = bound
	return d, nil
}

type developmentHost struct{}

func (developmentHost) ListTools(context.Context) ([]contract.ToolDef, error) { return nil, nil }
func (developmentHost) Dispatch(_ context.Context, call contract.ToolCall) (*contract.ToolResult, error) {
	if call.Name != "run_action" {
		return &contract.ToolResult{CallID: call.ID, ToolName: call.Name, Content: "开发调试只允许模拟已绑定的业务动作", IsError: true}, nil
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(call.Args), &payload); err != nil {
		return &contract.ToolResult{CallID: call.ID, ToolName: call.Name, Content: "调试动作输入无效", IsError: true}, nil
	}
	payload["simulated"] = true
	payload["message"] = "隔离调试已验证动作名称、输入字段和成员调用路径；未访问 Forge，未写入业务数据。"
	content, _ := json.Marshal(payload)
	return &contract.ToolResult{CallID: call.ID, ToolName: call.Name, Content: string(content)}, nil
}

func newDevelopmentDispatcher(ids []string, actions []DevelopmentAction) (*dispatcher, error) {
	return newDispatcherWithNotice(developmentHost{}, ids, developmentCatalog(actions), "开发调试会记录本次调用，但不会访问 Forge 或写入业务数据。")
}

func newDevelopmentDispatcherWithBindings(ids []string, actions []DevelopmentAction, bindings []frozen.BusinessCapabilityBinding) (*dispatcher, error) {
	checksum := sha256.Sum256([]byte("development-trial-material"))
	resources := []delegatedResource{{Type: "forge-file", ID: "development-trial-material", Name: "试跑样例材料.txt", Bytes: 1, SHA256: hex.EncodeToString(checksum[:])}}
	return newDispatcherWithBindings(developmentHost{}, ids, developmentCatalog(actions), bindings, resources, "隔离调试使用合成材料值验证参数映射；不会访问 Forge 或写入业务数据。")
}

func actionInputSchema(metadata actionMetadata) (json.RawMessage, error) {
	return actionInputSchemaWithBindings(metadata, nil)
}

func validateActionMetadata(metadata actionMetadata) error {
	seen := make(map[string]struct{}, len(metadata.Params))
	for _, param := range metadata.Params {
		rawName := param.Name
		if rawName == "" {
			rawName = param.Field
		}
		name := strings.TrimSpace(rawName)
		if name == "" || name != rawName {
			return errors.New("parameter name is empty or padded")
		}
		if _, exists := seen[name]; exists {
			return fmt.Errorf("duplicate parameter %q", name)
		}
		seen[name] = struct{}{}
		jsonType := normalizeActionParamType(param.Type)
		if strings.EqualFold(strings.TrimSpace(param.Type), "file") {
			continue
		}
		switch jsonType {
		case "string", "number", "boolean":
		case "array":
			return fmt.Errorf("array parameter %q has no item schema and cannot be exposed safely", name)
		default:
			return fmt.Errorf("unsupported parameter type %q", param.Type)
		}
	}
	return nil
}

func actionInputSchemaWithBindings(metadata actionMetadata, bindings []frozen.BusinessCapabilityParameterBinding) (json.RawMessage, error) {
	if err := validateActionMetadata(metadata); err != nil {
		return nil, err
	}
	protected := make(map[string]struct{}, len(bindings))
	for _, binding := range bindings {
		protected[binding.Name] = struct{}{}
	}
	properties := map[string]any{}
	required := make([]string, 0, 2)
	if metadata.RequiresRecord {
		properties["recordId"] = map[string]any{"type": "string", "minLength": 1, "description": "目标业务记录"}
		required = append(required, "recordId")
	} else {
		properties["recordId"] = map[string]any{"type": "string", "minLength": 1, "description": "目标业务记录（动作需要时填写）"}
	}
	paramProperties := make(map[string]any, len(metadata.Params))
	paramRequired := make([]string, 0, len(metadata.Params))
	for _, param := range metadata.Params {
		rawName := param.Name
		if rawName == "" {
			rawName = param.Field
		}
		name := strings.TrimSpace(rawName)
		if name == "" || name != rawName {
			return nil, errors.New("parameter name is empty or padded")
		}
		if _, exists := paramProperties[name]; exists {
			return nil, fmt.Errorf("duplicate parameter %q", name)
		}
		if _, isProtected := protected[name]; isProtected {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(param.Type), "file") {
			return nil, fmt.Errorf("file parameter %q requires an explicit task-material binding", name)
		}
		jsonType := normalizeActionParamType(param.Type)
		switch jsonType {
		case "", "string":
			jsonType = "string"
		case "number", "boolean":
		default:
			return nil, fmt.Errorf("unsupported parameter type %q", param.Type)
		}
		property := map[string]any{"type": jsonType}
		if strings.TrimSpace(param.Description) != "" {
			property["description"] = strings.TrimSpace(param.Description)
		}
		if len(param.Enum) > 0 {
			property["enum"] = param.Enum
		}
		paramProperties[name] = property
		if param.Required {
			paramRequired = append(paramRequired, name)
		}
	}
	paramsSchema := map[string]any{"type": "object", "properties": paramProperties, "additionalProperties": false}
	if len(paramRequired) > 0 {
		paramsSchema["required"] = paramRequired
		required = append(required, "params")
	}
	properties["params"] = paramsSchema
	schema := map[string]any{"type": "object", "properties": properties, "additionalProperties": false}
	if len(required) > 0 {
		schema["required"] = required
	}
	raw, err := json.Marshal(schema)
	return json.RawMessage(raw), err
}

func normalizeActionParamType(value string) string {
	switch strings.TrimSpace(strings.ToLower(value)) {
	case "", "string", "text", "file":
		return "string"
	case "integer", "currency":
		return "number"
	case "number", "boolean", "array":
		return strings.TrimSpace(strings.ToLower(value))
	default:
		return strings.TrimSpace(strings.ToLower(value))
	}
}

func bindActionParameters(metadata actionMetadata, bindings []frozen.BusinessCapabilityParameterBinding, resources []delegatedResource) (map[string]any, error) {
	if len(bindings) == 0 {
		return nil, nil
	}
	params := make(map[string]actionParam, len(metadata.Params))
	for _, param := range metadata.Params {
		name := param.Name
		if name == "" {
			name = param.Field
		}
		if name == "" {
			return nil, errors.New("Forge action parameter name is empty")
		}
		if _, duplicate := params[name]; duplicate {
			return nil, fmt.Errorf("Forge action parameter %q is duplicated", name)
		}
		params[name] = param
	}
	files := make([]delegatedResource, 0, len(resources))
	for _, resource := range resources {
		if resource.Type == "forge-file" {
			if strings.TrimSpace(resource.ID) == "" || strings.TrimSpace(resource.Name) == "" || resource.Bytes < 1 || !frozenSHA256.MatchString(resource.SHA256) {
				return nil, errors.New("frozen Forge file resource is invalid")
			}
			files = append(files, resource)
		}
	}
	result := make(map[string]any, len(bindings))
	for _, binding := range bindings {
		param, ok := params[binding.Name]
		if !ok {
			return nil, fmt.Errorf("Forge action parameter %q is not defined", binding.Name)
		}
		parameterType := strings.ToLower(strings.TrimSpace(param.Type))
		if parameterType == "file" && (binding.Source != frozen.BusinessSourceMaterialID || param.Multiple) {
			return nil, fmt.Errorf("file parameter %q supports only one explicitly bound file", binding.Name)
		}
		if parameterType != "" && parameterType != "string" && parameterType != "text" && parameterType != "file" {
			return nil, fmt.Errorf("Forge action parameter %q cannot receive a material value", binding.Name)
		}
		var value string
		switch binding.Source {
		case frozen.BusinessSourceMaterialID, frozen.BusinessSourceMaterialName, frozen.BusinessSourceMaterialSHA256:
			if len(files) != 1 {
				return nil, errors.New("a unique-file parameter source requires exactly one frozen Forge file")
			}
			file := files[0]
			switch binding.Source {
			case frozen.BusinessSourceMaterialID:
				value = file.ID
			case frozen.BusinessSourceMaterialName:
				value = file.Name
			case frozen.BusinessSourceMaterialSHA256:
				value = file.SHA256
			}
		case frozen.BusinessSourceMaterialsManifest:
			if len(files) == 0 {
				return nil, errors.New("a material manifest parameter source requires at least one frozen Forge file")
			}
			manifest := make([]map[string]string, 0, len(files))
			for _, file := range files {
				manifest = append(manifest, map[string]string{"file_id": file.ID, "name": file.Name, "sha256": file.SHA256})
			}
			encoded, err := json.Marshal(manifest)
			if err != nil {
				return nil, err
			}
			value = string(encoded)
		default:
			return nil, errors.New("material parameter source is unsupported")
		}
		result[binding.Name] = value
	}
	return result, nil
}

func (d *dispatcher) ListTools(context.Context) ([]contract.ToolDef, error) {
	return append([]contract.ToolDef(nil), d.tools...), nil
}

func (d *dispatcher) Dispatch(ctx context.Context, call contract.ToolCall) (*contract.ToolResult, error) {
	selected, ok := d.byTool[call.Name]
	if !ok {
		return &contract.ToolResult{CallID: call.ID, ToolName: call.Name, Content: "业务动作不在已发布团队能力中", IsError: true}, nil
	}
	if rejected := d.bound.Validate(call); rejected != nil {
		rejected.ToolName = call.Name
		return rejected, nil
	}
	var input struct {
		RecordID string         `json:"recordId,omitempty"`
		Params   map[string]any `json:"params,omitempty"`
	}
	decoder := json.NewDecoder(strings.NewReader(call.Args))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil {
		return &contract.ToolResult{CallID: call.ID, ToolName: call.Name, Content: "业务动作参数无效", IsError: true}, nil
	}
	if d.records != nil {
		// Protected identity comes from the frozen delegation, not model output.
		input.RecordID = d.records[call.Name]
	}
	for name, value := range d.params[call.Name] {
		if _, supplied := input.Params[name]; supplied {
			return &contract.ToolResult{CallID: call.ID, ToolName: call.Name, Content: "员工固定材料字段不能由成员替换", IsError: true}, nil
		}
		if input.Params == nil {
			input.Params = map[string]any{}
		}
		input.Params[name] = value
	}
	upstream, _ := json.Marshal(map[string]any{
		"actionName": selected.actionName, "objectName": selected.objectName,
		"recordId": input.RecordID, "params": input.Params,
	})
	var outcome ActionOutcomeEvent
	if d.trackOutcomes {
		invocationID := execution.InvocationID(ctx)
		if call.ID == "" || utf8.RuneCountInString(call.ID) > 256 || invocationID == "" || d.inputRevisionID == "" ||
			utf8.RuneCountInString(selected.objectName) > 128 || utf8.RuneCountInString(input.RecordID) > 128 {
			return nil, errors.New("Forge action call is missing durable provenance")
		}
		outcome = ActionOutcomeEvent{
			Source: ActionOutcomeSourceForgeMCP, InvocationID: invocationID, CallID: call.ID,
			CapabilityID: selected.capabilityID, ActionKey: selected.objectName + "." + selected.actionName,
			ActionLabel: boundedActionOutcomeLabel(selected.label), ActionName: selected.actionName, ObjectName: selected.objectName,
			InputRevisionID: d.inputRevisionID, RecordID: input.RecordID,
		}
		outcome.FrozenRecordSHA256 = d.recordHashes[call.Name]
		replay, guardErr := checkActionOutcomeReplay(ctx, outcome)
		if guardErr != nil {
			return nil, fmt.Errorf("check prior Forge action outcome before dispatch: %w", guardErr)
		}
		if replay.Blocked {
			message := "该业务动作已有平台记录，本次没有再次调用 Forge。"
			isError := false
			switch replay.Status {
			case ActionOutcomeStatusSucceeded:
				message = "平台已确认该业务动作执行成功；本次没有再次调用 Forge。"
			case ActionOutcomeStatusFailed:
				message, isError = "平台已记录该业务动作返回失败；本次没有再次调用 Forge。", true
			default:
				message, isError = "该业务动作上一次结果仍未知，请先核对业务记录；本次没有再次调用 Forge。", true
			}
			return &contract.ToolResult{CallID: call.ID, ToolName: call.Name, Content: message, IsError: isError}, nil
		}
		outcome.Phase = "started"
		if err := recordActionOutcome(ctx, outcome); err != nil {
			if errors.Is(err, ErrActionOutcomeUnresolved) {
				return &contract.ToolResult{CallID: call.ID, ToolName: call.Name,
					Content: "同一父运行中的业务动作仍有未确认结果；当前调用没有再次发送，请先核对业务记录。", IsError: true}, nil
			}
			return nil, fmt.Errorf("persist Forge action start before dispatch: %w", err)
		}
	}
	result, err := d.host.Dispatch(ctx, contract.ToolCall{ID: call.ID, Name: "run_action", Args: string(upstream)})
	if d.trackOutcomes {
		status := ActionOutcomeStatusUnknown
		switch {
		case errors.Is(err, mcphost.ErrFailClosed):
			status = ActionOutcomeStatusFailed
		case errors.Is(err, mcphost.ErrDispatchExplicitFailure):
			status = ActionOutcomeStatusFailed
		case err != nil:
			status = ActionOutcomeStatusUnknown
		case result != nil:
			status = classifyNativeActionResult(result)
		}
		outcome.Phase, outcome.Status = "result", status
		if persistErr := recordActionOutcome(ctx, outcome); persistErr != nil {
			return nil, fmt.Errorf("persist Forge action result: %w", persistErr)
		}
		if err != nil {
			message := "Forge 业务动作返回失败。"
			if status == ActionOutcomeStatusUnknown {
				message = "Forge 业务动作的结果未知，请先核对业务记录后再继续。"
			}
			return &contract.ToolResult{CallID: call.ID, ToolName: call.Name, Content: message, IsError: true}, nil
		}
		if result == nil {
			return &contract.ToolResult{CallID: call.ID, ToolName: call.Name,
				Content: "Forge 未返回可确认的业务动作结果，请先核对业务记录后再继续。", IsError: true}, nil
		}
		if status == ActionOutcomeStatusFailed {
			result.IsError = true
		}
		if status == ActionOutcomeStatusUnknown {
			return &contract.ToolResult{CallID: call.ID, ToolName: call.Name,
				Content: "Forge 未返回可确认的业务动作结果，请先核对业务记录后再继续。", IsError: true}, nil
		}
	}
	if result != nil {
		result.ToolName = call.Name
	}
	return result, err
}

func parseAction(id string) (action, error) {
	rest := strings.TrimPrefix(id, capabilityPrefix)
	if rest == id {
		return action{}, fmt.Errorf("unsupported business capability %q", id)
	}
	objectName, actionName, ok := strings.Cut(rest, ".")
	if !ok || strings.TrimSpace(objectName) == "" || strings.TrimSpace(actionName) == "" ||
		objectName != strings.TrimSpace(objectName) || actionName != strings.TrimSpace(actionName) {
		return action{}, fmt.Errorf("invalid Forge action capability %q", id)
	}
	return action{capabilityID: id, objectName: objectName, actionName: actionName}, nil
}

func virtualToolName(value action) string {
	base := strings.ToLower(value.objectName + "_" + value.actionName)
	base = strings.Trim(toolPart.ReplaceAllString(base, "_"), "_")
	if base == "" {
		base = "action"
	}
	digest := sha256.Sum256([]byte(value.capabilityID))
	return "forge_" + base + "_" + hex.EncodeToString(digest[:4])
}

func containsAll(allowed, requested []string) bool {
	set := make(map[string]struct{}, len(allowed))
	for _, value := range allowed {
		set[value] = struct{}{}
	}
	for _, value := range requested {
		if _, ok := set[value]; !ok {
			return false
		}
	}
	return true
}

func intersectActions(memberPublished, taskAllowed []string) []string {
	published := make(map[string]struct{}, len(memberPublished))
	for _, value := range memberPublished {
		published[value] = struct{}{}
	}
	result := make([]string, 0, len(taskAllowed))
	for _, value := range taskAllowed {
		if _, ok := published[value]; ok {
			result = append(result, value)
		}
	}
	return result
}

func clear(value []byte) {
	for index := range value {
		value[index] = 0
	}
}
func clearHeader(headers map[string]string) {
	for key := range headers {
		headers[key] = ""
		delete(headers, key)
	}
}

var _ workflow.RuntimeHostFactory = Factory{}
var _ workflow.RuntimeHostFactoryWithLLM = Factory{}
var _ contract.ToolDispatcher = (*dispatcher)(nil)
