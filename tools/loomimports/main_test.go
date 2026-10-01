package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestInventoryCountsExecutionImportsOnlyAndSkipsTestsAndContract(t *testing.T) {
	root := t.TempDir()
	files := map[string]string{
		"internal/kernel/a/run.go":      "package a\nimport (\n\t\"github.com/jinyitao123/loom\"\n\t\"github.com/jinyitao123/loom/stdlib\"\n\t\"github.com/jinyitao123/loom/contract\"\n)\n",
		"internal/kernel/a/second.go":   "package a\nimport \"github.com/jinyitao123/loom\"\n",
		"internal/kernel/a/run_test.go": "package a\nimport \"github.com/jinyitao123/loom/pgstore\"\n",
		"internal/kernel/b/only.go":     "package b\nimport \"github.com/jinyitao123/loom/contract\"\n",
		"internal/kernel/c/win.go":      "//go:build windows\npackage c\nimport _ \"github.com/jinyitao123/loom/provider/openai\"\n",
		"cmd/tool/main.go":              "package main\nimport \"github.com/jinyitao123/loom/pgstore\"\n",
	}
	for path, body := range files {
		full := filepath.Join(root, path)
		if err := os.MkdirAll(filepath.Dir(full), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0644); err != nil {
			t.Fatal(err)
		}
	}
	actual, err := inventory(root)
	if err != nil {
		t.Fatal(err)
	}
	want := []reference{
		{"cmd/tool", "loom/pgstore", 1},
		{"internal/kernel/a", "loom", 2},
		{"internal/kernel/a", "loom/stdlib", 1},
		{"internal/kernel/c", "loom/provider", 1},
	}
	if len(actual) != len(want) {
		t.Fatalf("inventory = %#v", actual)
	}
	for index := range want {
		if actual[index] != want[index] {
			t.Fatalf("entry %d = %#v, want %#v", index, actual[index], want[index])
		}
	}
}

func TestBoundaryCannotGrowAndMustLowerResolvedDebt(t *testing.T) {
	baseline := []reference{{"internal/kernel/a", "loom", 2}}
	if issues := check(baseline, baseline); len(issues) != 0 {
		t.Fatal(issues)
	}
	cases := map[string][]reference{
		"new package":     {{"internal/kernel/a", "loom", 2}, {"internal/app/api", "loom", 1}},
		"new import kind": {{"internal/kernel/a", "loom", 2}, {"internal/kernel/a", "loom/stdlib", 1}},
		"more files":      {{"internal/kernel/a", "loom", 3}},
		"debt paid, kept": {{"internal/kernel/a", "loom", 1}},
		"debt paid, gone": {},
	}
	for name, actual := range cases {
		if issues := check(actual, baseline); len(issues) == 0 {
			t.Errorf("%s: no issue reported", name)
		}
	}
	if issues := check([]reference{}, []reference{{"internal/kernel/a", "loom", 0}}); len(issues) == 0 {
		t.Error("a zero-file baseline entry was accepted")
	}
}
