// productsql inventories product table references in Kernel/Base Go literals.
// The checked baseline is temporary migration debt, never permission for new
// coupling. Each removal must lower it; new tables, paths, or counts fail.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

type reference struct {
	Path  string `json:"path"`
	Table string `json:"table"`
	Count int    `json:"count"`
}

var identifiers = regexp.MustCompile(`(?i)\bweave_[a-z0-9_]+\b`)
var productTables = map[string]bool{
	"weave_users": true, "weave_workspaces": true, "weave_members": true,
	"weave_teams": true, "weave_team_workers": true, "weave_team_dispatch_rules": true,
	"weave_team_roster_receipts": true, "weave_team_roster_audits": true,
	"weave_agents": true, "weave_agent_versions": true, "weave_agent_links": true,
	"weave_projects": true, "weave_project_resources": true, "weave_project_move_audits": true,
	"weave_conversations": true, "weave_messages": true, "weave_conversation_read_state": true,
	"weave_inbox_unread": true, "weave_chat_requests": true, "weave_attachments": true,
	"weave_drafts": true, "weave_build_runs": true, "weave_import_preview_tokens": true,
	"weave_team_evaluation_requests": true, "weave_team_template_requests": true,
	"weave_team_publication_requests": true, "weave_team_candidate_requests": true,
}

func isProductTable(table string) bool {
	return productTables[table] || strings.HasPrefix(table, "weave_team_build_") || strings.HasPrefix(table, "weave_draft_")
}
func inventory(root string) ([]reference, error) {
	counts := map[reference]int{}
	for _, band := range []string{"base", "kernel"} {
		err := filepath.WalkDir(filepath.Join(root, "internal", band), func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() {
				return nil
			}
			if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
			if err != nil {
				return err
			}
			relative, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			ast.Inspect(file, func(node ast.Node) bool {
				literal, ok := node.(*ast.BasicLit)
				if !ok || literal.Kind != token.STRING {
					return true
				}
				value, err := strconv.Unquote(literal.Value)
				if err != nil {
					return true
				}
				for _, table := range identifiers.FindAllString(strings.ToLower(value), -1) {
					if isProductTable(table) {
						counts[reference{Path: filepath.ToSlash(relative), Table: table}]++
					}
				}
				return true
			})
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	result := make([]reference, 0, len(counts))
	for key, count := range counts {
		key.Count = count
		result = append(result, key)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Path != result[j].Path {
			return result[i].Path < result[j].Path
		}
		return result[i].Table < result[j].Table
	})
	return result, nil
}
func check(actual, baseline []reference) []string {
	allowed := map[reference]int{}
	var issues []string
	for _, entry := range baseline {
		count := entry.Count
		entry.Count = 0
		if count < 1 || allowed[entry] != 0 {
			issues = append(issues, fmt.Sprintf("invalid baseline entry %s %s", entry.Path, entry.Table))
		}
		allowed[entry] = count
	}
	for _, entry := range actual {
		count := entry.Count
		entry.Count = 0
		maximum := allowed[entry]
		delete(allowed, entry)
		if count > maximum {
			issues = append(issues, fmt.Sprintf("%s: product table %s references increased %d -> %d; inject product facts instead", entry.Path, entry.Table, maximum, count))
		} else if count < maximum {
			issues = append(issues, fmt.Sprintf("%s: reduce resolved debt for %s in baseline %d -> %d", entry.Path, entry.Table, maximum, count))
		}
	}
	for entry, count := range allowed {
		issues = append(issues, fmt.Sprintf("%s: remove resolved debt for %s from baseline (%d -> 0)", entry.Path, entry.Table, count))
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
	raw, err := os.ReadFile(filepath.Join(*root, "tools/productsql/baseline.json"))
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
	count := 0
	for _, entry := range actual {
		count += entry.Count
	}
	fmt.Printf("product SQL ownership: %d remaining references across %d file/table pairs; no growth\n", count, len(actual))
	return nil
}
func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "productsql:", err)
		os.Exit(1)
	}
}
