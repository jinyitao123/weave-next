package teambuild

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/jackc/pgx/v5"
)

// ErrBuildDraftConflict rejects a stale concurrent draft replacement.
var ErrBuildDraftConflict = errors.New("build draft changed concurrently")

// LoadDraft reads one workspace and build-run scoped draft payload.
func (s *Store) LoadDraft(
	ctx context.Context,
	workspaceID, buildRunID, kind, key string,
) (json.RawMessage, int64, bool, error) {
	if s == nil || s.pool == nil {
		return nil, 0, false, errors.New("build draft store unavailable")
	}
	var payload json.RawMessage
	var revision int64
	err := s.pool.QueryRow(ctx, `
		SELECT payload_json, revision
		FROM weave_team_build_drafts
		WHERE workspace_id=$1 AND build_run_id=$2 AND draft_kind=$3 AND draft_key=$4
	`, strings.TrimSpace(workspaceID), strings.TrimSpace(buildRunID), strings.TrimSpace(kind), strings.TrimSpace(key)).Scan(&payload, &revision)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, 0, false, nil
		}
		return nil, 0, false, err
	}
	return append(json.RawMessage(nil), payload...), revision, true, nil
}

// SaveDraft durably replaces one build-run draft. The parent build run and
// composite key enforce workspace isolation.
func (s *Store) SaveDraft(
	ctx context.Context,
	workspaceID, buildRunID, kind, key string,
	expectedRevision int64,
	payload json.RawMessage,
) (int64, error) {
	if s == nil || s.pool == nil {
		return 0, errors.New("build draft store unavailable")
	}
	var revision int64
	workspaceID = strings.TrimSpace(workspaceID)
	buildRunID = strings.TrimSpace(buildRunID)
	kind = strings.TrimSpace(kind)
	key = strings.TrimSpace(key)
	var err error
	if expectedRevision == 0 {
		err = s.pool.QueryRow(ctx, `
			INSERT INTO weave_team_build_drafts(
				workspace_id, build_run_id, draft_kind, draft_key, payload_json, revision, updated_at
			) VALUES($1,$2,$3,$4,$5,1,NOW())
			ON CONFLICT(workspace_id, build_run_id, draft_kind, draft_key) DO UPDATE SET
				payload_json=EXCLUDED.payload_json
			WHERE weave_team_build_drafts.payload_json=EXCLUDED.payload_json
			RETURNING revision
		`, workspaceID, buildRunID, kind, key, payload).Scan(&revision)
	} else {
		err = s.pool.QueryRow(ctx, `
			UPDATE weave_team_build_drafts SET
				payload_json=$5,
				revision=CASE
					WHEN payload_json=$5 THEN revision
					ELSE revision+1
				END,
				updated_at=CASE
					WHEN payload_json=$5 THEN updated_at
					ELSE NOW()
				END
			WHERE workspace_id=$1 AND build_run_id=$2 AND draft_kind=$3 AND draft_key=$4
			  AND (revision=$6 OR payload_json=$5)
			RETURNING revision
		`, workspaceID, buildRunID, kind, key, payload, expectedRevision).Scan(&revision)
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, ErrBuildDraftConflict
	}
	return revision, err
}

// DeleteDraft removes one completed or discarded build-run draft.
func (s *Store) DeleteDraft(
	ctx context.Context,
	workspaceID, buildRunID, kind, key string,
	expectedRevision int64,
) error {
	if s == nil || s.pool == nil {
		return errors.New("build draft store unavailable")
	}
	result, err := s.pool.Exec(ctx, `
		DELETE FROM weave_team_build_drafts
		WHERE workspace_id=$1 AND build_run_id=$2 AND draft_kind=$3 AND draft_key=$4
		  AND revision=$5
	`, strings.TrimSpace(workspaceID), strings.TrimSpace(buildRunID), strings.TrimSpace(kind), strings.TrimSpace(key), expectedRevision)
	if err != nil || result.RowsAffected() == 1 {
		return err
	}
	_, _, found, loadErr := s.LoadDraft(ctx, workspaceID, buildRunID, kind, key)
	if loadErr != nil {
		return loadErr
	}
	if !found {
		return nil
	}
	return ErrBuildDraftConflict
}
