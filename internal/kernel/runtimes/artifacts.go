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

	"github.com/jinyitao123/weave/internal/base/fileartifact"
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

// CollectRunOutputArtifacts attaches current files and an independent collection
// receipt. Business gaps never rewrite the engine's terminal outcome.
func CollectRunOutputArtifacts(workDir string, before OutputArtifactSnapshot, result *engine.RunResult) error {
	result.Artifacts, result.Output, result.ArtifactCollection = collectOutputArtifacts(workDir, before, result.Output)
	for _, issue := range result.ArtifactCollection.Issues {
		if len(result.Diagnostics) >= 32 {
			break // Never discard an original engine diagnostic for collection.
		}
		if !issue.Claimed && issue.Kind != "missing_reference" {
			continue
		}
		result.Diagnostics = append(result.Diagnostics, engine.Diagnostic{
			Code: "delivery_artifact_uncollected", Message: fmt.Sprintf("%s: %q", issue.Reason, issue.Path),
		})
		break
	}
	return fileartifact.CollectionError(result.ArtifactCollection)
}

func collectOutputArtifacts(workDir string, before OutputArtifactSnapshot, answer string) ([]engine.Artifact, string, *fileartifact.CollectionEvidence) {
	root := filepath.Join(workDir, "outputs")
	entries, discoveryIssues := discoverOutputArtifactFiles(workDir)
	evidence := &fileartifact.CollectionEvidence{SchemaVersion: 1, Complete: true,
		Limits: fileartifact.CollectionLimits{MaxFiles: engine.MaxArtifactCount, MaxFileBytes: engine.MaxArtifactBytes, MaxTotalBytes: engine.MaxArtifactsTotalBytes}}
	issueKeys := map[string]bool{}
	appendIssue := func(issue fileartifact.CollectionIssue) {
		key := issue.Path + "\x00" + issue.Reason
		if issueKeys[key] {
			return
		}
		issueKeys[key] = true
		if issue.Kind == "limit" || issue.Kind == "error" {
			evidence.Complete = false
		}
		if len(evidence.Issues) >= fileartifact.MaxCollectionIssues-1 {
			evidence.Complete = false
			if len(evidence.Issues) == fileartifact.MaxCollectionIssues-1 {
				evidence.Issues = append(evidence.Issues, fileartifact.CollectionIssue{Kind: "limit", Reason: "collection_issues_truncated"})
			}
			if issue.Kind == "error" {
				evidence.Issues[len(evidence.Issues)-1] = issue
			}
			return
		}
		evidence.Issues = append(evidence.Issues, issue)
	}
	for _, issue := range discoveryIssues {
		appendIssue(issue)
	}
	rootFiles, rootErr := discoverRootArtifactFiles(workDir)
	if rootErr != nil {
		appendIssue(fileartifact.CollectionIssue{Kind: "error", Reason: "work_directory_unreadable"})
	}
	if answer != "" {
		for _, name := range rootFiles {
			if engine.ReferencesArtifact(answer, filepath.Base(name)) || engine.ReferencesArtifact(answer, filepath.ToSlash(name)) {
				entries = append(entries, name)
			}
		}
	}
	artifacts := make([]engine.Artifact, 0, min(len(entries), engine.MaxArtifactCount))
	seen := make(map[string]bool)
	savedReferences := make(map[string]bool)
	rejectedReferences := make(map[string]fileartifact.CollectionIssue)
	normalized := answer
	total := 0
	for _, name := range entries {
		relative, err := filepath.Rel(root, name)
		if err != nil {
			appendIssue(fileartifact.CollectionIssue{Kind: "error", Reason: "artifact_path_invalid"})
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
		reject := func(reason, kind string) {
			label := relative
			if len(label) > 200 {
				label = "referenced file"
			}
			issue := fileartifact.CollectionIssue{Path: label, Reason: reason, Kind: kind}
			for _, reference := range references {
				issue.Claimed = issue.Claimed || claimsCreatedArtifact(answer, reference)
				if !savedReferences[reference] {
					rejectedReferences[reference] = issue
				}
			}
			// A capability limit or technical failure matters even if the answer
			// omits the file. Ordinary stale files matter only when referenced.
			if kind != "missing_reference" {
				appendIssue(issue)
			}
		}
		contentType, supported := outputArtifactTypes[strings.ToLower(filepath.Ext(name))]
		if !supported {
			reject("unsupported_file_type", "limit")
			continue
		}
		if isRoot && before == nil {
			reject("invocation_snapshot_missing", "limit")
			continue
		}
		info, err := os.Lstat(name)
		if err != nil {
			reject("file_metadata_unreadable", "error")
			continue
		}
		if !info.Mode().IsRegular() || info.Size() < 0 {
			reject("file_not_regular", "limit")
			continue
		}
		if info.Size() > engine.MaxArtifactBytes {
			reject("file_exceeds_256_kib", "limit")
			continue
		}
		if len(artifacts) >= engine.MaxArtifactCount {
			reject(fmt.Sprintf("file_count_exceeds_%d", engine.MaxArtifactCount), "limit")
			continue
		}
		if info.Size()+int64(total) > engine.MaxArtifactsTotalBytes {
			reject("files_exceed_1_mib_total", "limit")
			continue
		}
		content, err := os.ReadFile(name)
		if err != nil {
			reject("file_unreadable", "error")
			continue
		}
		if !utf8.Valid(content) {
			reject("file_not_utf8", "error")
			continue
		}
		if len(content) > engine.MaxArtifactBytes || total+len(content) > engine.MaxArtifactsTotalBytes {
			reject("file_changed_beyond_size_limit", "limit")
			continue
		}
		if stamp, existed := before[name]; existed && stamp.Digest == sha256.Sum256(content) &&
			stamp.ModTime == info.ModTime().UnixNano() && stamp.Size == info.Size() {
			reject("file_not_written_by_this_invocation", "missing_reference")
			continue
		}
		if seen[relative] {
			reject("delivery_name_conflict", "error")
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
	if err := engine.ValidateArtifacts(artifacts); err != nil {
		appendIssue(fileartifact.CollectionIssue{Kind: "error", Reason: "invalid_delivery_files"})
		return nil, answer, evidence
	}
	references := make([]string, 0, len(rejectedReferences))
	for reference := range rejectedReferences {
		references = append(references, reference)
	}
	sort.Strings(references)
	for _, reference := range references {
		appendIssue(rejectedReferences[reference])
	}
	for _, issue := range uncollectedOutputReferences(workDir, answer, savedReferences, rejectedReferences) {
		appendIssue(issue)
	}
	return artifacts, normalized, evidence
}

// Inspect explicit delivery references without opening arbitrary host paths.
func uncollectedOutputReferences(workDir, answer string, saved map[string]bool, rejected map[string]fileartifact.CollectionIssue) []fileartifact.CollectionIssue {
	absolute, err := filepath.Abs(workDir)
	if err != nil {
		return []fileartifact.CollectionIssue{{Kind: "error", Reason: "workdir_path_invalid"}}
	}
	prefix := filepath.ToSlash(absolute) + "/"
	pattern := "(^|[\\s`\"'\\[(])((?:" + regexp.QuoteMeta(prefix) + "|outputs/)[^\\s`\"'()\\[\\]<>]+)"
	var issues []fileartifact.CollectionIssue
	for _, match := range regexp.MustCompile(pattern).FindAllStringSubmatch(answer, -1) {
		reference := strings.TrimRight(match[2], ".,;!?")
		if file, line, found := strings.Cut(reference, ":"); found && line != "" && strings.Trim(line, "0123456789") == "" {
			reference = file
		}
		if filepath.Ext(reference) == "" || saved[reference] {
			continue
		}
		if _, exists := rejected[reference]; exists {
			continue
		}
		label := strings.TrimPrefix(reference, prefix)
		if len(label) > 200 {
			label = "referenced file"
		}
		issues = append(issues, fileartifact.CollectionIssue{Path: label, Kind: "missing_reference", Reason: "file_not_collected", Claimed: claimsCreatedArtifact(answer, reference)})
	}
	return issues
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
	files, _ := discoverRootArtifactFiles(workDir)
	return files
}

func discoverRootArtifactFiles(workDir string) ([]string, error) {
	entries, err := os.ReadDir(workDir)
	if err != nil {
		return nil, err
	}
	files := make([]string, 0)
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || strings.HasPrefix(name, ".") || strings.EqualFold(name, "AGENTS.md") || strings.EqualFold(name, "CLAUDE.md") {
			continue
		}
		files = append(files, filepath.Join(workDir, name))
	}
	return files, nil
}

func outputArtifactFiles(workDir string) []string {
	files, _ := discoverOutputArtifactFiles(workDir)
	return files
}

func discoverOutputArtifactFiles(workDir string) ([]string, []fileartifact.CollectionIssue) {
	var issues []fileartifact.CollectionIssue
	root := filepath.Join(workDir, "outputs")
	entries := make([]string, 0)
	_ = filepath.WalkDir(root, func(name string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			if name != root || !os.IsNotExist(walkErr) {
				issues = append(issues, fileartifact.CollectionIssue{Kind: "error", Reason: "output_directory_unreadable"})
			}
			return nil
		}
		if entry == nil {
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
	return entries, issues
}
