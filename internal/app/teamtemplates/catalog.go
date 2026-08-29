package teamtemplates

import (
	"encoding/json"
	"errors"
	"sort"

	"github.com/jinyitao123/weave/internal/build/teamforge"
	"github.com/jinyitao123/weave/internal/build/teamtemplate"
)

type StaticCatalog struct {
	items map[string]Sample
}

func NewStaticCatalog() *StaticCatalog {
	return &StaticCatalog{items: map[string]Sample{
		"market-research": {
			Name: "market-research", DisplayName: "市场调研团队",
			Description: "并行取证与分析，汇总为可追溯的市场报告。", YAML: marketResearchSampleYAML,
			DeclarativeSpec: synthesisSampleDeclarativeSpec(
				"source-researcher", "提供一手来源、日期与原始链接",
				"market-analyst", "交叉验证数据并解释竞争格局",
				"research-editor", "汇总结论并标明证据边界",
			),
		},
		"content-production": {
			Name: "content-production", DisplayName: "内容生产团队",
			Description: "撰稿与审校循环，收敛到符合受众要求的成稿。", YAML: contentProductionSampleYAML,
			DeclarativeSpec: reworkSampleDeclarativeSpec(
				"content-writer", "提交结构完整、表达清晰的正文",
				"content-editor", "给出具体可执行的审校意见", 2,
			),
		},
		"code-review": {
			Name: "code-review", DisplayName: "代码评审团队",
			Description: "并行审查实现与测试，汇总为可执行的修改建议。", YAML: codeReviewSampleYAML,
			DeclarativeSpec: synthesisSampleDeclarativeSpec(
				"implementation-reviewer", "审查正确性、可维护性与边界处理",
				"test-reviewer", "审查测试覆盖、失败路径与回归风险",
				"review-synthesizer", "汇总问题、风险等级与建议修改顺序",
			),
		},
		"human-final-review": {
			Name: "human-final-review", DisplayName: "人工终审交付团队",
			Description: "接收完整交付稿，由工作区成员终审并提交决定与批注。", YAML: humanFinalReviewSampleYAML,
			DeclarativeSpec: humanFinalReviewDeclarativeSpec(),
		},
	}}
}

func (c *StaticCatalog) Resolve(name string, overrides map[string]any) ([]byte, error) {
	if c == nil {
		return nil, errors.New("sample catalog is unavailable")
	}
	sample, ok := c.items[name]
	if !ok {
		return nil, errors.New("sample does not exist")
	}
	return teamtemplate.ApplyOverrides([]byte(sample.YAML), overrides)
}

func (c *StaticCatalog) ResolveDeclarative(name string) (*teamforge.DeclarativeWorkflowSpecV1, error) {
	if c == nil {
		return nil, errors.New("sample catalog is unavailable")
	}
	sample, ok := c.items[name]
	if !ok {
		return nil, errors.New("sample does not exist")
	}
	if sample.DeclarativeSpec == nil {
		return nil, nil
	}
	raw, err := json.Marshal(sample.DeclarativeSpec)
	if err != nil {
		return nil, errors.New("sample declarative workflow cannot be copied")
	}
	var cloned teamforge.DeclarativeWorkflowSpecV1
	if err := json.Unmarshal(raw, &cloned); err != nil {
		return nil, errors.New("sample declarative workflow cannot be copied")
	}
	return &cloned, nil
}

func (c *StaticCatalog) List() []Sample {
	if c == nil {
		return []Sample{}
	}
	names := make([]string, 0, len(c.items))
	for name := range c.items {
		names = append(names, name)
	}
	sort.Strings(names)
	items := make([]Sample, 0, len(names))
	for _, name := range names {
		items = append(items, c.items[name])
	}
	return items
}
