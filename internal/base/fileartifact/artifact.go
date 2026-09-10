// Package fileartifact defines bounded UTF-8 files exchanged between workers and delivery storage.
package fileartifact

import (
	"fmt"
	"path"
	"strings"
	"unicode/utf8"
)

// File contains a relative output name and its complete UTF-8 content.
type File struct {
	Path        string `json:"path"`
	ContentType string `json:"content_type"`
	Content     string `json:"content"`
}

const (
	MaxArtifactCount       = 128
	MaxArtifactBytes       = 256 * 1024
	MaxArtifactsTotalBytes = 1024 * 1024
)

// Validate accepts only bounded UTF-8 files with relative delivery
// names. Host paths and traversal are rejected.
func Validate(artifacts []File) error {
	if len(artifacts) > MaxArtifactCount {
		return fmt.Errorf("too many engine artifacts")
	}
	total := 0
	seen := make(map[string]struct{}, len(artifacts))
	for index, artifact := range artifacts {
		name := strings.TrimSpace(artifact.Path)
		if name == "" || len(name) > 512 || !utf8.ValidString(name) || strings.Contains(name, "\\") || path.IsAbs(name) || path.Clean(name) != name || name == "." || strings.HasPrefix(name, "../") {
			return fmt.Errorf("engine artifact %d path is invalid", index)
		}
		if _, exists := seen[name]; exists {
			return fmt.Errorf("engine artifact %d path is duplicated", index)
		}
		seen[name] = struct{}{}
		if strings.TrimSpace(artifact.ContentType) == "" || len(artifact.ContentType) > 160 || !utf8.ValidString(artifact.ContentType) || !utf8.ValidString(artifact.Content) || len(artifact.Content) > MaxArtifactBytes {
			return fmt.Errorf("engine artifact %d content is invalid", index)
		}
		total += len(artifact.Content)
		if total > MaxArtifactsTotalBytes {
			return fmt.Errorf("engine artifacts exceed total size bound")
		}
	}
	return nil
}
