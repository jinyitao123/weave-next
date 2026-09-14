package runtimes

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// Production CLI construction belongs only to the Host claim handler. A
// platform, builder or MCP entry cannot silently recreate a direct executor.
func TestCLIConstructionRemainsBehindRuntimeHost(t *testing.T) {
	root := filepath.Clean("../../..")
	for _, band := range []string{"internal", "cmd"} {
		err := filepath.WalkDir(filepath.Join(root, band), func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			f, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
			if err != nil {
				return err
			}
			aliases := map[string]bool{}
			for _, imp := range f.Imports {
				value, _ := strconv.Unquote(imp.Path.Value)
				if value == "github.com/jinyitao123/weave/internal/kernel/engine" {
					alias := "engine"
					if imp.Name != nil {
						alias = imp.Name.Name
					}
					aliases[alias] = true
				}
			}
			rel, _ := filepath.Rel(root, path)
			ast.Inspect(f, func(node ast.Node) bool {
				switch n := node.(type) {
				case *ast.SelectorExpr:
					if ident, ok := n.X.(*ast.Ident); ok && aliases[ident.Name] && n.Sel.Name == "New" && rel != "internal/app/daemon/daemon.go" {
						t.Errorf("%s constructs CLI outside the Runtime Host", rel)
					}
				case *ast.Ident:
					if n.Name == "NewLocalExecutor" || n.Name == "LocalExecutor" {
						t.Errorf("%s restored a platform direct-execution state machine", rel)
					}
				}
				return true
			})
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}

func TestCanonicalEngineCapabilitiesDefaultsToSingleUserIsolation(t *testing.T) {
	got, err := canonicalEngineCapabilities([]string{"codex"}, []EngineCapability{{Engine: "codex", BinaryPath: "/bin/codex", BinaryVersion: "fixture", ProtocolVersion: "v1", AuthMode: AuthModeProvider, EndpointClass: "fixture"}})
	if err != nil {
		t.Fatal(err)
	}
	if got["codex"].SubjectIsolation != SubjectIsolationSingleUser {
		t.Fatalf("legacy capability was advertised as multi-user: %+v", got["codex"])
	}
}
