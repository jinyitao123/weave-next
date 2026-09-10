//go:build !windows

package engine

import (
	"errors"
	"fmt"
	"os/exec"
	"syscall"
)

func processTerminationReason(waitErr error) string {
	var exitErr *exec.ExitError
	if !errors.As(waitErr, &exitErr) || exitErr.ProcessState == nil {
		return ""
	}
	if state, ok := exitErr.Sys().(syscall.WaitStatus); ok && state.Signaled() {
		return fmt.Sprintf("runtime_process_interrupted: signal %s", state.Signal())
	}
	return ""
}

func applyProcAttr(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func terminateProcess(cmd *exec.Cmd) error {
	return syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
}

func killProcess(cmd *exec.Cmd) error {
	return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
}
