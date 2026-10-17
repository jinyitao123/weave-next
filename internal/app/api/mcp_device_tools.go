package api

import (
	"encoding/json"
	"net/http"
	"net/url"

	"github.com/google/uuid"
)

// Device tools of the MCP entry (contract 设备控制契约). Each calls the device
// routes as the caller, so only the person's own account session reaches
// their own devices.

// renderWeaveContent gives an answer carrying an "image" object
// ({data, mime_type}) as an image item followed by the rest as text.
func renderWeaveContent(content string, isError bool) []map[string]string {
	text := []map[string]string{{"type": "text", "text": content}}
	if isError {
		return text
	}
	var answer map[string]json.RawMessage
	if json.Unmarshal([]byte(content), &answer) != nil {
		return text
	}
	var image struct {
		Data     string `json:"data"`
		MimeType string `json:"mime_type"`
	}
	if raw, ok := answer["image"]; !ok || json.Unmarshal(raw, &image) != nil || image.Data == "" {
		return text
	}
	delete(answer, "image")
	rest, _ := json.Marshal(answer)
	if image.MimeType == "" {
		image.MimeType = "image/png"
	}
	return []map[string]string{{"type": "image", "data": image.Data, "mimeType": image.MimeType}, {"type": "text", "text": string(rest)}}
}

const (
	mcpSessionProp = `"session_id":{"type":"string","description":"会话标识，来自 device_session_open"}`
	mcpOpProp      = `"op_id":{"type":"string","description":"本次操作的 UUID；结果不确定时用同一个值重试，不会执行第二次"}`
)

// deviceOperation sends one operation and unwraps the device's answer.
func deviceOperation(call *mcpEntryCall, args map[string]any, kind, opID string, params map[string]any) (any, *mcpEntryError) {
	if failure := mcpRequire(args, "session_id"); failure != nil {
		return nil, failure
	}
	var answer struct {
		Status string          `json:"status"`
		Result json.RawMessage `json:"result"`
		Error  string          `json:"error"`
		OpID   string          `json:"op_id"`
	}
	path := "/v1/device-sessions/" + url.PathEscape(mcpText(args, "session_id")) + "/ops"
	if failure := call.route(http.MethodPost, path, map[string]any{"op_id": opID, "kind": kind, "params": params}, &answer); failure != nil {
		return nil, failure
	}
	switch answer.Status {
	case "succeeded":
		var result any
		_ = json.Unmarshal(answer.Result, &result)
		return result, nil
	case "failed":
		return nil, mcpFail("device_failed", "%s", answer.Error)
	}
	return map[string]any{"status": "pending", "op_id": answer.OpID, "message": "设备还没有回执；用同一个 op_id 再次调用即可继续等待"}, nil
}

// pick copies the named arguments that were given.
func pick(args map[string]any, names ...string) map[string]any {
	params := map[string]any{}
	for _, name := range names {
		if value, ok := args[name]; ok {
			params[name] = value
		}
	}
	return params
}

func callerOpID(args map[string]any) (string, *mcpEntryError) {
	opID := mcpText(args, "op_id")
	if uuid.Validate(opID) != nil {
		return "", mcpFail("invalid_arguments", "op_id 须为 UUID")
	}
	return opID, nil
}

var mcpDeviceTools = []mcpEntryTool{
	{Name: "device_list", Description: "列出使用者本人绑定的 Mac 与 Windows 设备：名称、平台、是否在线、本机允许的级别（observe 查看、act 操作、exec 执行）、不可用原因和正在进行的会话。",
		Schema: mcpSchema(``),
		Run: func(call *mcpEntryCall, args map[string]any) (any, *mcpEntryError) {
			var devices map[string]any
			if failure := call.route(http.MethodGet, "/v1/devices", nil, &devices); failure != nil {
				return nil, failure
			}
			return devices, nil
		}},
	{Name: "device_session_open", Description: "在一台设备上打开控制会话。reason 写清要做什么，会显示在被控机器上并记入审计。一台设备同一时间只有一个会话，已有会话时返回它；state 为 opening 时稍后用 device_session_read 查看，变为 ready 后才能操作。会话空闲 5 分钟或满 60 分钟自动结束。",
		Schema: mcpSchema(`"device_id":{"type":"string"},"reason":{"type":"string"},`+mcpRequestProp, "device_id", "reason", "request_id"),
		Run: func(call *mcpEntryCall, args map[string]any) (any, *mcpEntryError) {
			if failure := mcpRequire(args, "device_id", "reason", "request_id"); failure != nil {
				return nil, failure
			}
			var session map[string]any
			path := "/v1/devices/" + url.PathEscape(mcpText(args, "device_id")) + "/sessions"
			if failure := call.route(http.MethodPost, path, pick(args, "reason", "request_id"), &session); failure != nil {
				return nil, failure
			}
			return session, nil
		}},
	{Name: "device_session_read", Description: "读取会话状态：opening、ready、closing 或 ended，以及剩余时长和结束原因。",
		Schema: mcpSchema(mcpSessionProp, "session_id"),
		Run: func(call *mcpEntryCall, args map[string]any) (any, *mcpEntryError) {
			var session map[string]any
			if failure := call.route(http.MethodGet, "/v1/device-sessions/"+url.PathEscape(mcpText(args, "session_id")), nil, &session); failure != nil {
				return nil, failure
			}
			return session, nil
		}},
	{Name: "device_session_close", Description: "结束会话。被控机器上的标识随之消失；已经发往设备的操作不会被撤回。",
		Schema: mcpSchema(mcpSessionProp, "session_id"),
		Run: func(call *mcpEntryCall, args map[string]any) (any, *mcpEntryError) {
			var session map[string]any
			if failure := call.route(http.MethodDelete, "/v1/device-sessions/"+url.PathEscape(mcpText(args, "session_id")), nil, &session); failure != nil {
				return nil, failure
			}
			return session, nil
		}},
	{Name: "device_screenshot", Description: "截取设备屏幕或一个窗口，返回图片，以及之后操作要引用的 capture_id、尺寸和缩放比例。给出 window（窗口标识，来自 device_windows）时只截该窗口。",
		Schema: mcpSchema(mcpSessionProp+`,"display":{"type":"integer"},"window":{"type":"integer"},"max_edge":{"type":"integer"}`, "session_id"),
		Run: func(call *mcpEntryCall, args map[string]any) (any, *mcpEntryError) {
			return deviceOperation(call, args, "screenshot", uuid.NewString(), pick(args, "display", "window", "max_edge"))
		}},
	{Name: "device_ui_tree", Description: "读取一个窗口的界面结构：每个元素的角色、名称、取值、位置和 element_ref。比截图省上下文，能用时优先用它。",
		Schema: mcpSchema(mcpSessionProp+`,"window":{"type":"integer"},"max_nodes":{"type":"integer"}`, "session_id", "window"),
		Run: func(call *mcpEntryCall, args map[string]any) (any, *mcpEntryError) {
			return deviceOperation(call, args, "ui_tree", uuid.NewString(), pick(args, "window", "max_nodes"))
		}},
	{Name: "device_windows", Description: "列出设备上可见的窗口：应用名称、窗口标题、窗口标识、位置，以及前台窗口。",
		Schema: mcpSchema(mcpSessionProp, "session_id"),
		Run: func(call *mcpEntryCall, args map[string]any) (any, *mcpEntryError) {
			return deviceOperation(call, args, "windows", uuid.NewString(), map[string]any{})
		}},
	{Name: "device_act", Description: "在设备上操作一次。action 为 click、double_click、type、key、hotkey、scroll、drag、launch_app 或 quit_app。点击类给 window 与 element_ref（来自 device_ui_tree）或坐标 x、y（来自同一 window 的 device_screenshot，并带 based_on 为那次的 capture_id）；type 给 text；key 给 key 与可选 modifiers；hotkey 给 keys；scroll 给 x、y、direction；launch_app 与 quit_app 给 app。不得用来在 Forge 或小满界面中办理业务写入。",
		Schema: mcpSchema(mcpSessionProp+`,`+mcpOpProp+`,"action":{"type":"string","enum":["click","double_click","type","key","hotkey","scroll","drag","launch_app","quit_app"]},"window":{"type":"integer"},"element_ref":{"type":"string"},"x":{"type":"number"},"y":{"type":"number"},"to_x":{"type":"number"},"to_y":{"type":"number"},"based_on":{"type":"string"},"text":{"type":"string"},"key":{"type":"string"},"modifiers":{"type":"array","items":{"type":"string"}},"keys":{"type":"array","items":{"type":"string"}},"direction":{"type":"string","enum":["up","down","left","right"]},"amount":{"type":"integer"},"app":{"type":"string"}`, "session_id", "op_id", "action"),
		Run: func(call *mcpEntryCall, args map[string]any) (any, *mcpEntryError) {
			opID, failure := callerOpID(args)
			if failure != nil {
				return nil, failure
			}
			return deviceOperation(call, args, "act", opID, pick(args, "action", "window", "element_ref", "x", "y", "to_x", "to_y", "based_on", "text", "key", "modifiers", "keys", "direction", "amount", "app"))
		}},
	{Name: "device_exec", Description: "在设备上运行一个程序。argv 是程序与参数的数组，不经 shell 解释；需要 shell 时显式写 [\"/bin/zsh\",\"-lc\",\"…\"] 或 [\"powershell\",\"-Command\",\"…\"]。返回退出码与输出（有上限并标明是否截断）。timeout_seconds 最长 600。",
		Schema: mcpSchema(mcpSessionProp+`,`+mcpOpProp+`,"argv":{"type":"array","items":{"type":"string"},"minItems":1},"cwd":{"type":"string"},"timeout_seconds":{"type":"integer"}`, "session_id", "op_id", "argv"),
		Run: func(call *mcpEntryCall, args map[string]any) (any, *mcpEntryError) {
			opID, failure := callerOpID(args)
			if failure != nil {
				return nil, failure
			}
			return deviceOperation(call, args, "exec", opID, pick(args, "argv", "cwd", "timeout_seconds"))
		}},
	{Name: "device_file_read", Description: "读取设备上允许目录内的一个文本文件（8 MB 以内），返回内容与 SHA-256。",
		Schema: mcpSchema(mcpSessionProp+`,"path":{"type":"string"}`, "session_id", "path"),
		Run: func(call *mcpEntryCall, args map[string]any) (any, *mcpEntryError) {
			return deviceOperation(call, args, "file_read", uuid.NewString(), pick(args, "path"))
		}},
	{Name: "device_file_write", Description: "写入设备上允许目录内的一个文件。expected_sha256 为当前内容的摘要（新建时为空字符串），不符时不覆盖并返回冲突。",
		Schema: mcpSchema(mcpSessionProp+`,`+mcpOpProp+`,"path":{"type":"string"},"content":{"type":"string"},"expected_sha256":{"type":"string"}`, "session_id", "op_id", "path", "content", "expected_sha256"),
		Run: func(call *mcpEntryCall, args map[string]any) (any, *mcpEntryError) {
			opID, failure := callerOpID(args)
			if failure != nil {
				return nil, failure
			}
			return deviceOperation(call, args, "file_write", opID, pick(args, "path", "content", "expected_sha256"))
		}},
}

func init() {
	mcpEntryTools = append(mcpEntryTools, mcpDeviceTools...)
}
