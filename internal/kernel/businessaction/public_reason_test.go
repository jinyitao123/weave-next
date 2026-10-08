package businessaction

import (
	"strings"
	"testing"

	"github.com/jinyitao123/loom/contract"
)

func TestPublicActionFailureReasonOnlyExposesBoundedNativeBusinessText(t *testing.T) {
	const reason = "销售业务设置缺少项目客户分类；请先由管理员维护分类后再转化线索"
	for _, test := range []struct{ name, content, want string }{
		{"empty native", "action 'Convert' threw: Error: ", ""},
		{"invalid text", "action 'Convert' threw: Error: " + string([]byte{255}), ""},
		{"null", "null", ""},
		{"malformed JSON", `{"ok":false,"error":`, ""},
		{"native action", "action 'Convert' threw: Error: " + reason, reason},
		{"other business", "action 'Convert' threw: Error: 当前记录已提交，请刷新后核对状态", "当前记录已提交，请刷新后核对状态"},
		{"native stack removed", "action 'Convert' threw: Error: " + reason + "\n    at internal (/srv/app.js:4:2)", reason},
		{"structured error", `{"ok":false,"error":{"message":"金额必须大于零"}}`, "金额必须大于零"},
		{"structured message", `{"ok":false,"message":"请先指定负责人"}`, "请先指定负责人"},
		{"other action", "action 'Other' threw: Error: " + reason, ""},
		{"unclassified text", reason, ""},
		{"technical exception", "action 'Convert' threw: TypeError: cannot read property", ""},
		{"unknown diagnostic", "action 'Convert' threw: Error: database failed", ""},
		{"extra prose", "action 'Convert' threw: Error: " + reason + "\n附带私有原始请求", ""},
		{"credentials", `{"ok":false,"error":"授权失败 Bearer private-value"}`, ""},
		{"URL", `{"ok":false,"error":"请访问 https://private.example/path"}`, ""},
		{"host", `{"ok":false,"error":"连接 private.example 失败"}`, ""},
		{"address", `{"ok":false,"error":"连接 192.0.2.1 失败"}`, ""},
		{"IPv4 endpoint", `{"ok":false,"error":"连接 192.0.2.1:5432 失败"}`, ""},
		{"IPv6 endpoint", `{"ok":false,"error":"连接 [2001:db8::1]:443 失败"}`, ""},
		{"bare IPv6", `{"ok":false,"error":"连接 2001:db8:0:0:0:0:0:1 失败"}`, ""},
		{"host endpoint", `{"ok":false,"error":"连接 database:5432 失败"}`, ""},
		{"short ID", `{"ok":false,"error":"记录 ID：private 缺失"}`, ""},
		{"identifier", `{"ok":false,"error":"记录 11111111-2222-3333-4444-555555555555 不存在"}`, ""},
		{"internal field", `{"ok":false,"error":"字段 forge_customer 缺少内容"}`, ""},
		{"oversize", "action 'Convert' threw: Error: " + strings.Repeat("原因", 121), ""},
		{"markup", `{"ok":false,"error":"请点击 <script>private</script>"}`, ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			result := &contract.ToolResult{Content: test.content, IsError: true}
			if got := PublicActionFailureReason(result, "Convert"); got != test.want {
				t.Fatalf("reason=%q want=%q", got, test.want)
			}
		})
	}
	if got := PublicActionFailureReason(&contract.ToolResult{Content: `{"ok":true,"error":"不能冒充失败"}`}, "Convert"); got != "" {
		t.Fatalf("non-failure acquired a reason: %q", got)
	}
}

func TestNativeFailureReasonDoesNotTurnTextIntoReplayReceipt(t *testing.T) {
	var events []ActionOutcomeEvent
	reason := "请先维护客户分类后再办理"
	host := &outcomeTestHost{result: &contract.ToolResult{Content: "action 'ContractSubmit' threw: Error: " + reason, IsError: true}}
	dispatcher, call := newTrackedOutcomeDispatcher(t, host)
	ctx := outcomeTestContext(&events, operationLedgerGuard(&events), nil)
	first, err := dispatcher.Dispatch(ctx, call)
	if err != nil || first == nil || !first.IsError || first.Content != host.result.Content || len(events) != 2 || events[1].Status != ActionOutcomeStatusFailed || events[1].PublicReason != reason || events[1].Result != nil {
		t.Fatalf("failure reason or raw-text cache boundary changed: result=%+v events=%+v err=%v", first, events, err)
	}
	call.ID = "changed-model-call"
	second, err := dispatcher.Dispatch(ctx, call)
	if err != nil || !second.IsError || !second.StopLoop || host.calls != 1 || len(events) != 2 {
		t.Fatalf("display reason enabled replay: result=%+v calls=%d err=%v", second, host.calls, err)
	}
}
