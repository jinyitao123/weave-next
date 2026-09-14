package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

// ProcessIsolation confines an invocation to its subject's files. The Host
// supplies these paths; they are never taken from a model or execution input.
type ProcessIsolation struct {
	SubjectRoot    string
	ProtectedRoots []string
}

var ErrIsolationUnavailable = errors.New("runtime subject isolation is unavailable")

func canonicalIsolationPath(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil || strings.TrimSpace(path) == "" {
		return "", ErrIsolationUnavailable
	}
	return filepath.EvalSymlinks(abs)
}

func isolatedCommand(ctx context.Context, binary string, args []string, isolation *ProcessIsolation) (*exec.Cmd, error) {
	if isolation == nil {
		return exec.CommandContext(ctx, binary, args...), nil
	}
	root, err := canonicalIsolationPath(isolation.SubjectRoot)
	if err != nil {
		return nil, fmt.Errorf("%w: subject directory", ErrIsolationUnavailable)
	}
	cli, err := exec.LookPath(binary)
	if err != nil {
		return nil, err
	}
	cli, err = canonicalIsolationPath(cli)
	if err != nil {
		return nil, err
	}
	switch runtime.GOOS {
	case "darwin":
		if _, err := os.Stat("/usr/bin/sandbox-exec"); err != nil {
			return nil, ErrIsolationUnavailable
		}
		var policy strings.Builder
		policy.WriteString("(version 1)(allow default)(deny file-read* file-write*)(allow file-read-metadata)(allow file-read* (literal \"/\"))")
		// Permit only OS/runtime code and public network configuration. Data
		// outside the subject root is denied even when another Host stores it
		// outside the current operator home.
		for _, path := range []string{"/System", "/usr", "/bin", "/sbin", "/opt/homebrew/Cellar", "/opt/homebrew/opt", "/opt/homebrew/lib", "/opt/homebrew/bin", "/opt/homebrew/share", "/dev", "/private/var/db/dyld"} {
			fmt.Fprintf(&policy, "(allow file-read* (subpath %s))", strconv.Quote(path))
		}
		for _, path := range []string{"/private/var/select/sh", "/private/etc/hosts", "/private/etc/resolv.conf", "/private/etc/localtime", "/private/etc/ssl"} {
			fmt.Fprintf(&policy, "(allow file-read* (subpath %s))", strconv.Quote(path))
		}
		for _, protected := range isolation.ProtectedRoots {
			path, err := canonicalIsolationPath(protected)
			if err != nil {
				return nil, fmt.Errorf("%w: protected directory", ErrIsolationUnavailable)
			}
			fmt.Fprintf(&policy, "(deny file-read* (subpath %s))", strconv.Quote(path))
		}
		fmt.Fprintf(&policy, "(allow file-read* file-write* (subpath %s))", strconv.Quote(root))
		// getcwd/chdir need metadata for ancestor directories. Their contents
		// remain denied, including other subjects and the Host's journals.
		for parent := filepath.Dir(root); ; parent = filepath.Dir(parent) {
			fmt.Fprintf(&policy, "(allow file-read-metadata (literal %s))", strconv.Quote(parent))
			if parent == filepath.Dir(parent) {
				break
			}
		}

		// CLI launchers may live under the operator home. Permit their code,
		// never the operator's CLI configuration or credential directories.
		fmt.Fprintf(&policy, "(allow file-read* (literal %s))", strconv.Quote(cli))
		policy.WriteString("(allow file-write* (literal \"/dev/null\") (literal \"/dev/tty\"))(deny process-info*)")
		return exec.CommandContext(ctx, "/usr/bin/sandbox-exec", append([]string{"-p", policy.String(), cli}, args...)...), nil
	case "linux":
		bwrap, err := exec.LookPath("bwrap")
		if err != nil {
			return nil, ErrIsolationUnavailable
		}
		wrapped := []string{"--die-with-parent", "--new-session", "--unshare-user", "--unshare-pid", "--proc", "/proc", "--dev", "/dev", "--tmpfs", "/tmp"}
		for _, path := range []string{"/usr", "/bin", "/sbin", "/lib", "/lib64", "/etc/ssl", "/etc/hosts", "/etc/resolv.conf", "/etc/ld.so.cache", "/etc/nsswitch.conf", "/etc/passwd", "/etc/group"} {
			if _, err := os.Stat(path); err == nil {
				wrapped = append(wrapped, "--ro-bind", path, path)
			}
		}
		wrapped = append(wrapped, "--bind", root, root, "--chdir", root, "--", cli)
		return exec.CommandContext(ctx, bwrap, append(wrapped, args...)...), nil
	default:
		return nil, ErrIsolationUnavailable
	}
}

// ProbeIsolation tests both allowed own files and denial of a neighbouring
// subject's file. Merely finding a sandbox binary does not advertise strength.
func ProbeIsolation(ctx context.Context, root string) error {
	probeRoot, err := os.MkdirTemp(root, ".isolation-probe-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(probeRoot)
	owned := filepath.Join(probeRoot, "own")
	if err := os.Mkdir(owned, 0700); err != nil {
		return err
	}
	foreign := filepath.Join(probeRoot, "foreign")
	if err := os.WriteFile(foreign, []byte("private"), 0600); err != nil {
		return err
	}
	policy := &ProcessIsolation{SubjectRoot: owned, ProtectedRoots: []string{probeRoot}}
	cmd, err := isolatedCommand(ctx, "/bin/sh", []string{"-c", `printf allowed > "$1/own" && test "$(cat "$1/own")" = allowed && ! cat "$2" && ! printf changed > "$2"`, "probe", owned, foreign}, policy)
	if err != nil {
		return err
	}
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%w: probe failed: %v: %s", ErrIsolationUnavailable, err, output)
	}
	return nil
}
