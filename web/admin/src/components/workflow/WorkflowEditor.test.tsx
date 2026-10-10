import { fireEvent, render, screen, cleanup } from '@testing-library/react'
import { useState } from 'react'
import { afterEach, describe, expect, it } from 'vitest'
import fixtureText from '../../../../../internal/kernel/workflow/machine/testdata/code_verify_loop.json?raw'
import { serialGraph, type Graph } from '../../lib/graph'
import type { DevelopmentMember } from '../../lib/teams'
import { layout } from './FlowCanvas'
import { WorkflowEditor } from './WorkflowEditor'

const members: DevelopmentMember[] = ['编码', '验证', '核对'].map(id => ({ id, configuration: { display_name: id, role: 'worker', engine: 'loom', runtime_id: '', model: '', system_prompt: '' }, relationship: { duty: '', result_requirement: '', enabled: true } }))
function Controlled({ initial, changed }: { initial: Graph; changed?: (graph: Graph) => void }) {
  const [graph, setGraph] = useState(initial)
  return <WorkflowEditor graph={graph} members={members} onChange={operation => setGraph(current => { const next = operation(current); changed?.(next); return next })} />
}
afterEach(cleanup)
describe('workflow canvas interactions', () => {
  it('selects an existing node, edits only its instruction and preserves opaque data', () => {
    const graph = serialGraph(members.slice(0, 2).map(member => ({ member: { id: member.id, displayName: member.id }, label: member.id, requirement: '原交付要求' })))
    graph.custom = { future: true }; graph.nodes[1].config!.opaque = { nested: 1 }
    let saved: Graph | undefined
    render(<Controlled initial={graph} changed={value => { saved = value }} />)
    fireEvent.click(screen.getByRole('button', { name: '编辑步骤 验证' }))
    expect(screen.getByRole('button', { name: '编辑步骤 验证' }).getAttribute('aria-pressed')).toBe('true')
    fireEvent.change(screen.getByRole('textbox', { name: '工作要求' }), { target: { value: '新的交付要求' } })
    expect(saved?.nodes[1].config?.result_requirement).toBe('新的交付要求')
    expect(saved?.nodes[1].config?.opaque).toEqual({ nested: 1 })
    expect(saved?.custom).toEqual({ future: true })
    expect(saved?.edges).toEqual(graph.edges)
  })

  it('creates an actual parallel region through the graph editor form', () => {
    const graph = serialGraph(members.map(member => ({ member: { id: member.id, displayName: member.id }, label: member.id, requirement: '交付要求' })))
    let saved: Graph | undefined
    render(<Controlled initial={graph} changed={value => { saved = value }} />)
    fireEvent.click(screen.getByRole('button', { name: '编辑步骤 核对' }))
    expect(screen.queryByRole('button', { name: '添加并行分支' })).toBeNull()
    expect(screen.getByText(/交付前的最后一步不能并行/)).toBeTruthy()
    fireEvent.click(screen.getByRole('button', { name: '编辑步骤 验证' }))
    fireEvent.click(screen.getByRole('button', { name: '添加并行分支' }))
    expect(screen.getByRole('dialog').textContent).toContain('与“验证”同时执行')
    fireEvent.click(screen.getByRole('button', { name: '新步骤执行成员' }))
    fireEvent.mouseDown(screen.getByRole('option', { name: '核对' }))
    const names = screen.getAllByRole('textbox', { name: '步骤名称' }); fireEvent.change(names[names.length - 1], { target: { value: '并行核对' } })
    const requirements = screen.getAllByRole('textbox', { name: '工作要求' }); fireEvent.change(requirements[requirements.length - 1], { target: { value: '独立核对' } })
    fireEvent.click(screen.getByRole('button', { name: '添加分支' }))
    expect(screen.queryByRole('dialog')).toBeNull()
    expect(saved?.nodes.some(node => node.type === 'parallel')).toBe(true)
    expect(saved?.nodes.some(node => node.type === 'join')).toBe(true)
    expect(screen.getByRole('button', { name: '编辑步骤 并行核对' })).toBeTruthy()
  })

  it('renders unsupported raw nodes and exposes a read-only inspector without serial conversion', () => {
    const graph = serialGraph(members.slice(0, 2).map(member => ({ member: { id: member.id, displayName: member.id }, label: member.id, requirement: '' })))
    graph.nodes.push({ id: 'condition', type: 'condition', label: '已有条件', config: { raw: true } })
    let calls = 0
    render(<Controlled initial={graph} changed={() => { calls += 1 }} />)
    fireEvent.click(screen.getByRole('button', { name: '编辑步骤 已有条件' }))
    expect(screen.getByText('该步骤暂不提供配置编辑，原有节点与连接会保留。')).toBeTruthy()
    expect(screen.queryByRole('textbox', { name: '步骤名称' })).toBeNull()
    expect(calls).toBe(0)
  })

  it('keeps machine verification-loop controls and protects its iteration input bindings', () => {
    const graph = JSON.parse(fixtureText) as Graph
    render(<Controlled initial={graph} />)
    expect(screen.getByRole('switch', { name: '验证未通过时退回第一步' }).getAttribute('aria-checked')).toBe('true')
    fireEvent.click(screen.getByRole('button', { name: '编辑步骤 编码' }))
    expect(screen.getByText('输入沿用验证回路的本轮结果与上一轮验证意见。')).toBeTruthy()
    expect(screen.queryByRole('button', { name: '添加并行分支' })).toBeNull()
    expect(screen.queryByRole('button', { name: '删除步骤' })).toBeNull()
  })

  it('places delivery after the verification step and labels the back edge with its rounds', () => {
    const graph = JSON.parse(fixtureText) as Graph
    const box = layout(graph)
    const x = (id: string) => box.positions.get(id)!.x
    expect(x('loop')).toBeLessThan(x('code'))
    expect(x('code')).toBeLessThan(x('verify'))
    expect(x('verify')).toBeLessThan(x('deliver'))
    render(<Controlled initial={graph} />)
    expect(screen.getByText('未通过退回 · 最多 3 轮')).toBeTruthy()
  })

  it('edits the loop rounds from the loop step', () => {
    const graph = JSON.parse(fixtureText) as Graph
    let saved: Graph | undefined
    render(<Controlled initial={graph} changed={value => { saved = value }} />)
    fireEvent.click(screen.getByRole('button', { name: '编辑步骤 编码与验证' }))
    expect(screen.queryByText('该步骤暂不提供配置编辑，原有节点与连接会保留。')).toBeNull()
    fireEvent.click(screen.getByRole('button', { name: '验证回路最多轮数' }))
    fireEvent.mouseDown(screen.getByRole('option', { name: '5 轮' }))
    expect(saved?.nodes.find(node => node.id === 'loop')?.config?.max_iterations).toBe(5)
    expect(screen.getByText('未通过退回 · 最多 5 轮')).toBeTruthy()
  })

  it('zooms the canvas in steps and returns to fit-to-width', () => {
    render(<Controlled initial={JSON.parse(fixtureText) as Graph} />)
    const fit = screen.getByRole('button', { name: '适应宽度' })
    expect(fit.getAttribute('aria-pressed')).toBe('true')
    expect(screen.getByText('100%')).toBeTruthy()
    fireEvent.click(screen.getByRole('button', { name: '缩小' }))
    expect(screen.getByText('90%')).toBeTruthy()
    expect(fit.getAttribute('aria-pressed')).toBe('false')
    expect((screen.getByRole('region', { name: '流程画布' }).querySelector('.flow-editor__graph') as HTMLElement).style.transform).toBe('scale(0.9)')
    fireEvent.click(screen.getByRole('button', { name: '放大' }))
    fireEvent.click(screen.getByRole('button', { name: '放大' }))
    expect(screen.getByText('125%')).toBeTruthy()
    fireEvent.click(fit)
    expect(screen.getByText('100%')).toBeTruthy()
    expect(fit.getAttribute('aria-pressed')).toBe('true')
  })
})

describe("step inputs", () => {
  const chain = () => serialGraph(members.map(member => ({ member: { id: member.id, displayName: member.id }, label: member.id, requirement: "返回依据" })))

  it("lists the task and every earlier step as checkboxes with who produces them", () => {
    render(<Controlled initial={chain()} />)
    fireEvent.click(screen.getByRole("button", { name: "编辑步骤 核对" }))
    const panel = screen.getByRole("region", { name: "这一步会收到" })
    const boxes = Array.from(panel.querySelectorAll("input[type=checkbox]")) as HTMLInputElement[]
    expect(boxes.map(box => box.closest("label")?.textContent)).toEqual(["本次任务输入发起任务时提交的原始内容", "编码的结果由编码产出", "验证的结果由验证产出"])
    expect(boxes.map(box => box.checked)).toEqual([true, false, true])
    expect(screen.getByRole("status").textContent).toBe("开始时会收到：本次任务输入、验证的结果")
  })

  it("marks the chosen sources on the canvas and follows the checkboxes", () => {
    let saved: Graph | undefined
    render(<Controlled initial={chain()} changed={value => { saved = value }} />)
    fireEvent.click(screen.getByRole("button", { name: "编辑步骤 核对" }))
    const canvas = screen.getByRole("region", { name: "流程画布" })
    expect(canvas.querySelectorAll(".flow-editor__node.is-feeding")).toHaveLength(1)
    expect(canvas.querySelectorAll("path.is-feed")).toHaveLength(2)
    expect(canvas.querySelector(".flow-editor__task")).toBeTruthy()
    fireEvent.click(screen.getByRole("checkbox", { name: /编码的结果/ }))
    expect(canvas.querySelectorAll(".flow-editor__node.is-feeding")).toHaveLength(2)
    expect(screen.getByRole("status").textContent).toBe("开始时会收到：本次任务输入、编码的结果、验证的结果")
    expect(Object.values(saved!.nodes[2].inputs ?? {}).some(binding => binding.value.node_id === saved!.nodes[0].id)).toBe(true)
  })

  it("drops every binding of a source it switches off and warns when nothing is left", () => {
    const graph = chain()
    graph.nodes[1].inputs = { ...graph.nodes[1].inputs, review: { value: { source: "node_output", node_id: graph.nodes[0].id, path: "/summary", iteration: "previous_iteration" }, expected_type: "text" } as never }
    let saved: Graph | undefined
    render(<Controlled initial={graph} changed={value => { saved = value }} />)
    fireEvent.click(screen.getByRole("button", { name: "编辑步骤 验证" }))
    fireEvent.click(screen.getByRole("checkbox", { name: /本次任务输入/ }))
    fireEvent.click(screen.getByRole("checkbox", { name: /编码的结果/ }))
    expect(saved?.nodes[1].inputs).toEqual({})
    expect(screen.getByRole("status").textContent).toContain("没有选择任何内容")
  })

  it("highlights the source on the canvas while its row is pointed at", () => {
    render(<Controlled initial={chain()} />)
    fireEvent.click(screen.getByRole("button", { name: "编辑步骤 核对" }))
    const row = screen.getByRole("checkbox", { name: /验证的结果/ }).closest("label")!
    fireEvent.mouseEnter(row)
    expect(screen.getByRole("button", { name: "编辑步骤 验证" }).className).toContain("is-hot")
    fireEvent.mouseLeave(row)
    expect(screen.getByRole("button", { name: "编辑步骤 验证" }).className).not.toContain("is-hot")
  })
})
