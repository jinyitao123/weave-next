package capabilities

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const CredentialPrefix = "wv_cap_"

var ErrAccessDenied = errors.New("capability access denied")

type Application struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Enabled   bool      `json:"enabled"`
	CreatedAt time.Time `json:"created_at"`
}
type ApplicationCredential struct {
	ID        string     `json:"id"`
	AppID     string     `json:"app_id"`
	Name      string     `json:"name"`
	Scopes    []string   `json:"scopes"`
	RevokedAt *time.Time `json:"revoked_at,omitempty"`
	CreatedAt time.Time  `json:"created_at"`
}
type VersionGrant struct {
	AppID        string `json:"app_id"`
	CapabilityID string `json:"capability_id"`
	Revision     int64  `json:"revision"`
	Enabled      bool   `json:"enabled"`
}
type VersionSummary struct {
	CapabilityID string `json:"capability_id"`
	Name         string `json:"name"`
	Revision     int64  `json:"revision"`
}
type AccessSnapshot struct {
	Apps        []Application           `json:"apps"`
	Credentials []ApplicationCredential `json:"credentials"`
	Grants      []VersionGrant          `json:"grants"`
	Versions    []VersionSummary        `json:"versions"`
}
type ApplicationPrincipal struct {
	WorkspaceID  string
	AppID        string
	CredentialID string
	Scopes       []string
}

func (p ApplicationPrincipal) Allows(action string) bool { return slices.Contains(p.Scopes, action) }

type AccessStore struct{ pool *pgxpool.Pool }

func NewAccessStore(pool *pgxpool.Pool) *AccessStore { return &AccessStore{pool} }

func credentialHash(raw string) string {
	hash := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(hash[:])
}

func (s *AccessStore) Authenticate(ctx context.Context, raw string) (ApplicationPrincipal, error) {
	if s == nil || s.pool == nil || !strings.HasPrefix(raw, CredentialPrefix) {
		return ApplicationPrincipal{}, ErrAccessDenied
	}
	var p ApplicationPrincipal
	err := s.pool.QueryRow(ctx, `SELECT c.workspace_id,c.app_id,c.id,c.scopes
 FROM weave_capability_credentials c JOIN weave_capability_apps a ON a.workspace_id=c.workspace_id AND a.id=c.app_id
 WHERE c.key_hash=$1 AND c.revoked_at IS NULL AND a.enabled`, credentialHash(raw)).Scan(&p.WorkspaceID, &p.AppID, &p.CredentialID, &p.Scopes)
	if err != nil {
		return ApplicationPrincipal{}, ErrAccessDenied
	}
	return p, nil
}

func (s *AccessStore) AuthorizeInvocation(ctx context.Context, p ApplicationPrincipal, capabilityID string, revision int64) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := authorizeApplicationInvocation(ctx, tx, Invocation{WorkspaceID: p.WorkspaceID, ApplicationID: p.AppID, CredentialID: p.CredentialID, CapabilityID: capabilityID, Revision: revision}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *AccessStore) managerTx(ctx context.Context, workspace, actor string) (pgx.Tx, error) {
	if s == nil || s.pool == nil {
		return nil, ErrAccessDenied
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	var id string
	err = tx.QueryRow(ctx, `SELECT id FROM weave_users WHERE tenant_id=$1 AND id=$2 AND NOT disabled AND role IN ('admin','owner','developer') FOR SHARE`, workspace, actor).Scan(&id)
	if err != nil {
		_ = tx.Rollback(ctx)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrAccessDenied
		}
		return nil, err
	}
	return tx, nil
}

func accessEvent(ctx context.Context, tx pgx.Tx, ws, app, actor, action, resource string) error {
	_, err := tx.Exec(ctx, `INSERT INTO weave_capability_access_events(workspace_id,app_id,actor_id,action,resource_id) VALUES($1,$2,$3,$4,$5)`, ws, app, actor, action, resource)
	return err
}

func (s *AccessStore) CreateApplication(ctx context.Context, ws, actor, name string) (Application, error) {
	name = strings.TrimSpace(name)
	if name == "" || len(name) > 160 {
		return Application{}, fmt.Errorf("application name must contain 1 to 160 bytes")
	}
	tx, err := s.managerTx(ctx, ws, actor)
	if err != nil {
		return Application{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	app := Application{ID: "app_" + uuid.NewString(), Name: name, Enabled: true}
	if err := tx.QueryRow(ctx, `INSERT INTO weave_capability_apps(workspace_id,id,name,created_by) VALUES($1,$2,$3,$4) RETURNING created_at`, ws, app.ID, name, actor).Scan(&app.CreatedAt); err != nil {
		return Application{}, err
	}
	if err := accessEvent(ctx, tx, ws, app.ID, actor, "application.created", app.ID); err != nil {
		return Application{}, err
	}
	return app, tx.Commit(ctx)
}

func (s *AccessStore) SetApplicationEnabled(ctx context.Context, ws, actor, appID string, enabled bool) error {
	tx, err := s.managerTx(ctx, ws, actor)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	tag, err := tx.Exec(ctx, `UPDATE weave_capability_apps SET enabled=$3 WHERE workspace_id=$1 AND id=$2`, ws, appID, enabled)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return ErrNotFound
	}
	action := "application.disabled"
	if enabled {
		action = "application.enabled"
	}
	if err := accessEvent(ctx, tx, ws, appID, actor, action, appID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *AccessStore) IssueCredential(ctx context.Context, ws, actor, appID, name string, scopes []string) (ApplicationCredential, string, error) {
	name = strings.TrimSpace(name)
	if name == "" || len(name) > 160 || len(scopes) == 0 || len(scopes) > 3 {
		return ApplicationCredential{}, "", fmt.Errorf("credential name and 1 to 3 scopes are required")
	}
	scopes = slices.Clone(scopes)
	slices.Sort(scopes)
	for i, scope := range scopes {
		if !slices.Contains([]string{"invoke", "read", "cancel"}, scope) || (i > 0 && scopes[i-1] == scope) {
			return ApplicationCredential{}, "", fmt.Errorf("credential scopes are invalid")
		}
	}
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return ApplicationCredential{}, "", err
	}
	raw := CredentialPrefix + base64.RawURLEncoding.EncodeToString(secret)
	tx, err := s.managerTx(ctx, ws, actor)
	if err != nil {
		return ApplicationCredential{}, "", err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var app string
	if err := tx.QueryRow(ctx, `SELECT id FROM weave_capability_apps WHERE workspace_id=$1 AND id=$2 AND enabled FOR SHARE`, ws, appID).Scan(&app); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ApplicationCredential{}, "", ErrNotFound
		}
		return ApplicationCredential{}, "", err
	}
	credential := ApplicationCredential{ID: uuid.NewString(), AppID: appID, Name: name, Scopes: scopes}
	err = tx.QueryRow(ctx, `INSERT INTO weave_capability_credentials(workspace_id,app_id,id,name,key_hash,scopes,created_by) VALUES($1,$2,$3,$4,$5,$6,$7) RETURNING created_at`,
		ws, appID, credential.ID, name, credentialHash(raw), scopes, actor).Scan(&credential.CreatedAt)
	if err != nil {
		return ApplicationCredential{}, "", err
	}
	if err := accessEvent(ctx, tx, ws, appID, actor, "credential.issued", credential.ID); err != nil {
		return ApplicationCredential{}, "", err
	}
	if err := tx.Commit(ctx); err != nil {
		return ApplicationCredential{}, "", err
	}
	return credential, raw, nil
}

func (s *AccessStore) RevokeCredential(ctx context.Context, ws, actor, appID, credentialID string) error {
	tx, err := s.managerTx(ctx, ws, actor)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	tag, err := tx.Exec(ctx, `UPDATE weave_capability_credentials SET revoked_at=COALESCE(revoked_at,now()) WHERE workspace_id=$1 AND app_id=$2 AND id=$3`, ws, appID, credentialID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return ErrNotFound
	}
	if err := accessEvent(ctx, tx, ws, appID, actor, "credential.revoked", credentialID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *AccessStore) SetGrant(ctx context.Context, ws, actor string, grant VersionGrant) error {
	if grant.Revision < 1 {
		return fmt.Errorf("positive published revision required")
	}
	tx, err := s.managerTx(ctx, ws, actor)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var id string
	if err := tx.QueryRow(ctx, `SELECT id FROM weave_capability_apps WHERE workspace_id=$1 AND id=$2 FOR SHARE`, ws, grant.AppID).Scan(&id); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		return err
	}
	var revision int64
	if err := tx.QueryRow(ctx, `SELECT revision FROM weave_capability_revisions WHERE workspace_id=$1 AND capability_id=$2 AND revision=$3`, ws, grant.CapabilityID, grant.Revision).Scan(&revision); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrRevisionNotFound
		}
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO weave_capability_grants(workspace_id,app_id,capability_id,revision,enabled,updated_by)
 VALUES($1,$2,$3,$4,$5,$6) ON CONFLICT(workspace_id,app_id,capability_id,revision)
 DO UPDATE SET enabled=EXCLUDED.enabled,updated_by=EXCLUDED.updated_by,updated_at=now()`, ws, grant.AppID, grant.CapabilityID, grant.Revision, grant.Enabled, actor)
	if err != nil {
		return err
	}
	action := "grant.revoked"
	if grant.Enabled {
		action = "grant.enabled"
	}
	if err := accessEvent(ctx, tx, ws, grant.AppID, actor, action, fmt.Sprintf("%s/%d", grant.CapabilityID, grant.Revision)); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *AccessStore) Snapshot(ctx context.Context, ws, actor string) (AccessSnapshot, error) {
	tx, err := s.managerTx(ctx, ws, actor)
	if err != nil {
		return AccessSnapshot{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	result := AccessSnapshot{Apps: []Application{}, Credentials: []ApplicationCredential{}, Grants: []VersionGrant{}, Versions: []VersionSummary{}}
	rows, err := tx.Query(ctx, `SELECT id,name,enabled,created_at FROM weave_capability_apps WHERE workspace_id=$1 ORDER BY created_at,id`, ws)
	if err != nil {
		return result, err
	}
	for rows.Next() {
		var app Application
		if err := rows.Scan(&app.ID, &app.Name, &app.Enabled, &app.CreatedAt); err != nil {
			rows.Close()
			return result, err
		}
		result.Apps = append(result.Apps, app)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return result, err
	}
	rows, err = tx.Query(ctx, `SELECT id,app_id,name,scopes,revoked_at,created_at FROM weave_capability_credentials WHERE workspace_id=$1 ORDER BY created_at,id`, ws)
	if err != nil {
		return result, err
	}
	for rows.Next() {
		var c ApplicationCredential
		if err := rows.Scan(&c.ID, &c.AppID, &c.Name, &c.Scopes, &c.RevokedAt, &c.CreatedAt); err != nil {
			rows.Close()
			return result, err
		}
		result.Credentials = append(result.Credentials, c)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return result, err
	}
	rows, err = tx.Query(ctx, `SELECT app_id,capability_id,revision,enabled FROM weave_capability_grants WHERE workspace_id=$1 ORDER BY app_id,capability_id,revision`, ws)
	if err != nil {
		return result, err
	}
	for rows.Next() {
		var g VersionGrant
		if err := rows.Scan(&g.AppID, &g.CapabilityID, &g.Revision, &g.Enabled); err != nil {
			rows.Close()
			return result, err
		}
		result.Grants = append(result.Grants, g)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return result, err
	}
	rows, err = tx.Query(ctx, `SELECT capability_id,definition->>'name',revision FROM weave_capability_revisions WHERE workspace_id=$1 ORDER BY capability_id,revision DESC`, ws)
	if err != nil {
		return result, err
	}
	for rows.Next() {
		var v VersionSummary
		if err := rows.Scan(&v.CapabilityID, &v.Name, &v.Revision); err != nil {
			rows.Close()
			return result, err
		}
		result.Versions = append(result.Versions, v)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return result, err
	}
	return result, tx.Commit(ctx)
}

// Admission locks the app, credential and grant with the insertion transaction.
// Rotating credentials changes authentication, not invocation ownership.
func authorizeApplicationInvocation(ctx context.Context, tx pgx.Tx, i Invocation) error {
	var app string
	err := tx.QueryRow(ctx, `SELECT id FROM weave_capability_apps WHERE workspace_id=$1 AND id=$2 AND enabled FOR SHARE`, i.WorkspaceID, i.ApplicationID).Scan(&app)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrAccessDenied
		}
		return err
	}
	var credential string
	err = tx.QueryRow(ctx, `SELECT id FROM weave_capability_credentials WHERE workspace_id=$1 AND app_id=$2 AND id=$3 AND revoked_at IS NULL AND 'invoke'=ANY(scopes) FOR SHARE`,
		i.WorkspaceID, i.ApplicationID, i.CredentialID).Scan(&credential)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrAccessDenied
		}
		return err
	}
	return authorizeApplicationGrant(ctx, tx, i)
}

func authorizeApplicationGrant(ctx context.Context, tx pgx.Tx, i Invocation) error {
	var app string
	if err := tx.QueryRow(ctx, `SELECT id FROM weave_capability_apps WHERE workspace_id=$1 AND id=$2 AND enabled FOR SHARE`, i.WorkspaceID, i.ApplicationID).Scan(&app); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrAccessDenied
		}
		return err
	}
	var granted bool
	err := tx.QueryRow(ctx, `SELECT enabled FROM weave_capability_grants WHERE workspace_id=$1 AND app_id=$2 AND capability_id=$3 AND revision=$4 AND enabled FOR SHARE`,
		i.WorkspaceID, i.ApplicationID, i.CapabilityID, i.Revision).Scan(&granted)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrAccessDenied
		}
		return err
	}
	return nil
}
