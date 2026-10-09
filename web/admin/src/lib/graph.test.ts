import { describe, expect, it } from 'vitest'
import fixtureText from '../../../../internal/kernel/workflow/machine/testdata/code_verify_loop.json?raw'
import { serialGraph, verifyLoop, withoutVerifyLoop, withVerifyLoop, type Graph } from './graph'

// The Go workflow validator accepts this fixture; the console must build the
// same shape.
const fixture = JSON.parse(fixtureText) as Graph

function normalized(graph: Graph) {
  const loop = verifyLoop(graph)!
  const names = new Map([[loop.loopId, 'loop'], [loop.steps[0].id, 'code'], [loop.steps[1].id, 'verify'], ['deliver', 'deliver']])
  const text = JSON.stringify(graph, (_key, value) => typeof value === 'string' && names.has(value) ? names.get(value) : value)
  const parsed = JSON.parse(text) as Graph
  return {
    entry: parsed.entry_node_id,
    nodes: parsed.nodes.map((node) => ({ id: node.id, type: node.type, inputs: node.inputs, output: node.output, loop: node.type === 'loop' ? node.config : undefined, deliver: node.type === 'deliver' ? node.config : undefined })),
    edges: parsed.edges.map((edge) => `${edge.from_node_id}-${edge.route}-${edge.to_node_id}`).sort(),
  }
}

describe('verify loop', () => {
  const serial = serialGraph([
    { member: { id: 'coder', displayName: '编码' }, label: '编码', requirement: '完成代码修改' },
    { member: { id: 'verifier', displayName: '验证' }, label: '验证', requirement: '独立验证' },
  ])

  it('matches the validated machine graph', () => {
    const looped = withVerifyLoop(serial, 3)
    expect(verifyLoop(looped)?.rounds).toBe(3)
    expect(normalized(looped)).toEqual(normalized(fixture))
  })

  it('changes rounds in place and turns back into a serial flow', () => {
    const looped = withVerifyLoop(serial, 3)
    const changed = withVerifyLoop(looped, 5)
    expect(verifyLoop(changed)?.rounds).toBe(5)
    expect(changed.nodes.map((node) => node.id)).toEqual(looped.nodes.map((node) => node.id))
    const back = withoutVerifyLoop(changed)
    expect(verifyLoop(back)).toBeUndefined()
    expect(back.nodes.filter((node) => node.type === 'worker').map((node) => node.config?.agent_id)).toEqual(['coder', 'verifier'])
  })
})
