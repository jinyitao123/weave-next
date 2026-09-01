package runtimes

import (
	"crypto/sha256"
	"io/fs"
	"mime"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/jinyitao123/weave/internal/kernel/engine"
)

var outputArtifactTypes = map[string]string{
	".csv": "text/csv", ".html": "text/html", ".json": "application/json",
	".md": "text/markdown", ".svg": "image/svg+xml", ".tsv": "text/tab-separated-values",
	".txt": "text/plain", ".yaml": "application/yaml", ".yml": "application/yaml",
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
	for _, name := range outputArtifactFiles(workDir) {
		content, err := os.ReadFile(name)
		info, statErr := os.Stat(name)
		if err == nil && statErr == nil && len(content) <= engine.MaxArtifactBytes {
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
// rewritten after the supplied snapshot.
func CollectOutputArtifactsSince(workDir string, before OutputArtifactSnapshot) []engine.Artifact {
	root := filepath.Join(workDir, "outputs")
	entries := outputArtifactFiles(workDir)
	artifacts := make([]engine.Artifact, 0, min(len(entries), engine.MaxArtifactCount))
	total := 0
	for _, name := range entries {
		if len(artifacts) >= engine.MaxArtifactCount {
			break
		}
		extension := strings.ToLower(filepath.Ext(name))
		contentType := outputArtifactTypes[extension]
		info, err := os.Stat(name)
		if err != nil || !info.Mode().IsRegular() || info.Size() < 0 || info.Size() > engine.MaxArtifactBytes {
			continue
		}
		content, err := os.ReadFile(name)
		if err != nil || total+len(content) > engine.MaxArtifactsTotalBytes || !utf8.Valid(content) {
			continue
		}
		if stamp, existed := before[name]; existed && stamp.Digest == sha256.Sum256(content) &&
			stamp.ModTime == info.ModTime().UnixNano() && stamp.Size == info.Size() {
			continue
		}
		relative, err := filepath.Rel(root, name)
		if err != nil {
			continue
		}
		if detected := mime.TypeByExtension(extension); detected != "" {
			contentType = strings.SplitN(detected, ";", 2)[0]
		}
		artifacts = append(artifacts, engine.Artifact{
			Path: filepath.ToSlash(relative), ContentType: contentType, Content: string(content),
		})
		total += len(content)
	}
	if engine.ValidateArtifacts(artifacts) != nil {
		return nil
	}
	return artifacts
}

func outputArtifactFiles(workDir string) []string {
	root := filepath.Join(workDir, "outputs")
	entries := make([]string, 0)
	_ = filepath.WalkDir(root, func(name string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil || entry == nil {
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.Type().IsRegular() {
			if _, supported := outputArtifactTypes[strings.ToLower(filepath.Ext(name))]; supported {
				entries = append(entries, name)
			}
		}
		return nil
	})
	sort.Strings(entries)
	return entries
}
