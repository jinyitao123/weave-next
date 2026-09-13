package mcpregistry

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jinyitao123/weave/internal/base/frozen"
	"github.com/jinyitao123/weave/internal/kernel/secret"
)

var (
	ErrNotFound       = &Error{code: CodeNotFound}
	ErrConflict       = &Error{code: CodeConflict}
	ErrInUse          = errors.New("MCP server is in use")
	ErrClosed         = &Error{code: CodeClosed}
	ErrKeyUnavailable = &Error{code: CodeKeyUnavailable}
)

type Store struct {
	pool *pgxpool.Pool
	key  []byte
}

func New(pool *pgxpool.Pool, key []byte) *Store {
	return &Store{pool: pool, key: append([]byte(nil), key...)}
}

func (s *Store) Create(ctx context.Context, workspaceID, createdBy string, req UpsertServerRequest) (ServerView, error) {
	if err := ValidateUpsertServerRequest(req); err != nil {
		return ServerView{}, err
	}
	if err := s.requireKey(); err != nil {
		return ServerView{}, err
	}
	headers := map[string]string{}
	env := map[string]string{}
	if req.Transport == TransportStreamableHTTP {
		headers = createSecretMap(req.Headers)
	} else {
		env = createSecretMap(req.Env)
	}
	headersCipher, err := s.sealMap(headers)
	if err != nil {
		return ServerView{}, err
	}
	envCipher, err := s.sealMap(env)
	if err != nil {
		return ServerView{}, err
	}
	id := uuid.NewString()
	argsJSON, err := json.Marshal(normalizedArgs(req.Args))
	if err != nil {
		return ServerView{}, coded(ErrInvalidRequest, "encode args")
	}
	urlValue, commandValue := connectionValues(req)
	_, err = s.pool.Exec(ctx, `
		INSERT INTO weave_mcp_servers (
			id, workspace_id, slug, display_name, transport, url, command, args,
			headers_cipher, env_cipher, enabled, created_by
		)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8::jsonb, $9, $10, $11, $12)
	`, id, workspaceID, req.Slug, req.DisplayName, req.Transport, urlValue,
		commandValue, argsJSON, headersCipher, envCipher, req.Enabled, createdBy)
	if err != nil {
		return ServerView{}, mapStoreError(err)
	}
	return s.Get(ctx, workspaceID, id)
}

func (s *Store) List(ctx context.Context, workspaceID string) ([]ServerView, error) {
	if err := s.requireKey(); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, serverViewSelect+`
		WHERE s.workspace_id=$1 AND s.deleted_at IS NULL
		ORDER BY s.slug
	`, workspaceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	views := make([]ServerView, 0)
	for rows.Next() {
		view, err := s.scanView(rows)
		if err != nil {
			return nil, err
		}
		views = append(views, view)
	}
	return views, rows.Err()
}

// ListMetadata returns the workspace registry inventory without reading or
// decrypting headers/environment secrets. Capability discovery only needs
// this projection and therefore remains available after credential-key
// rotation; execution and mutation paths continue to fail closed when their
// connection material cannot be decrypted.
func (s *Store) ListMetadata(ctx context.Context, workspaceID string) ([]ServerMetadata, error) {
	rows, err := s.pool.Query(ctx, serverMetadataSelect+`
		WHERE s.workspace_id=$1 AND s.deleted_at IS NULL
		ORDER BY s.slug
	`, workspaceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	metadata := make([]ServerMetadata, 0)
	for rows.Next() {
		item, err := scanMetadata(rows)
		if err != nil {
			return nil, err
		}
		metadata = append(metadata, item)
	}
	return metadata, rows.Err()
}

func (s *Store) Get(ctx context.Context, workspaceID, id string) (ServerView, error) {
	if err := s.requireKey(); err != nil {
		return ServerView{}, err
	}
	view, err := s.scanView(s.pool.QueryRow(ctx, serverViewSelect+`
		WHERE s.workspace_id=$1 AND s.id=$2 AND s.deleted_at IS NULL
	`, workspaceID, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return ServerView{}, ErrNotFound
	}
	return view, err
}

func (s *Store) Update(ctx context.Context, workspaceID, id string, req UpsertServerRequest) (ServerView, error) {
	if err := ValidateUpsertServerRequest(req); err != nil {
		return ServerView{}, err
	}
	if err := s.requireKey(); err != nil {
		return ServerView{}, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return ServerView{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var (
		oldTransport     Transport
		oldURL           *string
		oldCommand       *string
		oldArgsJSON      []byte
		oldHeadersCipher string
		oldEnvCipher     string
		oldEnabled       bool
		oldRevision      int64
		oldRevokedAt     *time.Time
		oldDeletedAt     *time.Time
	)
	err = tx.QueryRow(ctx, `
		SELECT transport, url, command, args, headers_cipher, env_cipher,
		       enabled, functional_revision, revoked_at, deleted_at
		FROM weave_mcp_servers
		WHERE workspace_id=$1 AND id=$2
		FOR UPDATE
	`, workspaceID, id).Scan(
		&oldTransport, &oldURL, &oldCommand, &oldArgsJSON, &oldHeadersCipher,
		&oldEnvCipher, &oldEnabled, &oldRevision, &oldRevokedAt, &oldDeletedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return ServerView{}, ErrNotFound
	}
	if err != nil {
		return ServerView{}, err
	}
	if !oldEnabled || oldRevokedAt != nil || oldDeletedAt != nil {
		return ServerView{}, coded(ErrClosed, "closed MCP servers cannot be revived by ordinary update")
	}
	oldHeaders, err := s.openMap(oldHeadersCipher)
	if err != nil {
		return ServerView{}, fmt.Errorf("decrypt MCP server headers: %w", err)
	}
	oldEnv, err := s.openMap(oldEnvCipher)
	if err != nil {
		return ServerView{}, fmt.Errorf("decrypt MCP server environment: %w", err)
	}
	headers := map[string]string{}
	env := map[string]string{}
	if req.Transport == TransportStreamableHTTP {
		if oldTransport == TransportStreamableHTTP {
			headers = maps.Clone(oldHeaders)
		}
		patchSecretMap(headers, req.Headers, req.DeletedHeaders)
	} else {
		if oldTransport == TransportStdio {
			env = maps.Clone(oldEnv)
		}
		patchSecretMap(env, req.Env, req.DeletedEnv)
	}
	headersCipher, err := s.sealMap(headers)
	if err != nil {
		return ServerView{}, err
	}
	envCipher, err := s.sealMap(env)
	if err != nil {
		return ServerView{}, err
	}
	var oldArgs []string
	if err := json.Unmarshal(oldArgsJSON, &oldArgs); err != nil {
		return ServerView{}, fmt.Errorf("decode current MCP server args: %w", err)
	}
	urlValue, commandValue := connectionValues(req)
	functionalChanged := oldTransport != req.Transport ||
		!optionalStringEqual(oldURL, urlValue) ||
		!optionalStringEqual(oldCommand, commandValue) ||
		!slices.Equal(oldArgs, req.Args)
	securityChanged := !maps.Equal(oldHeaders, headers) || !maps.Equal(oldEnv, env)
	nextRevision := oldRevision
	if functionalChanged {
		nextRevision++
	}
	argsJSON, err := json.Marshal(normalizedArgs(req.Args))
	if err != nil {
		return ServerView{}, coded(ErrInvalidRequest, "encode args")
	}
	invalidateProbe := functionalChanged || !req.Enabled

	_, err = tx.Exec(ctx, `
		UPDATE weave_mcp_servers
		SET slug=$3, display_name=$4, transport=$5, url=$6, command=$7,
		    args=$8::jsonb, headers_cipher=$9, env_cipher=$10, enabled=$11,
		    functional_revision=$12,
		    status=CASE WHEN $13 OR $14 THEN 'unknown' ELSE status END,
		    protocol_version=CASE WHEN $13 THEN '' ELSE protocol_version END,
		    server_info=CASE WHEN $13 THEN '{}'::jsonb ELSE server_info END,
		    last_error=CASE WHEN $13 OR $14 THEN '' ELSE last_error END,
		    last_probed_at=CASE WHEN $13 THEN NULL ELSE last_probed_at END,
		    last_handshake_at=CASE WHEN $13 THEN NULL ELSE last_handshake_at END,
		    updated_at=now()
		WHERE workspace_id=$1 AND id=$2
	`, workspaceID, id, req.Slug, req.DisplayName, req.Transport, urlValue,
		commandValue, argsJSON, headersCipher, envCipher, req.Enabled,
		nextRevision, invalidateProbe, securityChanged)
	if err != nil {
		return ServerView{}, mapStoreError(err)
	}
	if invalidateProbe {
		if _, err := tx.Exec(ctx, `
			DELETE FROM weave_mcp_tools WHERE workspace_id=$1 AND server_id=$2
		`, workspaceID, id); err != nil {
			return ServerView{}, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return ServerView{}, err
	}
	return s.Get(ctx, workspaceID, id)
}

func (s *Store) Delete(ctx context.Context, workspaceID, id string) error {
	if err := s.requireKey(); err != nil {
		return err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var deletedAt *time.Time
	err = tx.QueryRow(ctx, `
		SELECT deleted_at
		FROM weave_mcp_servers
		WHERE workspace_id=$1 AND id=$2
		FOR UPDATE
	`, workspaceID, id).Scan(&deletedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return coded(ErrNotFound, "server is absent from workspace")
	}
	if err != nil {
		return err
	}

	if deletedAt != nil {
		return tx.Commit(ctx)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE weave_mcp_servers
		SET enabled=false, revoked_at=COALESCE(revoked_at, now()),
		    deleted_at=COALESCE(deleted_at, now()), status='unknown',
		    protocol_version='', server_info='{}', last_error='',
		    last_probed_at=NULL, last_handshake_at=NULL, updated_at=now()
		WHERE workspace_id=$1 AND id=$2
	`, workspaceID, id); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		DELETE FROM weave_mcp_tools WHERE workspace_id=$1 AND server_id=$2
	`, workspaceID, id); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Store) Resolve(ctx context.Context, workspaceID, id string) (ResolvedServer, error) {
	if err := s.requireKey(); err != nil {
		return ResolvedServer{}, err
	}
	var (
		server        Server
		urlValue      *string
		commandValue  *string
		argsJSON      []byte
		serverInfo    []byte
		headersCipher string
		envCipher     string
	)
	err := s.pool.QueryRow(ctx, `
		SELECT id, workspace_id, slug, display_name, transport, url, command, args,
		       functional_revision, enabled, revoked_at, status, protocol_version, server_info, last_error,
		       last_probed_at, last_handshake_at, created_by, created_at, updated_at,
		       deleted_at, headers_cipher, env_cipher
		FROM weave_mcp_servers
		WHERE workspace_id=$1 AND id=$2
	`, workspaceID, id).Scan(
		&server.ID, &server.WorkspaceID, &server.Slug, &server.DisplayName,
		&server.Transport, &urlValue, &commandValue, &argsJSON, &server.FunctionalRevision,
		&server.Enabled, &server.RevokedAt,
		&server.Status, &server.ProtocolVersion, &serverInfo, &server.LastError,
		&server.LastProbedAt, &server.LastHandshakeAt, &server.CreatedBy,
		&server.CreatedAt, &server.UpdatedAt, &server.DeletedAt, &headersCipher, &envCipher,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return ResolvedServer{}, ErrNotFound
	}
	if err != nil {
		return ResolvedServer{}, err
	}
	if !server.Enabled || server.RevokedAt != nil || server.DeletedAt != nil {
		return ResolvedServer{}, coded(ErrClosed, "MCP server is disabled, revoked, or deleted")
	}
	if err := populateServerJSON(&server, urlValue, commandValue, argsJSON, serverInfo); err != nil {
		return ResolvedServer{}, err
	}
	headers, err := s.openMap(headersCipher)
	if err != nil {
		return ResolvedServer{}, fmt.Errorf("decrypt MCP server headers: %w", err)
	}
	env, err := s.openMap(envCipher)
	if err != nil {
		return ResolvedServer{}, fmt.Errorf("decrypt MCP server environment: %w", err)
	}
	return ResolvedServer{Server: server, Headers: headers, Env: env}, nil
}

// RecordProbeSuccess atomically replaces the cached tool catalog and records
// the metadata from the same successful strict handshake.
func (s *Store) RecordProbeSuccess(
	ctx context.Context,
	workspaceID string,
	id string,
	protocol string,
	serverInfo json.RawMessage,
	tools []Tool,
) (ProbeResult, error) {
	if err := s.requireKey(); err != nil {
		return ProbeResult{}, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return ProbeResult{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var enabled bool
	var revokedAt, deletedAt *time.Time
	err = tx.QueryRow(ctx, `
		SELECT enabled, revoked_at, deleted_at
		FROM weave_mcp_servers
		WHERE workspace_id=$1 AND id=$2
		FOR UPDATE
	`, workspaceID, id).Scan(&enabled, &revokedAt, &deletedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return ProbeResult{}, ErrNotFound
	}
	if err != nil {
		return ProbeResult{}, err
	}
	if !enabled || revokedAt != nil || deletedAt != nil {
		return ProbeResult{}, coded(ErrClosed, "closed MCP servers cannot be probed")
	}
	if _, err := tx.Exec(ctx, `
		DELETE FROM weave_mcp_tools WHERE workspace_id=$1 AND server_id=$2
	`, workspaceID, id); err != nil {
		return ProbeResult{}, err
	}

	definitions := make([]frozen.FrozenToolDefinition, 0, len(tools))
	for _, tool := range tools {
		definitions = append(definitions, frozen.FrozenToolDefinition{Name: tool.Name, InputSchema: tool.InputSchema})
	}
	if _, err := frozen.NormalizeToolDefinitions(definitions); err != nil {
		return ProbeResult{}, errors.New("invalid MCP tool schema catalog")
	}
	discoveredAt := time.Now().UTC().Truncate(time.Microsecond)
	for _, tool := range tools {
		inputSchema := tool.InputSchema
		annotations := normalizeCatalogJSON(tool.Annotations)
		if _, err := tx.Exec(ctx, `
			INSERT INTO weave_mcp_tools (
				workspace_id, server_id, name, description, input_schema, annotations,
				read_only_hint, discovered_at
			)
			VALUES ($1, $2, $3, $4, $5::jsonb, $6::jsonb, $7, $8)
		`, workspaceID, id, tool.Name, tool.Description, inputSchema, annotations,
			tool.ReadOnlyHint, discoveredAt); err != nil {
			return ProbeResult{}, err
		}
	}
	serverInfo = normalizeCatalogJSON(serverInfo)
	if _, err := tx.Exec(ctx, `
		UPDATE weave_mcp_servers
		SET status='online', protocol_version=$3, server_info=$4::jsonb,
		    last_error='', last_probed_at=$5, last_handshake_at=$5,
		    updated_at=$5
		WHERE workspace_id=$1 AND id=$2 AND deleted_at IS NULL
	`, workspaceID, id, protocol, serverInfo, discoveredAt); err != nil {
		return ProbeResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return ProbeResult{}, err
	}
	return s.Catalog(ctx, workspaceID, id)
}

// RecordProbeFailure marks a server offline without touching the most recent
// successful handshake metadata or cached catalog.
func (s *Store) RecordProbeFailure(ctx context.Context, workspaceID, id, lastError string) (ServerView, error) {
	if err := s.requireKey(); err != nil {
		return ServerView{}, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return ServerView{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var enabled bool
	var revokedAt, deletedAt *time.Time
	err = tx.QueryRow(ctx, `
		SELECT enabled, revoked_at, deleted_at
		FROM weave_mcp_servers
		WHERE workspace_id=$1 AND id=$2
		FOR UPDATE
	`, workspaceID, id).Scan(&enabled, &revokedAt, &deletedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return ServerView{}, ErrNotFound
	}
	if err != nil {
		return ServerView{}, err
	}
	if !enabled || revokedAt != nil || deletedAt != nil {
		return ServerView{}, coded(ErrClosed, "closed MCP servers cannot record probe failures")
	}

	if _, err := tx.Exec(ctx, `
		UPDATE weave_mcp_servers
		SET status='offline', last_error=$3, last_probed_at=now(), updated_at=now()
		WHERE workspace_id=$1 AND id=$2
	`, workspaceID, id, lastError); err != nil {
		return ServerView{}, err
	}
	view, err := s.scanView(tx.QueryRow(ctx, serverViewSelect+`
		WHERE s.workspace_id=$1 AND s.id=$2
	`, workspaceID, id))
	if err != nil {
		return ServerView{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return ServerView{}, err
	}
	return view, nil
}

// Catalog returns only the most recently persisted successful tool catalog.
// It never contacts the configured MCP server.
func (s *Store) Catalog(ctx context.Context, workspaceID, id string) (ProbeResult, error) {
	if err := s.requireKey(); err != nil {
		return ProbeResult{}, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return ProbeResult{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var enabled bool
	var revokedAt, deletedAt *time.Time
	err = tx.QueryRow(ctx, `
		SELECT enabled, revoked_at, deleted_at
		FROM weave_mcp_servers
		WHERE workspace_id=$1 AND id=$2
		FOR SHARE
	`, workspaceID, id).Scan(&enabled, &revokedAt, &deletedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return ProbeResult{}, ErrNotFound
	}
	if err != nil {
		return ProbeResult{}, err
	}
	if deletedAt != nil {
		return ProbeResult{}, ErrNotFound
	}
	if !enabled || revokedAt != nil {
		return ProbeResult{}, coded(ErrClosed, "MCP server is disabled or revoked")
	}

	server, err := s.scanView(tx.QueryRow(ctx, serverViewSelect+`
		WHERE s.workspace_id=$1 AND s.id=$2
	`, workspaceID, id))
	if err != nil {
		return ProbeResult{}, err
	}
	tools, err := s.listCachedToolsTx(ctx, tx, workspaceID, id)
	if err != nil {
		return ProbeResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return ProbeResult{}, err
	}
	return ProbeResult{Server: server, Tools: tools}, nil
}

func (s *Store) listCachedToolsTx(ctx context.Context, tx pgx.Tx, workspaceID, id string) ([]Tool, error) {
	rows, err := tx.Query(ctx, `
		SELECT t.server_id, t.name, t.description, t.input_schema, t.annotations,
		       t.read_only_hint, t.discovered_at
		FROM weave_mcp_tools t
		JOIN weave_mcp_servers s
		  ON s.workspace_id=t.workspace_id AND s.id=t.server_id
		WHERE t.workspace_id=$1 AND t.server_id=$2 AND s.enabled=true
		  AND s.revoked_at IS NULL AND s.deleted_at IS NULL
		ORDER BY t.name
	`, workspaceID, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	tools := make([]Tool, 0)
	for rows.Next() {
		var tool Tool
		if err := rows.Scan(
			&tool.ServerID, &tool.Name, &tool.Description, &tool.InputSchema,
			&tool.Annotations, &tool.ReadOnlyHint, &tool.DiscoveredAt,
		); err != nil {
			return nil, err
		}
		tools = append(tools, tool)
	}
	return tools, rows.Err()
}

// ListAgentNames returns current, non-deleted agents that reference a registry
// server by stable ID. It reads the persisted JSON so it also sees refs before
// the runtime AgentRecord type begins consuming server_id in R3.
func (s *Store) ListAgentNames(ctx context.Context, workspaceID, id string) ([]string, error) {
	if _, err := s.Get(ctx, workspaceID, id); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `
		SELECT a.name
		FROM weave_agents a
		WHERE a.workspace_id=$1
		  AND a.deleted=false
		  AND COALESCE(a.spec->'mcp_servers', '[]'::jsonb)
		      @> jsonb_build_array(jsonb_build_object('server_id', $2::text))
		ORDER BY a.name
	`, workspaceID, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	names := make([]string, 0)
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		names = append(names, name)
	}
	return names, rows.Err()
}

func normalizeCatalogJSON(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 || string(raw) == "null" {
		return json.RawMessage(`{}`)
	}
	return raw
}

const serverViewSelect = `
	SELECT s.id, s.workspace_id, s.slug, s.display_name, s.transport, s.url,
	       s.command, s.args, s.functional_revision, s.enabled, s.revoked_at,
	       s.status, s.protocol_version,
	       s.server_info, s.last_error, s.last_probed_at, s.last_handshake_at,
	       s.created_by, s.created_at, s.updated_at, s.deleted_at,
	       s.headers_cipher, s.env_cipher,
	       (SELECT COUNT(*) FROM weave_mcp_tools t
	         WHERE t.workspace_id=s.workspace_id AND t.server_id=s.id),
	       (SELECT COUNT(*)
	          FROM weave_agents a
	         WHERE a.workspace_id=s.workspace_id
	           AND a.deleted=false
	           AND COALESCE(a.spec->'mcp_servers', '[]'::jsonb)
	               @> jsonb_build_array(jsonb_build_object('server_id', s.id)))
	FROM weave_mcp_servers s
`

const serverMetadataSelect = `
	SELECT s.id, s.workspace_id, s.slug, s.display_name, s.transport, s.url,
	       s.command, s.args, s.functional_revision, s.enabled, s.revoked_at,
	       s.status, s.protocol_version,
	       s.server_info, s.last_error, s.last_probed_at, s.last_handshake_at,
	       s.created_by, s.created_at, s.updated_at, s.deleted_at,
	       (SELECT COUNT(*) FROM weave_mcp_tools t
	         WHERE t.workspace_id=s.workspace_id AND t.server_id=s.id),
	       (SELECT COUNT(*)
	          FROM weave_agents a
	         WHERE a.workspace_id=s.workspace_id
	           AND a.deleted=false
	           AND COALESCE(a.spec->'mcp_servers', '[]'::jsonb)
	               @> jsonb_build_array(jsonb_build_object('server_id', s.id)))
	FROM weave_mcp_servers s
`

type rowScanner interface {
	Scan(dest ...any) error
}

func (s *Store) scanView(row rowScanner) (ServerView, error) {
	var (
		view          ServerView
		urlValue      *string
		commandValue  *string
		argsJSON      []byte
		serverInfo    []byte
		headersCipher string
		envCipher     string
	)
	err := row.Scan(
		&view.ID, &view.WorkspaceID, &view.Slug, &view.DisplayName,
		&view.Transport, &urlValue, &commandValue, &argsJSON,
		&view.FunctionalRevision, &view.Enabled, &view.RevokedAt,
		&view.Status, &view.ProtocolVersion, &serverInfo, &view.LastError,
		&view.LastProbedAt, &view.LastHandshakeAt, &view.CreatedBy,
		&view.CreatedAt, &view.UpdatedAt, &view.DeletedAt, &headersCipher, &envCipher,
		&view.ToolCount, &view.AgentCount,
	)
	if err != nil {
		return ServerView{}, err
	}
	if err := populateServerJSON(&view.Server, urlValue, commandValue, argsJSON, serverInfo); err != nil {
		return ServerView{}, err
	}
	headers, err := s.openMap(headersCipher)
	if err != nil {
		return ServerView{}, fmt.Errorf("decrypt MCP server headers: %w", err)
	}
	view.Headers = maskedHeaders(headers)
	env, err := s.openMap(envCipher)
	if err != nil {
		return ServerView{}, fmt.Errorf("decrypt MCP server environment: %w", err)
	}
	view.Env = maskedHeaders(env)
	return view, nil
}

func scanMetadata(row rowScanner) (ServerMetadata, error) {
	var (
		item         ServerMetadata
		urlValue     *string
		commandValue *string
		argsJSON     []byte
		serverInfo   []byte
	)
	err := row.Scan(
		&item.ID, &item.WorkspaceID, &item.Slug, &item.DisplayName,
		&item.Transport, &urlValue, &commandValue, &argsJSON,
		&item.FunctionalRevision, &item.Enabled, &item.RevokedAt,
		&item.Status, &item.ProtocolVersion, &serverInfo, &item.LastError,
		&item.LastProbedAt, &item.LastHandshakeAt, &item.CreatedBy,
		&item.CreatedAt, &item.UpdatedAt, &item.DeletedAt,
		&item.ToolCount, &item.AgentCount,
	)
	if err != nil {
		return ServerMetadata{}, err
	}
	if err := populateServerJSON(&item.Server, urlValue, commandValue, argsJSON, serverInfo); err != nil {
		return ServerMetadata{}, err
	}
	return item, nil
}

func populateServerJSON(server *Server, urlValue, commandValue *string, argsJSON, serverInfo []byte) error {
	if urlValue != nil {
		server.URL = *urlValue
	}
	if commandValue != nil {
		server.Command = *commandValue
	}
	if len(argsJSON) != 0 {
		if err := json.Unmarshal(argsJSON, &server.Args); err != nil {
			return fmt.Errorf("decode MCP server args: %w", err)
		}
	}
	server.ServerInfo = append(json.RawMessage(nil), serverInfo...)
	return nil
}

func (s *Store) requireKey() error {
	if len(s.key) != 32 {
		return ErrKeyUnavailable
	}
	return nil
}

func (s *Store) sealMap(values map[string]string) (string, error) {
	data, err := json.Marshal(values)
	if err != nil {
		return "", fmt.Errorf("encode MCP server secrets: %w", err)
	}
	sealed, err := secret.Seal(s.key, data)
	if err != nil {
		return "", fmt.Errorf("encrypt MCP server secrets: %w", err)
	}
	return sealed, nil
}

func (s *Store) openMap(ciphertext string) (map[string]string, error) {
	data, err := secret.Open(s.key, ciphertext)
	if err != nil {
		return nil, err
	}
	values := make(map[string]string)
	if err := json.Unmarshal(data, &values); err != nil {
		return nil, fmt.Errorf("decode MCP server secrets: %w", err)
	}
	return values, nil
}

func createSecretMap(input map[string]string) map[string]string {
	created := make(map[string]string)
	for key, value := range input {
		if value != "" && value != MaskedHeader {
			created[key] = value
		}
	}
	return created
}

func patchSecretMap(current, patch map[string]string, deleted []string) {
	for key, value := range patch {
		if value != "" && value != MaskedHeader {
			current[key] = value
		}
	}
	for _, key := range deleted {
		delete(current, key)
	}
}

func maskedHeaders(headers map[string]string) map[string]string {
	masked := make(map[string]string, len(headers))
	for key := range headers {
		masked[key] = MaskedHeader
	}
	return masked
}

func connectionValues(req UpsertServerRequest) (*string, *string) {
	if req.Transport == TransportStreamableHTTP {
		return &req.URL, nil
	}
	return nil, &req.Command
}

func optionalStringEqual(a, b *string) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

func normalizedArgs(args []string) []string {
	if args == nil {
		return []string{}
	}
	return args
}

func mapStoreError(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return coded(ErrConflict, "slug already exists")
	}
	return err
}
