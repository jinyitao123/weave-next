package runtimes

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"path"
	"strings"

	"github.com/jinyitao123/weave/internal/base/execution"
	"github.com/jinyitao123/weave/internal/kernel/taskqueue"
	"github.com/jinyitao123/weave/internal/kernel/engine"
)

const MaxInputFiles = 512
const MaxInputFilesBytes = 8 * 1024 * 1024

// InputFile is an immutable file copied from an exact upstream task receipt.
// Path is namespaced by its producer node, with SHA256 covering the UTF-8 bytes.
type InputFile struct {
	TaskID      string `json:"task_id"`
	NodeID      string `json:"node_id"`
	Path        string `json:"path"`
	ContentType string `json:"content_type"`
	SHA256      string `json:"sha256"`
	Content     string `json:"content"`
}

func ValidateInputFiles(files []InputFile) error {
	if len(files) > MaxInputFiles {
		return fmt.Errorf("too many upstream files")
	}
	seen := map[string]bool{}
	total := 0
	for _, file := range files {
		if file.TaskID == "" || file.NodeID == "" || path.Base(file.NodeID) != file.NodeID || file.NodeID == "." || file.NodeID == ".." || strings.Contains(file.NodeID, "\\") || !strings.HasPrefix(file.Path, file.NodeID+"/") {
			return fmt.Errorf("upstream file source identity is invalid")
		}
		if err := engine.ValidateArtifacts([]engine.Artifact{{Path: file.Path, ContentType: file.ContentType, Content: file.Content}}); err != nil {
			return err
		}
		digest := sha256.Sum256([]byte(file.Content))
		if hex.EncodeToString(digest[:]) != file.SHA256 || seen[file.Path] {
			return fmt.Errorf("upstream file hash or path conflicts")
		}
		seen[file.Path] = true
		total += len(file.Content)
		if total > MaxInputFilesBytes {
			return fmt.Errorf("upstream files exceed size bound")
		}
	}
	return nil
}

func (e *Executor) inputFiles(ctx context.Context, workspaceID, snapshotID string) ([]InputFile, error) {
	var files []InputFile
	seen := map[string]bool{}
	for _, id := range execution.InputTaskIDs(ctx) {
		if seen[id] {
			continue
		}
		seen[id] = true
		task, err := e.tasks.Get(ctx, workspaceID, id)
		if err != nil {
			return nil, err
		}
		if snapshotID == "" || task.RunSnapshotID != snapshotID || task.Kind != "engine_exec" || task.Status != taskqueue.StatusCompleted {
			return nil, fmt.Errorf("upstream task %s is outside this completed workflow", id)
		}
		var payload EngineExecRequest
		var result EngineExecResult
		if err := json.Unmarshal(task.Payload, &payload); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(task.Result, &result); err != nil {
			return nil, err
		}
		if result.Status != "" && result.Status != "completed" {
			return nil, fmt.Errorf("upstream task %s did not complete successfully", id)
		}
		if err := engine.ValidateArtifacts(result.Artifacts); err != nil {
			return nil, err
		}
		for _, artifact := range result.Artifacts {
			hash := sha256.Sum256([]byte(artifact.Content))
			files = append(files, InputFile{TaskID: id, NodeID: payload.NodeID, Path: payload.NodeID + "/" + artifact.Path, ContentType: artifact.ContentType, SHA256: hex.EncodeToString(hash[:]), Content: artifact.Content})
		}
	}
	return files, ValidateInputFiles(files)
}

func promptWithInputFiles(prompt string, files []InputFile) string {
	if len(files) == 0 {
		return prompt
	}
	var notice strings.Builder
	notice.WriteString("\n\nUpstream files provided by Weave in inputs/ (source and SHA-256 are in inputs/manifest.json). Read these exact files. Do not recreate a frozen baseline or search another worker's host directory. Preserve required application dependencies in outputs/ when assembling final delivery.\n")
	for _, file := range files {
		fmt.Fprintf(&notice, "- inputs/%s (source %s, sha256 %s)\n", file.Path, file.TaskID, file.SHA256)
	}
	return prompt + notice.String()
}

// SafeEndpointOrigin exposes provider location without paths or credentials.
func SafeEndpointOrigin(value string) string {
	parsed, err := url.Parse(strings.TrimSpace(value))
	if err != nil || parsed.Host == "" || (parsed.Scheme != "https" && parsed.Scheme != "http") {
		return ""
	}
	return parsed.Scheme + "://" + parsed.Host
}
