package runtimeprotocol

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPureRuntimeBoundaryImports(t *testing.T) {
	root := filepath.Join("..")
	for _, dir := range []string{"runtimeprotocol", "loomadapter"} {
		entries, err := os.ReadDir(filepath.Join(root, dir))
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range entries {
			if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") || strings.HasSuffix(entry.Name(), "_test.go") {
				continue
			}
			path := filepath.Join(root, dir, entry.Name())
			parsed, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
			if err != nil {
				t.Fatal(err)
			}
			for _, imported := range parsed.Imports {
				name := strings.Trim(imported.Path.Value, `"`)
				for _, forbidden := range []string{"/internal/app/", "/internal/build/", "/internal/kernel/taskqueue", "/internal/kernel/registry", "github.com/jackc/pgx"} {
					if strings.Contains(name, forbidden) {
						t.Fatalf("%s imports platform dependency %s", path, name)
					}
				}
			}
		}
	}
}
