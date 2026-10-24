package api

import (
	"encoding/json"
	"strings"
	"testing"
)

const editAssertedFlow = `[
	{"kind":"flow_add","name":"合同复核流程","description":"接合同材料，交付复核结论","member":"整理员"},
	{"kind":"step_add","flow":"合同复核流程","after":"整理员","member":"核对员","name":"核对条款","requirement":"逐条核对条款并给出依据"},
	{"kind":"member_add","ref":"checker","name":"格式检查员","duty":"按标准检查结果"}
]`

func editGuaranteeItems(t *testing.T, doc developmentDocument) []developmentGuaranteeView {
	t.Helper()
	summaries := developmentGuaranteeSummaries(&doc)
	if len(summaries) != 1 {
		t.Fatalf("summaries = %+v", summaries)
	}
	return summaries[0].Items
}

func TestGuaranteesAreProvenOnTheGraphAndBlockPublishingUntilTheyHold(t *testing.T) {
	doc := editTestDocument()
	editApply(t, &doc, editAssertedFlow)
	editApply(t, &doc, `[{"kind":"guarantees","flow":"合同复核流程","require":[
		{"type":"passes_through","step":"核对条款"},{"type":"checked","step":"核对条款"},{"type":"failure_handled","step":"整理员"}]}]`)
	items := editGuaranteeItems(t, doc)
	if len(items) != 3 || !items[0].Proven || items[1].Proven || items[2].Proven {
		t.Fatalf("before the structure exists: %+v", items)
	}
	unproven := developmentUnprovenGuarantees(&doc)
	if len(unproven) != 2 || unproven[0].Blocks != "publish" || !strings.Contains(unproven[0].Message, "check_loop") {
		t.Fatalf("unproven = %+v", unproven)
	}
	codes := 0
	for _, issue := range developmentIssues(doc, editTestCatalog, true) {
		if issue.Code == "guarantee_unproven" {
			codes++
		}
	}
	if codes != 2 {
		t.Fatalf("draft issues carry %d unproven guarantees", codes)
	}

	editApply(t, &doc, `[
		{"kind":"check_loop","flow":"合同复核流程","step":"核对条款","checker":"格式检查员","criteria":"每条结论引用条款原文","maxRounds":3},
		{"kind":"on_failure","flow":"合同复核流程","step":"整理员","member":"核对员","name":"整理失败说明","requirement":"说明原因"}
	]`)
	if refusal := editRefusal(t, doc, `[{"kind":"on_failure","flow":"合同复核流程","step":"核对条款","member":"整理员","name":"核对失败说明","requirement":"说明原因"}]`); !strings.Contains(refusal.Message, "检查回路") {
		t.Fatalf("a failure branch inside a check loop: %s", refusal.Message)
	}
	items = editGuaranteeItems(t, doc)
	for _, item := range items[:3] {
		if !item.Declared || !item.Proven {
			t.Fatalf("after the structure exists: %+v", items)
		}
	}
	if !strings.Contains(items[1].Text, "格式检查员") || !strings.Contains(items[1].Text, "最多 3 轮") || !strings.Contains(items[2].Text, "整理失败说明") {
		t.Fatalf("proof texts = %+v", items)
	}
	// The structure's own guarantees follow the declared ones.
	derived := []string{}
	for _, item := range items[3:] {
		derived = append(derived, item.Text)
	}
	if len(derived) != 2 || items[3].Declared {
		t.Fatalf("derived = %v", derived)
	}
	if len(developmentUnprovenGuarantees(&doc)) != 0 {
		t.Fatal("proven guarantees still block publishing")
	}

	// Assertions follow the step, not its name.
	editApply(t, &doc, `[{"kind":"step","flow":"合同复核流程","step":"核对条款","name":"条款核对"}]`)
	if items = editGuaranteeItems(t, doc); !items[0].Proven || !strings.Contains(items[0].Text, "条款核对") {
		t.Fatalf("after renaming: %+v", items[0])
	}
}

func TestPassesThroughNamesTheRouteThatAvoidsTheStep(t *testing.T) {
	doc := editTestDocument()
	editApply(t, &doc, editAssertedFlow)
	editApply(t, &doc, `[
		{"kind":"step_add","flow":"合同复核流程","after":"整理员","member":"整理员","name":"复查","requirement":"复查条款","placement":"parallel"},
		{"kind":"guarantees","flow":"合同复核流程","require":[{"type":"passes_through","step":"整理员"}]}
	]`)
	items := editGuaranteeItems(t, doc)
	if items[0].Proven || !strings.Contains(items[0].Text, "复查") || !strings.Contains(items[0].Text, "→") {
		t.Fatalf("a parallel branch avoids the step: %+v", items[0])
	}
	// A removed step leaves an assertion that cannot be proven.
	editApply(t, &doc, `[{"kind":"guarantees","flow":"合同复核流程","require":[{"type":"passes_through","step":"核对条款"}]}]`)
	editApply(t, &doc, `[{"kind":"step_remove","flow":"合同复核流程","step":"核对条款"}]`)
	if items = editGuaranteeItems(t, doc); items[0].Proven || !strings.Contains(items[0].Text, "已不在流程中") {
		t.Fatalf("after removing the step: %+v", items[0])
	}
}

func TestGuaranteesEditRefusesWhatItCannotProve(t *testing.T) {
	doc := editTestDocument()
	editApply(t, &doc, editAssertedFlow)
	for name, raw := range map[string]string{
		"unknown type": `[{"kind":"guarantees","flow":"合同复核流程","require":[{"type":"approved","step":"核对条款"}]}]`,
		"delivery":     `[{"kind":"guarantees","flow":"合同复核流程","require":[{"type":"passes_through","step":"交付结果"}]}]`,
		"missing step": `[{"kind":"guarantees","flow":"合同复核流程","require":[{"type":"checked","step":"不存在"}]}]`,
		"not a list":   `[{"kind":"guarantees","flow":"合同复核流程","require":"核对条款"}]`,
	} {
		if refusal := editRefusal(t, doc, raw); refusal.Message == "" {
			t.Fatalf("%s: empty refusal", name)
		}
	}
	// An empty list clears the assertions.
	editApply(t, &doc, `[{"kind":"guarantees","flow":"合同复核流程","require":[{"type":"passes_through","step":"核对条款"}]}]`)
	editApply(t, &doc, `[{"kind":"guarantees","flow":"合同复核流程","require":[]}]`)
	if len(doc.Workflows[0].Guarantees) != 0 {
		t.Fatalf("guarantees = %+v", doc.Workflows[0].Guarantees)
	}
}

func TestDraftViewRepeatsTheFlowGuarantees(t *testing.T) {
	doc := editTestDocument()
	editApply(t, &doc, editAssertedFlow)
	editApply(t, &doc, `[{"kind":"guarantees","flow":"合同复核流程","require":[{"type":"passes_through","step":"核对条款"}]}]`)
	raw, _ := json.Marshal(mcpDraftView(developmentDraft{Document: doc}, nil))
	if !strings.Contains(string(raw), `"guarantees":[{"declared":true,"proven":true,"text":"每条通往交付的路径都经过“核对条款”"}]`) {
		t.Fatalf("draft view = %s", raw)
	}
}
