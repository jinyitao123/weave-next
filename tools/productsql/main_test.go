package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInventoryIncludesEveryBuildTargetAndExcludesCommentsAndFixtures(t *testing.T) {
	root := t.TempDir()
	for _, band := range []string{"base", "kernel"} {
		if err := os.MkdirAll(filepath.Join(root, "internal", band), 0755); err != nil {
			t.Fatal(err)
		}
	}
	files := map[string]string{
		"internal/kernel/query_windows.go": "//go:build windows\npackage query\n// SELECT * FROM weave_users\nconst query = `SELECT * FROM weave_teams JOIN weave_agents ON true`\n",
		"internal/base/query.go":           "package query\nconst query = \"SELECT * FROM WEAVE_DRAFT_WORKFLOW_DEPENDENCIES\"\n",
		"internal/kernel/fixture_test.go":  "package query\nconst fixture = `INSERT INTO weave_users VALUES(1)`\n",
	}
	for path, body := range files {
		if err := os.WriteFile(filepath.Join(root, path), []byte(body), 0644); err != nil {
			t.Fatal(err)
		}
	}
	actual, err := inventory(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(actual) != 3 {
		t.Fatalf("inventory = %#v", actual)
	}
	for _, entry := range actual {
		if entry.Count != 1 || entry.Table == "weave_users" {
			t.Fatalf("unexpected reference %#v", entry)
		}
	}
}

func TestOwnershipBaselineCannotGrowAndMustRemoveResolvedDebt(t *testing.T) {
	baseline := []reference{{Path: "internal/kernel/example.go", Table: "weave_teams", Count: 1}}
	if issues := check(baseline, baseline); len(issues) != 0 {
		t.Fatal(issues)
	}
	cases := map[string][]reference{
		"table": {{Path: "internal/kernel/example.go", Table: "weave_users", Count: 1}},
		"path":  {{Path: "internal/base/example.go", Table: "weave_teams", Count: 1}},
		"count": {{Path: "internal/kernel/example.go", Table: "weave_teams", Count: 2}},
	}
	for name, actual := range cases {
		t.Run(name, func(t *testing.T) {
			if issues := check(actual, baseline); len(issues) == 0 || !strings.Contains(strings.Join(issues, " "), "increased") {
				t.Fatalf("growth accepted: %v", issues)
			}
		})
	}
	if issues := check(nil, baseline); len(issues) != 1 || !strings.Contains(issues[0], "remove resolved debt") {
		t.Fatalf("stale debt accepted: %v", issues)
	}
}
