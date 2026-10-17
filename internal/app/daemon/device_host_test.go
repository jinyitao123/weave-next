package daemon

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jinyitao123/weave/internal/kernel/runtimeprotocol"
)

// When the test binary is started as `<binary> mcp` with WEAVE_FAKE_CUA set,
// it plays cua-driver's stdio MCP server instead of running tests.
func init() {
	if os.Getenv("WEAVE_FAKE_CUA") == "" || len(os.Args) < 2 || os.Args[len(os.Args)-1] != "mcp" {
		return
	}
	runFakeCuaDriver()
	os.Exit(0)
}

func runFakeCuaDriver() {
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 0, 1<<20), 16<<20)
	out := json.NewEncoder(os.Stdout)
	for scanner.Scan() {
		var request struct {
			ID     *int64          `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if json.Unmarshal(scanner.Bytes(), &request) != nil || request.ID == nil {
			continue
		}
		// A log line before the answer must not be taken for it.
		_ = out.Encode(map[string]any{"jsonrpc": "2.0", "method": "notifications/message", "params": map[string]any{"data": "working"}})
		var result any
		switch request.Method {
		case "initialize":
			result = map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{}}
		case "tools/list":
			tools := []map[string]any{}
			for _, name := range []string{"list_windows", "get_window_state", "click", "type_text", "launch_app"} {
				tools = append(tools, map[string]any{"name": name, "inputSchema": map[string]any{"type": "object", "properties": map[string]any{"app": map[string]any{}}}})
			}
			result = map[string]any{"tools": tools}
		case "tools/call":
			var call struct {
				Name      string         `json:"name"`
				Arguments map[string]any `json:"arguments"`
			}
			_ = json.Unmarshal(request.Params, &call)
			structured := map[string]any{}
			switch call.Name {
			case "list_windows":
				structured["windows"] = []any{map[string]any{"window_id": 7, "pid": 42, "app_name": "TextEdit", "title": "Notes"}}
			case "get_window_state":
				structured["snapshot_id"] = "snap-1"
				structured["elements"] = []any{map[string]any{"element_index": 0, "role": "button", "element_token": "tok-1"}}
				if call.Arguments["include_screenshot"] == true {
					path := filepath.Join(os.Getenv("WEAVE_FAKE_CUA"), "shot.png")
					_ = os.WriteFile(path, []byte("png-bytes"), 0o600)
					structured["screenshot_file_path"] = path
					structured["screenshot_width"] = 800
				}
			default:
				structured["received"] = call.Arguments
				structured["tool"] = call.Name
			}
			result = map[string]any{"content": []any{}, "structuredContent": structured}
		}
		_ = out.Encode(map[string]any{"jsonrpc": "2.0", "id": *request.ID, "result": result})
	}
}

func fakeDriver(t *testing.T) (*cuaClient, string) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("WEAVE_FAKE_CUA", dir)
	client, err := startCuaClient(context.Background(), os.Args[0])
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(client.close)
	return client, dir
}

func perform(t *testing.T, host *deviceHost, driver *cuaClient, kind string, params map[string]any) map[string]any {
	t.Helper()
	raw, _ := json.Marshal(params)
	result := host.perform(context.Background(), driver, &runtimeprotocol.DeviceOperation{OpID: "op", Kind: kind, Params: raw})
	if result.Status != "succeeded" {
		t.Fatalf("%s: %s %s", kind, result.Status, result.Error)
	}
	var answer map[string]any
	if err := json.Unmarshal(result.Result, &answer); err != nil {
		t.Fatal(err)
	}
	return answer
}

func TestDeviceDrivesTheScreenThroughCuaDriver(t *testing.T) {
	driver, dir := fakeDriver(t)
	host := &deviceHost{config: deviceConfig{Levels: deviceLevels, FileRoots: []string{dir}}}

	shot := perform(t, host, driver, "screenshot", map[string]any{"window": 7})
	image := shot["image"].(map[string]any)
	if data, _ := base64.StdEncoding.DecodeString(image["data"].(string)); string(data) != "png-bytes" || shot["capture_id"] != "snap-1" || shot["width"] != float64(800) {
		t.Fatalf("screenshot = %v", shot)
	}
	if _, err := os.Stat(filepath.Join(dir, "shot.png")); !os.IsNotExist(err) {
		t.Fatal("the screenshot file was left on the machine")
	}

	tree := perform(t, host, driver, "ui_tree", map[string]any{"window": 7})
	element := tree["elements"].([]any)[0].(map[string]any)
	if element["element_ref"] != "tok-1" || element["element_token"] != nil {
		t.Fatalf("ui tree element = %v", element)
	}

	click := perform(t, host, driver, "act", map[string]any{"action": "double_click", "window": 7, "element_ref": "tok-1", "based_on": "snap-1"})
	received := click["received"].(map[string]any)
	target := received["target"].(map[string]any)
	if click["tool"] != "click" || received["element_token"] != "tok-1" || received["count"] != float64(2) || received["capture_id"] != "snap-1" || target["pid"] != float64(42) {
		t.Fatalf("click = %v", click)
	}

	launch := perform(t, host, driver, "act", map[string]any{"action": "launch_app", "app": "TextEdit"})
	if launch["received"].(map[string]any)["app"] != "TextEdit" {
		t.Fatalf("launch = %v", launch)
	}
	raw, _ := json.Marshal(map[string]any{"action": "quit_app", "app": "TextEdit"})
	if quit := host.perform(context.Background(), driver, &runtimeprotocol.DeviceOperation{Kind: "act", Params: raw}); quit.Status != "failed" {
		t.Fatalf("quit_app without a driver tool: %+v", quit)
	}
	raw, _ = json.Marshal(map[string]any{"window": 99})
	if missing := host.perform(context.Background(), driver, &runtimeprotocol.DeviceOperation{Kind: "ui_tree", Params: raw}); missing.Status != "failed" {
		t.Fatalf("unknown window: %+v", missing)
	}
}

func TestDeviceFilesStayInsideTheOwnersRoots(t *testing.T) {
	root, outside := t.TempDir(), t.TempDir()
	host := &deviceHost{config: deviceConfig{Levels: deviceLevels, FileRoots: []string{root}}}
	inside := filepath.Join(root, "notes.txt")

	written := perform(t, host, nil, "file_write", map[string]any{"path": inside, "content": "第一版", "expected_sha256": ""})
	read := perform(t, host, nil, "file_read", map[string]any{"path": inside})
	if read["content"] != "第一版" || read["sha256"] != written["sha256"] {
		t.Fatalf("read back %v after %v", read, written)
	}
	raw, _ := json.Marshal(map[string]any{"path": inside, "content": "覆盖", "expected_sha256": ""})
	if conflict := host.perform(context.Background(), nil, &runtimeprotocol.DeviceOperation{Kind: "file_write", Params: raw}); conflict.Status != "failed" || !strings.Contains(conflict.Error, "write_conflict") {
		t.Fatalf("stale write: %+v", conflict)
	}
	perform(t, host, nil, "file_write", map[string]any{"path": inside, "content": "第二版", "expected_sha256": written["sha256"]})

	secret := filepath.Join(outside, "secret.txt")
	if err := os.WriteFile(secret, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{secret, filepath.Join(root, "..", filepath.Base(outside), "secret.txt"), "relative.txt"} {
		raw, _ := json.Marshal(map[string]any{"path": path})
		if result := host.perform(context.Background(), nil, &runtimeprotocol.DeviceOperation{Kind: "file_read", Params: raw}); result.Status != "failed" {
			t.Fatalf("read %s outside the roots: %+v", path, result)
		}
	}
	link := filepath.Join(root, "escape")
	if err := os.Symlink(outside, link); err == nil {
		raw, _ := json.Marshal(map[string]any{"path": filepath.Join(link, "secret.txt")})
		if result := host.perform(context.Background(), nil, &runtimeprotocol.DeviceOperation{Kind: "file_read", Params: raw}); result.Status != "failed" {
			t.Fatalf("a link led outside the roots: %+v", result)
		}
	}
}

func TestDeviceExecReportsExitAndBoundsOutput(t *testing.T) {
	root := t.TempDir()
	host := &deviceHost{config: deviceConfig{Levels: deviceLevels, FileRoots: []string{root}}}
	t.Setenv("WEAVE_DEVICE_HELPER", "1")
	answer := perform(t, host, nil, "exec", map[string]any{"argv": []any{os.Args[0], "-test.run=TestDeviceExecHelper"}, "timeout_seconds": 60})
	if answer["exit_code"] != float64(3) || answer["stdout_truncated"] != true || len(answer["stdout"].(string)) != deviceOutputLimit {
		t.Fatalf("exec = exit %v, truncated %v, %d bytes", answer["exit_code"], answer["stdout_truncated"], len(answer["stdout"].(string)))
	}
	raw, _ := json.Marshal(map[string]any{"argv": []any{"definitely-not-a-program-weave"}})
	if missing := host.perform(context.Background(), nil, &runtimeprotocol.DeviceOperation{Kind: "exec", Params: raw}); missing.Status != "failed" {
		t.Fatalf("missing program: %+v", missing)
	}
	raw, _ = json.Marshal(map[string]any{"argv": []any{os.Args[0]}, "cwd": t.TempDir()})
	if outside := host.perform(context.Background(), nil, &runtimeprotocol.DeviceOperation{Kind: "exec", Params: raw}); outside.Status != "failed" {
		t.Fatalf("cwd outside the roots: %+v", outside)
	}
}

func TestDeviceExecHelper(t *testing.T) {
	if os.Getenv("WEAVE_DEVICE_HELPER") == "" {
		t.Skip("helper process only")
	}
	os.Stdout.WriteString(strings.Repeat("x", deviceOutputLimit+100))
	os.Exit(3)
}

func TestDeviceLocalConfigNarrowsLevels(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	if err := os.MkdirAll(filepath.Join(home, ".weave"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".weave", "device.json"), []byte(`{"levels":["observe","root"]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	host := newDeviceHost("")
	if !host.allows("screenshot") || host.allows("act") || host.allows("exec") {
		t.Fatalf("levels = %v", host.config.Levels)
	}
	if facts := host.facts(context.Background()); facts.Available || facts.UnavailableReason == "" {
		t.Fatalf("a device without a driver reported available: %+v", facts)
	}
}

func TestDeviceStopFileEndsAndClears(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	if deviceStopRequested() {
		t.Fatal("a stop was requested before anyone asked")
	}
	if err := StopDevice(); err != nil {
		t.Fatal(err)
	}
	if !deviceStopRequested() {
		t.Fatal("weave device stop left no request")
	}
	clearDeviceStop()
	if deviceStopRequested() {
		t.Fatal("the stop request outlived its session")
	}
}

func TestTrayScriptIsEncodedForPowerShell(t *testing.T) {
	encoded, err := base64.StdEncoding.DecodeString(encodePowerShell(trayScript))
	if err != nil || len(encoded)%2 != 0 {
		t.Fatal(err)
	}
	units := make([]rune, 0, len(encoded)/2)
	for i := 0; i < len(encoded); i += 2 {
		units = append(units, rune(uint16(encoded[i])|uint16(encoded[i+1])<<8))
	}
	if string(units) != trayScript || !strings.Contains(trayScript, "$env:WEAVE_DEVICE_STOP") {
		t.Fatal("the tray script does not survive encoding")
	}
}
