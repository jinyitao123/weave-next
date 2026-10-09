package daemon

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jinyitao123/weave/internal/kernel/engine"
	"github.com/jinyitao123/weave/internal/kernel/runtimeprotocol"
)

func gitFixture(t *testing.T, dir string, args ...string) string {
	t.Helper()
	command := exec.Command("git", gitArgs(args...)...)
	command.Dir = dir
	command.Env = append(os.Environ(), fixedCommitEnv()...)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v %s", args, err, output)
	}
	return strings.TrimSpace(string(output))
}

func codeFixtureRepository(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	source := t.TempDir()
	gitFixture(t, source, "init", "--quiet", "--initial-branch=main")
	if err := os.WriteFile(filepath.Join(source, "app.txt"), []byte("one\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitFixture(t, source, "add", "app.txt")
	gitFixture(t, source, "commit", "--quiet", "-m", "base")
	return "file://" + filepath.ToSlash(source)
}

func TestCodeVerificationRunsOnDeliveredHEADAndRejectsTrackedMutation(t *testing.T) {
	for _, command := range []string{
		"git rev-parse HEAD",
		"printf changed > app.txt",
		"git checkout HEAD~1 -- app.txt && git add app.txt",
		"git checkout --detach HEAD~1",
	} {
		t.Run(command, func(t *testing.T) {
			ctx := t.Context()
			session, err := prepareCodeWorkspace(ctx, t.TempDir(), t.TempDir(), runtimeprotocol.CodeWorkspace{Repository: codeFixtureRepository(t), Ref: "main", VerifyCommands: []string{command}}, codeRemote{}, "code", nil)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(session.repo, "app.txt"), []byte("delivered\n"), 0600); err != nil {
				t.Fatal(err)
			}
			result := engine.RunResult{Status: "completed"}
			if err := session.finish(ctx, &result); err != nil {
				t.Fatal(err)
			}
			files := artifactMap(result.Artifacts)
			var version codeVersion
			var evidence codeEvidence
			if err := json.Unmarshal([]byte(files[codeVersionPath]), &version); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal([]byte(files[codeEvidencePath]), &evidence); err != nil {
				t.Fatal(err)
			}
			if evidence.Commit != version.HeadSHA || evidence.Tree != version.TreeSHA {
				t.Fatal("evidence not bound to delivery")
			}
			if command == "git rev-parse HEAD" {
				if !evidence.TreeUnchanged || strings.TrimSpace(evidence.Commands[0].OutputTail) != version.HeadSHA {
					t.Fatalf("verified wrong HEAD: %+v", evidence)
				}
			} else if evidence.TreeUnchanged {
				t.Fatal("tracked/HEAD mutation reported verified")
			}
		})
	}
}

func artifactMap(artifacts []engine.Artifact) map[string]string {
	result := map[string]string{}
	for _, artifact := range artifacts {
		result[artifact.Path] = artifact.Content
	}
	return result
}

func handOver(t *testing.T, workDir, nodeID string, artifacts map[string]string) {
	t.Helper()
	for path, content := range artifacts {
		if !strings.HasPrefix(path, "code/") {
			continue
		}
		target := filepath.Join(workDir, "inputs", nodeID, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(target, []byte(content), 0o444); err != nil {
			t.Fatal(err)
		}
	}
}

func TestCodeWorkspaceRecordsExactCommitsAndHostEvidence(t *testing.T) {
	ctx := context.Background()
	repository := codeFixtureRepository(t)
	root := t.TempDir()
	spec := runtimeprotocol.CodeWorkspace{Repository: repository, Ref: "main", VerifyCommands: []string{
		"git ls-files --error-unmatch added.txt", "exit 3",
	}}

	// Stage one changes the code.
	first := t.TempDir()
	session, err := prepareCodeWorkspace(ctx, root, first, spec, codeRemote{}, "code", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(session.prompt("写代码"), "repo/") {
		t.Fatal("member is not told where the checkout is")
	}
	if err := os.WriteFile(filepath.Join(session.repo, "added.txt"), []byte("two\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(session.repo, "app.txt"), []byte("one\nmore\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	result := engine.RunResult{Status: "completed", Artifacts: []engine.Artifact{{Path: codeVersionPath, ContentType: "application/json", Content: `{"head_sha":"forged"}`}}}
	if err := session.finish(ctx, &result); err != nil {
		t.Fatal(err)
	}
	firstFiles := artifactMap(result.Artifacts)
	var version codeVersion
	if err := json.Unmarshal([]byte(firstFiles[codeVersionPath]), &version); err != nil {
		t.Fatal(err)
	}
	if version.HeadSHA == "forged" || !version.Changed || version.HeadSHA == version.BaseSHA || version.Patch != "complete" || len(version.Files) != 2 {
		t.Fatalf("stage one version = %+v", version)
	}
	if !strings.Contains(firstFiles[codePatchPath], "added.txt") {
		t.Fatal("patch does not carry the change")
	}
	var evidence codeEvidence
	if err := json.Unmarshal([]byte(firstFiles[codeEvidencePath]), &evidence); err != nil {
		t.Fatal(err)
	}
	if evidence.Commit != version.HeadSHA || len(evidence.Commands) != 2 || evidence.Commands[0].ExitCode != 0 || evidence.Commands[1].ExitCode != 3 || evidence.Commands[0].OutputSHA256 == "" {
		t.Fatalf("stage one evidence = %+v", evidence)
	}

	// Stage two rebuilds the exact commit, changes nothing and reuses evidence.
	second := t.TempDir()
	handOver(t, second, "code", firstFiles)
	session, err = prepareCodeWorkspace(ctx, root, second, spec, codeRemote{}, "verify", nil)
	if err != nil {
		t.Fatal(err)
	}
	if session.startSHA != version.HeadSHA {
		t.Fatalf("stage two started on %s, want %s", session.startSHA, version.HeadSHA)
	}
	if prompt := session.prompt("验证"); !strings.Contains(prompt, "平台已在接手的提交上运行验证命令") || !strings.Contains(prompt, "退出码 0") {
		t.Fatalf("stage two prompt lacks the Host evidence: %s", prompt)
	}
	if head := gitFixture(t, session.repo, "rev-parse", "HEAD"); head != version.HeadSHA {
		t.Fatalf("checkout HEAD = %s", head)
	}
	result = engine.RunResult{Status: "completed"}
	if err := session.finish(ctx, &result); err != nil {
		t.Fatal(err)
	}
	secondFiles := artifactMap(result.Artifacts)
	var secondVersion codeVersion
	if err := json.Unmarshal([]byte(secondFiles[codeVersionPath]), &secondVersion); err != nil {
		t.Fatal(err)
	}
	if secondVersion.HeadSHA != version.HeadSHA || secondVersion.ParentSHA != version.HeadSHA {
		t.Fatalf("unchanged stage moved the commit: %+v", secondVersion)
	}
	var reused codeEvidence
	if err := json.Unmarshal([]byte(secondFiles[codeEvidencePath]), &reused); err != nil {
		t.Fatal(err)
	}
	if reused.ReusedFrom != "code" || reused.Commit != version.HeadSHA || len(reused.Commands) != 2 {
		t.Fatalf("evidence was not reused for the same commit: %+v", reused)
	}

	// A tampered hand-over cannot produce a different commit silently.
	third := t.TempDir()
	tampered := map[string]string{}
	for path, content := range firstFiles {
		tampered[path] = content
	}
	tampered[codePatchPath] = strings.Replace(firstFiles[codePatchPath], "+two", "+TWO", 1)
	handOver(t, third, "code", tampered)
	if _, err := prepareCodeWorkspace(ctx, root, third, spec, codeRemote{}, "verify", nil); err == nil {
		t.Fatal("tampered upstream change was accepted")
	}

	// A stage that undoes everything hands over the base commit.
	fourth := t.TempDir()
	handOver(t, fourth, "code", firstFiles)
	session, err = prepareCodeWorkspace(ctx, root, fourth, runtimeprotocol.CodeWorkspace{Repository: repository, Ref: "main"}, codeRemote{}, "revert", nil)
	if err != nil {
		t.Fatal(err)
	}
	gitFixture(t, session.repo, "checkout", "--quiet", version.BaseSHA, "--", ".")
	if err := os.Remove(filepath.Join(session.repo, "added.txt")); err != nil {
		t.Fatal(err)
	}
	result = engine.RunResult{Status: "completed"}
	if err := session.finish(ctx, &result); err != nil {
		t.Fatal(err)
	}
	var reverted codeVersion
	if err := json.Unmarshal([]byte(artifactMap(result.Artifacts)[codeVersionPath]), &reverted); err != nil {
		t.Fatal(err)
	}
	if reverted.Changed || reverted.HeadSHA != version.BaseSHA {
		t.Fatalf("reverted stage = %+v", reverted)
	}
}

func TestCodeWorkspaceRefusesUnknownBranch(t *testing.T) {
	repository := codeFixtureRepository(t)
	if _, err := prepareCodeWorkspace(context.Background(), t.TempDir(), t.TempDir(), runtimeprotocol.CodeWorkspace{Repository: repository, Ref: "missing"}, codeRemote{}, "code", nil); err == nil || !strings.Contains(err.Error(), "missing") {
		t.Fatalf("unknown branch = %v", err)
	}
}

func TestCodeWorkspaceFollowUpStartsFromThePreviousDeliveredCommit(t *testing.T) {
	ctx := context.Background()
	repository := codeFixtureRepository(t)
	root := t.TempDir()
	spec := runtimeprotocol.CodeWorkspace{Repository: repository, Ref: "main"}
	first := t.TempDir()
	session, err := prepareCodeWorkspace(ctx, root, first, spec, codeRemote{}, "code", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(session.repo, "app.txt"), []byte("fixed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	result := engine.RunResult{Status: "completed"}
	if err := session.finish(ctx, &result); err != nil {
		t.Fatal(err)
	}
	files := artifactMap(result.Artifacts)
	var delivered codeVersion
	if err := json.Unmarshal([]byte(files[codeVersionPath]), &delivered); err != nil {
		t.Fatal(err)
	}
	// The follow-up names the original base and carries the delivered version.
	followUp := runtimeprotocol.CodeWorkspace{Repository: repository, Ref: delivered.BaseSHA, Seed: &runtimeprotocol.CodeSeed{Version: files[codeVersionPath], Patch: files[codePatchPath]}}
	next := t.TempDir()
	session, err = prepareCodeWorkspace(ctx, root, next, followUp, codeRemote{}, "code", nil)
	if err != nil {
		t.Fatal(err)
	}
	if session.startSHA != delivered.HeadSHA {
		t.Fatalf("follow-up started on %s, want %s", session.startSHA, delivered.HeadSHA)
	}
	content, err := os.ReadFile(filepath.Join(session.repo, "app.txt"))
	if err != nil || string(content) != "fixed\n" {
		t.Fatalf("follow-up checkout = %q %v", content, err)
	}
	// A later stage of the follow-up uses its real upstream, not the seed.
	if err := os.WriteFile(filepath.Join(session.repo, "app.txt"), []byte("fixed twice\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	result = engine.RunResult{Status: "completed"}
	if err := session.finish(ctx, &result); err != nil {
		t.Fatal(err)
	}
	second := artifactMap(result.Artifacts)
	later := t.TempDir()
	handOver(t, later, "code", second)
	session, err = prepareCodeWorkspace(ctx, root, later, followUp, codeRemote{}, "verify", nil)
	if err != nil {
		t.Fatal(err)
	}
	var secondVersion codeVersion
	if err := json.Unmarshal([]byte(second[codeVersionPath]), &secondVersion); err != nil {
		t.Fatal(err)
	}
	if session.startSHA != secondVersion.HeadSHA || secondVersion.BaseSHA != delivered.BaseSHA {
		t.Fatalf("later stage started on %s, want %s", session.startSHA, secondVersion.HeadSHA)
	}
}
