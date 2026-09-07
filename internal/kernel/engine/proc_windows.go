//go:build windows

package engine

import (
	"os/exec"
	"strconv"
)

func applyProcAttr(cmd *exec.Cmd) {}

// Windows has no Unix wait-status signal proof. Do not infer interruption
// merely from stderr text or an ordinary nonzero exit code.
func processTerminationReason(waitErr error) string { return "" }

func terminateProcess(cmd *exec.Cmd) error {
	return exec.Command("taskkill", "/pid", strconv.Itoa(cmd.Process.Pid), "/T", "/F").Run()
}

func killProcess(cmd *exec.Cmd) error {
	return exec.Command("taskkill", "/pid", strconv.Itoa(cmd.Process.Pid), "/T", "/F").Run()
}
