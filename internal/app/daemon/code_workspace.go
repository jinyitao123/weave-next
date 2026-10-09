package daemon

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/jinyitao123/weave/internal/kernel/engine"
	"github.com/jinyitao123/weave/internal/kernel/runtimeprotocol"
)

// A code task runs its member inside an exact checkout and reports the commit
// it produced. Every stage's commit is the run's base commit plus the stage's
// complete tree, written with fixed metadata, so the next Host can rebuild the
// exact same commit from the base and one cumulative patch and must prove it
// did before the member starts. The Host, not the member, writes the version,
// patch and evidence files.

const (
	codeRepoDir          = "repo"
	codeVersionPath      = "code/version.json"
	codePatchPath        = "code/changes.patch"
	codeEvidencePath     = "code/evidence.json"
	codeMaxPatchBytes    = 240 * 1024
	codeCommandTimeout   = 15 * time.Minute
	codeOutputTailBytes  = 16 * 1024
	codeGitCommandWindow = 10 * time.Minute
	codeCommitDate       = "2000-01-01T00:00:00+0000"
)

type codeFileStat struct {
	Path    string `json:"path"`
	Added   int    `json:"added"`
	Deleted int    `json:"deleted"`
	Binary  bool   `json:"binary,omitempty"`
}

type codePush struct {
	Branch string `json:"branch"`
	Status string `json:"status"` // pushed | failed | no_changes
	Error  string `json:"error,omitempty"`
}

type codeVersion struct {
	SchemaVersion int            `json:"schema_version"`
	Repository    string         `json:"repository"`
	Ref           string         `json:"ref"`
	BaseSHA       string         `json:"base_sha"`
	ParentSHA     string         `json:"parent_sha"`
	HeadSHA       string         `json:"head_sha"`
	TreeSHA       string         `json:"tree_sha"`
	NodeID        string         `json:"node_id"`
	Changed       bool           `json:"changed"`
	Files         []codeFileStat `json:"files"`
	Patch         string         `json:"patch"` // complete | too_large | not_utf8 | none
	Push          *codePush      `json:"push,omitempty"`
}

type codeCommand struct {
	Command      string    `json:"command"`
	ExitCode     int       `json:"exit_code"`
	TimedOut     bool      `json:"timed_out,omitempty"`
	StartedAt    time.Time `json:"started_at"`
	DurationMS   int64     `json:"duration_ms"`
	OutputSHA256 string    `json:"output_sha256"`
	OutputBytes  int64     `json:"output_bytes"`
	OutputTail   string    `json:"output_tail"`
}

type codeEvidence struct {
	SchemaVersion int           `json:"schema_version"`
	Commit        string        `json:"commit"`
	Tree          string        `json:"tree"`
	TreeUnchanged bool          `json:"tree_unchanged"`
	NodeID        string        `json:"node_id"`
	ReusedFrom    string        `json:"reused_from,omitempty"`
	Setup         *codeCommand  `json:"setup,omitempty"`
	Commands      []codeCommand `json:"commands"`
}

// codeSession is one stage's checkout, from preparation to the final report.
type codeSession struct {
	spec      runtimeprotocol.CodeWorkspace
	remote    codeRemote
	nodeID    string
	repo      string
	baseSHA   string
	startSHA  string
	startTree string
	upstream  *codeEvidence
	setup     *codeCommand
	logs      *taskLogs
}

var codeMirrorLocks sync.Map

func gitArgs(args ...string) []string {
	// Fixed settings keep blobs byte-identical on every Host regardless of the
	// local git configuration.
	// core.longpaths lets Git for Windows handle the deep invocation work
	// directories; other platforms ignore it.
	return append([]string{"-c", "core.autocrlf=false", "-c", "core.safecrlf=false", "-c", "core.longpaths=true", "-c", "commit.gpgsign=false", "-c", "core.hooksPath=" + os.DevNull}, args...)
}

func runGit(ctx context.Context, dir string, env []string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, codeGitCommandWindow)
	defer cancel()
	command := exec.CommandContext(ctx, "git", gitArgs(args...)...)
	command.Dir = dir
	command.Env = append(os.Environ(), env...)
	command.Env = append(command.Env, "GIT_TERMINAL_PROMPT=0")
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	if err := command.Run(); err != nil {
		message := strings.TrimSpace(stderr.String())
		if len(message) > 400 {
			message = message[:400]
		}
		return "", fmt.Errorf("git %s: %w: %s", args[0], err, message)
	}
	return strings.TrimSpace(stdout.String()), nil
}

func fixedCommitEnv() []string {
	return []string{
		"GIT_AUTHOR_NAME=Weave", "GIT_AUTHOR_EMAIL=weave@localhost", "GIT_AUTHOR_DATE=" + codeCommitDate,
		"GIT_COMMITTER_NAME=Weave", "GIT_COMMITTER_EMAIL=weave@localhost", "GIT_COMMITTER_DATE=" + codeCommitDate,
	}
}

// codeRemote is where the Host fetches and pushes a repository. The zero value
// is the repository itself with the Host's own git credentials; a proxied
// remote is the server's task-scoped git proxy, authenticated by headers that
// reach only that git process.
type codeRemote struct {
	url string
	env []string
}

func (remote codeRemote) resolve(repository string) codeRemote {
	if remote.url == "" {
		remote.url = repository
	}
	return remote
}

// proxiedCodeRemote returns the git proxy remote for one claimed task.
func proxiedCodeRemote(server, token string, task *runtimeprotocol.ExecutionClaim) codeRemote {
	headers := []string{
		"Authorization: Bearer " + token,
		runtimeprotocol.HeaderVersion + ": " + runtimeprotocol.ProtocolVersion,
		"X-Weave-Task-Epoch: " + strconv.FormatInt(task.ClaimEpoch, 10),
		"X-Weave-Task-Subject: " + task.Subject.Digest(),
	}
	env := []string{"GIT_CONFIG_COUNT=" + strconv.Itoa(len(headers))}
	for index, header := range headers {
		env = append(env, fmt.Sprintf("GIT_CONFIG_KEY_%d=http.extraHeader", index), fmt.Sprintf("GIT_CONFIG_VALUE_%d=%s", index, header))
	}
	return codeRemote{url: strings.TrimRight(server, "/") + "/v1/runtime/tasks/" + url.PathEscape(task.TaskID) + "/git", env: env}
}

// ensureCodeMirror keeps one bare mirror per repository below the Host root.
// The cache is keyed by the repository, whichever remote refreshes it.
func ensureCodeMirror(ctx context.Context, root, repository string, remote codeRemote) (string, error) {
	sum := sha256.Sum256([]byte(repository))
	mirror := filepath.Join(root, ".weave-git", hex.EncodeToString(sum[:12])+".git")
	lock, _ := codeMirrorLocks.LoadOrStore(mirror, &sync.Mutex{})
	lock.(*sync.Mutex).Lock()
	defer lock.(*sync.Mutex).Unlock()
	if _, err := os.Stat(filepath.Join(mirror, "HEAD")); err == nil {
		_, err := runGit(ctx, mirror, remote.env, "fetch", "--prune", "--force", "--", remote.url, "+refs/heads/*:refs/heads/*", "+refs/tags/*:refs/tags/*")
		return mirror, err
	}
	if err := os.MkdirAll(filepath.Dir(mirror), 0700); err != nil {
		return "", err
	}
	_ = os.RemoveAll(mirror)
	_, err := runGit(ctx, filepath.Dir(mirror), remote.env, "clone", "--mirror", "--", remote.url, mirror)
	return mirror, err
}

// upstreamCode returns the single code version handed over by upstream stages.
func upstreamCode(workDir string) (*codeVersion, string, *codeEvidence, error) {
	matches, _ := filepath.Glob(filepath.Join(workDir, "inputs", "*", "code", "version.json"))
	sort.Strings(matches)
	var chosen *codeVersion
	var patchPath string
	var evidence *codeEvidence
	for _, match := range matches {
		raw, err := os.ReadFile(match)
		if err != nil {
			return nil, "", nil, err
		}
		var version codeVersion
		if err := json.Unmarshal(raw, &version); err != nil || version.SchemaVersion != 1 || version.HeadSHA == "" {
			return nil, "", nil, errors.New("upstream code version is unreadable")
		}
		if chosen != nil && chosen.HeadSHA != version.HeadSHA {
			return nil, "", nil, errors.New("upstream stages handed over different code versions; merging them is not supported")
		}
		if chosen == nil {
			chosen = &version
			patchPath = filepath.Join(filepath.Dir(match), "changes.patch")
			if raw, err := os.ReadFile(filepath.Join(filepath.Dir(match), "evidence.json")); err == nil {
				var upstreamEvidence codeEvidence
				if json.Unmarshal(raw, &upstreamEvidence) == nil && upstreamEvidence.Commit == version.HeadSHA {
					evidence = &upstreamEvidence
				}
			}
		}
	}
	return chosen, patchPath, evidence, nil
}

// writeCodeSeed hands a follow-up's first stage the previous run's delivered
// version, exactly as an upstream stage would. Later stages already receive a
// real upstream hand-over and ignore the seed.
func writeCodeSeed(workDir string, seed *runtimeprotocol.CodeSeed) error {
	if seed == nil || seed.Version == "" {
		return nil
	}
	if existing, _ := filepath.Glob(filepath.Join(workDir, "inputs", "*", "code", "version.json")); len(existing) > 0 {
		return nil
	}
	dir := filepath.Join(workDir, "inputs", "_previous_run", "code")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "version.json"), []byte(seed.Version), 0o444); err != nil {
		return err
	}
	if seed.Patch == "" {
		return nil
	}
	return os.WriteFile(filepath.Join(dir, "changes.patch"), []byte(seed.Patch), 0o444)
}

func prepareCodeWorkspace(ctx context.Context, root, workDir string, spec runtimeprotocol.CodeWorkspace, remote codeRemote, nodeID string, logs *taskLogs) (*codeSession, error) {
	if _, err := exec.LookPath("git"); err != nil {
		return nil, errors.New("code task requires git on this runtime node")
	}
	remote = remote.resolve(spec.Repository)
	mirror, err := ensureCodeMirror(ctx, root, spec.Repository, remote)
	if err != nil {
		return nil, fmt.Errorf("fetch repository: %w", err)
	}
	session := &codeSession{spec: spec, remote: remote, nodeID: nodeID, repo: filepath.Join(workDir, codeRepoDir), logs: logs}
	_ = os.RemoveAll(session.repo)
	if _, err := runGit(ctx, workDir, nil, "clone", "--no-checkout", "--quiet", "--", mirror, session.repo); err != nil {
		return nil, fmt.Errorf("check out repository: %w", err)
	}
	// The member's own git commands run in the checkout too; keep them working
	// in deep Windows work directories.
	if _, err := runGit(ctx, session.repo, nil, "config", "core.longpaths", "true"); err != nil {
		return nil, err
	}
	if err := writeCodeSeed(workDir, spec.Seed); err != nil {
		return nil, err
	}
	upstream, patchPath, upstreamEvidence, err := upstreamCode(workDir)
	if err != nil {
		return nil, err
	}
	if upstream == nil {
		base, err := runGit(ctx, session.repo, nil, "rev-parse", "--verify", "--quiet", spec.Ref+"^{commit}")
		if err != nil || base == "" {
			return nil, fmt.Errorf("branch or commit %q is not in the repository", spec.Ref)
		}
		session.baseSHA, session.startSHA = base, base
	} else {
		session.baseSHA = upstream.BaseSHA
		if upstream.Changed && upstream.Patch != "complete" {
			return nil, errors.New("upstream code change is too large to hand over; reduce the change or split the task")
		}
		session.upstream = upstreamEvidence
	}
	if _, err := runGit(ctx, session.repo, nil, "checkout", "--quiet", "--detach", session.baseSHA); err != nil {
		return nil, fmt.Errorf("check out base commit: %w", err)
	}
	if upstream != nil {
		if upstream.Changed {
			if _, err := runGit(ctx, session.repo, nil, "apply", "--binary", "--index", "--whitespace=nowarn", patchPath); err != nil {
				return nil, fmt.Errorf("apply upstream change: %w", err)
			}
			tree, err := runGit(ctx, session.repo, nil, "write-tree")
			if err != nil {
				return nil, err
			}
			head, err := runGit(ctx, session.repo, fixedCommitEnv(), "commit-tree", tree, "-p", session.baseSHA, "-m", "Weave stage "+upstream.NodeID)
			if err != nil {
				return nil, err
			}
			if head != upstream.HeadSHA || tree != upstream.TreeSHA {
				return nil, errors.New("upstream code version could not be rebuilt exactly; refusing to run on a different commit")
			}
			if _, err := runGit(ctx, session.repo, nil, "checkout", "--quiet", "--detach", head); err != nil {
				return nil, err
			}
			session.startSHA = head
		} else {
			session.startSHA = upstream.HeadSHA
			if session.startSHA != session.baseSHA {
				return nil, errors.New("upstream reported an unchanged version on a different commit")
			}
		}
	}
	if session.startTree, err = runGit(ctx, session.repo, nil, "rev-parse", session.startSHA+"^{tree}"); err != nil {
		return nil, err
	}
	if strings.TrimSpace(spec.SetupScript) != "" {
		record := runCodeCommand(ctx, session.repo, spec.SetupScript, session.logs, "setup")
		session.setup = &record
	}
	return session, nil
}

func (session *codeSession) prompt(prompt string) string {
	short := session.startSHA
	if len(short) > 12 {
		short = short[:12]
	}
	notice := fmt.Sprintf("代码仓库已检出到工作目录下的 %s/（提交 %s）。只在 %s/ 内修改代码，不要自行提交或推送；平台会在你结束后记录确切提交", codeRepoDir, short, codeRepoDir)
	if len(session.spec.VerifyCommands) > 0 {
		notice += "，并在该提交上运行验证命令：" + strings.Join(session.spec.VerifyCommands, "；")
	}
	notice += "。"
	// A later stage judges the hand-over by what the Host actually observed,
	// not by the previous member's account of it.
	if evidence := session.upstream; evidence != nil && len(evidence.Commands) > 0 {
		results := make([]string, 0, len(evidence.Commands))
		for _, command := range evidence.Commands {
			outcome := fmt.Sprintf("退出码 %d", command.ExitCode)
			if command.TimedOut {
				outcome = "超时"
			}
			results = append(results, command.Command+" → "+outcome)
		}
		notice += "\n\n平台已在接手的提交上运行验证命令：" + strings.Join(results, "；") + "。完整输出尾部见 inputs/ 下的 code/evidence.json。"
	}
	return notice + "\n\n" + prompt
}

// finish records the stage's commit and evidence as Host-written artifacts,
// replacing any member file at the same paths.
func (session *codeSession) finish(ctx context.Context, result *engine.RunResult) error {
	if _, err := runGit(ctx, session.repo, nil, "add", "--all", "."); err != nil {
		return fmt.Errorf("record code change: %w", err)
	}
	tree, err := runGit(ctx, session.repo, nil, "write-tree")
	if err != nil {
		return err
	}
	version := codeVersion{SchemaVersion: 1, Repository: session.spec.Repository, Ref: session.spec.Ref, BaseSHA: session.baseSHA,
		ParentSHA: session.startSHA, HeadSHA: session.startSHA, TreeSHA: session.startTree, NodeID: session.nodeID, Files: []codeFileStat{}, Patch: "none"}
	baseTree, err := runGit(ctx, session.repo, nil, "rev-parse", session.baseSHA+"^{tree}")
	if err != nil {
		return err
	}
	switch {
	case tree == baseTree:
		// A stage that undoes every change hands over the base commit itself.
		version.HeadSHA, version.TreeSHA = session.baseSHA, baseTree
	case tree != session.startTree:
		head, err := runGit(ctx, session.repo, fixedCommitEnv(), "commit-tree", tree, "-p", session.baseSHA, "-m", "Weave stage "+session.nodeID)
		if err != nil {
			return err
		}
		version.HeadSHA, version.TreeSHA = head, tree
	}
	version.Changed = version.TreeSHA != baseTree
	// commit-tree writes an object but does not move HEAD. Verify the actual
	// delivered commit, including commands that read HEAD or compare to it.
	if _, err := runGit(ctx, session.repo, nil, "checkout", "--quiet", "--detach", version.HeadSHA); err != nil {
		return fmt.Errorf("check out delivered commit: %w", err)
	}
	artifacts := []engine.Artifact{}
	if version.Changed {
		numstat, err := runGit(ctx, session.repo, nil, "diff", "--numstat", "--no-renames", session.baseSHA, version.HeadSHA)
		if err != nil {
			return err
		}
		version.Files = parseNumstat(numstat)
		patch, err := runGit(ctx, session.repo, nil, "diff", "--binary", "--no-color", "--no-ext-diff", "--no-renames", "--full-index", session.baseSHA, version.HeadSHA)
		if err != nil {
			return err
		}
		patch += "\n"
		switch {
		case !utf8.ValidString(patch):
			version.Patch = "not_utf8"
		case len(patch) > codeMaxPatchBytes:
			version.Patch = "too_large"
		default:
			version.Patch = "complete"
			artifacts = append(artifacts, engine.Artifact{Path: codePatchPath, ContentType: "text/x-diff", Content: patch})
		}
	}
	if branch := session.spec.PushBranch; branch != "" {
		push := &codePush{Branch: branch, Status: "no_changes"}
		if version.Changed {
			if _, err := runGit(ctx, session.repo, session.remote.env, "push", "--force", "--", session.remote.url, version.HeadSHA+":refs/heads/"+branch); err != nil {
				push.Status, push.Error = "failed", trimmedError(err)
			} else {
				push.Status = "pushed"
			}
		}
		version.Push = push
	}
	if len(session.spec.VerifyCommands) > 0 {
		evidence := codeEvidence{SchemaVersion: 1, Commit: version.HeadSHA, Tree: version.TreeSHA, TreeUnchanged: true, NodeID: session.nodeID, Setup: session.setup, Commands: []codeCommand{}}
		if session.upstream != nil && session.upstream.Commit == version.HeadSHA && session.upstream.Tree == version.TreeSHA && session.upstream.TreeUnchanged {
			evidence = *session.upstream
			evidence.ReusedFrom = session.upstream.NodeID
		} else {
			for index, command := range session.spec.VerifyCommands {
				if !codeTreeMatches(ctx, session.repo, version) {
					evidence.TreeUnchanged = false
					break
				}
				evidence.Commands = append(evidence.Commands, runCodeCommand(ctx, session.repo, command, session.logs, fmt.Sprintf("command-%d", index+1)))
				if !codeTreeMatches(ctx, session.repo, version) {
					evidence.TreeUnchanged = false
					break
				}
			}
		}
		raw, _ := json.MarshalIndent(evidence, "", "  ")
		artifacts = append(artifacts, engine.Artifact{Path: codeEvidencePath, ContentType: "application/json", Content: string(raw)})
	}
	raw, _ := json.MarshalIndent(version, "", "  ")
	artifacts = append([]engine.Artifact{{Path: codeVersionPath, ContentType: "application/json", Content: string(raw)}}, artifacts...)
	reserved := map[string]bool{codeVersionPath: true, codePatchPath: true, codeEvidencePath: true}
	kept := make([]engine.Artifact, 0, len(result.Artifacts)+len(artifacts))
	for _, artifact := range result.Artifacts {
		if !reserved[artifact.Path] {
			kept = append(kept, artifact)
		}
	}
	result.Artifacts = append(kept, artifacts...)
	if err := engine.ValidateArtifacts(result.Artifacts); err != nil {
		// Host reports take precedence over member files when space is short.
		result.Artifacts = artifacts
		return engine.ValidateArtifacts(result.Artifacts)
	}
	return nil
}

// Tracked worktree and index changes, including a command moving HEAD, cannot
// be evidence for the originally delivered tree. Build output may be untracked.
func codeTreeMatches(ctx context.Context, repo string, version codeVersion) bool {
	head, err := runGit(ctx, repo, nil, "rev-parse", "HEAD")
	if err != nil || head != version.HeadSHA {
		return false
	}
	tree, err := runGit(ctx, repo, nil, "rev-parse", "HEAD^{tree}")
	if err != nil || tree != version.TreeSHA {
		return false
	}
	_, err = runGit(ctx, repo, nil, "diff", "--quiet", "--no-ext-diff", "HEAD", "--")
	if err != nil {
		return false
	}
	_, err = runGit(ctx, repo, nil, "diff", "--cached", "--quiet", "--no-ext-diff", "HEAD", "--")
	return err == nil
}

func trimmedError(err error) string {
	message := err.Error()
	if len(message) > 400 {
		message = message[:400]
	}
	return message
}

func parseNumstat(raw string) []codeFileStat {
	files := []codeFileStat{}
	for _, line := range strings.Split(raw, "\n") {
		parts := strings.SplitN(line, "\t", 3)
		if len(parts) != 3 {
			continue
		}
		stat := codeFileStat{Path: parts[2]}
		if parts[0] == "-" || parts[1] == "-" {
			stat.Binary = true
		} else {
			stat.Added, _ = strconv.Atoi(parts[0])
			stat.Deleted, _ = strconv.Atoi(parts[1])
		}
		files = append(files, stat)
	}
	return files
}

// shellCommand runs one declared command line with the platform shell.
func shellCommand(ctx context.Context, line string) *exec.Cmd {
	if runtime.GOOS == "windows" {
		if sh, err := exec.LookPath("sh"); err == nil {
			return exec.CommandContext(ctx, sh, "-c", line)
		}
		return exec.CommandContext(ctx, "cmd", "/C", line)
	}
	return exec.CommandContext(ctx, "sh", "-c", line)
}

// runCodeCommand runs one declared command; its complete output also goes to
// the task log stream when one is given.
func runCodeCommand(ctx context.Context, dir, line string, logs *taskLogs, stream string) codeCommand {
	record := codeCommand{Command: line, StartedAt: time.Now().UTC()}
	ctx, cancel := context.WithTimeout(ctx, codeCommandTimeout)
	defer cancel()
	command := shellCommand(ctx, line)
	command.Dir = dir
	digest := sha256.New()
	var tail bytes.Buffer
	writers := []io.Writer{digest, &boundedTail{buffer: &tail, limit: codeOutputTailBytes}, &countingWriter{count: &record.OutputBytes}}
	var logLines *lineWriter
	if logs != nil {
		logs.write(stream, "$ "+line)
		logLines = &lineWriter{logs: logs, stream: stream}
		writers = append(writers, logLines)
	}
	writer := io.MultiWriter(writers...)
	command.Stdout, command.Stderr = writer, writer
	err := command.Run()
	if logLines != nil {
		logLines.close()
	}
	record.DurationMS = time.Since(record.StartedAt).Milliseconds()
	record.OutputSHA256 = hex.EncodeToString(digest.Sum(nil))
	record.OutputTail = strings.ToValidUTF8(tail.String(), "�")
	switch {
	case err == nil:
		record.ExitCode = 0
	case ctx.Err() == context.DeadlineExceeded:
		record.ExitCode, record.TimedOut = -1, true
	default:
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			record.ExitCode = exitErr.ExitCode()
		} else {
			record.ExitCode = -1
			record.OutputTail = strings.ToValidUTF8(record.OutputTail+"\n"+err.Error(), "�")
		}
	}
	return record
}

type boundedTail struct {
	buffer *bytes.Buffer
	limit  int
}

func (tail *boundedTail) Write(data []byte) (int, error) {
	tail.buffer.Write(data)
	if extra := tail.buffer.Len() - tail.limit; extra > 0 {
		kept := append([]byte(nil), tail.buffer.Bytes()[extra:]...)
		tail.buffer.Reset()
		tail.buffer.Write(kept)
	}
	return len(data), nil
}

type countingWriter struct{ count *int64 }

func (writer *countingWriter) Write(data []byte) (int, error) {
	*writer.count += int64(len(data))
	return len(data), nil
}
