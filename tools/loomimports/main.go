// loomimports inventories which Weave packages import Loom's execution APIs
// (the graph engine, stdlib, stores and providers). Loom's contract package is
// the shared vocabulary and is not counted. The checked baseline is migration
// debt toward a single member-executor boundary: a new importing package, or a
// higher file count, fails, and removing an import must lower the baseline.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

type reference struct {
	Package string `json:"package"`
	Import  string `json:"import"`
	Files   int    `json:"files"`
}

const loomModule = "github.com/jinyitao123/loom"

// classify names the execution surface an import path belongs to; the empty
// string means the import is not an execution API.
func classify(path string) string {
	switch {
	case path == loomModule:
		return "loom"
	case path == loomModule+"/stdlib" || strings.HasPrefix(path, loomModule+"/stdlib/"):
		return "loom/stdlib"
	case path == loomModule+"/pgstore":
		return "loom/pgstore"
	case strings.HasPrefix(path, loomModule+"/provider/"):
		return "loom/provider"
	default:
		return ""
	}
}

func inventory(root string) ([]reference, error) {
	counts := map[reference]int{}
	for _, top := range []string{"internal", "cmd"} {
		base := filepath.Join(root, top)
		if _, err := os.Stat(base); os.IsNotExist(err) {
			continue
		}
		err := filepath.WalkDir(base, func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
			if err != nil {
				return err
			}
			relative, err := filepath.Rel(root, filepath.Dir(path))
			if err != nil {
				return err
			}
			seen := map[string]bool{}
			for _, spec := range file.Imports {
				imported, err := strconv.Unquote(spec.Path.Value)
				if err != nil {
					continue
				}
				if kind := classify(imported); kind != "" && !seen[kind] {
					seen[kind] = true
					counts[reference{Package: filepath.ToSlash(relative), Import: kind}]++
				}
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	result := make([]reference, 0, len(counts))
	for key, files := range counts {
		key.Files = files
		result = append(result, key)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Package != result[j].Package {
			return result[i].Package < result[j].Package
		}
		return result[i].Import < result[j].Import
	})
	return result, nil
}

func check(actual, baseline []reference) []string {
	allowed := map[reference]int{}
	var issues []string
	for _, entry := range baseline {
		files := entry.Files
		entry.Files = 0
		if files < 1 || allowed[entry] != 0 {
			issues = append(issues, fmt.Sprintf("invalid baseline entry %s %s", entry.Package, entry.Import))
		}
		allowed[entry] = files
	}
	for _, entry := range actual {
		files := entry.Files
		entry.Files = 0
		maximum := allowed[entry]
		delete(allowed, entry)
		switch {
		case maximum == 0:
			issues = append(issues, fmt.Sprintf("%s: new import of %s; reach Loom through the member executor port or the Loom adapter packages", entry.Package, entry.Import))
		case files > maximum:
			issues = append(issues, fmt.Sprintf("%s: files importing %s increased %d -> %d", entry.Package, entry.Import, maximum, files))
		case files < maximum:
			issues = append(issues, fmt.Sprintf("%s: lower the baseline for %s %d -> %d", entry.Package, entry.Import, maximum, files))
		}
	}
	for entry, files := range allowed {
		issues = append(issues, fmt.Sprintf("%s: remove resolved %s from the baseline (%d -> 0)", entry.Package, entry.Import, files))
	}
	sort.Strings(issues)
	return issues
}

func run() error {
	root := flag.String("root", ".", "repository root")
	emit := flag.Bool("inventory", false, "emit the current inventory for review")
	flag.Parse()
	actual, err := inventory(*root)
	if err != nil {
		return err
	}
	if *emit {
		encoder := json.NewEncoder(os.Stdout)
		encoder.SetIndent("", "  ")
		return encoder.Encode(actual)
	}
	raw, err := os.ReadFile(filepath.Join(*root, "tools/loomimports/baseline.json"))
	if err != nil {
		return err
	}
	var baseline []reference
	if err := json.Unmarshal(raw, &baseline); err != nil {
		return err
	}
	if issues := check(actual, baseline); len(issues) > 0 {
		return fmt.Errorf("%s", strings.Join(issues, "\n"))
	}
	files := 0
	for _, entry := range actual {
		files += entry.Files
	}
	fmt.Printf("loom import boundary: %d files across %d package/import pairs; no growth\n", files, len(actual))
	return nil
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "loomimports:", err)
		os.Exit(1)
	}
}
