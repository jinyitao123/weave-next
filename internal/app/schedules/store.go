package schedules

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Schedule represents a connector configuration (name override + sync schedule).
type Schedule struct {
	SourceURL   string    `json:"source_url"`
	Tenant      string    `json:"tenant"`
	DisplayName string    `json:"display_name,omitempty"` // user-defined name override
	SyncMode    string    `json:"sync_mode"`              // batch, event, manual
	Frequency   string    `json:"frequency"`              // hourly, daily, weekly
	TimeOfDay   string    `json:"time_of_day"`            // HH:MM
	Strategy    string    `json:"strategy"`               // full, incremental
	Conflict    string    `json:"conflict"`               // source, local, mark
	Tables      []string  `json:"tables"`                 // selected source tables
	UpdatedAt   time.Time `json:"updated_at"`
}

// Store provides CRUD operations on the weave_schedules table.
type Store struct {
	pool *pgxpool.Pool
}

// NewStore creates a new schedule store.
func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

// Get returns the schedule for a given source URL and tenant.
func (s *Store) Get(ctx context.Context, tenant, sourceURL string) (*Schedule, error) {
	var configJSON []byte
	var updatedAt time.Time
	err := s.pool.QueryRow(ctx,
		`SELECT config, updated_at FROM weave_schedules WHERE tenant = $1 AND source_url = $2`,
		tenant, sourceURL,
	).Scan(&configJSON, &updatedAt)
	if err != nil {
		return nil, err
	}

	var sched Schedule
	if err := json.Unmarshal(configJSON, &sched); err != nil {
		return nil, err
	}
	sched.SourceURL = sourceURL
	sched.Tenant = tenant
	sched.UpdatedAt = updatedAt
	return &sched, nil
}

// Upsert creates or updates a schedule for a given source URL.
func (s *Store) Upsert(ctx context.Context, sched *Schedule) error {
	configJSON, err := json.Marshal(sched)
	if err != nil {
		return err
	}
	_, err = s.pool.Exec(ctx, `
		INSERT INTO weave_schedules (source_url, tenant, config, updated_at)
		VALUES ($1, $2, $3, NOW())
		ON CONFLICT (tenant, source_url) DO UPDATE
		SET config = $3, updated_at = NOW()
	`, sched.SourceURL, sched.Tenant, configJSON)
	return err
}

// Delete removes a schedule for a given source URL and tenant.
func (s *Store) Delete(ctx context.Context, tenant, sourceURL string) error {
	tag, err := s.pool.Exec(ctx,
		`DELETE FROM weave_schedules WHERE tenant = $1 AND source_url = $2`,
		tenant, sourceURL,
	)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("schedule not found")
	}
	return nil
}

// List returns all schedules for a given tenant.
func (s *Store) List(ctx context.Context, tenant string) ([]Schedule, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT source_url, config, updated_at FROM weave_schedules WHERE tenant = $1 ORDER BY updated_at DESC`,
		tenant,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Schedule
	for rows.Next() {
		var sourceURL string
		var configJSON []byte
		var updatedAt time.Time
		if err := rows.Scan(&sourceURL, &configJSON, &updatedAt); err != nil {
			return nil, err
		}
		var sched Schedule
		if err := json.Unmarshal(configJSON, &sched); err != nil {
			continue
		}
		sched.SourceURL = sourceURL
		sched.Tenant = tenant
		sched.UpdatedAt = updatedAt
		out = append(out, sched)
	}
	return out, nil
}
