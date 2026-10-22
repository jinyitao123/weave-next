package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// passthroughEnv are the only parent variables a child process inherits.
// Everything else (DATABASE_URL, provider keys, WEAVE_*) is set explicitly, so
// the load-test Weave can never pick up the production database or a real
// model key from the parent shell.
var passthroughEnv = []string{
	"SystemRoot", "SYSTEMROOT", "SystemDrive", "SYSTEMDRIVE", "windir", "WINDIR", "ComSpec", "COMSPEC",
	"PATH", "Path", "PATHEXT", "TEMP", "TMP", "USERPROFILE", "HOME", "APPDATA", "LOCALAPPDATA",
	"COMPUTERNAME", "OS", "NUMBER_OF_PROCESSORS", "PROCESSOR_ARCHITECTURE", "ProgramData", "ProgramFiles",
}

func baseEnv() []string {
	out := []string{}
	seen := map[string]bool{}
	for _, k := range passthroughEnv {
		if v, ok := os.LookupEnv(k); ok && !seen[strings.ToUpper(k)] {
			seen[strings.ToUpper(k)] = true
			out = append(out, k+"="+v)
		}
	}
	return out
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

type child struct {
	name string
	cmd  *exec.Cmd
	log  *os.File
	done chan error
}

func startChild(name, bin string, args, env []string, logPath string) (*child, error) {
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		return nil, err
	}
	logFile, err := os.Create(logPath)
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(bin, args...)
	cmd.Env = env
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	if err := cmd.Start(); err != nil {
		logFile.Close()
		return nil, fmt.Errorf("start %s: %w", name, err)
	}
	c := &child{name: name, cmd: cmd, log: logFile, done: make(chan error, 1)}
	go func() { c.done <- cmd.Wait() }()
	return c, nil
}

func (c *child) Stop() {
	if c == nil || c.cmd.Process == nil {
		return
	}
	select {
	case <-c.done:
	default:
		_ = c.cmd.Process.Kill()
		select {
		case <-c.done:
		case <-time.After(10 * time.Second):
		}
	}
	_ = c.log.Close()
}

func (c *child) Exited() bool {
	select {
	case err := <-c.done:
		c.done <- err
		return true
	default:
		return false
	}
}

func waitHTTP(ctx context.Context, url string, c *child, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	hc := &http.Client{Timeout: 2 * time.Second}
	for time.Now().Before(deadline) {
		if c != nil && c.Exited() {
			return fmt.Errorf("%s exited before becoming ready (see %s)", c.name, c.log.Name())
		}
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if resp, err := hc.Do(req); err == nil {
			resp.Body.Close()
			if resp.StatusCode < 500 {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
	return fmt.Errorf("%s not ready at %s after %s", nameOf(c), url, timeout)
}

func nameOf(c *child) string {
	if c == nil {
		return "service"
	}
	return c.name
}

// weaveInstance is a Weave server started for one load-test run.
type weaveInstance struct {
	environment []string
	child       *child
	baseURL     string
	loginKey    string
	dbURL       string
}

type weaveOptions struct {
	bin          string
	port         int
	dbURL        string
	stubURL      string
	concurrency  int
	workDir      string
	extraEnv     []string
	logPath      string
	stubModels   []string
	readyTimeout time.Duration
}

func startWeave(ctx context.Context, o weaveOptions) (*weaveInstance, error) {
	loginKey := "lt-" + randomHex(16)
	env := baseEnv()
	env = append(env,
		"DATABASE_URL="+o.dbURL,
		fmt.Sprintf("PORT=%d", o.port),
		"JWT_SECRET="+randomHex(32),
		"WEAVE_SECRET_KEY="+randomHex(32),
		"WEAVE_ADMIN_USER=loadtest",
		"WEAVE_ADMIN_PASS="+randomHex(16),
		"WEAVE_LOGIN_KEY="+loginKey,
		"WEAVE_DEV_MODE=true",
		"WEAVE_WORKSPACES_ROOT="+filepath.Join(o.workDir, "workspaces"),
		"OPENAI_API_KEY=stub-"+randomHex(8),
		"OPENAI_BASE_URL="+o.stubURL,
		"OPENAI_MODELS="+strings.Join(o.stubModels, ","),
		"DEFAULT_MODEL="+o.stubModels[0],
		"LOG_LEVEL=warn",
		fmt.Sprintf("WEAVE_TEAMRUN_EXECUTOR_CONCURRENCY=%d", o.concurrency),
	)
	env = append(env, o.extraEnv...)
	c, err := startChild("weave", o.bin, nil, env, o.logPath)
	if err != nil {
		return nil, err
	}
	w := &weaveInstance{environment: append([]string(nil), env...), child: c, baseURL: fmt.Sprintf("http://127.0.0.1:%d", o.port), loginKey: loginKey, dbURL: o.dbURL}
	timeout := o.readyTimeout
	if timeout == 0 {
		timeout = 3 * time.Minute
	}
	if err := waitHTTP(ctx, w.baseURL+"/v1/health", c, timeout); err != nil {
		c.Stop()
		return nil, err
	}
	return w, nil
}

func (w *weaveInstance) Stop() {
	if w != nil {
		w.child.Stop()
	}
}
