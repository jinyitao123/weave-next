package db

import (
	"context"
	"io/fs"
	"testing"
	"testing/fstest"

	"github.com/jinyitao123/weave/internal/base/testutil"
)

func TestMergedMigrationsFreshAndExistingDatabase(t *testing.T) {
	for _, existing := range []bool{false, true} {
		name := "fresh"
		if existing {
			name = "already_applied_through_0164"
		}
		t.Run(name, func(t *testing.T) {
			pool := testutil.PostgresPool(t)
			ctx := context.Background()
			if existing {
				files := fstest.MapFS{}
				paths, err := migrationFiles(migrations)
				if err != nil {
					t.Fatal(err)
				}
				for _, path := range paths {
					if path >= "migrations/0165_" {
						continue
					}
					data, err := fs.ReadFile(migrations, path)
					if err != nil {
						t.Fatal(err)
					}
					files[path] = &fstest.MapFile{Data: data}
				}
				if err := migrateFS(ctx, pool, files); err != nil {
					t.Fatal(err)
				}
			}
			if err := Migrate(ctx, pool); err != nil {
				t.Fatal(err)
			}
			// Both branches' tables must exist; retaining a duplicate version would
			// otherwise make one branch disappear from the version-only ledger.
			for _, table := range []string{"weave_capability_definitions", "weave_capability_invocations", "weave_game_decision_bindings", "weave_game_decision_admissions", "weave_game_decision_cancellations"} {
				var exists bool
				if err := pool.QueryRow(ctx, `SELECT to_regclass($1) IS NOT NULL`, table).Scan(&exists); err != nil || !exists {
					t.Fatalf("missing %s: %v", table, err)
				}
			}
			var appliedBefore, appliedAfter int
			if err := pool.QueryRow(ctx, `SELECT count(*) FROM weave_schema_migrations`).Scan(&appliedBefore); err != nil {
				t.Fatal(err)
			}
			if err := Migrate(ctx, pool); err != nil {
				t.Fatal(err)
			}
			if err := pool.QueryRow(ctx, `SELECT count(*) FROM weave_schema_migrations`).Scan(&appliedAfter); err != nil {
				t.Fatal(err)
			}
			if appliedBefore != appliedAfter {
				t.Fatalf("migration replay changed ledger: %d -> %d", appliedBefore, appliedAfter)
			}
		})
	}
}
