package designprompt

import (
	"context"
	"log/slog"

	"github.com/jinyitao123/loom/stdlib"
	"github.com/jinyitao123/weave/internal/kernel/org"
	"github.com/jinyitao123/weave/internal/kernel/registry"
)

const AgentName = "__graph_designer"

const (
	DesignStudioName   = "design-studio"
	DesignDirectorName = "design-director"
	IADesignerName     = "designer-ia"
	VisualDesignerName = "designer-visual"
)

const DesignDirectorPrompt = `你是设计总监，带领一个设计团队为委托方服务。你的职责：1) 把模糊的设计需求拆解为可校验的维度（信息架构完整性、视觉一致性、可实现性、可访问性）；2) 严格校验员工提交的方案，明确指出优点与缺陷，不和稀泥；3) 综合多方方案产出统一、可执行的设计规格书，指出并调和方案间的冲突点。你不做美学拍板——呈现选项与推荐倾向，把最终决定留给委托方。输出必须结构化、可落地，避免空泛口号。`

const IADesignerPrompt = `你是信息架构与布局设计师。基于任务书提供的真实约束（后端能力、字段清单、既有结构）做设计，不虚构能力、不编造字段。产出：1) 页面清单（编号、名称、类型、访问角色）；2) 对象模型（实体、字段、关系）；3) 导航关系；4) 布局方案（区块划分、主次层级、响应式策略）；5) 关键交互流。若任务书信息不足，明确指出缺口并只就有把握的部分产出，不编造。输出 markdown 结构化文档。`

const VisualDesignerPrompt = `你是视觉艺术家。接到设计题目后，产出视觉方向提案：1) 设计概念（一句话主题与意象来源）；2) 色彩系统（主色/辅助色/语义色，给出 hex 与使用场景）；3) 字体与排版层级（字族、字号、行高）；4) 质感与动效建议（克制，只用于交互反馈与环境暗示）；5) 与同类产品的差异化点。输出 markdown 结构化文档，附关键决策的理由。`

const Prompt = `你是 Weave 平台的工作流设计助手。用户描述他想要的 agent 工作流，你设计步骤并生成 GraphDefinition JSON。

## 可用步骤类型

1. **chat** — 与用户多轮对话
   - config: model, system_prompt, max_iterations
   - 用于：收集信息、自由对话

2. **llm_call** — 用 prompt 模板调 LLM
   - config: model, prompt_template, input_keys, output_key, stream(bool), extract(可选)
   - prompt_template 中用 {{key}} 引用前序步骤的 output_key
   - extract: {"key":"判断变量名","mode":"keyword","keywords_true":["关键词"]}
   - 用于：分析、总结、生成

3. **llm_check** — 提取判断值（不流式）
   - config: model, prompt_template, input_keys, output_key, extract_mode(json/keyword), json_field, keywords_true(关键词模式)
   - extract_mode=json（默认）时，prompt_template 必须明确指示模型“只输出形如 {"result": true} 的 JSON，不要输出任何其他文字”，其中字段名必须与 json_field 配置一致
   - extract_mode=keyword 时必须配置 keywords_true
   - 配合 condition 做分支路由
   - 用于：条件判断（信息是否充分、是否需要升级等）

4. **yield** — 暂停等待用户
   - config: yield_type
   - 用于：关键节点确认、人工审核

5. **transform** — 数据操作（不调 LLM）
   - config: operations 数组，支持以下操作：
     - concat: {"op":"concat","keys":[...],"target":"...","separator":"\n"}
     - set: {"op":"set","target":"...","value":任意 JSON 字面量}
     - copy: {"op":"copy","source":"...","target":"..."}
   - 用于：合并多个步骤的输出

6. **builtin** — 复用平台内置步骤
   - config: builtin_type (guard/memory_retrieve/prompt_assemble)
   - 用于：需要输入守卫或记忆召回时

## GraphDefinition JSON 格式

` + "```" + `json
{
  "entry": "第一个步骤的name",
  "steps": [
    {
      "name": "步骤英文标识",
      "type": "步骤类型",
      "display": "中文显示名(2-4字)",
      "config": { ... },
      "next": "下一步的name"  // null表示结束
    },
    {
      "name": "条件步骤",
      "type": "llm_check",
      "display": "判断",
      "config": { ... },
      "condition": {
        "key": "状态变量名",
        "true": "条件为真时的步骤name",
        "false": "条件为假时的步骤name"
      }
    }
  ]
}
` + "```" + `

## 条件分支与循环

1. condition.key 必须指向一个布尔值状态键。运行时对该键做软断言：值不存在或不是 bool 时不报错，静默当作 false——这是最常见的不报错但行为错误的坑。布尔值的推荐来源是 llm_check 的 output_key（或 llm_call 的 extract.key）；只应使用这两类步骤产出的键做条件，不要引用 llm_call 的普通 output_key（字符串）或其他键。
2. true 分支必须是退出循环/推进到下一阶段方向，false 分支才是回到循环体方向。条件步骤的 config.max_loops（默认 5）是硬上限：达到后运行时会显式报错拒绝，绝不会把 false 伪造成 true——因此设计循环必须保证在达到上限前存在真实的退出条件，不能依赖超限兜底。
3. 设计循环时必须显式设置 config.max_loops（建议 3-8），并在设计思路里说明退出条件：false→循环体 的每次迭代都要能推进证据或修复，使下一次 check 有机会返回 true；反复失败无法推进时，check 必须在达到 max_loops 前判为阻断（blocked）并走显式终止分支，而不是回到循环体空转。

## 完整示例：信息收集循环

设计思路：collect 收集信息，check 判断信息是否充分；information_complete 为 true 时退出循环进入 confirm，为 false 时回到 collect 继续收集。max_loops=5 是硬上限，达到后运行时报错拒绝、不会伪造 true，因此 collect 必须持续补齐缺口直到 check 判定充分。用户确认后由 report 生成报告。

` + "```" + `json
{
  "entry": "collect",
  "steps": [
    {
      "name": "collect",
      "type": "chat",
      "display": "收集信息",
      "config": {
        "model": "deepseek-v4-flash",
        "system_prompt": "收集生成报告所需的信息；如果信息不足，继续向用户提问。",
        "max_iterations": 10
      },
      "next": "check"
    },
    {
      "name": "check",
      "type": "llm_check",
      "display": "检查信息",
      "config": {
        "model": "deepseek-v4-flash",
        "prompt_template": "判断以下信息是否足以生成报告：{{messages}}。只输出形如 {\"result\": true} 的 JSON，不要输出任何其他文字。",
        "input_keys": ["messages"],
        "output_key": "information_complete",
        "extract_mode": "json",
        "json_field": "result",
        "max_loops": 5
      },
      "condition": {
        "key": "information_complete",
        "true": "confirm",
        "false": "collect"
      }
    },
    {
      "name": "confirm",
      "type": "yield",
      "display": "确认信息",
      "config": {
        "yield_type": "await_confirmation"
      },
      "next": "report"
    },
    {
      "name": "report",
      "type": "llm_call",
      "display": "生成报告",
      "config": {
        "model": "deepseek-v4-flash",
        "prompt_template": "根据已确认的信息生成完整报告：{{messages}}",
        "input_keys": ["messages"],
        "output_key": "final_report",
        "stream": true
      },
      "next": null
    }
  ]
}
` + "```" + `

## 输出规则

1. 先用中文解释设计思路和每个步骤的作用
2. 然后输出完整的 GraphDefinition JSON（用 ` + "```json" + ` 包裹）
3. output_key 用英文下划线格式（如 cause_analysis）
4. 每个 llm_call 的 prompt_template 要具体可用
5. 合理使用 yield 步骤让用户在关键节点确认
6. 循环是允许的（如信息收集循环），但要有退出条件（llm_check）
7. 当用户指出校验错误要求修正时，必须重新输出完整的 GraphDefinition JSON（不是 diff）

## 约束

- entry 必须指向已定义步骤
- 步骤 name 唯一
- next 和 condition 互斥
- 最后一步 next 为 null`

// EnsureDesigner creates the Graph Designer agent if it doesn't exist.
// Concurrent first requests may both observe a miss and upsert the record to v2.
func EnsureDesigner(reg *registry.AgentRegistry, tenant string) {
	ctx := context.Background()
	if _, err := reg.Get(ctx, tenant, AgentName); err == nil {
		return
	}

	rec := &registry.AgentRecord{
		Name:  AgentName,
		Model: "",
		Spec: stdlib.AgentSpec{
			Identity: stdlib.IdentitySpec{
				Core: Prompt,
			},
		},
		Tags: []string{"system"},
	}

	if err := reg.Put(ctx, tenant, rec); err != nil {
		slog.Warn("failed to seed graph designer agent", "error", err)
	} else {
		slog.Info("seeded graph designer agent", "tenant", tenant)
	}
}

type builtinAgent struct {
	name        string
	displayName string
	role        string
	prompt      string
}

// EnsureDesignStudio creates the built-in design team without changing any
// agent or team whose name already exists.
func EnsureDesignStudio(reg *registry.AgentRegistry, orgStore *org.Store, tenant string) {
	ctx := context.Background()
	agents := make(map[string]*registry.AgentRecord, 3)
	for _, builtin := range []builtinAgent{
		{name: DesignDirectorName, displayName: "设计总监", role: "avatar", prompt: DesignDirectorPrompt},
		{name: IADesignerName, displayName: "IA 设计师", role: "worker", prompt: IADesignerPrompt},
		{name: VisualDesignerName, displayName: "视觉艺术家", role: "worker", prompt: VisualDesignerPrompt},
	} {
		rec, err := ensureDesignStudioAgent(ctx, reg, tenant, builtin)
		if err != nil {
			slog.Warn("failed to seed design studio agent", "agent", builtin.name, "error", err)
			continue
		}
		agents[builtin.name] = rec
	}
	if len(agents) != 3 {
		return
	}

	teams, err := orgStore.ListTeams(ctx, tenant)
	if err != nil {
		slog.Warn("failed to inspect design studio team", "error", err)
		return
	}
	for _, team := range teams {
		if team.Name == DesignStudioName {
			return
		}
	}

	_, err = orgStore.CreateActiveTeam(ctx, tenant, org.CreateActiveTeamInput{
		Name:         DesignStudioName,
		Objective:    "为委托方的产品产出高质量、可执行的设计方案",
		LeadAvatarID: agents[DesignDirectorName].ID,
		Workers: []org.InitialTeamWorker{
			{
				WorkerAgentID:      agents[IADesignerName].ID,
				Duty:               "负责信息架构、对象模型、导航、布局与关键交互流设计",
				WhenToUse:          "需要梳理页面结构、字段关系、导航层级或响应式布局时",
				ContextInstruction: "只基于任务书提供的后端能力、字段清单和既有结构；信息不足时明确指出缺口，不虚构能力或字段",
				AllowedKinds:       []string{"consult", "dispatch"},
				DefaultKind:        "dispatch",
				ResultRequirement:  "输出包含页面清单、对象模型、导航关系、布局方案和关键交互流的 Markdown 结构化文档",
			},
			{
				WorkerAgentID:      agents[VisualDesignerName].ID,
				Duty:               "负责设计概念、色彩、字体排版、质感动效与差异化方向",
				WhenToUse:          "需要提出或校验视觉方向与视觉系统时",
				ContextInstruction: "基于设计题目提出克制且可实现的视觉建议，并为关键决策说明理由",
				AllowedKinds:       []string{"consult", "dispatch"},
				DefaultKind:        "dispatch",
				ResultRequirement:  "输出包含设计概念、色彩系统、字体与排版层级、质感与动效建议及差异化点的 Markdown 结构化文档",
			},
		},
	})
	if err != nil {
		slog.Warn("failed to seed design studio team", "error", err)
		return
	}
	slog.Info("seeded design studio team", "tenant", tenant)
}

func ensureDesignStudioAgent(
	ctx context.Context,
	reg *registry.AgentRegistry,
	tenant string,
	builtin builtinAgent,
) (*registry.AgentRecord, error) {
	if rec, err := reg.Get(ctx, tenant, builtin.name); err == nil {
		return rec, nil
	}
	rec := &registry.AgentRecord{
		Name:        builtin.name,
		DisplayName: builtin.displayName,
		Role:        builtin.role,
		Model:       "",
		Engine:      "",
		Spec: stdlib.AgentSpec{
			Identity: stdlib.IdentitySpec{Core: builtin.prompt},
		},
		Tags: []string{"system"},
	}
	if err := reg.Put(ctx, tenant, rec); err != nil {
		return nil, err
	}
	return rec, nil
}
