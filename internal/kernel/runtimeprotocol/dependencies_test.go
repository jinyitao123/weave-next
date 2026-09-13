package runtimeprotocol

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"testing"
)

// Inspect the production closure, not only direct imports. A storage port in
// an otherwise data-only package used to pull pgx into runtimeagent unseen.
func TestPureRuntimeBoundaryDependencies(t *testing.T) {
	const prefix = "github.com/jinyitao123/weave/internal/"
	allowed := map[string]bool{}
	for _, name := range []string{
		"base/execution", "base/fileartifact", "base/frozen",
		"kernel/engine", "kernel/registry", "kernel/execspec", "kernel/executionport",
		"kernel/runtimeprotocol", "kernel/runtimeagent", "kernel/runtimehost", "kernel/loomadapter",
	} {
		allowed[prefix+name] = true
	}
	type platform struct{ os, arch string }
	targets := []platform{{runtime.GOOS, runtime.GOARCH}}
	for _, candidate := range []platform{{"linux", "amd64"}, {"windows", "amd64"}} {
		if candidate != targets[0] {
			targets = append(targets, candidate)
		}
	}
	for _, target := range targets {
		t.Run(target.os+"_"+target.arch, func(t *testing.T) {
			args := []string{"list", "-deps", "-json"}
			for _, name := range []string{"registry", "execspec", "executionport", "runtimeagent", "runtimeprotocol", "runtimehost", "loomadapter"} {
				args = append(args, prefix+"kernel/"+name)
			}
			cmd := exec.Command("go", args...)
			for _, entry := range os.Environ() {
				if !strings.HasPrefix(entry, "GOOS=") && !strings.HasPrefix(entry, "GOARCH=") && !strings.HasPrefix(entry, "CGO_ENABLED=") {
					cmd.Env = append(cmd.Env, entry)
				}
			}
			cmd.Env = append(cmd.Env, "GOOS="+target.os, "GOARCH="+target.arch, "CGO_ENABLED=0")
			var stderr bytes.Buffer
			cmd.Stderr = &stderr
			output, err := cmd.Output()
			if err != nil {
				t.Fatalf("inspect %s production closure: %v\n%s", target.os+"/"+target.arch, err, stderr.String())
			}
			decoder := json.NewDecoder(bytes.NewReader(output))
			visited := 0
			for {
				var dependency struct{ ImportPath string }
				err := decoder.Decode(&dependency)
				if err == io.EOF {
					break
				}
				if err != nil {
					t.Fatal(err)
				}
				name := dependency.ImportPath
				if strings.HasPrefix(name, prefix) && !allowed[name] {
					t.Errorf("runtime contract closure reaches platform implementation %s", name)
				}
				if strings.HasPrefix(name, "github.com/jackc/pgx") || name == "github.com/jinyitao123/loom/pgstore" || name == "database/sql" {
					t.Errorf("runtime contract closure reaches database implementation %s", name)
				}
				visited++
			}
			if visited == 0 {
				t.Fatal("dependency closure is empty")
			}
		})
	}
}
