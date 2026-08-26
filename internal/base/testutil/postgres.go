package testutil

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jinyitao123/loom/pgstore"
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
	// 测试夹具常把多条 seed SQL 放进一次 Exec；pgx 默认 prepared 模式拒绝多语句，
	// 测试池统一走 simple protocol（语义等价，仅无语句缓存）。
	cfg.ConnConfig.DefaultQueryExecMode = pgx.QueryExecModeSimpleProtocol
	cfg.ConnConfig.RuntimeParams["search_path"] = quotedSchema
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		admin.Close()
		t.Fatalf("connect test schema: %v", err)
	}
	if err := migrateLoomStore(ctx, databaseURL, quotedSchema); err != nil {
		pool.Close()
		_, _ = admin.Exec(context.Background(), "DROP SCHEMA "+quotedSchema+" CASCADE")
		admin.Close()
		t.Fatalf("migrate loom test store: %v", err)
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
	cfg.ConnConfig.DefaultQueryExecMode = pgx.QueryExecModeSimpleProtocol
	cfg.ConnConfig.RuntimeParams["search_path"] = quotedSchema + ",public"
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		admin.Close()
		t.Fatalf("connect test schema: %v", err)
	}
	if err := migrateLoomStore(ctx, databaseURL, quotedSchema); err != nil {
		pool.Close()
		_, _ = admin.Exec(context.Background(), "DROP SCHEMA "+quotedSchema+" CASCADE")
		admin.Close()
		t.Fatalf("migrate loom test store: %v", err)
	}

	t.Cleanup(func() {
		pool.Close()
		_, _ = admin.Exec(context.Background(), "DROP SCHEMA "+quotedSchema+" CASCADE")
		admin.Close()
	})
	return pool
}

func migrateLoomStore(ctx context.Context, databaseURL, quotedSchema string) error {
	migrationURL, err := connectionStringWithSearchPath(databaseURL, quotedSchema)
	if err != nil {
		return err
	}
	store, err := pgstore.New(migrationURL)
	if err != nil {
		return err
	}
	defer store.Close()
	return store.Migrate(ctx)
}

func connectionStringWithSearchPath(databaseURL, quotedSchema string) (string, error) {
	if strings.Contains(databaseURL, "://") {
		parsed, err := url.Parse(databaseURL)
		if err != nil {
			return "", fmt.Errorf("parse database URL: %w", err)
		}
		query := parsed.Query()
		query.Set("search_path", quotedSchema)
		parsed.RawQuery = query.Encode()
		return parsed.String(), nil
	}
	return databaseURL + " search_path='" + strings.ReplaceAll(quotedSchema, "'", "''") + "'", nil
}
