package daemon

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDaemonUsesRuntimeProtocolInsteadOfPlatformRecords(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") || strings.HasSuffix(entry.Name(), "_test.go") {
			continue
		}
		path := filepath.Clean(entry.Name())
		parsed, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, imported := range parsed.Imports {
			name := strings.Trim(imported.Path.Value, `"`)
			for _, forbidden := range []string{"/internal/kernel/taskqueue", "/internal/kernel/registry", "/internal/kernel/runtimes", "github.com/jackc/pgx"} {
				if strings.Contains(name, forbidden) {
					t.Fatalf("%s imports platform record package %s", path, name)
				}
			}
		}
	}
}
