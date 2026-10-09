package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jinyitao123/weave/internal/kernel/secret"
	"github.com/labstack/echo/v4"
)

// A workspace's own Feishu app, entered in the admin console. Each app_id
// belongs to one workspace, so the app_id-scoped transport queries stay inside
// it. Clients are cached by workspace and revision: a saved change is picked up
// on the next use on every instance, and the tenant token survives between uses.
type feishuAppCache struct {
	mu      sync.Mutex
	clients map[string]cachedFeishuApp
}

type cachedFeishuApp struct {
	revision int64
	client   *feishuClient
}

type storedFeishuApp struct {
	Workspace, AppID, TenantKey, SecretSealed, TokenSealed, KeySealed, CallbackKey string
	Revision                                                                      int64
}

const storedFeishuColumns = `workspace_id,app_id,tenant_key,app_secret_sealed,verification_token_sealed,encrypt_key_sealed,callback_key,revision`

func scanStoredFeishu(row pgx.Row) (storedFeishuApp, error) {
	var app storedFeishuApp
	err := row.Scan(&app.Workspace, &app.AppID, &app.TenantKey, &app.SecretSealed, &app.TokenSealed, &app.KeySealed, &app.CallbackKey, &app.Revision)
	return app, err
}

var errFeishuCredentialKey = errors.New("server credential key is not configured")

func (s *Server) openStoredFeishu(app storedFeishuApp) (*feishuClient, error) {
	cache := &s.feishuApps
	cache.mu.Lock()
	defer cache.mu.Unlock()
	if cached, ok := cache.clients[app.Workspace]; ok && cached.revision == app.Revision && cached.client.appID == app.AppID {
		return cached.client, nil
	}
	key, err := secret.KeyFromEnv()
	if err != nil {
		return nil, errFeishuCredentialKey
	}
	values := make([]string, 0, 3)
	for _, sealed := range []string{app.SecretSealed, app.TokenSealed, app.KeySealed} {
		plain, err := secret.Open(key, sealed)
		if err != nil {
			return nil, errors.New("feishu credential unreadable")
		}
		values = append(values, string(plain))
	}
	client := newFeishuClient(app.Workspace, app.AppID, values[0], values[1], values[2], app.TenantKey)
	if cache.clients == nil {
		cache.clients = map[string]cachedFeishuApp{}
	}
	cache.clients[app.Workspace] = cachedFeishuApp{revision: app.Revision, client: client}
	return client, nil
}

// feishuForWorkspace returns the workspace's own app, else the deployment app,
// else nil.
func (s *Server) feishuForWorkspace(ctx context.Context, workspace string) (*feishuClient, error) {
	pool := s.GetPool()
	if pool == nil {
		return nil, nil
	}
	app, err := scanStoredFeishu(pool.QueryRow(ctx, `SELECT `+storedFeishuColumns+` FROM weave_feishu_apps WHERE workspace_id=$1`, workspace))
	if errors.Is(err, pgx.ErrNoRows) {
		if s.Feishu.configured() {
			return s.Feishu, nil
		}
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return s.openStoredFeishu(app)
}

// feishuClients lists every app whose queues the employee event worker drains.
// An app that cannot be opened is reported without blocking the others.
func (s *Server) feishuClients(ctx context.Context) ([]*feishuClient, error) {
	clients := []*feishuClient{}
	if s.Feishu.configured() {
		clients = append(clients, s.Feishu)
	}
	pool := s.GetPool()
	if pool == nil {
		return clients, nil
	}
	rows, err := pool.Query(ctx, `SELECT `+storedFeishuColumns+` FROM weave_feishu_apps ORDER BY workspace_id`)
	if err != nil {
		return clients, err
	}
	apps, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (storedFeishuApp, error) { return scanStoredFeishu(row) })
	if err != nil {
		return clients, err
	}
	var failures []error
	for _, app := range apps {
		client, err := s.openStoredFeishu(app)
		if err != nil {
			failures = append(failures, err)
			continue
		}
		clients = append(clients, client)
	}
	return clients, errors.Join(failures...)
}

func (s *Server) handleFeishuWorkspaceEvent(c echo.Context) error {
	pool := s.GetPool()
	key := c.Param("callback")
	if pool == nil || !feishuCallbackKey.MatchString(key) {
		return c.NoContent(http.StatusNotFound)
	}
	app, err := scanStoredFeishu(pool.QueryRow(c.Request().Context(), `SELECT `+storedFeishuColumns+` FROM weave_feishu_apps WHERE callback_key=$1`, key))
	if errors.Is(err, pgx.ErrNoRows) {
		return c.NoContent(http.StatusNotFound)
	}
	if err != nil {
		return c.NoContent(http.StatusServiceUnavailable)
	}
	client, err := s.openStoredFeishu(app)
	if err != nil {
		return c.NoContent(http.StatusServiceUnavailable)
	}
	return s.receiveFeishuEvent(c, client)
}

var (
	feishuCallbackKey = regexp.MustCompile(`^[0-9a-f]{32}$`)
	feishuIdentifier  = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)
)

type feishuAppSecrets struct {
	AppSecret         bool `json:"appSecret"`
	VerificationToken bool `json:"verificationToken"`
	EncryptKey        bool `json:"encryptKey"`
}

type feishuAppCheck struct {
	At     time.Time `json:"at"`
	OK     bool      `json:"ok"`
	Reason string    `json:"reason,omitempty"`
}

type feishuAppView struct {
	Source         string           `json:"source"`
	AppID          string           `json:"appId,omitempty"`
	TenantKey      string           `json:"tenantKey,omitempty"`
	Secrets        feishuAppSecrets `json:"secrets"`
	CallbackPath   string           `json:"callbackPath,omitempty"`
	Revision       int64            `json:"revision"`
	UpdatedAt      *time.Time       `json:"updatedAt,omitempty"`
	UpdatedBy      string           `json:"updatedBy,omitempty"`
	LastCheck      *feishuAppCheck  `json:"lastCheck,omitempty"`
	BoundEmployees int              `json:"boundEmployees"`
}

const feishuDeploymentCallback = "/v1/integrations/feishu/events"

func (s *Server) feishuAppView(ctx context.Context, workspace string) (feishuAppView, error) {
	view := feishuAppView{Source: "none"}
	var updatedAt time.Time
	var checkAt *time.Time
	var checkOK *bool
	var checkReason *string
	err := s.GetPool().QueryRow(ctx, `SELECT app.app_id,app.tenant_key,app.callback_key,app.revision,app.updated_at,
  COALESCE(NULLIF(u.display_name,''),u.username,''),app.last_check_at,app.last_check_ok,app.last_check_reason
 FROM weave_feishu_apps app LEFT JOIN weave_users u ON u.id=app.updated_by WHERE app.workspace_id=$1`, workspace).Scan(
		&view.AppID, &view.TenantKey, &view.CallbackPath, &view.Revision, &updatedAt, &view.UpdatedBy, &checkAt, &checkOK, &checkReason)
	switch {
	case err == nil:
		view.Source, view.Secrets = "workspace", feishuAppSecrets{true, true, true}
		view.CallbackPath, view.UpdatedAt = feishuDeploymentCallback+"/"+view.CallbackPath, &updatedAt
		if checkAt != nil && checkOK != nil {
			view.LastCheck = &feishuAppCheck{At: *checkAt, OK: *checkOK}
			if checkReason != nil {
				view.LastCheck.Reason = *checkReason
			}
		}
	case errors.Is(err, pgx.ErrNoRows):
		if !s.Feishu.configured() {
			return view, nil
		}
		view.Source, view.AppID, view.TenantKey, view.CallbackPath = "deployment", s.Feishu.appID, s.Feishu.tenantKey, feishuDeploymentCallback
		view.Secrets = feishuAppSecrets{true, true, true}
	default:
		return view, err
	}
	err = s.GetPool().QueryRow(ctx, `SELECT count(*) FROM weave_feishu_links WHERE app_id=$1 AND workspace_id=$2 AND open_id IS NOT NULL AND expires_at>statement_timestamp()`,
		view.AppID, workspace).Scan(&view.BoundEmployees)
	return view, err
}

func (s *Server) handleGetFeishuApp(c echo.Context) error {
	if s.GetPool() == nil {
		return c.NoContent(http.StatusServiceUnavailable)
	}
	view, err := s.feishuAppView(c.Request().Context(), getTenant(c))
	if err != nil {
		return c.NoContent(http.StatusServiceUnavailable)
	}
	c.Response().Header().Set("Cache-Control", "no-store")
	return c.JSON(http.StatusOK, view)
}

type feishuAppInput struct {
	ExpectedRevision  int64   `json:"expectedRevision"`
	AppID             string  `json:"appId"`
	TenantKey         string  `json:"tenantKey"`
	AppSecret         *string `json:"appSecret"`
	VerificationToken *string `json:"verificationToken"`
	EncryptKey        *string `json:"encryptKey"`
}

func feishuSecretValid(value *string) bool {
	return value == nil || (*value != "" && len(*value) <= 512 && !strings.ContainsAny(*value, " \t\r\n"))
}

func feishuError(c echo.Context, status int, message string) error {
	return c.JSON(status, map[string]string{"error": message})
}

func (s *Server) handlePutFeishuApp(c echo.Context) error {
	pool := s.GetPool()
	if pool == nil {
		return c.NoContent(http.StatusServiceUnavailable)
	}
	var input feishuAppInput
	if err := c.Bind(&input); err != nil {
		return feishuError(c, http.StatusBadRequest, "请求格式不正确")
	}
	input.AppID, input.TenantKey = strings.TrimSpace(input.AppID), strings.TrimSpace(input.TenantKey)
	if !feishuIdentifier.MatchString(input.AppID) || !feishuIdentifier.MatchString(input.TenantKey) || input.ExpectedRevision < 0 ||
		!feishuSecretValid(input.AppSecret) || !feishuSecretValid(input.VerificationToken) || !feishuSecretValid(input.EncryptKey) {
		return feishuError(c, http.StatusBadRequest, "应用标识、租户标识或密钥格式不正确")
	}
	if s.Feishu != nil && s.Feishu.appID == input.AppID {
		return feishuError(c, http.StatusConflict, "该飞书应用已由部署环境使用")
	}
	ctx, workspace := c.Request().Context(), getTenant(c)
	tx, err := pool.Begin(ctx)
	if err != nil {
		return c.NoContent(http.StatusServiceUnavailable)
	}
	defer tx.Rollback(ctx)
	current, err := scanStoredFeishu(tx.QueryRow(ctx, `SELECT `+storedFeishuColumns+` FROM weave_feishu_apps WHERE workspace_id=$1 FOR UPDATE`, workspace))
	exists := err == nil
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return c.NoContent(http.StatusServiceUnavailable)
	}
	if (exists && input.ExpectedRevision != current.Revision) || (!exists && input.ExpectedRevision != 0) {
		return feishuError(c, http.StatusConflict, "飞书配置已被修改，请刷新后再保存")
	}
	replacing := !exists || current.AppID != input.AppID
	if replacing && (input.AppSecret == nil || input.VerificationToken == nil || input.EncryptKey == nil) {
		return feishuError(c, http.StatusUnprocessableEntity, "首次保存或更换应用时需要填写全部三项密钥")
	}
	fields := []string{}
	if !exists || current.AppID != input.AppID {
		fields = append(fields, "appId")
	}
	if !exists || current.TenantKey != input.TenantKey {
		fields = append(fields, "tenantKey")
	}
	sealed := map[string]string{"appSecret": current.SecretSealed, "verificationToken": current.TokenSealed, "encryptKey": current.KeySealed}
	plain := map[string]*string{"appSecret": input.AppSecret, "verificationToken": input.VerificationToken, "encryptKey": input.EncryptKey}
	if input.AppSecret != nil || input.VerificationToken != nil || input.EncryptKey != nil {
		key, err := secret.KeyFromEnv()
		if err != nil {
			return feishuError(c, http.StatusServiceUnavailable, "服务端未配置凭据加密密钥，不能保存飞书密钥")
		}
		for _, name := range []string{"appSecret", "verificationToken", "encryptKey"} {
			if plain[name] == nil {
				continue
			}
			if sealed[name], err = secret.Seal(key, []byte(*plain[name])); err != nil {
				return c.NoContent(http.StatusInternalServerError)
			}
			fields = append(fields, name)
		}
	}
	actor := getUserID(c)
	revision := int64(1)
	if exists {
		revision = current.Revision + 1
		_, err = tx.Exec(ctx, `UPDATE weave_feishu_apps SET app_id=$2,tenant_key=$3,app_secret_sealed=$4,verification_token_sealed=$5,encrypt_key_sealed=$6,
  revision=$7,updated_by=$8,updated_at=now(),last_check_at=CASE WHEN $9 THEN NULL ELSE last_check_at END,
  last_check_ok=CASE WHEN $9 THEN NULL ELSE last_check_ok END,last_check_reason=CASE WHEN $9 THEN NULL ELSE last_check_reason END
 WHERE workspace_id=$1`, workspace, input.AppID, input.TenantKey, sealed["appSecret"], sealed["verificationToken"], sealed["encryptKey"], revision, actor, len(fields) > 0)
	} else {
		raw := make([]byte, 16)
		if _, err = rand.Read(raw); err != nil {
			return c.NoContent(http.StatusServiceUnavailable)
		}
		_, err = tx.Exec(ctx, `INSERT INTO weave_feishu_apps(workspace_id,app_id,tenant_key,app_secret_sealed,verification_token_sealed,encrypt_key_sealed,callback_key,revision,updated_by)
 VALUES($1,$2,$3,$4,$5,$6,$7,1,$8)`, workspace, input.AppID, input.TenantKey, sealed["appSecret"], sealed["verificationToken"], sealed["encryptKey"], hex.EncodeToString(raw), actor)
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return feishuError(c, http.StatusConflict, "该飞书应用已被其他工作区使用")
	}
	if err != nil {
		return c.NoContent(http.StatusServiceUnavailable)
	}
	// Bindings made under a replaced app cannot reach the new one.
	if exists && current.AppID != input.AppID {
		if _, err = tx.Exec(ctx, `DELETE FROM weave_feishu_links WHERE app_id=$1 AND workspace_id=$2`, current.AppID, workspace); err != nil {
			return c.NoContent(http.StatusServiceUnavailable)
		}
	}
	changed, _ := json.Marshal(fields)
	if _, err = tx.Exec(ctx, `INSERT INTO weave_feishu_app_audit(workspace_id,actor,action,revision,fields) VALUES($1,$2,'save',$3,$4::jsonb)`, workspace, actor, revision, string(changed)); err != nil {
		return c.NoContent(http.StatusServiceUnavailable)
	}
	if err = tx.Commit(ctx); err != nil {
		return c.NoContent(http.StatusServiceUnavailable)
	}
	return s.handleGetFeishuApp(c)
}

func (s *Server) handleDeleteFeishuApp(c echo.Context) error {
	pool := s.GetPool()
	if pool == nil {
		return c.NoContent(http.StatusServiceUnavailable)
	}
	var input struct {
		ExpectedRevision int64 `json:"expectedRevision"`
	}
	if err := c.Bind(&input); err != nil {
		return feishuError(c, http.StatusBadRequest, "请求格式不正确")
	}
	ctx, workspace := c.Request().Context(), getTenant(c)
	tx, err := pool.Begin(ctx)
	if err != nil {
		return c.NoContent(http.StatusServiceUnavailable)
	}
	defer tx.Rollback(ctx)
	current, err := scanStoredFeishu(tx.QueryRow(ctx, `SELECT `+storedFeishuColumns+` FROM weave_feishu_apps WHERE workspace_id=$1 FOR UPDATE`, workspace))
	if errors.Is(err, pgx.ErrNoRows) {
		return feishuError(c, http.StatusNotFound, "本工作区没有保存飞书应用")
	}
	if err != nil {
		return c.NoContent(http.StatusServiceUnavailable)
	}
	if input.ExpectedRevision != current.Revision {
		return feishuError(c, http.StatusConflict, "飞书配置已被修改，请刷新后再删除")
	}
	// Accepted work keeps running; undelivered notices stay pending.
	if _, err = tx.Exec(ctx, `DELETE FROM weave_feishu_apps WHERE workspace_id=$1`, workspace); err != nil {
		return c.NoContent(http.StatusServiceUnavailable)
	}
	if _, err = tx.Exec(ctx, `DELETE FROM weave_feishu_links WHERE app_id=$1 AND workspace_id=$2`, current.AppID, workspace); err != nil {
		return c.NoContent(http.StatusServiceUnavailable)
	}
	if _, err = tx.Exec(ctx, `INSERT INTO weave_feishu_app_audit(workspace_id,actor,action,revision) VALUES($1,$2,'delete',$3)`, workspace, getUserID(c), current.Revision); err != nil {
		return c.NoContent(http.StatusServiceUnavailable)
	}
	if err = tx.Commit(ctx); err != nil {
		return c.NoContent(http.StatusServiceUnavailable)
	}
	return s.handleGetFeishuApp(c)
}

// feishuCheckReason classifies a token request without keeping provider text.
func feishuCheckReason(err error) string {
	if err == nil {
		return ""
	}
	if strings.HasPrefix(err.Error(), "feishu API code") {
		return "credentials_rejected"
	}
	return "unreachable"
}

func (s *Server) handleCheckFeishuApp(c echo.Context) error {
	pool := s.GetPool()
	if pool == nil {
		return c.NoContent(http.StatusServiceUnavailable)
	}
	ctx, workspace := c.Request().Context(), getTenant(c)
	app, err := scanStoredFeishu(pool.QueryRow(ctx, `SELECT `+storedFeishuColumns+` FROM weave_feishu_apps WHERE workspace_id=$1`, workspace))
	if errors.Is(err, pgx.ErrNoRows) {
		return c.JSON(http.StatusOK, map[string]any{"ok": false, "reason": "not_configured"})
	}
	if err != nil {
		return c.NoContent(http.StatusServiceUnavailable)
	}
	cached, err := s.openStoredFeishu(app)
	if errors.Is(err, errFeishuCredentialKey) {
		return feishuError(c, http.StatusServiceUnavailable, "服务端未配置凭据加密密钥，不能读取飞书密钥")
	}
	if err != nil {
		return c.NoContent(http.StatusServiceUnavailable)
	}
	// A fresh client, so a cached token cannot hide revoked credentials.
	_, err = newFeishuClient(cached.workspace, cached.appID, cached.appSecret, cached.verificationToken, cached.encryptKey, cached.tenantKey).tenantToken(ctx)
	reason := feishuCheckReason(err)
	if _, dbErr := pool.Exec(ctx, `UPDATE weave_feishu_apps SET last_check_at=now(),last_check_ok=$2,last_check_reason=NULLIF($3,'') WHERE workspace_id=$1 AND revision=$4`,
		workspace, err == nil, reason, app.Revision); dbErr != nil {
		return c.NoContent(http.StatusServiceUnavailable)
	}
	if _, dbErr := pool.Exec(ctx, `INSERT INTO weave_feishu_app_audit(workspace_id,actor,action,revision,outcome) VALUES($1,$2,'check',$3,$4)`,
		workspace, getUserID(c), app.Revision, map[bool]string{true: "ok", false: reason}[err == nil]); dbErr != nil {
		return c.NoContent(http.StatusServiceUnavailable)
	}
	status := http.StatusOK
	if reason == "unreachable" {
		status = http.StatusServiceUnavailable
	}
	return c.JSON(status, map[string]any{"ok": err == nil, "reason": reason})
}
