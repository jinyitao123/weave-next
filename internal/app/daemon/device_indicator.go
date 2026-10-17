package daemon

import (
	"context"
	"encoding/base64"
	"errors"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
	"unicode/utf16"
)

// While a session runs, the person at the machine can see it and stop it
// (weave-workbench decision 009). `weave device stop`, or the tray menu on
// Windows, writes a stop file the session checks before and after every
// operation.

func deviceStopFile() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".weave", "device-stop")
}

// StopDevice asks the control session on this machine to end.
func StopDevice() error {
	path := deviceStopFile()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(time.Now().Format(time.RFC3339)), 0o600)
}

func deviceStopRequested() bool {
	_, err := os.Stat(deviceStopFile())
	return err == nil
}

func clearDeviceStop() {
	if err := os.Remove(deviceStopFile()); err != nil && !errors.Is(err, os.ErrNotExist) {
		slog.Warn("device stop file could not be cleared", "error", err)
	}
}

// indicatorAvailable reports whether this platform can show the person a
// session; a device without one is reported unavailable.
func indicatorAvailable() bool {
	switch runtime.GOOS {
	case "darwin":
		_, err := exec.LookPath("osascript")
		return err == nil
	case "windows":
		_, err := exec.LookPath("powershell")
		return err == nil
	}
	return false
}

// showIndicator tells the person a session started and returns how to take
// the sign down. On Windows a tray icon stays for the whole session, with a
// menu item that stops it; on macOS a notification marks the start and the
// end, and cua-driver's own session cursor shows while it acts.
func showIndicator(ctx context.Context, reason string) func() {
	switch runtime.GOOS {
	case "darwin":
		executable, err := os.Executable()
		if err != nil {
			home, _ := os.UserHomeDir()
			executable = filepath.Join(home, ".weave", "bin", "weave-runtime")
		}
		notify(ctx, "Weave 正在控制这台 Mac", reason+"（在终端运行 "+deviceStopCommand(executable)+" 可请求停止）")
		return func() { notify(context.Background(), "Weave 已结束控制", reason) }
	case "windows":
		cmd := exec.Command("powershell", "-NoProfile", "-STA", "-WindowStyle", "Hidden", "-EncodedCommand", encodePowerShell(trayScript))
		cmd.Env = append(os.Environ(), "WEAVE_DEVICE_REASON="+reason, "WEAVE_DEVICE_STOP="+deviceStopFile())
		if err := cmd.Start(); err != nil {
			return func() {}
		}
		return func() {
			if cmd.Process != nil {
				_ = cmd.Process.Kill()
			}
			_ = cmd.Wait()
		}
	}
	return func() {}
}

// deviceStopCommand quotes the running node path for the Mac's shell, so it
// works on a fresh install without a global weave command or a changed PATH.
func deviceStopCommand(executable string) string {
	return "'" + strings.ReplaceAll(executable, "'", "'\\''") + "' device stop"
}

// notify passes the text as arguments, never inside the script, so a reason
// cannot change what runs.
func notify(ctx context.Context, title, text string) {
	notifyCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	_ = exec.CommandContext(notifyCtx, "osascript",
		"-e", "on run argv", "-e", "display notification (item 2 of argv) with title (item 1 of argv)", "-e", "end run",
		title, text).Run()
}

func encodePowerShell(script string) string {
	units := utf16.Encode([]rune(script))
	raw := make([]byte, 0, len(units)*2)
	for _, unit := range units {
		raw = append(raw, byte(unit), byte(unit>>8))
	}
	return base64.StdEncoding.EncodeToString(raw)
}

// trayScript reads the reason and the stop file from the environment.
const trayScript = `
Add-Type -AssemblyName System.Windows.Forms
Add-Type -AssemblyName System.Drawing
$text = "Weave 正在控制这台电脑：" + $env:WEAVE_DEVICE_REASON
if ($text.Length -gt 63) { $text = $text.Substring(0, 63) }
$icon = New-Object System.Windows.Forms.NotifyIcon
$icon.Icon = [System.Drawing.SystemIcons]::Information
$icon.Text = $text
$menu = New-Object System.Windows.Forms.ContextMenuStrip
$stop = $menu.Items.Add("停止 Weave 控制")
$stop.add_Click({
  Set-Content -Path $env:WEAVE_DEVICE_STOP -Value (Get-Date -Format o)
  $icon.Visible = $false
  [System.Windows.Forms.Application]::Exit()
})
$icon.ContextMenuStrip = $menu
$icon.Visible = $true
$icon.ShowBalloonTip(5000, "Weave", $text, [System.Windows.Forms.ToolTipIcon]::Info)
[System.Windows.Forms.Application]::Run()
$icon.Visible = $false
$icon.Dispose()
`
