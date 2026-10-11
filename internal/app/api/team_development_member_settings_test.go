package api

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestControlledEditsSetHowAMemberRuns(t *testing.T) {
	models := []string{"deepseek-flash", "deepseek-reasoner"}
	nodes := []editRuntime{
		{ID: "node-1", Name: "研发一号机", Engines: []string{"claude", "codex"}},
		{ID: "node-2", Name: "研发二号机", Engines: []string{"claude"}},
	}
	apply := func(doc *developmentDocument, raw string) error {
		_, err := applyDevelopmentOperationsWith(doc, editOps(t, raw), editTestCatalog, models, nodes...)
		return err
	}
	refused := func(doc developmentDocument, raw string) string {
		t.Helper()
		// A refused group may leave its copy half edited, as the handler's discarded draft is.
		var copied developmentDocument
		if err := json.Unmarshal([]byte(encodeDevelopment(doc)), &copied); err != nil {
			t.Fatal(err)
		}
		var refusal *editError
		if err := apply(&copied, raw); !errors.As(err, &refusal) {
			t.Fatalf("expected a refusal for %s, got %v", raw, err)
		}
		return refusal.Message
	}

	// A member moves to an external engine on a named node and back to the built-in one.
	doc := editTestDocument()
	if err := apply(&doc, `[{"kind":"member_engine","member":"整理员","engine":"claude","node":"研发二号机"}]`); err != nil {
		t.Fatal(err)
	}
	worker := &doc.Members[1].Configuration
	if worker.Engine != "claude" || worker.RuntimeID != "node-2" || worker.Model != "" {
		t.Fatalf("external member = %+v", worker)
	}
	if err := apply(&doc, `[{"kind":"member_engine","member":"整理员","node":"研发一号机","model":"sonnet"}]`); err != nil {
		t.Fatal(err)
	}
	if worker.RuntimeID != "node-1" || worker.Model != "sonnet" {
		t.Fatalf("moved member = %+v", worker)
	}
	for raw, want := range map[string]string{
		`[{"kind":"member_limits","member":"整理员","maxCostUsd":1}]`:                             "外部引擎",
		`[{"kind":"member_memory","member":"整理员","enabled":true}]`:                             "外部引擎",
		`[{"kind":"member","member":"整理员","model":"deepseek-flash"}]`:                          "member_engine",
		`[{"kind":"member_engine","member":"整理员","engine":"codex","node":"研发二号机"}]`:            "不提供 codex",
		`[{"kind":"member_engine","member":"整理员","engine":"opencode","node":"研发一号机"}]`:         "还没有提供 opencode",
		`[{"kind":"member_engine","member":"整理员","engine":"codex"}]`:                           "须用 node 指定",
		`[{"kind":"member_engine","member":"整理员","engine":"loom"}]`:                            "须同时用 model",
		`[{"kind":"member_engine","member":"整理员","engine":"loom","model":"gpt-unknown"}]`:      "不在当前组织可用的模型中",
		`[{"kind":"member_engine","member":"整理员","engine":"loom","model":"x","node":"研发一号机"}]`: "不选择节点",
		`[{"kind":"member_engine","member":"整理员","engine":"fortran"}]`:                         "引擎须为",
		`[{"kind":"member_engine","member":"整理员"}]`:                                            "缺少目标字段",
		`[{"kind":"member_engine","member":"负责人","engine":"claude","node":"研发一号机"}]`:           "负责人的引擎不能更换",
	} {
		if message := refused(doc, raw); !strings.Contains(message, want) {
			t.Errorf("%s refused with %q, want %q", raw, message, want)
		}
	}
	if err := apply(&doc, `[{"kind":"member_engine","member":"整理员","engine":"loom","model":"deepseek-reasoner"}]`); err != nil {
		t.Fatal(err)
	}
	if worker.Engine != "loom" || worker.RuntimeID != "" || worker.Model != "deepseek-reasoner" {
		t.Fatalf("built-in member = %+v", worker)
	}

	// Limits, the tool loop, memory, tool permissions and handoff kinds.
	worker.MCPServerIDs, worker.PermissionAllow, worker.PermissionDeny = []string{"srv"}, []string{"read"}, []string{"shell"}
	if err := apply(&doc, `[
		{"kind":"member_limits","member":"整理员","maxTokens":200000,"maxCostUsd":1.5,"stepBudget":40,"toolLoop":{"sliceRounds":20,"totalRounds":60}},
		{"kind":"member_memory","member":"整理员","enabled":true,"scope":"user"},
		{"kind":"member_tools","member":"整理员","denyAll":true,"clearExternal":true},
		{"kind":"member_handoff","member":"整理员","allowed":["consult","dispatch"],"default":"dispatch"}
	]`); err != nil {
		t.Fatal(err)
	}
	if worker.MaxTokens != 200000 || worker.MaxCostUSD != 1.5 || worker.StepBudget != 40 || worker.ToolLoopControl == nil || worker.ToolLoopControl.SliceRounds != 20 || worker.ToolLoopControl.InitialTotalRounds != 60 {
		t.Fatalf("limits = %+v %+v", worker, worker.ToolLoopControl)
	}
	if !worker.MemoryEnabled || worker.MemoryScope != "user" || len(worker.MCPServerIDs) != 0 || len(worker.PermissionAllow) != 0 || strings.Join(worker.PermissionDeny, ",") != "shell,*" {
		t.Fatalf("memory and tools = %+v", worker)
	}
	if relation := doc.Members[1].Relationship; strings.Join(relation.AllowedKinds, ",") != "consult,dispatch" || relation.DefaultKind != "dispatch" {
		t.Fatalf("handoff = %+v", relation)
	}
	if err := apply(&doc, `[{"kind":"member_limits","member":"整理员","maxCostUsd":0,"toolLoop":null},{"kind":"member_tools","member":"整理员","denyAll":false}]`); err != nil {
		t.Fatal(err)
	}
	if worker.MaxCostUSD != 0 || worker.ToolLoopControl != nil || strings.Join(worker.PermissionDeny, ",") != "shell" || worker.MaxTokens != 200000 {
		t.Fatalf("cleared limits = %+v", worker)
	}
	for raw, want := range map[string]string{
		`[{"kind":"member_limits","member":"整理员"}]`:                                                 "缺少目标字段",
		`[{"kind":"member_limits","member":"整理员","stepBudget":1.5}]`:                                "整数",
		`[{"kind":"member_limits","member":"整理员","maxTokens":-1}]`:                                  "0 表示不限制",
		`[{"kind":"member_limits","member":"整理员","maxTokens":"many"}]`:                              "须是数字",
		`[{"kind":"member_limits","member":"整理员","toolLoop":{"sliceRounds":2000,"totalRounds":5}}]`: "sliceRounds",
		`[{"kind":"member_memory","member":"整理员"}]`:                                                 "enabled",
		`[{"kind":"member_memory","member":"整理员","enabled":true,"scope":"planet"}]`:                 "记忆范围须为",
		`[{"kind":"member_tools","member":"整理员"}]`:                                                  "缺少目标字段",
		`[{"kind":"member_tools","member":"整理员","clearExternal":false}]`:                            "只能为 true",
		`[{"kind":"member_handoff","member":"负责人","allowed":["consult"]}]`:                          "负责人不设置",
		`[{"kind":"member_handoff","member":"整理员","allowed":[]}]`:                                   "至少一项",
		`[{"kind":"member_handoff","member":"整理员","allowed":["consult"],"default":"dispatch"}]`:     "允许的方式之一",
		`[{"kind":"member_handoff","member":"整理员"}]`:                                                "缺少目标字段",
	} {
		if message := refused(doc, raw); !strings.Contains(message, want) {
			t.Errorf("%s refused with %q, want %q", raw, message, want)
		}
	}

	// A kind a step already relies on cannot be taken away.
	if err := apply(&doc, `[{"kind":"flow_add","name":"提交流程","description":"提交材料包","member":"整理员"}]`); err != nil {
		t.Fatal(err)
	}
	inUse := ""
	for _, node := range editItems(editGraphOf(t, doc.Workflows[0]), "nodes") {
		if editNodeType(node) == "worker" {
			inUse = editString(editObject(node["config"])["kind"])
		}
	}
	other := map[string]string{"consult": "dispatch", "dispatch": "consult", "handoff": "consult"}[inUse]
	if other == "" {
		t.Fatalf("the first step has no handoff kind: %q", inUse)
	}
	if message := refused(doc, `[{"kind":"member_handoff","member":"整理员","allowed":["`+other+`"]}]`); !strings.Contains(message, "已有步骤在用") {
		t.Fatalf("in-use kind refused with %q", message)
	}
}
