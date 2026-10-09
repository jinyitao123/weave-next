// Graph editing ported from GooeyPi's team workspace; keep the server graph intact.
import { createUUID } from './ids'
import { serialSteps, verifyLoop, withVerifyLoop, type Graph } from './graph'
import type { DevelopmentDocument as TeamDefinition } from './teams'
export type Step = Graph['nodes'][number]
export type Binding = { value: { source: string; node_id?: string; path: string }; expected_type: string }

export const WORKBENCH_RESULT_PROTOCOL = 'workbench_result_v1' as const
export const WORKBENCH_RESULT_SCHEMA = {
  type: 'object',
  additionalProperties: false,
  required: ['disposition', 'summary', 'missing_items'],
  properties: {
    disposition: { type: 'string', enum: ['complete', 'needs_input'] },
    summary: { type: 'string' },
    missing_items: { type: 'array', items: { type: 'string' } },
  },
} as const

type Workflow = { graph_definition: Graph }

function record(value: unknown): Record<string, unknown> | undefined {
  return value && typeof value === 'object' && !Array.isArray(value) ? value as Record<string, unknown> : undefined
}

function stableJSON(value: unknown): string {
  if (Array.isArray(value)) return `[${value.map(stableJSON).join(',')}]`
  const item = record(value)
  if (item) return `{${Object.keys(item).sort().map((key) => `${JSON.stringify(key)}:${stableJSON(item[key])}`).join(',')}}`
  return JSON.stringify(value) ?? 'undefined'
}

function protocolOutput(type: unknown, schema: unknown): boolean {
  return type === 'json' && stableJSON(schema) === stableJSON(WORKBENCH_RESULT_SCHEMA)
}

function isPlainTextOutput(value: unknown): boolean {
  const output = record(value)
  return output?.type === 'text' && output.schema === undefined
}

export function validateWorkflowResultProtocol(flow: Workflow): string | undefined {
  const graph = flow.graph_definition
  if (graph.result_protocol === undefined) return undefined
  if (graph.result_protocol !== WORKBENCH_RESULT_PROTOCOL) return '流程使用了桌面暂不支持的结果分类协议。'
  const outputContract = record(graph.output_contract)
  if (!protocolOutput(outputContract?.type, outputContract?.schema)) return '结果分类与流程输出格式不一致，请重新选择结果分类。'
  const delivery = graph.nodes.find((node) => node.type === 'deliver')
  if (!delivery) return '流程缺少交付步骤。'
  const sourceId = String((delivery?.config?.result as { node_id?: unknown } | undefined)?.node_id ?? '')
  const source = graph.nodes.find((node) => node.id === sourceId)
  if (!source || !['lead', 'worker'].includes(source.type)) {
    return '当前交付来源不能生成结果分类。请在并行汇合后添加负责人或成员汇总步骤，并选择该步骤作为交付来源。'
  }
  const deliveryEdges = graph.edges.filter((edge) => edge.from_node_id === sourceId)
  if (deliveryEdges.length !== 1 || deliveryEdges[0]?.to_node_id !== delivery.id) {
    return '结果分类只能由直接连接交付步骤的负责人或成员生成。请把最终汇总成员设为交付来源；中间步骤仍需保持原有输出格式。'
  }
  const output = record(source.output)
  if (!protocolOutput(output?.type, output?.schema)) return '最终交付成员的输出格式与结果分类不一致，请重新选择结果分类。'
  return undefined
}

/** Applies the optional result protocol and delivery-source selection to one workflow draft. */
export function configureWorkflowResultProtocol(
  flow: Workflow,
  enabled: boolean,
  deliverySourceId?: string,
  previousDeliverySourceId?: string,
): Workflow {
  const graph = structuredClone(flow.graph_definition)
  if (graph.result_protocol !== undefined && graph.result_protocol !== WORKBENCH_RESULT_PROTOCOL) {
    throw new Error('流程使用了桌面暂不支持的结果分类协议。')
  }
  const wasEnabled = graph.result_protocol === WORKBENCH_RESULT_PROTOCOL
  const delivery = graph.nodes.find((node) => node.type === 'deliver')
  if (!delivery) throw new Error('流程缺少交付步骤。')
  const currentSourceId = String((delivery.config?.result as { node_id?: unknown } | undefined)?.node_id ?? '')
  const previousSourceId = previousDeliverySourceId ?? currentSourceId
  const nextSourceId = deliverySourceId ?? currentSourceId
  const nextSource = graph.nodes.find((node) => node.id === nextSourceId)
  if (!nextSource || !predecessors(graph, delivery.id).some((node) => node.id === nextSourceId)) {
    throw new Error('交付来源必须是当前流程的前序步骤。')
  }

  if (wasEnabled && (deliverySourceId && deliverySourceId !== currentSourceId || previousDeliverySourceId && previousDeliverySourceId !== currentSourceId)) {
    const outputContract = record(graph.output_contract)
    if (!protocolOutput(outputContract?.type, outputContract?.schema)) throw new Error('当前结果分类格式已被其他配置修改，桌面保留现有格式并停止切换。')
    const previous = graph.nodes.find((node) => node.id === previousSourceId)
    if (previous && ['lead', 'worker'].includes(previous.type)) {
      const previousOutput = record(previous.output)
      if (protocolOutput(previousOutput?.type, previousOutput?.schema)) {
        graph.nodes = graph.nodes.map((node) => node.id === previous.id ? { ...node, output: { type: 'text' } } : node)
      }
    }
  }

  graph.nodes = graph.nodes.map((node) => node.type === 'deliver' ? {
    ...node,
    config: { ...node.config, result: { ...(node.config?.result as object), source: 'node_output', node_id: nextSourceId, path: '' } },
  } : node)

  if (enabled) {
    if (!['lead', 'worker'].includes(nextSource.type)) {
      throw new Error('当前交付来源不能生成结果分类。请在并行汇合后添加负责人或成员汇总步骤，并选择该步骤作为交付来源。')
    }
    const outputContract = record(graph.output_contract)
    if (!wasEnabled && !isPlainTextOutput(graph.output_contract)) {
      throw new Error('当前流程已有其他输出格式，桌面不会覆盖它；请先恢复普通文本输出再启用结果分类。')
    }
    const sourceOutput = nextSource.output
    const sourceOutputIsProtocol = protocolOutput(record(sourceOutput)?.type, record(sourceOutput)?.schema)
    if (!wasEnabled && sourceOutput !== undefined && !isPlainTextOutput(sourceOutput)
      || wasEnabled && nextSourceId !== previousSourceId && sourceOutputIsProtocol
      || wasEnabled && nextSourceId === previousSourceId && sourceOutput !== undefined && !isPlainTextOutput(sourceOutput) && !sourceOutputIsProtocol) {
      throw new Error('当前交付成员已有其他输出格式，桌面不会覆盖它；请先恢复普通文本输出再启用结果分类。')
    }
    if (wasEnabled && outputContract && !protocolOutput(outputContract.type, outputContract.schema)) {
      throw new Error('结果分类与流程输出格式不一致，请重新选择结果分类。')
    }
    graph.result_protocol = WORKBENCH_RESULT_PROTOCOL
    graph.output_contract = { type: 'json', schema: WORKBENCH_RESULT_SCHEMA }
    graph.nodes = graph.nodes.map((node) => {
      if (node.id === nextSourceId) return { ...node, output: { type: 'json', schema: WORKBENCH_RESULT_SCHEMA } }
      if (wasEnabled && record(node.output) && protocolOutput(record(node.output)?.type, record(node.output)?.schema)) return { ...node, output: { type: 'text' } }
      return node
    })
    const nextFlow = { ...flow, graph_definition: graph }
    const issue = validateWorkflowResultProtocol(nextFlow)
    if (issue) throw new Error(issue)
    return nextFlow
  }

  if (wasEnabled) {
    const outputContract = record(graph.output_contract)
    if (!protocolOutput(outputContract?.type, outputContract?.schema)) {
      throw new Error('当前结果分类格式已被其他配置修改，桌面保留现有格式并停止切换。')
    }
    const source = graph.nodes.find((node) => node.id === currentSourceId)
    if (source && ['lead', 'worker'].includes(source.type) && source.output !== undefined
      && !isPlainTextOutput(source.output) && !protocolOutput(record(source.output)?.type, record(source.output)?.schema)) {
      throw new Error('当前交付成员已有其他输出格式，桌面保留现有格式并停止切换。')
    }
    delete graph.result_protocol
    graph.output_contract = { type: 'text' }
    const sourceOutput = record(source?.output)
    if (source && ['lead', 'worker'].includes(source.type) && protocolOutput(sourceOutput?.type, sourceOutput?.schema)) {
      graph.nodes = graph.nodes.map((node) => node.id === source.id ? { ...node, output: { type: 'text' } } : node)
    }
  }
  return { ...flow, graph_definition: graph }
}

export function bindings(step: Step): Record<string, Binding> { return (step.inputs ?? {}) as Record<string, Binding> }
export function originalBinding(): Binding { return { value: { source: 'run_input', path: '' }, expected_type: 'text' } }
export function predecessors(graph: Graph, id: string): Step[] {
  const found = new Set<string>()
  const visit = (to: string) => { for (const edge of graph.edges.filter((e) => e.to_node_id === to && e.route !== 'back')) { if (!found.has(edge.from_node_id)) { found.add(edge.from_node_id); visit(edge.from_node_id) } } }
  visit(id)
  return graph.nodes.filter((n) => found.has(n.id) && n.id !== id && ['lead', 'worker', 'join'].includes(n.type))
}
export function isParallelBranchWorker(graph: Graph, id: string): boolean {
  const incoming = graph.edges.filter((edge) => edge.to_node_id === id)
  const outgoing = graph.edges.filter((edge) => edge.from_node_id === id)
  return incoming.length === 1 && incoming[0].route === 'branch' && outgoing.length === 1 && outgoing[0].route === 'join'
}
export function canInsertSerialStep(graph: Graph, id: string): boolean {
  const node = graph.nodes.find((item) => item.id === id)
  if (node?.type === 'parallel') {
    const join = String(node.config?.join_node_id ?? '')
    return graph.nodes.some((item) => item.id === join && item.type === 'join')
  }
  if (!node || !['lead', 'worker', 'join'].includes(node.type) || isParallelBranchWorker(graph, id)) return false
  const outgoing = graph.edges.filter((edge) => edge.from_node_id === id)
  return outgoing.length === 1 && outgoing[0].route === 'success'
}
export function initialGraph(member: TeamDefinition['members'][number]): Graph {
  return { schema_version: 1, entry_node_id: 'lead', input_contract: { type: 'text' }, output_contract: { type: 'text' }, nodes: [
    { id: 'lead', type: 'lead', label: '理解任务', config: { instruction: '理解任务输入与团队目标，明确分工和输出要求。' }, inputs: { original: originalBinding() }, output: { type: 'text' } },
    { id: 'work', type: 'worker', label: member.configuration.display_name, config: { kind: 'consult', agent_id: member.id, agent_version: 1, result_requirement: member.relationship.result_requirement || '按照成员职责处理任务输入，返回结果和依据。' }, inputs: { original: originalBinding(), brief: { value: { source: 'node_output', node_id: 'lead', path: '' }, expected_type: 'text' } }, output: { type: 'text' } },
    { id: 'deliver', type: 'deliver', label: '交付结果', config: { result: { source: 'node_output', node_id: 'work', path: '' } } },
  ], edges: [{ id: 'a', from_node_id: 'lead', to_node_id: 'work', route: 'success' }, { id: 'b', from_node_id: 'work', to_node_id: 'deliver', route: 'success' }] }
}

function edge(from: string, to: string, route = 'success') { return { id: createUUID(), from_node_id: from, to_node_id: to, route } }
function memberStep(member: TeamDefinition['members'][number], previous?: Step, workerKind: 'consult' | 'dispatch' = 'consult'): Step {
  const common = { id: `step-${createUUID()}`, label: member.configuration.display_name, inputs: { original: originalBinding(), ...(previous && ['lead', 'worker', 'join'].includes(previous.type) ? { previous: { value: { source: 'node_output', node_id: previous.id, path: '' }, expected_type: previous.type === 'join' ? 'json' : 'text' } } : {}) }, output: { type: 'text' } }
  if (member.configuration.role === 'avatar') return { ...common, type: 'lead', config: { instruction: member.configuration.system_prompt || member.relationship.result_requirement || '汇总前序结果，按团队交付要求形成最终结果。' } }
  return { ...common, type: 'worker', config: { kind: workerKind, agent_id: member.id, agent_version: 1, result_requirement: member.relationship.result_requirement || '按照成员职责处理任务输入，返回结果与依据。' } }
}
export function insertStep(graph: Graph, id: string, member: TeamDefinition['members'][number]): { graph: Graph; selected: string } {
  const previous = graph.nodes.find((n) => n.id === id)
  if (previous?.type === 'parallel') {
    const join = String(previous.config?.join_node_id ?? '')
    if (!graph.nodes.some((n) => n.id === join && n.type === 'join')) throw new Error('并行区域缺少汇合步骤')
    return insertStep(graph, join, member)
  }
  if (isParallelBranchWorker(graph, id)) throw new Error('并行分支由单个成员直接进入汇合；请选中“并行分工”，在汇合后添加串行步骤')
  const outgoing = graph.edges.filter((e) => e.from_node_id === id)
  if (!previous || !['lead', 'worker', 'join'].includes(previous.type) || outgoing.length !== 1 || outgoing[0].route !== 'success') throw new Error('请选择连接完整的步骤后添加下一步')
  const next = memberStep(member, previous), after = outgoing[0].to_node_id
  const nextRoute = outgoing[0].route
  return { selected: next.id, graph: { ...graph, nodes: [...graph.nodes.map((n) => n.id === after && n.type === 'deliver' ? { ...n, config: { ...n.config, result: { ...(n.config?.result as object), source: 'node_output', node_id: next.id, path: '' } } } : n), next], edges: [...graph.edges.filter((e) => e !== outgoing[0]), edge(id, next.id), { ...outgoing[0], from_node_id: next.id, route: nextRoute }] } }
}
export function addParallelBranch(graph: Graph, id: string, member: TeamDefinition['members'][number]): { graph: Graph; selected: string } {
  if (member.configuration.role !== 'worker' || !member.relationship.enabled) throw new Error('并行分支只能由已启用的执行成员负责')
  const node = graph.nodes.find((n) => n.id === id)
  if (node?.type === 'parallel') {
    const next = memberStep(member, undefined, 'dispatch'), join = String(node.config?.join_node_id ?? '')
    if (!graph.nodes.some((n) => n.id === join && n.type === 'join')) throw new Error('并行区域缺少汇合步骤')
    return { selected: next.id, graph: { ...graph, nodes: [...graph.nodes, next], edges: [...graph.edges, edge(id, next.id, 'branch'), edge(next.id, join, 'join')] } }
  }
  if (node?.type !== 'worker') throw new Error('请选择执行步骤，将它扩展为并行分工')
  const incoming = graph.edges.filter((e) => e.to_node_id === id), outgoing = graph.edges.filter((e) => e.from_node_id === id)
  if (incoming.length !== 1 || outgoing.length !== 1 || !['success', 'branch'].includes(incoming[0].route) || !['success', 'join'].includes(outgoing[0].route)) throw new Error('该步骤连接不完整，请先调整流程')
  if (incoming[0].route === 'branch') return addParallelBranch(graph, incoming[0].from_node_id, member)
  const parallel = `parallel-${createUUID()}`, join = `join-${createUUID()}`, next = memberStep(member, graph.nodes.find((n) => n.id === incoming[0].from_node_id), 'dispatch')
  const existingDispatch = { ...node, config: { ...node.config, kind: 'dispatch' } }
  const nodes: Step[] = [...graph.nodes.map((item) => item.id === node.id ? existingDispatch : item), next, { id: parallel, type: 'parallel', label: '并行分工', config: { join_node_id: join } }, { id: join, type: 'join', label: '汇总分支', config: { policy: 'all_success' } }]
  // Existing consumers of this worker remain bound to it. A finalizer may also
  // select the new join result explicitly, instead of silently changing types.
  return { selected: next.id, graph: { ...graph, nodes, edges: [...graph.edges.filter((e) => e !== incoming[0] && e !== outgoing[0]), { ...incoming[0], to_node_id: parallel }, edge(parallel, id, 'branch'), edge(parallel, next.id, 'branch'), edge(id, join, 'join'), edge(next.id, join, 'join'), { ...outgoing[0], from_node_id: join }] } }
}
export function removeStep(graph: Graph, id: string): Graph {
  const node = graph.nodes.find((n) => n.id === id), incoming = graph.edges.filter((e) => e.to_node_id === id), outgoing = graph.edges.filter((e) => e.from_node_id === id)
  if (!node || !['worker', 'lead'].includes(node.type) || id === graph.entry_node_id || incoming.length !== 1 || outgoing.length !== 1) throw new Error('请选择连接完整的执行步骤')
  const dependent = graph.nodes.filter((n) => n.id !== id && (Object.values(bindings(n)).some((b) => b.value?.node_id === id) || (incoming[0].route === 'branch' && (n.config?.result as { node_id?: string })?.node_id === id)))
  if (dependent.length) throw new Error(`请先调整“${dependent.map((n) => n.label || '执行步骤').join('、')}”的输入来源`)
  if (incoming[0].route === 'branch') {
    if (node.type !== 'worker') throw new Error('并行分支只能移除执行成员')
    const siblings = graph.edges.filter((e) => e.from_node_id === incoming[0].from_node_id)
    if (siblings.length <= 2) throw new Error('并行区域至少保留两个分支；可先将并行区域改为串行')
    const join = graph.nodes.find((n) => n.id === outgoing[0].to_node_id)
    if (Number(join?.config?.success_count ?? 0) > siblings.length - 1) throw new Error('请先降低汇合所需的成功分支数')
    return { ...graph, nodes: graph.nodes.filter((n) => n.id !== id), edges: graph.edges.filter((e) => e.from_node_id !== id && e.to_node_id !== id) }
  }
  const previous = incoming[0].from_node_id
  if (incoming[0].route !== 'success' || outgoing[0].route !== 'success') throw new Error('循环或条件连接中的步骤不能直接删除')
  return { ...graph, nodes: graph.nodes.filter((n) => n.id !== id).map((n) => n.type === 'deliver' && (n.config?.result as { node_id?: string })?.node_id === id ? { ...n, config: { ...n.config, result: { ...(n.config?.result as object), source: 'node_output', node_id: previous, path: '' } } } : n), edges: [...graph.edges.filter((e) => e.from_node_id !== id && e.to_node_id !== id), { ...outgoing[0], from_node_id: previous }] }
}
export function serializeParallel(graph: Graph, id: string): Graph {
  const parallel = graph.nodes.find((n) => n.id === id), join = String(parallel?.config?.join_node_id ?? '')
  const branches = graph.edges.filter((e) => e.from_node_id === id).map((e) => e.to_node_id)
  const incoming = graph.edges.filter((e) => e.to_node_id === id), outgoing = graph.edges.filter((e) => e.from_node_id === join)
  if (parallel?.type !== 'parallel' || incoming.length !== 1 || outgoing.length !== 1 || !branches.length) throw new Error('并行区域连接不完整')
  if (graph.nodes.some((n) => Object.values(bindings(n)).some((b) => b.value?.node_id === join) || (n.config?.result as { node_id?: string })?.node_id === join)) throw new Error('请先将汇总结果的使用者改为读取各成员结果，再改为串行')
  const removed = new Set([id, join])
  const branchIds = new Set(branches)
  return { ...graph, nodes: graph.nodes.filter((n) => !removed.has(n.id)).map((node) => branchIds.has(node.id) && node.type === 'worker' ? { ...node, config: { ...node.config, kind: 'consult' } } : node), edges: [...graph.edges.filter((e) => !removed.has(e.from_node_id) && !removed.has(e.to_node_id)), { ...incoming[0], to_node_id: branches[0] }, ...branches.slice(1).map((to, i) => edge(branches[i], to)), { ...outgoing[0], from_node_id: branches[branches.length - 1] }] }
}


/** Executor edits replace only the known executor fields, never opaque config. */
export function assignExecutor(graph: Graph, id: string, member: TeamDefinition['members'][number]): Graph {
  const step = graph.nodes.find(node => node.id === id)
  if (!step || !member.relationship.enabled || !['lead', 'worker'].includes(step.type)) throw new Error('请先选择可用的执行成员')
  if (isParallelBranchWorker(graph, id) && member.configuration.role !== 'worker') throw new Error('并行分支只能由执行成员负责')
  const config = { ...step.config }
  if (member.configuration.role === 'avatar') {
    const instruction = String(config.instruction ?? config.result_requirement ?? '')
    for (const key of ['kind', 'agent_id', 'agent_version', 'result_requirement']) delete config[key]
    config.instruction = instruction
  } else {
    const requirement = String(config.result_requirement ?? config.instruction ?? '')
    delete config.instruction
    Object.assign(config, { kind: isParallelBranchWorker(graph, id) ? 'dispatch' : 'consult', agent_id: member.id, agent_version: 1, result_requirement: requirement })
  }
  return { ...graph, nodes: graph.nodes.map(node => node.id === id ? { ...node, type: member.configuration.role === 'avatar' ? 'lead' : 'worker', config } : node) }
}

/** Keep the existing two-step machine fixture while retaining opaque fields. */
export function setVerificationLoop(graph: Graph, enabled: boolean, rounds = 3): Graph {
  const existing = verifyLoop(graph)
  if (enabled) {
    if (existing) return withVerifyLoop(graph, rounds)
    const serial = serialSteps(graph)
    if (!serial.serial || serial.steps.length !== 2 || serial.steps.some(step => step.type !== 'worker')) throw new Error('只有两步成员流程可以设置验证退回')
    if (graph.result_protocol) throw new Error('请先保留当前交付协议；该流程不能直接改为验证回路')
    const changed = withVerifyLoop(graph, rounds)
    changed.nodes = changed.nodes.map(node => {
      const original = graph.nodes.find(previous => previous.id === node.id)
      if (!original) return node
      const inputs = { ...original.inputs }
      for (const [name, binding] of Object.entries(node.inputs ?? {})) inputs[name] = { ...inputs[name], ...binding, value: { ...inputs[name]?.value, ...binding.value } }
      return { ...original, ...node, config: { ...original.config, ...node.config }, ...(node.output ? { output: { ...original.output, ...node.output } } : {}), ...(node.inputs ? { inputs } : {}) }
    })
    const previousDelivery = graph.nodes.find(node => node.type === 'deliver')
    if (previousDelivery) {
      changed.nodes = changed.nodes.map(node => node.type === 'deliver' ? { ...previousDelivery, ...node, id: previousDelivery.id, config: { ...previousDelivery.config, ...node.config, result: { ...(previousDelivery.config?.result as object), ...(node.config?.result as object) } } } : node)
      changed.edges = changed.edges.map(item => item.to_node_id === 'deliver' ? { ...item, to_node_id: previousDelivery.id } : item)
    }
    return changed
  }
  if (!existing) return graph
  const [code, verify] = existing.steps
  const delivery = graph.nodes.find(node => node.type === 'deliver')!
  const codeInputs = { ...code.inputs }; delete codeInputs.review
  const verifyInputs = { ...verify.inputs }
  if (verifyInputs.previous) {
    const value = { ...verifyInputs.previous.value }; delete value.iteration
    verifyInputs.previous = { ...verifyInputs.previous, value }
  }
  const forward = graph.edges.find(item => item.from_node_id === code.id && item.to_node_id === verify.id)!
  const exit = graph.edges.find(item => item.from_node_id === existing.loopId && item.route === 'exit')!
  return { ...graph, entry_node_id: code.id,
    nodes: [{ ...code, inputs: codeInputs }, { ...verify, inputs: verifyInputs, output: { type: 'text' } },
      { ...delivery, config: { ...delivery.config, result: { ...(delivery.config?.result as object), source: 'node_output', node_id: verify.id, path: '' } } }],
    edges: [forward, { ...exit, from_node_id: verify.id, to_node_id: delivery.id, route: 'success' }],
  }
}
