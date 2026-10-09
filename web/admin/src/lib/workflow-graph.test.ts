import { describe, expect, it } from 'vitest'
import fixtureText from '../../../../internal/kernel/workflow/machine/testdata/code_verify_loop.json?raw'
import { serialGraph, verifyLoop, type Graph } from './graph'
import type { DevelopmentMember } from './teams'
import { addParallelBranch, assignExecutor, configureWorkflowResultProtocol, insertStep, predecessors, removeStep, serializeParallel, setVerificationLoop } from './workflow-graph'

export const member = (id: string): DevelopmentMember => ({ id, configuration: { display_name: id, role: 'worker', engine: 'loom', runtime_id: '', model: '', system_prompt: '' }, relationship: { duty: '', result_requirement: '返回依据', enabled: true } })
function graph() {
  return serialGraph(['编码', '验证'].map(name => ({ member: { id: name, displayName: name }, label: name, requirement: '返回依据' })))
}

describe('workflow canvas graph edits', () => {
  it('preserves opaque fields, schemas, bindings and unaffected raw nodes during insertion', () => {
    const input = graph()
    input.opaque = { schema: { unknown: true } }
    input.input_contract.schema = { description: 'keep' }
    const first = input.nodes[0], last = input.nodes.find(node => node.type === 'deliver')!
    first.config!.custom = { keep: 1 }
    first.inputs!.extra = { value: { source: 'literal', path: '', default: { source: 'literal', value: { raw: true } } }, expected_type: 'json' }
    const raw = { id: 'raw', type: 'condition', label: '已有条件', config: { predicate: { untouched: true } }, output: { schema: { nested: ['raw'] } }, opaque: { keep: true } }
    input.nodes.push(raw)
    Object.assign(input.edges[0], { opaque: { original: true } })
    const before = structuredClone(input)
    const changed = insertStep(input, first.id, member('新增'))
    expect(changed.graph.opaque).toEqual(before.opaque)
    expect(changed.graph.input_contract).toEqual(before.input_contract)
    expect(changed.graph.nodes.find(node => node.id === first.id)).toEqual(before.nodes[0])
    expect(changed.graph.nodes.find(node => node.id === raw.id)).toEqual(raw)
    expect(changed.graph.nodes.find(node => node.id === last.id)).toEqual(last)
    expect(changed.graph.edges.find(edge => edge.id === before.edges[0].id)).toMatchObject({ opaque: { original: true } })
    expect(input).toEqual(before)
  })

  it('expands a worker into parallel dispatch, supports third branch removal and serial conversion', () => {
    const input = graph(), first = input.nodes[1]
    const parallel = addParallelBranch(input, first.id, member('并行成员')).graph
    const split = parallel.nodes.find(node => node.type === 'parallel')!
    expect(parallel.nodes.find(node => node.id === first.id)?.config?.kind).toBe('dispatch')
    expect(parallel.edges.filter(edge => edge.from_node_id === split.id)).toHaveLength(2)
    expect(() => removeStep(parallel, parallel.nodes.find(node => node.config?.agent_id === '并行成员')!.id)).toThrow('至少保留两个')
    const third = addParallelBranch(parallel, split.id, member('第三成员'))
    const removed = removeStep(third.graph, third.selected)
    expect(removed.nodes).toEqual(parallel.nodes)
    const serial = serializeParallel(removed, split.id)
    expect(serial.nodes.some(node => ['parallel', 'join'].includes(node.type))).toBe(false)
    expect(serial.nodes.filter(node => node.type === 'worker').every(node => node.config?.kind === 'consult')).toBe(true)
  })

  it('blocks deleting referenced results and changing parallel structure against quorum', () => {
    const input = graph()
    expect(() => removeStep(input, input.nodes[1].id)).not.toThrow()
    const p = addParallelBranch(input, input.nodes[1].id, member('并行成员'))
    const split = p.graph.nodes.find(node => node.type === 'parallel')!
    const third = addParallelBranch(p.graph, split.id, member('第三成员'))
    const join = third.graph.nodes.find(node => node.type === 'join')!
    join.config = { policy: 'quorum', success_count: 3 }
    expect(() => removeStep(third.graph, third.selected)).toThrow('降低汇合')
    third.graph.nodes[1].inputs!.join = { value: { source: 'node_output', node_id: join.id, path: '' }, expected_type: 'json' }
    expect(() => serializeParallel(third.graph, split.id)).toThrow('汇总结果')
  })

  it('keeps member config extensions and allows only worker executors in parallel branches', () => {
    const input = graph(); input.nodes[0].config!.custom = { untouched: true }
    const changed = assignExecutor(input, input.nodes[0].id, member('新成员'))
    expect(changed.nodes[0].config?.custom).toEqual({ untouched: true })
    const p = addParallelBranch(input, input.nodes[1].id, member('并行成员'))
    const lead = member('负责人'); lead.configuration.role = 'avatar'
    expect(() => assignExecutor(p.graph, p.selected, lead)).toThrow('只能由执行成员')
  })

  it('keeps the existing machine verify-loop fixture and iteration bindings', () => {
    const fixture = JSON.parse(fixtureText) as Graph
    const before = structuredClone(fixture)
    const five = setVerificationLoop(fixture, true, 5)
    expect(verifyLoop(five)?.rounds).toBe(5)
    expect(five.nodes.filter(node => node.type !== 'loop')).toEqual(before.nodes.filter(node => node.type !== 'loop'))
    expect(five.edges).toEqual(before.edges)
    expect(predecessors(fixture, 'code').some(node => node.id === 'code' || node.id === 'verify')).toBe(false)
    const ordinary = setVerificationLoop(five, false)
    expect(verifyLoop(ordinary)).toBeUndefined()
    expect(ordinary.nodes.find(node => node.id === 'code')?.inputs?.review).toBeUndefined()
    expect(ordinary.nodes.find(node => node.id === 'verify')?.inputs?.previous.value.iteration).toBeUndefined()
    expect(verifyLoop(setVerificationLoop(ordinary, true, 3))?.rounds).toBe(3)
  })

  it('keeps result classification contracts consistent after an explicit delivery-source change', () => {
    const input = graph()
    const flow = configureWorkflowResultProtocol({ graph_definition: input }, true)
    expect(flow.graph_definition.result_protocol).toBe('workbench_result_v1')
    expect(flow.graph_definition.output_contract.type).toBe('json')
    expect(() => configureWorkflowResultProtocol({ graph_definition: input }, true, input.nodes[0].id)).toThrow('直接连接')
  })
})
