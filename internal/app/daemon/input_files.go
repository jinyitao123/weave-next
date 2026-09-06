package daemon

import (
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/jinyitao123/weave/internal/kernel/runtimes"
)

// Called only in the task's own isolated workdir, before the engine starts.
func materializeInputFiles(workDir string, files []runtimes.InputFile) error {
	if err := runtimes.ValidateInputFiles(files); err != nil {
		return err
	}
	if len(files) == 0 {
		return nil
	}
	manifest := make([]map[string]any, 0, len(files))
	for _, file := range files {
		name := filepath.Join(workDir, "inputs", filepath.FromSlash(file.Path))
		if err := os.MkdirAll(filepath.Dir(name), 0700); err != nil {
			return err
		}
		if err := os.WriteFile(name, []byte(file.Content), 0444); err != nil {
			return err
		}
		manifest = append(manifest, map[string]any{"path": file.Path, "source_task_id": file.TaskID, "source_node_id": file.NodeID, "sha256": file.SHA256, "size_bytes": len(file.Content), "content_type": file.ContentType})
	}
	content, err := json.MarshalIndent(map[string]any{"schema_version": 1, "files": manifest}, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(workDir, "inputs", "manifest.json"), content, 0444)
}
