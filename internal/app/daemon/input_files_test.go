package daemon

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/jinyitao123/weave/internal/kernel/runtimes"
)

func TestMaterializeUpstreamFileVerifiesContentBeforeWriting(t *testing.T) {
	content := "frozen: true\nvelocity: 0.05\n"
	file := runtimes.InputFile{TaskID: "task-source", NodeID: "lead", Path: "lead/baseline_frozen.yaml", ContentType: "application/yaml", Content: content, SHA256: fmt.Sprintf("%x", sha256.Sum256([]byte(content)))}
	for _, corrupt := range []string{"", "hash", "path"} {
		t.Run(corrupt, func(t *testing.T) {
			root := t.TempDir()
			input := file
			if corrupt == "hash" {
				input.Content = "different frozen version"
			}
			if corrupt == "path" {
				input.Path = "lead/../../outside.yaml"
			}
			err := materializeInputFiles(root, []runtimes.InputFile{input})
			if corrupt != "" {
				if err == nil {
					t.Fatal("corrupt file accepted")
				}
				if _, err := os.Stat(filepath.Join(root, "inputs")); !os.IsNotExist(err) {
					t.Fatal("partial input set written before validation")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			actual, err := os.ReadFile(filepath.Join(root, "inputs", "lead", "baseline_frozen.yaml"))
			if err != nil || string(actual) != content {
				t.Fatalf("mounted=%q err=%v", actual, err)
			}
			if _, err := os.Stat(filepath.Join(root, "inputs", "manifest.json")); err != nil {
				t.Fatal(err)
			}
		})
	}
}
