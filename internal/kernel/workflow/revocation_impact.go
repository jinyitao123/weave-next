package workflow

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jinyitao123/weave/internal/base/frozen"
	"github.com/jinyitao123/weave/internal/kernel/revocation"
)

var (
	ErrRevocationImpactInvalid  = errors.New("invalid revocation impact query")
	ErrRevocationImpactNotFound = errors.New("revocation impact relation not found")
)

const revocationImpactCursorSchemaVersion = 1

type RevocationImpactQuery struct {
	WorkspaceID   string
	TeamID        string
	WorkerAgentID string
	Limit         int
	Cursor        string
}

type RevocationImpactRelation struct {
	WorkerAgentID string   `json:"worker_agent_id"`
	Enabled       bool     `json:"enabled"`
	AllowedKinds  []string `json:"allowed_kinds"`
	DefaultKind   string   `json:"default_kind"`
}

type RevocationImpactItem struct {
	WorkflowID             string   `json:"workflow_id"`
	WorkflowVersion        int      `json:"workflow_version"`
	VersionStatus          string   `json:"version_status"`
	DraftReferencedKinds   []string `json:"draft_referenced_kinds"`
	PublishedFrozenKinds   []string `json:"published_frozen_kinds"`
	AuthorizationTightened bool     `json:"authorization_tightened"`
	Blocked                *bool    `json:"blocked"`
}

type RevocationImpactPage struct {
	Relation   RevocationImpactRelation `json:"relation"`
	Items      []RevocationImpactItem   `json:"items"`
	NextCursor *string                  `json:"next_cursor"`
}

type revocationImpactCursor struct {
	SchemaVersion   int    `json:"schema_version"`
	WorkflowID      string `json:"workflow_id"`
	WorkflowVersion int    `json:"workflow_version"`
}

func (s *Store) ReadRevocationImpact(
	ctx context.Context,
	query RevocationImpactQuery,
) (*RevocationImpactPage, error) {
	if query.WorkspaceID == "" || query.TeamID == "" || query.WorkerAgentID == "" {
		return nil, fmt.Errorf("%w: workspace, team, and worker are required", ErrRevocationImpactInvalid)
	}
	if query.Limit == 0 {
		query.Limit = 100
	}
	if query.Limit < 1 || query.Limit > 100 {
		return nil, fmt.Errorf("%w: limit must be between 1 and 100", ErrRevocationImpactInvalid)
	}
	cursor, err := decodeRevocationImpactCursor(query.Cursor)
	if err != nil {
		return nil, err
	}

	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{
		IsoLevel:   pgx.RepeatableRead,
		AccessMode: pgx.ReadOnly,
	})
	if err != nil {
		return nil, fmt.Errorf("begin revocation impact read: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	page := &RevocationImpactPage{Items: make([]RevocationImpactItem, 0, query.Limit)}
	if err := tx.QueryRow(ctx, `
		SELECT worker_agent_id, enabled, allowed_kinds, default_kind
		FROM weave_team_workers
		WHERE workspace_id=$1 AND team_id=$2 AND worker_agent_id=$3
	`, query.WorkspaceID, query.TeamID, query.WorkerAgentID).Scan(
		&page.Relation.WorkerAgentID,
		&page.Relation.Enabled,
		&page.Relation.AllowedKinds,
		&page.Relation.DefaultKind,
	); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf(
				"%w: team %q worker %q",
				ErrRevocationImpactNotFound,
				query.TeamID,
				query.WorkerAgentID,
			)
		}
		return nil, fmt.Errorf("read revocation impact relation: %w", err)
	}
	page.Relation.AllowedKinds = append([]string(nil), page.Relation.AllowedKinds...)

	rows, err := tx.Query(ctx, `
		SELECT
			version.workflow_id,
			version.version,
			version.status,
			CASE WHEN version.status='draft' THEN version.graph_definition END,
			artifact.artifact_schema_version,
			artifact.canonicalization_algorithm,
			artifact.canonicalization_version,
			artifact.hash_algorithm,
			artifact.content_hash,
			artifact.payload,
			admission.blocked
		FROM weave_team_workflows AS workflow
		JOIN weave_team_workflow_versions AS version
		  ON version.workspace_id=workflow.workspace_id
		 AND version.workflow_id=workflow.id
		LEFT JOIN weave_published_artifact_contents AS artifact
		  ON artifact.workspace_id=version.workspace_id
		 AND artifact.workflow_id=version.workflow_id
		 AND artifact.workflow_version=version.version
		LEFT JOIN weave_workflow_version_admission_statuses AS admission
		  ON admission.workspace_id=version.workspace_id
		 AND admission.workflow_id=version.workflow_id
		 AND admission.workflow_version=version.version
		WHERE workflow.workspace_id=$1
		  AND workflow.team_id=$2
		  AND (
			version.workflow_id COLLATE "C" > $3 COLLATE "C"
			OR (version.workflow_id=$3 AND version.version>$4)
		  )
		ORDER BY version.workflow_id COLLATE "C", version.version
	`, query.WorkspaceID, query.TeamID, cursor.WorkflowID, cursor.WorkflowVersion)
	if err != nil {
		return nil, fmt.Errorf("query revocation impact versions: %w", err)
	}

	for rows.Next() {
		var (
			workflowID                string
			workflowVersion           int
			versionStatus             string
			draftGraph                []byte
			artifactSchemaVersion     *int
			canonicalizationAlgorithm *string
			canonicalizationVersion   *int
			hashAlgorithm             *string
			contentHash               *string
			artifactPayload           []byte
			blocked                   *bool
		)
		if err := rows.Scan(
			&workflowID,
			&workflowVersion,
			&versionStatus,
			&draftGraph,
			&artifactSchemaVersion,
			&canonicalizationAlgorithm,
			&canonicalizationVersion,
			&hashAlgorithm,
			&contentHash,
			&artifactPayload,
			&blocked,
		); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan revocation impact version: %w", err)
		}

		item := RevocationImpactItem{
			WorkflowID:           workflowID,
			WorkflowVersion:      workflowVersion,
			VersionStatus:        versionStatus,
			DraftReferencedKinds: make([]string, 0),
			PublishedFrozenKinds: make([]string, 0),
		}
		switch versionStatus {
		case VersionStatusDraft:
			if len(draftGraph) == 0 || artifactSchemaVersion != nil || blocked != nil {
				rows.Close()
				return nil, errors.New("draft revocation impact facts are inconsistent")
			}
			references, err := revocation.ExtractGraphReferences(draftGraph)
			if err != nil {
				rows.Close()
				return nil, fmt.Errorf("decode draft revocation references: %w", err)
			}
			item.DraftReferencedKinds = revocationKindsForWorker(references, query.WorkerAgentID)
		case VersionStatusPublished:
			if artifactSchemaVersion == nil || canonicalizationAlgorithm == nil ||
				canonicalizationVersion == nil || hashAlgorithm == nil || contentHash == nil ||
				len(artifactPayload) == 0 || blocked == nil {
				rows.Close()
				return nil, errors.New("published revocation impact facts are incomplete")
			}
			payload, err := frozen.DecodeArtifactEnvelopeV1(frozen.ArtifactEnvelopeV1{
				WorkspaceID:               query.WorkspaceID,
				WorkflowID:                workflowID,
				WorkflowVersion:           workflowVersion,
				ArtifactSchemaVersion:     *artifactSchemaVersion,
				CanonicalizationAlgorithm: *canonicalizationAlgorithm,
				CanonicalizationVersion:   *canonicalizationVersion,
				HashAlgorithm:             *hashAlgorithm,
				ContentHash:               *contentHash,
				Payload:                   artifactPayload,
			})
			if err != nil {
				rows.Close()
				return nil, fmt.Errorf("decode published revocation artifact: %w", err)
			}
			if payload.Team.WorkspaceID != query.WorkspaceID || payload.Team.TeamID != query.TeamID {
				rows.Close()
				return nil, errors.New("published revocation artifact team does not match workflow")
			}
			references, err := revocation.ExtractGraphReferences(payload.GraphDefinition)
			if err != nil {
				rows.Close()
				return nil, fmt.Errorf("decode published revocation references: %w", err)
			}
			item.PublishedFrozenKinds = revocationKindsForWorker(references, query.WorkerAgentID)
			item.AuthorizationTightened = revocationAuthorizationTightened(
				item.PublishedFrozenKinds,
				page.Relation.AllowedKinds,
			)
			item.Blocked = blocked
		default:
			rows.Close()
			return nil, fmt.Errorf("unsupported workflow version status %q", versionStatus)
		}

		if len(item.DraftReferencedKinds) == 0 && len(item.PublishedFrozenKinds) == 0 {
			continue
		}
		page.Items = append(page.Items, item)
		if len(page.Items) > query.Limit {
			break
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("iterate revocation impact versions: %w", err)
	}
	rows.Close()

	if len(page.Items) > query.Limit {
		page.Items = page.Items[:query.Limit]
		last := page.Items[len(page.Items)-1]
		nextCursor, err := encodeRevocationImpactCursor(revocationImpactCursor{
			SchemaVersion:   revocationImpactCursorSchemaVersion,
			WorkflowID:      last.WorkflowID,
			WorkflowVersion: last.WorkflowVersion,
		})
		if err != nil {
			return nil, err
		}
		page.NextCursor = &nextCursor
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit revocation impact read: %w", err)
	}
	return page, nil
}

func revocationKindsForWorker(references []revocation.Reference, workerID string) []string {
	kinds := make([]string, 0)
	for _, reference := range references {
		if reference.WorkerAgentID == workerID {
			kinds = append(kinds, reference.Kind)
		}
	}
	return kinds
}

func revocationAuthorizationTightened(frozenKinds, currentKinds []string) bool {
	current := make(map[string]struct{}, len(currentKinds))
	for _, kind := range currentKinds {
		current[kind] = struct{}{}
	}
	for _, kind := range frozenKinds {
		if _, allowed := current[kind]; !allowed {
			return true
		}
	}
	return false
}

func decodeRevocationImpactCursor(raw string) (revocationImpactCursor, error) {
	if raw == "" {
		return revocationImpactCursor{SchemaVersion: revocationImpactCursorSchemaVersion}, nil
	}
	decoded, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil || base64.RawURLEncoding.EncodeToString(decoded) != raw {
		return revocationImpactCursor{}, fmt.Errorf("%w: cursor is not canonical base64url", ErrRevocationImpactInvalid)
	}
	decoder := json.NewDecoder(bytes.NewReader(decoded))
	decoder.DisallowUnknownFields()
	var cursor revocationImpactCursor
	if err := decoder.Decode(&cursor); err != nil {
		return revocationImpactCursor{}, fmt.Errorf("%w: decode cursor: %v", ErrRevocationImpactInvalid, err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return revocationImpactCursor{}, fmt.Errorf("%w: cursor has trailing JSON", ErrRevocationImpactInvalid)
	}
	if cursor.SchemaVersion != revocationImpactCursorSchemaVersion || cursor.WorkflowID == "" ||
		cursor.WorkflowID != strings.TrimSpace(cursor.WorkflowID) || cursor.WorkflowVersion < 1 {
		return revocationImpactCursor{}, fmt.Errorf("%w: cursor identity is invalid", ErrRevocationImpactInvalid)
	}
	canonical, err := json.Marshal(cursor)
	if err != nil || !bytes.Equal(canonical, decoded) {
		return revocationImpactCursor{}, fmt.Errorf("%w: cursor JSON is not canonical", ErrRevocationImpactInvalid)
	}
	return cursor, nil
}

func encodeRevocationImpactCursor(cursor revocationImpactCursor) (string, error) {
	if cursor.SchemaVersion != revocationImpactCursorSchemaVersion || cursor.WorkflowID == "" ||
		cursor.WorkflowVersion < 1 {
		return "", errors.New("encode revocation impact cursor: invalid identity")
	}
	raw, err := json.Marshal(cursor)
	if err != nil {
		return "", fmt.Errorf("encode revocation impact cursor: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}
