package runtimes

import (
	"crypto/sha256"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/jinyitao123/weave/internal/kernel/engine"
)

var outputArtifactTypes = map[string]string{
	".css": "text/css", ".js": "text/javascript", ".mjs": "text/javascript", ".cjs": "text/javascript",
	".py": "text/x-python", ".rb": "text/x-ruby", ".sh": "text/x-shellscript", ".log": "text/plain",
	".csv": "text/csv", ".html": "text/html", ".json": "application/json", ".jsonl": "application/x-ndjson",
	".md": "text/markdown", ".svg": "image/svg+xml", ".tsv": "text/tab-separated-values",
	".scad": "text/x-openscad", ".dxf": "image/vnd.dxf", ".txt": "text/plain", ".yaml": "application/yaml", ".yml": "application/yaml",
}

var ignoredOutputArtifactDirectories = map[string]struct{}{
	".git": {}, ".next": {}, ".turbo": {}, ".vinext": {}, ".wrangler": {}, "coverage": {}, "node_modules": {},
}

// OutputArtifactSnapshot identifies eligible files that already existed before
// an invocation, preventing a persistent runtime workspace from re-publishing
// stale outputs from an earlier run.
type outputArtifactStamp struct {
	Digest  [sha256.Size]byte
	ModTime int64
	Size    int64
}

type OutputArtifactSnapshot map[string]outputArtifactStamp

// SnapshotOutputArtifacts captures eligible outputs without exposing content.
func SnapshotOutputArtifacts(workDir string) OutputArtifactSnapshot {
	snapshot := OutputArtifactSnapshot{}
	for _, name := range append(outputArtifactFiles(workDir), rootArtifactFiles(workDir)...) {
		if _, supported := outputArtifactTypes[strings.ToLower(filepath.Ext(name))]; !supported {
			continue
		}
		info, err := os.Lstat(name)
		if err != nil || !info.Mode().IsRegular() || info.Size() > engine.MaxArtifactBytes {
			continue
		}
		content, err := os.ReadFile(name)
		if err == nil && len(content) <= engine.MaxArtifactBytes {
			snapshot[name] = outputArtifactStamp{
				Digest: sha256.Sum256(content), ModTime: info.ModTime().UnixNano(), Size: info.Size(),
			}
		}
	}
	return snapshot
}

// CollectOutputArtifacts reads bounded, regular UTF-8 files explicitly placed
// below outputs/. Symlinks, binary files, oversized files, and host paths are
// never transported through the runtime result.
func CollectOutputArtifacts(workDir string) []engine.Artifact {
	return CollectOutputArtifactsSince(workDir, nil)
}

// CollectOutputArtifactsSince returns only files created, content-changed, or
// rewritten after the supplied snapshot. A final answer may also explicitly
// reference a supported file in the workdir root, for older team instructions.
func CollectOutputArtifactsSince(workDir string, before OutputArtifactSnapshot, finalAnswer ...string) []engine.Artifact {
	answer := ""
	if len(finalAnswer) > 0 {
		answer = finalAnswer[0]
	}
	artifacts, _, _ := collectOutputArtifacts(workDir, before, answer)
	return artifacts
}

// CollectRunOutputArtifacts attaches current files and normalizes their explicit
// absolute references. Missing references remain diagnostics until the workflow
// selects a final delivery; a stage may legitimately describe a future file.
// An explicit claim that a file was already created still fails immediately.
func CollectRunOutputArtifacts(workDir string, before OutputArtifactSnapshot, result *engine.RunResult) {
	var gap *artifactCollectionGap
	result.Artifacts, result.Output, gap = collectOutputArtifacts(workDir, before, result.Output)
	if gap == nil {
		return
	}
	// Keep the transport bound even if the CLI filled its diagnostic channel.
	result.Diagnostics = append(result.Diagnostics[:min(len(result.Diagnostics), 31)], gap.Diagnostic)
	if result.Status == "completed" && gap.Claimed {
		result.Status, result.Err = "failed", gap.Code+": "+gap.Message
	}
}

type artifactCollectionGap struct {
	engine.Diagnostic
	Claimed bool
}

func collectOutputArtifacts(workDir string, before OutputArtifactSnapshot, answer string) ([]engine.Artifact, string, *artifactCollectionGap) {
	root := filepath.Join(workDir, "outputs")
	entries := outputArtifactFiles(workDir)
	if answer != "" {
		for _, name := range rootArtifactFiles(workDir) {
			if engine.ReferencesArtifact(answer, filepath.Base(name)) || engine.ReferencesArtifact(answer, filepath.ToSlash(name)) {
				entries = append(entries, name)
			}
		}
	}
	artifacts := make([]engine.Artifact, 0, min(len(entries), engine.MaxArtifactCount))
	seen := make(map[string]bool)
	savedReferences := make(map[string]bool)
	rejectedReferences := make(map[string]artifactCollectionGap)
	normalized := answer
	total := 0
	for _, name := range entries {
		relative, err := filepath.Rel(root, name)
		if err != nil {
			continue
		}
		isRoot := filepath.Dir(name) == filepath.Clean(workDir)
		if isRoot {
			relative = filepath.Base(name)
		}
		relative = filepath.ToSlash(relative)
		absolute, absErr := filepath.Abs(name)
		absolute = filepath.ToSlash(absolute)
		references := make([]string, 0, 3)
		for _, reference := range []string{relative, "outputs/" + relative, absolute} {
			if (isRoot && reference == "outputs/"+relative) || (absErr != nil && reference == absolute) {
				continue
			}
			if engine.ReferencesArtifact(answer, reference) {
				references = append(references, reference)
			}
		}
		reject := func(reason string) {
			// Only a bounded delivery name crosses the boundary, never a host path.
			label := relative
			if len(label) > 200 {
				label = "referenced file"
			}
			for _, reference := range references {
				if !savedReferences[reference] {
					rejectedReferences[reference] = artifactCollectionGap{
						Diagnostic: engine.Diagnostic{Code: "delivery_artifact_uncollected", Message: fmt.Sprintf("%s: %q", reason, label)},
						Claimed:    claimsCreatedArtifact(answer, reference),
					}
				}
			}
		}
		contentType, supported := outputArtifactTypes[strings.ToLower(filepath.Ext(name))]
		if !supported {
			reject("unsupported_file_type")
			continue
		}
		if isRoot && before == nil {
			reject("invocation_snapshot_missing")
			continue
		}
		info, err := os.Lstat(name)
		if err != nil || !info.Mode().IsRegular() || info.Size() < 0 {
			reject("file_unreadable_or_not_regular")
			continue
		}
		if info.Size() > engine.MaxArtifactBytes {
			reject("file_exceeds_256_kib")
			continue
		}
		if len(artifacts) >= engine.MaxArtifactCount {
			reject(fmt.Sprintf("file_count_exceeds_%d", engine.MaxArtifactCount))
			continue
		}
		if info.Size()+int64(total) > engine.MaxArtifactsTotalBytes {
			reject("files_exceed_512_kib_total")
			continue
		}
		content, err := os.ReadFile(name)
		if err != nil || !utf8.Valid(content) {
			reject("file_unreadable_or_not_utf8")
			continue
		}
		if len(content) > engine.MaxArtifactBytes || total+len(content) > engine.MaxArtifactsTotalBytes {
			reject("file_changed_beyond_size_limit")
			continue
		}
		if stamp, existed := before[name]; existed && stamp.Digest == sha256.Sum256(content) &&
			stamp.ModTime == info.ModTime().UnixNano() && stamp.Size == info.Size() {
			reject("file_not_written_by_this_invocation")
			continue
		}
		if seen[relative] {
			reject("delivery_name_conflict")
			continue
		}
		seen[relative] = true
		for _, reference := range references {
			savedReferences[reference] = true
			delete(rejectedReferences, reference)
		}
		artifacts = append(artifacts, engine.Artifact{
			Path: relative, ContentType: contentType, Content: string(content),
		})
		if absErr == nil {
			pattern := "(^|[\\s`\"'\\[(])" + regexp.QuoteMeta(absolute) + "($|[\\s`\"'\\]),:;!?]|\\.(?:\\s|$))"
			normalized = regexp.MustCompile(pattern).ReplaceAllStringFunc(normalized, func(reference string) string {
				return strings.Replace(reference, absolute, relative, 1)
			})
		}
		total += len(content)
	}
	if engine.ValidateArtifacts(artifacts) != nil {
		return nil, answer, &artifactCollectionGap{
			Diagnostic: engine.Diagnostic{Code: "delivery_artifact_uncollected", Message: "invalid_delivery_files"}, Claimed: true,
		}
	}
	if len(rejectedReferences) > 0 {
		references := make([]string, 0, len(rejectedReferences))
		for reference := range rejectedReferences {
			references = append(references, reference)
		}
		sort.Strings(references)
		gap := rejectedReferences[references[0]]
		for _, reference := range references {
			if claimed := rejectedReferences[reference]; claimed.Claimed {
				return artifacts, normalized, &claimed
			}
		}
		if missing := uncollectedOutputReference(workDir, answer, savedReferences); missing != nil && missing.Claimed {
			return artifacts, normalized, missing
		}
		return artifacts, normalized, &gap
	}
	return artifacts, normalized, uncollectedOutputReference(workDir, answer, savedReferences)
}

// Check explicit delivery-path references without opening them. This catches a
// missing file or an unsupported location without extending filesystem access.
func uncollectedOutputReference(workDir, answer string, saved map[string]bool) *artifactCollectionGap {
	absolute, err := filepath.Abs(workDir)
	if err != nil {
		return nil
	}
	prefix := filepath.ToSlash(absolute) + "/"
	pattern := "(^|[\\s`\"'\\[(])((?:" + regexp.QuoteMeta(prefix) + "|outputs/)[^\\s`\"'()\\[\\]<>]+)"
	var first *artifactCollectionGap
	for _, match := range regexp.MustCompile(pattern).FindAllStringSubmatch(answer, -1) {
		reference := strings.TrimRight(match[2], ".,;!?")
		// CLI Markdown links commonly point to a file and its first line.
		if file, line, found := strings.Cut(reference, ":"); found && line != "" && strings.Trim(line, "0123456789") == "" {
			reference = file
		}
		if filepath.Ext(reference) == "" || saved[reference] {
			continue
		}
		label := strings.TrimPrefix(reference, prefix)
		if len(label) > 200 {
			label = "referenced file"
		}
		gap := &artifactCollectionGap{
			Diagnostic: engine.Diagnostic{Code: "delivery_artifact_uncollected", Message: fmt.Sprintf("file_not_collected: %q", label)},
			Claimed:    claimsCreatedArtifact(answer, reference),
		}
		if gap.Claimed {
			return gap
		}
		if first == nil {
			first = gap
		}
	}
	return first
}

// This conservative early check recognizes explicit completed-action claims,
// not arbitrary mentions, instructions, quoted input, or plans. Final delivery
// validation is structural and does not depend on these language patterns.
var createdArtifactClaim = regexp.MustCompile(`(?i)^(?:(?:I (?:have )?)?(?:saved|created|wrote|written|generated)\b|已(?:经)?(?:成功)?(?:将[^。；\n]{0,64})?(?:保存|创建|写入|生成))`)

func claimsCreatedArtifact(answer, reference string) bool {
	fenced := false
	for _, line := range strings.Split(answer, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "```") || strings.HasPrefix(line, "~~~") {
			fenced = !fenced
			continue
		}
		line = strings.TrimSpace(strings.TrimPrefix(line, "- "))
		if !fenced && createdArtifactClaim.MatchString(line) && engine.ReferencesArtifact(line, reference) {
			return true
		}
	}
	return false
}

func rootArtifactFiles(workDir string) []string {
	entries, _ := os.ReadDir(workDir)
	files := make([]string, 0)
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || strings.HasPrefix(name, ".") || strings.EqualFold(name, "AGENTS.md") || strings.EqualFold(name, "CLAUDE.md") {
			continue
		}
		files = append(files, filepath.Join(workDir, name))
	}
	return files
}

func outputArtifactFiles(workDir string) []string {
	root := filepath.Join(workDir, "outputs")
	entries := make([]string, 0)
	_ = filepath.WalkDir(root, func(name string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil || entry == nil {
			return nil
		}
		if name != root && entry.IsDir() {
			if _, ignored := ignoredOutputArtifactDirectories[entry.Name()]; ignored {
				return filepath.SkipDir
			}
		}
		if !entry.IsDir() {
			// Metadata-only discovery also identifies explicit references that
			// cannot be transported. Collection still rejects symlinks/binaries.
			entries = append(entries, name)
		}
		return nil
	})
	// Prefer shallow, user-authored delivery files before framework internals.
	// The transport remains bounded, but a large application scaffold can no
	// longer displace sibling drawings, models, and acceptance manifests merely
	// because one deeply nested dependency sorts first alphabetically.
	sort.Slice(entries, func(left, right int) bool {
		leftRelative, leftErr := filepath.Rel(root, entries[left])
		rightRelative, rightErr := filepath.Rel(root, entries[right])
		if leftErr == nil && rightErr == nil {
			leftDepth := strings.Count(filepath.ToSlash(leftRelative), "/")
			rightDepth := strings.Count(filepath.ToSlash(rightRelative), "/")
			if leftDepth != rightDepth {
				return leftDepth < rightDepth
			}
		}
		return entries[left] < entries[right]
	})
	return entries
}
