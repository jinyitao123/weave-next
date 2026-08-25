//go:build windows

package engine

import (
	"os/exec"
	"strconv"
)

func applyProcAttr(cmd *exec.Cmd) {}

func terminateProcess(cmd *exec.Cmd) error {
	return exec.Command("taskkill", "/pid", strconv.Itoa(cmd.Process.Pid), "/T", "/F").Run()
}

func killProcess(cmd *exec.Cmd) error {
	return exec.Command("taskkill", "/pid", strconv.Itoa(cmd.Process.Pid), "/T", "/F").Run()
}
