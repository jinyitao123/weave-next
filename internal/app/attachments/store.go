package attachments

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var ErrNotFound = errors.New("attachment not found")

type Attachment struct {
	ID          string    `json:"id"`
	WorkspaceID string    `json:"workspace_id"`
	Filename    string    `json:"filename"`
	SizeBytes   int64     `json:"size"`
	SHA256      string    `json:"sha256"`
	ContentType string    `json:"content_type"`
	CreatedBy   string    `json:"created_by"`
	CreatedAt   time.Time `json:"created_at"`
}

type Store struct {
	pool *pgxpool.Pool
}

func New(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

func (s *Store) Create(ctx context.Context, attachment Attachment) (Attachment, error) {
	attachment.Filename = strings.TrimSpace(attachment.Filename)
	attachment.ContentType = strings.TrimSpace(attachment.ContentType)
	if attachment.ContentType == "" {
		attachment.ContentType = "application/octet-stream"
	}
	created, err := scanAttachment(s.pool.QueryRow(ctx, `
		INSERT INTO weave_attachments (
			id,workspace_id,filename,size_bytes,sha256,content_type,created_by
		) VALUES ($1,$2,$3,$4,$5,$6,$7)
		RETURNING id,workspace_id,filename,size_bytes,sha256,content_type,created_by,created_at
	`, attachment.ID, attachment.WorkspaceID, attachment.Filename, attachment.SizeBytes,
		attachment.SHA256, attachment.ContentType, attachment.CreatedBy))
	if err != nil {
		return Attachment{}, fmt.Errorf("create attachment catalog record: %w", err)
	}
	return created, nil
}

func (s *Store) Get(ctx context.Context, workspaceID, id string) (Attachment, error) {
	attachment, err := scanAttachment(s.pool.QueryRow(ctx, `
		SELECT id,workspace_id,filename,size_bytes,sha256,content_type,created_by,created_at
		FROM weave_attachments WHERE workspace_id=$1 AND id=$2
	`, workspaceID, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Attachment{}, ErrNotFound
	}
	if err != nil {
		return Attachment{}, fmt.Errorf("get attachment: %w", err)
	}
	return attachment, nil
}

func (s *Store) List(ctx context.Context, workspaceID string) ([]Attachment, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id,workspace_id,filename,size_bytes,sha256,content_type,created_by,created_at
		FROM weave_attachments WHERE workspace_id=$1
		ORDER BY created_at DESC,id
	`, workspaceID)
	if err != nil {
		return nil, fmt.Errorf("list attachments: %w", err)
	}
	defer rows.Close()
	items := make([]Attachment, 0)
	for rows.Next() {
		item, err := scanAttachment(rows)
		if err != nil {
			return nil, fmt.Errorf("scan attachment: %w", err)
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list attachments: %w", err)
	}
	return items, nil
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanAttachment(row rowScanner) (Attachment, error) {
	var attachment Attachment
	err := row.Scan(
		&attachment.ID, &attachment.WorkspaceID, &attachment.Filename,
		&attachment.SizeBytes, &attachment.SHA256, &attachment.ContentType,
		&attachment.CreatedBy, &attachment.CreatedAt,
	)
	return attachment, err
}
