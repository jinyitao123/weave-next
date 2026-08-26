// Code generated from /templates/*.yaml; DO NOT EDIT.

package teamtemplates

const marketResearchSampleYAML = `schema: team-template/v1
name: market-research
display_name: 市场调研团队
purpose: 持续产出有来源、可复核的市场与竞品调研报告
template: research_synthesis
template_parameters:
  lead_instruction: 拆解调研问题，组织并行取证，完成最终交付
  parallel_worker_refs: [source-researcher, market-analyst]
  finalizer_ref: research-editor
  result_requirements:
    research-editor: 汇总结论并标明证据边界
    source-researcher: 提供一手来源、日期与原始链接
    market-analyst: 交叉验证数据并解释竞争格局
members:
  - name: research-lead
    display_name: 调研负责人
    role: avatar
    responsibilities: [拆解问题, 组织协作, 汇总交付]
    capabilities: [task_delegation, report_writing]
  - name: source-researcher
    display_name: 资料调研员
    role: worker
    responsibilities: [检索一手资料, 记录来源]
    capabilities: [web_search, source_verification]
  - name: market-analyst
    display_name: 市场分析员
    role: worker
    responsibilities: [数据分析, 竞品对比, 交叉验证]
    capabilities: [analysis, fact_checking]
  - name: research-editor
    display_name: 调研编辑
    role: worker
    responsibilities: [汇总证据, 标注边界, 编辑交付]
    capabilities: [report_writing, evidence_synthesis]
lead: research-lead
delivery:
  success_criteria: [关键事实均可追溯, 覆盖指定市场与竞品, 明确区分事实与判断]
budget:
  max_cost_usd: 2
`

const contentProductionSampleYAML = `schema: team-template/v1
name: content-production
display_name: 内容生产团队
purpose: 稳定产出符合选题、受众与品牌要求的内容稿件
template: delivery_rework_loop
template_parameters:
  lead_instruction: 明确写作要求，组织撰稿与审校，收敛到可交付版本
  primary_ref: content-writer
  reviewer_ref: content-editor
  max_iterations: 2
  result_requirements:
    content-lead: 确认成稿满足选题和交付要求
    content-writer: 提交结构完整、表达清晰的正文
    content-editor: 给出具体可执行的审校意见
members:
  - name: content-lead
    display_name: 内容负责人
    role: avatar
    responsibilities: [澄清选题, 把控交付, 组织协作]
    capabilities: [task_delegation, editorial_planning]
  - name: content-writer
    display_name: 内容撰稿人
    role: worker
    responsibilities: [资料消化, 结构设计, 正文写作]
    capabilities: [writing, research]
  - name: content-editor
    display_name: 内容编辑
    role: worker
    responsibilities: [事实审校, 结构审校, 风格校准]
    capabilities: [editing, fact_checking]
lead: content-lead
delivery:
  success_criteria: [内容回应选题目标, 事实与引语可核验, 结构和语气适合目标受众]
budget:
  max_cost_usd: 3
`

const codeReviewSampleYAML = `schema: team-template/v1
name: code-review
display_name: 代码评审团队
purpose: 对代码变更进行并行审查，识别实现、测试与维护风险
template: parallel_review
template_parameters:
  lead_instruction: 明确变更目标与审查范围，组织并行评审，汇总结论与修改建议
  parallel_worker_refs: [implementation-reviewer, test-reviewer]
  finalizer_ref: review-synthesizer
  result_requirements:
    review-synthesizer: 汇总问题、风险等级与建议修改顺序
    implementation-reviewer: 审查正确性、可维护性与边界处理
    test-reviewer: 审查测试覆盖、失败路径与回归风险
members:
  - name: review-lead
    display_name: 评审负责人
    role: avatar
    responsibilities: [确定范围, 分派评审, 汇总结论]
    capabilities: [task_delegation, code_review]
  - name: implementation-reviewer
    display_name: 实现评审员
    role: worker
    responsibilities: [审查实现逻辑, 识别维护风险]
    capabilities: [code_analysis, architecture_review]
  - name: test-reviewer
    display_name: 测试评审员
    role: worker
    responsibilities: [审查测试覆盖, 识别回归风险]
    capabilities: [test_analysis, risk_detection]
  - name: review-synthesizer
    display_name: 评审汇总员
    role: worker
    responsibilities: [合并评审结论, 统一风险等级, 排定修改顺序]
    capabilities: [review_synthesis, risk_prioritization]
lead: review-lead
delivery:
  success_criteria: [每个问题均定位到具体变更, 风险等级有明确依据, 修改建议可直接执行]
budget:
  max_cost_usd: 2
`

const humanFinalReviewSampleYAML = `schema: team-template/v1
name: human-final-review
display_name: 人工终审交付团队
purpose: 接收完整交付稿，并由工作区成员完成最终审核与决定
template: delivery_rework_loop
template_parameters:
  lead_instruction: 明确终审要求，接收完整交付稿并提交人工终审
  primary_ref: delivery-writer
  reviewer_ref: quality-reviewer
  max_iterations: 1
  result_requirements:
    delivery-lead: 确认任务边界并组织交付
    delivery-writer: 形成包含依据与边界的完整交付稿
    quality-reviewer: 在进入人工终审前检查交付稿完整性
members:
  - name: delivery-lead
    display_name: 交付负责人
    role: avatar
    responsibilities: [澄清要求, 组织交付, 跟进终审]
    capabilities: [task_delegation, delivery_management]
  - name: delivery-writer
    display_name: 交付撰稿人
    role: worker
    responsibilities: [材料整理, 形成交付稿]
    capabilities: [analysis, writing]
  - name: quality-reviewer
    display_name: 质量审校员
    role: worker
    responsibilities: [完整性检查, 风险标注]
    capabilities: [quality_review, risk_detection]
lead: delivery-lead
delivery:
  success_criteria: [交付稿完整回应任务要求, 依据与边界清晰, 人工终审决定被记录]
budget:
  max_cost_usd: 2
`
