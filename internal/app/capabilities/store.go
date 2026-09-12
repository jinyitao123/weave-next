package capabilities

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const credentialPrefix = "wv_app_"

var capabilityKeyPattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,62}$`)

type Clock interface {
	Now() time.Time
}

type RealClock struct{}

func (RealClock) Now() time.Time { return time.Now() }

type Store struct {
	pool  *pgxpool.Pool
	clock Clock
}

func New(pool *pgxpool.Pool, clock Clock) *Store {
	if clock == nil {
		clock = RealClock{}
	}
	return &Store{pool: pool, clock: clock}
}

func (store *Store) Pool() *pgxpool.Pool {
	if store == nil {
		return nil
	}
	return store.pool
}

type CreateAppRequest struct {
	WorkspaceID              string
	Name                     string
	Description              string
	MaxConcurrentInvocations int
	CreatedBy                string
}

func (store *Store) CreateApp(ctx context.Context, request CreateAppRequest) (ServiceApp, error) {
	if store == nil || store.pool == nil || invalidIdentity(request.WorkspaceID) ||
		invalidLabel(request.Name, 120) || invalidIdentity(request.CreatedBy) ||
		len(request.Description) > 2000 || request.MaxConcurrentInvocations < 1 ||
		request.MaxConcurrentInvocations > 1024 {
		return ServiceApp{}, ErrInvalid
	}
	now := store.clock.Now().UTC().Truncate(time.Microsecond)
	app := ServiceApp{
		WorkspaceID: request.WorkspaceID, ID: "app-" + uuid.NewString(), Name: request.Name,
		Description: request.Description, Enabled: true,
		MaxConcurrentInvocations: request.MaxConcurrentInvocations,
		CreatedBy:                request.CreatedBy, CreatedAt: now, UpdatedAt: now,
	}
	tag, err := store.pool.Exec(ctx, `
		INSERT INTO weave_service_apps(
			workspace_id,id,name,description,enabled,max_concurrent_invocations,
			created_by,created_at,updated_at
		)
		SELECT $1,$2,$3,$4,true,$5,admin.id,$6,$6
		FROM weave_users AS admin
		WHERE admin.tenant_id=$1 AND admin.id=$7 AND admin.disabled=false
		  AND admin.role='admin'
	`, app.WorkspaceID, app.ID, app.Name, app.Description,
		app.MaxConcurrentInvocations, now, app.CreatedBy)
	if err != nil {
		return ServiceApp{}, fmt.Errorf("create service app: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return ServiceApp{}, fmt.Errorf("%w: workspace administrator is unavailable", ErrInvalid)
	}
	return app, nil
}

func (store *Store) SetAppEnabled(ctx context.Context, workspaceID, appID, updatedBy string, enabled bool) error {
	if store == nil || store.pool == nil || invalidIdentity(workspaceID) ||
		invalidIdentity(appID) || invalidIdentity(updatedBy) {
		return ErrInvalid
	}
	tag, err := store.pool.Exec(ctx, `
		UPDATE weave_service_apps AS app
		SET enabled=$4,updated_at=$5
		FROM weave_users AS admin
		WHERE app.workspace_id=$1 AND app.id=$2
		  AND admin.tenant_id=$1 AND admin.id=$3
		  AND admin.disabled=false AND admin.role='admin'
	`, workspaceID, appID, updatedBy, enabled, store.clock.Now().UTC())
	if err != nil {
		return fmt.Errorf("set service app enabled: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return ErrNotFound
	}
	return nil
}

type CreateCredentialRequest struct {
	WorkspaceID string
	AppID       string
	Name        string
	Scopes      []string
	ExpiresAt   *time.Time
	CreatedBy   string
}

func (store *Store) CreateCredential(ctx context.Context, request CreateCredentialRequest) (Credential, string, error) {
	scopes, err := normalizeScopes(request.Scopes)
	if store == nil || store.pool == nil || err != nil || invalidIdentity(request.WorkspaceID) ||
		invalidIdentity(request.AppID) || invalidLabel(request.Name, 120) ||
		invalidIdentity(request.CreatedBy) {
		return Credential{}, "", ErrInvalid
	}
	now := store.clock.Now().UTC().Truncate(time.Microsecond)
	if request.ExpiresAt != nil && !request.ExpiresAt.After(now) {
		return Credential{}, "", ErrInvalid
	}
	raw, err := generateCredential()
	if err != nil {
		return Credential{}, "", err
	}
	credential := Credential{
		WorkspaceID: request.WorkspaceID, ID: "cred-" + uuid.NewString(),
		AppID: request.AppID, Name: request.Name, Scopes: scopes,
		ExpiresAt: request.ExpiresAt, CreatedBy: request.CreatedBy, CreatedAt: now,
	}
	tag, err := store.pool.Exec(ctx, `
		INSERT INTO weave_service_app_credentials(
			workspace_id,id,app_id,name,key_hash,scopes,expires_at,created_by,created_at
		)
		SELECT app.workspace_id,$2,app.id,$3,$4,$5,$6,admin.id,$7
		FROM weave_service_apps AS app
		JOIN weave_users AS admin ON admin.tenant_id=app.workspace_id
		WHERE app.workspace_id=$1 AND app.id=$8 AND app.enabled=true
		  AND admin.id=$9 AND admin.disabled=false AND admin.role='admin'
	`, credential.WorkspaceID, credential.ID, credential.Name, hashCredential(raw),
		credential.Scopes, credential.ExpiresAt, now, credential.AppID, credential.CreatedBy)
	if err != nil {
		return Credential{}, "", fmt.Errorf("create service app credential: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return Credential{}, "", ErrNotFound
	}
	return credential, raw, nil
}

func (store *Store) ValidateCredential(ctx context.Context, raw string) (Principal, error) {
	if store == nil || store.pool == nil || !strings.HasPrefix(raw, credentialPrefix) || len(raw) != len(credentialPrefix)+64 {
		return Principal{}, ErrCredentialInvalid
	}
	var principal Principal
	var expiresAt *time.Time
	err := store.pool.QueryRow(ctx, `
		SELECT credential.workspace_id,credential.app_id,credential.id,
		       credential.scopes,credential.expires_at
		FROM weave_service_app_credentials AS credential
		JOIN weave_service_apps AS app
		  ON app.workspace_id=credential.workspace_id AND app.id=credential.app_id
		WHERE credential.key_hash=$1 AND credential.revoked_at IS NULL
		  AND app.enabled=true
	`, hashCredential(raw)).Scan(
		&principal.WorkspaceID, &principal.AppID, &principal.CredentialID,
		&principal.Scopes, &expiresAt,
	)
	if err != nil || expiresAt != nil && !expiresAt.After(store.clock.Now()) {
		return Principal{}, ErrCredentialInvalid
	}
	return principal, nil
}

func (store *Store) TouchCredential(ctx context.Context, workspaceID, credentialID string) error {
	if store == nil || store.pool == nil {
		return ErrCredentialInvalid
	}
	_, err := store.pool.Exec(ctx, `
		UPDATE weave_service_app_credentials SET last_used_at=$3
		WHERE workspace_id=$1 AND id=$2
	`, workspaceID, credentialID, store.clock.Now().UTC())
	return err
}

func (store *Store) RevokeCredential(ctx context.Context, workspaceID, credentialID, revokedBy string) error {
	if store == nil || store.pool == nil || invalidIdentity(workspaceID) ||
		invalidIdentity(credentialID) || invalidIdentity(revokedBy) {
		return ErrInvalid
	}
	tag, err := store.pool.Exec(ctx, `
		UPDATE weave_service_app_credentials AS credential
		SET revoked_at=$4
		FROM weave_users AS admin
		WHERE credential.workspace_id=$1 AND credential.id=$2
		  AND credential.revoked_at IS NULL
		  AND admin.tenant_id=$1 AND admin.id=$3
		  AND admin.disabled=false AND admin.role='admin'
	`, workspaceID, credentialID, revokedBy, store.clock.Now().UTC())
	if err != nil {
		return fmt.Errorf("revoke service app credential: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return ErrNotFound
	}
	return nil
}

type CreateCapabilityRequest struct {
	WorkspaceID string
	Key         string
	Name        string
	Description string
	CreatedBy   string
}

func (store *Store) CreateCapability(ctx context.Context, request CreateCapabilityRequest) (Capability, error) {
	if store == nil || store.pool == nil || invalidIdentity(request.WorkspaceID) ||
		!capabilityKeyPattern.MatchString(request.Key) || invalidLabel(request.Name, 120) ||
		len(request.Description) > 2000 || invalidIdentity(request.CreatedBy) {
		return Capability{}, ErrInvalid
	}
	now := store.clock.Now().UTC().Truncate(time.Microsecond)
	capability := Capability{
		WorkspaceID: request.WorkspaceID, ID: "cap-" + uuid.NewString(), Key: request.Key,
		Name: request.Name, Description: request.Description, Enabled: true,
		CreatedBy: request.CreatedBy, CreatedAt: now, UpdatedAt: now,
	}
	tag, err := store.pool.Exec(ctx, `
		INSERT INTO weave_capabilities(
			workspace_id,id,key,name,description,enabled,created_by,created_at,updated_at
		)
		SELECT $1,$2,$3,$4,$5,true,admin.id,$6,$6
		FROM weave_users AS admin
		WHERE admin.tenant_id=$1 AND admin.id=$7 AND admin.disabled=false
		  AND admin.role='admin'
	`, capability.WorkspaceID, capability.ID, capability.Key, capability.Name,
		capability.Description, now, capability.CreatedBy)
	if err != nil {
		return Capability{}, fmt.Errorf("create capability: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return Capability{}, ErrInvalid
	}
	return capability, nil
}

func generateCredential() (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generate service app credential: %w", err)
	}
	return credentialPrefix + hex.EncodeToString(raw), nil
}

func hashCredential(raw string) string {
	hash := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(hash[:])
}

func normalizeScopes(scopes []string) ([]string, error) {
	if len(scopes) == 0 || len(scopes) > 3 {
		return nil, ErrInvalid
	}
	allowed := map[string]bool{"invoke": true, "read": true, "cancel": true}
	result := append([]string(nil), scopes...)
	sort.Strings(result)
	for index, scope := range result {
		if !allowed[scope] || index > 0 && result[index-1] == scope {
			return nil, ErrInvalid
		}
	}
	return result, nil
}

func invalidIdentity(value string) bool {
	return value == "" || value != strings.TrimSpace(value) || len(value) > 200
}

func invalidLabel(value string, max int) bool {
	return value == "" || value != strings.TrimSpace(value) || len(value) > max
}

func isNoRows(err error) bool { return errors.Is(err, pgx.ErrNoRows) }
