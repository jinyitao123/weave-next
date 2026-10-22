import { describe, expect, it } from 'vitest'
import { allowRequiredHandoffKinds, capabilityName, capabilityNote, defaultCapabilityBinding, describeCapability, memberProblems, readTeamPackage, teamPackageFileName, nodeReadinessIssues, parseOutputSchema, requiredHandoffKinds, serverIssues, trialActions, trialStatus, workflowCapabilityIds, type BusinessCatalog, type DevelopmentDocument, type DevelopmentMember, type DevelopmentWorkflow } from './teams'

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

  it('asks a built-in lead for a model too, since it takes a step of the flow', () => {
    expect(memberProblems(ready('loom', { role: 'avatar' }))).toEqual(['使用内置引擎时需要选择模型'])
    expect(memberProblems(ready('loom', { role: 'avatar', model: 'deepseek-flash' }))).toEqual([])
  })

  it('checks the default handoff kind and the skills', () => {
    expect(memberProblems(ready('loom', { model: 'm', skills: [{ name: 'a', description: '', body: 'x', always_active: false }, { name: 'a', description: '', body: '', always_active: false }] }, { allowed_kinds: ['consult'], default_kind: 'dispatch' })))
      .toEqual(['默认交接方式必须是已选方式之一', '技能需要填写名称和内容', '技能名称不能重复'])
  })

  it('does not hold back a member for its MCP services, library skills or tool rules', () => {
    expect(memberProblems(ready('codex', { mcp_server_ids: ['crm'], permission_ask: ['bash'] }))).toEqual([])
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

describe('what a catalog entry adds to its name', () => {
  it('drops a description that only repeats the name', () => {
    expect(capabilityNote('读取旧版提交回执', ' 读取旧版提交回执 ')).toBe('')
    expect(capabilityNote('转为商机', '确认后会将线索转为客户档案和销售商机。')).toBe('确认后会将线索转为客户档案和销售商机。')
    expect(capabilityNote('转为商机', undefined)).toBe('')
  })
})

describe('what the server found in the saved draft', () => {
  it('says each finding the page does not already show, naming what it is about', () => {
    const draft = { revision: 1, published_revision: 0, updated_at: '', trials: [], document: { name: '团队', objective: '', audience: [], members: [], workflows: [] }, issues: [
      { scope: 'team', target: '团队', code: 'objective_missing', message: '团队目标为空，员工在桌面查不到这个团队', blocks: 'none' },
      { scope: 'member', target: '整理员', code: 'member_model_missing', message: '内置引擎成员需要选择模型', blocks: 'trial' },
      { scope: 'member', target: '整理员', code: 'business_action_unavailable', message: '绑定的一项业务动作已不在目录中或暂不可用，请重新选择', blocks: 'trial' },
      { scope: 'workflow', target: '跟进流程', workflow: '跟进流程', code: 'workflow_description_missing', message: '流程说明为空', blocks: 'none' },
      { scope: 'step', target: '核对', workflow: '跟进流程', code: 'step_member_missing', message: '步骤引用了已移出的成员，请重新选择执行者', blocks: 'trial' },
    ] } as Parameters<typeof serverIssues>[0]
    expect(serverIssues(draft)).toEqual([
      '团队目标为空，员工在桌面查不到这个团队',
      '“整理员”：绑定的一项业务动作已不在目录中或暂不可用，请重新选择',
      '流程“跟进流程”：流程说明为空',
      '流程“跟进流程”的步骤“核对”：步骤引用了已移出的成员，请重新选择执行者',
    ])
    expect(serverIssues(undefined)).toEqual([])
  })
})

describe('team package files', () => {
  it('accepts an exported team and refuses other files and versions', () => {
    expect(readTeamPackage('{"format":"weave-team-package","version":1,"team":{"name":"团队","objective":"目标"}}').team.name).toBe('团队')
    expect(() => readTeamPackage('not json')).toThrow('这个文件不是团队导出文件')
    expect(() => readTeamPackage('{"format":"weave-team-package","version":2,"team":{"name":"团队"}}')).toThrow('这个团队导出文件的版本当前不支持')
  })

  it('names the file after the team without characters a file system refuses', () => {
    expect(teamPackageFileName('合同/复核:团队')).toBe('合同_复核_团队.weave-team.json')
    expect(teamPackageFileName('  ')).toBe('团队.weave-team.json')
  })
})

describe('trialStatus', () => {
  const trial = (status: string, rejected_code?: string) => ({ request_id: 'r', revision: 1, workflow_id: 'w', run_id: '', status, created_at: '', rejected_code })
  const runLabel = (status: string) => `run:${status}`
  it('ends a refused trial with its reason instead of leaving it submitting', () => {
    expect(trialStatus(trial('rejected', 'workflow_version_conflict'), runLabel)).toEqual({ label: '未被接受', tone: 'danger', note: '草稿或它依赖的模型配置已变化，请重新准备后发起新的试跑' })
    expect(trialStatus(trial('rejected', 'unknown_code'), runLabel).note).toBe('这次试跑没有被接受，请重新准备后再试')
  })
  it('names a trial without a receipt and defers to the run once one exists', () => {
    expect(trialStatus(trial('submitting'), runLabel)).toEqual({ label: '提交中', tone: 'neutral' })
    expect(trialStatus(trial('succeeded'), runLabel)).toEqual({ label: 'run:succeeded', tone: 'success' })
    expect(trialStatus(trial('failed'), runLabel).tone).toBe('danger')
  })
})
