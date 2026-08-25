// Package storeext provides platform-level extensions to PGStore that are NOT
// part of the Loom kernel Store interface. These methods exist because Weave
// (the platform) needs pagination, retention cleanup, and bulk operations that
// are beyond the kernel's minimal Get/Put/Delete/List/Tx contract.
package storeext

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// escapeLike escapes LIKE special characters (%, _) so they are matched literally.
func escapeLike(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `%`, `\%`)
	s = strings.ReplaceAll(s, `_`, `\_`)
	return s
}

// PGExt provides platform-level query methods on the loom_store table.
// It operates on the same table as PGStore but through its own pool reference,
// keeping these methods out of the kernel package.
type PGExt struct {
	pool *pgxpool.Pool
}

// New creates a PGExt from a connection pool.
func New(pool *pgxpool.Pool) *PGExt {
	return &PGExt{pool: pool}
}

// BeginTx starts a caller-owned transaction. The caller is responsible for
// committing or rolling it back.
func (e *PGExt) BeginTx(ctx context.Context) (pgx.Tx, error) {
	if e == nil || e.pool == nil {
		return nil, fmt.Errorf("storeext: pool is unavailable")
	}
	tx, err := e.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("storeext: begin transaction: %w", err)
	}
	return tx, nil
}

// LockValueTx acquires the same transaction-scoped advisory lock used by
// MutateValue for one physical loom_store key.
func (e *PGExt) LockValueTx(
	ctx context.Context,
	tx pgx.Tx,
	namespace string,
	key string,
) error {
	if e == nil || e.pool == nil {
		return fmt.Errorf("storeext: pool is unavailable")
	}
	if tx == nil {
		return fmt.Errorf("storeext: tx must be non-nil")
	}
	if namespace == "" || key == "" {
		return fmt.Errorf("storeext: namespace and key must be non-empty")
	}
	if _, err := tx.Exec(
		ctx,
		`SELECT pg_advisory_xact_lock(hashtextextended($1, 0) # hashtextextended($2, 0))`,
		namespace,
		key,
	); err != nil {
		return fmt.Errorf("storeext: advisory lock: %w", err)
	}
	return nil
}

// ReadValueTx reads one exact loom_store value inside a caller-owned
// transaction.
func (e *PGExt) ReadValueTx(
	ctx context.Context,
	tx pgx.Tx,
	namespace string,
	key string,
) ([]byte, bool, error) {
	if e == nil || e.pool == nil {
		return nil, false, fmt.Errorf("storeext: pool is unavailable")
	}
	if tx == nil {
		return nil, false, fmt.Errorf("storeext: tx must be non-nil")
	}
	if namespace == "" || key == "" {
		return nil, false, fmt.Errorf("storeext: namespace and key must be non-empty")
	}
	var value []byte
	err := tx.QueryRow(
		ctx,
		`SELECT value FROM loom_store WHERE namespace=$1 AND key=$2`,
		namespace,
		key,
	).Scan(&value)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("storeext: read value tx: %w", err)
	}
	return bytes.Clone(value), true, nil
}

// PutValueTx writes one exact loom_store value inside a caller-owned
// transaction. The caller must already hold LockValueTx for this key.
func (e *PGExt) PutValueTx(
	ctx context.Context,
	tx pgx.Tx,
	namespace string,
	key string,
	value []byte,
) error {
	if e == nil || e.pool == nil {
		return fmt.Errorf("storeext: pool is unavailable")
	}
	if tx == nil {
		return fmt.Errorf("storeext: tx must be non-nil")
	}
	if namespace == "" || key == "" {
		return fmt.Errorf("storeext: namespace and key must be non-empty")
	}
	if _, err := tx.Exec(
		ctx,
		`INSERT INTO loom_store (namespace, key, value, updated_at)
		 VALUES ($1, $2, $3, NOW())
		 ON CONFLICT (namespace, key)
		 DO UPDATE SET value=EXCLUDED.value, updated_at=NOW()`,
		namespace,
		key,
		bytes.Clone(value),
	); err != nil {
		return fmt.Errorf("storeext: put value tx: %w", err)
	}
	return nil
}

// InsertValueTx inserts one exact loom_store value inside a caller-owned
// transaction without changing an existing row.
func (e *PGExt) InsertValueTx(
	ctx context.Context,
	tx pgx.Tx,
	namespace string,
	key string,
	value []byte,
) (bool, error) {
	if e == nil || e.pool == nil {
		return false, fmt.Errorf("storeext: pool is unavailable")
	}
	if tx == nil {
		return false, fmt.Errorf("storeext: tx must be non-nil")
	}
	if namespace == "" || key == "" {
		return false, fmt.Errorf("storeext: namespace and key must be non-empty")
	}
	tag, err := tx.Exec(
		ctx,
		`INSERT INTO loom_store (namespace, key, value, updated_at)
		 VALUES ($1, $2, $3, NOW())
		 ON CONFLICT (namespace, key) DO NOTHING`,
		namespace,
		key,
		bytes.Clone(value),
	)
	if err != nil {
		return false, fmt.Errorf("storeext: insert value tx: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// MutateValue atomically read-modify-writes one loom_store value under a
// Postgres transaction-scoped advisory lock keyed on (ns,key). The lock
// serializes concurrent mutators of the same key ACROSS processes (the API
// server and the separate jobs worker), which the kernel's whole-blob Put
// cannot do — a plain Get→modify→Put loses updates when two writers interleave.
// mutate receives the current value and its exact presence and returns the new
// value; returning the input unchanged is a no-op write.
func (e *PGExt) MutateValue(ctx context.Context, ns, key string, mutate func(current []byte, present bool) ([]byte, error)) error {
	if e == nil || e.pool == nil {
		return fmt.Errorf("storeext: pool is unavailable")
	}
	tx, err := e.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("storeext: begin mutate: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// hashtextextended gives a stable 64-bit lock key. Hash ns and key
	// separately and XOR — Postgres text cannot contain a null byte, so a
	// "ns\x00key" separator would be rejected (22021); XORing two hashes keeps
	// the lock domain exactly this one (ns,key) pair with no separator.
	if err := e.LockValueTx(ctx, tx, ns, key); err != nil {
		return err
	}

	var current []byte
	err = tx.QueryRow(ctx,
		`SELECT value FROM loom_store WHERE namespace=$1 AND key=$2`, ns, key,
	).Scan(&current)
	present := true
	if errors.Is(err, pgx.ErrNoRows) {
		present = false
		current = nil
		err = nil
	}
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("storeext: mutate get: %w", err)
	}

	next, err := mutate(bytes.Clone(current), present)
	if err != nil {
		return err
	}

	if _, err := tx.Exec(ctx,
		`INSERT INTO loom_store (namespace, key, value, updated_at)
		 VALUES ($1, $2, $3, NOW())
		 ON CONFLICT (namespace, key) DO UPDATE SET value=EXCLUDED.value, updated_at=NOW()`,
		ns, key, next,
	); err != nil {
		return fmt.Errorf("storeext: mutate put: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("storeext: commit mutate: %w", err)
	}
	return nil
}

// ReadValue reads one exact loom_store value and distinguishes a missing row
// from a present empty BYTEA.
func (e *PGExt) ReadValue(ctx context.Context, ns, key string) ([]byte, bool, error) {
	var value []byte
	err := e.pool.QueryRow(ctx,
		`SELECT value FROM loom_store WHERE namespace=$1 AND key=$2`, ns, key,
	).Scan(&value)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("storeext: read value: %w", err)
	}
	return bytes.Clone(value), true, nil
}

// ListKeys returns every key in one exact namespace in lexical order.
func (e *PGExt) ListKeys(ctx context.Context, namespace string) ([]string, error) {
	rows, err := e.pool.Query(
		ctx,
		`SELECT key FROM loom_store WHERE namespace=$1 ORDER BY key`,
		namespace,
	)
	if err != nil {
		return nil, fmt.Errorf("storeext: list keys: %w", err)
	}
	defer rows.Close()

	keys := make([]string, 0)
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			return nil, fmt.Errorf("storeext: list keys scan: %w", err)
		}
		keys = append(keys, key)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("storeext: list keys rows: %w", err)
	}
	return keys, nil
}

// DeleteByNamespace removes all keys in namespaces matching a LIKE pattern.
func (e *PGExt) DeleteByNamespace(ctx context.Context, nsPattern string) (int64, error) {
	tag, err := e.pool.Exec(ctx,
		`DELETE FROM loom_store WHERE namespace LIKE $1`, nsPattern)
	if err != nil {
		return 0, fmt.Errorf("storeext: delete by namespace: %w", err)
	}
	return tag.RowsAffected(), nil
}

// CleanupOlderThan deletes rows in namespaces matching nsPattern
// whose updated_at is older than cutoff. Returns rows deleted.
func (e *PGExt) CleanupOlderThan(ctx context.Context, nsPattern string, cutoff string) (int64, error) {
	tag, err := e.pool.Exec(ctx,
		`DELETE FROM loom_store WHERE namespace LIKE $1 AND updated_at < $2::timestamptz`,
		nsPattern, cutoff)
	if err != nil {
		return 0, fmt.Errorf("storeext: cleanup older than: %w", err)
	}
	return tag.RowsAffected(), nil
}

// ListPaginated returns keys with ordering and pagination.
func (e *PGExt) ListPaginated(ctx context.Context, ns, prefix string, limit, offset int) ([]string, error) {
	rows, err := e.pool.Query(ctx,
		`SELECT key FROM loom_store WHERE namespace=$1 AND key LIKE $2||'%' ESCAPE '\'
		 ORDER BY updated_at DESC LIMIT $3 OFFSET $4`, ns, escapeLike(prefix), limit, offset)
	if err != nil {
		return nil, fmt.Errorf("storeext: list paginated: %w", err)
	}
	defer rows.Close()

	var keys []string
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			return nil, fmt.Errorf("storeext: list paginated scan: %w", err)
		}
		keys = append(keys, k)
	}
	return keys, rows.Err()
}

// GetByKeys returns values for multiple keys in a single query.
// Returns a map of key → value for found keys.
func (e *PGExt) GetByKeys(ctx context.Context, ns string, keys []string) (map[string][]byte, error) {
	if len(keys) == 0 {
		return nil, nil
	}
	rows, err := e.pool.Query(ctx,
		`SELECT key, value FROM loom_store WHERE namespace=$1 AND key = ANY($2)`, ns, keys)
	if err != nil {
		return nil, fmt.Errorf("storeext: get by keys: %w", err)
	}
	defer rows.Close()

	result := make(map[string][]byte, len(keys))
	for rows.Next() {
		var k string
		var v []byte
		if err := rows.Scan(&k, &v); err != nil {
			return nil, fmt.Errorf("storeext: get by keys scan: %w", err)
		}
		result[k] = v
	}
	return result, rows.Err()
}

// AggregateUsage performs server-side aggregation of audit entries.
// Returns per-agent usage stats without loading all rows into application memory.
type UsageAgg struct {
	Agent     string
	Runs      int
	TokensIn  int
	TokensOut int
	CostUSD   float64
}

func (e *PGExt) AggregateUsage(ctx context.Context, ns string) ([]UsageAgg, error) {
	rows, err := e.pool.Query(ctx, `
		SELECT
			COALESCE(value->>'agent', 'unknown') AS agent,
			COUNT(*)::int AS runs,
			COALESCE(SUM((value->>'tokens_in')::int), 0)::int AS tokens_in,
			COALESCE(SUM((value->>'tokens_out')::int), 0)::int AS tokens_out,
			COALESCE(SUM((value->>'cost_usd')::numeric), 0)::float8 AS cost_usd
		FROM loom_store
		WHERE namespace = $1
		GROUP BY COALESCE(value->>'agent', 'unknown')
	`, ns)
	if err != nil {
		return nil, fmt.Errorf("storeext: aggregate usage: %w", err)
	}
	defer rows.Close()

	var results []UsageAgg
	for rows.Next() {
		var a UsageAgg
		if err := rows.Scan(&a.Agent, &a.Runs, &a.TokensIn, &a.TokensOut, &a.CostUSD); err != nil {
			return nil, fmt.Errorf("storeext: aggregate usage scan: %w", err)
		}
		results = append(results, a)
	}
	return results, rows.Err()
}

// CountByNamespace returns the number of rows in a namespace.
func (e *PGExt) CountByNamespace(ctx context.Context, ns string) (int, error) {
	var count int
	err := e.pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM loom_store WHERE namespace=$1`, ns).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("storeext: count: %w", err)
	}
	return count, nil
}
