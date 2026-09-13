package db

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed migrations/*.sql
var migrations embed.FS

// Migrate applies all pending database migrations in version order.
func Migrate(ctx context.Context, pool *pgxpool.Pool) error {
	return MigrateWithOptions(ctx, pool, MigrateOptions{
		LegacyScheduleTimezone: os.Getenv("WEAVE_LEGACY_SCHEDULE_TIMEZONE"),
	})
}

// MigrateOptions supplies explicit deployment facts required by data migrations.
type MigrateOptions struct {
	LegacyScheduleTimezone string
}

// MigrateWithOptions applies all pending migrations with explicit deployment facts.
func MigrateWithOptions(ctx context.Context, pool *pgxpool.Pool, opts MigrateOptions) error {
	return migrateFSWithOptions(ctx, pool, migrations, opts)
}

func migrateFS(ctx context.Context, pool *pgxpool.Pool, files fs.FS) error {
	return migrateFSWithOptions(ctx, pool, files, MigrateOptions{})
}

func migrateFSWithOptions(
	ctx context.Context,
	pool *pgxpool.Pool,
	files fs.FS,
	opts MigrateOptions,
) error {
	ledgerTx, err := beginLockedMigrationTx(ctx, pool)
	if err != nil {
		return fmt.Errorf("begin migration ledger: %w", err)
	}
	if _, err := ledgerTx.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS weave_schema_migrations (
			version TEXT PRIMARY KEY,
			applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
		)
	`); err != nil {
		_ = ledgerTx.Rollback(ctx)
		return fmt.Errorf("create migration ledger: %w", err)
	}
	if err := ledgerTx.Commit(ctx); err != nil {
		return fmt.Errorf("commit migration ledger: %w", err)
	}

	paths, err := migrationFiles(files)
	if err != nil {
		return err
	}
	for _, path := range paths {
		name := strings.TrimPrefix(path, "migrations/")
		version := strings.SplitN(name, "_", 2)[0]

		tx, err := beginLockedMigrationTx(ctx, pool)
		if err != nil {
			return fmt.Errorf("begin migration %s: %w", version, err)
		}

		var applied bool
		err = tx.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM weave_schema_migrations WHERE version=$1)`,
			version,
		).Scan(&applied)
		if err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("check migration %s: %w", version, err)
		}
		if applied {
			if err := tx.Commit(ctx); err != nil {
				return fmt.Errorf("commit migration %s: %w", version, err)
			}
			continue
		}
		if _, err := tx.Exec(ctx,
			`SELECT set_config('weave.legacy_schedule_timezone', $1, true)`,
			opts.LegacyScheduleTimezone,
		); err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("configure migration %s: %w", version, err)
		}

		sql, err := fs.ReadFile(files, path)
		if err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("read migration %s: %w", version, err)
		}
		if _, err := tx.Exec(ctx, string(sql)); err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("apply migration %s: %w", version, err)
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO weave_schema_migrations (version) VALUES ($1)`,
			version,
		); err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("record migration %s: %w", version, err)
		}
		if err := tx.Commit(ctx); err != nil {
			return fmt.Errorf("commit migration %s: %w", version, err)
		}
	}
	return nil
}

func beginLockedMigrationTx(ctx context.Context, pool *pgxpool.Pool) (pgx.Tx, error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, `
		SELECT pg_advisory_xact_lock(
			hashtextextended(current_schema() || ':weave_schema_migrations', 0)
		)
	`); err != nil {
		_ = tx.Rollback(ctx)
		return nil, fmt.Errorf("acquire migration lock: %w", err)
	}
	return tx, nil
}

func migrationFiles(files fs.FS) ([]string, error) {
	entries, err := fs.ReadDir(files, "migrations")
	if err != nil {
		return nil, fmt.Errorf("read migrations: %w", err)
	}

	type migrationFile struct {
		path    string
		version int
	}
	var found []migrationFile
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".sql") {
			continue
		}
		parts := strings.SplitN(entry.Name(), "_", 2)
		if len(parts) != 2 {
			return nil, fmt.Errorf("invalid migration filename %q", entry.Name())
		}
		version, err := strconv.Atoi(parts[0])
		if err != nil {
			return nil, fmt.Errorf("invalid migration filename %q", entry.Name())
		}
		found = append(found, migrationFile{
			path:    "migrations/" + entry.Name(),
			version: version,
		})
	}

	sort.Slice(found, func(i, j int) bool {
		if found[i].version == found[j].version {
			return found[i].path < found[j].path
		}
		return found[i].version < found[j].version
	})
	for i := 1; i < len(found); i++ {
		if found[i-1].version == found[i].version {
			return nil, fmt.Errorf("duplicate migration version %04d: %q and %q",
				found[i].version, strings.TrimPrefix(found[i-1].path, "migrations/"), strings.TrimPrefix(found[i].path, "migrations/"))
		}
	}
	paths := make([]string, len(found))
	for i := range found {
		paths[i] = found[i].path
	}
	return paths, nil
}
