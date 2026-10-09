// Serial workflow editing over Weave's graph definition. Ported from the
// GooeyPi desktop team workspace (weave-workbench desktop/src/pages/
// team-workspace/graph.ts, MIT) so both clients produce the same graphs.

export interface Binding { value: { source: string; node_id?: string; path: string; iteration?: string; default?: { source: string; value: unknown } }; expected_type: string }

export interface Step {
  id: string
  type: string
  label?: string
  config?: Record<string, unknown>
  inputs?: Record<string, Binding>
  output?: Record<string, unknown>
}

export interface GraphEdge { id: string; from_node_id: string; to_node_id: string; route: string }

export interface Graph {
  schema_version: number
  entry_node_id: string
  input_contract: Record<string, unknown>
  output_contract: Record<string, unknown>
  nodes: Step[]
  edges: GraphEdge[]
  [key: string]: unknown
}

export interface StepMember { id: string; displayName: string; resultRequirement?: string }

export const originalBinding = (): Binding => ({ value: { source: 'run_input', path: '' }, expected_type: 'text' })

const edge = (from: string, to: string, route = 'success'): GraphEdge => ({ id: crypto.randomUUID(), from_node_id: from, to_node_id: to, route })

function workerStep(member: StepMember, previous?: Step, requirement?: string): Step {
  return {
    id: `step-${crypto.randomUUID()}`,
    type: 'worker',
    label: member.displayName,
    config: { kind: 'consult', agent_id: member.id, agent_version: 1, result_requirement: requirement || member.resultRequirement || '按照成员职责处理任务输入，返回结果与依据。' },
    inputs: {
      original: originalBinding(),
      ...(previous && ['lead', 'worker', 'join'].includes(previous.type)
        ? { previous: { value: { source: 'node_output', node_id: previous.id, path: '' }, expected_type: previous.type === 'join' ? 'json' : 'text' } }
        : {}),
    },
    output: { type: 'text' },
  }
}

// A serial chain of members ending in delivery of the last step's result.
export function serialGraph(steps: Array<{ member: StepMember; label: string; requirement: string }>): Graph {
  const nodes: Step[] = []
  const edges: GraphEdge[] = []
  for (const item of steps) {
    const step = { ...workerStep(item.member, nodes.at(-1), item.requirement), label: item.label }
    if (nodes.length) edges.push(edge(nodes[nodes.length - 1].id, step.id))
    nodes.push(step)
  }
  const deliver: Step = { id: 'deliver', type: 'deliver', label: '交付结果', config: { result: { source: 'node_output', node_id: nodes[nodes.length - 1].id, path: '' } } }
  edges.push(edge(nodes[nodes.length - 1].id, deliver.id))
  return { schema_version: 1, entry_node_id: nodes[0].id, input_contract: { type: 'text' }, output_contract: { type: 'text' }, nodes: [...nodes, deliver], edges }
}

// Ordered steps from the entry node following single success edges; parallel
// regions are reported as unsupported so the console never rewrites them.
export function serialSteps(graph: Graph): { steps: Step[]; serial: boolean } {
  const steps: Step[] = []
  const seen = new Set<string>()
  let current = graph.nodes.find((node) => node.id === graph.entry_node_id)
  while (current && !seen.has(current.id)) {
    seen.add(current.id)
    if (current.type === 'deliver') return { steps, serial: true }
    if (!['lead', 'worker'].includes(current.type)) return { steps, serial: false }
    steps.push(current)
    const outgoing = graph.edges.filter((item) => item.from_node_id === current!.id)
    if (outgoing.length !== 1) return { steps, serial: false }
    current = graph.nodes.find((node) => node.id === outgoing[0].to_node_id)
  }
  return { steps, serial: false }
}

export function insertStepAfter(graph: Graph, id: string, member: StepMember): Graph {
  const previous = graph.nodes.find((node) => node.id === id)
  const outgoing = graph.edges.filter((item) => item.from_node_id === id)
  if (!previous || !['lead', 'worker'].includes(previous.type) || outgoing.length !== 1) throw new Error('请选择连接完整的步骤后添加下一步')
  const next = workerStep(member, previous)
  const after = outgoing[0].to_node_id
  return {
    ...graph,
    nodes: [...graph.nodes.map((node) => node.id === after && node.type === 'deliver' ? { ...node, config: { ...node.config, result: { source: 'node_output', node_id: next.id, path: '' } } } : node), next],
    edges: [...graph.edges.filter((item) => item !== outgoing[0]), edge(id, next.id), edge(next.id, after)],
  }
}

export function removeStep(graph: Graph, id: string): Graph {
  const node = graph.nodes.find((item) => item.id === id)
  const incoming = graph.edges.filter((item) => item.to_node_id === id)
  const outgoing = graph.edges.filter((item) => item.from_node_id === id)
  if (!node || !['worker', 'lead'].includes(node.type) || id === graph.entry_node_id || incoming.length !== 1 || outgoing.length !== 1) throw new Error('第一个步骤和连接不完整的步骤不能删除')
  const dependent = graph.nodes.filter((item) => item.id !== id && item.id !== outgoing[0].to_node_id && Object.values(item.inputs ?? {}).some((binding) => binding.value?.node_id === id))
  if (dependent.length) throw new Error(`请先调整“${dependent.map((item) => item.label || '执行步骤').join('、')}”的输入来源`)
  const previous = incoming[0].from_node_id
  const nodes = graph.nodes.filter((item) => item.id !== id).map((item) => {
    if (item.type === 'deliver' && (item.config?.result as { node_id?: string } | undefined)?.node_id === id) return { ...item, config: { ...item.config, result: { source: 'node_output', node_id: previous, path: '' } } }
    if (item.id === outgoing[0].to_node_id && item.inputs?.previous?.value.node_id === id) return { ...item, inputs: { ...item.inputs, previous: { ...item.inputs.previous, value: { ...item.inputs.previous.value, node_id: previous } } } }
    return item
  })
  return { ...graph, nodes, edges: [...graph.edges.filter((item) => item.from_node_id !== id && item.to_node_id !== id), edge(previous, outgoing[0].to_node_id)] }
}

export function updateStep(graph: Graph, id: string, patch: { label?: string; agentId?: string; requirement?: string }): Graph {
  return {
    ...graph,
    nodes: graph.nodes.map((node) => node.id !== id ? node : {
      ...node,
      ...(patch.label !== undefined ? { label: patch.label } : {}),
      config: {
        ...node.config,
        ...(patch.agentId !== undefined ? { agent_id: patch.agentId } : {}),
        ...(patch.requirement !== undefined ? { result_requirement: patch.requirement } : {}),
      },
    }),
  }
}

// A two-step verify loop: the second step judges the first and, while it
// reports passed=false, the work goes back to the first step, at most `rounds`
// times. Built from the workflow machine's own bounded loop node.
const verdictSchema = {
  type: 'object', additionalProperties: false, required: ['passed', 'summary'],
  properties: { passed: { type: 'boolean' }, summary: { type: 'string' } },
}
export const maxVerifyRounds = 5

export interface VerifyLoop { loopId: string; steps: [Step, Step]; rounds: number }

export function verifyLoop(graph: Graph): VerifyLoop | undefined {
  const loop = graph.nodes.find((node) => node.id === graph.entry_node_id)
  if (loop?.type !== 'loop') return undefined
  const out = (id: string, route: string) => graph.edges.filter((item) => item.from_node_id === id && item.route === route)
  const body = out(loop.id, 'body')
  const first = graph.nodes.find((node) => node.id === body[0]?.to_node_id)
  const next = first ? out(first.id, 'success') : []
  const second = graph.nodes.find((node) => node.id === next[0]?.to_node_id)
  const config = loop.config as { latch_node_id?: string; max_iterations?: number } | undefined
  if (body.length !== 1 || next.length !== 1 || first?.type !== 'worker' || second?.type !== 'worker' || config?.latch_node_id !== second.id) return undefined
  if (out(second.id, 'back')[0]?.to_node_id !== loop.id || graph.nodes.length !== 4 || graph.edges.length !== 4) return undefined
  return { loopId: loop.id, steps: [first, second], rounds: Number(config.max_iterations) || 1 }
}

export function withVerifyLoop(graph: Graph, rounds: number): Graph {
  const existing = verifyLoop(graph)
  if (existing) return { ...graph, nodes: graph.nodes.map((node) => node.id === existing.loopId ? { ...node, config: { ...node.config, max_iterations: rounds } } : node) }
  const { steps, serial } = serialSteps(graph)
  if (!serial || steps.length !== 2 || steps.some((step) => step.type !== 'worker')) throw new Error('只有两步流程可以设置验证退回')
  const [code, verify] = steps
  const loop: Step = {
    id: `loop-${crypto.randomUUID()}`, type: 'loop', label: '验证未通过时退回',
    config: {
      max_iterations: rounds, latch_node_id: verify.id,
      continue_predicate: { left: { source: 'node_output', node_id: verify.id, path: '/passed', iteration: 'current_iteration' }, operator: 'eq', right: { source: 'literal', value: false } },
    },
  }
  const reworked: Step = { ...code, inputs: { original: originalBinding(), review: { value: { source: 'node_output', node_id: verify.id, path: '/summary', iteration: 'previous_iteration', default: { source: 'literal', value: '' } }, expected_type: 'text' } as Binding } }
  const judged: Step = { ...verify, inputs: { original: originalBinding(), previous: { value: { source: 'node_output', node_id: code.id, path: '', iteration: 'current_iteration' }, expected_type: 'text' } as Binding }, output: { type: 'json', schema: verdictSchema } }
  const deliver: Step = { id: 'deliver', type: 'deliver', label: '交付结果', config: { result: { source: 'node_output', node_id: loop.id, path: '/latch_result/summary' } } }
  return {
    ...graph, entry_node_id: loop.id,
    nodes: [loop, reworked, judged, deliver],
    edges: [edge(loop.id, code.id, 'body'), edge(code.id, verify.id), edge(verify.id, loop.id, 'back'), edge(loop.id, deliver.id, 'exit')],
  }
}

export function withoutVerifyLoop(graph: Graph): Graph {
  const existing = verifyLoop(graph)
  if (!existing) return graph
  const [code, verify] = existing.steps
  const asStep = (step: Step) => ({ member: { id: String(step.config?.agent_id ?? ''), displayName: step.label ?? '' }, label: step.label ?? '', requirement: String(step.config?.result_requirement ?? '') })
  return { ...graph, ...serialGraph([asStep(code), asStep(verify)]) }
}
