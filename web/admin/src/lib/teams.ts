import { createUUID } from './ids'
import { api } from './api'
import { engineName } from './format'
import { serialGraph, withVerifyLoop, type Graph } from './graph'

export interface TeamRecord {
  id: string
  name: string
  display_name?: string
  objective?: string
  status: string
  default_workflow_id?: string
  updated_at: string
}

export interface MemberSkill { name: string; description: string; body: string; always_active: boolean }
export interface CapabilityBinding { capability_id: string; parameters: Array<{ name: string; source: string }> }
export interface ToolLoopControl { slice_rounds: number; initial_total_rounds: number }

// Only the fields the console edits are typed; anything else the server sent is
// carried through a save unchanged.
export interface MemberConfiguration {
  display_name: string
  role: string
  engine: string
  runtime_id: string
  model: string
  system_prompt: string
  skills?: MemberSkill[] | null
  business_capability_ids?: string[] | null
  business_capability_bindings?: CapabilityBinding[] | null
  tool_loop_control?: ToolLoopControl | null
  output_schema?: unknown
  permission_deny?: string[] | null
  memory_enabled?: boolean
  memory_scope?: string
  max_tokens?: number
  max_output_tokens?: number
  step_budget?: number
  max_cost_usd?: number
  max_tool_repeats?: number
  [key: string]: unknown
}

export interface MemberRelationship {
  duty: string
  when_to_use?: string
  context_instruction?: string
  allowed_kinds?: string[] | null
  default_kind?: string
  result_requirement: string
  enabled: boolean
  [key: string]: unknown
}

export interface DevelopmentMember {
  id: string
  configuration: MemberConfiguration
  relationship: MemberRelationship
}

export interface DevelopmentWorkflow {
  id: string
  name: string
  description: string
  graph_definition: Graph
  trigger_config: Record<string, unknown>
}

export interface DevelopmentDocument {
  name: string
  objective: string
  audience: string[]
  members: DevelopmentMember[]
  workflows: DevelopmentWorkflow[]
}

export interface DevelopmentTrial { request_id: string; revision: number; workflow_id: string; run_id: string; status: string; created_at: string }

export interface DevelopmentDraft {
  revision: number
  published_revision: number
  document: DevelopmentDocument
  published_document?: DevelopmentDocument
  updated_at: string
  trials: DevelopmentTrial[]
  publication_readiness?: { ready: boolean }
}

export const listTeamRecords = () => api<TeamRecord[]>('/v1/teams?status=all&purpose=development')
  .then((teams) => teams.filter((team) => team.status !== 'archived'))

const base = (teamId: string) => `/v1/teams/${encodeURIComponent(teamId)}/development`

export const readDevelopment = (teamId: string) => api<DevelopmentDraft>(base(teamId))
export const saveDevelopment = (teamId: string, revision: number, document: DevelopmentDocument) =>
  api<DevelopmentDraft>(base(teamId), { method: 'PUT', body: JSON.stringify({ expected_revision: revision, document }) })
export const publishDevelopment = (teamId: string, revision: number) =>
  api<DevelopmentDraft>(`${base(teamId)}/publish`, { method: 'POST', body: JSON.stringify({ revision }) })
export const startTrial = (teamId: string, revision: number, workflowId: string, input: string, requestId: string) =>
  api<{ run_id: string }>(`${base(teamId)}/trials`, { method: 'POST', body: JSON.stringify({ revision, workflow_id: workflowId, request_id: requestId, input, business_actions: [] }) })

interface CreatedAgent { id: string }

async function createAgent(name: string, displayName: string, role: 'avatar' | 'worker', engine: string, prompt: string): Promise<CreatedAgent> {
  return api<CreatedAgent>('/v1/agents', {
    method: 'POST',
    body: JSON.stringify({ name, display_name: displayName, role, engine, graph_type: 'standard', spec: { system_prompt: prompt } }),
  })
}

export const starterMembers = [
  { key: 'code', displayName: '编码', engine: 'claude', prompt: '分析需求、设计方案并修改代码。收到验证意见时先逐条修复。说明改了什么、为什么这样改，以及还没完成的部分。', requirement: '完成代码修改，说明改动内容、依据和未完成项。' },
  { key: 'verify', displayName: '验证', engine: 'codex', prompt: '独立验证上一步的代码：运行测试和检查，逐条记录执行的命令与结果，不根据描述推断结果。', requirement: '列出执行的检查命令和真实结果。全部通过时 passed 为 true；否则为 false，并在 summary 中写明未通过项和原因。' },
] as const

// Creates a team pre-filled with a coding member and an independent verifier,
// using the same agent and team endpoints as the desktop. Agents created before
// a failure are removed again.
export async function createStarterTeam(name: string, objective: string): Promise<string> {
  const suffix = createUUID()
  const created: string[] = []
  try {
    const lead = await createAgent(`team-${suffix}-lead`, '团队负责人', 'avatar', 'loom', `负责理解“${name}”的目标并汇总可核验结果。`)
    created.push(`team-${suffix}-lead`)
    const workers: Array<{ id: string; requirement: string; duty: string }> = []
    for (const member of starterMembers) {
      const agentName = `team-${suffix}-${member.key}`
      const agent = await createAgent(agentName, member.displayName, 'worker', member.engine, member.prompt)
      created.push(agentName)
      workers.push({ id: agent.id, requirement: member.requirement, duty: member.prompt })
    }
    const team = await api<{ id: string }>('/v1/teams', {
      method: 'POST',
      body: JSON.stringify({
        name: `team-${suffix}`, display_name: name, objective, primary_scenario: objective,
        success_criteria: '交付经过独立验证的代码改动', lead_avatar_id: lead.id,
        workers: workers.map((worker) => ({
          worker_agent_id: worker.id, duty: worker.duty, when_to_use: '负责人分配相关工作时', context_instruction: '保留任务上下文、代码版本和材料来源。',
          allowed_kinds: ['consult', 'dispatch', 'handoff'], default_kind: 'dispatch', result_requirement: worker.requirement,
        })),
      }),
    })
    return team.id
  } catch (error) {
    await Promise.allSettled(created.map((agentName) => api(`/v1/agents/${encodeURIComponent(agentName)}`, { method: 'DELETE' })))
    throw error
  }
}

// Adds the starter workflow (coding, then independent verification) when the
// draft has no workflow yet.
export function withStarterWorkflow(document: DevelopmentDocument): DevelopmentDocument {
  if (document.workflows.length) return document
  // The lead is required by the team but not used by the serial workflow; it
  // still needs a duty before a trial is accepted.
  document = { ...document, members: document.members.map((member) => member.configuration.role === 'avatar' && !member.relationship.duty?.trim()
    ? { ...member, relationship: { ...member.relationship, duty: '理解任务目标并汇总可核验结果' } }
    : member) }
  const workers = document.members.filter((member) => member.configuration.role === 'worker')
  if (workers.length < 2) return document
  const steps = starterMembers.map((starter, index) => {
    const member = workers.find((item) => item.configuration.display_name === starter.displayName) ?? workers[index]
    return { member: { id: member.id, displayName: member.configuration.display_name }, label: starter.displayName, requirement: starter.requirement }
  })
  return {
    ...document,
    workflows: [{
      id: createUUID(), name: '开发与验证', description: '编码后由另一引擎独立验证，未通过时退回编码',
      trigger_config: { schema_version: 1, type: 'conversation_explicit', config: {} },
      graph_definition: withVerifyLoop(serialGraph(steps), 3),
    }],
  }
}

export const cliEngines = ['claude', 'codex', 'opencode']
export const isCLIEngine = (engine: string) => cliEngines.includes(engine)

export interface NodeChoice { id: string; engines: string[] }

// Pins every CLI member without a node to one that accepts its engine,
// preferring nodes not already serving another engine (a publication freezes
// one engine per node).
export function pinMembers(document: DevelopmentDocument, nodes: NodeChoice[]): DevelopmentDocument {
  const used = new Map<string, string>()
  for (const member of document.members) if (isCLIEngine(member.configuration.engine) && member.configuration.runtime_id) used.set(member.configuration.runtime_id, member.configuration.engine)
  let changed = false
  const members = document.members.map((member) => {
    const config = member.configuration
    if (member.configuration.role !== 'worker' || !isCLIEngine(config.engine) || config.runtime_id) return member
    const candidates = nodes.filter((node) => node.engines.includes(config.engine))
    const choice = candidates.find((node) => !used.has(node.id) || used.get(node.id) === config.engine)
    if (!choice) return member
    used.set(choice.id, config.engine)
    changed = true
    return { ...member, configuration: { ...config, runtime_id: choice.id } }
  })
  return changed ? { ...document, members } : document
}

export const handoffKinds = [
  { value: 'consult', label: '咨询' },
  { value: 'dispatch', label: '派发' },
  { value: 'handoff', label: '交接' },
] as const

export const loadModelCatalog = () => api<{ models: string[] }>('/v1/development/model-catalog').then((catalog) => catalog.models ?? [])

// A Forge business action id looks like forge:action:<object>.<action>; the
// parts are shown as written until a catalog supplies the Chinese name.
export function describeCapability(id: string): { object: string; action: string } {
  const body = id.replace(/^forge:action:/, "")
  const dot = body.indexOf(".")
  return dot < 0 ? { object: "", action: body } : { object: body.slice(0, dot), action: body.slice(dot + 1) }
}

export const audienceLimit = 32

export function normalizeAudience(values: string[]): { value: string[]; error?: string } {
  const value: string[] = []
  for (const raw of values) {
    const item = raw.trim()
    if (!item) continue
    if (item.length > 128 || /[\u0000\r\n]/.test(item)) return { value, error: "权限集名称不能超过 128 个字符，也不能换行" }
    if (value.includes(item)) return { value, error: "权限集名称不能重复" }
    value.push(item)
  }
  if (value.length > audienceLimit) return { value, error: `最多 ${audienceLimit} 个权限集` }
  return { value }
}

// Empty text clears the schema; anything else must be a JSON object.
export function parseOutputSchema(text: string): { value: Record<string, unknown> | null; error?: string } {
  if (!text.trim()) return { value: null }
  try {
    const parsed: unknown = JSON.parse(text)
    if (!parsed || typeof parsed !== "object" || Array.isArray(parsed)) return { value: null, error: "输出结构必须是一个 JSON 对象" }
    return { value: parsed as Record<string, unknown> }
  } catch {
    return { value: null, error: "输出结构不是有效的 JSON" }
  }
}

export const toolLoopLimits = { sliceMax: 1000 }

const listed = (value: unknown) => Array.isArray(value) && value.some((item) => typeof item === 'string' && item.trim())

// What trial preparation would reject for one member, worded for the page.
// Isolated trials refuse MCP servers, library skills and allow/ask tool
// permissions, so a member carrying them cannot be published from here.
export function memberProblems(member: DevelopmentMember): string[] {
  const config = member.configuration, relationship = member.relationship
  const problems: string[] = []
  if (!relationship.duty?.trim()) problems.push('还没有填写职责')
  if (!config.system_prompt?.trim()) problems.push('还没有填写工作方法')
  if (config.role === 'worker' && !isCLIEngine(config.engine) && !config.model?.trim()) problems.push('使用内置引擎时需要选择模型')
  const loopControl = config.tool_loop_control
  if (loopControl && (!(loopControl.slice_rounds >= 1 && loopControl.slice_rounds <= toolLoopLimits.sliceMax) || !(loopControl.initial_total_rounds >= 1))) problems.push("工具循环的每片轮次需在 1 到 1000 之间，总轮次不能小于 1")
  if (config.output_schema != null && (typeof config.output_schema !== "object" || Array.isArray(config.output_schema))) problems.push("输出结构必须是一个 JSON 对象")
  const kinds = relationship.allowed_kinds ?? []
  if (config.role === 'worker' && kinds.length && !kinds.includes(relationship.default_kind ?? '')) problems.push('默认交接方式必须是已选方式之一')
  const skills = config.skills ?? []
  if (skills.some((skill) => !skill.name?.trim() || !skill.body?.trim())) problems.push('技能需要填写名称和内容')
  const names = skills.map((skill) => skill.name?.trim()).filter(Boolean)
  if (new Set(names).size !== names.length) problems.push('技能名称不能重复')
  if (listed(config.mcp_server_ids) || listed(config.skill_names) || listed(config.permission_allow) || listed(config.permission_ask)) {
    problems.push('带有试跑暂不支持的外部工具（MCP 服务、技能库技能或工具许可）')
  }
  return problems
}

export function memberConfigIssues(document: DevelopmentDocument): string[] {
  return document.members.flatMap((member) => memberProblems(member).map((problem) => `“${member.configuration.display_name || '未命名成员'}”${problem}`))
}

// Built-in engines run on the server; only CLI members depend on registered
// nodes. A member with no node and no accepting node gets one combined line.
export function nodeReadinessIssues(document: DevelopmentDocument, accepting: Record<string, number>): string[] {
  const lines: string[] = []
  const covered = new Set<string>()
  for (const member of document.members) {
    const config = member.configuration
    if (config.role !== 'worker' || member.relationship.enabled === false || !isCLIEngine(config.engine) || accepting[config.engine]) continue
    covered.add(`“${config.display_name}”还没有指定节点`)
    lines.push(`“${config.display_name}”使用的 ${engineName(config.engine)} 当前没有可接任务的节点${config.runtime_id ? '' : '，也还没有指定节点'}`)
  }
  return [...lines, ...memberIssues(document).filter((issue) => !covered.has(issue))]
}

export function memberIssues(document: DevelopmentDocument): string[] {
  const issues: string[] = []
  const byNode = new Map<string, DevelopmentMember>()
  for (const member of document.members.filter((item) => item.configuration.role === 'worker')) {
    const config = member.configuration
    if (!isCLIEngine(config.engine)) continue
    if (!config.runtime_id) { issues.push(`“${config.display_name}”还没有指定节点`); continue }
    const other = byNode.get(config.runtime_id)
    if (other && other.configuration.engine !== config.engine) issues.push(`“${other.configuration.display_name}”和“${config.display_name}”使用不同引擎，请分配到不同节点`)
    byNode.set(config.runtime_id, member)
  }
  return issues
}
