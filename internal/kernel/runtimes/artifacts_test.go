package runtimes

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jinyitao123/weave/internal/base/fileartifact"
	"github.com/jinyitao123/weave/internal/kernel/engine"
)

func TestSourceDeliveryKeepsApplicationAndVerificationDependenciesAcrossRuntimeWire(t *testing.T) {
	workDir := t.TempDir()
	files := map[string]string{
		"app/index.html":           `<link rel="stylesheet" href="style.css"><script src="calc.js"></script>`,
		"app/style.css":            "body { color: black; }",
		"app/calc.js":              "window.travelYears = distance => distance / 0.03;",
		"app/tests/check.cjs":      "const assert = require('node:assert/strict'); assert.equal(1, 1);\n",
		"model/recalc.py":          "print(4.25 / 0.03)\n",
		"model/derive_params.rb":   "require 'json'\nputs JSON.generate({speed: 0.03})\n",
		"drawings/concept.dxf":     "0\nSECTION\n2\nENTITIES\n0\nENDSEC\n0\nEOF\n",
		"verification/run.sh":      "#!/bin/sh\npython3 ../model/recalc.py\n",
		"verification/results.log": "passed\n",
	}
	// A combined model, application, drawing and review package can exceed 64
	// small files without approaching the unchanged total-byte transport limit.
	for index := 0; index < 90; index++ {
		files[fmt.Sprintf("review/requirement-%03d.md", index)] = "Verified requirement and its evidence.\n"
	}
	before := SnapshotOutputArtifacts(workDir)
	for name, content := range files {
		path := filepath.Join(workDir, "outputs", filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	result := engine.RunResult{Status: "completed", Output: "Saved `outputs/app/index.html` and its local dependencies."}
	CollectRunOutputArtifacts(workDir, before, &result)
	wire, err := json.Marshal(CLIEngineExecResult(result))
	if err != nil {
		t.Fatal(err)
	}
	var received EngineExecResult
	if err := json.Unmarshal(wire, &received); err != nil {
		t.Fatal(err)
	}
	if received.Status != "completed" || len(received.Artifacts) != len(files) {
		t.Fatalf("incomplete executable delivery: status=%s files=%d", received.Status, len(received.Artifacts))
	}
	for _, file := range received.Artifacts {
		if want, ok := files[file.Path]; !ok || file.Content != want {
			t.Fatalf("lost or changed dependency: %s", file.Path)
		}
	}
	if stale := CollectOutputArtifactsSince(workDir, SnapshotOutputArtifacts(workDir)); len(stale) != 0 {
		t.Fatalf("unchanged source files republished: %d", len(stale))
	}
}

func TestCollectOutputArtifactsIncludesOnlyReferencedCurrentRootFiles(t *testing.T) {
	workDir := t.TempDir()
	write := func(name, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(workDir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("stale.md", "previous task")
	before := SnapshotOutputArtifacts(workDir)
	body := "# Brief\n" + strings.Repeat("Complete result, not a receipt.\n", 400)
	write("brief.md", body)
	write("private.md", "not selected for delivery")
	write("AGENTS.md", "managed instructions")
	write(".secret.md", "hidden")
	if err := os.Symlink(filepath.Join(workDir, "private.md"), filepath.Join(workDir, "linked.md")); err != nil {
		t.Fatal(err)
	}
	answer := "已生成 `brief.md:1`; `stale.md`, `AGENTS.md`, `.secret.md`, `linked.md`."
	artifacts := CollectOutputArtifactsSince(workDir, before, answer)
	if len(artifacts) != 1 || artifacts[0].Path != "brief.md" || artifacts[0].Content != body || artifacts[0].ContentType != "text/markdown" {
		t.Fatalf("current delivery = %#v", artifacts)
	}
	if got := CollectOutputArtifactsSince(workDir, SnapshotOutputArtifacts(workDir), answer); len(got) != 0 {
		t.Fatalf("unchanged files republished: %#v", got)
	}
	if got := CollectOutputArtifactsSince(workDir, nil, answer); len(got) != 0 {
		t.Fatalf("root files collected without an invocation snapshot: %#v", got)
	}
}

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

func TestCollectRunOutputArtifactsPreservesEngineOutcomeWithUncollectedDelivery(t *testing.T) {
	for _, test := range []struct {
		name, filename, reason string
		content                string
		stale                  bool
		precedingFiles         int
		precedingBytes         int
	}{
		{name: "single file limit", filename: "brief.md", reason: "file_exceeds_256_kib", content: strings.Repeat("x", engine.MaxArtifactBytes+1)},
		{name: "total limit", filename: "outputs/z-brief.md", reason: "files_exceed_512_kib_total", content: "Final result", precedingFiles: 2, precedingBytes: engine.MaxArtifactBytes},
		{name: "count limit", filename: "outputs/z-brief.md", reason: fmt.Sprintf("file_count_exceeds_%d", engine.MaxArtifactCount), content: "Final result", precedingFiles: engine.MaxArtifactCount, precedingBytes: 1},
		{name: "stale root file", filename: "brief.md", reason: "file_not_written_by_this_invocation", content: "An earlier task's result", stale: true},
		{name: "stale outputs file", filename: "outputs/brief.md", reason: "file_not_written_by_this_invocation", content: "An earlier task's result", stale: true},
		{name: "unsupported root type", filename: "brief.pdf", reason: "unsupported_file_type", content: "%PDF-1.4"},
		{name: "unsupported outputs type", filename: "outputs/brief.pdf", reason: "unsupported_file_type", content: "%PDF-1.4"},
		{name: "invalid text", filename: "brief.md", reason: "file_not_utf8", content: string([]byte{0xff, 0xfe})},
	} {
		t.Run(test.name, func(t *testing.T) {
			workDir := t.TempDir()
			write := func(name, body string) {
				t.Helper()
				path := filepath.Join(workDir, name)
				if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if test.stale {
				write(test.filename, test.content)
			}
			before := SnapshotOutputArtifacts(workDir)
			if !test.stale {
				write(test.filename, test.content)
			}
			for index := 0; index < test.precedingFiles; index++ {
				write(fmt.Sprintf("outputs/a-%03d.txt", index), strings.Repeat("x", test.precedingBytes))
			}
			if test.precedingFiles == 0 {
				write("outputs/partial.txt", "Already completed work")
			}
			answer := "Saved [final](" + filepath.ToSlash(filepath.Join(workDir, test.filename)) + ")."
			result := engine.RunResult{Output: answer, Status: "completed", SessionID: "engine-session-1", Usage: &engine.UsageReceipt{InputTokens: 13, HasTokens: true}}
			collectionErr := CollectRunOutputArtifacts(workDir, before, &result)
			if (collectionErr != nil) != (test.reason == "file_not_utf8") {
				t.Fatalf("technical error classification: %v", collectionErr)
			}
			if result.Status != "completed" || result.Err != "" || !collectionHasIssue(result.ArtifactCollection, test.reason, true) || len(result.Diagnostics) != 1 {
				t.Fatalf("uncollected delivery accepted: status=%q error=%q diagnostics=%#v", result.Status, result.Err, result.Diagnostics)
			}
			if strings.Contains(result.Err, workDir) || result.Output != answer {
				t.Fatalf("host path leaked in failure or unsaved reference normalized: error=%q output=%q", result.Err, result.Output)
			}
			wantFiles := max(test.precedingFiles, 1)
			if len(result.Artifacts) != wantFiles {
				t.Fatalf("partial work lost: got %d files, want %d", len(result.Artifacts), wantFiles)
			}
			for _, artifact := range result.Artifacts {
				if artifact.Path == filepath.Base(test.filename) {
					t.Fatal("uncollected final was transported")
				}
			}
			// The daemon/server carrier preserves execution facts, actual files,
			// and collection gaps independently across JSON serialization.
			wire, err := json.Marshal(CLIEngineExecResult(result))
			if err != nil {
				t.Fatal(err)
			}
			var remote EngineExecResult
			if err := json.Unmarshal(wire, &remote); err != nil {
				t.Fatal(err)
			}
			restored := remote.EngineRunResult()
			if restored.Status != "completed" || restored.Err != "" || restored.SessionID != "engine-session-1" || restored.Usage == nil || restored.Usage.InputTokens != 13 || !collectionHasIssue(restored.ArtifactCollection, test.reason, true) || len(restored.Artifacts) != wantFiles || len(restored.Diagnostics) != 1 {
				t.Fatalf("remote delivery gap lost: %#v", restored)
			}
		})
	}
}

func TestCollectRunOutputArtifactsDoesNotConfuseUnrelatedOrEarlierFiles(t *testing.T) {
	workDir := t.TempDir()
	if err := os.Mkdir(filepath.Join(workDir, "outputs"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workDir, "outputs", "brief.md"), []byte("Earlier task"), 0o600); err != nil {
		t.Fatal(err)
	}
	before := SnapshotOutputArtifacts(workDir)
	if err := os.WriteFile(filepath.Join(workDir, "brief.md"), []byte("Current task result"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workDir, "outputs", "unrelated.pdf"), []byte("not selected"), 0o600); err != nil {
		t.Fatal(err)
	}
	result := engine.RunResult{Output: "Saved `brief.md`.", Status: "completed"}
	CollectRunOutputArtifacts(workDir, before, &result)
	if result.Status != "completed" || len(result.Diagnostics) != 0 || len(result.Artifacts) != 1 || result.Artifacts[0].Content != "Current task result" {
		t.Fatalf("earlier or unrelated file replaced current delivery: %#v", result)
	}
}

func TestCollectRunOutputArtifactsRetainsOriginalFailureAndBoundsDiagnostics(t *testing.T) {
	workDir := t.TempDir()
	before := SnapshotOutputArtifacts(workDir)
	if err := os.WriteFile(filepath.Join(workDir, "brief.pdf"), []byte("unsupported"), 0o600); err != nil {
		t.Fatal(err)
	}
	result := engine.RunResult{Output: "Saved `brief.pdf`.", Status: "timeout", Err: "original timeout"}
	for range 32 {
		result.Diagnostics = append(result.Diagnostics, engine.Diagnostic{Code: "cli_note", Message: "Earlier note"})
	}
	CollectRunOutputArtifacts(workDir, before, &result)
	if result.Status != "timeout" || result.Err != "original timeout" || len(result.Diagnostics) != 32 || result.Diagnostics[31].Code != "cli_note" || !collectionHasIssue(result.ArtifactCollection, "unsupported_file_type", true) {
		t.Fatalf("original failure overwritten: %#v", result)
	}
	if err := engine.ValidateDiagnostics(result.Diagnostics); err != nil {
		t.Fatal(err)
	}
}

func TestCollectRunOutputArtifactsRejectsExplicitMissingOrExcludedPath(t *testing.T) {
	for _, name := range []string{"outputs/missing.md", "other/brief.md", "outputs/app/node_modules/brief.md"} {
		t.Run(name, func(t *testing.T) {
			workDir := t.TempDir()
			before := SnapshotOutputArtifacts(workDir)
			path := filepath.Join(workDir, name)
			if name != "outputs/missing.md" {
				if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte("Not in the delivery area"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			for _, reference := range []string{filepath.ToSlash(path), name} {
				// Relative paths outside outputs/ are intentionally not inferred.
				if reference == "other/brief.md" {
					continue
				}
				result := engine.RunResult{Output: "Saved [file](" + reference + ":1).", Status: "completed"}
				CollectRunOutputArtifacts(workDir, before, &result)
				if result.Status != "completed" || result.Err != "" || len(result.Artifacts) != 0 || !collectionHasIssue(result.ArtifactCollection, "file_not_collected", true) {
					t.Fatalf("uncollected delivery-path reference accepted: %#v", result)
				}
			}
		})
	}
}

func TestCollectRunOutputArtifactsDefersPlansAndInputReferences(t *testing.T) {
	for _, answer := range []string{
		"随后由汇总员形成简报。最终交付文件为 `outputs/acceptance.md`。\n本节点仅提供工作简报，未调用队友或写入文件。",
		"Next, save the report to `outputs/acceptance.md`.",
		"Input example:\n> Saved `outputs/acceptance.md`.\nThis is only the plan.",
		"Original input:\n```text\n已保存 `outputs/acceptance.md`。\n```\nI have not written a file.",
	} {
		t.Run(answer, func(t *testing.T) {
			workDir := t.TempDir()
			result := engine.RunResult{Status: "completed", Output: answer}
			CollectRunOutputArtifacts(workDir, SnapshotOutputArtifacts(workDir), &result)
			if result.Status != "completed" || result.Err != "" || len(result.Artifacts) != 0 ||
				len(result.Diagnostics) != 1 || result.Diagnostics[0].Code != "delivery_artifact_uncollected" {
				t.Fatalf("plan was failed or missing evidence discarded: %#v", result)
			}
		})
	}
}

func TestCollectRunOutputArtifactsPreservesAllClaimsAfterPlan(t *testing.T) {
	for _, claim := range []string{"Saved `outputs/report.md`.", "已将简报保存到 `outputs/report.md`。"} {
		workDir := t.TempDir()
		result := engine.RunResult{Status: "completed", Output: "Later: `outputs/future.md`.\n" + claim}
		CollectRunOutputArtifacts(workDir, SnapshotOutputArtifacts(workDir), &result)
		if result.Status != "completed" || result.Err != "" || result.ArtifactCollection == nil || len(result.ArtifactCollection.Issues) != 2 || result.ArtifactCollection.Issues[0].Claimed || !result.ArtifactCollection.Issues[1].Claimed {
			t.Fatalf("plan hid a false creation claim: %#v", result)
		}
	}
}

func collectionHasIssue(evidence *fileartifact.CollectionEvidence, reason string, claimed bool) bool {
	if evidence == nil {
		return false
	}
	for _, issue := range evidence.Issues {
		if issue.Reason == reason && issue.Claimed == claimed {
			return true
		}
	}
	return false
}

func TestCollectRunOutputArtifactsKeepsTechnicalErrorsBeyondEvidenceLimit(t *testing.T) {
	workDir := t.TempDir()
	if err := os.Mkdir(filepath.Join(workDir, "outputs"), 0o700); err != nil {
		t.Fatal(err)
	}
	for index := 0; index < fileartifact.MaxCollectionIssues+5; index++ {
		if err := os.WriteFile(filepath.Join(workDir, "outputs", fmt.Sprintf("a-%03d.pdf", index)), []byte("unsupported"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(workDir, "outputs", "z-invalid.md"), []byte{0xff}, 0o600); err != nil {
		t.Fatal(err)
	}
	result := engine.RunResult{Status: "completed", SessionID: "original"}
	err := CollectRunOutputArtifacts(workDir, nil, &result)
	if err == nil || !strings.Contains(err.Error(), "file_not_utf8") || result.Status != "completed" || result.Err != "" || result.SessionID != "original" || result.ArtifactCollection.Complete || len(result.ArtifactCollection.Issues) != fileartifact.MaxCollectionIssues {
		t.Fatalf("technical failure hidden by evidence truncation: %#v %v", result, err)
	}
	if err := fileartifact.ValidateCollectionEvidence(result.ArtifactCollection); err != nil {
		t.Fatalf("unbounded evidence: %v", err)
	}
}

func TestCollectRunOutputArtifactsReportsUnavailableWorkDirectory(t *testing.T) {
	result := engine.RunResult{Status: "completed"}
	err := CollectRunOutputArtifacts(filepath.Join(t.TempDir(), "gone"), nil, &result)
	if err == nil || !strings.Contains(err.Error(), "work_directory_unreadable") || result.Status != "completed" || result.Err != "" {
		t.Fatalf("missing runtime workspace was treated as no business files: %#v %v", result, err)
	}
}
