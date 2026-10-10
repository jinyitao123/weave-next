import { describe, expect, it } from 'vitest'
import { allowRequiredHandoffKinds, capabilityName, defaultCapabilityBinding, describeCapability, memberProblems, nodeReadinessIssues, parseOutputSchema, requiredHandoffKinds, trialActions, workflowCapabilityIds, type BusinessCatalog, type DevelopmentDocument, type DevelopmentMember, type DevelopmentWorkflow } from './teams'

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

  it('parses an output schema', () => {
    expect(parseOutputSchema('  ')).toEqual({ value: null })
    expect(parseOutputSchema('{"a":1}')).toEqual({ value: { a: 1 } })
    expect(parseOutputSchema('[]').error).toContain('JSON 对象')
    expect(parseOutputSchema('{').error).toContain('格式不对')
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

describe('trial actions and handoff kinds', () => {
  const convert = 'forge:action:forge_sales_lead.sales_lead_convert_to_opportunity', read = 'forge:action:forge_sales_lead.read'
  const catalog: BusinessCatalog = { available: true, capabilities: [
    { id: convert, name: '线索转商机', description: '把线索转成商机', effect: 'write', status: 'available', requiresRecord: true,
      params: [{ name: 'material_file_ids', label: '材料', type: 'file', multiple: true }, { name: 'contract', type: 'file' }, { name: 'idempotency_key', type: 'file' }, { name: 'amount', type: 'number' }] },
  ] }
  const worker = (id: string, ids: string[], relation: Partial<DevelopmentMember['relationship']> = {}): DevelopmentMember => ({
    id, configuration: { display_name: id, role: 'worker', engine: 'loom', runtime_id: '', model: 'm', system_prompt: 'x', business_capability_ids: ids },
    relationship: { duty: 'd', result_requirement: '', enabled: true, ...relation },
  })
  const flow = (nodes: Array<Record<string, unknown>>): DevelopmentWorkflow => ({ id: 'flow', name: '流程', description: '', trigger_config: {}, graph_definition: { schema_version: 1, entry_node_id: 'a', nodes, edges: [] } as never })
  const document = (members: DevelopmentMember[], workflow: DevelopmentWorkflow): DevelopmentDocument => ({ name: '团队', objective: '', audience: [], members, workflows: [workflow] })

  it('collects the actions of the members a flow actually uses', () => {
    const steps = flow([{ id: 'a', type: 'worker', config: { agent_id: 'used', kind: 'consult' } }])
    const team = document([worker('used', [convert]), worker('idle', [read]), worker('off', [read], { enabled: false })], steps)
    expect(workflowCapabilityIds(team, steps)).toEqual([convert])
  })

  it('freezes trial definitions from the catalog and reports actions it does not list', () => {
    const result = trialActions([convert, read], catalog, new Set([convert]))
    expect(result.missing).toEqual([read])
    expect(result.actions).toEqual([expect.objectContaining({ capability_id: convert, name: 'sales_lead_convert_to_opportunity', object_name: 'forge_sales_lead', label: '线索转商机', requires_record: true, requires_confirmation: false, simulation_authorized: true })])
    expect(trialActions([convert], catalog, new Set()).actions[0].simulation_authorized).toBe(false)
    expect(trialActions([convert], undefined, new Set()).missing).toEqual([convert])
  })

  it('names an action from the catalog and maps its file parameters to the task materials', () => {
    expect(capabilityName(convert, catalog)).toBe('线索转商机')
    expect(capabilityName(read, catalog)).toBe('read')
    expect(defaultCapabilityBinding(catalog.capabilities[0])).toEqual({ capability_id: convert, parameters: [{ name: 'material_file_ids', source: 'materials.ids' }, { name: 'contract', source: 'materials.single.id' }] })
    expect(defaultCapabilityBinding({ id: read, name: '读取', effect: 'read', status: 'available' })).toBeUndefined()
  })

  it('allows the handoff kind a step needs without touching members that leave it open', () => {
    const steps = flow([{ id: 'a', type: 'worker', config: { agent_id: 'narrow', kind: 'dispatch' } }, { id: 'b', type: 'worker', config: { agent_id: 'open', kind: 'consult' } }])
    const team = document([worker('narrow', [], { allowed_kinds: ['consult'], default_kind: 'consult' }), worker('open', [])], steps)
    expect(requiredHandoffKinds(team, 'narrow')).toEqual(['dispatch'])
    const next = allowRequiredHandoffKinds(team)
    expect(next.members[0].relationship.allowed_kinds).toEqual(['consult', 'dispatch'])
    expect(next.members[1]).toBe(team.members[1])
    expect(allowRequiredHandoffKinds(next)).toBe(next)
  })
})
