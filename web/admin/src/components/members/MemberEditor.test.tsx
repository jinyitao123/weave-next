import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react'
import { useState } from 'react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import type { BusinessCatalog, DevelopmentDocument, DevelopmentMember } from '../../lib/teams'
import { MemberEditor } from './MemberEditor'

const member = (id: string, name: string, role: string, engine: string, extra: Partial<DevelopmentMember['configuration']> = {}, relation: Partial<DevelopmentMember['relationship']> = {}): DevelopmentMember => ({
  id,
  configuration: { display_name: name, role, engine, runtime_id: '', model: '', system_prompt: '工作方法', ...extra },
  relationship: { duty: '职责', result_requirement: '', enabled: true, ...relation },
})
const action = 'forge:action:forge_sales_lead.sales_lead_convert_to_opportunity'
const team = (): DevelopmentDocument => ({ name: '团队', objective: '', audience: [], workflows: [], members: [
  member('w-cli', '编码', 'worker', 'claude'),
  member('lead', '负责人员', 'avatar', 'loom'),
  member('w-loom', '整理', 'worker', 'loom', {
    model: 'deepseek-flash', permission_deny: ['bash'],
    business_capability_ids: [action], business_capability_bindings: [{ capability_id: action, parameters: [{ name: 'material_file_ids', source: 'materials.ids' }] }],
  }),
] })

function Controlled({ changed, initial, catalog }: { changed?(document: DevelopmentDocument): void; initial?: DevelopmentDocument; catalog?: BusinessCatalog }) {
  const [document, setDocument] = useState(initial ?? team())
  const apply = (next: DevelopmentDocument) => { setDocument(next); changed?.(next) }
  return <MemberEditor document={document} nodes={[]} accepting={{}} catalog={catalog}
    onConfig={(id, patch) => apply({ ...document, members: document.members.map((item) => item.id === id ? { ...item, configuration: { ...item.configuration, ...patch } } : item) })}
    onRelationship={(id, patch) => apply({ ...document, members: document.members.map((item) => item.id === id ? { ...item, relationship: { ...item.relationship, ...patch } } : item) })} />
}

const saved = (document: DevelopmentDocument | undefined, id: string) => document?.members.find((item) => item.id === id)
const pick = (name: RegExp) => fireEvent.click(screen.getByRole('button', { name }))
const tab = (name: string) => fireEvent.click(screen.getByRole('tab', { name }))

beforeEach(() => {
  vi.stubGlobal('fetch', vi.fn(async () => new Response(JSON.stringify({ version: '1', models: ['deepseek-flash', 'qwen-max'] }), { status: 200 })))
})
afterEach(() => { cleanup(); vi.unstubAllGlobals() })

describe('member editor', () => {
  it('lists the lead first, summarizes each member and keeps lead-only restrictions', () => {
    render(<Controlled />)
    const items = screen.getAllByRole('button').filter((button) => button.className.includes('members__item'))
    expect(items[0].textContent).toContain('负责人员')
    expect(items[0].textContent).toContain('负责人')
    expect(items.find((item) => item.textContent?.includes('整理'))?.textContent).toContain('1 个业务动作')
    expect(screen.queryByText('何时参与')).toBeNull()
    tab('执行')
    expect(screen.getByRole('button', { name: '负责人员的引擎' }).hasAttribute('disabled')).toBe(true)
    expect(screen.queryByRole('group', { name: '交接方式' })).toBeNull()
  })

  it('splits the detail into duty, ability and run tabs with engine-specific sections', () => {
    render(<Controlled />)
    pick(/^编码/)
    expect(screen.getByRole('region', { name: '输出结构' })).toBeTruthy()
    tab('能力')
    expect(screen.queryByRole('region', { name: '技能' })).toBeNull()
    tab('执行')
    expect(screen.getByRole('button', { name: '编码的节点' })).toBeTruthy()
    expect(screen.queryByRole('region', { name: '执行限制' })).toBeNull()
    pick(/^整理/)
    tab('能力')
    expect(screen.getByRole('region', { name: '技能' })).toBeTruthy()
    tab('执行')
    expect(screen.queryByRole('button', { name: '整理的节点' })).toBeNull()
    for (const region of ['执行限制', '工具循环', '记忆']) expect(screen.getByRole('region', { name: region })).toBeTruthy()
  })

  it('writes duty to the relationship and keeps other tool denials when toggling deny-all', () => {
    let last: DevelopmentDocument | undefined
    render(<Controlled changed={(document) => { last = document }} />)
    pick(/^整理/)
    fireEvent.change(screen.getByRole('textbox', { name: '职责' }), { target: { value: '整理材料' } })
    expect(saved(last, 'w-loom')?.relationship.duty).toBe('整理材料')
    tab('能力')
    fireEvent.click(screen.getByRole('switch', { name: '禁止使用工具' }))
    expect(saved(last, 'w-loom')?.configuration.permission_deny).toEqual(['bash', '*'])
    fireEvent.click(screen.getByRole('switch', { name: '禁止使用工具' }))
    expect(saved(last, 'w-loom')?.configuration.permission_deny).toEqual(['bash'])
  })

  it('shows the assigned business actions with their parameter sources and removes one with its bindings', () => {
    let last: DevelopmentDocument | undefined
    render(<Controlled changed={(document) => { last = document }} />)
    pick(/^整理/)
    tab('能力')
    const region = screen.getByRole('region', { name: '业务动作' })
    expect(region.textContent).toContain('sales_lead_convert_to_opportunity')
    expect(region.textContent).toContain('material_file_ids：取自本次提交的全部材料')
    fireEvent.click(screen.getByRole('button', { name: '移除业务动作 sales_lead_convert_to_opportunity' }))
    expect(last).toBeUndefined()
    fireEvent.click(screen.getByRole('button', { name: '取消' }))
    fireEvent.click(screen.getByRole('button', { name: '移除业务动作 sales_lead_convert_to_opportunity' }))
    fireEvent.click(screen.getByRole('button', { name: '移除' }))
    expect(saved(last, 'w-loom')?.configuration.business_capability_ids).toEqual([])
    expect(saved(last, 'w-loom')?.configuration.business_capability_bindings).toEqual([])
    expect(screen.getByRole('region', { name: '业务动作' }).textContent).toContain('还没有分配业务动作')
  })

  it('names actions from the catalog and adds one with its file parameters mapped to the task materials', () => {
    const quote = 'forge:action:forge_quote.submit'
    const catalog: BusinessCatalog = { available: true, capabilities: [
      { id: action, name: '线索转商机', description: '把线索转成商机', effect: 'write', status: 'available', params: [{ name: 'material_file_ids', label: '材料', type: 'file', multiple: true }] },
      { id: quote, name: '提交报价', effect: 'write', status: 'available', params: [{ name: 'attachment', type: 'file' }] },
      { id: 'forge:action:forge_quote.approve', name: '审批报价', effect: 'write', status: 'available', executionMode: 'employee_only' },
    ] }
    let last: DevelopmentDocument | undefined
    render(<Controlled catalog={catalog} changed={(document) => { last = document }} />)
    pick(/^整理/)
    tab('能力')
    const region = screen.getByRole('region', { name: '业务动作' })
    expect(region.textContent).toContain('线索转商机')
    expect(region.textContent).toContain('材料：取自本次提交的全部材料')
    expect(region.textContent).not.toContain('sales_lead_convert_to_opportunity')
    fireEvent.click(screen.getByRole('button', { name: '添加业务动作' }))
    const dialog = screen.getByRole('dialog')
    expect(dialog.textContent).toContain('提交报价')
    expect(dialog.textContent).not.toContain('审批报价')
    expect(dialog.textContent).not.toContain('线索转商机')
    fireEvent.click(screen.getByRole('button', { name: /提交报价/ }))
    expect(saved(last, 'w-loom')?.configuration.business_capability_ids).toEqual([action, quote])
    expect(saved(last, 'w-loom')?.configuration.business_capability_bindings).toContainEqual({ capability_id: quote, parameters: [{ name: 'attachment', source: 'materials.single.id' }] })
  })

  it('cannot add an action before the catalog has been read', () => {
    render(<Controlled />)
    pick(/^整理/)
    tab('能力')
    expect((screen.getByRole('button', { name: '添加业务动作' }) as HTMLButtonElement).disabled).toBe(true)
    expect(screen.getByRole('region', { name: '业务动作' }).textContent).toContain('重新登录')
  })

  it('offers node engines only while a node can take their work and keeps the handoff kinds a flow uses', () => {
    const initial = team()
    initial.workflows = [{ id: 'flow', name: '流程', description: '', trigger_config: {}, graph_definition: { schema_version: 1, entry_node_id: 'a', nodes: [{ id: 'a', type: 'worker', config: { agent_id: 'w-loom', kind: 'consult' } }], edges: [] } as never }]
    render(<Controlled initial={initial} />)
    pick(/^整理/)
    tab('执行')
    fireEvent.click(screen.getByRole('button', { name: '整理的引擎' }))
    expect(screen.getAllByRole('option').map((option) => option.textContent)).toEqual(['Loom'])
    expect((screen.getByRole('button', { name: '咨询' }) as HTMLButtonElement).disabled).toBe(true)
    expect((screen.getByRole('button', { name: '派发' }) as HTMLButtonElement).disabled).toBe(false)
  })

  it('flags external tool configuration that trials refuse and clears it on request', () => {
    let last: DevelopmentDocument | undefined
    const initial = team()
    initial.members[0].configuration.mcp_server_ids = ['crm']
    initial.members[0].configuration.permission_ask = ['bash']
    render(<Controlled initial={initial} changed={(document) => { last = document }} />)
    pick(/^编码/)
    tab('能力')
    expect(screen.getByRole('status').textContent).toContain('MCP 服务：crm')
    fireEvent.click(screen.getByRole('button', { name: '移除这些配置' }))
    expect(saved(last, 'w-cli')?.configuration).toMatchObject({ mcp_server_ids: [], skill_names: [], permission_allow: [], permission_ask: [] })
  })

  it('stores a valid output schema, refuses invalid JSON and clears on empty', () => {
    let last: DevelopmentDocument | undefined
    render(<Controlled changed={(document) => { last = document }} />)
    pick(/^整理/)
    const field = screen.getByRole('textbox', { name: /JSON Schema/ })
    fireEvent.change(field, { target: { value: '{"type":"object"' } })
    expect(screen.getByRole('alert').textContent).toContain('格式不对')
    expect(saved(last, 'w-loom')?.configuration.output_schema).toBeUndefined()
    fireEvent.change(field, { target: { value: '[1]' } })
    expect(screen.getByRole('alert').textContent).toContain('JSON 对象')
    fireEvent.change(field, { target: { value: '{"type":"object","required":["summary"]}' } })
    expect(screen.queryByRole('alert')).toBeNull()
    expect(saved(last, 'w-loom')?.configuration.output_schema).toEqual({ type: 'object', required: ['summary'] })
    fireEvent.change(field, { target: { value: '' } })
    expect(saved(last, 'w-loom')?.configuration.output_schema).toBeNull()
  })

  it('starts from the server default handoff kinds and keeps the default among the chosen ones', () => {
    let last: DevelopmentDocument | undefined
    render(<Controlled changed={(document) => { last = document }} />)
    pick(/^编码/)
    tab('执行')
    expect(screen.getByRole('button', { name: '咨询' }).getAttribute('aria-pressed')).toBe('true')
    expect(screen.getByRole('button', { name: '派发' }).getAttribute('aria-pressed')).toBe('true')
    fireEvent.click(screen.getByRole('button', { name: '交接' }))
    expect(saved(last, 'w-cli')?.relationship).toMatchObject({ allowed_kinds: ['consult', 'dispatch', 'handoff'], default_kind: 'dispatch' })
    fireEvent.click(screen.getByRole('button', { name: '派发' }))
    expect(saved(last, 'w-cli')?.relationship).toMatchObject({ allowed_kinds: ['consult', 'handoff'], default_kind: 'consult' })
  })

  it('stores limits as whole numbers, cost as a decimal and empty as unlimited', () => {
    let last: DevelopmentDocument | undefined
    render(<Controlled changed={(document) => { last = document }} />)
    pick(/^整理/)
    tab('执行')
    fireEvent.change(screen.getByRole('textbox', { name: '最多执行步数' }), { target: { value: '12.7' } })
    expect(saved(last, 'w-loom')?.configuration.step_budget).toBe(12)
    fireEvent.change(screen.getByRole('textbox', { name: '费用上限（美元）' }), { target: { value: '0.5' } })
    expect(saved(last, 'w-loom')?.configuration.max_cost_usd).toBe(0.5)
    fireEvent.change(screen.getByRole('textbox', { name: '最多执行步数' }), { target: { value: '' } })
    expect(saved(last, 'w-loom')?.configuration.step_budget).toBe(0)
  })

  it('turns the custom tool loop on with defaults, edits it and turns it off', () => {
    let last: DevelopmentDocument | undefined
    render(<Controlled changed={(document) => { last = document }} />)
    pick(/^整理/)
    tab('执行')
    fireEvent.click(screen.getByRole('switch', { name: '自定义工具循环轮次' }))
    expect(saved(last, 'w-loom')?.configuration.tool_loop_control).toEqual({ slice_rounds: 20, initial_total_rounds: 20 })
    fireEvent.change(screen.getByRole('textbox', { name: /每片最多轮次/ }), { target: { value: '8' } })
    expect(saved(last, 'w-loom')?.configuration.tool_loop_control).toEqual({ slice_rounds: 8, initial_total_rounds: 20 })
    fireEvent.click(screen.getByRole('switch', { name: '自定义工具循环轮次' }))
    expect(saved(last, 'w-loom')?.configuration.tool_loop_control).toBeNull()
  })

  it('offers catalog models for built-in engines', async () => {
    render(<Controlled />)
    pick(/^整理/)
    tab('执行')
    await waitFor(() => expect(globalThis.fetch).toHaveBeenCalled())
    fireEvent.click(screen.getByRole('button', { name: '整理的模型' }))
    expect(await screen.findByRole('option', { name: 'qwen-max' })).toBeTruthy()
  })
})
