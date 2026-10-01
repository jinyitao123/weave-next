package users

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/crypto/bcrypt"
)

const bcryptCost = 12

// User represents a registered human user.
type User struct {
	ID          string    `json:"id"`
	TenantID    string    `json:"tenant_id"`
	Username    string    `json:"username"`
	DisplayName string    `json:"display_name,omitempty"`
	Role        string    `json:"role"`
	Disabled    bool      `json:"disabled"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// Store provides CRUD operations on the weave_users table.
type Store struct {
	pool *pgxpool.Pool
}

// NewStore creates a new user store.
func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

// Create inserts a new user with a bcrypt-hashed password.
func (s *Store) Create(ctx context.Context, tenantID, username, password, displayName, role string) (*User, error) {
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcryptCost)
	if err != nil {
		return nil, fmt.Errorf("hash password: %w", err)
	}

	id := uuid.NewString()
	now := time.Now()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("create user: %w", err)
	}
	defer tx.Rollback(ctx)

	if _, err = tx.Exec(ctx, `
		INSERT INTO weave_workspaces (id, slug, name)
		VALUES ($1, $1, $1)
		ON CONFLICT (id) DO NOTHING
	`, tenantID); err != nil {
		return nil, fmt.Errorf("create user: %w", err)
	}
	_, err = tx.Exec(ctx,
		`INSERT INTO weave_users (id, tenant_id, username, password, display_name, role, created_at, updated_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $7)`,
		id, tenantID, username, string(hash), displayName, role, now)
	if err != nil {
		return nil, fmt.Errorf("create user: %w", err)
	}
	if _, err = tx.Exec(ctx,
		`INSERT INTO weave_members (workspace_id, user_id, role) VALUES ($1, $2, $3)`,
		tenantID, id, memberRole(role)); err != nil {
		return nil, fmt.Errorf("create user: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("create user: %w", err)
	}

	return &User{
		ID: id, TenantID: tenantID, Username: username,
		DisplayName: displayName, Role: role,
		CreatedAt: now, UpdatedAt: now,
	}, nil
}

// Authenticate verifies username+password and returns the user if valid.
func (s *Store) Authenticate(ctx context.Context, tenantID, username, password string) (*User, error) {
	var u User
	var hash string
	err := s.pool.QueryRow(ctx,
		`SELECT id, tenant_id, username, password, display_name, role, disabled, created_at, updated_at
		 FROM weave_users WHERE tenant_id=$1 AND username=$2`, tenantID, username,
	).Scan(&u.ID, &u.TenantID, &u.Username, &hash, &u.DisplayName, &u.Role, &u.Disabled, &u.CreatedAt, &u.UpdatedAt)
	if err != nil {
		return nil, fmt.Errorf("user not found")
	}
	if u.Disabled {
		return nil, fmt.Errorf("user is disabled")
	}
	if err := bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)); err != nil {
		return nil, fmt.Errorf("invalid password")
	}
	return &u, nil
}

// GetByID retrieves a user by tenant and ID.
func (s *Store) GetByID(ctx context.Context, tenantID, id string) (*User, error) {
	var u User
	err := s.pool.QueryRow(ctx,
		`SELECT id, tenant_id, username, display_name, role, disabled, created_at, updated_at
		 FROM weave_users WHERE id=$1 AND tenant_id=$2`, id, tenantID,
	).Scan(&u.ID, &u.TenantID, &u.Username, &u.DisplayName, &u.Role, &u.Disabled, &u.CreatedAt, &u.UpdatedAt)
	if err != nil {
		return nil, fmt.Errorf("user %q not found", id)
	}
	return &u, nil
}

// GetByUsername retrieves a user by tenant + username.
func (s *Store) GetByUsername(ctx context.Context, tenantID, username string) (*User, error) {
	var u User
	err := s.pool.QueryRow(ctx,
		`SELECT id, tenant_id, username, display_name, role, disabled, created_at, updated_at
		 FROM weave_users WHERE tenant_id=$1 AND username=$2`, tenantID, username,
	).Scan(&u.ID, &u.TenantID, &u.Username, &u.DisplayName, &u.Role, &u.Disabled, &u.CreatedAt, &u.UpdatedAt)
	if err != nil {
		return nil, fmt.Errorf("user %q not found", username)
	}
	return &u, nil
}

// BindExternal returns the existing Weave user for one trusted external
// identity or creates a disabled-password compatibility row on first login.
// Product access for Forge-bound sessions comes from the verified Forge
// permission claim; this row supplies only stable binding and disabled state.
func (s *Store) BindExternal(ctx context.Context, issuer, subject, tenantID, email, displayName string) (*User, error) {
	return s.bindExternal(ctx, issuer, subject, tenantID, email, displayName, "")
}

// BindExternalInOrganization is called only after a trusted native session
// proves this subject and active organization. It never changes workspace or
// user identity when filling an old, previously empty organization binding.
func (s *Store) BindExternalInOrganization(ctx context.Context, issuer, subject, tenantID, email, displayName, nativeOrganization string) (*User, error) {
	if strings.TrimSpace(nativeOrganization) == "" {
		return nil, errors.New("native organization is required")
	}
	return s.bindExternal(ctx, issuer, subject, tenantID, email, displayName, nativeOrganization)
}

func (s *Store) bindExternal(ctx context.Context, issuer, subject, tenantID, email, displayName, nativeOrganization string) (*User, error) {
	issuer = strings.TrimSpace(issuer)
	subject = strings.TrimSpace(subject)
	tenantID = strings.TrimSpace(tenantID)
	if issuer == "" || subject == "" || tenantID == "" {
		return nil, fmt.Errorf("external identity is incomplete")
	}
	digest := sha256.Sum256([]byte(issuer + "\x00" + subject + "\x00" + tenantID))
	lockKey := int64(binary.BigEndian.Uint64(digest[:8]))
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("bind external identity: %w", err)
	}
	defer tx.Rollback(ctx)
	if nativeOrganization != "" {
		orgDigest := sha256.Sum256([]byte(issuer + "\x00" + tenantID))
		if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, int64(binary.BigEndian.Uint64(orgDigest[:8]))); err != nil {
			return nil, err
		}
		var mismatch bool
		if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM weave_external_identities WHERE issuer=$1 AND workspace_id=$2 AND native_organization<>'' AND native_organization<>$3)`, issuer, tenantID, nativeOrganization).Scan(&mismatch); err != nil {
			return nil, err
		}
		if mismatch {
			return nil, errors.New("native organization differs from workspace binding")
		}
	}
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, lockKey); err != nil {
		return nil, fmt.Errorf("bind external identity: %w", err)
	}
	var user User
	err = tx.QueryRow(ctx, `SELECT u.id,u.tenant_id,u.username,u.display_name,u.role,u.disabled,u.created_at,u.updated_at
		FROM weave_external_identities e JOIN weave_users u ON u.id=e.user_id AND u.tenant_id=e.workspace_id
		WHERE e.issuer=$1 AND e.subject=$2 AND e.workspace_id=$3`, issuer, subject, tenantID).
		Scan(&user.ID, &user.TenantID, &user.Username, &user.DisplayName, &user.Role, &user.Disabled, &user.CreatedAt, &user.UpdatedAt)
	if err == nil {
		if user.Disabled {
			return nil, fmt.Errorf("external user is disabled")
		}
		if _, err = tx.Exec(ctx, `UPDATE weave_external_identities SET last_login_at=NOW(),native_organization=CASE WHEN $4<>'' THEN $4 ELSE native_organization END WHERE issuer=$1 AND subject=$2 AND workspace_id=$3`, issuer, subject, tenantID, nativeOrganization); err != nil {
			return nil, fmt.Errorf("bind external identity: %w", err)
		}
		if err = tx.Commit(ctx); err != nil {
			return nil, fmt.Errorf("bind external identity: %w", err)
		}
		return &user, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("bind external identity: %w", err)
	}
	if _, err = tx.Exec(ctx, `INSERT INTO weave_workspaces(id,slug,name) VALUES($1,$1,$1) ON CONFLICT(id) DO NOTHING`, tenantID); err != nil {
		return nil, fmt.Errorf("bind external identity: %w", err)
	}
	userID := "ext_" + hex.EncodeToString(digest[:16])
	username := "forge:" + hex.EncodeToString(digest[:12])
	if strings.TrimSpace(displayName) == "" {
		displayName = strings.TrimSpace(email)
	}
	if strings.TrimSpace(displayName) == "" {
		displayName = username
	}
	now := time.Now()
	if _, err = tx.Exec(ctx, `INSERT INTO weave_users(id,tenant_id,username,password,display_name,role,created_at,updated_at)
		VALUES($1,$2,$3,'!', $4,'member',$5,$5)`, userID, tenantID, username, displayName, now); err != nil {
		return nil, fmt.Errorf("bind external identity: %w", err)
	}
	if _, err = tx.Exec(ctx, `INSERT INTO weave_members(workspace_id,user_id,role) VALUES($1,$2,'member')`, tenantID, userID); err != nil {
		return nil, fmt.Errorf("bind external identity: %w", err)
	}
	if _, err = tx.Exec(ctx, `INSERT INTO weave_external_identities(issuer,subject,workspace_id,user_id,native_organization) VALUES($1,$2,$3,$4,$5)`, issuer, subject, tenantID, userID, nativeOrganization); err != nil {
		return nil, fmt.Errorf("bind external identity: %w", err)
	}
	if err = tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("bind external identity: %w", err)
	}
	return s.GetByID(ctx, tenantID, userID)
}

// List returns all users for a tenant.
func (s *Store) List(ctx context.Context, tenantID string) ([]User, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id, tenant_id, username, display_name, role, disabled, created_at, updated_at
		 FROM weave_users WHERE tenant_id=$1 ORDER BY created_at`, tenantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var users []User
	for rows.Next() {
		var u User
		if err := rows.Scan(&u.ID, &u.TenantID, &u.Username, &u.DisplayName, &u.Role, &u.Disabled, &u.CreatedAt, &u.UpdatedAt); err != nil {
			return nil, err
		}
		users = append(users, u)
	}
	return users, rows.Err()
}

// Update modifies a user's role, display_name, or disabled status.
func (s *Store) Update(ctx context.Context, tenantID, id, displayName, role string, disabled bool) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err = s.UpdateTx(ctx, tx, tenantID, id, displayName, role, disabled); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Store) UpdateTx(ctx context.Context, tx pgx.Tx, tenantID, id, displayName, role string, disabled bool) error {
	tag, err := tx.Exec(ctx,
		`UPDATE weave_users SET display_name=$1, role=$2, disabled=$3, updated_at=NOW()
		 WHERE id=$4 AND tenant_id=$5`,
		displayName, role, disabled, id, tenantID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("user %q not found", id)
	}
	return nil
}

// UpdateDisplayName modifies only a user's display name (self-service).
func (s *Store) UpdateDisplayName(ctx context.Context, tenantID, id, displayName string) error {
	tag, err := s.pool.Exec(ctx,
		`UPDATE weave_users SET display_name=$1, updated_at=NOW()
		 WHERE id=$2 AND tenant_id=$3`,
		displayName, id, tenantID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("user %q not found", id)
	}
	return nil
}

// UpdatePassword replaces a user's password with a bcrypt hash of the new one.
func (s *Store) UpdatePassword(ctx context.Context, tenantID, id, password string) error {
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcryptCost)
	if err != nil {
		return fmt.Errorf("hash password: %w", err)
	}
	tag, err := s.pool.Exec(ctx,
		`UPDATE weave_users SET password=$1, updated_at=NOW()
		 WHERE id=$2 AND tenant_id=$3`,
		string(hash), id, tenantID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("user %q not found", id)
	}
	return nil
}

// EnsureAdmin activates an existing user as a workspace administrator and
// converges the matching membership to owner without changing the password.
func (s *Store) EnsureAdmin(ctx context.Context, tenantID, id string) (*User, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	tag, err := tx.Exec(ctx, `UPDATE weave_users
		SET role='admin', disabled=false, updated_at=NOW()
		WHERE id=$1 AND tenant_id=$2`, id, tenantID)
	if err != nil {
		return nil, err
	}
	if tag.RowsAffected() != 1 {
		return nil, fmt.Errorf("user %q not found", id)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO weave_members(workspace_id,user_id,role)
		VALUES($1,$2,'owner')
		ON CONFLICT(workspace_id,user_id) DO UPDATE SET role='owner'`, tenantID, id); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return s.GetByID(ctx, tenantID, id)
}

// Delete removes a user by ID.
func (s *Store) Delete(ctx context.Context, tenantID, id string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err = s.DeleteTx(ctx, tx, tenantID, id); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
func (s *Store) DeleteTx(ctx context.Context, tx pgx.Tx, tenantID, id string) error {
	var deletedID string
	if err := tx.QueryRow(ctx,
		`DELETE FROM weave_users WHERE id=$1 AND tenant_id=$2 RETURNING id`, id, tenantID,
	).Scan(&deletedID); err != nil {
		return fmt.Errorf("user %q not found", id)
	}
	if _, err := tx.Exec(ctx,
		`DELETE FROM weave_members WHERE workspace_id=$1 AND user_id=$2`, tenantID, id,
	); err != nil {
		return err
	}
	return nil
}

// Count returns the total number of users for a tenant.
func (s *Store) Count(ctx context.Context, tenantID string) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx, `SELECT COUNT(*) FROM weave_users WHERE tenant_id=$1`, tenantID).Scan(&n)
	return n, err
}

// CountAll returns the total number of users across all tenants.
func (s *Store) CountAll(ctx context.Context) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx, `SELECT COUNT(*) FROM weave_users`).Scan(&n)
	return n, err
}

// Upsert creates a user if it doesn't exist, or updates role if it does.
// Used for seeding admin users on startup.
func (s *Store) Upsert(ctx context.Context, tenantID, username, password, displayName, role string) (*User, error) {
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcryptCost)
	if err != nil {
		return nil, fmt.Errorf("hash password: %w", err)
	}

	id := uuid.NewString()
	now := time.Now()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("upsert user: %w", err)
	}
	defer tx.Rollback(ctx)

	if _, err = tx.Exec(ctx, `
		INSERT INTO weave_workspaces (id, slug, name)
		VALUES ($1, $1, $1)
		ON CONFLICT (id) DO NOTHING
	`, tenantID); err != nil {
		return nil, fmt.Errorf("upsert user: %w", err)
	}
	if err = tx.QueryRow(ctx, `
		INSERT INTO weave_users (id, tenant_id, username, password, display_name, role, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $7)
		ON CONFLICT (tenant_id, username) DO UPDATE SET
			password = EXCLUDED.password,
			role = EXCLUDED.role,
			display_name = EXCLUDED.display_name,
			updated_at = NOW()
		RETURNING id
	`, id, tenantID, username, string(hash), displayName, role, now).Scan(&id); err != nil {
		return nil, fmt.Errorf("upsert user: %w", err)
	}
	if _, err = tx.Exec(ctx, `
		INSERT INTO weave_members (workspace_id, user_id, role)
		VALUES ($1, $2, $3)
		ON CONFLICT (workspace_id, user_id) DO UPDATE SET role=EXCLUDED.role
	`, tenantID, id, memberRole(role)); err != nil {
		return nil, fmt.Errorf("upsert user: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("upsert user: %w", err)
	}

	return s.GetByUsername(ctx, tenantID, username)
}

func memberRole(role string) string {
	if role == "admin" {
		return "owner"
	}
	return "member"
}
