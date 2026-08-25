package testutil

import (
	"context"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// PostgresPool returns a pool isolated to a temporary schema.
func PostgresPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	databaseURL := os.Getenv("TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}

	ctx := context.Background()
	admin, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatalf("connect test database: %v", err)
	}
	schemaName := "test_" + uuid.NewString()
	quotedSchema := pgx.Identifier{schemaName}.Sanitize()
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+quotedSchema); err != nil {
		admin.Close()
		t.Fatalf("create test schema: %v", err)
	}

	cfg, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		admin.Close()
		t.Fatalf("parse test database URL: %v", err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = quotedSchema
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		admin.Close()
		t.Fatalf("connect test schema: %v", err)
	}

	t.Cleanup(func() {
		pool.Close()
		_, _ = admin.Exec(context.Background(), "DROP SCHEMA "+quotedSchema+" CASCADE")
		admin.Close()
	})
	return pool
}

// PostgresVectorPool returns a schema-isolated pool with the pgvector
// extension available, skipping the test when the server lacks pgvector.
// The extension is installed into the public schema (extensions are
// database-global, so per-test schemas would collide) and public is kept on
// the search_path so the vector type stays resolvable; test tables still land
// in the isolated schema, which is first on the search_path.
func PostgresVectorPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	databaseURL := os.Getenv("TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}

	ctx := context.Background()
	admin, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatalf("connect test database: %v", err)
	}
	if _, err := admin.Exec(ctx, "CREATE EXTENSION IF NOT EXISTS vector WITH SCHEMA public"); err != nil {
		admin.Close()
		t.Skipf("pgvector extension unavailable: %v", err)
	}
	schemaName := "test_" + uuid.NewString()
	quotedSchema := pgx.Identifier{schemaName}.Sanitize()
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+quotedSchema); err != nil {
		admin.Close()
		t.Fatalf("create test schema: %v", err)
	}

	cfg, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		admin.Close()
		t.Fatalf("parse test database URL: %v", err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = quotedSchema + ",public"
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		admin.Close()
		t.Fatalf("connect test schema: %v", err)
	}

	t.Cleanup(func() {
		pool.Close()
		_, _ = admin.Exec(context.Background(), "DROP SCHEMA "+quotedSchema+" CASCADE")
		admin.Close()
	})
	return pool
}
