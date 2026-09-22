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
	Inner workflow.RuntimeHostFactory
	Store *Store
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
	if err != nil || len(bundle.Agent.BusinessCapabilityIDs) == 0 {
		return opts, closer, err
	}
	if f.Store == nil {
		if closer != nil {
			_ = closer.Close()
		}
		return compiler.FrozenBuildOpts{}, nil, fmt.Errorf("%w: business delegation store unavailable", mcphost.ErrFailClosed)
	}
	dispatcher, err := f.Store.dispatcher(ctx, bundle.Agent.BusinessCapabilityIDs)
	if err != nil {
		if closer != nil {
			_ = closer.Close()
		}
		return compiler.FrozenBuildOpts{}, nil, err
	}
	if dispatcher == nil {
		return opts, closer, nil
	}
	opts.Tools = mcphost.NewCompositeDispatcher(opts.Tools, dispatcher)
	return opts, closer, nil
}

type delegation struct {
	inputRevisionID string
	issuer          string
	token           []byte
	actions         []string
}

func (s *Store) dispatcher(ctx context.Context, requested []string) (contract.ToolDispatcher, error) {
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
	toolContract, err := mcphost.NewToolContract([]contract.ToolDef{runAction})
	if err != nil {
		clearHeader(headers)
		return nil, err
	}
	host := mcphost.NewHTTPHost(endpoint.String(), mcphost.WithHeaders(headers), mcphost.WithFilter([]string{"run_action"}), mcphost.WithToolContract(toolContract),
		mcphost.WithDispatchGuard(func(callCtx context.Context) error {
			return s.validate(callCtx, bound.inputRevisionID, bound.actions)
		}))
	clearHeader(headers)
	return newDispatcher(host, bound.actions)
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
	var actionsRaw []byte
	var expiresAt time.Time
	err = tx.QueryRow(ctx, `SELECT d.input_revision_id,d.issuer,d.credential_ciphertext,d.credential_sha256,d.allowed_actions,d.expires_at
		FROM weave_task_queue q
		JOIN weave_run_delivery_state r ON r.workspace_id=q.workspace_id AND r.run_snapshot_id=q.run_snapshot_id
		JOIN weave_task_business_delegations d ON d.workspace_id=r.workspace_id AND d.input_revision_id=r.input_revision_id
		WHERE q.workspace_id=$1 AND q.id=$2 AND d.user_id=$3 AND d.revoked_at IS NULL`,
		current.WorkspaceID, current.ID, current.Subject.UserID).Scan(&inputRevisionID, &issuer, &ciphertext, &digest, &actionsRaw, &expiresAt)
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
	return delegation{inputRevisionID: inputRevisionID, issuer: issuer, token: token, actions: allowed}, nil
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
	host   contract.ToolDispatcher
	tools  []contract.ToolDef
	byTool map[string]action
}

type action struct{ capabilityID, objectName, actionName string }

func newDispatcher(host contract.ToolDispatcher, ids []string) (*dispatcher, error) {
	d := &dispatcher{host: host, byTool: map[string]action{}}
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
		d.byTool[name] = parsed
		d.tools = append(d.tools, contract.ToolDef{
			Name:        name,
			Description: fmt.Sprintf("执行已发布的 Forge 业务动作 %s（对象 %s）。仅在当前员工明确授权且业务材料已核对时调用。", parsed.actionName, parsed.objectName),
			InputSchema: json.RawMessage(`{"type":"object","properties":{"recordId":{"type":"string","description":"目标业务记录"},"params":{"type":"object","description":"动作声明的业务参数"}},"additionalProperties":false}`),
		})
	}
	return d, nil
}

func (d *dispatcher) ListTools(context.Context) ([]contract.ToolDef, error) {
	return append([]contract.ToolDef(nil), d.tools...), nil
}

func (d *dispatcher) Dispatch(ctx context.Context, call contract.ToolCall) (*contract.ToolResult, error) {
	selected, ok := d.byTool[call.Name]
	if !ok {
		return &contract.ToolResult{CallID: call.ID, ToolName: call.Name, Content: "业务动作不在已发布团队能力中", IsError: true}, nil
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
	upstream, _ := json.Marshal(map[string]any{
		"actionName": selected.actionName, "objectName": selected.objectName,
		"recordId": input.RecordID, "params": input.Params,
	})
	result, err := d.host.Dispatch(ctx, contract.ToolCall{ID: call.ID, Name: "run_action", Args: string(upstream)})
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
