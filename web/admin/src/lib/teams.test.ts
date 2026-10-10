import { describe, expect, it } from 'vitest'
import { describeCapability, memberProblems, nodeReadinessIssues, normalizeAudience, parseOutputSchema, type DevelopmentDocument, type DevelopmentMember } from './teams'

const member = (name: string, engine: string, runtime = '', enabled = true): DevelopmentMember => ({
  id: name,
  configuration: { display_name: name, role: 'worker', engine, runtime_id: runtime, model: '', system_prompt: '' },
  relationship: { duty: '', result_requirement: '', enabled },
})
const team = (...members: DevelopmentMember[]): DevelopmentDocument => ({ name: '团队', objective: '', audience: [], members, workflows: [] })

describe('nodeReadinessIssues', () => {
  it('does not ask built-in engines for a node', () => {
    expect(nodeReadinessIssues(team(member('检查员', 'loom')), {})).toEqual([])
  })

  it('reports a CLI member once when it has neither an accepting node nor a pinned node', () => {
    expect(nodeReadinessIssues(team(member('验证', 'codex')), {})).toEqual(['“验证”使用的 Codex 当前没有可接任务的节点，也还没有指定节点'])
  })

  it('keeps the node-less line for a pinned member whose engine has no accepting node', () => {
    expect(nodeReadinessIssues(team(member('编码', 'claude', 'node-1')), {})).toEqual(['“编码”使用的 Claude 当前没有可接任务的节点'])
  })

  it('still asks an engine-ready member to pin a node', () => {
    expect(nodeReadinessIssues(team(member('编码', 'claude')), { claude: 1 })).toEqual(['“编码”还没有指定节点'])
  })

  it('ignores disabled members for the accepting-node check', () => {
    expect(nodeReadinessIssues(team(member('停用', 'codex', 'node-1', false)), {})).toEqual([])
  })
})

describe('memberProblems', () => {
  const ready = (engine: string, extra: Partial<DevelopmentMember['configuration']> = {}, relation: Partial<DevelopmentMember['relationship']> = {}): DevelopmentMember => ({
    id: 'm', configuration: { display_name: '成员', role: 'worker', engine, runtime_id: '', model: '', system_prompt: '方法', ...extra }, relationship: { duty: '职责', result_requirement: '', enabled: true, ...relation },
  })

  it('accepts a complete CLI member without a model', () => {
    expect(memberProblems(ready('codex'))).toEqual([])
  })

  it('asks for the duty, the working method and a model for built-in engines', () => {
    expect(memberProblems(ready('loom', { system_prompt: ' ' }, { duty: '' }))).toEqual(['还没有填写职责', '还没有填写工作方法', '使用内置引擎时需要选择模型'])
  })

  it('lets a built-in lead go without a model', () => {
    expect(memberProblems(ready('loom', { role: 'avatar' }))).toEqual([])
  })

  it('checks the default handoff kind and the skills', () => {
    expect(memberProblems(ready('loom', { model: 'm', skills: [{ name: 'a', description: '', body: 'x', always_active: false }, { name: 'a', description: '', body: '', always_active: false }] }, { allowed_kinds: ['consult'], default_kind: 'dispatch' })))
      .toEqual(['默认交接方式必须是已选方式之一', '技能需要填写名称和内容', '技能名称不能重复'])
  })

  it('flags external tools that isolated trials refuse', () => {
    expect(memberProblems(ready('codex', { mcp_server_ids: ['crm'] }))).toEqual(['带有试跑暂不支持的外部工具（MCP 服务、技能库技能或工具许可）'])
  })
})

describe('team helpers', () => {
  it('describes a Forge business action id', () => {
    expect(describeCapability('forge:action:forge_sales_lead.sales_lead_convert_to_opportunity')).toEqual({ object: 'forge_sales_lead', action: 'sales_lead_convert_to_opportunity' })
    expect(describeCapability('custom')).toEqual({ object: '', action: 'custom' })
  })

  it('normalizes the audience like the server does', () => {
    expect(normalizeAudience([' a ', '', 'b'])).toEqual({ value: ['a', 'b'] })
    expect(normalizeAudience(['a', ' a']).error).toBe('权限集名称不能重复')
    expect(normalizeAudience(['x'.repeat(129)]).error).toContain('128')
    expect(normalizeAudience(Array.from({ length: 33 }, (_, index) => `s${index}`)).error).toBe('最多 32 个权限集')
  })

  it('parses an output schema', () => {
    expect(parseOutputSchema('  ')).toEqual({ value: null })
    expect(parseOutputSchema('{"a":1}')).toEqual({ value: { a: 1 } })
    expect(parseOutputSchema('[]').error).toBe('输出结构必须是一个 JSON 对象')
    expect(parseOutputSchema('{').error).toBe('输出结构不是有效的 JSON')
  })

  it('checks the tool loop and the output schema of a member', () => {
    const base = (extra: Partial<DevelopmentMember['configuration']>): DevelopmentMember => ({
      id: 'm', configuration: { display_name: '成员', role: 'worker', engine: 'loom', runtime_id: '', model: 'm', system_prompt: '方法', ...extra }, relationship: { duty: '职责', result_requirement: '', enabled: true },
    })
    expect(memberProblems(base({ tool_loop_control: { slice_rounds: 1001, initial_total_rounds: 5 } }))).toEqual(['工具循环的每片轮次需在 1 到 1000 之间，总轮次不能小于 1'])
    expect(memberProblems(base({ tool_loop_control: { slice_rounds: 5, initial_total_rounds: 0 } }))).toHaveLength(1)
    expect(memberProblems(base({ tool_loop_control: { slice_rounds: 5, initial_total_rounds: 5 } }))).toEqual([])
    expect(memberProblems(base({ output_schema: ['x'] }))).toEqual(['输出结构必须是一个 JSON 对象'])
  })
})
