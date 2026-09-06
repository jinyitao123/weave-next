// Package execenv materializes the files needed by external CLI agent runtimes.
package execenv

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/jinyitao123/weave/internal/kernel/grounding"
	"github.com/jinyitao123/weave/internal/kernel/registry"
)

const defaultWorkspace = "_default"

const teamOrchestrationGuard = `# Platform orchestration boundary

Complete only the current workflow node's assigned responsibilities. Do not spawn or delegate to native CLI sub-agents. Weave owns team delegation, parallelism, review, retries, and delivery through the published workflow.

Return the complete assigned result in your final answer, not a completion receipt. For file deliverables, write supported UTF-8 files below outputs/ and explicitly reference their relative paths in the final answer. The platform collects only this invocation's new or rewritten files; local files elsewhere and unchanged files are not a delivery channel. Binary images and archives are not collected. When a delivered page needs a binary visual, provide a supported UTF-8 representation such as an SVG or HTML file embedding the actual image, and keep the editable source. Verify that the files being delivered include every required local link, resource, and script dependency; a file existing on this host does not establish its delivery. Never claim that a file was saved to Workbench yourself.`

// Attachment identifies a file to copy into an external CLI agent's workdir.
type Attachment struct {
	Filename string
	Path     string
}

// Materialize writes the runtime-neutral agent instructions and skills into a
// stable per-workspace, per-agent directory. Prompt is accepted as part of the
// materialization boundary but is passed to the CLI by the engine, not written
// to disk or copied into the environment.
func Materialize(root string, rec *registry.AgentRecord, prompt string, attachments []Attachment) (workDir string, env map[string]string, err error) {
	if rec == nil {
		return "", nil, errors.New("execenv: nil agent record")
	}
	if root == "" {
		return "", nil, errors.New("execenv: empty workspace root")
	}
	if err := validPathComponent(rec.Name); err != nil {
		return "", nil, fmt.Errorf("execenv: agent name: %w", err)
	}
	workspace := rec.WorkspaceID
	if workspace == "" {
		workspace = defaultWorkspace
	}
	if err := validPathComponent(workspace); err != nil {
		return "", nil, fmt.Errorf("execenv: workspace ID: %w", err)
	}

	workDir = filepath.Join(root, workspace, rec.Name, "workdir")
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		return "", nil, fmt.Errorf("execenv: create workdir: %w", err)
	}
	if err := writeFileIfChanged(filepath.Join(workDir, "AGENTS.md"), agentInstructions(rec), 0o644); err != nil {
		return "", nil, err
	}

	for _, skill := range rec.Spec.Skills {
		if skill.Body == "" {
			continue
		}
		if err := validPathComponent(skill.Name); err != nil {
			return "", nil, fmt.Errorf("execenv: skill name %q: %w", skill.Name, err)
		}
		path := filepath.Join(workDir, ".claude", "skills", skill.Name, "SKILL.md")
		if err := writeFileIfChanged(path, []byte(skill.Body), 0o644); err != nil {
			return "", nil, err
		}
	}
	for _, attachment := range attachments {
		content, readErr := os.ReadFile(attachment.Path)
		if readErr != nil {
			return "", nil, fmt.Errorf("execenv: read attachment %s: %w", attachment.Path, readErr)
		}
		path := filepath.Join(workDir, "attachments", attachment.Filename)
		if writeErr := writeFileIfChanged(path, content, 0o644); writeErr != nil {
			return "", nil, writeErr
		}
	}

	_ = prompt
	return workDir, map[string]string{}, nil
}

func agentInstructions(rec *registry.AgentRecord) []byte {
	var b strings.Builder
	b.WriteString("# Identity\n\n")
	b.WriteString(resolveIdentity(rec))
	if isPlatformManagedTeamAgent(rec) {
		b.WriteString("\n\n")
		b.WriteString(teamOrchestrationGuard)
	}
	b.WriteString("\n\n# Tool permissions\n\n")
	writePermissionRule(&b, "Allowed", rec.Permissions.Allow)
	writePermissionRule(&b, "Denied", rec.Permissions.Deny)
	writePermissionRule(&b, "Require confirmation", rec.Permissions.Ask)
	return []byte(b.String())
}

func isPlatformManagedTeamAgent(rec *registry.AgentRecord) bool {
	if rec == nil {
		return false
	}
	if strings.TrimSpace(rec.TeamID) != "" {
		return true
	}
	// TeamForge assets historically bind membership through the roster while
	// leaving AgentRecord.TeamID empty. The compiler-owned tag is therefore the
	// durable compatibility marker for those existing team agents.
	for _, tag := range rec.Tags {
		if strings.TrimSpace(tag) == "team-build-managed" {
			return true
		}
	}
	// Runtime task payloads intentionally carry a minimized AgentRecord and
	// older payloads omit both TeamID and Tags. TeamForge's compiler-owned
	// identity prefix remains present and is the final compatibility sentinel.
	identity := rec.Spec.Identity
	for _, value := range []string{
		identity.Core, identity.Extended, identity.Raw, rec.Spec.SystemPrompt,
	} {
		if strings.Contains(value, "in a platform-managed team.") {
			return true
		}
	}
	return false
}

func resolveIdentity(rec *registry.AgentRecord) string {
	// CLI-engine agents (opencode/codex/claude) never pass through CompileAgent,
	// so the anti-fabrication rule is attached here via the same shared helper —
	// this is historically the most fabrication-prone runtime, so it must not be
	// the one path that misses grounding.
	identity := rec.Spec.Identity
	var core string
	switch {
	case identity.Core != "" || identity.Extended != "":
		core = strings.TrimSpace(strings.TrimSpace(identity.Core) + "\n\n" + strings.TrimSpace(identity.Extended))
	case identity.Raw != "":
		core = strings.TrimSpace(identity.Raw)
	default:
		core = strings.TrimSpace(rec.Spec.SystemPrompt)
	}
	return grounding.GroundIdentity(core)
}

func writePermissionRule(b *strings.Builder, label string, tools []string) {
	if len(tools) == 0 {
		return
	}
	fmt.Fprintf(b, "- %s tools: %s.\n", label, strings.Join(tools, ", "))
}

func validPathComponent(value string) error {
	if value == "" || value == "." || value == ".." || filepath.Base(value) != value || strings.ContainsAny(value, `/\\`) {
		return fmt.Errorf("invalid path component %q", value)
	}
	return nil
}

func writeFileIfChanged(path string, content []byte, mode os.FileMode) error {
	existing, err := os.ReadFile(path)
	if err == nil {
		if sha256.Sum256(existing) == sha256.Sum256(content) {
			return nil
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("execenv: read %s: %w", path, err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("execenv: create directory for %s: %w", path, err)
	}
	if err := os.WriteFile(path, content, mode); err != nil {
		return fmt.Errorf("execenv: write %s: %w", path, err)
	}
	return nil
}
