package db

import (
	"io/fs"
	"strings"
	"testing"
	"testing/fstest"
)

func TestMigrationFilesRejectDuplicateVersion(t *testing.T) {
	files := fstest.MapFS{
		"migrations/0001_first.sql":  &fstest.MapFile{Data: []byte("SELECT 1")},
		"migrations/0001_second.sql": &fstest.MapFile{Data: []byte("SELECT 2")},
	}
	_, err := migrationFiles(fs.FS(files))
	if err == nil || !strings.Contains(err.Error(), "duplicate migration version 0001") {
		t.Fatalf("duplicate migration version accepted: %v", err)
	}
}

func TestMigrationFilesSortByNumericVersion(t *testing.T) {
	files := fstest.MapFS{
		"migrations/0010_ten.sql": &fstest.MapFile{},
		"migrations/0002_two.sql": &fstest.MapFile{},
	}
	paths, err := migrationFiles(fs.FS(files))
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) != 2 || paths[0] != "migrations/0002_two.sql" || paths[1] != "migrations/0010_ten.sql" {
		t.Fatalf("migration order = %v", paths)
	}
}

// Validate the real embedded migration set even when PostgreSQL tests are skipped.
func TestEmbeddedMigrationVersionsAreUnique(t *testing.T) {
	if _, err := migrationFiles(migrations); err != nil {
		t.Fatal(err)
	}
}
