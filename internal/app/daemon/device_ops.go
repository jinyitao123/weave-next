package daemon

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jinyitao123/weave/internal/kernel/runtimeprotocol"
)

const (
	deviceOutputLimit    = 256 << 10
	deviceFileLimit      = 8 << 20
	deviceExecDefault    = 60
	deviceExecMaximum    = 600
	deviceDefaultMaxEdge = 1568
)

// perform runs one operation. Errors become a failed result with a message
// the caller can act on; nothing here guesses or retries an act.
func (h *deviceHost) perform(ctx context.Context, driver *cuaClient, op *runtimeprotocol.DeviceOperation) runtimeprotocol.DeviceOperationResult {
	var p map[string]any
	if err := json.Unmarshal(op.Params, &p); err != nil {
		return deviceFailed("参数无效")
	}
	switch op.Kind {
	case "windows":
		return h.windows(ctx, driver)
	case "ui_tree":
		return h.uiTree(ctx, driver, p)
	case "screenshot":
		return h.screenshot(ctx, driver, p)
	case "act":
		return h.act(ctx, driver, p)
	case "exec":
		return h.exec(ctx, p)
	case "file_read":
		return h.fileRead(p)
	case "file_write":
		return h.fileWrite(p)
	}
	return deviceFailed("不支持的操作")
}

func number(p map[string]any, key string) (float64, bool) {
	value, ok := p[key].(float64)
	return value, ok
}

func cuaFailure(result *cuaResult, err error) (runtimeprotocol.DeviceOperationResult, bool) {
	if err != nil {
		return deviceFailed(err.Error()), true
	}
	if result.IsError {
		return deviceFailed(result.text()), true
	}
	return runtimeprotocol.DeviceOperationResult{}, false
}

func (h *deviceHost) windows(ctx context.Context, driver *cuaClient) runtimeprotocol.DeviceOperationResult {
	result, err := driver.call(ctx, "list_windows", map[string]any{"on_screen_only": true})
	if failed, bad := cuaFailure(result, err); bad {
		return failed
	}
	return deviceSucceeded(result.fields(), nil)
}

// windowPID finds the process that owns a window, which cua-driver needs
// alongside the window id.
func windowPID(ctx context.Context, driver *cuaClient, window float64) (float64, error) {
	result, err := driver.call(ctx, "list_windows", map[string]any{})
	if err != nil {
		return 0, err
	}
	windows, _ := result.fields()["windows"].([]any)
	for _, raw := range windows {
		entry, _ := raw.(map[string]any)
		if id, _ := entry["window_id"].(float64); id == window {
			if pid, ok := entry["pid"].(float64); ok {
				return pid, nil
			}
		}
	}
	return 0, fmt.Errorf("没有窗口 %v；请先用 device_windows 查看", window)
}

func (h *deviceHost) uiTree(ctx context.Context, driver *cuaClient, p map[string]any) runtimeprotocol.DeviceOperationResult {
	window, ok := number(p, "window")
	if !ok {
		return deviceFailed("需要 window")
	}
	pid, err := windowPID(ctx, driver, window)
	if err != nil {
		return deviceFailed(err.Error())
	}
	args := map[string]any{"pid": pid, "window_id": window, "include_accessibility_tree": true, "include_screenshot": false}
	if limit, ok := number(p, "max_nodes"); ok && limit > 0 {
		args["max_elements"] = limit
	}
	result, err := driver.call(ctx, "get_window_state", args)
	if failed, bad := cuaFailure(result, err); bad {
		return failed
	}
	fields := result.fields()
	if snapshot, ok := fields["snapshot_id"]; ok {
		fields["capture_id"] = snapshot
	}
	// Elements are referenced as element_ref in Weave's tools.
	if elements, ok := fields["elements"].([]any); ok {
		for _, raw := range elements {
			if element, ok := raw.(map[string]any); ok {
				if token, ok := element["element_token"]; ok {
					element["element_ref"] = token
					delete(element, "element_token")
				}
			}
		}
	}
	return deviceSucceeded(fields, nil)
}

// screenshot returns the image inline and removes any file the driver wrote,
// so no screenshot stays on the machine.
func (h *deviceHost) screenshot(ctx context.Context, driver *cuaClient, p map[string]any) runtimeprotocol.DeviceOperationResult {
	maxEdge := float64(deviceDefaultMaxEdge)
	if edge, ok := number(p, "max_edge"); ok && edge > 0 {
		maxEdge = edge
	}
	var result *cuaResult
	var err error
	if window, ok := number(p, "window"); ok {
		pid, pidErr := windowPID(ctx, driver, window)
		if pidErr != nil {
			return deviceFailed(pidErr.Error())
		}
		result, err = driver.call(ctx, "get_window_state", map[string]any{"pid": pid, "window_id": window,
			"include_screenshot": true, "include_accessibility_tree": false, "max_image_dimension": maxEdge})
	} else {
		tool := "get_desktop_state"
		if !driver.has(tool) {
			tool = "screenshot"
		}
		args := map[string]any{driver.param(tool, "max_image_dimension", "max_dimension"): maxEdge}
		if display, ok := number(p, "display"); ok {
			args[driver.param(tool, "display_id", "display")] = display
		}
		result, err = driver.call(ctx, tool, args)
	}
	if failed, bad := cuaFailure(result, err); bad {
		return failed
	}
	fields := result.fields()
	image := map[string]any{}
	if inline := result.image(); inline != nil {
		image["data"], image["mime_type"] = inline.Data, inline.MimeType
	} else if path, _ := fields["screenshot_file_path"].(string); path != "" {
		data, readErr := os.ReadFile(path)
		_ = os.Remove(path)
		if readErr != nil {
			return deviceFailed("截图文件无法读取")
		}
		image["data"], image["mime_type"] = base64.StdEncoding.EncodeToString(data), "image/png"
	} else {
		return deviceFailed("驱动没有返回截图")
	}
	answer := map[string]any{"image": image}
	for _, key := range []string{"screenshot_width", "screenshot_height", "window_bounds", "window_title", "app_name", "scale_factor"} {
		if value, ok := fields[key]; ok {
			answer[strings.TrimPrefix(key, "screenshot_")] = value
		}
	}
	if snapshot, ok := fields["snapshot_id"]; ok {
		answer["capture_id"] = snapshot
	}
	return deviceSucceeded(answer, nil)
}

func (h *deviceHost) act(ctx context.Context, driver *cuaClient, p map[string]any) runtimeprotocol.DeviceOperationResult {
	action, _ := p["action"].(string)
	var target map[string]any
	if window, ok := number(p, "window"); ok {
		pid, err := windowPID(ctx, driver, window)
		if err != nil {
			return deviceFailed(err.Error())
		}
		target = map[string]any{"kind": "window", "pid": pid, "window_id": window}
	}
	withTarget := func(args map[string]any) map[string]any {
		if target != nil {
			args["target"] = target
		}
		return args
	}
	var tool string
	var args map[string]any
	switch action {
	case "click", "double_click":
		if target == nil {
			return deviceFailed("点击需要 window")
		}
		count := 1
		if action == "double_click" {
			count = 2
		}
		args = withTarget(map[string]any{"delivery_mode": "foreground", "count": count})
		if ref, _ := p["element_ref"].(string); ref != "" {
			args["element_token"] = ref
		} else {
			x, okX := number(p, "x")
			y, okY := number(p, "y")
			if !okX || !okY {
				return deviceFailed("点击需要 element_ref，或坐标 x、y")
			}
			args["x"], args["y"] = x, y
		}
		if capture, _ := p["based_on"].(string); capture != "" {
			args["capture_id"] = capture
		}
		tool = "click"
	case "type":
		text, _ := p["text"].(string)
		tool, args = "type_text", withTarget(map[string]any{"text": text})
	case "key":
		key, _ := p["key"].(string)
		args = withTarget(map[string]any{"key": key})
		if modifiers, ok := p["modifiers"]; ok {
			args["modifiers"] = modifiers
		}
		tool = "press_key"
	case "hotkey":
		tool, args = "hotkey", withTarget(map[string]any{"keys": p["keys"]})
	case "scroll":
		args = withTarget(map[string]any{"x": p["x"], "y": p["y"], "direction": p["direction"]})
		if amount, ok := p["amount"]; ok {
			args["amount"] = amount
		}
		tool = "scroll"
	case "drag":
		tool, args = "drag", withTarget(map[string]any{"from_x": p["x"], "from_y": p["y"], "to_x": p["to_x"], "to_y": p["to_y"]})
	case "launch_app":
		app, _ := p["app"].(string)
		tool = "launch_app"
		args = map[string]any{driver.param(tool, "app", "name", "bundle_id"): app}
	case "quit_app":
		app, _ := p["app"].(string)
		for _, candidate := range []string{"quit_app", "terminate_app"} {
			if driver.has(candidate) {
				tool = candidate
			}
		}
		if tool == "" {
			return deviceFailed("驱动不支持退出应用；可改用 device_exec")
		}
		args = map[string]any{driver.param(tool, "app", "name", "bundle_id"): app}
	default:
		return deviceFailed("不支持的动作")
	}
	result, err := driver.call(ctx, tool, args)
	if failed, bad := cuaFailure(result, err); bad {
		return failed
	}
	return deviceSucceeded(result.fields(), nil)
}

// within resolves path, following links, and requires it under one of the
// owner's roots, so a link cannot lead outside them.
func (h *deviceHost) within(path string, mustExist bool) (string, error) {
	if !filepath.IsAbs(path) {
		return "", errors.New("需要绝对路径")
	}
	resolved := filepath.Clean(path)
	if real, err := filepath.EvalSymlinks(resolved); err == nil {
		resolved = real
	} else if mustExist {
		return "", errors.New("文件不存在")
	} else if parent, perr := filepath.EvalSymlinks(filepath.Dir(resolved)); perr == nil {
		resolved = filepath.Join(parent, filepath.Base(resolved))
	} else {
		return "", errors.New("目录不存在")
	}
	for _, root := range h.config.FileRoots {
		realRoot, err := filepath.EvalSymlinks(root)
		if err != nil {
			continue
		}
		if rel, err := filepath.Rel(realRoot, resolved); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return resolved, nil
		}
	}
	return "", errors.New("路径不在本机允许的目录内")
}

// boundedBuffer keeps at most deviceOutputLimit bytes. It holds the buffer as
// a field, not embedded, so io.Copy cannot bypass Write through ReadFrom.
type boundedBuffer struct {
	data      bytes.Buffer
	truncated bool
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	if room := deviceOutputLimit - b.data.Len(); room < len(p) {
		if room > 0 {
			b.data.Write(p[:room])
		}
		b.truncated = true
		return len(p), nil
	}
	return b.data.Write(p)
}

func (b *boundedBuffer) Len() int       { return b.data.Len() }
func (b *boundedBuffer) String() string { return b.data.String() }

func (h *deviceHost) exec(ctx context.Context, p map[string]any) runtimeprotocol.DeviceOperationResult {
	raw, _ := p["argv"].([]any)
	argv := make([]string, 0, len(raw))
	for _, value := range raw {
		text, ok := value.(string)
		if !ok {
			return deviceFailed("argv 须为字符串数组")
		}
		argv = append(argv, text)
	}
	if len(argv) == 0 || argv[0] == "" {
		return deviceFailed("需要 argv")
	}
	timeout := deviceExecDefault
	if seconds, ok := number(p, "timeout_seconds"); ok && seconds > 0 {
		timeout = min(int(seconds), deviceExecMaximum)
	}
	cwd, _ := p["cwd"].(string)
	if cwd == "" && len(h.config.FileRoots) > 0 {
		cwd = h.config.FileRoots[0]
	}
	cwd, err := h.within(cwd, true)
	if err != nil {
		return deviceFailed("cwd：" + err.Error())
	}
	runCtx, cancel := context.WithTimeout(ctx, time.Duration(timeout)*time.Second)
	defer cancel()
	cmd := exec.CommandContext(runCtx, argv[0], argv[1:]...)
	cmd.Dir = cwd
	var stdout, stderr boundedBuffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	runErr := cmd.Run()
	exitCode := 0
	var exitErr *exec.ExitError
	switch {
	case runCtx.Err() == context.DeadlineExceeded:
		exitCode = -1
	case errors.As(runErr, &exitErr):
		exitCode = exitErr.ExitCode()
	case runErr != nil:
		return deviceFailed("无法运行：" + runErr.Error())
	}
	answer := map[string]any{"exit_code": exitCode, "stdout": stdout.String(), "stderr": stderr.String(),
		"stdout_truncated": stdout.truncated, "stderr_truncated": stderr.truncated, "timed_out": runCtx.Err() == context.DeadlineExceeded}
	return deviceSucceeded(answer, map[string]any{"exit_code": exitCode, "stdout_bytes": stdout.Len(), "stderr_bytes": stderr.Len()})
}

func digest(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func (h *deviceHost) fileRead(p map[string]any) runtimeprotocol.DeviceOperationResult {
	path, _ := p["path"].(string)
	resolved, err := h.within(path, true)
	if err != nil {
		return deviceFailed(err.Error())
	}
	info, err := os.Stat(resolved)
	if err != nil || info.IsDir() {
		return deviceFailed("不是可读取的文件")
	}
	if info.Size() > deviceFileLimit {
		return deviceFailed("文件超过 8 MB")
	}
	data, err := os.ReadFile(resolved)
	if err != nil {
		return deviceFailed("文件无法读取")
	}
	answer := map[string]any{"sha256": digest(data), "bytes": len(data)}
	if utf8.Valid(data) {
		answer["content"] = string(data)
	} else {
		answer["content_base64"] = base64.StdEncoding.EncodeToString(data)
	}
	return deviceSucceeded(answer, map[string]any{"sha256": answer["sha256"], "bytes": len(data)})
}

func (h *deviceHost) fileWrite(p map[string]any) runtimeprotocol.DeviceOperationResult {
	path, _ := p["path"].(string)
	content, _ := p["content"].(string)
	expected, _ := p["expected_sha256"].(string)
	if len(content) > deviceFileLimit {
		return deviceFailed("内容超过 8 MB")
	}
	resolved, err := h.within(path, false)
	if err != nil {
		return deviceFailed(err.Error())
	}
	before := ""
	if current, err := os.ReadFile(resolved); err == nil {
		before = digest(current)
	} else if !errors.Is(err, os.ErrNotExist) {
		return deviceFailed("文件无法读取")
	}
	if before != expected {
		return deviceFailed("write_conflict：文件已被改动，当前摘要为 " + before)
	}
	temp, err := os.CreateTemp(filepath.Dir(resolved), ".weave-write-*")
	if err != nil {
		return deviceFailed("无法写入该目录")
	}
	_, writeErr := temp.WriteString(content)
	closeErr := temp.Close()
	if writeErr != nil || closeErr != nil || os.Rename(temp.Name(), resolved) != nil {
		_ = os.Remove(temp.Name())
		return deviceFailed("写入失败")
	}
	after := digest([]byte(content))
	return deviceSucceeded(map[string]any{"sha256": after, "bytes": len(content)},
		map[string]any{"sha256_before": before, "sha256_after": after, "bytes": len(content)})
}
