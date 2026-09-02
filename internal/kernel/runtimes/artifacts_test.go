package runtimes

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestCollectOutputArtifactsKeepsOnlyBoundedRegularTextFiles(t *testing.T) {
	workDir := t.TempDir()
	outputs := filepath.Join(workDir, "outputs")
	if err := os.MkdirAll(filepath.Join(outputs, "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(outputs, "app", "node_modules", "dependency"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(outputs, "app", ".next", "server"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outputs, "orders.csv"), []byte("id,total\n1,12\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outputs, "nested", "summary.json"), []byte(`{"ok":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outputs, "ledger.jsonl"), []byte("{\"tick\":1}\n{\"tick\":2}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outputs, "binary.png"), []byte{0, 1, 2}, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outputs, "app", "node_modules", "dependency", "package.json"), []byte(`{"private":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outputs, "app", ".next", "server", "manifest.json"), []byte(`{"generated":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	artifacts := CollectOutputArtifacts(workDir)
	if len(artifacts) != 3 || artifacts[0].Path != "ledger.jsonl" || artifacts[1].Path != "orders.csv" || artifacts[2].Path != "nested/summary.json" {
		t.Fatalf("artifacts=%+v", artifacts)
	}
	if artifacts[0].ContentType != "application/x-ndjson" || artifacts[0].Content != "{\"tick\":1}\n{\"tick\":2}\n" {
		t.Fatalf("jsonl=%+v", artifacts[0])
	}
	if artifacts[1].ContentType != "text/csv" || artifacts[1].Content != "id,total\n1,12\n" {
		t.Fatalf("csv=%+v", artifacts[1])
	}
	before := SnapshotOutputArtifacts(workDir)
	if unchanged := CollectOutputArtifactsSince(workDir, before); len(unchanged) != 0 {
		t.Fatalf("unchanged artifacts=%+v", unchanged)
	}
	orders := filepath.Join(outputs, "orders.csv")
	if err := os.WriteFile(orders, []byte("id,total\n1,12\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	newer := time.Now().Add(time.Second)
	if err := os.Chtimes(orders, newer, newer); err != nil {
		t.Fatal(err)
	}
	rewritten := CollectOutputArtifactsSince(workDir, before)
	if len(rewritten) != 1 || rewritten[0].Path != "orders.csv" {
		t.Fatalf("rewritten artifacts=%+v", rewritten)
	}
	if err := os.WriteFile(filepath.Join(outputs, "orders.csv"), []byte("id,total\n1,13\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	changed := CollectOutputArtifactsSince(workDir, before)
	if len(changed) != 1 || changed[0].Path != "orders.csv" {
		t.Fatalf("changed artifacts=%+v", changed)
	}
}
