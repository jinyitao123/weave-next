package runtimes

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const tokenPrefix = "rtk_"

var (
	// ErrInvalidEngines reports a hello payload outside the canonical CLI set.
	ErrInvalidEngines = errors.New("runtime engines must contain only claude, codex, or opencode")
	// ErrInvalidRuntimeToken reports a token that does not identify an open runtime.
	ErrInvalidRuntimeToken = errors.New("invalid runtime token")
	// ErrRuntimeUnavailable intentionally covers missing and closed runtimes.
	ErrRuntimeUnavailable = errors.New("runtime unavailable")
)

const (
	AuthModeChatGPT  = "chatgpt"
	AuthModeOAuth    = "oauth"
	AuthModeProvider = "provider"
	AuthModeUnknown  = "unknown"
)

// EngineCapability is the secretless, daemon-observed identity of one CLI
// engine. It lets operators distinguish a host subscription from a configured
// API provider without uploading credentials or local configuration contents.
type EngineCapability struct {
	Engine          string `json:"engine"`
	BinaryPath      string `json:"binary_path"`
	BinaryVersion   string `json:"binary_version"`
	AuthMode        string `json:"auth_mode"`
	ProtocolVersion string `json:"protocol_version"`
	EndpointClass   string `json:"endpoint_class"`
}

// onlineWindow is how recent a heartbeat must be for a runtime to count as
// online (daemons heartbeat every 30s; 90s tolerates two missed beats).
const onlineWindow = 90 * time.Second

// Runtime describes one registered remote engine runtime.
type Runtime struct {
	ID                       string                      `json:"id"`
	WorkspaceID              string                      `json:"workspace_id"`
	Name                     string                      `json:"name"`
	Engines                  []string                    `json:"engines"`
	EngineCapabilities       map[string]EngineCapability `json:"engine_capabilities"`
	FunctionalRevision       int64                       `json:"functional_revision"`
	TotalSlots               int                         `json:"total_slots"`
	ActiveSlots              int                         `json:"active_slots"`
	PoolID                   string                      `json:"pool_id,omitempty"`
	HealthStatus             string                      `json:"health_status"`
	ConsecutiveInfraFailures int                         `json:"consecutive_infra_failures"`
	QuarantineUntil          *time.Time                  `json:"quarantine_until,omitempty"`
	LastFailureReason        string                      `json:"last_failure_reason,omitempty"`
	Enabled                  bool                        `json:"enabled"`
	RevokedAt                *time.Time                  `json:"revoked_at,omitempty"`
	DeletedAt                *time.Time                  `json:"deleted_at,omitempty"`
	Online                   bool                        `json:"online"`
	LastHeartbeatAt          *time.Time                  `json:"last_heartbeat_at,omitempty"`
	CreatedAt                time.Time                   `json:"created_at"`
	UpdatedAt                time.Time                   `json:"updated_at"`
}

// Store persists runtime registrations and heartbeats.
type Store struct {
	pool  *pgxpool.Pool
	clock func() time.Time
}

// NewStore creates a runtime store.
func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool, clock: time.Now}
}

func (s *Store) now() time.Time {
	if s.clock == nil {
		return time.Now()
	}
	return s.clock()
}

// Create registers a runtime and returns its one-time raw token.
func (s *Store) Create(ctx context.Context, workspaceID, name string) (*Runtime, string, error) {
	token, err := generateToken()
	if err != nil {
		return nil, "", fmt.Errorf("generate runtime token: %w", err)
	}
	now := s.now()
	runtime := &Runtime{
		ID:                 uuid.NewString(),
		WorkspaceID:        workspaceID,
		Name:               name,
		Engines:            []string{},
		EngineCapabilities: map[string]EngineCapability{},
		FunctionalRevision: 1,
		TotalSlots:         1,
		HealthStatus:       "healthy",
		Enabled:            true,
		CreatedAt:          now,
		UpdatedAt:          now,
	}
	if _, err := s.pool.Exec(ctx, `
			INSERT INTO weave_runtimes (
				id, workspace_id, name, engines, token_hash,
				functional_revision, enabled, created_at, updated_at
			) VALUES ($1, $2, $3, '[]', $4, 1, true, $5, $5)
	`, runtime.ID, workspaceID, name, hashToken(token), now); err != nil {
		return nil, "", fmt.Errorf("create runtime: %w", err)
	}
	return runtime, token, nil
}

// ValidateToken returns the runtime identified by a raw runtime token.
func (s *Store) ValidateToken(ctx context.Context, token string) (*Runtime, error) {
	row := s.pool.QueryRow(ctx, `
			SELECT `+runtimeColumns+`
			FROM weave_runtimes
			WHERE token_hash=$1
			  AND enabled=true AND revoked_at IS NULL AND deleted_at IS NULL
	`, hashToken(token))
	runtime, err := scanRuntime(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrInvalidRuntimeToken
	}
	if err != nil {
		return nil, fmt.Errorf("validate runtime token: %w", err)
	}
	return runtime, nil
}

// Hello records the runtime's installed engines and refreshes its heartbeat.
func (s *Store) Hello(ctx context.Context, workspaceID, id string, engines []string) error {
	return s.HelloWithCapabilities(ctx, workspaceID, id, engines, nil, 1)
}

// HelloWithCapabilities records the daemon-observed executable/auth facts and
// capacity. The facts are diagnostic metadata only; credentials never cross
// this boundary.
func (s *Store) HelloWithCapabilities(
	ctx context.Context,
	workspaceID, id string,
	engines []string,
	capabilities []EngineCapability,
	totalSlots int,
) error {
	canonical, err := canonicalEngines(engines)
	if err != nil {
		return err
	}
	if totalSlots < 1 {
		return fmt.Errorf("runtime total slots must be positive")
	}
	capabilityMap, err := canonicalEngineCapabilities(canonical, capabilities)
	if err != nil {
		return err
	}
	encoded, err := json.Marshal(canonical)
	if err != nil {
		return fmt.Errorf("marshal runtime engines: %w", err)
	}
	encodedCapabilities, err := json.Marshal(capabilityMap)
	if err != nil {
		return fmt.Errorf("marshal runtime engine capabilities: %w", err)
	}
	now := s.now()
	tag, err := s.pool.Exec(ctx, `
			UPDATE weave_runtimes
			SET engines=$3, engine_capabilities=$4, total_slots=$5,
			    active_slots=0,
			    functional_revision=functional_revision + CASE
			      WHEN engines IS DISTINCT FROM $3::jsonb
			        OR engine_capabilities IS DISTINCT FROM $4::jsonb
			        OR total_slots IS DISTINCT FROM $5 THEN 1
			      ELSE 0
			    END,
			    last_heartbeat_at=$6, updated_at=$6
			WHERE workspace_id=$1 AND id=$2
			  AND enabled=true AND revoked_at IS NULL AND deleted_at IS NULL
	`, workspaceID, id, encoded, encodedCapabilities, totalSlots, now)
	if err != nil {
		return fmt.Errorf("hello runtime: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: runtime %q", ErrRuntimeUnavailable, id)
	}
	return nil
}

func canonicalEngineCapabilities(
	engines []string,
	capabilities []EngineCapability,
) (map[string]EngineCapability, error) {
	result := make(map[string]EngineCapability, len(capabilities))
	for _, capability := range capabilities {
		capability.Engine = strings.TrimSpace(capability.Engine)
		capability.BinaryPath = strings.TrimSpace(capability.BinaryPath)
		capability.BinaryVersion = strings.TrimSpace(capability.BinaryVersion)
		capability.AuthMode = strings.TrimSpace(capability.AuthMode)
		capability.ProtocolVersion = strings.TrimSpace(capability.ProtocolVersion)
		capability.EndpointClass = strings.TrimSpace(capability.EndpointClass)
		if !slices.Contains(engines, capability.Engine) {
			return nil, fmt.Errorf("runtime engine capability %q was not advertised", capability.Engine)
		}
		if capability.BinaryPath == "" || capability.BinaryVersion == "" || capability.ProtocolVersion == "" {
			return nil, fmt.Errorf("runtime engine capability %q is incomplete", capability.Engine)
		}
		switch capability.AuthMode {
		case AuthModeChatGPT, AuthModeOAuth, AuthModeProvider, AuthModeUnknown:
		default:
			return nil, fmt.Errorf("runtime engine capability %q has invalid auth mode", capability.Engine)
		}
		if capability.EndpointClass == "" {
			return nil, fmt.Errorf("runtime engine capability %q has no endpoint class", capability.Engine)
		}
		if _, exists := result[capability.Engine]; exists {
			return nil, fmt.Errorf("runtime engine capability %q is duplicated", capability.Engine)
		}
		result[capability.Engine] = capability
	}
	return result, nil
}

// Heartbeat refreshes the runtime's online timestamp.
func (s *Store) Heartbeat(ctx context.Context, workspaceID, id string) error {
	return s.HeartbeatWithLoad(ctx, workspaceID, id, -1)
}

// HeartbeatWithLoad refreshes liveness and, when supplied, the daemon's
// current number of occupied execution slots.
func (s *Store) HeartbeatWithLoad(ctx context.Context, workspaceID, id string, activeSlots int) error {
	now := s.now()
	tag, err := s.pool.Exec(ctx, `
			UPDATE weave_runtimes
			SET active_slots=CASE WHEN $3 < 0 THEN active_slots ELSE $3 END,
			    last_heartbeat_at=$4, updated_at=$4
			WHERE workspace_id=$1 AND id=$2
			  AND enabled=true AND revoked_at IS NULL AND deleted_at IS NULL
			  AND ($3 < 0 OR ($3 >= 0 AND $3 <= total_slots))
	`, workspaceID, id, activeSlots, now)
	if err != nil {
		return fmt.Errorf("heartbeat runtime: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: runtime %q", ErrRuntimeUnavailable, id)
	}
	return nil
}

// Configure updates operator-owned routing metadata. A nil poolID preserves
// the current pool; an empty pointed value removes explicit pool membership.
func (s *Store) Configure(ctx context.Context, workspaceID, id, name string, poolID *string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return errors.New("runtime name is required")
	}
	var normalizedPool *string
	if poolID != nil {
		value := strings.TrimSpace(*poolID)
		if len(value) > 80 {
			return errors.New("runtime pool id is too long")
		}
		normalizedPool = &value
	}
	now := s.now()
	tag, err := s.pool.Exec(ctx, `
		UPDATE weave_runtimes
		SET name=$3,
		    pool_id=CASE WHEN $4::boolean THEN NULLIF($5, '') ELSE pool_id END,
		    functional_revision=functional_revision + CASE
		      WHEN $4::boolean AND pool_id IS DISTINCT FROM NULLIF($5, '') THEN 1 ELSE 0 END,
		    updated_at=$6
		WHERE workspace_id=$1 AND id=$2
		  AND enabled=true AND revoked_at IS NULL AND deleted_at IS NULL
	`, workspaceID, id, name, poolID != nil, valueOrEmpty(normalizedPool), now)
	if err != nil {
		return fmt.Errorf("configure runtime: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: runtime %q", ErrRuntimeUnavailable, id)
	}
	return nil
}

func valueOrEmpty(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

// RecordInfrastructureFailure updates the automatic-routing health gate. A
// runtime is degraded after the first trusted infrastructure failure and is
// quarantined after three consecutive failures.
func (s *Store) RecordInfrastructureFailure(ctx context.Context, workspaceID, id, reason string) error {
	now := s.now()
	_, err := s.pool.Exec(ctx, `
		UPDATE weave_runtimes
		SET consecutive_infra_failures=consecutive_infra_failures+1,
		    health_status=CASE WHEN consecutive_infra_failures+1 >= 3 THEN 'quarantined' ELSE 'degraded' END,
		    quarantine_until=CASE WHEN consecutive_infra_failures+1 >= 3 THEN $4 ELSE quarantine_until END,
		    last_failure_reason=$3,
		    updated_at=$2
		WHERE workspace_id=$1 AND id=$5
	`, workspaceID, now, strings.TrimSpace(reason), now.Add(5*time.Minute), id)
	if err != nil {
		return fmt.Errorf("record runtime infrastructure failure: %w", err)
	}
	return nil
}

// RecordExecutionSuccess clears automatic quarantine only after a real task
// succeeds; a heartbeat alone is not treated as an engine capability probe.
func (s *Store) RecordExecutionSuccess(ctx context.Context, workspaceID, id string) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE weave_runtimes
		SET consecutive_infra_failures=0, health_status='healthy',
		    quarantine_until=NULL, last_failure_reason=NULL, updated_at=$3
		WHERE workspace_id=$1 AND id=$2
	`, workspaceID, id, s.now())
	if err != nil {
		return fmt.Errorf("record runtime execution success: %w", err)
	}
	return nil
}

// List returns the runtimes registered in one workspace.
func (s *Store) List(ctx context.Context, workspaceID string) ([]Runtime, error) {
	rows, err := s.pool.Query(ctx, `
			SELECT `+runtimeColumns+`,
				COALESCE(
					enabled=true AND revoked_at IS NULL AND last_heartbeat_at >= $2,
					false
				)
			FROM weave_runtimes
			WHERE workspace_id=$1 AND deleted_at IS NULL
		ORDER BY created_at DESC
	`, workspaceID, s.now().Add(-onlineWindow))
	if err != nil {
		return nil, fmt.Errorf("list runtimes: %w", err)
	}
	defer rows.Close()

	runtimes := make([]Runtime, 0)
	for rows.Next() {
		var runtime Runtime
		var engines, capabilities []byte
		if err := rows.Scan(
			&runtime.ID, &runtime.WorkspaceID, &runtime.Name, &engines, &capabilities,
			&runtime.FunctionalRevision, &runtime.TotalSlots, &runtime.ActiveSlots, &runtime.PoolID,
			&runtime.HealthStatus, &runtime.ConsecutiveInfraFailures, &runtime.QuarantineUntil,
			&runtime.LastFailureReason, &runtime.Enabled,
			&runtime.RevokedAt, &runtime.DeletedAt,
			&runtime.LastHeartbeatAt, &runtime.CreatedAt, &runtime.UpdatedAt, &runtime.Online,
		); err != nil {
			return nil, fmt.Errorf("scan runtime: %w", err)
		}
		if err := json.Unmarshal(engines, &runtime.Engines); err != nil {
			return nil, fmt.Errorf("decode runtime engines: %w", err)
		}
		if err := json.Unmarshal(capabilities, &runtime.EngineCapabilities); err != nil {
			return nil, fmt.Errorf("decode runtime engine capabilities: %w", err)
		}
		runtimes = append(runtimes, runtime)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list runtimes: %w", err)
	}
	return runtimes, nil
}

// Get returns one runtime from a workspace, including its current online state.
func (s *Store) Get(ctx context.Context, workspaceID, id string) (*Runtime, error) {
	row := s.pool.QueryRow(ctx, `
		SELECT `+runtimeColumns+`,
			COALESCE(last_heartbeat_at >= $3, false)
			FROM weave_runtimes
			WHERE workspace_id=$1 AND id=$2
			  AND enabled=true AND revoked_at IS NULL AND deleted_at IS NULL
	`, workspaceID, id, s.now().Add(-onlineWindow))
	var runtime Runtime
	var engines, capabilities []byte
	err := row.Scan(
		&runtime.ID, &runtime.WorkspaceID, &runtime.Name, &engines, &capabilities,
		&runtime.FunctionalRevision, &runtime.TotalSlots, &runtime.ActiveSlots, &runtime.PoolID,
		&runtime.HealthStatus, &runtime.ConsecutiveInfraFailures, &runtime.QuarantineUntil,
		&runtime.LastFailureReason, &runtime.Enabled,
		&runtime.RevokedAt, &runtime.DeletedAt,
		&runtime.LastHeartbeatAt, &runtime.CreatedAt, &runtime.UpdatedAt, &runtime.Online,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("%w: runtime %q", ErrRuntimeUnavailable, id)
	}
	if err != nil {
		return nil, fmt.Errorf("get runtime %q: %w", id, err)
	}
	if err := json.Unmarshal(engines, &runtime.Engines); err != nil {
		return nil, fmt.Errorf("decode runtime engines: %w", err)
	}
	if err := json.Unmarshal(capabilities, &runtime.EngineCapabilities); err != nil {
		return nil, fmt.Errorf("decode runtime engine capabilities: %w", err)
	}
	return &runtime, nil
}

// Rename updates one runtime's display name.
func (s *Store) Rename(ctx context.Context, workspaceID, id, name string) error {
	return s.Configure(ctx, workspaceID, id, name, nil)
}

// Delete permanently soft-closes one runtime from a workspace.
func (s *Store) Delete(ctx context.Context, workspaceID, id string) error {
	now := s.now()
	tag, err := s.pool.Exec(ctx, `
		UPDATE weave_runtimes
		SET enabled=false,
		    revoked_at=COALESCE(revoked_at, $3),
		    deleted_at=COALESCE(deleted_at, $3),
		    updated_at=$3
		WHERE workspace_id=$1 AND id=$2 AND deleted_at IS NULL
	`, workspaceID, id, now)
	if err != nil {
		return fmt.Errorf("delete runtime: %w", err)
	}
	if tag.RowsAffected() == 0 {
		var exists bool
		if err := s.pool.QueryRow(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM weave_runtimes
				WHERE workspace_id=$1 AND id=$2
			)
		`, workspaceID, id).Scan(&exists); err != nil {
			return fmt.Errorf("check deleted runtime: %w", err)
		}
		if !exists {
			return fmt.Errorf("%w: runtime %q", ErrRuntimeUnavailable, id)
		}
	}
	return nil
}

const runtimeColumns = `
		id, workspace_id, name, engines, engine_capabilities,
		functional_revision, total_slots, active_slots, COALESCE(pool_id, ''),
		health_status, consecutive_infra_failures, quarantine_until,
		COALESCE(last_failure_reason, ''), enabled,
		revoked_at, deleted_at, last_heartbeat_at, created_at, updated_at`

type rowScanner interface {
	Scan(dest ...any) error
}

func scanRuntime(row rowScanner) (*Runtime, error) {
	var runtime Runtime
	var engines, capabilities []byte
	if err := row.Scan(
		&runtime.ID, &runtime.WorkspaceID, &runtime.Name, &engines, &capabilities,
		&runtime.FunctionalRevision, &runtime.TotalSlots, &runtime.ActiveSlots, &runtime.PoolID,
		&runtime.HealthStatus, &runtime.ConsecutiveInfraFailures, &runtime.QuarantineUntil,
		&runtime.LastFailureReason, &runtime.Enabled,
		&runtime.RevokedAt, &runtime.DeletedAt,
		&runtime.LastHeartbeatAt, &runtime.CreatedAt, &runtime.UpdatedAt,
	); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(engines, &runtime.Engines); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(capabilities, &runtime.EngineCapabilities); err != nil {
		return nil, err
	}
	return &runtime, nil
}

func canonicalEngines(engines []string) ([]string, error) {
	canonical := make([]string, 0, len(engines))
	seen := make(map[string]struct{}, len(engines))
	for _, engine := range engines {
		switch engine {
		case "claude", "codex", "opencode":
		default:
			return nil, fmt.Errorf("%w: %q", ErrInvalidEngines, engine)
		}
		if _, ok := seen[engine]; ok {
			continue
		}
		seen[engine] = struct{}{}
		canonical = append(canonical, engine)
	}
	slices.Sort(canonical)
	return canonical, nil
}

func generateToken() (string, error) {
	random := make([]byte, 24)
	if _, err := rand.Read(random); err != nil {
		return "", err
	}
	return tokenPrefix + hex.EncodeToString(random), nil
}

func hashToken(token string) string {
	hash := sha256.Sum256([]byte(token))
	return hex.EncodeToString(hash[:])
}
